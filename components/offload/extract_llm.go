package offload

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/internal/extract"
	"github.com/rossoctl/context-guru/internal/logging"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/schema"
	"gopkg.in/yaml.v3"
)

// debugExtractLLM logs per-request candidate accounting. Gated on the LOG LEVEL rather
// than on its own env var: this is per-candidate accounting, which is what DEBUG means,
// and one switch for "tell me everything" beats one per component. CONTEXT_GURU_DEBUG=1
// still turns it on — internal/logging reads it as CG_LOG_LEVEL=debug.
func debugExtractLLM(c *components.Ctx) bool { return logging.Debugging(c.Ctx) }

// llmCallTimeout bounds a SINGLE in-request extract model call. Kept bounded so a slow
// or rate-limited compaction model fails open (leave the output verbatim this turn)
// instead of stalling the agent's request — synchronous compaction is on the hot path,
// so an unbounded wait here could push the agent's own request past its deadline.
//
// 15s WAS TOO TIGHT ON A LOADED SELF-HOSTED SERVER, and the failure was invisible.
// MEASURED on an on-prem vLLM under KV-cache pressure: server-side queue wait alone was
// p50 17.2s / p95 78.8s, i.e. THE OLD CEILING EXPIRED BEFORE THE MODEL EVEN STARTED on
// more than half of all calls. Observed over one 50-task SWE-bench arm at equal request
// volume:
//
//	leg                 proxy requests   llm_calls   calls/request   cg_added_ms_avg
//	low  (idle server)           2,513       2,093            0.83           5,530
//	high (KV-pressured)          2,387         255            0.11           8,563
//
// 8.2x fewer calls at 5% fewer requests, while per-request overhead ROSE 55% — the
// component was starting calls, blocking, hitting the ceiling, and discarding the work.
// Because it fails open silently, the arm degraded into a partial no-op that READ AS AN
// IMPROVEMENT on every dashboard (its "time penalty" shrank 42 points).
//
// The constant was a CLIENT-SIDE assumption about server latency. A hosted gateway
// answers in ~400ms; a shared on-prem GPU under load does not. So it is now configurable
// and defaults high enough that a *loaded* server still gets an answer:
//
//	CONTEXT_GURU_LLM_TIMEOUT=90s   (Go duration; bare integers are seconds)
//
// Tuning note: this is a per-call ceiling, not a target. Raising it trades "silently
// does nothing" for "measurably costs latency" — which is the correct trade, because the
// cost then SHOWS UP in the numbers instead of hiding. Watch `llm_timeouts` in /stats: a
// non-zero value means the budget is still too small for the server's current load. The
// economic gate's own latency brake (slowCallMs, extract_econ.go) is what stops
// speculative calls when the server is genuinely slow — that is the right layer for it,
// because it decides BEFORE spending the wall clock rather than after.
const defaultLLMCallTimeout = 90 * time.Second

// llmCallTimeout is resolved once at process start from the environment.
var llmCallTimeout = resolveLLMCallTimeout()

func resolveLLMCallTimeout() time.Duration {
	return resolveTimeoutEnv("CONTEXT_GURU_LLM_TIMEOUT", defaultLLMCallTimeout)
}

// Timeout/error counters. The fail-open path is CORRECT — compaction must never break
// the agent's request — but it must not be SILENT: an arm that quietly stops compacting
// looks like an arm that got faster. These are served at /stats (merged by the host, the
// same layering as FrozenStats) so `llm_calls` collapsing is visible as a timeout count
// rather than being mistaken for efficiency.
var (
	llmTimeouts int64
	llmErrors   int64
)

// LLMTimeouts returns the number of extract_llm calls abandoned on the per-call
// deadline. Non-zero means CONTEXT_GURU_LLM_TIMEOUT is too small for the current
// server load, and any token-savings number from this arm is an UNDERCOUNT of what
// the pipeline would have done on an unloaded server.
func LLMTimeouts() int64 { return atomic.LoadInt64(&llmTimeouts) }

// LLMErrors returns non-timeout failures of extract_llm model calls (transport,
// HTTP status, unparseable body, or a cancelled parent request).
func LLMErrors() int64 { return atomic.LoadInt64(&llmErrors) }

// LLMCallTimeout exposes the resolved per-call budget so /stats can report the
// configuration next to the counters (a timeout count is meaningless without it).
func LLMCallTimeout() time.Duration { return llmCallTimeout }

// llmConcurrency bounds how many of a request's candidate compactions run at once.
// Independent per-output calls run concurrently so a turn's parallel tool outputs cost
// ~one call's wall time instead of the sum. Bounded so a burst can't overwhelm the
// cheap-model endpoint. (A single-call batch alternative was measured ~3× worse on
// tokens saved with no latency win — see docs/CACHE_AWARE_ITERATIONS.md.)
const llmConcurrency = 4

func init() { components.Register("extract_llm", newExtractLLM) }

// ExtractLLM is the relevance-aware, LLM-driven tool-output reducer. A cheap model
// writes a small Starlark program that trims ONE tool output down to what the agent
// needs next (it may delete OR rewrite via regex, preserving ids/paths/errors
// verbatim, and may emit a one-line SUMMARY that goes into the marker). The program
// runs in a sandbox (no imports/IO, step+time limited) and the result must pass a
// sanity check (non-empty, strictly smaller, keep-ids present); on any miss the item
// is left verbatim. The full original is always stashed (reversible via expand).
//
// It is the EXPENSIVE pass, so it is throttled (per-session cadence + per-request
// cap) and — in cache-aware mode — only rewrites tool outputs in the uncached tail
// that are still medium/large AFTER the deterministic components ran. Prior
// compactions are reused byte-for-byte from state so the request prefix stays stable.
type ExtractLLM struct {
	minTokens     int
	strategy      string
	modelSource   string
	modelClient   components.Model
	trigger       components.Trigger
	mode          markerMode
	rewrite       bool
	llmEveryN     int
	llmMaxPerReq  int
	llmMaxPerSess int
	skipFileReads *bool // nil = auto (skip when cache-aware); true/false = force
	mu            sync.Mutex
	llmSeen       map[string]int // session -> count of qualifying (LLM-eligible) requests
	llmSpent      map[string]int // session -> model calls actually made (the per-session cap)

	// fireOnSize is `fire_on: size`: trigger on candidate size alone, and treat the
	// economic gate + caching-backend guard as advisory. See extractLLMConfig.FireOn.
	fireOnSize bool

	// minTokensSet records whether the operator pinned min_tokens / trigger explicitly.
	// When they did, their threshold governs (backward compatibility). When they did not,
	// the derived pressure-based trigger is the default — no per-workload tuning (#28 E).
	minTokensSet bool
	// gate enables the economic gate (#28 D). Default on; `economic_gate: false` restores
	// the old spend-on-size behavior for anyone who needs to reproduce old numbers.
	gate bool
	// allowCached permits extraction on prompt-caching backends. Default FALSE — see
	// extractLLMConfig.AllowOnCachingBackend for why the default ships disabled there.
	allowCached bool
	// pricing prices the extraction model's tokens for the gate's cost side (#28 D).
	pricing cheapmodel.Pricing
	// ratios learns this workload's real compression ratio instead of assuming one.
	ratios ratioTracker
	// prevTokens tracks per-session request size so growth rate is measurable (#28 E).
	prevTokens map[string]int
	// modelName identifies the extraction model in the global cache key, so switching
	// models misses rather than serving another model's extraction (#28 C).
	modelName string
	// modelMaxInput is the operator's pinned input budget for the extraction model
	// (0 = derive it, see inputLimit).
	modelMaxInput int
}

// Context budget for ONE extraction call.
//
// extract_llm sends ONE tool output per call (a per-output prompt, not an assembled
// conversation), so "the request is too big" here means a single prompt exceeding the
// EXTRACTION model's window — not too many messages. The prompt builders already bound the
// body they SHOW the model, but that bound is in characters and lives in internal/extract,
// so it is no protection against a small-window extraction model (an on-prem 8k/32k
// deployment, a gateway alias): the call then 400s, which fails open correctly but burns a
// round-trip and a slot in the request's wall clock every turn.
const (
	// cheapExtractOutputTokens mirrors the `max_tokens` the cheap clients send
	// (cheapmodel.Anthropic/OpenAI default to 2048). Most APIs bound input+output against
	// the same window — vLLM and friends reject the request outright — so the reply must be
	// reserved out of the budget rather than assumed free.
	cheapExtractOutputTokens = 2048
	// cheapExtractSlack covers the JSON envelope, role framing and provider-side
	// accounting that never appear in a token count of the text itself.
	cheapExtractSlack = 512
	// extractPromptOverheadTokens is the invariant part of the prompt: the ~1463-token
	// system preamble (measured, see cheapmodel.Anthropic.CompleteSystem) plus the keep-list
	// and section labels (≤60 short identifiers). Rounded up.
	extractPromptOverheadTokens = 2000
	// extractContextMargin marks up the estimate. tokens.Count is a real BPE count, but in
	// o200k_base — the extraction model may tokenize the same bytes 10-15% heavier
	// (Anthropic's tokenizer is not published, self-hosted models vary), so a count that is
	// exact for GPT is only an estimate for anyone else.
	extractContextMargin = 1.15
	// extractShownBodyChars mirrors the bound internal/extract puts on the body it SHOWS
	// the model (maxCodeContentChars: head+tail beyond it, the program still runs over the
	// full input at runtime). Counting the whole output instead would decline calls on
	// exactly the very large outputs this component exists for, since their prompt is
	// bounded. Mirrored rather than imported because it is unexported there; it is a
	// conservative direction only while it is >= extract's own bound, so raise it in step.
	extractShownBodyChars = 32000
	// defaultSizeThreshold is the per-output floor `fire_on: size` uses when the operator
	// named no min_tokens. It is NOT the legacy 300-token default on purpose: 300 is
	// cleared by almost every tool output, so a size trigger inheriting it would fire on
	// every turn of every session — which is precisely the 271-call, $3.26, 82x-underwater
	// behaviour the economic gate was built to stop. 2000 tokens is roughly the largest
	// output observed on Terminal-Bench, so this errs toward "fires rarely, tune it up or
	// down deliberately".
	defaultSizeThreshold = 2000
	// unknownModelInputLimit is the budget for an extraction model we cannot name. Low
	// enough to protect a small self-hosted deployment, high enough that it does not gate
	// realistic candidates (the largest tool output measured on Terminal-Bench was ~2k
	// tokens). Pin model_max_input_tokens when the real window is smaller.
	unknownModelInputLimit = 32768
)

// staticWindows is the last-resort model→window table modelinfo already maintains. Held as
// a package var because DefaultStatic allocates.
var staticWindows = modelinfo.DefaultStatic()

// inputLimit resolves the extraction model's input-token budget. Config pin first, then the
// model's own window as DATA (modelinfo's table), then a conservative default.
func (e *ExtractLLM) inputLimit(c *components.Ctx) int {
	if e.modelMaxInput > 0 {
		return e.modelMaxInput
	}
	if e.modelName != "" { // a config-pinned client: we know exactly which model it is
		if w, ok := staticWindows.Window(c.Ctx, e.modelName); ok {
			return w
		}
		return unknownModelInputLimit
	}
	// No pinned model. `source: config` means the host's separate cheap client, whose id we
	// never see — stay conservative. Otherwise the extraction model IS the proxied model,
	// and the host already resolved its window onto the Ctx.
	if e.modelSource != "config" && c.CtxWindow > 0 {
		return c.CtxWindow
	}
	return unknownModelInputLimit
}

// shownBodyTokens estimates what one tool output costs INSIDE the prompt: the same
// head+tail sample internal/extract shows the model, tokenized. The full output is not the
// right number — a 200k-token log still travels as a bounded sample.
func shownBodyTokens(content string) int {
	if len(content) > extractShownBodyChars {
		half := extractShownBodyChars / 2
		content = content[:half] + content[len(content)-half:]
	}
	return schema.TextTokens(content)
}

// fitsModelContext reports whether one extraction call whose body costs bodyTok can stay
// inside limit, with the reply and the tokenizer margin reserved.
//
// Errs toward declining: over-estimating costs one skipped compaction, visible as the
// over_model_context gate; under-estimating puts a request on the wire that the upstream
// may reject, which costs a round-trip and produces nothing.
func fitsModelContext(bodyTok, overheadTok, limit int) bool {
	est := int(float64(bodyTok+overheadTok) * extractContextMargin)
	return est+cheapExtractOutputTokens+cheapExtractSlack <= limit
}

type extractLLMConfig struct {
	MinTokens    int    `yaml:"min_tokens"`
	Strategy     string `yaml:"strategy"`             // code (default) | single | rlm | auto
	LLMEveryN    int    `yaml:"llm_every_n_requests"` // throttle LLM path: fire once per N requests/session
	LLMMaxPerReq int    `yaml:"llm_max_per_request"`  // cap LLM calls per firing request (0 = unlimited)
	// FireOn selects what decides that a request is worth a model call.
	//
	//	pressure (default) — the derived context-pressure trigger (#28 E): fire above
	//	                     60% of the window, or above 25% while growing >10%/turn.
	//	size               — fire whenever ANY candidate output clears MinTokens.
	//
	// `size` is the operator saying "I want this to run, bound it by size and by the
	// caps, not by economics". It therefore ALSO demotes the economic gate and the
	// caching-backend guard to ADVISORY: both still evaluate and still record what they
	// would have refused (visible as the economic_gate_advisory gate and at /stats), but
	// neither blocks the call. That is a deliberate licence to spend, and on a
	// prompt-caching backend our own measurements say most such calls lose money — which
	// is why the only brakes left are MinTokens, LLMMaxPerReq and LLMMaxPerSess. Set
	// those before setting this.
	FireOn string `yaml:"fire_on"`
	// LLMMaxPerSess caps model calls for the whole SESSION (0 = unlimited). The
	// per-request cap alone cannot bound a long session: 2 calls x 300 turns is 600
	// calls. With `fire_on: size` this is the outer bound on spend, so it is the number
	// that matters most.
	LLMMaxPerSess int                `yaml:"llm_max_per_session"`
	Model         modelConfig        `yaml:"model"`
	Trigger       components.Trigger `yaml:"trigger"`
	MarkerMode    string             `yaml:"marker_mode"` // full (default) | summary | off
	// Rewrite lets the program reword/summarize/collapse (not just delete), dropping
	// the strict deletion-only containment proof; ids/paths/errors/keep-ids are still
	// required verbatim by the sanity check. Default true (the powerful mode) — set
	// false to force verified deletion-only.
	Rewrite *bool `yaml:"rewrite"`
	// AllowOnCachingBackend re-enables extraction on prompt-caching backends. Unset =
	// FALSE: the component is disabled by default there, because every caching workload
	// measured in #28 came out net-negative even with the gate working correctly
	// (break-even ~30,500 tokens/output against a largest-observed 2,053). Shipping a
	// component our own numbers say loses money, guarded only by a doc note, is not a
	// defensible default. Set true if your outputs are genuinely huge; the gate's
	// economics then decide each call as normal.
	AllowOnCachingBackend *bool `yaml:"allow_on_caching_backend"`
	// EconomicGate opts out of the expected-value gate (#28 D). Unset = ON (the default):
	// only call the LLM when the expected saving exceeds the expected call cost, priced
	// from real model rates and the cache-awareness of the traffic. Set false to restore
	// the pre-#28 spend-on-size behavior — needed only to reproduce old benchmark numbers.
	EconomicGate *bool `yaml:"economic_gate"`
	// ModelMaxInput pins the EXTRACTION model's input-token budget, for deployments the
	// static table cannot name (a self-hosted id like `qwen3-coder-30b`, or a gateway alias
	// that hides the real model). Unset = resolved per model, see (*ExtractLLM).inputLimit.
	ModelMaxInput int `yaml:"model_max_input_tokens"`
	// SkipFileReads controls whether line-numbered source-file dumps are left verbatim.
	// Tri-state: unset = AUTO (skip when the request is prompt-cached, reduce otherwise);
	// true = always skip; false = always reduce. Rationale (measured, SWE-bench 50):
	// on a ~98%-cached agent, file reads already bill at the cheap cache-read rate, so
	// skeletonizing them saves almost nothing yet costs the compaction LLM + one-time
	// cache-write transitions → +30% billed cost. On a NON-caching backend the same
	// reduction is a direct saving. So AUTO skips file reads exactly when caching makes
	// them cheap. See docs/CACHE_AWARE_ITERATIONS.md.
	SkipFileReads *bool `yaml:"skip_file_reads"`
}

func newExtractLLM(raw []byte) (components.Component, error) {
	cfg := extractLLMConfig{MinTokens: 300, Strategy: "code"}
	// Detect whether the operator pinned a threshold BEFORE defaults are applied: the
	// distinction between "unset" and "set to the default value" is what decides whether
	// the smart trigger or their number governs, so it must be read from the raw YAML.
	explicit := false
	if len(raw) > 0 {
		var probe struct {
			MinTokens *int `yaml:"min_tokens"`
			Trigger   *struct {
				MinRequestTokens *int `yaml:"min_request_tokens"`
				MinOutputTokens  *int `yaml:"min_output_tokens"`
			} `yaml:"trigger"`
		}
		if err := yaml.Unmarshal(raw, &probe); err == nil {
			explicit = probe.MinTokens != nil ||
				(probe.Trigger != nil &&
					(probe.Trigger.MinRequestTokens != nil || probe.Trigger.MinOutputTokens != nil))
		}
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
	}
	rewrite := true
	if cfg.Rewrite != nil {
		rewrite = *cfg.Rewrite
	}
	if cfg.Strategy == "" {
		cfg.Strategy = "code"
	}
	gate := true // economic gate on by default (#28 D)
	if cfg.EconomicGate != nil {
		gate = *cfg.EconomicGate
	}
	fireOnSize := false
	switch cfg.FireOn {
	case "", "pressure":
	case "size":
		fireOnSize = true
		// A size trigger with no size is the 271-call failure mode: the legacy MinTokens
		// default is 300 tokens, which nearly every tool output clears, so the component
		// would fire on every turn. Require a real threshold and supply a conservative
		// one when the operator gave none.
		if !explicit {
			cfg.MinTokens = defaultSizeThreshold
			explicit = true // their number governs the floor, not the pressure curve
		}
	default:
		return nil, fmt.Errorf("extract_llm: fire_on must be pressure|size, got %q", cfg.FireOn)
	}
	// Off by default on caching backends (see AllowOnCachingBackend). Disabling the gate
	// entirely is an explicit request for pre-#28 behavior, so honor it here too — otherwise
	// `economic_gate: false` would still be silently blocked on caching traffic.
	allowCached := !gate
	if cfg.AllowOnCachingBackend != nil {
		allowCached = *cfg.AllowOnCachingBackend
	}
	return &ExtractLLM{
		minTokens: cfg.MinTokens, strategy: cfg.Strategy,
		modelSource: cfg.Model.Source, modelClient: cfg.Model.Client(),
		trigger: cfg.Trigger, mode: parseMarkerMode(cfg.MarkerMode), rewrite: rewrite,
		llmEveryN: cfg.LLMEveryN, llmMaxPerReq: cfg.LLMMaxPerReq,
		llmMaxPerSess: cfg.LLMMaxPerSess, fireOnSize: fireOnSize,
		skipFileReads: cfg.SkipFileReads, llmSeen: map[string]int{},
		llmSpent:     map[string]int{},
		minTokensSet: explicit, gate: gate, allowCached: allowCached,
		pricing:    cheapmodel.PricingFromEnv(),
		prevTokens: map[string]int{}, modelName: cfg.Model.Model,
		modelMaxInput: cfg.ModelMaxInput,
	}, nil
}

// noteRequestSize records this request's size for the session and returns the previous
// one, so the trigger can measure context growth rate (#28 E).
func (e *ExtractLLM) noteRequestSize(session string, tokens int) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	prev := e.prevTokens[session]
	e.prevTokens[session] = tokens
	return prev
}

func (*ExtractLLM) Name() string                 { return "extract_llm" }
func (*ExtractLLM) Enabled(*components.Ctx) bool { return true }

func (e *ExtractLLM) outputFloor(window int) int {
	return e.trigger.OutputFloor(window, e.minTokens)
}

// llmAllowedThisRequest applies the per-session cadence: true on the 1st qualifying
// request and every Nth after, so the LLM path fires "every multiple steps".
func (e *ExtractLLM) llmAllowedThisRequest(session string) bool {
	if e.llmEveryN <= 1 {
		return true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.llmSeen[session]++
	return (e.llmSeen[session]-1)%e.llmEveryN == 0
}

// reserveSessionBudget hands out up to want model-call slots from this session's
// remaining LLM_MAX_PER_SESSION allowance and returns how many were granted (want when
// the cap is unset). Slots are taken BEFORE the calls are made, so two concurrent turns
// of one session cannot both read "19 spent" and both spend.
//
// It counts CALLS, not requests: llm_every_n_requests already throttles requests, and a
// request may make up to llmMaxPerReq calls, so a request-based cap cannot bound spend.
func (e *ExtractLLM) reserveSessionBudget(session string, want int) int {
	if e.llmMaxPerSess <= 0 || want <= 0 {
		return want
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	left := e.llmMaxPerSess - e.llmSpent[session]
	if left <= 0 {
		return 0
	}
	if want > left {
		want = left
	}
	e.llmSpent[session] += want
	return want
}

var lineNumberedRe = regexp.MustCompile(`^\s{0,6}\d+[\t ]`)

// looksLikeFileRead reports whether content is a line-numbered source-file dump (a
// read/cat -n output): most non-empty lines begin with a line number. Such outputs
// are whole files the agent is working with — irreducible — so skip the model call.
func looksLikeFileRead(content string) bool {
	checked, numbered := 0, 0
	for _, ln := range strings.Split(content, "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		checked++
		if lineNumberedRe.MatchString(ln) {
			numbered++
		}
		if checked >= 40 {
			break
		}
	}
	return checked >= 8 && numbered*100/checked >= 60
}

func (e *ExtractLLM) Offload(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	// Resolved once: the candidate loop below tests it per tool output, and it is a
	// handler call rather than a field read.
	dbg := debugExtractLLM(c)
	fires := e.trigger.Fires(req, c.CtxWindow)
	goal := conversationGoal(req)
	query := keywords(goal)
	if len(query) == 0 {
		rep.Gate("no_goal_keywords")
		rep.Skipped = true
		return nil, nil
	}
	model := e.modelClient
	if model == nil {
		model = c.Model.For(e.modelSource)
	}
	// Per-session cadence: on throttled steps drop the model (skip this request).
	if model != nil && fires && !e.llmAllowedThisRequest(c.Session) {
		model = nil
	}
	// Derived trigger (#28 E): context pressure + growth rate replace a hand-tuned
	// threshold. When min_tokens/trigger is set explicitly the operator's value governs.
	reqTokens := schema.MessagesTokens(req)
	prevTokens := e.noteRequestSize(c.Session, reqTokens)
	pressure := contextPressure(reqTokens, c.CtxWindow)
	growth := growthRate(reqTokens, prevTokens)
	pressureFires, triggerReason := shouldFire(pressure, growth, e.minTokensSet, e.fireOnSize)
	// An unknown context window (0) makes pressure meaningless; fall back to the
	// configured Trigger alone, the same fail-open convention Trigger itself uses.
	if c.CtxWindow <= 0 {
		pressureFires, triggerReason = fires, "context window unknown; absolute trigger only"
	}
	if model != nil && !pressureFires {
		model = nil // no model call this request; frozen reapplications still run below
	}
	metrics.RecordExtractionReason(triggerReason)

	floor := e.outputFloor(c.CtxWindow)
	// Without an explicit min_tokens, derive the per-output floor from context pressure so
	// there is no per-workload number to pick (#28 E).
	if !e.minTokensSet && !e.fireOnSize {
		if pf := pressureFloor(c.CtxWindow, pressure); pf > 0 {
			floor = pf
		}
	}
	// Gate inputs shared by every candidate this request.
	val := savedTokenValue(c)
	ratio := e.ratios.ratio()
	turnsSoFar := len(req.Input)
	extCfg := extract.DefaultCfg()
	extCfg.Mode, extCfg.Floor, extCfg.Rewrite = e.strategy, floor, e.rewrite

	keepIDs := extract.HarvestIdentifiers(goal, 40)
	// Per-call context budget (constant across this request's candidates): the extraction
	// model's input limit, and the prompt's fixed cost around the tool output itself.
	inputLimit := e.inputLimit(c)
	promptOverhead := extractPromptOverheadTokens + schema.TextTokens(goal)
	tools := toolIndices(req)
	var keys []string
	changed := 0

	// apply splices a compacted projection + marker into message i (serial: store writes
	// and message mutation are not concurrency-safe).
	apply := func(i int, content, projected, summary string) {
		if projected == "" || schema.TextTokens(projected) >= schema.TextTokens(content) {
			return
		}
		hint := " [full output: call " + expand.ToolName + "]"
		newText, key, eff, ok := tryMark(c, e.mode, content, hint, func(tok string) string {
			if summary != "" {
				return projected + "\n[" + summary + "] " + tok
			}
			return projected + "\n" + tok
		})
		if !ok {
			return
		}
		commitMark(c, rep, eff, key, content)
		schema.SetMessageText(&req.Input[i], newText)
		changed++
		if key != "" {
			keys = append(keys, key)
		}
	}

	// Phase 1 (serial, cheap): reapply frozen compactions on every turn (keeps the
	// request prefix byte-stable so the provider cache stays warm), and collect the NEW
	// candidates that still need a model call.
	type cand struct {
		i       int
		content string
		id      string
	}
	var cands []cand
	skipFR := false
	if e.skipFileReads != nil {
		skipFR = *e.skipFileReads
	}
	var dbgTail, dbgFloor, dbgPlace, dbgReapply, dbgBigTailBlocked, dbgMaxSz int
	for _, i := range tools {
		msg := &req.Input[i]
		if !schema.Rewritable(*msg) {
			rep.Gate("non_text_blocks")
			continue
		}
		content := schema.MessageText(*msg)
		if content == "" || expand.HasPlaceholder(content) {
			dbgPlace++
			rep.Gate("empty_or_marker_present")
			continue
		}
		id := extract.ContentKey(content)
		// If the agent recently EXPANDED this content, leave it verbatim (re-compacting it
		// would just trigger another expand — a loop). The expand handler marks it.
		if isKeptVerbatim(c, id) {
			rep.Gate("kept_verbatim_after_expand")
			continue
		}
		// SAME-SESSION replay first. This session already sent these compacted bytes on an
		// earlier turn, so the provider's cached prefix holds the COMPACTED form and replaying
		// it is byte-identical — which is why it is safe at any depth, cached prefix included.
		//
		// One key per decision, not two. #40 unified the projected text and its summary into
		// a single JSON value precisely because the split `cg:res:` + `cg:sum1:` keys had
		// independent TTLs and pin slots, so a replay could hit one and miss the other and
		// emit HALF a decision — projected text with the summary segment silently gone.
		if cached, hit := getResult(c, id); hit {
			metrics.RecordExtractionCacheLookup(true)
			apply(i, content, cached.Projected, cached.Summary)
			dbgReapply++
			rep.Gate("reapplied_same_session")
			continue
		}
		// A NEW compaction, on the UNCACHED region only (cache-safe): when cache-aware that
		// is every message newer than last turn (index > MaxCachedIdx) — catching ALL of a
		// turn's tool outputs including PARALLEL tool calls, never the cached prefix. When
		// caching is off, any message is fair game. File reads included (largest mass);
		// safe because we never touch already-cached content and freeze+reapply the result.
		sz := schema.TextTokens(content)
		// No lost-decision repair here, unlike mask/failed_run: this replacement is a SAMPLED
		// model output (cheapmodel sends no temperature/seed), so re-deriving at depth could
		// emit different bytes inside the cached prefix — the very thing the repair exists to
		// prevent. And the trade doesn't pay even setting that aside: if the bytes differ the
		// suffix is cache-written either way, so re-deriving would buy a model call for
		// nothing. The model may also not run at all (throttle, timeout, floor), which would
		// leave the output verbatim at depth after the gate had already been lifted.
		if sz > dbgMaxSz {
			dbgMaxSz = sz
		}
		if c.CacheAware && !c.TailOnly(i) {
			dbgTail++
			if sz >= floor {
				dbgBigTailBlocked++ // a large output we skipped ONLY because it's not in the tail
				rep.Gate("cached_prefix_above_floor")
			}
			rep.Gate("cached_prefix")
			continue
		}
		// CROSS-SESSION reuse (#28 C), deliberately placed AFTER the tail gate — the
		// invariant TestGlobalCacheHitIsNotSplicedAtDepth exists to protect. An extraction is
		// a context-free derived result, so a global content-hash key lets a different session
		// skip re-deriving it (measured: 82 of 103 unique contents recurred across sessions).
		// But cache-safety differs from the same-session replay above: THIS session never sent
		// these compacted bytes, so the provider's cached prefix holds the ORIGINAL. Splicing
		// a global hit at depth would mutate already-cached content and force a suffix re-write
		// at 11.5x the read price — the same churn the tail gate exists to prevent, just
		// sourced from a cache instead of a model call, and exactly the harm #40 removed
		// repairLostResult to avoid. So a global hit is treated exactly like a new decision:
		// tail-only, then frozen and replayed from the same-session path at any depth. Do NOT
		// flatten this ordering: only getResult may bypass the tail gate.
		//
		// It cannot become a re-derive path (#40's stance): on a MISS this falls through to the
		// normal candidate flow, which is already tail-gated and floor-gated; nothing here
		// lifts a depth restriction the way the removed repairLostResult did.
		//
		// Reuse is gated on RECOVERABILITY, not verification. The result was derived toward
		// the goal of whichever session produced it, and in the default rewrite mode the
		// containment proof is deliberately skipped — so a reused result can be a lossy rewrite
		// steered by an unrelated task. That is acceptable only while the agent can get the
		// original back: with a full (reversible) marker the stash is refreshed and `expand`
		// recovers it. Without one (marker_mode summary/off, or a non-persisting store) the
		// drop is permanent, so fall back to same-session reuse only.
		//
		// One key per decision here too: the global namespace stores the projected text and
		// its summary in the SAME value, for the same reason #40 unified the session-scoped
		// pair. Splitting them across two global keys would re-create the half-a-decision bug
		// cross-session, where independent TTLs let a hit on one and a miss on the other emit
		// projected text with the summary segment silently gone.
		if !e.rewrite || effectiveMode(c, e.mode) == markerFull {
			if cached, hit := getResultGlobal(c, extract.ResultKey(id, e.modelName, extCfg)); hit {
				metrics.RecordExtractionCacheLookup(true)
				// Freeze into this session so later turns replay it byte-for-byte from the
				// same-session path above, at any depth.
				putResult(c, id, cached.Projected, cached.Summary)
				apply(i, content, cached.Projected, cached.Summary)
				dbgReapply++
				rep.Gate("reapplied_cross_session")
				continue
			}
		}
		metrics.RecordExtractionCacheLookup(false)
		if sz < floor {
			dbgFloor++
			rep.Gate("below_output_floor") // only medium/large outputs are worth a model call
			continue
		}
		if huge := e.trigger.IsHuge(sz, c.CtxWindow); !c.CacheAware && !fires && !huge {
			rep.Gate("request_trigger_not_fired")
			continue
		}
		if model == nil {
			// No client, or the cadence/pressure gate dropped it for this request.
			rep.Gate("no_model_this_request")
			continue
		}
		if skipFR && looksLikeFileRead(content) {
			rep.Gate("skip_file_read")
			continue
		}
		// Would this one output's prompt exceed the extraction model's context? Then there is
		// nothing to shed — the prompt is one tool output, and truncating it would hand the
		// model a body its program must then run against the FULL input at runtime. Leave the
		// output verbatim (fail open) rather than spend a round-trip on a request the upstream
		// may reject. Checked before the economic gate because it is cheaper and absolute.
		if !fitsModelContext(shownBodyTokens(content), promptOverhead, inputLimit) {
			rep.Gate("over_model_context")
			continue
		}
		// The economic gate (#28 D). This is the check the component never had: is one
		// LLM call worth it for THIS output, given that a saved token in a cached region
		// is worth 10x less? Where caching makes extraction pointless, suppress it; on a
		// non-caching backend or for recurring content, allow it.
		// Record the sighting BEFORE the gate reads it, and read the PRIOR value. The flag
		// means "seen on an earlier turn/session", so marking it after the gate allowed a
		// call made first sight reclassify itself as recurring and collect a 50% valuation
		// bump (6 expected reuses vs 4) it had not earned — the gate over-firing in the
		// opposite direction from the two pessimistic priors fixed earlier. Marking on
		// OBSERVATION also means a suppressed candidate still counts as seen, which is
		// correct: recurrence is a property of the content, not of what we decided to spend.
		seenBefore := markSeenContent(c, id)
		if e.gate {
			// Stop exploring once calls are observed to be slow: exploration spends wall
			// clock as well as money, and an agent on a task deadline feels the former more.
			explore := !tooSlowToExplore(metrics.ExtractionAvgLatencyMs()) &&
				e.ratios.exploring(c.Session)
			d := evaluateGate(sz, ratio, val, callCost(e.pricing, sz), seenBefore, turnsSoFar,
				explore, e.allowCached)
			if !d.allow && e.fireOnSize {
				// ADVISORY: `fire_on: size` is the operator taking the spending decision
				// away from the gate, so record what the gate would have refused — the
				// counterfactual is the only way anyone later sees what this cost — and
				// then proceed anyway. Deliberately does NOT bump the suppressed counter:
				// nothing was suppressed, and inflating that would make the gate look
				// like it was working when it has been overridden.
				metrics.RecordExtractionReason("advisory: " + d.reason)
				rep.Gate("economic_gate_advisory")
				if dbg {
					logging.From(c.Ctx).Debug("cg.extract_llm.gate", "decision", "advisory",
						"reason", d.reason, "size", sz, "exp_saving_usd", d.expSaving,
						"exp_cost_usd", d.expCost, "cacheAware", c.CacheAware)
				}
				d.allow = true
			}
			if !d.allow {
				metrics.RecordExtractionSuppressed(d.reason)
				// Just the gate name here: the per-reason breakdown already ships in
				// /stats via RecordExtractionSuppressed, and a full sentence makes a
				// poor histogram key.
				rep.Gate("economic_gate")
				if dbg {
					logging.From(c.Ctx).Debug("cg.extract_llm.gate", "decision", "suppress",
						"reason", d.reason, "size", sz, "exp_saving_usd", d.expSaving,
						"exp_cost_usd", d.expCost, "cacheAware", c.CacheAware)
				}
				continue
			}
			metrics.RecordExtractionReason(d.reason)
		}
		cands = append(cands, cand{i, content, id})
	}
	if dbg && len(tools) > 0 {
		logging.From(c.Ctx).Debug("cg.extract_llm", "tools", len(tools), "cands", len(cands),
			"reapplied", dbgReapply, "skip_placeholder", dbgPlace, "skip_tail", dbgTail,
			"skip_floor", dbgFloor, "max_output_tokens", dbgMaxSz, "big_but_not_tail", dbgBigTailBlocked,
			"cacheAware", c.CacheAware, "maxCachedIdx", c.MaxCachedIdx, "floor", floor,
			"nInput", len(req.Input))
	}
	if e.llmMaxPerReq > 0 && len(cands) > e.llmMaxPerReq {
		for k := e.llmMaxPerReq; k < len(cands); k++ {
			rep.Gate("over_per_request_cap")
		}
		cands = cands[:e.llmMaxPerReq] // cap model calls per request
	}
	// Then the session's own allowance. Reserved here, after the per-request cap, because
	// every surviving candidate becomes exactly one model call in phase 2 below — so the
	// reservation is the spend.
	if n := e.reserveSessionBudget(c.Session, len(cands)); n < len(cands) {
		for k := n; k < len(cands); k++ {
			rep.Gate("over_per_session_cap")
		}
		cands = cands[:n]
	}

	// Phase 2 (parallel): the candidate compactions are independent. A focused per-output
	// prompt ("trim THIS one output") gives a much better reduction than a single program
	// over the whole heterogeneous batch (measured on SWE: ~3× more tokens saved), and
	// running them concurrently (bounded) keeps a turn's cost to ~one call's wall time —
	// so parallel beats a single-call batch on tokens AND latency. Each output fails open
	// independently (a miss leaves that one verbatim).
	if len(cands) > 0 {
		type outT struct{ projected, summary string }
		out := make([]outT, len(cands))
		sem := make(chan struct{}, llmConcurrency)
		var wg sync.WaitGroup
		for k := range cands {
			wg.Add(1)
			go func(k int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				ctx, cancel := context.WithTimeout(c.Ctx, llmCallTimeout)
				defer cancel()
				before := schema.TextTokens(cands[k].content)
				start := time.Now()
				res, sum, _ := extract.RunExtractionSummary(ctx, cands[k].content, goal, keepIDs, before, extCfg, model)
				metrics.RecordExtractionCall(float64(time.Since(start).Milliseconds()))
				// CLASSIFY THE SILENT FAILURE — and classify it INDEPENDENTLY of whether
				// a result came back. RunExtractionSummary returns ("", "", "none") for every
				// failure mode, so timeout / sandbox rejection / "nothing shrank" are
				// indistinguishable in its return value. Our own ctx is the one reliable
				// signal: if its deadline expired, THIS call was abandoned.
				//
				// Do NOT fold this into an `else` of the success check. In `code` mode the
				// deterministic strategy runs as a fallback (extract.go:367-368), so a call
				// whose LLM leg timed out can still return a smaller `res` — and an `else`
				// would then record nothing. That is exactly the shape of the bug these
				// counters exist to expose: the arm keeps compacting a little, so no
				// dashboard looks broken while the expensive path has silently stopped.
				//
				// Fail-open behaviour is unchanged either way — this only records.
				timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
				if ctx.Err() != nil {
					if timedOut {
						atomic.AddInt64(&llmTimeouts, 1)
					} else {
						atomic.AddInt64(&llmErrors, 1)
					}
				}
				if res != "" && res != cands[k].content {
					out[k] = outT{res, sum}
					// Feed the observed ratio so the gate prices future calls on what this
					// workload actually achieves, not on an assumption.
					e.ratios.observe(before-schema.TextTokens(res), before)
					metrics.RecordExtractionSaving(before - schema.TextTokens(res))
				} else if !timedOut {
					e.ratios.observe(0, before) // a miss is real evidence: ratio 0
				}
				// TIMED OUT WITH NOTHING BACK => DELIBERATELY NOT OBSERVED. A ratio-0
				// observation means "the model looked at this output and could not shrink
				// it", which is real evidence about the workload. A deadline means the call
				// never finished — evidence about SERVER LATENCY, not compressibility — and
				// feeding it to the tracker makes the gate shut itself permanently on
				// exactly the deployment where the budget is already too small:
				//
				//   minRatioSampleTokens is 1500, so ONE timed-out medium output both ends
				//   this session's exploration (r.total >= the sample floor => exploring()
				//   returns false) and starts dragging ratio() down from the 0.12 prior. A
				//   few more and expectedRemoved falls below call cost for everything, so
				//   evaluateGate suppresses every call — and the tracker lives on the
				//   Pipeline for the proxy's LIFETIME, so nothing revises it afterwards.
				//
				// That is the self-justifying prior extract_econ.go's exploration budget
				// exists to prevent, re-entered through the timeout path. MEASURED: 13
				// timeouts in one 50-task arm at the 90s budget on a KV-pressured TP=1
				// server, i.e. this is a live regime, not a hypothetical. Skipping the
				// observation leaves the gate's estimate untouched; the timeouts are still
				// counted (above) and still brake exploration via slowCallMs, which is the
				// latency-aware layer that SHOULD react to a slow server.
			}(k)
		}
		wg.Wait()
		for k := range cands { // Phase 3 (serial): freeze + splice.
			if out[k].projected == "" {
				continue
			}
			// Publish to the GLOBAL namespace only when the result is recoverable (or verified
			// deletion-only) — the same condition the read side checks. An unverified, lossy
			// rewrite with no way back must not become another session's starting point; keep it
			// session-scoped so this session still benefits across its own turns.
			// Always write the session-scoped entry: this session's own cross-turn replay is
			// what keeps its cached prefix byte-stable, and it must not depend on whether the
			// result also qualified for cross-session sharing. Then publish globally only when
			// recoverable. One key per decision (#40) — projected text and summary travel
			// together, so a replay can never emit half a decision.
			putResult(c, cands[k].id, out[k].projected, out[k].summary)
			if !e.rewrite || effectiveMode(c, e.mode) == markerFull {
				putResultGlobal(c, extract.ResultKey(cands[k].id, e.modelName, extCfg),
					out[k].projected, out[k].summary)
			}
			apply(cands[k].i, cands[k].content, out[k].projected, out[k].summary)
		}
	}

	if changed == 0 {
		rep.Skipped = true
	}
	return keys, nil
}
