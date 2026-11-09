import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:arman_vpn/app_settings.dart';
import 'package:arman_vpn/core/android_core.dart';

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  const channel = MethodChannel('app/android');
  final messenger =
      TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger;
  late List<String> calls;
  Map<String, String> countries = {};

  setUp(() {
    calls = [];
    countries = {};
    messenger.setMockMethodCallHandler(channel, (call) async {
      calls.add(call.method);
      expect(call.method, 'routingCountries');
      return countries;
    });
  });
  tearDown(() => messenger.setMockMethodCallHandler(channel, null));

  test('first-run routing uses country without changing English', () async {
    countries = {'network_country': 'ir', 'sim_country': 'ir'};
    final settings = AppSettings()
      ..firstRun = true
      ..language = 'en';
    await AndroidCore.prepareFirstRunRouting(settings, systemCountry: 'US');
    expect(settings.firstRunRegion, 'ir');
    expect(settings.language, 'en');
    expect(settings.firstRun, isTrue);
    expect(calls, ['routingCountries']);
  });

  test(
    'a known roaming network wins over the Iranian SIM and Persian UI',
    () async {
      countries = {'network_country': 'tr', 'sim_country': 'ir'};
      final settings = AppSettings()
        ..firstRun = true
        ..language = 'fa';
      await AndroidCore.prepareFirstRunRouting(settings, systemCountry: 'IR');
      expect(settings.firstRunRegion, 'tr');
      expect(settings.language, 'fa');
      countries['network_country'] = 'de';
      await AndroidCore.prepareFirstRunRouting(settings, systemCountry: 'IR');
      expect(settings.firstRunRegion, '');
    },
  );

  test('Wi-Fi without a network country uses the SIM country', () async {
    countries = {'network_country': '', 'sim_country': 'cn'};
    final settings = AppSettings()..firstRun = true;
    await AndroidCore.prepareFirstRunRouting(settings, systemCountry: 'US');
    expect(settings.firstRunRegion, 'cn');
    expect(settings.language, isEmpty);
  });

  test(
    'a TV without telephony falls back to system country, not language',
    () async {
      final settings = AppSettings()
        ..firstRun = true
        ..language = 'fa';
      await AndroidCore.prepareFirstRunRouting(settings, systemCountry: 'BR');
      expect(settings.firstRunRegion, 'br');
      await AndroidCore.prepareFirstRunRouting(settings, systemCountry: 'US');
      expect(settings.firstRunRegion, '');
      expect(settings.language, 'fa');
    },
  );

  test('platform failure does not prevent onboarding', () async {
    messenger.setMockMethodCallHandler(channel, (_) async {
      throw PlatformException(code: 'unavailable');
    });
    final settings = AppSettings()..firstRun = true;
    await AndroidCore.prepareFirstRunRouting(settings, systemCountry: 'AF');
    expect(settings.firstRunRegion, 'af');
  });

  test(
    'an existing installation never queries country or changes its choices',
    () async {
      countries = {'network_country': 'ir'};
      final settings = AppSettings()..language = 'en';
      await AndroidCore.prepareFirstRunRouting(settings, systemCountry: 'IR');
      expect(calls, isEmpty);
      expect(settings.firstRunRegion, isNull);
      expect(settings.language, 'en');
      expect(settings.firstRun, isFalse);
    },
  );
}
