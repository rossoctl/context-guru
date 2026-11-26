package apply

import "testing"

// #1459: the old Opts.CanKeepAlive still works. It is read only when CanSweepAtPing is nil.
func TestSweepAtPingOfFallsBackToTheDeprecatedCanKeepAlive(t *testing.T) {
	yes := func(_, _ string) bool { return true }
	no := func(_, _ string) bool { return false }
	ask := func(o Opts) bool {
		f := canSweepAtPingFor(sweepAtPingOf(o), "s", "t")
		return f != nil && f()
	}
	if ask(Opts{}) {
		t.Error("neither set: true")
	}
	if !ask(Opts{CanSweepAtPing: yes}) {
		t.Error("new name: false")
	}
	if !ask(Opts{CanKeepAlive: yes}) {
		t.Error("old name: false")
	}
	if ask(Opts{CanSweepAtPing: no, CanKeepAlive: yes}) {
		t.Error("new name must win over the old one")
	}
}
