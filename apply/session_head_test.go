package apply

import (
	"encoding/json"
	"os"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/session"
)

// TestDerivedSessionKeyStableAcrossAgentSDKTurns pins the fix for the unstable
// derived session key.
//
// The fixture is REAL Claude-Agent-SDK traffic captured through the proxy
// (CONTEXT_GURU_CAPTURE) during a Terminal-Bench run, scrubbed: every prose span is
// replaced by a length-tagged placeholder, while the per-turn
// `<system-reminder><total_tokens>N tokens left</total_tokens>` blocks — the volatile
// bytes that caused the bug — are kept verbatim. Two conversations, three and two
// consecutive turns.
//
// Synthetic input would not have caught this: the volatile part is a system-role
// message the host APPENDS inside `messages` on every turn, which nobody writing a
// fixture by hand would think to add.
func TestDerivedSessionKeyStableAcrossAgentSDKTurns(t *testing.T) {
	raw, err := os.ReadFile("testdata/session_head_agentsdk.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	key := func(name string) string {
		body, ok := fx[name]
		if !ok {
			t.Fatalf("fixture %q missing", name)
		}
		norm, _ := normalize(bschemas.Anthropic, gjson.GetBytes(body, "messages").Array())
		if len(norm) == 0 {
			t.Fatalf("fixture %q normalized to no messages", name)
		}
		sys, firstUser := schema.SessionHead(norm)
		if sys == "" || firstUser == "" {
			t.Fatalf("fixture %q: sys=%q firstUser=%q — both must be non-empty or the test is not exercising the derivation", name, sys, firstUser)
		}
		return session.Scoped("", "", sys, firstUser)
	}

	// Every turn of one conversation must derive ONE key. Before the fix each of these
	// produced a different one, because the appended budget reminder changed `sys`.
	a1, a2, a3 := key("conv_a_turn1"), key("conv_a_turn2"), key("conv_a_turn3")
	if a1 != a2 || a2 != a3 {
		t.Errorf("consecutive turns of one conversation derived different keys: %q %q %q", a1, a2, a3)
	}
	b1, b2 := key("conv_b_turn1"), key("conv_b_turn2")
	if b1 != b2 {
		t.Errorf("consecutive turns of one conversation derived different keys: %q %q", b1, b2)
	}
	// ...and two genuinely different conversations must not collide into one state set.
	if a1 == b1 {
		t.Errorf("two different conversations collided on key %q", a1)
	}
	// Tenant scoping is unaffected by the head change.
	if got := session.Scoped("t1", "", "s", "u"); got == session.Scoped("t2", "", "s", "u") {
		t.Errorf("tenant scoping lost: %q", got)
	}
}
