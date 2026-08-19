package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// MODEL_INFO=off disables the FETCHED public price map — a network dependency, which is what
// an operator turns off. It must not also discard a price list they configured by path: with
// the check ahead of the load, MODEL_PRICES was never opened, so nothing was validated,
// nothing was logged, and every request priced as `partial` — the silent failure the
// fatal-on-malformed rule exists to prevent, arrived at from the other direction.
func TestModelPricesSurviveModelInfoOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.yaml")
	if err := os.WriteFile(path, []byte(`models: [{match: "aws/claude-sonnet-5", in: 1.52, out: 7.60}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MODEL_PRICES", path)
	t.Setenv("MODEL_INFO", "off")

	r := modelWindows()
	if r == nil {
		t.Fatal("MODEL_INFO=off dropped the resolver entirely, so the configured price list is gone")
	}
	p, ok := r.(modelinfo.Pricer)
	if !ok {
		t.Fatal("the resolver cannot price")
	}
	pr, found := p.Price(context.Background(), "aws/claude-sonnet-5")
	if !found || pr.Input == 0 {
		t.Fatal("the price list was not consulted with MODEL_INFO=off")
	}
	// And with no local list, off still means off.
	t.Setenv("MODEL_PRICES", "")
	if modelWindows() != nil {
		t.Error("MODEL_INFO=off with no price list should resolve nothing")
	}
}
