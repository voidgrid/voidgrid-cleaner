#!/usr/bin/env bash
# Docker-first test: runs `go test -race ./...` in the Dockerfile's
# `testenv` stage - the same Go + C toolchain (cgo, for -race) the image
# build's test gate uses. The stage is built (and cached) from
# deploy/docker/Dockerfile on each run; after the first build that's a
# cache hit.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT/scripts/_run.sh"

TESTENV_IMAGE=voidgrid-cleaner-testenv

# The testenv stage copies nothing from the build context, so a small
# context directory keeps this fast (the repo root would send .dev/ caches).
run_logged testenv docker build \
  --target testenv \
  -t "$TESTENV_IMAGE" \
  -f "$ROOT/deploy/docker/Dockerfile" \
  "$ROOT/deploy/docker" || exit $?

mkdir -p "$ROOT/.dev/gocache" "$ROOT/.dev/gomodcache"

run_logged test docker run --rm \
  --user "$(id -u):$(id -g)" \
  -e CGO_ENABLED=1 \
  -e GOCACHE=/cache/go-build \
  -e GOMODCACHE=/cache/gomod \
  -v "$ROOT":/src -w /src \
  -v "$ROOT/.dev/gocache":/cache/go-build \
  -v "$ROOT/.dev/gomodcache":/cache/gomod \
  "$TESTENV_IMAGE" \
  go test -race -v ./...
status=$?

skipped=$(grep -c '^--- SKIP' "$ROOT/.dev/logs/test.log" || true)
if [ "$skipped" -gt 0 ]; then
  echo "note: $skipped test(s) skipped"
fi

exit "$status"
