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

A candidate merges upstream main without a force push.
Merge conflicts, upstream history rewrites, failed tests, and build failures stop publication.
Upstream workflow-file changes also stop synchronization for manual integration.
GitHub's repository `GITHUB_TOKEN` cannot push workflow-file changes.
Integrate those changes locally with authorized SSH access or a workflows-authorized token, then dispatch maintenance again.
No upstream workflow changes are silently discarded.
The workflow runs the Antigravity regressions, all Go tests, and a server compile check.
An unchanged tested source and upstream release baseline produce no release.
A new candidate uses the latest stable upstream version plus `-akalsi.N`.
For example, `8.0.13-akalsi.1` is a fork snapshot, not an unchanged upstream release.

The Go 1.26.4 archive has a pinned SHA256.
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

## Local verification

```bash
./repo.sh setup
./repo.sh check-pr
./repo.sh test
./repo.sh build
```

Setup downloads the verified toolchain and locked modules into `.local/`.
Subsequent commands disable module downloads and use those local caches.
No arguments enters a shell without user startup files.

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
git merge --no-commit --no-ff "$UPSTREAM_SHA"
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
