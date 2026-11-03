package proxy

import (
	"testing"

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
