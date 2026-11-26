package schema

import (
	"reflect"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// CloneMessages copies a plain text message directly. Whatever shape goes in, the result must
// equal what the JSON round trip produces, because that is what every caller was written against.
func TestCloneMessagesMatchesTheJSONRoundTrip(t *testing.T) {
	str := func(s string) *schemas.ChatMessageContent { return &schemas.ChatMessageContent{ContentStr: &s} }
	name := "n"
	cases := []schemas.ChatMessage{
		{Role: "user", Content: str("hello")},
		{Role: "assistant", Content: str("")},
		{Role: "user", Content: str("café   \x00 \t \"q\" <b>&</b>")},
		{Role: "user", Content: str("bad utf8 \xff\xfe tail")},
		{Role: "", Content: str("no role")},
		{Role: "\xdb", Content: str("bad utf8 role")},
		{Role: "user", Name: &name, Content: str("named")},
		{Role: "user", Content: &schemas.ChatMessageContent{ContentBlocks: []schemas.ChatContentBlock{{Type: "text", Text: &name}}}},
		{Role: "user"},
	}
	got := CloneMessages(cases)
	want := cloneViaJSON(cases)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CloneMessages differs from the JSON round trip:\n got %#v\nwant %#v", got, want)
	}
	// And the copy is independent of the original.
	*cases[0].Content.ContentStr = "changed"
	if *got[0].Content.ContentStr != "hello" {
		t.Fatal("the clone aliases the original's text")
	}
}
