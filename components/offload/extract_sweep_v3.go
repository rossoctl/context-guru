package offload

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/compactionpoint"
	"github.com/rossoctl/context-guru/internal/coref"
	"github.com/rossoctl/context-guru/schema"
)

// SWEEP V3 (#491): rules, protected items, and one big cut per round, on the warm path.
//
// It is a CONFIGURATION of the warm path, not a component. It is OFF unless a v3 setting is written
// (selector rule or hybrid, fill_small, fill_large, min_drop_frac, protect_tools, protect_paths or
// protect_last_n). With none written, the warm path runs exactly as before.
//
// THE GATES, in order. P is the billed prompt of this thread (PrevBilledInputThread). C is the cut point:
// Ctx.CutFrac x the fill denominator when a summarizer is in the pipeline, else the fill denominator
// (CompactionPoint when known, else the window). C needs an exact window.
//
//	1. sweet spot     P >= fill(C) x C. fill is fill_small at C <= 200k, fill_large at C >= 1M, linear between.
//	2. one cut/round  after an accepted cut, wait until P has grown by min_drop_frac x C since the cut.
//	3. meaningful     D >= min_drop_frac x P. D is what survives the recheck, rules and judge together.
//	4. payback        the warm gate of #720, unchanged.
//
// THE SELECTION.
//
//	rule    rules only. No model call.
//	judge   the model, as before. Protected outputs are not offered.
//	hybrid  the rules decide what they can. The model is asked about the rest.
//
// THE RULES read tool names, tool input paths and commands from the request in flight. They write nothing
// to the Store: only ids, hashes and counts, in memory.
//
//	file_reread   a file read whose same path was read again later, or edited or written later.
//	build_rerun   a build, test or lint run (a shell tool) whose same command ran again later.
//
// An output that carries a marker, or that the agent expanded, is never a rule candidate.
//
// PROTECTED, never dropped by rule or judge: protect_tools (tool names), protect_paths (globs on the tool
// input path), the newest protect_last_n tool results, and anything expanded in this session.

const (
	selectorJudge  = "judge"
	selectorRule   = "rule"
	selectorHybrid = "hybrid"

	defaultProtectLastN = 10
	defaultFillSmall    = 0.85
	defaultFillLarge    = 0.6
	defaultMinDropFrac  = 0.15

	v3FillLoC = 200_000
	v3FillHiC = 1_000_000

	ruleFileReread = "file_reread"
	ruleBuildRerun = "build_rerun"
	// ruleUnreferenced is rule (c): nothing later references the output (the recheck's referenced_later test).
	ruleUnreferenced = "unreferenced"

	defaultRuleUnrefMinAge = 30
	sourceJudge            = "judge"

	maxV3Recent = 50
)

// v3Config is the parsed v3 part of the config.
type v3Config struct {
	on                   bool
	selector             string
	fillSmall, fillLarge float64
	minDropFrac          float64
	protectTools         map[string]bool
	protectPaths         []*regexp.Regexp
	protectLastN         int
	ruleUnref            bool
	ruleUnrefMinAge      int
	creditAvoided        bool
}

// parseSweepV3 reads the v3 settings. A value outside its range reads as its default, as the other warm
// settings do. Without warm: true a v3 setting does nothing.
func parseSweepV3(cfg extractSweepConfig) (v3Config, error) {
	out := v3Config{selector: selectorJudge}
	switch cfg.Selector {
	case "", selectorJudge:
	case selectorRule, selectorHybrid:
		out.selector = cfg.Selector
		out.on = true
	default:
		return out, fmt.Errorf("extract_llm_sweep: selector %q is not one of judge, rule, hybrid", cfg.Selector)
	}
	if cfg.FillSmall > 0 || cfg.FillLarge > 0 || cfg.MinDropFrac > 0 || len(cfg.ProtectTools) > 0 ||
		len(cfg.ProtectPaths) > 0 || cfg.ProtectLastN != nil || cfg.RuleUnreferenced {
		out.on = true
	}
	if !out.on {
		return out, nil
	}
	if !cfg.Warm {
		// Like every warm_* setting: without warm the setting does nothing. The gates live in the warm path.
		return v3Config{selector: selectorJudge}, nil
	}
	out.fillSmall, out.fillLarge, out.minDropFrac = cfg.FillSmall, cfg.FillLarge, cfg.MinDropFrac
	if out.fillSmall <= 0 || out.fillSmall > 1 {
		out.fillSmall = defaultFillSmall
	}
	if out.fillLarge <= 0 || out.fillLarge > 1 {
		out.fillLarge = defaultFillLarge
	}
	if out.minDropFrac <= 0 || out.minDropFrac > 1 {
		out.minDropFrac = defaultMinDropFrac
	}
	out.protectLastN = defaultProtectLastN
	if cfg.ProtectLastN != nil {
		out.protectLastN = *cfg.ProtectLastN
		if out.protectLastN < 0 {
			out.protectLastN = 0
		}
	}
	// On by default in v3 only: the plain warm path never reaches this line.
	out.creditAvoided = cfg.CreditAvoidedCompaction == nil || *cfg.CreditAvoidedCompaction
	out.ruleUnref, out.ruleUnrefMinAge = cfg.RuleUnreferenced, cfg.RuleUnreferencedMinAge
	if out.ruleUnrefMinAge <= 0 {
		out.ruleUnrefMinAge = defaultRuleUnrefMinAge
	}
	out.protectTools = map[string]bool{}
	for _, n := range cfg.ProtectTools {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			out.protectTools[n] = true
		}
	}
	for _, g := range cfg.ProtectPaths {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		re, err := globRegexp(g)
		if err != nil {
			return out, fmt.Errorf("extract_llm_sweep: protect_paths %q: %w", g, err)
		}
		out.protectPaths = append(out.protectPaths, re)
	}
	return out, nil
}

// globRegexp turns a path glob into an anchored regexp. * is any run of characters inside one folder, ** is
// any run across folders, ? is one character. A pattern with a leading / matches from the root; any other
// pattern may match the end of the path (so "*.md" and "docs/plan.md" work for any root).
func globRegexp(g string) (*regexp.Regexp, error) {
	var b strings.Builder
	rooted := strings.HasPrefix(g, "/")
	if rooted {
		b.WriteString("^")
	} else {
		b.WriteString("^(?:.*/)?")
	}
	for i := 0; i < len(g); i++ {
		switch ch := g[i]; ch {
		case '*':
			if i+1 < len(g) && g[i+1] == '*' {
				i++
				if i+1 < len(g) && g[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// v3Fill is the sweet spot as a share of C: fill_small at C <= 200k, fill_large at C >= 1M, linear between.
func v3Fill(c int, small, large float64) float64 {
	switch {
	case c <= v3FillLoC:
		return small
	case c >= v3FillHiC:
		return large
	}
	return small + (large-small)*float64(c-v3FillLoC)/float64(v3FillHiC-v3FillLoC)
}

// Where C came from (V3Stats.CSource, V3Cut.CSource).
const (
	cSourceCutFrac    = "cutfrac"    // CutFrac x the effective context, a summarizer is in the pipeline
	cSourceCompaction = "compaction" // the compaction point: operator, measured or assumed
	cSourceWindow     = "window"     // the model's context window
)

// v3CutPoint is C, or 0 when it is unknown. It needs an exact window (the same rule as the turns model).
// It also says where C came from.
//
// The sweet spot is a share of the EFFECTIVE context: where the client acts, not the window. So:
//
//	CutFrac x the effective context  when a summarizer is in the pipeline,
//	the compaction point             when its source is operator, measured or assumed,
//	the window                       otherwise.
//
// A compaction point whose source is window_fallback is the window under another name: the table had no entry
// and said "C is the whole window". It is treated as the window, and reported as "window".
func (e *ExtractSweep) v3CutPoint(c *components.Ctx) (cut int, source string) {
	if c == nil || !c.CtxWindowExact || c.CtxWindow <= 0 {
		return 0, ""
	}
	cut, source = c.CtxWindow, cSourceWindow
	if c.CompactionPoint > 0 && c.CompactionPointSource != string(compactionpoint.WindowFallback) {
		cut, source = c.CompactionPoint, cSourceCompaction
	}
	if c.CutFrac > 0 {
		cut, source = int(c.CutFrac*float64(cut)), cSourceCutFrac
	}
	return cut, source
}

// ---------------------------------------------------------------------------------------------------
// The rules and the protection. Tool names, paths and commands are read from the request in flight.

type v3Call struct{ name, path, cmd string }

// v3PlanBuilds counts plans built, so a test can show that a request which needs none builds none.
var v3PlanBuilds atomic.Int64

var cmdKeys = []string{"command", "cmd"}

// callCommand is the command of a shell tool call, with its white space collapsed, or "".
func callCommand(args string) string {
	if args == "" {
		return ""
	}
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return ""
	}
	for _, k := range cmdKeys {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.Join(strings.Fields(v), " ")
		}
	}
	return ""
}

func shellTool(name string) bool {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "bash"), strings.Contains(n, "shell"), strings.Contains(n, "terminal"):
		return true
	}
	switch n {
	case "exec", "execute", "run_command", "run_terminal_cmd", "command":
		return true
	}
	return false
}

// buildRun matches a command that builds, tests or lints.
var buildRun = regexp.MustCompile(`(?:^|[\s;&|(])(?:go (?:test|build|vet)|make|pytest|python[0-9.]* -m (?:pytest|unittest)|tox|nox|` +
	`(?:npm|pnpm|yarn)(?: run)? (?:test|build|lint)|npx (?:jest|vitest|tsc|eslint)|cargo (?:test|build|check|clippy)|` +
	`golangci-lint|tsc|eslint|ruff|mypy|flake8|pylint|mvn|gradlew?|bazel (?:test|build)|ctest|jest|vitest|rspec|dotnet (?:test|build))(?:$|[\s;&|)])`)

// v3Calls maps each tool call id to its name, path and command.
func v3Calls(req *bschemas.BifrostChatRequest) map[string]v3Call {
	out := map[string]v3Call{}
	for i := range req.Input {
		a := req.Input[i].ChatAssistantMessage
		if a == nil {
			continue
		}
		for _, tc := range a.ToolCalls {
			if tc.ID == nil {
				continue
			}
			name := ""
			if tc.Function.Name != nil {
				name = *tc.Function.Name
			}
			out[*tc.ID] = v3Call{name: name, path: callPath(tc.Function.Arguments), cmd: callCommand(tc.Function.Arguments)}
		}
	}
	return out
}

type v3Hit struct {
	idx          int
	toolID, rule string
	saves        int
	tokens       int
}

// v3Plan is what the rules and the protection say about one conversation.
type v3Plan struct {
	// protectedID holds the tool ids that are never dropped (protect_tools, protect_paths, protect_last_n).
	protectedID map[string]bool
	protected   map[int]bool // the same, by message index
	rules       []v3Hit
	ruleIdx     map[int]bool
}

func toolCallID(m bschemas.ChatMessage) string {
	if m.Role == bschemas.ChatMessageRoleTool && m.ChatToolMessage != nil && m.ChatToolMessage.ToolCallID != nil {
		return *m.ChatToolMessage.ToolCallID
	}
	return ""
}

// ruleTokens is the tokens the rule drops would remove, by rule, scaled (the content ratio, or 1).
func (p v3Plan) ruleTokens(scale float64) map[string]int {
	out := map[string]int{}
	for _, h := range p.rules {
		out[h.rule] += int(float64(h.saves) * scale)
	}
	return out
}

// v3Plan computes the protection and, unless the selector is judge, the rule drops, on req.
func (e *ExtractSweep) v3Plan(c *components.Ctx, req *bschemas.BifrostChatRequest) v3Plan {
	v3PlanBuilds.Add(1)
	p := v3Plan{protectedID: map[string]bool{}, protected: map[int]bool{}, ruleIdx: map[int]bool{}}
	tools := toolIndices(req)
	calls := v3Calls(req)
	protect := func(i int) {
		p.protected[i] = true
		if id := toolCallID(req.Input[i]); id != "" {
			p.protectedID[id] = true
		}
	}
	for k, i := range tools {
		if e.protectLastN > 0 && k >= len(tools)-e.protectLastN {
			protect(i)
			continue
		}
		call, ok := calls[toolCallID(req.Input[i])]
		if !ok {
			continue
		}
		if e.protectTools[strings.ToLower(call.name)] {
			protect(i)
			continue
		}
		if call.path != "" {
			for _, re := range e.protectPaths {
				if re.MatchString(call.path) {
					protect(i)
					break
				}
			}
		}
	}
	if e.selector == selectorJudge {
		return p
	}
	// Rule (b): the last message index of each build, test or lint command.
	lastRun := map[string]int{}
	for _, i := range tools {
		if call, ok := calls[toolCallID(req.Input[i])]; ok && call.cmd != "" && shellTool(call.name) && buildRun.MatchString(call.cmd) {
			lastRun[call.cmd] = i
		}
	}
	fe := newFileEvidence(req)
	// Rule (c): the recheck's own signal (coref over the agent's conversation), read once for the whole plan.
	var unref map[string]bool
	if e.ruleUnref {
		unref = unreferencedIDs(c, req, e.minTokens)
	}
	for k, i := range tools {
		if p.protected[i] {
			continue
		}
		id := toolCallID(req.Input[i])
		call := calls[id]
		rule := ""
		if path, ok := fe.read[i]; ok && fileSuperseded(fe, path, i) {
			rule = ruleFileReread
		} else if call.cmd != "" && shellTool(call.name) && buildRun.MatchString(call.cmd) && lastRun[call.cmd] > i {
			rule = ruleBuildRerun
		} else if unref[id] && len(tools)-1-k >= e.ruleUnrefMinAge {
			rule = ruleUnreferenced
		}
		if rule == "" || id == "" {
			continue
		}
		msg := &req.Input[i]
		if !schema.Rewritable(*msg) {
			continue
		}
		text := schema.MessageText(*msg)
		if text == "" || carriesRealMarker(c, text) {
			continue
		}
		tk := schema.TextTokens(text)
		if tk < e.minTokens {
			continue
		}
		if c != nil && c.Store != nil && (isKeptVerbatim(c, contentKey(text)) || restoredSpanGuard(c, text)) {
			continue // the agent expanded it
		}
		saves := tk - schema.TextTokens(sweepDescriptor(text))
		if saves <= 0 {
			continue
		}
		p.rules = append(p.rules, v3Hit{idx: i, toolID: id, rule: rule, saves: saves, tokens: tk})
		p.ruleIdx[i] = true
	}
	return p
}

// unreferencedIDs are the tool ids of outputs that introduced identifiers and that no later turn reused. It is
// the recheck's signal, from the same code: coref.IndexNovel over the agent's own conversation (RawInput when
// set), and a Record whose ConsumeLag is -1 (no reference at all). An output with no trackable identifier
// (Novel 0) is OPAQUE: no evidence either way, never a candidate.
func unreferencedIDs(c *components.Ctx, req *bschemas.BifrostChatRequest, minTokens int) map[string]bool {
	raw := req
	if c != nil && c.RawInput != nil {
		raw = &bschemas.BifrostChatRequest{Input: c.RawInput}
	}
	out := map[string]bool{}
	for _, r := range coref.IndexNovel(flattenForCoref(raw), minTokens, schema.TextTokens) {
		if r.ID != "" && r.Novel > 0 && r.ConsumeLag < 0 {
			out[r.ID] = true
		}
	}
	return out
}

// fileSuperseded: the file read at message i was read again, or edited, later.
func fileSuperseded(fe *fileEvidence, path string, i int) bool {
	for _, j := range fe.edits[path] {
		if j > i {
			return true
		}
	}
	for _, j := range fe.reads[path] {
		if j > i {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------------------------------
// The held source for a v3 apply: the rules' drops, with or without the judge's waiting answer.

type v3Held struct {
	mem    memHeld
	useMem bool
	rules  []heldOutput
	base   heldAnswer
	// ruleOf maps a tool id to the rule that chose it.
	ruleOf    map[string]string
	protected map[string]bool
	claimed   bool
	// claimedIDs are the drops the claim handed out, for the cold cut's split.
	claimedIDs []string
	// onClaim runs once, when a claim hands out an answer and before the apply changes a message.
	onClaim func()
}

// newV3Held builds the source. raw is the agent's own conversation: the hash and the prints are measured on
// it, as the recheck does.
func (e *ExtractSweep) newV3Held(c *components.Ctx, tkey string, plan v3Plan, raw []bschemas.ChatMessage, useMem bool) *v3Held {
	v := &v3Held{mem: memHeld{tkey}, useMem: useMem, ruleOf: map[string]string{}, protected: plan.protectedID}
	v.base = heldAnswer{V: heldVersion, Thread: tkey, AskedAtMs: time.Now().UnixMilli(), Model: c.ModelName,
		Judged: len(raw), Prints: msgPrints(raw)}
	for _, h := range plan.rules {
		ri, ok := rawToolMessage(raw, h.toolID)
		if !ok {
			continue
		}
		v.ruleOf[h.toolID] = h.rule
		v.rules = append(v.rules, heldOutput{ToolUseID: h.toolID, Hash: outputHash(raw[ri]),
			Tokens: h.tokens, Saves: h.saves, Drop: true})
	}
	return v
}

// combine puts the rules' drops into the judge's answer. The judge's coordinates stay, so the recheck's
// referenced_later test still measures what happened after the question was asked.
func (v *v3Held) combine(ans heldAnswer, have bool) (heldAnswer, bool) {
	if !have {
		if len(v.rules) == 0 {
			return heldAnswer{}, false
		}
		h := v.base
		h.Outputs = append([]heldOutput(nil), v.rules...)
		return h, true
	}
	seen := map[string]bool{}
	var outs []heldOutput
	for _, o := range v.rules {
		seen[o.ToolUseID] = true
		outs = append(outs, o) // a rule is a decision made now: it wins over an older verdict
	}
	for _, o := range ans.Outputs {
		if seen[o.ToolUseID] || v.protected[o.ToolUseID] {
			continue
		}
		outs = append(outs, o)
	}
	ans.Outputs = outs
	return ans, true
}

// ruleFor names what chose a drop, for the drop record: the rule, or the judge.
func (v *v3Held) ruleFor(id string) string {
	if r, ok := v.ruleOf[id]; ok {
		return r
	}
	return sourceJudge
}

func (v *v3Held) get(string) (heldAnswer, bool) {
	if v.claimed {
		return heldAnswer{}, false
	}
	var ans heldAnswer
	have := false
	if v.useMem {
		ans, have = v.mem.get("")
	}
	return v.combine(ans, have)
}

func (v *v3Held) claim(string) (heldAnswer, bool) {
	if v.claimed {
		return heldAnswer{}, false
	}
	var ans heldAnswer
	have := false
	if v.useMem {
		ans, have = v.mem.claim("")
	}
	h, ok := v.combine(ans, have)
	if ok {
		v.claimed = true
		if v.onClaim != nil {
			v.onClaim()
		}
		for _, o := range h.Outputs {
			if o.Drop {
				v.claimedIDs = append(v.claimedIDs, o.ToolUseID)
			}
		}
	}
	return h, ok
}

// put gives the judge's part back (a panicked apply). The rules are decided again on the next request.
func (v *v3Held) put(h heldAnswer) {
	if !v.useMem {
		return
	}
	var keep []heldOutput
	for _, o := range h.Outputs {
		if _, isRule := v.ruleOf[o.ToolUseID]; !isRule {
			keep = append(keep, o)
		}
	}
	h.Outputs = keep
	if len(keep) > 0 {
		v.mem.put(h)
	}
}

// ---------------------------------------------------------------------------------------------------
// The step.

// v3Dispatch is the warm step when sweep v3 is on. A cold request applies a waiting answer as before, but
// never a protected output. A warm request goes through the v3 gates.
func (e *ExtractSweep) v3Dispatch(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx,
	tkey string, in v3In) (keys []string, changed int) {
	if !in.cold {
		return e.v3Step(req, rep, c, tkey, in)
	}
	if !in.hasHeld || in.heldTried {
		return nil, 0
	}
	raw := req.Input
	if c.RawInput != nil {
		raw = c.RawInput
	}
	plan := e.v3Plan(c, req)
	plan.rules = nil
	src := e.newV3Held(c, tkey, plan, raw, true)
	// The answer is used now, applied or not. It is no longer "the judge gave no answer".
	warmMu.Lock()
	if t := warmFor(tkey); t != nil {
		t.v3Asked = false
	}
	warmMu.Unlock()
	// The marked set is read when the claim wins, before the apply changes any message, and not before.
	var before map[string]bool
	src.onClaim = func() { before = v3MarkedIDs(c, req) }
	keys, changed = e.applyHeldFrom(src, &warmApply{}, req, rep, c, tkey)
	if changed > 0 {
		e.v3ColdCut(c, req, tkey, src.claimedIDs, before, src.ruleOf)
	}
	return keys, changed
}

type v3In struct {
	cold                               bool
	run, count                         int
	growthFrom, growthTurns, prevLocal int
	busy, hasHeld, heldTried           bool
}

// v3Step is the warm step when sweep v3 is on. It runs the gates, selects, applies and triggers.
func (e *ExtractSweep) v3Step(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx,
	tkey string, in v3In) (keys []string, changed int) {
	P := c.PrevBilledInputThread
	C, cSource := e.v3CutPoint(c)

	var asked, waiting bool
	warmMu.Lock()
	t := warmFor(tkey)
	if t == nil {
		warmMu.Unlock()
		return nil, 0
	}
	// The first size seen after an accepted cut is the base the growth is measured from.
	if t.v3Await && P > 0 {
		t.v3Baseline, t.v3Await = P, false
	}
	asked = t.v3Asked
	// The "not yet" refusals are counted once per warm_every_n warm requests, so they read as reasons and not
	// as one per request. Its own counter: the question's pace counter (count) resets only when a question is asked.
	t.v3Reqs++
	paced := t.v3Reqs%e.warmEveryN == 0
	if t.v3Baseline > 0 && C > 0 {
		if P < t.v3Baseline { // the prompt shrank (a compaction, a rewind): a new round
			t.v3Baseline = 0
		} else if float64(P-t.v3Baseline) < e.minDropFrac*float64(C) {
			waiting = true
		}
	}
	warmMu.Unlock()
	if warmTriggerHook != nil {
		warmTriggerHook()
	}
	// The first request of a thread has no billed size yet: nothing to count. The two "not yet" refusals are
	// counted at the pace of the question (warm_every_n), so they read as reasons and not as one per request.
	if P <= 0 {
		return nil, 0
	}
	if C <= 0 {
		if paced {
			noteV3Refuse("cut_point_unknown")
		}
		return nil, 0
	}
	if paced {
		noteV3CSource(cSource)
	}
	if float64(P) < v3Fill(C, e.fillSmall, e.fillLarge)*float64(C) {
		if paced {
			noteV3Refuse("not_in_sweet_spot")
			noteV3Refusal(tkey, "not_in_sweet_spot", P, C, cSource)
		}
		return nil, 0
	}
	if waiting {
		noteV3Refuse("waiting_for_growth")
		return nil, 0
	}

	raw := req.Input
	if c.RawInput != nil {
		raw = c.RawInput
	}
	plan := e.v3Plan(c, req)
	apply := func(useMem bool) ([]string, int) {
		src := e.newV3Held(c, tkey, plan, raw, useMem)
		return e.v3Apply(src, req, rep, c, tkey, in, P, C, cSource)
	}
	resetCount := func() {
		warmMu.Lock()
		if t := warmFor(tkey); t != nil {
			t.count = 0
		}
		warmMu.Unlock()
	}

	// RULES ONLY: no question, no model call.
	if e.selector == selectorRule {
		if len(plan.rules) == 0 {
			if paced {
				noteV3Refuse("no_rule_drops")
				noteV3SizeRefusal(tkey, "rule_select", "no_rule_drops", P, C, cSource, 0, nil)
			}
			return nil, 0
		}
		if content, _, ok := warmRatios(c.ModelName, c.PrevBilledInput, in.prevLocal); ok {
			sum := 0
			for _, h := range plan.rules {
				sum += h.saves
			}
			if float64(sum)*content < e.minDropFrac*float64(P) {
				if paced {
					noteV3Refuse("drop_too_small")
					noteV3SizeRefusal(tkey, "rule_select", "drop_too_small", P, C, cSource, int(float64(sum)*content), plan.ruleTokens(content))
				}
				return nil, 0
			}
		}
		return apply(false)
	}
	// A waiting answer, not yet tried: apply it (with the rules, in hybrid).
	if in.hasHeld && !in.heldTried {
		resetCount()
		return apply(true)
	}
	// Hybrid: the question ended without an answer. The rules go ahead alone.
	if e.selector == selectorHybrid && !in.hasHeld && !in.busy && asked {
		warmMu.Lock()
		if t := warmFor(tkey); t != nil {
			t.v3Asked = false
		}
		warmMu.Unlock()
		noteV3HybridRulesOnly()
		return apply(false)
	}
	// The judge is asked, at the sweet spot, always. Only the pace and the billed size limit it.
	if in.busy || in.count < e.warmEveryN {
		return nil, 0
	}
	resetCount()
	if _, _, ok := warmRatios(c.ModelName, c.PrevBilledInput, in.prevLocal); !ok {
		sweepWarmNoBilled.Add(1)
		noteWarmTrigger("no_billed_size")
		return nil, 0
	}
	e.registerWarm(c, tkey)
	if e.selector == selectorHybrid {
		pending := false
		warmMu.Lock()
		if t := warmFor(tkey); t != nil && t.warmPending() {
			t.v3Asked, pending = true, true
		}
		warmMu.Unlock()
		if !pending {
			// No question could be registered (the keep-alive cannot ping this thread): the rules go alone,
			// through the same gates.
			noteV3HybridRulesOnly()
			return apply(false)
		}
	}
	return nil, 0
}

// v3CutRec is what one accepted cut leaves in the ledger.
type v3CutRec struct {
	p, c, d          int
	cSource          string
	dShareP, pShareC float64
	savingCompaction float64
	rules            map[string]int
	judge            int
	set              bool
}

func (e *ExtractSweep) v3Apply(src *v3Held, req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx,
	tkey string, in v3In, P, C int, cSource string) (keys []string, changed int) {
	wa := &warmApply{}
	inner := e.warmGate(c, req, in.run, in.growthFrom, in.growthTurns, in.prevLocal, wa)
	var cut v3CutRec
	wa.gate = e.v3Gate(inner, wa, c, tkey, in.prevLocal, P, C, cSource, src, &cut)
	if src.useMem {
		// An answer is being used (applied or refused). It is no longer "the judge gave no answer".
		warmMu.Lock()
		if t := warmFor(tkey); t != nil {
			t.v3Asked = false
		}
		warmMu.Unlock()
	}
	wk, wn := e.applyHeldFrom(src, wa, req, rep, c, tkey)
	if wn > 0 {
		if !cut.set {
			// A changed-prefix apply: the gates did not run (the provider rewrites from the change anyway), but
			// the cut is a cut. It is counted, and it starts the growth wait.
			cut = v3CutRec{p: P, c: C, cSource: cSource, pShareC: float64(P) / float64(C), judge: wn, set: true}
		}
		e.v3CutDone(tkey, cut)
	}
	return wk, wn
}

// v3CutDone books an applied cut and starts the growth wait.
func (e *ExtractSweep) v3CutDone(tkey string, cut v3CutRec) {
	warmMu.Lock()
	if t := warmFor(tkey); t != nil {
		t.v3Await, t.v3Baseline, t.v3Asked = true, 0, false
	}
	warmMu.Unlock()
	noteV3Cut(tkey, cut, e.selector)
}

// v3Gate is gate 3 (a meaningful drop) in front of gate 4 (payback, #720's gate).
func (e *ExtractSweep) v3Gate(inner func(recheckResult, []bschemas.ChatMessage) (bool, string), wa *warmApply,
	c *components.Ctx, tkey string, prevLocal, P, C int, cSource string, src *v3Held, cut *v3CutRec) func(recheckResult, []bschemas.ChatMessage) (bool, string) {
	return func(res recheckResult, raw []bschemas.ChatMessage) (bool, string) {
		var dBilled int
		measured := false
		if len(res.Valid) > 0 {
			if content, _, ok := warmRatios(c.ModelName, c.PrevBilledInput, prevLocal); ok {
				dBilled = int(float64(res.validTokens()) * content)
				measured = true
				// D by what chose it, for EVERY decision line of this gate, accepted or refused.
				wa.ruleD = map[string]int{}
				for _, o := range res.Valid {
					wa.ruleD[src.ruleFor(o.ToolUseID)] += int(float64(o.Saves) * content)
				}
			}
		}
		if measured {
			wa.dShareP, wa.pShareC = float64(dBilled)/float64(P), float64(P)/float64(C)
			wa.cutPoint, wa.cSource = C, cSource
			if float64(dBilled) < e.minDropFrac*float64(P) {
				noteV3Refuse("drop_too_small")
				ruleD := wa.ruleD
				noteWarmDecision(tkey, WarmDecision{Stage: "apply", Accept: false, Reason: "drop_too_small", Weight: 1,
					RemovedLocal: res.validTokens(), RemovedBilled: dBilled, Proposed: res.proposed, Dropped: res.reasonCounts(),
					DShareP: wa.dShareP, PShareC: wa.pShareC, CutPoint: C, CSource: cSource, RuleD: ruleD})
				return false, "drop_too_small"
			}
		}
		ok, reason := inner(res, raw)
		if !ok {
			if strings.HasPrefix(reason, "does_not_pay") {
				noteV3Refuse("does_not_pay")
			}
			return false, reason
		}
		*cut = v3CutRec{p: P, c: C, cSource: cSource, savingCompaction: wa.savingCompaction, d: dBilled, dShareP: wa.dShareP, pShareC: wa.pShareC, rules: map[string]int{}, set: true}
		for _, o := range res.Valid {
			if r, isRule := src.ruleOf[o.ToolUseID]; isRule {
				cut.rules[r]++
			} else {
				cut.judge++
			}
		}
		return true, ""
	}
}

// ---------------------------------------------------------------------------------------------------
// The ledger.

// V3Cut is one accepted v3 cut, in numbers. The thread is a hash label.
type V3Cut struct {
	Thread   string `json:"thread"`
	AtMs     int64  `json:"at_ms"`
	Selector string `json:"selector"`
	P        int    `json:"billed_prompt"`
	C        int    `json:"cut_point"`
	CSource  string `json:"c_source"`
	// SavingCompactionUSD is the credit for the compaction this cut avoids (0 when it avoids none).
	SavingCompactionUSD float64 `json:"saving_compaction_usd,omitempty"`
	D                   int     `json:"dropped_billed"`
	DShareP             float64 `json:"d_share_of_p"`
	PShareC             float64 `json:"p_share_of_c"`
	RuleDrops           int     `json:"rule_drops"`
	JudgeDrops          int     `json:"judge_drops"`
}

// V3Stats is the sweep v3 ledger for /stats.
type V3Stats struct {
	// Refused counts why a cut attempt was refused: not_in_sweet_spot, drop_too_small, waiting_for_growth,
	// does_not_pay (and cut_point_unknown, when C or P is not known).
	Refused map[string]int64 `json:"refused,omitempty"`
	// RuleDrops counts the drops of accepted cuts that a rule chose, by rule. JudgeDrops counts the model's.
	RuleDrops  map[string]int64 `json:"rule_drops,omitempty"`
	JudgeDrops int64            `json:"judge_drops,omitempty"`
	// HybridRulesOnly counts hybrid attempts that went ahead on the rules alone: the judge gave no answer.
	HybridRulesOnly int64 `json:"hybrid_rules_only,omitempty"`
	Cuts            int64 `json:"cuts"`
	// CompactionsAvoided counts accepted cuts that avoid a compaction within the horizon, and SavingCompactionUSD
	// is the credit they carried. They are what the gate counted, not a measurement.
	CompactionsAvoided  int64   `json:"compactions_avoided,omitempty"`
	SavingCompactionUSD float64 `json:"saving_compaction_usd,omitempty"`
	// CSource counts the sweet-spot checks by where C came from: cutfrac (a summarizer's fraction of the effective
	// context), compaction (the compaction point: operator, measured or assumed) or window (the model's window).
	CSource map[string]int64 `json:"c_source,omitempty"`
	Recent  []V3Cut          `json:"recent_cuts,omitempty"`
	// Expands counts the first expands of v3 drops by what chose them: a rule name, or judge. With rule_drops and
	// judge_drops it gives the expand rate per rule and for the judge.
	Expands map[string]int64 `json:"expands,omitempty"`
	// RecentRefusals lists the last cut attempts a size gate refused (drop_too_small, no_rule_drops), with D, D/P,
	// C, where C came from, and D by rule.
	RecentRefusals []V3Refusal `json:"recent_refusals,omitempty"`
}

// V3Refusal is one refused cut attempt, in numbers. The thread is a hash label.
type V3Refusal struct {
	Thread  string         `json:"thread"`
	AtMs    int64          `json:"at_ms"`
	Reason  string         `json:"reason"`
	P       int            `json:"billed_prompt"`
	C       int            `json:"cut_point"`
	CSource string         `json:"c_source"`
	D       int            `json:"dropped_billed"`
	DShareP float64        `json:"d_share_of_p"`
	RuleD   map[string]int `json:"rule_d,omitempty"`
}

// ruleDString renders D by rule as "rule:tokens,rule:tokens", sorted, for the log line.
func ruleDString(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+":"+strconv.Itoa(m[k]))
	}
	return strings.Join(parts, ",")
}

// noteV3Expand counts the first expand of a v3 drop by what chose it.
func noteV3Expand(rule string) {
	v3Mu.Lock()
	if v3Ledger.Expands == nil {
		v3Ledger.Expands = map[string]int64{}
	}
	v3Ledger.Expands[rule]++
	v3Mu.Unlock()
}

// Any reports whether v3 did or counted anything.
func (s V3Stats) Any() bool {
	return len(s.Refused) > 0 || len(s.Expands) > 0 || len(s.RecentRefusals) > 0 || len(s.CSource) > 0 || len(s.RuleDrops) > 0 || s.JudgeDrops > 0 || s.CompactionsAvoided > 0 || s.HybridRulesOnly > 0 || s.Cuts > 0 || len(s.Recent) > 0
}

var (
	v3Mu     sync.Mutex
	v3Ledger V3Stats
)

func noteV3Refuse(reason string) {
	v3Mu.Lock()
	if v3Ledger.Refused == nil {
		v3Ledger.Refused = map[string]int64{}
	}
	v3Ledger.Refused[reason]++
	v3Mu.Unlock()
}

func noteV3HybridRulesOnly() {
	v3Mu.Lock()
	v3Ledger.HybridRulesOnly++
	v3Mu.Unlock()
}

// noteV3CSource counts where C came from, once per warm_every_n warm requests, like the refusal counters.
func noteV3CSource(source string) {
	v3Mu.Lock()
	if v3Ledger.CSource == nil {
		v3Ledger.CSource = map[string]int64{}
	}
	v3Ledger.CSource[source]++
	v3Mu.Unlock()
}

// noteV3Refusal puts a "not in the sweet spot" refusal in the decision log with the numbers, so P, C and where
// C came from can be read. It runs at the pace of the question, like the counter.
func noteV3Refusal(tkey, reason string, p, c int, cSource string) {
	noteV3SizeRefusal(tkey, "sweet_spot", reason, p, c, cSource, 0, nil)
}

// noteV3SizeRefusal logs one refused cut attempt as a decision line, and keeps it in recent_refusals: D, D/P,
// P/C, C and where it came from, and D by rule.
func noteV3SizeRefusal(tkey, stage, reason string, p, c int, cSource string, d int, ruleD map[string]int) {
	dShare, pShare := 0.0, 0.0
	if p > 0 {
		dShare = float64(d) / float64(p)
	}
	if c > 0 {
		pShare = float64(p) / float64(c)
	}
	noteWarmDecision(tkey, WarmDecision{Stage: stage, Accept: false, Reason: reason, Weight: 1,
		RemovedBilled: d, DShareP: dShare, PShareC: pShare, CutPoint: c, CSource: cSource, RuleD: ruleD})
	v3Mu.Lock()
	v3Ledger.RecentRefusals = append(v3Ledger.RecentRefusals, V3Refusal{Thread: threadLabel(tkey), AtMs: time.Now().UnixMilli(),
		Reason: reason, P: p, C: c, CSource: cSource, D: d, DShareP: dShare, RuleD: ruleD})
	if len(v3Ledger.RecentRefusals) > maxV3Recent {
		v3Ledger.RecentRefusals = v3Ledger.RecentRefusals[len(v3Ledger.RecentRefusals)-maxV3Recent:]
	}
	v3Mu.Unlock()
}

func noteV3Cut(tkey string, r v3CutRec, selector string) {
	rd := 0
	v3Mu.Lock()
	defer v3Mu.Unlock()
	if v3Ledger.RuleDrops == nil {
		v3Ledger.RuleDrops = map[string]int64{}
	}
	for name, n := range r.rules {
		v3Ledger.RuleDrops[name] += int64(n)
		rd += n
	}
	v3Ledger.JudgeDrops += int64(r.judge)
	v3Ledger.Cuts++
	if r.savingCompaction > 0 {
		v3Ledger.CompactionsAvoided++
		v3Ledger.SavingCompactionUSD += r.savingCompaction
	}
	v3Ledger.Recent = append(v3Ledger.Recent, V3Cut{Thread: threadLabel(tkey), AtMs: time.Now().UnixMilli(), Selector: selector,
		P: r.p, C: r.c, CSource: r.cSource, SavingCompactionUSD: r.savingCompaction, D: r.d, DShareP: r.dShareP, PShareC: r.pShareC, RuleDrops: rd, JudgeDrops: r.judge})
	if len(v3Ledger.Recent) > maxV3Recent {
		v3Ledger.Recent = v3Ledger.Recent[len(v3Ledger.Recent)-maxV3Recent:]
	}
}

func v3Counters() V3Stats {
	v3Mu.Lock()
	defer v3Mu.Unlock()
	out := V3Stats{CompactionsAvoided: v3Ledger.CompactionsAvoided, SavingCompactionUSD: v3Ledger.SavingCompactionUSD, JudgeDrops: v3Ledger.JudgeDrops, HybridRulesOnly: v3Ledger.HybridRulesOnly, Cuts: v3Ledger.Cuts, Recent: append([]V3Cut(nil), v3Ledger.Recent...)}
	if len(v3Ledger.Refused) > 0 {
		out.Refused = map[string]int64{}
		for k, v := range v3Ledger.Refused {
			out.Refused[k] = v
		}
	}
	out.RecentRefusals = append([]V3Refusal(nil), v3Ledger.RecentRefusals...)
	if len(v3Ledger.Expands) > 0 {
		out.Expands = map[string]int64{}
		for k, n := range v3Ledger.Expands {
			out.Expands[k] = n
		}
	}
	if len(v3Ledger.CSource) > 0 {
		out.CSource = map[string]int64{}
		for k, n := range v3Ledger.CSource {
			out.CSource[k] = n
		}
	}
	if len(v3Ledger.RuleDrops) > 0 {
		out.RuleDrops = map[string]int64{}
		for k, v := range v3Ledger.RuleDrops {
			out.RuleDrops[k] = v
		}
	}
	return out
}

func resetV3ForTest() {
	v3Mu.Lock()
	v3Ledger = V3Stats{}
	v3Mu.Unlock()
}

// v3Filtered is a held source that never hands out a protected output. The cold apply reads the Store with
// it when v3 is on: an answer stored before a config change, or by a ping question, may name an output that
// is protected now. Defense in depth; the question itself does not offer protected outputs either.
//
// The protected set is built lazily, on the first answer that has an output to check. A cold request with no
// stored answer builds no plan.
type v3Filtered struct {
	heldSource
	plan    func() map[string]bool
	claimed *[]string
	// onClaim runs when a claim hands out an answer, before the apply changes a message.
	onClaim func()
}

func (f v3Filtered) strip(h heldAnswer, ok, claim bool) (heldAnswer, bool) {
	if !ok || len(h.Outputs) == 0 {
		return h, ok
	}
	protected := f.plan()
	if claim && f.onClaim != nil {
		f.onClaim()
	}
	kept := make([]heldOutput, 0, len(h.Outputs))
	for _, o := range h.Outputs {
		if protected[o.ToolUseID] {
			if claim {
				noteV3Refuse("cold_protected_filtered")
			}
			continue
		}
		kept = append(kept, o)
		if claim && o.Drop && f.claimed != nil {
			*f.claimed = append(*f.claimed, o.ToolUseID)
		}
	}
	h.Outputs = kept
	return h, true
}

// ruleFor: an answer in the Store was made by the judge.
func (f v3Filtered) ruleFor(string) string { return sourceJudge }

func (f v3Filtered) get(k string) (heldAnswer, bool) {
	h, ok := f.heldSource.get(k)
	return f.strip(h, ok, false)
}
func (f v3Filtered) claim(k string) (heldAnswer, bool) {
	h, ok := f.heldSource.claim(k)
	return f.strip(h, ok, true)
}

// v3ColdCut books a cold or changed-prefix apply as a cut and starts the growth wait. The applied drops are
// split into rule drops and judge drops through ruleOf, as the warm gate does: an id counts as applied when it
// was a claimed drop and its message carries a marker now that it did not carry before.
//
// ruleOf is EMPTY on every production path today: a cold answer never carries rule verdicts (the rules decide
// at the warm request, from the request in hand), so every cold drop is a judge drop. The split stays so a
// future path that does hand rule verdicts to a cold apply is counted right.
func (e *ExtractSweep) v3ColdCut(c *components.Ctx, req *bschemas.BifrostChatRequest, tkey string, claimed []string,
	before map[string]bool, ruleOf map[string]string) {
	cut := v3CutRec{p: c.PrevBilledInputThread, rules: map[string]int{}, set: true}
	for _, id := range claimed {
		if before[id] {
			continue
		}
		if i, ok := rawToolMessage(req.Input, id); !ok || !carriesRealMarker(c, schema.MessageText(req.Input[i])) {
			continue
		}
		if r, isRule := ruleOf[id]; isRule {
			cut.rules[r]++
		} else {
			cut.judge++
		}
	}
	if C, src := e.v3CutPoint(c); C > 0 {
		cut.c, cut.pShareC, cut.cSource = C, float64(cut.p)/float64(C), src
	}
	e.v3CutDone(tkey, cut)
}

// v3MarkedIDs is the set of tool ids whose message carries a marker already.
func v3MarkedIDs(c *components.Ctx, req *bschemas.BifrostChatRequest) map[string]bool {
	out := map[string]bool{}
	for _, i := range toolIndices(req) {
		if carriesRealMarker(c, schema.MessageText(req.Input[i])) {
			out[toolCallID(req.Input[i])] = true
		}
	}
	return out
}
