package offload

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

func newLinecapT(t *testing.T, yaml string) components.Offload {
	t.Helper()
	c, err := newLinecap([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return c.(components.Offload)
}

// runLinecap offloads one tool message and returns the rewritten text plus the gates.
func runLinecap(t *testing.T, comp components.Offload, body string) (string, map[string]int) {
	t.Helper()
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(body)}}
	var rep components.Report
	c := &components.Ctx{Session: "s", Store: store.NewMemory(store.Options{})}
	if _, err := comp.Offload(req, &rep, c); err != nil {
		t.Fatal(err)
	}
	return schema.MessageText(req.Input[0]), rep.Gates
}

// THE safety property of the cap. Every one of these lines is over the cap and every one
// carries something the agent has to act on, so every one must come back byte-identical.
// The cap is only deployable generically because of this list — a 600-character stack
// frame is long for the same reason the noise is.
func TestNeverTruncateAllowList(t *testing.T) {
	pad := strings.Repeat("x", 700)
	for _, line := range []string{
		"pkg/server/handler.go:412:37: undefined: " + pad,
		"internal/tokens/count.go:88: cannot use n (type int) as " + pad,
		`  File "/usr/lib/python3.9/site-packages/urllib3/connectionpool.py", line 787, in urlopen ` + pad,
		"	at com.example.Service.handle(Service.java:214) " + pad,
		"AssertionError: expected 3 but got 4 " + pad,
		"Traceback (most recent call last): " + pad,
		"panic: runtime error: index out of range [5] with length 3 " + pad,
		"FAILED tests/test_api.py::test_create_user - assert " + pad,
		"ERROR tests/test_db.py::test_migrate " + pad,
		"command failed with exit code 137 " + pad,
		"+	return fmt.Errorf(\"could not open %q: %w\", path, err) " + pad,
		"-	return nil " + pad,
		"@@ -14,7 +14,9 @@ func (s *Server) Handle(w http.ResponseWriter " + pad,
		"see https://example.com/docs/troubleshooting#connection-pool " + pad,
	} {
		// A blob big enough to clear min_size, with the protected line in the middle.
		filler := strings.Repeat("plain filler output line\n", 40)
		body := filler + line + "\n" + filler
		comp := newLinecapT(t, "collapse_duplicate_lines: false\n")
		got, _ := runLinecap(t, comp, body)
		if !strings.Contains(got, line) {
			t.Errorf("a protected line was cut:\n %.90q...", line)
		}
	}
}

// The cap itself: an unprotected over-long line loses its MIDDLE and says so. (It lost its
// tail until clipMiddle — which is what destroyed the file reference on real diagnostics.)
func TestCapTruncatesAnUnprotectedLongLine(t *testing.T) {
	long := strings.Repeat("noise", 400) // 2,000 chars, nothing actionable in it
	body := strings.Repeat("plain filler output line\n", 40) + long + "\n"
	got, _ := runLinecap(t, newLinecapT(t, "collapse_duplicate_lines: false\n"), body)
	if strings.Contains(got, long) {
		t.Fatal("a 2,000-char noise line survived the 500-char cap")
	}
	if !strings.Contains(got, "...[cut]...") {
		t.Fatal("an intra-line cut must be marked; a silent mid-line cut reads as corrupted output")
	}
	// Reversible: the marker names the expand tool, so the full line is recoverable.
	if !expand.HasPlaceholder(got) {
		t.Fatalf("lossy rewrite left no expand marker:\n%.200q", got)
	}
}

// The duplicate collapse is LOSSY (a repeat's position is not recoverable), so the
// elision has to be visible in the text itself, not only in the stash.
func TestScatteredDupCollapseAnnotatesTheCount(t *testing.T) {
	// Non-adjacent repeats — the case collapseObviousNoise does NOT handle.
	body := "build starting now\nreading configuration\nwarming the cache\n"
	for i := 0; i < 30; i++ {
		body += "resolving dependency graph for module\nstep " + strings.Repeat("a", i+1) + " done\n"
	}
	body += "build finished\nartifacts written\nall done here\n"
	got, _ := runLinecap(t, newLinecapT(t, ""), body)
	if !strings.Contains(got, "resolving dependency graph for module  (x30)") {
		t.Fatalf("want one copy annotated (x30), got:\n%.400q", got)
	}
	if n := strings.Count(got, "resolving dependency graph for module"); n != 1 {
		t.Fatalf("repeated line appears %d times after the collapse, want 1", n)
	}
	// Everything that was NOT a duplicate survives.
	for i := 0; i < 30; i++ {
		if !strings.Contains(got, "step "+strings.Repeat("a", i+1)+" done") {
			t.Fatalf("distinct line %d was dropped", i)
		}
	}
}

// A diff's repeated lines are POSITIONAL: two identical `+ return nil` lines are two
// distinct edits in two distinct hunks, and folding them to one with (x2) produces
// something that cannot be read as a diff.
func TestDupCollapseSkipsDiffShapedOutput(t *testing.T) {
	body := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n"
	for h := 0; h < 12; h++ {
		body += "@@ -10,3 +10,4 @@\n context line here for hunk\n+	return nil\n-	return err\n"
	}
	got, _ := runLinecap(t, newLinecapT(t, "max_line_chars: 0\n"), body)
	if got != body {
		t.Fatalf("a diff must survive the dup collapse unchanged:\n%.400q", got)
	}
	if n := strings.Count(got, "+	return nil"); n != 12 {
		t.Fatalf("%d of 12 diff additions survived", n)
	}
}

// Two lines that differ only in a line number are two FINDINGS, not one repeated twice.
// A count of two is not the same information as two locations.
func TestDupCollapseKeepsDistinctLineNumbers(t *testing.T) {
	body := "vet: analysing packages now\nloading module graph\nstarting checks\n"
	for i := 100; i < 130; i++ { // 30 DISTINCT source locations, same message text
		body += "pkg/handler.go:" + strconv.Itoa(i) + ": composite literal uses unkeyed fields\n"
	}
	body += "vet finished\n30 problems\nexit 1\n"
	got, _ := runLinecap(t, newLinecapT(t, "max_line_chars: 0\n"), body)
	if strings.Contains(got, "(x") {
		t.Fatalf("lines differing in a source location were collapsed:\n%.400q", got)
	}
}

// Short lines repeat constantly in structured output and each collapse costs an (xN)
// annotation, so collapsing them loses tokens as often as it saves them.
func TestDupCollapseSkipsShortLines(t *testing.T) {
	body := "opening the report file\nreading the entries\nparsing them now\n"
	body += strings.Repeat("}\n];\n---\n", 60)
	body += "report done\nclosing file\nfinished ok\n"
	got, _ := runLinecap(t, newLinecapT(t, "max_line_chars: 0\n"), body)
	if strings.Contains(got, "(x") {
		t.Fatalf("short structural lines were collapsed:\n%.200q", got)
	}
}

// The banner and the summary are where a model looks first, and a summary line that
// legitimately repeats a banner line is not noise.
func TestDupCollapseProtectsTheEdges(t *testing.T) {
	edge := "=== test session starts ==="
	body := edge + "\nplatform linux -- Python 3.9.18\nrootdir /srv/app\n"
	body += strings.Repeat("collecting test modules from disk\n", 40)
	body += "short test summary info\n" + edge + "\nexit code 1\n"
	got, _ := runLinecap(t, newLinecapT(t, "max_line_chars: 0\n"), body)
	if n := strings.Count(got, edge); n != 2 {
		t.Fatalf("banner/summary edge lines were collapsed (%d of 2 survive):\n%.300q", n, got)
	}
}

// Determinism is the cache-safety argument: the rewrite is a pure function of the
// message's own text, so re-sending the same output on a later turn produces the same
// bytes and no cached position is re-anchored. Nothing about that may depend on depth,
// the store, or the turn.
func TestLinecapIsAPureFunctionOfContent(t *testing.T) {
	body := strings.Repeat("repeated verbose progress line for the build\n", 20) +
		strings.Repeat("z", 900) + "\n" + strings.Repeat("filler line here\n", 20)
	comp := newLinecapT(t, "")
	first, _ := runLinecap(t, comp, body)
	if first == body {
		t.Fatal("fixture did not trigger either rule")
	}
	for turn := 1; turn < 8; turn++ {
		got, _ := runLinecap(t, comp, body) // fresh store every time: no state may be involved
		if got != first {
			t.Fatalf("turn %d produced different bytes for identical content:\n want %.150q\n got  %.150q",
				turn, first, got)
		}
	}
}

// Nothing to do must mean nothing done — and it must say so, or "acted: 0" is
// indistinguishable from a broken component.
func TestLinecapLeavesCleanOutputAloneAndReportsWhy(t *testing.T) {
	body := ""
	for i := 0; i < 40; i++ {
		body += "distinct short output line number " + string(rune('a'+i%26)) + strings.Repeat("q", i) + "\n"
	}
	got, gates := runLinecap(t, newLinecapT(t, ""), body)
	if got != body {
		t.Fatalf("clean output was rewritten:\n%.300q", got)
	}
	if gates["no_long_or_repeated_lines"] == 0 {
		t.Fatalf("must report why it declined, got %v", gates)
	}
}

// Marker-bearing content is off limits: a cap could cut the marker line and orphan the
// stash, which breaks reversibility for whichever component wrote it.
func TestLinecapSkipsMarkerBearingContent(t *testing.T) {
	body := strings.Repeat("repeated verbose progress line for the build\n", 20) +
		"<<cg:" + strings.Repeat("a", 64) + ">>\n"
	got, gates := runLinecap(t, newLinecapT(t, ""), body)
	if got != body {
		t.Fatal("rewrote content that already carries an offload marker")
	}
	if gates[GateAlreadyMarked] == 0 {
		t.Fatalf("want %s, got %v", GateAlreadyMarked, gates)
	}
}

// The two rules overlap on real traffic. Collapsing duplicates FIRST means a dropped
// duplicate is not also charged as a capped line — the other order double-counts the
// same tokens, which is how a 1.75M-token estimate becomes an upper bound rather than a
// measurement.
func TestDupCollapseRunsBeforeTheCap(t *testing.T) {
	long := strings.Repeat("noise", 300) // one over-long line, repeated
	body := "starting the run\nloading inputs\npreparing\n" +
		strings.Repeat(long+"\n", 10) +
		"run complete\nwriting output\ndone\n"
	lc := newLinecapT(t, "").(*Linecap)
	out, folds := lc.rewrite(body)
	// 9 duplicates dropped + 1 surviving copy capped = 10, not 19.
	if folds != 10 {
		t.Fatalf("folds=%d want 10 (9 dup drops + 1 cap on the survivor); a different number "+
			"means the two rules are charging the same tokens twice", folds)
	}
	if n := strings.Count(out, "(x10)"); n != 1 {
		t.Fatalf("want the surviving copy annotated (x10), got:\n%.300q", out)
	}
}

// Placement. Every Offload leaves a marker and every Offload skips marker-bearing content
// (skipReduce, so nothing double-reduces and no stash is orphaned) — so a MODEST reducer
// placed ahead of a DRASTIC one steals its candidates outright. Measured on 1,795 real
// captured requests with `general`: linecap 7th saved 5,524,476 tokens, which is WORSE than
// the 5,556,801 with no linecap at all, because it took 39,335 tokens off messages collapse
// would have taken 76,554 off. Last, it saves 5,811,621.
//
// So the invariant is a property of the component, not of one preset: linecap must not
// consume a message a heavier offloader would have taken. This asserts the mechanism —
// linecap declines content that already carries a marker — which is what makes running it
// last correct and running it early harmful.
func TestLinecapYieldsToAnEarlierOffloader(t *testing.T) {
	body := strings.Repeat("verbose repeated build progress line for the module\n", 30) +
		strings.Repeat("q", 900) + "\n"
	lc := newLinecapT(t, "")

	// Alone, it acts.
	if got, _ := runLinecap(t, lc, body); got == body {
		t.Fatal("fixture does not trigger linecap at all")
	}
	// After another offloader has marked the message, it must not touch it — otherwise it
	// would double-reduce and could cut the marker line that names the earlier stash.
	marked := "[older tool output masked] <<cg:" + strings.Repeat("b", 64) + ">>"
	got, gates := runLinecap(t, lc, marked+"\n"+body)
	if got != marked+"\n"+body {
		t.Fatalf("linecap rewrote a message another offloader had already marked:\n%.200q", got)
	}
	if gates[GateAlreadyMarked] == 0 {
		t.Fatalf("want %s, got %v", GateAlreadyMarked, gates)
	}
}

// The class TestNeverTruncateAllowList could not catch: every line there puts the actionable
// token at the HEAD, where a head-preserving cut never touches it. These put it at the TAIL,
// which is where real diagnostics put it — and a head-only cut destroyed all of them.
//
// The allow-list is now defence in depth rather than the thing correctness rests on: cutting
// the middle keeps both ends whether or not a pattern happens to match.
func TestCapKeepsATrailingReference(t *testing.T) {
	filler := strings.Repeat("plain filler output line\n", 40)
	for _, tc := range []struct {
		name, line, must string
	}{
		{"tsc resolve error, path at the end",
			"TS2345: Argument of type '{ id: number; nm: string; }' is not assignable to parameter of type " +
				strings.Repeat("'SomeVeryLongGenericWrapper<Partial<Props>>' ", 12) + "src/components/Widget.tsx:42:11",
			"src/components/Widget.tsx:42:11"},
		{"node cannot-find-module, two paths at the end",
			"Cannot find module " + strings.Repeat("x", 500) + " imported from /srv/app/node_modules/.pnpm/pkg/dist/index.mjs",
			"/srv/app/node_modules/.pnpm/pkg/dist/index.mjs"},
		{"json log line with file/line keys last",
			`{"level":"error","msg":"` + strings.Repeat("y", 520) + `","file":"internal/db/pool.go","line":214}`,
			`"line":214`},
		{"indented cargo diagnostic, defeats an anchored ^path:line",
			"  " + strings.Repeat("z", 600) + "  --> src/main.rs:5:9", "src/main.rs:5:9"},
		{"tab-indented go stack frame",
			"\t" + strings.Repeat("w", 600) + "\t/usr/local/go/src/net/http/server.go:2136 +0x4b", "server.go:2136"},
		{"a long absolute path with no line number at all",
			strings.Repeat("q", 600) + " /srv/app/pkg/handler/very/deep/path/file.go", "very/deep/path/file.go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := runLinecap(t, newLinecapT(t, "collapse_duplicate_lines: false\n"),
				filler+tc.line+"\n"+filler)
			if !strings.Contains(got, tc.must) {
				t.Errorf("the cap destroyed the only actionable token %q", tc.must)
			}
		})
	}
}

// The budget is a budget: head + marker + tail must never exceed the cap, or the "cap" grows
// the line it was meant to shrink and the never-worse guard is doing all the work.
func TestClipMiddleHonoursTheBudget(t *testing.T) {
	for _, n := range []int{1, 2, 5, 11, 12, 20, 100, 500} {
		for _, in := range []string{strings.Repeat("a", 2000), strings.Repeat("é", 2000)} {
			got := clipMiddle(in, n)
			if r := len([]rune(got)); r > n {
				t.Fatalf("cap %d produced %d runes", n, r)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("cap %d cut a rune in half", n)
			}
		}
	}
	// Under the cap, untouched.
	if got := clipMiddle("short line", 500); got != "short line" {
		t.Fatalf("a line under the cap was rewritten: %q", got)
	}
}
