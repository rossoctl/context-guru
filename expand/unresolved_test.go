package expand

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rossoctl/context-guru/internal/extract"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// EVERY KEY THE REAL MINTERS PRODUCE MUST BE WELL-FORMED, which is what makes shape-validation safe.
//
// WellFormedID is coupled to the minters' output shape, and the dangerous direction is a check that
// is too NARROW: an id it wrongly calls malformed is counted as the model's fault, which zeroes the
// counter that says WE broke reversibility. So this drives the actual minting functions rather than
// restating their format, and fails loudly if either changes shape.
func TestEveryMintedKeyIsWellFormed(t *testing.T) {
	inputs := []string{
		"",
		"short",
		strings.Repeat("2024-01-01 GET /users/42 200 12ms src/api/users.py\n", 700),
		`[{"id":1,"name":"keep this"},{"id":2,"name":"drop this"}]`,
		"a\tb\n\n  c   d\n",
	}
	for _, in := range inputs {
		// internal/extract.ContentKey: the per-output stash key (24 hex).
		if k := extract.ContentKey(in); !WellFormedID(k) {
			t.Errorf("ContentKey(%.20q) = %q, which WellFormedID rejects — an unresolved lookup "+
				"for it would be blamed on the model instead of counted as our defect", in, k)
		}
	}
	// A key of each length the minters use must pass, so a length dropping out of idHexLens is
	// caught here rather than by the counter quietly going to zero in production.
	for _, k := range []string{"0123456789abcdef", "0123456789abcdef01234567"} {
		if !WellFormedID(k) {
			t.Errorf("%q (len %d) is a length this proxy mints and must be well-formed", k, len(k))
		}
	}
}

// And the converse: things the model plausibly invents must NOT be counted as our defect, or the
// alertable counter fills with noise and stops being alertable.
func TestModelInventionsAreNotWellFormed(t *testing.T) {
	for _, bad := range []string{
		"",
		"toolu_01A9FabcdefghijkLMNOP",             // a tool-call id, not a marker id
		"0123456789ABCDEF",                        // uppercase hex — not what hex.EncodeToString emits
		"0123456789abcde",                         // 15: one short of a real length
		"0123456789abcdef0",                       // 17: one over
		"0123456789abcdef0123456789abcdef",        // 32: the result-cache key's length, never a marker
		"cg:xres:0123456789abcdef01234567",        // a namespaced key, not a marker id
		"the original content is no longer there", // prose
		"0123456789abcdefg",
	} {
		if WellFormedID(bad) {
			t.Errorf("%q was judged well-formed; a model invention counted as a context-guru "+
				"defect makes the alertable counter unalertable", bad)
		}
	}
}

// The classification must reach the counters through the REPAIR PATH, not just the helper — that is
// where a real unresolved id arrives.
func TestRepairCountsTheTwoCausesApart(t *testing.T) {
	mal0, mis0 := Unresolved()

	// A well-formed id with nothing behind it: our broken promise.
	missing := extract.ContentKey("content that was never stashed")
	// An id the model plainly invented.
	invented := "totally-made-up-id"

	st := store.NewMemory(store.Options{})
	body := repairFixture(t, missing, invented)
	out, restored := RepairToolResults("anthropic", body, func(id string) (string, bool) {
		return Resolve(st, id) // nothing is stashed, so both fail
	})
	if len(restored) != 0 {
		t.Fatalf("nothing was stashed, so nothing could be restored; got %v", restored)
	}
	// PRECONDITION: both ids actually reached the resolve path. Without this the counts below
	// could be zero because the fixture never matched, which reads identically to a working split.
	for _, id := range []string{missing, invented} {
		if !strings.Contains(string(out), id) {
			t.Fatalf("id %q never reached the repair path; the fixture does not exercise it", id)
		}
	}

	mal, mis := Unresolved()
	if mis-mis0 != 1 {
		t.Errorf("expected exactly one MISSING (a mintable id resolving to nothing), got %d",
			mis-mis0)
	}
	if mal-mal0 != 1 {
		t.Errorf("expected exactly one MALFORMED (an invented id), got %d", mal-mal0)
	}
}

// repairFixture builds an Anthropic body whose tool_result blocks carry the client's own failure
// text for two expand calls, which is the shape RepairToolResults exists to answer.
func repairFixture(t *testing.T, ids ...string) []byte {
	t.Helper()
	var calls, results strings.Builder
	for i, id := range ids {
		if i > 0 {
			calls.WriteString(",")
			results.WriteString(",")
		}
		calls.WriteString(`{"type":"tool_use","id":"call_` + id + `","name":"` + ToolName +
			`","input":{"id":"` + id + `"}}`)
		results.WriteString(`{"type":"tool_result","tool_use_id":"call_` + id +
			`","content":"Error: No such tool available: ` + ToolName + `"}`)
	}
	body := `{"model":"claude-x","messages":[` +
		`{"role":"user","content":"go"},` +
		`{"role":"assistant","content":[` + calls.String() + `]},` +
		`{"role":"user","content":[` + results.String() + `]}]}`
	if !gjson.Valid(body) {
		t.Fatalf("fixture is not valid JSON: %s", body)
	}
	return []byte(body)
}

// Guard against the counters being shared state that a parallel test could corrupt: they are
// process-wide by design (like every other counter in this package), so the tests above read a
// delta rather than an absolute. This pins that the accessor is doing an atomic load.
func TestUnresolvedReadsAreAtomic(t *testing.T) {
	before, _ := Unresolved()
	atomic.AddInt64(&unresolvedMalformed, 2)
	after, _ := Unresolved()
	if after-before != 2 {
		t.Errorf("Unresolved did not observe the increment: %d -> %d", before, after)
	}
}
