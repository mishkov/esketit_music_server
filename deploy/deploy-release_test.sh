#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/deploy-release.sh"

test_root="$(mktemp -d "${TMPDIR:-/tmp}/esketit-deploy-test.XXXXXXXX")"

cleanup_test() {
  case "$test_root" in
    "${TMPDIR:-/tmp}"/esketit-deploy-test.*)
      rm -rf -- "$test_root"
      ;;
    *)
      printf 'Refusing to remove unexpected test directory: %s\n' "$test_root" >&2
      ;;
  esac
}

trap cleanup_test EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

make_manifest() {
  local directory=$1
  (
    cd "$directory"
    sha256sum esketit_music_server REVISION > SHA256SUMS
  )
}

valid_bundle="$test_root/valid"
mkdir -p "$valid_bundle"
printf 'test binary\n' > "$valid_bundle/esketit_music_server"
printf '%040d\n' 0 > "$valid_bundle/REVISION"
make_manifest "$valid_bundle"

staging_dir=$valid_bundle
validate_checksum_manifest

invalid_manifest_bundle="$test_root/invalid-manifest"
cp -R "$valid_bundle" "$invalid_manifest_bundle"
printf '%064d  unexpected-file\n' 0 >> "$invalid_manifest_bundle/SHA256SUMS"

if (
  staging_dir=$invalid_manifest_bundle
  validate_checksum_manifest >/dev/null 2>&1
); then
  fail "checksum validation accepted an unexpected file"
fi

valid_archive="$test_root/valid.tar.gz"
COPYFILE_DISABLE=1 tar --no-xattrs -C "$valid_bundle" -czf "$valid_archive" .
archive_path=$valid_archive
validate_archive_listing

invalid_archive_bundle="$test_root/invalid-archive"
cp -R "$valid_bundle" "$invalid_archive_bundle"
printf 'unexpected\n' > "$invalid_archive_bundle/extra-file"
invalid_archive="$test_root/invalid.tar.gz"
COPYFILE_DISABLE=1 tar --no-xattrs -C "$invalid_archive_bundle" -czf "$invalid_archive" .

if (
  archive_path=$invalid_archive
  validate_archive_listing >/dev/null 2>&1
); then
  fail "archive validation accepted an unexpected file"
fi

printf 'PASS: deploy release archive validation tests\n'
