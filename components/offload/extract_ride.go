package offload

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/internal/extract"
	"github.com/rossoctl/context-guru/internal/logging"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

func init() { components.Register("extract_llm_ride", newExtractRide) }

// ExtractRide judges many tool outputs in ONE call that rides the session's own prompt cache: the
// question is appended to the body this session last sent upstream (components.PrefixAsker), so the
// whole transcript is a cache read and the call pays only for the instruction, the inventory and
// the answer. Each candidate gets keep | extract (a verbatim-grounded shorter version) | park (set
// aside behind a handle). Every removal is stashed behind a marker the expand tool restores, and
// every decision is frozen in the session store and replayed byte-identically on later turns, so
// the main session's cache is rewritten at most once per decision.
//
// It differs from extract_llm_sweep in what triggers it (any of a size, pending-count, context-fill
// or pre-expiry trigger, not only the pre-expiry window or the economic break-even) and in the
// answer shape (extract as well as drop). It reuses the sweep's candidate state, keep memory,
// marker and rewrite arithmetic rather than restating them.
type ExtractRide struct {
	minTokens, minLaterTurns, maxItems, minInventory int
	trigSize, trigPending                            int
	trigFillMin, trigFillMax                         float64
	trigPreExpiry                                    bool
	preExpiry                                        time.Duration
	minTurnsBetween, keepRecheck                     int
	role                                             string // auto | user | system
	econ                                             bool
	horizon                                          int
	expectRemoval                                    float64
	measuredRemoval                                  bool
	gateMargin                                       float64
	forceApply                                       bool
	remMu                                            sync.Mutex
	remOffered, remRemoved                           float64 // tokens, over all asks of this instance
	applyCold, earlyApply, calibrated, anchor        bool
	maxRatio, minVerbatim                            float64
	allowThinkingStrip                               bool
	mode                                             markerMode
	profiles                                         *summarizerProfiles
}

type extractRideConfig struct {
	MinTokens          int     `yaml:"min_tokens"`
	MinLaterTurns      int     `yaml:"min_later_turns"`
	MaxItems           int     `yaml:"max_items"`
	MinInventory       int     `yaml:"min_inventory"`
	TriggerSizeTokens  int     `yaml:"trigger_size_tokens"`
	TriggerPending     int     `yaml:"trigger_pending"`
	TriggerFillMin     float64 `yaml:"trigger_fill_min"`
	TriggerFillMax     float64 `yaml:"trigger_fill_max"`
	TriggerPreExpiry   *bool   `yaml:"trigger_pre_expiry"`
	PreExpirySeconds   int     `yaml:"pre_expiry_seconds"`
	MinTurnsBetween    *int    `yaml:"min_turns_between"`
	KeepRecheckTurns   *int    `yaml:"keep_recheck_turns"`
	InstructionRole    string  `yaml:"instruction_role"`
	Econ               *bool   `yaml:"econ"`
	HorizonTurns       int     `yaml:"horizon_turns"`
	ExpectedRemoval    float64 `yaml:"expected_removal"`
	ApplyMode          string  `yaml:"apply_mode"`
	EarlyApply         *bool   `yaml:"early_apply"`
	CalibratedHorizon  bool    `yaml:"calibrated_horizon"`
	Anchor             bool    `yaml:"anchor"`
	MeasuredRemoval    *bool   `yaml:"measured_removal"`
	GateMargin         float64 `yaml:"gate_margin"`
	ForceApply         bool    `yaml:"force_apply"`
	RemovalPrior       float64 `yaml:"removal_prior"`
	ExtractMaxRatio    float64 `yaml:"extract_max_ratio"`
	MinVerbatim        float64 `yaml:"min_verbatim"`
	AllowThinkingStrip bool    `yaml:"allow_thinking_strip"`
	MarkerMode         string  `yaml:"marker_mode"`
}

// rideSystemModels are the model-id substrings whose route (Anthropic /v1/messages through the
// reference gateway) was measured to accept a trailing system message and to return parseable
// triage answers under it, at the same cache-read rate as a user turn. Everything else resolves to
// a user turn, the form every route accepts. Widen it per route with instruction_role: system.
var rideSystemModels = []string{"claude-opus-5", "claude-sonnet-5"}

const (
	rideMaxItemsCap   = 24
	rideStatePrefix   = "ride-last:"
	rideReadFloorFrac = 0.8 // share of the call's prompt that must be a cache read to count as riding
)

func newExtractRide(raw []byte) (components.Component, error) {
	cfg := extractRideConfig{}
	if err := components.Decode(raw, &cfg); err != nil {
		return nil, err
	}
	def := func(v, d int) int {
		if v <= 0 {
			return d
		}
		return v
	}
	ifn := func(p *int, d int) int {
		if p == nil {
			return d
		}
		return *p
	}
	bfn := func(p *bool, d bool) bool {
		if p == nil {
			return d
		}
		return *p
	}
	role := strings.ToLower(strings.TrimSpace(cfg.InstructionRole))
	switch role {
	case "":
		role = "auto"
	case "auto", "user", "system":
	default:
		return nil, fmt.Errorf("extract_llm_ride: instruction_role %q must be auto, user or system", cfg.InstructionRole)
	}
	if cfg.MaxItems > rideMaxItemsCap {
		return nil, fmt.Errorf("extract_llm_ride: max_items %d exceeds %d, the most one reply budget can answer", cfg.MaxItems, rideMaxItemsCap)
	}
	e := &ExtractRide{
		minTokens: def(cfg.MinTokens, 800), minLaterTurns: ifn(&cfg.MinLaterTurns, 1),
		maxItems: def(cfg.MaxItems, 12), minInventory: def(cfg.MinInventory, 3),
		trigSize: cfg.TriggerSizeTokens, trigPending: cfg.TriggerPending,
		trigFillMin: cfg.TriggerFillMin, trigFillMax: cfg.TriggerFillMax,
		trigPreExpiry: bfn(cfg.TriggerPreExpiry, true), preExpiry: defaultPreExpiry,
		minTurnsBetween: ifn(cfg.MinTurnsBetween, 3), keepRecheck: ifn(cfg.KeepRecheckTurns, 6),
		role: role, econ: bfn(cfg.Econ, true), horizon: def(cfg.HorizonTurns, 30),
		maxRatio: cfg.ExtractMaxRatio, minVerbatim: cfg.MinVerbatim,
		allowThinkingStrip: cfg.AllowThinkingStrip, mode: parseMarkerMode(cfg.MarkerMode),
	}
	if e.trigSize == 0 && e.trigPending == 0 && e.trigFillMin == 0 {
		// No trigger configured: size and pending are the defaults the pre-expiry trigger sits beside.
		e.trigSize, e.trigPending = 4000, 3
	}
	if cfg.PreExpirySeconds > 0 {
		e.preExpiry = time.Duration(cfg.PreExpirySeconds) * time.Second
	}
	switch strings.ToLower(strings.TrimSpace(cfg.ApplyMode)) {
	case "", "immediate":
	case "cold":
		e.applyCold = true
	default:
		return nil, fmt.Errorf("extract_llm_ride: apply_mode %q must be immediate or cold", cfg.ApplyMode)
	}
	e.earlyApply, e.calibrated, e.anchor = bfn(cfg.EarlyApply, true), cfg.CalibratedHorizon, cfg.Anchor
	e.expectRemoval = cfg.ExpectedRemoval
	if e.expectRemoval <= 0 || e.expectRemoval > 1 {
		e.expectRemoval = 0.6
	}
	// measured_removal replaces the fixed expectation by a running estimate of the share of offered mass
	// the model has actually removed, with expected_removal (or removal_prior) as the prior.
	e.measuredRemoval = bfn(cfg.MeasuredRemoval, false)
	if cfg.GateMargin > 0 {
		e.gateMargin = cfg.GateMargin
	}
	e.forceApply = cfg.ForceApply
	if cfg.RemovalPrior > 0 && cfg.RemovalPrior <= 1 {
		e.expectRemoval = cfg.RemovalPrior
	}
	if e.maxRatio <= 0 || e.maxRatio >= 1 {
		e.maxRatio = 0.5
	}
	if e.minVerbatim <= 0 || e.minVerbatim > 1 {
		e.minVerbatim = 0.8
	}
	if p, err := loadSummarizerProfiles(summarizerProfilesYAML); err == nil {
		e.profiles = p
	}
	return e, nil
}

func (*ExtractRide) Name() string                 { return "extract_llm_ride" }
func (*ExtractRide) Enabled(*components.Ctx) bool { return true }

// resolveRole is the channel the instruction goes in. system only when asked for, or when auto and
// the model is on the measured allow-list; the asker still sends a user turn when the stored body
// cannot carry a system message at its end.
func (e *ExtractRide) resolveRole(model string) string {
	switch e.role {
	case "system", "user":
		return e.role
	}
	id := strings.ToLower(model)
	for _, m := range rideSystemModels {
		if m != "" && strings.Contains(id, strings.ToLower(m)) {
			return "system"
		}
	}
	return "user"
}

func rideStateKey(c *components.Ctx) string {
	return store.SweepKeepPrefix + rideStatePrefix + thread.Key(c.Session, c.Thread)
}

func rideLastTurn(c *components.Ctx) (int, bool) {
	b, ok := c.Store.Get(rideStateKey(c))
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(string(b))
	return n, err == nil
}

func rideSetLast(c *components.Ctx, turn int) {
	c.Store.Put(rideStateKey(c), []byte(strconv.Itoa(turn)))
}

// rideApply replaces msg's text by the frozen decision: park leaves the sweep's shape descriptor,
// extract leaves the extract. Either way the original is stashed behind a marker. replay re-derives
// the identical bytes for a decision already made.
func rideApply(c *components.Ctx, rep *components.Report, mode markerMode, msg *bschemas.ChatMessage,
	content, action, extractText string, replay bool) (string, bool) {
	if action == extract.RidePark {
		return sweepDrop(c, rep, mode, msg, content, replay)
	}
	hint := " [full output: call " + expand.ToolName + "]"
	newText, key, eff, ok := tryMark(c, mode, content, hint, func(tok string) string {
		if tok == "" {
			return extractText
		}
		return extractText + "\n" + tok
	})
	if !ok {
		return "", false
	}
	if replay {
		commitRefresh(c, rep, eff, key, content)
		recordOwner(c, key)
	} else if !commitMark(c, rep, eff, key, content) {
		return "", false
	}
	schema.SetMessageText(msg, newText)
	return key, true
}

func (e *ExtractRide) Offload(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	var keys []string
	changed := 0
	defer func() {
		if changed == 0 {
			rep.Skipped = true
		}
		if e.anchor {
			rideRecordEnd(c, req)
		}
	}()

	var cands []sweepCand
	var held []rideHeld
	var pend map[string]ridePend
	rewrite := false
	if e.applyCold {
		pend = ridePendLoad(c)
		rewrite = rideCompacted(c, len(req.Input)) || rideEnvChanged(c) || c.ColdCache || c.CachePhase(0) == components.CachePhaseCold
	}
	for _, i := range toolIndices(req) {
		msg := &req.Input[i]
		if !schema.Rewritable(*msg) {
			rep.Gate("non_text_blocks")
			continue
		}
		content := schema.MessageText(*msg)
		if content == "" || expand.HasPlaceholder(content) {
			continue
		}
		id := "ride:" + extract.ContentKey(content)
		if r, hit := getResult(c, id); hit {
			action := r.Summary
			if action != extract.RidePark && action != extract.RideExtract {
				continue
			}
			if k, ok := rideApply(c, rep, e.mode, msg, content, action, r.Projected, true); ok {
				changed++
				if k != "" {
					keys = append(keys, k)
				}
				rep.Replay("ride_reapplied")
			}
			continue
		}
		if p, isPend := pend[id]; isPend {
			held = append(held, rideHeld{I: i, ID: id, Content: content, Action: p.A, Text: p.T})
			continue
		}
		if isKeptVerbatim(c, id) || schema.TextTokens(content) < e.minTokens {
			continue
		}
		// The ask reads the previous turn's sent body: an output newer than it is invisible to it.
		if c.MaxCachedIdx >= 0 && i > c.MaxCachedIdx {
			rep.Gate("ride_candidate_not_in_prefix")
			continue
		}
		later := laterModelTurns(req, i)
		if later < e.minLaterTurns {
			rep.Gate("ride_candidate_too_new")
			continue
		}
		if held, _ := sweepKeepStillHolds(c, id, later, e.keepRecheck); held {
			rep.Gate("ride_keep_still_held")
			continue
		}
		toolID := ""
		if msg.ChatToolMessage != nil && msg.ChatToolMessage.ToolCallID != nil {
			toolID = *msg.ChatToolMessage.ToolCallID
		}
		cands = append(cands, sweepCand{i: i, content: content, id: id, toolID: toolID})
	}
	if e.applyCold {
		e.settle(req, rep, c, held, rewrite, &keys, &changed)
		held = nil // settled or still waiting; either way not applied again this request
		if rewrite && len(cands) > 0 {
			// The entry is gone or the transcript was rewritten: an ask now would write, not read.
			rep.Gate("ride_no_ask_on_rewrite")
			return keys, nil
		}
	}
	if len(cands) == 0 {
		return keys, nil
	}

	// Trigger. Any one of the configured conditions fires; the cooldown bounds how often.
	mass := 0
	for _, cd := range cands {
		mass += schema.TextTokens(cd.content)
	}
	_, fill := sweepPressure(req, c)
	var fired []string
	if e.trigSize > 0 && mass >= e.trigSize {
		fired = append(fired, "size")
	}
	if e.trigPending > 0 && len(cands) >= e.trigPending {
		fired = append(fired, "pending")
	}
	if e.trigFillMin > 0 && fill >= e.trigFillMin && (e.trigFillMax <= 0 || fill <= e.trigFillMax) {
		fired = append(fired, "fill")
	}
	preExpiry := e.trigPreExpiry && c.CachePhase(e.preExpiry) == components.CachePhasePreExpiry
	if preExpiry {
		fired = append(fired, "pre_expiry")
	}
	if len(fired) == 0 {
		rep.Gate("ride_no_trigger")
		return keys, nil
	}
	turn := modelTurns(req)
	if last, ok := rideLastTurn(c); ok && turn-last < e.minTurnsBetween {
		rep.Gate("ride_cooldown")
		return keys, nil
	}
	if len(cands) > e.maxItems {
		sort.SliceStable(cands, func(i, j int) bool {
			return schema.TextTokens(cands[i].content) > schema.TextTokens(cands[j].content)
		})
		rep.GateN("ride_over_item_cap", len(cands)-e.maxItems)
		cands = cands[:e.maxItems]
	}
	if len(cands) < e.minInventory {
		rep.GateN("ride_inventory_below_min", len(cands))
		return keys, nil
	}
	if c.PrefixAsk == nil {
		rep.Gate("ride_no_asker")
		return keys, nil
	}
	if e.econ && !preExpiry {
		// The ask itself must be expected to repay: a fraction of the pending mass is removed, the
		// removal is credited for at most horizon turns, and the price is one rewrite of the whole
		// conversation plus the ask (a read of the prefix, the instruction and the answer), all in
		// cache-read equivalents.
		saved := int(e.removalEstimate() * float64(mass))
		if e.rideNet(req, c, saved, firstCand(cands))-(1+e.gateMargin)*e.askCostEquiv(req, c) <= 0 {
			rep.Gate("ride_ask_not_repaid")
			return keys, nil
		}
	}
	if !e.allowThinkingStrip && e.profiles != nil && e.profiles.thinkingLastTurnOnly(c.ModelName) &&
		thinkingWouldBeStripped(req.Input) {
		rep.Gate("ride_thinking_would_strip")
		return keys, nil
	}
	if pc, ok := c.PrefixAsk.(components.PrefixCoverage); ok && c.MaxCachedIdx >= 0 {
		deepest := 0
		for _, cd := range cands {
			if cd.i > deepest {
				deepest = cd.i
			}
		}
		span, rawOK := rawSpanForHash(c, req, deepest+1)
		switch {
		case !rawOK:
			rep.Gate("ride_coords_shifted")
			return keys, nil
		case !pc.CoversSpan(thread.Key(c.Session, c.Thread), deepest+1, components.SpanHash(span)):
			rep.Gate("ride_stale_prefix")
			return keys, nil
		}
	}

	askMass := mass
	decisions, call := e.ask(req, c, rep, cands, strings.Join(fired, "+"))
	rideSetLast(c, turn)
	for _, g := range call.gates {
		rep.Gate(g)
	}
	if call.notRead {
		// A call that did not read the cache is the expensive failure (a full write); back off hard.
		// Other failures (timeout, 429, unparseable) keep only the ordinary cooldown.
		rideSetLast(c, turn+15)
	}

	var ok []rideRemoval
	for _, d := range decisions {
		cd := cands[d.Label]
		switch d.Action {
		case extract.RideKeep:
			rep.Gate("ride_kept")
			recordSweepKeep(c, cd.id, laterModelTurns(req, cd.i))
		case extract.RidePark:
			ok = append(ok, rideRemoval{k: d.Label, action: d.Action,
				saved: schema.TextTokens(cd.content) - schema.TextTokens(sweepDescriptor(cd.content))})
		case extract.RideExtract:
			sz := schema.TextTokens(cd.content)
			et := schema.TextTokens(d.Extract)
			switch {
			case strings.TrimSpace(d.Extract) == "" || expand.HasPlaceholder(d.Extract):
				rep.Gate("ride_extract_unusable")
			case float64(et) > e.maxRatio*float64(sz):
				rep.Gate("ride_extract_not_smaller")
			case !extract.GroundedExtract(cd.content, d.Extract, e.minVerbatim):
				rep.Gate("ride_extract_ungrounded")
			default:
				ok = append(ok, rideRemoval{k: d.Label, action: d.Action, text: strings.TrimSpace(d.Extract), saved: sz - et})
			}
		}
	}
	if call.rec.Component != "" && call.ridden {
		removed := 0
		for _, r := range ok {
			removed += r.saved
		}
		e.noteRemoval(float64(askMass), float64(removed))
	}
	if e.applyCold {
		// Store the answer; apply it only if the removal repays a rewrite now, else at the next
		// request that rewrites the cache anyway.
		var fresh []rideHeld
		pend = ridePendLoad(c)
		for _, r := range ok {
			cd := cands[r.k]
			h := rideHeld{I: cd.i, ID: cd.id, Content: cd.content, Action: r.action, Text: r.text}
			pend[cd.id] = ridePend{A: r.action, T: r.text}
			fresh = append(fresh, h)
		}
		ridePendSave(c, pend)
		total := e.settle(req, rep, c, fresh, false, &keys, &changed)
		if call.rec.Component != "" {
			call.rec.Accepted = len(ok) > 0
			call.rec.SavedTokens = total
			if len(ok) == 0 {
				call.rec.Rejection = "triage returned no applicable removal"
			} else if total == 0 {
				call.rec.Rejection = "held: the removal is applied at the next cold or rewriting request"
			}
			rep.Calls = append(rep.Calls, call.rec)
		}
		return keys, nil
	}
	if e.econ && !preExpiry && len(ok) > 0 {
		kept := e.affordable(req, c, cands, ok)
		if pruned := len(ok) - len(kept); pruned > 0 {
			rep.GateN("ride_unaffordable_pruned", pruned)
			for _, r := range ok {
				if !containsRemoval(kept, r.k) {
					recordSweepKeep(c, cands[r.k].id, laterModelTurns(req, cands[r.k].i))
				}
			}
		}
		ok = kept
	}

	val := savedTokenValue(c)
	total := 0
	for _, a := range ok {
		cd := cands[a.k]
		key, done := rideApply(c, rep, e.mode, &req.Input[cd.i], cd.content, a.action, a.text, false)
		if !done {
			continue
		}
		proj := a.text
		if a.action == extract.RidePark {
			proj = sweepDescriptor(cd.content)
		}
		putResult(c, cd.id, proj, a.action)
		if debugExtractLLM(c) {
			logging.From(c.Ctx).Debug("cg.ride.decision", "session", c.Session, "action", a.action,
				"tokens_before", schema.TextTokens(cd.content), "projected", proj)
		}
		got := schema.TextTokens(cd.content) - schema.TextTokens(schema.MessageText(req.Input[cd.i]))
		if got > 0 {
			total += got
			metrics.RecordExtractionSaving(rep.Component, got)
			metrics.RecordExtractionValue(rep.Component, float64(got)*val.perToken)
		}
		rep.Event("ride_" + a.action)
		changed++
		if key != "" {
			keys = append(keys, key)
		}
	}
	if call.rec.Component != "" {
		call.rec.Accepted = total > 0
		call.rec.SavedTokens = total
		if total == 0 && call.rec.Rejection == "" {
			call.rec.Rejection = "triage returned no applicable removal"
		}
		rep.Calls = append(rep.Calls, call.rec)
	}
	return keys, nil
}

// rideRemoval is one decision that survived validation, awaiting the affordability test.
type rideRemoval struct {
	k      int // index into the candidate list
	action string
	text   string
	saved  int
}

func containsRemoval(s []rideRemoval, k int) bool {
	for _, r := range s {
		if r.k == k {
			return true
		}
	}
	return false
}

// rideNet is the net benefit, in cache-read equivalents, of removing `saved` tokens: each later turn
// reads them no more, credited for at most horizon turns (the sweep's variant credits every turn until
// the window fills, which can exceed the turns a session lasts), against one write of the whole
// conversation that remains. The write covers everything after the system prompt, not only what follows
// the edit: with a breakpoint on the last message only, an edit anywhere leaves just the head readable.
func (e *ExtractRide) rideNet(req *bschemas.BifrostChatRequest, c *components.Ctx, saved, first int) float64 {
	if saved <= 0 {
		return 0
	}
	reqNow := schema.MessagesTokens(req)
	after := reqNow - saved
	if after < 1 {
		after = 1
	}
	turns := e.horizonFor(req)
	if c != nil && c.CtxWindow > 0 {
		if t := turnsRemainingAfter(reqNow, after, modelTurns(req), c.CtxWindow); t < turns {
			turns = t
		}
	}
	w := after
	if _, ok := e.anchorAt(req, c, first); ok {
		// With a breakpoint just before the first edit, the prefix up to the previous request's end is a
		// read and only the messages from the anchor on are written.
		w = 0
		for k := first - 1; k < len(req.Input); k++ {
			w += schema.TextTokens(schema.MessageText(req.Input[k]))
		}
		if w -= saved; w < 1 {
			w = 1
		}
	}
	return float64(saved)*float64(turns) - cacheWriteX*float64(w)
}

// rideDefaultHead is the system prompt plus tool declarations the ask reads in addition to the
// transcript, when the host does not report the previous turn's billed input (about 13k tokens for
// Claude Code).
const rideDefaultHead = 13000

// askCostEquiv prices one ask in cache-read equivalents: a read of the whole previous request (its billed
// input when known, else the transcript plus a default head: the system prompt and tools are read too),
// the instruction and inventory at fresh price, and about 250 output tokens at 5x fresh.
func (e *ExtractRide) askCostEquiv(req *bschemas.BifrostChatRequest, c *components.Ctx) float64 {
	prefix := float64(schema.MessagesTokens(req) + rideDefaultHead)
	if c != nil && c.PrevBilledInput > 0 {
		prefix = float64(c.PrevBilledInput)
	}
	return prefix + 10*(600+250*5)
}

// affordable applies all of rs or none: the rewrite is paid once for the whole batch, so the net grows
// with the mass removed and a subset is never better than the set.
func (e *ExtractRide) affordable(req *bschemas.BifrostChatRequest, c *components.Ctx,
	cands []sweepCand, rs []rideRemoval) []rideRemoval {

	saved := 0
	for _, r := range rs {
		saved += r.saved
	}
	if e.rideNet(req, c, saved, firstRemoval(cands, rs)) <= 0 {
		return nil
	}
	return rs
}

type rideCall struct {
	gates   []string
	rec     components.ModelCall
	ridden  bool // the call read the session's cache (it did not re-create the prefix)
	notRead bool // the call completed but did not ride: the back-off case
}

// ask puts the triage question to the request's own model over its cached prefix and returns the
// parsed per-candidate decisions (labels index cands). The call's full cost goes into call.rec.
// There is deliberately no content-copying fallback: a call that cannot ride the cache is not made.
func (e *ExtractRide) ask(req *bschemas.BifrostChatRequest, c *components.Ctx, rep *components.Report,
	cands []sweepCand, trigger string) (_ []extract.RideDecision, call rideCall) {

	items := make([]extract.AdjudicationItem, len(cands))
	before := 0
	for k, cd := range cands {
		sz := schema.TextTokens(cd.content)
		before += sz
		items[k] = extract.AdjudicationItem{Label: k, ID: cd.toolID, SizeTokens: sz,
			Head: extract.HeadLine(cd.content, extract.AdjudicationHeadChars)}
	}
	prompt := extract.BuildRideAsk(items)
	role := e.resolveRole(c.ModelName)
	ctx, cancel := context.WithTimeout(c.Ctx, llmCallTimeout)
	defer cancel()

	start := time.Now()
	var (
		reply string
		u     components.PrefixUsage
		err   error
	)
	session := thread.Key(c.Session, c.Thread)
	if ra, isRole := c.PrefixAsk.(components.RolePrefixAsker); isRole {
		reply, u, err = ra.AskAs(ctx, session, prompt, role)
	} else {
		role = "user"
		reply, u, err = c.PrefixAsk.Ask(ctx, session, prompt)
	}
	ms := float64(time.Since(start).Milliseconds())
	if errors.Is(err, components.ErrNoPrefix) {
		call.gates = append(call.gates, "ride_no_prefix")
		return nil, call
	}
	pricing := cheapmodel.PricingFromEnv()
	if !c.SelfRates.Zero() {
		pricing = ratesPricing(c.SelfRates)
	}
	cost := pricing.Cost(int64(u.Fresh), int64(u.Output), int64(u.CacheWrite), int64(u.CacheRead))
	metrics.RecordExtractionCall(rep.Component, ms)
	metrics.RecordExtractionSpend(rep.Component, cost)
	call.rec = components.ModelCall{
		Component: rep.Component, Model: c.ModelName, Strategy: "ride_" + role,
		CandidateTokens: before, LatencyMs: ms,
		PromptTokens: int64(u.Fresh), CompletionTokens: int64(u.Output),
		CacheRead: int64(u.CacheRead), CacheWrite: int64(u.CacheWrite), CostUSD: cost,
		GateReason: fmt.Sprintf("trigger=%s;items=%d;instr_tokens=%d;role=%s", trigger, len(cands),
			schema.TextTokens(prompt), role),
	}
	if err != nil {
		call.gates = append(call.gates, "ride_ask_failed")
		call.rec.Rejection = "ask failed: " + err.Error()
		logging.From(c.Ctx).Warn("cg.ride.ask_failed", "session", c.Session, "err", err)
		return nil, call
	}
	total := u.Fresh + u.CacheRead + u.CacheWrite
	call.ridden = total > 0 && float64(u.CacheRead) >= rideReadFloorFrac*float64(total)
	if !call.ridden {
		call.notRead = true
		call.gates = append(call.gates, "ride_not_cache_read")
		call.rec.Rejection = fmt.Sprintf("the ask did not ride the cache (read %d, write %d, fresh %d); "+
			"its answer is discarded", u.CacheRead, u.CacheWrite, u.Fresh)
		return nil, call
	}
	ds, parsed := extract.ParseRideDecisions(reply)
	if !parsed {
		if extract.ReplyWasTruncated(reply) {
			call.gates = append(call.gates, "ride_reply_truncated")
		} else {
			call.gates = append(call.gates, "ride_unparseable")
		}
		call.rec.Rejection = "reply did not parse; every output kept"
		return nil, call
	}
	seen := map[int]bool{}
	out := ds[:0]
	for _, d := range ds {
		switch {
		case d.Label < 0 || d.Label >= len(cands):
			call.gates = append(call.gates, "ride_unknown_label")
		case seen[d.Label]:
			call.gates = append(call.gates, "ride_duplicate_label")
		case d.Action != extract.RideKeep && d.Action != extract.RideExtract && d.Action != extract.RidePark:
			call.gates = append(call.gates, "ride_unknown_action")
		default:
			seen[d.Label] = true
			out = append(out, d)
		}
	}
	return out, call
}

func init() {
	components.RegisterFields("extract_llm_ride", extractRideConfig{}, []components.Field{
		{Key: "min_tokens", Type: components.FieldInt, Default: 800, Min: 1,
			Hint: "Per-output floor for being a candidate. Each candidate costs one inventory line in the ask."},
		{Key: "min_later_turns", Type: components.FieldInt, Default: 1, Min: 0,
			Hint: "Model turns that must follow an output before it is offered. 0 offers the newest outputs too."},
		{Key: "max_items", Type: components.FieldInt, Default: 12, Min: 1,
			Hint: "Most candidates one ask may carry (largest first). Capped at 24 by the reply budget."},
		{Key: "min_inventory", Type: components.FieldInt, Default: 3, Min: 1,
			Hint: "Fewest candidates worth asking about. Judgement against peers is better with more of them."},
		{Key: "trigger_size_tokens", Type: components.FieldInt, Min: 0,
			Hint: "Fire when the pending candidates total at least this many tokens (0 = off). With no trigger_* set, 4000."},
		{Key: "trigger_pending", Type: components.FieldInt, Min: 0,
			Hint: "Fire when at least this many candidates are pending (0 = off). With no trigger_* set, 3."},
		{Key: "trigger_fill_min", Type: components.FieldFloat, Min: 0,
			Hint: "Fire when the request occupies at least this share of the model window (0 = off)."},
		{Key: "trigger_fill_max", Type: components.FieldFloat, Min: 0,
			Hint: "Upper end of the context-fill band (0 = no upper bound)."},
		{Key: "trigger_pre_expiry", Type: components.FieldBool, Default: true,
			Hint: "Fire in the pre-expiry window, when rewriting the prefix costs little because the cache is about to lapse."},
		{Key: "pre_expiry_seconds", Type: components.FieldInt, Default: int(defaultPreExpiry / time.Second),
			Hint: "Width of the pre-expiry window."},
		{Key: "min_turns_between", Type: components.FieldInt, Default: 3, Min: 0,
			Hint: "Model turns between two asks of one session. A call that completed without reading the cache backs off 15 turns; other failures keep the cooldown."},
		{Key: "keep_recheck_turns", Type: components.FieldInt, Default: 6, Min: 0,
			Hint: "Turns before re-offering an output the model judged still needed."},
		{Key: "instruction_role", Type: components.FieldEnum, Default: "auto", Options: []string{"auto", "user", "system"},
			Hint: "Channel for the instruction. auto = a mid-conversation system message on models measured to follow it, else a final user turn."},
		{Key: "econ", Type: components.FieldBool, Default: true,
			Hint: "Apply only the removals that repay the cache rewrite they force, within horizon_turns. Skipped in the pre-expiry window."},
		{Key: "horizon_turns", Type: components.FieldInt, Default: 30, Min: 1,
			Hint: "Turns a removal is credited for in the affordability test."},
		{Key: "expected_removal", Type: components.FieldFloat, Default: 0.6,
			Hint: "Share of the pending candidate mass the pre-ask economic test assumes will be removed."},
		{Key: "apply_mode", Type: components.FieldEnum, Default: "immediate", Options: []string{"immediate", "cold"},
			Hint: "immediate = apply the answer on the turn it arrives. cold = ask while the prefix is warm (a cache read), store the answer, and apply it only on a request that rewrites the cache anyway (entry expired, client compaction), or earlier when the removal is expected to repay the rewrite (early_apply)."},
		{Key: "early_apply", Type: components.FieldBool, Default: true,
			Hint: "In cold mode, also apply held decisions on a warm request when saved tokens x horizon exceed the cost of one whole-conversation rewrite."},
		{Key: "calibrated_horizon", Type: components.FieldBool,
			Hint: "Credit a removal for the expected remaining requests (22 - t/2, between 11 and 22; fitted on SWE-bench Claude Code sessions) instead of horizon_turns."},
		{Key: "anchor", Type: components.FieldBool,
			Hint: "When decisions are applied on a warm request, place a cache breakpoint on the assistant message just before the first edited tool result, so the prefix up to the previous request's end is read and only the tail is rewritten. Needs a free breakpoint slot (the cap is 4 per request)."},
		{Key: "measured_removal", Type: components.FieldBool,
			Hint: "Use a running estimate of the share of offered mass the model really removes (prior removal_prior) in the pre-ask economic test, instead of the fixed expected_removal."},
		{Key: "gate_margin", Type: components.FieldFloat, Min: 0,
			Hint: "Ask only when the expected net exceeds (1 + gate_margin) times the ask's own price, so a call is made when it is clearly positive."},
		{Key: "force_apply", Type: components.FieldBool,
			Hint: "Analysis only: apply every held decision at once, whatever it costs (the net a decision would have if applied)."},
		{Key: "removal_prior", Type: components.FieldFloat,
			Hint: "Prior for measured_removal (0 = expected_removal). Worth 20,000 offered tokens when blended with measurements."},
		{Key: "extract_max_ratio", Type: components.FieldFloat, Default: 0.5,
			Hint: "An extract must be at most this share of the original's tokens."},
		{Key: "min_verbatim", Type: components.FieldFloat, Default: 0.8,
			Hint: "Share of an extract's lines that must occur verbatim in the original."},
		{Key: "allow_thinking_strip", Type: components.FieldBool,
			Hint: "Ask even on models that drop earlier thinking blocks when a non-tool-result message is appended (the ask then re-writes the cache)."},
		markerModeField(),
	})
}

// Pending decisions (apply_mode: cold). The ask is made while the prefix is warm, so it is a cache read;
// its answer is stored here and applied only on a request that rewrites the cache anyway (the entry has
// expired, or the client compacted), or earlier when the removal is expected to repay the rewrite.
type rideHeld struct {
	I       int
	ID      string
	Content string
	Action  string
	Text    string
}

type ridePend struct {
	A string `json:"a"`
	T string `json:"t,omitempty"`
}

func ridePendKey(c *components.Ctx) string {
	return store.SweepKeepPrefix + "ride-pend:" + thread.Key(c.Session, c.Thread)
}

func ridePendLoad(c *components.Ctx) map[string]ridePend {
	m := map[string]ridePend{}
	if b, ok := c.Store.Get(ridePendKey(c)); ok {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func ridePendSave(c *components.Ctx, m map[string]ridePend) {
	if b, err := json.Marshal(m); err == nil {
		c.Store.Put(ridePendKey(c), b)
	}
}

func rideLenKey(c *components.Ctx) string {
	return store.SweepKeepPrefix + "ride-len:" + thread.Key(c.Session, c.Thread)
}

// rideCompacted reports whether the transcript is shorter than on the previous request of this thread
// (the client compacted), which forces a full rewrite whatever we do. It records the current length.
func rideCompacted(c *components.Ctx, n int) bool {
	prev := -1
	if b, ok := c.Store.Get(rideLenKey(c)); ok {
		prev, _ = strconv.Atoi(string(b))
	}
	c.Store.Put(rideLenKey(c), []byte(strconv.Itoa(n)))
	return prev > 0 && n < prev
}

// horizonFor is the number of later requests a removal is credited for. A configured horizon_turns is
// used as is. With calibrated_horizon it is the expected number of requests still to come, fitted on 169
// real SWE-bench Claude Code sessions (opus-5; mean remaining requests 19 at request 5, 15 at 10, 12 at
// 15, 11 at 20 and beyond): 22 - t/2, held between 11 and 22. It is a property of that workload.
func (e *ExtractRide) horizonFor(req *bschemas.BifrostChatRequest) int {
	if !e.calibrated {
		return e.horizon
	}
	h := 22 - modelTurns(req)/2
	if h < 11 {
		h = 11
	}
	return h
}

// settle applies the held decisions when this request rewrites the cache anyway (rewrite) or when the
// removal is expected to repay the rewrite within the horizon (early). Returns the tokens removed.
func (e *ExtractRide) settle(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx,
	held []rideHeld, rewrite bool, keys *[]string, changed *int) int {

	if len(held) == 0 {
		return 0
	}
	reason := "cold"
	if e.forceApply {
		rewrite = true // analysis knob: apply every held decision as soon as it exists
	}
	if !rewrite {
		// Latest-first prefixes: an older edit makes the rewrite longer, so the best set may be a suffix.
		ord := append([]rideHeld(nil), held...)
		sort.Slice(ord, func(a, b int) bool { return ord[a].I > ord[b].I })
		best, bestK, saved := 0.0, 0, 0
		for k := 1; k <= len(ord); k++ {
			saved += schema.TextTokens(ord[k-1].Content) - rideProjectedTokens(ord[k-1])
			if net := e.rideNet(req, c, saved, ord[k-1].I); net > best {
				best, bestK = net, k
			}
		}
		if !e.earlyApply || bestK == 0 {
			rep.GateN("ride_held_waiting", len(held))
			return 0
		}
		if bestK < len(ord) {
			rep.GateN("ride_held_waiting", len(ord)-bestK)
		}
		held, reason = ord[:bestK], "early"
	}
	pend := ridePendLoad(c)
	total := 0
	val := savedTokenValue(c)
	for _, h := range held {
		key, got, done := e.applyDecision(req, rep, c, h)
		if !done {
			continue
		}
		delete(pend, h.ID)
		if got > 0 {
			total += got
			metrics.RecordExtractionSaving(rep.Component, got)
			metrics.RecordExtractionValue(rep.Component, float64(got)*val.perToken)
		}
		rep.Event("ride_applied_" + reason)
		*changed++
		if key != "" {
			*keys = append(*keys, key)
		}
	}
	ridePendSave(c, pend)
	if total > 0 {
		if !rewrite && e.anchor {
			first := held[0].I
			for _, h := range held {
				if h.I < first {
					first = h.I
				}
			}
			if idx, ok := e.anchorAt(req, c, first); ok && markBreakpoint(&req.Input[idx], c.LastMarkTTL) {
				rep.Event("ride_anchor_placed")
			}
		}
		c.Store.Put(rideApplyEndKey(c), []byte(strconv.Itoa(len(req.Input)-1)))
	}
	return total
}

func rideEndsKey(c *components.Ctx) string {
	return store.SweepKeepPrefix + rideStatePrefix + "ends:" + thread.Key(c.Session, c.Thread)
}

// rideEnd is a cache entry a request left at its last message: the hash of the forwarded prefix it
// described (every field of every message, cache marks excluded) and when it was written or last read.
type rideEnd struct {
	H string `json:"h"`
	T int64  `json:"t"`
}

// rideNow is the clock; a test replaces it. rideEntryTTL is how long an entry is trusted: the provider
// keeps one five minutes after its last use, with a margin for skew.
var rideNow = time.Now

const rideEntryTTL = 270 * time.Second

func rideLoadEnds(c *components.Ctx) map[int]rideEnd {
	ends := map[int]rideEnd{}
	if b, ok := c.Store.Get(rideEndsKey(c)); ok {
		_ = json.Unmarshal(b, &ends)
	}
	return ends
}

// rideEndValid reports whether an earlier request of this thread left a live cache entry exactly at
// message e whose prefix is byte-for-byte what req carries now.
func rideEndValid(c *components.Ctx, req *bschemas.BifrostChatRequest, e int) bool {
	en, ok := rideLoadEnds(c)[e]
	if !ok || e < 0 || e >= len(req.Input) || rideNow().UnixMilli()-en.T >= rideEntryTTL.Milliseconds() {
		return false
	}
	return en.H == ridePrefixHash(req.Input[:e+1])
}

// rideRecordEnd remembers the entry this request leaves at its last message. A request that rewrote the
// cache (cold) found none of the earlier entries alive, so they are forgotten, not trusted.
func rideRecordEnd(c *components.Ctx, req *bschemas.BifrostChatRequest) {
	if len(req.Input) == 0 {
		return
	}
	ends := rideLoadEnds(c)
	now := rideNow().UnixMilli()
	if c.ColdCache {
		ends = map[int]rideEnd{}
	}
	for e, en := range ends {
		if e >= len(req.Input) || now-en.T >= rideEntryTTL.Milliseconds() {
			delete(ends, e)
		}
	}
	last := len(req.Input) - 1
	ends[last] = rideEnd{H: ridePrefixHash(req.Input), T: now}
	if len(ends) > 64 {
		lo := last
		for e := range ends {
			if e < lo {
				lo = e
			}
		}
		delete(ends, lo)
	}
	if b, err := json.Marshal(ends); err == nil {
		c.Store.Put(rideEndsKey(c), b)
	}
}

func rideApplyEndKey(c *components.Ctx) string {
	return store.SweepKeepPrefix + "ride-applyend:" + thread.Key(c.Session, c.Thread)
}

// anchorAt reports where a cache breakpoint can sit so that an edit starting at message `first` rewrites
// only what follows the previous request's end. The anchor is the assistant message just before the
// edited tool result: a breakpoint there looks back one block, finds the entry the previous request wrote
// at its last message (first-2), and reads up to it. It requires that entry to hold the content we are
// about to send (no edit applied at or before it after it was written) and a free breakpoint slot.
func (e *ExtractRide) anchorAt(req *bschemas.BifrostChatRequest, c *components.Ctx, first int) (int, bool) {
	if !e.anchor || !e.applyCold || c == nil || first < 2 || first > len(req.Input) {
		return 0, false
	}
	idx := first - 1
	m := &req.Input[idx]
	if m.Role != bschemas.ChatMessageRoleAssistant || m.Content == nil || len(m.Content.ContentBlocks) == 0 ||
		m.Content.ContentBlocks[len(m.Content.ContentBlocks)-1].CacheControl != nil {
		return 0, false
	}
	if !rideEndValid(c, req, first-2) {
		return 0, false
	}
	if c.LastMarkTTL == "" && c.CacheTTLMs >= 3_600_000 {
		return 0, false // a one-hour request whose last mark we cannot read: a 5m anchor could precede 1h marks
	}
	visible := 0
	for i := range req.Input {
		if req.Input[i].Content == nil {
			continue
		}
		for _, blk := range req.Input[i].Content.ContentBlocks {
			if blk.CacheControl != nil {
				visible++
			}
		}
	}
	existing := visible
	if c.ExistingBreakpoints > visible {
		existing = c.ExistingBreakpoints // also counts system blocks and tool_result marks components never see
	}
	if existing >= 4 {
		return 0, false
	}
	return idx, true
}

// markBreakpoint puts the anchor mark on m. Its TTL is that of the last mark after it (the client's, on the
// last message), so a one-hour mark never follows a five-minute one; marks before it are the client's
// and, if the request is valid, at least as long.
func markBreakpoint(m *bschemas.ChatMessage, lastTTL string) bool {
	last := &m.Content.ContentBlocks[len(m.Content.ContentBlocks)-1]
	cc := &bschemas.CacheControl{Type: bschemas.CacheControlTypeEphemeral}
	if lastTTL == "1h" {
		ttl := "1h"
		cc.TTL = &ttl
	}
	last.CacheControl = cc
	return true
}

func firstCand(cands []sweepCand) int {
	first := -1
	for _, cd := range cands {
		if first < 0 || cd.i < first {
			first = cd.i
		}
	}
	return first
}

func firstRemoval(cands []sweepCand, rs []rideRemoval) int {
	first := -1
	for _, r := range rs {
		if i := cands[r.k].i; first < 0 || i < first {
			first = i
		}
	}
	return first
}

func rideProjectedTokens(h rideHeld) int {
	if h.Action == extract.RidePark {
		return schema.TextTokens(sweepDescriptor(h.Content))
	}
	return schema.TextTokens(h.Text)
}

// applyDecision removes one output, freezes the decision, and returns the tokens it saved.
func (e *ExtractRide) applyDecision(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx, h rideHeld) (string, int, bool) {
	key, done := rideApply(c, rep, e.mode, &req.Input[h.I], h.Content, h.Action, h.Text, false)
	if !done {
		return "", 0, false
	}
	proj := h.Text
	if h.Action == extract.RidePark {
		proj = sweepDescriptor(h.Content)
	}
	putResult(c, h.ID, proj, h.Action)
	if debugExtractLLM(c) {
		logging.From(c.Ctx).Debug("cg.ride.decision", "session", c.Session, "action", h.Action,
			"tokens_before", schema.TextTokens(h.Content), "projected", proj)
	}
	return key, schema.TextTokens(h.Content) - schema.TextTokens(schema.MessageText(req.Input[h.I])), true
}

// rideEnvChanged reports whether the model or the declared tool set differs from the thread's previous
// request. Either change invalidates the cached prefix (tools and system come first), so that request
// is a full rewrite whatever we do. It records the current values.
func rideEnvChanged(c *components.Ctx) bool {
	key := store.SweepKeepPrefix + "ride-env:" + thread.Key(c.Session, c.Thread)
	cur := c.ModelName + "|" + c.ToolsHash
	prev, ok := c.Store.Get(key)
	c.Store.Put(key, []byte(cur))
	return ok && string(prev) != cur
}

// removalPriorWeight is how many offered tokens the prior is worth when blending with measurements.
const removalPriorWeight = 20000.0

// removalEstimate is the share of offered mass expected to be removed: the prior until asks have been
// made, then the prior blended with the realised share, weighted by offered tokens.
func (e *ExtractRide) removalEstimate() float64 {
	if !e.measuredRemoval {
		return e.expectRemoval
	}
	e.remMu.Lock()
	defer e.remMu.Unlock()
	return (e.remRemoved + e.expectRemoval*removalPriorWeight) / (e.remOffered + removalPriorWeight)
}

func (e *ExtractRide) noteRemoval(offered, removed float64) {
	if !e.measuredRemoval {
		return
	}
	e.remMu.Lock()
	e.remOffered += offered
	e.remRemoved += removed
	e.remMu.Unlock()
}

// ridePrefixHash identifies a message prefix the way the provider's cache sees it: the text of each
// message however it is represented (a string, or one or several text blocks), plus every non-text part
// (images, tool calls and results, reasoning), with cache marks excluded. SpanHash would also tell a
// string from a one-block list, which the provider does not, and Claude Code resends an older message as
// a string after sending it as a marked block.
func ridePrefixHash(msgs []bschemas.ChatMessage) string {
	h := sha1.New()
	for i := range msgs {
		m := msgs[i]
		h.Write([]byte(m.Role))
		h.Write([]byte{0})
		h.Write([]byte(schema.MessageText(m)))
		h.Write([]byte{0})
		if m.ChatAssistantMessage != nil {
			b, _ := json.Marshal(m.ChatAssistantMessage)
			h.Write(b)
		}
		h.Write([]byte{0})
		if m.ChatToolMessage != nil {
			b, _ := json.Marshal(m.ChatToolMessage)
			h.Write(b)
		}
		h.Write([]byte{0})
		if m.Content != nil {
			for _, blk := range m.Content.ContentBlocks {
				if blk.Type == bschemas.ChatContentBlockTypeText {
					continue
				}
				blk.CacheControl = nil
				b, _ := json.Marshal(blk)
				h.Write(b)
			}
		}
		h.Write([]byte{1})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
