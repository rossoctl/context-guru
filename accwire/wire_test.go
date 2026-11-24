package accwire

import (
	"testing"

	"github.com/rossoctl/accountant"
)

func TestForPicksTheWireOnce(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"anthropic", `{"system":"s","messages":[{"role":"user","content":"hi"}]}`, "messages"},
		{"chat completions", `{"messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}]}`, "messages"},
		{"responses array", `{"instructions":"s","input":[{"role":"user","content":"hi"}]}`, "responses"},
		{"responses string", `{"input":"hi"}`, "responses"},
		{"both: messages wins", `{"input":"x","messages":[{"role":"user","content":"hi"}]}`, "messages"},
		{"messages not an array, input array", `{"messages":null,"input":[{"role":"user"}]}`, "responses"},
		{"no conversation", `{"model":"m"}`, "messages"},
		{"empty", ``, "messages"},
		{"invalid", `{"input":`, "messages"},
	}
	for _, c := range cases {
		if got := For([]byte(c.body)).Name(); got != c.want {
			t.Errorf("%s: For = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestWireIndex(t *testing.T) {
	cases := []struct {
		wire Wire
		path string
		idx  int
		ok   bool
	}{
		{Messages, "messages.3", 3, true},
		{Messages, "messages.3.content.1.text", 3, true},
		{Messages, "messages.x", 0, false},
		{Messages, "system.0.text", 0, false},
		{Messages, "input.3", 0, false},
		{Responses, "input.3", 3, true},
		{Responses, "input.3.content.1.text", 3, true},
		{Responses, "input", 0, true}, // a string input is one position
		{Responses, "input.-1", 0, false},
		{Responses, "input.x", 0, false},
		{Responses, "instructions", 0, false}, // the system section is no position
		{Responses, "messages.3", 0, false},
	}
	for _, c := range cases {
		idx, ok := c.wire.WireIndex(c.path)
		if ok != c.ok || (ok && idx != c.idx) {
			t.Errorf("%s.WireIndex(%q) = %d,%v want %d,%v", c.wire.Name(), c.path, idx, ok, c.idx, c.ok)
		}
	}
}

// Each wire reads its own sections: Responses takes instructions and input, Messages does not.
func TestWiresReadTheirOwnSections(t *testing.T) {
	resp := []byte(`{"instructions":"sys","input":[{"role":"user","content":"a"},{"role":"user","content":"b"}]}`)
	if n := len(Responses.Body(resp).Pos); n != 3 {
		t.Fatalf("Responses positions = %d, want 3 (1 system, 2 input)", n)
	}
	if n := len(Messages.Body(resp).Pos); n != 0 {
		t.Fatalf("Messages read a Responses body: %d positions", n)
	}
	msg := []byte(`{"system":"sys","messages":[{"role":"user","content":"a","cache_control":{"type":"ephemeral"}}]}`)
	body, bps := Messages.Parse(msg, nil)
	if len(body.Pos) != 2 || len(bps) != 1 || bps[0].Pos != (accountant.Position{Section: accountant.SectionMessages, Index: 0}) {
		t.Fatalf("Messages.Parse = %d positions, breakpoints %+v", len(body.Pos), bps)
	}
}

func TestEnvelopeOpsPerWire(t *testing.T) {
	cl := []byte(`{"tools":[{"name":"a"}],"messages":[{"role":"user","content":"hi"}]}`)
	fw := []byte(`{"tools":[{"name":"a"},{"name":"b"}],"messages":[{"role":"user","content":"hi"}]}`)
	ops := For(cl).EnvelopeOps(cl, fw)
	if len(ops) != 1 || ops[0].At.Section != accountant.SectionTools || ops[0].At.Index != 1 {
		t.Fatalf("an added declaration: %+v", ops)
	}
	rcl := []byte(`{"instructions":"s","input":[{"role":"user","content":"hi"}]}`)
	rfw := []byte(`{"instructions":"s2","input":[{"role":"user","content":"hi"}]}`)
	ops = For(rcl).EnvelopeOps(rcl, rfw)
	if len(ops) != 1 || ops[0].At.Section != accountant.SectionSystem {
		t.Fatalf("Responses instructions edit: %+v", ops)
	}
	if got := Messages.EnvelopeOps(rcl, rfw); got != nil {
		t.Fatalf("Messages gave ops for a body with no messages array: %+v", got)
	}
}

func TestMessageOpsInsert(t *testing.T) {
	before := []byte(`{"input":[{"role":"user","content":"a"}]}`)
	after := []byte(`{"input":[{"role":"user","content":"a"},{"role":"system","content":"list"}]}`)
	ops := For(before).MessageOps("tooldefer", before, after)
	if len(ops) != 1 || ops[0].MessageSplice == nil || ops[0].MessageSplice.At != 1 {
		t.Fatalf("Responses insertion: %+v", ops)
	}
}
