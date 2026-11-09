import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:arman_vpn/app_settings.dart';
import 'package:arman_vpn/core/announcements.dart';
import 'package:arman_vpn/core/updates.dart';
import 'package:arman_vpn/l10n/app_localizations.dart';
import 'package:arman_vpn/ui/announcement_panel.dart';

void main() {
  testWidgets(
    'a centered notice preserves page geometry and outside controls',
    (tester) async {
      final settings = AppSettings();
      final store = AnnouncementStore(settings, version: '1.0.14');
      Updates.announcements.value = store;
      var taps = 0;
      const pageKey = ValueKey('unchanged-page');
      await tester.pumpWidget(
        MaterialApp(
          localizationsDelegates: AppLocalizations.localizationsDelegates,
          supportedLocales: AppLocalizations.supportedLocales,
          home: Scaffold(
            body: AnnouncementOverlay(
              settings: settings,
              onOpenUpdates: () {},
              child: ColoredBox(
                key: pageKey,
                color: Colors.blue,
                child: Align(
                  alignment: Alignment.topLeft,
                  child: FilledButton(
                    onPressed: () => taps++,
                    child: const Text('Connect'),
                  ),
                ),
              ),
            ),
          ),
        ),
      );
      await tester.pumpAndSettle();
      final page = tester.getRect(find.byKey(pageKey));
      final connect = tester.getRect(find.text('Connect'));
      store.receive(
        parseAnnouncements([
          {
            'id': 'floating',
            'enabled': true,
            'title': {'en': 'Service notice'},
            'message': {'en': 'No page reflow.'},
          },
        ]),
      );
      await tester.pumpAndSettle();
      expect(tester.getRect(find.byKey(pageKey)), page);
      expect(tester.getRect(find.text('Connect')), connect);
      expect(find.text('Service notice'), findsOneWidget);
      final card = tester.getRect(
        find.byKey(const ValueKey('announcement-card')),
      );
      expect(card.center.dx, closeTo(page.center.dx, 1));
      expect(card.center.dy, closeTo(page.center.dy, 1));
      await tester.tap(find.text('Connect'));
      expect(taps, 1);
      expect(tester.takeException(), isNull);
      await tester.pumpWidget(const SizedBox());
      Updates.reset();
    },
  );

  testWidgets(
    'a failed external action shows its address without acknowledging the notice',
    (tester) async {
      final settings = AppSettings();
      final store = AnnouncementStore(settings, version: '1.0.14');
      Updates.announcements.value = store;
      store.receive(
        parseAnnouncements([
          {
            'id': 'external',
            'enabled': true,
            'title': {'en': 'Service notice'},
            'message': {'en': 'More information is available.'},
            'display': {'mode': 'until_acknowledged'},
            'action': {
              'type': 'open_url',
              'url': 'https://example.invalid/status',
              'label': {'en': 'Read more'},
            },
          },
        ]),
      );
      await tester.pumpWidget(
        MaterialApp(
          localizationsDelegates: AppLocalizations.localizationsDelegates,
          supportedLocales: AppLocalizations.supportedLocales,
          home: Scaffold(
            body: Column(
              children: [
                AnnouncementPanel(
                  settings: settings,
                  onOpenUpdates: () {},
                  openExternal: (_) async => false,
                ),
              ],
            ),
          ),
        ),
      );
      await tester.pumpAndSettle();
      await tester.tap(find.text('Read more'));
      await tester.pumpAndSettle();
      expect(
        find.textContaining('https://example.invalid/status'),
        findsOneWidget,
      );
      expect(settings.announcementAcknowledged, isEmpty);
      expect(store.visible, hasLength(1));
      await tester.pumpWidget(const SizedBox());
      Updates.reset();
    },
  );
}
