package dash

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// appScript matches the <script> tags that load app.js and the per-tab files split out of it.
var appScript = regexp.MustCompile(`<script defer src="(app\.js|tabs/[a-z-]+\.js)"></script>`)

// appFileNames is app.js and the files under ui/tabs/, in the order index.html loads them.
//
// They were one file until #1089 split app.js by tab. Many tests read the dashboard source to
// check that a field, a label or a call is still there. Those tests do not care which file holds
// it, so they read appSource, which is every one of these files joined in load order.
func appFileNames() ([]string, error) {
	html, err := uiFS.ReadFile("ui/index.html")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range appScript.FindAllStringSubmatch(string(html), -1) {
		out = append(out, "ui/"+m[1])
	}
	return out, nil
}

// appSource is app.js and every tab file, joined in load order. It has the same signature as
// uiFS.ReadFile, so a test that read ui/app.js reads this instead.
func appSource() ([]byte, error) {
	names, err := appFileNames()
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	for _, n := range names {
		src, err := uiFS.ReadFile(n)
		if err != nil {
			return nil, err
		}
		b.Write(src)
		b.WriteString("\n")
	}
	return []byte(b.String()), nil
}

// readApp is appSource for a test.
func readApp(t *testing.T) string {
	t.Helper()
	b, err := appSource()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// appFiles is appFileNames for a test.
func appFiles(t *testing.T) []string {
	t.Helper()
	names, err := appFileNames()
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// TestIndexLoadsEveryTabFileRightAfterAppJS keeps the split of app.js safe to load.
//
// The files are classic scripts that share one global scope. Function declarations are hoisted
// only inside one file, so a top-level line can use a name only from its own file or an earlier
// one. app.js holds the shared helpers and loads first. Each tab file then registers its loader.
// Two mistakes break the page with no error in a test: a tab file that index.html does not load
// (its tab opens an empty section forever), and a tab file loaded before app.js (it throws on
// load, because `loaders` does not exist yet).
func TestIndexLoadsEveryTabFileRightAfterAppJS(t *testing.T) {
	names := appFiles(t)
	if len(names) < 2 || names[0] != "ui/app.js" {
		t.Fatalf("index.html must load app.js first, then the tab files; it loads %v", names)
	}

	onDisk, err := filepath.Glob(filepath.Join("ui", "tabs", "*.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range onDisk {
		if !slices.Contains(names, filepath.ToSlash(f)) {
			t.Errorf("%s exists but index.html does not load it with a plain <script defer> tag", f)
		}
	}

	html := readUI(t, "ui/index.html")
	block := regexp.MustCompile(`<script defer src="app\.js"></script>\n(?:<!--[\s\S]*?-->\n)?((?:<script defer src="tabs/[a-z-]+\.js"></script>\n)+)`)
	m := block.FindStringSubmatch(html)
	if m == nil || strings.Count(m[1], "<script") != len(names)-1 {
		t.Errorf("the tab files must be the scripts directly after app.js in index.html, with nothing between them")
	}

	// Each tab file registers its own loader. The name is the view it opens.
	for _, n := range names[1:] {
		src, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(src), "// ") || !strings.Contains(string(src), "\n'use strict';\n") {
			t.Errorf("%s must start with a header comment and 'use strict'", n)
		}
		if !strings.Contains(string(src), "Object.assign(loaders, {") {
			t.Errorf("%s registers no loader; the router cannot open its view", n)
		}
	}
}
