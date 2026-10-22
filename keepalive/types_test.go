package keepalive

import (
	"math"
	"testing"
	"time"
)

func TestPolicyCeilingResolvesZeroToTheDefault(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{{0, DefaultMaxUSDPerPing}, {0.1, 0.1}, {-1, math.Inf(1)}}
	for _, c := range cases {
		if got := (Policy{MaxUSDPerPing: c.in}).Ceiling(); got != c.want {
			t.Errorf("Ceiling(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestPolicyOnNeedsAllThree(t *testing.T) {
	on := Policy{KeepAlive: true, Idle: time.Minute, MaxPings: 2}
	if !on.On() {
		t.Fatal("complete policy reported off")
	}
	for _, p := range []Policy{
		{Idle: time.Minute, MaxPings: 2},
		{KeepAlive: true, MaxPings: 2},
		{KeepAlive: true, Idle: time.Minute},
	} {
		if p.On() {
			t.Errorf("%+v reported on", p)
		}
	}
}

func TestDisabledReadsTheKillSwitch(t *testing.T) {
	for _, v := range []string{"off", "0", "false", " OFF "} {
		t.Setenv("CONTEXT_GURU_KEEPALIVE", v)
		if !Disabled() {
			t.Errorf("CONTEXT_GURU_KEEPALIVE=%q did not disable", v)
		}
	}
	t.Setenv("CONTEXT_GURU_KEEPALIVE", "")
	if Disabled() {
		t.Error("empty CONTEXT_GURU_KEEPALIVE disabled the keep-alive")
	}
}
