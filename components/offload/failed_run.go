package offload

import (
	"regexp"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
)

func init() { components.Register("failed_run", newFailedRun) }

// runMarkers identify a tool output that is a test/build run — the kind that is
// superseded when the agent re-runs after a fix.
//
// The structurally line-initial markers are ANCHORED to the start of a line (leading
// indentation allowed). They used to match anywhere in the blob, and a replay over 1,795
// real SWE-bench requests found that 9 of 81 collapses were misclassifications: a
// 22,698-byte line-numbered source read of astropy's qdp.py, a sympy source read, an
// xarray test file and a `git show` diff were each collapsed and LABELLED "superseded by
// a later failed→re-run", because the phrase `Traceback (most recent` occurred inside the
// source text and `=+ failures` matched an ordinary `= failures` assignment.
//
// The old comment said false positives "only cost an expand round-trip, never data".
// True, and it undersold the cost: the round-trip lands on the file the agent is mid-patch
// on, and the label asserts something false about the content. Anchoring kills all four
// observed shapes for free, because a line-numbered read begins every line with its number.
//
// `\d+ (passed|failed|error)` stays UNANCHORED deliberately: pytest pads its summary
// (`==== 1 failed, 40 passed in 12.31s ====`), so that one is mid-line by construction.
var runMarkers = regexp.MustCompile(`(?im)(\d+ (passed|failed|error)|^[ \t]*(BUILD (SUCCESS|FAIL)|=+ (FAILURES|test session)|Traceback \(most recent|FAILED\b|panic:|npm ERR!))`)

// failMarkers identify a run that FAILED. Only a failed earlier run is safely
// "superseded" by a later run (the agent fixed it and moved on); a PASSED/successful
// earlier run is a distinct result the agent may still reference (e.g. `pytest test_a`
// passing, then `pytest test_b`), so it is kept verbatim. Restricting collapse to
// failures is what keeps failed_run from hiding a still-relevant successful result —
// a general, agent-agnostic safety rule.
// Note: the count must be NON-ZERO ("0 failed" is a PASS, not a failure); the bare
// pytest "FAILED <path>" token is matched CASE-SENSITIVELY so a lowercase "0 failed"
// summary doesn't trip it.
// Same anchoring rule as runMarkers, for the same measured reason: `traceback (most
// recent` and `=+ failures` inside a source read must not make a file look like a failure.
var failMarkers = regexp.MustCompile(`(?im)([1-9]\d* (failed|error(s|ed)?)\b|\bexit(ed with)? (code )?[1-9]|^[ \t]*(build fail|=+ failures|traceback \(most recent|panic:|npm err!)|^[ \t]*(?-i:FAILED)\b)`)

// FailedRun collapses earlier test/build runs that a later run supersedes: only
// the most recent run-like tool output is kept in full; earlier ones become a
// pointer + stash. This is the "provable-reason" collapse — a superseded run is
// safely recoverable via expand if the agent still needs it.
type FailedRun struct {
	minTokens int
	mode      markerMode
	coldCache bool
}

type failedRunConfig struct {
	MinTokens  int    `yaml:"min_tokens"`
	MarkerMode string `yaml:"marker_mode"` // full (default) | summary | off
	// ColdCache lets a NEW collapse act at any depth on a turn whose prompt cache has
	// provably expired (see components.Ctx.TailOnlyCold). ON by default; see
	// coldCacheDefault.
	ColdCache *bool `yaml:"cold_cache"`
}

func newFailedRun(raw []byte) (components.Component, error) {
	cfg := failedRunConfig{MinTokens: 100}
	if err := components.Decode(raw, &cfg); err != nil {
		return nil, err
	}
	return &FailedRun{minTokens: cfg.MinTokens, mode: parseMarkerMode(cfg.MarkerMode),
		coldCache: coldCacheDefault(cfg.ColdCache)}, nil
}

func (FailedRun) Name() string                 { return "failed_run" }
func (FailedRun) Enabled(*components.Ctx) bool { return true }

func (fr *FailedRun) Offload(req *schemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	// Find indices of run-like tool outputs, in order.
	var runs []int
	for i := range req.Input {
		m := req.Input[i]
		if m.Role != schemas.ChatMessageRoleTool {
			continue
		}
		if !schema.Rewritable(m) {
			rep.Gate("non_text_blocks") // would be dropped by a text rewrite
			continue
		}
		content := schema.MessageText(m)
		if schema.TextTokens(content) < fr.minTokens {
			rep.Gate("below_min_tokens")
			continue
		}
		if expand.HasPlaceholder(content) {
			rep.Gate("marker_present") // already offloaded
			continue
		}
		if !runMarkers.MatchString(content) {
			rep.Gate("not_run_like")
			continue
		}
		runs = append(runs, i)
	}
	if len(runs) < 2 {
		// The component's whole premise: a LATER run supersedes an earlier one. One run
		// (or none) in a request means there is nothing to supersede.
		rep.Gate("fewer_than_two_runs")
		rep.Skipped = true
		return nil, nil
	}
	// Keep the last run in full; collapse every earlier one THAT FAILED. A passed/
	// successful earlier run is a distinct result the agent may still reference, so
	// it stays verbatim (only genuinely-superseded failures are collapsed).
	var keys []string
	changed := 0
	for _, i := range runs[:len(runs)-1] {
		m := &req.Input[i]
		content := schema.MessageText(*m)
		// Reapply a previously-frozen collapse on EVERY turn (cache-stable), regardless
		// of the tail boundary — the agent re-sends the original, so we must re-collapse
		// it to the same bytes or it reverts to full and churns the cache.
		if fk, _, ok := reapplyFrozen(c, fr.Name(), m); ok {
			changed++
			keys = append(keys, fk...)
			continue
		}
		if isKeptVerbatim(c, contentKey(content)) {
			rep.Gate("kept_verbatim_after_expand")
			continue
		}
		if !failMarkers.MatchString(content) {
			rep.Gate("earlier_run_passed") // not superseded — keep it
			continue
		}
		// A NEW collapse mutates an OLDER message (the superseded run), which on a
		// prompt-cached agent flips already-cached content full→collapsed and forces a
		// cache-write of the whole suffix — the dominant +cost we measured (121 such
		// transitions on SWE-50). So a new collapse is restricted to the UNCACHED TAIL,
		// per MESSAGE, exactly as mask does it: content the provider has not cached yet
		// can be collapsed for free, and the freeze below replays the same bytes on later
		// turns so the representation never flips at depth.
		//
		// This gate used to read `if c.CacheAware && ...` — per REQUEST rather than per
		// message. resolveCacheAware is true by default for Anthropic/Bedrock/Vertex, so
		// that form declined every new collapse at every depth and the component could
		// never act at all on the flagship workload; the escape hatch below was
		// unreachable too, because the only freeze() call is DOWNSTREAM of this gate, so
		// no first freeze could ever be established for a repair to recover.
		// TestFailedRunCacheAwareStillCollapsesTheTail is the missing half that catches it.
		//
		// The one exception is a freeze this session established and the store then LOST:
		// the provider already holds the collapsed bytes for this run, so re-deriving them
		// (deterministic) preserves its cache, while leaving the run verbatim is what
		// forces the suffix re-write.
		// cold_cache (ON by default) lifts the depth restriction on a turn whose cache has
		// provably expired. That is the ONE case where this component's own use case — fail,
		// edit for several turns, re-run — is reachable at depth for free: by the turn the
		// re-run arrives the failed run is deep in the prefix, so on a warm turn collapsing
		// it deliberately BUYS a suffix cache-write, and on a cold turn the suffix is being
		// re-written regardless.
		if !c.TailOnlyCold(i, fr.coldCache) && !repairLostFreeze(c, fr.Name(), content) {
			rep.Gate("cached_prefix")
			continue
		}
		newText, key, eff, ok := tryMark(c, fr.mode, content, " [full output: call "+expand.ToolName+"]",
			func(tok string) string { return "[superseded by a later failed→re-run] " + tok })
		if !ok {
			rep.Gate("marker_no_win") // collapse+marker wouldn't shrink this run
			continue
		}
		commitMark(c, rep, eff, key, content)
		schema.SetMessageText(m, newText)
		freeze(c, fr.Name(), content, newText) // freeze so later turns replay it (no churn)
		changed++
		if key != "" {
			keys = append(keys, key)
		}
	}
	if changed == 0 {
		rep.Skipped = true
	}
	return keys, nil
}

func init() {
	components.RegisterFields("failed_run", failedRunConfig{}, []components.Field{
		{Key: "min_tokens", Type: components.FieldInt, Default: 100, Min: 1,
			Hint: "Only collapse a superseded failed run above this many tokens."},
		markerModeField(),
		coldCacheField(),
	})
}
