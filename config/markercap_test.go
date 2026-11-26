package config

import (
	"testing"

	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/offload"
	_ "github.com/rossoctl/context-guru/components/reformat"
)

// The list must match the components that actually accept the key: a component added later
// with a marker_mode field and not listed here would keep defaulting to full in Cortex.
func TestMarkerModeComponentsMatchTheRegistry(t *testing.T) {
	for _, name := range components.Names() {
		if name == "skeleton" {
			// skeleton (-tags cg_skeleton) has a marker_mode field but accepts only full, so it
			// is deliberately not capped and not listed. See markerModeComponents.
			continue
		}
		_, err := components.New(name, []byte("marker_mode: summary\n"))
		accepts := err == nil
		if accepts != markerModeComponents[name] {
			t.Errorf("%s: accepts marker_mode = %v, listed = %v (err: %v)",
				name, accepts, markerModeComponents[name], err)
		}
	}
}

func TestCapMarkerModeOverridesThePresetDefault(t *testing.T) {
	c, err := LoadBytes([]byte("preset: general\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CapMarkerMode("summary"); err != nil {
		t.Fatal(err)
	}
	for _, name := range c.Pipeline {
		if !markerModeComponents[name] {
			continue
		}
		var m map[string]any
		node := c.Components[name]
		if err := node.Decode(&m); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if m["marker_mode"] != "summary" {
			t.Errorf("%s: marker_mode = %v, want summary", name, m["marker_mode"])
		}
	}
	if _, err := c.Build(components.NopEmitter{}); err != nil {
		t.Fatalf("Build after CapMarkerMode: %v", err)
	}
}

func TestCapMarkerModeKeepsAStricterChoice(t *testing.T) {
	c, err := LoadBytes([]byte("pipeline: [collapse]\ncomponents:\n  collapse: {marker_mode: \"off\", max_tokens: 300}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CapMarkerMode("summary"); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	n := c.Components["collapse"]
	_ = n.Decode(&m)
	if m["marker_mode"] != "off" || m["max_tokens"] != 300 {
		t.Fatalf("collapse = %v, want marker_mode off and max_tokens kept", m)
	}
	if err := c.CapMarkerMode("full"); err == nil {
		t.Fatal(`CapMarkerMode("full") accepted`)
	}
}

// offload.parseMarkerMode maps every unrecognized value to full, so an unknown value must be
// capped like full, not left alone as if it ranked below summary.
func TestCapMarkerModeCapsAnUnrecognizedValueAsFull(t *testing.T) {
	for _, v := range []string{"Full", "fulll"} {
		c, err := LoadBytes([]byte("pipeline: [collapse]\ncomponents:\n  collapse: {marker_mode: \"" + v + "\"}\n"))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.CapMarkerMode("summary"); err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		n := c.Components["collapse"]
		if err := n.Decode(&m); err != nil {
			t.Fatal(err)
		}
		if m["marker_mode"] != "summary" {
			t.Errorf("marker_mode %q: after cap = %v, want summary", v, m["marker_mode"])
		}
	}
}
