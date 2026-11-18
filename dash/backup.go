package dash

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// The backup copy (#1204).
//
// On 2026-11-16 the size rule deleted a laptop's whole history and nothing could undo it. The
// janitor now keeps TWO copies of the database, `<db>.bak` (newest) and `<db>.bak.1` (the one
// before), and makes a new one before a pass deletes rows by the age, size or quota rule.
//
//   - Two copies. A new copy moves the current one to `.bak.1`, so one bad copy never leaves
//     the user with nothing. Both count toward disk: the backup takes up to TWICE the database
//     size on disk.
//   - A new copy only when the current one is older than backupMaxAge. A series of bad passes
//     (the incident was six in 25 minutes) must not overwrite the good copy with a bad one.
//   - The disk-pressure rule makes none. A copy needs disk, and that rule runs because the disk
//     is full.
//   - The copy holds stored data, content rows included when content capture is on. It is as
//     sensitive as the database itself, so it is written with mode 0600. docs/data-dependence.md
//     has the row.
//   - The user restores a copy by hand: stop the proxy, replace the database file with it.

// backupMaxAge is how old the copy may get before the janitor makes a new one. A var so a test can
// change it.
var backupMaxAge = 24 * time.Hour

// backupPath names the copy of the database at path.
func backupPath(path string) string { return path + ".bak" }

// backupTo writes a consistent copy of the database to dst with VACUUM INTO. It writes to a temp
// name and renames, so a copy that stopped half way never takes the place of a good one.
func (d *DB) backupTo(dst string) error {
	// The copy is made inside a private directory (MkdirTemp creates it with mode 0700), so no
	// other local user can read it while VACUUM INTO writes it with the default file mode. The
	// directory is next to the database, so the final rename stays on one filesystem.
	dir, err := os.MkdirTemp(filepath.Dir(dst), ".cg-backup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	tmp := filepath.Join(dir, "copy.db")
	if _, err := d.sql.Exec(`VACUUM INTO ?`, tmp); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	// Rotate: the current copy becomes <db>.bak.1 (replacing the older one) only now, when the new
	// copy is complete. A failed copy above never touches either file.
	prev := dst + ".1"
	rotated := false
	if _, err := os.Stat(dst); err == nil {
		if err := os.Rename(dst, prev); err != nil {
			return err
		}
		rotated = true
	}
	if err := os.Rename(tmp, dst); err != nil {
		if rotated {
			os.Rename(prev, dst) // put the current copy back
		}
		return err
	}
	return nil
}

// backupDue is THE backup rule: it says whether a new copy must be made now. Keep the rule in
// this one function so it is easy to change. Now: a copy is due when none exists or the
// current one is older than backupMaxAge.
func backupDue(dst string, now time.Time) bool {
	fi, err := os.Stat(dst)
	return err != nil || now.Sub(fi.ModTime()) >= backupMaxAge
}

// ensureBackup makes sure a copy younger than backupMaxAge exists, and reports whether one does.
// It returns true without a copy when the backup is switched off or the database is in memory:
// there is nothing to protect or no way to do it, and the rules keep their old behavior. It
// returns false only when a copy was needed and could not be made. The size rule treats false as
// "do not delete in this pass"; the other rules go on (fail open).
func (r *Recorder) ensureBackup() bool {
	path := r.db.Path()
	if r.opts.DisableBackup || path == "" || path == ":memory:" || r.db.readOnly {
		return true
	}
	dst := backupPath(path)
	if !backupDue(dst, time.Now()) {
		return true
	}
	start := time.Now()
	if err := r.db.backupTo(dst); err != nil {
		slog.Warn("dash: could not make the backup copy before deleting rows",
			"path", dst, "err", err)
		return false
	}
	var size int64
	if fi, err := os.Stat(dst); err == nil {
		size = fi.Size()
	}
	slog.Info("dash: made a backup copy before deleting rows",
		"path", dst, "bytes", size, "took", fmt.Sprint(time.Since(start).Round(time.Millisecond)))
	return true
}
