package all

import (
	"testing"

	"github.com/rossoctl/context-guru/components"
)

// Pipeline.HasOffload must agree with the Offload interface for every component the binary can
// build — that agreement is the whole reason the gate asks the interface instead of keeping a list
// of "lossy" component names, and it is what stops the list-shaped version of this from rotting.
//
// This lives in `all` rather than in `components` because the registry is only populated here: the
// blank imports in all.go are what run each component's init(). The same test inside `components`
// skipped for lack of registrations, which looked like coverage and was not.
//
// It walks the registry rather than a fixture, so a new lossy component needs no change here, and a
// component that quietly stops implementing Offload fails immediately.
func TestHasOffloadAgreesWithTheOffloadInterface(t *testing.T) {
	names := components.Names()
	if len(names) == 0 {
		t.Fatal("no components registered: this package's blank imports are what register them, " +
			"so an empty registry means the test is checking nothing")
	}
	var checked, offloaders int
	for _, name := range names {
		c, err := components.New(name, nil)
		if err != nil || c == nil {
			// A component that will not build from a nil config block; the proxy-level tests
			// exercise those with real configuration.
			continue
		}
		_, isOffload := c.(components.Offload)
		if isOffload {
			offloaders++
		}
		got := components.NewPipeline([]components.Component{c}, nil).HasOffload()
		if got != isOffload {
			t.Errorf("component %q: implements components.Offload = %v, but a pipeline holding "+
				"only it reports HasOffload() = %v", name, isOffload, got)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no component could be built from a nil config, so nothing was actually checked")
	}
	// Both sides must be non-empty or the agreement is trivial: all-false would agree with a
	// HasOffload that always returns false, which is the mirror-image defect.
	if offloaders == 0 {
		t.Errorf("checked %d components and none implement Offload — either the registry is not "+
			"what it should be, or this test would pass against a broken HasOffload", checked)
	}
	if offloaders == checked {
		t.Errorf("all %d checked components implement Offload, so this test would also pass "+
			"against a HasOffload that always returns true", checked)
	}
	t.Logf("checked %d of %d registered components; %d implement Offload", checked, len(names), offloaders)
}
