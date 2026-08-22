package extract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/maphash"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/rossoctl/context-guru/internal/tokens"
)

// cgMarkerRe matches the offload marker so ContentKey is marker-insensitive (a
// re-sent body that a sibling component marked still hits the extraction cache).
var cgMarkerRe = regexp.MustCompile(`<<cg:[A-Za-z0-9_-]{1,64}>>`)

const sampleChars = 4000

// Model is the cheap-model client the extractor calls. The host (proxy / plugin)
// injects a concrete implementation; the extractor core stays transport-free.
type Model interface {
	Complete(ctx context.Context, prompt string) (string, error)
}

// SystemModel is the optional capability a Model may also implement: send the invariant
// instructions as a separately-cacheable stable prefix. cheapmodel's Anthropic (a
// `system` block + cache_control) and OpenAI (a leading system message) both do. A Model
// that does NOT implement it still works — the extractor falls back to the single-message
// prompt — so this is additive, not a breaking interface change.
type SystemModel interface {
	CompleteSystem(ctx context.Context, system, prompt string) (string, error)
}

// SystemBlocksModel is the further optional capability: send the invariant instructions as
// SEVERAL ordered blocks, so a provider can cache each prefix separately. It exists because
// the general contract is identical for every tenant while the aggressiveness block is not,
// and one joined string would give the two a single cache key — making the shared half
// unshared the moment two tenants pick different levels.
type SystemBlocksModel interface {
	CompleteBlocks(ctx context.Context, system []string, prompt string) (string, error)
}

// completeSplit sends (systemBlocks, user) through the best capability the client has:
// separate cacheable blocks, else one joined system field, else a single user message.
// Identical content in all three cases — only the caching differs — so a Model that
// implements neither optional interface still works.
func completeSplit(ctx context.Context, model Model, system []string, user string) (string, error) {
	if bm, ok := model.(SystemBlocksModel); ok {
		return bm.CompleteBlocks(ctx, system, user)
	}
	joined := strings.Join(system, "\n\n")
	if sm, ok := model.(SystemModel); ok {
		return sm.CompleteSystem(ctx, joined, user)
	}
	return model.Complete(ctx, joined+"\n\n"+user)
}

// Cfg configures extraction.
type Cfg struct {
	Mode  string // auto | single | rlm | deterministic
	Floor int    // token floor; rlm kicks in at max(floor*4, 8000) in auto
	// MinKeepRatio is the fraction of the body the result must still contain. 0 disables the
	// blunt ratio backstop and leaves only the keep-set check, which is not enough on its own:
	// see minKeepRatioFloor for the live failure that proved it.
	MinKeepRatio       float64
	AllowDeterministic bool
	MaxChars           int // deterministic projection window
	// AllowedStrategies, when non-empty, restricts strategyOrder to these strategy names
	// (code | single | rlm | deterministic) preserving the computed order. Empty means
	// "all" — prior behavior. Lets config enable/disable strategies purely by name.
	AllowedStrategies []string
	// Rewrite opts out of the containment proof (deletion-only guarantee): the model
	// may reword/summarize/rewrite freely. Lossy + unverified — the caller must accept
	// that (e.g. a non-full marker_mode). Default false keeps the verified guarantee.
	Rewrite bool
	// CacheContext moves the conversation context into a trailing CACHEABLE system block
	// instead of the user message. Worth it only when the request will make MORE THAN ONE
	// call: the context is identical across a request's candidates, so calls 2..N read it
	// instead of re-sending it — but a cache WRITE costs 1.25x fresh, so paying it for a
	// single call is a 25% loss. The caller decides; default false keeps the old placement.
	//
	// Measured on production: five haiku calls on ONE request each sent ~138,000 prompt
	// tokens with cache_read=0 and cache_write=0, the same transcript five times over.
	CacheContext bool
	// Aggressiveness selects the compaction target taught in the second system block
	// (low | medium | high; empty = medium). It changes what the model is ASKED for, never
	// what is ACCEPTED — the verbatim-preservation, strictly-smaller and (in deletion-only
	// mode) subsequence checks are identical at every level.
	Aggressiveness Aggressiveness
}

// minKeepRatioFloor is the fraction of a body an extraction must still contain to be an
// extraction rather than a deletion.
//
// The keep-ratio backstop existed and was DEAD: DefaultCfg never set MinKeepRatio, so
// insanityReason's check was unreachable for every caller. FOUND LIVE on a cold sweep through
// the proxy, and it is the worst failure in the corpus: a 7,414-token Go source file came back as
// the 23 characters `# … 463 lines elided …` — the entire file gone — and it passed every check.
// Not empty, not degenerate, no keep-id dropped (the keep-list is small by design), and the
// derivation check EXCLUDES elision markers, so stripping them left an empty string and the
// function returned a perfect 1.0 for it. A result made only of markers is vacuously derived from
// anything.
//
// 0.05 is calibrated on the PRE-CHANGE production corpus: over its 62 accepted calls the reduction
// histogram had zero entries above 75% removed and the largest removal kept 9.5%. That corpus no
// longer bounds the behaviour — the first accepted call of the post-change measurement removed 87.4%
// and kept 12.6%, outside the range the floor was fitted to. 12.6% still clears 5%, but the margin
// is thinner than "rejects nothing that has ever legitimately happened" implies, so read the claim
// as "nothing in the old corpus" rather than "nothing".
//
// Re-check against the next production window rather than treating this as settled. Raise it only
// with a measurement showing real reductions being refused; lower it only if a workload of genuinely
// uniform lists is shown to need it.
const minKeepRatioFloor = 0.05

// DefaultCfg mirrors the reference prototype's ExtractCfg defaults.
func DefaultCfg() Cfg {
	return Cfg{Mode: "auto", Floor: 3000, AllowDeterministic: true, MaxChars: sampleChars,
		MinKeepRatio: minKeepRatioFloor}
}

var wsRe = regexp.MustCompile(`\s+`)

// ContentKey is a stable, marker- and whitespace-insensitive key for a body, so the
// same output re-sent on a later turn hits the extraction cache.
//
// Memoized by a fast hash of the input, like internal/tokens.Count and for the same
// reason: every offloader asks skipReduce about every candidate, so one request
// normalizes and sha256s the SAME tool output three or four times over — two regexp
// rewrites of a 50 KB body each time. Bounded the same way (cleared wholesale past cap).
func ContentKey(text string) string {
	ck := ckKey{n: len(text), h: ckHash(text)}
	ckMu.Lock()
	k, ok := ckMap[ck]
	ckMu.Unlock()
	if ok {
		return k
	}
	s := text
	if strings.Contains(s, markerOpen) {
		s = cgMarkerRe.ReplaceAllString(s, "")
	}
	s = strings.TrimSpace(wsRe.ReplaceAllString(s, " "))
	sum := sha256.Sum256([]byte(s))
	k = hex.EncodeToString(sum[:])[:24]
	ckMu.Lock()
	if len(ckMap) >= ckCacheCap {
		ckMap = make(map[ckKey]string, 1024)
	}
	ckMap[ck] = k
	ckMu.Unlock()
	return k
}

// markerOpen is the marker's fixed prefix: no marker can be present without it, so a
// plain substring check decides whether the regexp rewrite is needed at all.
const markerOpen = "<<cg:"

const ckCacheCap = 20_000

// ckKey pairs the hash with the content LENGTH. A 64-bit collision is already
// vanishingly unlikely, but unlike a mis-memoized token count a wrong ContentKey means a
// wrong stashed original on expand, so the cheapest available second opinion is worth an
// int compare: colliding contents must now also be the same size.
type ckKey struct {
	n int
	h uint64
}

var (
	ckMu   sync.Mutex
	ckSeed = maphash.MakeSeed()
	ckMap  = make(map[ckKey]string, 1024)
)

func ckHash(text string) uint64 {
	var h maphash.Hash
	h.SetSeed(ckSeed)
	h.WriteString(text)
	return h.Sum64()
}

// ResultKey is the GLOBAL cache key for a derived extraction result. Unlike a
// conversational reference (issue #27's xdedup index, which is deliberately
// session-scoped because "same as step N" only means anything in-session), an extraction
// is a CONTEXT-FREE derived result: the same bytes under the same extractor semantics
// yield the same reduction in any session. Measured on Terminal-Bench, 82 of 103 unique
// contents recurred ACROSS sessions, so a session prefix threw away ~80% of the reuse.
//
// The key must include everything that materially changes the result, or a stale entry is
// served silently — which is worse than a miss, because nothing surfaces it:
//   - contentKey: the content itself (marker/whitespace-insensitive)
//   - PromptVersion: prompt + acceptance semantics
//   - model: a different extractor model writes a different program
//   - cfgFingerprint: the config fields that steer the result (mode, rewrite, floor)
//
// A change to ANY of these misses rather than mis-serves. Changing the key schema
// invalidates existing entries exactly once — acceptable, and noted in the docs.
func ResultKey(contentKey, model string, cfg Cfg) string {
	return resultKeyWithVersion(contentKey, model, cfg, PromptVersion)
}

// resultKeyWithVersion is ResultKey with the prompt version injected, so a test can prove
// the version genuinely participates in the hash (a version that is documented but not
// hashed is the exact bug that serves stale extractions forever).
func resultKeyWithVersion(contentKey, model string, cfg Cfg, version string) string {
	h := sha256.New()
	for _, part := range []string{
		"cg:xres", keySchema, version, model, contentKey, cfgFingerprint(cfg),
	} {
		h.Write([]byte(part))
		// Length-prefixed separator: no concatenation of two parts can be mistaken for
		// another pair (e.g. ("ab","c") must not collide with ("a","bc")).
		h.Write([]byte{0})
	}
	return "cg:xres:" + hex.EncodeToString(h.Sum(nil))[:32]
}

// keySchema versions the KEY LAYOUT itself (as distinct from the prompt). Bump it if the
// set or order of key components changes.
const keySchema = "k1"

// cfgFingerprint captures the Cfg fields that can change an accepted result. Fields that
// only affect WHICH strategies are attempted but not what a given result means are still
// included — a result derived under a different strategy order is a different result.
func cfgFingerprint(cfg Cfg) string {
	allowed := append([]string(nil), cfg.AllowedStrategies...)
	sort.Strings(allowed) // order-insensitive: the same set must fingerprint the same
	// Floor is included ONLY in "auto" mode. It is now derived from context pressure, so it
	// changes as the window fills; including it unconditionally would rotate the cache key
	// mid-session and throw away most of the cross-session reuse this key exists to capture.
	// And it cannot change the result elsewhere: strategyOrder reads Floor only on the
	// "auto" branch (max(Floor*4, 8000), deciding whether "rlm" precedes "code"); in
	// code/single/rlm/deterministic modes it is unread. Include it exactly where it matters.
	floor := "-"
	if cfg.Mode == "auto" {
		floor = strconv.Itoa(cfg.Floor)
	}
	return strings.Join([]string{
		cfg.Mode,
		floor,
		strconv.FormatBool(cfg.Rewrite),
		strconv.FormatBool(cfg.AllowDeterministic),
		strconv.FormatFloat(cfg.MinKeepRatio, 'f', 4, 64),
		strconv.Itoa(cfg.MaxChars),
		strings.Join(allowed, ","),
		// Aggressiveness changes the prompt, so it MUST rotate the key: without it the
		// global result cache would serve a low-aggressiveness extraction to a request
		// that asked for high, with nothing to notice. (The level's text is also in
		// PromptVersion, which covers the case of the text itself changing; this covers
		// two levels coexisting on one deployment.)
		string(cfg.Aggressiveness),
	}, "|")
}

var identRe = regexp.MustCompile(`[A-Za-z_][\w./-]{3,}|\b\d{3,}\b`)

// looksLikeAnIdentifier separates a REFERENCE the agent made from an ordinary English word.
//
// identRe matches any word of four or more letters, so harvesting from the agent's recent
// turns — which are prose — collects "against", "including", "large", "exactly". Those then
// enter the KEEP list the model is told to preserve verbatim AND the recall check that
// refuses a reduction dropping any of them. MEASURED over 40 real captured tool outputs:
// 13 of 40 code-leg calls were refused for "dropping" a common English word, the single
// largest cause of rejection — a good compaction paid full price and was thrown away
// because it removed a noise line containing the word "large".
//
// A real reference carries a mark prose does not: a separator (_ . / -), a digit, or an
// inner capital (parse_config, src/api/users.py, test_col_insert, IndexError, sympy-1.11).
// A bare run of lowercase letters is a word, and the surrounding transcript still holds it.
func looksLikeAnIdentifier(s string) bool {
	for i, r := range s {
		switch {
		case r == '_' || r == '.' || r == '/' || r == '-':
			return true
		case r >= '0' && r <= '9':
			return true
		case i > 0 && r >= 'A' && r <= 'Z':
			return true
		}
	}
	return false
}

// HarvestIdentifiers pulls distinctive identifiers (paths, symbols, ids, numbers)
// from the agent's recent turns — the keep-set the extractor must retain.
func HarvestIdentifiers(text string, cap int) []string {
	var seen []string
	idx := map[string]struct{}{}
	for _, m := range identRe.FindAllString(text, -1) {
		// Trim trailing punctuation. identRe allows '.', '/' and '-' INSIDE an identifier so
		// that a path like src/api/users.py survives whole — but that also swallows the
		// sentence period in "fix parse_config.", and the resulting id then matches nothing in
		// the tool output. The recall check skips any id that is absent from the body, so the
		// identifier the agent actually named ended up protecting nothing: MEASURED, that is
		// how a source-file read was reduced to a single character with the check satisfied.
		m = strings.TrimRight(m, "./-")
		if len(m) < 4 || !looksLikeAnIdentifier(m) {
			continue
		}
		if _, ok := idx[m]; ok {
			continue
		}
		idx[m] = struct{}{}
		seen = append(seen, m)
		if len(seen) >= cap {
			break
		}
	}
	return seen
}

// deterministicInput is parseBody guarded by the same rule the prompt builder uses.
//
// parseBody will happily turn `     1\timport json…` — a line-numbered file read — into the
// JSON NUMBER 1, and the deterministic projection of the number 1 is the string "1". MEASURED
// live: a 3,598-token source file came back as a single character, accepted, because it was
// smaller and the keep-id that should have caught it had been harvested with a trailing period.
// buildCodeUserPart already guards against exactly this for the text it SHOWS the model
// (isJSONContainer); the deterministic strategy had no such guard, so the guard belongs here
// too rather than in one of the two callers.
func deterministicInput(body string) any {
	if !isJSONContainer(body) {
		return body // raw text: project over the text, never over a number parsed out of it
	}
	v := parseBody(body)
	if isRawString(v) {
		return body
	}
	return v
}

// parseBody returns the parsed value handed to the extractor: JSON if possible, then
// NDJSON (as a list), else the raw string.
func parseBody(text string) any {
	var v any
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	if err := dec.Decode(&v); err == nil {
		return v
	}
	if recs := parseNDJSON(text); recs != nil {
		return recs
	}
	return text
}

func parseNDJSON(text string) []any {
	var lines []string
	for _, ln := range strings.Split(text, "\n") {
		if strings.TrimSpace(ln) != "" {
			lines = append(lines, ln)
		}
	}
	if len(lines) < 2 {
		return nil
	}
	var recs []any
	for _, ln := range lines {
		var v any
		dec := json.NewDecoder(strings.NewReader(ln))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			return nil
		}
		switch v.(type) {
		case map[string]any, []any:
			recs = append(recs, v)
		default:
			return nil
		}
	}
	return recs
}

func resultToText(v any) string {
	switch v.(type) {
	case map[string]any, []any:
		// Compact, not indented: this is the reduced tool-output value, and
		// indentation whitespace inflates the BPE token count (it can exceed the
		// original), which trips the never-inflate gate and silently drops the
		// extraction. Compact JSON keeps the projection a real reduction.
		b, err := json.Marshal(v)
		if err == nil {
			return string(b)
		}
	}
	return fmt.Sprint(v)
}

func stripFences(s string) string {
	c := strings.TrimSpace(s)
	if !strings.HasPrefix(c, "```") {
		return c
	}
	lines := strings.Split(c, "\n")
	if strings.HasPrefix(lines[0], "```") {
		lines = lines[1:]
	}
	if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "```" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// --- sanity + validation gate ---

func extractionIsSane(bodyText, resultText string, keepIDs []string, minKeepRatio float64) bool {
	return insanityReason(bodyText, resultText, keepIDs, minKeepRatio) == ""
}

// insanityReason is extractionIsSane plus WHICH check refused, "" when none did.
func insanityReason(bodyText, resultText string, keepIDs []string, minKeepRatio float64) string {
	if resultText == "" {
		return "empty result"
	}
	bodyN := tokens.Count(bodyText)
	switch strings.TrimSpace(resultText) {
	case "", "[]", "{}", "null", `""`:
		if bodyN > 0 {
			return "degenerate result"
		}
	}
	// Markers are NOT kept content. derivationRatio already strips them; counting them here
	// let 21 markers plus ONE surviving line — 0.36% of a 280-line body — clear the 5% floor,
	// which is the same hole this floor exists to close, one step diluted. The two checks have
	// to agree about what a marker is, and the honest answer is "ours, not the input's".
	if kept := tokens.Count(stripElisionMarkers(resultText)); minKeepRatio > 0 &&
		float64(kept) < minKeepRatio*float64(bodyN) {
		return "below the keep-ratio floor"
	}
	for _, kid := range keepIDs {
		if len(kid) < 5 || !strings.ContainsFunc(kid, isLetter) {
			continue // too short or too numeric to be a distinctive reference
		}
		n := strings.Count(bodyText, kid)
		if n == 0 || strings.Contains(resultText, kid) {
			continue
		}
		// PERVASIVE identifiers are exempt, and this is the difference between the check
		// protecting recall and the check preventing compaction.
		//
		// The rule is "an id the agent referenced must survive verbatim", which is right for
		// a NEEDLE: a specific error code, a path mentioned once, a test name. It is wrong for
		// an id that appears on every line of the noise. MEASURED live: an access log where
		// every one of 900 routine lines carried `handler=src/api/users.py` — the very path in
		// the user's request. A reduction that keeps only the ERROR line therefore drops that
		// id and was rejected, so 3 of 6 calls paid full price for nothing BECAUSE the model
		// had compacted well; the calls that passed were the ones that kept sample noise.
		//
		// An id repeated this often is boilerplate, not a reference at risk of being lost: the
		// marker's summary names what was elided, the original is recoverable via expand, and
		// the surrounding transcript — where the id was harvested from — still contains it.
		if n > pervasiveIDOccurrences {
			continue
		}
		return "dropped a referenced identifier: " + kid
	}
	return ""
}

// pervasiveIDOccurrences is where "the agent referenced this" stops meaning "this exact
// occurrence must survive". A needle is rare by definition; twenty-plus copies in one tool
// output is structure, not a reference.
const pervasiveIDOccurrences = 20

func isLetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// validateExtraction is the acceptance gate. It returns WHY it refused, because
// "rejected by the acceptance check" covers three unrelated failures — a keep-id the
// reduction dropped, a degenerate stub, and (in rewrite mode) a result that is not
// derived from the input at all — and they have different fixes.
func validateExtraction(resultText, bodyText string, keepIDs []string, cfg Cfg) (bool, string) {
	if why := insanityReason(bodyText, resultText, keepIDs, cfg.MinKeepRatio); why != "" {
		return false, why
	}
	if cfg.Rewrite {
		// Rewrite mode drops the exact projection proof — the caller accepted a lossy
		// rewrite, and the prompt's own examples strip columns and add elision markers, so
		// a strict subsequence check would refuse the thing we asked for. What it must NOT
		// accept is a result that does not DERIVE from the input: a paraphrase, a
		// renumbered file, an invented value. Nothing checked that at all, in the shipped
		// default, and the only backstop was that the original stays recoverable via
		// context_guru_expand — at the cost of an agent turn.
		//
		// So: measure how much of the result is character-for-character traceable to the
		// input (0.2 ms) and refuse a result that mostly is not. Elision markers, which the
		// model is told to add, are excluded from the measurement rather than tolerated as
		// slack, so the floor can be strict without penalising a well-marked reduction.
		if r := derivationRatio(resultText, bodyText); r < minDerivationRatio {
			return false, fmt.Sprintf("not derived from the input (%.0f%% traceable)", 100*r)
		}
		return true, ""
	}
	if !IsContained(parseBody(resultText), parseBody(bodyText)) {
		return false, "not a subsequence of the input"
	}
	return true, ""
}

// minDerivationRatio is the floor on how much of a rewritten result must be traceable to
// the input, character for character, in order.
//
// It is a CALIBRATION KNOB, not a law: measured over 40 real captured tool outputs the
// accepted reductions sit at 100% (a filter deletes; it does not retype), while the mode
// this catches — a paraphrase or an invented value — lands far below. 0.9 leaves room for
// a rewrite that genuinely retypes a little (a collapsed count, a reflowed prefix) without
// leaving room to invent a whole record. Raise it toward 1.0 if fabrication is ever seen
// passing; lower it only with a measurement showing good reductions being refused.
const minDerivationRatio = 0.9

// derivationRatio is the fraction of the result's bytes that can be matched, IN ORDER,
// against the body — i.e. how much of the result is a character subsequence of the input,
// with partial credit. 1.0 means "obtainable by deleting characters" (the deletion-only
// guarantee); a paraphrase, a renumbered line or a fabricated record cannot match and
// drags the fraction down in proportion to how much was invented.
//
// Lines the model was ASKED to add — the elision markers naming what went — are dropped
// before the comparison, so a heavily marked reduction is not mistaken for a fabricated
// one. Single pass over both strings.
func derivationRatio(result, body string) float64 {
	var b strings.Builder
	b.Grow(len(result))
	for _, ln := range strings.Split(result, "\n") {
		if isElisionMarker(ln) {
			continue
		}
		b.WriteString(ln)
	}
	res := b.String()
	if len(res) == 0 {
		return 1 // nothing but markers: the keep-set and never-worse checks govern
	}
	// An UNMATCHED result byte must not consume the body cursor — otherwise a single
	// inserted character (a colon the model added) sends the scan to the end of the body
	// and the rest of a perfectly derived result reads as fabricated. So each byte is
	// looked for from the cursor forward, and only a hit advances it.
	//
	// ponytail: bounded lookahead + early exit keep this linear-ish. A derived byte sits
	// near its predecessor, so scanning past scanWindow bytes for one character means the
	// result was reordered, which is exactly what should not pass. Widen the window if a
	// real reduction is ever measured to be refused for reordering it did not do.
	const scanWindow = 4096
	budget := int(float64(len(res)) * (1 - minDerivationRatio)) // unmatched bytes we can afford
	matched, missed, j := 0, 0, 0
	for i := 0; i < len(res); i++ {
		k, end := j, j+scanWindow
		if end > len(body) {
			end = len(body)
		}
		for k < end && body[k] != res[i] {
			k++
		}
		if k < end {
			matched++
			j = k + 1
			continue
		}
		if missed++; missed > budget {
			break // cannot reach the floor any more; stop paying for the proof
		}
	}
	return float64(matched) / float64(len(res))
}

// stripElisionMarkers drops the marker lines, so a check asking "how much of the input survived"
// is not answered by our own annotations.
func stripElisionMarkers(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, ln := range strings.Split(s, "\n") {
		if !isElisionMarker(ln) {
			b.WriteString(ln)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// isElisionMarker reports whether a line is one of the "N lines elided" notes the prompt
// asks for (or the recovery marker itself) rather than content from the input.
func isElisionMarker(ln string) bool {
	t := strings.TrimSpace(ln)
	if t == "" {
		return true
	}
	return strings.Contains(t, "elided") || strings.Contains(t, expandToolName) || strings.Contains(t, markerOpen)
}

// expandToolName is the recovery tool the markers name. Duplicated as a plain string
// rather than imported: internal/extract must not depend on the component layer that
// registers the tool.
const expandToolName = "context_guru_expand"

// intersectAllowed filters order to the allowed set (non-empty), preserving order.
// Empty allowed means no filtering.
func intersectAllowed(order, allowed []string) []string {
	if len(allowed) == 0 {
		return order
	}
	allow := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		allow[a] = struct{}{}
	}
	out := make([]string, 0, len(order))
	for _, s := range order {
		if _, ok := allow[s]; ok {
			out = append(out, s)
		}
	}
	return out
}

// Modes are the values Cfg.Mode accepts, in the order a settings form should offer them.
// The empty string means "auto".
//
// It is a declared list because the settings form used to carry its own copy, which had
// drifted: "deterministic" was missing, so a stored `strategy: deterministic` was not
// recognised, fell back to "code", and the next save WROTE `strategy: code` over it —
// silently turning an LLM-free configuration into one that makes model calls. Anything
// offering these values reads them from here; TestModesAreTheModesRawStrategyOrderHonors
// pins the list against the switch below.
var Modes = []string{"auto", "code", "single", "rlm", "deterministic"}

func strategyOrder(tokenEst int, cfg Cfg) []string {
	return intersectAllowed(rawStrategyOrder(tokenEst, cfg), cfg.AllowedStrategies)
}

func rawStrategyOrder(tokenEst int, cfg Cfg) []string {
	switch cfg.Mode {
	case "deterministic":
		return []string{"deterministic"}
	case "single", "rlm":
		order := []string{cfg.Mode}
		if cfg.AllowDeterministic {
			order = append(order, "deterministic")
		}
		return order
	case "code":
		order := []string{"code"}
		if cfg.AllowDeterministic {
			order = append(order, "deterministic")
		}
		return order
	default: // auto
		// "code" (model-written Starlark filter over the full body) is primary for
		// mid-size bodies; "rlm" (chunked) above the floor. "single" (JSON-return) and
		// "deterministic" are ordered fallbacks behind the primary.
		floor4 := cfg.Floor * 4
		if floor4 < 8000 {
			floor4 = 8000
		}
		var order []string
		if tokenEst >= floor4 {
			order = []string{"rlm", "code", "single"}
		} else {
			order = []string{"code", "single"}
		}
		if cfg.AllowDeterministic {
			order = append(order, "deterministic")
		}
		return order
	}
}

// RunExtraction tries strategies in order, returning the first candidate that is
// strictly smaller AND passes the validation gate, else ("", "none"). Fail-open: the
// caller keeps the original on "none".
func RunExtraction(ctx context.Context, body, goal string, keepIDs []string, tokenEst int, cfg Cfg, model Model) (string, string) {
	out, _, strat := RunExtractionSummary(ctx, body, goal, keepIDs, tokenEst, cfg, model)
	return out, strat
}

// RunExtractionSummary is RunExtraction plus the one-line SUMMARY the "code"
// strategy's program optionally emitted (empty for the other strategies). The
// summary is used as the marker digest so the agent sees the gist of the elided
// output inline.
func RunExtractionSummary(ctx context.Context, body, goal string, keepIDs []string, tokenEst int, cfg Cfg, model Model) (string, string, string) {
	out, summary, strategy, _ := RunExtractionDetail(ctx, body, goal, keepIDs, tokenEst, cfg, model)
	return out, summary, strategy
}

// RunExtractionDetail is RunExtractionSummary plus WHY it failed.
//
// The three-value version reported "none" for every failure alike, and this file's own
// callers complain about it: a reply that never arrived, a program the sandbox rejected, a
// result that did not shrink and a result that failed the recall check are indistinguishable
// in the return value, and they have completely different fixes — raise the reply budget,
// fix the prompt, stop calling, loosen the keep-set. On a real session that cost four calls,
// ~$0.27 and 100 seconds to learn nothing at all, because every one of them just said "none".
//
// reason is empty on success, and a short stable slug otherwise, per strategy tried.
func RunExtractionDetail(ctx context.Context, body, goal string, keepIDs []string, tokenEst int, cfg Cfg, model Model) (result, summary, strategy, reason string) {
	base := tokens.Count(body)
	var reasons []string
	for _, name := range strategyOrder(tokenEst, cfg) {
		var cand, sum, why string
		switch name {
		case "code":
			cand, sum, why = runStarlark(ctx, body, goal, keepIDs, model, cfg.Rewrite, cfg.CacheContext, cfg.Aggressiveness)
		case "single":
			cand = runSingle(ctx, body, goal, keepIDs, model)
		case "rlm":
			cand = runRLMBatched(ctx, body, goal, keepIDs, model)
		case "deterministic":
			cand = resultToText(DeterministicProject(deterministicInput(body), keepIDs, cfg.MaxChars))
		}
		// SAY WHAT WAS DROPPED, before the size check so the markers are inside the
		// budget the never-inflate gate enforces. Only in rewrite mode: deletion-only
		// mode proves the result is a subsequence of the input, and a marker is not.
		//
		// THE COST OF THAT, stated because this file otherwise reads as if marking were
		// universal: with rewrite:false there is no marker, so a contiguous window just
		// under MaxChars is accepted with nothing showing the gap. capTruncated still
		// refuses one AT the cap and IsContained still proves the result is a real
		// subsequence, so nothing is fabricated — but the reader is not told what went.
		// Deletion-only mode trades the gap notice for the containment proof.
		if cfg.Rewrite {
			cand = markElisions(cand, body)
		}
		switch {
		case cand == "":
			// No usable candidate. `why` says WHICH of the several very different causes it
			// was — no reply, a transport error, or a program the sandbox refused (bad
			// syntax, a step/time/memory limit, a non-string OUTPUT). They have opposite
			// fixes, and reporting them alike hid a 92% syntax-rejection rate for months.
			if why == "" {
				// The program ran and assigned OUTPUT = "". Distinct from every other
				// cause: the model DID answer and the sandbox DID accept it.
				why = "program produced an empty OUTPUT"
			}
			reasons = append(reasons, name+": "+why)
			continue
		case tokens.Count(cand) >= base:
			reasons = append(reasons, name+": result not smaller")
			continue
		case capTruncated(cand, body, cfg.MaxChars):
			// A contiguous slice of the input at the character cap, with nothing saying
			// content was dropped. MEASURED in production: 25 of 62 accepted results were
			// exactly 4,000 characters with an empty summary, 15 of them a byte-for-byte
			// prefix cut ending mid-line, and they supplied 53% of all reported savings.
			// One of them turned four `ls -l` listings into two with no marker and a
			// syntactically broken final row. That is not an extraction and it must not be
			// counted as one; a windowed reduction that names its gaps is accepted above.
			reasons = append(reasons, name+": truncated at the character cap")
			continue
		case cfg.MaxChars == 0 && isLineWindow(cand, body):
			// The caller withheld the window for this content (see the component's
			// minWindowRatio), so a contiguous run of the body's lines is refused from ANY
			// strategy rather than only from the deterministic projection. `head -n` is a
			// truncation whatever produced it, and a marker makes it honest without making
			// it an extraction. FOUND LIVE: a grep result came back as its first 37 of 158
			// lines with an elision note, accepted, on content where every line is a
			// distinct fact.
			reasons = append(reasons, name+": a contiguous window is not a reduction of this content")
			continue
		}
		if ok, why := validateExtraction(cand, body, keepIDs, cfg); !ok {
			reasons = append(reasons, name+": acceptance check: "+why)
			continue
		}
		return cand, sum, name, ""
	}
	return "", "", "none", strings.Join(reasons, "; ")
}

// runSingle asks the model for the filtered subset in one call. It is a FALLBACK
// behind the primary full-body strategies: "code" (a model-written Starlark filter)
// and "rlm" (chunked) both run over the FULL body, so they never truncate. runSingle
// inlines the body into the prompt via buildPrompt, which truncates to sampleChars to
// bound prompt cost — acceptable for a fallback, since whatever the model returns is
// still containment-checked against the full body before it can be spliced in.
func runSingle(ctx context.Context, body, goal string, keepIDs []string, model Model) string {
	if model == nil {
		return ""
	}
	out, err := model.Complete(ctx, buildPrompt(body, goal, keepIDs))
	if err != nil {
		return ""
	}
	return stripFences(out)
}

const rlmChunkSize = 20
const rlmConcurrency = 6

// runRLMBatched chunks a large list body and asks the model to filter each chunk
// concurrently, then merges the kept records (order-preserving). Containment over the
// merged result is checked by the caller. Non-list bodies fall back to a single call.
func runRLMBatched(ctx context.Context, body, goal string, keepIDs []string, model Model) string {
	if model == nil {
		return ""
	}
	parsed := parseBody(body)
	list, ok := parsed.([]any)
	if !ok || len(list) <= rlmChunkSize {
		return runSingle(ctx, body, goal, keepIDs, model)
	}
	var chunks [][]any
	for i := 0; i < len(list); i += rlmChunkSize {
		end := i + rlmChunkSize
		if end > len(list) {
			end = len(list)
		}
		chunks = append(chunks, list[i:end])
	}

	results := make([][]any, len(chunks))
	sem := make(chan struct{}, rlmConcurrency)
	var wg sync.WaitGroup
	for ci, chunk := range chunks {
		wg.Add(1)
		sem <- struct{}{}
		go func(ci int, chunk []any) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() { _ = recover() }()
			chunkJSON, err := json.Marshal(chunk)
			if err != nil {
				return
			}
			out, err := model.Complete(ctx, buildPrompt(string(chunkJSON), goal, keepIDs))
			if err != nil {
				return
			}
			var kept []any
			if json.Unmarshal([]byte(stripFences(out)), &kept) == nil {
				results[ci] = kept
			}
			// On parse failure the chunk contributes nothing (safe drop; containment holds).
		}(ci, chunk)
	}
	wg.Wait()

	var merged []any
	for _, r := range results {
		merged = append(merged, r...)
	}
	if len(merged) == 0 {
		return ""
	}
	b, err := json.Marshal(merged)
	if err != nil {
		return ""
	}
	return string(b)
}
