package keepalive

import (
	"testing"

	"github.com/rossoctl/context-guru/internal/waitcause"
)

// The prefix floor follows the wait cause: 20k for a tool wait and for any subagent wait, 80k for the
// other waits of a main thread. With the plan off, one floor.
func TestWaitMinPrefixByCause(t *testing.T) {
	p := Policy{WaitPlan: true, MinPrefixTokens: 80000}
	for _, c := range []struct {
		cause    string
		subagent bool
		want     int
	}{
		{waitcause.WaitingOwnTool, false, 20000},
		{waitcause.WaitingOwnTool, true, 20000},
		{waitcause.WaitingHuman, false, 80000},
		{waitcause.WaitingBackground, false, 20000},
		{waitcause.WaitingTeammate, false, 20000},
		{waitcause.WaitingUnknown, false, 80000},
		{waitcause.WaitingHuman, true, 20000},
		{waitcause.WaitingBackground, true, 20000},
		{"", false, 80000},
	} {
		if got := p.WaitMinPrefix(c.cause, c.subagent); got != c.want {
			t.Errorf("%q subagent=%v: floor %d, want %d", c.cause, c.subagent, got, c.want)
		}
	}
	off := p
	off.WaitPlan = false
	if got := off.WaitMinPrefix(waitcause.WaitingOwnTool, true); got != 80000 {
		t.Errorf("plan off: floor %d, want the one floor 80000", got)
	}
	// The tool-wait floor never raises the general one.
	low := Policy{WaitPlan: true, MinPrefixTokens: 10000}
	if got := low.WaitMinPrefix(waitcause.WaitingOwnTool, false); got != 10000 {
		t.Errorf("general floor 10000: tool-wait floor %d, want 10000", got)
	}
	custom := Policy{WaitPlan: true, MinPrefixTokens: 80000, ToolWaitMinPrefix: 30000}
	if got := custom.WaitMinPrefix(waitcause.WaitingOwnTool, false); got != 30000 {
		t.Errorf("custom tool-wait floor: %d, want 30000", got)
	}
}

// The gate of a kept entry reads the floor of the entry's own wait.
func TestPingableUsesTheFloorOfTheWait(t *testing.T) {
	pol := Policy{WaitPlan: true, KeepAlive: true, Idle: 240e9, MaxPings: 2, MinPrefixTokens: 80000}
	mk := func(wait, thread string) *kaEntry {
		return &kaEntry{turn: 1, prefix: 30000, pol: pol, wait: wait, thread: thread}
	}
	if !mk(waitcause.WaitingOwnTool, "").pingable() {
		t.Error("a main tool wait at 30k was not pingable")
	}
	if !mk(waitcause.WaitingHuman, "a:x").pingable() {
		t.Error("a subagent wait at 30k was not pingable")
	}
	if !mk(waitcause.WaitingBackground, "").pingable() || !mk(waitcause.WaitingTeammate, "").pingable() {
		t.Error("a main wait for background work or a teammate at 30k was not pingable")
	}
	if mk(waitcause.WaitingUnknown, "").pingable() {
		t.Error("a main wait with an unknown cause at 30k was pingable")
	}
	if mk(waitcause.WaitingHuman, "").pingable() {
		t.Error("a main wait for a person at 30k was pingable")
	}
}
