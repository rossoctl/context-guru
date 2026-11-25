package waitcause

import (
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

// conversation and item_reference hold history at the provider, as previous_response_id does (#1298).
// The body is then only the newest turn, so both the scan and the tracker treat it as unknown.
func TestServerHeldHistoryFormsAreUnknown(t *testing.T) {
	for name, body := range map[string]string{
		"conversation string": `{"conversation":"conv_1","input":[{"role":"user","content":"a"}]}`,
		"conversation object": `{"conversation":{"id":"conv_1"},"input":[{"role":"user","content":"a"}]}`,
		"item_reference":      `{"input":[{"type":"item_reference","id":"msg_1"},{"role":"user","content":"a"}]}`,
	} {
		if s := ScanBody(nil, bschemas.OpenAI, []byte(body)); s != nil {
			t.Errorf("%s: scan = %+v, want unknown (nil)", name, s)
		}
		obs := New().Observe("t", "s", "", bschemas.OpenAI, []byte(body))
		if obs.WakeCause != "" || obs.MsgCount != 0 {
			t.Errorf("%s: want the zero Observation, got %+v", name, obs)
		}
	}
}
