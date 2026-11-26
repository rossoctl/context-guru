package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/rossoctl/context-guru/campaign"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/kvcache"
	"github.com/rossoctl/context-guru/predictive"
)

// The "auto" builder: POST /api/cache/campaigns/build with "builder":"auto" picks the tenant's plan with campaign.BuildSpec (the library holds the
// menu, the tuner call, the held-out evaluation and the ship gate). It runs in-process: no model, no Python. This file reads the history, calls the
// library and writes the result.

// autoData is the fleet's history an auto build reads for the tuner's prior: one read capped at the dashboard's newest-rows limit. A tenant's
// own rows are read separately by autoRowsFor, so a busy fleet can never push them out of the tenant's build. Truncation is reported, never silent.
type autoData struct {
	fleet                   []*kvcache.Request
	since                   int64
	fleetRead, fleetMatched int64
	fleetRows               map[bool]autoRowSet // campaign.Rows of the fleet read, by session_end
}

// autoRowSet is tuner rows with their facts, in the same order.
type autoRowSet struct {
	rows  []predictive.AutoRow
	facts []campaign.RowFacts
}

func (h *Handler) autoFleet(since, until int64) (*autoData, error) {
	fleet, total, err := h.opts.Dashboard.DB().KVCacheDataset(dash.Filter{TenantAll: true, Since: since, Until: until}, dash.KVCacheOptions{ByThread: true})
	if err != nil {
		return nil, err
	}
	return &autoData{fleet: fleet, since: since, fleetRead: int64(len(fleet)), fleetMatched: total, fleetRows: map[bool]autoRowSet{}}, nil
}

// autoOwn reads one tenant's own requests in full (ownTruncated: the read hit the dashboard's row cap). The adapt report needs them raw.
func (h *Handler) autoOwn(d *autoData, tenant string, untilMs int64) (own []*kvcache.Request, ownTruncated bool, err error) {
	own, n, err := h.opts.Dashboard.DB().KVCacheDataset(dash.Filter{Tenant: tenant, Since: d.since, Until: untilMs}, dash.KVCacheOptions{ByThread: true})
	return own, n > int64(len(own)), err
}

// rowsWith is the tuner's rows for one tenant: the fleet's rows of every other tenant, plus the tenant's own rows built from own (its rows in
// the fleet read are replaced, so each is counted once).
func (d *autoData) rowsWith(own []*kvcache.Request, tenant string, price func(string, int64) (float64, float64, float64, float64, bool), sessionEnd bool, untilMs int64) autoRowSet {
	fr, ok := d.fleetRows[sessionEnd]
	if !ok {
		fr.rows, fr.facts = campaign.Rows(d.fleet, price, sessionEnd, untilMs)
		d.fleetRows[sessionEnd] = fr
	}
	mine, mineFacts := campaign.Rows(own, price, sessionEnd, untilMs)
	out := autoRowSet{rows: make([]predictive.AutoRow, 0, len(fr.rows)+len(mine)), facts: make([]campaign.RowFacts, 0, len(fr.rows)+len(mine))}
	for i, r := range fr.rows {
		if r.Scope != tenant {
			out.rows, out.facts = append(out.rows, r), append(out.facts, fr.facts[i])
		}
	}
	out.rows, out.facts = append(out.rows, mine...), append(out.facts, mineFacts...)
	return out
}

// autoRowsFor is rowsWith on the tenant's own read. The tenant's requests are read here and dropped once its rows are built, so a refresh pass
// holds the fleet read plus one tenant's read at a time (each at most the dashboard's row cap). ownTruncated: the tenant's own read hit the cap.
func (h *Handler) autoRowsFor(d *autoData, tenant string, price func(string, int64) (float64, float64, float64, float64, bool), sessionEnd bool, untilMs int64) (set autoRowSet, ownTruncated bool, err error) {
	own, ownTruncated, err := h.autoOwn(d, tenant, untilMs)
	if err != nil {
		return autoRowSet{}, false, err
	}
	return d.rowsWith(own, tenant, price, sessionEnd, untilMs), ownTruncated, nil
}

// note records in the spec what the build read, and says so when a read was cut at the cap.
func (d *autoData) note(spec map[string]any, ownTruncated bool) {
	if ev, _ := spec["evidence"].(map[string]any); ev != nil {
		ev["data"] = map[string]any{"fleet_rows_read": d.fleetRead, "fleet_rows_in_window": d.fleetMatched, "fleet_truncated": d.fleetMatched > d.fleetRead,
			"tenant_truncated": ownTruncated}
	}
	cv, _ := spec["caveats"].([]string)
	if d.fleetMatched > d.fleetRead {
		cv = append(cv, fmt.Sprintf("the fleet prior used the newest %d of %d rows in the window (the tenant's own rows are all read)", d.fleetRead, d.fleetMatched))
	}
	if ownTruncated {
		cv = append(cv, "the tenant's own history exceeds the read cap: only its newest rows are used")
	}
	spec["caveats"] = cv
}
func (h *Handler) runAutoBuild(id, out string, in cacheBuildIn, since, until int64) {
	defer func() { buildMu.Lock(); delete(buildActive, id); buildMu.Unlock() }()
	t0 := time.Now()
	fail := func(msg string, err error) {
		slog.Warn("context-guru: auto campaign build failed", "campaign", id, "err", err)
		_ = writeJSONFile(filepath.Join(out, "status.json"), map[string]any{"state": "failed", "error": msg})
	}
	data, err := h.autoFleet(since, until)
	var set autoRowSet
	var cut bool
	if err == nil {
		set, cut, err = h.autoRowsFor(data, in.ID, h.autoPrice(), in.SessionEnd, until)
	}
	if err != nil {
		fail("could not read the fleet's history", err)
		return
	}
	spec, rec := campaign.BuildSpec(set.rows, set.facts, in.ID, since, until, in.SessionEnd, in.SegmentBy)
	data.note(spec, cut)
	if err := writeSpecValidated(out, spec); err != nil {
		fail("the built spec did not validate: "+err.Error(), err)
		return
	}
	_ = writeJSONFile(filepath.Join(out, "status.json"), map[string]any{"state": "done", "recommendation": rec, "seconds": math.Round(time.Since(t0).Seconds()*10) / 10})
}

// Refresh: a live (shadow or active) auto campaign is rebuilt in place every autoRefreshEvery on the same window length ending now, so its plans
// follow the traffic. In offline replay a plan built once and left for weeks drifted and lost money; rebuilt daily it did not. The mode is kept; a rebuild that finds too little history keeps the previous plan.
// CG_CAMPAIGN_AUTO_REFRESH is a Go duration ("24h", the default); "0" or "off" turns it off.
func autoRefreshEvery() time.Duration {
	v := os.Getenv("CG_CAMPAIGN_AUTO_REFRESH")
	if v == "" {
		return 24 * time.Hour
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	return 0
}

var autoRefreshLast atomic.Int64

// maybeRefreshAutoCampaigns is called from the keeper's sweep tick; it starts at most one refresh per period, never alongside a build.
func (h *Handler) maybeRefreshAutoCampaigns(now time.Time) {
	every := autoRefreshEvery()
	last := autoRefreshLast.Load()
	if every <= 0 || h.predRT() == nil || h.opts.Dashboard == nil || h.opts.Dashboard.DB() == nil || now.UnixMilli()-last < every.Milliseconds() ||
		!autoRefreshLast.CompareAndSwap(last, now.UnixMilli()) {
		return
	}
	if last == 0 { // the first tick after a start only sets the clock: a restart is not a reason to rebuild
		return
	}
	go h.refreshAutoCampaigns(now)
}

func (h *Handler) refreshAutoCampaigns(now time.Time) {
	buildMu.Lock()
	if len(buildActive) > 0 {
		buildMu.Unlock()
		return
	}
	buildActive["auto-refresh"] = true
	buildMu.Unlock()
	defer func() { buildMu.Lock(); delete(buildActive, "auto-refresh"); buildMu.Unlock() }()
	rt := h.predRT()
	rt.mu.RLock()
	live := make([]*predCampaign, 0, len(rt.byTenant))
	for _, pc := range rt.byTenant {
		live = append(live, pc)
	}
	rt.mu.RUnlock()
	// One read of the fleet's history per pass, for the longest window any live auto campaign uses, shared by all of them (the fleet read is
	// capped at the dashboard's newest-rows limit; each campaign tenant's own rows are read in turn, see autoRowsFor), released when the pass ends.
	type job struct {
		pc     *predCampaign
		base   string
		since  int64
		se     bool
		segBy  string
		active bool
	}
	u := now.UnixMilli()
	var jobs []job
	oldest := u
	for _, pc := range live {
		base := filepath.Join(rt.dir, pc.meta.ID)
		var old, req map[string]any
		if readJSONFile(filepath.Join(base, "spec.json"), &old) != nil || readJSONFile(filepath.Join(base, "request.json"), &req) != nil {
			continue
		}
		if b, _ := old["builder"].(map[string]any); b == nil || b["engine"] != "auto" {
			continue
		}
		since, _ := req["since"].(float64)
		until, _ := req["until"].(float64)
		se, _ := req["session_end"].(bool)
		segBy, _ := req["segment_by"].(string)
		pc.mu.Lock()
		active := pc.meta.Mode == campaignModeActive
		pc.mu.Unlock()
		j := job{pc, base, u - int64(until-since), se, segBy, active}
		jobs = append(jobs, j)
		oldest = min(oldest, j.since)
	}
	if len(jobs) == 0 {
		return
	}
	data, err := h.autoFleet(oldest, u)
	if err != nil {
		slog.Warn("context-guru: auto campaign refresh could not read the history", "err", err)
		return
	}
	price := h.autoPrice()
	for _, j := range jobs { // one tenant's own read at a time, dropped before the next (see autoRowsFor)
		set, cut, err := h.autoRowsFor(data, j.pc.meta.Tenant, price, j.se, u)
		if err != nil {
			slog.Warn("context-guru: auto campaign refresh could not read a tenant's history", "campaign", j.pc.meta.ID, "err", err)
			continue
		}
		rows := make([]predictive.AutoRow, 0, len(set.rows))
		facts := make([]campaign.RowFacts, 0, len(set.rows))
		for i, r := range set.rows {
			if r.EndMs >= j.since {
				rows = append(rows, r)
				facts = append(facts, set.facts[i])
			}
		}
		spec, rec := campaign.BuildSpec(rows, facts, j.pc.meta.Tenant, j.since, u, j.se, j.segBy)
		data.note(spec, cut)
		if rec == "insufficient_data" || (j.active && rec != "ship") {
			// an ACTIVE campaign changes its plan only on a rebuild that passed the same ship gate the mode change needs
			_ = writeJSONFile(filepath.Join(j.base, "status.json"), map[string]any{"state": "done", "recommendation": readStatus(j.base)["recommendation"],
				"refresh_kept_previous_plan": rec, "refreshed_at": now.UnixMilli()})
			continue
		}
		spec["refreshed_at"] = now.UTC().Format(time.RFC3339)
		b, err := json.Marshal(spec)
		var ps *predictive.Spec
		if err == nil {
			ps, err = predictive.ParseSpec(b)
		}
		if err != nil {
			slog.Warn("context-guru: auto campaign refresh produced an invalid spec; the previous one stays", "campaign", j.pc.meta.ID, "err", err)
			continue
		}
		if !j.pc.swapSpec(j.base, spec, ps, rec) {
			continue
		}
		_ = writeJSONFile(filepath.Join(j.base, "status.json"), map[string]any{"state": "done", "recommendation": rec, "refreshed_at": now.UnixMilli()})
	}
}

// swapSpec installs a refreshed, already-parsed spec, in memory AND on disk, unless the campaign is ACTIVE and the rebuild was not "ship". The
// mode check, the file write and the swap happen under one lock, so a campaign switched to active while its rebuild ran never takes a plan that
// failed the gate, now or after a restart (which reloads spec.json).
func (pc *predCampaign) swapSpec(dir string, spec map[string]any, ps *predictive.Spec, rec string) bool {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.meta.Mode == campaignModeActive && rec != "ship" {
		return false
	}
	// The write runs under pc.mu, which the request path also takes; it is one small file once a day, so the hold is short.
	if err := writeJSONFile(filepath.Join(dir, "spec.json"), spec); err != nil {
		return false
	}
	pc.spec = ps
	return true
}

// writeSpecValidated parses the spec with the runtime's own parser BEFORE it replaces dir/spec.json (written to a temp file and renamed), so a bad
// rebuild never overwrites a working campaign.
func writeSpecValidated(dir string, spec map[string]any) error {
	b, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	if _, err := predictive.ParseSpec(b); err != nil {
		return err
	}
	return writeJSONFile(filepath.Join(dir, "spec.json"), spec)
}

func readJSONFile(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// autoPrice is the per-model price lookup of an auto build, memoised for one build. The lookup is
// by model, then the rates are the ones of the request's prompt size (modelinfo.Price.At): a long
// request replays at its tier, as it was billed.
func (h *Handler) autoPrice() func(string, int64) (float64, float64, float64, float64, bool) {
	type pr struct {
		p  modelinfo.Price
		ok bool
	}
	prices := map[string]pr{}
	return func(m string, promptTokens int64) (float64, float64, float64, float64, bool) {
		p, seen := prices[m]
		if !seen && h.opts.Prices != nil {
			if q, ok := h.opts.Prices.Price(context.Background(), m); ok && !q.Zero() {
				p = pr{q, true}
			}
			prices[m] = p
		}
		if !p.ok {
			return 0, 0, 0, 0, false
		}
		q := p.p.At(promptTokens)
		return q.Input, q.Output, q.CacheRead, q.CacheWrite, true
	}
}
