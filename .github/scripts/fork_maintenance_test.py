"""Deterministic fork release selection and publication failure checks."""
from contextlib import redirect_stderr
import importlib.util
import io
import json
import os
from pathlib import Path
import shlex
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('fork_maintenance', Path(__file__).with_name('fork-maintenance.py'))
maintenance = importlib.util.module_from_spec(spec)
spec.loader.exec_module(maintenance)


class MaintenanceTests(unittest.TestCase):
  def test_revision_ignores_upstream_and_other_baselines(self):
    tags = ['v8.0.13', 'v8.0.12-akalsi.99', 'v8.0.13-akalsi.2', 'v7.2.125-meta.1']
    self.assertEqual(maintenance.next_version('v8.0.13', tags), '8.0.13-akalsi.3')
    self.assertEqual(maintenance.next_version('v8.0.14', tags), '8.0.14-akalsi.1')

  def test_rejects_injectable_or_unsupported_tags(self):
    for tag in ('main', 'v8.0.13;true', 'v8.0.13-beta.1', 'v8.0.13\n'):
      with self.subTest(tag=tag), self.assertRaises(ValueError):
        maintenance.next_version(tag, [])

  def test_latest_selection_rejects_drafts_and_prereleases(self):
    for tag, draft, pre in [('v9.0.0', True, False), ('v9.0.0', False, True), ('other', False, False)]:
      with self.subTest(tag=tag), self.assertRaises(ValueError):
        maintenance.stable_release({'tag_name': tag, 'draft': draft, 'prerelease': pre})
    release = {'tag_name': 'v8.0.13', 'draft': False, 'prerelease': False}
    self.assertIs(maintenance.stable_release(release), release)
    self.assertLess(maintenance.version_numbers('8.0.9'), maintenance.version_numbers('8.0.13'))

  def test_release_history_keeps_completed_releases_after_many_drafts(self):
    pages = [[{'draft': True}] * 100, [{'draft': False, 'tag_name': 'v8.0.13-akalsi.1'}]]
    with patch.object(maintenance, 'run', return_value=json.dumps(pages)) as run:
      releases = maintenance.release_history()
    self.assertEqual(len(releases), 101)
    self.assertFalse(releases[-1]['draft'])
    self.assertIn('--paginate', run.call_args.args)

  def test_unchanged_source_is_noop_even_after_many_drafts(self):
    from types import SimpleNamespace
    source = 'b' * 40
    upstream = 'a' * 40
    completed = {'draft': False, 'prerelease': False, 'tag_name': 'v8.0.13-akalsi.1',
      'assets': [{'name': 'provenance.json', 'id': 1}]}
    history = [{'draft': True, 'prerelease': False, 'tag_name': 'v8.0.13-akalsi.2'}] * 101 + [completed]
    def run(*argv):
      if argv[0] == 'gh':
        return json.dumps({'source_sha': source, 'upstream_main_sha': upstream, 'upstream_release_tag': 'v8.0.13'})
      if argv[1] in ('status', 'diff', 'fetch'):
        return ''
      return upstream if argv[-1] == 'FETCH_HEAD' else source
    def subprocess_run(argv, **kwargs):
      return SimpleNamespace(returncode=1 if 'MERGE_HEAD' in argv else 0, stdout='', stderr='')
    with patch.dict(os.environ, {'GITHUB_REPOSITORY': maintenance.REPOSITORY}), \
        patch.object(maintenance, 'run', side_effect=run), \
        patch.object(maintenance.subprocess, 'run', side_effect=subprocess_run), \
        patch.object(maintenance, 'api', return_value={'draft': False, 'prerelease': False, 'tag_name': 'v8.0.13'}), \
        patch.object(maintenance, 'release_history', return_value=history), \
        patch.object(maintenance, 'write_output') as output:
      maintenance.prepare()
    output.assert_called_once_with('changed', 'false')

  def test_workflow_change_requires_manual_integration(self):
    from types import SimpleNamespace
    def run(*argv):
      if argv[1] in ('status', 'fetch'):
        return ''
      if argv[1] == 'diff':
        return '.github/workflows/release.yaml'
      return 'a' * 40
    with patch.dict(os.environ, {'GITHUB_REPOSITORY': maintenance.REPOSITORY}), \
        patch.object(maintenance, 'run', side_effect=run), \
        patch.object(maintenance.subprocess, 'run', return_value=SimpleNamespace(returncode=1, stdout='', stderr='')), \
        patch.object(maintenance, 'api') as api:
      with self.assertRaisesRegex(ValueError, 'workflow files'):
        maintenance.prepare()
      api.assert_not_called()

  def test_conflict_stops_before_release_or_push(self):
    calls = []
    def run(*argv):
      calls.append(argv)
      return '' if argv[:3] == ('git', 'status', '--porcelain') else 'a' * 40
    with patch.dict(os.environ, {'GITHUB_REPOSITORY': maintenance.REPOSITORY}), \
        patch.object(maintenance, 'run', side_effect=run), \
        patch.object(maintenance, 'api') as api, \
        patch.object(maintenance.subprocess, 'run', side_effect=subprocess.CalledProcessError(1, ['git', 'merge'])):
      with self.assertRaisesRegex(ValueError, 'Merge stopped') as raised:
        maintenance.prepare()
      self.assertIsInstance(raised.exception.__cause__, subprocess.CalledProcessError)
      api.assert_not_called()
    self.assertFalse(any('push' in argv for argv in calls))

  def test_merge_explicitly_selects_ort_without_side_selection(self):
    result = subprocess.CompletedProcess([], 0, stdout='', stderr='')
    with patch.object(maintenance.subprocess, 'run', return_value=result) as merge:
      maintenance.merge_upstream('f' * 40, 'a' * 40)
    command = merge.call_args.args[0]
    self.assertIn('--strategy=ort', command)
    self.assertFalse(any('ours' in argument or 'theirs' in argument for argument in command))

  def test_merge_git_failure_without_conflicts_preserves_cause(self):
    error = subprocess.CalledProcessError(128, ['git', 'merge'],
      output='merge detail\n', stderr='fatal: missing object\n')
    with patch.object(maintenance.subprocess, 'run', side_effect=error), \
        patch.object(maintenance, 'run', return_value=''):
      with self.assertRaises(ValueError) as raised:
        maintenance.merge_upstream('f' * 40, 'a' * 40)
    self.assertIs(raised.exception.__cause__, error)
    diagnostic = str(raised.exception)
    for expected in ('without confirmed source conflicts', 'f' * 40, 'a' * 40,
        'exit status 128', 'merge detail\n', 'fatal: missing object\n', 'git status --short'):
      with self.subTest(expected=expected):
        self.assertIn(expected, diagnostic)
    self.assertNotIn('git add --', diagnostic)
    self.assertNotIn('git push', diagnostic)

  def test_pristine_guard_stops_before_fetch_or_merge(self):
    with patch.dict(os.environ, {'GITHUB_REPOSITORY': maintenance.REPOSITORY}), \
        patch.object(maintenance, 'run', return_value='?? unreviewed.txt') as run, \
        patch.object(maintenance.subprocess, 'run') as merge:
      with self.assertRaisesRegex(ValueError, 'tracked or untracked changes'):
        maintenance.prepare()
    run.assert_called_once_with('git', 'status', '--porcelain')
    merge.assert_not_called()

  def test_publish_rejects_changed_source_before_tagging(self):
    with tempfile.TemporaryDirectory() as directory:
      state = Path(directory) / 'candidate.json'
      state.write_text(json.dumps({'source_sha': 'a' * 40}))
      with patch.dict(os.environ, {'GITHUB_REPOSITORY': maintenance.REPOSITORY}), \
          patch.object(maintenance, 'STATE', state), \
          patch.object(maintenance, 'run', return_value='b' * 40) as run:
        with self.assertRaisesRegex(ValueError, 'source changed'):
          maintenance.publish()
        self.assertEqual(run.call_count, 1)

  def test_baseline_cannot_move_backward_for_a_backport(self):
    maintenance.require_forward_baseline('v8.0.14', 'v8.0.13')
    maintenance.require_forward_baseline('v8.0.13', 'v8.0.13')
    with self.assertRaisesRegex(ValueError, 'regressed'):
      maintenance.require_forward_baseline('v8.0.9', 'v8.0.13')

  def test_draft_is_exposed_only_after_all_assets_upload(self):
    from types import SimpleNamespace
    for fail_upload in (False, True):
      with self.subTest(fail_upload=fail_upload), tempfile.TemporaryDirectory() as directory:
        base = Path(directory)
        state = base / 'candidate.json'
        candidate = {'version': '8.0.13-akalsi.1', 'source_sha': 'a' * 40,
          'upstream_main_sha': 'b' * 40, 'upstream_release_tag': 'v8.0.13'}
        state.write_text(json.dumps(candidate))
        (base / 'archive').mkdir()
        (base / 'archive/cli-proxy-api').write_bytes(b'synthetic binary')
        (base / 'CLIProxyAPI_8.0.13-akalsi.1_linux_amd64.tar.gz').write_bytes(b'synthetic archive')
        calls = []
        def run(*argv):
          calls.append(argv)
          if argv[:3] == ('git', 'rev-parse', 'HEAD'):
            return candidate['source_sha']
          if fail_upload and argv[:3] == ('gh', 'release', 'create'):
            raise subprocess.CalledProcessError(1, argv)
          return ''
        with patch.dict(os.environ, {'GITHUB_REPOSITORY': maintenance.REPOSITORY}), \
            patch.object(maintenance, 'STATE', state), patch.object(maintenance, 'DIST', base), \
            patch.object(maintenance, 'run', side_effect=run), \
            patch.object(maintenance.subprocess, 'run', return_value=SimpleNamespace(
              stdout='CLIProxyAPI Version: 8.0.13-akalsi.1,', stderr='')) as banner:
          if fail_upload:
            with self.assertRaises(subprocess.CalledProcessError):
              maintenance.publish()
          else:
            maintenance.publish()
        self.assertEqual(set(banner.call_args.kwargs['env']), {'PATH', 'HOME', 'LANG'})
        self.assertEqual(banner.call_args.kwargs['cwd'], base)
        create = next(argv for argv in calls if argv[:3] == ('gh', 'release', 'create'))
        self.assertIn('--draft', create)
        for member in ('checksums.txt', 'release.env', 'provenance.json'):
          self.assertIn(str(base / member), create)
        edits = [argv for argv in calls if argv[:3] == ('gh', 'release', 'edit')]
        self.assertEqual(len(edits), 0 if fail_upload else 1)
        self.assertTrue(any('--atomic' in argv for argv in calls))
        self.assertFalse(any('--force' in argv or '--clobber' in argv for argv in calls))

  def test_owner_guard_runs_before_network_or_publication(self):
    with patch.dict(os.environ, {'GITHUB_REPOSITORY': maintenance.UPSTREAM}), \
        patch.object(maintenance, 'run') as run:
      for action in (maintenance.prepare, maintenance.publish):
        with self.assertRaises(ValueError):
          action()
      run.assert_not_called()


class GitIntegrationTests(unittest.TestCase):
  def setUp(self):
    self.directory = tempfile.TemporaryDirectory()
    self.addCleanup(self.directory.cleanup)
    self.root = Path(self.directory.name)
    self.state = self.root / '.local/candidate.json'
    self.output = self.root / '.local/output'
    self.environment = patch.dict(os.environ, {
      'GITHUB_REPOSITORY': maintenance.REPOSITORY, 'GITHUB_OUTPUT': str(self.output),
      'GIT_CONFIG_NOSYSTEM': '1', 'GIT_CONFIG_GLOBAL': os.devnull,
      'GIT_AUTHOR_NAME': 'Fixture', 'GIT_AUTHOR_EMAIL': 'fixture@example.invalid',
      'GIT_COMMITTER_NAME': 'Fixture', 'GIT_COMMITTER_EMAIL': 'fixture@example.invalid',
      'GIT_AUTHOR_DATE': '2026-01-01T00:00:00Z', 'GIT_COMMITTER_DATE': '2026-01-01T00:00:00Z',
    })
    self.environment.start()
    self.addCleanup(self.environment.stop)
    self.git('init', '--initial-branch=main')
    self.git('config', 'commit.gpgsign', 'false')
    self.git('config', 'core.hooksPath', os.devnull)
    (self.root / '.git/info/exclude').write_text('.local/\n')
    self.state.parent.mkdir()
    self.output.write_text('')
    (self.root / 'bridge.txt').write_text('base\n')
    self.git('add', '.')
    self.git('commit', '-m', 'base')
    self.git('branch', 'upstream')
    self.git('remote', 'add', 'origin', str(self.root))

  def git(self, *argv):
    return subprocess.check_output(['git', *argv], cwd=self.root, text=True,
      stderr=subprocess.STDOUT).strip()

  def diverge(self, conflict, bridge_path='bridge.txt'):
    if bridge_path != 'bridge.txt':
      self.git('mv', '--', 'bridge.txt', bridge_path)
      self.git('commit', '-m', 'rename bridge fixture')
      self.git('branch', '-f', 'upstream', 'HEAD')
    (self.root / bridge_path).write_text('fork\n')
    self.git('commit', '-am', 'fork bridge')
    fork_sha = self.git('rev-parse', 'HEAD')
    self.git('switch', 'upstream')
    target = bridge_path if conflict else 'upstream.txt'
    (self.root / target).write_text('upstream\n')
    self.git('add', '.')
    self.git('commit', '-m', 'upstream change')
    upstream_sha = self.git('rev-parse', 'HEAD')
    self.git('tag', 'v8.0.13')
    self.git('switch', 'main')
    return fork_sha, upstream_sha

  def prepare(self):
    self.calls = []
    original_run = maintenance.run
    def local_run(*argv):
      self.calls.append(argv)
      if argv[:3] == ('git', 'fetch', 'origin'):
        return original_run('git', 'fetch', str(self.root), 'main', '--tags')
      if argv[:3] == ('git', 'fetch', f'https://github.com/{maintenance.UPSTREAM}.git'):
        return original_run('git', 'fetch', str(self.root), 'upstream', '--tags')
      return original_run(*argv)
    self.stderr = io.StringIO()
    with patch.object(maintenance, 'ROOT', self.root), \
        patch.object(maintenance, 'STATE', self.state), \
        patch.object(maintenance, 'run', side_effect=local_run), \
        patch.object(maintenance, 'api', return_value={
          'draft': False, 'prerelease': False, 'tag_name': 'v8.0.13'}) as api, \
        patch.object(maintenance, 'release_history', return_value=[]), \
        patch.object(maintenance.sys, 'argv', ['fork-maintenance.py', 'prepare']), \
        redirect_stderr(self.stderr):
      result = maintenance.main()
    self.api_calls = api.call_count
    return result

  def test_real_conflict_reports_sources_paths_and_recovery_without_publication(self):
    fork_sha, upstream_sha = self.diverge(conflict=True)
    self.assertEqual(self.prepare(), 1)
    diagnostic = self.stderr.getvalue()
    for expected in ('source conflicts', fork_sha, upstream_sha, 'bridge.txt',
        'git switch -c', 'git merge --no-commit --no-ff', 'git add -- bridge.txt',
        './repo.sh check-pr', './repo.sh test', './repo.sh build',
        'gh workflow run fork-maintenance', 'CONFLICT (content)', 'exit status 1'):
      with self.subTest(expected=expected):
        self.assertIn(expected, diagnostic)
    self.assertEqual(self.git('diff', '--name-only', '--diff-filter=U'), 'bridge.txt')
    self.assertEqual(self.git('rev-parse', 'HEAD'), fork_sha)
    self.assertEqual(self.git('rev-parse', 'MERGE_HEAD'), upstream_sha)
    self.assertFalse(self.state.exists())
    self.assertEqual(self.output.read_text(), '')
    self.assertEqual(self.api_calls, 0)
    self.assertFalse(any(argv[0] == 'gh' or argv[1] in ('push', 'tag') for argv in self.calls))
    self.assertEqual(self.git('tag', '--list'), 'v8.0.13')

  def test_real_conflict_preserves_whitespace_and_shell_special_characters(self):
    bridge_path = ' bridge \' "$value";[x]*.txt \t\r\n'
    self.diverge(conflict=True, bridge_path=bridge_path)
    self.assertEqual(self.prepare(), 1)
    diagnostic = self.stderr.getvalue()
    add = 'git add -- ' + diagnostic.split('\ngit add -- ', 1)[1].split('\ngit commit', 1)[0]
    self.assertEqual(shlex.split(add), ['git', 'add', '--', bridge_path])
    self.assertIn(f'  {json.dumps(bridge_path)}\n', diagnostic)
    (self.root / bridge_path).write_text('resolved\n')
    subprocess.check_call(shlex.split(add), cwd=self.root)
    self.assertEqual(self.git('diff', '--name-only', '--diff-filter=U'), '')

  def test_recovery_targets_the_fork_without_assuming_remote_names(self):
    fork_url = f'https://github.com/{maintenance.REPOSITORY}.git'
    upstream_url = f'https://github.com/{maintenance.UPSTREAM}.git'
    self.git('remote', 'set-url', 'origin', upstream_url)
    self.git('remote', 'add', 'fork', fork_url)
    self.assertEqual(self.git('remote', 'get-url', 'origin'), upstream_url)
    self.assertEqual(self.git('remote', 'get-url', 'fork'), fork_url)
    self.diverge(conflict=True)
    self.assertEqual(self.prepare(), 1)
    diagnostic = self.stderr.getvalue()
    self.assertIn(f'git fetch {fork_url} main --tags', diagnostic)
    self.assertNotIn('git fetch origin', diagnostic)
    pushes = [shlex.split(line) for line in diagnostic.splitlines() if line.startswith('git push ')]
    self.assertEqual(pushes, [['git', 'push', fork_url, 'HEAD:refs/heads/main']])
    self.assertFalse(any(maintenance.UPSTREAM in argv[2] or argv[2] in ('origin', 'upstream')
      for argv in pushes))

  def test_real_independent_edits_in_one_file_merge_and_commit_cleanly(self):
    middle = ''.join(f'unchanged line {number}\n' for number in range(20))
    base = 'first\n' + middle + 'last\n'
    (self.root / 'bridge.txt').write_text(base)
    self.git('commit', '-am', 'shared baseline')
    self.git('branch', '-f', 'upstream', 'HEAD')
    (self.root / 'bridge.txt').write_text('fork first\n' + middle + 'last\n')
    self.git('commit', '-am', 'fork first line')
    fork_sha = self.git('rev-parse', 'HEAD')
    self.git('switch', 'upstream')
    (self.root / 'bridge.txt').write_text('first\n' + middle + 'upstream last\n')
    self.git('commit', '-am', 'upstream last line')
    upstream_sha = self.git('rev-parse', 'HEAD')
    self.git('tag', 'v8.0.13')
    self.git('switch', 'main')
    self.assertEqual(self.prepare(), 0, self.stderr.getvalue())
    self.assertEqual((self.root / 'bridge.txt').read_text(), 'fork first\n' + middle + 'upstream last\n')
    self.assertEqual(self.git('show', '-s', '--format=%P', 'HEAD'), f'{fork_sha} {upstream_sha}')
    self.assertEqual(self.git('status', '--porcelain'), '')
    self.assertEqual(json.loads(self.state.read_text())['upstream_main_sha'], upstream_sha)

  def test_real_upstream_only_file_keeps_both_parents_and_provenance_compatible(self):
    fork_sha, upstream_sha = self.diverge(conflict=False)
    self.assertEqual(self.prepare(), 0, self.stderr.getvalue())
    candidate = json.loads(self.state.read_text())
    source = self.git('rev-parse', 'HEAD')
    self.assertEqual(self.git('show', '-s', '--format=%P', 'HEAD'), f'{fork_sha} {upstream_sha}')
    self.assertEqual(candidate, {'schema_version': 1, 'version': '8.0.13-akalsi.1',
      'source_sha': source, 'upstream_main_sha': upstream_sha,
      'upstream_release_tag': 'v8.0.13', 'upstream_release_sha': upstream_sha,
      'patch_sha': maintenance.PATCH_SHA, 'go_version': maintenance.GO_VERSION, 'snapshot': True})
    self.assertEqual((self.root / 'bridge.txt').read_text(), 'fork\n')
    self.assertEqual((self.root / 'upstream.txt').read_text(), 'upstream\n')
    self.assertEqual(self.git('status', '--porcelain'), '')
    self.assertIn('changed=true\n', self.output.read_text())
    self.assertEqual(self.api_calls, 1)
    self.assertFalse(any(argv[0] == 'gh' or argv[1] == 'push' for argv in self.calls))


if __name__ == '__main__':
  unittest.main()
