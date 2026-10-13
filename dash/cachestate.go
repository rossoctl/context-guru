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
	Session string `json:"session"`
	Model   string `json:"model"`
	LastTS  int64  `json:"last_ts"` // epoch ms of the last request that touched the cache (its START)
	// EndTS is when that request finished (LastTS + the upstream time). The provider's TTL clock
	// runs from the response, not from the send, so a long answer keeps the cache warm longer
	// than LastTS+TTL says. Equal to LastTS when the duration is unknown.
	EndTS      int64 `json:"end_ts"`
	NowMS      int64 `json:"now_ms"` // server clock, so a client with skew can still count down
	TTLSeconds int64 `json:"ttl_s"`  // 3600 if the last cache write was billed at the 1h tier, else 300
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
	// PingsLeft and HeldUntilMS are the main thread's keep-alive plan (see ThreadCache).
	PingsLeft   int   `json:"pings_left,omitempty"`
	HeldUntilMS int64 `json:"held_until_ms,omitempty"`
	// Threads is the session's cache entries, one per thread (the main agent and each subagent
	// have separate provider caches), most recently used first. Only threads that touched the
	// cache within the last two hours are listed: older ones are finished work.
	Threads []ThreadCache `json:"threads,omitempty"`
}

// ThreadCache is one thread's cache entry. Main is the session's primary thread.
type ThreadCache struct {
	ID           string  `json:"id"`
	Main         bool    `json:"main"`
	Model        string  `json:"model"`
	LastTS       int64   `json:"last_ts"`
	EndTS        int64   `json:"end_ts"`
	TTLSeconds   int64   `json:"ttl_s"`
	PrefixTokens int64   `json:"prefix_tokens"`
	RewriteUSD   float64 `json:"rewrite_usd"`
	Priced       bool    `json:"priced"`
	// PingsLeft is how many keep-alive pings the proxy still plans for this thread, and
	// HeldUntilMS when its cache goes cold if no real request arrives first: the last planned
	// ping's start plus the TTL. Both omitted when no ping is planned.
	PingsLeft   int   `json:"pings_left,omitempty"`
	HeldUntilMS int64 `json:"held_until_ms,omitempty"`
}

// KeepAlivePlan is one thread's scheduled keep-alive, as the proxy's keeper holds it in memory.
type KeepAlivePlan struct {
	NextPingMS int64 // when the next ping fires (epoch ms)
	PingsLeft  int   // pings still to send, the next one included
	IdleMS     int64 // the spacing between pings
}

// heldUntil is when a thread's cache goes cold with this plan: the last ping's start plus the TTL.
func (p KeepAlivePlan) heldUntil(ttlS int64) int64 {
	return p.NextPingMS + int64(p.PingsLeft-1)*p.IdleMS + ttlS*1000
}

// SetKeepAlivePlans supplies the keeper's per-thread plan for one session, keyed by thread id.
// dash holds no keep-alive state of its own; nil means no plan is ever shown.
func (a *API) SetKeepAlivePlans(fn func(tenantID string, allTenants bool, session string) map[string]KeepAlivePlan) {
	a.kaPlans = fn
}

// maxDurationMS bounds the upstream time trusted for EndTS: a bogus value must not hold a dead
// cache "warm" for hours.
const maxDurationMS = 15 * 60 * 1000

func endTS(ts int64, upstreamMS float64) int64 {
	if upstreamMS <= 0 || upstreamMS > maxDurationMS {
		return ts
	}
	return ts + int64(upstreamMS)
}

// ThreadsFor lists the session's per-thread cache entries touched since sinceMS.
func (d *DB) ThreadsFor(f Filter, sinceMS int64) ([]ThreadCache, error) {
	cond, args := withKeepAlive(f).where()
	rows, err := d.sql.QueryContext(d.readCtx(), `SELECT r.thread_id, MAX(r.ts), r.upstream_ms, r.model,
		r.fresh_input + r.cache_read + r.cache_write,
		COALESCE((SELECT CASE WHEN w.cache_write_1h > 0 THEN 3600 ELSE 300 END FROM requests w
			WHERE w.session_id = r.session_id AND w.tenant_id = r.tenant_id AND w.thread_id = r.thread_id
			AND w.cache_write > 0 ORDER BY w.ts DESC LIMIT 1), 300)
		FROM requests r WHERE `+cond+` AND r.status BETWEEN 200 AND 299
		AND r.cache_read + r.cache_write > 0 AND r.ts >= ?
		GROUP BY r.thread_id ORDER BY MAX(r.ts) DESC LIMIT 16`, append(args, sinceMS)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ThreadCache
	for rows.Next() {
		var (
			t  ThreadCache
			ms float64
		)
		if err := rows.Scan(&t.ID, &t.LastTS, &ms, &t.Model, &t.PrefixTokens, &t.TTLSeconds); err != nil {
			return nil, err
		}
		t.Main, t.EndTS = t.ID == "", endTS(t.LastTS, ms)
		out = append(out, t)
	}
	return out, rows.Err()
}

// CacheStateFor reads the session's most recent cache-touching request. nil, nil = none yet.
func (d *DB) CacheStateFor(f Filter) (*CacheState, error) {
	cond, args := withKeepAlive(f).where()
	var (
		s     CacheState
		fresh int64
		kaI   int
		upMS  float64
	)
	err := d.sql.QueryRowContext(d.readCtx(), `SELECT r.ts, r.upstream_ms, r.model, r.fresh_input, r.cache_read, r.cache_write, r.keepalive,
		COALESCE((SELECT CASE WHEN w.cache_write_1h > 0 THEN 3600 ELSE 300 END FROM requests w
			WHERE w.session_id = r.session_id AND w.tenant_id = r.tenant_id AND w.cache_write > 0
			ORDER BY w.ts DESC LIMIT 1), 300)
		FROM requests r WHERE `+cond+` AND r.status BETWEEN 200 AND 299
		AND r.cache_read + r.cache_write > 0 ORDER BY r.ts DESC LIMIT 1`, args...).Scan(
		&s.LastTS, &upMS, &s.Model, &fresh, &s.CacheRead, &s.CacheWrite, &kaI, &s.TTLSeconds)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.Session, s.KeepAlive, s.EndTS = f.Session, kaI == 1, endTS(s.LastTS, upMS)
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
	pf := a.priceFn(r)
	rewrite := func(model string, tokens, ttl int64) (usd float64, ok bool) {
		if pf == nil {
			return 0, false
		}
		p, ok := pf(model)
		if !ok {
			return 0, false
		}
		if ttl == 3600 { // 1h write = 2.0x input; same derivation as CostWithCacheWrite1h
			return float64(tokens) * p.Input * 2.0, true
		}
		return float64(tokens) * p.CacheWrite, true
	}
	s.RewriteUSD, s.Priced = rewrite(s.Model, s.PrefixTokens, s.TTLSeconds)
	if ths, err := a.db(r).ThreadsFor(f, s.NowMS-2*3600*1000); err == nil {
		for i := range ths {
			ths[i].RewriteUSD, ths[i].Priced = rewrite(ths[i].Model, ths[i].PrefixTokens, ths[i].TTLSeconds)
		}
		s.Threads = ths
	}
	if a.kaPlans != nil {
		plans := a.kaPlans(f.Tenant, f.TenantAll, f.Session)
		for i := range s.Threads {
			if p, ok := plans[s.Threads[i].ID]; ok && p.PingsLeft > 0 {
				s.Threads[i].PingsLeft, s.Threads[i].HeldUntilMS = p.PingsLeft, p.heldUntil(s.Threads[i].TTLSeconds)
			}
		}
		if p, ok := plans[""]; ok && p.PingsLeft > 0 {
			ttl := s.TTLSeconds
			for _, t := range s.Threads {
				if t.Main {
					ttl = t.TTLSeconds
				}
			}
			s.PingsLeft, s.HeldUntilMS = p.PingsLeft, p.heldUntil(ttl)
		}
	}
	s.Window, s.WindowExact, _ = modelinfo.Exact(a.windows, r.Context(), s.Model)
	writeJSON(w, s)
}
