package predictive

import (
	"math"
	"sort"
	"strings"
	"time"
)

// GBSNames are the 72 inputs of the LightGBM discrete-time hazard, in training order (scripts/campaign/gbsfeat.py NAMES): 22 request columns and 50
// thread / session features. Categorical ones are strings in Row.Cat and are coded by the model with its own table.
var GBSNames = [NGBS]string{
	"messages", "tools", "system_blocks", "stream", "session_turn", "turn_in_thread", "hour_utc", "dow", "prefix_tok", "output_tok", "read_cost", "rebuild_cost", "ping_cost", "cost_usd", "fresh_input",
	"stake", "ping_over_stake", "family", "agent_type", "provider", "stop_reason", "thinking",
	"m_n_req_thread", "m_n_req_session", "m_session_age_s", "m_thread_age_s", "m_ctx_tokens", "m_ctx_window", "m_ctx_fill", "m_cache_read_ratio", "m_cache_write_ratio", "m_fresh_ratio",
	"m_rewrite", "m_rewrites_last5", "m_reqs_since_rewrite", "m_ctx_growth", "m_ctx_growth_ratio", "m_gap1", "m_gap2", "m_gap3", "m_gap_mean5", "m_gap_median5", "m_gap_std5", "m_gap_max10",
	"m_gap_trend5", "m_gap_ewma", "m_is_user_turn", "m_since_user_turn_s", "m_tool_streak", "m_reqs_since_user_turn", "m_messages", "m_messages_delta", "m_reqs_since_compaction", "m_compactions",
	"m_stop_reason", "m_prev_stop_reason", "m_model", "m_agent", "m_cache_ttl", "m_tools", "m_system_blocks", "m_out_tokens", "m_out_ratio", "m_resp_s", "m_ttfb_s", "m_resp_mean5", "m_thinking",
	"m_effort", "m_max_tokens", "m_status_ok", "m_cost_req", "m_cost_session_cum",
}

// NGBS is len(GBSNames); GBSBase is where they start in Row.F.
const (
	NGBS    = 72
	GBSBase = NIn
	NAll    = NIn + NGBS
)

// CatNames are the categorical GBS inputs; Row.Cat is indexed by position in this list.
var CatNames = [NCat]string{"family", "agent_type", "provider", "stop_reason", "thinking", "m_stop_reason", "m_prev_stop_reason", "m_model", "m_agent", "m_cache_ttl", "m_thinking", "m_effort"}

// NCat is len(CatNames).
const NCat = 12

// Row is every live input for one decision point. F[:NIn] is the MLP block; F[GBSBase:] the GBS block (categorical slots NaN, see Cat).
// Ctx and ModelName let a model derive the two inputs that depend on its own training table (served context window).
type Row struct {
	F         []float64
	Cat       [NCat]string
	Ctx       float64
	ModelName string
	// Skipped is true when, within the last 10 requests of the thread, an earlier request had not finished when this one's features were taken (parallel
	// requests on one thread): the response-dependent history inputs then come from the last finished request and can differ from the offline definition.
	Skipped bool
}

// NewRow allocates a Row.
func NewRow() *Row { return &Row{F: make([]float64, NAll)} }

// arrivalState is what is known when a request STARTS: the thread's idle-gap history. The gap before a request is its start minus the previous request's
// response end; a previous request still running has not ended, so the gap is 0 (the same value the offline definition gives, clipped at 0).
type arrivalState struct {
	firstTs          int64
	gaps             [10]float64 // newest first, NaN = none
	ewmaNum, ewmaDen float64
}

func newArrivalState(prev *Arrival, ts int64) (s arrivalState) {
	for i := range s.gaps {
		s.gaps[i] = nan
	}
	s.firstTs = ts
	if prev == nil {
		return s
	}
	s.firstTs = prev.ar.firstTs
	gap := 0.0
	if prev.endMs != 0 {
		gap = math.Max(float64(ts-prev.endMs)/1000, 0)
	}
	s.gaps[0] = gap
	copy(s.gaps[1:], prev.ar.gaps[:9])
	s.ewmaNum, s.ewmaDen = 0.7*prev.ar.ewmaNum+math.Log1p(gap), 0.7*prev.ar.ewmaDen+1
	return s
}

// gbsState is a thread's response-dependent aggregates after one featured request; the next request starts from its nearest featured predecessor.
type gbsState struct {
	n          int
	ctx, msgs  float64
	stop       string
	endMs      int64
	rew4       [4]float64
	lastRew    int
	userTurnTs int64
	userTurnN  int
	toolStreak int
	compCount  float64
	lastComp   int
	resp5      [5]float64
	nresp      int
	sinceSkip  int // requests since the thread last had to skip a running predecessor
}

var nan = math.NaN()

func ratio(a, b float64) float64 {
	if b == 0 || math.IsNaN(b) {
		return nan
	}
	return a / b
}

func nanStats(g []float64) (n int, mean, median, std, max float64) {
	v := make([]float64, 0, len(g))
	for _, x := range g {
		if !math.IsNaN(x) {
			v = append(v, x)
		}
	}
	n = len(v)
	if n == 0 {
		return 0, nan, nan, nan, nan
	}
	var s float64
	max = v[0]
	for _, x := range v {
		s += x
		max = math.Max(max, x)
	}
	mean = s / float64(n)
	std = nan
	if n >= 2 {
		var q float64
		for _, x := range v {
			q += (x - mean) * (x - mean)
		}
		std = math.Sqrt(q / float64(n-1))
	}
	sort.Float64s(v)
	if n%2 == 1 {
		median = v[n/2]
	} else {
		median = (v[n/2-1] + v[n/2]) / 2
	}
	return
}

// gbsFill computes the GBS block (scripts/campaign/gbsfeat.py is the reference). Called with the tracker lock held, before a's chain is cut.
func (t *Tracker) gbsFill(a *Arrival, r *Response, row *Row) {
	p := a.prev
	skipped := false
	for p != nil && p.gs == nil {
		p = p.prev
		skipped = true // an earlier request of this thread is still running: its tokens and stop reason are not known yet
	}
	var prev *gbsState
	if p != nil {
		prev = p.gs
	}
	ts := a.req.TsMs
	n := a.turnInThr + 1
	ctx := float64(r.Fresh + r.Prefix)
	ctxp := ctx
	if ctx <= 0 {
		ctxp = nan
	}
	msgs := float64(r.Messages)
	cost := r.CostUSD
	ss := t.sess[sessKey(&r.Request)]
	sessCost := cost
	if ss != nil {
		ss.cost += cost
		sessCost = ss.cost
	}
	st := &gbsState{n: n, ctx: ctx, msgs: msgs, stop: r.StopReason, endMs: r.EndMs, sinceSkip: 1000}
	switch {
	case skipped:
		st.sinceSkip = 0
	case prev != nil:
		st.sinceSkip = min(prev.sinceSkip+1, 1000)
	}
	row.Skipped = st.sinceSkip < 10
	ar := &a.ar
	// rewrites (a full-prefix rewrite is cache_write >= 0.8 of the context); the thread's first request does not count towards the windowed measures
	rew := 0.0
	if ctx > 0 && float64(r.CacheWrite) >= 0.8*ctx {
		rew = 1
	}
	rewNF := 0.0
	if n > 1 {
		rewNF = rew
	}
	last5 := rewNF
	if prev != nil {
		st.rew4[0] = rewNF
		copy(st.rew4[1:], prev.rew4[:3])
		last5 += prev.rew4[0] + prev.rew4[1] + prev.rew4[2] + prev.rew4[3]
		st.lastRew = prev.lastRew
	} else {
		st.rew4[0] = rewNF
	}
	if n > 1 && rew == 1 {
		st.lastRew = n
	}
	sinceRew := nan
	if st.lastRew > 0 {
		sinceRew = float64(n - st.lastRew)
	}
	// who started the turn
	userTurn := prev == nil || prev.stop != "tool_use"
	if userTurn {
		st.userTurnTs, st.userTurnN = ts, n
	} else {
		st.userTurnTs, st.userTurnN = prev.userTurnTs, prev.userTurnN
	}
	if r.StopReason == "tool_use" {
		st.toolStreak = 1
		if prev != nil {
			st.toolStreak += prev.toolStreak
		}
	}
	// compaction proxy
	comp := prev != nil && (msgs < 0.7*prev.msgs || ctx < 0.6*prev.ctx)
	if prev != nil {
		st.compCount, st.lastComp = prev.compCount, prev.lastComp
	}
	if comp {
		st.compCount++
		st.lastComp = n
	}
	lc := st.lastComp
	if lc == 0 {
		lc = 1
	}
	// response durations
	resp := r.UpstreamMs / 1000
	st.resp5[0] = resp
	st.nresp = 1
	if prev != nil {
		copy(st.resp5[1:], prev.resp5[:4])
		st.nresp = min(prev.nresp+1, 5)
	}
	var rs float64
	for i := 0; i < st.nresp; i++ {
		rs += st.resp5[i]
	}

	f := row.F[GBSBase:]
	stake := float64(r.Prefix) * (r.PWrite5m - r.PRead)
	end := time.UnixMilli(r.EndMs).UTC()
	hr, dow := float64(end.Hour()), float64((int(end.Weekday())+6)%7)
	i := 0
	put := func(v float64) { f[i] = v; i++ }
	put(msgs)
	put(float64(r.Tools))
	put(float64(r.SystemBlocks))
	put(b2f(r.Stream))
	put(float64(a.turnInSess))
	put(float64(a.turnInThr))
	put(hr)
	put(dow)
	put(float64(r.Prefix))
	put(float64(r.Output))
	put(float64(r.Prefix) * r.PRead)
	put(float64(r.Prefix) * r.PWrite5m)
	put(r.PingCost())
	put(cost)
	put(float64(r.Fresh))
	put(stake)
	if stake != 0 {
		put(r.PingCost() / stake)
	} else {
		put(nan)
	}
	for k := 0; k < 5; k++ { // family, agent_type, provider, stop_reason, thinking: categorical slots
		put(nan)
	}
	put(float64(n))
	put(float64(a.turnInSess + 1))
	put(float64(ts-a.sessStartMs) / 1000)
	put(float64(ts-ar.firstTs) / 1000)
	put(ctx)
	put(nan) // m_ctx_window: the model's table
	put(nan) // m_ctx_fill
	put(ratio(float64(r.Prefix-r.CacheWrite), ctxp))
	put(ratio(float64(r.CacheWrite), ctxp))
	put(ratio(float64(r.Fresh), ctxp))
	put(rew)
	put(last5)
	put(sinceRew)
	if prev != nil {
		put(ctx - prev.ctx)
		put(ratio(ctx, prev.ctx))
	} else {
		put(nan)
		put(nan)
	}
	put(ar.gaps[0])
	put(ar.gaps[1])
	put(ar.gaps[2])
	n5, mean5, med5, std5, _ := nanStats(ar.gaps[:5])
	_, _, _, _, max10 := nanStats(ar.gaps[:])
	put(mean5)
	put(med5)
	put(std5)
	put(max10)
	trend := nan
	if n5 == 5 {
		l := func(i int) float64 { return math.Log1p(ar.gaps[i]) }
		trend = (l(0)*2 + l(1) - l(3) - 2*l(4)) / 10
	}
	put(trend)
	ew := nan
	if ar.ewmaDen > 0 {
		ew = math.Expm1(ar.ewmaNum / ar.ewmaDen)
	}
	put(ew)
	put(b2f(userTurn))
	put(float64(ts-st.userTurnTs) / 1000)
	put(float64(st.toolStreak))
	put(float64(n - st.userTurnN))
	put(msgs)
	if prev != nil {
		put(msgs - prev.msgs)
	} else {
		put(nan)
	}
	put(float64(n - lc))
	put(st.compCount)
	for k := 0; k < 4; k++ { // m_stop_reason, m_prev_stop_reason, m_model, m_agent: categorical
		put(nan)
	}
	put(nan) // m_cache_ttl (categorical)
	put(float64(r.Tools))
	put(float64(r.SystemBlocks))
	put(float64(r.Output))
	put(ratio(float64(r.Output), ctxp))
	put(resp)
	put(r.TTFBMs / 1000)
	put(rs / float64(st.nresp))
	put(nan) // m_thinking (categorical)
	put(nan) // m_effort (categorical)
	put(float64(r.MaxTokens))
	put(1) // m_status_ok: only successful responses are decision points
	put(cost)
	put(sessCost)

	none := func(s string) string {
		if s == "" {
			return "none"
		}
		return s
	}
	agent := r.Agent
	if agent == "" {
		agent = "unknown"
	}
	prevStop := "first"
	if prev != nil {
		prevStop = none(prev.stop)
	}
	mm := r.Model
	for _, pre := range []string{"aws/", "azure/", "Azure/"} {
		mm = strings.TrimPrefix(mm, pre)
	}
	row.Cat = [NCat]string{family(r.Model), agent, r.Provider, r.StopReason, r.Thinking, none(r.StopReason), prevStop, mm, r.Agent, none(r.CacheTTL), none(r.Thinking), none(r.Effort)}
	row.Ctx, row.ModelName = ctx, r.Model
	a.gs = st
}
