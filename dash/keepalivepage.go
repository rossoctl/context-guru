package dash

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/kvcache"
)

// The Keep-alive PAGE: a per-session drill-down beside the keep-alive tab's own account-wide
// ledger (dash/keepalive.go) — one conversation's real timeline, what every available policy
// would have decided at each of its real decision points, the feature values behind those
// decisions, and what each policy would have spent and saved on this session and on the
// account it belongs to.
//
// # Reuse, not reinvention
//
// Every number here rests on machinery this package or package kvcache already has and has
// already been tested: KVCacheDataset for the row-to-kvcache.Request conversion (the same
// (tenant, session, model) partitioning kvcache.go's own LEAD window uses), kvcache.Observation
// and kvcache.Strategy for the predictors (a struct that CANNOT carry a future-derived field —
// see kvcache/strategy.go's own doc comment), kvcache.NewOptimal for the hindsight ceiling,
// KVCacheSimulate for the alternative-policy economics, kaSaved and KeepAliveLedger for the
// real, exact credit. This file is the glue between them and the one thing none of them do on
// their own: walk a single session's rows forward in time, building one leak-free Observation
// per row.
//
// # The one rule this file exists to enforce
//
// A predictor must be handed an Observation built from ONLY the rows up to and including the
// one it is deciding about. kaObservation's signature is the enforcement: it takes one row plus
// already-advanced state (entry, turn, sinceLastMs, hist), never a slice a caller could
// misindex into the future. TestKeepAlivePagePredictorsCannotSeeTheFuture is the same
// reflection-based poison-value check kvcache's own TestStrategiesCannotSeeTheFuture uses,
// applied to what this file builds.

// kaPageArms is the curated subset of kvcache's registry shown side by side on this page —
// smaller than the whole KV-cache tab's list, because this page is about the keep-alive
// decision specifically: hold plainly, hold with pings, hold only when the stop_reason says the
// turn is really over, hold by the account's own learned history, hold by the compiled
// reuse model, and the two bounds (the shipped baseline and the unreachable ceiling).
var kaPageArms = []string{
	kvcache.StrategyFixed5m,
	kvcache.StrategyKeepAlive5m,
	kvcache.StrategyKeepAlive5mOnce,
	kvcache.StrategyExtend1h,
	kvcache.StrategyStopReasonGated,
	kvcache.StrategyHistorical,
	kvcache.StrategyKeepAliveBudget,
	kvcache.StrategyOptimal,
}

// The feature-availability classes. See docs/results/kv-ttl-predictor-features.md, which this
// catalog transcribes rather than restates from memory — a second, drifted copy of that table
// is exactly the failure mode this project keeps hitting.
const (
	availLive                 = "live"
	availOfflineDerivable     = "offline-derivable"
	availNeedsInstrumentation = "needs-instrumentation"
)

// KAFeature is one row of the feature inspector: what a predictor could know, where it comes
// from, and — for this specific decision point — what it actually was.
//
// Missing is never hidden. A feature this deployment cannot instrument yet is listed with
// Missing=true and MissingReason stating why, in the same table as the features that ARE
// available, rather than left off the page.
type KAFeature struct {
	Name         string `json:"name"`
	Source       string `json:"source"`
	Availability string `json:"availability"`
	Explanation  string `json:"explanation"`
	// Value, Missing and MissingReason are filled in PER DECISION POINT by
	// kaFeatureValuesAt — everything above is the static catalog.
	Value         string `json:"value,omitempty"`
	Missing       bool   `json:"missing"`
	MissingReason string `json:"missing_reason,omitempty"`
	// Example is a sanitized real value from THIS session's own history — never a made-up
	// number standing in for a real one.
	Example string `json:"example,omitempty"`
}

// kaFeatureCatalog is the static half of the feature inspector: name, source, availability
// class and explanation. kaFeatureValuesAt fills in Value/Missing/Example per decision point
// from a fresh copy of this slice, so callers can never see one call's values bleed into
// another's.
func kaFeatureCatalog() []KAFeature {
	return []KAFeature{
		{Name: "stop_reason (3-cluster)", Source: "requests.stop_reason",
			Availability: availLive, Explanation: "The last turn's terminal reason, grouped into " +
				"still-working / looks-done-isn't / actually-done. The single strongest signal " +
				"found for whether the NEXT request lands in the rescuable 5m-1h band."},
		{Name: "tenant identity", Source: "requests.tenant_id", Availability: availLive,
			Explanation: "Which account this is. Per-tenant band rate ranges 0.76%-41.4% on the " +
				"production corpus, so a rule tuned on the pooled population is wrong for almost " +
				"everyone."},
		{Name: "hour of day (UTC)", Source: "requests.ts", Availability: availLive,
			Explanation: "The decision instant's UTC hour. Real structure exists (two peaks, a " +
				"trough), but the store carries no per-tenant timezone, so this cannot be read as " +
				"\"morning\" for any specific account."},
		{Name: "day of week", Source: "requests.ts (derived)", Availability: availOfflineDerivable,
			Explanation: "Cheap to derive from the timestamp; not yet a field on Observation."},
		{Name: "Turn (request ordinal in conversation)", Source: "already on Observation",
			Availability: availLive, Explanation: "How many requests this conversation has served " +
				"so far, on this model. Later turns are measurably less likely to land in the " +
				"rescuable band."},
		{Name: "SinceLastMs (the just-closed gap)", Source: "already on Observation",
			Availability: availLive, Explanation: "How long this conversation had been idle before " +
				"the request just served. \"The single most useful past fact a strategy has\", per " +
				"the domain package's own comment."},
		{Name: "cached prefix size (CachedTokens)", Source: "already on Observation",
			Availability: availLive, Explanation: "The billed prefix (cache_read + cache_write) " +
				"this request leaves behind — what a ping refreshes and what a lapse re-creates."},
		{Name: "per-(tenant,model,bucket) historical reuse rate", Source: "kvcache.History",
			Availability: availOfflineDerivable, Explanation: "The share of this cell's " +
				"ALREADY-CLOSED gaps that closed within 5m/1h, with an explicit fallback level. " +
				"Leak-free by construction: this page seeds it from this session's own gaps only " +
				"(see the session note on the predictors panel), not the account's full history."},
		{Name: "agent (client dialect)", Source: "requests.agent", Availability: availLive,
			Explanation: "claude-cli / openai / anthropic / litellm / curl-python-*. A genuinely " +
				"distinct population per dialect, not just a label."},
		{Name: "model", Source: "requests.model", Availability: availLive,
			Explanation: "The cache does not transfer across models — a session that switches " +
				"model is two conversations for every predictor on this page."},
		{Name: "requested TTL tier (cache_ttl)", Source: "requests.cache_ttl",
			Availability: availLive, Explanation: "What the client ASKED for, not what was " +
				"honoured — cache_write_1h is the only evidence a 1h request was actually granted."},
		{Name: "cache_miss_reason", Source: "requests.cache_miss_reason", Availability: availLive,
			Explanation: "hit / cold_start / ttl_expiry / prefix_change / unknown. A forced-miss " +
				"reason (cold_start, prefix_change) overrides any TTL choice — no policy on this " +
				"page can rescue one."},
		{Name: "declared tool count / system_blocks", Source: "requests.tools, .system_blocks",
			Availability: availOfflineDerivable, Explanation: "A proxy for task complexity, not " +
				"yet validated as a predictive feature."},
		{Name: "number of this tenant's other sessions currently open",
			Source: "not tracked as a live count", Availability: availNeedsInstrumentation,
			Explanation: "Would need a per-tenant open-session gauge; not stored today.",
			Missing:     true, MissingReason: "no session registry is persisted anywhere this page " +
				"can read"},
		{Name: "last tool call name, per-request", Source: "tool_uses (session-level only)",
			Availability: availNeedsInstrumentation, Explanation: "tool_uses is keyed by " +
				"(session, tool name) with first/last-seen timestamps, not by request.",
			Missing: true, MissingReason: "no request-level tool-call log exists"},
		{Name: "subagent / sidechain marker", Source: "no parent/child link recorded",
			Availability: availNeedsInstrumentation, Explanation: "Would need a session hierarchy " +
				"column that does not exist.",
			Missing: true, MissingReason: "no parent/child session link is stored"},
	}
}

// kaEntryState is the cache entry's state ENTERING a decision — what the previous request on
// the same (session, model) left behind. The zero value is TTLNone: no entry.
type kaEntryState struct {
	Tier      kvcache.TTL
	ExpiresAt int64
}

// kaEntryAfter derives the entry a row leaves behind, from that row's own recorded outcome —
// present-tense fact, never a later row's.
func kaEntryAfter(row *kvcache.Request) kaEntryState {
	if row.CachedContext <= 0 || row.TTL == kvcache.TTLNone {
		return kaEntryState{}
	}
	return kaEntryState{Tier: row.TTL, ExpiresAt: row.TS + row.TTL.Lifetime().Milliseconds()}
}

// kaObservation builds one row's Observation from ONLY that row and state the caller has
// already advanced up to it — never from a slice it could misindex into the future. This
// signature IS the leak guard: there is no way to reach a later row from inside it.
func kaObservation(row *kvcache.Request, entry kaEntryState, turn int, sinceLastMs int64,
	hist *kvcache.History, prices *kvcache.PriceList) kvcache.Observation {
	return kvcache.Observation{
		User: row.User, Conversation: row.ConversationID, Model: row.Model,
		RequestID: row.ID, Now: row.TS, HourUTC: row.HourUTC, Bucket: row.Bucket,
		CachedTokens: row.CachedContext, SinceLastMs: sinceLastMs,
		TTL: entry.Tier, ExpiresAt: entry.ExpiresAt, Turn: turn,
		StopReason: row.StopReason, Stats: hist, Pricing: prices.For(row.Model),
	}
}

// kaFeatureValuesAt fills the per-decision half of the feature inspector from a fresh copy of
// the catalog, using only `row`, the Observation already built for it, and the entry/history
// state entering it — the same past-only inputs the predictors themselves see.
func kaFeatureValuesAt(row *kvcache.Request, obs kvcache.Observation, hist *kvcache.History) []KAFeature {
	fs := kaFeatureCatalog()
	set := func(name, value string) {
		for i := range fs {
			if fs[i].Name == name {
				fs[i].Value = value
				return
			}
		}
	}
	setMissing := func(name, reason string) {
		for i := range fs {
			if fs[i].Name == name {
				fs[i].Missing, fs[i].MissingReason = true, reason
				return
			}
		}
	}
	set("stop_reason (3-cluster)", string(kvcache.ClusterOf(row.StopReason))+" ("+
		displayOr(row.StopReason, "unset")+")")
	set("tenant identity", row.User)
	set("hour of day (UTC)", fmt.Sprintf("%02d:00", row.HourUTC))
	set("day of week", time.UnixMilli(row.TS).UTC().Weekday().String())
	set("Turn (request ordinal in conversation)", fmt.Sprintf("%d", obs.Turn))
	if obs.Turn > 1 {
		set("SinceLastMs (the just-closed gap)", fmt.Sprintf("%.1fs", float64(obs.SinceLastMs)/1000))
	} else {
		setMissing("SinceLastMs (the just-closed gap)",
			"this model's first request in the session — no prior gap exists")
	}
	set("cached prefix size (CachedTokens)", fmt.Sprintf("%d tokens", obs.CachedTokens))
	if p, n, level := hist.ReuseWithin(obs.User, obs.Model, obs.Bucket, 5*time.Minute); n > 0 {
		set("per-(tenant,model,bucket) historical reuse rate",
			fmt.Sprintf("%.1f%% within 5m (n=%d, level=%s)", p*100, n, level))
	} else {
		setMissing("per-(tenant,model,bucket) historical reuse rate",
			"no closed gaps yet in this session's own history at this point")
	}
	set("agent (client dialect)", row.Agent)
	set("model", row.Model)
	set("requested TTL tier (cache_ttl)", displayOr(row.TTLSource, "unknown")+": "+
		displayOr(string(row.TTL), "none"))
	set("cache_miss_reason", displayOr(row.MissReason, "hit"))
	return fs
}

func displayOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// KAPredictorDecision is what one candidate policy would have decided at one real decision
// point, using only what kaObservation gave it.
type KAPredictorDecision struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Action      string `json:"action"`
	// Unreachable marks the arm that is told the true next-request time (kvcache.StrategyOptimal)
	// — a ceiling, never a result. Carried straight from kvcache.StrategySpec so this page cannot
	// drift from the domain's own flag.
	Unreachable bool `json:"unreachable,omitempty"`
}

// kaRunPredictor calls Decide with a recover(), for the same reason kvcache.Simulate itself
// does: this file has no recover() of its own between it and an HTTP handler, and a panicking
// Predictor (BudgetPolicy's compiled-in model) must not take the whole page down with it.
func kaRunPredictor(s kvcache.Strategy, spec kvcache.StrategySpec, obs kvcache.Observation) (out KAPredictorDecision) {
	out = KAPredictorDecision{Name: s.Name(), Unreachable: spec.Unreachable}
	if d, ok := s.(kvcache.Describer); ok {
		out.Description = d.Describe()
	}
	defer func() {
		if r := recover(); r != nil {
			out.Action = "error"
		}
	}()
	out.Action = string(s.Decide(obs))
	return out
}

// KAPing is one keep-alive ping observed to have fired after a decision point, before the next
// one.
type KAPing struct {
	TS      int64   `json:"ts"`
	CostUSD float64 `json:"cost_usd"`
	// ReadNothing is cache_read = 0: the ping arrived after the entry had already lapsed and
	// re-created it rather than refreshing it — see kaSaved's own doc comment.
	ReadNothing bool `json:"read_nothing"`
	// Wrote is cache_write > cache_read: the ping's own request paid the CREATION rate, the
	// pathology of a schedule whose interval exceeds the lifetime it is protecting.
	Wrote bool `json:"wrote"`
}

// KADecision is one real request: the timeline row, the pings that followed it, every
// predictor's call on it, and the feature values behind that call.
type KADecision struct {
	RequestID int64  `json:"request_id"`
	TS        int64  `json:"ts"`
	Turn      int    `json:"turn"`
	Model     string `json:"model"`
	// ModelSwitch marks the first request on a model other than the session's first — see
	// kvcache/kvcache.go's own comment: a cache entry does not transfer between models, so this
	// is a second conversation as far as every predictor on this page is concerned.
	ModelSwitch bool   `json:"model_switch,omitempty"`
	StopReason  string `json:"stop_reason"`
	StopCluster string `json:"stop_cluster"`

	CachedTokens int64 `json:"cached_tokens"`
	// SinceLastMs is the gap that closed just before this request arrived — PAST information,
	// safe on every predictor's Observation.
	SinceLastMs int64 `json:"since_last_ms"`

	// TTLInForce/RemainingSeconds describe the entry ENTERING this decision — derived from the
	// PREVIOUS request on this model, never from this row's own outcome.
	TTLInForce       string  `json:"ttl_in_force"`
	RemainingSeconds float64 `json:"remaining_seconds_at_arrival,omitempty"`

	// IdleTriggerSeconds and CoverageSeconds are the shipped policy's own X and K*X+TTL, on the
	// wire so the timeline can draw both boundaries without retyping the arithmetic.
	IdleTriggerSeconds float64 `json:"idle_trigger_seconds"`
	CoverageSeconds    float64 `json:"coverage_seconds"`

	// The real, present-tense outcome of THIS request.
	Hit         bool    `json:"hit"`
	MissReason  string  `json:"miss_reason"`
	Addressable bool    `json:"addressable"`
	CacheRead   int64   `json:"cache_read"`
	CacheWrite  int64   `json:"cache_write"`
	CostUSD     float64 `json:"cost_usd"`

	// SavedUSDCredited is kaSaved's own reachability-corrected figure, never the raw column —
	// see kaSaved's doc comment for why the raw column overstates 7.1% of credited rows on the
	// production corpus. CreditReachable false on a row whose raw credit is > 0 means exactly
	// that phantom case.
	SavedUSDCredited float64 `json:"saved_usd_credited,omitempty"`
	CreditReachable  bool    `json:"credit_reachable,omitempty"`

	Pings      []KAPing              `json:"pings"`
	Predictors []KAPredictorDecision `json:"predictors"`
	Features   []KAFeature           `json:"features"`
}

// KASession is the whole per-session page payload.
type KASession struct {
	SessionID  string   `json:"session_id"`
	TenantID   string   `json:"tenant_id"`
	Models     []string `json:"models"`
	MultiModel bool     `json:"multi_model"`

	Decisions []KADecision `json:"decisions"`
	Requests  int64        `json:"requests"`
	Pings     int64        `json:"pings"`

	Economics      *KAPageEconomics  `json:"economics,omitempty"`
	LedgerVsReplay *KALedgerVsReplay `json:"ledger_vs_replay,omitempty"`

	// SessionStatsNote states the one simplification every predictor decision on this page
	// rests on: HistoricalProbability's Stats are seeded from THIS SESSION'S OWN gaps only, not
	// the account's full history — a single conversation rarely has six closed gaps in the same
	// (model, hour-bucket) cell, so the per-request predictor panel will show that arm falling
	// through to a coarser fallback level (or LevelNone) more often than the account-wide replay
	// in Economics does. The cohort/session economics panel does NOT have this limitation: it
	// runs kvcache.Simulate over the real dataset, which accumulates History the same way
	// production statistics would.
	SessionStatsNote string `json:"session_stats_note"`
}

const kaSessionStatsNote = "The side-by-side predictor panel seeds each arm's historical " +
	"statistics from this session's own gaps only, walked forward in time — leak-free, but " +
	"thinner than the account's full history, so an arm that leans on history (historical-" +
	"probability) may show a coarser fallback level here than it would on the account's real " +
	"traffic. The economics panel below does not have this limitation: it replays the real " +
	"dataset."

// kaBuildDecisions is the per-row walk: chronological, one Observation per row, built from
// ONLY that row and state already advanced past every earlier one. It is a pure function of
// its arguments — no database, no clock — which is what makes it directly testable with a
// hand-built []*kvcache.Request and a spy Strategy, the same way kvcache's own
// TestStrategiesCannotSeeTheFuture tests Simulate.
//
// reqs must already be chronological (KVCacheDataset's own contract) and may mix models — the
// per-model maps below are the partition, exactly kvcache.Conversation's own (user, session,
// model) key.
func kaBuildDecisions(reqs []*kvcache.Request, priceList *kvcache.PriceList, strategies []kvcache.Strategy,
	specByName map[string]kvcache.StrategySpec, credits map[int64]kaCreditInfo) ([]KADecision, []string) {
	hist := kvcache.NewHistory()
	lastByModel := map[string]*kvcache.Request{}
	entryByModel := map[string]kaEntryState{}
	turnByModel := map[string]int{}
	seenModel := map[string]bool{}
	firstRowSeen := false

	out := make([]KADecision, 0, len(reqs))
	for _, row := range reqs {
		prev := lastByModel[row.Model]
		var sinceLastMs int64
		if prev != nil && prev.HasNext && prev.IdleMs != nil {
			sinceLastMs = *prev.IdleMs
			// Close the gap into History BEFORE building THIS row's Observation — the exact
			// order kvcache.Simulate itself uses, and the reason this row's own Stats lookup may
			// legitimately include the gap that ended in its own arrival.
			hist.Observe(row.User, row.Model, kvcache.BucketAt(prev.TS),
				time.Duration(*prev.IdleMs)*time.Millisecond)
		}
		turnByModel[row.Model]++
		turn := turnByModel[row.Model]
		entry := entryByModel[row.Model]

		obs := kaObservation(row, entry, turn, sinceLastMs, hist, priceList)

		dec := KADecision{
			RequestID: row.ID, TS: row.TS, Turn: turn, Model: row.Model,
			ModelSwitch: firstRowSeen && !seenModel[row.Model],
			StopReason:  row.StopReason, StopCluster: string(kvcache.ClusterOf(row.StopReason)),
			CachedTokens: row.CachedContext, SinceLastMs: sinceLastMs,
			IdleTriggerSeconds: recIdleSeconds,
			CoverageSeconds:    CoverageSeconds(recIdleSeconds, recMaxPings),
			Hit:                row.Hit, MissReason: row.MissReason,
			Addressable: row.MissReason == "ttl_expiry" && row.CacheWrite > 0,
			CacheRead:   row.CacheRead, CacheWrite: row.CacheWrite, CostUSD: row.CostUSD,
		}
		if entry.Tier != kvcache.TTLNone {
			dec.TTLInForce = entry.Tier.Label()
			dec.RemainingSeconds = float64(entry.ExpiresAt-row.TS) / 1000
		} else {
			dec.TTLInForce = "none"
		}
		if c, ok := credits[row.ID]; ok {
			dec.SavedUSDCredited, dec.CreditReachable = c.usd, c.usd > 0
		}
		seenModel[row.Model] = true
		firstRowSeen = true

		for _, s := range strategies {
			dec.Predictors = append(dec.Predictors, kaRunPredictor(s, specByName[s.Name()], obs))
		}
		dec.Features = kaFeatureValuesAt(row, obs, hist)
		out = append(out, dec)

		entryByModel[row.Model] = kaEntryAfter(row)
		lastByModel[row.Model] = row
	}
	var models []string
	for m := range seenModel {
		models = append(models, m)
	}
	return out, models
}

// KeepAlivePageSession builds the whole per-session page.
//
// sessionFilter's Session must already be set — the caller (the HTTP handler) is the one place
// that both parses the query string and enforces tenant scope, and duplicating that here would
// be a second place a scoping bug could hide.
func (d *DB) KeepAlivePageSession(sessionFilter, cohortFilter Filter, price modelinfo.Pricer) (*KASession, error) {
	if sessionFilter.Session == "" {
		return nil, fmt.Errorf("dash: name a session")
	}
	reqs, _, err := d.KVCacheDataset(sessionFilter, KVCacheOptions{})
	if err != nil {
		return nil, err
	}
	out := &KASession{SessionID: sessionFilter.Session, SessionStatsNote: kaSessionStatsNote}
	if len(reqs) == 0 {
		return out, nil
	}
	out.TenantID = reqs[0].User
	out.Requests = int64(len(reqs))

	pings, err := d.kaPagePings(sessionFilter)
	if err != nil {
		return nil, err
	}
	out.Pings = int64(len(pings))
	credits, err := d.kaPageCredits(sessionFilter)
	if err != nil {
		return nil, err
	}

	priceList := kvcache.NewPriceList(context.Background(), modelsOf(reqs), price, kvcache.Multipliers{}, nil)
	cfg := kvcache.Config{Prices: priceList}

	specByName := map[string]kvcache.StrategySpec{}
	for _, spec := range kvcache.Registry() {
		specByName[spec.Name] = spec
	}
	var strategies []kvcache.Strategy
	for _, name := range kaPageArms {
		s, err := kvcache.NewStrategy(name, reqs, cfg)
		if err != nil {
			continue // an arm this build cannot construct (e.g. a missing compiled model) is
			// skipped rather than failing the whole page — the rest of the comparison still
			// stands.
		}
		strategies = append(strategies, s)
	}

	out.Decisions, out.Models = kaBuildDecisions(reqs, priceList, strategies, specByName, credits)
	kaAttachPings(out.Decisions, pings)
	out.MultiModel = len(out.Models) > 1

	econ, err := d.kaPageEconomics(sessionFilter, cohortFilter, price)
	if err != nil {
		return nil, err
	}
	out.Economics = econ
	lvr, err := d.kaLedgerVsReplay(cohortFilter, econ.Cohort)
	if err != nil {
		return nil, err
	}
	out.LedgerVsReplay = lvr
	return out, nil
}

// kaAttachPings assigns each ping to the latest decision at or before it, chronologically —
// a display convenience (which real request the ping followed), not the financial attribution:
// the dollar credit for a rescued request is kaSaved's, computed independently and already on
// each KADecision as SavedUSDCredited.
func kaAttachPings(decisions []KADecision, pings []kaPingLogRow) {
	if len(decisions) == 0 {
		return
	}
	di := 0
	for _, p := range pings {
		for di+1 < len(decisions) && decisions[di+1].TS <= p.ts {
			di++
		}
		decisions[di].Pings = append(decisions[di].Pings, KAPing{
			TS: p.ts, CostUSD: p.cost, ReadNothing: p.read == 0, Wrote: p.write > p.read,
		})
	}
}

// kaPingLogRow is one keep-alive ping row, as read for the timeline.
type kaPingLogRow struct {
	ts          int64
	cost        float64
	read, write int64
}

// kaPagePings reads the ping rows in scope, chronologically — the SECOND query the whole tab's
// convention requires (see dash/keepalive.go's own top-of-file comment): a ping row must never
// be read through the same query as agent traffic.
func (d *DB) kaPagePings(f Filter) ([]kaPingLogRow, error) {
	kaCond, kaArgs := withKeepAlive(f).where()
	rows, err := d.sql.QueryContext(d.readCtx(), `SELECT r.ts, r.cost_usd, r.cache_read, r.cache_write
		FROM requests r WHERE `+kaCond+` AND r.keepalive = 1 ORDER BY r.ts, r.id`, kaArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []kaPingLogRow
	for rows.Next() {
		var p kaPingLogRow
		if err := rows.Scan(&p.ts, &p.cost, &p.read, &p.write); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// kaCreditInfo is one agent row's reachability-corrected keep-alive credit.
type kaCreditInfo struct{ usd float64 }

// kaPageCredits reads kaSaved's ceiling-corrected credit for every row in scope that carries a
// raw keepalive_saved_usd — including the phantom ones kaSaved zeroes, so the caller can tell
// "credited" from "recorded but unreachable" (kaCreditInfo.usd == 0 despite the row being present).
func (d *DB) kaPageCredits(f Filter) (map[int64]kaCreditInfo, error) {
	cond, args := f.where()
	rows, err := d.sql.QueryContext(d.readCtx(), `SELECT r.id, `+kaSaved("r.")+`
		FROM requests r WHERE `+cond+` AND r.keepalive_saved_usd > 0`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]kaCreditInfo{}
	for rows.Next() {
		var id int64
		var usd float64
		if err := rows.Scan(&id, &usd); err != nil {
			return nil, err
		}
		out[id] = kaCreditInfo{usd: usd}
	}
	return out, rows.Err()
}

// KAPageEconomics is requirement 4's whole payload: what every candidate policy would have
// spent and saved, on this session and on the cohort, side by side. Both are exactly
// KVCacheSimulation — the KV-cache tab's own, already-tested simulation payload, with its own
// Arms (Unreachable/NeedsDataset/Baseline flags), Results, Savings and Pricing — reused rather
// than re-shaped, because a second definition of "what optimal means" is the specific mistake
// this comparison exists to not make.
type KAPageEconomics struct {
	Session *KVCacheSimulation `json:"session"`
	Cohort  *KVCacheSimulation `json:"cohort"`
}

func (d *DB) kaPageEconomics(sessionFilter, cohortFilter Filter, price modelinfo.Pricer) (*KAPageEconomics, error) {
	cfg := KVCacheSimConfig{Strategies: kaPageArms, Baseline: kvcache.StrategyFixed5m}
	sess, err := d.KVCacheSimulate(sessionFilter, KVCacheOptions{}, price, cfg)
	if err != nil {
		return nil, err
	}
	coh, err := d.KVCacheSimulate(cohortFilter, KVCacheOptions{}, price, cfg)
	if err != nil {
		return nil, err
	}
	return &KAPageEconomics{Session: sess, Cohort: coh}, nil
}

// kaReplayDisagreeToleranceUSD is the smallest gap between the ledger and the replay worth
// calling out. Below it, the two are the same number modulo floating-point noise.
const kaReplayDisagreeToleranceUSD = 0.01

// KALedgerVsReplay is requirement 6: the real ledger's net beside what a clean keepalive-5m
// replay says the same traffic would have netted, so a disagreement is shown rather than
// picked between.
type KALedgerVsReplay struct {
	LedgerNetUSD float64 `json:"ledger_net_usd"`
	LedgerPings  int64   `json:"ledger_pings"`
	ReplayArm    string  `json:"replay_arm"`
	ReplayNetUSD float64 `json:"replay_net_usd"`
	// ReplayKnown false means the replay could not be priced (no rate for any model in scope) —
	// then ReplayNetUSD is not a comparable zero, it is an absence.
	ReplayKnown bool   `json:"replay_known"`
	Disagree    bool   `json:"disagree"`
	Note        string `json:"note"`
}

const kaLedgerVsReplayNote = "The ledger is exact attribution on the real, historical pings " +
	"this account actually sent, under whatever schedule and operational conditions were in " +
	"force at the time. The replay instead asks what a clean keepalive-5m policy would have " +
	"cost and saved on the SAME traffic, from the recorded tiers and rates alone — no " +
	"operational slippage, no partial rollout, no missed ping. A gap between the two is not a " +
	"bug in either figure; it says how far the real mechanism runs from the textbook one."

func (d *DB) kaLedgerVsReplay(cohortFilter Filter, cohortEcon *KVCacheSimulation) (*KALedgerVsReplay, error) {
	led, err := d.KeepAliveLedger(cohortFilter)
	if err != nil {
		return nil, err
	}
	out := &KALedgerVsReplay{
		LedgerNetUSD: led.NetUSD, LedgerPings: led.Pings,
		ReplayArm: kvcache.StrategyKeepAlive5m, Note: kaLedgerVsReplayNote,
	}
	if cohortEcon != nil {
		for i, r := range cohortEcon.Results {
			if r.Strategy == kvcache.StrategyKeepAlive5m && i < len(cohortEcon.Savings) {
				out.ReplayNetUSD, out.ReplayKnown = cohortEcon.Savings[i].AbsoluteUSD, r.Valued
				break
			}
		}
	}
	if out.ReplayKnown {
		out.Disagree = math.Abs(out.LedgerNetUSD-out.ReplayNetUSD) > kaReplayDisagreeToleranceUSD
	}
	return out, nil
}

// KAPageSessionRow is one row of the session picker / cohort list.
type KAPageSessionRow struct {
	SessionID      string  `json:"session_id"`
	TenantID       string  `json:"tenant_id"`
	Model          string  `json:"model"`
	Agent          string  `json:"agent"`
	StopReason     string  `json:"stop_reason"`
	Requests       int64   `json:"requests"`
	Addressable    int64   `json:"addressable_misses"`
	AddressableUSD float64 `json:"addressable_usd"`
}

// KeepAlivePageSessions lists the sessions matching a cohort filter (tenant/model/agent/
// stop_reason, all already carried by Filter), for the session picker and the cohort summary.
// Costliest-addressable-miss first, same ordering rule as the tab's own KeepAliveSessions —
// consistent with it rather than a second, differently-sorted list of the same sessions.
func (d *DB) KeepAlivePageSessions(f Filter, limit int) ([]*KAPageSessionRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	cond, args := f.where()
	rows, err := d.sql.QueryContext(d.readCtx(), `SELECT r.session_id, MIN(r.tenant_id),
			MIN(r.model), MIN(r.agent), MIN(r.stop_reason), COUNT(*),
			COALESCE(SUM(CASE WHEN r.cache_miss_reason='ttl_expiry' AND r.cache_write>0 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN r.cache_miss_reason='ttl_expiry' AND r.cache_write>0 THEN r.cost_usd ELSE 0 END),0)
		FROM requests r WHERE `+cond+` AND r.session_id <> ''
		GROUP BY r.session_id ORDER BY 7 DESC, 6 DESC LIMIT ?`,
		append(append([]any(nil), args...), limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*KAPageSessionRow{}
	for rows.Next() {
		var s KAPageSessionRow
		if err := rows.Scan(&s.SessionID, &s.TenantID, &s.Model, &s.Agent, &s.StopReason,
			&s.Requests, &s.Addressable, &s.AddressableUSD); err != nil {
			return nil, err
		}
		out = append(out, &s)
	}
	return out, rows.Err()
}
