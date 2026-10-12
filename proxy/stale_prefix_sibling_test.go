package proxy

import (
	"encoding/json"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
)

// TestCoversSpanRenormalizesSiblingTextGroups is #457's requirement 2 (coordinates): the
// stale-prefix guard's CoversSpan (sentStash, proxy/prefixask.go) must read the stashed
// preBody through the SAME apply.normalize every other guard uses, not a separate or stale
// copy of the old (sibling-dropping) logic. If it ever drifted back to the old behavior, a
// wire message holding a sibling text block beside a tool_result would normalize to ONE
// message instead of two, and this test's hardcoded expectation would catch the mismatch.
func TestCoversSpanRenormalizesSiblingTextGroups(t *testing.T) {
	preBody, _ := json.Marshal(map[string]any{
		"model": "claude-x",
		"messages": []map[string]any{
			{"role": "user", "content": "go"},
			{"role": "assistant", "content": []map[string]any{
				{"type": "tool_use", "id": "call_a", "name": "Read", "input": map[string]any{}},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "text", "text": "mid-turn sibling text"},
				{"type": "tool_result", "tool_use_id": "call_a", "content": "tool output"},
			}},
		},
	})

	norm := apply.NormalizeMessages(bschemas.Anthropic, preBody)
	// Hardcoded, not derived from the same call the assertions below exercise: [user, assistant
	// tool_use, sibling, tool] — if CoversSpan ever stopped reading through the current
	// apply.normalize, this body would normalize to 3 (the sibling dropped), not 4, and this
	// line would fail before any CoversSpan assertion even runs.
	if len(norm) != 4 {
		t.Fatalf("got %d normalized messages, want 4 (the fixture itself is broken): %+v", len(norm), norm)
	}

	stash := newSentStash()
	const session = "sibling-coverage-test"
	stash.put(session, bschemas.Anthropic, preBody, preBody, "")

	fullHash := components.SpanHash(norm)
	if !stash.CoversSpan(session, 4, fullHash) {
		t.Error("CoversSpan(4, fullHash) = false, want true — the sibling-inclusive span should be fully covered")
	}
	if stash.CoversSpan(session, 5, fullHash) {
		t.Error("CoversSpan(5, fullHash) = true, want false — only 4 normalized messages exist")
	}
	// A hash computed over a span that EXCLUDES the sibling must not satisfy a request for the
	// full (sibling-inclusive) span at the same count — proves the hash is sensitive to the
	// sibling's presence, not merely to the count.
	withoutSibling := []bschemas.ChatMessage{norm[0], norm[1], norm[3]} // drop norm[2], the sibling
	if stash.CoversSpan(session, 4, components.SpanHash(withoutSibling)) {
		t.Error("CoversSpan(4, hashWithoutSibling) = true, want false — the sibling must count toward the span's identity")
	}
}
