package expand

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// InjectMode controls whether the expand tool is advertised on outgoing requests.
//
//   - "auto" (default): inject whenever the request already declares tools AND the store
//     can persist stashes. Both conditions are properties of the SESSION, not of the turn,
//     so the `tools` array a session sends is byte-identical on every request in it.
//
//     That stability is the whole point. `tools` sits ahead of `system` and `messages` in
//     the provider's cache hash, so a change to the array cannot be cached against the old
//     prefix: the first request carrying a NEW tools array re-creates the ENTIRE prefix at
//     the write rate, not just the tail. This condition used to also require that the
//     request carry an expandable marker, which made advertising a property of the turn:
//     the array grew on the first turn that offloaded and shrank again on any later turn
//     that carried no marker.
//
//     MEASURED, and narrower than an earlier version of this comment claimed. Direct
//     probe, byte-identical 9.5k-token system block, only `tools` changed: A->A read 9,471
//     / write 0; A->B write 9,558 / read 0 — the whole prefix. But A->B->A->B then reads on
//     all three, because the provider keeps BOTH lineages alive within the TTL. So the cost
//     is ONE full-prefix write per NEW tools variant, plus a doubled write delta while it
//     alternates — not a full miss on every flip in both directions, which is what this
//     comment used to say.
//
//     On a real 7-turn Claude Code session the old build flipped once, at the turn the
//     first dedup marker appeared: tools 20->21, that turn billed write 43,359 / read 0
//     against the 40,320-token prefix the first three turns had built ($0.0824 against
//     $0.0071 for a comparable turn with no flip). After the fix, one tools hash across all
//     7 turns and that turn billed write 163 / read 43,135. Paired sessions, run in both
//     arm orders: input cost $0.212401 -> $0.137185, a 35.4% and 35.3% saving on 0.1 pp of
//     spread, with total input tokens up 313 — the same bytes, moved from 1.25x to 0.1x.
//     The tool declaration's own cost is +129 tokens on the first write, included in those
//     figures. A session that never offloads saves nothing.
//
//     It never perturbs a request that uses no tools — the riskiest case for models that
//     penalize an unexpected tool — because `hasTools` is still required. What it DOES do
//     is advertise on a request with nothing to expand, which is safe now for a reason
//     that is elsewhere: an expand call that resolves nothing is answered with a
//     placeholder tool_result and the turn completes normally (proxy.serve). It used to
//     replay the model's raw tool_use to the client instead, which on an agent's own
//     compaction request reads as "the summary came back empty" — three of those in a row
//     and Claude Code disables auto-compact for the session. THAT is what the marker
//     condition was really protecting against, and it was protecting against it in the
//     wrong place: at the advertisement, at the cost of the prefix, rather than at the
//     resolution, which costs one bounded round trip and only when a model asks for an id
//     that has aged out.
//
//   - "always": inject whenever the store persists, creating the tools array if absent.
//     Differs from auto only for a request that declares NO tools, which is the case auto
//     deliberately leaves alone.
//
//   - "never": never inject (the pre-D2 behavior; pair with marker_mode: summary).
const (
	InjectAuto   = "auto"
	InjectAlways = "always"
	InjectNever  = "never"
)

// Inject appends the expand tool definition to the request body's tools array in a
// byte-stable way, so a model that offloaded content can call context_guru_expand to
// get it back (closing the reversibility loop the proxy's continuation handler drives).
//
// It is idempotent (a request that already declares the tool is returned unchanged),
// appends the tool LAST so the client's own tools keep their exact order and the
// provider prefix cache stays warm, and respects a forcing tool_choice (skips, so it
// never changes which tool the model is compelled to call). Fail-open: any error
// returns the original body with injected=false.
func Inject(provider, mode string, body []byte, storePersists bool) (out []byte, injected bool) {
	if mode == InjectNever || !storePersists {
		return body, false
	}
	// Respect an explicit non-auto tool_choice: forcing/none means we must not
	// perturb tool selection.
	if tc := gjson.GetBytes(body, "tool_choice"); tc.Exists() {
		if !toolChoiceIsAuto(tc) {
			return body, false
		}
	}
	tools := gjson.GetBytes(body, "tools")
	hasTools := tools.Exists() && tools.IsArray() && len(tools.Array()) > 0
	// No marker condition, deliberately: see the note on InjectAuto. Advertising must not
	// depend on anything that varies turn to turn, or the tools array varies with it and
	// every variation is a whole-prefix cache miss.
	if mode == InjectAuto && !hasTools {
		return body, false
	}
	// Idempotent: skip if the expand tool is already present.
	if HasTool(provider, body) {
		return body, false
	}
	nb, err := sjson.SetRawBytes(body, "tools.-1", ToolDefRaw(provider))
	if err != nil {
		return body, false // fail open
	}
	return nb, true
}

// HasTool reports whether body's tools array already declares the expand tool. It is
// the ADVERTISE test: a host must intercept expand calls exactly when this is true, or
// it either declares a tool whose use it ignores (the call reaches a client that has no
// such tool) or pays to inspect responses that cannot contain one.
func HasTool(provider string, body []byte) bool {
	nameField := "function.name"
	if provider == "anthropic" {
		nameField = "name"
	}
	for _, t := range gjson.GetBytes(body, "tools").Array() {
		if t.Get(nameField).String() == ToolName {
			return true
		}
	}
	return false
}

// toolChoiceIsAuto reports whether a tool_choice value leaves the model free to
// choose (so injecting one more tool is safe). OpenAI: the string "auto". Anthropic:
// an object {"type":"auto"}. Anything else (none/required/any/a specific tool) is
// treated as forcing and injection is skipped.
func toolChoiceIsAuto(tc gjson.Result) bool {
	if tc.Type == gjson.String {
		return tc.String() == "auto"
	}
	if tc.IsObject() {
		return tc.Get("type").String() == "auto"
	}
	return false
}
