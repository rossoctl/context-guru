// Package coref builds the Tier-1 co-reference index that co-reference-aware
// compaction decides from: for each tool output in a request, which identifiers that
// output INTRODUCED, and whether any later turn carried them forward.
//
// It is deliberately free of the bifrost schema, of the components package and of the
// tokenizer. The index is a pure function of a flattened message list, which is what
// lets the Go component and the offline measurement pass
// (deploy/harbor/coref.py) share ONE definition of "reference" and be checked against
// the same known-answer fixture. A component whose notion of a reference had drifted
// from the script's would be calibrated against thresholds measured for a different
// algorithm — the thresholds are the whole output of the measurement, so that drift
// would be silent and total.
//
// See docs/proposals/coref-compaction.md for what the index is FOR: §2 (the three
// tiers and the echo confound), §3 (open vs closed, and why recency is measured from
// the head of the transcript rather than from the output).
package coref

import (
	"regexp"
	"strings"
)

// identRe matches an identifier-ish token: the things a model actually carries forward
// out of a tool output — paths, symbols, ids, hashes, error codes, line numbers. Prose
// is filtered out by distinctive below rather than by a stopword list, which does not
// survive a change of domain (or of natural language).
var identRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_./:\-]{2,63}|\b\d{3,}\b|\b[0-9a-f]{7,40}\b`)

// numericRe matches a token that is purely a number (with thousands/decimal separators).
var numericRe = regexp.MustCompile(`^\d[\d,._]*$`)

// punctEdge is the punctuation trimmed from a token's ends before it is judged. Interior
// punctuation is structure; trailing punctuation is usually a sentence mark.
const punctEdge = "._:-/"

// distinctive keeps tokens that look like an identifier rather than an English word: after
// trimming surrounding punctuation they carry INTERIOR structure (_ . / : -), a digit, or
// CamelCase.
//
// The point is precision, not recall. A token that also occurs in ordinary prose produces a
// spurious reference, and a spurious reference makes an output look load-bearing when it is
// not — which suppresses compaction silently. Missing a real reference is the safe
// direction: it can only make the index report LESS cuttable mass than exists.
//
// All three rules below were measured rather than guessed. An earlier version also accepted
// any token of 10+ characters, any token containing punctuation anywhere, and any number of
// 3+ digits; run over real agent traffic, the top "references" it produced were
// `description`, `transparency`, `efficiency`, `conditions`, `e.g.`, `try:`, `None:` and
// `2026`, which inflated referenced mass from 51% to 71% of all tool-output tokens:
//
//   - No bare length rule. Long English words are still English words, and a real
//     identifier almost always carries structure, a digit, or camelCase (`src/auth.py`,
//     `session_id`, `GraphStore`). One carrying none of those cannot be told from prose.
//   - Punctuation must be INTERIOR. `e.g.` / `try:` / `memory.` are prose plus a sentence
//     mark; trimmed they become `e.g` / `try` / `memory` and fail on their own merits, while
//     `src/auth.py` and `v1/messages` are untouched.
//   - A bare number needs 5+ digits or a separator. `2026` is a year and `447` is a line
//     number or a count; both recur everywhere. Hashes, ids and versions survive.
func distinctive(t string) bool {
	t = strings.Trim(t, punctEdge)
	if len(t) < 4 {
		return false
	}
	if numericRe.MatchString(t) {
		digits := 0
		for i := 0; i < len(t); i++ {
			if t[i] >= '0' && t[i] <= '9' {
				digits++
			}
		}
		return digits >= 5 || strings.ContainsAny(t, ",._")
	}
	if strings.ContainsAny(t, "_./:-") {
		return true
	}
	for i := 0; i < len(t); i++ {
		if t[i] >= '0' && t[i] <= '9' {
			return true
		}
	}
	return hasCamel(t)
}

// hasCamel reports whether t contains a lower→upper ASCII transition (camelCase).
// identRe only ever yields ASCII, so a byte scan is equivalent to the `[a-z][A-Z]`
// pattern the measurement script uses.
func hasCamel(t string) bool {
	for i := 1; i < len(t); i++ {
		if t[i-1] >= 'a' && t[i-1] <= 'z' && t[i] >= 'A' && t[i] <= 'Z' {
			return true
		}
	}
	return false
}

// Idents returns the distinctive identifier tokens in s, as a set. Tokens are trimmed of
// surrounding punctuation so `memory.` and `memory` are ONE token rather than two that can
// never match each other.
func Idents(s string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, t := range identRe.FindAllString(s, -1) {
		if distinctive(t) {
			out[strings.Trim(t, punctEdge)] = struct{}{}
		}
	}
	return out
}

// Result is one tool output under evaluation, identified by the tool-call id that
// produced it (a single turn can carry several).
type Result struct {
	ID   string
	Text string
}

// Message is one flattened transcript entry. Texts is the reference-BEARING surface
// (prose, plus tool-call names and arguments); Results is the mass under evaluation.
//
// Both surfaces feed the echo-exclusion set, and only Texts can constitute a
// reference — see Index. Callers build these 1:1 with their own message indices so a
// Record's Idx points back at the message it came from.
type Message struct {
	Texts   []string
	Results []Result
}

// Class is the verdict for one tool output.
type Class string

// The three verdicts. Cutting Unreferenced needs no reference model beyond "nobody
// ever used this"; cutting Closed is the large, early cut that needs the thresholds.
const (
	// Unreferenced — no later turn exactly reuses anything this output introduced.
	// The safest cut on Tier 1, and blind to Tier 2 (a value that was transformed
	// before being restated leaves no exact match). Never read this as "unused".
	Unreferenced Class = "unreferenced"
	// Closed — referenced a small number of times, and not for a long time. Whatever
	// the model took survives in the turn that took it, so the original is redundant
	// with content still in the request. The case-A large-cut candidate.
	Closed Class = "closed"
	// Open — referenced recently, or repeatedly. Still load-bearing; keep.
	Open Class = "open"
)

// Record is the per-output measurement the classifier decides from.
type Record struct {
	// Idx is the caller's message index; ID the tool-call id within it.
	Idx int
	ID  string
	// SizeTokens is the output's own size — the mass a cut would recover.
	SizeTokens int
	// Novel counts the identifiers this output INTRODUCED (see Index).
	Novel int
	// Refs counts the later turns that reused at least one novel identifier.
	Refs int
	// RefAge is how many messages AGO the last reference was, counted from the head of
	// the transcript; -1 when there was none. This is the A/B axis. The tempting
	// quantities — the output's own depth, or the gap from the output to its reference —
	// are both something else: "recent messages vs early messages" is a statement about
	// now, so it has to be measured from now.
	RefAge int
	// ConsumeLag is how many messages after the output its LAST reference was; -1 when
	// there was none. Reported separately from RefAge because it answers a different
	// question — how long the output stayed live — and conflating the two is what makes
	// a hot old span look like a cold one.
	ConsumeLag int
	// UsedFrac is the share of the novel identifiers the model actually carried
	// forward. A low value on a referenced output is the "took one value, does not need
	// the rest" pattern, measured rather than assumed.
	UsedFrac float64
}

// Classify applies the open/closed predicate. closedDist is the recency floor (a last
// reference NEWER than this many messages ago keeps the output open); openReps is the
// repetition ceiling (referenced at least this many times keeps it open regardless of
// age, because a span referenced repeatedly is a hot span that happens to be old).
func Classify(r Record, closedDist, openReps int) Class {
	if r.Refs == 0 {
		return Unreferenced
	}
	if (openReps > 0 && r.Refs >= openReps) || r.RefAge < closedDist {
		return Open
	}
	return Closed
}

// Index computes one Record per tool output at least minOutputTokens in size, using
// tok to measure size (nil falls back to the ~4-chars/token proxy the measurement
// script uses).
//
// Two exclusions do the real work, and neither is optional:
//
// ECHO. Only identifiers the output INTRODUCED are eligible. If the agent calls
// Read(src/auth.py), the path arrives in the tool-call ARGUMENT, is echoed by the
// result, and appears again in a later Edit(src/auth.py) — an exact matcher sees a
// reference from the output to a later turn, but nothing was ever taken FROM the
// output. So a token is novel only if it appears nowhere at or before this message
// (in any surface), nor in a sibling result of the same turn. On the fixture, dropping
// this guard flips a plainly-unreferenced file read to open and halves the measured
// cuttable mass: it is the difference between a usable measurement and one that
// reports everything as load-bearing.
//
// BOILERPLATE. A token echoed by many outputs (more than max(5, outputs/4)) is
// session furniture — a banner, a prompt, a repeated header — not a carried value.
//
// Outputs below minOutputTokens get no Record but still contribute to both exclusion
// sets, because they are part of the context the model saw.
func Index(msgs []Message, minOutputTokens int, tok func(string) int) []Record {
	return index(msgs, minOutputTokens, tok, true)
}

// index is Index with the echo guard made switchable, so the test can run the negative
// control that proves the guard is what produces the result. The knob is unexported on
// purpose: priorGuard=false is a KNOWN-WRONG index (it counts the tool-call argument
// echoed by its own result as a reference), and no caller should be able to select it.
func index(msgs []Message, minOutputTokens int, tok func(string) int, priorGuard bool) []Record {
	if tok == nil {
		tok = approxTokens
	}
	n := len(msgs)

	// Per-surface token sets. refTokens is the reference-bearing surface of each
	// message; resTokens the outputs, keyed by tool-call id.
	refTokens := make([]map[string]struct{}, n)
	resTokens := make([]map[string]map[string]struct{}, n)
	nOut := 0
	for i := range msgs {
		refTokens[i] = Idents(strings.Join(msgs[i].Texts, " "))
		resTokens[i] = make(map[string]map[string]struct{}, len(msgs[i].Results))
		for _, r := range msgs[i].Results {
			resTokens[i][r.ID] = Idents(r.Text)
			nOut++
		}
	}

	// firstSeen[t] is the lowest message index at which t occurs in ANY surface, so
	// "t was already in context before message i" is firstSeen[t] < i. This replaces a
	// per-message snapshot of the running union — same answer, but O(distinct tokens)
	// memory instead of O(messages × tokens), which matters at the transcript sizes
	// this component fires on.
	firstSeen := make(map[string]int)
	note := func(i int, set map[string]struct{}) {
		for t := range set {
			if _, ok := firstSeen[t]; !ok {
				firstSeen[t] = i
			}
		}
	}
	for i := range msgs {
		note(i, refTokens[i])
		for _, toks := range resTokens[i] {
			note(i, toks)
		}
	}

	// spread[t] is how many distinct outputs carry t; past a threshold it is furniture.
	spread := make(map[string]int)
	for i := range msgs {
		for _, toks := range resTokens[i] {
			for t := range toks {
				spread[t]++
			}
		}
	}
	commonAt := nOut / 4
	if commonAt < 5 {
		commonAt = 5
	}

	var recs []Record
	for i := range msgs {
		for _, r := range msgs[i].Results {
			size := tok(r.Text)
			if size < minOutputTokens {
				continue
			}
			// Sibling results of this same turn: the producing tool call normally lands in
			// the previous message (and so in firstSeen), but a batched turn carries several
			// results at once and they must not credit each other.
			siblings := map[string]struct{}{}
			for id, toks := range resTokens[i] {
				if id == r.ID {
					continue
				}
				for t := range toks {
					siblings[t] = struct{}{}
				}
			}
			novel := map[string]struct{}{}
			for t := range resTokens[i][r.ID] {
				if fs, ok := firstSeen[t]; priorGuard && ok && fs < i {
					continue // already in context before this output existed
				}
				if _, ok := siblings[t]; ok {
					continue
				}
				if spread[t] > commonAt {
					continue // session furniture
				}
				if _, ok := refTokens[i][t]; ok {
					continue // the same turn's own prose/arguments
				}
				novel[t] = struct{}{}
			}

			rec := Record{Idx: i, ID: r.ID, SizeTokens: size, Novel: len(novel), RefAge: -1, ConsumeLag: -1}
			used := map[string]struct{}{}
			last := -1
			for j := i + 1; j < n; j++ {
				if len(refTokens[j]) == 0 {
					continue
				}
				hit := false
				for t := range novel {
					if _, ok := refTokens[j][t]; ok {
						used[t] = struct{}{}
						hit = true
					}
				}
				if hit {
					rec.Refs++
					last = j
				}
			}
			if last >= 0 {
				rec.RefAge = n - last
				rec.ConsumeLag = last - i
			}
			if len(novel) > 0 {
				rec.UsedFrac = float64(len(used)) / float64(len(novel))
			}
			recs = append(recs, rec)
		}
	}
	return recs
}

// approxTokens is the ~4-chars/token proxy the offline pass uses, so an Index built
// without a real tokenizer sizes outputs the same way the measurement did.
func approxTokens(s string) int {
	if n := len(s) / 4; n > 1 {
		return n
	}
	return 1
}
