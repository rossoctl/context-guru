package reqfacts

import (
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

// conversation and item_reference hold history at the provider, as previous_response_id does (#1298):
// the input is only the newest turn, so the scan reports no tool results.
func TestScanLifecycleServerHeldHistoryForms(t *testing.T) {
	out := `{"type":"function_call_output","call_id":"c1","output":"result text"}`
	for name, body := range map[string]string{
		"conversation string": `{"conversation":"conv_1","input":[` + out + `]}`,
		"conversation object": `{"conversation":{"id":"conv_1"},"input":[` + out + `]}`,
		"item_reference":      `{"input":[{"type":"item_reference","id":"msg_1"},` + out + `]}`,
	} {
		if q := scanLifecycleBody(bschemas.OpenAI, []byte(body)); q != (Lifecycle{}) {
			t.Errorf("%s: got %+v, want no facts", name, q)
		}
	}
}
