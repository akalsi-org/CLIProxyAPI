import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('transport_report', Path(__file__).with_name('codex-transport-report.py'))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class TransportReportTests(unittest.TestCase):
  def test_medians_preserve_trial_evidence(self):
    prefix = 'BenchmarkCodexHTTPTransportMatrix/bytes=1024/c=8/headers=stable/pooled-ws-4 '
    output = module.report(prefix + '50 100 ns/op 0 tcp/op 1000 ttfe-p95-ns\n' +
                           prefix + '50 200 ns/op 0 tcp/op 2000 ttfe-p95-ns\nPASS\n')
    self.assertEqual(output['scenario_count'], 1)
    self.assertEqual(output['trial_count'], 2)
    self.assertEqual(output['scenarios'][0]['summary']['ns/op']['median'], 150)
    self.assertEqual(len(output['scenarios'][0]['trials']), 2)

  def test_failures_are_not_reported_as_performance_results(self):
    with self.assertRaisesRegex(ValueError, 'failed'):
      module.report('--- FAIL: BenchmarkCodexHTTPTransportMatrix\nFAIL\n')

  def test_different_metric_sets_cannot_be_combined(self):
    prefix = 'BenchmarkCodexHTTPTransportMatrix/bytes=1024/c=1/headers=changing/utls-http-4 '
    with self.assertRaisesRegex(ValueError, 'metrics differ'):
      module.report(prefix + '50 100 ns/op\n' + prefix + '50 100 ns/op 1 tcp/op\nPASS\n')

  def test_partial_logs_cannot_claim_completed_acceptance(self):
    with self.assertRaisesRegex(ValueError, 'not completed'):
      module.report('BenchmarkCodexHTTPTransportMatrix/bytes=1024/c=1/headers=stable/pooled-ws-4 50 100 ns/op\n')


if __name__ == '__main__':
  unittest.main()
