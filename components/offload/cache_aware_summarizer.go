package offload

import (
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
// ⛔ IT NEEDS A components.MessagesModel AND DECLINES WITHOUT ONE. A plain Model flattens the
// conversation to one string, which is exactly the prefix-destroying shape this exists to avoid,
// so a client without the capability makes the component skip. A silent fallback would report
// this method's latency while paying the old method's cost. Because a declining arm compacts
// nothing and is byte-identical to `off` on every other metric, the decline is COUNTED: read
// CacheAwareSummarizerDeclined before believing any delta from this arm.
//
// Span selection, kept-verbatim repair and orphan repair all come from summarize_pairing.go —
// the same functions `summarize` uses, not a second implementation. A tool exchange is atomic
// and both boundaries must respect that; a private copy of the invariant would be a second
// place for it to drift, and the drift would surface as an exception-rate difference blamed on
// the method under test.
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
	Profiles     []struct {
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
)

func CacheAwareSummarizerCalls() int64    { return atomic.LoadInt64(&cacheAwareCalls) }
func CacheAwareSummarizerTimeouts() int64 { return atomic.LoadInt64(&cacheAwareTimeouts) }
func CacheAwareSummarizerErrors() int64   { return atomic.LoadInt64(&cacheAwareErrors) }

// CacheAwareSummarizerDeclined counts turns that reached the model step and stopped because no
// components.MessagesModel was available. That is the ONLY reason it counts — a role the backend
// rejects returns an error and lands in Errors, not here. Non-zero means this arm is not
// measuring cache-reuse compaction; it is measuring `off`.
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
	components.RegisterFields("cache_aware_summarizer", cacheAwareSummarizerConfig{}, append([]components.Field{
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
	}, append(modelFields("model"), components.TriggerFields("trigger")...)...))
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
	if err := cfg.Trigger.Validate("cache_aware_summarizer"); err != nil {
		return nil, err
	}
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
func (s *CacheAwareSummarizer) Offload(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	msgs := req.Input
	// Attribute any spend a DETACHED summarizer call incurred since this session's last turn.
	// First thing, and unconditionally: the money was spent whatever this turn decides.
	takeDeferredUsage(c)
	headCount, start, end := summarizeSpan(msgs, s.keepLastTurns)
	if !s.trigger.Fires(req, c) || end <= start {
		rep.Skipped = true
		return nil, nil
	}
	// Never summarize away content the agent just expanded: that is the bounce loop cg:keep:
	// exists to prevent, and it falsifies expand.RestoredInPlace's "present above" pointer.
	if trimmed := trimSpanForKeptVerbatim(msgs, start, end, func(content string) bool {
		_, skip := skipReduce(c, content)
		return skip
	}); trimmed <= start {
		rep.Skipped = true
		return nil, nil
	} else {
		end = trimmed
	}

	// Reuse first: no model call, and the spliced bytes stay identical so the forwarded prefix is
	// stable. This is the path that makes the component's name true.
	if out, keys, ok := s.tryReuse(c, msgs, headCount, start, end); ok {
		req.Input = out
		return keys, nil
	}

	span := msgs[start:end]
	if schema.MessagesTokens(&bschemas.BifrostChatRequest{Input: span}) < s.minTokens {
		rep.Skipped = true
		return nil, nil
	}
	// The bill is set by the WHOLE conversation, because that is what gets sent. Checked before the
	// client is even resolved, so an over-large session costs nothing to decline.
	if s.maxRequestTokens > 0 &&
		schema.MessagesTokens(&bschemas.BifrostChatRequest{Input: msgs}) > s.maxRequestTokens {
		atomic.AddInt64(&cacheAwareTooLarge, 1)
		rep.Gate("request_over_max_tokens")
		rep.Skipped = true
		return nil, nil
	}
	model := s.modelClient
	if model == nil {
		model = c.Model.For(s.modelSource)
	}
	if model == nil {
		rep.Skipped = true
		return nil, nil
	}
	mm, okModel := model.(components.MessagesModel)
	if !okModel {
		atomic.AddInt64(&cacheAwareDeclined, 1)
		rep.Gate("no_messages_model")
		rep.Skipped = true
		return nil, nil
	}
	// Don't pay for a summary the store cannot keep: a full marker promises the span is
	// restorable, so check the room before the call rather than discovering it after.
	mode := effectiveMode(c, s.mode)
	// Size the room check against the payload we would actually stash, not a token guess.
	spanJSON, jerr := json.Marshal(span)
	if jerr != nil {
		return nil, jerr
	}
	if mode == markerFull && !store.StashRoom(c.Store, len(spanJSON)) {
		atomic.AddInt64(&cacheAwareRefusedStash, 1)
		rep.Gate("no_stash_room")
		rep.Skipped = true
		return nil, nil
	}

	profiles := s.resolveProfiles()
	// The pinned-system guard, at the point of use. Declining costs this arm its compaction;
	// proceeding would risk a summary that is really the model's next turn, which no metric here
	// would reveal.
	if s.explicitSystem {
		if r, matched := profiles.roleFor(s.modelID); !matched || r != bschemas.ChatMessageRoleSystem {
			atomic.AddInt64(&cacheAwareUnverifiedSystem, 1)
			rep.Gate("unverified_system_role")
			rep.Skipped = true
			return nil, nil
		}
	}
	role := s.instructionRole
	if s.roleAuto && s.profilesPath != "" {
		role, _ = profiles.roleFor(s.modelID)
	}
	instruction := bschemas.ChatMessage{Role: role}
	schema.SetMessageText(&instruction, profiles.prompt(role))

	// [conversation..., instruction]. The conversation is passed UNMODIFIED and in order: any
	// edit changes the rendered prefix and forfeits the match this component exists for.
	ask := make([]bschemas.ChatMessage, 0, len(msgs)+1)
	ask = append(ask, msgs...)
	ask = append(ask, instruction)

	// COMMISSION, do not block. The call covers most of the transcript against a 300 s budget, so
	// running it inline would stall the triggering turn by minutes — billed against the agent's own
	// timeout. summarize_async.go exists for exactly this reason. So this turn forwards UNTOUCHED
	// and the next eligible turn finds the checkpoint and splices; see cache_aware_async.go.
	if gate := s.startAsyncSummary(c, mm, ask, span, end-start); gate != "" {
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
func (s *CacheAwareSummarizer) tryReuse(c *components.Ctx, msgs []bschemas.ChatMessage, headCount, start, end int) ([]bschemas.ChatMessage, []string, bool) {
	if s.resummarizeTokens <= 0 {
		return nil, nil, false
	}
	cp, ok := loadCheckpoint(c)
	if !ok || cp.CoveredCount <= 0 || cp.SummaryMsg == "" {
		return nil, nil, false
	}
	boundary := start + cp.CoveredCount
	if boundary > end {
		return nil, nil, false
	}
	covered := msgs[start:boundary]
	if spanHash(covered) != cp.CoveredHash {
		return nil, nil, false
	}
	if schema.MessagesTokens(&bschemas.BifrostChatRequest{Input: msgs[boundary:end]}) >= s.resummarizeTokens {
		return nil, nil, false
	}
	// Refresh the stashed original so expand keeps resolving it.
	if cp.Key != "" {
		if b, err := json.Marshal(covered); err == nil {
			c.Store.Put(cp.Key, b)
		}
	}
	out := s.splice(msgs, headCount, boundary, cp.SummaryMsg)
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
