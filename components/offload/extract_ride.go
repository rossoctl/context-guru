package offload

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
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
	}()

	var cands []sweepCand
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

// affordable keeps the latest-first prefix of rs whose net, saved*min(turns, horizon) - 11.5*rewritten
// (cache-read equivalents), is highest, and nothing when no prefix is positive. The horizon cap is
// the difference from the sweep's selectAffordableDrops, which credits every turn until the window
// fills: a figure that can exceed the number of turns a session lasts. Without a known window it
// keeps everything.
func (e *ExtractRide) affordable(req *bschemas.BifrostChatRequest, c *components.Ctx,
	cands []sweepCand, rs []rideRemoval) []rideRemoval {

	if c == nil || c.CtxWindow <= 0 {
		return rs
	}
	ord := append([]rideRemoval(nil), rs...)
	sort.Slice(ord, func(a, b int) bool { return cands[ord[a].k].i > cands[ord[b].k].i })
	best, bestK, saved, shallowest := 0.0, 0, 0, 0
	for n := 1; n <= len(ord); n++ {
		saved += ord[n-1].saved
		if i := cands[ord[n-1].k].i; n == 1 || i < shallowest {
			shallowest = i
		}
		_, rewritten, turns := prefixRewriteNet(req, saved, shallowest, c)
		if turns > e.horizon {
			turns = e.horizon
		}
		if net := float64(saved)*float64(turns) - cacheWriteX*float64(rewritten); net > best {
			best, bestK = net, n
		}
	}
	return ord[:bestK]
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
		{Key: "extract_max_ratio", Type: components.FieldFloat, Default: 0.5,
			Hint: "An extract must be at most this share of the original's tokens."},
		{Key: "min_verbatim", Type: components.FieldFloat, Default: 0.8,
			Hint: "Share of an extract's lines that must occur verbatim in the original."},
		{Key: "allow_thinking_strip", Type: components.FieldBool,
			Hint: "Ask even on models that drop earlier thinking blocks when a non-tool-result message is appended (the ask then re-writes the cache)."},
		markerModeField(),
	})
}
