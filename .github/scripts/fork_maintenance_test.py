"""Deterministic fork release selection and publication failure checks."""
import importlib.util
import json
import os
from pathlib import Path
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
      return SimpleNamespace(returncode=1 if 'MERGE_HEAD' in argv else 0)
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
        patch.object(maintenance.subprocess, 'run', return_value=SimpleNamespace(returncode=1)), \
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
      with self.assertRaises(subprocess.CalledProcessError):
        maintenance.prepare()
      api.assert_not_called()
    self.assertFalse(any('push' in argv for argv in calls))

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
              stdout='CLIProxyAPI Version: 8.0.13-akalsi.1,', stderr='')):
          if fail_upload:
            with self.assertRaises(subprocess.CalledProcessError):
              maintenance.publish()
          else:
            maintenance.publish()
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


if __name__ == '__main__':
  unittest.main()
