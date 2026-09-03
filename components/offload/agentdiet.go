package offload

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

func init() { components.Register("agentdiet", newAgentDiet) }

// AgentDiet is a baseline reproduction of the trajectory-reduction method from
// "Reducing Cost of LLM Agents with Trajectory Reduction" (Xiao, Gao, Peng, Xiong;
// FSE 2026, arXiv:2509.23586), which the paper calls AgentDiet. It is offered as a
// COMPARABLE BASELINE next to context-guru's own reducers, so the published method
// can be A/B'd against `extract_llm` / `summarize` on the same traffic.
//
// The method, and what makes it different from extract_llm:
//
//  1. Its unit is the STEP — one assistant message plus the tool results answering
//     it — not one tool output. A step is reduced as a whole.
//  2. A FIXED AGE, not a size or an economic estimate, chooses the target: when the
//     agent has completed step s, only step s-a is eligible (`delay_steps`, a=2).
//     The most recent a steps are never touched, so a bad reduction can never
//     corrupt what the agent is working on right now — the paper's protection
//     against a malfunctioning reflection model.
//  3. The reflection model gets a SLIDING WINDOW of surrounding steps as context
//     (`context_steps`, b=1 ⇒ steps [s-a-b … s]), serialized as XML. This is the
//     part extract_llm structurally cannot do: seeing neighbouring steps is what
//     lets the model recognise content as *redundant* (already stated nearby) or
//     *expired* (mattered only to a finished sub-goal) rather than merely verbose.
//  4. Two thresholds bound the spend. A step shorter than `min_step_tokens`
//     (θ=500) is not worth a call at all; and a reduction that comes back is
//     APPLIED only if it clears `min_saved_tokens` OR `max_keep_ratio`, so a
//     marginal rewrite does not pay a cache-write for nothing.
//  5. A reduction is made ONCE and then frozen: later turns replay the same bytes
//     (freeze/reapplyFrozen), so the request prefix stays stable and reductions
//     accumulate across the session exactly as they do in the paper, where the
//     agent's own trajectory is edited in place.
//
// Deviations from the paper, all forced by the wire and documented in
// docs/components/agentdiet.md:
//
//   - The paper's reflection module REPLACES the whole step with one assistant
//     message. context-guru rewrites messages in place (only `summarize` changes
//     the message count, and it must run alone for that reason), so the reduction
//     is written into the step's tool-result messages — where 63% of trajectory
//     tokens live, by the paper's own accounting (30.4K of 48.4K).
//   - Tool-call ARGUMENTS are not reduced. On Anthropic traffic bifrost's schema
//     does not model `tool_use` blocks at all, so their name and input are not
//     visible to any component here; the assistant message is not even Rewritable.
//     That forgoes the paper's str_replace_editor redundancy case (~25% of
//     trajectory tokens) and is the main reason to expect a smaller reduction here
//     than the paper's 39.9%–59.7%.
//   - The reflection prompt is written from the paper's description (its four
//     parts: job, format, the three waste categories, anti-loss guidelines) rather
//     than copied from the authors' artifact.
//
// Defaults are the paper's tuned values (a=2, b=1, θ=500). `min_saved_tokens` (400)
// and `max_keep_ratio` (0.8) come from the authors' artifact, whose apply-gate is
// `saved >= 400 || keep < 0.8` — Algorithm 1 in the paper states this more simply as
// `l_orig - l_reduced > θ`. The artifact's form is used because it is what produced
// the published numbers; setting `min_saved_tokens` to θ and `max_keep_ratio` to 0
// reproduces the paper's stated gate exactly.
type AgentDiet struct {
	delay        int     // a: steps of protection before a step becomes eligible
	ctxBefore    int     // b: steps of leading context handed to the model
	minStep      int     // θ: a step below this many tokens is not worth a call
	minSaved     int     // apply if the reduction saves at least this many tokens…
	maxKeepRatio float64 // …or if it keeps less than this fraction of the step
	modelSource  string
	modelClient  components.Model
	mode         markerMode
	tailOnly     bool
}

// agentDietConfig is the `agentdiet:` block.
type agentDietConfig struct {
	// DelaySteps is a (default 2). 0 is accepted, for an ablation, but it targets the
	// step the agent has just completed and so gives up the paper's protection against
	// a malfunctioning reflection model — the claim that the most recent a steps are
	// never touched holds only for a >= 1.
	DelaySteps     *int        `yaml:"delay_steps"`
	ContextSteps   *int        `yaml:"context_steps"`    // b (default 1)
	MinStepTokens  *int        `yaml:"min_step_tokens"`  // θ (default 500)
	MinSavedTokens *int        `yaml:"min_saved_tokens"` // default 400
	MaxKeepRatio   *float64    `yaml:"max_keep_ratio"`   // default 0.8
	Model          modelConfig `yaml:"model"`
	MarkerMode     string      `yaml:"marker_mode"` // full (default) | summary | off
	// CacheTailOnly restricts NEW reductions to the uncached tail. Default FALSE,
	// which is the faithful setting and unusual for this repo: the target step is
	// chosen by age, so with a>=1 it is ALWAYS inside the provider's cached prefix,
	// and a tail restriction would make the component a silent no-op on caching
	// backends (the failure mode #28 left extract_llm with). The paper accepts one
	// cache-write of the suffix per reduced step and counts it in its cost figures;
	// because the decision is then frozen and replayed byte-identically, that write
	// happens once per step, not once per turn. Set true only if you would rather
	// keep the cache pristine and reduce nothing.
	CacheTailOnly *bool `yaml:"cache_tail_only"`
}

func newAgentDiet(raw []byte) (components.Component, error) {
	cfg := agentDietConfig{}
	if err := components.Decode(raw, &cfg); err != nil {
		return nil, err
	}
	d := &AgentDiet{
		delay: 2, ctxBefore: 1, minStep: 500, minSaved: 400, maxKeepRatio: 0.8,
		modelSource: cfg.Model.Source, modelClient: cfg.Model.Client(),
		mode: parseMarkerMode(cfg.MarkerMode),
	}
	if cfg.DelaySteps != nil && *cfg.DelaySteps >= 0 {
		d.delay = *cfg.DelaySteps
	}
	if cfg.ContextSteps != nil && *cfg.ContextSteps >= 0 {
		d.ctxBefore = *cfg.ContextSteps
	}
	if cfg.MinStepTokens != nil && *cfg.MinStepTokens >= 0 {
		d.minStep = *cfg.MinStepTokens
	}
	if cfg.MinSavedTokens != nil && *cfg.MinSavedTokens >= 0 {
		d.minSaved = *cfg.MinSavedTokens
	}
	if cfg.MaxKeepRatio != nil && *cfg.MaxKeepRatio >= 0 {
		d.maxKeepRatio = *cfg.MaxKeepRatio
	}
	if cfg.CacheTailOnly != nil {
		d.tailOnly = *cfg.CacheTailOnly
	}
	return d, nil
}

func (*AgentDiet) Name() string                 { return "agentdiet" }
func (*AgentDiet) Enabled(*components.Ctx) bool { return true }

// NeedsModel reports that this component calls an LLM. With no model available it
// degrades to replaying whatever it already froze — never an error.
func (*AgentDiet) NeedsModel() bool { return true }

// defaultAgentDietTimeout bounds ONE reflection call. It matches extract_llm's
// default for the same reason (a loaded self-hosted server queues for tens of
// seconds before it starts generating), but is a separate knob because the prompts
// differ in size by about the window multiple: extract_llm sends one tool output,
// this sends b+1+a serialized steps.
const defaultAgentDietTimeout = 90 * time.Second

var agentDietTimeout = resolveTimeoutEnv("CONTEXT_GURU_AGENTDIET_TIMEOUT", defaultAgentDietTimeout)

// Fail-open counters. Abandoning the call is correct — compaction must never break
// the agent's request — but it must not be silent, or an arm that quietly stopped
// reducing reads as an arm that got faster.
var (
	agentDietTimeouts int64
	agentDietErrors   int64
)

// AgentDietTimeouts returns the reflection calls abandoned on the per-call deadline.
// Non-zero means CONTEXT_GURU_AGENTDIET_TIMEOUT is too small for the server's load,
// and this arm's savings are an undercount rather than a measurement.
func AgentDietTimeouts() int64 { return atomic.LoadInt64(&agentDietTimeouts) }

// AgentDietErrors returns non-timeout reflection failures (transport, HTTP status,
// unparseable reply, cancelled parent request).
func AgentDietErrors() int64 { return atomic.LoadInt64(&agentDietErrors) }

// AgentDietCallTimeout exposes the resolved per-call budget, so /stats can report it
// beside the counters (a timeout count means nothing without the budget it hit).
func AgentDietCallTimeout() time.Duration { return agentDietTimeout }

// step is one turn of the agent loop: the assistant message that issued the tool
// call(s), and the tool-result messages that answered it.
type step struct {
	assistant int   // req.Input index of the assistant message; -1 if not visible
	tools     []int // req.Input indices of the tool results answering it
}

// splitSteps groups a request into COMPLETED agent steps, in order. A step opens on
// an assistant message and collects the tool results that follow; a user or system
// message closes it (a new human turn).
//
// Only steps that actually carry a tool result are returned. An assistant message
// still awaiting its result is the in-flight turn, and counting it would slide the
// age window by one on alternating requests — which would move the target step and
// break the reduce-once-then-freeze property this component depends on.
func splitSteps(req *bschemas.BifrostChatRequest) []step {
	var out []step
	cur := step{assistant: -1}
	open := false
	flush := func() {
		if open && len(cur.tools) > 0 {
			out = append(out, cur)
		}
		cur, open = step{assistant: -1}, false
	}
	for i := range req.Input {
		switch req.Input[i].Role {
		case bschemas.ChatMessageRoleAssistant:
			flush()
			cur, open = step{assistant: i}, true
		case bschemas.ChatMessageRoleTool:
			if !open { // a tool result whose call we cannot see (dialect quirk)
				cur, open = step{assistant: -1}, true
			}
			cur.tools = append(cur.tools, i)
		default:
			flush()
		}
	}
	flush()
	return out
}

// serializeStep renders one step in the XML shape the reflection prompt describes:
//
//	<step id="N">
//	<think>…assistant reasoning…</think>
//	<call tool="bash">{"command":"…"}</call>
//	<result id="0">…tool output…</result>
//	</step>
//
// `<result>` carries an id so a reduced reply can be mapped back to the right
// message when a step made parallel tool calls. `<call>` covers both dialects:
// OpenAI-shaped traffic exposes tool calls in bifrost's schema directly, and the
// host's normalize step lifts Anthropic `tool_use` blocks into the same field
// (apply.attachToolUse), since bifrost does not model them.
func serializeStep(req *bschemas.BifrostChatRequest, id int, s step) string {
	var b strings.Builder
	b.WriteString(`<step id="`)
	b.WriteString(strconv.Itoa(id))
	b.WriteString("\">\n")
	if s.assistant >= 0 {
		m := req.Input[s.assistant]
		if txt := strings.TrimSpace(schema.MessageText(m)); txt != "" {
			b.WriteString("<think>" + txt + "</think>\n")
		}
		if m.ChatAssistantMessage != nil {
			for _, tc := range m.ChatAssistantMessage.ToolCalls {
				name := ""
				if tc.Function.Name != nil {
					name = *tc.Function.Name
				}
				b.WriteString(`<call tool="` + name + `">`)
				b.WriteString(strings.TrimSpace(tc.Function.Arguments))
				b.WriteString("</call>\n")
			}
		}
	}
	for k, i := range s.tools {
		b.WriteString(`<result id="` + strconv.Itoa(k) + `">`)
		b.WriteString(schema.MessageText(req.Input[i]))
		b.WriteString("</result>\n")
	}
	b.WriteString("</step>")
	return b.String()
}

// reflectPrompt is the reflection instruction, written from the paper's description
// of its four parts (job, input/output format, the three waste categories with
// examples, and the guidelines that stop the model deleting too much). The examples
// are the ones the paper names from its own pilot study.
const reflectPrompt = `You are compressing ONE step of an AI coding agent's trajectory to cut token cost.

Each step is wrapped in <step id="N">. Inside it, <think> is the agent's reasoning,
<call tool="..."> is a tool invocation, and <result id="k"> is that tool's output.

Compress only the step with the id you are asked for, and within it only the
<result> payloads. Aim for 20%-50% of the original length. The result must stay
useful: a reader must be able to continue the task down the same path.

Remove only waste, of these three kinds:
- Useless — irrelevant to the task: __pycache__/.egg-info entries in a directory
  listing, the per-test "PASSED" lines of a green test run, "make[2]: Entering/Leaving
  directory" noise.
- Redundant — already stated elsewhere in this window: the replacement text an editor
  echoes back after applying it, code already read in an earlier step.
- Expired — mattered only to a finished sub-goal: the other files opened while
  scanning grep hits, once the relevant one has been identified.

Guidelines:
- Replace what you remove with "..." and a short takeaway, e.g.
  "... (individual test lines omitted; mostly PASSED)" or "... (same as above)".
- Keep the structure unchanged: XML tags, indentation, line numbers.
- Keep every error, failure, traceback, diff, path, identifier and number that the
  agent might still need. When in doubt, keep it.
- Reproduce each <result id="k"> tag with its id unchanged.

Output the compressed step only: begin with <step id="N"> and stop immediately after
</step>. No commentary, no code fences.`

// buildPrompt assembles the single prompt string components.Model accepts. The
// paper's implementation splits this across a system message, a user message and an
// assistant prefill with a </step> stop sequence; components.Model is one prompt in,
// text out, so the parts are folded together and the reply is parsed defensively
// instead (see parseReducedResults).
func buildPrompt(window string, targetID int) string {
	return reflectPrompt + "\n\n" + window +
		"\n\nNow compress the step with id " + strconv.Itoa(targetID) + "."
}

// parseReducedResults pulls the `<result id="k">…</result>` payloads out of a
// reflection reply. It tolerates the wrappers a model adds when it cannot be given a
// stop sequence: leading prose, code fences, a repeated `<step>` opener, and a
// missing trailing `</step>`. Returns the payloads by result id; an id the model
// dropped is simply absent, and its message is left verbatim.
//
// nResults is how many results the target step has, and it exists for one case: a block
// whose `id=` is MISSING or unparseable. Reading such a block as id 0 is safe when the
// step has a single result — there is nothing to confuse it with, and a model dropping
// the attribute is an expected event here, since it cannot be given a stop sequence. But
// when the step made PARALLEL tool calls, that default would splice one tool's
// compressed output into ANOTHER tool's message. That is a MISATTRIBUTION rather than a
// lossy reduction, and it is the one failure this parser must not have: the never-worse
// token check cannot see it (the text is smaller, merely wrong), the agent then reads an
// answer that never belonged to the call above it, and the decision is frozen and
// replayed for the rest of the session. So an unlabelled block is dropped whenever the
// step has more than one result — the prompt asks the model to reproduce every id
// unchanged, and this is the parser not depending on it having obeyed.
func parseReducedResults(out string, nResults int) map[int]string {
	res := map[int]string{}
	// Prefer the region inside the step envelope when the model emitted one, so
	// surrounding prose cannot contribute stray tags.
	if i := strings.Index(out, "<step"); i >= 0 {
		if j := strings.Index(out[i:], ">"); j >= 0 {
			out = out[i+j+1:]
		}
	}
	if i := strings.Index(out, "</step>"); i >= 0 {
		out = out[:i]
	}
	const open = "<result"
	const close = "</result>"
	for {
		i := strings.Index(out, open)
		if i < 0 {
			return res
		}
		rest := out[i+len(open):]
		gt := strings.Index(rest, ">")
		if gt < 0 {
			return res
		}
		id, labelled := 0, false
		if attr := rest[:gt]; strings.Contains(attr, "id=") {
			raw := attr[strings.Index(attr, "id=")+3:]
			raw = strings.Trim(raw, ` "'`)
			if sp := strings.IndexAny(raw, ` "'/>`); sp >= 0 {
				raw = raw[:sp]
			}
			if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
				id, labelled = n, true
			}
		}
		// An unlabelled block is unambiguous only in a single-result step; see above.
		keep := labelled || nResults == 1
		body := rest[gt+1:]
		end := strings.Index(body, close)
		if end < 0 {
			// Unterminated final block: keep what arrived rather than dropping the
			// whole reduction, then stop.
			if _, dup := res[id]; keep && !dup {
				res[id] = body
			}
			return res
		}
		if _, dup := res[id]; keep && !dup {
			res[id] = body[:end]
		}
		out = body[end+len(close):]
	}
}

func (d *AgentDiet) Offload(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	steps := splitSteps(req)
	var keys []string
	changed := 0
	// Indices whose reduction was replayed below. Phase 2 must not consider them again:
	// after a replay the message holds the REDUCED text, so a frozen lookup keyed on its
	// content hash would MISS and the step could be reduced a second time. skipReduce
	// catches this via the marker in the default mode, but not under marker_mode: off,
	// and it is reachable whenever two consecutive requests have the same step count
	// (an agent retry).
	replayed := map[int]bool{}

	// Phase 1 — replay every reduction this session already froze, on EVERY turn and
	// at any depth. The agent re-sends the original trajectory each turn, so without
	// this a reduced step would flip reduced→full→reduced and churn the provider's
	// cache; replaying the same bytes is what makes reductions accumulate over a
	// session the way editing the agent's own trajectory does in the paper.
	for _, s := range steps {
		for _, i := range s.tools {
			msg := &req.Input[i]
			if !schema.Rewritable(*msg) {
				continue
			}
			if fk, saved, ok := reapplyFrozen(c, rep, d.Name(), msg); ok {
				rep.TokensBefore += saved // best-effort; the pipeline recomputes exactly
				keys = append(keys, fk...)
				changed++
				replayed[i] = true
			}
		}
	}

	// Phase 2 — one NEW reduction, on the step that has just aged past the delay.
	target := len(steps) - 1 - d.delay
	// The paper also requires the trajectory to be at least b+a steps deep before any
	// reflection happens, so the window it hands the model is properly populated.
	if target < 0 || len(steps) < d.ctxBefore+d.delay {
		if changed == 0 {
			rep.Skipped = true
		}
		return keys, nil
	}
	ts := steps[target]

	// Which of the target step's results may we act on at all? Anything already
	// carrying a marker, or expanded by the agent, is off limits (never double-reduce,
	// never fight the expand loop).
	type item struct {
		resultID int // position within the step, i.e. the k in <result id="k">
		i        int // req.Input index
		content  string
	}
	var items []item
	for k, i := range ts.tools {
		if replayed[i] {
			continue // already reduced; its bytes are frozen
		}
		msg := req.Input[i]
		if !schema.Rewritable(msg) {
			continue
		}
		content := schema.MessageText(msg)
		if content == "" || skipReduce(c, content) {
			continue
		}
		// A frozen decision was replayed above; a second reduction of the same bytes
		// would orphan its stash.
		if _, hit := c.Store.Get(frozenKey(c.Session, d.Name(), contentKey(content))); hit {
			continue
		}
		if d.tailOnly && !c.TailOnly(i) {
			continue
		}
		items = append(items, item{k, i, content})
	}
	if len(items) == 0 {
		if changed == 0 {
			rep.Skipped = true
		}
		return keys, nil
	}

	// θ: the step must be long enough that a call can pay for itself.
	stepText := serializeStep(req, target, ts)
	stepTokens := schema.TextTokens(stepText)
	if stepTokens <= d.minStep {
		if changed == 0 {
			rep.Skipped = true
		}
		return keys, nil
	}

	model := d.modelClient
	if model == nil {
		model = c.Model.For(d.modelSource)
	}
	if model == nil { // no model configured: replay-only, never an error
		if changed == 0 {
			rep.Skipped = true
		}
		return keys, nil
	}

	// The reserve, before the model call — the same ordering summarize needs and for the same
	// reason, one step weaker because this component acts per message.
	//
	// Every plan this reflection produces will want a payload of its own, so if the reserve
	// cannot take even the smallest of them the call is paid and every plan is then declined at
	// commitMark: a model call for a step that is guaranteed to be left verbatim. Probing with
	// the smallest candidate is deliberately the WEAKEST useful test — it skips only when
	// nothing at all can be admitted, and never declines a step whose plans might still fit.
	// A partial fit stays the per-message decision it already is.
	smallest := 0
	for _, it := range items {
		if n := len(it.content); smallest == 0 || n < smallest {
			smallest = n
		}
	}
	if effectiveMode(c, d.mode) == markerFull && !store.StashRoom(c.Store, smallest) {
		// Counted, not just gated. stash_refused is documented — in config.md, in routes.md and on
		// the counter itself — as THE signal to raise a budget, and deliberately upstream of
		// expand_unresolved_missing. A component whose refusals are invisible to it undercuts that:
		// a deployment starving agentdiet would decline a whole step's removals every turn while
		// /stats read 0. One increment per declined step, which is what was declined.
		stashRefusals.Add(1)
		rep.Gate("stash_reserve_exhausted")
		if changed == 0 {
			rep.Skipped = true
		}
		return keys, nil
	}

	// The sliding window: b steps before the target through the a steps after it.
	var win strings.Builder
	lo := target - d.ctxBefore
	if lo < 0 {
		lo = 0
	}
	for id := lo; id < len(steps); id++ {
		if id == target {
			win.WriteString(stepText)
		} else {
			win.WriteString(serializeStep(req, id, steps[id]))
		}
		win.WriteString("\n")
	}

	ctx, cancel := context.WithTimeout(c.Ctx, agentDietTimeout)
	defer cancel()
	out, err := model.Complete(ctx, buildPrompt(win.String(), target))
	if err != nil || strings.TrimSpace(out) == "" {
		// Fail open: leave the step verbatim, but record WHY so a run that stopped
		// reducing cannot be mistaken for a run with nothing to reduce.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			atomic.AddInt64(&agentDietTimeouts, 1)
		} else {
			atomic.AddInt64(&agentDietErrors, 1)
		}
		if changed == 0 {
			rep.Skipped = true
		}
		return keys, nil
	}

	// The id space the reply answers is the one serializeStep emitted, i.e. EVERY result
	// of the target step — not the subset that survived the gates above.
	reduced := parseReducedResults(out, len(ts.tools))

	// The apply-gate, computed over the whole step so the ratio has the paper's
	// denominator: accept only a reduction that saves real tokens.
	saved := 0
	type plan struct {
		i       int
		content string
		text    string
	}
	var plans []plan
	for _, it := range items {
		got, ok := reduced[it.resultID]
		if !ok {
			continue
		}
		got = strings.TrimSpace(got)
		if got == "" {
			continue
		}
		before, after := schema.TextTokens(it.content), schema.TextTokens(got)
		if after >= before {
			continue // a rewrite that grew is not a reduction
		}
		saved += before - after
		plans = append(plans, plan{it.i, it.content, got})
	}
	if len(plans) == 0 || !(saved >= d.minSaved || float64(stepTokens-saved) < d.maxKeepRatio*float64(stepTokens)) {
		if changed == 0 {
			rep.Skipped = true
		}
		return keys, nil
	}

	// Phase 3 — splice, stash and freeze. Each result gets its own reversible marker
	// so `expand` can restore the full original, and its own never-worse check so a
	// marker cannot make one message bigger.
	for _, p := range plans {
		hint := " [full output: call " + expand.ToolName + "]"
		newText, key, eff, ok := tryMark(c, d.mode, p.content, hint, func(tok string) string {
			return p.text + "\n" + tok
		})
		if !ok {
			continue
		}
		if !commitMark(c, rep, eff, key, p.content) {
			continue // the store cannot back the marker; leave this message verbatim
		}
		schema.SetMessageText(&req.Input[p.i], newText)
		freeze(c, d.Name(), p.content, newText)
		changed++
		if key != "" {
			keys = append(keys, key)
		}
	}

	if changed == 0 {
		rep.Skipped = true
	}
	return keys, nil
}

func init() {
	f := []components.Field{
		{Key: "delay_steps", Type: components.FieldInt, Default: 2,
			Hint: "a: how many of the most recent steps are never touched. 0 is accepted for an ablation, but it targets the step just completed and gives up the paper's protection against a malfunctioning reflection model."},
		{Key: "context_steps", Type: components.FieldInt, Default: 1,
			Hint: "b: how many steps before the target are shown to the reflection model as context."},
		{Key: "min_step_tokens", Type: components.FieldInt, Default: 500,
			Hint: "θ: skip a step smaller than this."},
		{Key: "min_saved_tokens", Type: components.FieldInt, Default: 400,
			Hint: "Apply the reflection only if it saves at least this many tokens (the authors' artifact gate)."},
		{Key: "max_keep_ratio", Type: components.FieldFloat, Default: 0.8,
			Hint: "…and only if the rewrite keeps less than this fraction of the original."},
		{Key: "cache_tail_only", Type: components.FieldBool,
			Hint: "Restrict NEW reductions to the uncached tail. Default false, which is the faithful setting: the target step is chosen by age, so it is always inside the cached prefix and a tail restriction would make this component a silent no-op."},
		markerModeField(),
	}
	components.RegisterFields("agentdiet", agentDietConfig{}, append(f, modelFields("model")...))
}
