#!/usr/bin/env bash
# Docker-first build: compiles the binary inside the official Go image
# (same version as go.mod and the Dockerfile builder stage), so no Go
# toolchain is needed on the host.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT/scripts/_run.sh"


mkdir -p "$ROOT/.dev/gocache" "$ROOT/.dev/gomodcache" "$ROOT/bin"

run_logged build docker run --rm \
  --user "$(id -u):$(id -g)" \
  -e CGO_ENABLED=0 \
  -e GOCACHE=/cache/go-build \
  -e GOMODCACHE=/cache/gomod \
  -v "$ROOT":/src -w /src \
  -v "$ROOT/.dev/gocache":/cache/go-build \
  -v "$ROOT/.dev/gomodcache":/cache/gomod \
  golang:1.26-alpine \
  go build -o bin/voidgrid-cleaner ./cmd/voidgrid-cleaner
exit $?
