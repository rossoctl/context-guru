package keepalive

import "testing"

// The deprecated HeadTTLMinTokens field still works for a host that sets it: tokens x 4 bytes, and
// the new field wins when both are set.
func TestHeadTTLGateBytes(t *testing.T) {
	cases := []struct {
		name string
		in   Policy
		want int
	}{
		{"unset: off", Policy{}, 0},
		{"old field only", Policy{HeadTTLMinTokens: 50000}, 200000},
		{"new field only", Policy{HeadTTLMinBytes: 7000}, 7000},
		{"both: new wins", Policy{HeadTTLMinBytes: 7000, HeadTTLMinTokens: 50000}, 7000},
	}
	for _, c := range cases {
		if got := c.in.HeadTTLGateBytes(); got != c.want {
			t.Errorf("%s: HeadTTLGateBytes() = %d, want %d", c.name, got, c.want)
		}
	}
}
