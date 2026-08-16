package dash

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"
)

// Disk-pressure eviction: the LRU that keeps a hosted deployment from filling the
// host's filesystem.
//
// Three rules run in order on every janitor pass, and the order is the design:
//
//  1. Per-tenant row quota — FAIRNESS first. One heavy user must be trimmed to its
//     own quota before anyone else's history is considered, otherwise the person
//     causing the pressure is the one whose data survives.
//  2. Age (in DB.Prune) — a bound nobody has to think about.
//  3. Filesystem pressure — the backstop, measured against the WHOLE filesystem
//     rather than our own bytes, because on a shared box most of the usage is
//     somebody else's and a self-referential budget cannot see the host filling up.
//
// Everything evicts whole SESSIONS, oldest last-activity first. A session is the
// unit a user reasons about; evicting individual requests leaves conversations with
// missing turns and totals that do not add up.

// diskWatermarks resolves the configured watermarks, applying defaults and
// rejecting a nonsensical pair rather than acting on it.
func (r *Recorder) diskWatermarks() (high, low float64, enabled bool) {
	high, low = r.opts.DiskHighWatermark, r.opts.DiskLowWatermark
	if high < 0 {
		return 0, 0, false // explicitly disabled
	}
	if high == 0 {
		high = defaultDiskHigh
	}
	if low == 0 {
		low = defaultDiskLow
	}
	// A low watermark at or above the high one would make the loop exit immediately
	// (no eviction, silently) or never (evict everything). Refuse both.
	if low >= high || high >= 1 {
		slog.Warn("dash: ignoring nonsensical disk watermarks",
			"high", high, "low", low)
		return 0, 0, false
	}
	return high, low, true
}

// minKeepBytes floors how small the disk rule will make this database.
func (r *Recorder) minKeepBytes() int64 {
	if r.opts.MinKeepBytes > 0 {
		return r.opts.MinKeepBytes
	}
	return defaultMinKeepBytes
}

// SetTenantQuota supplies the per-tenant row quota a manager set in the control plane.
// Returning 0 means "this tenant has no quota of its own", which falls back to
// Options.MaxRowsPerTenant. Wired by the host after the control database is open (the
// recorder starts first), and left nil in single-tenant mode.
func (r *Recorder) SetTenantQuota(fn func(tenantID string) int64) {
	if r == nil {
		return
	}
	r.tenantQuota.Store(&fn)
}

// quotaFor resolves one tenant's effective row quota: the value a manager set for it,
// else the server-wide default. 0 means unlimited.
func (r *Recorder) quotaFor(tenantID string) int64 {
	if fn := r.tenantQuota.Load(); fn != nil && tenantID != "" {
		if n := (*fn)(tenantID); n > 0 {
			return n
		}
	}
	return r.opts.MaxRowsPerTenant
}

// enforceQuotas trims any tenant over its row quota back to it.
func (r *Recorder) enforceQuotas() {
	// Nothing configured at all: skip the count query entirely, which is what a
	// single-tenant proxy does on every pass.
	if r.opts.MaxRowsPerTenant <= 0 && r.tenantQuota.Load() == nil {
		return
	}
	counts, err := r.db.TenantRowCounts()
	if err != nil {
		slog.Warn("dash: tenant quota check failed", "err", err)
		return
	}
	for id, rows := range counts {
		quota := r.quotaFor(id)
		if quota <= 0 || rows <= quota {
			// Per tenant, so a "why was I trimmed / why was I not" question has an answer
			// with both numbers in it. DEBUG: it is one line per tenant per pass.
			slog.Debug("dash: tenant is within its row quota", "tenant", id,
				"rows", rows, "quota", quota)
			continue
		}
		n, err := r.trimTenantToQuota(id, quota)
		if err != nil {
			slog.Warn("dash: tenant quota trim failed", "tenant", id, "err", err)
			continue
		}
		if n > 0 {
			slog.Info("dash: trimmed a tenant to its row quota",
				"tenant", id, "was_rows", rows, "quota", quota, "requests_evicted", n)
		}
	}
}

// trimTenantToQuota brings one tenant back to its quota, oldest session first, using
// the same MIGRATION the disk rule uses: with cold storage configured each session is
// uploaded and verified before its local rows go. A row quota is a bound on local
// disk, not a retention policy, so hitting it must not destroy history the remote
// could have held — and it silently did, which was the second half of the bug.
func (r *Recorder) trimTenantToQuota(tenant string, quota int64) (int64, error) {
	if r.remote == nil {
		return r.db.DropOldestSessionsOfTenant(tenant, quota)
	}
	var evicted int64
	// Bounded much tighter than the delete-only path, because each pass here is an
	// rclone round trip on the WRITER goroutine. A tenant far over quota is brought
	// most of the way back now and the rest on the next pass, five minutes later —
	// which is the right trade against stalling every tenant's inserts.
	// ponytail: one session per pass; batch them if a tenant is routinely 10k+ rows over.
	for pass := 0; pass < 16; pass++ {
		rows, err := r.db.tenantRowCount(tenant)
		if err != nil || rows <= quota {
			return evicted, err
		}
		cands, err := r.db.oldestLocalSessionsOfTenant(tenant, 1)
		if err != nil || len(cands) == 0 {
			return evicted, err
		}
		n, err := r.evictSessions(cands)
		if err != nil {
			return evicted, err
		}
		if n == 0 {
			// Nothing moved and nothing deleted — ArchiveRequired with an unreachable
			// remote. Stop rather than spin; the next pass tries again.
			return evicted, nil
		}
		evicted += n
	}
	return evicted, nil
}

// relieveDiskPressure evicts the oldest sessions while the filesystem holding the
// database is above the high watermark, stopping at the low one.
func (r *Recorder) relieveDiskPressure() {
	high, low, ok := r.diskWatermarks()
	if !ok {
		return
	}
	path := r.db.Path()
	if path == "" || path == ":memory:" {
		return // nothing on disk to relieve
	}
	dir := filepath.Dir(path)

	used, ok := r.probeDisk(dir)
	if !ok || used < high {
		// The commonest janitor question is "why did it not evict anything?", and this
		// silent return is nearly always the answer. Say so, with the numbers that decided
		// it — including probed=false, which means the statfs failed and the rule is not
		// running at all rather than deciding there is room.
		slog.Debug("dash: disk pass took no action", "probed", ok,
			"used_frac", used, "high", high, "low", low, "dir", dir)
		return
	}
	floor := r.minKeepBytes()
	slog.Warn("dash: filesystem above the high watermark; evicting oldest sessions",
		"used_frac", used, "high", high, "low", low, "dir", dir)

	var deleted int64
	// Bounded passes: this runs on the writer goroutine, so it must always give the
	// insert path back its turn even if the disk never recovers.
	for pass := 0; pass < 32; pass++ {
		size, err := r.db.sizeBytes()
		if err != nil {
			slog.Warn("dash: could not size the database", "err", err)
			return
		}
		if size <= floor {
			// The right thing to say out loud. If the filesystem is full because of
			// something other than us, deleting our last megabyte helps nobody, and a
			// blank dashboard would hide the real problem rather than show it.
			slog.Warn("dash: at the retention floor and the filesystem is still full; "+
				"the pressure is not coming from the dashboard",
				"used_frac", used, "size_bytes", size, "floor_bytes", floor,
				"deleted_requests", deleted)
			return
		}
		var sessions int64
		if err := r.db.sql.QueryRow(
			`SELECT COUNT(DISTINCT session_id) FROM requests`).Scan(&sessions); err != nil {
			slog.Warn("dash: could not count sessions", "err", err)
			return
		}
		drop := int(sessions / 10)
		if drop < 1 {
			drop = 1
		}
		n, err := r.evictOldestSessions(drop)
		if err != nil {
			slog.Warn("dash: session eviction failed", "err", err)
			return
		}
		if n == 0 {
			slog.Warn("dash: nothing left to evict but the filesystem is still full",
				"used_frac", used)
			return
		}
		deleted += n
		if err := r.db.reclaim(); err != nil {
			slog.Warn("dash: reclaiming freed pages failed", "err", err)
			return
		}
		if used, ok = r.probeDisk(dir); !ok {
			return
		}
		if used <= low {
			slog.Info("dash: disk pressure relieved",
				"used_frac", used, "low", low, "evicted_sessions", drop*(pass+1),
				"deleted_requests", deleted)
			return
		}
	}
	slog.Warn("dash: still above the low watermark after the pass budget; will retry next cycle",
		"used_frac", used, "deleted_requests", deleted)
}

// probeDisk reads filesystem usage, honouring a test override.
func (r *Recorder) probeDisk(dir string) (float64, bool) {
	if r.diskProbe != nil {
		return r.diskProbe(dir)
	}
	return diskUsage(dir)
}

// evictOldestSessions frees local space by moving the oldest sessions out. With cold
// storage configured this is a MIGRATION — upload, verify, then delete — so the disk
// rule stops being destructive and history is bounded by the remote instead of by
// this filesystem. That is the whole reason the cold tier exists.
//
// Without cold storage, or when the remote refuses, it falls back to deleting. That
// is a deliberate and uncomfortable choice: a full filesystem takes down every user's
// agent, which is worse than losing old metrics. ArchiveRequired inverts it for an
// operator who would rather risk the disk than lose a row, and either way the loss is
// logged in terms that say exactly what happened.
func (r *Recorder) evictOldestSessions(n int) (int64, error) {
	if r.remote == nil {
		return r.db.DropOldestSessions(n)
	}
	cands, err := r.db.oldestLocalSessions(n)
	if err != nil {
		return 0, err
	}
	return r.evictSessions(cands)
}

// evictSessions migrates the given sessions out of the local database: upload, verify,
// delete — falling back to deletion per evictOldestSessions' contract. Shared by the
// disk rule and the row-quota rule so there is one place that decides whether losing
// history is acceptable.
func (r *Recorder) evictSessions(cands []coldCandidate) (int64, error) {
	// A bounded context: this runs on the writer goroutine (the disk rule is part of
	// the prune pass), so it must not sit in rclone for minutes while inserts queue.
	// Under pressure, reclaiming SOME space now beats reclaiming all of it later.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var freed int64
	for _, c := range cands {
		moved, err := r.ArchiveSessionFull(ctx, c)
		if err == nil {
			freed += moved
			continue
		}
		slog.Warn("dash: could not archive a session being evicted",
			"session", c.SessionID, "remote", r.remote.Describe(), "err", err)
		if r.opts.ArchiveRequired {
			// Refuse to lose it. The caller sees no progress and the filesystem stays
			// full, which is what this option asks for.
			continue
		}
		res, derr := r.db.sql.Exec(`DELETE FROM requests WHERE session_id = ?`, c.SessionID)
		if derr != nil {
			return freed, derr
		}
		d, _ := res.RowsAffected()
		freed += d
		// Not a warning about a retry — a statement that data is gone. Anyone reading
		// this log needs to know the archive did not happen.
		slog.Error("dash: DELETED a session that could not be archived; this history is lost",
			"session", c.SessionID, "tenant", c.TenantID, "requests", d,
			"hint", "set --archive-required to keep data and let the disk fill instead")
	}
	return freed, nil
}
