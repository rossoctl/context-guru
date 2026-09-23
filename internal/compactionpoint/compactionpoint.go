// Package compactionpoint answers ONE question: at what provider-billed input does a conversation's
// own compaction mechanism act?
//
// It is a separate package because two very different layers need the same answer and neither can
// own it. The GATE needs it to decide when to fire (components, through apply); the PANEL needs it to
// size an episode's attribution span (dash). A copy in each would be two tables to keep in agreement,
// which is the failure this repo has already had with cache TTLs and with token rulers.
//
// It is NOT in internal/modelinfo, despite being keyed by model, because it is not a property of the
// model. It is a property of a (deployment, client, model) TRIPLE: the same model behind a different
// agent, or behind Claude Code with a different auto-compact threshold, acts somewhere else. modelinfo
// owns what the provider publishes; this owns what a client does.
package compactionpoint

import (
	"strings"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// THE DEFINITION
//
//	C(deployment, client, model) is the PROVIDER-BILLED INPUT at which the conversation's own
//	compaction mechanism acts -- the largest prompt that mechanism allows to be sent before it
//	rewrites the transcript.
//
// Two figures derive from it and nothing else should:
//
//	the TRIGGER   summarize fires at  frac x C          (shipped frac 0.9)
//	the SPAN      an episode covers   C - frac x C       = (1 - frac) x C
//
// Both from C rather than from the model window W because that IS the argument for the feature:
// compacting just before the conversation's own mechanism would have acted captures the saving of a
// large prefix going cold, and costs no accuracy that was not already going to be lost -- a
// compaction was going to happen there anyway. Firing against W is only correct when C == W.
//
// # WHICH RULER, and why this definition never needs the client's own number
//
// C is in provider-billed input (fresh + cache_read + cache_write): the ruler the gate compares
// (Ctx.PrevBilledInput), the ruler a context window is stated in, and the only one every party
// shares. One real request, counted three ways:
//
//	provider-billed input        199,184   = 0.996 of a 200,000 window   <-- C is in THIS
//	Claude Code's own count      ~168,000  = 0.840                        <-- never used here
//	our message-text count       147,493   = 0.737                        <-- tokens_before
//
// A client thresholding at ~168,000 of ITS count and the provider billing 199,184 on that same
// request are THE SAME EVENT. Defining C as an observation in billed tokens means the conversion
// factor between rulers never has to be known, and so cannot be got wrong. Putting 0.840 here would
// be the units error Trigger.Fires shipped with, one level up.
//
// # "WHENEVER THE NATIVE COMPACTION WORKS" -- four cases, because it genuinely differs
//
//  1. THE CLIENT COMPACTS (Claude Code and similar). C is OBSERVED: the billed input of a request
//     the agent-compaction detector flagged. proxy/agentcompaction.go already computes that verdict,
//     so C is measurable from traffic with no new capture. Verified on a real session: exactly one of
//     53 rows was flagged, and it billed 199,184 -- the transcript at its largest -- with the
//     following turn's tokens_before drop agreeing exactly.
//  2. WE ASK THE PROVIDER TO COMPACT (#241). C is CHOSEN, not observed: the trigger.input_tokens we
//     set. Exactly known, in billed tokens, and on the wire.
//  3. NOTHING COMPACTS (a raw API client, llm-d, self-hosted). C is the hard limit, i.e. the window:
//     C == W, and that is a FACT. Also the deployment where this component matters most, because
//     nothing else stands between the session and a 400.
//  4. UNKNOWN -- never observed, and case 1 vs case 3 indistinguishable. C == W as a GUESS, which
//     must not report alike with case 3.
//
// # C IS NOT ON THE WIRE, and max_tokens is NOT a substitute -- both checked
//
// The tempting shortcut, and why it is not taken. Claude Code sends max_tokens on every request
// (32,000 on haiku), and W - max_tokens = 168,000, which is close to the ~167,000 threshold its own
// indicator reports. It is ONE data point and it is probably a coincidence:
//
//   - max_tokens is the OUTPUT budget. Reading it as a compaction threshold requires the client to
//     have chosen a "reserve one full response" policy, and nothing observed says it did.
//   - THERE IS EVIDENCE AGAINST the mechanism that would make it non-arbitrary. If the provider
//     enforced `input + max_tokens <= W`, the client would be FORCED to compact around
//     W - max_tokens. It does not: a captured request billed 199,184 input with max_tokens 32,000 on
//     a 200,000 window -- 231,184 together -- and succeeded. So W - max_tokens is not a constraint
//     the provider imposes, and the client landing near it would be policy we have not seen.
//   - It is in the CLIENT's ruler anyway, so using it would need a billed conversion ratio we would
//     be guessing (1.186 on the one observation).
//
// AND THE PROVIDER DOES NOT TELL US EITHER. A real Claude Code request was captured and inspected in
// full: `max_tokens`, `messages`, `metadata` (device and session ids), `model`, `output_config` (a
// JSON output schema), `system`, `temperature`, `thinking`, `tools`, `stream`. No `context_management`
// block, and nothing anywhere in the record naming context, compaction, a limit, a window, a budget or
// a threshold. That is not surprising on reflection -- where the CLIENT compacts is the client's own
// policy, so the provider has no reason to know it.
//
// So the only sound source is DIRECT OBSERVATION in billed tokens, which is case 1 below and already
// works. scripts/scenarios/d-maxtokens-rule.sh exists to FALSIFY the max_tokens shortcut on a second
// model rather than to adopt it -- cheap insurance against someone building on the coincidence.
//
// # THE STATISTIC, once a deployment has several observations
//
// A LOW percentile, and that is principled rather than taste: a lower C fires EARLIER (still beating
// the client) and narrows the span (under-reports), so it is conservative for both uses. A C that is
// too HIGH is the dangerous one -- the client resets before billed input ever reaches frac x C, so
// the component never fires at all and looks broken. An observation is also an UPPER bound on the
// threshold, since the client acts on EXCEEDING it, so a low percentile plus frac x C pulls under
// twice.
//
// #239 is the issue for learning C per deployment. Until then the table below is a bootstrap.

// Source says where a resolved C came from, so a published figure can be read for what it is. The
// same measured/guessed discipline modelinfo.Exact keeps for windows.
type Source string

const (
	// Measured: case 1 or 2 -- observed in billed tokens on a real run, or set by us.
	Measured Source = "measured"
	// Assumed: reported behaviour or a reasoned bound, not observed.
	Assumed Source = "assumed"
	// WindowFallback: case 3 and case 4 together -- C == W, because either nothing compacts here
	// (a fact) or we have never seen this client compact (a guess). This package cannot yet tell
	// those apart, which is itself worth reporting rather than smoothing over.
	WindowFallback Source = "window_fallback"
)

// Point is one resolved C.
type Point struct {
	// Frac is C as a fraction of the model's context window, in provider-billed tokens.
	Frac   float64
	Source Source
	// Note is why this value, in enough detail to tell whether it still holds.
	Note string
}

// Tokens is C in absolute billed tokens for a given window. 0 when the window is unknown.
func (p Point) Tokens(window int) int {
	if window <= 0 || p.Frac <= 0 {
		return 0
	}
	return int(p.Frac * float64(window))
}

// table is matched by SUBSTRING against the normalized model id, LONGEST pattern first, so
// `claude-haiku-4-5` beats a bare `claude` and a provider prefix or a `[1m]` variant suffix does not
// defeat the match (modelinfo.NormalizeID strips both).
//
// Longest-match rather than declaration order, because a table whose correctness depends on nobody
// appending in the wrong place is a table that gets appended to in the wrong place -- which is
// exactly what #233 was, in the window table next door.
//
// EVERY ENTRY IS A BOOTSTRAP. It is what a deployment gets before it has observations of its own, and
// it cannot capture the largest source of variation: the client's auto-compact threshold is settable,
// so two tenants on the same model legitimately differ.
var table = []struct {
	Pattern string
	Point   Point
}{
	{"claude-haiku-4-5", Point{
		Frac:   0.996,
		Source: Measured,
		Note: "case 1, measured: on a real Claude Code session the request the agent-compaction " +
			"detector flagged billed 199,184 of a 200,000 window, and the next turn's tokens_before " +
			"drop agreed. n=1, one client version, one configuration. Consistent with the client " +
			"reserving max_tokens (32,000) of headroom in its own accounting.",
	}},
	{"claude-opus-5", Point{
		Frac:   1.00,
		Source: Assumed,
		Note: "reported behaviour: Claude Code on Opus runs to essentially 100% of the window before " +
			"compacting. Not observed in billed tokens by any run, and NOT corroborated by the " +
			"max_tokens derivation -- that shortcut is one data point on haiku with evidence against " +
			"its mechanism, so it cannot be cited in support of this entry. 1.00 is the LEAST " +
			"conservative value available and too-high is the dangerous direction for the trigger, so " +
			"a measurement here can only move published savings down.",
	}},
	{"claude-sonnet-5", Point{
		Frac:   1.00,
		Source: Assumed,
		Note: "no observation of any kind; inherited from the Opus entry on the reasoning that both " +
			"ship a 1M window behind the same client. The entry most worth measuring.",
	}},
}

// For resolves a model's compaction point from the bootstrap table.
//
// An unlisted model gets C == W. That is NOT conservative for the span -- too high makes the span too
// wide and OVER-reports -- and it is chosen anyway, because a fabricated lower bound would make every
// unlisted model's savings quietly smaller by an amount nobody chose, and an operator comparing two
// models would be reading a difference this table invented. C == W is transparently an absence of
// information rather than a guess pretending to be one, and the Source says so.
func For(model string) Point {
	id := modelinfo.NormalizeID(model)
	best := Point{Frac: 1.00, Source: WindowFallback,
		Note: "no entry and no observation for this model, so C is the whole window. Correct where " +
			"nothing compacts (a raw API client); a guess where the client compacts and we have not " +
			"seen it do so. This package cannot yet tell those apart."}
	longest := 0
	for _, e := range table {
		if len(e.Pattern) > longest && strings.Contains(id, e.Pattern) {
			best, longest = e.Point, len(e.Pattern)
		}
	}
	return best
}
