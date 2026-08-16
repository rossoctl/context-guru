#!/usr/bin/env bash
# Back up the context-guru CONTROL database to Box.
#
# This is the one irreplaceable file: tenants, token hashes, per-tenant configuration,
# the audit trail. The metrics database is a derived view and is not backed up — under
# pressure it archives itself to Box session by session, and anything it loses beyond
# that is reconstructible from nothing that matters.
#
# Uses `sqlite3 .backup`, not `cp`. Copying a live SQLite file while the service is
# writing yields a torn snapshot that may not open at all — and a backup you discover
# is unreadable at restore time is worse than no backup, because you stopped worrying.
#
# Restore:
#   systemctl stop context-guru
#   rclone cat box:context-guru/backup/cg-control-<date>.db > /var/lib/context-guru/cg-control.db
#   chown cg:cg /var/lib/context-guru/cg-control.db && systemctl start context-guru

set -euo pipefail

CONTROL_DB="${CONTROL_DB:-/var/lib/context-guru/cg-control.db}"
REMOTE_BASE="${ARCHIVE_REMOTE:-box:context-guru}"
RCLONE_CONFIG_PATH="${RCLONE_CONFIG:-/var/lib/context-guru/rclone.conf}"
KEEP_DAYS="${KEEP_DAYS:-30}"

if [ ! -f "$CONTROL_DB" ]; then
  echo "backup: no control database at $CONTROL_DB — nothing to do (not an error)"
  exit 0
fi

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
TMP="$(mktemp -t cg-control-backup-XXXXXX.db)"
# The snapshot may contain token hashes and per-tenant config. Not secrets that can be
# replayed, but not world-readable either.
chmod 600 "$TMP"
trap 'rm -f "$TMP" "$TMP-wal" "$TMP-shm"' EXIT

# .backup takes a consistent snapshot of a live database, WAL included.
sqlite3 "$CONTROL_DB" ".backup '$TMP'"
# Prove it opens and the tenant table is intact before shipping it anywhere. A backup
# that is verified only at restore time is a backup nobody has verified.
COUNT="$(sqlite3 "$TMP" 'SELECT COUNT(*) FROM tenants' 2>/dev/null || echo FAIL)"
if [ "$COUNT" = "FAIL" ]; then
  echo "backup: the snapshot does not open or has no tenants table — REFUSING to upload" >&2
  exit 1
fi

DEST="${REMOTE_BASE}/backup/cg-control-${STAMP}.db"
gzip -c "$TMP" | rclone --config "$RCLONE_CONFIG_PATH" rcat "${DEST}.gz"

# Confirm it landed before reporting success.
if ! rclone --config "$RCLONE_CONFIG_PATH" lsjson --stat "${DEST}.gz" >/dev/null 2>&1; then
  echo "backup: upload did not land at ${DEST}.gz" >&2
  exit 1
fi
echo "backup: ${DEST}.gz uploaded ($COUNT tenants)"

# Age out old snapshots. Box is effectively unlimited, but an unbounded pile of daily
# snapshots makes finding the right one harder, which is the thing that actually costs
# time during a restore.
rclone --config "$RCLONE_CONFIG_PATH" delete --min-age "${KEEP_DAYS}d" \
    "${REMOTE_BASE}/backup/" 2>/dev/null || true
