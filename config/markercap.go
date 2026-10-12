package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// markerModeComponents are the components with a `marker_mode` field. Held to the registry by
// TestMarkerModeComponentsMatchTheRegistry, so a new offloader cannot be missed silently.
var markerModeComponents = map[string]bool{
	"agentdiet": true, "cache_aware_summarizer": true, "cmdfilter": true, "collapse": true,
	"coref": true, "dedup": true, "extract": true, "extract_llm": true,
	"extract_llm_sweep": true, "failed_run": true, "linecap": true, "mask": true,
	"readlifecycle": true, "skeleton": true, "smartcrush": true, "summarize": true,
}

var markerRank = map[string]int{"off": 0, "summary": 1, "full": 2, "": 2}

// CapMarkerMode lowers every pipeline component's marker_mode to at most max. A host that cannot
// run the expand loop (it cannot replace a streamed response) calls this after LoadBytes, so no
// preset can make content "reversible" through a tool call nothing will ever answer.
func (c *Config) CapMarkerMode(max string) error {
	if max != "summary" && max != "off" {
		return fmt.Errorf("config: CapMarkerMode(%q): want summary or off", max)
	}
	if c.Components == nil {
		c.Components = map[string]yaml.Node{}
	}
	for _, name := range c.Pipeline {
		if !markerModeComponents[name] {
			continue
		}
		m := map[string]any{}
		if n, ok := c.Components[name]; ok && n.Kind != 0 {
			if err := n.Decode(&m); err != nil {
				return fmt.Errorf("config: %s: %w", name, err)
			}
		}
		cur, _ := m["marker_mode"].(string)
		if markerRank[cur] > markerRank[max] {
			m["marker_mode"] = max
		}
		var n yaml.Node
		if err := n.Encode(m); err != nil {
			return fmt.Errorf("config: %s: %w", name, err)
		}
		c.Components[name] = n
	}
	return nil
}
