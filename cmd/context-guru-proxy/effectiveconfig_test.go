package main

import (
	"testing"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/store"
)

// /config MUST PUBLISH WHAT THE STORE WILL USE.
//
// stash_ttl_seconds is capped at ttl_seconds inside NewMemory, silently. effectiveConfig published
// the CONFIGURED field, so `stash_ttl_seconds: 20000` with `ttl_seconds: 10000` showed 20000 on
// /config and the dashboard while the store used 10000 — an operator told one number while another
// runs, which is #200's shape in the config surface instead of the metrics one.
//
// This test lives HERE rather than in store, and that is the point of it: a store-side test that
// compares store.EffectiveStashTTLSeconds against a store built from the same Options is true by
// construction and cannot fail, so main.go could regress with every store test still green. The
// drift is between the PUBLISHED map and the store, so the assertion has to span both.
func TestConfigPublishesTheEffectivePayloadHorizon(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want int
	}{
		// The case that diverged.
		{"a payload horizon over the decision horizon", "store: {ttl_seconds: 10000, stash_ttl_seconds: 20000}\n", 10000},
		// And the ones that did not, which must keep working.
		{"a shorter payload horizon", "store: {ttl_seconds: 10000, stash_ttl_seconds: 600}\n", 600},
		{"defaults", "store: {}\n", int(store.DefaultStashTTL.Seconds())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.LoadBytes([]byte("preset: codesafe\n" + tc.yaml))
			if err != nil {
				t.Fatal(err)
			}
			eff := effectiveConfig(cfg, ":0", "", "", "", "", false, "")
			st, ok := eff["store"].(map[string]any)
			if !ok {
				t.Fatalf("no store block in the published config: %#v", eff)
			}
			got, ok := st["stash_ttl_seconds"].(int)
			if !ok {
				t.Fatalf("stash_ttl_seconds is %T, not an int: %#v", st["stash_ttl_seconds"], st)
			}
			if got != tc.want {
				t.Errorf("/config publishes stash_ttl_seconds=%d, want %d — the dashboard would "+
					"advertise a horizon the store does not use", got, tc.want)
			}
			// The published value and the store's must be the same number, whatever it is. This is
			// the assertion that actually spans the drift: it reads the map main.go builds and the
			// store the same Options produce.
			if want := store.EffectiveStashTTLSeconds(cfg.Store); got != want {
				t.Errorf("/config publishes %d but a store from the same Options uses %d", got, want)
			}
		})
	}
}
