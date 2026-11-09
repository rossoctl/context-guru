package dash

import (
	"database/sql"
	"net/http"
)

// LimitWindow is one subscription limit window (5 hours or 7 days), as the provider's
// anthropic-ratelimit-unified-* response headers gave it on one answer (#984). A nil field and
// an empty Status mean the header was missing or did not parse: it is left out, never read as 0.
type LimitWindow struct {
	// Utilization is the used share of the window: a 0..1 fraction as the header gives it, in steps of 0.01.
	Utilization *float64 `json:"utilization,omitempty"`
	// ResetS is when the window resets, in unix seconds.
	ResetS *int64 `json:"reset_s,omitempty"`
	// Status is the provider's word for the window, such as "allowed" or "rejected".
	Status string `json:"status,omitempty"`
}

// Empty is true when the window has no value.
func (w LimitWindow) Empty() bool { return w.Utilization == nil && w.ResetS == nil && w.Status == "" }

// Limits are the subscription limit values of one upstream answer. Numbers and short status
// words only: metadata, the same at every storage level.
type Limits struct {
	FiveHour LimitWindow `json:"five_hour"`
	SevenDay LimitWindow `json:"seven_day"`
}

// Empty is true when no header gave a value.
func (l Limits) Empty() bool { return l.FiveHour.Empty() && l.SevenDay.Empty() }

// limitCols are the requests columns that hold Event.Limits, in insert order (see store.go).
// Nullable with no default: NULL is "the header was missing", which must stay apart from a 0.
// limit_seen is 1 on a row with at least one value, so the partial index holds only those rows.
const limitCols = `limit_seen, limit_5h_utilization, limit_5h_reset, limit_5h_status,
		limit_7d_utilization, limit_7d_reset, limit_7d_status`

// limitArgs are the insert values for limitCols.
func limitArgs(l *Limits) []any {
	if l == nil || l.Empty() {
		return []any{0, nil, nil, nil, nil, nil, nil}
	}
	win := func(w LimitWindow) []any {
		var u, r, s any
		if w.Utilization != nil {
			u = *w.Utilization
		}
		if w.ResetS != nil {
			r = *w.ResetS
		}
		if w.Status != "" {
			s = w.Status
		}
		return []any{u, r, s}
	}
	return append(append([]any{1}, win(l.FiveHour)...), win(l.SevenDay)...)
}

// LatestLimits is the newest row's limit values for the filter's tenant (or every tenant when
// f.TenantAll), and the row's time in epoch ms. nil when no row has any.
func (d *DB) LatestLimits(f Filter) (*Limits, int64, error) {
	q := `SELECT ts, limit_5h_utilization, limit_5h_reset, limit_5h_status,
		limit_7d_utilization, limit_7d_reset, limit_7d_status
		FROM requests
		WHERE limit_seen = 1 AND tenant_id = ? ORDER BY ts DESC, id DESC LIMIT 1`
	args := []any{f.Tenant}
	if f.TenantAll {
		q = `SELECT ts, limit_5h_utilization, limit_5h_reset, limit_5h_status,
		limit_7d_utilization, limit_7d_reset, limit_7d_status
		FROM requests
		WHERE limit_seen = 1 ORDER BY ts DESC, id DESC LIMIT 1`
		args = nil
	}
	var ts int64
	var u5, u7 sql.NullFloat64
	var r5, r7 sql.NullInt64
	var s5, s7 sql.NullString
	err := d.sql.QueryRowContext(d.readCtx(), q, args...).Scan(&ts, &u5, &r5, &s5, &u7, &r7, &s7)
	if err == sql.ErrNoRows {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	return &Limits{FiveHour: limitWindow(u5, r5, s5), SevenDay: limitWindow(u7, r7, s7)}, ts, nil
}

// limitWindow is one window read back from its three nullable columns.
func limitWindow(u sql.NullFloat64, r sql.NullInt64, s sql.NullString) LimitWindow {
	var w LimitWindow
	if u.Valid {
		v := u.Float64
		w.Utilization = &v
	}
	if r.Valid {
		v := r.Int64
		w.ResetS = &v
	}
	w.Status = s.String
	return w
}

func (a *API) limitsRoutes() []route {
	return []route{{"GET /api/me/limits", scopeTenant, a.meLimits}}
}

// meLimits is GET /api/me/limits: the latest subscription limit values the proxy saw for the
// caller's own account. {} when it never saw any (an API key, Bedrock, Vertex).
//
// Always the caller's OWN account, a manager's too: "me" is in the path, so the manager's
// service-wide default of the other routes does not apply here. A single-tenant proxy has one
// account, so it reads every row.
func (a *API) meLimits(w http.ResponseWriter, r *http.Request) {
	f, p, ok := a.scope(r)
	if !ok {
		a.unauthorized(w)
		return
	}
	if a.auth != nil {
		f.Tenant, f.TenantAll = p.TenantID, false
	}
	l, at, err := a.db(r).LatestLimits(f)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if l == nil {
		writeJSON(w, struct{}{})
		return
	}
	body := map[string]any{"subscription": true, "at_ms": at}
	if !l.FiveHour.Empty() {
		body["five_hour"] = l.FiveHour
	}
	if !l.SevenDay.Empty() {
		body["seven_day"] = l.SevenDay
	}
	writeJSON(w, body)
}
