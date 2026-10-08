package offload

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/logging"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
	"gopkg.in/yaml.v3"
)

func init() { components.Register("cache_aware_summarizer", newCacheAwareSummarizer) }

//go:embed summarizer_model_profiles.yaml
var summarizerProfilesYAML []byte

// CacheAwareSummarizer summarizes by APPENDING the instruction to the conversation instead of
// rebuilding the prompt, so the summarization call can reuse a prefix the backend already has
// rather than paying fresh prefill for tokens it has already seen.
//
// `summarize` builds a fresh prompt — a preamble plus the rendered transcript as one user
// message — which shares no prefix with the conversation it describes, so that call is a cache
// miss on every token. This one sends [the conversation, verbatim and in order] + [one appended
// instruction]. It is the shape prompt-caching guidance prescribes for a fork: reuse the
// parent's prefix and append only what the fork adds.
//
// ⛔ HOW FAR THE REUSE ACTUALLY GOES, because the naive reading of the paragraph above is wrong
// after the first compaction. The prefix that would be hit is one the BACKEND has seen, and the
// backend only ever receives what this proxy forwards. The agent keeps sending its own full
// uncompacted history, but from the first triggering turn onward the backend has received
// COMPACTED requests — so there is no full-history prefix upstream to match, and the shared
// prefix is roughly the pinned head. Two consequences worth measuring rather than assuming:
// the saving is largest on the first compaction and smaller afterwards, and sending the full
// growing history as a side call can evict the compacted prefix the forwarded request needs.
// Judge this component on the backend's own telemetry (vllm:prefix_cache_hits_total, or
// usage.cache_read_input_tokens, which the OpenAI client records) — never on a savings percentage.
//
// ⛔ IT NEEDS EITHER A components.MessagesModel OR A components.PrefixAsker, AND DECLINES WITHOUT
// BOTH. A plain Model flattens the conversation to one string, which is exactly the
// prefix-destroying shape this exists to avoid, so a client without either capability makes the
// component skip. A silent fallback would report this method's latency while paying the old
// method's cost. Because a declining arm compacts nothing and is byte-identical to `off` on every
// other metric, the decline is COUNTED: read CacheAwareSummarizerDeclined before believing any
// delta from this arm.
//
// THE PREFIXASKER PATH EXISTS BECAUSE THE MESSAGESMODEL ONE IS UNREACHABLE ON THIS DEPLOYMENT'S
// OWN TRAFFIC (#275). On Anthropic incoming-model requests — `model.source: incoming`, the
// default — the host wraps the request's own client as internal/cheapmodel.Anthropic, which
// implements Complete but not CompleteMessages; only internal/cheapmodel/openai.go does. Without
// a second path this component ran only on an OpenAI-shaped `model.source: config` deployment and
// declined every turn of Anthropic-only traffic — exactly `off`, invisibly, for the one route the
// feature was built for. Ctx.PrefixAsk completes one question against the PREVIOUS turn's SENT
// body (components.PrefixAsker's own docstring explains why that body, not the incoming one), so
// it is the SAME cache-reading shape as the MessagesModel path, just a different wire mechanism —
// one appended user-role message, no system-role option, since PrefixAsker always sends it as a
// trailing user turn (see summaryCaller in cache_aware_async.go for where the two converge, and
// CacheAwareSummarizerPrefixAskUsed for which one actually ran).
//
// Span selection, kept-verbatim repair and orphan repair all come from summarize_pairing.go —
// the same functions `summarize` uses, not a second implementation. A tool exchange is atomic
// and both boundaries must respect that; a private copy of the invariant would be a second
// place for it to drift, and the drift would surface as an exception-rate difference blamed on
// the method under test.
//
// # Two cache-state modes, and they mean genuinely different things
//
// `trigger.cache_state: any` (the default) summarizes and USES the summary now: the turn that
// crosses the fill threshold commissions a checkpoint, and the next eligible turn splices it,
// warm or cold, exactly as every component in this file described before this comment existed.
//
// `trigger.cache_state: pre_expiry` means something else: PREPARE one in the background, and
// use it only on a cold return. A checkpoint commissioned while the cache is still believed
// live (phase PreExpiry) is held RESERVED (sumCheckpoint.Reserved) rather than spliced — warm
// turns keep forwarding the full history untouched, because splicing it now would be the exact
// cache-destructive rewrite this mode exists to avoid. While reserved, an idle keep-alive ping
// still pings the FULL history (that is what the provider actually has cached); whether a ping
// is instead spent refreshing the reserve with a FRESH summary is no longer a token-count
// comparison against resummarize_tokens — since #415 it is a money gate (expected saving on the
// eventual cold return against the refresh call's own projected cost; see
// checkpointRefreshOpportunity and fireSummarySubstitute) — rather than on a plain read of a
// prefix the reserve no longer matches. Only a turn whose phase is Cold (the cache is actually
// gone) or Unknown (no
// cache-aware tracking exists on this request at all, so there is nothing live to protect)
// graduates the reserve: splices it, tail and all, and marks it live from then on — see
// cacheAwareApplyPhase. From that turn forward the session behaves exactly like `any` for that
// one checkpoint: replayed while current, re-summarized fresh once stale, on any phase.
//
// WHY THIS IS WORTH THE COMPLEXITY: a warm turn reading a 900k-token prefix costs about $0.18 on
// Sonnet 5 at the cache-read rate; reading the ~80k reserve summarizes down to costs about
// $0.016 instead — paid at every ping instead of a free read, which is why it is spent only when
// the reserve has grown enough to be worth refreshing, not on every ping unconditionally. The
// return this buys: a COLD return against the full 900k prefix costs about $0.20 to write once
// the ~80k reserve is already in hand, against roughly $2.25 to re-write the full 900k from
// scratch — the rewrite cache_state: pre_expiry exists to avoid paying for AT THE WORST possible
// moment, a cold turn that is already paying full freight for everything else.
type CacheAwareSummarizer struct {
	keepLastTurns     int
	instructionRole   bschemas.ChatMessageRole
	roleAuto          bool
	explicitSystem    bool
	modelID           string
	profilesPath      string
	profiles          *summarizerProfiles
	minTokens         int
	maxRequestTokens  int
	resummarizeTokens int
	modelSource       string
	modelClient       components.Model
	trigger           components.Trigger
	mode              markerMode
	// overrideProfiles caches the on-disk registry after the first successful read.
	overrideProfiles atomic.Value
}

type cacheAwareSummarizerConfig struct {
	// KeepLastTurns is how many trailing messages stay verbatim; the span between the pinned
	// head and them is what the summary replaces. The head is summarizeSpan's, so it is not a
	// knob here — see that function for why it is 1 and when it drops to 0.
	KeepLastTurns int `yaml:"keep_last_turns"`
	// InstructionRole is where the appended instruction goes: auto (resolved per model from the
	// profile registry) | system | user.
	//
	// ⛔ The two failure modes are not equally visible. A provider that rejects a trailing
	// system message returns a 400. A chat template that DROPS or HOISTS it fails silently: the
	// model continues the task and its next turn is recorded as the summary. Hence auto, hence a
	// `user` default, and hence the construction-time refusal of an explicit `system` for a model
	// the registry has not verified.
	InstructionRole string `yaml:"instruction_role"`
	// ModelID is the served model id that `auto` resolves against. Without it nothing can match
	// the system_models allow-list, so `auto` resolves to `user`.
	ModelID string `yaml:"model_id"`
	// ProfilesPath overrides the embedded registry with a file on disk, so a deployment can
	// promote a model it verified itself without rebuilding.
	//
	// ⚠️ Read LAZILY and NON-FATALLY, falling back to the embedded registry and counting the
	// fallback. Reading it at construction made this the only component whose config could not be
	// swept by the settings form's perturbation test — every declared string field is set to a
	// sentinel and the document must still build, which an os.ReadFile of a sentinel path cannot.
	ProfilesPath string `yaml:"profiles_path"`
	MinTokens    int    `yaml:"min_tokens"`
	// MaxRequestTokens refuses to commission a summary whose OUTBOUND request would exceed this
	// many tokens. 0 = no cap.
	//
	// ⛔ A REFUSAL, NEVER A TRUNCATION. min_tokens asks "is the span worth a call"; this asks "can
	// we afford the call", and they are different questions because the request carries the WHOLE
	// conversation, not the span. Truncating it to fit is not available: the appended-suffix shape
	// is the entire mechanism, and a truncated conversation is a different prefix that matches
	// nothing. So an over-large session declines and says so, rather than paying for a call that
	// cannot hit and may evict the prefix the forwarded request needs.
	MaxRequestTokens int `yaml:"max_request_tokens"`
	// ResummarizeTokens: once a summary exists, REUSE it — no model call, and the spliced message
	// stays byte-identical so the forwarded prefix is stable — until the untouched tail since that
	// checkpoint grows past this many tokens. 0 = re-summarize every eligible turn.
	//
	// ⛔ Not optional for a component named for cache reuse. Without a checkpoint every triggering
	// turn re-derives a different summary, so the FORWARDED request's prefix changes at the head
	// on every turn and the request carrying the agent's answer never gets a hit past it — the
	// component would invalidate the cache it exists to protect, and pay a synchronous model call
	// per turn to do it.
	ResummarizeTokens int                `yaml:"resummarize_tokens"`
	MarkerMode        string             `yaml:"marker_mode"` // full (default) | summary | off
	Model             modelConfig        `yaml:"model"`
	Trigger           components.Trigger `yaml:"trigger"`
}

// summarizerProfiles is summarizer_model_profiles.yaml; see that file for the provenance of
// every entry and for how to verify a model before promoting it.
//
// SystemModels is the ALLOW-LIST, and it is the only thing that grants a trailing role: system
// instruction. Profiles is a provenance record and grants nothing: an entry there says what we
// know about a model's template or provider, and promoting one means adding its match string to
// SystemModels as well. The two were one list until a measured failure separated them — a
// profile marked `role: system` was reached over a gateway whose translation layer does not
// preserve a mid-array system message, so knowing what a model accepts turned out to be a
// different question from knowing what the path to it accepts.
type summarizerProfiles struct {
	Prompts struct {
		System string `yaml:"system"`
		User   string `yaml:"user"`
	} `yaml:"prompts"`
	// SystemModels are match strings whose models take the instruction as role: system. EMPTY
	// on this build: no model+path combination is currently verified. See the file header.
	SystemModels []string `yaml:"system_models"`
	// ThinkingLastTurnModels are match strings for models that keep only the LAST turn's
	// thinking block — see thinkingLastTurnOnly. An ALLOW-list, not a block-list, for the same
	// reason SystemModels is one: an unmatched model defaults to "keep all", so a model this
	// list has never heard of is never gated on the strength of a guess.
	ThinkingLastTurnModels []string `yaml:"thinking_last_turn_models"`
	Profiles               []struct {
		Match    string `yaml:"match"`
		Verified string `yaml:"verified"`
	} `yaml:"profiles"`
}

// roleFor resolves the appended instruction's role for a model id, and reports whether the
// answer came from the system_models ALLOW-LIST. The first substring match wins, so specific
// ids precede family prefixes in the file. matched=false means the registry does not permit a
// system-role instruction for this model, which is the state an explicit `system` is refused
// for — and with an empty system_models that is every model, deliberately.
//
// There is no configurable fallback role any more. `default_role` was one, and a global switch
// that can re-grant `system` to every unmatched model at once is the opposite of an allow-list,
// so an unmatched model is now unconditionally `user`.
func (p *summarizerProfiles) roleFor(modelID string) (role bschemas.ChatMessageRole, matched bool) {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id != "" {
		for _, m := range p.SystemModels {
			if m != "" && strings.Contains(id, strings.ToLower(strings.TrimSpace(m))) {
				return bschemas.ChatMessageRoleSystem, true
			}
		}
	}
	return bschemas.ChatMessageRoleUser, false
}

// thinkingLastTurnOnly reports whether modelID is one documented to keep only the LAST turn's
// thinking block, matched the same way roleFor matches system_models: the first substring hit
// wins, case-insensitive, over a short allow-list rather than a guessed block-list.
//
// Per Anthropic's own "Thinking block preservation by model"
// (https://platform.claude.com/docs/en/build-with-claude/thinking#thinking-block-preservation-by-model,
// read 2026-10-08): "Keep the last turn only: earlier Opus and Sonnet models, and all Haiku
// models through Claude Haiku 4.5. When you pass older thinking blocks back, the API strips
// them automatically" — and, from the surrounding page, that stripping happens the moment a
// non-tool-result user message is sent, which is exactly the shape this component's own
// appended instruction takes. "Keep all prior turns" covers Opus 4.5+, Sonnet 4.6+, Haiku 5.5,
// and the Fable/Mythos family — none of which this list names, so an unmatched model (every
// model this registry has not positively identified as last-turn-only) is "keep all" by
// default, the safe direction here: declining when thinking would NOT actually be stripped
// costs a compaction opportunity, but proceeding when it WOULD be costs a silently smaller
// cache read than the dashboard's own numbers would suggest — the gap a live test measured on
// claude-haiku-4-5 (reading ~2,100-2,150 tokens short of the immediately preceding real turn,
// in two independent sessions) before this gate existed. The registry (see
// summarizer_model_profiles.yaml's thinking_last_turn_models) also names Claude 3.7 Sonnet,
// bare Opus 4 and Opus 4.1, and bare Sonnet 4 and Sonnet 4.5, per the same doc page's cutoffs.
func (p *summarizerProfiles) thinkingLastTurnOnly(modelID string) bool {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" {
		return false
	}
	for _, m := range p.ThinkingLastTurnModels {
		if m != "" && strings.Contains(id, strings.ToLower(strings.TrimSpace(m))) {
			return true
		}
	}
	return false
}

// prompt returns the instruction text for a role. The two differ by more than tone: the user
// variant has to establish in TEXT that this is an operator instruction and that the task must
// not be continued, because the system channel carries both for free.
func (p *summarizerProfiles) prompt(role bschemas.ChatMessageRole) string {
	if role == bschemas.ChatMessageRoleSystem && strings.TrimSpace(p.Prompts.System) != "" {
		return p.Prompts.System
	}
	return p.Prompts.User
}

var errNoSummarizerPrompt = errors.New(
	"cache_aware_summarizer: the profile registry defines no user prompt, so there is no safe " +
		"instruction to append (the user variant is the fallback for every unresolved model)")

func loadSummarizerProfiles(raw []byte) (*summarizerProfiles, error) {
	var p summarizerProfiles
	if err := yaml.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	// Refused rather than defaulted: an empty user prompt would append a blank instruction and
	// the model would simply continue the conversation, which then scores as a summary.
	if strings.TrimSpace(p.Prompts.User) == "" {
		return nil, errNoSummarizerPrompt
	}
	return &p, nil
}

// resolveProfiles returns the on-disk override when profiles_path is set and readable, else the
// embedded registry. A read or parse failure is non-fatal and COUNTED — silently running on the
// embedded registry while an operator believes their override is live is exactly the kind of
// invisible divergence this component's counters exist to prevent.
func (s *CacheAwareSummarizer) resolveProfiles() *summarizerProfiles {
	if s.profilesPath == "" {
		return s.profiles
	}
	if p, ok := s.overrideProfiles.Load().(*summarizerProfiles); ok && p != nil {
		return p
	}
	b, err := os.ReadFile(s.profilesPath)
	if err == nil {
		if p, perr := loadSummarizerProfiles(b); perr == nil {
			s.overrideProfiles.Store(p)
			return p
		}
	}
	atomic.AddInt64(&cacheAwareProfileFallbacks, 1)
	s.overrideProfiles.Store(s.profiles)
	return s.profiles
}

// defaultCacheAwareTimeout matches summarize's ceiling: one call over most of the transcript, so
// the budget must cover server queue wait plus a large prefill plus generation.
const defaultCacheAwareTimeout = 300 * time.Second

var cacheAwareTimeout = resolveTimeoutEnv("CONTEXT_GURU_CACHE_AWARE_SUMMARIZER_TIMEOUT",
	resolveTimeoutEnv("CONTEXT_GURU_SUMMARIZE_TIMEOUT", defaultCacheAwareTimeout))

// Counters. Calls is reported because this method's cost is per compacted turn, so a reward or
// latency delta read without it is unattributable. The rest each name ONE failure, because they
// call for different fixes and `reverted` cannot tell them apart.
var (
	cacheAwareCalls            int64
	cacheAwareTimeouts         int64
	cacheAwareErrors           int64
	cacheAwareDeclined         int64
	cacheAwareEmpty            int64
	cacheAwareRefusedStash     int64
	cacheAwareProfileFallbacks int64
	cacheAwareUnverifiedSystem int64
	cacheAwareTooLarge         int64
	// cacheAwareNoPrefix counts a PrefixAsker-backed call's FIRST-TURN case (components.ErrNoPrefix):
	// nothing stashed yet to append to. Not a failure — every session hits this once — and counted
	// apart from cacheAwareErrors for the same reason extract_llm_sweep's sweep_no_prefix is
	// separate from sweep_ask_failed: the two mean "nothing to attend" and "the read failed",
	// which call for opposite attention.
	cacheAwareNoPrefix int64
	// cacheAwareStalePrefix counts a decline because the PrefixAsk path's stashed body does not
	// (yet, or any longer) cover the span this turn is about to commission a summary for — see
	// the stale-prefix guard in Offload and components.PrefixCoverage. Not the same failure as
	// cacheAwareNoPrefix: that is "nothing stashed at all" (every session's first turn, routine),
	// this is "something IS stashed and it is the wrong something" (a body the host's stash
	// dropped for being oversized, or a race against this session's own concurrent turn) —
	// caught before paying for the call rather than after.
	cacheAwareStalePrefix int64
	// cacheAwarePrefixAskUsed counts turns that reached the model step via components.PrefixAsker
	// rather than a components.MessagesModel — i.e. every turn that activated on a route whose
	// client has no MessagesModel at all, which on this deployment IS the Anthropic
	// incoming-model path (#275). Zero here on a deployment running Anthropic-only traffic means
	// this component is still measuring `off`, whatever CacheAwareSummarizerDeclined says.
	cacheAwarePrefixAskUsed int64
	// cacheAwareCacheReadTokens/cacheAwareCacheWriteTokens are the cumulative cache tiers of
	// every commission call this component has made (any trigger, any path) — the direct,
	// process-wide answer to "is the call actually reading warm", which is this component's
	// whole argument and was otherwise visible only on a per-call dashboard row (see
	// deferredCall in summarize_async.go). cacheAwareCacheReadTokens staying at 0 across many
	// calls is the same "never verify a cache win from placement, read the number" discipline
	// cheapmodel.CacheUsage's own docstring states for the cheap-model path.
	cacheAwareCacheReadTokens  int64
	cacheAwareCacheWriteTokens int64
	// cacheAwareSummaryTruncated counts a PAID-FOR call whose raw reply was missing the closing
	// </summary> tag the prompt asks for — the signature of a reply cut off mid-generation by
	// the model's own token/thinking budget. Counted and refused rather than committed: see
	// cacheAwareSummaryIncomplete in cache_aware_async.go for why this has to be checked on the
	// UNTRUSTED raw text before sanitizeSummary/ensureSummaryTags touch it, or a truncated reply
	// is indistinguishable from a complete one by the time either of those has run.
	cacheAwareSummaryTruncated int64
	// cacheAwareThinkingWouldStrip counts a decline because appending the instruction would, on
	// this model, strip every earlier thinking block from the backend's cached context — see
	// thinkingWouldBeStripped and summarizerProfiles.thinkingLastTurnOnly. Not a failure: the
	// call is never made, so nothing was paid for and lost. Zero on a deployment that runs
	// last-turn-only models at all means either none of its traffic carries earlier thinking
	// blocks, or this gate has nothing to catch there yet.
	cacheAwareThinkingWouldStrip int64
)

func CacheAwareSummarizerCalls() int64    { return atomic.LoadInt64(&cacheAwareCalls) }
func CacheAwareSummarizerTimeouts() int64 { return atomic.LoadInt64(&cacheAwareTimeouts) }
func CacheAwareSummarizerErrors() int64   { return atomic.LoadInt64(&cacheAwareErrors) }
func CacheAwareSummarizerNoPrefix() int64 { return atomic.LoadInt64(&cacheAwareNoPrefix) }
func CacheAwareSummarizerStalePrefix() int64 {
	return atomic.LoadInt64(&cacheAwareStalePrefix)
}
func CacheAwareSummarizerPrefixAskUsed() int64 {
	return atomic.LoadInt64(&cacheAwarePrefixAskUsed)
}
func CacheAwareSummarizerTruncated() int64 { return atomic.LoadInt64(&cacheAwareSummaryTruncated) }
func CacheAwareSummarizerThinkingWouldStrip() int64 {
	return atomic.LoadInt64(&cacheAwareThinkingWouldStrip)
}

// CacheAwareSummarizerCacheTokens returns the cumulative cache-read/cache-write tokens of every
// commission call this component has made, across every trigger and path.
func CacheAwareSummarizerCacheTokens() (read, write int64) {
	return atomic.LoadInt64(&cacheAwareCacheReadTokens), atomic.LoadInt64(&cacheAwareCacheWriteTokens)
}

// CacheAwareSummarizerDeclined counts turns that reached the model step and stopped because
// NEITHER a components.MessagesModel NOR a components.PrefixAsker was available. A role the
// backend rejects returns an error and lands in Errors, not here. Non-zero means this arm is not
// measuring cache-reuse compaction on those turns; it is measuring `off` — though a deployment
// may still be compacting plenty of OTHER turns via PrefixAsk (CacheAwareSummarizerPrefixAskUsed)
// at the same time, which this counter alone cannot distinguish from total inertness.
func CacheAwareSummarizerDeclined() int64 { return atomic.LoadInt64(&cacheAwareDeclined) }

// CacheAwareSummarizerEmpty counts calls that were PAID FOR and returned nothing usable. It is
// the signature of the silent failure the profile registry guards against: an instruction the
// template dropped or hoisted, so the model answered the conversation instead of the request.
func CacheAwareSummarizerEmpty() int64 { return atomic.LoadInt64(&cacheAwareEmpty) }

// CacheAwareSummarizerRefusedStash counts summaries abandoned because the store would not accept
// the span. The splice is skipped rather than completed, because a marker promising a span the
// store never took is a lossy Offload advertising reversibility it does not have.
func CacheAwareSummarizerRefusedStash() int64 { return atomic.LoadInt64(&cacheAwareRefusedStash) }

// CacheAwareSummarizerProfileFallbacks counts turns that fell back to the embedded registry
// because profiles_path was unreadable or unparseable.
func CacheAwareSummarizerProfileFallbacks() int64 {
	return atomic.LoadInt64(&cacheAwareProfileFallbacks)
}

// CacheAwareSummarizerUnverifiedSystem counts turns declined because instruction_role was pinned
// to `system` for a model no registry profile marks as accepting a trailing system message.
// Non-zero means the arm is configured for a silent failure and is refusing to take it.
func CacheAwareSummarizerUnverifiedSystem() int64 {
	return atomic.LoadInt64(&cacheAwareUnverifiedSystem)
}

// CacheAwareSummarizerTooLarge counts turns declined because the outbound request would have
// exceeded max_request_tokens. Non-zero means this session outgrew the method rather than that
// anything failed.
func CacheAwareSummarizerTooLarge() int64 { return atomic.LoadInt64(&cacheAwareTooLarge) }

func CacheAwareSummarizerCallTimeout() time.Duration { return cacheAwareTimeout }

func init() {
	f := []components.Field{
		{Key: "keep_last_turns", Type: components.FieldInt, Default: 10, Min: 0,
			Hint: "Messages kept verbatim at the tail. The default matches summarize's tuned tail " +
				"so the two are comparable; it is a chosen starting point, not a measured optimum."},
		{Key: "instruction_role", Type: components.FieldEnum, Default: "auto",
			Options: []string{"auto", "system", "user"},
			Hint: "Where the appended instruction goes. auto resolves per model from the embedded " +
				"registry and needs model_id. An explicit `system` for a model the registry has " +
				"not verified is REFUSED at construction: a provider returns a clean 400, but a " +
				"chat template that drops or hoists the message fails silently."},
		{Key: "model_id", Type: components.FieldString,
			Hint: "The served model id that instruction_role: auto resolves against."},
		{Key: "profiles_path", Type: components.FieldString,
			Hint: "Overrides the embedded model registry with a file on disk. Read lazily; an " +
				"unreadable path falls back to the embedded registry and increments " +
				"cache_aware_summarizer_profile_fallbacks."},
		{Key: "min_tokens", Type: components.FieldInt, Default: 500, Min: 1,
			Hint: "Smallest span worth one model call. Note this gates the SPAN; the request carries " +
				"the whole conversation — see max_request_tokens."},
		{Key: "max_request_tokens", Type: components.FieldInt, Default: 0, Min: 0,
			Hint: "Refuse to commission a summary whose outbound request would exceed this many " +
				"tokens (0 = no cap). A refusal, never a truncation: truncating the conversation " +
				"would change the prefix and defeat the mechanism."},
		{Key: "resummarize_tokens", Type: components.FieldInt, Default: 6000, Min: 0,
			Hint: "Reuse the existing summary with no model call until the tail since that " +
				"checkpoint grows past this many tokens. 0 = re-summarize every eligible turn, " +
				"which changes the forwarded prefix every turn and forfeits the cache stability " +
				"this component exists for."},
		markerModeField(),
	}
	f = append(f, modelFields("model")...)
	// The shared trigger descriptors, with the two defaults this component's constructor
	// (applyCacheAwareTriggerDefaults) actually installs — mirroring summarize's own override of
	// the same two keys and for the same reason: Field.Default documents what an ABSENT key means
	// to THIS component, and form.go's normalize() writes an enum's Default back into the saved
	// document for an unset value, so a wrong one here would persist a cache_state this component
	// never chose.
	tf := components.TriggerFields("trigger")
	for i := range tf {
		switch tf[i].Key {
		case "trigger.cache_state":
			tf[i].Default = cacheAwareDefaultCacheState
		case "trigger.min_request_frac":
			tf[i].Default = cacheAwareDefaultRequestFrac
		}
	}
	components.RegisterFields("cache_aware_summarizer", cacheAwareSummarizerConfig{}, append(f, tf...))
}

// cacheAwareDefaultRequestFrac and cacheAwareDefaultCacheState mirror summarize's own trigger
// defaults (summarizeDefaultRequestFrac/summarizeDefaultCacheState) for the first half of the same
// reason: a zero components.Trigger fires on every request carrying more than ~11 messages and a
// 500-token span (components/trigger.go:20,266), which is far more aggressive than the measured,
// defensible 0.9-of-C threshold summarize ships, and nothing about this component's own mechanism
// changes that argument — it still ties up a model call and a stash slot on every eligible turn.
//
// The default is `any`, not `pre_expiry`, even though THIS component is the one whose call
// actually needs a live prefix (see summarizeDefaultCacheState's retraction of `pre_expiry` for
// `summarize`, and the comment above it explaining why `pre_expiry` is honoured here instead of
// being inert). Two things make `any` the safer SHIPPED default rather than `pre_expiry`:
//
//   - `pre_expiry`'s window is seconds wide by design (DefaultPreExpiry is one minute inside a
//     five-minute lifetime), so a deployment that never happens to send a request inside it simply
//     never compacts — the same "silently dead on this deployment" failure CacheAllows' own
//     Unknown-permits-by-default rule exists to avoid for `summarize`.
//   - Firing on `any` and arriving COLD does not pay the double-rewrite a naive reading would
//     predict: see cache_aware_cold.go, which defers the side call until the forwarded request's
//     own usage confirms the cache it just rewrote, so the side call always reads warm.
//
// `pre_expiry` stays reachable and still restricts for a deployment that wants the stronger
// guarantee of only ever reusing a cache the component KNOWS is still alive (never Unknown either
// — see Trigger.CacheAllows' Unknown-with-pre_expiry clause, which this component's defaults do
// not relax).
const (
	cacheAwareDefaultRequestFrac = summarizeDefaultRequestFrac
	cacheAwareDefaultCacheState  = summarizeDefaultCacheState
)

// applyCacheAwareTriggerDefaults installs this component's trigger defaults for keys the operator
// did not write, reusing summarize's raw-YAML presence probe (triggerKeysPresent) rather than a
// second copy of it — see that function for why the zero value of MinRequestFrac cannot be used
// to detect "absent".
func applyCacheAwareTriggerDefaults(raw []byte, t *components.Trigger) error {
	present := triggerKeysPresent(raw)
	if !present["min_request_frac"] {
		t.MinRequestFrac = cacheAwareDefaultRequestFrac
	}
	if t.CacheState == "" {
		t.CacheState = cacheAwareDefaultCacheState
	}
	return t.Validate("cache_aware_summarizer")
}

func newCacheAwareSummarizer(raw []byte) (components.Component, error) {
	cfg := cacheAwareSummarizerConfig{
		KeepLastTurns: 10, MinTokens: 500, ResummarizeTokens: 6000, InstructionRole: "auto",
	}
	if err := components.Decode(raw, &cfg); err != nil {
		return nil, err
	}
	if cfg.KeepLastTurns < 0 {
		return nil, errors.New("cache_aware_summarizer: keep_last_turns must be >= 0")
	}
	// Validated here as well as declared: the settings form clamps Min, a hand-written YAML file
	// does not, and min_tokens: 0 silently disables the gate.
	if cfg.MinTokens < 1 {
		return nil, errors.New("cache_aware_summarizer: min_tokens must be >= 1")
	}
	if cfg.MaxRequestTokens < 0 {
		return nil, errors.New("cache_aware_summarizer: max_request_tokens must be >= 0")
	}
	if cfg.ResummarizeTokens < 0 {
		return nil, errors.New("cache_aware_summarizer: resummarize_tokens must be >= 0")
	}
	// Installs min_request_frac: 0.9 / cache_state: any for keys the operator did not write, and
	// validates cache_state — see applyCacheAwareTriggerDefaults.
	if err := applyCacheAwareTriggerDefaults(raw, &cfg.Trigger); err != nil {
		return nil, err
	}
	// NOTE: an earlier revision rejected `resummarize_tokens: 0` together with
	// `cache_state: pre_expiry` here, because the keep-alive refresh decision used to compare the
	// reserve's own tail against resummarize_tokens — making 0 a "pay for an identical summary on
	// every single ping, forever" trap. Issue #415 replaced that token-count comparison with a
	// money gate (expected saving vs. projected cost, in proxy's fireSummarySubstitute): any
	// nonempty tail is now merely an OPPORTUNITY the gate prices, so a tiny or zero-token-threshold
	// tail simply never earns back a nonzero call cost and the ping stays plain — no config-time
	// trap left to refuse here. resummarize_tokens keeps its original, turn-based meaning
	// unchanged (see its own re-summarize check below).
	profiles, err := loadSummarizerProfiles(summarizerProfilesYAML)
	if err != nil {
		return nil, err
	}
	c := &CacheAwareSummarizer{
		keepLastTurns: cfg.KeepLastTurns, modelID: cfg.ModelID, profilesPath: cfg.ProfilesPath,
		profiles: profiles, minTokens: cfg.MinTokens, maxRequestTokens: cfg.MaxRequestTokens, resummarizeTokens: cfg.ResummarizeTokens,
		modelSource: cfg.Model.Source, modelClient: cfg.Model.Client(),
		trigger: cfg.Trigger, mode: parseMarkerMode(cfg.MarkerMode),
	}
	switch strings.ToLower(strings.TrimSpace(cfg.InstructionRole)) {
	case "", "auto":
		c.roleAuto = true
		c.instructionRole, _ = profiles.roleFor(cfg.ModelID)
	case "user":
		c.instructionRole = bschemas.ChatMessageRoleUser
	case "system":
		// Pinned, and checked at the point of USE rather than here. The dangerous combination is
		// an explicit `system` for a model no profile marks as accepting a trailing system
		// message: a template that drops or hoists it leaves the model answering the conversation,
		// and that answer is recorded as the summary. Offload DECLINES on that combination — the
		// same discipline as the missing-MessagesModel case, and for the same reason: loud refusal
		// beats an invisible wrong answer.
		//
		// ⚠️ Not a construction error, deliberately. Every declared enum option must still build a
		// valid document (config/form_test.go sets each one in turn), so refusing here would make
		// this the one component whose config surface the settings form cannot sweep — exactly the
		// trap profiles_path fell into.
		c.explicitSystem = true
		c.instructionRole = bschemas.ChatMessageRoleSystem
	default:
		return nil, errors.New("cache_aware_summarizer: instruction_role must be auto|system|user, got " +
			cfg.InstructionRole)
	}
	return c, nil
}

func (CacheAwareSummarizer) Name() string                 { return "cache_aware_summarizer" }
func (CacheAwareSummarizer) Enabled(*components.Ctx) bool { return true }
func (*CacheAwareSummarizer) NeedsModel() bool            { return true }

// InstructionRole exposes the resolved role so /stats and a probe can report which channel this
// arm actually used; with `auto` it cannot be read off the config alone.
func (s *CacheAwareSummarizer) InstructionRole() string { return string(s.instructionRole) }

// Offload rewrites the transcript to [head, summary, tail]. The summary comes from a model call
// built as [the whole conversation] + [instruction], and is REUSED across turns until the tail
// grows past resummarize_tokens — without that reuse the forwarded prefix would change every
// turn and this component would invalidate the cache it exists to protect.
// hasThinkingBlock reports whether an assistant message carries a thinking block — plain
// (`Reasoning`/a `reasoning.text` detail, bifrost's normalized form of Anthropic's `thinking`)
// or encrypted (`reasoning.encrypted`, bifrost's form of `redacted_thinking`). Either shape is
// what Anthropic's own preservation rule keys on; this component does not need to tell them
// apart; strips/keeps the whole block together.
func hasThinkingBlock(m bschemas.ChatMessage) bool {
	return m.ChatAssistantMessage != nil &&
		(m.Reasoning != nil || len(m.ReasoningDetails) > 0)
}

// thinkingWouldBeStripped reports whether appending a non-tool-result message to msgs would,
// on a last-turn-only model, strip every thinking block before the LAST message from the
// backend's own cached context — see summarizerProfiles.thinkingLastTurnOnly for the citation
// and the live-measured gap this check exists to close. The last message itself is excluded:
// appending AFTER it is exactly the turn whose thinking (if any) survives on either kind of
// model, so it carries no risk of its own.
func thinkingWouldBeStripped(msgs []bschemas.ChatMessage) bool {
	if len(msgs) == 0 {
		return false
	}
	for _, m := range msgs[:len(msgs)-1] {
		if hasThinkingBlock(m) {
			return true
		}
	}
	return false
}

func (s *CacheAwareSummarizer) Offload(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	msgs := req.Input
	// Attribute any spend a DETACHED summarizer call incurred since this session's last turn.
	// First thing, and unconditionally: the money was spent whatever this turn decides.
	takeDeferredUsage(c, rep)
	headCount, start, end := summarizeSpan(msgs, 1, s.keepLastTurns)
	if end <= start {
		rep.Skipped = true
		return nil, nil
	}
	// Never summarize away content the agent just expanded: that is the bounce loop cg:keep:
	// exists to prevent, and it falsifies expand.RestoredInPlace's "present above" pointer.
	if trimmed := trimSpanForKeptVerbatim(msgs, start, end, func(content string) bool {
		_, skip := skipReduce(c, content)
		return skip || restoredSpanGuard(c, content)
	}); trimmed <= start {
		rep.Skipped = true
		return nil, nil
	} else {
		end = trimmed
	}

	// Computed HERE, before tryReuse, because tryReuse itself now needs it: a checkpoint
	// commissioned under cache_state: pre_expiry is held IN RESERVE (see sumCheckpoint.Reserved)
	// until a turn whose phase is Cold or Unknown graduates it — see cacheAwareApplyPhase and the
	// package comment on cache_aware_summarizer's two cache-state modes.
	phase := c.CachePhase(s.trigger.PreExpiry())

	// Reuse first, UNCONDITIONALLY ON THE TRIGGER — the gates below decide whether to PAY for a
	// FRESH summary, never whether an already-paid-for checkpoint stays in the body we forward.
	// Those were one decision until the trigger gained a cache-state condition, and conflating
	// them is cache-DESTRUCTIVE rather than merely a lost saving: `cache_state: pre_expiry` is true
	// on a small fraction of turns by design, so a turn the gate declines would otherwise forward
	// the FULL transcript — bytes that diverge from the cached prefix at the first summarized
	// message, forcing the 1.25x suffix rewrite this component exists to avoid, on every quiet
	// turn instead of saving it. See summarize.Offload's own comment on this; the two components
	// share the bug class (and, now, the fix).
	//
	// UNCONDITIONAL IS NO LONGER THE WHOLE STORY under cache_state: pre_expiry. tryReuse itself
	// now withholds a RESERVED checkpoint (one commissioned while the cache was still believed
	// live) from this splice until phase graduates it — see tryReuse's own comment. `any` mode
	// never reserves, so this call is exactly as unconditional for it as it always was.
	out, keys, ok, stale := s.tryReuse(c, msgs, headCount, start, end, phase)
	if ok {
		if len(keys) == 0 {
			rep.Irreversible = true // reused a non-full checkpoint (nothing stashed)
		}
		req.Input = out
		return keys, nil
	}
	// declineButReplayStale is every later decline's fallback: a checkpoint whose covered hash
	// still matched tryReuse's check is a faithful summary even when it is too stale to be
	// REUSED as-is (resummarize_tokens exceeded) or when a resource below (model, stash room,
	// an unverified role) is simply unavailable this turn. Sending it beats sending the full
	// transcript — those bytes are what an earlier turn already forwarded — so every decline
	// from here on tries it before giving up. See replayStale.
	declineButReplayStale := func() ([]string, error) {
		if stale {
			if out, keys, ok := s.replayStale(c, msgs, headCount, start); ok {
				if len(keys) == 0 {
					rep.Irreversible = true
				}
				req.Input = out
				return keys, nil
			}
		}
		rep.Skipped = true
		return nil, nil
	}

	// THE GATES DECIDE WHETHER TO PAY FOR A FRESH SUMMARY. `sized`/`resolvable` ask "is this
	// request big enough" on the two separate rulers Trigger.Fires itself keeps separate (see its
	// own docstring); `phased` asks "does the configured cache_state permit firing on THIS turn's
	// cache phase" via Ctx.CachePhase/Trigger.CacheAllows — the thing this component used to never
	// ask (#400). A turn that fails `sized` or `resolvable` has nothing worth registering for the
	// keep-alive substitution below either, so those two decline (falling back to a stale replay
	// first) rather than reaching the resource checks; a turn that fails only `phased` still has
	// a candidate worth keeping (fill is there, the component is just waiting for the right cache
	// phase to spend on it), so it falls through to the resource checks and the keep-alive
	// registration before declining.
	sized := s.trigger.Fires(req, c)
	if !sized {
		rep.Gate("below_request_trigger")
	}
	resolvable := s.trigger.FracResolvable(c)
	if !resolvable {
		rep.Gate("window_not_exact")
	}
	if !sized || !resolvable {
		return declineButReplayStale()
	}
	phased := s.trigger.CacheAllows(c, phase)
	if !phased {
		rep.Gate("cache_state_declined_" + phase.String())
	}

	span := msgs[start:end]
	if schema.MessagesTokens(&bschemas.BifrostChatRequest{Input: span}) < s.minTokens {
		return declineButReplayStale()
	}
	// The bill is set by the WHOLE conversation, because that is what gets sent. Checked before the
	// client is even resolved, so an over-large session costs nothing to decline.
	if s.maxRequestTokens > 0 &&
		schema.MessagesTokens(&bschemas.BifrostChatRequest{Input: msgs}) > s.maxRequestTokens {
		atomic.AddInt64(&cacheAwareTooLarge, 1)
		rep.Gate("request_over_max_tokens")
		return declineButReplayStale()
	}
	model := s.modelClient
	if model == nil {
		model = c.Model.For(s.modelSource)
	}
	var mm components.MessagesModel
	if model != nil {
		mm, _ = model.(components.MessagesModel)
	}
	// usePrefixAsk is the fallback for every route whose client has no MessagesModel at all —
	// which on this deployment's own traffic is every Anthropic incoming-model request (#275).
	// Checked independently of `model`: Ctx.PrefixAsk is a SEPARATE client the host resolves for
	// itself (the previous turn's stashed sent body + the caller's own credential), not something
	// derived from model.source, so it can be usable even when `model` resolved to nil.
	usePrefixAsk := mm == nil && c.PrefixAsk != nil
	if mm == nil && !usePrefixAsk {
		if model != nil {
			// A model WAS resolved and simply cannot take a message array — the configured
			// client's own capability gap, distinct from "nothing configured at all".
			atomic.AddInt64(&cacheAwareDeclined, 1)
			rep.Gate("no_messages_model")
		}
		return declineButReplayStale()
	}
	profiles := s.resolveProfiles()
	// Declines BEFORE building anything: on a last-turn-only model, appending the instruction
	// after an earlier thinking block strips that block from the backend's own cached context
	// (see thinkingLastTurnOnly), so the cache this component exists to read is smaller than
	// the dashboard's own numbers would suggest — silently, which is worse than not compacting
	// at all. c.ModelName is the ACTUAL model this request targets, not s.modelID (the
	// operator's pinned model for the system-role check below, a different question).
	if profiles.thinkingLastTurnOnly(c.ModelName) && thinkingWouldBeStripped(msgs) {
		atomic.AddInt64(&cacheAwareThinkingWouldStrip, 1)
		rep.Gate("thinking_would_be_stripped")
		logging.From(c.Ctx).Debug("cg.cache_aware_summarizer.thinking_would_be_stripped",
			"session", c.Session, "model", c.ModelName)
		return declineButReplayStale()
	}
	// The pinned-system guard, at the point of use. Declining costs this arm its compaction;
	// proceeding would risk a summary that is really the model's next turn, which no metric here
	// would reveal. Moot on the PrefixAsk path, which can never send a system-role instruction at
	// all (see the role resolution below) — an operator who pinned `system` while this turn can
	// only reach the model via PrefixAsk is told so explicitly rather than silently downgraded.
	if s.explicitSystem {
		if usePrefixAsk {
			rep.Gate("system_role_unsupported_via_prefix_ask")
			return declineButReplayStale()
		}
		if r, matched := profiles.roleFor(s.modelID); !matched || r != bschemas.ChatMessageRoleSystem {
			atomic.AddInt64(&cacheAwareUnverifiedSystem, 1)
			rep.Gate("unverified_system_role")
			return declineButReplayStale()
		}
	}
	role := s.instructionRole
	if s.roleAuto && s.profilesPath != "" {
		role, _ = profiles.roleFor(s.modelID)
	}
	if usePrefixAsk {
		// components.PrefixAsker always appends the question as a trailing USER message (see its
		// own docstring) — there is no channel for a system-role instruction through this path.
		// Overridden HERE rather than at `role`'s own resolution above, because that resolution
		// also has to stay correct for a deployment that later gets a MessagesModel on the same
		// config.
		role = bschemas.ChatMessageRoleUser
	}
	instruction := bschemas.ChatMessage{Role: role}
	schema.SetMessageText(&instruction, profiles.prompt(role))

	// Build the ONE call this commission will make, whichever path it takes — see summaryCaller
	// in cache_aware_async.go. Built HERE, on the request's own goroutine, and not a moment later:
	// everything it closes over (askCopy, or the asker/session/instruction trio) must be read
	// before this function returns, the same discipline startAsyncSummary's own copy used to
	// enforce for `ask` directly.
	var call summaryCaller
	// path tags every dashboard row and /stats figure this commission produces (see
	// deferredCall in summarize_async.go) with which model path actually ran — the only way an
	// operator can tell "this is compacting via PrefixAsk on my Anthropic traffic" from "this
	// never left the no_messages_model decline".
	path := "messages"
	if usePrefixAsk {
		path = "prefix_ask"
		atomic.AddInt64(&cacheAwarePrefixAskUsed, 1)
		asker, session, instructionText := c.PrefixAsk, c.Session, schema.MessageText(instruction)
		// requiredCount/requiredHash freeze THIS turn's own idea of what span the eventual call
		// must cover — computed now, synchronously, while msgs is still exactly what this turn
		// saw. The call itself runs later: detached (startAsyncSummary) or substituted for an
		// idle ping (commissionSync), sometimes turns after this one returns. See the
		// STALE-PREFIX GUARD comment below for why the check belongs IN the call rather than
		// here: at this point in Offload, the host's stash still holds (at most) the PREVIOUS
		// turn's body — THIS turn's own body is not stashed until after Offload returns — so a
		// check made HERE would fail on every ordinary turn whose own span simply grew past
		// what the previous turn's stash held, not only the stale one. By the time the call
		// actually runs, the host has had the chance to stash at least this turn's own
		// forwarded body.
		requiredCount, requiredHash := end, spanHash(msgs[:end])
		call = func(ctx context.Context) (string, error) {
			// STALE-PREFIX GUARD. The PrefixAsk path answers from whatever body the host's
			// stash holds for this session, which this component never built and cannot
			// otherwise verify. That body can be the wrong one — proxy's sentStash drops a
			// body over its own size cap rather than updating it, so a session whose turn grew
			// past that cap can leave an OLDER, shorter body in the stash — and Ask would
			// silently answer from a transcript that does not cover `requiredCount`, producing
			// a checkpoint that CLAIMS to cover msgs[start:end] while the model never saw most
			// of it. Checked here, immediately before paying for the call, against the
			// asker's own reported coverage (components.PrefixCoverage); an asker that does not
			// implement it is trusted as before — this guard exists to catch a NEW failure
			// mode, not to narrow who may answer.
			if pc, ok := asker.(components.PrefixCoverage); ok &&
				!pc.CoversSpan(session, requiredCount, requiredHash) {
				return "", components.ErrStalePrefix
			}
			reply, _, err := asker.Ask(ctx, session, instructionText)
			// usage is NOT read here: PrefixAsker.Ask's underlying CompletePrefixed/
			// CompletePrefixedResponses already records cache_read/cache_write into the ambient
			// cheapmodel sink (recordUsageCache) exactly as CompleteMessages does, so every
			// caller of `call` that reads usage off a WithDetachedSink-scoped ctx (both
			// startAsyncSummary and commissionSync do) gets it identically either way.
			return reply, err
		}
	} else {
		// [conversation..., instruction]. The conversation is passed UNMODIFIED and in order: any
		// edit changes the rendered prefix and forfeits the match this component exists for.
		ask := make([]bschemas.ChatMessage, 0, len(msgs)+1)
		ask = append(ask, msgs...)
		ask = append(ask, instruction)
		call = func(ctx context.Context) (string, error) { return mm.CompleteMessages(ctx, "", ask) }
	}

	// Register this turn's commission material as the idle keep-alive's substitute for a bare
	// cache-read ping, REGARDLESS of `phased` below. A turn the cache-state gate just declined
	// (fill is there; the component is waiting for the right phase) is exactly the session whose
	// NEXT ping the keeper would otherwise spend on a 1-token read — and this material lets it
	// spend on a cache-reading summary instead. Registering when `phased` is also true is safe:
	// the registration is only ever consumed between requests, after this turn has already either
	// fired (so the registry's own single-flight check in commissionSummary refuses a second
	// dispatch and the keeper falls back to an ordinary ping) or deferred for cold (same check).
	// See cache_aware_keepalive.go.
	s.registerKeepAliveCandidate(c, call, path, span, end-start)

	// Don't pay for a summary the store cannot keep: a full marker promises the span is
	// restorable, so check the room before the call rather than discovering it after.
	//
	// DELIBERATELY AFTER registration, not before. This is a check on whether the store has
	// capacity RIGHT NOW, which is exactly the kind of transient condition that can be true again
	// by the time a later turn or the keep-alive substitute actually dispatches — unlike the
	// model/role checks above it, which are permanent for this session as configured. Blocking
	// registration on it needlessly cost the keep-alive opportunity: a review of a live session
	// found candidates going unregistered on turns that recorded cache_state_declined_* because
	// a LATER check in this function (this one, in its old position) returned first, with no
	// trace of which check it was. commitAsyncSummary re-checks StashRoom itself before writing
	// anything, so moving this later costs no safety.
	mode := effectiveMode(c, s.mode)
	spanJSON, jerr := json.Marshal(span)
	if jerr != nil {
		return nil, jerr
	}
	if mode == markerFull && !store.StashRoom(c.Store, len(spanJSON)) {
		atomic.AddInt64(&cacheAwareRefusedStash, 1)
		rep.Gate("no_stash_room")
		return declineButReplayStale()
	}

	if !phased {
		return declineButReplayStale()
	}

	// COLD-TURN ORDERING. Firing the side call now, while THIS turn's own forwarded request is
	// also about to rewrite the same cold prefix, pays the full-prefix rewrite TWICE instead of
	// once: the side call misses the cache exactly as the forwarded request does, so neither of
	// them is the cheap cache READ this component's whole argument depends on. Deferring until the
	// forwarded request's own usage confirms the rewrite happened turns that into a guaranteed
	// cache HIT — see cache_aware_cold.go for the registry and proxy's call into
	// ResolveDeferredCacheAwareSummary once that usage is observed.
	//
	// Only CachePhaseCold needs this. Unknown is permitted to fire (CacheAllows' own Unknown rule)
	// but says nothing about whether the prefix is actually gone, so there is no established
	// double-rewrite to avoid guarding against; Warm and PreExpiry mean the prefix the forwarded
	// request is about to send is itself still live, so the side call already reads it.
	if phase == components.CachePhaseCold {
		s.deferColdSummary(c, call, path, span, end-start)
		rep.Event("cold_commission_deferred")
		rep.Skipped = true
		return nil, nil
	}

	// reserved marks a cache_state: pre_expiry commission as NOT YET APPLIED — see
	// sumCheckpoint.Reserved and tryReuse's own comment. Only PreExpiry reaches this line under
	// pre_expiry mode (CacheAllows permits only PreExpiry and Unknown for it, and Unknown is
	// deliberately excluded here: it means no cache-aware tracking exists on this
	// request/deployment at all — the same reasoning Trigger.CacheAllows' own docstring gives for
	// why a size-gated compactor must fire on Unknown rather than treat it as "wait and see". A
	// reserve that can only graduate on Cold would never graduate on a deployment whose phase is
	// always Unknown, so Unknown applies immediately instead, exactly like `any`.
	reserved := s.trigger.CacheState == components.CacheStatePreExpiry && phase == components.CachePhasePreExpiry

	// COMMISSION, do not block. The call covers most of the transcript against a 300 s budget, so
	// running it inline would stall the triggering turn by minutes — billed against the agent's own
	// timeout. summarize_async.go exists for exactly this reason. So this turn forwards UNTOUCHED
	// and the next eligible turn finds the checkpoint and splices; see cache_aware_async.go.
	if gate := s.startAsyncSummary(c, call, path, "turn", reserved, span, end-start); gate != "" {
		rep.Gate(gate)
	}
	rep.Skipped = true
	return nil, nil
}

// splice builds [head, summary, tail] and repairs orphans already present in the inbound
// request — alignment stops this component from CREATING one, but not from forwarding one.
func (s *CacheAwareSummarizer) splice(msgs []bschemas.ChatMessage, headCount, boundary int, summaryText string) []bschemas.ChatMessage {
	// USER role, never system: a system message anywhere but index 0 is rejected by the provider.
	// That is independent of the INSTRUCTION's role, which is appended at the end of a fork
	// request where a trailing system message is legal on the models the registry marks.
	summaryMsg := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleUser}
	schema.SetMessageText(&summaryMsg, summaryText)
	out := make([]bschemas.ChatMessage, 0, headCount+1+len(msgs)-boundary)
	out = append(out, msgs[:headCount]...)
	out = append(out, summaryMsg)
	out = append(out, msgs[boundary:]...)
	if repaired, n := dropOrphanedToolResults(out); n > 0 {
		out = repaired
	}
	return out
}

// tryReuse re-emits the existing summary when the covered prefix is byte-unchanged and the tail
// since that checkpoint is still under resummarize_tokens. No model call, and the summary message
// is identical to the one the previous turn forwarded — which is the property that keeps the
// provider's prefix alive past the head.
//
// ⚠️ The checkpoint namespace is shared with `summarize` (store.SumPrefix + session). Safe
// because both components restructure the message list and therefore must run ALONE, so they
// cannot both be in one pipeline; and CoveredHash rejects a checkpoint whose covered prefix does
// not match, so a preset switch mid-session degrades to a fresh summary rather than reusing a
// stale one.
// tryReuse's fourth return, stale, is what lets a turn the fresh-commission gates decline still
// replay an EXISTING checkpoint instead of forwarding the full transcript — see replayStale and
// the comment at this function's call site in Offload. Mirrors summarize.tryReuse's own
// (out, keys, ok, stale) shape and for the identical reason: a checkpoint whose covered hash
// still matches is a faithful summary of msgs[start:boundary] whether or not its tail has grown
// past resummarize_tokens, so "stale" and "no checkpoint at all" cannot share one false — the
// caller needs to tell them apart to know whether there is anything left to fall back to.
// cacheAwareApplyPhase reports whether a RESERVED checkpoint (one commissioned under
// cache_state: pre_expiry while the cache was still believed live) may graduate — be spliced
// into the forwarded body, and marked no-longer-reserved from then on — on a turn classified at
// this phase.
//
// Cold is the phase the reserve design exists for: the cache is actually gone, so splicing now
// costs nothing that was not already lost, and the whole point of holding the summary in reserve
// was to have it ready for exactly this moment instead of paying for it inline on the cold turn.
//
// Unknown ALSO graduates, and that is a deliberate asymmetry with PreExpiry/Warm rather than an
// oversight. Unknown means no cache-aware tracking exists for this request at all — a
// non-caching provider, `cache_mode: off`, a bypassed turn, a first turn — so there is no live
// prefix a graduation-on-Unknown could disturb. A reserve that could ONLY graduate on Cold would
// never graduate at all on a deployment whose phase is always Unknown, which would make
// cache_state: pre_expiry silently dead there — the identical failure mode
// Trigger.CacheAllows' own docstring already rejects for the same reason, applied here to
// graduation instead of to firing.
func cacheAwareApplyPhase(phase components.CachePhase) bool {
	return phase == components.CachePhaseCold || phase == components.CachePhaseUnknown
}

// tryReuse re-emits (or, for a reserved checkpoint, GRADUATES and then re-emits) an existing
// checkpoint. phase is the CURRENT turn's cache phase, needed only for the Reserved case — see
// cacheAwareApplyPhase.
//
// WHY A RESERVED CHECKPOINT IS GATED HERE AND NOT AT THE CALL SITE: Offload calls this
// unconditionally, before any of its own gates run (see its own comment on why reuse must be
// unconditional on the TRIGGER). A reserve is a narrower exception to that unconditionality —
// not "has the trigger decided to pay", but "has this specific checkpoint earned the right to be
// LIVE yet" — and keeping both decisions in the one function that already owns the checkpoint's
// shape keeps a future change to one from silently missing the other.
func (s *CacheAwareSummarizer) tryReuse(c *components.Ctx, msgs []bschemas.ChatMessage, headCount, start, end int, phase components.CachePhase) (out []bschemas.ChatMessage, keys []string, ok, stale bool) {
	cp, found := loadCheckpoint(c)
	if !found || cp.CoveredCount <= 0 || cp.SummaryMsg == "" {
		return nil, nil, false, false
	}
	boundary := start + cp.CoveredCount
	if boundary > end {
		return nil, nil, false, false
	}
	covered := msgs[start:boundary]
	if spanHash(covered) != cp.CoveredHash {
		return nil, nil, false, false // prefix diverged (different session / edited) → fresh
	}
	// RESERVE GATING. Only ever true for a checkpoint commissioned under THIS component's
	// CURRENT cache_state: pre_expiry — a leftover Reserved flag from a config that has since
	// switched to `any` is deliberately ignored (`any` has no reserve concept, so an existing
	// checkpoint is live the moment it is found, exactly as it always was for that mode).
	if cp.Reserved && s.trigger.CacheState == components.CacheStatePreExpiry {
		if !cacheAwareApplyPhase(phase) {
			// Still waiting for a cold (or untracked) return. NOT reported as stale: staleness
			// is a question about whether an ALREADY-LIVE checkpoint needs refreshing, which
			// does not arise until this one graduates — reporting it here would let
			// declineButReplayStale's fallback splice a reserve into a WARM turn, which is
			// exactly the cache-destructive rewrite pre_expiry's reserve design exists to defer.
			return nil, nil, false, false
		}
		// Graduating NOW: splice whatever is held, however stale its tail has grown — a fresh
		// re-summarize at this exact turn would reintroduce the same double-rewrite risk
		// cold-turn deferral already guards against for cache_state: any (see Offload's own
		// cold-defer branch), and the point of holding a reserve was to have something ready for
		// this moment rather than paying for it inline here. Persisted before splicing so a
		// concurrent request on the same session cannot double-graduate it.
		cp.Reserved = false
		saveCheckpoint(c, cp)
		if cp.Key != "" {
			if b, err := json.Marshal(covered); err == nil {
				c.Store.Put(cp.Key, b)
			}
		}
		spliced := s.splice(msgs, headCount, boundary, cp.SummaryMsg)
		if cp.Key != "" {
			return spliced, []string{cp.Key}, true, false
		}
		return spliced, nil, true, false
	}
	// resummarizeTokens <= 0 means "roll the checkpoint forward on every eligible turn" rather
	// than "never reuse" — checked HERE, after the hash match, so it reports STALE rather than
	// nothing. A caller that cannot pay for a roll-forward (the fresh gates just declined) still
	// needs to be told a valid checkpoint exists, or a config carrying `resummarize_tokens: 0`
	// would replay nothing and oscillate between the full and summarized shapes turn to turn.
	if s.resummarizeTokens <= 0 ||
		schema.MessagesTokens(&bschemas.BifrostChatRequest{Input: msgs[boundary:end]}) >= s.resummarizeTokens {
		return nil, nil, false, true
	}
	// Refresh the stashed original so expand keeps resolving it.
	if cp.Key != "" {
		if b, err := json.Marshal(covered); err == nil {
			c.Store.Put(cp.Key, b)
		}
	}
	spliced := s.splice(msgs, headCount, boundary, cp.SummaryMsg)
	if cp.Key != "" {
		return spliced, []string{cp.Key}, true, false
	}
	return spliced, nil, true, false
}

// replayStale re-emits an existing-but-stale checkpoint when the fresh-commission gates decline
// (below the size threshold, an unresolvable window, or a cache_state the current phase does not
// permit). The covered hash still matched in tryReuse's check — this checkpoint is a faithful
// summary of msgs[start:boundary], merely not rolled forward to also cover the newest messages —
// so sending it is strictly better than the full transcript: those bytes are what an EARLIER turn
// already forwarded, and the provider has already cached a prefix ending at them. Mirrors
// summarize.replayStale; see that function's own comment for why the fresh gates must not also
// gate this fallback (they decide whether to PAY for a new summary, never whether an old one
// stays in the body).
func (s *CacheAwareSummarizer) replayStale(c *components.Ctx, msgs []bschemas.ChatMessage, headCount, start int) ([]bschemas.ChatMessage, []string, bool) {
	cp, ok := loadCheckpoint(c)
	if !ok || cp.CoveredCount <= 0 || cp.SummaryMsg == "" {
		return nil, nil, false // nothing to fall back to
	}
	boundary := start + cp.CoveredCount
	if boundary > len(msgs) {
		return nil, nil, false
	}
	covered := msgs[start:boundary]
	// The stash refresh, for the same reason tryReuse's does one: this payload was accepted on
	// the turn the checkpoint was made, so a key already present is retained whatever the
	// reserve says, and a refresh failure only means the replayed marker is dangling — the
	// replay still proceeds, because the summary text must stay byte-identical to the turn that
	// created it.
	if cp.Key != "" {
		if b, err := json.Marshal(covered); err == nil {
			c.Store.Put(cp.Key, b)
		}
	}
	out := s.splice(msgs, headCount, boundary, cp.SummaryMsg)
	// A MESSAGE-count shrink is not a TOKEN shrink: emitCheckpoint-equivalent splicing can return
	// fewer messages but MORE tokens on a short covered span replaced by a verbose summary, and
	// the pipeline's never-worse rule would then revert this component anyway — forwarding the
	// full transcript, the exact byte-flip this fallback exists to avoid, just silently instead
	// of with a name attached.
	if len(out) >= len(msgs) ||
		schema.MessagesTokens(&bschemas.BifrostChatRequest{Input: out}) >=
			schema.MessagesTokens(&bschemas.BifrostChatRequest{Input: msgs}) {
		return nil, nil, false
	}
	if cp.Key != "" {
		return out, []string{cp.Key}, true
	}
	return out, nil, true
}

func cacheAwareSummaryWrapper(summary, key string, mode markerMode) string {
	body := "=== Compacted Context (cache-aware summary) ===\n" +
		"The earlier part of this conversation has been compacted into the summary below. " +
		"The messages before this one, and the messages after it, are verbatim originals.\n\n" +
		summary + "\n\n" +
		"Treat this summary as the earlier context and the following messages as the most " +
		"recent context, then continue the task. Do not summarize the conversation again."
	switch mode {
	case markerFull:
		return body + "\n" + expand.Marker(key) + " [full compacted span: call " + expand.ToolName + "]"
	case markerSummary:
		return body + "\n" + expand.SummaryMarker
	default:
		return body
	}
}
