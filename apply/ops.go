package apply

import (
	"github.com/rossoctl/context-guru/components"
	cf "github.com/rossoctl/context-guru/counterfactual"
)

// OpsFromRun turns a run's per-message changes into counterfactual operations, in pipeline
// order. wire maps a req.Input index to the wire message index (wireGroups). applied is false when
// the rewritten body was not the one forwarded; a report that ran in observe mode is DryRun too,
// whatever applied says: its operations are computed but never sent.
//
// A reverted or skipped report changed nothing upstream and reports nothing. A report that changed
// the message count has no Changes (Task 5's splice ops), and every report after it indexes a
// req.Input that no longer lines up with wire, so those changes are dropped, never guessed.
func OpsFromRun(rr components.RunReport, wire []int, applied bool) (ops []cf.Operation, dropped int) {
	return opsFromRun(rr, wire, applied, nil)
}

// opsFromRun is OpsFromRun plus keep: when non-nil, a change whose req.Input index it rejects is
// counted as dropped. The equal-count writeback passes the set of messages that really reached the
// forwarded body, so an operation never describes a rewrite that is not on the wire.
func opsFromRun(rr components.RunReport, wire []int, applied bool, keep func(idx int) bool) (ops []cf.Operation, dropped int) {
	countChanged := false
	for _, r := range rr.Components {
		if r.Skipped || r.Reverted {
			continue
		}
		for _, c := range r.Changes {
			if wire == nil || countChanged || c.Idx < 0 || c.Idx >= len(wire) || (keep != nil && !keep(c.Idx)) {
				dropped++
				continue
			}
			ops = append(ops, cf.Operation{
				Kind:       cf.KindRewrite,
				Operator:   "context-guru." + r.Component,
				DryRun:     !applied || r.Mode == components.ModeObserve,
				At:         cf.Position{Section: cf.SectionMessages, Index: wire[c.Idx]},
				SizeBefore: cf.Size{Bytes: c.BytesBefore},
				SizeAfter:  cf.Size{Bytes: c.BytesAfter},
				ChangeID:   "cg:" + r.Component + ":" + c.Hash,
			})
		}
		if r.CountChanged {
			countChanged = true
		}
	}
	return ops, dropped
}
