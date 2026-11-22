package offload

import (
	"testing"

	"github.com/rossoctl/context-guru/components"
)

// The owner's counter on #1196: each request counts its cache verdict once, by reason, as the event
// sweep_cold_verdict_<reason>. One case per reason. The counted reason and the verdict the sweep acts
// on come from one function, so the second check ties them together.
func TestTheCacheVerdictIsCountedOnceByReason(t *testing.T) {
	unknown := func(c *components.Ctx) { c.CacheAware, c.CacheTTLMs, c.MaxCachedIdx = false, 0, -1 }
	for _, tc := range []struct {
		name     string
		prep     func(c *components.Ctx)
		read     int // the cache read of the previous turn
		want     string
		wantCold bool
	}{
		{"known phase, cache gone", func(c *components.Ctx) { c.ColdCache = true }, 0, verdictKnownCold, true},
		{"known phase, cache live", func(c *components.Ctx) {}, 0, verdictKnownWarm, false},
		{"unknown phase, known cached prefix", func(c *components.Ctx) { unknown(c); c.CacheAware, c.MaxCachedIdx = true, 3 }, 0, verdictUnknownPrefix, false},
		{"unknown phase, the previous turn read the cache", unknown, 5000, verdictUnknownLive, false},
		{"unknown phase, the previous turn read nothing", unknown, 0, verdictUnknownCold, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newWarmRig(t, "w-verdict-"+tc.want, "warm: true\nwarm_every_n: 50\nwarm_min_age_turns: 5\n", payingTail(), 1.0)
			c := warmCtx(r.session, r.st)
			tc.prep(c)
			r.request(c) // the first request of the thread creates its warm state
			ResolveWarmQuestion(r.session, "", true, tc.read)

			c = warmCtx(r.session, r.st)
			tc.prep(c)
			_, rep := r.request(c)

			for _, reason := range []string{verdictKnownCold, verdictKnownWarm, verdictUnknownPrefix, verdictUnknownLive, verdictUnknownCold} {
				want := 0
				if reason == tc.want {
					want = 1
				}
				if got := rep.Events[eventVerdictPrefix+reason]; got != want {
					t.Errorf("%s = %d, want %d (events %v)", eventVerdictPrefix+reason, got, want, rep.Events)
				}
			}
			cold, why := sweepVerdict(c)
			if cold != tc.wantCold || why != tc.want || sweepColdFor(c) != cold {
				t.Errorf("sweepVerdict = (%v, %q), want (%v, %q)", cold, why, tc.wantCold, tc.want)
			}
		})
	}
}
