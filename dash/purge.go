package dash

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// Erasing one tenant's observability data.
//
// This is the half of "delete a user" that the control database cannot do. Accounts live
// in tenant/ (their own file, real migrations, ON DELETE CASCADE across tokens, sessions
// and agent keys); requests live HERE, in a different file with no foreign key reaching
// across. So deleting a tenant row removes their credentials and leaves their traffic:
// request rows, component rows, captured transcripts, the monthly spend rollup, and
// whatever has already been uploaded to cold storage under archive/<tenant>/.
//
// Three properties this function is built around.
//
//   - COLD STORAGE FIRST. The archived_sessions index is the ONLY record of what is in
//     the bucket, so deleting the index before the objects orphans them permanently —
//     still costing storage, no longer findable by anything. Objects go first, and an
//     index row whose object could not be removed is KEPT so a retry can find it.
//   - CHILD ROWS EXPLICITLY, not only by ON DELETE CASCADE. The cascade is real and
//     enabled, but it depends on a per-connection pragma, and "did we leave orphans"
//     should not be a question whose answer is a DSN. Deleting children first also means
//     a failure part-way through leaves fewer, not more, danglers.
//   - POINT IN TIME. Capture is asynchronous (a buffered channel and a 250 ms flush), so a
//     request that was in flight when the purge ran can land a moment after it. Callers
//     that are deleting the ACCOUNT run the purge again after the account row is gone —
//     by then nothing can authenticate, so the second pass is final. See proxy's
//     ctlDeleteTenant.

// PurgeResult counts what a purge removed, so the caller can report it and so a manager
// pressing an irreversible button sees what it did rather than a bare "ok".
type PurgeResult struct {
	Requests   int64 `json:"requests"`
	Components int64 `json:"components"`
	Content    int64 `json:"content"`
	// Archives is index rows removed; Objects is objects deleted from cold storage.
	Archives int64 `json:"archives"`
	Objects  int64 `json:"objects"`
	// ObjectErrors names the cold-storage objects that could NOT be deleted. Their index
	// rows are deliberately left behind so a later purge retries them, which is also why
	// this is a list of paths rather than a count: an operator has to be able to go and
	// look.
	ObjectErrors []string `json:"object_errors,omitempty"`
	// SpendRows is monthly rollup rows removed.
	SpendRows int64 `json:"spend_rows"`
}

// Removed reports whether anything at all was deleted.
func (p PurgeResult) Removed() bool {
	return p.Requests+p.Components+p.Content+p.Archives+p.Objects+p.SpendRows > 0
}

// ErrPurgeNoTenant refuses a purge with no tenant named.
//
// "" is a LEGITIMATE tenant id in this schema — it is what every single-tenant and
// pre-tenancy row carries — so an empty argument would not be a no-op, it would erase the
// deployment's own history. A caller reaching here with "" has a bug, and the bug should
// not be destructive.
var ErrPurgeNoTenant = errors.New("dash: refusing to purge with no tenant id")

// PurgeTenant deletes everything this database and its cold storage hold for one tenant.
// It does NOT touch the control database — the caller owns the account row.
//
// Partial progress is real and intentional: every step reports what it removed even when a
// later one fails, so a caller can log what happened and retry. Retrying is safe; every
// statement here is idempotent.
func (r *Recorder) PurgeTenant(ctx context.Context, tenantID string) (PurgeResult, error) {
	var out PurgeResult
	if r == nil || r.db == nil {
		return out, nil
	}
	if tenantID == "" {
		return out, ErrPurgeNoTenant
	}

	// 1. Cold storage. Listed from the local index with scoping ON (TenantAll would hand
	// this pass every tenant's archives), and the objects go before their index rows.
	rows, err := r.db.ArchivedSessions(Filter{Tenant: tenantID}, 500)
	if err != nil {
		return out, err
	}
	kept := map[string]bool{} // sessions whose object survived: their index row stays
	for _, a := range rows {
		for _, path := range []string{a.ContentPath, a.FullPath} {
			if path == "" {
				continue
			}
			if r.remote == nil {
				// Configured cold storage that is unreachable right now, or none at all. Either
				// way the object cannot be removed, and saying so beats deleting the only
				// pointer to it.
				kept[a.SessionID] = true
				out.ObjectErrors = append(out.ObjectErrors,
					path+" (cold storage is not reachable from this process)")
				continue
			}
			if derr := r.remote.Delete(ctx, path); derr != nil {
				kept[a.SessionID] = true
				out.ObjectErrors = append(out.ObjectErrors, fmt.Sprintf("%s: %v", path, derr))
				continue
			}
			out.Objects++
		}
	}
	for _, a := range rows {
		if kept[a.SessionID] {
			continue
		}
		res, derr := r.db.sql.ExecContext(ctx,
			`DELETE FROM archived_sessions WHERE session_id = ? AND tenant_id = ?`,
			a.SessionID, tenantID)
		if derr != nil {
			return out, derr
		}
		n, _ := res.RowsAffected()
		out.Archives += n
	}

	// 2. Local rows. Children first, then the requests they hang off, then the spend
	// rollup — which retention never deletes, so nothing else would ever remove it.
	for _, step := range []struct {
		into *int64
		sql  string
	}{
		{&out.Content, `DELETE FROM request_content WHERE request_id IN
		                (SELECT id FROM requests WHERE tenant_id = ?)`},
		{&out.Components, `DELETE FROM request_components WHERE request_id IN
		                   (SELECT id FROM requests WHERE tenant_id = ?)`},
		{&out.Requests, `DELETE FROM requests WHERE tenant_id = ?`},
		{&out.SpendRows, `DELETE FROM tenant_spend WHERE tenant_id = ?`},
	} {
		res, derr := r.db.sql.ExecContext(ctx, step.sql, tenantID)
		if derr != nil {
			return out, derr
		}
		n, _ := res.RowsAffected()
		*step.into += n
	}

	// Hand the freed pages back. A purge is the one deletion big enough to be worth
	// reclaiming immediately — it is usually run because something has to go away, and
	// leaving the bytes in the file makes "purged" look untrue to anyone watching the disk.
	if out.Requests > 0 {
		if err := r.db.reclaim(); err != nil {
			slog.Warn("dash: purged a tenant but could not reclaim the pages", "err", err)
		}
	}
	if len(out.ObjectErrors) > 0 {
		return out, fmt.Errorf("dash: %d cold-storage object(s) could not be deleted: %v",
			len(out.ObjectErrors), out.ObjectErrors)
	}
	return out, nil
}

// TenantHasRows reports whether any local row still names this tenant. Used by the
// deletion path's own check, and by its test: "no orphans left" is the property, and a
// caller that cannot ask cannot assert it.
func (d *DB) TenantHasRows(tenantID string) (bool, error) {
	var n int64
	err := d.sql.QueryRowContext(d.readCtx(), `SELECT
	  (SELECT COUNT(*) FROM requests WHERE tenant_id = ?) +
	  (SELECT COUNT(*) FROM archived_sessions WHERE tenant_id = ?) +
	  (SELECT COUNT(*) FROM tenant_spend WHERE tenant_id = ?)`,
		tenantID, tenantID, tenantID).Scan(&n)
	return n > 0, err
}

// OrphanRows counts child rows whose request is gone — component rows and content rows
// with no parent. It is a WHOLE-TABLE check, not per tenant, because that is the failure
// it exists to catch: a delete that took the parents and left the children belonging to
// nobody, which no tenant-scoped query can see.
func (d *DB) OrphanRows() (components, content int64, err error) {
	if err = d.sql.QueryRowContext(d.readCtx(), `SELECT COUNT(*) FROM request_components c
	  WHERE NOT EXISTS (SELECT 1 FROM requests r WHERE r.id = c.request_id)`).Scan(&components); err != nil {
		return 0, 0, err
	}
	err = d.sql.QueryRowContext(d.readCtx(), `SELECT COUNT(*) FROM request_content c
	  WHERE NOT EXISTS (SELECT 1 FROM requests r WHERE r.id = c.request_id)`).Scan(&content)
	return components, content, err
}
