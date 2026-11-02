package keepalive

import (
	"net/http"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

// recordWithTools records two turns on sess1; the second response ends with the given tool calls.
func recordWithTools(k *Keeper, at time.Time, names []string) {
	for i, ts := range []time.Time{at.Add(-time.Second), at} {
		r := Record{Key: sess1, StartedAt: ts, Body: []byte(kaBody), Provider: bschemas.Anthropic,
			Route: "/v1/messages", Status: http.StatusOK,
			Usage:   Usage{CacheRead: 48576, CacheWrite: 1200, StopReason: "tool_use"},
			UsageOK: true, Policy: kaPolicy(), Header: kaHeader(), Hold: fakeOf(k).hold(sess1)}
		if i == 1 {
			r.ToolUseNames = names
		}
		k.Record(r)
	}
}

// #857: a thread whose last response is a terminal tool call is done. It gets no ping.
func TestDoneThreadGetsNoPing(t *testing.T) {
	k, fd, clock := testKeeper(t)
	start := clock.now()
	recordWithTools(k, start, []string{"Read", "StructuredOutput"})
	if n := k.Sweep(start.Add(280 * time.Second)); n != 0 {
		t.Fatalf("a done thread was pinged (%d)", n)
	}
	if fd.n() != 0 {
		t.Fatalf("a done thread sent %d requests", fd.n())
	}
	if got := fd.holds(sess1); got != 0 {
		t.Errorf("credential still held %d time(s) for a done thread", got)
	}
}

// The terminal tool must be LAST. Another call after it means the thread goes on.
func TestTerminalToolNotLastIsStillPinged(t *testing.T) {
	k, _, clock := testKeeper(t)
	start := clock.now()
	recordWithTools(k, start, []string{"StructuredOutput", "Bash"})
	if n := k.Sweep(start.Add(280 * time.Second)); n != 1 {
		t.Fatalf("thread with a later tool call was not pinged (%d)", n)
	}
}

// An empty list is the switch: today's behavior, the done thread is pinged.
func TestEmptyTerminalListKeepsTodaysPings(t *testing.T) {
	k, _, clock := testKeeperWith(t, func(o *Options) { o.TerminalTools = []string{} })
	start := clock.now()
	recordWithTools(k, start, []string{"StructuredOutput"})
	if n := k.Sweep(start.Add(280 * time.Second)); n != 1 {
		t.Fatalf("with the rule off, the thread was not pinged (%d)", n)
	}
}

// A configured list replaces the default.
func TestCustomTerminalList(t *testing.T) {
	k, _, clock := testKeeperWith(t, func(o *Options) { o.TerminalTools = []string{"Finish"} })
	start := clock.now()
	recordWithTools(k, start, []string{"Finish"})
	if n := k.Sweep(start.Add(280 * time.Second)); n != 0 {
		t.Fatalf("custom terminal tool was pinged (%d)", n)
	}
}
