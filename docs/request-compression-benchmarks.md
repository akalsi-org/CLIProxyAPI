# Request compression benchmarks

These local measurements cover transport compression, not model inference or production network latency.
They use deterministic fixtures, not user conversations or credentials.

## Environment

- Date: 2026-10-24 UTC.
- CPU: AMD Ryzen AI 9 HX 370 with Radeon 890M; 24 logical CPUs.
- Platform: Linux x86_64, kernel `7.2.5-3-omarchy`.
- Toolchain: Go `1.26.10`, through `repo.sh`, with `GOMAXPROCS=4`.
- Conditions: three warm-process benchmark runs; other repository checks could run concurrently.

```bash
./repo.sh go test ./internal/runtime/executor/helps \
  -run '^$' -bench '^BenchmarkRequestCompression' \
  -benchmem -benchtime=300ms -count=3

./repo.sh go test ./sdk/api/handlers \
  -run '^$' -bench '^BenchmarkReadRequestBodyIdentity$' \
  -benchmem -count=3
```

## Codec and policy measurements

The history fixture contains 194,059 bytes of repetitive synthetic JSON.
The attachment fixture contains 174,781 bytes of JSON with deterministic, seeded base64 data.
Neither fixture establishes a production compression ratio.

| Fixture | Codec | Plain bytes | Sent bytes | Time range | Allocated bytes/op | Allocations/op |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| History | gzip `BestSpeed` | 194,059 | 1,944 | 226.6–231.9 µs | 3,952 | 6 |
| History | zstd `SpeedFastest` | 194,059 | 648 | 74.2–75.9 µs | 4,096 | 1 |
| Attachment | gzip `BestSpeed` | 174,781 | 131,505 | 760.0–777.0 µs | 524,201–526,794 | 13 |
| Attachment | zstd `SpeedFastest` | 174,781 | 174,781 | 66.8–74.8 µs | 413,711–420,116 | 4 |

The zstd attachment result fails the savings threshold, so the request keeps its original bytes.
That case still pays the compression-attempt cost.
Encoders reuse workspaces; request-sized output buffers do not enter a pool.

## Actual attachment and transport boundary

These measurements include `WithRequestCompression` and a fake transport, without network calls or inference.
The transport closes the transmitted body and returns an empty successful response.

| Path | Time range | Allocated bytes/op | Allocations/op |
| --- | ---: | ---: | ---: |
| gzip history, attachment and send | 228.1–229.8 µs | 5,816–5,827 | 17 |
| zstd history, attachment and send | 75.6–80.0 µs | 5,184–9,583 | 12 |
| Small-body bypass, attachment only | 5.6–6.4 ns | 0 | 0 |
| Custom-endpoint bypass, attachment only | 164.1–166.1 ns | 0 | 0 |
| Small-body bypass, attachment and send | 14.1–14.4 ns | 0 | 0 |
| Custom-endpoint bypass, attachment and send | 172.0–177.3 ns | 0 | 0 |

Tests require zero attachment allocations for small bodies and unsupported endpoints.
Compression remains below usage tracking, so rejected attempts do not determine successful-response token timing.

## Existing identity decoder

The identity benchmark reuses a constructed request and measures reader reset plus decoding.
The production identity path remains unchanged.

| Source | Time range | Allocated bytes/op | Allocations/op |
| --- | ---: | ---: | ---: |
| Original ingress | 2.543–2.595 µs | 11,136 | 9 |
| Updated ingress | 2.580–2.747 µs | 11,136 | 9 |

Allocations remain equal.
The timing ranges overlap, but these runs do not establish equal latency under isolated conditions.
No network or model speedup follows from these measurements.

## Local evidence

Complete output remains under `.local/fork-diagnostics/`, outside version control:

- `request-compression-benchmark-final.log`
- `compression-ingress/compression-ingress-bench-before.log`
- `compression-ingress/compression-ingress-bench-after.log`

The benchmark fixtures and commands remain in the test sources.
See [the compression contract](../FORK.md#http-request-compression) for endpoints, thresholds, fallback rules, and compatibility limits.
