#!/usr/bin/env bash
# Back up the context-guru CONTROL database to Box.
#
# This is the one irreplaceable file: tenants, token hashes, per-tenant configuration,
# the audit trail. The metrics database is a derived view and is not backed up — under
# pressure it archives itself to Box session by session, and anything it loses beyond
# that is reconstructible from nothing that matters.
#
# Uses SQLite's ONLINE BACKUP api, not `cp`. Copying a live SQLite file while the service
# is writing yields a torn snapshot that may not open at all — and a backup you discover
# is unreadable at restore time is worse than no backup, because you stopped worrying.
#
# Driven through python3's sqlite3 module rather than the `sqlite3` CLI. The CLI is NOT
# part of a minimal RHEL install and was absent on the first host this ran on, so the
# timer failed every night with `sqlite3: command not found` (exit 127) and the control
# database — tenants, token hashes, per-tenant config, the audit trail — went unbacked-up
# with no symptom except a line in the journal nobody reads. python3 is already a hard
# requirement of install.sh and of the proxy's own helper scripts, and
# Connection.backup() is the same online-backup API the CLI's `.backup` calls.
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

# A consistent snapshot of a live database, WAL included.
if ! python3 -c '
import sqlite3, sys
src, dst = sqlite3.connect(sys.argv[1]), sqlite3.connect(sys.argv[2])
src.backup(dst)
dst.close(); src.close()
' "$CONTROL_DB" "$TMP"; then
  echo "backup: could not snapshot $CONTROL_DB — REFUSING to upload" >&2
  exit 1
fi
# Prove it opens and the tenant table is intact before shipping it anywhere. A backup
# that is verified only at restore time is a backup nobody has verified. Read-only, and
# through a second connection, so this checks the FILE rather than the handle that wrote it.
COUNT="$(python3 -c '
import sqlite3, sys
c = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True)
print(c.execute("SELECT COUNT(*) FROM tenants").fetchone()[0])
' "$TMP" 2>/dev/null || echo FAIL)"
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
