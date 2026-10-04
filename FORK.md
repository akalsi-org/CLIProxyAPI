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

If synchronization conflicts, resolve it on an integration branch and run the same checks.
Do not force-push main or discard the reviewed patch silently.
If branch protection blocks publication, inspect the policy before changing it.
If the pinned toolchain no longer builds upstream, review and update its version and checksum together.
After repairs, dispatch `fork-maintenance` on main.
Verify the release manifest before updating a my-init pin manually.
