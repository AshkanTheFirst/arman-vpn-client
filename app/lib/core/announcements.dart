import 'dart:async';
import 'dart:io';

import 'package:flutter/foundation.dart';

import '../app_settings.dart';
import '../update_manifest.dart';

Map<String, String> _translations(Object? value) =>
    value is Map<String, Object?>
    ? {
        for (final e in value.entries)
          if (e.value is String && (e.value! as String).trim().isNotEmpty)
            e.key: e.value! as String,
      }
    : {};

String translated(Map<String, String> values, String language) =>
    values[language.replaceAll('-', '_')] ??
    values[language.split(RegExp('[-_]')).first] ??
    values['en'] ??
    values['fa'] ??
    values.values.firstOrNull ??
    '';

class AnnouncementAction {
  const AnnouncementAction(this.type, this.label, {this.url, this.target});
  final String type;
  final Map<String, String> label;
  final String? url;
  final String? target;

  static AnnouncementAction? fromJson(Object? value) {
    if (value is! Map<String, Object?>) return null;
    final label = _translations(value['label']);
    if (label.isEmpty) return null;
    if (value['type'] == 'navigate' && value['target'] == 'updates') {
      return AnnouncementAction('navigate', label, target: 'updates');
    }
    if (value['type'] == 'open_url' && validUpdateUrl(value['url'])) {
      return AnnouncementAction(
        'open_url',
        label,
        url: value['url']! as String,
      );
    }
    return null;
  }
}

class Announcement {
  Announcement._(
    this.json,
    this.id,
    this.revision,
    this.title,
    this.message,
    this.startsAt,
    this.expiresAt,
    this.action,
  );

  final Map<String, Object?> json;
  final String id;
  final int revision;
  final Map<String, String> title;
  final Map<String, String> message;
  final DateTime? startsAt;
  final DateTime? expiresAt;
  final AnnouncementAction? action;
  String get key => '$id:$revision';
  bool get enabled => json['enabled'] == true;
  String get type => json['type'] is String ? json['type']! as String : 'info';
  int get priority => json['priority'] is int ? json['priority']! as int : 0;
  bool get dismissible => json['dismissible'] != false;
  String get mode =>
      (json['display'] as Map<String, Object?>?)?['mode'] as String? ?? 'once';

  bool eligible(String os, String version, DateTime now) {
    if (!enabled ||
        (startsAt != null && now.isBefore(startsAt!)) ||
        (expiresAt != null && !now.isBefore(expiresAt!))) {
      return false;
    }
    final platforms = json['platforms'];
    if (platforms is List && platforms.isNotEmpty && !platforms.contains(os)) {
      return false;
    }
    final min = json['min_app_version'] as String?;
    final max = json['max_app_version'] as String?;
    return !(min != null && newerVersion(min, version)) &&
        !(max != null && newerVersion(version, max));
  }

  static Announcement? fromJson(Object? value) {
    if (value is! Map<String, Object?> ||
        value['id'] is! String ||
        (value['id']! as String).trim().isEmpty) {
      return null;
    }
    final revision = value['revision'] ?? 1;
    if (revision is! int || revision < 1) return null;
    final title = _translations(value['title']);
    final message = _translations(value['message']);
    if (title.isEmpty || message.isEmpty) return null;
    final display = value['display'];
    if (display != null && display is! Map<String, Object?>) return null;
    final mode = (display as Map<String, Object?>?)?['mode'] ?? 'once';
    if (!['once', 'per_launch', 'until_acknowledged'].contains(mode)) {
      return null;
    }
    for (final key in ['min_app_version', 'max_app_version']) {
      if (value[key] != null && !validUpdateVersion(value[key])) return null;
    }
    DateTime? time(String key) =>
        value[key] is String ? DateTime.tryParse(value[key]! as String) : null;
    final start = time('starts_at'), end = time('expires_at');
    if ((value['starts_at'] != null && start == null) ||
        (value['expires_at'] != null && end == null) ||
        (start != null && end != null && !start.isBefore(end))) {
      return null;
    }
    return Announcement._(
      Map.unmodifiable(value),
      value['id']! as String,
      revision,
      title,
      message,
      start,
      end,
      AnnouncementAction.fromJson(value['action']),
    );
  }
}

List<Announcement> parseAnnouncements(Object? value) {
  final items = <String, Announcement>{};
  if (value is List) {
    for (final entry in value) {
      final item = Announcement.fromJson(entry);
      if (item != null && !items.containsKey(item.id)) items[item.id] = item;
    }
  }
  return List.unmodifiable(items.values);
}

/// Delivery state is local to this installation; revisions allow a changed
/// message to be shown again. Time limits are reevaluated while the app runs.
class AnnouncementStore extends ChangeNotifier {
  AnnouncementStore(
    this.settings, {
    String? os,
    required this.version,
    DateTime Function()? now,
  }) : os = os ?? Platform.operatingSystem,
       now = now ?? DateTime.now {
    _items = parseAnnouncements(settings.announcementCache);
    _schedule();
  }

  final AppSettings settings;
  final String os;
  final String version;
  final DateTime Function() now;
  List<Announcement> _items = [];
  final _presented = <String>{};
  final _dismissed = <String>{};
  Timer? _timer;

  List<Announcement> get visible {
    final items = _items
        .where(
          (a) =>
              a.eligible(os, version, now()) &&
              !_dismissed.contains(a.key) &&
              switch (a.mode) {
                'once' =>
                  settings.announcementSeen[a.id] != a.revision ||
                      _presented.contains(a.key),
                'until_acknowledged' =>
                  settings.announcementAcknowledged[a.id] != a.revision,
                _ => true,
              },
        )
        .toList();
    items.sort((a, b) {
      final priority = b.priority.compareTo(a.priority);
      return priority != 0 ? priority : a.id.compareTo(b.id);
    });
    return items;
  }

  void receive(List<Announcement> items) {
    _items = items;
    settings.update(announcementCache: [for (final a in items) a.json]);
    _schedule();
    notifyListeners();
  }

  void presented(Announcement item) {
    if (!_presented.add(item.key) || item.mode != 'once') return;
    settings.update(
      announcementSeen: {...settings.announcementSeen, item.id: item.revision},
    );
  }

  void dismiss(Announcement item) {
    if (!item.dismissible) return;
    acknowledge(item);
  }

  /// Closing or successfully following the explicit action acknowledges a
  /// message. A failed external launch leaves it available to try again.
  void acknowledge(Announcement item) {
    presented(item);
    _dismissed.add(item.key);
    if (item.mode == 'until_acknowledged') {
      settings.update(
        announcementAcknowledged: {
          ...settings.announcementAcknowledged,
          item.id: item.revision,
        },
      );
    }
    notifyListeners();
  }

  void _schedule() {
    _timer?.cancel();
    final at = now();
    final boundaries = [
      for (final item in _items) ...[?item.startsAt, ?item.expiresAt],
    ].where((t) => t.isAfter(at)).toList()..sort();
    if (boundaries.isNotEmpty) {
      _timer = Timer(boundaries.first.difference(at), () {
        _schedule();
        notifyListeners();
      });
    }
  }

  @override
  void dispose() {
    _timer?.cancel();
    super.dispose();
  }
}
