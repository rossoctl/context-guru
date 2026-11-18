// Package timingguard stops new timing patterns in test code.
//
// Why a sibling of internal/boundary: boundary enforces one rule, the dispatch split. This guard
// is a different rule about a different kind of file (tests, not production code). A separate
// package keeps each rule and its allow-list apart.
//
// What it flags, in *_test.go files only:
//
//   - sleep: a call to time.Sleep, or a <-time.After(...) outside a select (bare, x := or var x =).
//   - poll:  a for loop that uses a time.Now().Add(...) deadline, or time.After(...), as its limit.
//   - after: a select case on <-time.After(...), used as a test timeout.
//
// These patterns fail on a slow CI runner. Use testutil.WaitFor, testutil.WaitForState or
// (Upstream).WaitForCall instead. They wait for the condition and use a long safety bound.
//
// A real case (an injected fake delay, or a sleep that is the thing under test) carries a line
// comment on the same line or the line above:
//
//	//timing:ok <reason>
//
// The marker is "//timing:ok", then white space, then a reason. The reason must not be empty.
// "//timing:okay" is not the marker. The guard follows an alias import and a dot-import of time.
//
// The baseline file lists the existing cases per test file and kind. It only shrinks: a count
// that goes up fails, and a count that goes down also fails until the baseline is lowered.
// Regenerate it with:
//
//	go test ./internal/timingguard -run TestTestTiming -update
package timingguard

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite baseline.txt from the current tree")

const baselineFile = "baseline.txt"

// Kind names a timing pattern.
type Kind string

const (
	Sleep Kind = "sleep"
	Poll  Kind = "poll"
	After Kind = "after"
)

var kinds = []Kind{Sleep, Poll, After}

// Counts is the number of unmarked cases per kind in one file.
type Counts map[Kind]int

func (c Counts) total() int {
	n := 0
	for _, v := range c {
		n += v
	}
	return n
}

// exemptPrefixes are module-relative paths the guard does not scan. The helpers themselves
// must sleep and poll.
var exemptPrefixes = []string{"internal/testutil/"}

const okMarker = "//timing:ok"

// scanSource finds the unmarked timing patterns in one test file. rel is the module-relative
// path. A file that is not a *_test.go file returns no counts. malformed lists each
// //timing:ok comment that has no reason.
func scanSource(rel string, src []byte) (counts Counts, malformed []string, err error) {
	counts = Counts{}
	if !strings.HasSuffix(rel, "_test.go") {
		return counts, nil, nil
	}
	for _, p := range exemptPrefixes {
		if strings.HasPrefix(rel, p) {
			return counts, nil, nil
		}
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, rel, src, parser.ParseComments)
	if err != nil {
		return nil, nil, err
	}
	// The local name of the time import: the alias, "." for a dot-import, or "time".
	timeAlias := ""
	for _, imp := range file.Imports {
		if strings.Trim(imp.Path.Value, "\"") != "time" {
			continue
		}
		timeAlias = "time"
		if imp.Name != nil {
			timeAlias = imp.Name.Name
		}
	}
	if timeAlias == "" || timeAlias == "_" {
		return counts, nil, nil
	}
	dotImport := timeAlias == "."
	// Limit of the dot-import check. Without type checking, the guard cannot see a name from
	// another file of the package, or a local variable that shadows Sleep, After or Now. It sees
	// only the names that this file declares at package level, and it skips those. A bare
	// Sleep(...) that this file does not declare counts as time.Sleep.
	shadowed := map[string]bool{}
	if dotImport {
		for _, d := range file.Decls {
			switch dd := d.(type) {
			case *ast.FuncDecl:
				if dd.Recv == nil {
					shadowed[dd.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, sp := range dd.Specs {
					switch v := sp.(type) {
					case *ast.ValueSpec:
						for _, n := range v.Names {
							shadowed[n.Name] = true
						}
					case *ast.TypeSpec:
						shadowed[v.Name.Name] = true
					}
				}
			}
		}
	}

	// Lines that carry a valid //timing:ok.
	okLines := map[int]bool{}
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			if !strings.HasPrefix(c.Text, okMarker) {
				continue
			}
			rest := strings.TrimPrefix(c.Text, okMarker)
			// "//timing:okay ..." is not the marker. The marker ends at white space or at the end.
			if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
				continue
			}
			line := fset.Position(c.Pos()).Line
			if strings.TrimSpace(rest) == "" {
				malformed = append(malformed, fmt.Sprintf("%s:%d: //timing:ok needs a reason", rel, line))
				continue
			}
			okLines[line] = true
		}
	}
	record := func(pos token.Pos, k Kind) {
		line := fset.Position(pos).Line
		if okLines[line] || okLines[line-1] {
			return
		}
		counts[k]++
	}

	isTimeCall := func(n ast.Node, name string) (*ast.CallExpr, bool) {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return nil, false
		}
		if dotImport {
			id, ok := call.Fun.(*ast.Ident)
			return call, ok && id.Name == name && !shadowed[name]
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return nil, false
		}
		id, ok := sel.X.(*ast.Ident)
		return call, ok && id.Name == timeAlias
	}
	// isNowAdd reports time.Now().Add(...).
	isNowAdd := func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Add" {
			return false
		}
		_, isNow := isTimeCall(sel.X, "Now")
		return isNow
	}

	// Names assigned from time.Now().Add(...) in a function: deadline variables.
	deadlineNames := func(body ast.Node) map[string]bool {
		names := map[string]bool{}
		ast.Inspect(body, func(n ast.Node) bool {
			switch s := n.(type) {
			case *ast.AssignStmt:
				for i, rhs := range s.Rhs {
					if id, ok := identAt(s.Lhs, i); ok && isNowAdd(rhs) {
						names[id] = true
					}
				}
			case *ast.ValueSpec:
				for i, v := range s.Values {
					if i < len(s.Names) && isNowAdd(v) {
						names[s.Names[i].Name] = true
					}
				}
			}
			return true
		})
		return names
	}

	// pollLoop reports whether the for loop uses a deadline as its limit.
	pollLoop := func(f *ast.ForStmt, names map[string]bool) bool {
		found := false
		limit := func(n ast.Node) bool {
			if n == nil {
				return true
			}
			if isNowAdd(n) {
				found = true
			}
			if _, ok := isTimeCall(n, "After"); ok {
				found = true
			}
			if id, ok := n.(*ast.Ident); ok && names[id.Name] {
				found = true
			}
			return !found
		}
		if f.Init != nil {
			ast.Inspect(f.Init, limit)
		}
		if f.Cond != nil {
			ast.Inspect(f.Cond, limit)
		}
		if found {
			return true
		}
		// A deadline variable checked inside the body: time.Now().After(deadline) and so on.
		if f.Body != nil {
			ast.Inspect(f.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return !found
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return !found
				}
				switch sel.Sel.Name {
				case "After", "Before", "Until", "Compare":
					for _, a := range call.Args {
						if id, ok := a.(*ast.Ident); ok && names[id.Name] {
							found = true
						}
					}
				}
				return !found
			})
		}
		return found
	}

	walkFunc := func(body ast.Node) {
		names := deadlineNames(body)
		inSelect := map[ast.Stmt]bool{} // a select case counts once, as after
		ast.Inspect(body, func(n ast.Node) bool {
			switch s := n.(type) {
			case *ast.ForStmt:
				if pollLoop(s, names) {
					record(s.Pos(), Poll)
				}
			case *ast.CommClause:
				if s.Comm != nil && isAfterRecv(s.Comm, isTimeCall) {
					inSelect[s.Comm] = true
					record(s.Pos(), After)
				}
			case *ast.ExprStmt:
				if !inSelect[s] && isAfterRecv(s, isTimeCall) {
					record(s.Pos(), Sleep)
				}
			case *ast.AssignStmt:
				if !inSelect[s] && isAfterRecv(s, isTimeCall) {
					record(s.Pos(), Sleep)
				}
			case *ast.DeclStmt:
				if isAfterRecv(s, isTimeCall) {
					record(s.Pos(), Sleep)
				}
			case *ast.CallExpr:
				if _, ok := isTimeCall(n, "Sleep"); ok {
					record(s.Pos(), Sleep)
				}
			}
			return true
		})
	}
	for _, d := range file.Decls {
		switch fn := d.(type) {
		case *ast.FuncDecl:
			if fn.Body != nil {
				walkFunc(fn.Body)
			}
		case *ast.GenDecl:
			// Package-level var with a func literal.
			walkFunc(fn)
		}
	}
	return counts, malformed, nil
}

// isAfterRecv reports whether s is <-time.After(...), v := <-time.After(...) or
// var v = <-time.After(...).
func isAfterRecv(s ast.Stmt, isTimeCall func(ast.Node, string) (*ast.CallExpr, bool)) bool {
	var e ast.Expr
	switch c := s.(type) {
	case *ast.ExprStmt:
		e = c.X
	case *ast.AssignStmt:
		if len(c.Rhs) == 1 {
			e = c.Rhs[0]
		}
	case *ast.DeclStmt:
		if gd, ok := c.Decl.(*ast.GenDecl); ok && gd.Tok == token.VAR {
			for _, sp := range gd.Specs {
				if vs, ok := sp.(*ast.ValueSpec); ok && len(vs.Values) == 1 && isUnaryAfter(vs.Values[0], isTimeCall) {
					return true
				}
			}
		}
		return false
	}
	return isUnaryAfter(e, isTimeCall)
}

func isUnaryAfter(e ast.Expr, isTimeCall func(ast.Node, string) (*ast.CallExpr, bool)) bool {
	u, ok := e.(*ast.UnaryExpr)
	if !ok || u.Op != token.ARROW {
		return false
	}
	_, ok = isTimeCall(u.X, "After")
	return ok
}

func identAt(lhs []ast.Expr, i int) (string, bool) {
	if i >= len(lhs) {
		return "", false
	}
	id, ok := lhs[i].(*ast.Ident)
	if !ok {
		return "", false
	}
	return id.Name, true
}

// compare checks the current counts against the baseline. up lists each file and kind whose
// count rose. down lists each one whose count fell. A file not in the baseline has baseline 0.
func compare(current, baseline map[string]Counts) (up, down []string) {
	paths := map[string]bool{}
	for p := range current {
		paths[p] = true
	}
	for p := range baseline {
		paths[p] = true
	}
	var sorted []string
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	for _, p := range sorted {
		for _, k := range kinds {
			cur, base := current[p][k], baseline[p][k]
			switch {
			case cur > base:
				up = append(up, fmt.Sprintf("%s: %s count is %d, baseline is %d. Fix: use testutil.WaitFor "+
					"or testutil.WaitForCall, or add //timing:ok <reason> on the line or the line above. "+
					"Do not raise the baseline. To regenerate it after you lower a count: "+
					"go test ./internal/timingguard -run TestTestTiming -update",
					p, k, cur, base))
			case cur < base:
				down = append(down, fmt.Sprintf("%s: %s count is %d, baseline is %d. Lower the baseline.",
					p, k, cur, base))
			}
		}
	}
	return up, down
}

func formatBaseline(m map[string]Counts) string {
	var b strings.Builder
	b.WriteString("# Timing patterns in test files, per file and kind. See timingguard_test.go.\n")
	b.WriteString("# This file only shrinks. Regenerate: go test ./internal/timingguard -run TestTestTiming -update\n")
	var paths []string
	for p, c := range m {
		if c.total() > 0 {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	for _, p := range paths {
		b.WriteString(p)
		for _, k := range kinds {
			if n := m[p][k]; n > 0 {
				fmt.Fprintf(&b, " %s=%d", k, n)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func parseBaseline(text string) (map[string]Counts, error) {
	m := map[string]Counts{}
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		c := Counts{}
		for _, kv := range f[1:] {
			k, v, ok := strings.Cut(kv, "=")
			n, err := strconv.Atoi(v)
			if !ok || err != nil || n <= 0 || !validKind(Kind(k)) {
				return nil, fmt.Errorf("%s:%d: bad entry %q", baselineFile, i+1, kv)
			}
			c[Kind(k)] = n
		}
		m[f[0]] = c
	}
	return m, nil
}

func validKind(k Kind) bool {
	for _, v := range kinds {
		if v == k {
			return true
		}
	}
	return false
}

func scanTree(root string) (map[string]Counts, []string, error) {
	current := map[string]Counts{}
	var malformed []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		c, bad, err := scanSource(rel, src)
		if err != nil {
			return err
		}
		malformed = append(malformed, bad...)
		if c.total() > 0 {
			current[rel] = c
		}
		return nil
	})
	return current, malformed, err
}

func TestTestTimingBaseline(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	current, malformed, err := scanTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(malformed) > 0 {
		t.Fatalf("a //timing:ok comment needs a reason after it:\n%s", strings.Join(malformed, "\n"))
	}
	if *update {
		if err := os.WriteFile(baselineFile, []byte(formatBaseline(current)), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(baselineFile)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := parseBaseline(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	up, down := compare(current, baseline)
	if len(up) > 0 {
		t.Errorf("new timing patterns in test code. They fail on slow CI runners. Do not raise the baseline.\n%s",
			strings.Join(up, "\n"))
	}
	if len(down) > 0 {
		t.Errorf("the baseline is too high. Lower it with:\n"+
			"  go test ./internal/timingguard -run TestTestTiming -update\n%s", strings.Join(down, "\n"))
	}
}

// --- tests of the guard ---------------------------------------------------------------------

func scan(t *testing.T, rel, body string) (Counts, []string) {
	t.Helper()
	src := "package p\nimport \"time\"\nvar _ = time.Second\nfunc f() {\n" + body + "\n}\n"
	c, bad, err := scanSource(rel, []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return c, bad
}

func TestTestTimingScanCatchesEachKind(t *testing.T) {
	cases := []struct {
		name string
		body string
		want Counts
	}{
		{"sleep", `time.Sleep(time.Second)`, Counts{Sleep: 1}},
		{"bare after", `<-time.After(time.Second)`, Counts{Sleep: 1}},
		{"poll init", `for end := time.Now().Add(time.Second); time.Now().Before(end); { }`, Counts{Poll: 1}},
		{"poll cond", `for time.Now().Before(time.Now().Add(time.Second)) { }`, Counts{Poll: 1}},
		{"poll deadline var", "deadline := time.Now().Add(time.Second)\nfor time.Now().Before(deadline) { }", Counts{Poll: 1}},
		{"poll deadline in body", "deadline := time.Now().Add(time.Second)\nfor {\nif time.Now().After(deadline) { return }\n}", Counts{Poll: 1}},
		{"poll after in cond", `for ch := time.After(time.Second); ch != nil; { }`, Counts{Poll: 1}},
		{"select after", "c := make(chan int)\nselect {\ncase <-c:\ncase <-time.After(time.Second):\n}", Counts{After: 1}},
		{"select after assign", "c := make(chan int)\nselect {\ncase <-c:\ncase v := <-time.After(time.Second):\n_ = v\n}", Counts{After: 1}},
		{"clean", "c := make(chan int)\nselect {\ncase <-c:\n}\nt0 := time.Now().Add(-time.Hour)\n_ = t0", Counts{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := scan(t, "x/x_test.go", tc.body)
			for _, k := range kinds {
				if got[k] != tc.want[k] {
					t.Errorf("%s count = %d, want %d (all: %v)", k, got[k], tc.want[k], got)
				}
			}
		})
	}
}

func TestTestTimingScanHonorsTheEscapeHatch(t *testing.T) {
	c, bad := scan(t, "x/x_test.go", "//timing:ok injected fake delay\ntime.Sleep(time.Second)")
	if c.total() != 0 || len(bad) != 0 {
		t.Errorf("line above: counts %v, malformed %v", c, bad)
	}
	c, bad = scan(t, "x/x_test.go", "time.Sleep(time.Second) //timing:ok the sleep is the thing under test")
	if c.total() != 0 || len(bad) != 0 {
		t.Errorf("same line: counts %v, malformed %v", c, bad)
	}
	// Two lines above is too far.
	c, _ = scan(t, "x/x_test.go", "//timing:ok reason\n_ = 1\ntime.Sleep(time.Second)")
	if c[Sleep] != 1 {
		t.Errorf("a marker two lines above must not count: %v", c)
	}
}

func TestTestTimingScanRejectsAnEmptyReason(t *testing.T) {
	for _, body := range []string{"//timing:ok\ntime.Sleep(time.Second)", "//timing:ok   \ntime.Sleep(time.Second)"} {
		c, bad := scan(t, "x/x_test.go", body)
		if len(bad) != 1 {
			t.Errorf("malformed = %v, want 1 entry", bad)
		}
		if c[Sleep] != 1 {
			t.Errorf("a marker with no reason must not excuse the sleep: %v", c)
		}
	}
}

func TestTestTimingScanIgnoresFilesItShould(t *testing.T) {
	c, _ := scan(t, "x/x.go", `time.Sleep(time.Second)`)
	if c.total() != 0 {
		t.Errorf("non-test file counted: %v", c)
	}
	c, _ = scan(t, "internal/testutil/testutil_test.go", `time.Sleep(time.Second)`)
	if c.total() != 0 {
		t.Errorf("testutil counted: %v", c)
	}
	c, _ = scan(t, "internal/testutil/testdb/db_test.go", `time.Sleep(time.Second)`)
	if c.total() != 0 {
		t.Errorf("testutil/testdb counted: %v", c)
	}
}

func TestTestTimingScanFollowsAnAliasedImport(t *testing.T) {
	src := "package p\nimport tm \"time\"\nfunc f() { tm.Sleep(1) }\n"
	c, _, err := scanSource("x/x_test.go", []byte(src))
	if err != nil || c[Sleep] != 1 {
		t.Errorf("aliased import: counts %v, err %v", c, err)
	}
}

func scanFile(t *testing.T, src string) (Counts, []string) {
	t.Helper()
	c, bad, err := scanSource("x/x_test.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return c, bad
}

func TestTestTimingScanCountsAReceiveOutsideASelect(t *testing.T) {
	for name, body := range map[string]string{
		"assign":    "x := <-time.After(time.Second)\n_ = x",
		"var":       "var x = <-time.After(time.Second)\n_ = x",
		"typed var": "var x time.Time = <-time.After(time.Second)\n_ = x",
	} {
		c, _ := scan(t, "x/x_test.go", body)
		if c[Sleep] != 1 || c.total() != 1 {
			t.Errorf("%s: counts %v, want sleep=1 only", name, c)
		}
	}
}

func TestTestTimingScanFollowsADotImport(t *testing.T) {
	c, _ := scanFile(t, "package p\nimport . \"time\"\nfunc f() { Sleep(1); <-After(1); x := <-After(1); _ = x }\n")
	if c[Sleep] != 3 {
		t.Errorf("dot-import: counts %v, want sleep=3", c)
	}
	c, _ = scanFile(t, "package p\nimport . \"time\"\nfunc f() { for end := Now().Add(1); Now().Before(end); { } }\n")
	if c[Poll] != 1 {
		t.Errorf("dot-import poll: counts %v, want poll=1", c)
	}
}

func TestTestTimingScanSkipsALocalFuncNamedSleep(t *testing.T) {
	// No dot-import of time: a bare Sleep is not time.Sleep.
	c, _ := scanFile(t, "package p\nimport \"time\"\nvar _ = time.Second\nfunc Sleep(int) {}\nfunc f() { Sleep(1) }\n")
	if c.total() != 0 {
		t.Errorf("local Sleep counted: %v", c)
	}
	// Dot-import of time, and the file declares its own Sleep: it shadows the import.
	c, _ = scanFile(t, "package p\nimport . \"time\"\nvar _ = Second\nfunc Sleep(int) {}\nfunc f() { Sleep(1) }\n")
	if c.total() != 0 {
		t.Errorf("shadowing Sleep counted: %v", c)
	}
}

func TestTestTimingScanDoesNotTakeOkayForTheMarker(t *testing.T) {
	c, bad := scan(t, "x/x_test.go", "//timing:okay x\ntime.Sleep(time.Second)")
	if c[Sleep] != 1 {
		t.Errorf("//timing:okay must not excuse the sleep: %v", c)
	}
	if len(bad) != 0 {
		t.Errorf("//timing:okay is not a marker, so it is not malformed: %v", bad)
	}
	c, bad = scan(t, "x/x_test.go", "//timing:ok reason\ntime.Sleep(time.Second)")
	if c.total() != 0 || len(bad) != 0 {
		t.Errorf("//timing:ok reason must excuse the sleep: %v %v", c, bad)
	}
}

func TestTestTimingRatchetFailsUpAndDown(t *testing.T) {
	base := map[string]Counts{"a_test.go": {Sleep: 2}}
	up, down := compare(map[string]Counts{"a_test.go": {Sleep: 3}}, base)
	if len(up) != 1 || len(down) != 0 {
		t.Errorf("rise: up %v, down %v", up, down)
	}
	if !strings.Contains(up[0], "a_test.go") || !strings.Contains(up[0], "sleep") ||
		!strings.Contains(up[0], "3") || !strings.Contains(up[0], "2") || !strings.Contains(up[0], "testutil.WaitFor") {
		t.Errorf("message misses a part: %s", up[0])
	}
	up, down = compare(map[string]Counts{"a_test.go": {Sleep: 1}}, base)
	if len(up) != 0 || len(down) != 1 || !strings.Contains(down[0], "Lower the baseline") {
		t.Errorf("fall: up %v, down %v", up, down)
	}
	up, down = compare(map[string]Counts{}, base)
	if len(down) != 1 {
		t.Errorf("a file that left the tree must lower the baseline: %v %v", up, down)
	}
	up, down = compare(map[string]Counts{"a_test.go": {Sleep: 2}}, base)
	if len(up)+len(down) != 0 {
		t.Errorf("equal counts must pass: %v %v", up, down)
	}
	up, _ = compare(map[string]Counts{"new_test.go": {Poll: 1}}, base)
	if len(up) != 1 {
		t.Errorf("a new file is allowed 0: %v", up)
	}
}

func TestTestTimingBaselineRoundTrips(t *testing.T) {
	m := map[string]Counts{"a/b_test.go": {Sleep: 2, After: 1}, "c_test.go": {Poll: 4}}
	got, err := parseBaseline(formatBaseline(m))
	if err != nil {
		t.Fatal(err)
	}
	if up, down := compare(got, m); len(up)+len(down) != 0 {
		t.Errorf("round trip changed counts: %v %v", up, down)
	}
}
