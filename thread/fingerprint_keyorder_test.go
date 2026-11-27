package thread

import "testing"

// A one-text-block array and the plain string it collapses to are the same message (the provider
// normalizes both). That must hold whatever order the client writes the block's keys in.
func TestFingerprintBlockKeyOrderDoesNotChangeTheMessage(t *testing.T) {
	str := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	for name, blk := range map[string]string{
		"type first":  `{"type":"text","text":"hello"}`,
		"text first":  `{"text":"hello","type":"text"}`,
		"with a mark": `{"text":"hello","cache_control":{"type":"ephemeral"},"type":"text"}`,
	} {
		arr := []byte(`{"messages":[{"role":"user","content":[` + blk + `]}]}`)
		a, ok1 := Fingerprint(str)
		b, ok2 := Fingerprint(arr)
		if !ok1 || !ok2 || len(a) != 1 || len(b) != 1 {
			t.Fatalf("%s: fingerprint failed: %v %v", name, ok1, ok2)
		}
		if a[0] != b[0] {
			t.Errorf("%s: the string form and the block form fingerprint differently", name)
		}
	}
}
