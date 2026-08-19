#!/usr/bin/env bash

set -Eeuo pipefail

readonly APP_NAME="esketit_music_server"
readonly APP_ROOT="/opt/$APP_NAME"
readonly RELEASES_DIR="$APP_ROOT/releases"
readonly CURRENT_LINK="$APP_ROOT/current"
readonly DATA_ROOT="/var/lib/$APP_NAME"
readonly DB_PATH="$DATA_ROOT/tracks_db.sqlite"
readonly BACKUP_DIR="$DATA_ROOT/backups"
readonly INCOMING_DIR="/var/tmp/$APP_NAME/incoming"
readonly SERVICE_NAME="$APP_NAME.service"
readonly HEALTH_URL="http://127.0.0.1:8080/healthz"
readonly LOCK_FILE="/run/lock/$APP_NAME-deploy.lock"
readonly MAX_ARCHIVE_BYTES=$((256 * 1024 * 1024))
readonly MAX_BINARY_BYTES=$((256 * 1024 * 1024))
readonly MIN_FREE_HEADROOM_BYTES=$((32 * 1024 * 1024))
readonly HEALTH_ATTEMPTS=30
readonly HEALTH_DELAY_SECONDS=1

verify_only=false
archive_path=""
staging_dir=""
temporary_link=""
backup_tmp=""
backup_path=""
previous_release=""
validated_revision=""
installed_release_dir=""
switched_release=false
service_stopped=false
deployment_succeeded=false

usage() {
  cat <<EOF
Usage: sudo $0 [--verify-only] $INCOMING_DIR/<release>.tar.gz

Options:
  --verify-only  Validate and extract the archive into temporary storage without
                 changing the current release, database, or systemd service.
  -h, --help     Show this help message.
EOF
}

log() {
  printf '%s %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*"
}

die() {
  log "ERROR: $*" >&2
  exit 1
}

remove_private_staging_dir() {
  if [[ -z "$staging_dir" || ! -d "$staging_dir" ]]; then
    return
  fi

  case "$staging_dir" in
    "$RELEASES_DIR"/.staging.*)
      rm -rf -- "$staging_dir"
      staging_dir=""
      ;;
    *)
      log "Refusing to remove unexpected staging path: $staging_dir" >&2
      ;;
  esac
}

remove_temporary_link() {
  if [[ -z "$temporary_link" ]]; then
    return
  fi

  case "$temporary_link" in
    "$APP_ROOT"/.current.*)
      rm -f -- "$temporary_link"
      temporary_link=""
      ;;
    *)
      log "Refusing to remove unexpected temporary link: $temporary_link" >&2
      ;;
  esac
}

remove_temporary_backup() {
  if [[ -z "$backup_tmp" || ! -e "$backup_tmp" ]]; then
    return
  fi

  case "$backup_tmp" in
    "$BACKUP_DIR"/.tracks_db.*.tmp)
      rm -f -- "$backup_tmp"
      backup_tmp=""
      ;;
    *)
      log "Refusing to remove unexpected temporary backup: $backup_tmp" >&2
      ;;
  esac
}

switch_current_link() {
  local target=$1

  [[ -d "$target" ]] || return 1

  temporary_link="$(mktemp "$APP_ROOT/.current.XXXXXXXX")"
  rm -f -- "$temporary_link"
  ln -s -- "$target" "$temporary_link"
  mv -Tf -- "$temporary_link" "$CURRENT_LINK"
  temporary_link=""
}

wait_for_health() {
  local attempt response

  for ((attempt = 1; attempt <= HEALTH_ATTEMPTS; attempt++)); do
    if response="$(
      curl \
        --fail \
        --silent \
        --show-error \
        --connect-timeout 1 \
        --max-time 2 \
        "$HEALTH_URL" 2>/dev/null
    )" && [[ "$response" == '{"status":"ok"}' ]]; then
      return 0
    fi
    sleep "$HEALTH_DELAY_SECONDS"
  done

  return 1
}

rollback_service() {
  set +e

  log "Deployment failed; starting automatic binary rollback"
  journalctl -u "$SERVICE_NAME" -n 50 --no-pager >&2
  systemctl stop "$SERVICE_NAME"

  if [[ "$switched_release" == true ]]; then
    if switch_current_link "$previous_release"; then
      log "Restored current symlink to $previous_release"
      switched_release=false
    else
      log "ERROR: failed to restore current symlink to $previous_release" >&2
    fi
  fi

  systemctl reset-failed "$SERVICE_NAME"
  if systemctl start "$SERVICE_NAME" && wait_for_health; then
    service_stopped=false
    log "Previous release is healthy after rollback"
  else
    log "ERROR: previous release did not become healthy after rollback" >&2
    log "The pre-deployment database backup is $backup_path" >&2
  fi
}

cleanup() {
  local status=$?
  trap - EXIT

  set +e
  remove_temporary_link
  remove_private_staging_dir
  remove_temporary_backup

  if ((status != 0)) && [[ "$deployment_succeeded" != true ]] && \
    { [[ "$service_stopped" == true ]] || [[ "$switched_release" == true ]]; }; then
    rollback_service
  fi

  exit "$status"
}

parse_arguments() {
  while (($# > 0)); do
    case "$1" in
      --verify-only)
        verify_only=true
        shift
        ;;
      -h | --help)
        usage
        exit 0
        ;;
      --*)
        usage >&2
        die "unknown option: $1"
        ;;
      *)
        if [[ -n "$archive_path" ]]; then
          usage >&2
          die "only one release archive may be supplied"
        fi
        archive_path=$1
        shift
        ;;
    esac
  done

  if [[ -z "$archive_path" ]]; then
    usage >&2
    die "a release archive is required"
  fi
}

require_root() {
  if ((EUID != 0)); then
    die "this script must run as root; use sudo"
  fi
}

require_commands() {
  local command_name
  for command_name in \
    curl \
    df \
    file \
    flock \
    grep \
    journalctl \
    mktemp \
    readlink \
    sha256sum \
    sqlite3 \
    systemctl \
    tar; do
    command -v "$command_name" >/dev/null 2>&1 || \
      die "required command is unavailable: $command_name"
  done
}

resolve_archive_path() {
  local incoming_real supplied_path archive_real

  [[ -d "$INCOMING_DIR" ]] || die "incoming directory does not exist: $INCOMING_DIR"
  incoming_real="$(readlink -f -- "$INCOMING_DIR")"

  if [[ "$archive_path" != /* ]]; then
    supplied_path="$PWD/$archive_path"
  else
    supplied_path=$archive_path
  fi

  [[ ! -L "$supplied_path" ]] || die "release archive must not be a symbolic link"
  [[ -f "$supplied_path" ]] || die "release archive does not exist: $supplied_path"

  archive_real="$(readlink -f -- "$supplied_path")"
  [[ "$(dirname -- "$archive_real")" == "$incoming_real" ]] || \
    die "release archive must be a direct child of $INCOMING_DIR"
  [[ "$(basename -- "$archive_real")" == *.tar.gz ]] || \
    die "release archive must have a .tar.gz suffix"

  archive_path=$archive_real
}

validate_archive_listing() {
  local entry
  local saw_root=false
  local saw_binary=false
  local saw_revision=false
  local saw_checksums=false

  while IFS= read -r entry; do
    case "$entry" in
      ./)
        [[ "$saw_root" == false ]] || die "archive contains duplicate entry: $entry"
        saw_root=true
        ;;
      ./esketit_music_server)
        [[ "$saw_binary" == false ]] || die "archive contains duplicate entry: $entry"
        saw_binary=true
        ;;
      ./REVISION)
        [[ "$saw_revision" == false ]] || die "archive contains duplicate entry: $entry"
        saw_revision=true
        ;;
      ./SHA256SUMS)
        [[ "$saw_checksums" == false ]] || die "archive contains duplicate entry: $entry"
        saw_checksums=true
        ;;
      *)
        die "archive contains unexpected entry: $entry"
        ;;
    esac
  done < <(tar -tzf "$archive_path")

  [[ "$saw_binary" == true ]] || die "archive does not contain esketit_music_server"
  [[ "$saw_revision" == true ]] || die "archive does not contain REVISION"
  [[ "$saw_checksums" == true ]] || die "archive does not contain SHA256SUMS"
}

check_available_space() {
  local archive_bytes binary_bytes database_bytes wal_bytes free_bytes required_bytes

  archive_bytes="$(stat -c '%s' "$archive_path")"
  ((archive_bytes > 0)) || die "release archive is empty"
  ((archive_bytes <= MAX_ARCHIVE_BYTES)) || \
    die "release archive exceeds the $MAX_ARCHIVE_BYTES byte limit"

  binary_bytes="$(tar -xOzf "$archive_path" ./esketit_music_server | wc -c)"
  binary_bytes="${binary_bytes//[[:space:]]/}"
  ((binary_bytes > 0)) || die "embedded server binary is empty"
  ((binary_bytes <= MAX_BINARY_BYTES)) || \
    die "server binary exceeds the $MAX_BINARY_BYTES byte limit"

  database_bytes=0
  wal_bytes=0
  if [[ -f "$DB_PATH" ]]; then
    database_bytes="$(stat -c '%s' "$DB_PATH")"
  fi
  if [[ -f "$DB_PATH-wal" ]]; then
    wal_bytes="$(stat -c '%s' "$DB_PATH-wal")"
  fi

  free_bytes="$(df -PB1 "$RELEASES_DIR" | awk 'NR == 2 {print $4}')"
  required_bytes=$((binary_bytes + database_bytes + wal_bytes + MIN_FREE_HEADROOM_BYTES))
  ((free_bytes >= required_bytes)) || \
    die "insufficient free space: need $required_bytes bytes, have $free_bytes bytes"
}

validate_checksum_manifest() {
  local line hash marker filename
  local saw_binary=false
  local saw_revision=false

  while IFS= read -r line || [[ -n "$line" ]]; do
    if [[ "$line" =~ ^([0-9a-f]{64})[[:space:]]+([*]?)(esketit_music_server|REVISION)$ ]]; then
      hash=${BASH_REMATCH[1]}
      marker=${BASH_REMATCH[2]}
      filename=${BASH_REMATCH[3]}
      : "$hash" "$marker"
    else
      die "SHA256SUMS contains an invalid entry"
    fi

    case "$filename" in
      esketit_music_server)
        [[ "$saw_binary" == false ]] || die "SHA256SUMS contains duplicate binary entries"
        saw_binary=true
        ;;
      REVISION)
        [[ "$saw_revision" == false ]] || die "SHA256SUMS contains duplicate REVISION entries"
        saw_revision=true
        ;;
    esac
  done < "$staging_dir/SHA256SUMS"

  [[ "$saw_binary" == true && "$saw_revision" == true ]] || \
    die "SHA256SUMS must contain exactly the binary and REVISION"

  (
    cd "$staging_dir"
    sha256sum --check --strict --status SHA256SUMS
  )
}

extract_and_validate_archive() {
  local nested_entry revision binary_description

  staging_dir="$(mktemp -d "$RELEASES_DIR/.staging.XXXXXXXX")"
  chmod 0700 "$staging_dir"

  tar \
    --extract \
    --gzip \
    --file="$archive_path" \
    --directory="$staging_dir" \
    --no-same-owner \
    --no-same-permissions

  nested_entry="$(find "$staging_dir" -mindepth 2 -print -quit)"
  [[ -z "$nested_entry" ]] || die "archive extracted an unexpected nested path: $nested_entry"

  [[ -f "$staging_dir/$APP_NAME" && ! -L "$staging_dir/$APP_NAME" ]] || \
    die "server binary is not a regular file"
  [[ -f "$staging_dir/REVISION" && ! -L "$staging_dir/REVISION" ]] || \
    die "REVISION is not a regular file"
  [[ -f "$staging_dir/SHA256SUMS" && ! -L "$staging_dir/SHA256SUMS" ]] || \
    die "SHA256SUMS is not a regular file"

  [[ "$(find "$staging_dir" -mindepth 1 -maxdepth 1 -type f | wc -l | tr -d ' ')" == 3 ]] || \
    die "release must contain exactly three regular files"

  [[ "$(stat -c '%s' "$staging_dir/REVISION")" -le 128 ]] || \
    die "REVISION is unexpectedly large"
  [[ "$(stat -c '%s' "$staging_dir/SHA256SUMS")" -le 4096 ]] || \
    die "SHA256SUMS is unexpectedly large"

  revision="$(tr -d '\r\n' < "$staging_dir/REVISION")"
  [[ "$revision" =~ ^[0-9a-f]{40}$ ]] || die "REVISION must contain one full lowercase Git SHA"
  [[ "$(wc -l < "$staging_dir/REVISION" | tr -d ' ')" -le 1 ]] || \
    die "REVISION must contain only one line"

  validate_checksum_manifest

  binary_description="$(file -b "$staging_dir/$APP_NAME")"
  [[ "$binary_description" == *"ELF 64-bit LSB"* ]] || \
    die "server binary is not a 64-bit Linux ELF executable"
  [[ "$binary_description" == *"x86-64"* || "$binary_description" == *"x86_64"* ]] || \
    die "server binary is not built for linux/amd64"
  [[ "$binary_description" == *"statically linked"* ]] || \
    die "server binary is not statically linked"

  validated_revision=$revision
}

install_release() {
  local revision=$1
  local release_dir="$RELEASES_DIR/$revision"

  if [[ -e "$release_dir" || -L "$release_dir" ]]; then
    die "release already exists: $release_dir"
  fi

  chown -R root:root "$staging_dir"
  chmod 0755 "$staging_dir"
  chmod 0755 "$staging_dir/$APP_NAME"
  chmod 0444 "$staging_dir/REVISION" "$staging_dir/SHA256SUMS"

  mv -- "$staging_dir" "$release_dir"
  staging_dir=""
  installed_release_dir=$release_dir
}

create_database_backup() {
  local revision=$1
  local timestamp integrity_result

  [[ -f "$DB_PATH" ]] || die "production database does not exist: $DB_PATH"

  install -d -o root -g root -m 0700 "$BACKUP_DIR"
  timestamp="$(date -u '+%Y%m%dT%H%M%SZ')"
  backup_path="$BACKUP_DIR/tracks_db.$timestamp.$revision.sqlite"
  backup_tmp="$BACKUP_DIR/.tracks_db.$timestamp.$revision.$$.tmp"

  [[ ! -e "$backup_path" && ! -e "$backup_tmp" ]] || \
    die "database backup path already exists"

  sqlite3 "$DB_PATH" \
    '.timeout 30000' \
    ".backup '$backup_tmp'"

  integrity_result="$(sqlite3 "$backup_tmp" 'PRAGMA integrity_check;')"
  [[ "$integrity_result" == "ok" ]] || die "database backup failed integrity_check: $integrity_result"

  chown root:root "$backup_tmp"
  chmod 0600 "$backup_tmp"
  mv -- "$backup_tmp" "$backup_path"
  backup_tmp=""

  log "Database backup created: $backup_path"
}

main() {
  local revision release_dir

  parse_arguments "$@"
  require_root
  require_commands
  trap cleanup EXIT

  install -d -o root -g root -m 0755 "$APP_ROOT" "$RELEASES_DIR"
  exec 9>"$LOCK_FILE"
  flock -n 9 || die "another deployment is already running"

  resolve_archive_path
  validate_archive_listing
  check_available_space
  extract_and_validate_archive
  revision=$validated_revision
  log "Validated release archive for revision $revision"

  if [[ "$verify_only" == true ]]; then
    log "Verification completed; no deployment changes were made"
    deployment_succeeded=true
    return
  fi

  [[ -L "$CURRENT_LINK" ]] || die "current release link is missing or is not a symlink: $CURRENT_LINK"
  previous_release="$(readlink -f -- "$CURRENT_LINK")"
  [[ -d "$previous_release" ]] || die "current release target does not exist: $previous_release"
  case "$previous_release" in
    "$RELEASES_DIR"/*) ;;
    *) die "current release points outside $RELEASES_DIR: $previous_release" ;;
  esac

  systemctl is-active --quiet "$SERVICE_NAME" || die "service is not currently active: $SERVICE_NAME"

  install_release "$revision"
  release_dir=$installed_release_dir
  log "Installed immutable release: $release_dir"

  service_stopped=true
  log "Stopping $SERVICE_NAME before the database snapshot"
  systemctl stop "$SERVICE_NAME"

  create_database_backup "$revision"

  switch_current_link "$release_dir"
  switched_release=true
  log "Switched current release to $release_dir"

  systemctl reset-failed "$SERVICE_NAME"
  systemctl start "$SERVICE_NAME"
  service_stopped=false

  if ! wait_for_health; then
    die "new release did not become healthy at $HEALTH_URL"
  fi

  deployment_succeeded=true
  log "Deployment succeeded for revision $revision"
  log "Previous release retained at $previous_release"
  log "Database backup retained at $backup_path"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
