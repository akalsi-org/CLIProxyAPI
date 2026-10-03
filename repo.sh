#!/usr/bin/env bash
# Fork tooling. The upstream module path and native plugin ABI remain unchanged.
set -euo pipefail
ROOT="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
export REPO_ROOT="$ROOT" REPO_LOCAL="$ROOT/.local" REPO_ARCH="$(uname -m)" REPO_TOOLCHAIN=go1.26.4 REPO_SHELL=1
export GOPATH="$REPO_LOCAL/cache/gopath" GOMODCACHE="$REPO_LOCAL/cache/modules" GOCACHE="$REPO_LOCAL/cache/go-build"
export GOTOOLCHAIN=local GOMAXPROCS="${GOMAXPROCS:-4}" PYTHONDONTWRITEBYTECODE=1
export PATH="$REPO_LOCAL/toolchains/go/bin:$PATH"
cd "$ROOT"
cmd="${1:-shell}"
if [ "$#" -gt 0 ]; then shift; fi
case "$cmd" in
  help|-h|--help)
    printf 'Usage: ./repo.sh [setup|test|check-pr|build|help|COMMAND [ARGS...]]\n'
    printf 'No arguments enters the fork tooling shell. Run setup once to install verified Go 1.26.4.\n'
    exit 0 ;;
  setup)
    [ "$(uname -s)" = Linux ] && [ "$REPO_ARCH" = x86_64 ] || { printf 'Fork setup requires Linux x86_64.\n' >&2; exit 1; }
    archive="$REPO_LOCAL/toolchains/go1.26.4.tar.gz"
    mkdir -p "$REPO_LOCAL/toolchains"
    if [ ! -f "$archive" ]; then
      tmp="$(mktemp "$REPO_LOCAL/toolchains/go-download.XXXXXX")"
      trap 'rm -f "$tmp"' EXIT
      curl --fail --location --silent --show-error --max-time 180 https://go.dev/dl/go1.26.4.linux-amd64.tar.gz -o "$tmp"
      printf '%s  %s\n' 1153d3d50e0ac764b447adfe05c2bcf08e889d42a02e0fe0259bd47f6733ad7f "$tmp" | sha256sum -c -
      mv "$tmp" "$archive"
    fi
    printf '%s  %s\n' 1153d3d50e0ac764b447adfe05c2bcf08e889d42a02e0fe0259bd47f6733ad7f "$archive" | sha256sum -c -
    if [ ! -x "$REPO_LOCAL/toolchains/go/bin/go" ]; then tar -xzf "$archive" -C "$REPO_LOCAL/toolchains"; fi
    go version
    go mod download
    exit 0 ;;
esac
[ -x "$REPO_LOCAL/toolchains/go/bin/go" ] || { printf 'Missing pinned Go toolchain. Run ./repo.sh setup\n' >&2; exit 1; }
[ "$(go version)" = 'go version go1.26.4 linux/amd64' ] || { printf 'Unexpected Go toolchain. Run ./repo.sh setup\n' >&2; exit 1; }
export GOPROXY=off
case "$cmd" in
  shell) exec bash --noprofile --norc -i ;;
  test) python3 -m unittest discover -s .github/scripts -p '*_test.py'; exec go test ./... ;;
  check-pr) exec go test ./internal/runtime/executor -run 'TestClassifyAntigravity429|TestDecideAntigravity429|TestNewAntigravityStatusErr' -count=1 ;;
  build) mkdir -p "$REPO_LOCAL/bin"; CGO_ENABLED=1 go build -o "$REPO_LOCAL/bin/cli-proxy-api" ./cmd/server/ ;;
  *) exec "$cmd" "$@" ;;
esac
