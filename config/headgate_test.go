package config

import (
	"strings"
	"testing"
)

// The settings page posts only keepalive and head_ttl_1h. A save must keep a head gate tuned
// elsewhere, in either spelling.
func TestApplyFormKeepsTheHeadGateWhenTheFormDoesNotSendIt(t *testing.T) {
	for _, key := range []string{"head_ttl_min_bytes: 300000", "head_ttl_min_tokens: 50000"} {
		doc := "preset: off\ncache:\n  keepalive: false\n  head_ttl_1h: true\n  " + key + "\n"
		out, err := ApplyForm(doc, Form{Cache: &CacheForm{KeepAlive: true, HeadTTL1h: true}})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, key) {
			t.Errorf("a settings save dropped %q:\n%s", key, out)
		}
	}
}

// tokens x 4 must not overflow, so both keys have an upper bound.
func TestHeadGateUpperBound(t *testing.T) {
	for _, doc := range []string{
		"preset: off\ncache:\n  head_ttl_min_tokens: 9000000000000000000\n",
		"preset: off\ncache:\n  head_ttl_min_bytes: 9000000000000000000\n",
		"preset: off\ncache:\n  head_ttl_min_bytes: 1073741825\n",
	} {
		if _, err := LoadBytes([]byte(doc)); err == nil {
			t.Errorf("accepted an out-of-range gate: %q", doc)
		}
	}
	if _, err := LoadBytes([]byte("preset: off\ncache:\n  head_ttl_min_bytes: 1073741824\n")); err != nil {
		t.Errorf("refused the largest allowed gate: %v", err)
	}
}
