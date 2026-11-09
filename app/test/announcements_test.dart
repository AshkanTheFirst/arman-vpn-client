import 'package:flutter_test/flutter_test.dart';
import 'package:arman_vpn/app_settings.dart';
import 'package:arman_vpn/core/announcements.dart';

Map<String, Object?> announcement({
  String id = 'notice',
  String mode = 'once',
  int priority = 10,
  int revision = 1,
}) => {
  'id': id,
  'revision': revision,
  'enabled': true,
  'priority': priority,
  'type': 'info',
  'title': {'fa': 'اطلاعیه', 'en': 'Service notice'},
  'message': {'fa': 'متن اطلاعیه', 'en': 'A service message.'},
  'display': {'mode': mode},
  'dismissible': true,
};

void main() {
  test('filters disabled messages, platforms, versions and dates, then orders by priority', () {
    final now = DateTime.utc(2026, 11, 9, 12);
    final store = AnnouncementStore(
      AppSettings(),
      os: 'windows',
      version: '1.0.14',
      now: () => now,
    );
    addTearDown(store.dispose);
    store.receive(
      parseAnnouncements([
        announcement(id: 'low'),
        announcement(id: 'high', priority: 200),
        {...announcement(id: 'disabled'), 'enabled': false},
        {
          ...announcement(id: 'android'),
          'platforms': ['android'],
        },
        {...announcement(id: 'newer'), 'min_app_version': '1.0.15'},
        {...announcement(id: 'older'), 'max_app_version': '1.0.13'},
        {...announcement(id: 'expired'), 'expires_at': '2026-11-09T11:59:00Z'},
        {...announcement(id: 'future'), 'starts_at': '2026-11-09T12:01:00Z'},
        {
          ...announcement(id: 'inclusive'),
          'min_app_version': '1.0.14',
          'max_app_version': '1.0.14',
        },
      ]),
    );
    expect(store.visible.map((a) => a.id), ['high', 'inclusive', 'low']);
    expect(translated(store.visible.first.title, 'fa'), 'اطلاعیه');
    expect(translated(store.visible.first.title, 'fr'), 'Service notice');
    expect(translated({'zh_Hant': '繁體'}, 'zh-Hant'), '繁體');
  });

  test('once is recorded only after presentation and a new revision is shown again', () {
    final settings = AppSettings();
    AnnouncementStore open() =>
        AnnouncementStore(settings, os: 'windows', version: '1.0.14');
    var store = open();
    store.receive(parseAnnouncements([announcement()]));
    expect(settings.announcementSeen, isEmpty);
    store.presented(store.visible.single);
    expect(store.visible, hasLength(1));
    store.dispose();
    store = open();
    expect(store.visible, isEmpty);
    store.receive(parseAnnouncements([announcement(revision: 2)]));
    expect(store.visible.single.revision, 2);
    store.dispose();
  });

  test(
    'per-launch returns after restart while acknowledged messages stay hidden',
    () {
      final settings = AppSettings();
      var store = AnnouncementStore(settings, version: '1.0.14');
      store.receive(
        parseAnnouncements([
          announcement(id: 'launch', mode: 'per_launch'),
          announcement(id: 'ack', mode: 'until_acknowledged'),
        ]),
      );
      for (final a in store.visible) {
        store.dismiss(a);
      }
      expect(store.visible, isEmpty);
      store.dispose();
      store = AnnouncementStore(settings, version: '1.0.14');
      expect(store.visible.map((a) => a.id), ['launch']);
      store.receive([]);
      expect(store.visible, isEmpty);
      store.dispose();
    },
  );

  test(
    'non-dismissible messages require their explicit action or remote removal',
    () {
      final store = AnnouncementStore(AppSettings(), version: '1.0.14');
      addTearDown(store.dispose);
      store.receive(
        parseAnnouncements([
          {...announcement(mode: 'until_acknowledged'), 'dismissible': false},
        ]),
      );
      final a = store.visible.single;
      store.dismiss(a);
      expect(store.visible, hasLength(1));
      store.acknowledge(a);
      expect(store.visible, isEmpty);
    },
  );

  test(
    'bad messages do not hide valid ones and unsupported actions are omitted',
    () {
      final messages = parseAnnouncements([
        {'id': 'bad'},
        {...announcement(id: 'date'), 'starts_at': 'not a date'},
        {...announcement(id: 'version'), 'max_app_version': 14},
        {
          ...announcement(),
          'action': {
            'type': 'open_url',
            'label': {'en': 'Open'},
            'url': 'javascript:alert(1)',
          },
        },
      ]);
      expect(messages, hasLength(1));
      expect(messages.single.action, isNull);
    },
  );

  testWidgets('start and expiry boundaries update a running store', (
    tester,
  ) async {
    var now = DateTime.utc(2026, 11, 9, 12);
    final store = AnnouncementStore(
      AppSettings(),
      version: '1.0.14',
      now: () => now,
    );
    var changes = 0;
    store.addListener(() => changes++);
    store.receive(
      parseAnnouncements([
        {
          ...announcement(),
          'starts_at': now.add(const Duration(seconds: 10)).toIso8601String(),
          'expires_at': now.add(const Duration(seconds: 20)).toIso8601String(),
        },
      ]),
    );
    expect(store.visible, isEmpty);
    final before = changes;
    now = now.add(const Duration(seconds: 10));
    await tester.pump(const Duration(seconds: 10));
    expect(store.visible, hasLength(1));
    expect(changes, greaterThan(before));
    now = now.add(const Duration(seconds: 10));
    await tester.pump(const Duration(seconds: 10));
    expect(store.visible, isEmpty);
    store.dispose();
  });
}
