package proxy

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// The prompt the provider bills (accountant R56).
//
// Tool deferral sets defer_loading on a tool and leaves the definition in the bytes the proxy sends. The provider
// keeps that tool out of the prompt and bills nothing for the definition, so the forwarded body is larger than the
// prompt that was billed. The library reads body bytes, so it would see no shrink, and the sum check could not tie
// the saving to the bodies.
//
// billedBody is the forwarded body with each deferred definition cut to a stub, a bare {"name":...}. The accountant
// gets this copy. Nothing else does: the bytes sent to the provider, the cache breakpoints and the ping facts all
// come from the real body. Deferral is then an ordinary shrink of `tools` positions, reported as one operation.

// deferredStub is what the accountant's copy of a deferred definition keeps. The provider bills no definition, so the
// stub is as small as a tool can be and still be a tool. Our own tokenizer does not enter here: the library converts
// the bytes with the request's own ratio, billed tokens over body bytes.
func deferredStub(name string) string {
	return `{"name":` + jsonString(name) + `,"defer_loading":true}`
}

func jsonString(s string) string {
	b, _ := sjson.SetBytes([]byte(`{}`), "v", s)
	return gjson.GetBytes(b, "v").Raw
}

// deferredNames is the set of names of the definitions the body defers.
func deferredNames(body []byte) map[string]bool {
	out := map[string]bool{}
	for _, t := range gjson.GetBytes(body, "tools").Array() {
		if n := t.Get("name").String(); n != "" && t.Get("defer_loading").Bool() {
			out[n] = true
		}
	}
	return out
}

// ownDeferred is the names the PROXY deferred on this request: deferred in the forwarded body, and not already
// deferred in the body deferral read. A tool the client deferred itself (Claude Code does) is free in the baseline
// too, because the provider bills no definition for it whoever set the mark, so it is no saving of ours and it keeps
// its definition, with its cache_control, in the accountant's copy.
func ownDeferred(forwarded, preDefer []byte) map[string]bool {
	out := deferredNames(forwarded)
	for n := range deferredNames(preDefer) {
		delete(out, n)
	}
	return out
}

// billedBody returns the forwarded body as the provider bills it, with the definitions in own cut to stubs. ok=false
// means the accountant should not be given a stub-sized copy: nothing in own is in the body, or a stub would not be
// smaller than the definition it replaces. A stub that grows the tool is not a deferral the provider billed less for,
// and an operation that reports it as a shrink would book a saving nothing measured. The caller then keeps the
// forwarded body as it is, and the event books no figure (the log line says acc_fallback deferral_unplaced) instead of
// booking a wrong one. The tools array is rebuilt in one pass: a copy of the body for each stub is O(body x tools).
func billedBody(forwarded []byte, own map[string]bool) (billed []byte, ok bool) {
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
		stub := deferredStub(name)
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
