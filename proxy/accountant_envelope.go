package proxy

import (
	"hash/fnv"
	"strconv"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/adjudicate"
)

// The envelope is the part of a request no pipeline component sees: the tools array, the system
// array, and the cache marks. The proxy changes it in several places (apply's toolschema,
// toolfilter, cachesplit, headttl and tail mark; expand.Inject and adjudicate.Inject after the
// pipeline), and none of them reports an operation. The library's sum check then fails on the
// tools and system sections, and the event books no saving.
//
// envelopeOps reports each change from the two bodies instead: the client body and the body that
// went upstream. A reporter that diffs bodies cannot miss a rewrite that forgot to report itself,
// which is why it is one function here and not a report in each of the seven places.
//
// The forms are the shared cases' (accountant test/cases):
//   - a declaration the forwarded body added is a rewrite at its forwarded index, SizeBefore 0
//     (rewrite/expand-first-injection/break-once/5m);
//   - declarations it removed are one rewrite with a span for each, at the client indexes, SizeAfter
//     0 (rewrite/four-operations/hot/5m);
//   - a declaration both bodies carry with different bytes is a rewrite in place
//     (rewrite/toolschema-mid-session/break-then-saving/5m);
//   - a system block split in two is one rewrite on the client block, its SizeAfter the bytes of
//     both halves (split/delivered/stable-half-read), with a cg:cachesplit: ChangeID, the prefix
//     the library recognises a split by;
//   - a system block whose bytes changed in place (a ttl label, a moved mark) is a rewrite in place.
//
// Every ChangeID is derived from what changed, never from the turn, so the same change gets the
// same ChangeID on every request and the library's "a reported operation replaces the active one
// with its ChangeID" keeps it applied once.

// envelopeOwner names the operator of a tools change. expand and adjudicate inject a declaration
// under a name only they use; every other tools change is the envelope rewrite of apply, which the
// pipeline's own report attributes to toolschema (a rewrite in place) or toolfilter (a removal).
func envelopeOwner(name string, added, removed bool) string {
	switch {
	case added && name == expand.ToolName:
		return "expand"
	case added && name == adjudicate.ToolName:
		return "adjudicate"
	case removed:
		return "toolfilter"
	default:
		return "toolschema"
	}
}

// envelopeOps lists the operations that explain the tools and system sections' difference between
// the client body and the forwarded body. It returns nil when both bodies have no messages array
// (accBodies gives an empty Body for them).
func envelopeOps(client, forwarded []byte) []accountant.Operation {
	if !gjson.GetBytes(client, "messages").IsArray() || !gjson.GetBytes(forwarded, "messages").IsArray() {
		return nil
	}
	ops := toolsOps(gjson.GetBytes(client, "tools").Array(), gjson.GetBytes(forwarded, "tools").Array())
	return append(ops, systemOps(systemBlocks(client), systemBlocks(forwarded))...)
}

// toolsOps matches declarations by name. An unnamed declaration (an Anthropic server tool carries a
// type, and a name too, but a malformed one may not) is matched by its position in the unnamed ones.
// A name that repeats is matched by its occurrence: the second "x" of the client body is the second
// "x" of the forwarded body, so an append or an in-place rewrite keeps matching the bytes. A removal
// among repeated names cannot be placed (the declarations are indistinguishable, so which one went is
// unknown), and the reporter returns nil: the sum check then sends the event to the no-saving
// fallback, as for any change it cannot explain.
func toolsOps(cl, fw []gjson.Result) []accountant.Operation {
	repeated := false
	key := func(arr []gjson.Result) ([]string, map[string]int) {
		keys, at := make([]string, len(arr)), map[string]int{}
		seen := map[string]int{}
		for i, d := range arr {
			base := d.Get("name").String()
			if base == "" {
				base = "\x00" // unnamed declarations share one counter
			}
			// The first occurrence keeps the bare name, so the owner check and the ChangeID hash see
			// the name as before; a repeat gets its occurrence number.
			k := base
			if n := seen[base]; n > 0 {
				k = base + "\x00" + strconv.Itoa(n)
			}
			seen[base]++
			repeated = repeated || seen[base] > 1
			keys[i] = k
			at[k] = i
		}
		return keys, at
	}
	ck, cat := key(cl)
	fk, fat := key(fw)
	var ops []accountant.Operation
	var removed []accountant.Span
	removedBytes, removedHash := 0, fnv.New64a()
	for i, k := range ck {
		if _, ok := fat[k]; ok {
			continue
		}
		n := len(cl[i].Raw)
		removed = append(removed, accountant.Span{Section: accountant.SectionTools, From: i, To: i,
			SizeBefore: accountant.Size{Bytes: n}, SizeAfter: accountant.Size{Bytes: 0}})
		removedBytes += n
		removedHash.Write([]byte(k))
		removedHash.Write([]byte{0})
	}
	if len(removed) > 0 && repeated {
		return nil
	}
	if len(removed) > 0 {
		op := envelopeOp("toolfilter", accountant.Position{Section: accountant.SectionTools, Index: removed[0].From},
			removedBytes, 0, "decl-removed:"+accHex(removedHash.Sum64()))
		if len(removed) > 1 {
			op.Spans = removed
		}
		ops = append(ops, op)
	}
	for i, k := range fk {
		j, inClient := cat[k]
		switch {
		case !inClient:
			ops = append(ops, envelopeOp(envelopeOwner(k, true, false),
				accountant.Position{Section: accountant.SectionTools, Index: i}, 0, len(fw[i].Raw), "decl-added:"+hashOf(k)))
		case cl[j].Raw != fw[i].Raw:
			at := accountant.Position{Section: accountant.SectionTools, Index: j}
			// apply rewrites the declaration first and relabels its mark to 1h last, so the rewrite ends
			// at the forwarded bytes without the ttl. Reported as one operation, the relabel would carry
			// no headttl ChangeID and the library would not book its premium.
			if mid, ok := withoutHeadTTL(cl[j].Raw, fw[i].Raw); ok {
				if mid != cl[j].Raw {
					ops = append(ops, envelopeOp(envelopeOwner(k, false, false), at, len(cl[j].Raw), len(mid),
						"decl-rewritten:"+hashOf(k)))
				}
				ops = append(ops, envelopeOpID("headttl", at, len(mid), len(fw[i].Raw), "headttl:"+hashOf(k)))
				continue
			}
			ops = append(ops, envelopeOp(envelopeOwner(k, false, false), at, len(cl[j].Raw), len(fw[i].Raw),
				"decl-rewritten:"+hashOf(k)))
		}
	}
	return ops
}

// systemBlocks is the system section as accBodies hashes it: a string system prompt is one block.
func systemBlocks(raw []byte) []string {
	s := gjson.GetBytes(raw, "system")
	switch {
	case s.IsArray():
		var out []string
		s.ForEach(func(_, v gjson.Result) bool { out = append(out, v.Raw); return true })
		return out
	case s.Exists():
		return []string{s.Raw}
	}
	return nil
}

// systemOps explains the system section. The forwarded body has the client's blocks, one of them
// possibly split in two (cachesplit), and any block possibly relabelled in place (headttl). The
// split is found as the one client block whose text equals the text of two adjacent forwarded
// blocks; nothing else in the proxy changes the system block count.
func systemOps(cl, fw []string) []accountant.Operation {
	var ops []accountant.Operation
	split := -1
	if len(fw) == len(cl)+1 {
		for i := range cl {
			if i+1 < len(fw) && textOf(cl[i]) != "" && textOf(cl[i]) == textOf(fw[i])+textOf(fw[i+1]) {
				split = i
				break
			}
		}
		if split < 0 {
			return nil // a count change this reporter cannot explain: the sum check says so
		}
	} else if len(fw) != len(cl) {
		return nil
	}
	for i, j := 0, 0; i < len(cl); i, j = i+1, j+1 {
		pos := accountant.Position{Section: accountant.SectionSystem, Index: i}
		if i == split {
			// apply relabels the head to 1h before it splits the block, so the split starts from the
			// relabelled bytes. The relabel is its own operation: the library books the 1h premium
			// only through a headttl: ChangeID.
			before := cl[i]
			if relabelled := withHeadTTL(cl[i], fw[j]); relabelled != cl[i] {
				ops = append(ops, envelopeOpID("headttl", pos, len(cl[i]), len(relabelled), "headttl:"+hashOf(textOf(cl[i]))))
				before = relabelled
			}
			ops = append(ops, envelopeOpID("cachesplit", pos, len(before), len(fw[j])+len(fw[j+1]),
				"cg:cachesplit:"+hashOf(textOf(cl[i]))))
			j++
			continue
		}
		if cl[i] != fw[j] {
			// The library finds the head by the prefix "headttl:" (design §4.4), not "cg:". Its state is per
			// thread, so the thread part of the spelled-out form is not needed to keep two heads apart.
			ops = append(ops, envelopeOpID("headttl", pos, len(cl[i]), len(fw[j]), "headttl:"+hashOf(textOf(cl[i]))))
		}
	}
	return ops
}

// withHeadTTL is client with its cache mark relabelled to 1h, when forwarded carries a 1h mark and the
// client's did not: the bytes upgradeHeadTTL leaves a block before anything else rewrites it. It is
// client itself when there is no relabel to explain.
//
// It relies on upgradeHeadTTL (apply/headttl.go) only ever relabelling a mark the client already
// sent. If that function learns to ADD a mark where the client had none, headTTLRelabelled is false
// for it, the relabel is absorbed into the split or the rewrite again, and the library never books
// the 1h premium. Change both together.
func withHeadTTL(client, forwarded string) string {
	if !headTTLRelabelled(client, forwarded) {
		return client
	}
	out, err := sjson.Set(client, "cache_control.ttl", "1h")
	if err != nil {
		return client
	}
	return out
}

// withoutHeadTTL splits a tools declaration's change at the relabel: mid is the forwarded declaration
// without the 1h label, the bytes the declaration's own rewrite produced. ok is false when the
// forwarded declaration carries no relabel. When relabelling the client's declaration gives the
// forwarded one exactly, mid is the client's, and the change is the relabel alone.
func withoutHeadTTL(client, forwarded string) (mid string, ok bool) {
	if !headTTLRelabelled(client, forwarded) {
		return "", false
	}
	if withHeadTTL(client, forwarded) == forwarded {
		return client, true
	}
	mid, err := sjson.Delete(forwarded, "cache_control.ttl")
	if err != nil {
		return "", false
	}
	return mid, true
}

// headTTLRelabelled is true when the forwarded mark asks for 1h and the client's did not.
func headTTLRelabelled(client, forwarded string) bool {
	cc := gjson.Get(client, "cache_control")
	return cc.IsObject() && cc.Get("ttl").String() != "1h" && gjson.Get(forwarded, "cache_control.ttl").String() == "1h"
}

func textOf(raw string) string {
	r := gjson.Parse(raw)
	if r.Type == gjson.String {
		return r.String()
	}
	return r.Get("text").String()
}

// envelopeOp is one rewrite with the ChangeID cg:<component>:<key>.
func envelopeOp(component string, at accountant.Position, before, after int, key string) accountant.Operation {
	return envelopeOpID(component, at, before, after, "cg:"+component+":"+key)
}

func envelopeOpID(component string, at accountant.Position, before, after int, changeID string) accountant.Operation {
	return accountant.Operation{
		Kind:       accountant.KindRewrite,
		Operator:   "context-guru." + component,
		At:         at,
		SizeBefore: accountant.Size{Bytes: before},
		SizeAfter:  accountant.Size{Bytes: after},
		ChangeID:   changeID,
	}
}

func hashOf(s string) string {
	h := fnv.New64a()
	h.Write([]byte(s))
	return accHex(h.Sum64())
}

func accHex(v uint64) string { return strconv.FormatUint(v, 16) }
