# HTTP clients over WebSockets

The opt-in bridge accepts existing HTTP clients and reuses compatible upstream Codex WebSocket connections.
It does not require clients to implement WebSockets.
It applies only to requests resolved to the Codex executor.
A GPT model name alone does not select this transport.

## Enablement

The provider-wide `http-websockets` option defaults to false.
The selected credential must also enable its existing `websockets` option.
Credential selection and scheduling remain unchanged.

Legacy configuration:

```yaml
codex:
  http-websockets: true
```

Canonical version-eight configuration:

```yaml
upstream:
  codex:
    http-websockets: true
```

Loading either layout does not force configuration migration.
Installing a release does not enable this option automatically.
Disable it to retain the existing HTTP transport.
Production configuration changes and proxy restart require operator approval.

## Behavior

Messages, chat completions, and Responses clients retain their HTTP response formats.
Streaming clients receive their existing SSE translation, not raw WebSocket frames.
Nonstreaming clients receive the existing translated JSON response.
The bridge retains HTTP payload rules and independent full-request semantics.
It does not infer `previous_response_id` or reuse another request's conversation input.

Dedicated image endpoints, compact requests, Home execution lifecycles, and existing execution-session requirements retain their current path.
Response-header passthrough also retains HTTP when per-response header compatibility cannot be preserved.
Direct downstream WebSocket sessions remain separate.

## Ownership and limits

The HTTP bridge leases one socket exclusively for each active request.
It reserves up to 32 process-wide slots and eight slots per credential.
These are engineering limits, not claimed provider limits.
Reservations include sockets dialing, active, idle, and closing.
Busy capacity selects HTTP before sending an upstream WebSocket request.
There is no acquisition wait queue.
Idle retention lasts at most 60 seconds.
Idle cleanup does not impose a generation timeout.

Reuse requires compatible caller scope, credential epoch, endpoint, proxy, and final handshake headers.
Changed caller headers can prevent reuse.
Credential rotation retires incompatible resources without replaying active work.
Pool IDs identify transport resources only.
They do not become provider conversation identity or LCP affinity.

The HTTP-owned event queue retains at most one decoded event.
Each decoded message has a 50 MiB limit, matching the current HTTP streaming scanner limit.
Nonstreaming bridge requests also use this explicit limit.
The queue limit does not describe total process memory.
Reader, producer, slice growth, request preparation, and translation storage add memory separately.
For 32 slots, queued payload storage alone can reach 1,600 MiB at the maximum message size.
Measure typical allocation costs with the transport benchmark rather than inferring them from that worst case.

## Failures

Healthy terminal completion permits reuse after executor cleanup finishes.
Cancellation, malformed events, oversized messages, and uncertain termination discard the socket before capacity returns.
Lease generations prevent stale cancellation callbacks from affecting a new borrower.

An unsupported HTTP 426 upgrade can fall back before `response.create` is sent.
Authentication and quota failures retain their existing error policy.
They are not treated as unsupported transport.
After sending may have started execution, ambiguous failures cannot trigger automatic replay.
The guard applies to executor retries, credential/model failover, and frontend bootstrap retries.
A manual client retry can still repeat an upstream operation.

## Verification

Use the pinned repository toolchain:

```bash
./repo.sh test
./repo.sh build
./repo.sh go test -race ./internal/runtime/executor ./internal/runtime/executor/helps \
  ./sdk/cliproxy/executor ./sdk/cliproxy/auth ./sdk/api/handlers
```

[The benchmark document](CODEX-TRANSPORT-BENCHMARKS.md) separates synthetic transport costs from model-generation latency.
No Codex OAuth `stream_id` multiplexing capability is assumed.
