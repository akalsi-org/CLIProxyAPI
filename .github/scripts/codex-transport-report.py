#!/usr/bin/env python3
"""Aggregate the forkbench transport matrix without discarding trial evidence."""

import argparse
import json
from pathlib import Path
import re
import statistics
import sys

ROW = re.compile(r'^BenchmarkCodexHTTPTransportMatrix/bytes=(\d+)/c=(\d+)/headers=(stable|changing)/(utls-http|native-http|ephemeral-ws|pooled-ws)-\d+\s+(\d+)\s+(.*)$')


def report(text):
  if re.search(r'^FAIL(?:\s|$)|^--- FAIL:', text, re.MULTILINE):
    raise ValueError('benchmark failed; inspect the complete input log')
  if not re.search(r'^PASS$', text, re.MULTILINE):
    raise ValueError('benchmark has not completed successfully; wait for its completion')
  groups = {}
  for line in text.splitlines():
    match = ROW.match(line)
    if match is None:
      continue
    size, concurrency, headers, mode, iterations, values = match.groups()
    fields = values.split()
    if len(fields) % 2:
      raise ValueError('incomplete benchmark row; preserve the original output')
    metrics = {fields[index + 1]: float(fields[index]) for index in range(0, len(fields), 2)}
    key = (int(size), int(concurrency), headers, mode)
    groups.setdefault(key, []).append({'iterations': int(iterations), 'metrics': metrics})
  if not groups:
    raise ValueError('no transport matrix rows found; run the documented forkbench command')
  rows = []
  for key, trials in sorted(groups.items()):
    units = set(trials[0]['metrics'])
    if any(set(trial['metrics']) != units for trial in trials):
      raise ValueError('trial metrics differ; do not combine different benchmark versions')
    rows.append({'input_bytes': key[0], 'concurrency': key[1], 'headers': key[2], 'mode': key[3],
      'summary': {unit: {'median': statistics.median(trial['metrics'][unit] for trial in trials),
        'min': min(trial['metrics'][unit] for trial in trials),
        'max': max(trial['metrics'][unit] for trial in trials)} for unit in sorted(units)},
      'trials': trials})
  return {'method': 'Medians and trial ranges; latency percentiles remain per-trial observations',
    'scenario_count': len(rows), 'trial_count': sum(len(row['trials']) for row in rows), 'scenarios': rows}


def main():
  parser = argparse.ArgumentParser(description=__doc__)
  parser.add_argument('log', type=Path)
  args = parser.parse_args()
  try:
    data = report(args.log.read_text())
  except (OSError, ValueError) as error:
    print(f'Transport report failed: {error}. Read {args.log} before retrying.', file=sys.stderr)
    return 1
  data['complete_input_log'] = str(args.log.resolve())
  print(json.dumps(data, indent=2))
  return 0


if __name__ == '__main__':
  sys.exit(main())
