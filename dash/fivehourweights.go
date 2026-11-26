package dash

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// The 5-hour weights on the dashboard (#1047): the rows for the fit, its cache, and
// GET /api/me/fivehour-weights. The fit itself is fivehourfit.go.

const (
	// fhLookback is how far back the fit reads: two weeks of windows, so the weights follow a
	// change of plan or of models within days.
	fhLookback = 14 * 24 * time.Hour
	// fhMaxRows bounds the read. The newest rows are kept.
	fhMaxRows = 100_000
	// fhCacheTTL is how long one fit is served before the next read. The weights move slowly:
	// a page load never refits.
	fhCacheTTL = 15 * time.Minute
)

// FiveHourRows reads the rows for the fit: the newest rows since sinceMs that carry a 5-hour share
// and reset, for the filter's tenant (every tenant when f.TenantAll). Numbers and the model name
// only. Pings and the requests sent in place of a ping count too: they used the limit as well. A row
// carries the share only when the user's own credential sent it (#984), so a row sent with another
// credential is never in the fit.
func (d *DB) FiveHourRows(f Filter, sinceMs int64) ([]FiveHourRow, error) {
	cond, args := `r.limit_seen = 1 AND r.tenant_id = ?`, []any{f.Tenant}
	if f.TenantAll {
		cond, args = `r.limit_seen = 1`, nil
	}
	q := `SELECT r.id, r.ts, r.limit_5h_reset, r.limit_5h_utilization, r.fresh_input, r.cache_read,
			MAX(r.cache_write - r.cache_write_1h, 0), r.cache_write_1h, r.output_tokens, r.model
		FROM requests r
		WHERE ` + cond + ` AND r.ts >= ? AND r.limit_5h_utilization IS NOT NULL AND r.limit_5h_reset IS NOT NULL
		ORDER BY r.ts DESC, r.id DESC LIMIT ?`
	rows, err := d.sql.QueryContext(d.readCtx(), q, append(args, sinceMs, fhMaxRows)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FiveHourRow
	for rows.Next() {
		var r FiveHourRow
		if err := rows.Scan(&r.ID, &r.TsMs, &r.Reset, &r.Util, &r.Input, &r.CacheRead, &r.Write5m, &r.Write1h, &r.Output, &r.Model); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PingTokensSince is what the cache strategy's automatic pings billed since sinceMs, by tier, for
// the 5-hour estimate: the pings used the limit too, and the dollar estimate nets their cost.
func (d *DB) PingTokensSince(f Filter, sinceMs int64) (FiveHourPing, error) {
	cond, args := `r.tenant_id = ? AND `, []any{f.Tenant}
	if f.TenantAll {
		cond, args = ``, nil
	}
	var p FiveHourPing
	err := d.sql.QueryRowContext(d.readCtx(), `SELECT COALESCE(SUM(r.fresh_input),0), COALESCE(SUM(r.cache_read),0),
			COALESCE(SUM(MAX(r.cache_write - r.cache_write_1h, 0)),0), COALESCE(SUM(r.cache_write_1h),0),
			COALESCE(SUM(r.output_tokens),0)
		FROM requests r WHERE `+cond+`r.ts >= ? AND `+autoPing, append(args, sinceMs)...).
		Scan(&p.Input, &p.CacheRead, &p.CacheWrite5m, &p.CacheWrite1h, &p.Output)
	return p, err
}

// FiveHourPing is the pings' billed tokens in the open 5-hour window.
type FiveHourPing struct {
	TierTokens
	Output int64 `json:"output"`
}

// fhCache holds one fit per account for fhCacheTTL.
type fhCache struct {
	mu sync.Mutex
	m  map[string]fhCacheEntry
}

type fhCacheEntry struct {
	at  time.Time
	fit FiveHourFit
}

// fiveHourFit is the account's fit, from the cache when it is fresh. Fail open: an error or a panic
// in the read or the fit gives a fit that did not pass the gate, so every screen keeps its
// API-price figures.
func (a *API) fiveHourFit(r *http.Request, f Filter) (fit FiveHourFit) {
	key := f.Tenant
	if f.TenantAll {
		key = "\x00all"
	}
	a.fiveHour.mu.Lock()
	if e, ok := a.fiveHour.m[key]; ok && time.Since(e.at) < fhCacheTTL {
		a.fiveHour.mu.Unlock()
		return e.fit
	}
	a.fiveHour.mu.Unlock()
	// Concurrent first loads of one account share one read and one fit (the jsonInflight group the
	// other caches use, under a key of its own).
	v, _, _ := a.jsonInflight.Do("fivehour\x00"+key, func() (any, error) {
		return a.computeFiveHourFit(r, f, key), nil
	})
	return v.(FiveHourFit)
}

// computeFiveHourFit reads the rows, fits, and caches the answer, a fit below the gate too. A read
// error is not cached: it may be the leading caller's own cancelled request.
func (a *API) computeFiveHourFit(r *http.Request, f Filter, key string) (fit FiveHourFit) {
	readFailed := false
	defer func() {
		if p := recover(); p != nil {
			fit = FiveHourFit{Reason: fmt.Sprintf("the fit failed: %v", p)}
		}
		if readFailed {
			return
		}
		a.fiveHour.mu.Lock()
		if a.fiveHour.m == nil || len(a.fiveHour.m) > 256 {
			a.fiveHour.m = map[string]fhCacheEntry{}
		}
		a.fiveHour.m[key] = fhCacheEntry{at: time.Now(), fit: fit}
		a.fiveHour.mu.Unlock()
	}()
	rows, err := a.db(r).FiveHourRows(f, time.Now().Add(-fhLookback).UnixMilli())
	if err != nil {
		readFailed = true
		return FiveHourFit{Reason: "the rows could not be read"}
	}
	return FitFiveHourWeights(rows, tierPriceFn(a.priceFn(r)))
}

// tierPriceFn turns the dashboard's price lookup into the fit's per-tier prices. nil stays nil.
func tierPriceFn(fn func(string) (modelinfo.Price, bool)) func(string) (TierPrice, bool) {
	if fn == nil {
		return nil
	}
	return func(model string) (TierPrice, bool) {
		p, ok := fn(model)
		if !ok || p.Input <= 0 {
			return TierPrice{}, false
		}
		return TierPrice{p.Input, p.CacheReadFor(model), p.CacheWrite5mFor(model), p.CacheWrite1hFor(model), p.Output}, true
	}
}

// ownSubscription is the caller's own-account filter when the caller looks at their own account and
// that account is on a Claude subscription. ok is false for a manager's service-wide or other-tenant
// view, an API key, Bedrock or Vertex: those keep the API-price figures.
func (a *API) ownSubscription(r *http.Request, f Filter, p Principal) (Filter, bool) {
	if a.auth != nil {
		if f.TenantAll || f.Tenant != p.TenantID {
			return f, false
		}
	}
	sub, err := a.db(r).LatestAuthSubscription(f)
	if err != nil || !sub {
		return f, false
	}
	return f, true
}

func (a *API) fiveHourRoutes() []route {
	return []route{{"GET /api/me/fivehour-weights", scopeTenant, a.meFiveHourWeights}}
}

// meFiveHourWeights is GET /api/me/fivehour-weights: the learned 5-hour weights of the caller's own
// account (FiveHourFit), or {} when the account is not on a Claude subscription. With since (unix
// ms, the open window's start) and a fit that passed the gate, window_ping adds what the cache
// strategy's pings billed since then, for the Overview's 5-hour estimate.
//
// Always the caller's own account, a manager's too, as /api/me/limits.
func (a *API) meFiveHourWeights(w http.ResponseWriter, r *http.Request) {
	f, p, ok := a.scope(r)
	if !ok {
		a.unauthorized(w)
		return
	}
	if a.auth != nil {
		f.Tenant, f.TenantAll = p.TenantID, false
	}
	f, sub := a.ownSubscription(r, f, p)
	if !sub {
		writeJSON(w, struct{}{})
		return
	}
	body := struct {
		FiveHourFit
		WindowPing *FiveHourPing `json:"window_ping,omitempty"`
	}{FiveHourFit: a.fiveHourFit(r, f)}
	if since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64); since > 0 && body.OK {
		if ping, err := a.db(r).PingTokensSince(f, since); err == nil {
			body.WindowPing = &ping
		} else {
			// Fail open: without the pings the page cannot net them, so it keeps the dollar estimate.
			body.FiveHourFit = FiveHourFit{Reason: "the pings could not be read"}
		}
	}
	writeJSON(w, body)
}
