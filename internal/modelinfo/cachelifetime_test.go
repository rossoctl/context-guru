package modelinfo

import (
	"strings"
	"testing"
	"time"
)

func TestCacheLifetime(t *testing.T) {
	for _, c := range []struct {
		provider string
		model    string
		oneHour  bool
		want     time.Duration
	}{
		{"anthropic", "", false, 5 * time.Minute},
		{"anthropic", "", true, time.Hour},
		{"bedrock", "", true, time.Hour},
		{"", "", false, 5 * time.Minute},
		{"openai", "gpt-5.6", false, 30 * time.Minute},
		{"openai", "gpt-5.6", true, 30 * time.Minute},
		{"openai", "azure/gpt-5.7-mini", false, 30 * time.Minute},
		{"openai", "gpt-4o", false, 5 * time.Minute},
		{"openai", "gpt-4.1", false, 5 * time.Minute},
		{"openai", "", false, 5 * time.Minute},
	} {
		if got := CacheLifetime(c.provider, c.model, c.oneHour); got != c.want {
			t.Errorf("CacheLifetime(%q,%q,%v)=%v want %v", c.provider, c.model, c.oneHour, got, c.want)
		}
	}
}

func TestCacheLifetimeSQLUsesTheSameValues(t *testing.T) {
	q := CacheLifetimeSQL("p", "h > 0", time.Second)
	for _, want := range []string{"p = 'openai' THEN 1800", "h > 0 THEN 3600", "ELSE 300"} {
		if !strings.Contains(q, want) {
			t.Errorf("%q lacks %q", q, want)
		}
	}
	if q := CacheLifetimeSQL("p", "h", time.Millisecond); !strings.Contains(q, "THEN 1800000") {
		t.Errorf("ms form wrong: %q", q)
	}
}
