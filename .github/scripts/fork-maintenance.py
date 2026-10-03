#!/usr/bin/env python3
"""Synchronize and publish the akalsi-org fork without rewriting history."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys

REPOSITORY = 'akalsi-org/CLIProxyAPI'
UPSTREAM = 'router-for-me/CLIProxyAPI'
PATCH_SHA = 'c2f8accb748095a31745f4b86025a13199fd4d59'
GO_VERSION = '1.26.4'
RELEASE_TAG = re.compile(r'v([0-9]+\.[0-9]+\.[0-9]+)$')
FORK_TAG = re.compile(r'v([0-9]+\.[0-9]+\.[0-9]+)-akalsi\.([1-9][0-9]*)$')
ROOT = Path(__file__).resolve().parents[2]
STATE = ROOT / '.local/fork-candidate.json'
DIST = ROOT / '.local/fork-release'


def run(*argv):
  return subprocess.check_output(argv, cwd=ROOT, text=True).strip()


def api(path):
  return json.loads(run('gh', 'api', path))


def next_version(upstream_tag, tags):
  match = RELEASE_TAG.fullmatch(upstream_tag)
  if not match:
    raise ValueError('unsupported upstream release tag')
  base = match[1]
  revisions = [int(found[2]) for tag in tags if (found := FORK_TAG.fullmatch(tag)) and found[1] == base]
  return f'{base}-akalsi.{max(revisions, default=0) + 1}'


def stable_release(release):
  if release['draft'] or release['prerelease'] or not RELEASE_TAG.fullmatch(release['tag_name']):
    raise ValueError('upstream latest release is not a supported stable version')
  return release


def release_history():
  pages = json.loads(run('gh', 'api', '--paginate', '--slurp',
    f'repos/{REPOSITORY}/releases?per_page=100'))
  return [release for page in pages for release in page]


def version_numbers(value):
  return tuple(int(part) for part in value.split('.'))


def require_forward_baseline(upstream_tag, previous_tag):
  if version_numbers(upstream_tag[1:]) < version_numbers(previous_tag[1:]):
    raise ValueError('upstream release baseline regressed; manual review is required')


def prepare():
  if os.environ.get('GITHUB_REPOSITORY') != REPOSITORY:
    raise ValueError('maintenance only runs in akalsi-org/CLIProxyAPI GitHub Actions')
  if run('git', 'status', '--porcelain'):
    raise ValueError('candidate checkout has tracked or untracked changes')
  run('git', 'fetch', 'origin', 'main', '--tags')
  original_sha = run('git', 'rev-parse', 'HEAD')
  run('git', 'fetch', f'https://github.com/{UPSTREAM}.git', 'main', '--tags')
  upstream_sha = run('git', 'rev-parse', 'FETCH_HEAD')
  subprocess.run(['git', 'merge', '--no-commit', '--no-ff', upstream_sha], cwd=ROOT, check=True)
  merge_head = subprocess.run(['git', 'rev-parse', '-q', '--verify', 'MERGE_HEAD'],
    cwd=ROOT, capture_output=True)
  if merge_head.returncode == 0:
    run('git', 'commit', '-m', 'fork: sync upstream main',
      '-m', 'Co-Authored-By: Claude Code <noreply@anthropic.com>')
  workflow_changes = run('git', 'diff', '--name-only', original_sha, 'HEAD', '--', '.github/workflows')
  if workflow_changes:
    raise ValueError('upstream changes workflow files; integrate them manually with a workflows-authorized token')
  release = stable_release(api(f'repos/{UPSTREAM}/releases/latest'))
  upstream_tag = release['tag_name']
  upstream_release_sha = run('git', 'rev-parse', f'{upstream_tag}^{{commit}}')
  source_sha = run('git', 'rev-parse', 'HEAD')
  releases = release_history()
  for published in releases:
    if published['draft'] or published['prerelease'] or not FORK_TAG.fullmatch(published['tag_name']):
      continue
    asset = next((asset for asset in published['assets'] if asset['name'] == 'provenance.json'), None)
    if asset is None:
      continue
    previous = json.loads(run('gh', 'api', '-H', 'Accept: application/octet-stream',
      f'repos/{REPOSITORY}/releases/assets/{asset["id"]}'))
    require_forward_baseline(upstream_tag, previous['upstream_release_tag'])
    ancestry = subprocess.run(['git', 'merge-base', '--is-ancestor',
      previous['upstream_main_sha'], upstream_sha], cwd=ROOT)
    if ancestry.returncode != 0:
      raise ValueError('upstream main history changed; manual review is required')
    if previous['source_sha'] == source_sha and previous['upstream_release_tag'] == upstream_tag:
      print('Unchanged tested source: no release needed.')
      write_output('changed', 'false')
      return
    break
  tags = run('git', 'tag', '--list').splitlines()
  tags.extend(release['tag_name'] for release in releases)
  version = next_version(upstream_tag, tags)
  candidate = {'schema_version': 1, 'version': version, 'source_sha': source_sha,
    'upstream_main_sha': upstream_sha, 'upstream_release_tag': upstream_tag,
    'upstream_release_sha': upstream_release_sha, 'patch_sha': PATCH_SHA, 'go_version': GO_VERSION,
    'snapshot': source_sha != upstream_release_sha}
  STATE.parent.mkdir(parents=True, exist_ok=True)
  STATE.write_text(json.dumps(candidate, indent=2) + '\n')
  write_output('changed', 'true')
  write_output('version', version)
  write_output('source_sha', source_sha)
  print(f'Candidate {version}: upstream main {upstream_sha}, source {source_sha}')


def write_output(key, value):
  path = os.environ.get('GITHUB_OUTPUT')
  if path:
    with open(path, 'a') as handle:
      handle.write(f'{key}={value}\n')


def publish():
  if os.environ.get('GITHUB_REPOSITORY') != REPOSITORY:
    raise ValueError('publishing only runs in the owning fork')
  candidate = json.loads(STATE.read_text())
  if run('git', 'rev-parse', 'HEAD') != candidate['source_sha']:
    raise ValueError('candidate source changed after preparation')
  if run('git', 'status', '--porcelain'):
    raise ValueError('tracked or untracked source changed during testing')
  version = candidate['version']
  if not FORK_TAG.fullmatch('v' + version):
    raise ValueError('invalid candidate version')
  archive = DIST / f'CLIProxyAPI_{version}_linux_amd64.tar.gz'
  binary = DIST / 'archive/cli-proxy-api'
  archive_hash = hashlib.sha256(archive.read_bytes()).hexdigest()
  binary_hash = hashlib.sha256(binary.read_bytes()).hexdigest()
  banner = subprocess.run([str(binary), '-h'], capture_output=True, text=True, timeout=10, check=True,
    cwd=DIST, env={'PATH': os.environ.get('PATH', '/usr/bin:/bin'), 'HOME': str(DIST), 'LANG': 'C'})
  if f'CLIProxyAPI Version: {version},' not in banner.stdout + banner.stderr:
    raise ValueError('built binary has the wrong version')
  base_url = f'https://github.com/{REPOSITORY}/releases/download/v{version}'
  (DIST / 'checksums.txt').write_text(f'{archive_hash}  {archive.name}\n{binary_hash}  cli-proxy-api\n')
  (DIST / 'release.env').write_text(f'CLI_PROXY_API_VERSION={version}\n'
    f'CLI_PROXY_API_ARCHIVE_URL={base_url}/{archive.name}\n'
    f'CLI_PROXY_API_ARCHIVE_SHA256={archive_hash}\n'
    f'CLI_PROXY_API_BINARY_SHA256={binary_hash}\n'
    f'CLI_PROXY_API_VERSION_OUTPUT={version}\n')
  (DIST / 'provenance.json').write_text(json.dumps(candidate, indent=2) + '\n')
  tag = 'v' + version
  run('git', 'tag', '-a', tag, candidate['source_sha'], '-m', f'Tested fork snapshot {version}')
  # Atomic branch/tag publication fails if another writer changed main.
  helper = '!f() { if [ "$1" = get ]; then printf "username=x-access-token\\npassword=%s\\n" "$GH_TOKEN"; fi; }; f'
  run('git', '-c', f'credential.helper={helper}', 'push', '--atomic',
    'origin', 'HEAD:refs/heads/main', f'refs/tags/{tag}')
  notes = (f'Tested fork main snapshot based on upstream {candidate["upstream_release_tag"]}.\n\n'
    f'Upstream main: {candidate["upstream_main_sha"]}\nFork source: {candidate["source_sha"]}\n'
    f'Includes PR #6335: {PATCH_SHA}\n\n'
    'Linux amd64, native plugin support, GLIBC 2.17 ceiling. '
    'This is a main-branch snapshot, not an unchanged upstream release. See provenance.json.')
  assets = [str(archive), str(DIST / 'checksums.txt'), str(DIST / 'release.env'), str(DIST / 'provenance.json')]
  # Publish only after every required asset was uploaded to the draft.
  run('gh', 'release', 'create', tag, '--repo', REPOSITORY, '--verify-tag', '--draft',
    '--title', tag, '--notes', notes, *assets)
  run('gh', 'release', 'edit', tag, '--repo', REPOSITORY, '--draft=false', '--latest')
  print(f'Published {REPOSITORY} {tag}')


def main():
  parser = argparse.ArgumentParser(description=__doc__)
  parser.add_argument('action', choices=('prepare', 'publish'))
  args = parser.parse_args()
  try:
    prepare() if args.action == 'prepare' else publish()
  except (ValueError, OSError, KeyError, subprocess.CalledProcessError) as error:
    print(f'Fork maintenance failed: {error}. Inspect this workflow run before retrying.', file=sys.stderr)
    return 1
  return 0


if __name__ == '__main__':
  sys.exit(main())
