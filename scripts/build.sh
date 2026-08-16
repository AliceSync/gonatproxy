#!/usr/bin/env bash
# Builds all binaries into ./bin.
# Run from the repo root:  ./scripts/build.sh
set -euo pipefail
cd "$(dirname "$0")/.."

mkdir -p bin
VERSION="$(git describe --always --dirty 2>/dev/null || echo dev)"
LDF="-s -w -X main.version=$VERSION"

echo "building server..."
CGO_ENABLED=0 go build -trimpath -ldflags="$LDF" -o bin/deepseek-server ./cmd/server
echo "building client (entry)..."
CGO_ENABLED=0 go build -trimpath -ldflags="$LDF" -o bin/deepseek-client ./cmd/client
echo "building natclient..."
CGO_ENABLED=0 go build -trimpath -ldflags="$LDF" -o bin/deepseek-natclient ./cmd/natclient

echo
echo "binaries:"
ls -lh bin/
