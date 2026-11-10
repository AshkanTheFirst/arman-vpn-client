/// One sanitized diagnostic line emitted by the core, in elapsed time order.
class RelayTestStep {
  RelayTestStep.fromJson(Map<String, Object?> j)
    : stage = j['stage'] as String? ?? '',
      status = j['status'] as String? ?? 'info',
      code = j['code'] as String? ?? '',
      detail = j['detail'] as String? ?? '',
      elapsedMs = (j['elapsed_ms'] as num?)?.toInt() ?? 0;

  final String stage;
  final String status;
  final String code;
  final String detail;
  final int elapsedMs;

  String get logLine =>
      '${(elapsedMs / 1000).toStringAsFixed(2)}s  ${status.toUpperCase().padRight(7)}  [$stage${code.isEmpty ? '' : ': $code'}] $detail';
}

class RelayTestResult {
  RelayTestResult.fromJson(Map<String, Object?> j)
    : ok = j['ok'] == true,
      host = j['host'] as String? ?? '',
      totalMs = (j['total_ms'] as num?)?.toInt() ?? 0,
      steps = [
        for (final step in j['steps'] as List<Object?>? ?? [])
          if (step is Map<String, Object?>) RelayTestStep.fromJson(step),
      ];

  final bool ok;
  final String host;
  final int totalMs;
  final List<RelayTestStep> steps;
}

class RelayTestEvent {
  RelayTestEvent.fromJson(Map<String, Object?> j)
    : step = switch (j['step']) {
        final Map<String, Object?> step => RelayTestStep.fromJson(step),
        _ => null,
      },
      result = switch (j['result']) {
        final Map<String, Object?> result => RelayTestResult.fromJson(result),
        _ => null,
      };

  final RelayTestStep? step;
  final RelayTestResult? result;
}
