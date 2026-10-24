# Fork maintenance

This fork follows `router-for-me/CLIProxyAPI` main and publishes tested Linux amd64 snapshots.
It preserves upstream history and the upstream Go module path.
It includes PR #6335's Antigravity quota cooldown, except that a bare `RESOURCE_EXHAUSTED` uses the quota-aware model backoff in `ANTIGRAVITY-COOLING.md`.
The original patch commit is `c2f8accb748095a31745f4b86025a13199fd4d59`.
The obsolete fork-only provider overlay is retired; upstream provider implementations remain upstream-owned.

## Owners

The `fork-maintenance` GitHub Actions workflow owns synchronization and releases.
It runs hourly at minute 17 UTC and supports manual dispatch.
GitHub can delay scheduled runs; this schedule is not an execution-time guarantee.
Workflow concurrency serializes runs without canceling an active publication.
The upstream release, Docker, and automatic PR-retarget workflows are disabled through fork repository settings.
They remain disabled when their source files receive upstream changes.

A separate my-init `timerctl` job follows completed public releases.
It updates `config/cli-proxy-api/release.env` without committing, installing, or restarting services.
Neither normal my-init installation nor pin following requires GitHub credentials.

## Publication contract

A candidate merges upstream main without a force push, using Git's explicit `ort` three-way strategy.
Git accepts upstream-only files and independent edits within shared files automatically.
The existing hourly workflow remains the only upstream synchronization scheduler.
It runs `./repo.sh test-maintenance` before preparation, without Go setup or network access.
Overlapping changes still require review; maintenance never applies file allowlists or automatic `ours`/`theirs` selection.
Merge conflicts, upstream history rewrites, failed tests, and build failures stop publication.
GitHub's repository `GITHUB_TOKEN` cannot push workflow-file changes.
The `FORK_MAINTENANCE_TOKEN` repository secret holds a token with contents and workflows write access.
If that secret is set, upstream workflow-file changes merge like other changes, and publication pushes with it.
If that secret is absent, workflow-file changes stop synchronization, and the error says to set it.
In that case, integrate the changes locally with authorized SSH access, then dispatch maintenance again.
A token push or tag starts workflows that a `GITHUB_TOKEN` push cannot start.
Before a token publication, preparation reads each workflow's state through the Actions API.
It stops if an active workflow other than `fork-maintenance` runs on `push`, `create`, `release`, or `workflow_run`.
A workflow file that is not yet on main counts as active, because GitHub enables it when the push adds it.
Disable such a workflow in the fork Actions settings, then dispatch maintenance again.
`fork-maintenance` runs only on its schedule and manual dispatch, so its own push cannot start it again.
No upstream workflow changes are silently discarded.
The workflow runs the Antigravity regressions, all Go tests, and a server compile check.
An unchanged tested source and upstream release baseline produce no release.
A new candidate uses the latest stable upstream version plus `-akalsi.N`.
For example, `8.0.13-akalsi.1` is a fork snapshot, not an unchanged upstream release.

The Go 1.26.10 archive has a pinned SHA256.
A pinned manylinux2014 container builds plugin-capable Linux amd64 with a GLIBC 2.17 ceiling.
Builds use committed catalogs; they do not regenerate catalogs from moving remote sources.

Every completed release includes:

- `CLIProxyAPI_<version>_linux_amd64.tar.gz`
- `checksums.txt`, containing archive and binary hashes
- `release.env`, containing the five my-init installation fields
- `provenance.json`, containing exact fork, upstream, patch, and toolchain identities

The archive contains exactly `cli-proxy-api`, `LICENSE`, `README.md`, `README_CN.md`, and `config.example.yaml`.
A draft receives all assets before becoming public.
An atomic git push publishes the tested main commit and a new immutable tag.
Published tags and assets are never overwritten.
Interrupted publication can leave an orphan tag or draft; a subsequent run reserves a new revision.
Inspect the failed run before deleting anything.

## HTTP WebSocket bridge compatibility

`upstream.codex.http-websockets` enables pooled upstream WebSockets for eligible HTTP requests with WebSocket-enabled credentials.
Bridge requests use payload rules for the `codex` protocol.
Final payload rules must not add `previous_response_id`, `generate`, `prompt_cache_retention`, `safety_identifier`, or `stream_options`.
The final `stream` value must remain `true`; rules must not remove it or set another value.
Incompatible configured payloads return HTTP 400 before pool acquisition or any upstream send.
The bridge no longer silently removes or overwrites these configured values.
Ordinary WebSocket requests without the HTTP bridge retain their existing behavior.

## HTTP request compression

Claude and OpenAI HTTP handlers accept plain JSON, gzip, and zstd request bodies.
The configured request limit applies to encoded bodies and each decoded layer.
Malformed compressed bodies return HTTP 400; bodies above the configured limit return HTTP 413.
Existing plain JSON clients remain compatible.
Encoded-byte limits still belong to server middleware; standalone SDK users must enforce equivalent limits.
Decoded-byte limits do not change the existing zstd decoder window settings.
Request logging remains unchanged; stored gzip request bodies retain their compressed wire representation.

The proxy compresses finalized upstream JSON only at these verified HTTPS endpoints:

| Endpoint | Request encoding |
| --- | --- |
| `api.anthropic.com/v1/messages` | gzip, fastest level |
| `chatgpt.com/backend-api/codex/responses` | zstd, fastest level |
| `chatgpt.com/backend-api/codex/v1/responses` | zstd, fastest level |

Compression starts at 32 KiB and must save both 1 KiB and 5% of the original body.
Small bodies, existing content encodings, custom gateways, and other endpoints retain their existing behavior.
Token-count, compact, and WebSocket requests do not gain upstream request compression.
OpenAI-compatible API-key endpoints remain uncompressed because their request-encoding support is not established here.

Compression follows payload rules and never changes JSON semantics or prompt-cache prefixes.
Request lengths and replay bodies describe the actual transmitted bytes.
A compressed request can retry once without compression after an explicit HTTP 415 encoding rejection.
Other errors, transport failures, and successful responses never trigger this compression fallback.
Cancellation stops fallback; the helper never retries a consumed response stream.
There is no capability cache, background probe, or new configuration flag.
Existing response decompression and SSE streaming remain unchanged.

Fork-owned codec and fallback policy lives under `internal/runtime/executor/helps/`.
Provider executors attach the wrapper before usage tracking; compression and its fallback policy stay in the helper.
The wrapper returns only the final response to usage tracking, so rejected attempts do not determine token timing.
Claude ingress reuses the shared request decoder instead of adding another middleware layer.
Keep these integration points narrow during upstream merges.
When upstream supplies equivalent behavior, remove the redundant fork policy after running the compatibility tests.

Compression reduces transport bytes, not model tokens or token charges.
Local compression benchmarks do not establish production time-to-first-token improvements.
See [request compression benchmarks](docs/request-compression-benchmarks.md) for fixtures, commands, measurements, and allocation checks.

## Local verification

```bash
./repo.sh test-maintenance
./repo.sh setup
./repo.sh check-pr
./repo.sh test
./repo.sh build
```

The maintenance lane runs standard-library Python tests before toolchain setup.
The full test lane reuses that maintenance lane before running Go tests.
Setup downloads the verified toolchain and locked modules into `.local/`.
Subsequent commands disable module downloads and use those local caches.
No arguments enters a shell without user startup files.

## Failed-test comparison

`./repo.sh test` retains human-readable Go output and runs the Python checks first.
`./repo.sh test -json` forwards Go flags and keeps Python diagnostics on stderr.
The workflow records complete candidate Go JSON and stderr under `.local/fork-diagnostics/`.
If the full-test step fails, maintenance compares recorded Go failures with the exact prepared upstream commit.
The candidate failure still blocks compilation, tagging, push, and release publication.
Comparison results never authorize publication or disable tests.

For local comparison, record a failing candidate run first.
Keep the report with its matching source checkout.

```bash
set -o pipefail
mkdir -p .local/fork-diagnostics
./repo.sh test -json 2> .local/fork-diagnostics/candidate.stderr.log \
  | tee .local/fork-diagnostics/candidate.jsonl
./repo.sh diagnose-upstream .local/fork-diagnostics/candidate.jsonl
```

By default, comparison reads `upstream_main_sha` and `source_sha` from `.local/fork-candidate.json`.
Without prepared state, supply an exact upstream commit explicitly.

```bash
./repo.sh diagnose-upstream .local/fork-diagnostics/candidate.jsonl --upstream-sha '<exact-upstream-commit-sha>'
```

The diagnostic verifies the commit and creates a private, shared local clone without checkout.
It detaches the clone at the exact upstream SHA and removes the clone's source remote.
Hooks and global/system Git configuration remain disabled.
Baseline tests see their own Git root and exact upstream HEAD, never the parent fork repository.
The diagnostic never fetches, changes source refs, creates worktrees, or executes upstream setup scripts or `repo.sh`.
It reuses the pinned Go toolchain and repository caches with offline, readonly module settings.
Reruns select top-level tests but preserve every failed test and subtest identity for comparison.
Missing subtests remain inconclusive even when their parent passes or an unrelated sibling fails.
Failures without test identities rerun the complete failed package.

Each run preserves source SHAs, report hashes, commands, exit codes, stdout, stderr, and `summary.json`.
The workflow uploads these diagnostics after success or failure.
The default comparison budget is 600 seconds; `--timeout-seconds` accepts up to 1200 seconds.
Every command exit cleans its owned process group, including descendants after a successful launcher exit.
Timeout or cancellation retains an inconclusive result.

Results distinguish these cases:

- `observed_upstream_failure`: every recorded failing test/subtest identity, or the same package, also fails upstream.
- `passes_upstream`: the observed tests or package pass upstream; investigate a possible fork integration regression.
- `inconclusive`: evidence is missing, mixed, malformed, interrupted, or blocked by build, setup, package, or cache problems.

A missing upstream test never counts as a pass.
Matching identities do not prove identical failure causes.
An upstream pass does not prove the fork caused the failure.
If Python checks fail before Go starts, the comparison reports missing Go evidence as inconclusive.

## Recovery

If synchronization conflicts, maintenance reports both source SHAs, conflicting paths, Git output, and recovery commands.
Preparation stops before candidate creation, tests, tagging, push, or release publication.
The failed checkout retains its conflict state for inspection.
A Git failure without unmerged paths receives separate diagnostics, not conflict-resolution instructions.
Inspect its cause before retrying.

In a clean local checkout, use the exact source SHAs from the failed run.
The commands below use shell variables for those reported values.
Explicit URLs avoid remote-name assumptions in Actions and local checkouts.
Actions uses the fork as `origin`; local checkouts can use upstream as `origin` and the fork as `fork`.

```bash
FORK_SHA='<reported-fork-source-sha>'
UPSTREAM_SHA='<reported-upstream-source-sha>'
git fetch https://github.com/akalsi-org/CLIProxyAPI.git main --tags
git fetch https://github.com/router-for-me/CLIProxyAPI.git main --tags
git switch -c "fix-fork-sync-${UPSTREAM_SHA:0:12}" "$FORK_SHA"
git merge --no-commit --no-ff --strategy=ort "$UPSTREAM_SHA"
git diff --name-only --diff-filter=U
```

Resolve each reported path and review both sides' behavior.
Use `git add -- <resolved-path>` for each resolved path.
Do not stage unrelated files.
Then commit the integration and run every required check.

```bash
git diff --check
git commit -m "fork: sync upstream main" \
  -m "Co-Authored-By: Claude Code <noreply@anthropic.com>"
./repo.sh setup
./repo.sh check-pr
./repo.sh test
./repo.sh build
```

If every check passes, review the integration before publication.
With authorized credentials, push the integration and dispatch maintenance.

```bash
git push https://github.com/akalsi-org/CLIProxyAPI.git HEAD:refs/heads/main
gh workflow run fork-maintenance --repo akalsi-org/CLIProxyAPI --ref main
```

If main advanced, stop and integrate the new commits before repeating verification.
If workflow files changed, use authorized SSH access or a workflows-authorized token.
Do not force-push main or discard reviewed fork behavior silently.

### Limit repeated bridge conflicts

Keep fork-owned bridge policy in small helpers beside upstream-owned request and stream code.
Keep upstream call-site changes narrow and document each policy difference.
Before integration, compare upstream changes against those call sites and their policy helpers.
Test both preserved fork behavior and new upstream behavior with deterministic regressions.
When upstream implements the same guarantee, remove the redundant fork change after verification.
These measures reduce overlapping edits; they cannot prevent all conflicts.
Do not auto-select either side, retry an unchanged conflict, or bypass publication checks.

If branch protection blocks publication, inspect the policy before changing it.
If the pinned toolchain no longer builds upstream, review and update its version and checksum together.
After repairs, dispatch `fork-maintenance` on main.
Verify the release manifest before updating a my-init pin manually.
