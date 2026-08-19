package components

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"

	"gopkg.in/yaml.v3"
)

// Constructor builds a component instance. It receives the raw config block for
// its name (may be nil), so a component can be configured purely from YAML with
// no core change. Returning an error aborts pipeline construction.
type Constructor func(raw []byte) (Component, error)

var (
	regMu    sync.RWMutex
	registry = map[string]Constructor{}
)

// Register makes a component available by name. Called from each component's
// init(); double-registration or an empty name panics at boot (the
// database/sql pattern AuthBridge also uses).
func Register(name string, c Constructor) {
	if name == "" {
		panic("components: Register with empty name")
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := registry[name]; dup {
		panic("components: duplicate registration for " + name)
	}
	registry[name] = c
}

// Names lists every registered component name, sorted.
func Names() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// New builds a single component by name with its raw config block.
func New(name string, raw []byte) (Component, error) {
	regMu.RLock()
	ctor, ok := registry[name]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("components: unknown component %q (registered: %v)", name, Names())
	}
	return ctor(raw)
}

// Field describes ONE configurable key of one component, for the settings form. It is form
// metadata only: nothing in the run path reads it, and a component's constructor behaves
// identically whether or not its fields are declared.
//
// It exists because the settings page used to hand-list the keys it knew about, in Go and
// again in JavaScript, and nothing could notice an omission: the form exposed 18 keys of
// ~100, one component of fifteen, and the keys it did expose had already drifted from the
// values the engine accepts (a stored `strategy: deterministic` was not recognised and got
// REWRITTEN to `code`, silently turning an LLM-free configuration into one that makes model
// calls). Declaring the keys beside the config struct they describe, and deriving the form
// from that, makes the omission a compile-or-test failure instead of a silent one.
type Field struct {
	// Key is the DOTTED path of the key inside the component's block —
	// `cold_cache.min_tokens`, `model.source` — so a nested block needs no nesting here.
	Key string `json:"key"`
	// Type is one of the Field* constants below. It tells the form what control to draw
	// and the server what a posted value must decode to.
	Type string `json:"type"`
	// Default is what an ABSENT key means to the component, i.e. what the form should show
	// greyed as the fallback. It is NOT the recommended prefill — that is a policy layer
	// (see config.RecommendedExtractLLM), and conflating the two is how a form ends up
	// writing its own opinion over an operator's deliberate value. nil = the type's zero.
	Default any `json:"default,omitempty"`
	// Options is the permitted set for Type == FieldEnum, in display order.
	Options []string `json:"options,omitempty"`
	// Hint is one line of prose for the field, taken from the doc comment already written
	// on the config struct.
	Hint string `json:"hint,omitempty"`
	// Secret marks a credential: the form must never echo a stored value back, and the
	// control is write-only.
	Secret bool `json:"secret,omitempty"`
	// Min is the smallest accepted number. It carries real semantics: 0 on a CAP means
	// "unlimited" and is a legitimate choice, while a size threshold with Min 1 rejects 0
	// because 0 there is not a setting, it is a removed brake.
	Min int `json:"min,omitempty"`
}

// Field types.
const (
	FieldBool    = "bool"
	FieldInt     = "int"
	FieldFloat   = "float"
	FieldEnum    = "enum"
	FieldString  = "string"
	FieldStrings = "strings"
)

var (
	fields = map[string][]Field{}
	protos = map[string]any{}
)

// RegisterFields declares a component's configurable keys, from the same init() that
// registers the component. config is a ZERO VALUE of the component's config struct: the
// anti-drift test reflects over its yaml tags and fails when the declared keys and the
// struct's keys disagree, which is the only thing that catches "a component gained a knob
// and nobody told the form".
func RegisterFields(name string, config any, f []Field) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := fields[name]; dup {
		panic("components: duplicate RegisterFields for " + name)
	}
	fields[name] = f
	protos[name] = config
}

// Fields returns a component's declared fields, in declaration order.
func Fields(name string) []Field {
	regMu.RLock()
	defer regMu.RUnlock()
	return fields[name]
}

// AllFields returns every registered component's declared fields.
func AllFields() map[string][]Field {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make(map[string][]Field, len(fields))
	for k, v := range fields {
		if v == nil {
			v = []Field{} // serves as [] rather than null: a component with no knobs
		}
		out[k] = v
	}
	return out
}

// ConfigProto returns the zero-value config struct a component declared with its fields.
// For the parity test; nothing in the run path needs it.
func ConfigProto(name string) (any, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	p, ok := protos[name]
	return p, ok
}

// Decode unmarshals a component's raw config block STRICTLY: an unknown key is an error
// rather than silence.
//
// Non-strict was the default and it made the settings form untrustworthy in the one way
// that matters — `min_tokns: 5000` on any component parsed fine, changed nothing, and
// reported nothing. config.LoadBytes has rejected typos at the top level since day one;
// per-component blocks are where the numbers that cost money live.
func Decode(raw []byte, dst any) error {
	if len(raw) == 0 {
		return nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
