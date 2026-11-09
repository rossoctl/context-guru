package proxy

import (
	"testing"
	"time"

	"github.com/rossoctl/accountant"
)

// sectionDelta is the client-minus-forwarded bytes of one section, the sum check's "bodies" side.
func sectionDelta(cl, fw []byte, s accountant.Section) int {
	sec := map[accountant.Section]int{accountant.SectionTools: 0, accountant.SectionSystem: 1, accountant.SectionMessages: 2}[s]
	bytes := func(b accountant.Body) int {
		end := len(b.Pos)
		if sec < 2 {
			end = b.Start[sec+1]
		}
		n := 0
		for _, p := range b.Pos[b.Start[sec]:end] {
			n += int(p.Bytes)
		}
		return n
	}
	return bytes(accBodies(cl)) - bytes(accBodies(fw))
}

// opsDelta is the operations' before-minus-after bytes of one section, the sum check's "ops" side.
func opsDelta(ops []accountant.Operation, s accountant.Section) int {
	n := 0
	for _, o := range ops {
		if o.At.Section == s {
			n += o.SizeBefore.Bytes - o.SizeAfter.Bytes
		}
	}
	return n
}

func TestEnvelopeOpsExplainEveryByte(t *testing.T) {
	const msgs = `"messages":[{"role":"user","content":"hi"}]`
	tA := `{"name":"a","description":"x","input_schema":{"type":"object","$schema":"s"}}`
	tB := `{"name":"b","description":"y"}`
	tC := `{"name":"c","description":"z"}`
	big := `{"type":"text","text":"STABLE\n<env>git</env>","cache_control":{"type":"ephemeral"}}`
	tM := `{"name":"m","description":"x","input_schema":{"type":"object","$schema":"s"},"cache_control":{"type":"ephemeral"}}`
	for _, tc := range []struct {
		name      string
		cl, fw    string
		operators []string
		changeIDs []string
		wantNoOps bool
	}{
		{name: "no change",
			cl: `{"tools":[` + tA + `],` + msgs + `}`, fw: `{"tools":[` + tA + `],` + msgs + `}`, wantNoOps: true},
		{name: "expand injected",
			cl:        `{"tools":[` + tA + `],` + msgs + `}`,
			fw:        `{"tools":[` + tA + `,{"name":"context_guru_expand","description":"e"}],` + msgs + `}`,
			operators: []string{"context-guru.expand"}},
		{name: "two removed, one rewritten in place",
			cl:        `{"tools":[` + tA + `,` + tB + `,` + tC + `],` + msgs + `}`,
			fw:        `{"tools":[{"name":"a","description":"x","input_schema":{"type":"object"}}],` + msgs + `}`,
			operators: []string{"context-guru.toolfilter", "context-guru.toolschema"}},
		{name: "system split",
			cl:        `{"system":[` + big + `],` + msgs + `}`,
			fw:        `{"system":[{"type":"text","text":"STABLE\n","cache_control":{"type":"ephemeral"}},{"type":"text","text":"<env>git</env>"}],` + msgs + `}`,
			operators: []string{"context-guru.cachesplit"}},
		{name: "system relabelled to 1h",
			cl:        `{"system":[` + big + `],` + msgs + `}`,
			fw:        `{"system":[{"type":"text","text":"STABLE\n<env>git</env>","cache_control":{"type":"ephemeral","ttl":"1h"}}],` + msgs + `}`,
			operators: []string{"context-guru.headttl"}},
		// apply relabels the head first and splits second, so the forwarded first half carries the 1h mark.
		{name: "system relabelled to 1h and split",
			cl:        `{"system":[` + big + `],` + msgs + `}`,
			fw:        `{"system":[{"type":"text","text":"STABLE\n","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":"<env>git</env>"}],` + msgs + `}`,
			operators: []string{"context-guru.headttl", "context-guru.cachesplit"}},
		{name: "tool rewritten and relabelled to 1h",
			cl:        `{"tools":[` + tM + `],` + msgs + `}`,
			fw:        `{"tools":[{"name":"m","description":"x","cache_control":{"type":"ephemeral","ttl":"1h"}}],` + msgs + `}`,
			operators: []string{"context-guru.toolschema", "context-guru.headttl"}},
		{name: "tool relabelled to 1h only",
			cl:        `{"tools":[` + tM + `],` + msgs + `}`,
			fw:        `{"tools":[{"name":"m","description":"x","input_schema":{"type":"object","$schema":"s"},"cache_control":{"type":"ephemeral","ttl":"1h"}}],` + msgs + `}`,
			operators: []string{"context-guru.headttl"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := envelopeOps([]byte(tc.cl), []byte(tc.fw))
			if tc.wantNoOps {
				if len(ops) != 0 {
					t.Fatalf("ops %+v, want none", ops)
				}
				return
			}
			var got []string
			for _, o := range ops {
				got = append(got, o.Operator)
				if o.ChangeID == "" {
					t.Errorf("op %+v has no ChangeID", o)
				}
			}
			if len(got) != len(tc.operators) {
				t.Fatalf("operators %v, want %v", got, tc.operators)
			}
			for i := range got {
				if got[i] != tc.operators[i] {
					t.Fatalf("operators %v, want %v", got, tc.operators)
				}
			}
			for _, s := range []accountant.Section{accountant.SectionTools, accountant.SectionSystem} {
				if o, b := opsDelta(ops, s), sectionDelta([]byte(tc.cl), []byte(tc.fw), s); o != b {
					t.Errorf("section %s: ops %d bytes, bodies %d bytes", s, o, b)
				}
			}
		})
	}
}

// The ChangeID is derived from the change, never from the turn: the same injection on the next turn
// gets the same ChangeID, so the library replaces the active op and prices it once.
func TestEnvelopeChangeIDIsStableAcrossTurns(t *testing.T) {
	cl1 := `{"tools":[{"name":"a"}],"messages":[{"role":"user","content":"hi"}]}`
	cl2 := `{"tools":[{"name":"a"}],"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ok"}]}`
	inj := func(cl string) string {
		return cl[:len(`{"tools":[{"name":"a"}`)] + `,{"name":"context_guru_expand"}` + cl[len(`{"tools":[{"name":"a"}`):]
	}
	a := envelopeOps([]byte(cl1), []byte(inj(cl1)))
	b := envelopeOps([]byte(cl2), []byte(inj(cl2)))
	if len(a) != 1 || len(b) != 1 || a[0].ChangeID != b[0].ChangeID {
		t.Fatalf("turn 1 %+v, turn 2 %+v: want one op each with the same ChangeID", a, b)
	}
}

// A removal among repeated names cannot be placed, so nothing is reported and the sum check gates the
// event: the first "x" removed and the second rewritten used to come out as the second removed and the
// first rewritten, with the right sizes at the wrong positions.
func TestEnvelopeReportsNothingForARemovalAmongRepeatedNames(t *testing.T) {
	const msgs = `"messages":[{"role":"user","content":"hi"}]`
	x1, x2, y := `{"name":"x","description":"one"}`, `{"name":"x","description":"two!!"}`, `{"name":"y"}`
	cl := []byte(`{"tools":[` + x1 + `,` + x2 + `,` + y + `],` + msgs + `}`)
	fw := []byte(`{"tools":[{"name":"x","description":"two!!!"},` + y + `],` + msgs + `}`)
	if ops := envelopeOps(cl, fw); len(ops) != 0 {
		t.Fatalf("ops %+v, want none for an ambiguous removal", ops)
	}
}

// A tool name that repeats still matches by occurrence, so the operations explain every byte. Before,
// the last duplicate won the name and the operations did not match the bodies (the sum check failed).
func TestEnvelopeOpsExplainDuplicateToolNames(t *testing.T) {
	const msgs = `"messages":[{"role":"user","content":"hi"}]`
	x1 := `{"name":"x","description":"first"}`
	x2 := `{"name":"x","description":"second, longer"}`
	for _, tc := range []struct{ name, cl, fw string }{
		{"first duplicate rewritten", `{"tools":[` + x1 + `,` + x2 + `],` + msgs + `}`,
			`{"tools":[{"name":"x","description":"1"},` + x2 + `],` + msgs + `}`},
		{"a duplicate added", `{"tools":[` + x1 + `],` + msgs + `}`, `{"tools":[` + x1 + `,` + x2 + `],` + msgs + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := envelopeOps([]byte(tc.cl), []byte(tc.fw))
			if got, want := opsDelta(ops, accountant.SectionTools), sectionDelta([]byte(tc.cl), []byte(tc.fw), accountant.SectionTools); got != want {
				t.Fatalf("ops explain %d tools bytes, bodies differ by %d (ops %+v)", got, want, ops)
			}
		})
	}
}

// The head-TTL op reaches the library's premium effect through the real reporter: the proxy relabels
// the client's 5m system breakpoint to 1h, the response bills the write at 1h, and the library books
// the premium on the headttl change. The library finds the head by the prefix "headttl:"; a ChangeID
// under any other prefix is priced as nothing.
func TestHeadTTLRelabelBooksThePremium(t *testing.T) {
	sys := func(ttl string) string {
		return `"system":[{"type":"text","text":"HEAD","cache_control":{"type":"ephemeral"` + ttl + `}}]`
	}
	const msgs = `"messages":[{"role":"user","content":"hi"}]`
	client := []byte(`{` + sys("") + `,` + msgs + `}`)
	fwd := []byte(`{` + sys(`,"ttl":"1h"`) + `,` + msgs + `}`)
	ops := envelopeOps(client, fwd)
	if len(ops) != 1 || ops[0].Operator != "context-guru.headttl" {
		t.Fatalf("ops %+v, want one headttl op", ops)
	}
	st := newSavingsTracker()
	req := st.begin(accountant.RequestMeta{RequestID: "r1", Session: "s", Model: "m", At: time.Unix(1_700_000_000, 0)}, client)
	st.end(req, accountant.Thread{ID: "main"}, fwd, ops, accountant.EntryFacts{TTL: time.Hour, Lifetime: accountant.LifetimeExact})
	res := st.compute(req, Usage{CacheWrite: 100, CacheWrite1h: 100})
	for _, e := range res.CacheEffects {
		if e.Kind == accountant.EffectTTLPremium {
			return
		}
	}
	t.Fatalf("no ttlPremium effect for ChangeID %q: %+v", ops[0].ChangeID, res.CacheEffects)
}

// A head relabelled to 1h INSIDE a block that cachesplit then splits still books the premium: the
// relabel is reported as its own headttl operation, not absorbed into the split.
func TestHeadTTLRelabelOfASplitBlockBooksThePremium(t *testing.T) {
	const msgs = `"messages":[{"role":"user","content":"hi"}]`
	client := []byte(`{"system":[{"type":"text","text":"STABLE\n<env>git</env>","cache_control":{"type":"ephemeral"}}],` + msgs + `}`)
	fwd := []byte(`{"system":[{"type":"text","text":"STABLE\n","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":"<env>git</env>"}],` + msgs + `}`)
	ops := envelopeOps(client, fwd)
	assertPremiumBooked(t, client, fwd, ops)
}

// The same for a tool declaration that was rewritten and then relabelled.
func TestHeadTTLRelabelOfARewrittenToolBooksThePremium(t *testing.T) {
	const msgs = `"messages":[{"role":"user","content":"hi"}]`
	client := []byte(`{"tools":[{"name":"m","description":"x","input_schema":{"type":"object","$schema":"s"},"cache_control":{"type":"ephemeral"}}],` + msgs + `}`)
	fwd := []byte(`{"tools":[{"name":"m","description":"x","cache_control":{"type":"ephemeral","ttl":"1h"}}],` + msgs + `}`)
	ops := envelopeOps(client, fwd)
	assertPremiumBooked(t, client, fwd, ops)
}

func assertPremiumBooked(t *testing.T, client, fwd []byte, ops []accountant.Operation) {
	t.Helper()
	for _, s := range []accountant.Section{accountant.SectionTools, accountant.SectionSystem} {
		if o, b := opsDelta(ops, s), sectionDelta(client, fwd, s); o != b {
			t.Errorf("section %s: ops %d bytes, bodies %d bytes (ops %+v)", s, o, b, ops)
		}
	}
	st := newSavingsTracker()
	req := st.begin(accountant.RequestMeta{RequestID: "r1", Session: "s", Model: "m", At: time.Unix(1_700_000_000, 0)}, client)
	st.end(req, accountant.Thread{ID: "main"}, fwd, ops, accountant.EntryFacts{TTL: time.Hour, Lifetime: accountant.LifetimeExact})
	res := st.compute(req, Usage{CacheWrite: 100, CacheWrite1h: 100})
	if len(res.Checks) != 0 {
		t.Errorf("checks failed: %+v", res.Checks)
	}
	for _, e := range res.CacheEffects {
		if e.Kind == accountant.EffectTTLPremium {
			return
		}
	}
	t.Fatalf("no ttlPremium effect: ops %+v, effects %+v", ops, res.CacheEffects)
}

func TestMessageOpsExplainTheDeferralList(t *testing.T) {
	m0, m1 := `{"role":"user","content":"hi"}`, `{"role":"system","content":"env"}`
	m1l := `{"role":"system","content":"env\n\nlist of names"}`
	ins := `{"role":"system","content":"list of names"}`
	for _, tc := range []struct {
		name, before, after string
		wantOps             int
	}{
		{"appended", `{"messages":[` + m0 + `,` + m1 + `]}`, `{"messages":[` + m0 + `,` + m1l + `]}`, 1},
		{"inserted", `{"messages":[` + m0 + `,` + m1 + `]}`, `{"messages":[` + m0 + `,` + ins + `,` + m1 + `]}`, 1},
		{"unchanged", `{"messages":[` + m0 + `]}`, `{"messages":[` + m0 + `]}`, 0},
	} {
		ops := messageOps("tooldefer", []byte(tc.before), []byte(tc.after))
		if len(ops) != tc.wantOps {
			t.Fatalf("%s: %d ops, want %d", tc.name, len(ops), tc.wantOps)
		}
		if got, want := opsDelta(ops, accountant.SectionMessages), sectionDelta([]byte(tc.before), []byte(tc.after), accountant.SectionMessages); got != want {
			t.Errorf("%s: ops explain %d bytes of messages, the bodies differ by %d", tc.name, got, want)
		}
	}
}
