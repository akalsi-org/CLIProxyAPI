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

  def test_stable_selection_skips_drafts_and_prereleases(self):
    releases = [{'tag_name': tag, 'draft': draft, 'prerelease': pre} for tag, draft, pre in
      [('v9.0.0', True, False), ('v9.0.0', False, True), ('other', False, False), ('v8.0.13', False, False)]]
    self.assertEqual(maintenance.stable_release(releases)['tag_name'], 'v8.0.13')

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

  def test_owner_guard_runs_before_network_or_publication(self):
    with patch.dict(os.environ, {'GITHUB_REPOSITORY': maintenance.UPSTREAM}), \
        patch.object(maintenance, 'run') as run:
      for action in (maintenance.prepare, maintenance.publish):
        with self.assertRaises(ValueError):
          action()
      run.assert_not_called()


if __name__ == '__main__':
  unittest.main()
