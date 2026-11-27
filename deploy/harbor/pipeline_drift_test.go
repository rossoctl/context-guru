// Package harbor holds the benchmark harnesses (Python). This Go test exists only to
// keep them from drifting away from the components the product actually ships: the
// harnesses hardcode their arms' component lists, and docs/results/REPRODUCE.md points
// readers at them. An arm that names a component the binary does not have would run
// without it (config drops a removed name with a warning) and report numbers under the
// wrong name.
//
// The `codesmart` and `codesafe` arms in swebench.py and terminalbench.py are the only
// definition of those two configs now: the presets of the same names were retired.
package harbor

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/config"
)

var pipelineLine = regexp.MustCompile(`pipeline: \[([^\]]*)\]`)

func TestHarnessArmsNameOnlyRegisteredComponents(t *testing.T) {
	registered := map[string]bool{}
	for _, n := range components.Names() {
		registered[n] = true
	}
	for _, f := range []string{"swebench.py", "terminalbench.py"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, m := range pipelineLine.FindAllStringSubmatch(string(b), -1) {
			for _, name := range strings.Split(m[1], ",") {
				name = strings.TrimSpace(name)
				if name == "" || strings.ContainsAny(name, "{}") {
					continue // an f-string placeholder, not a component name
				}
				found++
				if !registered[name] {
					t.Errorf("%s has an arm that names %q, which is not a registered component; "+
						"the arm would run without it and report under the wrong name.", f, name)
				}
			}
		}
		if found == 0 {
			t.Errorf("%s: no pipeline line found, so this guard checks nothing", f)
		}
	}
}

var armListLine = regexp.MustCompile(`(?s)\b(FREE|DET_CONFIGS)\s*=\s*\[(.*?)\]`)
var quoted = regexp.MustCompile(`"([^"]+)"`)

// The replay and sweep scripts keep lists of arm names that they hand to `-pipeline` or `--configs`.
// A name in such a list must be a registered component, a preset that still runs components, or one
// of the two arms the replay scripts define themselves. Anything else would run an empty pipeline and
// report under that name.
func TestReplayArmListsNameOnlyRunnableArms(t *testing.T) {
	registered := map[string]bool{}
	for _, n := range components.Names() {
		registered[n] = true
	}
	own := map[string]bool{"off": true, "phi_evict": true}
	for _, f := range []string{"replay.py", "replay2.py", "../../scripts/analysis/cc_replay_sweep.py"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, m := range armListLine.FindAllStringSubmatch(string(b), -1) {
			for _, q := range quoted.FindAllStringSubmatch(m[2], -1) {
				name := q[1]
				found++
				if registered[name] || own[name] {
					continue
				}
				if p, ok := config.PresetPipeline(name); ok && len(p) > 0 {
					continue
				}
				t.Errorf("%s: the arm list names %q, which is not a registered component or a preset "+
					"that runs components; the arm would run an empty pipeline and report under that name.", f, name)
			}
		}
		if found == 0 {
			t.Errorf("%s: no FREE or DET_CONFIGS list found, so this guard checks nothing", f)
		}
	}
}
