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
	}
}

var (
	aggressivenessValues = []string{"low", "medium", "high"}
	contextValues        = []string{"goal", "recent", "full"}
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

	loose := func() error {
		var d formDoc
		if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
			return err
		}
		f.Pipeline, f.Mode = d.Pipeline, d.Mode
		d.Components.ExtractLLM.into(f.ExtractLLM, contains(d.Pipeline, "extract_llm"))
		return nil
	}

	c, err := LoadBytes([]byte(doc))
	if err != nil {
		return f, loose()
	}
	f.Pipeline, f.Mode = c.Pipeline, c.Mode
	blk, ok := c.Components["extract_llm"]
	if !ok {
		// Not configured: the defaults stand, except that a component absent from the
		// pipeline is off however its block reads.
		f.ExtractLLM.PerOutput = false
		f.ExtractLLM.ColdEnabled = false
		return f, nil
	}
	var x extractLLMDoc
	if err := blk.Decode(&x); err != nil {
		return f, nil
	}
	x.into(f.ExtractLLM, contains(c.Pipeline, "extract_llm"))
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

type extractLLMDoc struct {
	PerOutput       *bool  `yaml:"per_output"`
	FireOn          string `yaml:"fire_on"`
	MinTokens       int    `yaml:"min_tokens"`
	MaxPerRequest   int    `yaml:"llm_max_per_request"`
	MaxPerSession   int    `yaml:"llm_max_per_session"`
	Aggressiveness  string `yaml:"aggressiveness"`
	Context         string `yaml:"context"`
	ContextMessages int    `yaml:"context_messages"`
	ColdCache       struct {
		Enabled   bool `yaml:"enabled"`
		MinTokens int  `yaml:"min_tokens"`
	} `yaml:"cold_cache"`
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
}

func setIf(dst *int, v int) {
	if v > 0 {
		*dst = v
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
			blk["fire_on"] = map[bool]string{true: "size", false: "pressure"}[x.SizeTrigger]
			blk["min_tokens"] = x.MinTokens
			blk["llm_max_per_request"] = x.MaxPerRequest
			blk["llm_max_per_session"] = x.MaxPerSession
			blk["aggressiveness"] = x.Aggressiveness
			blk["context"] = x.Context
			blk["context_messages"] = x.ContextMessages
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
			delete(comps, "extract_llm")
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
	for name, v := range map[string]int{
		"min_tokens": x.MinTokens, "max_per_request": x.MaxPerRequest,
		"max_per_session": x.MaxPerSession, "context_messages": x.ContextMessages,
		"cold_min_tokens": x.ColdMinTokens,
	} {
		if v < 0 {
			return fmt.Errorf("config: %s cannot be negative", name)
		}
	}
	return nil
}

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
