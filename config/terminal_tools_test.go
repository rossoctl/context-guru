package config

import (
	"reflect"
	"testing"
)

// #857: absent is nil (shipped list), [] is a non-nil empty list (rule off), a list is kept.
func TestKeepAliveTerminalToolsYAML(t *testing.T) {
	cases := []struct {
		name, doc string
		want      []string
		wantNil   bool
	}{
		{"absent", "preset: off\ncache:\n  keepalive: true\n", nil, true},
		{"empty", "preset: off\ncache:\n  keepalive: true\n  keepalive_terminal_tools: []\n", []string{}, false},
		{"list", "preset: off\ncache:\n  keepalive_terminal_tools: [Finish]\n", []string{"Finish"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := LoadBytes([]byte(c.doc))
			if err != nil {
				t.Fatal(err)
			}
			got := cfg.Cache.KeepAliveTerminalTools
			if (got == nil) != c.wantNil || (!c.wantNil && !reflect.DeepEqual(got, c.want)) {
				t.Fatalf("got %#v (nil=%v), want %#v (nil=%v)", got, got == nil, c.want, c.wantNil)
			}
		})
	}
}

// #858: the wait-plan keys. Absent: the plan is on with the shipped counts. Present: kept.
func TestKeepAliveWaitPlanYAML(t *testing.T) {
	cfg, err := LoadBytes([]byte("preset: off\ncache:\n  keepalive: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Cache.WaitPlanOn() || cfg.Cache.KeepAliveWaitPlan != nil {
		t.Errorf("absent: plan on = %v, raw %v; want on and nil", cfg.Cache.WaitPlanOn(), cfg.Cache.KeepAliveWaitPlan)
	}
	r := cfg.Cache.Resolved()
	if r.KeepAliveToolWaitMaxPings != 8 || r.KeepAliveBackgroundExtraPings != 3 || r.KeepAliveHumanExtraPings != 1 {
		t.Errorf("absent: resolved = %d/%d/%d, want 8/3/1", r.KeepAliveToolWaitMaxPings,
			r.KeepAliveBackgroundExtraPings, r.KeepAliveHumanExtraPings)
	}
	cfg, err = LoadBytes([]byte("preset: off\ncache:\n  keepalive: true\n  keepalive_wait_plan: false\n" +
		"  keepalive_tool_wait_max_pings: 5\n  keepalive_background_extra_pings: 4\n  keepalive_human_extra_pings: 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	r = cfg.Cache.Resolved()
	if cfg.Cache.WaitPlanOn() || r.KeepAliveToolWaitMaxPings != 5 || r.KeepAliveBackgroundExtraPings != 4 ||
		r.KeepAliveHumanExtraPings != 2 {
		t.Errorf("present: on=%v %d/%d/%d, want off and 5/4/2", cfg.Cache.WaitPlanOn(),
			r.KeepAliveToolWaitMaxPings, r.KeepAliveBackgroundExtraPings, r.KeepAliveHumanExtraPings)
	}
	for _, bad := range []string{"keepalive_tool_wait_max_pings: 12", "keepalive_human_extra_pings: -1"} {
		if _, err := LoadBytes([]byte("preset: off\ncache:\n  " + bad + "\n")); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// #858: the default minimum prefix is 20000; an explicit value stays.
func TestKeepAliveMinPrefixDefaultIs20k(t *testing.T) {
	if got := (CacheConfig{}).Resolved().KeepAliveMinPrefixTokens; got != 20000 {
		t.Errorf("default = %d, want 20000", got)
	}
	if got := (CacheConfig{KeepAliveMinPrefixTokens: 80000}).Resolved().KeepAliveMinPrefixTokens; got != 80000 {
		t.Errorf("explicit 80000 became %d", got)
	}
}
