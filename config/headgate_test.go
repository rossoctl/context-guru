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

// A settings save posts only keepalive and head_ttl_1h. Every tuning key it does not send stays.
func TestApplyFormKeepsEveryCacheTuningKeyItDoesNotSend(t *testing.T) {
	keys := []string{
		"keepalive_idle_seconds: 200", "keepalive_openai_idle_seconds: 90", "keepalive_max_pings: 3",
		"keepalive_min_prefix_tokens: 12000", "keepalive_max_usd_per_ping: 0.07",
	}
	doc := "preset: off\ncache:\n  keepalive: true\n  " + strings.Join(keys, "\n  ") + "\n"
	out, err := ApplyForm(doc, Form{Cache: &CacheForm{KeepAlive: false, HeadTTL1h: true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if !strings.Contains(out, k) {
			t.Errorf("a settings save dropped %q:\n%s", k, out)
		}
	}
	if !strings.Contains(out, "keepalive: false") {
		t.Errorf("the keepalive switch was not saved:\n%s", out)
	}
}

// "Back to the default" is said by name in CacheReset, because 0 means "not stated".
func TestApplyFormCacheResetDeletesNamedKeysOnly(t *testing.T) {
	doc := "preset: off\ncache:\n  keepalive_max_pings: 3\n  keepalive_idle_seconds: 200\n  head_ttl_min_bytes: 300000\n"
	out, err := ApplyForm(doc, Form{Cache: &CacheForm{}, CacheReset: []string{"keepalive_max_pings", "head_ttl_min_bytes"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"keepalive_max_pings", "head_ttl_min_bytes"} {
		if strings.Contains(out, gone) {
			t.Errorf("%s survived a reset:\n%s", gone, out)
		}
	}
	if !strings.Contains(out, "keepalive_idle_seconds: 200") {
		t.Errorf("an unnamed key was deleted:\n%s", out)
	}
}

// A typo in cache_reset must fail, not report success. The handler returns this error as a 400.
func TestApplyFormRejectsAnUnknownCacheResetName(t *testing.T) {
	doc := "preset: off\ncache:\n  keepalive_max_pings: 3\n"
	_, err := ApplyForm(doc, Form{Cache: &CacheForm{}, CacheReset: []string{"keepalive_max_ping"}})
	if err == nil || !strings.Contains(err.Error(), "keepalive_max_ping") || !strings.Contains(err.Error(), "keepalive_max_pings") {
		t.Fatalf("want an error naming the typo and the valid keys, got %v", err)
	}
}
