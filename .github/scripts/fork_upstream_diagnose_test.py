"""Offline upstream comparison and repository-lane regressions."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
MODULE = 'example.invalid/proxy'


class DiagnoseTests(unittest.TestCase):
  def setUp(self):
    self.directory = tempfile.TemporaryDirectory()
    self.addCleanup(self.directory.cleanup)
    self.root = Path(self.directory.name)
    self.environment = dict(os.environ, GIT_CONFIG_NOSYSTEM='1', GIT_CONFIG_GLOBAL=os.devnull,
      GIT_AUTHOR_NAME='Fixture', GIT_AUTHOR_EMAIL='fixture@example.invalid',
      GIT_COMMITTER_NAME='Fixture', GIT_COMMITTER_EMAIL='fixture@example.invalid',
      GIT_AUTHOR_DATE='2026-01-01T00:00:00Z', GIT_COMMITTER_DATE='2026-01-01T00:00:00Z',
      PYTHONDONTWRITEBYTECODE='1')
    self.git('init', '--initial-branch=main')
    self.git('config', 'commit.gpgsign', 'false')
    self.git('config', 'core.hooksPath', os.devnull)
    (self.root / 'go.mod').write_text(f'module {MODULE}\n\ngo 1.26.0\n')
    (self.root / 'upstream-marker').write_text('upstream\n')
    (self.root / 'repo.sh').write_text('exit 97\n')
    self.git('add', '.')
    self.git('commit', '-m', 'exact upstream')
    self.upstream = self.git('rev-parse', 'HEAD')
    (self.root / 'upstream-marker').write_text('fork\n')
    self.git('commit', '-am', 'fork source')
    self.source = self.git('rev-parse', 'HEAD')
    scripts = self.root / '.github/scripts'
    scripts.mkdir(parents=True)
    source = ROOT / '.github/scripts/fork-upstream-diagnose.py'
    self.script = scripts / source.name
    shutil.copyfile(source, self.script)
    self.diagnostics = self.root / '.local/fork-diagnostics'
    self.diagnostics.mkdir(parents=True)
    self.report = self.diagnostics / 'candidate.jsonl'
    self.state = self.root / '.local/fork-candidate.json'
    self.state.write_text(json.dumps({'source_sha': self.source, 'upstream_main_sha': self.upstream}))
    self.go = self.root / '.local/toolchains/go/bin/go'
    self.go.parent.mkdir(parents=True)
    self.go.write_text('''#!/usr/bin/env python3
import json, os, pathlib, signal, subprocess, sys, time
if sys.argv[1:] == ['version']:
  print('go version go1.26.4 linux/amd64')
  sys.exit(0)
assert pathlib.Path('upstream-marker').read_text() == 'upstream\\n'
assert '-mod=readonly' in sys.argv and '-count=1' in sys.argv and '-json' in sys.argv
assert os.environ['GOTOOLCHAIN'] == 'local'
assert os.environ['GOWORK'] == 'off' and os.environ['GOPROXY'] == 'off'
fixture = pathlib.Path(__file__).resolve().parents[4] / '.local/fork-diagnostics'
mode = (fixture / 'scenario').read_text()
print('BASELINE_GIT_ROOT=' + subprocess.check_output(['git', 'rev-parse', '--show-toplevel'], text=True).strip(), file=sys.stderr)
print('BASELINE_GIT_HEAD=' + subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True).strip(), file=sys.stderr)
if mode == 'survivor':
  child = subprocess.Popen([sys.executable, '-I', '-c', 'import time; time.sleep(60)'],
    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
  (fixture / 'child.pid').write_text(str(child.pid))
if mode in ('timeout', 'cancel'):
  child = subprocess.Popen([sys.executable, '-c',
    "import signal,time; signal.signal(signal.SIGTERM,signal.SIG_IGN); print('ready',flush=True); time.sleep(60)"],
    stdout=subprocess.PIPE, text=True)
  child.stdout.readline()
  (fixture / 'child.pid').write_text(str(child.pid))
  if mode == 'cancel': os.kill(os.getppid(), signal.SIGTERM)
  time.sleep(60)
if mode == 'missing-package':
  print('directory worker not found', file=sys.stderr)
  sys.exit(1)
if mode == 'malformed':
  print('not JSON')
  sys.exit(1)
pkg = 'example.invalid/proxy/worker'
def event(action, test=None, **extra):
  value = dict(Action=action, Package=pkg, **extra)
  if test is not None: value['Test'] = test
  print(json.dumps(value))
if mode == 'build':
  event('fail', FailedBuild=pkg)
  sys.exit(1)
if mode != 'missing':
  event('run', 'TestBridge')
  if mode == 'different-subtest': event('fail', 'TestBridge/unrelated')
  if mode == 'same-subtest': event('fail', 'TestBridge/subcase')
  event('fail' if mode in ('same', 'different-subtest', 'same-subtest') else 'pass', 'TestBridge')
event('fail' if mode in ('same', 'different-subtest', 'same-subtest') else 'pass')
sys.exit(1 if mode in ('same', 'different-subtest', 'same-subtest') else 0)
''')
    self.go.chmod(0o700)
    self.events([{'Action': 'fail', 'Package': MODULE + '/worker', 'Test': 'TestBridge'},
      {'Action': 'fail', 'Package': MODULE + '/worker'}])

  def git(self, *args):
    return subprocess.check_output(['git', *args], cwd=self.root, env=self.environment,
      text=True, stderr=subprocess.STDOUT).strip()

  def events(self, events):
    self.report.write_text(''.join(json.dumps(event) + '\n' for event in events))

  def diagnose(self, mode='same', *args):
    (self.diagnostics / 'scenario').write_text(mode)
    result = subprocess.run([sys.executable, '-I', str(self.script), str(self.report), *args],
      cwd=self.root, env=self.environment, capture_output=True, text=True,
      timeout=15)
    summaries = list(self.diagnostics.glob('upstream-*/summary.json'))
    self.assertEqual(len(summaries), 1, result.stdout + result.stderr)
    self.summary_path = summaries[0]
    return result, json.loads(self.summary_path.read_text())

  def test_same_failure_uses_exact_upstream_and_collapses_subtests(self):
    result, summary = self.diagnose()
    self.assertEqual(result.returncode, 0, result.stderr)
    self.assertEqual(summary['source_sha'], self.source)
    self.assertEqual(summary['upstream_main_sha'], self.upstream)
    self.assertEqual(summary['comparisons'][0]['tests'], ['TestBridge'])
    self.assertEqual(summary['comparisons'][0]['status'], 'observed_upstream_failure')
    self.assertEqual(self.git('rev-parse', 'HEAD'), self.source)
    self.assertEqual(self.git('for-each-ref', '--format=%(refname)'), 'refs/heads/main')
    self.assertTrue((self.summary_path.parent / 'package-0.stdout.jsonl').is_file())
    self.assertFalse((self.summary_path.parent / 'tree').exists())

  def test_baseline_has_its_own_git_root_and_exact_upstream_head(self):
    result, summary = self.diagnose()
    self.assertEqual(result.returncode, 0, result.stderr)
    stderr = (self.summary_path.parent / 'package-0.stderr.log').read_text()
    values = dict(line.split('=', 1) for line in stderr.splitlines() if line.startswith('BASELINE_GIT_'))
    self.assertNotEqual(values['BASELINE_GIT_ROOT'], str(self.root))
    self.assertEqual(values['BASELINE_GIT_HEAD'], self.upstream)
    self.assertTrue(Path(values['BASELINE_GIT_ROOT']).is_relative_to(self.summary_path.parent))
    self.assertEqual(self.git('rev-parse', 'HEAD'), self.source)
    self.assertEqual(self.git('for-each-ref', '--format=%(refname)'), 'refs/heads/main')

  def test_missing_subtest_never_counts_as_an_upstream_pass(self):
    self.events([{'Action': 'fail', 'Package': MODULE + '/worker', 'Test': 'TestBridge/subcase'},
      {'Action': 'fail', 'Package': MODULE + '/worker', 'Test': 'TestBridge'},
      {'Action': 'fail', 'Package': MODULE + '/worker'}])
    result, summary = self.diagnose('pass')
    self.assertEqual(result.returncode, 1)
    self.assertEqual(summary['comparisons'][0]['status'], 'inconclusive')

  def test_different_failing_subtest_never_counts_as_the_same_failure(self):
    self.events([{'Action': 'fail', 'Package': MODULE + '/worker', 'Test': 'TestBridge/subcase'},
      {'Action': 'fail', 'Package': MODULE + '/worker', 'Test': 'TestBridge'},
      {'Action': 'fail', 'Package': MODULE + '/worker'}])
    result, summary = self.diagnose('different-subtest')
    self.assertEqual(result.returncode, 1)
    self.assertEqual(summary['comparisons'][0]['status'], 'inconclusive')

  def test_same_subtest_is_preserved_while_only_the_top_level_filter_runs(self):
    self.events([{'Action': 'fail', 'Package': MODULE + '/worker', 'Test': 'TestBridge/subcase'},
      {'Action': 'fail', 'Package': MODULE + '/worker', 'Test': 'TestBridge'},
      {'Action': 'fail', 'Package': MODULE + '/worker'}])
    result, summary = self.diagnose('same-subtest')
    self.assertEqual(result.returncode, 0, result.stderr)
    self.assertEqual(summary['comparisons'][0]['tests'], ['TestBridge', 'TestBridge/subcase'])
    command = json.loads((self.summary_path.parent / 'package-0.command.json').read_text())
    self.assertEqual(command[command.index('-run') + 1], '^(TestBridge)$')

  def test_successful_launcher_cannot_leave_a_running_descendant(self):
    result, summary = self.diagnose('survivor')
    self.assertEqual(result.returncode, 0, result.stderr)
    pid = int((self.diagnostics / 'child.pid').read_text())
    try:
      stat = Path(f'/proc/{pid}/stat')
      if stat.exists():
        self.assertEqual(stat.read_text().split()[2], 'Z', 'successful launcher left a running descendant')
    finally:
      try:
        os.kill(pid, 9)
      except ProcessLookupError:
        pass

  def test_upstream_pass_is_only_possible_integration_regression_evidence(self):
    result, summary = self.diagnose('pass')
    self.assertEqual(result.returncode, 0, result.stderr)
    self.assertEqual(summary['comparisons'][0]['status'], 'passes_upstream')
    self.assertIn('not permission to publish', summary['notice'])

  def test_missing_test_is_inconclusive_even_when_go_exits_zero(self):
    result, summary = self.diagnose('missing')
    self.assertEqual(result.returncode, 1)
    self.assertEqual(summary['comparisons'][0]['status'], 'inconclusive')
    self.assertIn('missing', summary['comparisons'][0]['reason'])

  def test_upstream_build_failure_is_inconclusive(self):
    result, summary = self.diagnose('build')
    self.assertEqual(result.returncode, 1)
    self.assertEqual(summary['comparisons'][0]['status'], 'inconclusive')

  def test_malformed_baseline_is_inconclusive(self):
    result, summary = self.diagnose('malformed')
    self.assertEqual(result.returncode, 1)
    self.assertEqual(summary['status'], 'inconclusive')

  def test_malformed_candidate_never_runs_baseline(self):
    self.report.write_text('not JSON\n')
    result, summary = self.diagnose()
    self.assertEqual(result.returncode, 1)
    self.assertEqual(summary['status'], 'inconclusive')
    self.assertFalse(list(self.summary_path.parent.glob('package-*.stdout.jsonl')))

  def test_external_package_is_rejected(self):
    self.events([{'Action': 'fail', 'Package': 'other.invalid/proxy/worker', 'Test': 'TestBridge'}])
    result, summary = self.diagnose()
    self.assertEqual(result.returncode, 1)
    self.assertIn('module', summary['reason'])

  def test_explicit_sha_works_without_candidate_state(self):
    self.state.unlink()
    result, summary = self.diagnose('same', '--upstream-sha', self.upstream)
    self.assertEqual(result.returncode, 0, result.stderr)
    self.assertEqual(summary['upstream_main_sha'], self.upstream)

  def test_unknown_sha_is_inconclusive_without_fetch(self):
    result, summary = self.diagnose('same', '--upstream-sha', 'f' * 40)
    self.assertEqual(result.returncode, 1)
    self.assertEqual(summary['status'], 'inconclusive')

  def test_timeout_terminates_descendant_processes(self):
    result, summary = self.diagnose('timeout', '--timeout-seconds', '2')
    self.assertEqual(result.returncode, 1)
    self.assertEqual(summary['status'], 'inconclusive')
    self.assertIn('timeout', summary['reason'])
    pid = int((self.diagnostics / 'child.pid').read_text())
    stat = Path(f'/proc/{pid}/stat')
    if stat.exists():
      self.assertEqual(stat.read_text().split()[2], 'Z', 'diagnostic left a running descendant')

  def test_cancellation_terminates_descendant_processes(self):
    result, summary = self.diagnose('cancel')
    self.assertEqual(result.returncode, 1)
    self.assertEqual(summary['status'], 'inconclusive')
    self.assertIn('cancelled', summary['reason'])
    pid = int((self.diagnostics / 'child.pid').read_text())
    stat = Path(f'/proc/{pid}/stat')
    if stat.exists():
      self.assertEqual(stat.read_text().split()[2], 'Z', 'diagnostic left a running descendant')

  def test_missing_package_is_inconclusive(self):
    result, summary = self.diagnose('missing-package')
    self.assertEqual(result.returncode, 1)
    self.assertEqual(summary['status'], 'inconclusive')
    stderr = self.summary_path.parent / 'package-0.stderr.log'
    self.assertIn('directory worker not found', stderr.read_text())

  def test_missing_toolchain_is_inconclusive(self):
    self.go.unlink()
    result, summary = self.diagnose()
    self.assertEqual(result.returncode, 1)
    self.assertEqual(summary['status'], 'inconclusive')
    self.assertIn('No such file', summary['reason'])

  def test_changed_candidate_source_is_inconclusive(self):
    (self.root / 'later').write_text('later\n')
    self.git('add', 'later')
    self.git('commit', '-m', 'later source')
    result, summary = self.diagnose()
    self.assertEqual(result.returncode, 1)
    self.assertIn('source changed', summary['reason'])

  def test_report_without_failed_tests_is_inconclusive(self):
    self.events([{'Action': 'pass', 'Package': MODULE + '/worker', 'Test': 'TestBridge'}])
    result, summary = self.diagnose()
    self.assertEqual(result.returncode, 1)
    self.assertIn('no failed', summary['reason'])

  def test_package_only_failure_reruns_the_complete_package(self):
    self.events([{'Action': 'fail', 'Package': MODULE + '/worker'}])
    result, summary = self.diagnose()
    self.assertEqual(result.returncode, 0, result.stderr)
    self.assertEqual(summary['comparisons'][0]['tests'], [])
    command = json.loads((self.summary_path.parent / 'package-0.command.json').read_text())
    self.assertNotIn('-run', command)
    self.assertEqual(command[-1], './worker')

  def test_maintenance_lane_runs_without_a_go_toolchain_or_setup(self):
    shutil.copyfile(ROOT / 'repo.sh', self.root / 'repo.sh')
    (self.root / 'repo.sh').chmod(0o700)
    self.go.unlink()
    (self.root / '.github/scripts/sample_test.py').write_text(
      'import unittest\nclass T(unittest.TestCase):\n def test_preflight(self): self.assertTrue(True)\n')
    result = subprocess.run(['bash', str(self.root / 'repo.sh'), 'test-maintenance'],
      cwd=self.root, env=self.environment, capture_output=True, text=True, timeout=15)
    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
    self.assertIn('Ran 1 test', result.stderr)

  def test_test_lane_forwards_flags_and_keeps_python_output_off_json_stdout(self):
    shutil.copyfile(ROOT / 'repo.sh', self.root / 'repo.sh')
    (self.root / 'repo.sh').chmod(0o700)
    (self.root / '.github/scripts/sample_test.py').write_text(
      'import unittest\nclass T(unittest.TestCase):\n def test_output(self): print("python diagnostic")\n')
    self.go.write_text('''#!/usr/bin/env python3
import json,sys
if sys.argv[1:] == ['version']:
  print('go version go1.26.4 linux/amd64')
else:
  print(json.dumps(sys.argv[1:]))
''')
    result = subprocess.run(['bash', str(self.root / 'repo.sh'), 'test', '-json'],
      cwd=self.root, env=self.environment, capture_output=True, text=True, timeout=15)
    self.assertEqual(result.returncode, 0, result.stderr)
    self.assertEqual(json.loads(result.stdout), ['test', '-json', './...'])
    self.assertIn('python diagnostic', result.stderr)


class WorkflowTests(unittest.TestCase):
  def test_candidate_failure_remains_a_publication_gate(self):
    workflow = (ROOT / '.github/workflows/fork-release.yml').read_text()
    self.assertIn("github.repository == 'akalsi-org/CLIProxyAPI'", workflow)
    self.assertIn("cron: '17 * * * *'", workflow)
    self.assertIn('cancel-in-progress: false', workflow)
    self.assertLess(workflow.index('./repo.sh test-maintenance'), workflow.index('- name: Prepare upstream candidate'))
    self.assertNotIn('continue-on-error', workflow)
    self.assertIn('id: full_tests', workflow)
    self.assertIn("if: failure() && steps.full_tests.outcome == 'failure'", workflow)
    self.assertIn('set -euo pipefail', workflow)
    self.assertIn('./repo.sh test -json', workflow)
    self.assertIn('| tee .local/fork-diagnostics/candidate.jsonl', workflow)
    self.assertIn("if: success() && steps.candidate.outputs.changed == 'true'", workflow)
    self.assertRegex(workflow, r'actions/upload-artifact@[0-9a-f]{40}')
    self.assertIn('if: always()', workflow)
    self.assertIn('include-hidden-files: true', workflow)
    publication = workflow.split('- name: Publish tested source and completed release', 1)[1]
    self.assertNotIn('always()', publication.split('- name:', 1)[0])
    self.assertNotIn('failure()', publication.split('- name:', 1)[0])


if __name__ == '__main__':
  unittest.main()
