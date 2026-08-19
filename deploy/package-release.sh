#!/usr/bin/env bash

set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPOSITORY_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
readonly SCRIPT_DIR REPOSITORY_ROOT
readonly APP_NAME="esketit_music_server"
readonly TARGET_OS="linux"
readonly TARGET_ARCH="amd64"

release_dir=""
release_dir_created=false
temporary_archive=""
package_succeeded=false

log() {
  printf '%s\n' "$*"
}

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

sha256_files() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$@"
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$@"
  else
    die "sha256sum or shasum is required"
  fi
}

cleanup() {
  local status=$?
  trap - EXIT

  if ((status != 0)) && [[ "$package_succeeded" != true ]]; then
    if [[ -n "$temporary_archive" && -f "$temporary_archive" ]]; then
      case "$temporary_archive" in
        "$REPOSITORY_ROOT"/dist/.esketit_music_server-*.tar.gz.tmp.*)
          rm -f -- "$temporary_archive"
          ;;
      esac
    fi

    if [[ "$release_dir_created" == true && -n "$release_dir" && -d "$release_dir" ]]; then
      case "$release_dir" in
        "$REPOSITORY_ROOT"/dist/esketit_music_server-*-linux-amd64)
          rm -rf -- "$release_dir"
          ;;
      esac
    fi
  fi

  exit "$status"
}

trap cleanup EXIT

main() {
  local release_sha short_sha release_name archive_path archive_checksum_path

  (($# == 0)) || die "this script does not accept arguments"

  cd "$REPOSITORY_ROOT"

  [[ -z "$(git status --porcelain)" ]] || \
    die "refusing to package a dirty working tree; commit or stash changes first"

  release_sha="$(git rev-parse HEAD)"
  short_sha="$(git rev-parse --short=12 HEAD)"
  [[ "$release_sha" =~ ^[0-9a-f]{40}$ ]] || die "unable to resolve a full Git revision"

  release_name="$APP_NAME-$short_sha-$TARGET_OS-$TARGET_ARCH"
  release_dir="$REPOSITORY_ROOT/dist/$release_name"
  archive_path="$REPOSITORY_ROOT/dist/$release_name.tar.gz"
  archive_checksum_path="$archive_path.sha256"
  temporary_archive="$REPOSITORY_ROOT/dist/.$release_name.tar.gz.tmp.$$"

  if [[ -e "$release_dir" || -e "$archive_path" || -e "$archive_checksum_path" ]]; then
    die "release output already exists for $short_sha"
  fi

  log "Testing revision $release_sha..."
  go test ./...

  mkdir -p "$release_dir"
  release_dir_created=true

  log "Building $APP_NAME for $TARGET_OS/$TARGET_ARCH..."
  OUTPUT_PATH="$release_dir/$APP_NAME" \
    TARGET_OS="$TARGET_OS" \
    TARGET_ARCH="$TARGET_ARCH" \
    "$REPOSITORY_ROOT/build_server.sh"

  chmod 0755 "$release_dir/$APP_NAME"
  printf '%s\n' "$release_sha" > "$release_dir/REVISION"

  (
    cd "$release_dir"
    sha256_files "$APP_NAME" REVISION > SHA256SUMS
  )

  # macOS copyfile metadata creates AppleDouble files such as
  # ._esketit_music_server on Linux. Disable both that mechanism and xattrs so
  # the deployment archive always contains exactly the three expected files.
  COPYFILE_DISABLE=1 tar \
    --no-xattrs \
    -C "$release_dir" \
    -czf "$temporary_archive" \
    .

  mv -- "$temporary_archive" "$archive_path"
  temporary_archive=""

  (
    cd "$REPOSITORY_ROOT/dist"
    sha256_files "$release_name.tar.gz" > "$release_name.tar.gz.sha256"
  )

  package_succeeded=true
  log "Release bundle created:"
  log "$archive_path"
  log "$archive_checksum_path"
}

main "$@"
