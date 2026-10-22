package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/predictive"
)

// Predictive keep-alive campaigns (spec cg.cache.campaign/2).
//
// A campaign is built offline from one tenant's own history (see campaignbuild.go and scripts/campaign) and then runs here in
// one of three modes:
//
//	off     loaded but inert
//	shadow  decides on live traffic, logs and scores what it WOULD have done, changes nothing
//	active  sets the number of keep-alive pings per request (CachePolicy.MaxPings) and the 280 s spacing
//
// The decision costs microseconds and needs only what the proxy sees when a response ends, so it runs inline in keeper.record.
// An active campaign acts only where the tenant's OWN policy has the keep-alive on: it can lower or shape the ping count, it never turns the mechanism on for a tenant that opted out
// (unlike a manager strategy, which forces it on). Shadow mode decides and scores whatever the tenant's setting, and retains nothing.
// A manager-created keep-alive strategy that matches the tenant wins over a campaign (it is the more explicit instruction), and
// a per-session override wins over both. The per-ping dollar ceiling is never widened.

const (
	campaignModeOff    = "off"
	campaignModeShadow = "shadow"
	campaignModeActive = "active"

	// The closed-form replay constants of scripts/campaign/cgpredict.py (plan_eval): the provider cliff, the scheduling lateness, a ping's duration, and the fitted benefit scale.
	cfTC, cfLate, cfPingDur = 296.5, 1.0, 2.0
	// cfScale and cfPingScale are scripts/campaign/engine_calibration.json (benefit_scale, ping_scale); a test keeps them equal.
	cfScale, cfPingScale = 0.948, 1.346

	predPendingMax = 4096
	predResolveTTL = 2 * time.Hour
)

// campaignMeta is the small mutable record next to a campaign's spec (<dir>/<id>/meta.json).
type campaignMeta struct {
	ID        string `json:"id"`
	Tenant    string `json:"tenant"`
	ScopeType string `json:"scope_type"`
	Mode      string `json:"mode"`
	CreatedAt int64  `json:"created_at"`
	Actor     string `json:"actor"`
	// ModeBy is who last changed the mode; Via is "adapt" for a campaign /api/adapt/cache built. A non-manager may not override a campaign a
	// manager last set, and an adapt revert undoes only the caller's own adapt campaign.
	ModeBy string `json:"mode_by,omitempty"`
	Via    string `json:"via,omitempty"`
}

// shadowStats is the running score of a campaign's decisions on live traffic. Dollars are CLOSED-FORM ESTIMATES of the same kind as the build's
// held-out table (counterfactual), not billed amounts; in active mode the billed pings carry the campaign id on their dash rows.
type shadowStats struct {
	Decisions   int64     `json:"decisions"`
	KHist       [13]int64 `json:"k_hist"`
	NoPrice     int64     `json:"skipped_no_price"`
	OptedOut    int64     `json:"active_skipped_tenant_opt_out"`
	Resolved    int64     `json:"resolved"`
	Returned    int64     `json:"returned"`
	WouldPings  int64     `json:"would_pings"`
	Rescues     int64     `json:"would_rescues"`
	EstPingUSD  float64   `json:"est_ping_usd"`
	EstRescUSD  float64   `json:"est_rescue_usd"`
	EstNetUSD   float64   `json:"est_net_usd"`
	LastDecided int64     `json:"last_decision_ms"`
}

type predPending struct {
	endMs, startMs int64
	k              int
	x              float64
	stake, ping    float64
}

type predCampaign struct {
	meta  campaignMeta
	spec  *predictive.Spec
	model predictive.Model
	tr    *predictive.Tracker

	mu      sync.Mutex
	stats   shadowStats
	pending map[string][]predPending // per thread, one entry per decided request: overlapping requests on a thread each keep their own
	npend   int
	calls   int
	logf    *os.File
}

// predDecision is what record() needs back.
type predDecision struct {
	campaign *predCampaign
	k        int
	idleS    float64
	active   bool
}

// predRuntime is the set of loaded campaigns, one per tenant.
type predRuntime struct {
	dir      string
	mu       sync.RWMutex
	byTenant map[string]*predCampaign
}

func newPredRuntime(dir string) *predRuntime {
	rt := &predRuntime{dir: dir, byTenant: map[string]*predCampaign{}}
	if dir != "" {
		rt.loadAll()
	}
	return rt
}

// forTenant is the hot-path lookup: nil for every tenant without a live campaign.
func (rt *predRuntime) forTenant(id string) *predCampaign {
	if rt == nil {
		return nil
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return rt.byTenant[id]
}

func readCampaignFiles(dir, id string) (campaignMeta, *predictive.Spec, predictive.Model, error) {
	var m campaignMeta
	base := filepath.Join(dir, id)
	b, err := os.ReadFile(filepath.Join(base, "meta.json"))
	if err != nil {
		return m, nil, nil, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, nil, nil, err
	}
	sb, err := os.ReadFile(filepath.Join(base, "spec.json"))
	if err != nil {
		return m, nil, nil, err
	}
	spec, err := predictive.ParseSpec(sb)
	if err != nil {
		return m, nil, nil, err
	}
	var model predictive.Model
	if spec.Policy.Model != nil {
		// A model file that does not match the digest the spec was built with is not the model that was evaluated: refuse it.
		mb, err := os.ReadFile(filepath.Join(base, filepath.Base(spec.Policy.Model.Ref)))
		if err != nil {
			return m, nil, nil, err
		}
		sum := sha256.Sum256(mb)
		if hex.EncodeToString(sum[:]) != spec.Policy.Model.SHA256 {
			return m, nil, nil, fmt.Errorf("model digest does not match the spec")
		}
		if model, err = predictive.ParseModel(mb); err != nil {
			return m, nil, nil, err
		}
	}
	return m, spec, model, nil
}

// load puts a campaign (read from disk) into service for its tenant, replacing any earlier one. Mode off removes it.
func (rt *predRuntime) load(id string) error {
	m, spec, model, err := readCampaignFiles(rt.dir, id)
	if err != nil {
		return err
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if old := rt.byTenant[m.Tenant]; old != nil && old.meta.ID != id {
		// one live campaign per tenant: the replaced one must not come back at the next restart
		old.meta.Mode = campaignModeOff
		_ = writeJSONFile(filepath.Join(rt.dir, old.meta.ID, "meta.json"), old.meta)
		old.close()
		delete(rt.byTenant, m.Tenant)
	}
	if cur := rt.byTenant[m.Tenant]; cur != nil && cur.meta.ID == id {
		cur.mu.Lock()
		cur.meta.Mode, cur.meta.ModeBy = m.Mode, m.ModeBy
		cur.mu.Unlock()
		if m.Mode == campaignModeOff {
			delete(rt.byTenant, m.Tenant)
			cur.close()
		}
		return nil
	}
	if m.Mode == campaignModeOff {
		return nil
	}
	pc := &predCampaign{meta: m, spec: spec, model: model, tr: predictive.NewTracker(), pending: map[string][]predPending{}}
	if f, err := os.OpenFile(filepath.Join(rt.dir, id, "shadow.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		pc.logf = f
	}
	rt.byTenant[m.Tenant] = pc
	return nil
}

func (rt *predRuntime) loadAll() {
	ents, err := os.ReadDir(rt.dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if err := rt.load(e.Name()); err != nil {
			slog.Warn("context-guru: could not load a cache campaign", "campaign", e.Name(), "err", err)
		}
	}
}

func (pc *predCampaign) close() {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.logf != nil {
		_ = pc.logf.Close()
		pc.logf = nil
	}
}

func (pc *predCampaign) snapshot() (shadowStats, string) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return pc.stats, pc.meta.Mode
}

// arrive is called when a request on a thread starts: it feeds the tracker, returns the request's Arrival (carry it to the response: it is the request's identity),
// and scores the earlier shadow decisions of this thread whose response had ended before this request started (they returned). Nil when no campaign applies.
func (rt *predRuntime) arrive(tenantID, session, thread string, now time.Time) *predictive.Arrival {
	pc := rt.forTenant(tenantID)
	if pc == nil || session == "" {
		return nil
	}
	ms := now.UnixMilli()
	key := session + "\x00" + thread
	a := pc.tr.Arrive(predictive.Request{Tenant: tenantID, Session: session, Thread: thread, TsMs: ms})
	pc.mu.Lock()
	defer pc.mu.Unlock()
	keep := pc.pending[key][:0]
	for _, p := range pc.pending[key] {
		if p.endMs <= ms {
			pc.npend--
			pc.resolveLocked(p, float64(ms-p.endMs)/1000, true)
		} else {
			keep = append(keep, p)
		}
	}
	if len(keep) == 0 {
		delete(pc.pending, key)
	} else {
		pc.pending[key] = keep
	}
	return a
}

// resolveLocked scores one decision in closed form: ping j starts at j*x - u + late after the response end, fires if the conversation has not returned yet,
// and the entry survives to the return if the last touch is within the cache lifetime of it. Same arithmetic as plan_eval in the build.
func (pc *predCampaign) resolveLocked(p predPending, gap float64, returned bool) {
	pc.resolveStopLocked(p, gap, returned, math.Inf(1))
}

// resolveStopLocked is resolveLocked when the client ended the session stop seconds after the response ended: pings that would start later are never sent.
func (pc *predCampaign) resolveStopLocked(p predPending, gap float64, returned bool, stop float64) {
	fired, rescued := planOutcome(p.k, p.x, float64(p.endMs-p.startMs)/1000, gap, returned, stop)
	st := &pc.stats
	st.Resolved++
	st.WouldPings += int64(fired)
	cost := cfPingScale * float64(fired) * p.ping
	st.EstPingUSD += cost
	net := -cost
	if returned {
		st.Returned++
	}
	if rescued {
		st.Rescues++
		st.EstRescUSD += cfScale * p.stake
		net += cfScale * p.stake
	}
	st.EstNetUSD += net
	if pc.logf != nil {
		g := any(nil)
		if returned {
			g = math.Round(gap*10) / 10
		}
		line, _ := json.Marshal(map[string]any{"t": time.Now().UnixMilli(), "k": p.k, "gap_s": g, "fired": fired, "rescued": rescued, "net_usd": math.Round(net*1e4) / 1e4})
		_, _ = pc.logf.Write(append(line, '\n'))
	}
}

// sweepLocked scores decisions that never saw a successor within predResolveTTL as no-return.
func (pc *predCampaign) sweepLocked(nowMs int64) {
	for k, ps := range pc.pending {
		keep := ps[:0]
		for _, p := range ps {
			if nowMs-p.endMs > predResolveTTL.Milliseconds() {
				pc.npend--
				pc.resolveLocked(p, 0, false)
			} else {
				keep = append(keep, p)
			}
		}
		if len(keep) == 0 {
			delete(pc.pending, k)
		} else {
			pc.pending[k] = keep
		}
	}
}

// decide runs the campaign for one finished response. ok=false means no campaign applies (the caller proceeds exactly as before).
func (k *keeper) predictiveDecide(a *predictive.Arrival, optedOut bool, tenantID, session, thread, provider string, startedAt time.Time, body []byte, userAgent string, u Usage) (d predDecision, ok bool) {
	// A campaign must never change what the keeper does by failing: any panic in the model or the tracker means "no campaign applies" for this response.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("context-guru: a cache campaign panicked; the response proceeds without it", "panic", r)
			d, ok = predDecision{}, false
		}
	}()
	return k.predictiveDecideInner(a, optedOut, tenantID, session, thread, provider, startedAt, body, userAgent, u)
}

func (k *keeper) predictiveDecideInner(a *predictive.Arrival, optedOut bool, tenantID, session, thread, provider string, startedAt time.Time, body []byte, userAgent string, u Usage) (predDecision, bool) {
	pc := k.pred.forTenant(tenantID)
	if pc == nil || u.CacheRead+u.CacheWrite <= 0 {
		return predDecision{}, false
	}
	model := gjson.GetBytes(body, "model").String()
	var pIn, pOut, pRead, pWrite float64
	if p := k.h.opts.Prices; p != nil && model != "" {
		if price, ok := p.Price(context.Background(), model); ok && !price.Zero() {
			pIn, pOut, pRead, pWrite = price.Input, price.Output, price.CacheRead, price.CacheWrite
		}
	}
	now := k.now()
	pc.mu.Lock()
	pc.calls++
	if pc.calls%256 == 0 {
		pc.sweepLocked(now.UnixMilli())
	}
	pc.mu.Unlock()
	if a == nil { // the request started before the campaign was loaded, or on a path that did not carry its arrival
		a = pc.tr.Arrive(predictive.Request{Tenant: tenantID, Session: session, Thread: thread, TsMs: startedAt.UnixMilli()})
	}
	if pRead <= 0 || pWrite <= pRead {
		pc.mu.Lock()
		pc.stats.NoPrice++
		pc.mu.Unlock()
		return predDecision{}, false
	}
	lf := u.Live
	if lf == nil { // a path that did not hand over the capture's facts: read what the body says
		lf = &LiveFacts{Messages: int(gjson.GetBytes(body, "messages.#").Int()), Tools: int(gjson.GetBytes(body, "tools.#").Int()), Thinking: gjson.GetBytes(body, "thinking.type").String(),
			Stream: gjson.GetBytes(body, "stream").Bool(), Agent: dash.AgentFor(userAgent), MaxTokens: int(gjson.GetBytes(body, "max_tokens").Int())}
	}
	nowMs := now.UnixMilli()
	cost := float64(u.FreshInput)*pIn + float64(u.CacheRead)*pRead + float64(u.CacheWrite)*pWrite + float64(u.Output)*pOut
	resp := &predictive.Response{
		Request: predictive.Request{Tenant: tenantID, Session: session, Thread: thread, Model: model, TsMs: startedAt.UnixMilli()},
		EndMs:   nowMs, DecMs: nowMs, Provider: provider, CacheTTL: lf.CacheTTL, Effort: lf.Effort, MaxTokens: lf.MaxTokens, SystemBlocks: lf.SystemBlocks,
		TTFBMs: lf.TTFBMs, UpstreamMs: float64(nowMs - startedAt.UnixMilli()), CostUSD: cost,
		Prefix: u.CacheRead + u.CacheWrite, CacheWrite: u.CacheWrite, Fresh: u.FreshInput, Output: u.Output, Messages: lf.Messages, Tools: lf.Tools,
		StopReason: u.StopReason, Agent: lf.Agent, Thinking: lf.Thinking, Stream: lf.Stream, PIn: pIn, POut: pOut, PRead: pRead, PWrite5m: pWrite,
	}
	pc.tr.Ended(a, resp)
	pingCost := resp.PingCost()
	st := startedAt.UTC()
	facts := predictive.Facts{Family: familyOf(model), StopReason: u.StopReason, Model: model, Agent: resp.Agent, PrefixTok: float64(resp.Prefix), PingCost: pingCost,
		HourUTC: float64(st.Hour()), DowUTC: float64(st.Weekday())}
	pc.mu.Lock()
	spec := pc.spec // an auto campaign's refresh swaps it (refreshAutoCampaigns)
	pc.mu.Unlock()
	rule := spec.RuleFor(&facts)
	kk, idle := 0, 280.0
	var row *predictive.Row
	if pc.model != nil { // the tracker must see every response, whichever rule the segment applies
		row = predictive.NewRow()
		pc.tr.Features(a, resp, row)
	} else {
		pc.tr.Release(a, resp) // plan-only campaign: no features, and no history to accumulate
	}
	switch rule.Kind {
	case predictive.RuleModel:
		d := predictive.Decide(pc.model, row, predictive.Econ{PrefixTok: float64(resp.Prefix), PWrite5m: pWrite, PRead: pRead, PingCost: pingCost})
		kk, idle = d.K, pc.model.Rule().XS
	case predictive.RulePlan:
		kk, idle = spec.PlanOf(rule)
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.stats.Decisions++
	pc.stats.KHist[min(kk, 12)]++
	pc.stats.LastDecided = now.UnixMilli()
	if pc.npend >= predPendingMax {
		pc.sweepLocked(now.UnixMilli())
	}
	if optedOut && pc.meta.Mode == campaignModeActive {
		pc.stats.OptedOut++
	}
	if pc.npend < predPendingMax {
		pc.npend++
		tk := session + "\x00" + thread
		pc.pending[tk] = append(pc.pending[tk], predPending{endMs: resp.EndMs, startMs: startedAt.UnixMilli(), k: kk, x: idle, stake: float64(resp.Prefix) * (pWrite - pRead), ping: pingCost})
	}
	return predDecision{campaign: pc, k: kk, idleS: idle, active: pc.meta.Mode == campaignModeActive && !optedOut}, true
}

func familyOf(model string) string {
	m := strings.ToLower(strings.TrimPrefix(model, "aws/"))
	for _, f := range []string{"opus", "sonnet", "haiku"} {
		if strings.Contains(m, f) {
			return f
		}
	}
	return "other"
}

// campaignStrategyID tags the pings a campaign bought on their dash rows.
func campaignStrategyID(id string) string { return "campaign:" + id }

// apply writes an ACTIVE campaign's decision into the entry's policy. Account-level bounds (the per-ping dollar ceiling) are left alone.
func (d predDecision) apply(pol CachePolicy) CachePolicy {
	if !d.active {
		return pol
	}
	if d.k <= 0 {
		pol.KeepAlive = false
		return pol
	}
	// The campaign decides HOW MANY pings, never their spacing: that stays the tenant's configured keep-alive interval (the shipped 270 s when none
	// is configured), so a campaign never pings later than it (live, pings 275-280 s after a response miss the cache 19% of the time) nor more
	// often than the tenant's own policy would.
	idle := pol.Idle
	if idle <= 0 {
		idle = campaignKeepAlive5mIdle * time.Second
	}
	pol.KeepAlive, pol.Idle, pol.MaxPings = true, idle, d.k
	// A campaign is scored on five-minute economics (cfTC, PWrite5m), so its k is a count of
	// five-minute windows. On a thread billed at the one-hour tier the keeper swaps in the one-hour
	// schedule (Policy.ForOneHour); k then only bounds it from above, as an override's does.
	k1h := pol.MaxPings1h
	if k1h <= 0 {
		k1h = modelinfo.AnthropicDefaultKeepAliveMaxPings1h
	}
	pol.MaxPings1h = min(k1h, d.k)
	pol.MinPrefixTokens, pol.PredictorID, pol.PredictorThreshold = 0, "", 0
	return pol
}

// listCampaignIDs returns campaign ids on disk, newest first.
func listCampaignIDs(dir string) []string {
	ents, _ := os.ReadDir(dir)
	type idt struct {
		id string
		t  int64
	}
	var out []idt
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if fi, err := e.Info(); err == nil {
			out = append(out, idt{e.Name(), fi.ModTime().UnixNano()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].t > out[j].t })
	ids := make([]string, len(out))
	for i, o := range out {
		ids[i] = o.id
	}
	return ids
}

// campaignDir is where built campaigns live: CG_CAMPAIGN_DIR, else a "campaigns" directory beside the dashboard database. "" when neither exists.
func (h *Handler) campaignDir() string {
	if d := os.Getenv("CG_CAMPAIGN_DIR"); d != "" {
		return d
	}
	if h.opts.Dashboard != nil && h.opts.Dashboard.DB() != nil && h.opts.Dashboard.DB().Path() != "" && h.opts.Dashboard.DB().Path() != ":memory:" {
		return filepath.Join(filepath.Dir(h.opts.Dashboard.DB().Path()), "campaigns")
	}
	return ""
}

// eraseTenant removes a tenant's campaigns from memory and disk. They are derived from the history a purge or a delete removes, and they hold a model
// fitted to it, so they go with it. Nil-safe.
func (rt *predRuntime) eraseTenant(tenantID string) {
	if rt == nil || rt.dir == "" {
		return
	}
	rt.mu.Lock()
	if pc := rt.byTenant[tenantID]; pc != nil {
		pc.close()
		delete(rt.byTenant, tenantID)
	}
	rt.mu.Unlock()
	for _, id := range listCampaignIDs(rt.dir) {
		var m campaignMeta
		if b, err := os.ReadFile(filepath.Join(rt.dir, id, "meta.json")); err == nil && json.Unmarshal(b, &m) == nil && m.Tenant == tenantID {
			_ = os.RemoveAll(filepath.Join(rt.dir, id))
		}
	}
}

// sessionEnded scores a campaign's open decisions of an ended session as never-returned with the pings after the end cancelled.
// This is what the client's SessionEnd (or SessionStart compact: the old prefix is dead) signal is worth to the policy; it is called by the signal handler.
// Only decisions whose request started before cutoff are resolved (a zero cutoff resolves them all): a late compact event must not score the
// post-compaction request's own decision as never-returned. A non-zero cutoff is the compact path, which (like retireSessionStartedBefore)
// covers the main thread only: a subagent's or teammate's open decision stays open. Nil-safe.
func (rt *predRuntime) sessionEnded(tenantID, session string, now, cutoff time.Time) {
	pc := rt.forTenant(tenantID)
	if pc == nil {
		return
	}
	pre := session + "\x00"
	pc.mu.Lock()
	defer pc.mu.Unlock()
	for key, ps := range pc.pending {
		if !strings.HasPrefix(key, pre) || (!cutoff.IsZero() && key != pre) {
			continue
		}
		keep := ps[:0]
		for _, p := range ps {
			if !cutoff.IsZero() && p.startMs >= cutoff.UnixMilli() {
				keep = append(keep, p)
				continue
			}
			pc.npend--
			pc.resolveStopLocked(p, 0, false, float64(now.UnixMilli()-p.endMs)/1000)
		}
		if len(keep) == 0 {
			delete(pc.pending, key)
		} else {
			pc.pending[key] = keep
		}
	}
}
