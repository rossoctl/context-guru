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
// A reverted or skipped report changed nothing upstream and reports nothing. The first report that
// changed the message count carries one splice change, mapped to a MessageSplice operation when it
// covers whole wire messages (else dropped). Every report after it indexes a req.Input that no
// longer lines up with wire, so those changes are dropped, never guessed.
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
			if c.Splice != nil {
				if countChanged {
					dropped++
					continue
				}
				op, ok := spliceOp(r, c, wire, applied)
				if !ok {
					dropped++
					continue
				}
				ops = append(ops, op)
				continue
			}
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

// spliceOp maps a run's splice, in req.Input (slot) space, to a MessageSplice operation in wire
// space. It reports false when the range does not cover whole wire messages: several slots can
// share one wire message, and a splice that cuts one in two cannot be expressed in messages.
func spliceOp(r components.Report, c components.Change, wire []int, applied bool) (cf.Operation, bool) {
	sp := c.Splice
	n := len(wire)
	if wire == nil || sp.At < 0 || sp.Removed < 0 || sp.Inserted < 0 || sp.At+sp.Removed > n {
		return cf.Operation{}, false
	}
	// A boundary is clean when the slots on either side of it belong to different wire messages.
	// The range is [At, At+Removed); an insertion (Removed == 0) is the empty range at At.
	if sp.At > 0 && sp.At < n && wire[sp.At-1] == wire[sp.At] {
		return cf.Operation{}, false
	}
	end := sp.At + sp.Removed
	if sp.Removed > 0 && end < n && wire[end-1] == wire[end] {
		return cf.Operation{}, false
	}
	if !denseAcross(wire, sp.At, end) {
		return cf.Operation{}, false
	}
	var at, removed int
	switch {
	case sp.Removed > 0:
		at = wire[sp.At]
		removed = wire[end-1] - wire[sp.At] + 1
	case sp.At < n:
		at = wire[sp.At]
	default:
		// An append: one past the last wire message, not len(slots).
		at = 0
		if n > 0 {
			at = wire[n-1] + 1
		}
	}
	op := cf.Operation{
		Kind:          cf.KindRewrite,
		Operator:      "context-guru." + r.Component,
		DryRun:        !applied || r.Mode == components.ModeObserve,
		At:            cf.Position{Section: cf.SectionMessages, Index: at},
		SizeBefore:    cf.Size{Bytes: c.BytesBefore},
		SizeAfter:     cf.Size{Bytes: c.BytesAfter},
		ChangeID:      "cg:" + r.Component + ":" + c.Hash,
		MessageSplice: &cf.MessageSplice{At: at, Removed: removed, Inserted: sp.Inserted},
	}
	return op, true
}

// denseAcross reports whether wire has no hole over the slots [at, end) and the pairs bordering
// that range. normalize skips a message it cannot parse and gives it no slot, so wire can read
// {0, 3, 3, 4}: wire messages 1 and 2 are in the body but not in req.Input. A splice that crosses
// or touches such a hole would count wire messages the component never held, and the position of
// what it inserted would be ambiguous, so the caller drops it. Every consecutive pair wire[k-1],
// wire[k] for k in [max(at,1), min(end,len-1)] must differ by 0 or 1. An append at the end has no
// pair to check, so a trailing unparseable message cannot be detected here (known limit).
func denseAcross(wire []int, at, end int) bool {
	lo := at
	if lo < 1 {
		lo = 1
	}
	hi := end
	if hi > len(wire)-1 {
		hi = len(wire) - 1
	}
	for k := lo; k <= hi; k++ {
		if d := wire[k] - wire[k-1]; d < 0 || d > 1 {
			return false
		}
	}
	return true
}
