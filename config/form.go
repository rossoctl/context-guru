package config

import (
	"bytes"
	"fmt"

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
type Form struct {
	// Pipeline is the component run order, resolved: for a `preset:` document it is what
	// the preset expands to, not the empty list the text literally contains.
	Pipeline []string `json:"pipeline"`
	Mode     string   `json:"mode"`
	// ExtractLLM is nil when the form does not manage that component on this request.
	ExtractLLM *ExtractLLMForm `json:"extract_llm"`
	// ParseError is set when the stored document did not load strictly and these fields
	// came from a best-effort read of it. The settings page must say so and refuse to
	// save: a save from a misread form would post whatever the fallback managed to see,
	// and with the YAML box gone there is no other way to correct the document from the
	// page. Fixing it needs the account editor, which still takes a document.
	ParseError string `json:"parse_error,omitempty"`
}

// ExtractLLMForm is the compaction-model component's knobs, the ones the settings page
// owns. It is deliberately not all of them: strategy, model routing, marker_mode and the
// gate overrides stay where a manager sets them, and a round trip through here leaves them
// untouched.
type ExtractLLMForm struct {
	// PerOutput is the hot-path pass, ColdEnabled the cold-cache sweep. Both false means
	// the component is removed from the pipeline entirely — there would be nothing left
	// for it to do, and leaving it in would cost a pass over every request for nothing.
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

	// The three below decide whether the component can act AT ALL, which is why they are
	// on the form and not left to a manager. Every one of them was, on a real account,
	// silently the reason a fully configured extract_llm did nothing:
	//
	//	AllowOnCachingBackend — unset means FALSE, and the economic gate then hard-declines
	//	  every candidate whose tokens are prompt-cached. On Claude Code against Anthropic
	//	  that is the whole workload, so the component ran 251 times and acted 0 times.
	//	ModelSource — "config" selects an operator-configured compaction model, and the
	//	  hosted service HAS none (it would be spending the operator's credential on a
	//	  tenant's traffic). So "config" there means "no model", i.e. never call anything.
	//	  "incoming" uses the caller's own model and key, which is what a tenant wants.
	//	Strategy / EveryNRequests / TriggerMinRequestTokens — already in real stored
	//	  documents, so a form that could not show them was editing a config it did not
	//	  describe.
	AllowOnCachingBackend bool   `json:"allow_on_caching_backend"`
	ModelSource           string `json:"model_source"`
	Strategy              string `json:"strategy"`
	EveryNRequests        int    `json:"every_n_requests"`
	TriggerMinTokens      int    `json:"trigger_min_tokens"`
}

// DefaultExtractLLMForm is what the settings page pre-fills. The sweep on, the hot path
// off: the cold turn is the regime where our own measurements say a model call pays, and
// the per-output pass on a warm cache is the one they say loses.
func DefaultExtractLLMForm() ExtractLLMForm {
	return ExtractLLMForm{
		PerOutput: false, ColdEnabled: true, SizeTrigger: false,
		MinTokens: 2000, MaxPerRequest: 2, MaxPerSession: 20,
		Aggressiveness: "medium", Context: "recent", ContextMessages: 7,
		ColdMinTokens: 1000,
		// incoming, not config: on the hosted service there is no operator-configured
		// compaction model, so `source: config` is a component that can never make a call.
		ModelSource: "incoming", Strategy: "code", EveryNRequests: 1,
		TriggerMinTokens: 3000,
		// Left FALSE: the component's own measurements say it loses money on a caching
		// backend, and a default that starts spending is not this form's call. It is a
		// FIELD now, so the operator who wants it can see it and tick it, which is the part
		// that was missing — it was invisible, not just off.
		AllowOnCachingBackend: false,
	}
}

var (
	aggressivenessValues = []string{"low", "medium", "high"}
	contextValues        = []string{"goal", "recent", "full"}
	modelSourceValues    = []string{"incoming", "config"}
	strategyValues       = []string{"code", "single", "rlm", "auto"}
)

// ParseForm reads a configuration document into form fields.
//
// Best-effort by design: it is what the settings page renders from, and a document it
// cannot fully understand must still draw a usable form. So it tries the strict loader
// first (which resolves presets, the whole reason to bother) and falls back to a loose
// decode of just the keys the form owns.
func ParseForm(doc string) (Form, error) {
	var f Form
	f.ExtractLLM = new(ExtractLLMForm)
	*f.ExtractLLM = DefaultExtractLLMForm()

	loose := func(strictErr error) (Form, error) {
		var d formDoc
		if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
			return f, err
		}
		f.Pipeline, f.Mode = d.Pipeline, d.Mode
		d.Components.ExtractLLM.into(f.ExtractLLM, contains(d.Pipeline, "extract_llm"))
		// Recorded on the form, not only returned: the caller publishes this to the settings
		// page, and the page has to be able to say "these fields are a guess at a document
		// that does not load" rather than draw them as fact and accept a save over them.
		f.ParseError = strictErr.Error()
		return f, nil
	}

	c, err := LoadBytes([]byte(doc))
	if err != nil {
		return loose(err)
	}
	f.Pipeline, f.Mode = c.Pipeline, c.Mode
	inPipeline := contains(c.Pipeline, "extract_llm")
	blk, ok := c.Components["extract_llm"]
	if !ok {
		// No block of its own. That does NOT mean the component is off: a bare `preset:`
		// resolves to a pipeline containing extract_llm and no per-component blocks at all,
		// and per_output defaults to true there. Forcing both switches off here showed such
		// an account an empty form and then, on save, wrote a pipeline with the component
		// REMOVED — the same silent loss this type exists to prevent.
		extractLLMDoc{}.into(f.ExtractLLM, inPipeline)
		return f, nil
	}
	var x extractLLMDoc
	if err := blk.Decode(&x); err != nil {
		return f, nil
	}
	x.into(f.ExtractLLM, inPipeline)
	return f, nil
}

// formDoc is the loose fallback shape: non-strict, so every other component's block and
// every key this form does not own decode into nothing and are ignored.
type formDoc struct {
	Pipeline   []string `yaml:"pipeline"`
	Mode       string   `yaml:"mode"`
	Components struct {
		ExtractLLM extractLLMDoc `yaml:"extract_llm"`
	} `yaml:"components"`
}

// Pointers on the ints, so ABSENT and 0 are different answers. With plain ints a stored
// `llm_max_per_session: 0` — which the component reads as UNLIMITED — was indistinguishable
// from an unset key, so the form displayed 20, and the next save wrote 20 over a deliberate
// "no cap". A settings page must not change a value it was only asked to display.
type extractLLMDoc struct {
	PerOutput       *bool  `yaml:"per_output"`
	FireOn          string `yaml:"fire_on"`
	MinTokens       *int   `yaml:"min_tokens"`
	MaxPerRequest   *int   `yaml:"llm_max_per_request"`
	MaxPerSession   *int   `yaml:"llm_max_per_session"`
	Aggressiveness  string `yaml:"aggressiveness"`
	Context         string `yaml:"context"`
	ContextMessages *int   `yaml:"context_messages"`
	ColdCache       struct {
		Enabled   bool `yaml:"enabled"`
		MinTokens *int `yaml:"min_tokens"`
	} `yaml:"cold_cache"`
	AllowOnCaching *bool  `yaml:"allow_on_caching_backend"`
	Strategy       string `yaml:"strategy"`
	EveryN         *int   `yaml:"llm_every_n_requests"`
	Model          struct {
		Source string `yaml:"source"`
	} `yaml:"model"`
	Trigger struct {
		MinRequestTokens *int `yaml:"min_request_tokens"`
	} `yaml:"trigger"`
}

// into overlays what the document said onto the defaults. inPipeline gates both switches:
// a block configuring the component says nothing about whether it runs.
func (x extractLLMDoc) into(f *ExtractLLMForm, inPipeline bool) {
	f.PerOutput = inPipeline && (x.PerOutput == nil || *x.PerOutput)
	f.ColdEnabled = inPipeline && x.ColdCache.Enabled
	f.SizeTrigger = x.FireOn == "size"
	setIf(&f.MinTokens, x.MinTokens)
	setIf(&f.MaxPerRequest, x.MaxPerRequest)
	setIf(&f.MaxPerSession, x.MaxPerSession)
	setIf(&f.ContextMessages, x.ContextMessages)
	setIf(&f.ColdMinTokens, x.ColdCache.MinTokens)
	if contains(aggressivenessValues, x.Aggressiveness) {
		f.Aggressiveness = x.Aggressiveness
	}
	if contains(contextValues, x.Context) {
		f.Context = x.Context
	}
	setIf(&f.EveryNRequests, x.EveryN)
	setIf(&f.TriggerMinTokens, x.Trigger.MinRequestTokens)
	f.AllowOnCachingBackend = x.AllowOnCaching != nil && *x.AllowOnCaching
	if contains(strategyValues, x.Strategy) {
		f.Strategy = x.Strategy
	}
	// Absent means "incoming" in the component, so an absent key must not display as
	// "config" — and an absent key must not be REWRITTEN to config either.
	if contains(modelSourceValues, x.Model.Source) {
		f.ModelSource = x.Model.Source
	}
}

// setIf overlays a value the document actually stated, 0 included.
func setIf(dst *int, v *int) {
	if v != nil {
		*dst = *v
	}
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// ApplyForm writes the form's fields into doc and returns the new document, validated.
//
// Unknown keys survive: doc is decoded into a generic map, the managed keys are set on it,
// and the whole thing is re-marshalled. So a manager's `model:` block or a second
// component's settings are still there afterwards, which the old string surgery only
// achieved for the one block it special-cased.
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

	comps := child(m, "components")
	pipeline := append([]string(nil), f.Pipeline...)
	if x := f.ExtractLLM; x != nil {
		if x.PerOutput || x.ColdEnabled {
			blk := child(comps, "extract_llm")
			blk["per_output"] = x.PerOutput
			// fire_on follows the explicit choice and never per_output: deriving it once
			// meant ticking a checkbox quietly turned the spending brakes advisory.
			fireOn := "pressure"
			if x.SizeTrigger {
				fireOn = "size"
			}
			blk["fire_on"] = fireOn
			blk["min_tokens"] = x.MinTokens
			blk["llm_max_per_request"] = x.MaxPerRequest
			blk["llm_max_per_session"] = x.MaxPerSession
			blk["aggressiveness"] = x.Aggressiveness
			blk["context"] = x.Context
			blk["context_messages"] = x.ContextMessages
			blk["allow_on_caching_backend"] = x.AllowOnCachingBackend
			blk["strategy"] = x.Strategy
			blk["llm_every_n_requests"] = x.EveryNRequests
			// model and trigger carry keys this form does not own (base_url, api_key, the
			// other trigger thresholds), so ONE key is set inside each existing block
			// rather than the block being replaced.
			mdl := child(blk, "model")
			mdl["source"] = x.ModelSource
			blk["model"] = mdl
			trg := child(blk, "trigger")
			trg["min_request_tokens"] = x.TriggerMinTokens
			blk["trigger"] = trg
			cold := child(blk, "cold_cache")
			cold["enabled"] = x.ColdEnabled
			cold["min_tokens"] = x.ColdMinTokens
			blk["cold_cache"] = cold
			comps["extract_llm"] = blk
			if !contains(pipeline, "extract_llm") {
				// Before the deterministic `extract` where there is one: the cheap pass
				// should see whatever the model pass leaves, which is the order every
				// shipped preset uses.
				pipeline = insertBefore(pipeline, "extract_llm", "extract")
			}
		} else {
			// Off means "do not run it", not "forget how it was configured". Deleting the
			// whole block took `model:` with it, so re-enabling the component later ran it
			// on the expensive default model — a form that quietly costs money. Only the
			// keys this form owns go; the rest is somebody's deliberate configuration, and
			// leaving the component out of the pipeline is what actually stops it.
			if blk, ok := comps["extract_llm"].(map[string]any); ok {
				for _, k := range managedKeys {
					delete(blk, k)
				}
				if len(blk) == 0 {
					delete(comps, "extract_llm")
				} else {
					comps["extract_llm"] = blk
				}
			}
			pipeline = remove(pipeline, "extract_llm")
		}
	}
	if len(comps) == 0 {
		delete(m, "components")
	} else {
		m["components"] = comps
	}
	if f.Pipeline != nil || f.ExtractLLM != nil {
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

// normalize fills the enum fields a caller left empty with the component's own defaults.
// A browser running a cached copy of the page does not know about a field added since, and
// answering its save with `model_source "" is not incoming or config` would be a settings
// page that breaks on deploy for exactly as long as the old bundle lives in a cache. Empty
// means "the component's default", which is what an absent key already means.
func (f *Form) normalize() {
	x := f.ExtractLLM
	if x == nil {
		return
	}
	d := DefaultExtractLLMForm()
	for _, c := range []struct {
		dst *string
		def string
	}{{&x.ModelSource, d.ModelSource}, {&x.Strategy, d.Strategy},
		{&x.Aggressiveness, d.Aggressiveness}, {&x.Context, d.Context}} {
		if *c.dst == "" {
			*c.dst = c.def
		}
	}
	if x.EveryNRequests <= 0 {
		x.EveryNRequests = d.EveryNRequests
	}
}

func (f Form) validate() error {
	if f.Mode != "" && f.Mode != "sync" && f.Mode != "observe" {
		return fmt.Errorf("config: mode %q is not sync or observe", f.Mode)
	}
	x := f.ExtractLLM
	if x == nil {
		return nil
	}
	if !contains(aggressivenessValues, x.Aggressiveness) {
		return fmt.Errorf("config: aggressiveness %q is not low, medium or high", x.Aggressiveness)
	}
	if !contains(contextValues, x.Context) {
		return fmt.Errorf("config: context %q is not goal, recent or full", x.Context)
	}
	if !contains(modelSourceValues, x.ModelSource) {
		return fmt.Errorf("config: model_source %q is not incoming or config", x.ModelSource)
	}
	if !contains(strategyValues, x.Strategy) {
		return fmt.Errorf("config: strategy %q is not code, single, rlm or auto", x.Strategy)
	}
	// A zero SIZE threshold is not a configuration, it is a removed brake: every candidate
	// output clears it, and with fire_on: size that is the only content gate there is. The
	// caps are different — the component documents 0 as "unlimited", so 0 is a real choice
	// there and the form's hint says so.
	for name, v := range map[string]int{
		"min_tokens": x.MinTokens, "context_messages": x.ContextMessages,
		"cold_min_tokens": x.ColdMinTokens, "every_n_requests": x.EveryNRequests,
	} {
		if v < 1 {
			return fmt.Errorf("config: %s must be at least 1", name)
		}
	}
	for name, v := range map[string]int{
		"max_per_request": x.MaxPerRequest, "max_per_session": x.MaxPerSession,
		// 0 here is "no absolute request floor", the component's own unset meaning.
		"trigger_min_tokens": x.TriggerMinTokens,
	} {
		if v < 0 {
			return fmt.Errorf("config: %s cannot be negative (0 means unlimited)", name)
		}
	}
	return nil
}

// managedKeys are the keys inside an extract_llm block that this form writes, and therefore
// the only ones it may remove.
// `model` and `trigger` are deliberately absent: the form owns ONE key inside each and
// the rest of those blocks is somebody's deliberate configuration, so switching the
// component off leaves both alone rather than deleting a base_url with them.
var managedKeys = []string{"per_output", "fire_on", "min_tokens", "llm_max_per_request",
	"llm_max_per_session", "aggressiveness", "context", "context_messages", "cold_cache",
	"allow_on_caching_backend", "strategy", "llm_every_n_requests"}

// child returns m[key] as a map, creating it when absent and replacing it when it is
// something else (a `components:` that decoded as nil because the key was written with no
// body is the common case).
func child(m map[string]any, key string) map[string]any {
	if sub, ok := m[key].(map[string]any); ok && sub != nil {
		return sub
	}
	return map[string]any{}
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
