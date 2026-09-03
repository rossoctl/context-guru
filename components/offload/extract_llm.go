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

	"golang.org/x/sync/singleflight"

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
	maxChars    int

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
	// o200k_base — the extraction model tokenizes the same bytes heavier (Anthropic's
	// tokenizer is not published, self-hosted models vary), so a count that is exact for GPT
	// is only an estimate for anyone else.
	//
	// MEASURED 2026-08-19, identical bytes on both sides of the wire: o200k counted 6,396;
	// claude-haiku-4-5 billed 8,222 (1.29x) and aws/claude-sonnet-5 billed 10,574 (1.65x).
	// The old 1.15 under-stated BOTH, and the direction matters: this margin's only job is
	// keeping a request inside a window, where under-counting puts a prompt on the wire the
	// upstream may reject. So it takes the conservative-high end of the measurement, not the
	// mean. Cost estimation uses the same measurement at the haiku end — see
	// extract_econ.go's realTokenMarkup, which wants accuracy rather than headroom.
	// Re-measure with the profile's live probe whenever the extraction model family changes.
	extractContextMargin = 1.65
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
//
// effSource is the source the extraction model was ACTUALLY resolved from, which is not always
// the configured one: ModelSpec falls back from `incoming` to the static client whenever no
// incoming client could be built (the proxy returns nil when no usable credential is on the
// request). Sizing the prompt by e.modelSource instead would then hand the REQUEST model's window
// to a call that is really going to the small static model — over-estimating, on a coding agent by
// as much as 1M against 200k, which is the direction fitsModelContext calls the costly one: the
// request goes out, the upstream rejects it, and the round-trip buys nothing. Pass "" when the
// effective source is not known and the configured one is used as before.
//
// A CONFIG-PINNED client needs no correction and never reaches the effSource branch, which is worth
// stating because it looks like a second path that could resurrect the mismatch: Offload threads
// effSource only through the `model == nil` branch, so a client resolved from e.modelClient keeps
// the configured source. It is safe because modelConfig.Client() requires model.model to be
// non-empty, so e.modelName is always set whenever e.modelClient is, and the e.modelName != ""
// branch below (the static-table lookup) short-circuits before effSource or CtxWindow is consulted.
// Raised in review on #110; recorded here so the next reader need not re-derive it.
func (e *ExtractLLM) inputLimit(c *components.Ctx, effSource string) int {
	if effSource == "" {
		effSource = e.modelSource
	}
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
	if effSource != "config" && c.CtxWindow > 0 {
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
	// Context selects how much conversation the extraction prompt carries:
	// goal | recent (default) | full. See contextMode.
	Context string `yaml:"context"`
	// ContextMessages is the N for `context: recent` (0 = defaultContextMessages).
	ContextMessages int `yaml:"context_messages"`
	// MaxChars bounds the deterministic projection's window (0 = the extractor's default).
	//
	// Exposed because it is the size of the largest thing the model-free fallback can return,
	// and it was a hardcoded 4,000 that quietly became the modal output: 25 of 62 accepted
	// production results were exactly 4,000 characters. The window is line-aligned and marked
	// now, so raising this trades a bigger honest fragment against prompt cost; a result that
	// hits the cap with nothing saying so is refused whatever this is set to.
	MaxChars int `yaml:"max_chars"`
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

// movedToSweep names the keys the cold-sweep split took out of this component, and where each one
// went. They are refused rather than ignored: `cold_cache: {enabled: true}` silently accepted would
// read as "the sweep is on" while nothing swept, which is the most expensive possible misreading of
// this config — the sweep exists for the turns that are 4% of requests and 31% of spend.
var movedToSweep = []struct {
	key, why string
}{
	{"per_output", "this component now IS the warm/tail pass, so there is nothing to switch off; " +
		"remove the key. The cold sweep is a separate component in the pipeline"},
	{"cold_cache", "the whole-transcript sweep is now the `extract_llm_sweep` component. " +
		"cold_cache.min_tokens becomes its min_tokens, cold_cache.min_idle_seconds its " +
		"min_idle_seconds, and cold_cache.max_calls its max_calls (which now bounds BATCH calls); " +
		"cold_cache.enabled becomes the component's presence in the pipeline"},
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
	// KEYS THAT MOVED TO extract_llm_sweep, refused BEFORE components.Decode's KnownFields
	// rejects them with a generic yaml message. Breaking existing configs is deliberate — there is
	// one deployment and it is migrated by hand — but a removed key must say where it went, or the
	// operator reads "field not found" and concludes the key was a typo rather than relocated.
	if len(raw) > 0 {
		var probe map[string]yaml.Node
		if err := yaml.Unmarshal(raw, &probe); err == nil {
			for _, m := range movedToSweep {
				if _, present := probe[m.key]; present {
					return nil, fmt.Errorf("extract_llm: %s has moved to the extract_llm_sweep "+
						"component: %s", m.key, m.why)
				}
			}
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
		ctxMode: ctxMode, ctxMessages: cfg.ContextMessages, maxChars: cfg.MaxChars,
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

// extractionContext renders the conversation the extraction prompt will carry, in the
// configured mode. One method so every caller (and every test) agrees on what the model is
// told — the prompt's relevance judgement rests entirely on this.
// THE SWEEP NO LONGER FORCES `full`, and that is the single largest change to this
// component's economics. It used to, on the argument that judging a message three hours back
// requires knowing what happened since. The argument does not survive measurement:
//
//   - `full` IS the whole request. Measured at 127 turns: 138,596 context tokens against a
//     138,341-token request — 99% of the sweep's prompt is a copy of the transcript it is
//     compacting, sent once per candidate.
//   - the break-even removal R* at k=4 is 113,286 tokens under `full` and 6,833 under
//     `recent` — a 16x lower bar. Above R*/T = 1 the sweep must delete more than the whole
//     transcript to pay, which is structurally impossible; `full` sits there.
//   - the one real counter-argument was about KEEP-IDS: a full-transcript context took
//     acceptance from 3/4 to 0/6, because every unique token in the noise became a required
//     identifier. That is now separable and already separated — HarvestIdentifiers below
//     reads `ctxRecent` explicitly, never this string.
//   - measured on the cold corpus (bench/cold.jsonl, 8 requests, verified cache_read=0):
//     `full` sent 36,686 prompt tokens for one candidate of 15,473 and cost $0.0387 to
//     remove 0 tokens; `recent` is in the table in docs/components/extract_llm.md.
//
// An operator who wants the old behaviour writes `context: full`, which now means what it
// says on every turn instead of being imposed on one.
func (e *ExtractLLM) extractionContext(req *bschemas.BifrostChatRequest) string {
	return conversationContext(req, e.ctxMode, e.ctxMessages)
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
// It returns onCard so the caller needs only ONE rate-card lookup per request. That matters on the
// request path: modelinfo.LiteLLM.Price takes a mutex, may refresh, and on a key that is not an exact
// match falls back to an O(n) scan of every priced model — so asking twice for the same answer is a
// real cost, not a style point.
func (e *ExtractLLM) pricingFor(c components.Ctx) (p cheapmodel.Pricing, onCard bool) {
	// A NAMED compaction model, priced from the operator's own card. This is the branch that
	// used to fall through to CHEAP_MODEL_PRICE_* — i.e. to haiku LIST rates — and it is the
	// common case, because naming a cheap model is the whole point of the config. The card is
	// the same one requests.cg_llm_cost_usd is computed from, so the gate now spends against
	// the number that reaches the invoice and the two recorded totals agree.
	if e.modelName != "" {
		if r := c.RatesFor; r != nil {
			if rates := r(e.modelName); !rates.Zero() {
				return ratesPricing(rates), true
			}
		}
		return e.pricing, false
	}
	if e.modelSource == "config" || c.SelfRates.Zero() {
		return e.pricing, false
	}
	return ratesPricing(c.SelfRates), false
}

// ratesPricing converts the host's per-token card into cheapmodel's per-MTok form.
func ratesPricing(r components.TokenRates) cheapmodel.Pricing {
	return cheapmodel.Pricing{
		InputPerMTok:      r.Input * 1_000_000,
		OutputPerMTok:     r.Output * 1_000_000,
		CacheReadPerMTok:  r.CacheRead * 1_000_000,
		CacheWritePerMTok: r.CacheWrite * 1_000_000,
	}
}

// extractInflight collapses concurrent extractions of identical content into one model call.
// Keyed on extract.ResultKey — the same key the persistent cross-session cache uses — so the
// in-flight window and the stored window agree on what "identical" means.
var extractInflight singleflight.Group

// inflightDeduped counts calls avoided because an identical extraction was already running.
var inflightDeduped atomic.Int64

func (e *ExtractLLM) Offload(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	// Resolved once: the candidate loop below tests it per tool output, and it is a
	// handler call rather than a field read.
	dbg := debugExtractLLM(c)
	fires := e.trigger.Fires(req, c.CtxWindow)
	goal := e.extractionContext(req)
	query := keywords(goal)
	if len(query) == 0 {
		rep.Gate("no_goal_keywords")
		rep.Skipped = true
		return nil, nil
	}
	model := e.modelClient
	// The source the model is ACTUALLY resolved from, which the prompt budget below must be
	// sized against rather than the configured one — see inputLimit.
	effSource := e.modelSource
	if model == nil {
		// ForModel, not For: `model.model` names the model to COMPACT with even when the
		// source is the incoming request. Without that, compaction on a coding agent runs on
		// the agent's frontier model, and the arithmetic never closes — a real cold-cache
		// sweep measured here cut the provider bill by $0.63 and spent $1.25 of opus doing
		// it. Same endpoint, same credential, cheap model.
		var usedSource string
		model, usedSource = c.Model.ForModelSource(e.modelSource, e.modelName)
		if usedSource != "" {
			effSource = usedSource
		}
		// The fallback from `incoming` to the static model is a DIFFERENT credential on a
		// DIFFERENT endpoint, so it cannot be silent: an operator whose config says
		// `source: incoming` would otherwise have no way to learn that none of their calls
		// went there. This gate is what makes an authentication failure attributable to the
		// credential that was actually presented.
		if model != nil && usedSource != "" && e.modelSource != "config" && usedSource == "config" {
			rep.Event("model_source_fell_back_to_config")
		}
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
	metrics.RecordExtractionReason(rep.Component, triggerReason)

	floor := e.outputFloor(c.CtxWindow)
	// Without an explicit min_tokens, derive the per-output floor from context pressure so
	// there is no per-workload number to pick (#28 E).
	if !e.minTokensSet && !e.fireOnSize {
		if pf := pressureFloor(c.CtxWindow, pressure); pf > 0 {
			floor = pf
		}
	}
	// Gate inputs shared by every candidate this request.
	//
	// pricing is the extraction model's REAL rates where the host could resolve them. The
	// built-in default is claude-haiku's, while the shipped model.source is `incoming` — the
	// agent's own model — so on a sonnet-class agent the fallback understates every call by
	// about 3x, and the gate spends on that number. MEASURED on a real session: a call
	// recorded at $0.0276 had cost about $0.083.
	pricing, onCard := e.pricingFor(*c)
	// SAY SO WHEN THE RATE CARD IS A GUESS. When a cheap model is named, its own rates govern
	// (pricingFor), and those rates come from CHEAP_MODEL_PRICE_* — which is unset on this
	// deployment, so the gate spends against haiku LIST ($1/$5 per MTok) while the dashboard
	// prices the same request from the operator's card ($0.80/$4.00), ~25% apart. The gate
	// cannot resolve the operator's card itself (no price table reaches a component), so the
	// remaining honest move is to make the divergence visible on every request that spends
	// on it rather than let a silent 25% sit under every allow/suppress decision.
	// Only when the card could not answer EITHER — RatesFor is the operator's own price table
	// and it is what the bill is computed from, so a hit there is not a guess. Before it was
	// reachable this gate fired on every single request, which meant it named a real defect
	// and yet carried no information about which requests it applied to.
	if e.modelName != "" && !cheapmodel.PricingConfigured() && !onCard {
		rep.Gate("cheap_model_price_unconfigured")
	}
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
	if e.maxChars > 0 {
		extCfg.MaxChars = e.maxChars
	}

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
	// 0/3 were, and the failures were "rejected by the acceptance check". That WAS the cold
	// sweep's own configuration, back when the sweep forced context: full — so this was the
	// difference between the sweep working and the sweep being an expensive no-op. Harvesting
	// from ctxRecent explicitly is also what makes the sweep's context mode a free choice
	// again (see extractionContext): the keep-list no longer moves when the context does.
	keepIDs := extract.HarvestIdentifiers(conversationContext(req, ctxRecent, e.ctxMessages), 40)
	// Per-call context budget (constant across this request's candidates): the extraction
	// model's input limit, and the prompt's fixed cost around the tool output itself.
	inputLimit := e.inputLimit(c, effSource)
	promptOverhead := extractPromptOverheadTokens + schema.TextTokens(goal)
	// The same prompt, for the COST model rather than the window check: callCost adds the
	// static preamble itself, so it must be given only the variable part.
	goalOverhead := promptOverheadTokens + schema.TextTokens(goal)
	tools := toolIndices(req)
	var keys []string
	changed := 0

	// apply splices a compacted projection + marker into message i (serial: store writes
	// and message mutation are not concurrency-safe).
	//
	// IT REPORTS WHETHER THE SPLICE HAPPENED, and every caller must gate on that. It can
	// decline for three reasons now — the projection does not shrink, the marker-inclusive
	// never-worse check fails, or the store's rewind reserve refuses the payload — and after
	// #188 the last of those makes it a silent no-op on a path whose callers used to assume it
	// always acted. What they recorded ahead of it (a frozen cg:res: decision, a token-savings
	// metric, a replay event) then described a splice that did not occur. See commitMark.
	//
	// replay=true marks a decision this session already stamped and sent on an earlier turn.
	// Then the payload write is a REFRESH, which must never refuse: declining would send the
	// message verbatim where the provider's cached prefix holds the compacted bytes, which is
	// the cache-destructive direction and cannot un-send the marker anyway. See commitRefresh.
	apply := func(i int, content, projected, summary string, replay bool) bool {
		if projected == "" || schema.TextTokens(projected) >= schema.TextTokens(content) {
			return false
		}
		hint := " [full output: call " + expand.ToolName + "]"
		newText, key, eff, ok := tryMark(c, e.mode, content, hint, func(tok string) string {
			if summary != "" {
				return projected + "\n[" + summary + "] " + tok
			}
			return projected + "\n" + tok
		})
		if !ok {
			return false
		}
		if replay {
			// Never refuses; a false answer means the payload is gone and the marker being
			// replayed is dangling. Counted there, and the replay proceeds regardless. It also
			// owns rep.Irreversible for the degraded marker modes, which commitMark used to set
			// on this path — see commitRefresh.
			commitRefresh(c, rep, eff, key, content)
			recordOwner(c, key)
		} else if !commitMark(c, rep, eff, key, content) {
			return false // the store cannot back the marker; leave this output verbatim
		}
		schema.SetMessageText(&req.Input[i], newText)
		changed++
		if key != "" {
			keys = append(keys, key)
		}
		return true
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
		// noWindow withholds the model-free character window for content whose class says a
		// window cannot be a faithful reduction of it. See minWindowRatio.
		noWindow bool
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
	skipFR := c.CacheAware
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
			metrics.RecordExtractionCacheLookup(rep.Component, true)
			// A REPLAY is where the amortization actually happens, so credit it — at the rate
			// a re-sent token would have been billed at, which on a caching backend is the
			// cache-read rate. This is the other half of the honest net figure: the first
			// application alone under-reports the value, and pricing the replays at the first
			// application's rate over-reports it by 12.5x.
			// Every one of these three describes a splice, so all three wait for it. Before
			// #188 apply always acted, so recording ahead of it was merely untidy; now it can
			// decline, and a run with an exhausted reserve reported replays and token savings
			// that never happened — over-reporting the exact figure the iteration-024 re-run
			// will be judged on.
			if apply(i, content, cached.Projected, cached.Summary, true) {
				if saved := schema.TextTokens(content) - schema.TextTokens(cached.Projected); saved > 0 {
					metrics.RecordExtractionValue(rep.Component, float64(saved)*val.repeatPerToken)
				}
				dbgReapply++
				rep.Replay("reapplied_same_session")
			}
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
				metrics.RecordExtractionCacheLookup(rep.Component, true)
				// A cross-session hit is a NEW decision for THIS session — the comment above
				// is explicit that this session never sent these compacted bytes — so the
				// payload write is a commitMark that may refuse, and the splice comes before
				// the freeze for the same reason it does in phase 3. Freezing a decision this
				// session never spliced is what lets the same-session replay path above splice
				// it at depth on a later turn, inside the cached prefix, which is the harm the
				// tail gate two blocks up exists to prevent.
				if !apply(i, content, cached.Projected, cached.Summary, false) {
					continue
				}
				// Freeze into this session so later turns replay it byte-for-byte from the
				// same-session path above, at any depth.
				putResult(c, id, cached.Projected, cached.Summary)
				dbgReapply++
				rep.Replay("reapplied_cross_session")
				continue
			}
		}
		if sz < floor {
			dbgFloor++
			rep.Gate("below_output_floor") // only medium/large outputs are worth a model call
			continue
		}
		// Count the MISS here, below the floor, so the hit rate has a denominator that means
		// something. Above the floor it counted every tool output too small to ever be an
		// extraction candidate as a cache miss: 124,679 of 133,725 recorded misses in
		// production were below_output_floor, which reported a 2.09% hit rate for a cache
		// whose real rate over reachable candidates is 24.0% — 30 replays per model call, one
		// of the few parts of this component that unambiguously pays. A metric that argues for
		// optimizing something already working is worse than no metric.
		metrics.RecordExtractionCacheLookup(rep.Component, false)
		// The operator's REQUEST-level trigger, honored on every turn this component sees.
		//
		// This condition used to carry a cold-sweep carve-out, and before that `!c.CacheAware`.
		// Both are gone: the sweep is its own component now, and Trigger's zero value fires always
		// (see components/trigger.go — "a zero field is no constraint"), so `!fires` is reachable
		// only when min_request_tokens / min_request_frac / min_messages was set and not met. There
		// is no derived value here for a carve-out to protect; the derived pressure trigger is
		// separate and gates the model earlier via shouldFire.
		//
		// IsHuge still overrides: a single output that large is worth a call whatever the
		// request-level threshold says.
		if huge := e.trigger.IsHuge(sz, c.CtxWindow); !fires && !huge {
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
		// Classify ONCE per candidate. It feeds two decisions — the gate's expected-yield ratio
		// and whether the model-free window is offered at all — and it is ten regexes over the
		// blob head, so asking twice per candidate is real work on the request path.
		cls, clsRatio, clsOK := contentClass(content)
		gateReason := "gate off"
		if e.gate {
			// Stop exploring once calls are observed to be slow: exploration spends wall
			// clock as well as money, and an agent on a task deadline feels the former more.
			explore := !tooSlowToExplore(metrics.ExtractionP50LatencyMs(rep.Component)) &&
				e.ratios.exploring(c.Session)
			// goalOverhead, not promptOverhead: the gate needs the VARIABLE part of the
			// prompt, because callCost adds the static preamble itself. promptOverhead is
			// the window-fitting figure and bundles extractPromptOverheadTokens = 2000,
			// which is "the preamble plus keep-list, rounded" — so passing it here billed
			// the 1,893-token preamble TWICE, a 2,442-billed-token over-estimate that made
			// every call look ~25% dearer than it is and suppressed calls that would pay.
			// The observed-cost reconciliation below hid it in steady state (the same
			// double count sits in analyticBaseline, so the ratio cancels) and not at all
			// before the first observation, which is exactly when the gate decides whether
			// there will ever be one.
			//
			// The rendered conversation still counts, which was the point of the original
			// change: under `context: full` (every cold sweep) the transcript IS the prompt.
			// MEASURED on production: five haiku calls on ONE request each sent ~138,000
			// prompt tokens while the gate priced them at <=6,663 — 21x to 31x low, which is
			// what let the sweep spend $0.71 to remove 63 tokens worth $0.0003.
			// THIS candidate's own measured compression ratio where its content class is
			// recognised, in place of the ratio learned across every class at once. The
			// pooled figure cannot separate a JSON blob that shrinks 2.8% from a directory
			// listing that shrinks 65.5%, so it priced both at the same expected saving —
			// and 31% of the reachable token mass in this workload sits in the two classes
			// that compress worst. See contentclass.go for the table and its provenance.
			candRatio := ratio
			if clsOK {
				candRatio = clsRatio
			}
			// Per CANDIDATE, not per request: a tail candidate is not in the provider's
			// cache and is worth the write rate, not the read rate. See savedTokenValueAt.
			d := evaluateGate(sz, candRatio, savedTokenValueAt(c, i), callCost(pricing, sz, goalOverhead), seenBefore, turnsSoFar,
				explore, e.allowCached)
			if !d.allow && e.fireOnSize {
				// ADVISORY: `fire_on: size` is the operator taking the spending decision
				// away from the gate, so record what the gate would have refused — the
				// counterfactual is the only way anyone later sees what this cost — and
				// then proceed anyway. Deliberately does NOT bump the suppressed counter:
				// nothing was suppressed, and inflating that would make the gate look
				// like it was working when it has been overridden.
				d.reason = "advisory: " + d.reason
				metrics.RecordExtractionReason(rep.Component, d.reason)
				rep.Gate("economic_gate_advisory")
				if dbg {
					logging.From(c.Ctx).Debug("cg.extract_llm.gate", "decision", "advisory",
						"reason", d.reason, "size", sz, "exp_saving_usd", d.expSaving,
						"exp_cost_usd", d.expCost, "cacheAware", c.CacheAware)
				}
				d.allow = true
			}
			if !d.allow {
				metrics.RecordExtractionSuppressed(rep.Component, d.reason)
				// Just the gate name here: the per-reason breakdown already ships in
				// /stats via RecordExtractionSuppressed, and a full sentence makes a
				// poor histogram key.
				rep.Gate("economic_gate")
				if clsOK {
					// WHICH content class was refused, so an operator can answer "why did
					// this not run" without re-deriving the class. This is the counter the
					// content prefilter is visible through: no separate gate, because the
					// decision is the same expected-saving comparison as every other.
					// Cardinality stays bounded by code (one constant prefix x the ten
					// classes in contentclass.go), which is what promexport's gate-label
					// series assumes.
					rep.Gate("low_yield_content_class:" + cls)
				}
				if dbg {
					logging.From(c.Ctx).Debug("cg.extract_llm.gate", "decision", "suppress",
						"reason", d.reason, "size", sz, "exp_saving_usd", d.expSaving,
						"exp_cost_usd", d.expCost, "cacheAware", c.CacheAware)
				}
				continue
			}
			metrics.RecordExtractionReason(rep.Component, d.reason)
			gateReason = d.reason
		}
		// A class whose measured reduction cannot support a fixed-size window must not be
		// offered one: MaxChars 0 leaves the projection unable to shrink, so it fails the
		// strictly-smaller check instead of returning a truncation. Carried per candidate
		// because the class is a property of the content, not of the request.
		noWindow := clsOK && clsRatio < minWindowRatio
		cands = append(cands, cand{i: i, content: content, id: id, gate: gateReason, noWindow: noWindow})
	}
	if dbg && len(tools) > 0 {
		logging.From(c.Ctx).Debug("cg.extract_llm", "tools", len(tools), "cands", len(cands),
			"reapplied", dbgReapply, "skip_placeholder", dbgPlace, "skip_tail", dbgTail,
			"skip_floor", dbgFloor, "max_output_tokens", dbgMaxSz, "big_but_not_tail", dbgBigTailBlocked,
			"cacheAware", c.CacheAware, "maxCachedIdx", c.MaxCachedIdx, "floor", floor,
			"nInput", len(req.Input))
	}
	// Caps. extract_llm_sweep is bounded by its OWN cap and does not draw on these: the two
	// paths are switched independently and have opposite economics, so a shared budget would
	// silently disable one depending on which fired first.
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
		// The conversation context is identical for every candidate in this request, so with
		// two or more calls it is worth writing into the provider's cache once and reading it
		// back on the rest. With ONE call there is nothing to read it back, and a cache write
		// costs 1.25x fresh — so paying for it would be a 25% loss. Decided here because this
		// is the only place the final candidate count is known (the caps above trim it).
		extCfg.CacheContext = len(cands) > 1
		// THE ONE-WRITER-THEN-READERS ORDERING WENT WITH THE SWEEP. cheapmodel.claimCacheWrite
		// suppresses the breakpoint on CONCURRENT siblings (a cache entry only ever written is
		// worse than no breakpoint), so with llmConcurrency=4 the first call takes the write slot
		// and calls 2-4 send no mark. Serializing the first call fixes that but costs a whole
		// gateway queue round — ~2-4 s p50, tail 12-16 s. It was paid only where the money is
		// overwhelming: a turn whose entire transcript re-bills at 1.25x fresh. On this warm
		// per-output path the extra second buys a fraction of a cent, so it stays fully concurrent
		// and the flag above is best-effort.
		// saved/before carry the CALL's arithmetic to phase 3 rather than letting the goroutine
		// that computed it book the outcome — see the accept branch in runCall.
		type outT struct {
			projected, summary string
			saved, before      int
		}
		out := make([]outT, len(cands))
		// One deferred debug record per call, invoked after phase 3 so `accepted` describes the
		// splice rather than the model reply. nil for a call that never ran (a single-flight
		// follower) or when DEBUG is off.
		logRows := make([]func(), len(cands))
		// One record per call, written to its own slot so the goroutines need no lock (a
		// Report is copied by value across this codebase and cannot carry one).
		calls := make([]components.ModelCall, len(cands))
		// AND THE SAME RULE FOR GATES, which it did not previously get (#119).
		//
		// The two gates raised inside runCall — deduped_inflight_extraction and reply_truncated —
		// were calling rep.Gate from the goroutines. Report.Gates is a plain map with no lock, for
		// exactly the reason the comment above gives, so two concurrent raises are a data race on
		// a Go map. That is NOT a wrong counter and it is NOT a recoverable panic: the runtime
		// aborts the process with `fatal error: concurrent map writes`, which in a proxy means the
		// whole process dies rather than one component failing open. The dedup gate is the
		// reachable one — singleflight releases every follower of a shared key at the same instant,
		// so N identical candidates in one request give N-1 simultaneous raises.
		//
		// Same discipline as the records: each call appends its own gate names to its own slot, and
		// the serial phase below raises them.
		gateNames := make([][]string, len(cands))
		sem := make(chan struct{}, llmConcurrency)
		var wg sync.WaitGroup
		runCall := func(k int) {
			ctx, cancel := context.WithTimeout(c.Ctx, llmCallTimeout)
			defer cancel()
			// A per-CALL accounting window nested inside the request's, so this one
			// call's tokens and cost are attributable without hiding them from the
			// request's own bill (see cheapmodel.WithCallSink).
			ctx, callSink := cheapmodel.WithCallSink(ctx)
			before := schema.TextTokens(cands[k].content)
			start := time.Now()
			// Per-candidate config, differing from the request's only in whether the
			// model-free window is available. Deliberately NOT used for the cross-session
			// ResultKey below: the window decision is a pure function of the content, and
			// the content is already in that key, so varying MaxChars there would rotate
			// the key for no gain and make the read and write sides disagree.
			callCfg := extCfg
			if cands[k].noWindow {
				callCfg.MaxChars = 0
			}
			// SINGLE-FLIGHT on the cross-session result key. The persistent global cache
			// (getResultGlobal above) only helps a request that arrives AFTER the first one
			// finished; two requests carrying byte-identical content at the same time both
			// missed it and both paid. MEASURED on two live sessions started together: the
			// same 4,577-token candidate was extracted twice 1.6s apart, $0.0224 for a result
			// the system was already deriving — 54% of that run's entire extraction spend for
			// nothing. Colleagues working one repo through one proxy is exactly this shape.
			//
			// `executed` is what keeps the accounting honest: singleflight hands every waiter
			// the leader's value, so without it each waiter would record a ModelCall and its
			// saved tokens, double-counting a saving that happened once. Only the goroutine
			// whose closure actually ran sets its own flag.
			var (
				res, sum, strategy, why string
				executed                bool
			)
			sfv, _, _ := extractInflight.Do(extract.ResultKey(cands[k].id, e.modelName, extCfg),
				func() (any, error) {
					executed = true
					r, sm, st, w := extract.RunExtractionDetail(ctx, cands[k].content, goal,
						keepIDs, before, callCfg, model)
					return [4]string{r, sm, st, w}, nil
				})
			if v, ok := sfv.([4]string); ok {
				res, sum, strategy, why = v[0], v[1], v[2], v[3]
			}
			if !executed {
				// A concurrent request derived this exact result; take it and charge nothing.
				inflightDeduped.Add(1)
				gateNames[k] = append(gateNames[k], "deduped_inflight_extraction")
				out[k] = outT{projected: res, summary: sum}
				return
			}
			latency := float64(time.Since(start).Milliseconds())
			metrics.RecordExtractionCall(rep.Component, latency)
			_, inTok, outTok := callSink.Totals()
			cw, cr := callSink.CacheTotals()
			calls[k] = components.ModelCall{
				Component: rep.Component, Model: callModel,
				Strategy: strategy, Aggressiveness: string(e.aggro),
				Cold:            c.ColdCache,
				CandidateTokens: before, LatencyMs: latency,
				PromptTokens: inTok, CompletionTokens: outTok,
				CacheRead: cr, CacheWrite: cw,
				CostUSD:    pricing.Cost(inTok, outTok, cw, cr),
				GateReason: cands[k].gate,
				Before:     cands[k].content,
				Rejection:  why,
			}
			// THIS component's spend, at THIS component's rates. /stats used to derive
			// extraction cost from cheapmodel's process-global token totals through one rate
			// card, which is neither this component's spend nor extraction's: `summarize` and
			// `agentdiet` land in the same totals, and extract_llm_sweep pays the request's own
			// frontier model while the card is haiku's. See metrics.RecordExtractionSpend.
			metrics.RecordExtractionSpend(rep.Component, calls[k].CostUSD)
			// A reply that stopped exactly at the output cap was TRUNCATED, so the
			// Starlark program is incomplete, unparseable, and the whole call — its
			// money and its seconds — bought nothing. It is indistinguishable from
			// "the model declined to shrink this" in the return value, and the two
			// have opposite fixes: raise the cap versus stop calling. MEASURED on a
			// real session: 26.8s and ~$0.08 for a reply cut off at 2048 tokens.
			if outTok >= int64(cheapExtractOutputTokens) {
				calls[k].GateReason = "reply truncated at the output cap: " + calls[k].GateReason
				gateNames[k] = append(gateNames[k], "reply_truncated")
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
				out[k] = outT{projected: res, summary: sum}
				calls[k].Summary, calls[k].After = sum, res
				// THE OUTCOME IS CARRIED, NOT BOOKED. A model result is not a removal: phase 3
				// still has to splice it, and after #188 that can decline — the reserve refuses
				// the payload, or the never-worse check fails with the marker included. Recording
				// here meant a saturated reserve reported the full saving, credited the gross
				// value, logged `accepted=true` for a request that kept its original, and fed the
				// ratio tracker a saving that never happened. That last one is the worst of them:
				// the ratio prices FUTURE calls, so the mis-recording propagated into decisions
				// about work not yet done.
				//
				// Recorded in phase 3 instead, per candidate, once the splice is a fact. This
				// runs in a goroutine, so the value simply rides in the slot phase 3 already reads.
				out[k].saved = before - schema.TextTokens(res)
				out[k].before = before
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

			// ONE RECORD PER CALL (#177). Until this existed the component's only message was
			// `cg.extract_llm`, one per request, carrying the DECISION and nothing about the
			// CALL — so a run credited with 101 calls at 59,009 ms mean latency and a net value
			// of -$1.162 had no per-request trace at all. Three things were unanswerable and
			// each of them stopped an investigation: which requests made the calls, whether a
			// 59-second mean was 101 slow calls or a few multi-minute outliers dragging it (the
			// two have opposite fixes), and which candidates lost money. extract_llm_sweep's
			// `cg.sweep.ask` already makes its economics reconstructable; this is the same for
			// the tail pass.
			//
			// `accepted` is the never-worse outcome — the same condition that spliced the result
			// above, so the log cannot say accepted while the request kept the original — and
			// `rejection` is why a call produced nothing when it did not. Both, not one: an
			// empty rejection on a rejected call is what made timeout, sandbox refusal and
			// "nothing shrank" indistinguishable.
			//
			// DEBUG-gated by `dbg`, resolved once per request: the strings below are cheap but
			// they are per CALL, and the repo's rule is that a payload costing anything to build
			// is guarded. `content_key` rather than the content — the key is what the result
			// cache, the freeze and the cross-session lookup are all keyed on, so it is the
			// identity that joins this record to every other one about the same candidate.
			// DEFERRED TO AFTER PHASE 3, not emitted here, and the comment above is why: `accepted`
			// is only the never-worse outcome if it is read after the splice has been attempted.
			// Phase 3 now fills Accepted/SavedTokens, so a record emitted in this goroutine would
			// report accepted=false on every call — the mirror image of the overclaim it used to
			// make. A closure rather than a struct because each goroutine's locals (latency, the
			// token tiers, timedOut) are exactly what the record needs, and they are already here.
			if dbg {
				logRows[k] = func() {
					logging.From(c.Ctx).Debug("cg.extract_llm.call",
						"session", c.Session, "content_key", cands[k].id,
						"candidate_tokens", before, "model", callModel,
						"latency_ms", latency, "input_tokens", inTok, "output_tokens", outTok,
						"cache_read", cr, "cache_write", cw, "cost_usd", calls[k].CostUSD,
						"accepted", calls[k].Accepted, "saved_tokens", calls[k].SavedTokens,
						"rejection", calls[k].Rejection, "gate", cands[k].gate,
						"strategy", strategy, "timed_out", timedOut)
				}
			}
		}
		for k := 0; k < len(cands); k++ {
			wg.Add(1)
			go func(k int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				runCall(k)
			}(k)
		}
		wg.Wait()
		// Serial from here: every goroutine has returned. Only slots that actually made a
		// call are reported — a single-flight FOLLOWER returns before filling its slot, and
		// reporting that zero value put a phantom `cand=0 saved=0 $0.00` row in the ledger,
		// inflating the call count with work that by definition did not happen.
		for k := range calls {
			// The gates first, and unconditionally: a single-flight FOLLOWER returns before
			// filling its ModelCall slot, and its gate is precisely the one that says so.
			for _, g := range gateNames[k] {
				rep.Gate(g)
			}
		}
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
			// THE SPLICE FIRST, then the decision. The order was the other way, and the
			// reserve's new ability to refuse made that a defect: a declined removal left a
			// pinned cg:res: record claiming "this session sent these compacted bytes" for a
			// message that went upstream unchanged. On a later turn the same-session replay
			// path above reads that record and deliberately bypasses the cache-tail gate —
			// its reasoning is that the bytes were already sent — so once a reserve slot
			// freed, the compaction was spliced into a message by then inside the provider's
			// cached prefix, forcing a full-suffix cache write at ~11.5x the read price. That
			// is exactly what TestGlobalCacheHitIsNotSplicedAtDepth exists to prevent, arrived
			// at from the other side.
			if !apply(cands[k].i, cands[k].content, out[k].projected, out[k].summary, false) {
				continue
			}
			// The splice is a fact, so the outcome may now be booked. `accepted` in the ledger row
			// is documented as "the never-worse outcome — the same condition that spliced the
			// result above, so the log cannot say accepted while the request kept the original",
			// and that claim is only true if it is set here.
			// Feed the observed ratio so the gate prices future calls on what this workload
			// actually achieves. Only a splice is evidence of that: a result the reserve refused
			// tells us the model CAN shrink this content and that we could not use it, and a
			// future call will be refused the same way — so crediting the ratio would keep the
			// gate authorising calls whose output is discarded. A model that produced nothing is
			// separate evidence and is still observed as ratio 0, in runCall.
			//
			// THE COST OF NOT OBSERVING, stated because the choice is a trade and not a free win:
			// under a persistently saturated reserve nothing advances r.total, so ratio() stays
			// pinned to its prior and exploring() keeps granting maxExploreCalls per session
			// instead of self-terminating once the sample is large enough. That is a small
			// permanent spend on exactly the deployments this change is for. It is bounded per
			// session and it buys a tracker that is not lying, which is the better side of the
			// trade — but the previous behaviour did terminate exploration, and pretending
			// otherwise would misread the diff.
			//
			// Only for a slot that actually made a call. A single-flight FOLLOWER returns before
			// filling its slot, so before is 0 there and the three lines below would book zeros —
			// and set Accepted on a row whose Component is "" — for a splice that did happen. The
			// follower's saving goes unbooked either way (its leader books the shared result once);
			// what this guard removes is the pretence of measuring it.
			if out[k].before > 0 {
				calls[k].Accepted = true
				calls[k].SavedTokens = out[k].saved
				e.ratios.observe(out[k].saved, out[k].before)
				metrics.RecordExtractionSaving(rep.Component, out[k].saved)
				// What the removal was WORTH, at this turn's regime. On a cold sweep that is the
				// cache-write rate; the replays above are credited at the read rate.
				metrics.RecordExtractionValue(rep.Component, float64(out[k].saved)*val.perToken)
			}
			putResult(c, cands[k].id, out[k].projected, out[k].summary)
			if !e.rewrite || effectiveMode(c, e.mode) == markerFull {
				putResultGlobal(c, extract.ResultKey(cands[k].id, e.modelName, extCfg),
					out[k].projected, out[k].summary)
			}
		}
		// AFTER phase 3, because rep.Calls takes a COPY of each row and phase 3 is what sets
		// Accepted/SavedTokens now. Appending before it exported a ledger of rows that all read
		// accepted=false, which is the mirror image of the bug being fixed.
		for k := range calls {
			if calls[k].Component != "" {
				rep.Calls = append(rep.Calls, calls[k])
			}
			if logRows[k] != nil {
				logRows[k]()
			}
		}
	}

	if changed == 0 {
		rep.Skipped = true
	}
	return keys, nil
}

func init() {
	f := []components.Field{
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
			Hint: "The N for context: recent (0 = 2). This is the single biggest lever on what a call COSTS: measured in production the rendered conversation was most of a 3,785-token prompt sent to compress a 2,700-token candidate. Raise it only where acceptance measurably needs it."},
		{Key: "max_chars", Type: components.FieldInt,
			Hint: "Window for the model-free deterministic projection (0 = 4000). The window is line-aligned and names what it dropped; a result that hits the cap with nothing saying so is refused whatever this is set to."},
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
	}
	f = append(f, modelFields("model")...)
	components.RegisterFields("extract_llm", extractLLMConfig{}, append(f, components.TriggerFields("trigger")...))
}
