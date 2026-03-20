#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP_NAME="esketit_music_server"
APP_BINARY="$SCRIPT_DIR/$APP_NAME"

DEFAULT_SONGS_DIR="$HOME/Projects/esketit_music/media_storage/songs"
DEFAULT_ALBUM_COVERS_DIR="$HOME/Projects/esketit_music/media_storage/album_covers"
DEFAULT_TRACKS_DB_PATH="$SCRIPT_DIR/tracks_db.json"

export SONGS_DIR="${SONGS_DIR:-$DEFAULT_SONGS_DIR}"
export ALBUM_COVERS_DIR="${ALBUM_COVERS_DIR:-$DEFAULT_ALBUM_COVERS_DIR}"
export TRACKS_DB_PATH="${TRACKS_DB_PATH:-$DEFAULT_TRACKS_DB_PATH}"

if [[ -z "${AUTH_SECRET:-}" ]]; then
  if command -v openssl >/dev/null 2>&1; then
    AUTH_SECRET="$(openssl rand -base64 48)"
  else
    AUTH_SECRET="$(LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 64)"
  fi
  export AUTH_SECRET
  echo "Generated AUTH_SECRET for this run."
fi

if [[ ${#AUTH_SECRET} -lt 32 ]]; then
  echo "AUTH_SECRET must contain at least 32 characters." >&2
  exit 1
fi

if [[ ! -d "$SONGS_DIR" ]]; then
  echo "SONGS_DIR does not exist: $SONGS_DIR" >&2
  exit 1
fi

if [[ ! -x "$APP_BINARY" ]]; then
  echo "Server binary not found or not executable: $APP_BINARY" >&2
  echo "Build it first with ./build_server.sh or copy a built binary to the server." >&2
  exit 1
fi

echo "Starting $APP_NAME..."
echo "SONGS_DIR=$SONGS_DIR"
echo "ALBUM_COVERS_DIR=$ALBUM_COVERS_DIR"
echo "TRACKS_DB_PATH=$TRACKS_DB_PATH"
exec "$APP_BINARY"
