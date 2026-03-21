#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP_NAME="esketit_music_server"
TARGET_OS="${TARGET_OS:-linux}"
TARGET_ARCH="${TARGET_ARCH:-amd64}"
OUTPUT_NAME="${OUTPUT_NAME:-$APP_NAME}"
OUTPUT_PATH="${OUTPUT_PATH:-$SCRIPT_DIR/$OUTPUT_NAME}"

usage() {
  cat <<EOF
Usage: ./build_server.sh [--current-machine]

Options:
  --current-machine  Build for the host OS and architecture reported by Go.
  -h, --help        Show this help message.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --current-machine)
      TARGET_OS="$(go env GOHOSTOS)"
      TARGET_ARCH="$(go env GOHOSTARCH)"
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

echo "Building $APP_NAME for ${TARGET_OS}/${TARGET_ARCH}..."
CGO_ENABLED=0 \
GOOS="$TARGET_OS" \
GOARCH="$TARGET_ARCH" \
GOCACHE="${GOCACHE:-/tmp/go-build-cache}" \
go build -o "$OUTPUT_PATH" .

echo "Build complete: $OUTPUT_PATH"
