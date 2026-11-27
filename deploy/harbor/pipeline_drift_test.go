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
