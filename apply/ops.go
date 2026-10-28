package apply

import (
	"strconv"

	"github.com/rossoctl/context-guru/components"
	cf "github.com/rossoctl/context-guru/counterfactual"
)

// changeID is the counterfactual ChangeID of one change: cg:<component>:<idx>:<hash>. idx is the
// req.Input index (the slot, the start of a splice), so two identical tool outputs that one
// component rewrites in one request get two ids (unique_change would fire on one id). The index is
// stable across the turns of a transcript that grows by appending, which is the only case in which
// a change stays active: a client edit or compaction at or before the position ends the change in
// the library anyway (doc.go decision 4). The slot index and not the wire index, because several
// slots can share one wire message (an Anthropic user message with several tool_result blocks).
// The hash is of the text as the client sent it (components.changesOf).
func changeID(component string, c components.Change) string {
	return "cg:" + component + ":" + strconv.Itoa(c.Idx) + ":" + c.Hash
}

// OpsFromRun turns a run's per-message changes into counterfactual operations, in pipeline
// order. The sizes are the components' TEXT sizes; the caller must convert them to wire bytes
// (wireSizes) before the operations reach the library. wire maps a req.Input index to the wire message index (wireGroups). applied is false when
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
				ChangeID:   changeID(r.Component, c),
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
		ChangeID:      changeID(r.Component, c),
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

// wireSizes replaces the text sizes OpsFromRun puts on ops with WIRE-byte sizes: the bytes of the
// JSON message elements, which is what the host measures the bodies in (proxy cfBodies). The
// library's sum check compares the two, so text sizes fail it on every request (escaping, role and
// ids are in the wire bytes and not in the text).
//
// inLen(i) is the length of wire message i of the body the pipeline read, outLen(i) the length of
// wire message i of the body forwarded. inTotal and outTotal are the bytes of all messages of each.
//
// Per wire message, the first operation's SizeBefore is the message before and the last one's
// SizeAfter the message after; with several operations on one message (several components, or
// several slots), the boundaries between them split the wire delta in proportion to each
// operation's text delta, so the chain is contiguous and its total exact. A splice's SizeBefore is
// the bytes of the wire messages it removed, its SizeAfter the bytes of those it inserted. An
// operation on a message a later splice removed keeps an approximate size (its text delta): the
// library ends it, because the splice's SizeBefore then equals the client bytes of the range.
//
// It reports false when the sizes do not reconcile: the operations' net change must equal
// inTotal - outTotal. A message that changed with no operation (a metadata-only write, a cache
// anchor, a rewritten survivor of a splice) makes it false, and the caller then drops every
// operation, so the event falls back to the legacy figures rather than publish a figure the bytes
// do not support.
func wireSizes(ops []cf.Operation, inLen, outLen func(i int) (int, bool), inTotal, outTotal int) bool {
	var sp *cf.MessageSplice
	for _, o := range ops {
		if o.MessageSplice != nil {
			sp = o.MessageSplice
		}
	}
	outIdx := func(i int) int {
		switch {
		case sp == nil || i < sp.At:
			return i
		case i >= sp.At+sp.Removed:
			return i - sp.Removed + sp.Inserted
		}
		return -1 // removed by the splice
	}
	net := 0
	groups := map[int][]int{} // wire index -> op indices, in pipeline order
	var order []int
	for k := range ops {
		o := &ops[k]
		if o.MessageSplice != nil {
			b, a := 0, 0
			for i := o.MessageSplice.At; i < o.MessageSplice.At+o.MessageSplice.Removed; i++ {
				n, ok := inLen(i)
				if !ok {
					return false
				}
				b += n
			}
			for i := o.MessageSplice.At; i < o.MessageSplice.At+o.MessageSplice.Inserted; i++ {
				n, ok := outLen(i)
				if !ok {
					return false
				}
				a += n
			}
			o.SizeBefore, o.SizeAfter = cf.Size{Bytes: b}, cf.Size{Bytes: a}
			net += b - a
			continue
		}
		if _, seen := groups[o.At.Index]; !seen {
			order = append(order, o.At.Index)
		}
		groups[o.At.Index] = append(groups[o.At.Index], k)
	}
	for _, wi := range order {
		ks := groups[wi]
		b, ok := inLen(wi)
		if !ok {
			return false
		}
		oi := outIdx(wi)
		if oi < 0 {
			// Removed by the later splice: the library ends these operations (see above).
			cur := b
			for _, k := range ks {
				d := ops[k].SizeBefore.Bytes - ops[k].SizeAfter.Bytes
				next := max(cur-d, 0)
				ops[k].SizeBefore, ops[k].SizeAfter = cf.Size{Bytes: cur}, cf.Size{Bytes: next}
				cur = next
			}
			continue
		}
		a, ok := outLen(oi)
		if !ok {
			return false
		}
		chain(ops, ks, b, a)
		net += b - a
	}
	return net == inTotal-outTotal
}

// chain gives the operations ks, all on one wire message of b bytes before and a bytes after,
// contiguous wire sizes from b to a, splitting b - a in proportion to their text deltas. With no
// text delta at all, the whole wire delta goes to the last operation.
func chain(ops []cf.Operation, ks []int, b, a int) {
	t := 0
	for _, k := range ks {
		t += ops[k].SizeBefore.Bytes - ops[k].SizeAfter.Bytes
	}
	w, cum, cur := b-a, 0, b
	for n, k := range ks {
		next := a
		if n < len(ks)-1 {
			next = b
			if t != 0 {
				cum += ops[k].SizeBefore.Bytes - ops[k].SizeAfter.Bytes
				next = b - int(int64(w)*int64(cum)/int64(t))
			}
			next = max(next, 0)
		}
		ops[k].SizeBefore, ops[k].SizeAfter = cf.Size{Bytes: cur}, cf.Size{Bytes: next}
		cur = next
	}
}
