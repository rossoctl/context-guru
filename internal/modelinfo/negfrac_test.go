package modelinfo

import "testing"

func TestParseTableRejectsNegativeCacheWriteFrac(t *testing.T) {
	if _, err := ParseTable([]byte("cache_write_frac: -1\nmodels:\n  - {match: x, in: 1, out: 2}\n")); err == nil {
		t.Fatal("a negative cache_write_frac must not load")
	}
	if _, err := ParseTable([]byte("cache_write_frac: 0\nmodels:\n  - {match: x, in: 1, out: 2}\n")); err != nil {
		t.Fatalf("0 means default: %v", err)
	}
}
