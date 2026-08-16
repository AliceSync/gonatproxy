#!/usr/bin/env bash
# Builds all binaries into ./bin.
# Run from the repo root:  ./scripts/build.sh
set -euo pipefail
cd "$(dirname "$0")/.."

mkdir -p bin
VERSION="$(date)"
LDF="-s -w -X main.version=$VERSION"

echo "building server..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$LDF" -o bin/deepseek-server_linux_amd64 ./cmd/server
echo "building client (entry)..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$LDF" -o bin/deepseek-client_linux_amd64 ./cmd/client
echo "building natclient..."
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="$LDF" -o bin/deepseek-natclient_linux_arm64 ./cmd/natclient

echo
echo "binaries:"
ls -lh bin/
