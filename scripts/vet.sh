#!/usr/bin/env bash
# Docker-first vet: runs `go vet` inside the official Go image.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT/scripts/_run.sh"

mkdir -p "$ROOT/.dev/gocache" "$ROOT/.dev/gomodcache"

run_logged vet docker run --rm \
  --user "$(id -u):$(id -g)" \
  -e GOCACHE=/cache/go-build \
  -e GOMODCACHE=/cache/gomod \
  -v "$ROOT":/src -w /src \
  -v "$ROOT/.dev/gocache":/cache/go-build \
  -v "$ROOT/.dev/gomodcache":/cache/gomod \
  golang:1.26-alpine \
  go vet ./...
exit $?
