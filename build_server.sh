#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP_NAME="esketit_music_server"
TARGET_OS="${TARGET_OS:-linux}"
TARGET_ARCH="${TARGET_ARCH:-amd64}"
OUTPUT_NAME="${OUTPUT_NAME:-$APP_NAME}"
OUTPUT_PATH="${OUTPUT_PATH:-$SCRIPT_DIR/$OUTPUT_NAME}"

echo "Building $APP_NAME for ${TARGET_OS}/${TARGET_ARCH}..."
CGO_ENABLED=0 \
GOOS="$TARGET_OS" \
GOARCH="$TARGET_ARCH" \
GOCACHE="${GOCACHE:-/tmp/go-build-cache}" \
go build -o "$OUTPUT_PATH" .

echo "Build complete: $OUTPUT_PATH"
