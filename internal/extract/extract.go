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
	Mode               string  // auto | single | rlm | deterministic
	Floor              int     // token floor; rlm kicks in at max(floor*4, 8000) in auto
	MinKeepRatio       float64 // 0 disables the blunt ratio backstop (keep-set check governs)
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
	// Aggressiveness selects the compaction target taught in the second system block
	// (low | medium | high; empty = medium). It changes what the model is ASKED for, never
	// what is ACCEPTED — the verbatim-preservation, strictly-smaller and (in deletion-only
	// mode) subsequence checks are identical at every level.
	Aggressiveness Aggressiveness
}

// DefaultCfg mirrors the reference prototype's ExtractCfg defaults.
func DefaultCfg() Cfg {
	return Cfg{Mode: "auto", Floor: 3000, AllowDeterministic: true, MaxChars: sampleChars}
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

// HarvestIdentifiers pulls distinctive identifiers (paths, symbols, ids, numbers)
// from the agent's recent turns — the keep-set the extractor must retain.
func HarvestIdentifiers(text string, cap int) []string {
	var seen []string
	idx := map[string]struct{}{}
	for _, m := range identRe.FindAllString(text, -1) {
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
	if resultText == "" {
		return false
	}
	bodyN, resN := tokens.Count(bodyText), tokens.Count(resultText)
	switch strings.TrimSpace(resultText) {
	case "", "[]", "{}", "null", `""`:
		if bodyN > 0 {
			return false
		}
	}
	if minKeepRatio > 0 && float64(resN) < minKeepRatio*float64(bodyN) {
		return false
	}
	for _, kid := range keepIDs {
		if len(kid) >= 5 && strings.ContainsFunc(kid, isLetter) &&
			strings.Contains(bodyText, kid) && !strings.Contains(resultText, kid) {
			return false
		}
	}
	return true
}

func isLetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func validateExtraction(resultText, bodyText string, keepIDs []string, cfg Cfg) bool {
	if !extractionIsSane(bodyText, resultText, keepIDs, cfg.MinKeepRatio) {
		return false
	}
	// Rewrite mode deliberately drops the lossless-projection proof (the caller
	// accepted a lossy rewrite). Sanity + strictly-smaller (checked by the caller)
	// still apply.
	if cfg.Rewrite {
		return true
	}
	return IsContained(parseBody(resultText), parseBody(bodyText))
}

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
	base := tokens.Count(body)
	for _, name := range strategyOrder(tokenEst, cfg) {
		var cand, summary string
		switch name {
		case "code":
			cand, summary = runStarlark(ctx, body, goal, keepIDs, model, cfg.Rewrite, cfg.Aggressiveness)
		case "single":
			cand = runSingle(ctx, body, goal, keepIDs, model)
		case "rlm":
			cand = runRLMBatched(ctx, body, goal, keepIDs, model)
		case "deterministic":
			cand = resultToText(DeterministicProject(parseBody(body), keepIDs, cfg.MaxChars))
		}
		if cand == "" || tokens.Count(cand) >= base {
			continue
		}
		if validateExtraction(cand, body, keepIDs, cfg) {
			return cand, summary, name
		}
	}
	return "", "", "none"
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
