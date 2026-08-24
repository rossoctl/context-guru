package config

import (
	"bytes"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/rossoctl/context-guru/components"
	"gopkg.in/yaml.v3"
)

// Form is the dashboard's configuration as FIELDS rather than as YAML text, in both
// directions: ParseForm reads a document into it for rendering, ApplyForm writes it back
// into one on save.
//
// It exists because the browser used to do this with regular expressions, and that was not
// a stylistic problem, it was a data-loss bug. The save path replaced the whole `pipeline:`
// line with a flow-style one, so a document whose pipeline was written as a YAML block
// sequence
//
//	pipeline:
//	  - format
//	  - extract
//
// became
//
//	pipeline: [format, extract_llm]
//	  - format
//	  - extract
//
// which is `config: yaml: line 3: did not find expected key` — the error two accounts hit
// on every single save, unrecoverable from the UI because the refusal left the stored
// document in place for the next attempt to mangle again. A `preset:` document lost its
// whole pipeline the same way, silently, because the regex found no flow-style line to read
// the existing names out of.
//
// A real YAML library on the server cannot make that class of mistake, and the round trip
// preserves every key the form does not know about. What it does NOT preserve is comments
// and key order: a Go map has neither. That is the deliberate trade — the settings page is
// fields now, so nothing in the document is hand-written prose any more.
//
// # Why it is descriptor-driven
//
// The first version hand-wrote one Go struct field, one YAML key and one JavaScript control
// per knob. It reached 18 keys of about a hundred, one component of fifteen, and it had
// already drifted in both directions: the browser's default table duplicated the server's,
// and the strategy list was missing a value the engine accepts (see R8 in
// docs/how-to/settings-form.md). So the fields are now DECLARED beside the config struct
// they describe — components.Field — and everything here is a walk over those declarations.
// Adding a knob to a component adds it to the form, and forgetting to is a test failure.
type Form struct {
	// Pipeline is the component run order, resolved: for a `preset:` document it is what
	// the preset expands to, not the empty list the text literally contains. It is also
	// the ONLY statement of what is enabled — see R2 in the docs: a block is configuration,
	// not enablement, and reading "no block" as "off" silently removed components from
	// bare-preset accounts on save.
	Pipeline []string `json:"pipeline"`
	// PipelineKnown lists the component names the CLIENT rendered a control for. Any name in
	// the STORED pipeline that is not in this list is preserved at its original index,
	// whatever Pipeline says — so removing a component is an act a client has to CLAIM, and a
	// client that cannot see one cannot take it out.
	//
	// It exists because a save silently reverted an operator's pipeline: `linecap` was
	// dropped and `toon` re-added. The server never knew either name — the pipeline is a
	// wholesale write of whatever the browser rebuilt out of its checkbox grid — so a stale
	// render or a cached bundle whose /api/options predated a component produces exactly that
	// diff, and the server had no way to tell it from a deliberate removal. Now it does.
	//
	// Empty or absent means "this client declared nothing", and then NOTHING may be removed.
	// That is the safe direction for an old bundle and for a hand-rolled API client: both can
	// still add and reorder.
	PipelineKnown []string `json:"pipeline_known,omitempty"`
	// PipelineBase is the pipeline the page RENDERED from. A mismatch against the stored
	// document means the document moved under the page, and the save is refused with a 409 —
	// which is what stops a stale page ADDING a component back. Preserving the unmodelled
	// only defends against dropping.
	//
	// nil is accepted (an older bundle does not send it) and then PipelineKnown is the only
	// defence, which is again the safe direction. Checked at the HTTP layer, where the stored
	// document is: see proxy.Handler.ctlUpdateMe.
	PipelineBase []string `json:"pipeline_base,omitempty"`
	Mode         string   `json:"mode"`
	// Cache is the `cache:` block — host-level prompt-cache policy (the idle keep-alive and
	// the mixed-TTL head), which is not a component and so has no descriptor to be drawn
	// from. nil means the form does not state it and ApplyForm leaves the document's own
	// block alone, exactly as an absent component block does.
	//
	// A pointer rather than a value for that reason alone: `keepalive: false` and "this form
	// has nothing to say about the cache" are different instructions, and with a value type
	// every save from a page that had not drawn the control would switch a tenant's
	// keep-alive off.
	//
	// Reachable only from a direct Go or API caller, in practice. ParseForm's strict path
	// always fills it in (cacheForm is unconditional), and its loose path leaves it nil but
	// also sets ParseError — which the save route answers with a 409 before ApplyForm is ever
	// called. So the nil branch in ApplyForm is a contract for library callers, not a state
	// the settings page can produce.
	Cache *CacheForm `json:"cache,omitempty"`
	// Components holds, per component name, the DOTTED key paths that component's block
	// actually states — `{"extract_llm": {"cold_cache.min_tokens": 800}}`. Only keys the
	// document really carries are present: an absent key means "the component's default",
	// which is a different thing from a value, and prefilling defaults here is how a save
	// wrote 20 over a deliberate `llm_max_per_session: 0` (R3).
	//
	// A component missing from this map is not touched by ApplyForm at all.
	Components map[string]map[string]any `json:"components"`
	// ParseError is set when the stored document did not load strictly and these fields
	// came from a best-effort read of it. The settings page must say so and refuse to
	// save: a save from a misread form would post whatever the fallback managed to see,
	// and with the YAML box gone there is no other way to correct the document from the
	// page. Fixing it needs the account editor, which still takes a document.
	ParseError string `json:"parse_error,omitempty"`
}

// CacheForm is the settings page's view of the `cache:` block.
type CacheForm struct {
	KeepAlive                bool    `json:"keepalive"`
	KeepAliveIdleSeconds     int     `json:"keepalive_idle_seconds,omitempty"`
	KeepAliveMaxPings        int     `json:"keepalive_max_pings,omitempty"`
	KeepAliveMaxUSDPerPing   float64 `json:"keepalive_max_usd_per_ping,omitempty"`
	KeepAliveMinPrefixTokens int     `json:"keepalive_min_prefix_tokens,omitempty"`
	HeadTTL1h                bool    `json:"head_ttl_1h"`
	HeadTTLMinTokens         int     `json:"head_ttl_min_tokens,omitempty"`
}

// ExtractLLMForm is the RECOMMENDED prefill for the compaction-model component — a policy
// layer, deliberately not the same thing as the component's own defaults.
//
// components.Field.Default answers "what does an absent key mean?" (min_tokens 300, cold
// cache off). This answers "what should the settings page offer somebody switching the
// component on for the first time?", which is a different question with a different source:
// our own measurements. Conflating the two is how a form ends up writing its opinion over
// an operator's deliberate value.
type ExtractLLMForm struct {
	// PerOutput is the hot-path pass, ColdEnabled the cold-cache sweep. Both false means
	// the component is removed from the pipeline entirely — there would be nothing left
	// for it to do, and its constructor refuses that combination outright.
	PerOutput   bool `json:"per_output"`
	ColdEnabled bool `json:"cold_enabled"`
	// SizeTrigger picks fire_on: size over the default pressure trigger. Named for what
	// the operator is choosing, not for the YAML value, because it also demotes the
	// economic gate to advisory and the UI has to say so.
	SizeTrigger     bool   `json:"size_trigger"`
	MinTokens       int    `json:"min_tokens"`
	MaxPerRequest   int    `json:"max_per_request"`
	MaxPerSession   int    `json:"max_per_session"`
	Aggressiveness  string `json:"aggressiveness"`
	Context         string `json:"context"`
	ContextMessages int    `json:"context_messages"`
	ColdMinTokens   int    `json:"cold_min_tokens"`

	// The three below decide whether the component can act AT ALL. Every one of them was,
	// on a real account, silently the reason a fully configured extract_llm did nothing:
	//
	//	AllowOnCachingBackend — unset means FALSE, and the economic gate then hard-declines
	//	  every candidate whose tokens are prompt-cached. On Claude Code against Anthropic
	//	  that is the whole workload, so the component ran 251 times and acted 0 times.
	//	ModelSource — "config" selects an operator-configured compaction model, and the
	//	  hosted service HAS none (it would be spending the operator's credential on a
	//	  tenant's traffic). So "config" there means "no model", i.e. never call anything.
	//	  "incoming" uses the caller's own model and key, which is what a tenant wants.
	//	Strategy / EveryNRequests / TriggerMinTokens — already in real stored documents.
	AllowOnCachingBackend bool   `json:"allow_on_caching_backend"`
	ModelSource           string `json:"model_source"`
	// ModelName is the model to COMPACT with, on the source's endpoint and credential.
	// Empty means "whatever the source's own model is", which for `incoming` is the agent's
	// frontier model — and compaction on one does not pay: a measured cold-cache sweep cut
	// the provider bill by $0.63 and spent $1.25 of opus doing it. This field is how the
	// same work runs on a cheap model without the operator paying for it.
	ModelName        string `json:"model_name"`
	Strategy         string `json:"strategy"`
	EveryNRequests   int    `json:"every_n_requests"`
	TriggerMinTokens int    `json:"trigger_min_tokens"`
}

// DefaultExtractLLMForm is what the settings page pre-fills. The sweep on, the hot path
// off: the cold turn is the regime where our own measurements say a model call pays, and
// the per-output pass on a warm cache is the one they say loses.
//
// These are housellm's values, and as of the cold-floor change they no longer deviate from
// it anywhere — including AllowOnCachingBackend, which this function has always returned
// false and which the preset now also omits. housellm is the one extract_llm configuration
// this service has actually run and measured, so the form offers what production runs.
//
// PerOutput is TRUE here, matching the preset, and it now does something: savedTokenValueAt
// prices a candidate by position, so a tail candidate is valued at the cache-WRITE rate rather
// than the request-level cache-READ rate, and the hot path can pay on the uncached tail.
//
// MinTokens 8000 is what makes that safe, and it is derived from measurement rather than
// picked: a call costs ~$0.0193 per ACCEPTED result (output-dominated, and including the
// 1-in-5 that is rejected outright), which needs 4,060 saved tokens at the cache-write rate,
// which at the observed ~65% reduction needs a ~6,250-token candidate. At MinTokens 1000 the
// same real sessions lost $0.036 — every call allowed by the exploration budget rather than by
// the arithmetic. Lower this and the form starts recommending a measured loss.
//
// AllowOnCachingBackend stays false and is vestigial either way: the check it lifts no longer
// fires on a warm turn (the tail is not cached; depth is refused by the tail gate first).
//
// ColdMinTokens 1000 is the value that decides whether this component does anything at all
// on this service: every extraction call production has ever made was a cold one, and at
// 3000 the sweep refused every candidate it saw (below_output_floor on all 36 sweeping
// turns, 0 extractions across 3,437 requests). cold_cache.max_calls is still not modelled
// by this form, so an account prefilled from here gets the component's own default of 4
// rather than the preset's 20.
//
// MaxPerSession 0 is read by the component as UNLIMITED — an operator decision, and now a
// live one, since PerOutput true means the hot arm reads it. What bounds a long session in
// practice is eligibility rather than this cap: 132 calls across three days of heavy use,
// under a per-session cap of 40 that was never approached.
func DefaultExtractLLMForm() ExtractLLMForm {
	return ExtractLLMForm{
		PerOutput: true, ColdEnabled: true, SizeTrigger: false,
		MinTokens: 8000, MaxPerRequest: 8, MaxPerSession: 0,
		Aggressiveness: "medium", Context: "recent", ContextMessages: 2,
		ColdMinTokens: 1000,
		// incoming, not config: on the hosted service there is no operator-configured
		// compaction model, so `source: config` is a component that can never make a call.
		ModelSource: "incoming", ModelName: "claude-haiku-4-5",
		Strategy: "code", EveryNRequests: 1,
		TriggerMinTokens:      3000,
		AllowOnCachingBackend: false,
	}
}

// RecommendedComponents renders the recommended prefill as form values, keyed the way the
// form is: dotted paths inside a component's block. Served at /api/options so the page
// offers one recommendation from one place rather than a second copy of it in JavaScript.
func RecommendedComponents() map[string]map[string]any {
	d := DefaultExtractLLMForm()
	fireOn := "pressure"
	if d.SizeTrigger {
		fireOn = "size"
	}
	return map[string]map[string]any{"extract_llm": {
		"per_output":                 d.PerOutput,
		"fire_on":                    fireOn,
		"min_tokens":                 d.MinTokens,
		"llm_max_per_request":        d.MaxPerRequest,
		"llm_max_per_session":        d.MaxPerSession,
		"llm_every_n_requests":       d.EveryNRequests,
		"aggressiveness":             d.Aggressiveness,
		"context":                    d.Context,
		"context_messages":           d.ContextMessages,
		"strategy":                   d.Strategy,
		"allow_on_caching_backend":   d.AllowOnCachingBackend,
		"cold_cache.enabled":         d.ColdEnabled,
		"cold_cache.min_tokens":      d.ColdMinTokens,
		"model.source":               d.ModelSource,
		"model.model":                d.ModelName,
		"trigger.min_request_tokens": d.TriggerMinTokens,
	}}
}

// ParseForm reads a configuration document into form fields.
//
// Best-effort by design: it is what the settings page renders from, and a document it
// cannot fully understand must still draw a usable form. So it tries the strict loader
// first (which resolves presets, the whole reason to bother) and falls back to a loose
// decode of the same shape, recording why (R1).
func ParseForm(doc string) (Form, error) {
	var f Form
	c, strictErr := LoadBytes([]byte(doc))
	if strictErr == nil {
		f.Pipeline, f.Mode = c.Pipeline, c.Mode
		f.Cache = cacheForm(c.Cache)
		// LoadBytes is strict about the top level but hands each component's block to its
		// constructor as an opaque node, and THAT is where the strict check now lives. So a
		// document with `min_tokns: 5000` under a component loads and does not build: the
		// fields below are read correctly, but the page must still say the stored document
		// is broken and refuse to save over it, exactly as for a top-level typo (R1).
		if _, err := c.Build(components.NopEmitter{}); err != nil {
			f.ParseError = err.Error()
		}
		blocks := make(map[string]any, len(c.Components))
		for name, node := range c.Components {
			var m map[string]any
			if err := node.Decode(&m); err == nil {
				blocks[name] = m
			}
		}
		f.Components = readBlocks(blocks)
		return f, nil
	}
	// Loose: non-strict, so an unknown top-level key or a mistyped component key decodes
	// into nothing instead of failing. Presets are NOT resolved here — there is no
	// resolver that can run on a document that does not load — so the pipeline is
	// whatever the text literally says.
	var d struct {
		Pipeline   []string       `yaml:"pipeline"`
		Mode       string         `yaml:"mode"`
		Components map[string]any `yaml:"components"`
	}
	if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
		return f, err
	}
	f.Pipeline, f.Mode = d.Pipeline, d.Mode
	f.Components = readBlocks(d.Components)
	// Recorded on the form, not only returned: the caller publishes this to the settings
	// page, and the page has to be able to say "these fields are a guess at a document
	// that does not load" rather than draw them as fact and accept a save over them.
	f.ParseError = strictErr.Error()
	return f, nil
}

// cacheForm renders the loaded `cache:` block for the settings page. Always non-nil on a
// document that loaded: the page draws an unchecked box for an absent block, and a save from
// it then states `keepalive: false` explicitly, which is what the reader of the document
// should see.
func cacheForm(c CacheConfig) *CacheForm {
	return &CacheForm{
		KeepAlive: c.KeepAlive, KeepAliveIdleSeconds: c.KeepAliveIdleSeconds,
		KeepAliveMaxPings:        c.KeepAliveMaxPings,
		KeepAliveMaxUSDPerPing:   c.KeepAliveMaxUSDPerPing,
		KeepAliveMinPrefixTokens: c.KeepAliveMinPrefixTokens,
		HeadTTL1h:                c.HeadTTL1h, HeadTTLMinTokens: c.HeadTTLMinTokens,
	}
}

// readBlocks pulls every DECLARED key each component's block actually states. Keys the
// document does not state are absent from the result, not defaulted (R3), and a secret is
// never echoed back (R5 of the settings docs).
func readBlocks(blocks map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for name, decls := range components.AllFields() {
		blk, ok := blocks[name].(map[string]any)
		if !ok {
			continue
		}
		vals := map[string]any{}
		for _, fd := range decls {
			if fd.Secret {
				continue
			}
			v, ok := getPath(blk, fd.Key)
			if !ok {
				continue
			}
			if v, ok := coerce(fd, v); ok {
				vals[fd.Key] = v
			}
		}
		if len(vals) > 0 {
			out[name] = vals
		}
	}
	return out
}

// ApplyForm writes the form's fields into doc and returns the new document, validated.
//
// Unknown keys survive: doc is decoded into a generic map, the declared keys are set on it,
// and the whole thing is re-marshalled. So a manager's `store:` block or a component the
// form does not know is still there afterwards, which the old string surgery only achieved
// for the one block it special-cased.
func ApplyForm(doc string, f Form) (string, error) {
	f.normalize()
	if err := f.validate(); err != nil {
		return "", err
	}
	m := map[string]any{}
	if doc != "" {
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			return "", fmt.Errorf("config: the current document does not parse, so it cannot be edited as fields: %w", err)
		}
		if m == nil {
			m = map[string]any{}
		}
	}
	if f.Mode != "" {
		m["mode"] = f.Mode
	}
	// The cache block. Only the keys the form states, and only when it states any: writing
	// zeroes for the tuning fields would freeze today's defaults into every document that
	// ever visited the settings page, which is the same mistake prefilling component
	// defaults was (R3 in the settings docs).
	if f.Cache != nil {
		cb := child(m, "cache")
		cb["keepalive"] = f.Cache.KeepAlive
		cb["head_ttl_1h"] = f.Cache.HeadTTL1h
		for k, v := range map[string]int{
			"keepalive_idle_seconds":      f.Cache.KeepAliveIdleSeconds,
			"keepalive_max_pings":         f.Cache.KeepAliveMaxPings,
			"keepalive_min_prefix_tokens": f.Cache.KeepAliveMinPrefixTokens,
			"head_ttl_min_tokens":         f.Cache.HeadTTLMinTokens,
		} {
			if v > 0 {
				cb[k] = v
			} else {
				delete(cb, k)
			}
		}
		if f.Cache.KeepAliveMaxUSDPerPing > 0 {
			cb["keepalive_max_usd_per_ping"] = f.Cache.KeepAliveMaxUSDPerPing
		} else {
			delete(cb, "keepalive_max_usd_per_ping")
		}
	}

	// What the STORED document runs, so a component leaving the pipeline can be told apart
	// from one that was never in it. Without that distinction a round trip on a `preset:
	// codesafe` document deleted the preset's tuned block for every component the preset
	// does not run.
	var was []string
	if cur, err := LoadBytes([]byte(doc)); err == nil {
		was = cur.Pipeline
	}

	pipeline := append([]string(nil), f.Pipeline...)
	pipeline = applyExtractLLMCoupling(pipeline, f.Components["extract_llm"])
	// Before the component loop, not after: a preserved component is ON, and the loop below
	// CLEARS the declared keys of anything it reads as switched off. Resolving the pipeline
	// once, here, is what stops a save preserving a component in the run order while deleting
	// the block that configures it.
	pipeline = preserveUnmodelled(was, pipeline, f.Pipeline, f.PipelineKnown)

	comps := child(m, "components")
	for name, decls := range components.AllFields() {
		vals, sent := f.Components[name]
		if !sent {
			continue // not on this form: leave whatever the document says alone
		}
		on := contains(pipeline, name)
		if !on && !contains(was, name) {
			continue // off before and off now: nothing to write and nothing to clear
		}
		blk := child(comps, name)
		for _, fd := range decls {
			v, ok := vals[fd.Key]
			switch {
			case on && ok:
				setPath(blk, fd.Key, v)
			case on && fd.Secret:
				// A secret is never echoed into the form, so "absent" cannot mean
				// "cleared" — clearing takes an explicit empty string. Otherwise every
				// save would delete the stored credential.
			default:
				// Switched off, or a key the form left blank: the declared key goes and
				// the component's own default takes over. Only DECLARED leaf paths are
				// deleted, never a parent block, so a key this form does not own cannot
				// be taken out with one (R4/R7).
				delPath(blk, fd.Key)
			}
		}
		if len(blk) == 0 {
			delete(comps, name)
		} else {
			comps[name] = blk
		}
	}
	if len(comps) == 0 {
		delete(m, "components")
	} else {
		m["components"] = comps
	}
	if f.Pipeline != nil || len(f.Components) > 0 {
		m["pipeline"] = pipeline
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(m); err != nil {
		return "", fmt.Errorf("config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("config: %w", err)
	}
	out := buf.String()
	// The same strict check the proxy builds with, here rather than at the caller, so no
	// route can store a document this produced without it having been built once.
	if err := Validate([]byte(out)); err != nil {
		return "", err
	}
	return out, nil
}

// applyExtractLLMCoupling is the ONE per-component exception to "enabled == in the
// pipeline", and it is here because the component's own constructor demands it: per_output
// false with the cold sweep off is refused outright ("nothing to do"), so a form that
// wrote that combination would produce a document the proxy cannot build.
//
// Note what it does NOT do: derive fire_on from per_output. That was tried, and it meant
// ticking a checkbox quietly turned the spending brakes advisory — fire_on is its own
// declared enum, and it follows only the explicit choice.
func applyExtractLLMCoupling(pipeline []string, vals map[string]any) []string {
	const name = "extract_llm"
	if vals == nil {
		return pipeline
	}
	hot, hotSet := vals["per_output"].(bool)
	cold, _ := vals["cold_cache.enabled"].(bool)
	if hotSet && !hot && !cold {
		return remove(pipeline, name)
	}
	if !contains(pipeline, name) && ((hotSet && hot) || cold) {
		// Before the deterministic `extract` where there is one: the cheap pass should see
		// whatever the model pass leaves, which is the order every shipped preset uses.
		return insertBefore(pipeline, name, "extract")
	}
	return pipeline
}

// preserveUnmodelled re-inserts every component the STORED pipeline ran that the posted one
// does not mention AND the client did not declare a control for.
//
// This is the rule that makes a save unable to drop what the client cannot see. The posted
// pipeline is a wholesale replacement built from a checkbox grid, so a name missing from it is
// ambiguous: the operator may have unticked it, or the page may never have drawn a box for it
// (a cached bundle whose /api/options predates the component) or may have drawn it from a
// document that has since changed. `known` disambiguates — it is what the client says it drew —
// and absence from `known` resolves the ambiguity in favour of the stored document.
//
// Position is preserved, not appended: a pipeline is an ORDER, and `linecap` reappearing at the
// end is a different configuration from `linecap` where the operator put it. Each survivor goes
// back at the index it held in `was`, which for the common case of one unmodelled component in
// the middle restores the document exactly.
//
// A DECLARED removal still removes: a name in `known` and absent from `sent` is gone.
//
// `sent` is what the CLIENT posted and `resolved` is that after applyExtractLLMCoupling, and
// the two are deliberately different arguments. The membership test is on `sent`, because this
// rule is about what the client said; the insertion is into `resolved`, because that is the
// pipeline being written. Testing membership on the resolved list instead would undo the
// coupling's own removal of extract_llm — a SERVER decision, not a client omission — and put
// a component back that its own constructor refuses to build.
func preserveUnmodelled(was, resolved, sent, known []string) []string {
	out := append([]string(nil), resolved...)
	for i, name := range was {
		if contains(out, name) || contains(sent, name) || contains(known, name) {
			continue
		}
		// Clamped, because `was` may be longer than what is being written.
		at := i
		if at > len(out) {
			at = len(out)
		}
		out = append(out[:at:at], append([]string{name}, out[at:]...)...)
	}
	return out
}

// normalize fills enum fields a caller left EMPTY with the component's own default, and
// drops nulls.
//
// A browser running a cached copy of the page does not know about a field added since, and
// answering its save with `model.source "" is not incoming or config` would be a settings
// page that breaks on deploy for exactly as long as the old bundle lives in a cache. Empty
// means "the component's default", which is what an absent key already means.
func (f *Form) normalize() {
	for name, decls := range components.AllFields() {
		vals := f.Components[name]
		if vals == nil {
			continue
		}
		for _, fd := range decls {
			v, ok := vals[fd.Key]
			if !ok {
				continue
			}
			if v == nil {
				delete(vals, fd.Key)
				continue
			}
			if s, isStr := v.(string); isStr && s == "" && fd.Type == components.FieldEnum {
				if def, isStr := fd.Default.(string); isStr {
					vals[fd.Key] = def
				} else {
					delete(vals, fd.Key)
				}
			}
		}
	}
}

// validate checks every posted value against its declaration. Every failure is a 400 that
// NAMES the field: extract_llm's own loader used to be non-strict, so a value it did not
// understand did nothing at all, silently, on the one component that spends money.
func (f Form) validate() error {
	if f.Mode != "" && f.Mode != "sync" && f.Mode != "observe" {
		return fmt.Errorf("config: mode %q is not sync or observe", f.Mode)
	}
	if f.Cache != nil {
		if err := (CacheConfig{
			KeepAlive: f.Cache.KeepAlive, KeepAliveIdleSeconds: f.Cache.KeepAliveIdleSeconds,
			KeepAliveMaxPings:        f.Cache.KeepAliveMaxPings,
			KeepAliveMaxUSDPerPing:   f.Cache.KeepAliveMaxUSDPerPing,
			KeepAliveMinPrefixTokens: f.Cache.KeepAliveMinPrefixTokens,
			HeadTTL1h:                f.Cache.HeadTTL1h, HeadTTLMinTokens: f.Cache.HeadTTLMinTokens,
		}).validate(); err != nil {
			return err
		}
	}
	all := components.AllFields()
	for _, name := range sortedKeys(f.Components) {
		decls, ok := all[name]
		if !ok {
			return fmt.Errorf("config: %q is not a registered component", name)
		}
		vals := f.Components[name]
		byKey := make(map[string]components.Field, len(decls))
		for _, fd := range decls {
			byKey[fd.Key] = fd
		}
		for _, key := range sortedKeys(vals) {
			fd, ok := byKey[key]
			if !ok {
				return fmt.Errorf("config: %s.%s is not a configurable key", name, key)
			}
			if err := validateValue(name, fd, vals[key]); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateValue(comp string, fd components.Field, v any) error {
	where := comp + "." + fd.Key
	switch fd.Type {
	case components.FieldBool:
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("config: %s must be true or false, got %v", where, v)
		}
	case components.FieldInt:
		n, ok := asInt(v)
		if !ok {
			return fmt.Errorf("config: %s must be a whole number, got %v", where, v)
		}
		// Min carries the semantics two hand-written maps used to: 0 on a CAP means
		// unlimited and is legitimate, while 0 on a size threshold is not a setting, it is
		// a removed brake — every candidate clears it.
		if n < fd.Min {
			if fd.Min == 0 {
				return fmt.Errorf("config: %s cannot be negative (0 means unlimited)", where)
			}
			return fmt.Errorf("config: %s must be at least %d", where, fd.Min)
		}
	case components.FieldFloat:
		x, ok := asFloat(v)
		if !ok {
			return fmt.Errorf("config: %s must be a number, got %v", where, v)
		}
		if x < float64(fd.Min) {
			return fmt.Errorf("config: %s cannot be below %d", where, fd.Min)
		}
	case components.FieldEnum:
		s, ok := v.(string)
		if !ok || !contains(fd.Options, s) {
			return fmt.Errorf("config: %s %q is not one of %s", where, v, strings.Join(fd.Options, ", "))
		}
	case components.FieldString:
		if _, ok := v.(string); !ok {
			return fmt.Errorf("config: %s must be text, got %v", where, v)
		}
	case components.FieldStrings:
		if _, ok := asStrings(v); !ok {
			return fmt.Errorf("config: %s must be a list of text values, got %v", where, v)
		}
	default:
		return fmt.Errorf("config: %s has unknown field type %q", where, fd.Type)
	}
	return nil
}

// coerce turns a value read out of a YAML document (or posted as JSON, where every number
// arrives as a float64) into the type the field declares. A value of the wrong shape is
// dropped rather than shown: the form would otherwise render a string where a number
// belongs and post it straight back.
func coerce(fd components.Field, v any) (any, bool) {
	switch fd.Type {
	case components.FieldBool:
		b, ok := v.(bool)
		return b, ok
	case components.FieldInt:
		return asInt(v)
	case components.FieldFloat:
		return asFloat(v)
	case components.FieldEnum, components.FieldString:
		s, ok := v.(string)
		return s, ok
	case components.FieldStrings:
		return asStrings(v)
	}
	return nil, false
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n == math.Trunc(n) {
			return int(n), true
		}
	case float32:
		return asInt(float64(n))
	}
	return 0, false
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

func asStrings(v any) ([]string, bool) {
	switch xs := v.(type) {
	case []string:
		return xs, true
	case []any:
		out := make([]string, 0, len(xs))
		for _, x := range xs {
			s, ok := x.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// getPath reads a dotted key path out of a decoded block.
func getPath(m map[string]any, path string) (any, bool) {
	keys := strings.Split(path, ".")
	for _, k := range keys[:len(keys)-1] {
		sub, ok := m[k].(map[string]any)
		if !ok {
			return nil, false
		}
		m = sub
	}
	v, ok := m[keys[len(keys)-1]]
	return v, ok
}

// setPath sets a dotted key path, creating the blocks on the way. It MERGES into an
// existing block rather than replacing it, so the keys this form does not own inside
// `model:` or `trigger:` survive a save.
func setPath(m map[string]any, path string, v any) {
	keys := strings.Split(path, ".")
	for _, k := range keys[:len(keys)-1] {
		sub := child(m, k)
		m[k] = sub
		m = sub
	}
	m[keys[len(keys)-1]] = v
}

// delPath deletes a dotted key path and prunes any block it emptied — a bare `cold_cache:
// {}` left behind is not what the operator wrote, and yaml renders it.
func delPath(m map[string]any, path string) {
	keys := strings.Split(path, ".")
	if len(keys) == 1 {
		delete(m, keys[0])
		return
	}
	sub, ok := m[keys[0]].(map[string]any)
	if !ok {
		return
	}
	delPath(sub, strings.Join(keys[1:], "."))
	if len(sub) == 0 {
		delete(m, keys[0])
	}
}

// child returns m[key] as a map, creating it when absent — INSERTED INTO m — and replacing
// it when it is something else (a `components:` that decoded as nil because the key was
// written with no body is the common case).
//
// The insertion is the whole contract and it used to be missing: the function returned a
// fresh detached map and left m untouched, so a caller that did not assign the result back
// wrote its whole block into a throwaway. `components:` and setPath both happened to assign
// it back and were fine; the `cache:` block did not, and a tenant switching the keep-alive on
// for the first time — the one case where the key is absent — had their consent silently
// discarded. Fixed here rather than at that one call site: the next block someone adds to
// ApplyForm cannot repeat it. The three callers that do assign are unaffected, and the two
// that prune an empty block (`components:` and each component's own) still prune it, so an
// empty map created here does not reach the document.
func child(m map[string]any, key string) map[string]any {
	if sub, ok := m[key].(map[string]any); ok && sub != nil {
		return sub
	}
	sub := map[string]any{}
	m[key] = sub
	return sub
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func insertBefore(xs []string, v, before string) []string {
	for i, x := range xs {
		if x == before {
			return append(xs[:i:i], append([]string{v}, xs[i:]...)...)
		}
	}
	return append(xs, v)
}

func remove(xs []string, v string) []string {
	out := xs[:0:0]
	for _, x := range xs {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}
