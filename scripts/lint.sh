#!/usr/bin/env bash
# Docker-first lint: runs golangci-lint via its own official image, so no
# local install is needed.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT/scripts/_run.sh"

mkdir -p "$ROOT/.dev/gocache" "$ROOT/.dev/gomodcache" "$ROOT/.dev/lintcache"

run_logged lint docker run --rm \
  --user "$(id -u):$(id -g)" \
  -e GOCACHE=/cache/go-build \
  -e GOMODCACHE=/cache/gomod \
  -e GOLANGCI_LINT_CACHE=/cache/lint \
  -v "$ROOT":/src -w /src \
  -v "$ROOT/.dev/gocache":/cache/go-build \
  -v "$ROOT/.dev/gomodcache":/cache/gomod \
  -v "$ROOT/.dev/lintcache":/cache/lint \
  golangci/golangci-lint:latest \
  golangci-lint run
exit $?
