import 'dart:io';

/// Android's scheduled offer and its user-requested latest stable release.
enum UpdateCheck { automatic, manual }

/// Whether "major.minor.patch" [a] is newer than [b].
bool newerVersion(String a, String b) {
  List<int> parts(String v) => [
    for (final p in v.replaceFirst(RegExp('^[vV]'), '').split('.'))
      int.tryParse(p) ?? 0,
  ];
  final x = parts(a), y = parts(b);
  for (var i = 0; i < 3; i++) {
    final xi = i < x.length ? x[i] : 0, yi = i < y.length ? y[i] : 0;
    if (xi != yi) return xi > yi;
  }
  return false;
}

/// A named file in the signed checksum list, downloaded from any HTTPS host.
class ReleaseDownload {
  const ReleaseDownload(this.file, this.url);

  final String file;
  final String url;

  Map<String, Object?> toJson() => {'file': file, 'url': url};
}

/// An update offered to one platform. Empty downloads identify a legacy
/// GitHub release whose filenames are derived from its version.
class Release {
  const Release(
    this.version,
    this.url, {
    this.mandatory = false,
    this.checksumsUrl = '',
    this.signatureUrl = '',
    this.downloads = const {},
  });

  final String version;
  final String url;
  final bool mandatory;
  final String checksumsUrl;
  final String signatureUrl;
  final Map<String, ReleaseDownload> downloads;

  Map<String, Object?> toJson() => {
    'version': version,
    'release_url': url,
    'mandatory': mandatory,
    if (downloads.isNotEmpty) ...{
      'checksums_url': checksumsUrl,
      'signature_url': signatureUrl,
      'downloads': {for (final e in downloads.entries) e.key: e.value.toJson()},
    },
  };

  /// The desktop updater always unpacks an archive, including installed
  /// Windows copies; the manual Download button may offer its installer.
  Map<String, Object?>? installSource({String? os, String? platformVersion}) {
    if (downloads.isEmpty) return null;
    final key = archiveKey(
      os ?? Platform.operatingSystem,
      platformVersion ?? Platform.version,
    );
    final asset = downloads[key];
    if (asset == null) {
      throw const FormatException('No update archive for this system');
    }
    return {
      ...asset.toJson(),
      'checksums_url': checksumsUrl,
      'signature_url': signatureUrl,
    };
  }
}

String archiveKey(String os, String platformVersion) => switch (os) {
  'windows' => 'archive-x64',
  'macos' => 'archive',
  'linux' => platformVersion.contains('arm64') ? 'arm64' : 'amd64',
  'android' => 'universal',
  _ => '',
};

bool validUpdateVersion(Object? value) =>
    value is String && RegExp(r'^\d+\.\d+\.\d+$').hasMatch(value);

bool validUpdateUrl(Object? value) {
  if (value is! String) return false;
  final uri = Uri.tryParse(value);
  if (uri == null ||
      uri.host.isEmpty ||
      uri.userInfo.isNotEmpty ||
      uri.hasFragment) {
    return false;
  }
  final ip = InternetAddress.tryParse(uri.host);
  final local = uri.host == 'localhost' || (ip?.isLoopback ?? false);
  return uri.scheme == 'https' || (uri.scheme == 'http' && local);
}

bool _validUpdateFile(Object? value) =>
    value is String &&
    value.isNotEmpty &&
    value.trim() == value &&
    !value.contains('..') &&
    !RegExp(r'[/\\:\x00\r\n]').hasMatch(value);

/// An explicit URL wins over the shared release directory. Encode the
/// filename as a path segment so characters such as # cannot become a URL
/// fragment. Invalid explicit URLs never fall back to another download.
String? _manifestFileUrl(Object? url, Object? file, Object? base) {
  if (url != null) return validUpdateUrl(url) ? url as String : null;
  if (!_validUpdateFile(file) || !validUpdateUrl(base)) return null;
  final directory = Uri.parse(base! as String);
  if (directory.hasQuery) return null;
  return directory
      .replace(
        path: directory.path.endsWith('/')
            ? directory.path
            : '${directory.path}/',
      )
      .resolveUri(Uri(path: file! as String))
      .toString();
}

/// Resolve the public compact manifest into the same complete record used
/// by the cache and the core's update API. The core receives exact URLs;
/// it does not need to interpret the public manifest again.
Release? _manifestRelease(Object? value, Map<String, Object?>? releases) {
  if (releases == null) return releaseFromJson(value);
  if (value is! Map<String, Object?> || !validUpdateVersion(value['version'])) {
    return null;
  }
  final shared = releases[value['version']];
  if (shared is! Map<String, Object?>) return null;
  final record = <String, Object?>{...shared, ...value};
  final assets = record['downloads'];
  if (assets is! Map<String, Object?> || assets.isEmpty) return null;
  final downloads = <String, Object?>{};
  for (final entry in assets.entries) {
    final asset = entry.value;
    if (asset is! Map<String, Object?> || !_validUpdateFile(asset['file'])) {
      return null;
    }
    final url = _manifestFileUrl(
      asset['url'],
      asset['file'],
      record['base_url'],
    );
    if (url == null) return null;
    downloads[entry.key] = {'file': asset['file'], 'url': url};
  }
  return releaseFromJson({
    'version': record['version'],
    'release_url': record['release_url'],
    'mandatory': record['mandatory'],
    'checksums_url': _manifestFileUrl(
      record['checksums_url'],
      record['checksums_file'],
      record['base_url'],
    ),
    'signature_url': _manifestFileUrl(
      record['signature_url'],
      record['signature_file'],
      record['base_url'],
    ),
    'downloads': downloads,
  });
}

/// Validates a platform record (also the cached form). Missing or invalid
/// rich metadata never silently becomes a guessed GitHub download URL.
Release? releaseFromJson(Object? value) {
  if (value is! Map<String, Object?> ||
      !validUpdateVersion(value['version']) ||
      !validUpdateUrl(value['release_url']) ||
      (value['mandatory'] != null && value['mandatory'] is! bool)) {
    return null;
  }
  final assets = value['downloads'];
  final downloads = <String, ReleaseDownload>{};
  if (assets != null) {
    if (assets is! Map<String, Object?> ||
        assets.isEmpty ||
        !validUpdateUrl(value['checksums_url']) ||
        !validUpdateUrl(value['signature_url'])) {
      return null;
    }
    for (final entry in assets.entries) {
      final asset = entry.value;
      if (asset is! Map<String, Object?> || !validUpdateUrl(asset['url'])) {
        return null;
      }
      final file = asset['file'];
      if (!_validUpdateFile(file)) {
        return null;
      }
      downloads[entry.key] = ReleaseDownload(
        file! as String,
        asset['url']! as String,
      );
    }
  }
  return Release(
    value['version']! as String,
    value['release_url']! as String,
    mandatory: value['mandatory'] == true,
    checksumsUrl: downloads.isEmpty ? '' : value['checksums_url']! as String,
    signatureUrl: downloads.isEmpty ? '' : value['signature_url']! as String,
    downloads: Map.unmodifiable(downloads),
  );
}

/// Android has independently managed stable and latest releases.
/// A flat record remains valid while the remote manifest is being updated.
Release? releaseForCheck(
  Object? value, {
  required String os,
  required String current,
  UpdateCheck check = UpdateCheck.automatic,
  Map<String, Object?>? releases,
}) {
  if (os != 'android' ||
      value is! Map<String, Object?> ||
      (!value.containsKey('stable') && !value.containsKey('latest'))) {
    return _manifestRelease(value, releases);
  }
  final stable = _manifestRelease(value['stable'], releases);
  if (stable == null || stable.downloads.isEmpty) return null;
  if (check == UpdateCheck.automatic) return stable;
  final latest = _manifestRelease(value['latest'], releases);
  if (latest == null ||
      latest.downloads.isEmpty ||
      newerVersion(stable.version, latest.version)) {
    return null;
  }
  // A manual check must not hide a required stable update when the
  // user has not installed that version yet.
  return stable.mandatory && newerVersion(stable.version, current)
      ? releaseFromJson({...latest.toJson(), 'mandatory': true})
      : latest;
}
