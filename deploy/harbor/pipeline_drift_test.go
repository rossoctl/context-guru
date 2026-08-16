// Package harbor holds the benchmark harnesses (Python). This Go test exists only to
// keep them from drifting away from the pipeline the product actually ships: the
// harnesses hardcode their arm's component list, and docs/results/REPRODUCE.md points
// readers at them to reproduce the codesmart arm. When the two disagree, the published
// numbers describe something other than the default anyone runs.
package harbor

import (
	"os"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/config"
)

// Checked for EVERY preset a harness names an arm after, not just codesmart. The first
// version of this guard covered codesmart alone, and `codesafe` was found still running
// `cacheinject` where the shipped preset runs `cachesplit` — the same drift, in the same
// file, one arm down. A guard that covers one instance of a class of bug invites the rest.
//
// Arms that deliberately run a NON-preset pipeline are exempt by construction: they are
// keyed by their own name (e.g. "cacheonly", which isolates the cache lever by running
// `[cacheinject]` alone) and are not preset names, so PresetPipeline never resolves them.
var harnessPresetArms = []string{"codesmart", "codesafe"}

func TestHarnessPresetArmsMatchTheShippedPresets(t *testing.T) {
	for _, f := range []string{"swebench.py", "terminalbench.py"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for _, preset := range harnessPresetArms {
			comps, ok := config.PresetPipeline(preset)
			if !ok || len(comps) == 0 {
				t.Fatalf("%s preset resolved to nothing", preset)
			}
			// An arm the harness does not define at all is fine — terminalbench.py has no
			// codesafe arm. What is not fine is defining one that runs something else.
			if !strings.Contains(src, `"`+preset+`"`) {
				continue
			}
			want := "pipeline: [" + strings.Join(comps, ", ") + "]"
			if !strings.Contains(src, want) {
				t.Errorf("%s names a %q arm but does not run the shipped %s pipeline; "+
					"expected a line with\n  %s\nPublished numbers from this arm would describe "+
					"a different product than the one that ships.", f, preset, preset, want)
			}
		}
	}
}
