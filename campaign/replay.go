// Package campaign holds the keep-alive campaign math that every host shares: the closed-form
// replay of a ping plan, the calibrated constants, the fidelity model behind the adapt report, the
// ship gate, the auto builder's spec, and the mapping of a suggest arm to a live strategy.
//
// A host (the proxy, Cortex) reads history and traffic, calls these functions, and keeps the
// transport, the files and the HTTP views. Nothing here imports a host package.
package campaign

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"time"

	"github.com/rossoctl/context-guru/agent"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/kvcache"
	"github.com/rossoctl/context-guru/predictive"
)

// The closed-form replay constants of scripts/campaign/cgpredict.py (plan_eval): the provider cliff, the scheduling lateness and a ping's
// duration, in seconds.
const (
	Cliff   = 296.5
	Late    = 1.0
	PingDur = 2.0
)

// BenefitScale and PingScale are scripts/campaign/engine_calibration.json (benefit_scale, ping_scale); a test keeps them equal. They calibrate the
// closed form to the pinned replay engine.
const (
	BenefitScale = 0.948
	PingScale    = 1.346
)

const (
	// AutoK is the highest ping count of the auto builder's menu.
	AutoK = 6
	// AutoX is the menu spacing. 240 s, not 280: live, pings 275-280 s after a response end miss the provider's cache 19% of the time,
	// and 240 s also leaves room for a ping's 60 s timeout inside the 5-minute cache (#707, #716). It is the shipped default idle.
	AutoX    = config.DefaultKeepAliveIdle
	autoBoot = 1000
)

// The evidence floors of a build: held-out rows and sessions (the same floors as scripts/campaign/build_campaign.py). Variables only so tests can
// run the whole build on a small fixture.
var AutoMinTst, AutoMinSes = 500, 20

// PlanOutcome is the closed-form replay of one decision: ping j starts at j*x - u + late after the response end and fires if the conversation has not
// returned (and the client has not ended the session) by then; the entry survives to the return if the last touch is within the cache lifetime of it.
func PlanOutcome(k int, x, u, gap float64, returned bool, stop float64) (fired int, rescued bool) {
	if !returned {
		gap = math.Inf(1)
	}
	touch := 0.0
	for j := 1; j <= k; j++ {
		s := float64(j)*x - u + Late
		if s >= gap || s >= stop {
			break
		}
		fired++
		touch = s + PingDur
	}
	return fired, k > 0 && returned && gap-touch < Cliff && !(gap < Cliff)
}

// Priced reports whether a row's rates let a campaign decide: a cache read must cost something and a write must cost more than a read.
func Priced(pRead, pWrite float64) bool { return pRead > 0 && pWrite > pRead }

// Stake is one avoided rewrite: the prefix times what a write costs over a read. Gateway dollars.
func Stake(prefix int64, pRead, pWrite float64) float64 { return float64(prefix) * (pWrite - pRead) }

// Score is the closed-form result of one decision, in the builder's units (gateway dollars times the calibrated scales).
type Score struct {
	Fired   int
	Rescued bool
	PingUSD float64 // PingScale x pings fired x one ping
	RescUSD float64 // BenefitScale x stake when rescued, else 0
	NetUSD  float64 // RescUSD - PingUSD
}

// ScoreDecision scores plan (k pings x x seconds apart) on a request of duration u seconds whose conversation returned after gap seconds (returned),
// when the client ends the session stop seconds after the response (+Inf for never). ping and stake are one ping and one avoided rewrite.
func ScoreDecision(k int, x, u, gap float64, returned bool, stop, ping, stake float64) Score {
	fired, rescued := PlanOutcome(k, x, u, gap, returned, stop)
	s := Score{Fired: fired, Rescued: rescued}
	s.PingUSD = PingScale * float64(fired) * ping
	s.NetUSD = -s.PingUSD
	if rescued {
		s.RescUSD = BenefitScale * stake
		s.NetUSD += s.RescUSD
	}
	return s
}

// RowFacts is what a decision point carries besides its tuner row: its timing (to replay any plan, not only the menu), its dollars at the gateway's
// rates, and the facts a campaign segment tests.
type RowFacts struct {
	U, Gap             float64 // request duration and the gap from its response end to the next request, seconds
	Returned           bool
	ToEnd              float64 // seconds from this response's end to the end of the session's last response (any model): SessionEnd cancels pings after it
	Stake, Ping        float64 // one avoided rewrite (prefix x (write - read)) and one ping, gateway $
	Turn               int     // requests of the conversation before this one
	Prefix             int64
	Hour, Dow          int // request start, UTC
	StartMs            int64
	Model, Agent, Stop string
	Cause              string  // the row's stored wait cause (waiting_for), "" on an old row
	Subagent           bool    // the request is on a subagent thread (keepalive.IsSubagentThread)
	OpenAI             bool    // an OpenAI request: the keeper gives it the general floor only
	PRead, PWrite      float64 // gateway rates of the row's model
	NextPrefix         int64   // the next request's cached prefix (0 = none or unknown)
	Span               float64 // seconds from the response end to the next request of the same session thread (any model): the keeper's entry is per thread, so its pings stop then
}

// Outcome replays plan (k pings x s apart) on the row; sessionEnd stops the pings after the session's last response.
func (f *RowFacts) Outcome(k int, x float64, sessionEnd bool) (fired int, rescued bool) {
	stop := math.Inf(1)
	if sessionEnd {
		stop = f.ToEnd
	}
	return PlanOutcome(k, x, f.U, f.Gap, f.Returned, stop)
}

// The shipped default keep-alive (config.Default*): K2 at 240 s on a prefix of at least 80K tokens, at most $0.50 a ping, never after a thread's first
// request, with SessionEnd.
func (f *RowFacts) DefaultK() int {
	if f.Turn < 1 || f.Prefix < config.DefaultKeepAliveMinPrefix || f.Ping > keepalive.DefaultMaxUSDPerPing {
		return 0
	}
	return config.DefaultKeepAliveMaxPings
}

// Gain is the net of plan (k, x) on the row: bScale x the avoided rewrite, minus pScale x the pings. Scales 1 = gateway dollars.
func (f *RowFacts) Gain(k int, x float64, sessionEnd bool, bScale, pScale float64) float64 {
	fired, rescued := f.Outcome(k, x, sessionEnd)
	g := -pScale * float64(fired) * f.Ping
	if rescued {
		g += bScale * f.Stake
	}
	return g
}

// Facts is the row as a campaign segment tests it (predictive.Facts).
func (f *RowFacts) Facts() predictive.Facts {
	return NewFacts(f.Model, f.Stop, f.Agent, f.Prefix, f.Ping, f.Hour, f.Dow)
}

// NewFacts is the one place that turns a request's attributes into the facts a campaign segment tests, for the live decision and for a replay.
func NewFacts(model, stop, agent string, prefix int64, pingCost float64, hourUTC, dowUTC int) predictive.Facts {
	return predictive.Facts{Family: predictive.FamilyOf(model), StopReason: stop, Model: model, Agent: agent, PrefixTok: float64(prefix), PingCost: pingCost,
		HourUTC: float64(hourUTC), DowUTC: float64(dowUTC)}
}

// Rows turns the dashboard dataset into tuner rows plus each row's facts (same order). A row whose outcome is not known by untilMs (its response ended
// less than the last ping plus the cache lifetime before it) is dropped, and so is one with no price or incomplete accounting. Gains are in the
// builder's units (calibrated to the pinned replay engine).
func Rows(reqs []*kvcache.Request, price func(model string) (pIn, pOut, pRead, pWrite float64, ok bool), sessionEnd bool, untilMs int64) ([]predictive.AutoRow, []RowFacts) {
	type sk struct{ user, conv string }
	lastEnd := map[sk]int64{}
	byThr := map[sk][]int64{} // request starts per thread of a session: the keeper's turns and entries are per thread
	for _, r := range reqs {
		if r.KeepAlive {
			continue
		}
		k := sk{r.User, r.ConversationID}
		lastEnd[k] = max(lastEnd[k], r.TS+int64(r.UpstreamMs))
		t := sk{r.User, r.ConversationID + "\x00" + r.ThreadID} // the keeper counts turns per thread
		byThr[t] = append(byThr[t], r.TS)
	}
	for _, ts := range byThr {
		sort.Slice(ts, func(i, j int) bool { return ts[i] < ts[j] })
	}
	byID := make(map[int64]*kvcache.Request, len(reqs))
	for _, r := range reqs {
		byID[r.ID] = r
	}
	out := make([]predictive.AutoRow, 0, len(reqs))
	facts := make([]RowFacts, 0, len(reqs))
	for _, r := range reqs {
		end := r.TS + int64(r.UpstreamMs)
		if r.KeepAlive || r.Rejected || !r.CostKnown || r.CachedContext <= 0 || float64(end)+2700e3 > float64(untilMs) {
			continue
		}
		pIn, pOut, pRead, pWrite, ok := price(r.Model)
		if !ok || pWrite <= pRead {
			continue
		}
		key, tk := sk{r.User, r.ConversationID}, sk{r.User, r.ConversationID + "\x00" + r.ThreadID}
		st := time.UnixMilli(r.TS).UTC()
		f := RowFacts{U: r.UpstreamMs / 1000, Returned: r.HasNext, ToEnd: float64(max(0, lastEnd[key]-end)) / 1000,
			Stake: Stake(r.CachedContext, pRead, pWrite), Ping: (&predictive.Response{Prefix: r.CachedContext, PIn: pIn, POut: pOut, PRead: pRead}).PingCost(),
			Turn: sort.Search(len(byThr[tk]), func(i int) bool { return byThr[tk][i] >= r.TS }), Prefix: r.CachedContext,
			Hour: st.Hour(), Dow: int(st.Weekday()), StartMs: r.TS, Model: r.Model, Agent: agent.ModelAgentOf(r.Agent, r.Provider), Stop: r.StopReason, Cause: r.WaitingFor, Subagent: keepalive.IsSubagentThread(r.ThreadID), OpenAI: r.Provider == "openai", PRead: pRead, PWrite: pWrite}
		f.Span = math.Inf(1)
		ss := byThr[tk]
		if j := sort.Search(len(ss), func(i int) bool { return ss[i] > r.TS }); j < len(ss) {
			f.Span = float64(ss[j]-end) / 1000
		}
		if r.HasNext {
			f.Gap = float64(r.NextTS-end) / 1000
			if n := byID[r.NextID]; n != nil {
				f.NextPrefix = n.CachedContext
			}
		}
		g := make([]float64, AutoK+1)
		for k := 1; k <= AutoK; k++ {
			g[k] = f.Gain(k, AutoX, sessionEnd, BenefitScale, PingScale)
		}
		out = append(out, predictive.AutoRow{Scope: r.User, Session: r.ConversationID, EndMs: end, Gain: g})
		facts = append(facts, f)
	}
	return out, facts
}

// SessionCI is the 95% interval of sum(v) by bootstrap over sessions.
func SessionCI(v []float64, sess []string, seed int64) [2]float64 {
	idx := map[string]int{}
	var tot []float64
	for i, s := range sess {
		j, ok := idx[s]
		if !ok {
			j = len(tot)
			idx[s] = j
			tot = append(tot, 0)
		}
		tot[j] += v[i]
	}
	if len(tot) == 0 {
		return [2]float64{}
	}
	rng := rand.New(rand.NewSource(seed))
	b := make([]float64, autoBoot)
	for i := range b {
		for range tot {
			b[i] += tot[rng.Intn(len(tot))]
		}
	}
	sort.Float64s(b)
	r2 := func(x float64) float64 { return math.Round(x*100) / 100 }
	return [2]float64{r2(b[autoBoot*25/1000]), r2(b[autoBoot*975/1000-1])}
}

// PlanName is a menu plan's name: "none" or "K<k>".
func PlanName(k int) string {
	if k == 0 {
		return "none"
	}
	return fmt.Sprintf("K%d", k)
}

// SumOf is the sum of v.
func SumOf(v []float64) float64 {
	t := 0.0
	for _, x := range v {
		t += x
	}
	return t
}

// GateLeg is one baseline of the ship gate: its per-row net on the held-out rows.
type GateLeg struct {
	Name string
	V    []float64
}

// GateLegs are the ship gate's baselines, in one place for the build and the adapt report: what the tenant would get anyway, blanket K3, the
// fleet plan and (when known) the shipped default. A hindsight-best blanket plan is no gate: when the tuner picks a fixed plan it can at best tie it.
func GateLegs(k3, fleet, def []float64, fleetPlan int) []GateLeg {
	legs := []GateLeg{{"blanket K3", k3}, {"the fleet plan " + PlanName(fleetPlan), fleet}}
	if def != nil {
		legs = append(legs, GateLeg{"the default", def})
	}
	return legs
}

// ShipGate is the auto builder's ship rule, shared by the build and the adapt report: the subject's held-out net is positive and its paired
// session-bootstrap interval against every leg lies above zero. failed names each leg (and the sign) that did not hold.
func ShipGate(subject []float64, legs []GateLeg, sess []string) (pass bool, failed []string) {
	pass = true
	if t := SumOf(subject); t <= 0 {
		pass = false
		failed = append(failed, fmt.Sprintf("its own net %+.2f is not positive", t))
	}
	for _, l := range legs {
		a := make([]float64, len(subject))
		for j := range a {
			a[j] = subject[j] - l.V[j]
		}
		if c := SessionCI(a, sess, 1); c[0] <= 0 {
			pass = false
			failed = append(failed, fmt.Sprintf("vs %s %+.2f [%.2f, %.2f]", l.Name, SumOf(a), c[0], c[1]))
		}
	}
	return pass, failed
}
