# Codex transport benchmarks

This document owns the fork's HTTP-client transport measurements.
Synthetic transport results do not establish provider-generation latency or production throughput.

## Reproduce

```bash
./repo.sh setup
GODEBUG=x509usefallbackroots=1 ./repo.sh go test -tags forkbench ./internal/runtime/executor \
  -run '^$' -bench BenchmarkCodexHTTPTransportMatrix -benchtime=100x -count=3 -timeout=20m
```

The `forkbench` build tag excludes the fixture and its private certificate authority from ordinary tests and builds.
The benchmark verifies certificates against its private authority.
It does not disable TLS verification or contact a provider.
A loopback CONNECT proxy directs the synthetic `chatgpt.com` hostname exclusively to the test TLS server.
All transport variants use that same proxy hop and server.

## Workloads

Requests contain generated input of 1 KiB, 64 KiB, 1 MiB, or 8 MiB.
Both HTTP and WebSocket receivers stream-drain complete uploads before emitting identical response events and usage.
Neither receiver materializes the entire upload merely to discard it.
The server performs no inference and introduces no artificial generation delay.
The client uses the ordinary HTTP streaming conversion path, not the downstream WebSocket path.
Logging remains at warning level for every variant.
Ordinary HTTP request identity is enriched and cached before executor entry, matching handler and conductor behavior.
Every variant receives the same ordinary request, options, and identity context.
Identity derivation has a separate setup measurement; it is not claimed as a bridge-specific saving.
No execution-session identity or downstream WebSocket flag is injected.

| Variant | Mechanism |
| --- | --- |
| `utls-http` | Current protected-host HTTP executor; dedicated TCP/TLS connection per request |
| `native-http` | Shared native HTTP transport with verified TLS and connection reuse |
| `ephemeral-ws` | Existing WebSocket executor without persistent execution-session identity |
| `pooled-ws` | Opt-in HTTP bridge with exclusively leased persistent upstream sockets |

Each scenario measures one cold request separately.
A response barrier then warms the requested concurrent lanes before timed operations.
Timed work uses concurrency levels one, four, eight, and sixteen.
Stable-header scenarios permit compatible socket reuse.
Changing-header scenarios vary `X-Client-Request-Id` for every request.
The bridge must preserve those header differences, even when reuse becomes impossible.

## Metrics and scope

- `ns/op` measures aggregate elapsed time divided by successful requests.
- `requests/s` measures aggregate throughput, not inverse individual request latency under concurrency.
- `ttfe-p50-ns` and `ttfe-p95-ns` measure time to first generated text delta.
- `complete-p50-ns` and `complete-p95-ns` measure request completion latency.
- `cold-ttfe-ns` and `cold-complete-ns` measure one initial request, outside the warm timing loop.
- `tcp/op`, `tls/op`, and `upgrade/op` count new upstream connections, handshakes, and upgrades.
- `wire-B/op` counts encrypted bytes at the test server, excluding the proxy's separate CONNECT headers.
- `peak-conns` measures live upstream TLS connections during the warm workload, including retained idle sockets.
- Go's `B/op` and `allocs/op` include the executor, fixture server, and fixture proxy.
- `identity-setup-ns/op` measures ordinary identity preparation separately, before executor timing.

The fixture uses a fixed response sequence and synthetic credentials.
It does not read live configuration, provider OAuth records, client tokens, or user prompts.
No socket multiplexing capability is assumed for the Codex OAuth backend.
One leased WebSocket runs one request at a time.

Connection reuse counts and resource bounds are deterministic correctness gates.
Timing results remain measurements, not CI timing thresholds.

## Recorded results

Measured on 2026-10-03 with Go 1.26.4, Linux 7.2.5-3-omarchy, and an AMD Ryzen AI 9 HX 370.
The machine has 24 logical CPUs; the repository sets `GOMAXPROCS=4`.
The complete matrix measured 128 scenarios, each with 25 operations and three trials.
No CPU affinity, frequency control, or scheduling isolation was applied.
Source fingerprint, including Go sources, tests, go.mod, and go.sum:
`25323770cc31380dd8277c58d2dfcc9985efb5f0b7b7e083d4824f14ef73f55e`.

The following values are medians of trial elapsed time per request, in milliseconds.
They cover serial, stable-header, warmed connections.

| Generated input | Current uTLS HTTP | Reused native HTTP | Ephemeral WebSocket | Pooled WebSocket |
| --- | ---: | ---: | ---: | ---: |
| 1 KiB | 0.824 | 0.138 | 0.684 | 0.108 |
| 64 KiB | 2.146 | 1.456 | 1.927 | 1.423 |
| 1 MiB | 19.996 | 19.431 | 19.690 | 21.614 |
| 8 MiB | 147.614 | 144.768 | 141.700 | 154.513 |

Pooled WebSockets reduce small-request overhead substantially compared with the current per-request TLS path.
For 1 MiB and 8 MiB inputs, this loopback test still shows additional CPU and elapsed-time cost.
Do not describe the bridge as universally faster.
The opt-in remains disabled by default.

Warm stable-header HTTP and pooled WebSockets require no new timed TLS handshakes in the serial cases.
Current uTLS HTTP and ephemeral WebSockets require one per request.
Pooled allocation medians are approximately 0.05, 1.01, 14.17, and 112.18 MiB per operation across these sizes.
Current uTLS allocation medians are approximately 0.28, 1.56, 17.64, and 137.76 MiB.
These allocation totals include the synthetic server and proxy, not only the client executor.

For 1 KiB inputs with eight concurrent stable-header clients:

| Transport | Median requests/second |
| --- | ---: |
| Current uTLS HTTP | 3784 |
| Reused native HTTP | 20072 |
| Ephemeral WebSocket | 5003 |
| Pooled WebSocket | 28366 |

At concurrency sixteen, the eight-per-credential socket limit causes some requests to retain HTTP.
The pooled median falls to 9906 requests/second, versus 21460 for reused native HTTP.
This demonstrates a capacity tradeoff, not a hidden queue or an unlimited throughput claim.

Changing `X-Client-Request-Id` prevents handshake-compatible socket reuse.
Those cases establish approximately one new TLS connection per request.
Reused native HTTP remains substantially faster for that synthetic changing-header workload.
The bridge does not remove those headers to improve its measurements.

Ordinary identity derivation is measured separately.
Its cost grows with input size and applies to both transport paths before executor entry.
The benchmark excludes it from the warmed executor elapsed time; it is not an end-to-end client latency measurement.

The local test does not include provider inference, internet RTT, or provider-side prompt-cache behavior.
Avoiding repeated remote handshakes may help production latency, but these tests do not measure that benefit.
No live provider benchmark or production enablement occurred.

## Evidence and earlier experiments

Complete final measurements: `/tmp/codex-http-ws-final-corrected-matrix.log`.
The report retains individual trials: `/tmp/codex-http-ws-final-corrected-matrix.json`.

```bash
./repo.sh python3 .github/scripts/codex-transport-report.py /tmp/codex-http-ws-final-corrected-matrix.log
```

Earlier experiments used asymmetric upload receivers and lacked the normal cached HTTP identity context.
Their results remain preserved locally but are excluded from this recorded comparison.
Profile-guided changes removed absent-field deletion copies and repeated input-item sanitization.
HTTP-owned dials use a 32 KiB write buffer, adding at most 896 KiB across 32 slots.
Native WebSocket buffer defaults and shared request validation remain unchanged.
