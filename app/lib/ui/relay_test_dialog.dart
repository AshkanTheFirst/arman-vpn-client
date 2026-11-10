import 'dart:async';
import 'dart:math' as math;

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

import '../core/core_api.dart';
import '../core/core_controller.dart';
import '../core/relay_test.dart';
import '../l10n/app_localizations.dart';
import 'widgets.dart';

Future<void> showRelayTestDialog(
  BuildContext context,
  CoreController core,
  String address,
) => showDialog<void>(
  context: context,
  builder: (_) => _RelayTestDialog(core: core, address: address),
);

class _RelayTestDialog extends StatefulWidget {
  const _RelayTestDialog({required this.core, required this.address});

  final CoreController core;
  final String address;

  @override
  State<_RelayTestDialog> createState() => _RelayTestDialogState();
}

class _RelayTestDialogState extends State<_RelayTestDialog> {
  final _steps = <RelayTestStep>[];
  final _scroll = ScrollController();
  StreamSubscription<RelayTestEvent>? _subscription;
  RelayTestResult? _result;
  String? _error;
  bool _running = true;
  int _runId = 0;

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addPostFrameCallback((_) {
      if (mounted) _run(initial: true);
    });
  }

  @override
  void dispose() {
    _runId++;
    unawaited(_subscription?.cancel());
    _scroll.dispose();
    super.dispose();
  }

  Future<void> _run({bool initial = false}) async {
    if (_running && !initial) return;
    final l = AppLocalizations.of(context);
    final id = ++_runId;
    await _subscription?.cancel();
    if (!mounted) return;
    setState(() {
      _running = true;
      _steps.clear();
      _result = null;
      _error = null;
    });
    try {
      final stream = await widget.core.call(
        (api) async => api.testRelay(widget.address),
      );
      if (!mounted || id != _runId) return;
      _subscription = stream.listen(
        (event) {
          if (!mounted || id != _runId) return;
          setState(() {
            if (event.step case final step?) _steps.add(step);
            if (event.result case final result?) {
              _result = result;
              _steps
                ..clear()
                ..addAll(result.steps);
              _running = false;
            }
          });
          WidgetsBinding.instance.addPostFrameCallback((_) {
            if (mounted && _scroll.hasClients) {
              _scroll.jumpTo(_scroll.position.maxScrollExtent);
            }
          });
        },
        onError: (Object error) {
          if (!mounted || id != _runId) return;
          setState(() {
            _running = false;
            _error = error is CoreApiException
                ? error.message
                : l.relayTestInterrupted;
          });
        },
        onDone: () {
          if (!mounted || id != _runId) return;
          setState(() {
            _running = false;
            if (_result == null && _error == null) {
              _error = l.relayTestInterrupted;
            }
          });
        },
      );
    } on CoreUnavailable catch (e) {
      if (mounted && id == _runId) {
        setState(() {
          _running = false;
          _error = describeProblem(l, e);
        });
      }
    }
  }

  String get _report => [
    for (final step in _steps) step.logLine,
    if (_error != null) 'ERROR  $_error',
  ].join('\n');

  @override
  Widget build(BuildContext context) {
    final l = AppLocalizations.of(context);
    final theme = Theme.of(context);
    final result = _result;
    final text = _running
        ? l.testing
        : result?.ok == true
        ? l.relayTestPassed
        : l.relayTestFailed;
    return AlertDialog(
      title: Text(l.relayTestTitle),
      content: SizedBox(
        width: 720,
        height: math.min(480, MediaQuery.sizeOf(context).height * .58),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            Row(
              children: [
                if (_running)
                  const SizedBox.square(
                    dimension: 20,
                    child: CircularProgressIndicator(strokeWidth: 2),
                  )
                else
                  Icon(
                    result?.ok == true
                        ? Icons.check_circle_outline_rounded
                        : Icons.error_outline_rounded,
                    color: result?.ok == true
                        ? StatusColors.good
                        : theme.colorScheme.error,
                  ),
                const SizedBox(width: 10),
                Expanded(child: Text(text)),
              ],
            ),
            const SizedBox(height: 8),
            Text(l.relayTestHint, style: theme.textTheme.bodySmall),
            const SizedBox(height: 12),
            Expanded(
              child: DecoratedBox(
                decoration: BoxDecoration(
                  color: theme.colorScheme.surfaceContainerLow,
                  border: Border.all(color: theme.colorScheme.outlineVariant),
                  borderRadius: BorderRadius.circular(12),
                ),
                child: Directionality(
                  textDirection: TextDirection.ltr,
                  child: Scrollbar(
                    controller: _scroll,
                    child: SelectionArea(
                      child: ListView(
                        controller: _scroll,
                        padding: const EdgeInsets.all(12),
                        children: [
                          for (final step in _steps)
                            Padding(
                              padding: const EdgeInsets.only(bottom: 8),
                              child: Text(
                                step.logLine,
                                style: TextStyle(
                                  fontFamily: 'Consolas',
                                  fontFamilyFallback: const [
                                    'Menlo',
                                    'DejaVu Sans Mono',
                                    'monospace',
                                  ],
                                  fontSize: 12.5,
                                  height: 1.45,
                                  color: switch (step.status) {
                                    'error' => theme.colorScheme.error,
                                    'warning' => StatusColors.busy,
                                    'ok' => StatusColors.good,
                                    _ => theme.colorScheme.onSurface,
                                  },
                                ),
                              ),
                            ),
                          if (_error != null)
                            Text(
                              _error!,
                              style: TextStyle(color: theme.colorScheme.error),
                            ),
                        ],
                      ),
                    ),
                  ),
                ),
              ),
            ),
          ],
        ),
      ),
      actions: [
        TextButton.icon(
          onPressed: _report.isEmpty
              ? null
              : () => Clipboard.setData(ClipboardData(text: _report)),
          icon: const Icon(Icons.copy_all_outlined),
          label: Text(l.relayTestCopy),
        ),
        TextButton(
          onPressed: _running ? null : () => _run(),
          child: Text(l.retry),
        ),
        FilledButton(
          onPressed: () => Navigator.pop(context),
          child: Text(l.close),
        ),
      ],
    );
  }
}
