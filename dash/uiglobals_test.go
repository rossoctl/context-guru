package dash

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// topLevelName matches a top-level declaration at the start of a line: a function, a class, or
// a const, let or var binding.
var topLevelName = regexp.MustCompile(`^(?:async\s+)?(?:function\*?\s*|class\s+|const\s+|let\s+|var\s+)([A-Za-z_$][A-Za-z0-9_$]*)`)

// The dashboard scripts are classic scripts, so they share one global scope. When a name is
// declared twice, the last function declaration wins everywhere, because declarations are
// hoisted. The other copy is then dead code, and an edit to it has no effect (#1056: kv in
// app.js). A const, let or class declared twice in two files stops the second file with a
// SyntaxError. This test fails on either case, in one file or across files.
func TestDashboardScriptsDeclareEachTopLevelNameOnce(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("ui", "*.js"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, ".min.js") {
			continue // a vendored library, not our code
		}
		fh, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for line := 1; sc.Scan(); line++ {
			m := topLevelName.FindStringSubmatch(sc.Text())
			if m == nil {
				continue
			}
			checked++
			at := filepath.Base(f) + ":" + strconv.Itoa(line)
			if prev, ok := seen[m[1]]; ok {
				t.Errorf("%s is declared twice at the top level: %s and %s", m[1], prev, at)
				continue
			}
			seen[m[1]] = at
		}
		if err := sc.Err(); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		fh.Close()
	}
	// A pass that checked nothing is not a pass.
	if checked < 100 {
		t.Fatalf("checked only %d top-level names in ui/*.js; the pattern or the path is wrong", checked)
	}
}

