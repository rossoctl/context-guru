package offload

import (
	"testing"

	"github.com/rossoctl/context-guru/components"
)

// #1459: the old Ctx.CanKeepAlive still works. It is read only when CanSweepAtPing is nil.
func TestPingableReadsTheDeprecatedCanKeepAlive(t *testing.T) {
	yes, no := func() bool { return true }, func() bool { return false }
	for _, tc := range []struct {
		name string
		c    components.Ctx
		want bool
	}{
		{"neither set", components.Ctx{}, false},
		{"new name", components.Ctx{CanSweepAtPing: yes}, true},
		{"old name", components.Ctx{CanKeepAlive: yes}, true},
		{"old name, false", components.Ctx{CanKeepAlive: no}, false},
		{"new name wins over the old one", components.Ctx{CanSweepAtPing: no, CanKeepAlive: yes}, false},
	} {
		if got := pingable(&tc.c); got != tc.want {
			t.Errorf("%s: pingable = %v, want %v", tc.name, got, tc.want)
		}
	}
}
