package offload

import (
	"context"
	"fmt"
	"strings"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/coref"
	"github.com/rossoctl/context-guru/internal/extract"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/schema"
)

// THE MERGED DESIGN: one model call that carries the co-reference criterion, replacing the
// per-output trim loop.
//
// The idea, in the form it was originally put: once you are already paying for an LLM call, that
// call can also decide what has been referenced and is spent — so the backward-looking index and the
// forward-looking model stop being two passes. The value would be in the cases an exact matcher
// structurally cannot judge: Tier-2/3 reuse (a value transformed before being restated leaves no
// substring) and the anchor-vs-payload ambiguity (was the reference a pointer, or the payload
// itself?).
//
// WHAT THE EVIDENCE ALREADY SAYS, so this is not built naively:
//
//   * The PER-OUTPUT form of this is REFUTED — 6% live-kept, inside the null model's error bar
//     (docs/results/coref-selection-experiment.md finding 1). Shown one output at a time, a model
//     just drops it. So this implementation is BULK: all candidates in one call, judged
//     comparatively, which measured 58%.
//   * The deterministic index still beat every model arm (95% live-kept at 11% false-drop, free),
//     and neither floor-symmetry nor a widened Tier-2 ground truth closed the gap
//     (docs/experiments/loca/iter009/results.md). So the prior here is NEGATIVE.
//   * Those experiments measured decision quality on captured traffic and explicitly could not speak
//     to reward. Reward is the one axis left, and the only reason to build this.
//
// Opt-in via `selection_mode: merged`. Default stays the per-output trim loop.
//
// The integration point is deliberately narrow: this fills the SAME projected/summary slots the
// parallel per-output loop fills, so freezing, marker creation, the store, the never-worse check and
// every counter downstream are untouched and shared. Only the decision changes, not the mechanics.

// mergedSampleChars bounds each output shown in the prompt. The whole point is comparative
// judgement across many outputs, so the per-output budget must stay small enough that ~15 of them
// plus the contract still fit the extraction model's window.
const mergedSampleChars = 4000

// mergedMaxItems caps one adjudication. ~15 is the size the bulk arm was measured at.
const mergedMaxItems = 15

// renderEvidence formats one output's co-reference record for the prompt. Counts only — no
// identifier lists — because the measured win came from comparative ranking, not from more detail,
// and every extra token here is paid on every candidate.
func renderEvidence(r *coref.Record, laterTurns int) string {
	if r == nil {
		// No record: the output was below the index's size floor, so the index has no opinion. Say
		// so plainly rather than emitting zeros, which would read as "nothing referenced it".
		return fmt.Sprintf("no index record (below size floor); later_turns=%d", laterTurns)
	}
	age := "never"
	if r.RefAge >= 0 {
		age = fmt.Sprintf("%d messages ago", r.RefAge)
	}
	return fmt.Sprintf("novel=%d refs=%d ref_age=%s used_frac=%.2f later_turns=%d verdict_of_index=%s",
		r.Novel, r.Refs, age, r.UsedFrac, r.LaterTurns,
		coref.Classify(*r, corefClosedDistDefault, corefOpenRepsDefault, corefMinLaterDefault))
}

// mergedInput is one candidate offered for adjudication. A package-level type because
// extract_llm.go's `cand` is function-local, and coupling to it would drag this whole decision path
// back inside Offload.
type mergedInput struct {
	Idx     int
	Content string
	ID      string
}

// mergedDecision is what phase 3 needs: the projection to splice and its summary segment. Keyed by
// message index on return so the caller maps it back without either side knowing the other's types.
type mergedDecision struct {
	Projected string
	Summary   string
}

// adjudicateMerged runs ONE model call over ALL candidates and returns the decisions keyed by
// message index. Any failure returns nil, which the caller treats as "change nothing" — the
// fail-open direction the whole pipeline uses.
func (e *ExtractLLM) adjudicateMerged(
	req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx,
	cands []mergedInput, goal string, model components.Model,
) map[int]mergedDecision {
	if model == nil || len(cands) == 0 {
		return nil
	}
	if len(cands) > mergedMaxItems {
		cands = cands[:mergedMaxItems]
	}

	// The index's own measurements, keyed by message index, so the model sees what the
	// deterministic pass concluded and can veto it rather than duplicate it.
	recs := map[int]*coref.Record{}
	for _, r := range coref.Index(flattenForCoref(req), e.minTokens, schema.TextTokens) {
		rr := r
		recs[r.Idx] = &rr
	}
	laterTurns := func(i int) int {
		n := 0
		for j := i + 1; j < len(req.Input); j++ {
			if req.Input[j].Role == bschemas.ChatMessageRoleAssistant {
				n++
			}
		}
		return n
	}

	items := make([]extract.BulkItem, 0, len(cands))
	for _, cd := range cands {
		items = append(items, extract.BulkItem{
			Index:      cd.Idx,
			ID:         cd.ID,
			SizeTokens: schema.TextTokens(cd.Content),
			Evidence:   renderEvidence(recs[cd.Idx], laterTurns(cd.Idx)),
			Sample:     truncateForPrompt(cd.Content, mergedSampleChars),
		})
	}

	ctx, cancel := context.WithTimeout(c.Ctx, llmCallTimeout)
	defer cancel()
	start := time.Now()
	reply, err := model.Complete(ctx, extract.BuildBulkPrompt(goal, items))
	metrics.RecordExtractionCall(float64(time.Since(start).Milliseconds()))
	if err != nil {
		rep.Gate("merged_call_failed")
		return nil
	}
	verdicts := extract.ParseBulkVerdicts(reply)
	if len(verdicts) == 0 {
		rep.Gate("merged_unparseable")
		return nil
	}

	byIdx := map[int]int{} // message index -> position in cands
	for k := range cands {
		byIdx[cands[k].Idx] = k
	}
	dec := map[int]mergedDecision{}
	for _, v := range verdicts {
		k, ok := byIdx[v.Index]
		if !ok {
			continue // a verdict for something we did not offer
		}
		content := cands[k].Content
		before := schema.TextTokens(content)
		var projected, summary string
		switch v.Verdict {
		case "drop":
			// The residue is the SHAPE descriptor, not a head peek: for a record set the first
			// rows say nothing about whether the field you want is in there. See corefstub.go.
			projected, summary = mergedResidue(content), "adjudicated spent"
			rep.Gate("merged_drop")
		case "trim":
			kept := strings.TrimSpace(v.Kept)
			// CONTAINMENT. A trim may only return text that was actually shown; anything else is
			// the model having written prose where it was asked to copy records. Reject rather
			// than splice an invention.
			if kept == "" || !strings.Contains(content, kept) {
				rep.Gate("merged_trim_not_contained")
				continue
			}
			projected = kept
			rep.Gate("merged_trim")
		default:
			rep.Gate("merged_keep")
			continue
		}
		if projected == "" {
			continue
		}
		after := schema.TextTokens(projected)
		if after >= before { // never-worse; phase 3 would discard it anyway
			continue
		}
		// Feed the observed ratio so the economic gate prices FUTURE calls on what this workload
		// actually achieves. The per-output loop does the same; without it the merged arm would be
		// gated on the other shape's history.
		e.ratios.observe(before-after, before)
		metrics.RecordExtractionSaving(before - after)
		dec[v.Index] = mergedDecision{projected, summary}
	}
	return dec
}

// mergedResidue is what a dropped output leaves behind.
//
// corefStub gives the SHAPE for structured content ("200 records, fields: name/id/address"), which
// corefstub.go argues is the right residue for a record set: its first rows say nothing about
// whether the field you want is in there. But it returns "" for anything that is not a JSON array
// or object — and that is most real tool output: logs, file reads, tracebacks. Measured: "" for
// newline-delimited JSON and for plain text.
//
// Without a fallback the merged arm would silently fail to act on those: the drop verdict is
// recorded in the gate counters while the projection comes back empty and phase 3 skips it, so the
// arm would look like it was deciding and removing nothing. That is exactly the "acted: 0 is not
// diagnosable" failure the Report.Gates comment warns about, one layer further in.
//
// For unstructured content a HEAD PEEK is the right residue, by corefstub.go's own reasoning
// applied in the other direction: the head identifies the whole for a file read or a traceback, and
// is only misleading for record sets — which corefStub already covers.
func mergedResidue(content string) string {
	if s := corefStub(content); s != "" {
		return s
	}
	head := strings.TrimSpace(content)
	if len(head) > 96 {
		head = head[:96]
	}
	return fmt.Sprintf("%s… [%d chars omitted]", head, len(content)-len(head))
}

// truncateForPrompt bounds one output shown in the bulk prompt, marking the cut so the model knows
// it is judging an excerpt and must not "complete" the missing part in a trim.
func truncateForPrompt(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n…[excerpt truncated for adjudication; a trim must copy only text shown above]"
}
