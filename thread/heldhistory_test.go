package thread

import "testing"

// A body that leaves history at the provider has no message list to fingerprint: conversation and
// item_reference hold it as previous_response_id does (#1298).
func TestFingerprintServerHeldHistoryForms(t *testing.T) {
	for name, body := range map[string]string{
		"conversation string": `{"conversation":"conv_1","input":[{"role":"user","content":"hi"}]}`,
		"conversation object": `{"conversation":{"id":"conv_1"},"input":[{"role":"user","content":"hi"}]}`,
		"item_reference":      `{"input":[{"type":"item_reference","id":"msg_1"},{"role":"user","content":"hi"}]}`,
	} {
		if _, ok := Fingerprint([]byte(body)); ok {
			t.Errorf("%s: fingerprint reported a message list", name)
		}
	}
}
