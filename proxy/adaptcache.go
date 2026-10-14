package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/kvcache"
	"github.com/rossoctl/context-guru/predictive"
)

// `/context-guru:adapt cache`: replay keep-alive strategies on the caller's own history and enforce the chosen one as an auto campaign.
// Design: docs/proposals/adapt-cache.md; the wire shape is the plugin's /context-guru:adapt contract (kind, version, options, recommendation, basis_id).
// The replay is the closed form the auto builder and the live shadow scorer use (rowFacts.outcome). Every dollar is at the gateway's rates, scale 1;
// engine_usd is the same calibrated to the pinned replay engine, the unit of the builder's gate.
//
//	GET  /api/adapt/cache?since=&until= (or days=N; session_end=0)  the report
//	POST /api/adapt/cache/apply {"option","basis"}                 "default" switches the caller's live campaign off; an adapted option runs the
//	                                                               auto builder, whose held-out ship gate decides active (ship) or shadow
//
// Scope is the caller's own tenant: the dashboard cookie in hosted mode (a manager may name ?tenant=), the whole local history single-tenant.

// adaptSegments are the adapted strategies besides "auto" (one plan for the tenant), each a segmentation the builder can enforce.
var adaptSegments = []string{"dow", "hour", "prefix", "model", "client", "stop_reason"}

var (
	adaptCacheMu sync.Mutex // one report or apply at a time: a report reads the fleet's whole window

	adaptBasisMu sync.Mutex
	adaptBasis   = map[string]adaptBasisRec{} // per tenant: the latest report an apply may refer to
)

type adaptBasisRec struct {
	id           string
	since, until int64
	se           bool
	current      string
	at           time.Time
}

func (h *Handler) mountAdapt(m *http.ServeMux) {
	m.HandleFunc("GET /api/adapt/cache", h.adaptCacheReport)
	m.HandleFunc("POST /api/adapt/cache/apply", h.adaptCacheApply)
}

// adaptCaller is whose history and whose keep-alive this is: the shared /api/adapt/* principal (adaptapply.go: the dashboard cookie or the
// account's own proxy token hosted, direct loopback single-tenant), resolved to the account's tenancy.
func (h *Handler) adaptCaller(r *http.Request) (tn *Tenancy, who adaptWho, err error) {
	who, err = h.adaptPrincipal(r)
	if err != nil {
		return nil, who, err
	}
	if who.Local {
		return h.static.Load(), who, nil
	}
	t, err := h.registry().Get(who.TenantID)
	if err != nil {
		return nil, who, statusError{http.StatusNotFound, "no such account"}
	}
	tn, err = h.opts.Tenants.ForTenant(t)
	return tn, who, err
}

func adaptBasisKey(who adaptWho, tn *Tenancy) string { return adaptActor(who) + "\x00" + tn.ID }

// adaptActor is the audit actor of an adapt change.
func adaptActor(who adaptWho) string {
	if who.actor != nil {
		return who.actor.ID
	}
	return "local"
}

// adaptTally is one strategy's replay over a set of rows.
type adaptTally struct {
	Net, Engine, Rescue, Ping float64
	Pings, Rescues            float64 // expected counts
}

// adaptPlan is a strategy's decision on row i: k pings, x seconds apart, cancelled by SessionEnd or not.
type adaptPlan func(i int) (k int, x float64, se bool)

// adaptScore replays plan on the rows idx: the totals, and each row's net in gateway dollars (per) and in the builder's units (eng).
func adaptScore(facts []rowFacts, idx []int, plan adaptPlan, fp *fidParams) (t adaptTally, per, eng []float64) {
	per, eng = make([]float64, len(idx)), make([]float64, len(idx))
	for j, i := range idx {
		f := &facts[i]
		k, x, se := plan(i)
		rescue, ping, pings, rescues := f.expected(k, x, se, fp)
		t.Pings += pings
		t.Ping += ping
		t.Rescues += rescues
		t.Rescue += rescue
		fired, rescued := f.outcome(k, x, se)
		ge := -cfPingScale * float64(fired) * f.ping
		if rescued {
			ge += cfScale * f.stake
		}
		t.Net += rescue - ping
		t.Engine += ge
		per[j], eng[j] = rescue-ping, ge
	}
	return t, per, eng
}

// currentPlan replays the caller's own policy as configured now: its live campaign's plan where one is active (rules campaigns only), else the
// account's keep-alive with its floor, ceiling, first-request skip and predictor gate. SessionEnd counts only if this deployment honours the signal.
// A manager strategy (time windows) and per-session overrides are not replayed; a model campaign is reported as not replayable.
func (h *Handler) currentPlan(tn *Tenancy, facts []rowFacts) (adaptPlan, string, bool) {
	pol := tn.Cache
	se := clientSignalEnabled()
	x := pol.Idle.Seconds()
	if x <= 0 {
		x = autoX
	}
	if pc := h.predRT().forTenant(tn.ID); pc != nil {
		pc.mu.Lock()
		spec, active := pc.spec, pc.meta.Mode == campaignModeActive
		id := pc.meta.ID
		pc.mu.Unlock()
		if active && pol.KeepAlive {
			if spec.Policy.Model != nil {
				return nil, "active campaign " + id + " runs a model; it cannot be replayed offline", false
			}
			return func(i int) (int, float64, bool) {
				f := &facts[i]
				ff := predictive.Facts{Family: familyOf(f.model), StopReason: f.stop, Model: f.model, Agent: f.agent, PrefixTok: float64(f.prefix), PingCost: f.ping,
					HourUTC: float64(f.hour), DowUTC: float64(f.dow)}
				k, _ := spec.PlanOf(spec.RuleFor(&ff))
				return k, x, se
			}, "active campaign " + id, true
		}
	}
	if !pol.on() {
		return func(int) (int, float64, bool) { return 0, 0, false }, "keep-alive off", true
	}
	gate, _ := predictorFor(pol.PredictorID)
	return func(i int) (int, float64, bool) {
		f := &facts[i]
		if f.turn < 1 || f.prefix < int64(pol.MinPrefixTokens) || f.ping > pol.Ceiling() || (pol.PredictorID != "" && (gate == nil || gate(f.stop) < pol.PredictorThreshold)) {
			return 0, x, se
		}
		return pol.MaxPings, x, se
	}, fmt.Sprintf("K%d every %.0f s, prefix >= %d tokens, <= $%.2f a ping", pol.MaxPings, x, pol.MinPrefixTokens, pol.Ceiling()), true
}

func (h *Handler) adaptCacheReport(w http.ResponseWriter, r *http.Request) {
	tn, who, err := h.adaptCaller(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	if h.opts.Dashboard == nil || h.opts.Dashboard.DB() == nil {
		ctlErr(w, http.StatusServiceUnavailable, "the report needs the dashboard database (--dashboard)")
		return
	}
	q := r.URL.Query()
	until, _ := strconv.ParseInt(q.Get("until"), 10, 64)
	since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
	if until <= 0 {
		until = time.Now().UnixMilli()
	}
	if since <= 0 {
		days, _ := strconv.Atoi(q.Get("days"))
		if days <= 0 {
			days = 30
		}
		since = until - int64(days)*86400e3
	}
	if since >= until || until-since > 365*86400e3 {
		ctlErr(w, http.StatusBadRequest, "the window must be since < until and at most 365 days")
		return
	}
	// The process-wide slot first, so a busy answer never spends the account's hourly budget (the rule /api/adapt/compaction follows).
	if !adaptCacheMu.TryLock() {
		w.Header().Set("Retry-After", "30")
		ctlErr(w, http.StatusServiceUnavailable, "another adapt analysis is running; try again in a minute")
		return
	}
	defer adaptCacheMu.Unlock()
	// A report reads the fleet's window, and an account's proxy token can ask for one: at most adaptReportBurst an hour per account.
	if err := h.limiter.AllowAction("adapt-cache-report|"+tn.ID, "cache reports", adaptReportBurst); err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	rep, code, msg := h.adaptReport(tn, adaptBasisKey(who, tn), since, until, q.Get("session_end") != "0")
	if code != http.StatusOK {
		ctlErr(w, code, msg)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

var adaptLabels = map[string][2]string{
	"never":   {"No pings", "never keep the cache warm"},
	"default": {"Default keep-alive", "2 pings 270 s apart on prefixes >= 80K tokens, <= $0.50 a ping, none after a thread's first request, stopped by SessionEnd"},
	"K1":      {"1 ping, every response", "one ping 270 s after every response"},
	"K2":      {"2 pings, every response", "two pings 270 s apart after every response"},
	"K3":      {"3 pings, every response", "three pings 270 s apart after every response"},
	"auto":    {"Adapted: one plan for you", "the ping count learned from your history, shrunk toward everyone's"},
}

// adaptReport is the whole report; adaptCacheReport only does the I/O around it.
// basisKey names whose report this is (actor and account), so a manager reading an account does not replace that account's own basis.
func (h *Handler) adaptReport(tn *Tenancy, basisKey string, since, until int64, se bool) (map[string]any, int, string) {
	db := h.opts.Dashboard.DB()
	// the same reads the auto builder makes: the fleet's rows for the prior (autoFleet), the account's own rows in full (autoOwn)
	data, err := h.autoFleet(since, until)
	var reqs []*kvcache.Request
	ownCut := false
	if err == nil {
		reqs, ownCut, err = h.autoOwn(data, tn.ID, until)
	}
	if err != nil {
		return nil, http.StatusInternalServerError, "could not read the history"
	}
	fleetCut := data.fleetMatched > data.fleetRead
	if ownCut {
		// a report on part of the account's window would read as the whole of it; the campaign build refuses the same window
		return nil, http.StatusUnprocessableEntity, "your history in that window is larger than one analysis read; choose a shorter window (days=N)"
	}
	tenant := tn.ID
	// population: the caller's requests and why each one that is not a decision point is not
	price := h.autoPrice()
	nReq, nUnknown := 0, 0
	bill := 0.0
	drop := map[string]int{}
	sessions := map[string]bool{}
	for _, q := range reqs {
		if q.User != tenant || q.KeepAlive {
			continue
		}
		nReq++
		sessions[q.ConversationID] = true
		if q.CostKnown {
			bill += q.CostUSD
		} else {
			nUnknown++
		}
		_, _, pr, pw, ok := price(q.Model)
		switch {
		case q.Rejected:
			drop["rejected"]++
		case !q.CostKnown:
			drop["incomplete_accounting"]++
		case q.CachedContext <= 0:
			drop["no_cached_prefix"]++
		case !ok || pw <= pr:
			drop["unpriced"]++
		case float64(q.TS+int64(q.UpstreamMs))+2700e3 > float64(until):
			drop["unresolved_at_window_end"]++
		}
	}
	if nReq == 0 {
		return nil, http.StatusNotFound, "no traffic for this account in that window"
	}
	set := data.rowsWith(reqs, tenant, price, se, until)
	rows, facts := set.rows, set.facts
	fp := fitFidelity(reqs, tenant)
	led, err := db.KeepAliveLedger(dash.Filter{Tenant: tenant, Since: since, Until: until})
	if err != nil {
		return nil, http.StatusInternalServerError, "could not read the keep-alive ledger"
	}
	var all []int
	for i, r := range rows {
		if r.Scope == tenant {
			all = append(all, i)
		}
	}
	evals := map[string]*autoEvalResult{"": autoEval(rows, facts, tenant, until, "")}
	for _, s := range adaptSegments {
		evals[s] = autoEval(rows, facts, tenant, until, s)
	}
	test := evals[""].test // the held-out rows depend only on the tenant's own rows: the same for every segmentation
	ss := make([]string, len(test))
	for j, i := range test {
		ss[j] = rows[i].Session
	}
	fixed := func(k int, s bool) adaptPlan { return func(int) (int, float64, bool) { return k, autoX, s } }
	defPlan := func(i int) (int, float64, bool) { return facts[i].defaultK(), autoX, true }
	_, defHeld, defEng := adaptScore(facts, test, defPlan, &fp)

	type strat struct {
		id, label, detail string
		in, held          adaptPlan // held nil = same as in
		adapted           bool
		unavailable       string
		fleet             func() int // an adapted option's fleet plan (tuned before the cut)
		plan              string     // an adapted option's plan per segment, for display (in the account's timezone)
	}
	// Everything shown is in the account's strategy timezone, so a day x hour cell and an option's hours are the same span as a keep-alive window
	// the user would write. Only display: the by_dow / by_hour rules run live on UTC days and hours.
	tz := h.keeper.strategyTZFor(tenant)
	loc, err := time.LoadLocation(tz)
	if err != nil {
		tz, loc = "UTC", time.UTC
	}
	ref := time.UnixMilli(until)
	cur, curLabel, curOK := h.currentPlan(tn, facts)
	// default before current: on a tie (a current setting that is the default) the default is named best
	strats := []strat{{id: "never", in: fixed(0, se)}, {id: "default", in: defPlan}, {id: "current", label: "Your current setting", detail: curLabel, in: cur},
		{id: "K1", in: fixed(1, se)}, {id: "K2", in: fixed(2, se)}, {id: "K3", in: fixed(3, se)}}
	if !curOK {
		strats[2].unavailable = curLabel
	}
	for _, s := range append([]string{""}, adaptSegments...) {
		e := evals[s]
		st := strat{id: "auto", adapted: true}
		if s != "" {
			what := map[string]string{"dow": "day of the week", "hour": "hour of the day", "prefix": "prompt size band", "model": "model", "client": "client",
				"stop_reason": "stop reason"}[s]
			st.id, st.label, st.detail = "by_"+s, "Adapted: a plan per "+what, "a ping count per "+what+", each learned from your history and shrunk toward everyone's"
			if s == "dow" || s == "hour" {
				st.detail += "; shown in " + tz + " (the rule runs on UTC " + map[string]string{"dow": "days", "hour": "hours"}[s] + ", so across a daylight-saving change the shown span moves by an hour)"
			}
		}
		if e.insufficient != "" {
			st.unavailable = "too little history: " + e.insufficient
		} else {
			st.in = func(i int) (int, float64, bool) { return e.inPlan(i), autoX, se }
			st.held = func(i int) (int, float64, bool) { return e.heldPlan(i), autoX, se }
			st.fleet = func() int { return e.fit.FleetPlan }
			if s != "" {
				st.plan = segPlanDisplay(s, facts, tenant, rows, func(key string) int { return e.final.PlanFor(tenant + "\x1f" + key) }, loc, ref)
			}
		}
		strats = append(strats, st)
	}

	// The never-ping bill: what was billed, minus the pings actually sent, plus the rewrites they avoided (the ledger's ceiling).
	neverBill := bill + led.PingUSD - led.NetUSD
	var options []any
	labelOf := map[string]string{}
	heldPer := map[string][]float64{}
	bestID, bestNet := "", math.Inf(-1)
	var bestCI [2]float64
	bestDiff := 0.0
	gateID, failID, failWhy := "", "", ""
	gateNet, gateDiff, failNet := 0.0, 0.0, 0.0
	var gateCI [2]float64
	curPings, curPingUSD := 0.0, 0.0
	inTally := map[string]adaptTally{}
	// the one line the report owes when gateway dollars and the builder's calibrated gate disagree: for the adapted option that leads in gateway dollars
	var disagree []string
	disID, disNet := "", 0.0
	for _, s := range strats {
		label, detail := s.label, s.detail
		if l, ok := adaptLabels[s.id]; ok {
			label, detail = l[0], l[1]
		}
		row := map[string]any{"id": s.id, "label": label, "detail": detail, "enforceable": s.id == "default" || (s.adapted && s.unavailable == "")}
		if s.plan != "" {
			row["plan"] = s.plan
		}
		labelOf[s.id] = label
		if s.unavailable != "" {
			row["unavailable"] = s.unavailable
			options = append(options, row)
			continue
		}
		in, per, _ := adaptScore(facts, all, s.in, &fp)
		if s.id == "current" {
			curPings, curPingUSD = in.Pings, in.Ping
		}
		inTally[s.id] = in
		ih, ihu := helpedHurt(rows, all, per)
		row["in_sample"] = map[string]any{"saved_usd": r2(in.Rescue), "spend_usd": r2(in.Ping), "net_usd": r2(in.Net), "engine_usd": r2(in.Engine),
			"billed_usd": r2(neverBill - in.Net), "pings": r2(in.Pings), "rescues": r2(in.Rescues), "sessions_helped": ih, "sessions_hurt": ihu}
		held := s.held
		if held == nil {
			held = s.in
		}
		ht, hp, he := adaptScore(facts, test, held, &fp)
		// The table's numbers are held-out, the same rows for every option, so the best row and its numbers are one basis.
		hh, hhu := helpedHurt(rows, test, hp)
		row["saved_usd"], row["spend_usd"], row["net_usd"], row["engine_usd"] = r2(ht.Rescue), r2(ht.Ping), r2(ht.Net), r2(ht.Engine)
		row["pings"], row["rescues"], row["sessions_helped"], row["sessions_hurt"] = r2(ht.Pings), r2(ht.Rescues), hh, hhu
		d := make([]float64, len(hp))
		for j := range hp {
			d[j] = hp[j] - defHeld[j]
		}
		ci := sessionCI(d, ss, 1)
		ho := map[string]any{"ci95": sessionCI(hp, ss, 0), "vs_default_usd": r2(sumOf(d)), "vs_default_ci95": ci}
		row["held_out"] = ho
		options = append(options, row)
		heldPer[s.id] = hp
		if s.adapted {
			// The campaign builder's own gate, on the same rows in the same units: positive, and a paired session interval above zero against the
			// shipped default, blanket K3 and the fleet plan. An option that fails it would run in shadow if applied.
			_, _, k3 := adaptScore(facts, test, fixed(3, se), &fp)
			_, _, fl := adaptScore(facts, test, fixed(s.fleet(), se), &fp)
			pass, why := shipGate(he, gateLegs(k3, fl, defEng, s.fleet()), ss)
			ho["builder_gate"] = map[string]any{"pass": pass, "failed": why, "unit": "engine_usd"}
			if realBeats := ci[0] > 0; realBeats != pass && (disID == "" || ht.Net > disNet) {
				disID, disNet, disagree = s.id, ht.Net, nil
				if realBeats {
					disagree = append(disagree, fmt.Sprintf("%s leads the default in gateway dollars (%+.2f [%.2f, %.2f]) but fails the builder's calibrated gate; the gate decides, so it would run in shadow unless applied anyway.", s.id, sumOf(d), ci[0], ci[1]))
				} else {
					disagree = append(disagree, fmt.Sprintf("%s passes the builder's calibrated gate, but in gateway dollars its lead over the default (%+.2f [%.2f, %.2f]) is not clear of zero.", s.id, sumOf(d), ci[0], ci[1]))
				}
			}
			if pass && (gateID == "" || ht.Net > gateNet) {
				gateID, gateNet, gateDiff, gateCI = s.id, ht.Net, sumOf(d), ci
			}
			if !pass && (failID == "" || ht.Net > failNet) {
				failID, failNet, failWhy = s.id, ht.Net, strings.Join(why, "; ")
			}
		}
		if s.id != "never" && ht.Net > bestNet {
			bestID, bestNet, bestCI, bestDiff = s.id, ht.Net, ci, sumOf(d)
		}
	}
	beats := bestID != "default" && bestCI[0] > 0
	var summary string
	var recommendation any
	switch {
	case bestID == "":
		summary = "There is too little history to compare strategies; keep the default."
	case gateID != "":
		summary = fmt.Sprintf("%s beats the default by $%.2f [%.2f, %.2f] on your last 30%% of decisions (plan tuned only on what came before) and passes the campaign builder's gate (it also beats blanket K3 and the fleet plan).",
			gateID, gateDiff, gateCI[0], gateCI[1])
		recommendation = map[string]any{"option": gateID, "change": "keep-alive: default -> " + labelOf[gateID] + " (" + gateID + ")", "net_delta_usd": r2(gateDiff), "effective": "now",
			"reason": "it beat the default, blanket K3 and the fleet plan on held-out data"}
	case beats && failID == bestID:
		summary = fmt.Sprintf("%s is $%.2f [%.2f, %.2f] ahead of the default on your last 30%% of decisions, but it does not pass the campaign builder's gate (in the builder's calibrated dollars: %s), so applying it would only run it in shadow. Keep the default.",
			bestID, bestDiff, bestCI[0], bestCI[1], failWhy)
	case beats:
		summary = fmt.Sprintf("%s is $%.2f [%.2f, %.2f] ahead of the default on your last 30%% of decisions, but it is a fixed plan, shown for comparison, and no adapted plan passes the campaign builder's gate. Keep the default.",
			bestID, bestDiff, bestCI[0], bestCI[1])
	case bestID == "default":
		summary = "The default keep-alive is the best strategy on your held-out data; keep it."
	default:
		summary = fmt.Sprintf("No strategy beats the default on held-out data: the best, %s, is $%.2f ahead on your last 30%% of decisions, but the 95%% interval [%.2f, %.2f] includes zero. Keep the default.",
			bestID, bestDiff, bestCI[0], bestCI[1])
	}
	// The decision is relative: best vs the default vs your current setting, held-out, same replay. Absolute dollars are secondary.
	pair := func(a, b string) map[string]any {
		x, y := heldPer[a], heldPer[b]
		if x == nil || y == nil {
			return nil
		}
		d := make([]float64, len(x))
		for j := range d {
			d[j] = x[j] - y[j]
		}
		return map[string]any{"usd": r2(sumOf(d)), "ci95": sessionCI(d, ss, 1)}
	}
	relative := map[string]any{"best": bestID, "best_vs_default": pair(bestID, "default"), "current_vs_default": pair("current", "default"),
		"best_vs_current": pair(bestID, "current"), "basis": "held-out, paired by session, gateway dollars of the same replay for every option"}
	if c := pair("current", "default"); c != nil && c["usd"].(float64) != 0 {
		ci := c["ci95"].([2]float64)
		summary += fmt.Sprintf(" Your current setting is $%.2f [%.2f, %.2f] against the default on the same decisions.", c["usd"], ci[0], ci[1])
	}
	// Per held-out week, for the plugin's "a week" line: the held-out slice spans from the cut to the window end.
	weeks := float64(until-evals[""].cut) / (7 * 86400e3)
	perWeek := func(usd float64) any {
		if weeks <= 0 {
			return nil
		}
		return r2(usd / weeks)
	}
	if rec, ok := recommendation.(map[string]any); ok {
		rec["ci95"], rec["per_week_usd"] = gateCI, perWeek(gateDiff)
	}
	// The gain of the change on offer (the recommendation, else the held-out leader), against the default and against the current setting:
	// held-out, paired by session, the same replay for both, so it is always on the same basis as the table.
	gainID := bestID
	if rec, ok := recommendation.(map[string]any); ok {
		gainID = rec["option"].(string)
	}
	var gain any
	if vd := pair(gainID, "default"); vd != nil {
		gain = map[string]any{"option": gainID, "recommended": recommendation != nil, "vs_default": vd, "vs_current": pair(gainID, "current"),
			"per_week_vs_default_usd": perWeek(vd["usd"].(float64)), "class": "counterfactual", "held_out": true, "upper_bound": true}
	}
	heatmaps := map[string]any{}
	for id, hp := range heldPer {
		if id != "default" {
			heatmaps[id] = adaptHeatmap(facts, test, hp, defHeld, loc)
		}
	}
	var heatmap any
	if hm, ok := heatmaps[gainID]; ok {
		heatmap = map[string]any{"metric": "net_usd", "option": gainID, "tz": tz, "rows": "Mon..Sun", "cols": "00..23",
			"label": "what " + gainID + " adds over the default, by day of week and hour of the request in " + tz + " (held-out, upper bound)", "values": hm}
	}
	current := "current"
	if !curOK {
		current = ""
	}
	// The absolute dollars stay labelled an upper bound. A self-check that drops the label is not possible in-process: the builder's calibrated
	// closed form (engine_usd) can agree with these dollars where both run above the full replay engine,
	// so agreeing with it proves nothing; the label can go once the replay follows thread lineage AND is reconciled against that engine.
	threads := false
	for _, q := range reqs {
		if q.User == tenant && q.ThreadID != "" {
			threads = true
			break
		}
	}
	warnings := []string{"the dollar amounts are an upper bound (they run above the full replay engine); compare options with each other, which is what the recommendation does."}
	if fleetCut {
		warnings = append(warnings, "the shared prior (everyone's history) holds only the newest rows of this window; your own rows are all read.")
	}
	if n := drop["unresolved_at_window_end"]; n > 0 {
		warnings = append(warnings, fmt.Sprintf("%d responses ended too close to the window end to know whether the session came back; they are not scored.", n))
	}
	if nUnknown > 0 {
		warnings = append(warnings, fmt.Sprintf("%d requests have incomplete token accounting: their cost is unknown, not zero, and they are not scored.", nUnknown))
	}
	warnings = append(warnings, disagree...)
	warnings = append(warnings, fmt.Sprintf("the table is your last 30%% of decisions (%d decisions, %d sessions), plans tuned only on what came before; whole-window numbers are under in_sample (optimistic for adapted plans).", len(test), nSessOf(rows, test)))
	warnings = append(warnings, fmt.Sprintf("the best of %d options is picked on the same held-out rows it is scored on: with this many options one clears a 95%% interval by chance more often than 1 in 20.", len(heldPer)-1))
	if se {
		warnings = append(warnings, "SessionEnd is scored as always delivered, at once: an upper bound on what the hook saves (a lost or late signal sends the pings anyway).")
	}
	if !se {
		warnings = append(warnings, "scored without the SessionEnd hook: every session's last response is pinged as if the user might return.")
	}
	rep := map[string]any{
		"kind": "cache", "version": 1, "summary": summary, "class": "counterfactual",
		"method":      "replay of your past sessions at the gateway's prices (dollars: an upper bound)",
		"upper_bound": true,
		"window":      map[string]any{"since": since, "until": until, "sessions": len(sessions), "requests": nReq},
		"current":     current, "default": "default", "best": bestID, "options": options, "recommendation": recommendation, "items": []any{}, "warnings": warnings,
		"held_out": map[string]any{"decision_points": len(test), "best_vs_default_usd": r2(bestDiff), "best_vs_default_ci95": bestCI, "beats_default": beats,
			"note": "plans tuned on everything before your last 30% of decisions and scored on that 30%; paired 95% intervals by bootstrap over sessions"},
		"population": map[string]any{"decision_points": len(all), "requests_cost_unknown": nUnknown, "dropped": drop},
		"observed": map[string]any{"class": "observed", "bill_usd": r2(bill + led.PingUSD), "pings": led.Pings, "ping_usd": r2(led.PingUSD), "misses_avoided": led.MissesAvoided,
			"saved_usd_ceiling": r2(led.SavedUSD), "net_usd": r2(led.NetUSD),
			"note": "what the keep-alive actually did in this window (the dashboard's ledger); saved is a ceiling: another session with the same prefix could have kept the entry warm"},
		"relative":   relative,
		"gain":       gain,
		"display_tz": tz,
		"heatmap":    heatmap, "option_heatmaps": heatmaps, "heatmap_tz": tz,
		"to_date": map[string]any{"class": "observed", "since": since, "until": until, "saved_usd": r2(led.NetUSD), "saved_ceiling_usd": r2(led.SavedUSD),
			"ping_usd": r2(led.PingUSD), "pings": led.Pings,
			"label": "what your current keep-alive saved so far: the rewrites its pings avoided (a ceiling) minus what the pings cost, as billed"},
		"disagreement": strings.Join(disagree, ""), // one line, or "" when gateway dollars and the gate agree
		"calibration": map[string]any{
			"observed_ping_usd_mean": meanOr(led.PingUSD, float64(led.Pings)), "replay_ping_usd_mean": meanOr(curPingUSD, curPings),
			"benefit_scale": cfScale, "ping_scale": cfPingScale,
			"gaps_classified": len(all), "gaps_unknown": drop["unresolved_at_window_end"], "gaps_classified_share": math.Round(float64(len(all))/math.Max(1, float64(len(all)+drop["unresolved_at_window_end"]))*1000) / 1000,
			"decision_points_share_of_requests": math.Round(float64(len(all))/float64(nReq)*1000) / 1000,
			"note":                              "money is the closed-form replay in expectation over ping rewrites (a rewrite stops the span), the miss of a rescued request, a cliff with a spread, the head a missed request reads anyway and hits after long gaps from other sessions (h, fitted on your own history). It does not follow thread lineage inside a conversation, so it is labelled an upper bound. engine_usd is the builder's calibrated closed form, the unit of its gate. Observed and replayed mean ping cost cover different pings (the observed ones were sent under whatever policy ran then). Responses whose outcome is unknown at the window end are not scored, so they are never counted as savings.",
			"h_beyond_cliff":                    r2(fp.h), "h_fit_requests": fp.n, "threads_recorded": threads,
			"default_net_over_engine": ratioOr(inTally["default"]), "k3_net_over_engine": ratioOr(inTally["K3"]),
		},
		"assumptions": map[string]any{"session_end": se, "cliff_s": cfTC, "ping_spacing_s": autoX, "benefit_scale": cfScale, "ping_scale": cfPingScale,
			"note": "net = rewrites avoided (prefix x (cache write - cache read)) minus pings (prefix x cache read + input + output), vs never pinging; billed = the never-ping bill (billed - pings sent + their observed saving) minus net; engine_usd = the builder's calibrated closed form, the unit of its gate; a session's last response is a no-return; time segments are UTC"},
	}
	b, _ := json.Marshal(rep)
	sum := sha256.Sum256(append(b, tenant...))
	rep["basis_id"] = hex.EncodeToString(sum[:6])
	adaptBasisMu.Lock()
	adaptBasis[basisKey] = adaptBasisRec{id: rep["basis_id"].(string), since: since, until: until, se: se, at: time.Now()}
	adaptBasisMu.Unlock()
	return rep, http.StatusOK, ""
}

func r2(x float64) float64 { return math.Round(x*100) / 100 }

// helpedHurt counts the sessions a strategy's rows net positive and negative on.
func helpedHurt(rows []predictive.AutoRow, idx []int, per []float64) (helped, hurt int) {
	by := map[string]float64{}
	for j, i := range idx {
		by[rows[i].Session] += per[j]
	}
	for _, v := range by {
		if v > 1e-9 {
			helped++
		} else if v < -1e-9 {
			hurt++
		}
	}
	return helped, hurt
}

func ratioOr(t adaptTally) any {
	if t.Engine <= 0 {
		return nil
	}
	return math.Round(t.Net/t.Engine*1000) / 1000
}

func meanOr(sum, n float64) any {
	if n <= 0 {
		return nil
	}
	return math.Round(sum/n*1e5) / 1e5
}

func nSessOf(rows []predictive.AutoRow, idx []int) int {
	m := map[string]bool{}
	for _, i := range idx {
		m[rows[i].Session] = true
	}
	return len(m)
}

type adaptApplyIn struct {
	Option string `json:"option"`
	Basis  string `json:"basis"`
	// Force ("apply anyway") switches an adapted plan on even when the builder's held-out gate rejects it. Never implied.
	Force bool `json:"force"`
}

// adaptApplyWait bounds how long an apply waits for its build (in-process: seconds on a month of history).
var adaptApplyWait = 110 * time.Second

// adaptMidBuild, a test seam, runs between an adapt build's end and its mode change.
var adaptMidBuild func()

// adaptReportBurst is how many cache reports one account may ask for per hour (as /api/adapt/compaction allows).
const adaptReportBurst = 12

// adaptBasisTTL is how long a report may be applied after it was computed.
const adaptBasisTTL = time.Hour

func (h *Handler) adaptCacheApply(w http.ResponseWriter, r *http.Request) {
	tn, who, err := h.adaptCaller(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	actor := adaptActor(who)
	var in adaptApplyIn
	if err := readJSON(w, r, &in); err != nil {
		readErr(w, err)
		return
	}
	segBy := strings.TrimPrefix(in.Option, "by_")
	if _, ok := segFeatures[segBy]; in.Option != "default" && in.Option != "revert" && in.Option != "auto" && (!ok || segBy == in.Option) {
		ctlErr(w, http.StatusBadRequest, fmt.Sprintf("%q is shown for comparison only: apply default, auto or a by_* option", in.Option))
		return
	}
	adaptBasisMu.Lock()
	b, ok := adaptBasis[adaptBasisKey(who, tn)]
	adaptBasisMu.Unlock()
	// An adapted option needs the basis of the caller's own latest report, as every adapt kind does; "default" (and "revert") needs none.
	if in.Option != "default" && in.Option != "revert" && (!ok || in.Basis == "" || in.Basis != b.id || time.Since(b.at) > adaptBasisTTL) {
		ctlErr(w, http.StatusConflict, "the numbers changed since you saw them (or the proxy restarted); re-run the report")
		return
	}
	previous := "default"
	live := h.predRT().forTenant(tn.ID)
	if live != nil {
		previous = "campaign " + live.meta.ID
	}
	if live != nil && h.managedCampaign(live, who) {
		ctlErr(w, http.StatusConflict, "campaign managed by your account admin: ask them to change it")
		return
	}
	if !h.adaptAllow("cache:" + tn.ID) {
		code, msg := statusOf(errAdaptRate)
		ctlErr(w, code, msg)
		return
	}
	if h.predRT() == nil || h.campaignDir() == "" {
		ctlErr(w, http.StatusServiceUnavailable, "campaigns need the keep-alive and the dashboard database on this deployment")
		return
	}
	if !adaptCacheMu.TryLock() {
		ctlErr(w, http.StatusTooManyRequests, "another adapt analysis is running; try again in a minute")
		return
	}
	defer adaptCacheMu.Unlock()
	if in.Option == "revert" {
		// undo the caller's own adapt apply, and only that: the live campaign must be one an adapt apply by this same actor started and last set.
		// Not the shared adaptRevert: that restores the last config_yaml change, and a cache apply writes a campaign, never the config document.
		var via, by string
		if live != nil {
			live.mu.Lock()
			via, by = live.meta.Via, live.meta.ModeBy
			live.mu.Unlock()
		}
		if live == nil || via != "adapt" || by != actor {
			ctlErr(w, http.StatusConflict, "the live keep-alive is not an adapt apply of yours; nothing to undo")
			return
		}
		if _, code, msg := h.setCampaignMode(actor, live.meta.ID, campaignModeOff, false); code != http.StatusOK {
			ctlErr(w, code, msg)
			return
		}
		if who.actor != nil {
			h.adaptAudit(who, "cache revert", previous, "default")
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": "revert", "previous": previous, "effective": "now",
			"note": "Campaign " + live.meta.ID + " is off; your own keep-alive setting is in force."})
		return
	}
	if in.Option == "default" {
		note := "The default was already in force: you had no live campaign."
		if live != nil {
			if _, code, msg := h.setCampaignMode(actor, live.meta.ID, campaignModeOff, false); code != http.StatusOK {
				ctlErr(w, code, msg)
				return
			}
			note = "Campaign " + live.meta.ID + " is off; your own keep-alive setting is in force."
		}
		if who.actor != nil {
			h.adaptAudit(who, "cache default", previous, "default")
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": "default", "previous": previous, "effective": "now", "note": note})
		return
	}
	if in.Option == "auto" {
		segBy = ""
	}
	done := make(chan struct{})
	var managedNow atomic.Bool
	after := h.adaptAfterBuild(actor, in.Force, func() bool {
		pc := h.predRT().forTenant(tn.ID)
		managedNow.Store(pc != nil && h.managedCampaign(pc, who))
		return managedNow.Load()
	})
	bi := cacheBuildIn{Scope: "tenant", ID: tn.ID, Objective: "net_usd", Builder: "auto", SessionEnd: b.se, SegmentBy: segBy, Via: "adapt"}
	out, code, msg := h.startCacheBuild(r.Context(), actor, bi, b.since, b.until, func(id string) {
		if adaptMidBuild != nil {
			adaptMidBuild()
		}
		after(id)
		close(done)
	})
	if code != http.StatusAccepted {
		ctlErr(w, code, msg)
		return
	}
	id, _ := out["campaign_id"].(string)
	select {
	case <-done:
	case <-time.After(adaptApplyWait):
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": false, "applied": "building", "previous": previous, "effective": "now", "campaign_id": id,
			"note": "The campaign build is still running; nothing has changed yet. If its held-out gate passes it goes live by itself."})
		return
	}
	if managedNow.Load() {
		ctlErr(w, http.StatusConflict, "campaign managed by your account admin: a manager set one while this was building, so nothing changed")
		return
	}
	v, _ := h.campaignView(id, false)
	rec, _ := v["recommendation"].(string)
	mode, _ := v["mode"].(string)
	res := map[string]any{"ok": mode == campaignModeActive, "applied": in.Option, "previous": previous, "effective": "now", "campaign_id": id, "recommendation": rec,
		"undo": map[string]any{"option": "revert"}, "summary": v["summary"]}
	switch {
	case v["state"] == "failed":
		res["applied"], res["note"] = "none", fmt.Sprintf("The build failed (%v). Nothing changed.", v["error"])
	case rec == "insufficient_data":
		res["applied"], res["note"] = "none", "Too little history to build a campaign. Nothing changed."
	case mode == campaignModeActive && rec != "ship":
		var spec map[string]any
		_ = readJSONFile(filepath.Join(h.campaignDir(), id, "spec.json"), &spec)
		res["note"] = fmt.Sprintf("Applied anyway, as asked: the builder's held-out gate did NOT pass, but campaign %s is live with the plan you chose (%v). "+
			"It is not rebuilt daily: the daily rebuild changes a live campaign only on a passing gate.", id, h.planForDisplay(spec, tn.ID))
	case in.Force && rec == "baseline":
		res["applied"], res["note"] = "none", "The chosen plan could not be installed, so nothing changed."
	case mode == campaignModeActive:
		var spec map[string]any
		_ = readJSONFile(filepath.Join(h.campaignDir(), id, "spec.json"), &spec)
		res["note"] = "The builder's held-out gate passed: campaign " + id + " is live and decides the ping count for your sessions: " + h.planForDisplay(spec, tn.ID) + "."
	default:
		res["applied"], res["note"] = "shadow", "The builder's held-out gate did not pass (the plan did not beat the default, blanket K3 and the fleet plan on held-out data), so it was not switched on. Campaign "+
			id+" runs in shadow: it decides on live traffic and changes nothing; your own keep-alive setting stays in force."
		res["force_available"] = true
	}
	if who.actor != nil {
		what := fmt.Sprintf("cache %s (campaign %s, %s)", in.Option, id, mode)
		if mode == campaignModeActive && rec != "ship" {
			what = fmt.Sprintf("cache %s (campaign %s, active: forced, gate failed)", in.Option, id)
		}
		h.adaptAudit(who, what, previous, fmt.Sprint(res["applied"]))
	}
	writeJSON(w, http.StatusOK, res)
}

// adaptAfterBuild is what an adapt apply does when its build ends: the builder's own held-out gate decides. Ship goes live; anything else
// evaluated runs in shadow (it decides and scores on live traffic and changes nothing); too little history leaves it off.
// blocked, if set, is asked again under the mode-change lock, just before the write: a build takes up to adaptApplyWait, and a manager may set a
// campaign meanwhile.
func (h *Handler) adaptAfterBuild(actor string, force bool, blocked func() bool) func(id string) {
	return func(id string) {
		mode := campaignModeShadow
		if force {
			mode = campaignModeActive
		}
		switch readStatus(filepath.Join(h.campaignDir(), id))["recommendation"] {
		case "ship":
			mode = campaignModeActive
		case "baseline":
			if force && !h.forcePolicy(id) {
				return // the chosen plan could not be installed: leave the campaign off rather than run the fleet plan as if it were the choice
			}
		default:
			return
		}
		refuse := func() (bool, string) { return blocked != nil && blocked(), "campaign managed by your account admin" }
		if _, code, msg := h.setCampaignModeIf(actor, id, mode, force, refuse); code != http.StatusOK {
			slog.Warn("context-guru: an adapt apply could not set its campaign's mode", "campaign", id, "mode", mode, "err", msg)
		}
	}
}

// forcePolicy makes an "apply anyway" run the plan the user chose: a non-ship build's spec holds the fleet plan as its policy and the tenant's
// own tuned plan under own_policy; this swaps them on disk before the campaign is loaded.
func (h *Handler) forcePolicy(id string) bool {
	dir := filepath.Join(h.campaignDir(), id)
	var spec map[string]any
	if readJSONFile(filepath.Join(dir, "spec.json"), &spec) != nil || spec["own_policy"] == nil {
		return false
	}
	spec["policy"], spec["forced"] = spec["own_policy"], true
	if ev, ok := spec["evidence"].(map[string]any); ok {
		if sh, ok := ev["shipped"].(map[string]any); ok {
			sh["plan"], sh["note"] = spec["own_plan"], "applied anyway: the tenant's own plan, although the held-out gate did not pass"
		}
	}
	return writeSpecValidated(dir, spec) == nil
}

// managedCampaign reports whether a non-manager caller must leave the live campaign alone: its mode was last set by a manager who is not the
// account itself. A manager may change any campaign; a single-tenant proxy has no managers.
func (h *Handler) managedCampaign(pc *predCampaign, who adaptWho) bool {
	reg := h.registry()
	if reg == nil || (who.actor != nil && who.actor.IsManager()) {
		return false
	}
	pc.mu.Lock()
	by := pc.meta.ModeBy
	if by == "" { // a campaign written before mode_by existed: its creator set it
		by = pc.meta.Actor
	}
	pc.mu.Unlock()
	if by == "" {
		return true // nobody recorded: fail closed
	}
	if by == who.TenantID {
		return false
	}
	// A writer whose account no longer exists reads as unmanaged: a deleted manager no longer holds the account's campaign.
	t, err := reg.Get(by)
	return err == nil && t.IsManager()
}

// adaptHeatmap is an option's held-out net minus the default's, summed by the day of week (Monday first) and hour of the request start in loc.
func adaptHeatmap(facts []rowFacts, idx []int, per, def []float64, loc *time.Location) [7][24]float64 {
	var m [7][24]float64
	for j, i := range idx {
		t := time.UnixMilli(facts[i].startMs).In(loc)
		m[(int(t.Weekday())+6)%7][t.Hour()] += per[j] - def[j]
	}
	for d := range m {
		for h := range m[d] {
			m[d][h] = r2(m[d][h])
		}
	}
	return m
}

// segLabel is how one segment of a by_dow / by_hour plan reads in loc: the UTC hour or day it runs on, shown as the local span it covers
// around ref. Other features read as they are.
func segLabel(segBy, key string, loc *time.Location, ref time.Time) string {
	ref = ref.UTC()
	switch segBy {
	case "hour":
		var hr int
		if _, err := fmt.Sscanf(key, "%02d", &hr); err != nil {
			return key
		}
		t := time.Date(ref.Year(), ref.Month(), ref.Day(), hr, 0, 0, 0, time.UTC).In(loc)
		return t.Format("15:04") + "-" + t.Add(time.Hour).Format("15:04")
	case "dow":
		for d := 0; d < 7; d++ {
			day := time.Date(ref.Year(), ref.Month(), ref.Day()+d, 0, 0, 0, 0, time.UTC)
			if day.Weekday().String() == key {
				a, b := day.In(loc), day.Add(24*time.Hour).In(loc)
				return a.Format("Mon 15:04") + " - " + b.Format("Mon 15:04")
			}
		}
	}
	return key
}

// segPlanDisplay is "segment: plan" for every segment value the account's rows have, in the account's timezone, in UTC order.
func segPlanDisplay(segBy string, facts []rowFacts, tenant string, rows []predictive.AutoRow, planOf func(key string) int, loc *time.Location, ref time.Time) string {
	seen := map[string]bool{}
	var keys []string
	for i, r := range rows {
		if r.Scope != tenant {
			continue
		}
		k, _ := segFeatures[segBy](&facts[i])
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = segLabel(segBy, k, loc, ref) + ": " + planName(planOf(k))
	}
	return strings.Join(parts, "; ")
}

// policyDisplay is a campaign policy's segments as the account reads them (segLabel), for an apply's note.
func policyDisplay(policy any, loc *time.Location, ref time.Time) string {
	pm, _ := policy.(map[string]any)
	segs, _ := pm["segments"].([]any)
	var parts []string
	for _, sg := range segs {
		m, _ := sg.(map[string]any)
		id, _ := m["id"].(string)
		segBy, key, ok := strings.Cut(id, ":")
		if !ok {
			continue
		}
		rule, _ := m["rule"].(map[string]any)
		plan := "none"
		if p, _ := rule["plan"].(string); p != "" {
			plan = p
		}
		parts = append(parts, segLabel(segBy, key, loc, ref)+": "+plan)
	}
	if fb, _ := pm["fallback"].(map[string]any); fb != nil && len(parts) > 0 {
		plan := "none"
		if p, _ := fb["plan"].(string); p != "" {
			plan = p
		}
		parts = append(parts, "otherwise "+plan)
	}
	return strings.Join(parts, "; ")
}

// planForDisplay is a built campaign's own plan as the account reads it: segments in its strategy timezone, or the single plan's name.
func (h *Handler) planForDisplay(spec map[string]any, tenantID string) string {
	loc, err := time.LoadLocation(h.keeper.strategyTZFor(tenantID))
	if err != nil {
		loc = time.UTC
	}
	if d := policyDisplay(spec["own_policy"], loc, time.Now()); d != "" {
		return d + " (" + loc.String() + ")"
	}
	return fmt.Sprint(spec["own_plan"])
}
