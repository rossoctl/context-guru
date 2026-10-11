package dash

// GET /api/spend/explain — "what did I spend, on what, and where did the cache go cold".
//
// One pass over the window's requests, priced at the DEPLOYED gateway rates, split three ways the
// plugin's insights need and no existing route returns together:
//
//   - dollars by billed tier (fresh / cache read / cache write / output), per day, model, session;
//   - full-prefix REWRITES classified from the token columns (cache_write >= 0.8 x context), each
//     with a cause read from the idle gap and the message count, and the dollars it cost OVER what
//     a cache hit would have cost;
//   - the live state of each recent session (context size, cache tier, last touch), so the client
//     can answer "am I about to pay a rewrite".
//
// Everything here is OBSERVED (token counts the provider reported, priced at configured rates)
// except the cause label, which is an inference from timing and says so (see rewriteCause).
// cache_miss_reason is deliberately not read: it short-circuits on cache_read > 0.

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

const (
	// rewriteFrac is the share of a request's context that must have been WRITTEN to the cache
	// for the request to count as a full-prefix rewrite.
	rewriteFrac = 0.8
	// rewriteMinCtx keeps a 300-token request from being reported as a rewrite.
	rewriteMinCtx = 2000
	ttl5m         = 5 * 60 * 1000
	ttl1h         = 60 * 60 * 1000
	liveLookback  = 2 * 60 * 60 * 1000
)

// Rewrite causes, ordered roughly by how avoidable they are.
const (
	CauseColdStart    = "cold_start"      // first request of the session for this model
	CauseIdleExpiry   = "idle_expiry"     // gap since the previous request exceeded the cache TTL
	CauseNewThread    = "new_thread"      // same session, different tools/system (subagent or config change)
	CauseHistory      = "history_rewrite" // message count fell: compaction, rewind or edit
	CausePrefixChange = "prefix_change"   // warm, longer history, still rewritten: tools/system/MCP changed
)

type SpendUSD struct {
	Fresh      float64 `json:"fresh"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Output     float64 `json:"output"`
	Total      float64 `json:"total"`
}

func (s *SpendUSD) add(o SpendUSD) {
	s.Fresh += o.Fresh
	s.CacheRead += o.CacheRead
	s.CacheWrite += o.CacheWrite
	s.Output += o.Output
	s.Total += o.Total
}

type RewriteCause struct {
	N      int     `json:"n"`
	Tokens int64   `json:"tokens"`
	USD    float64 `json:"usd"` // excess over a cache hit on the same tokens
	// WriteUSD is the FULL price of those writes (what the rows were billed for them), so the
	// causes partition the cache-write line instead of overlapping it.
	WriteUSD float64 `json:"write_usd"`
}

// GapBand is how many idle-expiry rewrites followed a pause of a given length, and what they cost.
type GapBand struct {
	Band string  `json:"band"`
	N    int     `json:"n"`
	USD  float64 `json:"usd"`
}

var gapBands = []struct {
	name  string
	below int64 // seconds, exclusive
}{{"5-10m", 600}, {"10-30m", 1800}, {"30-60m", 3600}, {"1-4h", 4 * 3600}, {">4h", 1 << 62}}

type RewriteEvent struct {
	TS      int64   `json:"ts"`
	Session string  `json:"session"`
	Model   string  `json:"model"`
	Context int64   `json:"context"`
	GapS    int64   `json:"gap_s"` // -1 when there was no previous request
	Cause   string  `json:"cause"`
	USD     float64 `json:"usd"`
}

type SpendSession struct {
	Session    string   `json:"session"`
	Model      string   `json:"model"` // the model that cost most in this session
	First      int64    `json:"first"`
	Last       int64    `json:"last"`
	Turns      int      `json:"turns"`
	USD        SpendUSD `json:"usd"`
	Rewrites   int      `json:"rewrites"`
	RewriteUSD float64  `json:"rewrite_usd"`
	modelUSD   map[string]float64
}

type SpendModel struct {
	Model      string   `json:"model"`
	Requests   int      `json:"requests"`
	USD        SpendUSD `json:"usd"`
	RewriteUSD float64  `json:"rewrite_usd"`
}

type SpendDay struct {
	Day        int64              `json:"day"` // local midnight, epoch ms
	Requests   int                `json:"requests"`
	USD        SpendUSD           `json:"usd"`
	RewriteUSD float64            `json:"rewrite_usd"`
	Rewrites   int                `json:"rewrites"`
	ByModel    map[string]float64 `json:"by_model"`
}

type SessionDay struct {
	Session string  `json:"session"`
	Day     int64   `json:"day"`
	USD     float64 `json:"usd"`
}

type SpendRate struct {
	Input, Output, CacheRead, CacheWrite, CacheWrite1h float64 // USD per token
}

type LiveSession struct {
	Session  string `json:"session"`
	Model    string `json:"model"`
	LastTS   int64  `json:"last_ts"`
	Context  int64  `json:"context"`
	TTLMs    int64  `json:"ttl_ms"`
	TTL1h    bool   `json:"ttl_1h"`
	Messages int    `json:"messages"`
}

type SpendExplain struct {
	Since, Until int64                         `json:"-"`
	Window       map[string]int64              `json:"window"`
	Requests     int                           `json:"requests"`
	Priced       int                           `json:"priced_requests"`
	Unpriced     int                           `json:"unpriced_requests"`
	USD          SpendUSD                      `json:"usd"`
	StoredUSD    float64                       `json:"stored_cost_usd"` // sum of cost_usd, to reconcile against usd.total
	Rewrites     map[string]*RewriteCause      `json:"rewrites"`
	IdleGaps     []GapBand                     `json:"idle_gaps"`
	TopRewrites  []RewriteEvent                `json:"top_rewrites"`
	Models       []*SpendModel                 `json:"models"`
	Sessions     []*SpendSession               `json:"sessions"`
	Daily        []*SpendDay                   `json:"daily"`
	SessionDays  []SessionDay                  `json:"session_days"`
	Live         []LiveSession                 `json:"live"`
	Rates        map[string]map[string]float64 `json:"rates"`
	Ledger       SpendLedger                   `json:"ledger"`
}

// SpendLedger is context-guru's own books for the window, so a client can print an honest net:
// the modelled gross saving minus what context-guru itself spent.
type SpendLedger struct {
	GrossSavedUSD float64 `json:"gross_saved_usd"` // sum(baseline - cost) over non-ping rows; MODELLED, session-accumulative
	CGLLMUSD      float64 `json:"cg_llm_usd"`      // context-guru's own model calls (summariser)
	PingUSD       float64 `json:"ping_usd"`        // keep-alive pings, billed to the caller
	PingSavedUSD  float64 `json:"ping_saved_usd"`  // modelled
}

type spendRow struct {
	ts                               int64
	session, model                   string
	tools, sysBlocks, messages       int
	fresh, read, write, write1h, out int64
	cost, baseline, cgllm, kaSaved   float64
	keepalive                        bool
	complete                         bool
}

type threadKey struct {
	session, model string
	tools, sys     int
}

type threadState struct {
	ts       int64
	messages int
	ttl      int64
}

func ttlFor(write1h int64, prev int64) int64 {
	if write1h > 0 {
		return ttl1h
	}
	return prev
}

func priceOf(rates map[string]modelinfo.Price, price func(string) (modelinfo.Price, bool), model string) (modelinfo.Price, bool) {
	if p, ok := rates[model]; ok {
		return p, !p.Zero()
	}
	p, ok := modelinfo.Price{}, false
	if price != nil {
		p, ok = price(model)
	}
	if ok && p.Zero() {
		ok = false
	}
	rates[model] = p
	return p, ok
}

func classify(p modelinfo.Price, r spendRow) SpendUSD {
	u := SpendUSD{
		Fresh:      float64(r.fresh) * p.Input,
		CacheRead:  float64(r.read) * p.CacheRead,
		CacheWrite: float64(r.write) * p.CacheWrite,
		Output:     float64(r.out) * p.Output,
	}
	if r.write1h > 0 {
		if prem := 2*p.Input - p.CacheWrite; prem > 0 {
			u.CacheWrite += float64(r.write1h) * prem
		}
	}
	u.Total = u.Fresh + u.CacheRead + u.CacheWrite + u.Output
	return u
}

// rewriteCause explains WHY a full-prefix write happened. The cause is an inference from the idle
// gap and the message count; the dollars are not.
func rewriteCause(prev *threadState, sessionSeen bool, r spendRow) (string, int64) {
	if prev == nil {
		if sessionSeen {
			return CauseNewThread, -1
		}
		return CauseColdStart, -1
	}
	gap := r.ts - prev.ts
	switch {
	case gap > prev.ttl:
		return CauseIdleExpiry, gap / 1000
	case r.messages < prev.messages:
		return CauseHistory, gap / 1000
	default:
		return CausePrefixChange, gap / 1000
	}
}

// SpendExplainFor runs the pass. tzMin is the caller's UTC offset in minutes, for day buckets.
func (d *DB) SpendExplainFor(f Filter, price func(string) (modelinfo.Price, bool), tzMin int, now int64, top int) (*SpendExplain, error) {
	f.WithKeepAlive = true // a ping is real spend and it refreshes the cache; it must be in the chain
	warm := f
	if warm.Since > 0 {
		warm.Since -= ttl1h // earlier requests exist only to give the first counted request a past
	}
	cond, args := warm.where()
	rows, err := d.sql.QueryContext(d.readCtx(), `SELECT r.ts, r.session_id, r.model, r.tools, r.system_blocks,
		r.messages, r.fresh_input, r.cache_read, r.cache_write, r.cache_write_1h, r.output_tokens,
		r.cost_usd, r.baseline_cost_usd, r.cg_llm_cost_usd, r.keepalive_saved_usd, r.keepalive,
		r.token_accounting FROM requests r WHERE `+cond+` ORDER BY r.ts, r.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := &SpendExplain{Rewrites: map[string]*RewriteCause{}}
	rates := map[string]modelinfo.Price{}
	threads := map[threadKey]*threadState{}
	seen := map[[2]string]bool{}
	models := map[string]*SpendModel{}
	sessions := map[string]*SpendSession{}
	days := map[int64]*SpendDay{}
	sdays := map[[2]any]*SessionDay{}
	var events []RewriteEvent
	var gaps []GapBand

	dayOf := func(ts int64) int64 {
		off := int64(tzMin) * 60000
		return (ts+off)/86400000*86400000 - off
	}
	for rows.Next() {
		var r spendRow
		var ka int
		var acct string
		if err := rows.Scan(&r.ts, &r.session, &r.model, &r.tools, &r.sysBlocks, &r.messages,
			&r.fresh, &r.read, &r.write, &r.write1h, &r.out, &r.cost, &r.baseline, &r.cgllm,
			&r.kaSaved, &ka, &acct); err != nil {
			return nil, err
		}
		r.keepalive, r.complete = ka != 0, acct == AccountingComplete
		counted := f.Since == 0 || r.ts >= f.Since
		p, priced := priceOf(rates, price, r.model)
		ctx := r.fresh + r.read + r.write
		tk := threadKey{r.session, r.model, r.tools, r.sysBlocks}
		prev := threads[tk]
		sk := [2]string{r.session, r.model}
		var cause string
		var gap int64
		isRewrite := r.complete && ctx >= rewriteMinCtx && float64(r.write) >= rewriteFrac*float64(ctx)
		if isRewrite {
			cause, gap = rewriteCause(prev, seen[sk], r)
		}
		// advance the thread whether or not the row is counted: that is what the warm-up is for
		st := prev
		if st == nil {
			st = &threadState{ttl: ttl5m}
			threads[tk] = st
		}
		st.ts, st.messages, st.ttl = r.ts, r.messages, ttlFor(r.write1h, ttl5m)
		if r.write1h == 0 && r.write == 0 && prev != nil {
			st.ttl = prev.ttl // a pure read keeps the tier the entry was written at
		}
		seen[sk] = true
		if !counted {
			continue
		}
		out.Requests++
		out.StoredUSD += r.cost
		if !r.complete || !priced {
			out.Unpriced++
			continue
		}
		out.Priced++
		u := classify(p, r)
		out.USD.add(u)
		out.Ledger.CGLLMUSD += r.cgllm
		if r.keepalive {
			out.Ledger.PingUSD += r.cost
			out.Ledger.PingSavedUSD += r.kaSaved
		} else {
			out.Ledger.GrossSavedUSD += r.baseline - r.cost
		}
		m := models[r.model]
		if m == nil {
			m = &SpendModel{Model: r.model}
			models[r.model] = m
		}
		m.Requests++
		m.USD.add(u)
		s := sessions[r.session]
		if s == nil {
			s = &SpendSession{Session: r.session, First: r.ts, modelUSD: map[string]float64{}}
			sessions[r.session] = s
		}
		s.Last, s.Turns = r.ts, s.Turns+1
		s.USD.add(u)
		s.modelUSD[r.model] += u.Total
		dk := dayOf(r.ts)
		dd := days[dk]
		if dd == nil {
			dd = &SpendDay{Day: dk, ByModel: map[string]float64{}}
			days[dk] = dd
		}
		dd.Requests++
		dd.USD.add(u)
		dd.ByModel[r.model] += u.Total
		sd := sdays[[2]any{r.session, dk}]
		if sd == nil {
			sd = &SessionDay{Session: r.session, Day: dk}
			sdays[[2]any{r.session, dk}] = sd
		}
		sd.USD += u.Total
		if isRewrite {
			excess := float64(r.write) * (p.CacheWrite - p.CacheRead)
			if r.write1h > 0 {
				if prem := 2*p.Input - p.CacheWrite; prem > 0 {
					excess += float64(r.write1h) * prem
				}
			}
			if excess < 0 {
				excess = 0
			}
			rc := out.Rewrites[cause]
			if rc == nil {
				rc = &RewriteCause{}
				out.Rewrites[cause] = rc
			}
			rc.N++
			rc.Tokens += r.write
			rc.USD += excess
			rc.WriteUSD += float64(r.write) * p.CacheWrite
			if r.write1h > 0 {
				if prem := 2*p.Input - p.CacheWrite; prem > 0 {
					rc.WriteUSD += float64(r.write1h) * prem
				}
			}
			if cause == CauseIdleExpiry {
				if gaps == nil {
					gaps = make([]GapBand, len(gapBands))
					for i := range gapBands {
						gaps[i].Band = gapBands[i].name
					}
				}
				for i := range gapBands {
					if gap < gapBands[i].below {
						gaps[i].N++
						gaps[i].USD += excess
						break
					}
				}
			}
			m.RewriteUSD += excess
			s.Rewrites++
			s.RewriteUSD += excess
			dd.Rewrites++
			dd.RewriteUSD += excess
			events = append(events, RewriteEvent{TS: r.ts, Session: r.session, Model: r.model,
				Context: ctx, GapS: gap, Cause: cause, USD: excess})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if top <= 0 {
		top = 8
	}
	sort.Slice(events, func(i, j int) bool { return events[i].USD > events[j].USD })
	if len(events) > top {
		events = events[:top]
	}
	out.TopRewrites = events
	out.IdleGaps = gaps
	for _, m := range models {
		out.Models = append(out.Models, m)
	}
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].USD.Total > out.Models[j].USD.Total })
	for _, s := range sessions {
		best := 0.0
		for m, v := range s.modelUSD {
			if v > best {
				best, s.Model = v, m
			}
		}
		out.Sessions = append(out.Sessions, s)
	}
	sort.Slice(out.Sessions, func(i, j int) bool { return out.Sessions[i].USD.Total > out.Sessions[j].USD.Total })
	for _, dd := range days {
		out.Daily = append(out.Daily, dd)
	}
	sort.Slice(out.Daily, func(i, j int) bool { return out.Daily[i].Day < out.Daily[j].Day })
	for _, sd := range sdays {
		out.SessionDays = append(out.SessionDays, *sd)
	}
	sort.Slice(out.SessionDays, func(i, j int) bool {
		if out.SessionDays[i].Day != out.SessionDays[j].Day {
			return out.SessionDays[i].Day < out.SessionDays[j].Day
		}
		return out.SessionDays[i].Session < out.SessionDays[j].Session
	})
	out.Rates = map[string]map[string]float64{}
	for model, p := range rates {
		if p.Zero() {
			continue
		}
		out.Rates[model] = map[string]float64{"input": p.Input, "output": p.Output,
			"cache_read": p.CacheRead, "cache_write": p.CacheWrite, "cache_write_1h": 2 * p.Input}
	}
	out.Window = map[string]int64{"since": f.Since, "until": f.Until}
	live, err := d.liveSessions(f, now)
	if err != nil {
		return nil, err
	}
	out.Live = live
	return out, nil
}

// liveSessions returns, for every session that touched the cache in the last two hours, the
// request that carried the LARGEST context (the main conversation, not a subagent) as of now.
func (d *DB) liveSessions(f Filter, now int64) ([]LiveSession, error) {
	lf := Filter{Tenant: f.Tenant, TenantAll: f.TenantAll, Since: now - liveLookback, WithKeepAlive: true}
	cond, args := lf.where()
	rows, err := d.sql.QueryContext(d.readCtx(), `SELECT r.ts, r.session_id, r.model, r.messages,
		r.fresh_input, r.cache_read, r.cache_write, r.cache_write_1h FROM requests r
		WHERE `+cond+` AND r.token_accounting = 'complete' ORDER BY r.ts`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type acc struct {
		LiveSession
		touch int64
	}
	by := map[string]*acc{}
	for rows.Next() {
		var ts, fresh, read, write, w1h int64
		var sess, model string
		var msgs int
		if err := rows.Scan(&ts, &sess, &model, &msgs, &fresh, &read, &write, &w1h); err != nil {
			return nil, err
		}
		ctx := fresh + read + write
		a := by[sess]
		if a == nil {
			a = &acc{}
			by[sess] = a
			a.Session = sess
		}
		a.touch = ts                                   // the last request of ANY thread refreshed some cache entry
		if ctx*2 >= a.Context || ts-a.LastTS > ttl1h { // keep the main thread's numbers
			if ctx >= a.Context/2 {
				a.Model, a.LastTS, a.Context, a.Messages = model, ts, ctx, msgs
				a.TTL1h = w1h > 0 || (a.TTL1h && write == 0)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []LiveSession
	for _, a := range by {
		a.TTLMs = ttl5m
		if a.TTL1h {
			a.TTLMs = ttl1h
		}
		a.LastTS = a.touch
		out = append(out, a.LiveSession)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastTS > out[j].LastTS })
	return out, nil
}

func (a *API) spendRoutes() []route {
	return []route{
		{"GET /api/spend/explain", scopeTenant, a.spendExplain},
		{"GET /api/tools/last-used", scopeTenant, a.toolsLastUsed},
	}
}

func (a *API) spendExplain(w http.ResponseWriter, r *http.Request) {
	f, p, ok := a.scope(r)
	if !ok {
		a.unauthorized(w)
		return
	}
	q := r.URL.Query()
	tz, _ := strconv.Atoi(q.Get("tz"))
	now := atoi64(q.Get("now"))
	if now <= 0 {
		now = time.Now().UnixMilli()
	}
	top := atoiDefault(q.Get("top"), 8)
	price := a.priceFn(r)
	a.serveJSON(w, r, &a.spendCache, cacheKey(p, r), func(db *DB) ([]byte, error) {
		rep, err := db.SpendExplainFor(f, price, tz, now, top)
		if err != nil {
			return nil, err
		}
		return json.Marshal(rep)
	})
}

// toolLastUsed is when a tool, MCP tool or skill was last invoked in ANY retained session, so the
// plugin can warn before suggesting removal of something used recently. Names only, no content.
type toolLastUsed struct {
	Name  string `json:"name"`
	Skill string `json:"skill,omitempty"`
	Calls int64  `json:"calls"`
	Last  int64  `json:"last_ts"`
}

func (d *DB) ToolsLastUsed(f Filter) ([]toolLastUsed, error) {
	q := `SELECT name, skill, SUM(calls), MAX(last_ts) FROM tool_uses`
	var args []any
	if !f.TenantAll {
		q += ` WHERE tenant_id = ?`
		args = append(args, f.Tenant)
	}
	rows, err := d.sql.QueryContext(d.readCtx(), q+` GROUP BY name, skill`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []toolLastUsed{}
	for rows.Next() {
		var t toolLastUsed
		if err := rows.Scan(&t.Name, &t.Skill, &t.Calls, &t.Last); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (a *API) toolsLastUsed(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		a.unauthorized(w)
		return
	}
	out, err := a.db(r).ToolsLastUsed(f)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"tools": out})
}
