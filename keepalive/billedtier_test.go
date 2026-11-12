package keepalive

import (
	"testing"

	"github.com/tidwall/gjson"
)

// BilledOneHour is the response half of ResolveTier: for a request that asked 1h on every
// breakpoint, the two return the same answer for every usage and every remembered tier (#1078).
func TestBilledOneHourIsResolveTiersResponseHalf(t *testing.T) {
	body := []byte(`{"system":[{"type":"text","text":"x","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[]}`)
	if !gjson.ValidBytes(body) {
		t.Fatal("bad fixture")
	}
	for _, u := range []Usage{
		{}, {CacheRead: 100}, {CacheWrite: 100}, {CacheWrite: 100, CacheWrite1h: 40},
		{CacheWrite: 100, CacheWrite1h: 100},
	} {
		for _, prev := range []bool{false, true} {
			if got, want := BilledOneHour(u.CacheWrite, u.CacheWrite1h, prev), ResolveTier(body, u, true, prev); got != want {
				t.Errorf("usage %+v prev %v: BilledOneHour = %v, ResolveTier = %v", u, prev, got, want)
			}
		}
	}
}
