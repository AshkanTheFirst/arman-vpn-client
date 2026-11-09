import 'dart:convert';
import 'dart:io';
import 'dart:typed_data';

import 'update_manifest.dart';
import 'core/announcements.dart';

export 'update_manifest.dart';

/// The app's version, "major.minor.patch": `version` in pubspec.yaml, which
/// the build passes (scripts/build-*.ps1: --dart-define=APP_VERSION=1.1.0).
/// "0.0.0" is a development build.
const appVersion = String.fromEnvironment('APP_VERSION', defaultValue: '0.0.0');

/// The GitHub repository, "owner/name", for the source link and the update
/// check.
const githubRepo = 'armanlife/arman-vpn-client';

const updateManifestUrl =
    'https://raw.githubusercontent.com/arman-org/update-manifests/main/arman-vpn-client/updates.json';
const updateManifestMirror =
    'https://cdn.jsdelivr.net/gh/arman-org/update-manifests@main/arman-vpn-client/updates.json';

/// A release server used instead of GitHub, to try updates with a local
/// test release: it serves [updatesFile] at `<server>/updates.json`, answers
/// `<server>/releases/latest` as GitHub's API does, and serves the releases'
/// files, which the core downloads from it too (core/internal/update,
/// ServerEnv).
String get updateServer =>
    (Platform.environment['ARMAN_VPN_UPDATE_SERVER'] ?? '').replaceFirst(
      RegExp(r'/+$'),
      '',
    );

/// The Linux archive's name part for the processor in Dart's
/// [platformVersion] ("... on "linux_arm64"").
String linuxName(String platformVersion) =>
    'Linux-${platformVersion.contains('arm64') ? 'arm64' : 'amd64'}.tar.gz';

/// The name part of the release file that [os]'s app installs or downloads
/// (`Arman-VPN-<version>-<part>`); a release that has it was built for
/// [os]. null for another system.
String? releaseFilePart(String os) => switch (os) {
  'windows' => 'Windows-x64.zip',
  'linux' => linuxName(Platform.version),
  'macos' => 'macOS.tar.gz',
  'android' => 'Android-universal.apk',
  _ => null,
};

/// The manifest filename, also served by the local update test server.
const updatesFile = 'updates.json';

/// Reads the platform's update manifest, its jsDelivr mirror, then the
/// latest GitHub release if neither supplies usable desktop metadata.
/// Android uses stable for scheduled checks and latest for manual
/// checks; unavailable Android metadata is retried without bypassing it.
/// Legacy version-only files remain supported for transition releases.
/// null when [current] is the newest, or when there is no release yet; an
/// error while the named release is not published yet, so the next look
/// comes soon. [proxy] ("host:port"), when set, carries the requests (the
/// VPN's own proxy, for networks where GitHub is blocked); they fall back
/// to a direct connection. [server], [updatesMirror], [os], and [current]
/// are for tests; a custom [server] uses a mirror only when one is supplied.
Future<Release?> checkForUpdate({
  String? proxy,
  String repo = githubRepo,
  String? server,
  String? manifestUrl,
  String? updatesMirror,
  String? os,
  String current = appVersion,
  UpdateCheck check = UpdateCheck.automatic,
  void Function(List<Announcement>)? onAnnouncements,
}) async {
  final srv = server ?? updateServer;
  final client = HttpClient()
    ..connectionTimeout = const Duration(seconds: 15)
    ..findProxy = (_) =>
        proxy == null || srv.isNotEmpty ? 'DIRECT' : 'PROXY $proxy; DIRECT';
  try {
    final system = os ?? Platform.operatingSystem;
    var metadata = await _updateMetadata(
      client,
      manifestUrl ?? (srv.isEmpty ? updateManifestUrl : '$srv/$updatesFile'),
      system,
      srv,
      repo,
      check,
      current,
    );
    final mirror =
        updatesMirror ??
        (srv.isEmpty && manifestUrl == null ? updateManifestMirror : null);
    if (metadata == null && mirror != null) {
      metadata = await _updateMetadata(
        client,
        mirror,
        system,
        srv,
        repo,
        check,
        current,
      );
    }
    if (metadata == null) {
      if (system == 'android') {
        throw const HttpException('Android update metadata is unavailable');
      }
      return await _latestRelease(client, srv, repo, current, system);
    }
    onAnnouncements?.call(metadata.announcements);
    final release = metadata.release;
    if (!newerVersion(release.version, current)) return null;
    if (release.downloads.isNotEmpty) {
      final asset = release.downloads[archiveKey(system, Platform.version)];
      if (asset == null) {
        throw const FormatException('No update package for this system');
      }
      if (!await _published(client, asset.url)) {
        throw HttpException('version ${release.version} is not published yet');
      }
      return release;
    }
    final wanted = release.version;
    final releases = srv.isEmpty
        ? 'https://github.com/$repo/releases'
        : '$srv/releases';
    final part = releaseFilePart(system);
    final file = part == null ? 'SHA256SUMS.txt' : 'Arman-VPN-$wanted-$part';
    if (!await _published(client, '$releases/download/v$wanted/$file')) {
      throw HttpException('version $wanted is not published yet');
    }
    return release;
  } finally {
    client.close(force: true);
  }
}

/// [os]'s version in [updates], the decoded [updatesFile]:
/// "major.minor.patch", else null.
String? versionFor(Object? updates, String os) {
  final v = updates is Map<String, Object?> ? updates[os] : null;
  return v is String && RegExp(r'^\d+\.\d+\.\d+$').hasMatch(v) ? v : null;
}

/// The platform's rich manifest record or a legacy version-only offer;
/// null permits the caller to try the next metadata source.
Future<({Release release, List<Announcement> announcements})?> _updateMetadata(
  HttpClient client,
  String url,
  String os,
  String server,
  String repo,
  UpdateCheck check,
  String current,
) async {
  try {
    final (status, body) = await _get(client, url);
    if (status != 200) return null;
    final json = jsonDecode(body);
    if (json is Map<String, Object?> && json.containsKey('platforms')) {
      final platforms = json['platforms'];
      if (platforms is! Map<String, Object?>) return null;
      final releases = json['releases'];
      if (json.containsKey('releases') && releases is! Map<String, Object?>) {
        return null;
      }
      final release = releaseForCheck(
        platforms[os],
        os: os,
        current: current,
        check: check,
        releases: releases as Map<String, Object?>?,
      );
      return release != null && release.downloads.isNotEmpty
          ? (
              release: release,
              announcements: parseAnnouncements(json['announcements']),
            )
          : null;
    }
    final version = versionFor(json, os);
    if (version == null) return null;
    final base = server.isEmpty ? 'https://github.com/$repo' : server;
    return (
      release: Release(version, '$base/releases/tag/v$version'),
      announcements: <Announcement>[],
    );
  } catch (_) {
    return null;
  }
}

/// The latest release on GitHub (GitHub's API), when newer than [current].
Future<Release?> _latestRelease(
  HttpClient client,
  String server,
  String repo,
  String current,
  String system,
) async {
  final (status, body) = await _get(
    client,
    server.isEmpty
        ? 'https://api.github.com/repos/$repo/releases/latest'
        : '$server/releases/latest',
    accept: 'application/vnd.github+json',
  );
  // GitHub's answer for a repository without releases.
  if (status == 404) return null;
  if (status != 200) throw HttpException('HTTP $status');
  final j = jsonDecode(body);
  final tag = j is Map<String, Object?> && j['tag_name'] is String
      ? j['tag_name']! as String
      : '';
  final url = j is Map<String, Object?> && j['html_url'] is String
      ? j['html_url']! as String
      : '';
  final version = tag.replaceFirst(RegExp('^[vV]'), '');
  if (!validUpdateVersion(version) ||
      !validUpdateUrl(url) ||
      !newerVersion(version, current)) {
    return null;
  }
  if (j is Map<String, Object?> && j['assets'] is List) {
    final assets = <String, String>{
      for (final a in j['assets']! as List)
        if (a is Map<String, Object?> &&
            a['name'] is String &&
            validUpdateUrl(a['browser_download_url']))
          a['name']! as String: a['browser_download_url']! as String,
    };
    final parts = switch (system) {
      'windows' => {
        'archive-x64': 'Windows-x64.zip',
        'installer-x64': 'Windows-Setup-x64.exe',
      },
      'android' => {
        for (final arch in ['universal', 'arm64-v8a', 'armeabi-v7a', 'x86_64'])
          arch: 'Android-$arch.apk',
      },
      'linux' => {'amd64': 'Linux-amd64.tar.gz', 'arm64': 'Linux-arm64.tar.gz'},
      'macos' => {'archive': 'macOS.tar.gz', 'installer': 'macOS.dmg'},
      _ => <String, String>{},
    };
    final downloads = <String, Object?>{};
    for (final entry in parts.entries) {
      final fixed = 'Arman-VPN-${entry.value}';
      final versioned = 'Arman-VPN-$version-${entry.value}';
      final file = assets.containsKey(fixed) ? fixed : versioned;
      if (assets.containsKey(file)) {
        downloads[entry.key] = {'file': file, 'url': assets[file]};
      }
    }
    final release = releaseFromJson({
      'version': version,
      'release_url': url,
      'checksums_url': assets['SHA256SUMS.txt'],
      'signature_url': assets['SHA256SUMS.txt.sig'],
      'downloads': downloads,
    });
    if (release == null ||
        !release.downloads.containsKey(archiveKey(system, Platform.version))) {
      throw const FormatException(
        'The release has no signed update package for this system',
      );
    }
    return release;
  }
  return Release(version, url);
}

/// Whether the release file at [url] is there: GitHub answers a published
/// release's file with a redirect to where it is stored, and 404 before.
Future<bool> _published(HttpClient client, String url) async {
  final req = await client
      .headUrl(Uri.parse(url))
      .timeout(const Duration(seconds: 20));
  req
    ..followRedirects = false
    ..headers.set(HttpHeaders.userAgentHeader, 'arman-vpn/$appVersion');
  final res = await req.close().timeout(const Duration(seconds: 20));
  await res.drain<void>();
  if (res.statusCode == 404) return false;
  if (res.statusCode != 200 &&
      res.statusCode != 206 &&
      ![301, 302, 303, 307, 308].contains(res.statusCode)) {
    throw HttpException('HTTP ${res.statusCode}');
  }
  return true;
}

/// GETs [url] with the app's headers: the status and the body.
Future<(int, String)> _get(
  HttpClient client,
  String url, {
  String accept = 'application/json',
}) async {
  final req = await client
      .getUrl(Uri.parse(url))
      .timeout(const Duration(seconds: 20));
  req.headers
    ..set(HttpHeaders.acceptHeader, accept)
    ..set(HttpHeaders.userAgentHeader, 'arman-vpn/$appVersion');
  final res = await req.close().timeout(const Duration(seconds: 20));
  final bytes = BytesBuilder(copy: false);
  await for (final chunk in res.timeout(const Duration(seconds: 20))) {
    if (bytes.length + chunk.length > 512 * 1024) {
      throw const FormatException('Update metadata is too large');
    }
    bytes.add(chunk);
  }
  return (res.statusCode, utf8.decode(bytes.takeBytes()));
}
