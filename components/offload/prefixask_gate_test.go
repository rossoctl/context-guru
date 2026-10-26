package offload

import (
	"testing"

	"github.com/rossoctl/context-guru/components"
)

// #721: the one decision on whether a prefix-ask answer used the cache.
func TestPrefixAskCacheOK(t *testing.T) {
	for _, tc := range []struct {
		name        string
		read, write int
		turn        int
		capOn       bool
		ok          bool
		reason      string
	}{
		{"A5: read equals the turn, small write", 25512, 53, 25512, false, true, ""},
		{"read above the turn", 25588, 6, 25512, false, true, ""},
		{"model switch: read far below the turn", 13440, 12000, 41886, false, false, prefixAskReadBelowTurn},
		{"read one below the turn", 25511, 0, 25512, false, false, prefixAskReadBelowTurn},
		{"read of zero", 0, 0, 25512, false, false, prefixAskReadZero},
		{"read of zero, turn unknown", 0, 0, 0, false, false, prefixAskReadZero},
		{"turn unknown, any read above 0 passes", 100, 5000, 0, false, true, ""},
		{"typed prompt: question rewrites the text turn's tail, cap on", 25516, 300, 25516, true, true, ""},
		{"typed prompt: a tail of a few hundred, cap off", 25516, 300, 25516, false, true, ""},
		{"large write, cap off: not the rule", 25512, 12142, 25512, false, true, ""},
		{"large write, cap on", 25512, 12142, 25512, true, false, prefixAskWriteOverCap},
		{"small write, cap on", 25512, 53, 25512, true, true, ""},
		{"write at the 2000 ceiling, big prefix, cap on", 200000, 2000, 200000, true, true, ""},
		{"write over the 2000 ceiling, cap on", 200000, 2001, 200000, true, false, prefixAskWriteOverCap},
		{"write over 2 percent of a small prefix, cap on", 1000, 100, 1000, true, false, prefixAskWriteOverCap},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := prefixAskJudge(components.PrefixUsage{CacheRead: tc.read, CacheWrite: tc.write}, tc.turn, tc.capOn)
			if ok != tc.ok || reason != tc.reason {
				t.Errorf("got (%v, %q), want (%v, %q)", ok, reason, tc.ok, tc.reason)
			}
		})
	}
}

// The shipped switch is on, so the shipped decision applies the cap (#721).
func TestPrefixAskCacheOKShipsWithTheCapOn(t *testing.T) {
	if ok, why := prefixAskCacheOK(components.PrefixUsage{CacheRead: 25512, CacheWrite: 12142}, 25512); ok || why != prefixAskWriteOverCap {
		t.Fatalf("got (%v, %q), want the cap to refuse a 12,142 write", ok, why)
	}
	if ok, why := prefixAskCacheOK(components.PrefixUsage{CacheRead: 25516, CacheWrite: 300}, 25516); !ok {
		t.Fatalf("a typed-prompt tail write of 300 was refused: %q", why)
	}
}
