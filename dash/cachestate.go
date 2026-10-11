package dash

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// CacheState is the SERVER's view of one session's prompt cache: what the provider last billed us
// for, not what the client guesses. The status line prefers this over Claude Code's own
// prompt_cache block because the proxy sees every request of the session (keep-alive pings
// included), after every rewrite.
//
// Numbers and enum labels only - no content - so it is scopeTenant like the keep-alive reads.
type CacheState struct {
	Session    string `json:"session"`
	Model      string `json:"model"`
	LastTS     int64  `json:"last_ts"` // epoch ms of the last request that touched the cache
	NowMS      int64  `json:"now_ms"`  // server clock, so a client with skew can still count down
	TTLSeconds int64  `json:"ttl_s"`   // 3600 if the last cache write was billed at the 1h tier, else 300
	// PrefixTokens is what the last request sent: fresh + cache_read + cache_write. If the cache
	// is cold, this is what the next send rewrites.
	PrefixTokens int64 `json:"prefix_tokens"`
	CacheRead    int64 `json:"cache_read"`
	CacheWrite   int64 `json:"cache_write"`
	KeepAlive    bool  `json:"keepalive"` // the last touch was a context-guru ping
	// RewriteUSD prices PrefixTokens at the cache-write rate (x2.0/1.25 on the 1h tier). Omitted
	// (0 with Priced=false) when the model has no price: unknown, never free.
	RewriteUSD float64 `json:"rewrite_usd"`
	Priced     bool    `json:"priced"`
	// Window is the served context window for Model, Exact=false when it is a guess.
	Window      int  `json:"window"`
	WindowExact bool `json:"window_exact"`
}

// CacheStateFor reads the session's most recent cache-touching request. nil, nil = none yet.
func (d *DB) CacheStateFor(f Filter) (*CacheState, error) {
	cond, args := withKeepAlive(f).where()
	var (
		s     CacheState
		fresh int64
		kaI   int
	)
	err := d.sql.QueryRowContext(d.readCtx(), `SELECT r.ts, r.model, r.fresh_input, r.cache_read, r.cache_write, r.keepalive,
		COALESCE((SELECT CASE WHEN w.cache_write_1h > 0 THEN 3600 ELSE 300 END FROM requests w
			WHERE w.session_id = r.session_id AND w.tenant_id = r.tenant_id AND w.cache_write > 0
			ORDER BY w.ts DESC LIMIT 1), 300)
		FROM requests r WHERE `+cond+` AND r.status BETWEEN 200 AND 299
		AND r.cache_read + r.cache_write > 0 ORDER BY r.ts DESC LIMIT 1`, args...).Scan(
		&s.LastTS, &s.Model, &fresh, &s.CacheRead, &s.CacheWrite, &kaI, &s.TTLSeconds)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.Session, s.KeepAlive = f.Session, kaI == 1
	s.PrefixTokens = fresh + s.CacheRead + s.CacheWrite
	return &s, nil
}

func (a *API) cacheStateRoutes() []route {
	return []route{{"GET /api/cachestate", scopeTenant, a.cacheState}}
}

func (a *API) cacheState(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		a.unauthorized(w)
		return
	}
	if f.Session == "" { // a cache belongs to a session; an unscoped "last request" would mix people
		httpErr(w, http.StatusBadRequest, "session is required")
		return
	}
	s, err := a.db(r).CacheStateFor(f)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s == nil {
		writeJSON(w, map[string]any{"session": f.Session, "none": true, "now_ms": time.Now().UnixMilli()})
		return
	}
	s.NowMS = time.Now().UnixMilli()
	if pf := a.priceFn(r); pf != nil {
		if p, ok := pf(s.Model); ok {
			s.Priced = true
			if s.TTLSeconds == 3600 { // 1h write = 2.0x input; same derivation as CostWithCacheWrite1h
				s.RewriteUSD = float64(s.PrefixTokens) * p.Input * 2.0
			} else {
				s.RewriteUSD = float64(s.PrefixTokens) * p.CacheWrite
			}
		}
	}
	s.Window, s.WindowExact, _ = modelinfo.Exact(a.windows, r.Context(), s.Model)
	writeJSON(w, s)
}
