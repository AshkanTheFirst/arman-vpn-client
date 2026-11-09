import 'dart:async';

import 'package:flutter/material.dart';

import '../app_settings.dart';
import '../core/announcements.dart';
import '../core/updates.dart';
import '../l10n/app_localizations.dart';
import '../platform.dart';
import 'widgets.dart';

/// Centers the message over the window without changing the page layout.
class AnnouncementOverlay extends StatelessWidget {
  const AnnouncementOverlay({
    super.key,
    required this.settings,
    required this.onOpenUpdates,
    required this.child,
  });
  final AppSettings settings;
  final VoidCallback onOpenUpdates;
  final Widget child;

  Widget _layout(BuildContext context, bool visible) => Stack(
    fit: StackFit.expand,
    children: [
      child,
      if (visible)
        Positioned.fill(
          child: IgnorePointer(
            child: ColoredBox(
              color: Theme.of(context).colorScheme.scrim
                  .withValues(alpha: 0.16),
            ),
          ),
        ),
      Center(
        child: ConstrainedBox(
          constraints: const BoxConstraints(maxWidth: 480),
          child: AnnouncementPanel(
            settings: settings,
            onOpenUpdates: onOpenUpdates,
          ),
        ),
      ),
    ],
  );

  @override
  Widget build(BuildContext context) =>
      ValueListenableBuilder<AnnouncementStore?>(
        valueListenable: Updates.announcements,
        builder: (context, store, _) => store == null
            ? _layout(context, false)
            : ListenableBuilder(
                listenable: store,
                builder: (context, _) =>
                    _layout(context, store.visible.isNotEmpty),
              ),
      );
}

/// One message at a time, with a clear title, readable body, and action.
class AnnouncementPanel extends StatefulWidget {
  const AnnouncementPanel({
    super.key,
    required this.settings,
    required this.onOpenUpdates,
    this.openExternal,
  });
  final AppSettings settings;
  final VoidCallback onOpenUpdates;
  final Future<bool> Function(String)? openExternal;

  @override
  State<AnnouncementPanel> createState() => _AnnouncementPanelState();
}

class _AnnouncementPanelState extends State<AnnouncementPanel>
    with WidgetsBindingObserver {
  bool _opening = false;
  AppLifecycleState? _lifecycle;

  @override
  void initState() {
    super.initState();
    _lifecycle = WidgetsBinding.instance.lifecycleState;
    WidgetsBinding.instance.addObserver(this);
  }

  @override
  void didChangeAppLifecycleState(AppLifecycleState state) {
    setState(() => _lifecycle = state);
  }

  @override
  void dispose() {
    WidgetsBinding.instance.removeObserver(this);
    super.dispose();
  }

  Future<void> _act(AnnouncementStore store, Announcement item) async {
    final action = item.action;
    if (_opening || action == null) return;
    setState(() => _opening = true);
    try {
      if (action.type == 'navigate') {
        widget.onOpenUpdates();
        store.acknowledge(item);
      } else {
        final opened = await (widget.openExternal ?? openUrl)(action.url!);
        if (!mounted) return;
        if (opened) {
          store.acknowledge(item);
        } else {
          await showLink(context, action.url!);
        }
      }
    } on Exception {
      if (mounted && action.url != null) await showLink(context, action.url!);
    } finally {
      if (mounted) setState(() => _opening = false);
    }
  }

  @override
  Widget build(
    BuildContext context,
  ) => ValueListenableBuilder<AnnouncementStore?>(
    valueListenable: Updates.announcements,
    builder: (context, store, _) => store == null
        ? const SizedBox.shrink()
        : ListenableBuilder(
            listenable: store,
            builder: (context, _) {
              final item = store.visible.firstOrNull;
              if (item == null) return const SizedBox.shrink();
              if (!Updates.windowHidden() &&
                  _lifecycle != AppLifecycleState.hidden &&
                  _lifecycle != AppLifecycleState.paused) {
                WidgetsBinding.instance.addPostFrameCallback((_) {
                  if (mounted) store.presented(item);
                });
              }
              final l = AppLocalizations.of(context);
              final language = widget.settings.language.isEmpty
                  ? l.localeName
                  : widget.settings.language;
              final scheme = Theme.of(context).colorScheme;
              final (color, icon) = switch (item.type) {
                'critical' => (scheme.error, Icons.error_outline_rounded),
                'warning' => (
                  const Color(0xFFB77900),
                  Icons.warning_amber_rounded,
                ),
                'maintenance' => (scheme.tertiary, Icons.build_outlined),
                _ => (scheme.primary, Icons.info_outline_rounded),
              };
              return SafeArea(
                child: Padding(
                  padding: const EdgeInsets.all(20),
                  child: Semantics(
                    container: true,
                    liveRegion: true,
                    child: Material(
                      key: const ValueKey('announcement-card'),
                      elevation: 20,
                      shadowColor: scheme.shadow.withValues(alpha: 0.22),
                      clipBehavior: Clip.antiAlias,
                      color: scheme.surfaceContainerLowest,
                      shape: RoundedRectangleBorder(
                        borderRadius: BorderRadius.circular(24),
                        side: BorderSide(color: color.withValues(alpha: 0.18)),
                      ),
                      child: Padding(
                        padding: const EdgeInsets.all(24),
                        child: Column(
                          mainAxisSize: MainAxisSize.min,
                          crossAxisAlignment: CrossAxisAlignment.stretch,
                          children: [
                            Row(
                              crossAxisAlignment: CrossAxisAlignment.start,
                              children: [
                                Container(
                                  width: 46,
                                  height: 46,
                                  decoration: BoxDecoration(
                                    color: color.withValues(alpha: 0.10),
                                    borderRadius: BorderRadius.circular(14),
                                  ),
                                  child: Icon(icon, color: color, size: 25),
                                ),
                                const SizedBox(width: 14),
                                Expanded(
                                  child: Padding(
                                    padding: const EdgeInsets.only(top: 8),
                                    child: Text(
                                      translated(item.title, language),
                                      style: Theme.of(context)
                                          .textTheme
                                          .titleMedium
                                          ?.copyWith(
                                            fontWeight: FontWeight.w700,
                                            height: 1.5,
                                          ),
                                    ),
                                  ),
                                ),
                                if (item.dismissible)
                                  IconButton(
                                    tooltip: l.dismiss,
                                    onPressed: () => store.dismiss(item),
                                    icon: const Icon(
                                      Icons.close_rounded,
                                      size: 20,
                                    ),
                                    visualDensity: VisualDensity.compact,
                                  ),
                              ],
                            ),
                            const SizedBox(height: 18),
                            ConstrainedBox(
                              constraints: BoxConstraints(
                                maxHeight:
                                    (MediaQuery.sizeOf(context).height * 0.24)
                                        .clamp(64, 180),
                              ),
                              child: SingleChildScrollView(
                                child: Text(
                                  translated(item.message, language),
                                  style: Theme.of(context).textTheme.bodyMedium
                                      ?.copyWith(
                                        color: scheme.onSurfaceVariant,
                                        height: 1.75,
                                      ),
                                ),
                              ),
                            ),
                            if (item.action case final action?) ...[
                              const SizedBox(height: 24),
                              FilledButton.icon(
                                onPressed: _opening
                                    ? null
                                    : () => unawaited(_act(store, item)),
                                style: FilledButton.styleFrom(
                                  padding: const EdgeInsets.symmetric(
                                    horizontal: 18,
                                    vertical: 15,
                                  ),
                                  shape: RoundedRectangleBorder(
                                    borderRadius: BorderRadius.circular(14),
                                  ),
                                ),
                                icon: Icon(
                                  action.type == 'navigate'
                                      ? Icons.system_update_alt_rounded
                                      : Icons.open_in_new_rounded,
                                  size: 19,
                                ),
                                label: Text(translated(action.label, language)),
                              ),
                            ] else if (item.dismissible) ...[
                              const SizedBox(height: 24),
                              OutlinedButton(
                                onPressed: () => store.dismiss(item),
                                style: OutlinedButton.styleFrom(
                                  padding: const EdgeInsets.symmetric(
                                    vertical: 14,
                                  ),
                                  shape: RoundedRectangleBorder(
                                    borderRadius: BorderRadius.circular(14),
                                  ),
                                ),
                                child: Text(l.close),
                              ),
                            ],
                          ],
                        ),
                      ),
                    ),
                  ),
                ),
              );
            },
          ),
  );
}
