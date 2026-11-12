package config

import (
	"reflect"
	"testing"
)

// The Codex plugin writes `cache: strategy: <name>`. Older installs have `cache: keepalive: true|false`.
// Both forms must give the same effective config, apart from the Strategy name itself.
func TestCodexStrategyFormsGiveTheSameConfig(t *testing.T) {
	cases := []struct{ strategy, keepalive string }{
		{"30-min-ping", "true"},
		{"none", "false"},
	}
	for _, c := range cases {
		for _, preset := range []string{"off", "conservative", "medium", "high", "xhigh", "ultrahigh"} {
			head := "preset: " + preset + "\ncache:\n"
			a, err := LoadBytes([]byte(head + "  strategy: " + c.strategy + "\n"))
			if err != nil {
				t.Fatalf("%s/%s: %v", c.strategy, preset, err)
			}
			b, err := LoadBytes([]byte(head + "  keepalive: " + c.keepalive + "\n"))
			if err != nil {
				t.Fatalf("%s/%s: %v", c.strategy, preset, err)
			}
			if a.Cache.Strategy != c.strategy {
				t.Errorf("%s: strategy not kept: %q", c.strategy, a.Cache.Strategy)
			}
			a.Cache.Strategy = ""
			if !reflect.DeepEqual(a, b) {
				t.Errorf("%s/%s: effective config differs:\nstrategy form: %+v\nkeepalive form: %+v", c.strategy, preset, a, b)
			}
		}
	}
}

// Every preset the Codex picker offers is registered in the proxy.
func TestCodexPickerPresetsAreRegistered(t *testing.T) {
	known := map[string]bool{}
	for _, n := range PresetNames() {
		known[n] = true
	}
	for _, n := range []string{"off", "conservative", "medium", "high", "xhigh", "ultrahigh"} {
		if !known[n] {
			t.Errorf("preset %q is not registered in the proxy", n)
		}
	}
}

// The Codex picker offers a strategy only when ALL its providers are openai or any. A row for
// anthropic + openai is not offered. This mirrors offered_strategies in codex_plugin.py.
func TestCodexOffersTheOpenAIAndAnyStrategies(t *testing.T) {
	var got []string
	for _, s := range Strategies {
		ok := len(s.Providers) > 0
		for _, p := range s.Providers {
			if p != "openai" && p != "any" {
				ok = false
			}
		}
		if ok {
			got = append(got, s.Name)
		}
	}
	if !reflect.DeepEqual(got, []string{"none", "30-min-ping"}) {
		t.Errorf("Codex would offer %v, want [none 30-min-ping]", got)
	}
}
