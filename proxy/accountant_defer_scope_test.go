package proxy

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/accountant"
)

const scopeMsgs = `"messages":[{"role":"user","content":"hi"}]`

func scopeTool(name string, deferred bool, extra string) string {
	d := ""
	if deferred {
		d = `,"defer_loading":true`
	}
	return `{"name":"` + name + `","description":"` + strings.Repeat("d", 300) + `"` + extra + d + `}`
}

// tooldeferOps counts the operations on the tooldefer row: one for each tool the proxy deferred.
func tooldeferOps(ops []accountant.Operation) (n int) {
	for _, o := range ops {
		if o.Operator == "context-guru.tooldefer" {
			n++
		}
	}
	return n
}

// F1: a tool the CLIENT deferred (Claude Code does) is already free in the baseline: the provider bills no
// definition for it, whoever set the mark. Only the tools the proxy deferred are a saving of ours.
func TestOnlyTheProxysOwnDeferralIsStubbedAndPriced(t *testing.T) {
	cacheMark := `,"cache_control":{"type":"ephemeral"}`
	clientDeferred := scopeTool("WebSearch", true, cacheMark) // the client deferred it, full definition, with a mark
	ours := scopeTool("mcp__x__y", false, "")
	keep := scopeTool("Read", false, "")
	cl := []byte(`{"tools":[` + keep + `,` + clientDeferred + `,` + ours + `],` + scopeMsgs + `}`)
	// The proxy defers mcp__x__y only: the definition stays, with the mark.
	fw := []byte(`{"tools":[` + keep + `,` + clientDeferred + `,` + scopeTool("mcp__x__y", true, "") + `],` + scopeMsgs + `}`)
	c := &capture{accClient: cl, accPreDefer: cl}
	billed := c.accBilled(fw)
	if !strings.Contains(string(billed), strings.Repeat("d", 300)) || strings.Count(string(billed), strings.Repeat("d", 300)) != 2 {
		t.Fatalf("billed copy should keep the client-deferred and the kept definitions full, and stub only ours")
	}
	if gjson.GetBytes(billed, "tools.1.cache_control").Raw == "" {
		t.Errorf("the client-deferred tool lost its cache_control in the accountant copy")
	}
	ops := c.envelopeOpsFor(billed)
	if n := tooldeferOps(ops); n != 1 {
		t.Errorf("tooldefer ops = %d, want 1 (only the tool the proxy deferred): %+v", n, ops)
	}
}

// F2: a declaration filter that removed a tool before the deferred one shifts the billed index below the
// client index. The deferred rewrite must still land on the tooldefer row.
func TestDeferralIsFoundByNameWhenAFilterRemovedAToolBeforeIt(t *testing.T) {
	removed := scopeTool("Removed", false, "")
	ours := scopeTool("mcp__x__y", false, "")
	keep := scopeTool("Read", false, "")
	cl := []byte(`{"tools":[` + removed + `,` + keep + `,` + ours + `],` + scopeMsgs + `}`)
	fw := []byte(`{"tools":[` + keep + `,` + scopeTool("mcp__x__y", true, "") + `],` + scopeMsgs + `}`)
	c := &capture{accClient: cl, accPreDefer: cl}
	billed := c.accBilled(fw)
	if strings.Count(string(billed), strings.Repeat("d", 300)) != 1 {
		t.Fatalf("billed copy should stub the deferred tool: %s", billed)
	}
	ops := c.envelopeOpsFor(billed)
	if n := tooldeferOps(ops); n != 1 {
		t.Fatalf("tooldefer ops = %d, want 1: %+v", n, ops)
	}
	if got, want := opsDelta(ops, accountant.SectionTools), sectionDelta(cl, billed, accountant.SectionTools); got != want {
		t.Errorf("ops explain %d tools bytes, the bodies differ by %d", got, want)
	}
}

// scopeRun drives the real library tracker with the ops envelopeOpsFor builds for a deferral of `names`.
// The client declares a, b, c in full. The billed copy stubs the names given.
type scopeRun struct {
	run accRun
	n   int
}

func scopeBodies(stub ...string) (client, billed []byte) {
	tools := func(stubbed map[string]bool) string {
		var s []string
		for _, n := range []string{"a", "b", "c"} {
			s = append(s, scopeTool(n, stubbed[n], ""))
		}
		return strings.Join(s, ",")
	}
	set := map[string]bool{}
	for _, n := range stub {
		set[n] = true
	}
	var parts []string
	for _, n := range []string{"a", "b", "c"} {
		if set[n] {
			parts = append(parts, deferredStub(n))
		} else {
			parts = append(parts, scopeTool(n, false, ""))
		}
	}
	return []byte(`{"tools":[` + tools(nil) + `],` + scopeMsgs + `}`), []byte(`{"tools":[` + strings.Join(parts, ",") + `],` + scopeMsgs + `}`)
}

func (r *scopeRun) do(t *testing.T, client, billed []byte, ops []accountant.Operation) accountant.Result {
	t.Helper()
	r.n++
	return r.run.do(r.n, client, billed, Usage{FreshInput: 10, CacheRead: 1000}, ops...)
}

func hasSumCheck(res accountant.Result) bool {
	for _, ch := range res.Checks {
		if ch.Check == "sum" || ch.Check == "unique_change" || ch.Check == "conservation" {
			return true
		}
	}
	return false
}

// F3: a thread whose state was written by the previous code carries the deferral as per-tool
// cg:toolschema:decl-rewritten:<hash> operations. The new code must not report the same bytes again under a
// different ChangeID: the library keeps the old operations active and the sum check counts the bytes twice.
func TestAThreadStateFromThePreviousCodeStillTiesAfterTheUpgrade(t *testing.T) {
	r := &scopeRun{run: accRun{newSavingsTracker(), accountant.Thread{ID: "main"}}}
	client, billed := scopeBodies("b", "c")
	// The previous code: one toolschema op per deferred tool.
	var old []accountant.Operation
	for _, o := range envelopeOps(client, billed) {
		if strings.HasPrefix(o.ChangeID, "cg:toolschema:decl-rewritten:") {
			old = append(old, o)
		}
	}
	if len(old) != 2 {
		t.Fatalf("precondition: %d per-tool ops from the previous code, want 2", len(old))
	}
	if res := r.do(t, client, billed, old); hasSumCheck(res) {
		t.Fatalf("the previous code's first request must tie: %+v", res.Checks)
	}
	// The new code, same thread, same deferral.
	c := &capture{accClient: client, accPreDefer: client}
	res := r.do(t, client, billed, c.envelopeOpsFor(billed))
	if hasSumCheck(res) {
		t.Errorf("after the upgrade the sum check fires on a thread whose deferral did not change: %+v", res.Checks)
	}
}

// F3, second shape: a mid-thread rebuild changes the deferred set. The set's operation must be replaced, not
// left active beside the new one.
func TestAChangedDeferredSetReplacesTheOldOperation(t *testing.T) {
	r := &scopeRun{run: accRun{newSavingsTracker(), accountant.Thread{ID: "main"}}}
	client, billed1 := scopeBodies("b", "c")
	c1 := &capture{accClient: client, accPreDefer: client}
	if res := r.do(t, client, billed1, c1.envelopeOpsFor(billed1)); hasSumCheck(res) {
		t.Fatalf("first request must tie: %+v", res.Checks)
	}
	// The set GROWS: a is deferred as well. The old operation must not stay active beside the new one.
	_, billed2 := scopeBodies("a", "b", "c")
	c2 := &capture{accClient: client, accPreDefer: client}
	if res := r.do(t, client, billed2, c2.envelopeOpsFor(billed2)); hasSumCheck(res) {
		t.Errorf("after the deferred set grew the checks fire: %+v", res.Checks)
	}
}

// The set SHRINKS: b is loaded again and is back at its full size.
func TestAShrunkDeferredSetStillTies(t *testing.T) {
	r := &scopeRun{run: accRun{newSavingsTracker(), accountant.Thread{ID: "main"}}}
	client, billed1 := scopeBodies("b", "c")
	c1 := &capture{accClient: client, accPreDefer: client}
	if res := r.do(t, client, billed1, c1.envelopeOpsFor(billed1)); hasSumCheck(res) {
		t.Fatalf("first request must tie: %+v", res.Checks)
	}
	_, billed2 := scopeBodies("c")
	c2 := &capture{accClient: client, accPreDefer: client}
	if res := r.do(t, client, billed2, c2.envelopeOpsFor(billed2)); hasSumCheck(res) {
		t.Errorf("after the deferred set shrank the checks fire: %+v", res.Checks)
	}
}
