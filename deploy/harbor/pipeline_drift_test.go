// Package harbor holds the benchmark harnesses (Python). This Go test exists only to
// keep them from drifting away from the components the product actually ships: the
// harnesses hardcode their arms' component lists, and docs/results/REPRODUCE.md points
// readers at them. An arm that names a component the binary does not have fails Build, and the
// proxy exits. An arm that names a REMOVED component (config.removedComponents) is worse: config
// drops the name with a warning, the arm runs without it, and its numbers report under the old name.
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
var armDictBlock = regexp.MustCompile(`(?s)\bCONFIGS\s*=\s*\{(.*?)\n\}`)
var armDictKey = regexp.MustCompile(`(?m)^\s*"([^"]+)"\s*:`)
var quoted = regexp.MustCompile(`"([^"]+)"`)

// The replay and sweep scripts keep lists of arm names that they hand to `-pipeline`, to a config
// file or to `--configs`. A name in such a list must be a registered component, a preset that still
// runs components, or `off`. Two different failures hide here, and they look different:
//
//   - A name that is not registered at all (a component that was deleted, or one behind a build
//     tag such as skeleton) makes Build fail, and the proxy exits.
//   - A name in config.removedComponents is dropped with a warning, so the arm runs an empty
//     pipeline and reports its numbers under the removed name. A retired preset does the same.
//
// `off` is the passthrough arm, which every script defines itself. It is the only exemption.
func TestReplayArmListsNameOnlyRunnableArms(t *testing.T) {
	registered := map[string]bool{}
	for _, n := range components.Names() {
		registered[n] = true
	}
	own := map[string]bool{"off": true}
	for _, f := range []string{"replay.py", "replay2.py", "sweep.py", "../../scripts/analysis/cc_replay_sweep.py"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, m := range armListLine.FindAllStringSubmatch(string(b), -1) {
			for _, q := range quoted.FindAllStringSubmatch(m[2], -1) {
				names = append(names, q[1])
			}
		}
		for _, m := range armDictBlock.FindAllStringSubmatch(string(b), -1) {
			for _, k := range armDictKey.FindAllStringSubmatch(m[1], -1) {
				names = append(names, k[1])
			}
		}
		if len(names) == 0 {
			t.Errorf("%s: no FREE, DET_CONFIGS or CONFIGS list found, so this guard checks nothing", f)
		}
		for _, name := range names {
			if registered[name] || own[name] {
				continue
			}
			if p, ok := config.PresetPipeline(name); ok && len(p) > 0 {
				continue
			}
			t.Errorf("%s: the arm list names %q, which is not a registered component or a preset that "+
				"runs components. If the name is unregistered, Build fails and the proxy exits. If it was "+
				"removed (a removed component or a retired preset), the arm runs an empty pipeline and "+
				"reports under that name.", f, name)
		}
	}
}
