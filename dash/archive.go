package dash

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Archiving a cold session to object storage, and reading it back.
//
// The invariant that matters, and the one thing to check if this code is ever
// changed: NOTHING IS DELETED LOCALLY UNTIL THE UPLOAD IS CONFIRMED PRESENT AND THE
// RIGHT SIZE. Upload, stat, compare, then delete. A failed upload leaves the local
// copy exactly where it was and the pass retries later, so the worst case is that
// disk is not reclaimed — never that history is gone.
//
// The format is gzipped JSONL, one Event per line, components and content embedded.
// Not a SQLite file and not a custom binary: an archive that outlives this schema
// has to be readable by something other than this program, and one JSON object per
// request is the shape a person can grep.

// RemoteName describes the CONFIGURED cold storage, or "" when none is configured.
//
// "Configured" is the load-bearing word, and it is the fix for a payload that lied.
// The host disables archiving when the boot reachability probe fails (it leaves
// Options.Remote nil), so this used to return "" for a deployment that is fully
// configured and merely unreachable — and the dashboard rendered "cold storage is not
// configured on this deployment" directly above two archived sessions. Options.RemoteName
// carries the configured name past a failed probe so the two facts stay separate; see
// RemoteReachable for the other half.
func (r *Recorder) RemoteName() string {
	if r == nil {
		return ""
	}
	if r.opts.RemoteName != "" {
		return r.opts.RemoteName
	}
	if r.remote == nil {
		return ""
	}
	return r.remote.Describe()
}

// RemoteReachable reports whether cold storage is USABLE right now — i.e. whether the
// host handed us a working Remote. False with a non-empty RemoteName means "configured
// but unreachable", which is a transient operational fact; false with an empty name
// means "no cold storage on this deployment", which is a permanent one. Rendering both
// as the same sentence is how an archive list ended up under "not configured".
func (r *Recorder) RemoteReachable() bool { return r != nil && r.remote != nil }

// ArchiveKind names what left the local database.
const (
	// ArchiveContent moved a session's transcripts out. Its metric rows stay local
	// and stay queryable — only the diff view has to reach for cold storage.
	ArchiveContent = "content"
	// ArchiveFull moved the whole session out. Its rows are gone locally; the
	// dashboard shows it from the archive index and fetches on demand.
	ArchiveFull = "full"
)

// ArchivedSession is one row of the cold-storage index.
type ArchivedSession struct {
	SessionID    string `json:"session_id"`
	TenantID     string `json:"tenant_id"`
	FirstTS      int64  `json:"first_ts"`
	LastTS       int64  `json:"last_ts"`
	Requests     int64  `json:"requests"`
	ContentPath  string `json:"content_path,omitempty"`
	ContentBytes int64  `json:"content_bytes,omitempty"`
	FullPath     string `json:"full_path,omitempty"`
	FullBytes    int64  `json:"full_bytes,omitempty"`
	ArchivedAt   int64  `json:"archived_at"`
	Remote       string `json:"remote,omitempty"`
}

// Archived reports whether the whole session is in cold storage.
func (a ArchivedSession) Archived() bool { return a.FullPath != "" }

// coldCandidate is a session eligible to move out, with the facts needed to name
// its object and decide whether it is worth moving.
type coldCandidate struct {
	SessionID string
	TenantID  string
	FirstTS   int64
	LastTS    int64
	Requests  int64
}

// coldSessions lists sessions whose last activity is older than idleBefore and
// which have not already been archived in the requested way.
//
// Ordered oldest-first so a pass that runs out of budget makes progress on the
// coldest data rather than nibbling at whatever the query happened to return.
func (d *DB) coldSessions(idleBefore int64, kind string, limit int) ([]coldCandidate, error) {
	var notYet string
	switch kind {
	case ArchiveContent:
		// Only sessions that still have content locally are worth a content pass.
		notYet = `AND EXISTS (SELECT 1 FROM request_content c WHERE c.request_id = r.id)
		          AND r.session_id NOT IN (SELECT session_id FROM archived_sessions WHERE content_path <> '')`
	case ArchiveFull:
		notYet = `AND r.session_id NOT IN (SELECT session_id FROM archived_sessions WHERE full_path <> '')`
	default:
		return nil, fmt.Errorf("dash: unknown archive kind %q", kind)
	}
	q := `SELECT r.session_id, MAX(r.tenant_id), MIN(r.ts), MAX(r.ts), COUNT(*)
	      FROM requests r WHERE r.session_id <> '' ` + notYet + `
	      GROUP BY r.session_id HAVING MAX(r.ts) < ?
	      ORDER BY MAX(r.ts) ASC LIMIT ?`
	rows, err := d.sql.Query(q, idleBefore, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []coldCandidate
	for rows.Next() {
		var c coldCandidate
		if err := rows.Scan(&c.SessionID, &c.TenantID, &c.FirstTS, &c.LastTS, &c.Requests); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// oldestLocalSessions lists the least-recently-active sessions still held locally,
// for the disk-pressure path — which archives rather than deletes.
func (d *DB) oldestLocalSessions(limit int) ([]coldCandidate, error) {
	return d.scanCandidates(
		`SELECT session_id, MAX(tenant_id), MIN(ts), MAX(ts), COUNT(*) FROM requests
		 GROUP BY session_id ORDER BY MAX(ts) ASC LIMIT ?`, limit)
}

// scanCandidates runs a candidate query and scans its rows.
func (d *DB) scanCandidates(q string, args ...any) ([]coldCandidate, error) {
	rows, err := d.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []coldCandidate
	for rows.Next() {
		var c coldCandidate
		if err := rows.Scan(&c.SessionID, &c.TenantID, &c.FirstTS, &c.LastTS, &c.Requests); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// oldestLocalSessionsOfTenant is oldestLocalSessions narrowed to one tenant, for the
// row-quota path — which archives rather than deletes, exactly like the disk path.
func (d *DB) oldestLocalSessionsOfTenant(tenant string, limit int) ([]coldCandidate, error) {
	return d.scanCandidates(
		`SELECT session_id, MAX(tenant_id), MIN(ts), MAX(ts), COUNT(*) FROM requests
		 WHERE tenant_id = ? GROUP BY session_id ORDER BY MAX(ts) ASC LIMIT ?`, tenant, limit)
}

// sessionEvents loads a session's full events, content included, for export. The
// archiver runs behind the tenant boundary rather than in front of it — it moves
// whatever the retention rules picked — so it reads with scoping off.
func (d *DB) sessionEvents(sessionID string) ([]*Event, error) {
	return d.SessionEvents(Filter{TenantAll: true}, sessionID, true)
}

// encodeArchive renders events as gzipped JSONL.
func encodeArchive(evs []*Event) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	enc := json.NewEncoder(zw)
	for _, e := range evs {
		if err := enc.Encode(e); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DecodeArchive parses a gzipped JSONL archive. Exported so an operator can read an
// object back with a few lines of Go, or so a future importer does not have to
// reverse-engineer the format.
func DecodeArchive(gz []byte) ([]*Event, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	dec := json.NewDecoder(zr)
	var out []*Event
	for dec.More() {
		var e Event
		if err := dec.Decode(&e); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, nil
}

// archivePath names an object. Tenant-first then year/month, so a tenant's data is
// contiguous (it can be handed over or deleted as a unit) and no single Box folder
// accumulates an unlistable number of children.
//
// Every interpolated segment is validated, and it returns an error rather than a
// best-effort path. All three inputs are trusted today — the tenant id and kind come
// from the control database and the schema, and client-supplied session ids are
// allow-listed at the root in session.Scoped — so this is defence in depth. It is worth
// four lines anyway: this function's entire job is building a remote path, and a path
// builder that cannot be talked into a "../" is one that stays correct when its next
// caller trusts something it should not.
func archivePath(c coldCandidate, kind string) (string, error) {
	if kind != ArchiveContent && kind != ArchiveFull {
		return "", fmt.Errorf("dash: unknown archive kind %q", kind)
	}
	tenant := c.TenantID
	if tenant == "" {
		tenant = "_single" // single-tenant deployments still need one stable folder
	}
	if !safeSegment(tenant) || !safeSegment(c.SessionID) {
		return "", fmt.Errorf("dash: refusing to build an archive path from tenant %q session %q",
			tenant, c.SessionID)
	}
	t := time.UnixMilli(c.LastTS).UTC()
	return fmt.Sprintf("archive/%s/%04d/%02d/%s.%s.jsonl.gz",
		tenant, t.Year(), int(t.Month()), c.SessionID, kind), nil
}

// safeSegment reports whether s may be one component of a remote path: an allow-list,
// like session.safeExplicit, because a denylist of separators is a guess about every
// storage backend's parser. All-dots names are out too — "." and ".." are the two names
// a path actually interprets.
func safeSegment(s string) bool {
	if s == "" || len(s) > 128 || strings.Trim(s, ".") == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == ':' || c == '-':
		default:
			return false
		}
	}
	return true
}

// ArchiveSessionContent moves one session's transcripts to cold storage and deletes
// them locally, leaving its metric rows queryable. This is the big win on disk:
// content is the overwhelming majority of the bytes and is read rarely.
func (r *Recorder) ArchiveSessionContent(ctx context.Context, c coldCandidate) (int64, error) {
	if r.remote == nil {
		return 0, errors.New("dash: no cold storage configured")
	}
	evs, err := r.db.sessionEvents(c.SessionID)
	if err != nil {
		return 0, err
	}
	// Keep only the content; the metrics stay local, so shipping them twice would
	// just make the object bigger for nothing.
	slim := make([]*Event, 0, len(evs))
	total := 0
	for _, e := range evs {
		if len(e.Content) == 0 {
			continue
		}
		total += len(e.Content)
		slim = append(slim, &Event{ID: e.ID, TS: e.TS, TenantID: e.TenantID,
			SessionID: e.SessionID, Content: e.Content})
	}
	if total == 0 {
		return 0, nil
	}
	blob, err := encodeArchive(slim)
	if err != nil {
		return 0, err
	}
	path, err := archivePath(c, ArchiveContent)
	if err != nil {
		return 0, err
	}
	if err := r.putVerified(ctx, path, blob); err != nil {
		return 0, err
	}
	// Only now is it safe to drop the local copy.
	res, err := r.db.sql.Exec(`DELETE FROM request_content WHERE request_id IN (
		SELECT id FROM requests WHERE session_id = ?)`, c.SessionID)
	if err != nil {
		// The object is uploaded but the local delete failed. Harmless and
		// self-correcting: the index row below is not written, so the next pass
		// re-uploads and retries the delete.
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := r.db.markArchived(c, ArchiveContent, path, int64(len(blob)), r.remote.Describe()); err != nil {
		return n, err
	}
	return n, nil
}

// ArchiveSessionFull moves a whole session to cold storage and deletes its local
// rows. Used by the age rule for genuinely old sessions and by the disk-pressure
// rule, where it replaces outright deletion — which is the point of this whole
// tier: eviction becomes migration.
func (r *Recorder) ArchiveSessionFull(ctx context.Context, c coldCandidate) (int64, error) {
	if r.remote == nil {
		return 0, errors.New("dash: no cold storage configured")
	}
	evs, err := r.db.sessionEvents(c.SessionID)
	if err != nil {
		return 0, err
	}
	if len(evs) == 0 {
		return 0, nil
	}
	blob, err := encodeArchive(evs)
	if err != nil {
		return 0, err
	}
	path, err := archivePath(c, ArchiveFull)
	if err != nil {
		return 0, err
	}
	if err := r.putVerified(ctx, path, blob); err != nil {
		return 0, err
	}
	res, err := r.db.sql.Exec(`DELETE FROM requests WHERE session_id = ?`, c.SessionID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := r.db.markArchived(c, ArchiveFull, path, int64(len(blob)), r.remote.Describe()); err != nil {
		return n, err
	}
	return n, nil
}

// putVerified uploads and then confirms the object is there at the expected size.
// This is the gate in front of every local delete. A Put that returns nil is not
// proof on its own — a truncated upload, a proxy that swallowed the body, a remote
// that accepted and dropped it all look like success from the writer's side.
func (r *Recorder) putVerified(ctx context.Context, path string, blob []byte) error {
	if err := r.remote.Put(ctx, path, blob); err != nil {
		return fmt.Errorf("upload %s: %w", path, err)
	}
	size, err := r.remote.Size(ctx, path)
	if err != nil {
		return fmt.Errorf("confirm %s: %w", path, err)
	}
	if size != int64(len(blob)) {
		return fmt.Errorf("confirm %s: uploaded %d bytes, remote reports %d — refusing to delete the local copy",
			path, len(blob), size)
	}
	return nil
}

// markArchived records or updates the index row for a session.
func (d *DB) markArchived(c coldCandidate, kind, path string, size int64, remote string) error {
	now := time.Now().UnixMilli()
	if _, err := d.sql.Exec(`INSERT INTO archived_sessions
	  (session_id,tenant_id,first_ts,last_ts,requests,archived_at,remote)
	  VALUES (?,?,?,?,?,?,?)
	  ON CONFLICT(session_id) DO UPDATE SET
	    last_ts=MAX(last_ts,excluded.last_ts), requests=MAX(requests,excluded.requests),
	    archived_at=excluded.archived_at, remote=excluded.remote`,
		c.SessionID, c.TenantID, c.FirstTS, c.LastTS, c.Requests, now, remote); err != nil {
		return err
	}
	col := "content_path"
	sizeCol := "content_bytes"
	if kind == ArchiveFull {
		col, sizeCol = "full_path", "full_bytes"
	}
	_, err := d.sql.Exec(
		`UPDATE archived_sessions SET `+col+` = ?, `+sizeCol+` = ? WHERE session_id = ?`,
		path, size, c.SessionID)
	return err
}

// ArchivedSessions lists the cold-storage index for a tenant, newest first. Served
// from the LOCAL index, so browsing history costs nothing and works even when the
// remote is unreachable.
func (d *DB) ArchivedSessions(f Filter, limit int) ([]ArchivedSession, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT session_id,tenant_id,first_ts,last_ts,requests,content_path,content_bytes,
	      full_path,full_bytes,archived_at,remote FROM archived_sessions`
	var args []any
	if !f.TenantAll {
		q += ` WHERE tenant_id = ?`
		args = append(args, f.Tenant)
	}
	q += ` ORDER BY last_ts DESC LIMIT ?`
	args = append(args, limit)
	rows, err := d.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ArchivedSession
	for rows.Next() {
		var a ArchivedSession
		if err := rows.Scan(&a.SessionID, &a.TenantID, &a.FirstTS, &a.LastTS, &a.Requests,
			&a.ContentPath, &a.ContentBytes, &a.FullPath, &a.FullBytes,
			&a.ArchivedAt, &a.Remote); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ArchivedSessionByID reads one index row.
func (d *DB) ArchivedSessionByID(sessionID string) (ArchivedSession, error) {
	var a ArchivedSession
	err := d.sql.QueryRow(`SELECT session_id,tenant_id,first_ts,last_ts,requests,
	  content_path,content_bytes,full_path,full_bytes,archived_at,remote
	  FROM archived_sessions WHERE session_id = ?`, sessionID).Scan(
		&a.SessionID, &a.TenantID, &a.FirstTS, &a.LastTS, &a.Requests,
		&a.ContentPath, &a.ContentBytes, &a.FullPath, &a.FullBytes, &a.ArchivedAt, &a.Remote)
	return a, err
}

// FetchArchived reads a session back from cold storage WITHOUT reinserting it. A
// read-only restore: the dashboard can show an archived session without dragging it
// back into the hot tier and re-triggering the eviction that put it there.
func (r *Recorder) FetchArchived(ctx context.Context, sessionID string) ([]*Event, error) {
	if r.remote == nil {
		return nil, errors.New("dash: no cold storage configured")
	}
	a, err := r.db.ArchivedSessionByID(sessionID)
	if err != nil {
		return nil, err
	}
	path := a.FullPath
	if path == "" {
		path = a.ContentPath
	}
	if path == "" {
		return nil, ErrRemoteMissing
	}
	blob, err := r.remote.Get(ctx, path)
	if err != nil {
		return nil, err
	}
	return DecodeArchive(blob)
}

// FetchArchivedContent returns the archived transcripts for one request, for the
// diff view of a session whose content has moved out.
func (r *Recorder) FetchArchivedContent(ctx context.Context, sessionID string, requestID int64) ([]ContentRow, error) {
	evs, err := r.FetchArchived(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	for _, e := range evs {
		if e.ID == requestID {
			return e.Content, nil
		}
	}
	return nil, ErrRemoteMissing
}

// archiveIdle runs the age-based passes: content first, then whole sessions once
// they are much older. Called from the archiver goroutine, never from the writer —
// these are network round trips measured in seconds, and the writer goroutine owes
// the request path a fast insert.
func (r *Recorder) archiveIdle(ctx context.Context) {
	if r.remote == nil {
		return
	}
	now := time.Now()
	budget := r.opts.ArchiveBatch
	if budget <= 0 {
		budget = defaultArchiveBatch
	}

	// Content first. This is where the bytes are, so it is what keeps the local
	// database small enough that the disk rule never has to fire.
	if age := r.opts.ArchiveContentAfter; age > 0 {
		cands, err := r.db.coldSessions(now.Add(-age).UnixMilli(), ArchiveContent, budget)
		if err != nil {
			slog.Warn("dash: could not list sessions for content archiving", "err", err)
		}
		var moved, rows int64
		for _, c := range cands {
			if ctx.Err() != nil {
				return
			}
			n, err := r.ArchiveSessionContent(ctx, c)
			if err != nil {
				// Fail soft: the local copy is untouched, so the only cost is that disk
				// was not reclaimed this pass.
				slog.Warn("dash: content archiving failed; keeping the local copy",
					"session", c.SessionID, "err", err)
				continue
			}
			if n > 0 {
				moved++
				rows += n
			}
		}
		if moved > 0 {
			slog.Info("dash: archived session transcripts to cold storage",
				"sessions", moved, "content_rows", rows, "remote", r.remote.Describe())
		}
	}

	// Then whole sessions, for history old enough that nobody is browsing it.
	if age := r.opts.ArchiveSessionAfter; age > 0 {
		cands, err := r.db.coldSessions(now.Add(-age).UnixMilli(), ArchiveFull, budget)
		if err != nil {
			slog.Warn("dash: could not list sessions for archiving", "err", err)
		}
		var moved, rows int64
		for _, c := range cands {
			if ctx.Err() != nil {
				return
			}
			n, err := r.ArchiveSessionFull(ctx, c)
			if err != nil {
				slog.Warn("dash: session archiving failed; keeping the local copy",
					"session", c.SessionID, "err", err)
				continue
			}
			moved++
			rows += n
		}
		if moved > 0 {
			slog.Info("dash: archived whole sessions to cold storage",
				"sessions", moved, "requests", rows, "remote", r.remote.Describe())
		}
	}
}
