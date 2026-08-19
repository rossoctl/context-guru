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
	fireOnSize  bool
	aggro       extract.Aggressiveness
	ctxMode     contextMode
	ctxMessages int
	perOutput   bool
	cold        coldCacheConfig

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
	// cheapExtractOutputTokens mirrors the `max_tokens` the cheap clients send. Most APIs
	// bound input+output against the same window — vLLM and friends reject the request
	// outright — so the reply must be reserved out of the budget rather than assumed free.
	// Taken from the client's own constant so the two cannot drift: they did, and the
	// mismatch showed up as replies truncated at exactly the cap.
	cheapExtractOutputTokens = cheapmodel.DefaultMaxTokens
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
	if e.modelName != "" { // the model is named in config, so we know exactly which it is
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

// coldCacheConfig is the whole-transcript sweep.
//
// Why it exists, and why it is not just "extraction with a bigger budget": on a turn whose
// prompt cache has expired, the provider re-bills the ENTIRE transcript as cache creation
// at 1.25x the fresh rate. Measured on this deployment over 1.4 days, those turns were 4%
// of requests and 31% of spend ($360 of $1,173, ~$1.64 per turn against $0.144 warm), and
// the shipped pipeline saved 0.015% of it. Two things are true only on that turn: removing
// a token is worth 12.5x what it is worth on a warm turn, and rewriting deep history is
// free because there is no live cached prefix left to invalidate. So the sweep is not
// aggression, it is taking the one window where the arithmetic is overwhelmingly in favour.
type coldCacheConfig struct {
	Enabled bool `yaml:"enabled"`
	// MinTokens is the per-output floor for the sweep (0 = 1000). Lower than the hot path's,
	// because on this turn every candidate is being re-billed at the write rate anyway.
	MinTokens int `yaml:"min_tokens"`
	// MinIdleSeconds demands MORE idle time than the provider TTL implies (0 = just the
	// TTL). Raises the bar, never lowers it: the TTL check is the correctness condition and
	// this is only extra caution.
	MinIdleSeconds int `yaml:"min_idle_seconds"`
	// MaxCalls caps model calls for one sweep (0 = unlimited). Unlimited is the default
	// because the sweep runs once per idle gap on a turn that is already expensive, and the
	// operator asked for maximum saving there rather than a latency bound.
	MaxCalls int `yaml:"max_calls"`
}

// defaultColdFloor is the sweep's per-output floor when none is configured.
const defaultColdFloor = 1000

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
	// PerOutput enables the HOT-PATH pass: reduce individual tool outputs as they arrive.
	// Unset = true (today's behaviour). Set false to run only the cold-cache sweep below,
	// which is a different economic proposition and deserves its own switch.
	PerOutput *bool `yaml:"per_output"`
	// ColdCache configures the whole-transcript sweep on a turn whose prompt cache has
	// expired. Off by default.
	ColdCache coldCacheConfig `yaml:"cold_cache"`
	// Context selects how much conversation the extraction prompt carries:
	// goal | recent (default) | full. See contextMode.
	Context string `yaml:"context"`
	// ContextMessages is the N for `context: recent` (0 = 7).
	ContextMessages int `yaml:"context_messages"`
	// Aggressiveness selects the compaction target taught to the model: low | medium
	// (default) | high. It changes what the model is ASKED for, never what is ACCEPTED —
	// the verbatim-preservation and strictly-smaller checks are identical at every level,
	// and in rewrite: false the subsequence proof still holds. It is part of the result
	// cache key, so switching levels misses rather than replaying the other level's answer.
	Aggressiveness string `yaml:"aggressiveness"`
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
	}
	if err := components.Decode(raw, &cfg); err != nil {
		return nil, err
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
	aggro, err := extract.ParseAggressiveness(cfg.Aggressiveness)
	if err != nil {
		return nil, fmt.Errorf("extract_llm: %w", err)
	}
	ctxMode, err := parseContextMode(cfg.Context)
	if err != nil {
		return nil, fmt.Errorf("extract_llm: %w", err)
	}
	perOutput := true
	if cfg.PerOutput != nil {
		perOutput = *cfg.PerOutput
	}
	if cfg.ColdCache.MinTokens <= 0 {
		cfg.ColdCache.MinTokens = defaultColdFloor
	}
	if !perOutput && !cfg.ColdCache.Enabled {
		return nil, fmt.Errorf("extract_llm: per_output: false with cold_cache disabled " +
			"leaves the component with nothing to do; remove it from the pipeline instead")
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
		llmMaxPerSess: cfg.LLMMaxPerSess, fireOnSize: fireOnSize, aggro: aggro,
		ctxMode: ctxMode, ctxMessages: cfg.ContextMessages,
		perOutput: perOutput, cold: cfg.ColdCache,
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

// sweepThisRequest reports whether this turn gets the whole-transcript sweep: the operator
// enabled it, the provider's cache has certainly expired, and any extra idle requirement is
// met. Everything it unlocks (rewriting at depth, pricing at the write rate) is only correct
// when the cache really is gone, so all three must hold.
func (e *ExtractLLM) sweepThisRequest(c *components.Ctx) bool {
	if !e.cold.Enabled || c == nil || !c.ColdCache {
		return false
	}
	if e.cold.MinIdleSeconds > 0 && c.IdleMs < int64(e.cold.MinIdleSeconds)*1000 {
		return false
	}
	return true
}

// extractionContext renders the conversation the extraction prompt will carry, in the
// configured mode. One method so every caller (and every test) agrees on what the model is
// told — the prompt's relevance judgement rests entirely on this.
func (e *ExtractLLM) extractionContext(req *bschemas.BifrostChatRequest, sweeping bool) string {
	mode := e.ctxMode
	if sweeping {
		// The sweep judges the WHOLE transcript at once, so it gets the whole conversation
		// whatever the configured mode: deciding what a message three hours back may lose
		// requires knowing what happened since.
		mode = ctxFull
	}
	return conversationContext(req, mode, e.ctxMessages)
}

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

// llmTruncated counts extraction replies that stopped at the output cap. Its own counter
// because it is the one failure whose fix is a config change rather than a decision to stop
// calling, and it was previously invisible: a truncated program parses as nothing, which
// looks exactly like a model that declined to compact.
var llmTruncated int64

// LLMTruncated returns the number of extraction replies cut off at the output cap.
func LLMTruncated() int64 { return atomic.LoadInt64(&llmTruncated) }

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

// pricingFor is the extraction model's REAL rates where the host could resolve them.
//
// The built-in default is haiku-class, while the shipped model.source is `incoming` — the
// agent's own model — so on a sonnet-class agent the default understates every call by about
// 3x, and the gate spends against that number. MEASURED: a call recorded at $0.0276 had cost
// about $0.083. Hence the agent's own rates when the agent is what compacts.
//
// But ONLY then. `model.model` re-points an incoming-source client at a cheap model on the
// same endpoint, and applying the agent's rates to those calls is the same error in the other
// direction: on the deployment where this was found opus is 4.75x haiku, the per-call figure
// on the Components tab disagreed with the request row by exactly that factor, and a
// configuration that pays read as one that loses money. When a model is named, its rates
// govern.
func (e *ExtractLLM) pricingFor(c components.Ctx) cheapmodel.Pricing {
	if e.modelSource == "config" || e.modelName != "" || c.SelfRates.Zero() {
		return e.pricing
	}
	return cheapmodel.Pricing{
		InputPerMTok:      c.SelfRates.Input * 1_000_000,
		OutputPerMTok:     c.SelfRates.Output * 1_000_000,
		CacheReadPerMTok:  c.SelfRates.CacheRead * 1_000_000,
		CacheWritePerMTok: c.SelfRates.CacheWrite * 1_000_000,
	}
}

func (e *ExtractLLM) Offload(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	// Resolved once: the candidate loop below tests it per tool output, and it is a
	// handler call rather than a field read.
	dbg := debugExtractLLM(c)
	sweeping := e.sweepThisRequest(c)
	if !e.perOutput && !sweeping {
		// per_output: false — this component is here only for the cold sweep, and this is a
		// warm turn. Frozen replays below still run: they are free and they are what keeps
		// the prefix byte-stable.
		rep.Gate("per_output_disabled")
	}
	fires := e.trigger.Fires(req, c.CtxWindow)
	goal := e.extractionContext(req, sweeping)
	query := keywords(goal)
	if len(query) == 0 {
		rep.Gate("no_goal_keywords")
		rep.Skipped = true
		return nil, nil
	}
	model := e.modelClient
	if model == nil {
		// ForModel, not For: `model.model` names the model to COMPACT with even when the
		// source is the incoming request. Without that, compaction on a coding agent runs on
		// the agent's frontier model, and the arithmetic never closes — a real cold-cache
		// sweep measured here cut the provider bill by $0.63 and spent $1.25 of opus doing
		// it. Same endpoint, same credential, cheap model.
		var usedSource string
		model, usedSource = c.Model.ForModelSource(e.modelSource, e.modelName)
		// The fallback from `incoming` to the static model is a DIFFERENT credential on a
		// DIFFERENT endpoint, so it cannot be silent: an operator whose config says
		// `source: incoming` would otherwise have no way to learn that none of their calls
		// went there. This gate is what makes an authentication failure attributable to the
		// credential that was actually presented.
		if model != nil && usedSource != "" && e.modelSource != "config" && usedSource == "config" {
			rep.Gate("model_source_fell_back_to_config")
		}
	}
	// Per-session cadence: on throttled steps drop the model (skip this request). The sweep
	// is exempt — it happens at most once per idle gap, which is its own throttle, and
	// skipping it means paying the full re-billing of the transcript instead.
	if model != nil && !sweeping && fires && !e.llmAllowedThisRequest(c.Session) {
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
	if model != nil && !pressureFires && !sweeping {
		model = nil // no model call this request; frozen reapplications still run below
	}
	if model != nil && !e.perOutput && !sweeping {
		model = nil // cold-sweep-only configuration, and this is a warm turn
	}
	if sweeping {
		triggerReason = "cold cache: prompt cache expired, whole transcript re-billed"
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
	if sweeping {
		// The sweep's own floor. Every candidate on this turn is being re-billed at the
		// cache-write rate whatever we do, so the bar for "worth a call" is genuinely lower
		// than on a warm turn.
		floor = e.cold.MinTokens
	}
	// Gate inputs shared by every candidate this request.
	//
	// pricing is the extraction model's REAL rates where the host could resolve them. The
	// built-in default is claude-haiku's, while the shipped model.source is `incoming` — the
	// agent's own model — so on a sonnet-class agent the fallback understates every call by
	// about 3x, and the gate spends on that number. MEASURED on a real session: a call
	// recorded at $0.0276 had cost about $0.083.
	pricing := e.pricingFor(*c)
	// The model id actually used, for the record. `source: incoming` pins no name, so without
	// this every recorded call said model="".
	callModel := e.modelName
	if callModel == "" {
		callModel = c.ModelName
	}
	val := savedTokenValue(c)
	ratio := e.ratios.ratio()
	turnsSoFar := len(req.Input)
	extCfg := extract.DefaultCfg()
	extCfg.Mode, extCfg.Floor, extCfg.Rewrite = e.strategy, floor, e.rewrite
	extCfg.Aggressiveness = e.aggro

	// Keep-ids are harvested from the AGENT's OWN WORDS, never from the tool outputs — even
	// when the prompt's context includes them.
	//
	// The keep-list means "identifiers the agent referenced recently, so do not lose them".
	// Harvesting it from a context that CONTAINS the candidate turns every unique token in the
	// noise into a required identifier, and then no reduction can pass: a single timestamp
	// appearing once in a 900-line log becomes something the result must reproduce verbatim.
	//
	// MEASURED, three samples each on the same 26k-token access log: with a small
	// conversation-only context 2/3 extractions were accepted; with a full-transcript context
	// 0/3 were, and the failures were "rejected by the acceptance check". That is the cold
	// sweep's own configuration (it forces context: full), so this was the difference between
	// the sweep working and the sweep being an expensive no-op.
	keepIDs := extract.HarvestIdentifiers(conversationContext(req, ctxRecent, e.ctxMessages), 40)
	// Per-call context budget (constant across this request's candidates): the extraction
	// model's input limit, and the prompt's fixed cost around the tool output itself.
	inputLimit := e.inputLimit(c)
	promptOverhead := extractPromptOverheadTokens + schema.TextTokens(goal)
	// MODEL ESCALATION, sweep only. The sweep sends the whole transcript as context, so the
	// prompt's fixed part alone can exceed a small extraction model's window — and then
	// fitsModelContext correctly declines every candidate and the sweep silently does
	// nothing on exactly the largest, most expensive transcripts. When that happens, fall
	// back to the model the AGENT is using: it demonstrably holds this conversation, since
	// it is about to be sent the same one.
	escalated := false
	if sweeping && model != nil && !fitsModelContext(0, promptOverhead, inputLimit) {
		if inc := c.Model.For("incoming"); inc != nil && c.CtxWindow > inputLimit {
			model, inputLimit, escalated = inc, c.CtxWindow, true
			// The call is now going to a DIFFERENT model, so the two things derived from
			// which model it is must be re-derived. Leaving them meant an escalated call was
			// recorded under the pinned cheap model's id and priced at its rates — the exact
			// ~3x understatement the pricing block above exists to remove, reintroduced on
			// the most expensive calls the component makes.
			if !c.SelfRates.Zero() {
				pricing = cheapmodel.Pricing{
					InputPerMTok:      c.SelfRates.Input * 1_000_000,
					OutputPerMTok:     c.SelfRates.Output * 1_000_000,
					CacheReadPerMTok:  c.SelfRates.CacheRead * 1_000_000,
					CacheWritePerMTok: c.SelfRates.CacheWrite * 1_000_000,
				}
			}
			if c.ModelName != "" {
				callModel = c.ModelName
			}
			if dbg {
				logging.From(c.Ctx).Debug("cg.extract_llm.escalate",
					"reason", "transcript exceeds the extraction model's window",
					"overhead_tokens", promptOverhead, "window", c.CtxWindow)
			}
		}
	}
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
		// gate is what the economic gate concluded for this candidate — including when the
		// conclusion was overridden. Recorded per call so an operator who turned the gate
		// advisory can still see what it would have refused.
		gate string
	}
	var cands []cand
	// skip_file_reads is TRI-STATE, and unset really means AUTO.
	//
	// It did not. `skipFR := false` made unset mean "always reduce", while this file's own
	// config comment and docs/components/extract_llm.md both document unset as AUTO: skip
	// line-numbered source dumps when the request is prompt-cached, reduce them otherwise.
	// The default therefore defeated the entire measured rationale for the flag — on a
	// ~98%-cached agent a file read already bills at the cache-read rate, so skeletonizing it
	// saves almost nothing and costs a model call plus a one-time cache-write transition,
	// measured at +30% billed cost. Live confirmation of the same shape: a real session sent a
	// ~7k-token Go file read to the model, which spent 40 s on a reply that hit the output cap
	// and saved nothing.
	//
	// A cold sweep is the exception in the other direction: nothing is cached, file reads are
	// the largest mass in a coding transcript, and every token of them is being re-billed at
	// the cache-write rate — so AUTO reduces them there.
	skipFR := c.CacheAware && !sweeping
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
		// The tail gate exists to protect a LIVE cached prefix. On a sweep there is none —
		// the provider's entry expired, so this turn re-writes the whole transcript into a
		// new cache entry whatever we do, and a message at depth is exactly as free to
		// rewrite as one in the tail. This is the only place that restriction is lifted, and
		// only because the condition it protects against is provably absent.
		if c.CacheAware && !sweeping && !c.TailOnly(i) {
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
		gateReason := "gate off"
		if e.gate {
			// Stop exploring once calls are observed to be slow: exploration spends wall
			// clock as well as money, and an agent on a task deadline feels the former more.
			explore := !tooSlowToExplore(metrics.ExtractionAvgLatencyMs()) &&
				e.ratios.exploring(c.Session)
			// promptOverhead, not the 200-token constant: it already counts the rendered
			// conversation context, which under `context: full` (every cold sweep) IS the
			// prompt. Measured on production: five haiku calls on ONE request each sent
			// ~138,000 prompt tokens while the gate priced them at <=6,663 — 21x to 31x low,
			// which is what let the sweep spend $0.71 to remove 63 tokens worth $0.0003.
			d := evaluateGate(sz, ratio, val, callCost(pricing, sz, promptOverhead), seenBefore, turnsSoFar,
				explore, e.allowCached)
			if !d.allow && e.fireOnSize {
				// ADVISORY: `fire_on: size` is the operator taking the spending decision
				// away from the gate, so record what the gate would have refused — the
				// counterfactual is the only way anyone later sees what this cost — and
				// then proceed anyway. Deliberately does NOT bump the suppressed counter:
				// nothing was suppressed, and inflating that would make the gate look
				// like it was working when it has been overridden.
				d.reason = "advisory: " + d.reason
				metrics.RecordExtractionReason(d.reason)
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
			gateReason = d.reason
		}
		cands = append(cands, cand{i: i, content: content, id: id, gate: gateReason})
	}
	if dbg && len(tools) > 0 {
		logging.From(c.Ctx).Debug("cg.extract_llm", "tools", len(tools), "cands", len(cands),
			"reapplied", dbgReapply, "skip_placeholder", dbgPlace, "skip_tail", dbgTail,
			"skip_floor", dbgFloor, "max_output_tokens", dbgMaxSz, "big_but_not_tail", dbgBigTailBlocked,
			"cacheAware", c.CacheAware, "maxCachedIdx", c.MaxCachedIdx, "floor", floor,
			"nInput", len(req.Input))
	}
	// Caps. The sweep is bounded by its OWN cap and does not draw on the hot path's
	// per-request or per-session allowance: the two paths are switched independently and
	// have opposite economics, so letting a sweep drain the session budget would silently
	// disable the hot path (or the reverse) depending on which fired first.
	if sweeping {
		if e.cold.MaxCalls > 0 && len(cands) > e.cold.MaxCalls {
			for k := e.cold.MaxCalls; k < len(cands); k++ {
				rep.Gate("over_cold_sweep_cap")
			}
			cands = cands[:e.cold.MaxCalls]
		}
	} else {
		if e.llmMaxPerReq > 0 && len(cands) > e.llmMaxPerReq {
			for k := e.llmMaxPerReq; k < len(cands); k++ {
				rep.Gate("over_per_request_cap")
			}
			cands = cands[:e.llmMaxPerReq] // cap model calls per request
		}
		// Then the session's own allowance. Reserved here, after the per-request cap,
		// because every surviving candidate becomes exactly one model call in phase 2
		// below — so the reservation is the spend.
		if n := e.reserveSessionBudget(c.Session, len(cands)); n < len(cands) {
			for k := n; k < len(cands); k++ {
				rep.Gate("over_per_session_cap")
			}
			cands = cands[:n]
		}
	}

	// Phase 2 (parallel): the candidate compactions are independent. A focused per-output
	// prompt ("trim THIS one output") gives a much better reduction than a single program
	// over the whole heterogeneous batch (measured on SWE: ~3× more tokens saved), and
	// running them concurrently (bounded) keeps a turn's cost to ~one call's wall time —
	// so parallel beats a single-call batch on tokens AND latency. Each output fails open
	// independently (a miss leaves that one verbatim).
	if len(cands) > 0 {
		// The conversation context is identical for every candidate in this request, so with
		// two or more calls it is worth writing into the provider's cache once and reading it
		// back on the rest. With ONE call there is nothing to read it back, and a cache write
		// costs 1.25x fresh — so paying for it would be a 25% loss. Decided here because this
		// is the only place the final candidate count is known (the caps above trim it).
		extCfg.CacheContext = len(cands) > 1
		type outT struct{ projected, summary string }
		out := make([]outT, len(cands))
		// One record per call, written to its own slot so the goroutines need no lock (a
		// Report is copied by value across this codebase and cannot carry one).
		calls := make([]components.ModelCall, len(cands))
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
				// A per-CALL accounting window nested inside the request's, so this one
				// call's tokens and cost are attributable without hiding them from the
				// request's own bill (see cheapmodel.WithCallSink).
				ctx, callSink := cheapmodel.WithCallSink(ctx)
				before := schema.TextTokens(cands[k].content)
				start := time.Now()
				res, sum, strategy, why := extract.RunExtractionDetail(ctx, cands[k].content, goal,
					keepIDs, before, extCfg, model)
				latency := float64(time.Since(start).Milliseconds())
				metrics.RecordExtractionCall(latency)
				_, inTok, outTok := callSink.Totals()
				cw, cr := callSink.CacheTotals()
				calls[k] = components.ModelCall{
					Component: rep.Component, Model: callModel,
					Strategy: strategy, Aggressiveness: string(e.aggro),
					Cold: sweeping, Escalated: escalated,
					CandidateTokens: before, LatencyMs: latency,
					PromptTokens: inTok, CompletionTokens: outTok,
					CacheRead: cr, CacheWrite: cw,
					CostUSD:    pricing.Cost(inTok, outTok, cw, cr),
					GateReason: cands[k].gate,
					Before:     cands[k].content,
					Rejection:  why,
				}
				// A reply that stopped exactly at the output cap was TRUNCATED, so the
				// Starlark program is incomplete, unparseable, and the whole call — its
				// money and its seconds — bought nothing. It is indistinguishable from
				// "the model declined to shrink this" in the return value, and the two
				// have opposite fixes: raise the cap versus stop calling. MEASURED on a
				// real session: 26.8s and ~$0.08 for a reply cut off at 2048 tokens.
				if outTok >= int64(cheapExtractOutputTokens) {
					calls[k].GateReason = "reply truncated at the output cap: " + calls[k].GateReason
					rep.Gate("reply_truncated")
					atomic.AddInt64(&llmTruncated, 1)
				}
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
					calls[k].Accepted = true
					calls[k].SavedTokens = before - schema.TextTokens(res)
					calls[k].Summary, calls[k].After = sum, res
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
		rep.Calls = calls      // serial: every goroutine has returned
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
			// Not published globally when the call escalated to the agent's model: the global
			// key is built from e.modelName, so a result derived by a DIFFERENT model would
			// be served to other sessions under the configured model's key. Session-scoped
			// reuse (the replay that keeps this session's prefix stable) is unaffected.
			if escalated {
				continue
			}
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

func init() {
	f := []components.Field{
		{Key: "per_output", Type: components.FieldBool, Default: true,
			Hint: "The HOT-PATH pass: reduce individual tool outputs as they arrive. With this off and cold_cache.enabled off the component has nothing to do and refuses to build — take it out of the pipeline instead."},
		{Key: "fire_on", Type: components.FieldEnum, Default: "pressure", Options: []string{"pressure", "size"},
			Hint: "What decides a request is worth a model call. pressure = the derived context-pressure trigger. size = fire whenever any candidate clears min_tokens, which ALSO demotes the economic gate and the caching-backend guard to advisory — a deliberate licence to spend, so set the caps first."},
		{Key: "min_tokens", Type: components.FieldInt, Default: 300, Min: 1,
			Hint: "Per-output floor. Setting it pins the trigger to your number instead of the derived pressure curve. Under fire_on: size an unset floor is raised to 2000, because 300 is a threshold nearly every output clears."},
		{Key: "strategy", Type: components.FieldEnum, Default: "code", Options: extract.Modes,
			Hint: "How the extraction is produced. code = model-written Starlark filter over the body; single = one JSON-returning call; rlm = chunked, for very large bodies; deterministic = NO model call at all; auto picks by size."},
		{Key: "aggressiveness", Type: components.FieldEnum, Default: "medium",
			Options: []string{string(extract.AggroLow), string(extract.AggroMedium), string(extract.AggroHigh)},
			Hint:    "The compaction target taught to the model. It changes what is ASKED for, never what is ACCEPTED — the verbatim-preservation and strictly-smaller checks are identical at every level."},
		{Key: "context", Type: components.FieldEnum, Default: "recent", Options: []string{"goal", "recent", "full"},
			Hint: "How much conversation the extraction prompt carries: just the goal, the recent N messages, or the whole transcript."},
		{Key: "context_messages", Type: components.FieldInt, Default: defaultContextMessages,
			Hint: "The N for context: recent (0 = 7)."},
		{Key: "llm_every_n_requests", Type: components.FieldInt, Default: 1,
			Hint: "Throttle: fire at most once per N requests in a session (0 or 1 = every request)."},
		{Key: "llm_max_per_request", Type: components.FieldInt,
			Hint: "Cap model calls for one request. 0 = UNLIMITED, which is a real choice and not an unset value."},
		{Key: "llm_max_per_session", Type: components.FieldInt,
			Hint: "Cap model calls for the whole session. 0 = UNLIMITED. The per-request cap alone cannot bound a long session (2 calls x 300 turns is 600 calls), so with fire_on: size this is the outer bound on spend."},
		{Key: "allow_on_caching_backend", Type: components.FieldBool,
			Hint: "Re-enable extraction on prompt-caching backends. Unset = FALSE, and the economic gate then hard-declines every candidate whose tokens are prompt-cached — on Claude Code against Anthropic that is the whole workload, which is how a fully configured component ran 251 times and acted 0 times."},
		{Key: "economic_gate", Type: components.FieldBool, Default: true,
			Hint: "Only call the model when the expected saving exceeds the expected call cost, priced from real rates. Turning it off restores spend-on-size, needed only to reproduce old benchmark numbers."},
		{Key: "rewrite", Type: components.FieldBool, Default: true,
			Hint: "Let the program reword and summarize rather than only delete. Off forces verified deletion-only (the containment proof); ids, paths and errors are required verbatim either way."},
		{Key: "skip_file_reads", Type: components.FieldBool,
			Hint: "Leave line-numbered source dumps verbatim. Unset = AUTO: skip them when the request is prompt-cached (where reducing them measured +30% billed cost), reduce them otherwise."},
		{Key: "model_max_input_tokens", Type: components.FieldInt,
			Hint: "Pin the EXTRACTION model's input budget, for a model id the static table cannot name (a self-hosted id, or a gateway alias). Unset = resolved per model."},
		markerModeField(),
		{Key: "cold_cache.enabled", Type: components.FieldBool,
			Hint: "The whole-transcript sweep on a turn whose prompt cache has EXPIRED. Measured here: those turns were 4% of requests and 31% of spend, and removing a token on one is worth 12.5x what it is worth on a warm turn."},
		{Key: "cold_cache.min_tokens", Type: components.FieldInt, Default: defaultColdFloor, Min: 1,
			Hint: "Per-output floor for the sweep. Lower than the hot path's, because on that turn every candidate is being re-billed at the write rate anyway."},
		{Key: "cold_cache.min_idle_seconds", Type: components.FieldInt,
			Hint: "Demand MORE idle time than the provider TTL implies (0 = just the TTL). Raises the bar, never lowers it."},
		{Key: "cold_cache.max_calls", Type: components.FieldInt,
			Hint: "Cap model calls for one sweep. 0 = unlimited, which is the default because the sweep runs once per idle gap on a turn that is already expensive."},
	}
	f = append(f, modelFields("model")...)
	components.RegisterFields("extract_llm", extractLLMConfig{}, append(f, components.TriggerFields("trigger")...))
}
