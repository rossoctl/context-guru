package accrec

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/accwire"
	"github.com/rossoctl/context-guru/tooldefer"
)

// The prompt the provider bills (accountant R56).
//
// Tool deferral sets defer_loading on a tool and leaves the definition in the bytes the proxy sends. The provider
// keeps that tool out of the prompt and bills nothing for the definition, so the forwarded body is larger than the
// prompt that was billed. The library reads body bytes, so it would see no shrink, and the sum check could not tie
// the saving to the bodies.
//
// BilledBody is the forwarded body with each definition the proxy itself deferred cut to a stub, a bare
// {"name":...}. A tool the client deferred keeps its definition. The accountant gets this copy. Nothing else does:
// the bytes sent to the provider, the cache breakpoints and the ping facts all come from the real body. Deferral is
// then an ordinary shrink of `tools` positions, reported as one operation for each tool.

// DeferredStub is what the accountant's copy of a deferred definition keeps. The provider bills no definition, so the
// stub is as small as a tool can be and still be a tool. Our own tokenizer does not enter here: the library converts
// the bytes with the request's own ratio, billed tokens over body bytes.
func DeferredStub(name string) string {
	return `{"name":` + jsonString(name) + `,"defer_loading":true}`
}

func jsonString(s string) string {
	b, _ := sjson.SetBytes([]byte(`{}`), "v", s)
	return gjson.GetBytes(b, "v").Raw
}

// DeferredNames is the set of names of the definitions the body defers.
func DeferredNames(body []byte) map[string]bool {
	out := map[string]bool{}
	for _, t := range gjson.GetBytes(body, "tools").Array() {
		if n := t.Get("name").String(); n != "" && t.Get("defer_loading").Bool() {
			out[n] = true
		}
	}
	return out
}

// OwnDeferred is the names the PROXY deferred on this request: deferred in the forwarded body, and not already
// deferred in the body deferral read. A tool the client deferred itself (Claude Code does) is free in the baseline
// too, because the provider bills no definition for it whoever set the mark, so it is no saving of ours and it keeps
// its definition, with its cache_control, in the accountant's copy.
func OwnDeferred(forwarded, preDefer []byte) map[string]bool {
	out := DeferredNames(forwarded)
	for n := range DeferredNames(preDefer) {
		delete(out, n)
	}
	return out
}

// BilledBody returns the forwarded body as the provider bills it, with the definitions in own cut to stubs. ok=false
// means the accountant should not be given a stub-sized copy: nothing in own is in the body, or a stub would not be
// smaller than the definition it replaces. A stub that grows the tool is not a deferral the provider billed less for,
// and an operation that reports it as a shrink would book a saving nothing measured. The caller then keeps the
// forwarded body as it is, and the event books no figure (the log line says acc_fallback deferral_unplaced) instead of
// booking a wrong one. The tools array is rebuilt in one pass: a copy of the body for each stub is O(body x tools).
func BilledBody(forwarded []byte, own map[string]bool) (billed []byte, ok bool) {
	if len(own) == 0 {
		return forwarded, false
	}
	tools := gjson.GetBytes(forwarded, "tools").Array()
	parts := make([]string, len(tools))
	stubbed := 0
	for i, t := range tools {
		name := t.Get("name").String()
		if !own[name] {
			parts[i] = t.Raw
			continue
		}
		stub := DeferredStub(name)
		if len(stub) >= len(t.Raw) {
			return forwarded, false
		}
		parts[i] = stub
		stubbed++
	}
	if stubbed == 0 {
		return forwarded, false
	}
	billed, err := sjson.SetRawBytes(forwarded, "tools", []byte("["+strings.Join(parts, ",")+"]"))
	if err != nil {
		return forwarded, false
	}
	return billed, true
}

// IsStub says whether the tool at i of the billed body is a deferred stub: a bare name and the defer_loading mark.
func IsStub(billed []byte, i int) bool {
	t := gjson.GetBytes(billed, "tools."+strconv.Itoa(i))
	return t.Get("defer_loading").Bool() && t.Get("name").Exists()
}

// EnvelopeOpsFor is the wire's EnvelopeOps for the body the accountant records, with tool deferral's edits attributed
// to the tooldefer operator so its row does not land on toolschema (#908). client is the body the agent sent, billed
// the body BilledBody gave, and preDefer the body deferral read (nil when deferral did not run). Deferral's edits are
// two: the definitions the proxy deferred, each now a stub (R56), and the search tool it adds, an ordinary added
// declaration.
//
// Each keeps the ChangeID EnvelopeOps gave it, and only the operator changes. The ChangeID is what the library
// replaces by, so a thread whose state was written before this change (per-tool cg:toolschema:decl-rewritten and
// decl-added operations) has them replaced on its next request instead of kept beside a second copy, and a deferred
// set that grows or shrinks mid-thread re-reports each surviving tool under its own stable ID. One merged operation
// would carry a hash of the whole set, so any change of the set would orphan it.
func EnvelopeOpsFor(client, billed, preDefer []byte) []accountant.Operation {
	ops := accwire.For(client).EnvelopeOps(client, billed)
	if preDefer == nil {
		return ops
	}
	// Match by NAME: a rewritten declaration is reported at its CLIENT index, and a declaration removed before it
	// makes the billed index smaller.
	own := OwnDeferred(billed, preDefer)
	atBilled := map[string]int{}
	for i, t := range gjson.GetBytes(billed, "tools").Array() {
		atBilled[t.Get("name").String()] = i
	}
	clientTools := gjson.GetBytes(client, "tools").Array()
	out := make([]accountant.Operation, 0, len(ops))
	for _, o := range ops {
		switch {
		case o.Operator == "context-guru.toolschema" && o.At.Section == accountant.SectionTools &&
			strings.HasPrefix(o.ChangeID, "cg:toolschema:decl-rewritten:") && o.At.Index < len(clientTools) &&
			own[clientTools[o.At.Index].Get("name").String()] && IsStub(billed, atBilled[clientTools[o.At.Index].Get("name").String()]):
			o.Operator = "context-guru.tooldefer"
		case o.Operator == "context-guru.toolschema" && strings.HasPrefix(o.ChangeID, "cg:toolschema:decl-added:") &&
			gjson.GetBytes(billed, "tools."+strconv.Itoa(o.At.Index)+".name").String() == tooldefer.SearchName:
			o.Operator = "context-guru.tooldefer"
		}
		out = append(out, o)
	}
	return out
}
