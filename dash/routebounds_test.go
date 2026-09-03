package dash

// routeBounds is a map keyed by route PATTERN STRING, which means a typo in a key is not an
// error — it is an entry that matches nothing and silently leaves that route on the default
// bound. That is the failure this file exists to catch, and it is the same class of silent
// mistake as the one routeBounds was introduced to fix: a timeout that looks configured and
// is not.

import (
	"testing"
	"time"
)

// Every key in routeBounds must name a route that is actually mounted.
func TestRouteBoundsHasNoDeadEntries(t *testing.T) {
	a, _ := newTestAPI(t, Options{})
	mounted := map[string]bool{}
	for _, rt := range a.routes() {
		mounted[rt.pattern] = true
	}
	for pattern := range routeBounds {
		if !mounted[pattern] {
			t.Errorf("routeBounds names %q, which is not a mounted route.\n"+
				"A key that matches nothing is not an error at compile time and not a failure at "+
				"runtime — the route just quietly keeps the default bound, which is exactly the "+
				"kind of timeout-that-looks-configured this map was added to eliminate.", pattern)
		}
	}
}

// And the reverse direction, as a prompt rather than a rule: a route left on the default is
// asserting that it is CHEAP. This does not fail for new routes — it cannot know their cost —
// but it does fail if someone removes a bound from one of the routes measured as expensive,
// because that is a regression to the state that produced the outage.
func TestExpensiveRoutesKeepTheirLongerBound(t *testing.T) {
	// Measured on the production database through the real handlers, unfiltered default view.
	// Each of these exceeded, or ran within a whisker of, the 10s default.
	measured := map[string]string{
		"GET /api/stats":            "~5.7s cold",
		"GET /api/facets":           "~1.9s, spikes under load",
		"GET /api/components":       "10.2-10.4s all-time — failed 100% of requests",
		"GET /api/tools":            "58.8s cold / 12.6s warm",
		"GET /api/toolfilter":       "~21s",
		"GET /api/prompt":           "~22s",
		"GET /api/kvcache":          "4.5-7.8s idle",
		"GET /api/kvcache/simulate": "7.7-8.6s idle — 86% of the old bound",
		"GET /api/kvcache/suggest":  "6.1-7.3s idle",
		"GET /api/keepalive":        "2.2-2.7s idle, timed out live",
	}
	for pattern, why := range measured {
		d, ok := routeBounds[pattern]
		if !ok || d <= dashHandlerTimeout {
			got := "the 10s default"
			if ok {
				got = d.String()
			}
			t.Errorf("%s is back on %s, but it was measured at %s.\n"+
				"Routes at or past the default bound return 503 to every reader the moment any "+
				"other load exists; that is the outage this map was written for.", pattern, got, why)
		}
	}
	if dashHeavyTimeout <= dashHandlerTimeout {
		t.Errorf("dashHeavyTimeout (%v) is not longer than dashHandlerTimeout (%v), so every "+
			"entry above is a no-op", dashHeavyTimeout, dashHandlerTimeout)
	}
	_ = time.Second
}
