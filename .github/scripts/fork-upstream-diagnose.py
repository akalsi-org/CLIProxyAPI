#!/usr/bin/env python3
"""Compare failed candidate Go tests with the exact upstream commit, without publishing."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parents[2]
NOTICE = ('Baseline results are evidence, not permission to publish. '
  'Matching test identities do not prove identical failure causes.')


class Cancelled(Exception):
  pass


def cancel(signum, frame):
  raise Cancelled(f'cancelled by signal {signum}')


def expire(signum, frame):
  raise TimeoutError('diagnostic timeout')


def execute(command, cwd, directory, name, deadline, environment=None):
  (directory / f'{name}.command.json').write_text(json.dumps(command) + '\n')
  with (directory / f'{name}.stdout.jsonl').open('wb') as stdout, \
      (directory / f'{name}.stderr.log').open('wb') as stderr:
    remaining = deadline - time.monotonic()
    if remaining <= 0:
      raise TimeoutError('diagnostic timeout before command launch')
    process = subprocess.Popen(command, cwd=cwd, env=environment,
      stdout=stdout, stderr=stderr, start_new_session=True)
    try:
      code = process.wait(timeout=remaining)
    except subprocess.TimeoutExpired as error:
      raise TimeoutError('diagnostic timeout; command process group terminated') from error
    finally:
      try:
        os.killpg(process.pid, signal.SIGTERM)
        process.wait(timeout=2)
      except (ProcessLookupError, subprocess.TimeoutExpired):
        pass
      finally:
        try:
          os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
          pass
        process.wait()
      (directory / f'{name}.exit-code').write_text(f'{process.returncode}\n')
  return code


def events(path):
  result = []
  for line in path.read_text().splitlines():
    event = json.loads(line)
    if not isinstance(event, dict) or not isinstance(event.get('Action'), str):
      raise ValueError('malformed Go JSON event')
    result.append(event)
  if not result:
    raise ValueError('missing Go JSON events')
  return result


def module_path(tree):
  match = re.search(r'^module\s+"?([^"\s]+)"?\s*$', (tree / 'go.mod').read_text(), re.MULTILINE)
  if not match:
    raise ValueError('missing module declaration')
  return match[1]


def failures(recorded, module):
  packages = {}
  for event in recorded:
    if event['Action'] not in ('fail', 'build-fail'):
      continue
    package = event.get('Package', event.get('ImportPath', ''))
    if not isinstance(package, str) or not (package == module or package.startswith(module + '/')):
      raise ValueError('failed package is outside the candidate module')
    suffix = package[len(module) + 1:] if package != module else ''
    if package != module and (not suffix or any(
        not part or part in ('.', '..') or not re.fullmatch(r'[\w.~-]+', part)
        for part in suffix.split('/'))):
      raise ValueError('invalid failed package path within module')
    tests = packages.setdefault(package, set())
    if 'Test' in event:
      if not isinstance(event['Test'], str) or not event['Test']:
        raise ValueError('invalid failed test identity')
      tests.add(event['Test'])
  if not packages:
    raise ValueError('candidate report contains no failed Go package or test')
  return packages


def classify(recorded, package, tests, code):
  if code < 0:
    return 'inconclusive', 'upstream command terminated by a signal'
  relevant = [event for event in recorded if event.get('Package') == package]
  if any(event.get('FailedBuild') or event['Action'] == 'build-fail' for event in recorded):
    return 'inconclusive', 'upstream build/setup failure'
  failed = {event.get('Test') for event in relevant if event['Action'] == 'fail' and event.get('Test')}
  passed = {event.get('Test') for event in relevant if event['Action'] == 'pass' and event.get('Test')}
  package_passed = any(event['Action'] == 'pass' and 'Test' not in event for event in relevant)
  if tests:
    if tests <= failed:
      return 'observed_upstream_failure', 'the same top-level test identities failed upstream'
    if code == 0 and package_passed and tests <= passed:
      return 'passes_upstream', 'candidate failures pass upstream; possible fork integration regression'
    return 'inconclusive', 'missing test results, mixed outcomes, or upstream build/setup failure'
  if failed:
    return 'observed_upstream_failure', 'the same package failed upstream; test causes can differ'
  if code == 0 and package_passed and passed:
    return 'passes_upstream', 'the package passes upstream; possible fork integration regression'
  return 'inconclusive', 'missing tests/package or upstream build/setup failure'


def diagnose(report, upstream_sha, timeout):
  base = ROOT / '.local/fork-diagnostics'
  base.mkdir(parents=True, exist_ok=True, mode=0o700)
  directory = Path(tempfile.mkdtemp(prefix='upstream-', dir=base))
  summary = {'status': 'inconclusive', 'notice': NOTICE, 'comparisons': [],
    'candidate_report': str(report), 'source_sha': None, 'upstream_main_sha': upstream_sha}
  deadline = time.monotonic() + timeout
  signal.signal(signal.SIGALRM, expire)
  signal.setitimer(signal.ITIMER_REAL, timeout)
  try:
    git_environment = {key: os.environ[key] for key in ('PATH', 'LANG', 'LC_ALL') if key in os.environ}
    git_environment.update(GIT_CONFIG_NOSYSTEM='1', GIT_CONFIG_GLOBAL=os.devnull,
      GIT_NO_REPLACE_OBJECTS='1', HOME=str(directory))
    state = {} if upstream_sha else json.loads((ROOT / '.local/fork-candidate.json').read_text())
    upstream_sha = upstream_sha or state['upstream_main_sha']
    summary['upstream_main_sha'] = upstream_sha
    if not isinstance(upstream_sha, str) or not re.fullmatch(r'[0-9a-f]{40}|[0-9a-f]{64}', upstream_sha):
      raise ValueError('upstream identity must be an exact commit SHA')
    for name, revision in [('source', 'HEAD'), ('upstream', upstream_sha + '^{commit}')]:
      code = execute(['git', 'rev-parse', '--verify', revision], ROOT, directory, name, deadline, git_environment)
      if code:
        raise ValueError(f'{name} commit verification failed; inspect {name}.stderr.log')
      resolved = (directory / f'{name}.stdout.jsonl').read_text().strip()
      if name == 'source':
        summary['source_sha'] = resolved
        if state.get('source_sha') != resolved and state:
          raise ValueError('candidate source changed since preparation')
      elif resolved != upstream_sha:
        raise ValueError('resolved upstream commit differs from the requested SHA')
    data = report.read_bytes()
    summary['candidate_report_sha256'] = hashlib.sha256(data).hexdigest()
    (directory / 'candidate.jsonl').write_bytes(data)
    module = module_path(ROOT)
    packages = failures(events(directory / 'candidate.jsonl'), module)
    with tempfile.TemporaryDirectory(prefix='tree-', dir=directory) as temporary:
      tree = Path(temporary) / 'checkout'
      code = execute(['git', '-c', 'core.hooksPath=' + os.devnull, '-c', 'init.templateDir=',
        'clone', '--shared', '--no-checkout', '--no-tags', '--config', 'core.hooksPath=' + os.devnull,
        '--', str(ROOT), str(tree)], ROOT, directory, 'clone', deadline, git_environment)
      if code:
        raise ValueError('isolated clone failed; inspect clone.stderr.log')
      code = execute(['git', '-c', 'core.hooksPath=' + os.devnull, 'checkout', '--detach', upstream_sha],
        tree, directory, 'checkout', deadline, git_environment)
      if code:
        raise ValueError('upstream checkout failed; inspect checkout.stderr.log')
      code = execute(['git', 'remote', 'remove', 'origin'], tree, directory,
        'remove-source-remote', deadline, git_environment)
      if code:
        raise ValueError('isolated remote cleanup failed; inspect remove-source-remote.stderr.log')
      if module_path(tree) != module:
        raise ValueError('upstream module differs from the candidate module')
      home = tree / '.diagnostic-home'
      home.mkdir()
      environment = {key: os.environ[key] for key in
        ('PATH', 'LANG', 'LC_ALL', 'CC', 'CXX', 'CGO_ENABLED', 'GOMAXPROCS') if key in os.environ}
      environment.update(GIT_CONFIG_NOSYSTEM='1', GIT_CONFIG_GLOBAL=os.devnull, GIT_NO_REPLACE_OBJECTS='1',
        HOME=str(home), TMPDIR=str(home),
        GOTOOLCHAIN='local', GOWORK='off', GOPROXY='off', GOSUMDB='off', GOFLAGS='',
        GOPATH=str(ROOT / '.local/cache/gopath'), GOMODCACHE=str(ROOT / '.local/cache/modules'),
        GOCACHE=str(ROOT / '.local/cache/go-build'))
      go = ROOT / '.local/toolchains/go/bin/go'
      for index, (package, tests) in enumerate(sorted(packages.items())):
        name = f'package-{index}'
        relative = package[len(module):].lstrip('/')
        command = [str(go), 'test', '-mod=readonly', '-count=1', '-json', '-buildvcs=false']
        if tests:
          top_levels = sorted({test.split('/')[0] for test in tests})
          command += ['-run', '^(' + '|'.join(re.escape(test) for test in top_levels) + ')$']
        command.append('./' + relative if relative else '.')
        comparison = {'package': package, 'tests': sorted(tests), 'status': 'inconclusive',
          'reason': 'comparison did not complete', 'stdout': str(directory / f'{name}.stdout.jsonl'),
          'stderr': str(directory / f'{name}.stderr.log')}
        summary['comparisons'].append(comparison)
        code = execute(command, tree, directory, name, deadline, environment)
        comparison['exit_code'] = code
        comparison['status'], comparison['reason'] = classify(
          events(directory / f'{name}.stdout.jsonl'), package, tests, code)
    summary['status'] = ('complete' if all(item['status'] != 'inconclusive'
      for item in summary['comparisons']) else 'inconclusive')
  except (ValueError, OSError, KeyError, TypeError, TimeoutError,
      KeyboardInterrupt, Cancelled) as error:
    summary['reason'] = str(error) or 'cancelled'
  finally:
    signal.setitimer(signal.ITIMER_REAL, 0)
  (directory / 'summary.json').write_text(json.dumps(summary, indent=2) + '\n')
  print(f'Upstream comparison {summary["status"]}: {directory / "summary.json"}')
  print(NOTICE)
  return 0 if summary['status'] == 'complete' else 1


def main():
  parser = argparse.ArgumentParser(description=__doc__)
  parser.add_argument('report', type=Path)
  parser.add_argument('--upstream-sha')
  parser.add_argument('--timeout-seconds', type=float, default=600)
  args = parser.parse_args()
  if not 0 < args.timeout_seconds <= 1200:
    parser.error('timeout must be greater than zero and at most 1200 seconds')
  signal.signal(signal.SIGTERM, cancel)
  signal.signal(signal.SIGINT, cancel)
  return diagnose(args.report.resolve(), args.upstream_sha, args.timeout_seconds)


if __name__ == '__main__':
  sys.exit(main())
