package config

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The preset tables in the docs must match the presets the product actually ships — as a SET,
// in both directions: every preset has a row, every row is a preset, and every row's pipeline is
// the one that runs.
//
// This guard exists because they did not. Auditing the "What each one runs" table in
// docs/how-to/choose-a-preset.md against the `presets` map in config/config.go found EVERY row
// stale: three presets (`codesmart`, `coding`, `general`) were still documented as running
// `toon`, which was retired from them after it acted 0 times on 5,752 production requests and
// converted 0 candidates in 11.67M measured tokens, and every row omitted components that do
// run — the lossless pair (`textclean`, `searchfold`) and `linecap`. Three presets had no row at
// all (`agentdiet`, `house`, `housellm`). docs/reference/presets.md was correct at the same
// moment, so the two documents contradicted each other and a reader had no way to tell which one
// was lying.
//
// That is worse than an out-of-date sentence: the table is what somebody reads to decide whether
// a preset does anything they object to, and the answer it gave was wrong in the direction that
// matters — it named a component that does not run and omitted three that do.
//
// The set check in BOTH directions is deliberate, and it is what the first version of this guard
// got wrong: it iterated the rows it happened to find and asked only "is this row right?", with a
// hand-tuned coverage floor standing in for completeness. Against 12 multi-component presets that
// floor tolerated deleting seven rows, and a preset that ships with no row is exactly the defect
// this PR fixed by hand. The reverse direction matters just as much: a documented preset that
// does not exist is not a cosmetic error, it is a STARTUP failure —
//
//	$ PRESET=cache ./context-guru-proxy
//	config: config: unknown preset "cache"      # process exits
//
// the same class as the `skeleton`/`coding` incident this repo's own comments describe.
//
// Same reasoning as deploy/harbor/pipeline_drift_test.go, applied to the docs instead of the
// benchmark harnesses.
var presetTableDocs = []string{
	"../docs/how-to/choose-a-preset.md",
	"../docs/reference/presets.md",
}

// docRow matches a markdown table row whose first cell is a `preset` name in backticks, and
// captures the rest of the row (where the component list lives, in either `a, b` or `a → b`
// form — the two files use different separators on purpose, so the check reads component names
// rather than trying to normalise the formatting).
//
// The name class is deliberately wider than the presets that exist today: a row naming
// `code-smart` or `preset2` must be REPORTED as documenting a preset that does not exist, not
// skipped for failing to look like a name this file recognises.
var docRow = regexp.MustCompile("(?m)^\\|\\s*`([a-z0-9_-]+)`\\s*\\|(.*)$")

// componentToken matches one component name in a pipeline cell.
//
// The two documents format a pipeline differently — choose-a-preset.md writes the whole list
// inside ONE pair of backticks (`format, textclean, cachesplit`) while presets.md backticks each
// name and joins them with arrows (`format` → `textclean`). An earlier version of this guard
// looked for backticked names only, which silently matched nothing in the first file: every
// multi-component row was skipped, and the guard's coverage there was limited to the presets
// that happen to run exactly one component. So the cell is stripped of backticks and split on
// the separators instead, which reads both forms.
var componentToken = regexp.MustCompile(`^[a-z][a-z_]*$`)

// pipelineFromCell reads the component list out of a table cell, in either document's format.
func pipelineFromCell(cell string) []string {
	cell = strings.ReplaceAll(cell, "`", " ")
	cell = strings.ReplaceAll(cell, "→", ",")
	cell = strings.ReplaceAll(cell, "->", ",")
	var out []string
	for _, tok := range strings.Split(cell, ",") {
		tok = strings.TrimSpace(tok)
		if componentToken.MatchString(tok) {
			out = append(out, tok)
		}
	}
	return out
}

func TestPresetDocsDoNotDrift(t *testing.T) {
	for _, path := range presetTableDocs {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}

		documented := map[string][]string{}
		for _, m := range docRow.FindAllStringSubmatch(string(b), -1) {
			name, row := m[1], m[2]
			// The pipeline cell is the one that lists components. In choose-a-preset.md it is
			// the whole rest of the row; in presets.md the prose that follows also mentions
			// component names, so only the FIRST cell after the name is read.
			cell := row
			if i := strings.Index(row, "|"); i >= 0 {
				cell = row[:i]
			}
			documented[name] = pipelineFromCell(cell)
		}

		// Direction 1: every preset that ships is documented, with the pipeline it runs.
		// Iterated over the map, not over the rows, so a missing row FAILS instead of simply
		// not being checked.
		names := make([]string, 0, len(presets))
		for name := range presets {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			want := presets[name]
			got, ok := documented[name]
			if !ok {
				t.Errorf("%s: preset %q ships %v but has no row in this table. "+
					"An undocumented preset is the same defect as a misdocumented one: a reader "+
					"cannot tell it exists, or what it would do to their context.", path, name, want)
				continue
			}
			// `off` is the passthrough: no components on either side, and the table writes it
			// as *(empty)*. Anything else that documents an empty pipeline for a preset which
			// runs components is caught by the comparison below.
			if len(want) == 0 && len(got) == 0 {
				continue
			}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("%s: preset %q is documented as running\n  %v\nbut ships\n  %v\n"+
					"A reader uses this table to decide what a preset will do to their context; "+
					"naming a component that does not run, or omitting one that does, is the "+
					"failure this guard exists for.", path, name, got, want)
			}
		}

		// Direction 2: nothing is documented that does not exist. `--preset <name>` for a name
		// that is not in the map does not degrade, it exits at startup, so a row inviting one is
		// a break dressed as documentation.
		rows := make([]string, 0, len(documented))
		for name := range documented {
			rows = append(rows, name)
		}
		sort.Strings(rows)
		for _, name := range rows {
			if _, ok := presets[name]; !ok {
				t.Errorf("%s: preset %q is documented but does not exist in the presets map; "+
					"`--preset %s` exits at startup with `unknown preset %q`.", path, name, name, name)
			}
		}
	}
}
