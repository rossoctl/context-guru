package predictive

import (
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rossoctl/context-guru/kvcache"
)

// FeatureNames is the model's input vector, in order. Every one is a function of what the proxy has seen by the time a response ends:
// that response's own sizes and stop reason, the idle gaps this thread and tenant have ALREADY closed, and counters of its own traffic.
var FeatureNames = [NIn]string{
	"l_prefix", "l_out", "l_fresh", "cw_frac", "l_msgs", "l_tools", "l_turn", "l_sturn", "l_dur", "l_sage", "pg_first", "pg_pos", "pg_neg", "pg2",
	"hs", "hc", "ds", "dc", "kv_p5", "kv_p1", "kv_ln", "gq_nan", "l_gq", "think", "stream", "l_ping",
	"sr_tool_use", "sr_stop_sequence", "sr_end_turn", "sr_stop", "sr_tool_calls", "fam_opus", "fam_sonnet", "fam_haiku",
	"ag_claude-cli", "ag_openai", "ag_asyncopenai",
}

// pingFresh is the measured law of a keep-alive's own cost: ~7 fresh input tokens and 1 output token on top of the cached read.
const pingFresh = 7

const (
	gapRing    = 50   // closed gaps per tenant the quantile looks at
	gapMin     = 10   // gaps needed before the quantile exists
	cellMin    = 6    // closed gaps a history cell needs before it is trusted over its parent (kvcache.minCell)
	pruneEvery = 4096 // events between idle-state sweeps
	idleDrop   = 6 * time.Hour
)

// Request is what the proxy knows when a request STARTS.
type Request struct {
	Tenant, Session, Thread, Model string
	TsMs                           int64
}

// Response is what it knows when that request's response has ENDED. Prices are $ per token. The request's own start time
// is the one the Arrival carries (Request.TsMs here is ignored).
type Response struct {
	Request
	EndMs, DecMs                      int64  // provider-side response end, and when the proxy finished returning it (>= EndMs)
	Provider, CacheTTL, Effort        string // request facts: serving provider, cache tier asked for ("ephemeral_5m", ...), reasoning effort
	MaxTokens, SystemBlocks           int
	TTFBMs, UpstreamMs, CostUSD       float64 // time to first byte, upstream duration, billed cost of this request
	Prefix, CacheWrite, Fresh, Output int64
	Messages, Tools                   int
	StopReason, Agent, Thinking       string // Agent: the dash agent family ("claude-cli", "openai", ...)
	Stream                            bool
	PIn, POut, PRead, PWrite5m        float64
}

// PingCost is what one keep-alive ping on this prefix costs.
func (r *Response) PingCost() float64 {
	return float64(r.Prefix)*r.PRead + pingFresh*r.PIn + r.POut
}

// Arrival is the state a request carries from its start to its end: everything that depends on what came BEFORE it.
type Arrival struct {
	req         Request
	turnInThr   int
	turnInSess  int
	sessStartMs int64
	prev        *Arrival // previous request on the thread; cut once this one's features are taken
	endMs       int64    // set when this response ends; 0 while in flight
	pg          float64  // seconds from the previous response's end to this start (NaN on a thread's first request); valid once done
	done        bool
	gs          *gbsState // response-dependent thread aggregates after this request; nil until its features are taken
	ar          arrivalState
	depth       int // unfinished arrivals chained behind this one (bounded, see maxChain)
}

// maxChain bounds how many never-featured predecessors an arrival keeps reachable: a request that is dropped before its response (error, client gone, session ended)
// would otherwise extend the chain without limit.
const maxChain = 32

type threadState struct {
	last     *Arrival
	pending  []pendingGap
	recent   [16]int64 // start times of the latest arrivals, for a response that ends after its successor already started
	nrecent  int
	lastSeen int64
}

type pendingGap struct {
	startMs, endMs int64
	tenant, model  string
	bucket         kvcache.Bucket
}

type sessState struct {
	startMs int64
	turns   int
	seen    int64
	cost    float64 // billed cost of the session's featured requests so far
}

type cellKey struct {
	tenant, model string
	bucket        kvcache.Bucket
}

type cellStat struct{ n, h5, h1 int }

// Tracker accumulates, per campaign scope, the causal history the features need. All state is bounded: idle threads and sessions are dropped after six hours.
type Tracker struct {
	mu      sync.Mutex
	threads map[string]*threadState
	sess    map[string]*sessState
	cells   map[cellKey]*cellStat
	gaps    map[string][]float64 // tenant -> last gapRing closed gaps, oldest first
	events  int
}

// NewTracker returns an empty tracker.
func NewTracker() *Tracker {
	return &Tracker{threads: map[string]*threadState{}, sess: map[string]*sessState{}, cells: map[cellKey]*cellStat{}, gaps: map[string][]float64{}}
}

func threadKey(r *Request) string { return r.Tenant + "\x00" + r.Session + "\x00" + r.Thread }
func sessKey(r *Request) string   { return r.Tenant + "\x00" + r.Session }

// Arrive registers a request that is starting. Call it once per request, in start order; keep the result for Features.
func (t *Tracker) Arrive(r Request) *Arrival {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tick(r.TsMs)
	th := t.threads[threadKey(&r)]
	if th == nil {
		th = &threadState{}
		t.threads[threadKey(&r)] = th
	}
	ss := t.sess[sessKey(&r)]
	if ss == nil {
		ss = &sessState{startMs: r.TsMs}
		t.sess[sessKey(&r)] = ss
	}
	// A response that ended before this request started is a COMPATIBLE predecessor: its gap closes now.
	keep := th.pending[:0]
	for _, p := range th.pending {
		if p.endMs <= r.TsMs {
			t.closeGap(p, r.TsMs)
		} else {
			keep = append(keep, p)
		}
	}
	th.pending = keep
	a := &Arrival{req: r, turnInThr: 0, turnInSess: ss.turns, sessStartMs: ss.startMs, prev: th.last}
	a.ar = newArrivalState(th.last, r.TsMs)
	if th.last != nil && th.last.gs == nil {
		a.depth = th.last.depth + 1
		if a.depth > maxChain {
			a.prev, a.depth = nil, 0
		}
	}
	if th.last != nil {
		a.turnInThr = th.last.turnInThr + 1
	}
	ss.turns++
	ss.seen, th.lastSeen = r.TsMs, r.TsMs
	th.recent[th.nrecent%len(th.recent)] = r.TsMs
	th.nrecent++
	th.last = a
	return a
}

func (t *Tracker) closeGap(p pendingGap, nextStartMs int64) {
	gap := float64(nextStartMs-p.startMs) / 1000
	for _, k := range []cellKey{{p.tenant, p.model, p.bucket}, {p.tenant, p.model, ""}, {p.tenant, "", ""}, {"", p.model, ""}, {}} {
		c := t.cells[k]
		if c == nil {
			c = &cellStat{}
			t.cells[k] = c
		}
		c.n++
		if gap <= 300 {
			c.h5++
		}
		if gap <= 3600 {
			c.h1++
		}
	}
	g := append(t.gaps[p.tenant], gap)
	if len(g) > gapRing {
		g = g[len(g)-gapRing:]
	}
	t.gaps[p.tenant] = g
}

// tick drops state idle for six hours, now and then.
func (t *Tracker) tick(nowMs int64) {
	if t.events++; t.events%pruneEvery != 0 {
		return
	}
	cut := nowMs - idleDrop.Milliseconds()
	for k, th := range t.threads {
		if th.lastSeen < cut {
			delete(t.threads, k)
		}
	}
	for k, s := range t.sess {
		if s.seen < cut {
			delete(t.sess, k)
		}
	}
}

func lg(v float64) float64 { return math.Log1p(math.Max(v, 0)) }

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// family mirrors the research tables: opus / sonnet / haiku by substring of the lower-cased model name.
func family(model string) string {
	m := strings.ToLower(strings.TrimPrefix(model, "aws/"))
	switch {
	case strings.Contains(m, "opus"):
		return "opus"
	case strings.Contains(m, "sonnet"):
		return "sonnet"
	case strings.Contains(m, "haiku"):
		return "haiku"
	}
	return "other"
}

// pgOf is the idle seconds between the previous request's response end and a's start. A predecessor whose response has not ended
// when we ask necessarily ends after a started, so only the sign matters then (-1).
func pgOf(a *Arrival) float64 {
	switch {
	case a.done:
		return a.pg
	case a.prev == nil:
		return math.NaN()
	case a.prev.endMs == 0:
		return -1
	}
	return float64(a.req.TsMs-a.prev.endMs) / 1000
}

// quantile80 is pandas' linear-interpolated 0.8 quantile of the tenant's last closed gaps, NaN while fewer than gapMin.
func (t *Tracker) quantile80(tenant string) float64 {
	g := t.gaps[tenant]
	if len(g) < gapMin {
		return math.NaN()
	}
	s := append([]float64(nil), g...)
	sort.Float64s(s)
	pos := 0.8 * float64(len(s)-1)
	lo := int(math.Floor(pos))
	if lo+1 >= len(s) {
		return s[len(s)-1]
	}
	return s[lo] + (pos-float64(lo))*(s[lo+1]-s[lo])
}

// hist is the kvcache History fallback chain: the most specific cell with >= 6 closed gaps, else the global cell.
func (t *Tracker) hist(tenant, model string, b kvcache.Bucket) (p5, p1 float64, n int) {
	chain := []cellKey{{tenant, model, b}, {tenant, model, ""}, {tenant, "", ""}, {"", model, ""}}
	for _, k := range chain {
		if c := t.cells[k]; c != nil && c.n >= cellMin {
			return float64(c.h5) / float64(c.n), float64(c.h1) / float64(c.n), c.n
		}
	}
	if c := t.cells[cellKey{}]; c != nil && c.n > 0 {
		return float64(c.h5) / float64(c.n), float64(c.h1) / float64(c.n), c.n
	}
	return 0, 0, 0
}

// Features computes every model input for a response that has ended into row (from NewRow). Call Ended first (or at the same moment).
func (t *Tracker) Features(a *Arrival, r *Response, row *Row) {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := row.F
	pg := pgOf(a)
	pg2 := math.NaN()
	if a.prev != nil {
		pg2 = pgOf(a.prev)
	}
	first := math.IsNaN(pg)
	end := time.UnixMilli(r.EndMs).UTC()
	hr, dow := float64(end.Hour()), float64((int(end.Weekday())+6)%7)
	p5, p1, n5 := t.hist(r.Tenant, strings.ToLower(r.Model), kvcache.BucketAt(a.req.TsMs))
	gq := t.quantile80(r.Tenant)
	prefix := float64(r.Prefix)
	v := [NIn]float64{
		lg(prefix), lg(float64(r.Output)), lg(float64(r.Fresh)), float64(r.CacheWrite) / math.Max(prefix, 1), lg(float64(r.Messages)), lg(float64(r.Tools)),
		lg(float64(a.turnInThr)), lg(float64(a.turnInSess)), lg(float64(r.EndMs-a.req.TsMs) / 1000), lg(float64(a.req.TsMs-a.sessStartMs) / 1000),
		b2f(first), 0, 0, 0,
		math.Sin(2 * math.Pi * hr / 24), math.Cos(2 * math.Pi * hr / 24), math.Sin(2 * math.Pi * dow / 7), math.Cos(2 * math.Pi * dow / 7),
		p5, p1, lg(float64(n5)), b2f(math.IsNaN(gq)), 0,
		b2f(r.Thinking == "adaptive" || r.Thinking == "enabled"), b2f(r.Stream), lg(r.PingCost() * 1000),
	}
	if !first {
		v[11], v[12] = lg(pg), b2f(pg < 0)
	}
	if !math.IsNaN(pg2) {
		v[13] = lg(pg2)
	}
	if !math.IsNaN(gq) {
		v[22] = lg(gq)
	}
	copy(out, v[:26])
	for i, s := range [...]string{"tool_use", "stop_sequence", "end_turn", "stop", "tool_calls"} {
		out[26+i] = b2f(r.StopReason == s)
	}
	for i, s := range [...]string{"opus", "sonnet", "haiku"} {
		out[31+i] = b2f(family(r.Model) == s)
	}
	for i, s := range [...]string{"claude-cli", "openai", "asyncopenai"} {
		out[34+i] = b2f(r.Agent == s)
	}
	t.gbsFill(a, r, row)
	a.pg, a.done, a.prev = pg, true, nil // the chain behind this request is no longer needed
}

// Release is Ended for a request whose features are never taken (a campaign that only applies fixed plans): it drops the predecessor chain so the thread's history does not
// accumulate.
func (t *Tracker) Release(a *Arrival, r *Response) {
	t.Ended(a, r)
	t.mu.Lock()
	defer t.mu.Unlock()
	a.pg, a.done, a.prev = pgOf(a), true, nil
}

// Ended records that a response has finished, at its provider-side end: from now on a request that starts on the same thread
// closes this request's gap. A successor that already started (we were told late) closes it at once.
func (t *Tracker) Ended(a *Arrival, r *Response) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a.endMs = r.EndMs
	th := t.threads[threadKey(&r.Request)]
	if th == nil {
		return
	}
	p := pendingGap{startMs: a.req.TsMs, endMs: r.EndMs, tenant: r.Tenant, model: strings.ToLower(r.Model), bucket: kvcache.BucketAt(a.req.TsMs)}
	best := int64(math.MaxInt64)
	for i := 0; i < len(th.recent) && i < th.nrecent; i++ {
		if ts := th.recent[i]; ts >= p.endMs && ts != p.startMs && ts < best {
			best = ts
		}
	}
	if best != math.MaxInt64 {
		t.closeGap(p, best)
	} else {
		th.pending = append(th.pending, p)
	}
}
