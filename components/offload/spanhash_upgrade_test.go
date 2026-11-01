package offload

import (
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
)

// A summary checkpoint stores summaryCoveredHash. One written before #783 over a span with a
// one-text-block message must still be reused after the upgrade, with and without an adapter
// stash payload (Responses).
func TestASummaryCheckpointStoredByTheOldRuleIsStillReused(t *testing.T) {
	text := "go"
	span := []bschemas.ChatMessage{{Role: bschemas.ChatMessageRoleUser, Content: &bschemas.ChatMessageContent{
		ContentBlocks: []bschemas.ChatContentBlock{{Type: bschemas.ChatContentBlockTypeText, Text: &text,
			CacheControl: &bschemas.CacheControl{Type: bschemas.CacheControlTypeEphemeral}}}}}}
	asString := []bschemas.ChatMessage{{Role: bschemas.ChatMessageRoleUser, Content: &bschemas.ChatMessageContent{ContentStr: &text}}}
	payload := func(int, int, []bschemas.ChatMessage) ([]byte, error) { return []byte(`[{"x":1}]`), nil }
	for name, c := range map[string]*components.Ctx{"plain": {}, "stash payload": {SummaryStashPayload: payload}} {
		old := components.LegacySpanHash(span)
		if c.SummaryStashPayload != nil {
			b, _ := c.SummaryStashPayload(0, 1, span)
			old = hashKey(string(b) + ":" + old)
		}
		if old == summaryCoveredHash(c, 0, 1, span) {
			t.Fatalf("%s: precondition: the old and the new hash must differ", name)
		}
		if !summaryCoveredHashMatches(c, 0, 1, span, old) {
			t.Errorf("%s: a checkpoint stored by the old rule read as a different span", name)
		}
		if !summaryCoveredHashMatches(c, 0, 1, asString, summaryCoveredHash(c, 0, 1, span)) {
			t.Errorf("%s: the string form of the covered message read as a different span", name)
		}
		other := "stop"
		if summaryCoveredHashMatches(c, 0, 1, []bschemas.ChatMessage{{Role: bschemas.ChatMessageRoleUser,
			Content: &bschemas.ChatMessageContent{ContentStr: &other}}}, old) {
			t.Errorf("%s: a different span matched", name)
		}
	}
}
