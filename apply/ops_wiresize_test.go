package apply_test

import (
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/apply"
	cf "github.com/rossoctl/context-guru/counterfactual"
)

// wireDelta is, per wire message index, len(in) - len(out) of an equal-count rewrite.
func wireDelta(t *testing.T, in, out []byte) map[int]int {
	t.Helper()
	a, b := gjson.GetBytes(in, "messages").Array(), gjson.GetBytes(out, "messages").Array()
	if len(a) != len(b) {
		t.Fatalf("message count changed: %d -> %d", len(a), len(b))
	}
	d := map[int]int{}
	for i := range a {
		if a[i].Raw != b[i].Raw {
			d[i] = len(a[i].Raw) - len(b[i].Raw)
		}
	}
	return d
}

// checkChains asserts the library's sum invariant on Trace.Ops: per wire message, the ops form a
// contiguous chain from the inbound message's bytes to the forwarded one's, and nothing else moved.
func checkChains(t *testing.T, body []byte, res apply.Result) {
	t.Helper()
	in := gjson.GetBytes(body, "messages").Array()
	out := gjson.GetBytes(res.Body, "messages").Array()
	want := wireDelta(t, body, res.Body)
	got := map[int]int{}
	last := map[int]int{}
	for _, o := range res.Trace.Ops {
		i := o.At.Index
		if prev, ok := last[i]; ok {
			if o.SizeBefore.Bytes != prev {
				t.Fatalf("op chain broken at wire %d: %d after %d", i, o.SizeBefore.Bytes, prev)
			}
		} else if o.SizeBefore.Bytes != len(in[i].Raw) {
			t.Fatalf("first op on wire %d: SizeBefore %d, message is %d bytes", i, o.SizeBefore.Bytes, len(in[i].Raw))
		}
		last[i] = o.SizeAfter.Bytes
		got[i] += o.SizeBefore.Bytes - o.SizeAfter.Bytes
	}
	for i, a := range last {
		if a != len(out[i].Raw) {
			t.Fatalf("last op on wire %d: SizeAfter %d, forwarded message is %d bytes", i, a, len(out[i].Raw))
		}
	}
	for i, d := range want {
		if got[i] != d {
			t.Fatalf("wire %d: ops remove %d bytes, bodies differ by %d", i, got[i], d)
		}
	}
	for i, g := range got {
		if want[i] != g {
			t.Fatalf("wire %d: ops remove %d bytes, bodies differ by %d", i, g, want[i])
		}
	}
}

// One component, one OpenAI tool message: the op carries the JSON message element's bytes (role,
// tool_call_id, escaped newlines), not the decoded text.
func TestTraceOpsCarryWireBytesNotTextBytes(t *testing.T) {
	res := traceOf(t, "attr1", bschemas.OpenAI, string(toolBody()))
	if len(res.Trace.Ops) != 1 || res.Trace.OpsDropped != 0 {
		t.Fatalf("ops = %+v dropped = %d", res.Trace.Ops, res.Trace.OpsDropped)
	}
	checkChains(t, toolBody(), res)
}

// Two components on one message: the boundary between them splits the wire delta, the chain stays
// contiguous and the total exact.
func TestTraceOpsTwoComponentsOnOneMessageChain(t *testing.T) {
	res := traceOf(t, "attr1, attr2", bschemas.OpenAI, string(toolBody()))
	if len(res.Trace.Ops) != 2 || res.Trace.OpsDropped != 0 {
		t.Fatalf("ops = %+v dropped = %d", res.Trace.Ops, res.Trace.OpsDropped)
	}
	checkChains(t, toolBody(), res)
	if res.Trace.Ops[0].SizeAfter.Bytes == res.Trace.Ops[0].SizeBefore.Bytes {
		t.Fatalf("the first component's share must not be zero: %+v", res.Trace.Ops[0])
	}
}

// Two tool_results in one Anthropic wire message, rewritten by two components: four ops on one wire
// message, one chain. Also: the two slots get two ChangeIDs although the component is the same.
func TestTraceOpsSlotsSharingAWireMessageChain(t *testing.T) {
	long := strings.Repeat("a long original tool output line that the component will rewrite\\n", 8)
	body := `{"model":"claude-x","messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"R","input":{}},{"type":"tool_use","id":"b","name":"R","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"` + long + `"},{"type":"tool_result","tool_use_id":"b","content":"` + long + `"}]}
	]}`
	res := traceOf(t, "attr1, attr2", bschemas.Anthropic, body)
	if len(res.Trace.Ops) != 4 || res.Trace.OpsDropped != 0 {
		t.Fatalf("ops = %+v dropped = %d", res.Trace.Ops, res.Trace.OpsDropped)
	}
	checkChains(t, []byte(body), res)
	seen := map[string]bool{}
	for _, o := range res.Trace.Ops {
		if seen[o.ChangeID] {
			t.Fatalf("duplicate ChangeID %q for two identical outputs", o.ChangeID)
		}
		seen[o.ChangeID] = true
	}
}

// A count-changing run: the splice op's sizes are the removed and inserted JSON elements, and the
// library's sum check passes on the bodies apply produced.
func TestTraceOpsSpliceWireBytesPassTheSumCheck(t *testing.T) {
	long := strings.Repeat("some long earlier turn text ", 20)
	body := `{"model":"claude-x","messages":[
		{"role":"user","content":"first"},
		{"role":"assistant","content":"` + long + `"},
		{"role":"user","content":"` + long + `"},
		{"role":"assistant","content":"tail one"},
		{"role":"user","content":"final"}
	]}`
	res := traceOf(t, "midsum", bschemas.Anthropic, body)
	if len(res.Trace.Ops) != 1 {
		t.Fatalf("ops = %+v dropped %d", res.Trace.Ops, res.Trace.OpsDropped)
	}
	r := libraryResult([]byte(body), res.Body, res.Trace.Ops)
	for _, c := range r.Checks {
		if c.Check == "sum" || c.Check == "unique_change" {
			t.Fatalf("checks = %+v", r.Checks)
		}
	}
}

// libraryResult runs the library on the inbound and forwarded bodies, positions measured as raw
// JSON elements (the proxy's cfBodies does the same).
func libraryResult(client, fwd []byte, ops []cf.Operation) cf.Result {
	b := func(raw []byte) cf.Body {
		var m [][]byte
		for _, v := range gjson.GetBytes(raw, "messages").Array() {
			m = append(m, []byte(v.Raw))
		}
		return cf.NewBody(nil, nil, m)
	}
	ts := cf.NewTrackers()
	r := ts.BeginRequest(cf.RequestMeta{RequestID: "r1", Session: "s", Billing: cf.BillingFrom(cf.BillingAPI)}, b(client), nil)
	for _, o := range ops {
		r.Add(o)
	}
	r.EndRequest(cf.Thread{ID: "main"}, b(fwd), nil, cf.EntryFacts{Lifetime: cf.LifetimeUnknown})
	return r.Compute(cf.Usage{Input: 1000})
}
