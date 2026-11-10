package proxy

import (
	"strconv"

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

// deferredTools lists the positions in `tools` of the definitions the forwarded body defers.
func deferredTools(forwarded []byte) []int {
	var at []int
	for i, t := range gjson.GetBytes(forwarded, "tools").Array() {
		if t.Get("defer_loading").Bool() && t.Get("name").String() != "" {
			at = append(at, i)
		}
	}
	return at
}

// billedBody returns the forwarded body as the provider bills it. ok=false means the accountant should not be given a
// stub-sized copy: the body has no deferred tool, or a stub would not be smaller than the definition it replaces. A
// stub that grows the tool is not a deferral the provider billed less for, and an operation that reports it as a
// shrink would book a saving nothing measured. The caller then keeps the forwarded body as it is, and the event says
// why it books no figure (acc_fallback deferral_unplaced) instead of booking a wrong one.
func billedBody(forwarded []byte) (billed []byte, ok bool) {
	at := deferredTools(forwarded)
	if len(at) == 0 {
		return forwarded, false
	}
	billed = forwarded
	for _, i := range at {
		path := "tools." + strconv.Itoa(i)
		def := gjson.GetBytes(billed, path)
		stub := deferredStub(def.Get("name").String())
		if len(stub) >= len(def.Raw) {
			return forwarded, false
		}
		var err error
		if billed, err = sjson.SetRawBytes(billed, path, []byte(stub)); err != nil {
			return forwarded, false
		}
	}
	return billed, true
}
