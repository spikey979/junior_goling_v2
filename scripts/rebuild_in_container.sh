#!/usr/bin/env bash
set -euo pipefail

# Rebuilds the Go binary inside the persistent builder container and restarts the app service.
# Shows progress to avoid confusion during first-time package install/module download.

echo "[rebuild] Ensuring builder service is up..."
if ! docker compose ps -q builder >/dev/null 2>&1 || [ -z "$(docker compose ps -q builder)" ]; then
  docker compose up -d builder
fi

echo "[rebuild] Installing toolchain (first run only) inside builder..."
docker compose exec -T builder bash -lc '
  set -e;
  export DEBIAN_FRONTEND=noninteractive;
  apt-get update && apt-get install -y --no-install-recommends build-essential pkg-config ca-certificates;
  echo "[builder] Toolchain ready.";
'

echo "[rebuild] Downloading modules (cached after first time)..."
docker compose exec -T builder bash -lc '
  set -e;
  mkdir -p bin;
  GO_BIN=${GO_BIN:-/usr/local/go/bin/go};
  if ! command -v "$GO_BIN" >/dev/null 2>&1; then
    echo "[builder][fatal] Go toolchain not found at $GO_BIN" >&2; exit 1; fi;
  "$GO_BIN" env GOPROXY;
  "$GO_BIN" mod download all;
  echo "[builder] Modules downloaded.";
'

echo "[rebuild] Building linux/amd64 binary..."
docker compose exec -T builder bash -lc '
  set -e;
  export CGO_ENABLED=1 GOOS=linux GOARCH=amd64 GOFLAGS=-mod=mod;
  GO_BIN=${GO_BIN:-/usr/local/go/bin/go};
  "$GO_BIN" build -v -o bin/aidispatcher ./cmd/app;
  ls -lh bin/aidispatcher;
'

echo "[rebuild] Restarting app to pick up new binary..."
docker compose restart app
echo "[rebuild] Done."
