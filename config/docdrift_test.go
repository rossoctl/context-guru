package config

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The preset tables in the docs must match the presets the product actually ships.
//
// This guard exists because they did not. Auditing the "What each one runs" table in
// docs/how-to/choose-a-preset.md against the `presets` map above found EVERY row stale: three
// presets (`codesmart`, `coding`, `general`) were still documented as running `toon`, which was
// retired from them after it acted 0 times on 5,752 production requests and converted 0
// candidates in 11.67M measured tokens, and every row omitted components that do run — the
// lossless pair (`textclean`, `searchfold`) and `linecap`. docs/reference/presets.md was correct
// at the same moment, so the two documents contradicted each other and a reader had no way to
// tell which one was lying.
//
// That is worse than an out-of-date sentence: the table is what somebody reads to decide whether
// a preset does anything they object to, and the answer it gave was wrong in the direction that
// matters — it named a component that does not run and omitted three that do.
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
var docRow = regexp.MustCompile("(?m)^\\|\\s*`([a-z_]+)`\\s*\\|(.*)$")

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

func TestDocumentedPresetPipelinesMatchTheShippedOnes(t *testing.T) {
	for _, path := range presetTableDocs {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		seen := map[string]bool{}
		for _, m := range docRow.FindAllStringSubmatch(string(b), -1) {
			name, row := m[1], m[2]
			want, ok := presets[name]
			if !ok {
				// A row for something that is not a preset (a component reference table, a
				// config key) is not this test's business.
				continue
			}
			// The pipeline cell is the one that lists components. In choose-a-preset.md it is
			// the whole rest of the row; in presets.md the prose that follows also mentions
			// component names, so only the FIRST cell after the name is read.
			cell := row
			if i := strings.Index(row, "|"); i >= 0 {
				cell = row[:i]
			}
			names := pipelineFromCell(cell)
			// A row that lists no components at all is either the `off` passthrough or a row
			// that documents something else about the preset; only check the ones that claim
			// to list a pipeline.
			if len(names) == 0 {
				if len(want) > 0 && strings.Contains(cell, "empty") {
					t.Errorf("%s: preset %q is documented as empty but runs %v", path, name, want)
				}
				continue
			}
			seen[name] = true
			if strings.Join(names, ",") != strings.Join(want, ",") {
				t.Errorf("%s: preset %q is documented as running\n  %v\nbut ships\n  %v\n"+
					"A reader uses this table to decide what a preset will do to their context; "+
					"naming a component that does not run, or omitting one that does, is the "+
					"failure this guard exists for.", path, name, names, want)
			}
		}
		// Coverage, not just "something matched". The previous version of this guard passed
		// while checking only the single-component presets in one of these files, so a count
		// is asserted: the multi-component rows are exactly the ones that were rotting.
		multi := 0
		for name := range seen {
			if len(presets[name]) > 1 {
				multi++
			}
		}
		if len(seen) == 0 || multi < 5 {
			t.Errorf("%s: this guard only checked %d preset rows (%d of them multi-component). "+
				"The table's shape must have changed and the check has silently stopped covering "+
				"it — which is how the stale rows survived in the first place.", path, len(seen), multi)
		}
	}
}
