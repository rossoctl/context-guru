package dash

import (
	"database/sql"
	"net/http"
	"sort"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/keepalive"
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
	TTLSeconds int64 `json:"ttl_s"`  // 1800 on OpenAI GPT-5.6+, 3600 if the last cache write was billed at the 1h tier or the last touch was a free 1-hour ping (#1243), else 300
	// TTLUnproven is true while ttl_s is 3600 only because of a free 1-hour ping on a route not yet proven to
	// grant the tier. The clients draw the clock with a "1h?" mark. Omitted otherwise.
	TTLUnproven bool `json:"ttl_unproven,omitempty"`
	// FreeHourPing is true while ttl_s is 3600 because the last touch was a free 1-hour ping, proven or not.
	// A manual ping would move that entry back to 5 minutes, so the clients hide the Ping button. Omitted otherwise.
	FreeHourPing bool `json:"free_hour_ping,omitempty"`
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
	// NextWarmUSD and NextColdUSD price the next request's cached prefix both ways: read from a
	// warm cache (PrefixTokens x the cache-read rate), or rewritten into a cold one (the same
	// as RewriteUSD). Both omitted when the model has no price. A client built before they
	// existed falls back to RewriteUSD for the cold case.
	NextWarmUSD *float64 `json:"next_warm_usd,omitempty"`
	NextColdUSD *float64 `json:"next_cold_usd,omitempty"`
	// Pingable is true when the proxy holds this session's main thread's body and key, so POST
	// /api/me/cache/ping can work (with keep-alive off and no opt-in it holds nothing and answers 409
	// "not held"). PingReason says why not, when it is false: "not opted in", "too large", "expired",
	// "over budget", "not worth it", "unsupported", "pings used", "stopped", "below limits", "done" or "no entry". Both are omitted when the host cannot say, so a client treats
	// absence as "unknown".
	Pingable   *bool  `json:"pingable,omitempty"`
	PingReason string `json:"ping_reason,omitempty"`
	// Window is the served context window for Model, Exact=false when it is a guess.
	Window      int  `json:"window"`
	WindowExact bool `json:"window_exact"`
	// PingsLeft and HeldUntilMS are the main thread's keep-alive plan (see ThreadCache).
	PingsLeft   int   `json:"pings_left,omitempty"`
	HeldUntilMS int64 `json:"held_until_ms,omitempty"`
	// NextPingMS is when the next planned ping fires (epoch ms). Omitted when no ping is planned.
	// The mod hides its Ping button while this is before the cache expires.
	NextPingMS int64 `json:"next_ping_ms,omitempty"`
	// Money is the session's `saved $x of $y`, main thread and the rest, over the whole session.
	Money *SessionMoney `json:"money,omitempty"`
	// Threads is the session's cache entries, one per thread (the main agent and each subagent
	// have separate provider caches), most recently used first. Only threads that touched the
	// cache within the last two hours are listed: older ones are finished work.
	Threads []ThreadCache `json:"threads,omitempty"`
}

// ThreadCache is one thread's cache entry. Main is the session's primary thread.
type ThreadCache struct {
	ID         string `json:"id"`
	Main       bool   `json:"main"`
	Model      string `json:"model"`
	LastTS     int64  `json:"last_ts"`
	EndTS      int64  `json:"end_ts"`
	TTLSeconds int64  `json:"ttl_s"`
	// provider is the row's provider, with Model it gives the lifetime (apply.EntryLifetime). Not sent.
	provider string
	// TTLUnproven: see CacheState.
	TTLUnproven  bool    `json:"ttl_unproven,omitempty"`
	FreeHourPing bool    `json:"free_hour_ping,omitempty"` // see CacheState
	PrefixTokens int64   `json:"prefix_tokens"`
	RewriteUSD   float64 `json:"rewrite_usd"`
	Priced       bool    `json:"priced"`
	// NextWarmUSD and NextColdUSD: see CacheState.
	NextWarmUSD *float64 `json:"next_warm_usd,omitempty"`
	NextColdUSD *float64 `json:"next_cold_usd,omitempty"`
	// Pingable and PingReason: see CacheState.
	Pingable   *bool  `json:"pingable,omitempty"`
	PingReason string `json:"ping_reason,omitempty"`
	// PingsLeft is how many keep-alive pings the proxy still plans for this thread, and
	// HeldUntilMS when its cache goes cold if no real request arrives first: the last planned
	// ping's start plus the TTL. Both omitted when no ping is planned.
	PingsLeft   int   `json:"pings_left,omitempty"`
	HeldUntilMS int64 `json:"held_until_ms,omitempty"`
	// NextPingMS is when the next planned ping fires (epoch ms), omitted when none is planned.
	NextPingMS int64 `json:"next_ping_ms,omitempty"`
	// SavedUSD and BaselineUSD are this thread's `saved $x of $y` (see threadMoney).
	SavedUSD    float64 `json:"saved_usd"`
	BaselineUSD float64 `json:"baseline_usd"`
}

// KeepAlivePlan is one thread's scheduled keep-alive, as the proxy's keeper holds it in memory.
type KeepAlivePlan struct {
	RefreshedMS int64 // when the provider's entry was last known refreshed (epoch ms)
	NextPingMS  int64 // when the next ping fires (epoch ms)
	PingsLeft   int   // pings still to send, the next one included
	IdleMS      int64 // the spacing between pings
}

// heldUntil is when a thread's cache goes cold with this plan. Each planned ping extends the entry
// only if it fires before the entry has expired, so the chain starts from the last REAL refresh,
// not from a ping that was refused or failed, and stops at the first ping that would come too late.
func (p KeepAlivePlan) heldUntil(ttlS int64) int64 {
	end := p.RefreshedMS + ttlS*1000
	for i := 0; i < p.PingsLeft; i++ {
		at := p.NextPingMS + int64(i)*p.IdleMS
		if at >= end {
			break
		}
		end = at + ttlS*1000
	}
	return end
}

// SetKeepAlivePlans supplies the keeper's per-thread plan for one session, keyed by thread id.
// dash holds no keep-alive state of its own; nil means no plan is ever shown.
func (a *API) SetKeepAlivePlans(fn func(tenantID string, allTenants bool, session string) map[string]KeepAlivePlan) {
	a.kaPlans = fn
}

// OneHourTouch is the fact (read by the keeper from the store's one record, apply.LiveOneHourPing) that a thread's last touch was a FREE 1-hour ping: the keep-alive
// ping asked every cache marker at 1 hour and read the prefix without writing. The entry then lives an
// hour from the ping, so the clock counts an hour from it, until the thread's next real request. Metadata only.
type OneHourTouch struct {
	// AtMS is when the ping started (epoch ms). The fact ends an hour after it, and holds only while the thread's
	// last touch is within oneHourSlackMS of it.
	AtMS int64
	// Proven is true when the route is proven to grant the 1-hour tier. False: the clock is a belief the
	// thread's return will test, and the clients mark it ("1h?").
	Proven bool
}

// SetOneHourTouches supplies the keeper's per-thread 1-hour-ping facts for one session, keyed by thread
// id. dash keeps none of its own; nil means the clock is never lengthened.
func (a *API) SetOneHourTouches(fn func(tenantID string, allTenants bool, session string) map[string]OneHourTouch) {
	a.oneHour = fn
}

// oneHourSlackMS is how far after the ping's start the ping's own row may sit. The row is recorded when the
// response arrives, and a ping times out at 60 s, so 2 minutes covers it. A row further past the ping is a later
// touch (a manual press, a request): the entry is no longer the 1-hour one the ping made.
const oneHourSlackMS = 2 * 60 * 1000

// applyOneHour lengthens the clock of every entry whose last touch was a free 1-hour ping to the hour, and
// marks it unproven while the route is. The session entry follows the thread that holds the same touch.
// Prices keep the tier the entry was billed at: the next request writes its own tier, not the ping's.
// It changes nothing for an entry already at an hour or more (a client that asks 1h itself).
func applyOneHour(s *CacheState, touches map[string]OneHourTouch) {
	for i := range s.Threads {
		t := &s.Threads[i]
		// The lifetime of an entry the last touch left at 1 hour, for THIS provider and model: 30 minutes on
		// OpenAI GPT-5.6+, where no free 1-hour ping exists and nothing is lengthened.
		hour := touchTTLSeconds(t.provider, t.Model)
		o, ok := touches[t.ID]
		// The touch counts only while the thread's last touch is that ping: a newer row means a later touch
		// moved the entry (defense in depth; the keeper also drops the fact at a request, and refuses a press while it is live).
		if !ok || t.TTLSeconds >= hour || t.LastTS > o.AtMS+oneHourSlackMS {
			continue
		}
		t.TTLSeconds, t.TTLUnproven, t.FreeHourPing = hour, !o.Proven, true
		// A thread already at an hour from a real 1-hour write is skipped above and carries no flag, so the button
		// shows. If it also holds a live free-ping touch, the keeper still refuses the press (ErrAtOneHour): safe,
		// because that press sends the entry's own 1-hour markers and shortens nothing, but the user sees a 409.
		if s.LastTS == t.LastTS && s.TTLSeconds < hour {
			s.TTLSeconds, s.TTLUnproven = hour, !o.Proven
			// Only the main thread's touch hides the button: a sub-agent's free ping must not hide it for a
			// main-thread press the keeper would allow.
			s.FreeHourPing = t.Main
		}
	}
}

// PingState is one thread's answer to "can a manual ping work now": Pingable, or the Reason it cannot.
type PingState struct {
	Pingable bool
	Reason   string
}

// SetPingStates supplies, per thread of one session, whether the keeper holds what a manual ping
// needs and why not when it does not. dash keeps no keep-alive state; nil means it is never stated.
func (a *API) SetPingStates(fn func(tenantID string, allTenants bool, session string) map[string]PingState) {
	a.pingable = fn
}

// reasonIf is the reason only when the thread cannot be pinged.
func reasonIf(st PingState) string {
	if st.Pingable {
		return ""
	}
	return st.Reason
}

// pingableFor is the host's per-thread answer, nil when it cannot say (no source, or no keeper).
func (a *API) pingableFor(f Filter) map[string]PingState {
	if a.pingable == nil {
		return nil
	}
	return a.pingable(f.Tenant, f.TenantAll, f.Session)
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

// ThreadsFor lists the session's per-thread cache entries touched since sinceMS (at most 16, most
// recent first) and the session's money split into the main thread and every other one. Two
// indexed reads of the session's rows (cacheThreadsQuery, cacheTTLQuery), whatever the number of
// threads.
func (d *DB) ThreadsFor(f Filter, sinceMS int64) ([]ThreadCache, *SessionMoney, error) {
	ttls, err := d.threadTTLs(f)
	if err != nil {
		return nil, nil, err
	}
	q, args := cacheThreadsQuery(f)
	rows, err := d.sql.QueryContext(d.readCtx(), q, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var (
		out   []ThreadCache
		money SessionMoney
	)
	for rows.Next() {
		var (
			t                                    ThreadCache
			last                                 sql.NullInt64
			ms                                   sql.NullFloat64
			model, provider                      sql.NullString
			prefix                               sql.NullInt64
			base, cost, cgllm, split, kaSv, ping float64
			manualSaved, manualCost              float64
			deferNet                             float64
		)
		if err := rows.Scan(&t.ID, &last, &ms, &model, &provider, &prefix,
			&base, &cost, &cgllm, &split, &kaSv, &ping, &manualSaved, &manualCost, &deferNet); err != nil {
			return nil, nil, err
		}
		t.provider = provider.String
		// A thread with no write has no billed tier: its lifetime is the provider's and the model's base one
		// (OpenAI GPT-5.6+: 30 minutes), not a fixed 5 minutes.
		t.TTLSeconds = billedTTL(t.provider, model.String, 0)
		if ttl, ok := ttls[t.ID]; ok {
			t.TTLSeconds = ttl
		}
		t.SavedUSD, t.BaselineUSD = threadMoney(base, cost, cgllm, split, kaSv, ping, manualSaved, manualCost, deferNet)
		m := &money.Others
		if t.ID == "" {
			m = &money.Main
		}
		m.SavedUSD, m.BaselineUSD, m.Threads = m.SavedUSD+t.SavedUSD, m.BaselineUSD+t.BaselineUSD, m.Threads+1
		if !last.Valid || last.Int64 < sinceMS {
			continue // no cache touch in the window: money only
		}
		t.LastTS, t.Model, t.PrefixTokens = last.Int64, model.String, prefix.Int64
		t.Main, t.EndTS = t.ID == "", endTS(t.LastTS, ms.Float64)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastTS > out[j].LastTS })
	if len(out) > maxCacheThreads {
		out = out[:maxCacheThreads]
	}
	return out, &money, nil
}

// maxCacheThreads bounds the cache entries one /api/cachestate lists.
const maxCacheThreads = 16

// threadTTLs maps each thread of the session to the TTL its latest cache write was billed at: 1800
// on OpenAI GPT-5.6+, 3600 for the 1h tier, else 300. A thread with no write is absent (ThreadsFor then takes the base lifetime of its provider and model: 300, or 1800 on OpenAI GPT-5.6+).
func (d *DB) threadTTLs(f Filter) (map[string]int64, error) {
	q, args := cacheTTLQuery(f)
	rows, err := d.sql.QueryContext(d.readCtx(), q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var (
			id, provider, model string
			last, oneHour       int64
		)
		if err := rows.Scan(&id, &last, &oneHour, &provider, &model); err != nil {
			return nil, err
		}
		out[id] = billedTTL(provider, model, oneHour)
	}
	return out, rows.Err()
}

// billedTierSQL is 1 when the HEAD of the write on row alias lives 1 hour, else 0. A mixed write
// (1h head, 5m tail) is 1: the panel's clock follows the head. The rule is
// keepalive.HeadLifetimeOneHour, through keepalive.HeadLifetimeOneHourWriteSQL. SQL cannot
// parse a model name, so it gives only the BILLED tier; the Go side finishes the lifetime with
// modelinfo.CacheLifetime from the row's provider and model (billedTTL).
func billedTierSQL(alias string) string {
	return "CASE WHEN " + keepalive.HeadLifetimeOneHourWriteSQL(alias) + " THEN 1 ELSE 0 END"
}

// billedTTL is the lifetime in seconds of an entry written at the billed tier.
func billedTTL(provider, model string, oneHour int64) int64 {
	return int64(apply.EntryLifetime(apply.CacheFacts{Provider: bschemas.ModelProvider(provider), Model: model,
		BilledOneHour: oneHour == 1}) / time.Second)
}

// touchTTLSeconds is the lifetime in seconds of an entry whose last touch left it at the 1-hour tier.
func touchTTLSeconds(provider, model string) int64 {
	return int64(apply.EntryLifetime(apply.CacheFacts{Provider: bschemas.ModelProvider(provider), Model: model,
		LastOneHour: true}) / time.Second)
}

// cacheTTLQuery is threadTTLs' one statement: GROUP BY thread over the session's cache writes
// (agent rows and pings, any status), with the TTL taken as a bare column from the row that holds
// the one MAX(ts), so from the thread's latest write. It replaces a correlated subquery per
// thread, which walked the session's rows once for each thread: 1.6 s for 300 threads (#570).
// Like that subquery, it reads every write of the session: the request's other filters (a time
// window, a model) do not change which tier the provider's cache entry is on.
func cacheTTLQuery(f Filter) (string, []any) {
	cond, args := withKeepAlive(Filter{Tenant: f.Tenant, TenantAll: f.TenantAll, Session: f.Session}).where()
	return `SELECT r.thread_id, MAX(r.ts), ` + billedTierSQL("r.") + `, r.provider, r.model
		FROM requests r WHERE ` + cond + ` AND r.cache_write > 0 GROUP BY r.thread_id`, args
}

// cacheThreadsQuery is ThreadsFor's money and cache statement, pulled out for the EXPLAIN-plan
// test. GROUP BY thread over every row of the session (agent rows and pings, any status): the
// money columns sum all of them, while the cache columns come from the thread's latest successful
// cache-touching row. That works because the select has exactly one MAX(), and SQLite then takes
// the bare columns (upstream_ms, model and the prefix) from the row that holds it. A thread with
// no such row has a NULL MAX and is reported for its money only. Its TTL comes from cacheTTLQuery:
// a correlated subquery here would run once per thread.
func cacheThreadsQuery(f Filter) (string, []any) {
	cond, args := withKeepAlive(f).where()
	const agent = "r.keepalive = 0"
	return `SELECT r.thread_id,
		MAX(CASE WHEN r.status BETWEEN 200 AND 299 AND r.cache_read + r.cache_write > 0 THEN r.ts END),
		r.upstream_ms, r.model, r.provider, r.fresh_input + r.cache_read + r.cache_write,
		COALESCE(SUM(CASE WHEN ` + agent + ` THEN r.baseline_cost_usd ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN ` + agent + ` THEN r.cost_usd ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN ` + agent + ` THEN r.cg_llm_cost_usd ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN ` + agent + ` THEN r.cachesplit_saved_usd ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN ` + agent + ` THEN ` + kaSavedSince("r.", f.Since) + ` ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN ` + autoPing + ` THEN r.cost_usd ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN ` + manualPing + ` THEN r.manual_saved_usd ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN ` + manualPing + ` THEN r.cost_usd ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN ` + agent + ` THEN ` + deferNetCol + ` ELSE 0 END),0)
		FROM requests r WHERE ` + cond + ` GROUP BY r.thread_id`, args
}

// threadMoney is one thread's `saved $x of $y` on one basis. y is what its requests would have cost
// without context-guru, at the same prices: the baseline (the requests uncompacted) plus the
// cache-split and keep-alive savings, which the baseline does not contain. x is y minus what was
// actually paid: the requests, context-guru's own model calls and its keep-alive pings. So x <= y,
// and x is the same net as savingsArithmetic's total. Only savings written per request are in it;
// the two credits priced on read (CachesplitHistoricalUSD, an estimate, and DeclFilterSavings,
// which needs Go-side pricing per row) are not. Tool deferral's net (#888) is in: the definitions it
// left out are outside the baseline, so like the split and keep-alive savings it raises y too.
//
// The user's own pings join both sides: the write they avoided raises y, and their cost lowers x
// like any other spend, so pressing Ping moves `saved` by (avoided - cost).
func threadMoney(baseline, cost, cgllm, split, kaSv, ping, manualSaved, manualCost, deferNet float64) (saved, base float64) {
	_, _, saved = savingsArithmetic(cost, baseline, cgllm, split, kaSv, ping, manualSaved-manualCost, deferNet)
	return saved, baseline + split + kaSv + manualSaved + deferNet
}

// SessionMoney is a session's `saved $x of $y` split into the main thread and the sum of every
// other thread (sub-agents, teammates, and side requests such as Claude Code's classifier).
type SessionMoney struct {
	Main   ThreadMoney `json:"main"`
	Others ThreadMoney `json:"others"`
}

// ThreadMoney is `saved $x of $y` for one or more threads; see threadMoney. Threads counts them.
type ThreadMoney struct {
	SavedUSD    float64 `json:"saved_usd"`
	BaselineUSD float64 `json:"baseline_usd"`
	Threads     int     `json:"threads"`
}

// CacheStateFor reads the session's most recent cache-touching request. nil, nil = none yet.
func (d *DB) CacheStateFor(f Filter) (*CacheState, error) {
	cond, args := withKeepAlive(f).where()
	var (
		s         CacheState
		fresh     int64
		kaI       int
		upMS      float64
		oneHour   int64
		provider  string
		lifeModel string
	)
	// ONE row decides the lifetime: the session's latest write w gives the billed tier, the
	// provider and the model alike (as threadTTLs does). A session that changed model mid-way
	// must not take the tier from one row and the model gate from another. With no write yet,
	// the latest request r supplies provider and model, and the tier is 0.
	err := d.sql.QueryRowContext(d.readCtx(), `SELECT r.ts, r.upstream_ms, r.model, r.fresh_input, r.cache_read, r.cache_write, r.keepalive,
		COALESCE((SELECT `+billedTierSQL("w.")+` FROM requests w
			WHERE w.session_id = r.session_id AND w.tenant_id = r.tenant_id AND w.cache_write > 0
			ORDER BY w.ts DESC LIMIT 1), 0),
		COALESCE((SELECT w.provider FROM requests w
			WHERE w.session_id = r.session_id AND w.tenant_id = r.tenant_id AND w.cache_write > 0
			ORDER BY w.ts DESC LIMIT 1), r.provider),
		COALESCE((SELECT w.model FROM requests w
			WHERE w.session_id = r.session_id AND w.tenant_id = r.tenant_id AND w.cache_write > 0
			ORDER BY w.ts DESC LIMIT 1), r.model)
		FROM requests r WHERE `+cond+` AND r.status BETWEEN 200 AND 299
		AND r.cache_read + r.cache_write > 0 ORDER BY r.ts DESC LIMIT 1`, args...).Scan(
		&s.LastTS, &upMS, &s.Model, &fresh, &s.CacheRead, &s.CacheWrite, &kaI, &oneHour, &provider, &lifeModel)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.TTLSeconds = billedTTL(provider, lifeModel, oneHour)
	s.Session, s.KeepAlive, s.EndTS = f.Session, kaI == 1, endTS(s.LastTS, upMS)
	s.PrefixTokens = fresh + s.CacheRead + s.CacheWrite
	return &s, nil
}

const (
	// NextInputWindow is how many of a session's last billed requests set its next-request size.
	NextInputWindow = 10
	// DefaultNextInputTokens is the new input of a next request when the session has no history.
	// It is a user turn plus a tool result, not a ping. The value, 1,000 tokens, is the rounded
	// median of fresh_input + cache_write over a local dashboard sample of billed requests (pings
	// excluded), mostly Anthropic. No OpenAI sample yet.
	DefaultNextInputTokens = 1000
)

// NextInputTokens is the new input to expect on the next real request of a model in the session:
// the median of fresh_input + cache_write (the tokens not read from the cache; on Anthropic a new
// turn is billed as cache write) of the last NextInputWindow billed requests of that model (pings
// and non-2xx rows excluded). With none, the same median over the whole session. With none either,
// or on a read error, DefaultNextInputTokens. It reads token counts only. DefaultNextInputTokens
// lives here because only the dashboard's own database can measure it.
func (d *DB) NextInputTokens(f Filter, model string) int64 {
	f.WithKeepAlive = false
	if model != "" {
		f.Model = model
		if v, ok := d.nextInputSizes(f); !ok {
			return DefaultNextInputTokens
		} else if len(v) > 0 {
			return medianNextInput(v)
		}
		f.Model = ""
	}
	v, _ := d.nextInputSizes(f)
	return medianNextInput(v)
}

// nextInputSizes reads the sizes of the last NextInputWindow billed requests of f. ok is false on
// any read error: a partial list is never returned.
func (d *DB) nextInputSizes(f Filter) (v []int64, ok bool) {
	cond, args := f.where()
	rows, err := d.sql.QueryContext(d.readCtx(), `SELECT r.fresh_input + r.cache_write FROM requests r WHERE `+cond+
		` AND r.status BETWEEN 200 AND 299 AND r.cache_read + r.cache_write + r.fresh_input > 0
		ORDER BY r.ts DESC LIMIT ?`, append(args, NextInputWindow)...)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, false
		}
		v = append(v, n)
	}
	if rows.Err() != nil {
		return nil, false
	}
	return v, true
}

// medianNextInput is the median of v (the mean of the two middle values for an even count), or
// the default for no values.
func medianNextInput(v []int64) int64 {
	if len(v) == 0 {
		return DefaultNextInputTokens
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	if len(v)%2 == 1 {
		return v[len(v)/2]
	}
	return (v[len(v)/2-1] + v[len(v)/2]) / 2
}

// NextRequestUSD prices a cached prefix of `tokens` for the next request: warm is a cache read, cold
// is a rewrite at the write rate of the tier the cache was written at (2.0x input on the 1h tier;
// same derivation as CostWithCacheWrite1h). nextIn is the new input that request adds to the
// prefix (NextInputTokens); it picks the price tier. One definition for /api/cachestate and the
// manual ping, so the "kept warm" credit is the cold figure the status line already showed.
func NextRequestUSD(p modelinfo.Price, tokens, nextIn, ttlS int64) (warm, cold float64) {
	p = p.At(tokens + nextIn) // the next request sends this prefix plus new input: its tier's rates
	if ttlS == int64(modelinfo.AnthropicCacheTTL1h/time.Second) {
		return float64(tokens) * p.CacheRead, float64(tokens) * p.CacheWrite1h()
	}
	return float64(tokens) * p.CacheRead, float64(tokens) * p.CacheWrite
}

// rewrite is next's cold half, the figure RewriteUSD has always carried.
func rewrite(next func(string, int64, int64) (float64, float64, bool), model string, tokens, ttl int64) (float64, bool) {
	_, cold, ok := next(model, tokens, ttl)
	return cold, ok
}

// nextPtrs is next as the two omit-when-unpriced response fields.
func nextPtrs(next func(string, int64, int64) (float64, float64, bool), model string, tokens, ttl int64) (warm, cold *float64) {
	w, c, ok := next(model, tokens, ttl)
	if !ok {
		return nil, nil
	}
	return &w, &c
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
		// No cache touch yet, but the session may already have cost (and saved) money.
		body := map[string]any{"session": f.Session, "none": true, "now_ms": time.Now().UnixMilli()}
		if _, money, err := a.db(r).ThreadsFor(f, 0); err == nil && money.Main.Threads+money.Others.Threads > 0 {
			body["money"] = money
		}
		writeJSON(w, body)
		return
	}
	s.NowMS = time.Now().UnixMilli()
	pf := a.priceFn(r)
	// next prices a cached prefix of `tokens` for the NEXT request: cold is the rewrite (the cache
	// write rate, 2.0x input on the 1h tier), warm is the cache-read rate.
	sizes := map[string]int64{} // the next-request size of each model, read once
	next := func(model string, tokens, ttl int64) (warm, cold float64, ok bool) {
		if pf == nil {
			return 0, 0, false
		}
		p, ok := pf(model)
		if !ok {
			return 0, 0, false
		}
		nextIn, seen := sizes[model]
		if !seen { // only a priced model reads the database
			nextIn = a.db(r).NextInputTokens(f, model)
			sizes[model] = nextIn
		}
		warm, cold = NextRequestUSD(p, tokens, nextIn, ttl)
		return warm, cold, true
	}
	s.RewriteUSD, s.Priced = rewrite(next, s.Model, s.PrefixTokens, s.TTLSeconds)
	s.NextWarmUSD, s.NextColdUSD = nextPtrs(next, s.Model, s.PrefixTokens, s.TTLSeconds)
	if ths, money, err := a.db(r).ThreadsFor(f, s.NowMS-2*3600*1000); err == nil {
		s.Money = money
		for i := range ths {
			ths[i].RewriteUSD, ths[i].Priced = rewrite(next, ths[i].Model, ths[i].PrefixTokens, ths[i].TTLSeconds)
			ths[i].NextWarmUSD, ths[i].NextColdUSD = nextPtrs(next, ths[i].Model, ths[i].PrefixTokens, ths[i].TTLSeconds)
		}
		s.Threads = ths
	}
	if a.kaPlans != nil {
		plans := a.kaPlans(f.Tenant, f.TenantAll, f.Session)
		for i := range s.Threads {
			if p, ok := plans[s.Threads[i].ID]; ok && p.PingsLeft > 0 {
				s.Threads[i].PingsLeft, s.Threads[i].HeldUntilMS = p.PingsLeft, p.heldUntil(s.Threads[i].TTLSeconds)
				s.Threads[i].NextPingMS = p.NextPingMS
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
			s.NextPingMS = p.NextPingMS
		}
	}
	// After the prices, which keep the tier the entry was billed at.
	if a.oneHour != nil {
		applyOneHour(s, a.oneHour(f.Tenant, f.TenantAll, f.Session))
	}
	if held := a.pingableFor(f); held != nil {
		// A thread the host knows nothing about (a restart, or no request since) has no entry.
		state := func(id string) PingState {
			if st, ok := held[id]; ok {
				return st
			}
			return PingState{Reason: "no entry"}
		}
		for i := range s.Threads {
			st := state(s.Threads[i].ID)
			s.Threads[i].Pingable, s.Threads[i].PingReason = &st.Pingable, reasonIf(st)
		}
		st := state("")
		s.Pingable, s.PingReason = &st.Pingable, reasonIf(st)
	}
	s.Window, s.WindowExact, _ = modelinfo.Exact(a.windows, r.Context(), s.Model)
	writeJSON(w, s)
}
