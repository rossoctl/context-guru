package testutil

import (
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// recorder is a testing.TB that records Fatalf instead of ending the test.
type recorder struct {
	testing.TB
	msg string
}

func (r *recorder) Helper() {}
func (r *recorder) Fatalf(f string, a ...any) {
	r.msg = strings.TrimSpace(fmt.Sprintf(f, a...))
	panic(r) // stands in for runtime.Goexit
}

// runFatal runs fn and returns the Fatalf message, or "" if fn did not fail.
func runFatal(fn func(t testing.TB)) (msg string) {
	r := &recorder{}
	defer func() {
		if p := recover(); p != nil {
			if p != r {
				panic(p)
			}
			msg = r.msg
		}
	}()
	fn(r)
	return ""
}

func TestWaitForReturnsWhenTheConditionHolds(t *testing.T) {
	var n atomic.Int32
	go func() { time.Sleep(20 * time.Millisecond); n.Store(1) }()
	WaitFor(t, "n to be 1", func() bool { return n.Load() == 1 })
}

func TestWaitForFailsWithTheMessageWhenTheConditionNeverHolds(t *testing.T) {
	defer SetBoundForTest(50 * time.Millisecond)()
	msg := runFatal(func(tb testing.TB) {
		WaitForState(tb, "the moon to fall", func() bool { return false }, func() string { return "still up" })
	})
	if !strings.Contains(msg, "the moon to fall") || !strings.Contains(msg, "timed out after") ||
		!strings.Contains(msg, "state: still up") {
		t.Fatalf("message = %q, want what, elapsed time and state", msg)
	}
	if m := runFatal(func(tb testing.TB) { WaitFor(tb, "x", func() bool { return false }) }); !strings.Contains(m, "waiting for x") {
		t.Fatalf("WaitFor message = %q", m)
	}
}

// deadlineTB is a testing.TB whose Deadline is in the near future.
type deadlineTB struct {
	recorder
	dl time.Time
}

func (d *deadlineTB) Deadline() (time.Time, bool) { return d.dl, true }

func TestWaitForGivesUpBeforeTheTestDeadlineWithItsOwnMessage(t *testing.T) {
	// The test deadline is 5.2 s away; the margin is 5 s; so the bound is about 0.2 s, not 30 s.
	d := &deadlineTB{dl: time.Now().Add(deadlineMargin + 200*time.Millisecond)}
	start := time.Now()
	func() {
		defer func() {
			if p := recover(); p != nil && p != &d.recorder {
				panic(p)
			}
		}()
		WaitFor(d, "never", func() bool { return false })
	}()
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("WaitFor waited %v; it must stop before the test deadline", el)
	}
	if !strings.Contains(d.msg, "waiting for never") {
		t.Fatalf("message = %q", d.msg)
	}
}

func TestWaitForStateSurvivesAPanickingStateFunc(t *testing.T) {
	defer SetBoundForTest(30 * time.Millisecond)()
	msg := runFatal(func(tb testing.TB) {
		WaitForState(tb, "x", func() bool { return false }, func() string { panic("boom") })
	})
	if !strings.Contains(msg, "waiting for x") || !strings.Contains(msg, "boom") {
		t.Fatalf("message = %q", msg)
	}
}

func TestNewUpstreamWithNilRespondReplies200AndEmptyJSON(t *testing.T) {
	up := NewUpstream(t, nil)
	resp, err := http.Post(up.URL+"/x", "text/plain", strings.NewReader("a"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "{}" {
		t.Fatalf("got %d %q, want 200 {}", resp.StatusCode, b)
	}
}

func TestUpstreamWaitForCallSeesALateRequest(t *testing.T) {
	up := NewUpstream(t, func(w http.ResponseWriter, r *http.Request) {})
	go func() {
		time.Sleep(20 * time.Millisecond)
		resp, err := http.Post(up.URL+"/late", "text/plain", strings.NewReader("hello"))
		if err == nil {
			resp.Body.Close()
		}
	}()
	got := up.WaitForCall(t, "the late call", 1, func(c Call) bool { return string(c.Body) == "hello" })
	if len(got) != 1 || got[0].Path != "/late" {
		t.Fatalf("got %+v", got)
	}
}

func TestUpstreamWaitForCallFailsWhenNoRequestMatches(t *testing.T) {
	defer SetBoundForTest(50 * time.Millisecond)()
	up := NewUpstream(t, func(w http.ResponseWriter, r *http.Request) {})
	msg := runFatal(func(tb testing.TB) {
		up.WaitForCall(tb, "a call that never comes", 1, func(Call) bool { return false })
	})
	if !strings.Contains(msg, "a call that never comes") || !strings.Contains(msg, "0 of 1") {
		t.Fatalf("message = %q", msg)
	}
}

// internal/testutil is for test files only. A non-test file that imports it would pull test
// helpers into a shipped binary.
func TestNoProductionPackageImportsTestutil(t *testing.T) {
	const path = "github.com/rossoctl/context-guru/internal/testutil"
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var seen int
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == ".git" || n == ".claude" || n == "node_modules" || n == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		if strings.HasPrefix(p, filepath.Join(root, "internal", "testutil")+string(os.PathSeparator)) {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
		if perr != nil {
			return nil
		}
		seen++
		for _, imp := range f.Imports {
			if s, _ := strconv.Unquote(imp.Path.Value); s == path {
				rel, _ := filepath.Rel(root, p)
				t.Errorf("%s imports internal/testutil; only _test.go files may", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen < 50 {
		t.Fatalf("scanned only %d non-test files; the walk is broken", seen)
	}
}
