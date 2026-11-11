package main

import (
	"testing"

	"github.com/rossoctl/context-guru/config"
)

// The cache policy carries the head gate in bytes. An old config with only head_ttl_min_tokens
// gates at tokens x 4 bytes, as the old bytes/4 test did.
func TestCachePolicyCarriesTheHeadGateInBytes(t *testing.T) {
	cases := []struct {
		name string
		in   config.CacheConfig
		want int
	}{
		{"off: zero", config.CacheConfig{KeepAlive: true}, 0},
		{"on, nothing set: default", config.CacheConfig{HeadTTL1h: true}, 200000},
		{"on, old key only", config.CacheConfig{HeadTTL1h: true, HeadTTLMinTokens: 50000}, 200000},
		{"on, new key wins", config.CacheConfig{HeadTTL1h: true, HeadTTLMinTokens: 50000, HeadTTLMinBytes: 7000}, 7000},
	}
	for _, c := range cases {
		if got := cachePolicy(c.in).HeadTTLMinBytes; got != c.want {
			t.Errorf("%s: HeadTTLMinBytes = %d, want %d", c.name, got, c.want)
		}
	}
}
