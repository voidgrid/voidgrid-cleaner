# syntax=docker/dockerfile:1

# One process (the app, PID 1) and one SQLite file on /data.

# Test toolchain shared by the image build and scripts/test.sh. gcc and
# musl-dev are only for `-race` (cgo); the SQLite driver is pure Go.
FROM golang:1.26-alpine AS testenv
RUN apk add --no-cache gcc musl-dev

# Tests gate the image: a failing vet or test fails the build.
FROM testenv AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go vet ./...
RUN go test -race ./...
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/voidgrid-cleaner ./cmd/voidgrid-cleaner

FROM alpine:3.22
RUN apk add --no-cache ca-certificates sqlite
COPY --from=builder /out/voidgrid-cleaner /usr/local/bin/voidgrid-cleaner
RUN addgroup -g 1000 voidgrid && \
    adduser -D -H -u 1000 -G voidgrid voidgrid && \
    mkdir -p /data && chown 1000:1000 /data
USER 1000:1000
ENV VGC_LISTEN_ADDR=0.0.0.0:8790 \
    VGC_DB_PATH=/data/voidgrid-cleaner.db
VOLUME ["/data"]
EXPOSE 8790
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8790/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/voidgrid-cleaner"]
