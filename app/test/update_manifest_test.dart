import 'dart:convert';
import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:arman_vpn/app_info.dart';
import 'package:arman_vpn/app_settings.dart';
import 'package:arman_vpn/core/updates.dart';
import 'package:arman_vpn/core/announcements.dart';

Map<String, Object?> record(
  String base, {
  String version = '1.2.3',
  bool mandatory = false,
}) => {
  'version': version,
  'mandatory': mandatory,
  'release_url': '$base/releases/tag/v$version',
  'checksums_url': '$base/checksums',
  'signature_url': '$base/signature',
  'downloads': {
    for (final key in [
      'archive-x64',
      'installer-x64',
      'arm64-v8a',
      'universal',
      'archive',
      'installer',
      'amd64',
      'arm64',
    ])
      key: {
        'file': 'package-$key.zip',
        'url': '$base/download/$key?version=$version',
      },
  },
};

Future<String> fixture(
  Map<String, (int, String)> routes, {
  List<String>? requests,
}) async {
  final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
  addTearDown(() => server.close(force: true));
  server.listen((request) {
    final path = '${request.method} ${request.uri.path}';
    requests?.add(path);
    final (status, body) = routes[path] ?? (404, '');
    request.response.statusCode = status;
    if (request.method != 'HEAD') request.response.write(body);
    request.response.close();
  });
  return 'http://127.0.0.1:${server.port}';
}

Map<String, Object?> compactManifest(String base) {
  const files = {
    'android': {
      'universal': 'Arman-VPN-Android-universal.apk',
      'arm64-v8a': 'Arman-VPN-Android-arm64-v8a.apk',
      'armeabi-v7a': 'Arman-VPN-Android-armeabi-v7a.apk',
      'x86_64': 'Arman-VPN-Android-x86_64.apk',
    },
    'windows': {
      'installer-x64': 'Arman-VPN-Windows-Setup-x64.exe',
      'archive-x64': 'Arman-VPN-Windows-x64.zip',
    },
    'linux': {
      'amd64': 'Arman-VPN-Linux-amd64.tar.gz',
      'arm64': 'Arman-VPN-Linux-arm64.tar.gz',
    },
    'macos': {
      'installer': 'Arman-VPN-macOS.dmg',
      'archive': 'Arman-VPN-macOS.tar.gz',
    },
  };
  Map<String, Object?> platform(String os, String version) => {
    'version': version,
    'mandatory': false,
    'downloads': {
      for (final entry in files[os]!.entries) entry.key: {'file': entry.value},
    },
  };
  return {
    'releases': {
      for (final version in ['1.0.15', '1.0.16'])
        version: {
          'release_url': '$base/releases/tag/v$version',
          'base_url': '$base/v$version/',
          'checksums_file': 'SHA256SUMS.txt',
          'signature_file': 'SHA256SUMS.txt.sig',
        },
    },
    'platforms': {
      for (final os in files.keys)
        os: os == 'android'
            ? {
                'stable': platform(os, '1.0.15'),
                'latest': platform(os, '1.0.16'),
              }
            : platform(os, '1.0.15'),
    },
    'announcements': [
      {
        'id': 'compact-notice',
        'enabled': true,
        'title': {'en': 'App update'},
        'message': {'en': 'Please update the app.'},
      },
    ],
  };
}

void main() {
  test('compact releases resolve each platform and Android channel', () {
    const base = 'https://downloads.example';
    final manifest = compactManifest(base);
    final releases = manifest['releases']! as Map<String, Object?>;
    final platforms = manifest['platforms']! as Map<String, Object?>;
    for (final os in platforms.keys) {
      for (final check in UpdateCheck.values) {
        final release = releaseForCheck(
          platforms[os],
          os: os,
          current: '1.0.14',
          check: check,
          releases: releases,
        )!;
        final version = os == 'android' && check == UpdateCheck.manual
            ? '1.0.16'
            : '1.0.15';
        expect(release.version, version);
        expect(release.url, '$base/releases/tag/v$version');
        expect(release.checksumsUrl, '$base/v$version/SHA256SUMS.txt');
        expect(release.signatureUrl, '$base/v$version/SHA256SUMS.txt.sig');
        for (final download in release.downloads.values) {
          expect(download.url, '$base/v$version/${download.file}');
        }
        final cached = releaseFromJson(
          jsonDecode(jsonEncode(release.toJson())),
        )!;
        expect(cached.toJson(), release.toJson());
        if (os != 'android') {
          final source = cached.installSource(
            os: os,
            platformVersion: 'linux_arm64',
          )!;
          expect(source['checksums_url'], release.checksumsUrl);
          expect(source['signature_url'], release.signatureUrl);
          expect(source['url'], '$base/v$version/${source['file']}');
        }
      }
    }
  });

  test(
    'compact metadata checks publication and delivers announcements',
    () async {
      final assets = await fixture({
        for (final file in [
          'Arman-VPN-Windows-x64.zip',
          'Arman-VPN-Linux-amd64.tar.gz',
          'Arman-VPN-macOS.tar.gz',
          'Arman-VPN-Android-universal.apk',
        ])
          'HEAD /v1.0.15/$file': (200, ''),
        'HEAD /v1.0.16/Arman-VPN-Android-universal.apk': (200, ''),
      });
      final manifest = compactManifest(assets);
      final requests = <String>[];
      final server = await fixture({
        'GET /updates.json': (200, jsonEncode(manifest)),
      }, requests: requests);
      for (final os in ['android', 'windows', 'linux', 'macos']) {
        List<Announcement>? announcements;
        final release = await checkForUpdate(
          server: server,
          os: os,
          current: '1.0.14',
          onAnnouncements: (items) => announcements = items,
        );
        expect(release?.version, '1.0.15');
        expect(announcements?.single.id, 'compact-notice');
      }
      final latest = (await checkForUpdate(
        server: server,
        os: 'android',
        current: '1.0.15',
        check: UpdateCheck.manual,
      ))!;
      expect(
        await downloadUrl(latest, os: 'android', abi: 'arm64-v8a'),
        '$assets/v1.0.16/Arman-VPN-Android-arm64-v8a.apk',
      );
      expect(requests, List.filled(5, 'GET /updates.json'));
    },
  );

  test('compact explicit HTTPS URLs override the shared release directory', () {
    final manifest = compactManifest('https://downloads.example');
    final releases = manifest['releases']! as Map<String, Object?>;
    final shared = releases['1.0.15']! as Map<String, Object?>;
    shared.remove('base_url');
    shared['checksums_url'] = 'https://metadata.example/sums?version=15';
    shared['signature_url'] = 'https://metadata.example/signature?version=15';
    final windows =
        (manifest['platforms']! as Map<String, Object?>)['windows']!
            as Map<String, Object?>;
    final downloads = windows['downloads']! as Map<String, Object?>;
    for (final entry in downloads.entries) {
      (entry.value! as Map<String, Object?>)['url'] =
          'https://another-host.example/get/${entry.key}?version=15';
    }
    final release = releaseForCheck(
      windows,
      os: 'windows',
      current: '1.0.14',
      releases: releases,
    )!;
    expect(release.checksumsUrl, shared['checksums_url']);
    expect(release.signatureUrl, shared['signature_url']);
    expect(
      release.installSource(os: 'windows')!['url'],
      'https://another-host.example/get/archive-x64?version=15',
    );
  });

  test('compact invalid references and explicit URLs are not guessed', () {
    for (final change in <String, Object?>{
      'missing-version': null,
      'base_url': 'http://downloads.example/files/',
      'base-query': 'https://downloads.example/files/?token=example',
      'checksums_file': '../SHA256SUMS.txt',
      'signature_file': '',
      'asset-url': 'http://downloads.example/package.zip',
      'asset-file': '../package.zip',
    }.entries) {
      final manifest = compactManifest('https://downloads.example');
      final releases = manifest['releases']! as Map<String, Object?>;
      final shared = releases['1.0.15']! as Map<String, Object?>;
      final windows =
          (manifest['platforms']! as Map<String, Object?>)['windows']!
              as Map<String, Object?>;
      if (change.key == 'missing-version') {
        releases.remove('1.0.15');
      } else if (change.key.startsWith('asset-')) {
        final downloads = windows['downloads']! as Map<String, Object?>;
        (downloads['archive-x64']! as Map<String, Object?>)[change.key ==
                    'asset-url'
                ? 'url'
                : 'file'] =
            change.value;
      } else {
        shared[change.key == 'base-query' ? 'base_url' : change.key] =
            change.value;
      }
      expect(
        releaseForCheck(
          windows,
          os: 'windows',
          current: '1.0.14',
          releases: releases,
        ),
        isNull,
        reason: change.key,
      );
    }
  });

  test('compact mandatory stable updates also apply to manual latest', () {
    final manifest = compactManifest('https://downloads.example');
    final channels =
        (manifest['platforms']! as Map<String, Object?>)['android']!
            as Map<String, Object?>;
    (channels['stable']! as Map<String, Object?>)['mandatory'] = true;
    final releases = manifest['releases']! as Map<String, Object?>;
    for (final current in ['1.0.14', '1.0.15']) {
      final latest = releaseForCheck(
        channels,
        os: 'android',
        current: current,
        check: UpdateCheck.manual,
        releases: releases,
      )!;
      expect(latest.version, '1.0.16');
      expect(latest.mandatory, current == '1.0.14');
    }
  });

  test(
    'Android daily and manual checks select their own release and APK',
    () async {
      final requests = <String>[];
      final assets = await fixture({'HEAD /download/universal': (200, '')});
      final server = await fixture({
        'GET /updates.json': (
          200,
          jsonEncode({
            'platforms': {
              'android': {
                'stable': record(assets, version: '1.0.15'),
                'latest': record(assets, version: '1.0.16'),
              },
            },
            'announcements': [
              {
                'id': 'android-notice',
                'enabled': true,
                'title': {'en': 'Service notice'},
                'message': {'en': 'A notice for both checks.'},
              },
            ],
          }),
        ),
      }, requests: requests);
      for (final check in UpdateCheck.values) {
        List<Announcement>? received;
        final release = (await checkForUpdate(
          server: server,
          os: 'android',
          current: '1.0.14',
          check: check,
          onAnnouncements: (items) => received = items,
        ))!;
        final version = check == UpdateCheck.automatic ? '1.0.15' : '1.0.16';
        expect(release.version, version);
        expect(
          await downloadUrl(release, os: 'android', abi: 'arm64-v8a'),
          '$assets/download/arm64-v8a?version=$version',
        );
        expect(received?.single.id, 'android-notice');
      }
      expect(requests, ['GET /updates.json', 'GET /updates.json']);
    },
  );

  test(
    'Android manual check finds latest when stable is already installed',
    () async {
      final assets = await fixture({'HEAD /download/universal': (200, '')});
      final requests = <String>[];
      final server = await fixture({
        'GET /updates.json': (
          200,
          jsonEncode({
            'platforms': {
              'android': {
                'stable': record(assets, version: '1.0.15'),
                'latest': record(assets, version: '1.0.16'),
              },
            },
          }),
        ),
      }, requests: requests);
      expect(
        await checkForUpdate(server: server, os: 'android', current: '1.0.15'),
        isNull,
      );
      expect(
        (await checkForUpdate(
          server: server,
          os: 'android',
          current: '1.0.15',
          check: UpdateCheck.manual,
        ))?.version,
        '1.0.16',
      );
      expect(
        await checkForUpdate(
          server: server,
          os: 'android',
          current: '1.0.16',
          check: UpdateCheck.manual,
        ),
        isNull,
      );
      expect(
        await checkForUpdate(
          server: server,
          os: 'android',
          current: '1.0.17',
          check: UpdateCheck.manual,
        ),
        isNull,
      );
      expect(requests, List.filled(4, 'GET /updates.json'));
    },
  );

  test(
    'Android manual offers retain a required stable update until installed',
    () {
      final channels = {
        'stable': record(
          'https://downloads.example',
          version: '1.0.15',
          mandatory: true,
        ),
        'latest': record('https://downloads.example', version: '1.0.16'),
      };
      expect(
        releaseForCheck(
          channels,
          os: 'android',
          current: '1.0.14',
          check: UpdateCheck.manual,
        )?.mandatory,
        isTrue,
      );
      expect(
        releaseForCheck(
          channels,
          os: 'android',
          current: '1.0.15',
          check: UpdateCheck.manual,
        )?.mandatory,
        isFalse,
      );
    },
  );

  test('Android metadata failures never bypass controlled offers via GitHub latest', () async {
    for (final check in UpdateCheck.values) {
      final requests = <String>[];
      final server = await fixture({
        'GET /updates.json': (503, ''),
        'GET /mirror': (200, 'invalid JSON'),
      }, requests: requests);
      await expectLater(
        checkForUpdate(
          server: server,
          updatesMirror: '$server/mirror',
          os: 'android',
          current: '1.0.14',
          check: check,
        ),
        throwsA(isA<HttpException>()),
      );
      expect(requests, ['GET /updates.json', 'GET /mirror']);
    }
  });

  test(
    'invalid Android latest tries the mirror while daily still uses stable',
    () async {
      final assets = await fixture({'HEAD /download/universal': (200, '')});
      for (final latest in <Object?>[
        null,
        record(assets, version: '1.0.16-beta'),
        record(assets, version: '1.0.14'),
      ]) {
        final requests = <String>[];
        final server = await fixture({
          'GET /updates.json': (
            200,
            jsonEncode({
              'platforms': {
                'android': {
                  'stable': record(assets, version: '1.0.15'),
                  'latest': latest,
                },
              },
            }),
          ),
          'GET /mirror': (
            200,
            jsonEncode({
              'platforms': {
                'android': {
                  'stable': record(assets, version: '1.0.15'),
                  'latest': record(assets, version: '1.0.16'),
                },
              },
            }),
          ),
        }, requests: requests);
        expect(
          (await checkForUpdate(
            server: server,
            updatesMirror: '$server/mirror',
            os: 'android',
            current: '1.0.14',
          ))?.version,
          '1.0.15',
        );
        expect(
          (await checkForUpdate(
            server: server,
            updatesMirror: '$server/mirror',
            os: 'android',
            current: '1.0.14',
            check: UpdateCheck.manual,
          ))?.version,
          '1.0.16',
        );
        expect(requests, [
          'GET /updates.json',
          'GET /updates.json',
          'GET /mirror',
        ]);
      }
    },
  );

  test(
    'an unpublished Android latest fails without offering another release',
    () async {
      final assets = await fixture({});
      final requests = <String>[];
      final server = await fixture({
        'GET /updates.json': (
          200,
          jsonEncode({
            'platforms': {
              'android': {
                'stable': record(assets, version: '1.0.14'),
                'latest': record(assets, version: '1.0.16'),
              },
            },
          }),
        ),
      }, requests: requests);
      await expectLater(
        checkForUpdate(
          server: server,
          os: 'android',
          current: '1.0.14',
          check: UpdateCheck.manual,
        ),
        throwsA(isA<HttpException>()),
      );
      expect(requests, ['GET /updates.json']);
    },
  );

  test(
    'flat manifests remain usable during deployment for both Android checks',
    () {
      for (final check in UpdateCheck.values) {
        expect(
          releaseForCheck(
            record('https://downloads.example'),
            os: 'android',
            current: '1.0.0',
            check: check,
          )?.version,
          '1.2.3',
        );
        expect(
          releaseForCheck(
            record('https://downloads.example'),
            os: 'windows',
            current: '1.0.0',
            check: check,
          )?.version,
          '1.2.3',
        );
      }
    },
  );

  test(
    'announcements are delivered even when no app update is available',
    () async {
      final server = await fixture({
        'GET /updates.json': (
          200,
          jsonEncode({
            'platforms': {
              'windows': record('https://downloads.example', version: '1.0.14'),
            },
            'announcements': [
              {
                'id': 'service',
                'enabled': true,
                'title': {'en': 'Service notice'},
                'message': {'en': 'A new announcement.'},
              },
              {'id': 'invalid'},
            ],
          }),
        ),
      });
      List<Announcement>? received;
      final update = await checkForUpdate(
        server: server,
        os: 'windows',
        current: '1.0.14',
        onAnnouncements: (items) => received = items,
      );
      expect(update, isNull);
      expect(received?.single.id, 'service');
    },
  );

  test(
    'latest-release fallback resolves fixed assets without versioned aliases',
    () async {
      final requests = <String>[];
      final server = await fixture({
        'GET /releases/latest': (
          200,
          jsonEncode({
            'tag_name': 'v1.2.3',
            'html_url': 'https://github.com/example/app/releases/tag/v1.2.3',
            'assets': [
              for (final name in [
                'Arman-VPN-Windows-x64.zip',
                'Arman-VPN-Windows-Setup-x64.exe',
                'SHA256SUMS.txt',
                'SHA256SUMS.txt.sig',
              ])
                {
                  'name': name,
                  'browser_download_url': 'https://downloads.example/$name',
                },
            ],
          }),
        ),
      }, requests: requests);
      final release = (await checkForUpdate(
        server: server,
        os: 'windows',
        current: '1.0.0',
      ))!;
      expect(
        release.installSource(os: 'windows')!['url'],
        'https://downloads.example/Arman-VPN-Windows-x64.zip',
      );
      expect(requests, ['GET /updates.json', 'GET /releases/latest']);
    },
  );

  test(
    'latest-release fallback rejects a release without this platform archive',
    () async {
      final server = await fixture({
        'GET /releases/latest': (
          200,
          jsonEncode({
            'tag_name': 'v1.2.3',
            'html_url': 'https://github.com/example/app/releases/tag/v1.2.3',
            'assets': [
              for (final name in [
                'Arman-VPN-Android-universal.apk',
                'SHA256SUMS.txt',
                'SHA256SUMS.txt.sig',
              ])
                {
                  'name': name,
                  'browser_download_url': 'https://downloads.example/$name',
                },
            ],
          }),
        ),
      });
      await expectLater(
        checkForUpdate(server: server, os: 'windows', current: '1.0.0'),
        throwsA(isA<FormatException>()),
      );
    },
  );

  test('external URLs and fixed signed filenames survive serialization', () {
    final value = record('https://downloads.example', mandatory: true);
    final release = releaseFromJson(value)!;
    expect(release.mandatory, isTrue);
    final source = release.installSource(os: 'windows');
    expect(source, {
      'file': 'package-archive-x64.zip',
      'url': 'https://downloads.example/download/archive-x64?version=1.2.3',
      'checksums_url': 'https://downloads.example/checksums',
      'signature_url': 'https://downloads.example/signature',
    });
    expect(
      releaseFromJson(jsonDecode(jsonEncode(release.toJson())))!.toJson(),
      release.toJson(),
    );
    expect(
      release.installSource(
        os: 'linux',
        platformVersion: 'linux_arm64',
      )!['file'],
      'package-arm64.zip',
    );
    expect(release.installSource(os: 'macos')!['file'], 'package-archive.zip');
  });

  test(
    'manual downloads use the correct platform, ABI, and package kind',
    () async {
      final release = releaseFromJson(record('https://downloads.example'))!;
      expect(
        await downloadUrl(release, os: 'android', abi: 'arm64-v8a'),
        contains('/arm64-v8a?'),
      );
      expect(
        await downloadUrl(release, os: 'android', abi: 'unknown'),
        contains('/universal?'),
      );
      expect(
        await downloadUrl(release, os: 'windows', installed: true),
        contains('/installer-x64?'),
      );
      expect(
        await downloadUrl(release, os: 'windows', installed: false),
        contains('/archive-x64?'),
      );
      expect(await downloadUrl(release, os: 'macos'), contains('/installer?'));
      expect(
        await downloadUrl(release, os: 'linux', platformVersion: 'linux_arm64'),
        contains('/arm64?'),
      );
    },
  );

  test('malformed and unsafe manifest entries are rejected', () {
    for (final change in <Map<String, Object?>>[
      {'version': 'v1.2.3'},
      {'mandatory': 'true'},
      {'checksums_url': 'http://downloads.example/checksums'},
      {'signature_url': 'file:///signature'},
      {'release_url': 'javascript:alert(1)'},
      {
        'downloads': {
          'archive-x64': {
            'file': '../package.zip',
            'url': 'https://downloads.example/file',
          },
        },
      },
      {
        'downloads': {
          'archive-x64': {
            'file': 'package.zip',
            'url': 'https://user:pass@downloads.example/file',
          },
        },
      },
    ]) {
      expect(
        releaseFromJson({...record('https://downloads.example'), ...change}),
        isNull,
        reason: '$change',
      );
    }
  });

  test(
    'a manifest chooses an external archive without guessing its URL',
    () async {
      final assetRequests = <String>[];
      final assets = await fixture({
        'HEAD /download/archive-x64': (200, ''),
      }, requests: assetRequests);
      final requests = <String>[];
      final server = await fixture({
        'GET /updates.json': (
          200,
          jsonEncode({
            'platforms': {'windows': record(assets, mandatory: true)},
          }),
        ),
      }, requests: requests);
      final release = await checkForUpdate(
        server: server,
        os: 'windows',
        current: '1.0.0',
      );
      expect(release?.mandatory, isTrue);
      expect(
        release?.downloads['archive-x64']?.url,
        '$assets/download/archive-x64?version=1.2.3',
      );
      expect(requests, ['GET /updates.json']);
      expect(assetRequests, ['HEAD /download/archive-x64']);
    },
  );

  test(
    'a current platform record is authoritative over the mirror and latest',
    () async {
      final requests = <String>[];
      final server = await fixture({
        'GET /updates.json': (
          200,
          jsonEncode({
            'platforms': {
              'windows': record('https://downloads.example', version: '1.0.0'),
            },
          }),
        ),
      }, requests: requests);
      expect(
        await checkForUpdate(
          server: server,
          updatesMirror: '$server/mirror',
          os: 'windows',
          current: '1.0.0',
        ),
        isNull,
      );
      expect(requests, ['GET /updates.json']);
    },
  );

  test('invalid primary metadata uses the valid manifest mirror', () async {
    final assets = await fixture({'HEAD /download/archive-x64': (200, '')});
    final requests = <String>[];
    final server = await fixture({
      'GET /updates.json': (
        200,
        jsonEncode({
          'platforms': {
            'windows': {'version': '1.2.3'},
          },
        }),
      ),
      'GET /mirror': (
        200,
        jsonEncode({
          'platforms': {'windows': record(assets)},
        }),
      ),
    }, requests: requests);
    expect(
      (await checkForUpdate(
        server: server,
        updatesMirror: '$server/mirror',
        os: 'windows',
        current: '1.0.0',
      ))?.version,
      '1.2.3',
    );
    expect(requests, ['GET /updates.json', 'GET /mirror']);
  });

  test(
    'an unpublished manifest package does not fall back to another release',
    () async {
      final assets = await fixture({});
      final requests = <String>[];
      final server = await fixture({
        'GET /updates.json': (
          200,
          jsonEncode({
            'platforms': {'windows': record(assets)},
          }),
        ),
      }, requests: requests);
      await expectLater(
        checkForUpdate(server: server, os: 'windows', current: '1.0.0'),
        throwsA(isA<HttpException>()),
      );
      expect(requests, ['GET /updates.json']);
    },
  );

  test(
    'cached metadata retains links and mandatory status after a restart',
    () async {
      final dir = await Directory.systemTemp.createTemp('update-metadata-');
      addTearDown(() => dir.delete(recursive: true));
      final settings = AppSettings(file: File('${dir.path}/app-settings.json'));
      final release = releaseFromJson(
        record('https://downloads.example', mandatory: true),
      )!;
      settings.update(
        updateVersion: release.version,
        updateUrl: release.url,
        updateDetails: release.toJson(),
      );
      final deadline = DateTime.now().add(const Duration(seconds: 3));
      AppSettings saved;
      do {
        await Future<void>.delayed(const Duration(milliseconds: 20));
        saved = await AppSettings.load(dir.path);
      } while (saved.updateDetails.isEmpty &&
          DateTime.now().isBefore(deadline));
      final cached = releaseFromJson(saved.updateDetails)!;
      expect(cached.mandatory, isTrue);
      expect(
        cached.installSource(os: 'windows'),
        release.installSource(os: 'windows'),
      );
    },
  );
}
