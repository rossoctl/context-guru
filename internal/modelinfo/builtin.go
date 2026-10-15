package modelinfo

import "context"

// Builtin answers EXACT windows for a short list of current Claude models, for the time the
// downloaded LiteLLM map is not there: no network, a request that arrives before the download ends,
// or a model the map does not list yet. Without it every min_request_frac trigger is off in those
// cases, because DefaultStatic is never exact (issue #515).
//
// An entry matches ONE normalized model id (provider prefix and [1m] removed), and nothing else. There is no substring or prefix match.
// `claude-sonnet-5-future` is not `claude-sonnet-5`, so a model with no entry stays inexact and
// falls through to DefaultStatic. TestBuiltinPinnedToBundledMap pins every value to the bundled
// LiteLLM snapshot, so an entry that drifts from the public map fails CI.
//
// Chain order: the downloaded map, then Builtin, then DefaultStatic. The map stays the authority.
type Builtin struct{ windows map[string]int }

// builtinWindows holds normalized ids (see normalize). Add a model only when the bundled snapshot
// in testdata lists it.
var builtinWindows = map[string]int{
	"claude-fable-5":    1000000,
	"claude-fable-5-1":  1000000,
	"claude-opus-5-5":   1000000,
	"claude-sonnet-5-5": 1000000,
	"claude-haiku-5-5":  1000000,
	"claude-haiku-4-5":  200000,
	"claude-opus-5":     1000000,
	"claude-sonnet-5":   1000000,
	"claude-opus-4-6":   1000000,
	"claude-opus-4-7":   1000000,
	"claude-opus-4-8":   1000000,
	"claude-sonnet-4-6": 1000000,
}

// DefaultBuiltin returns the built-in exact list.
func DefaultBuiltin() Builtin { return Builtin{windows: builtinWindows} }

// Window returns the window for an exactly listed model.
func (b Builtin) Window(_ context.Context, model string) (int, bool) {
	// The last path segment, so a gateway route (aws/…, anthropic/…) matches. Still one whole id.
	_, tail := normalize(model)
	w, ok := b.windows[tail]
	return w, ok
}

// WindowExact reports exact=true for every hit. A miss says nothing about exactness.
func (b Builtin) WindowExact(ctx context.Context, model string) (int, bool, bool) {
	w, ok := b.Window(ctx, model)
	return w, ok, ok
}

// NewChain is the default chain for the public map: live map, then Builtin, then DefaultStatic.
func NewChain(live Resolver) Chain {
	return Chain{live, DefaultBuiltin(), DefaultStatic()}
}
