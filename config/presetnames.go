package config

import "sort"

// PresetNames lists every registered preset, sorted. Served to the settings page so a
// user can only choose a preset the server will actually accept — a dropdown built from
// a hardcoded list in the UI is a dropdown that goes stale the moment a preset is added
// or removed.
func PresetNames() []string {
	out := make([]string, 0, len(presets))
	for name := range presets {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
