package offload

import (
	"fmt"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/components/reformat"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// hits builds `file:LINE:text` rows for one file.
func hits(file string, n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%s:%d:    return handle(request, %d)\n", file, i*10, i)
	}
	return b.String()
}

func TestCapSearchOutputKeepsFirstNAndLast(t *testing.T) {
	in := hits("pkg/a.go", 30) + hits("pkg/b.go", 2)
	out, ok := CapSearchOutput(in, 3, 0, true)
	if !ok {
		t.Fatal("expected a cap")
	}
	for _, want := range []string{"pkg/a.go:10:", "pkg/a.go:30:", "pkg/a.go: +26 more matches", "pkg/a.go:300:", "pkg/b.go:10:", "pkg/b.go:20:"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "pkg/a.go:40:") {
		t.Errorf("row past the cap survived:\n%s", out)
	}
	out, _ = CapSearchOutput(in, 3, 0, false)
	if strings.Contains(out, "pkg/a.go:300:") || !strings.Contains(out, "+27 more matches") {
		t.Errorf("keep_last=false should drop the last row and count it:\n%s", out)
	}
}

func TestCapSearchOutputMaxFiles(t *testing.T) {
	in := hits("a.go", 2) + hits("b.go", 2) + hits("c.go", 3)
	out, ok := CapSearchOutput(in, 0, 2, true)
	if !ok || strings.Contains(out, "c.go:") || !strings.Contains(out, "+1 more files (3 matches)") {
		t.Errorf("got ok=%v\n%s", ok, out)
	}
}

func TestCapSearchOutputContextRows(t *testing.T) {
	in := "a.go:10:hit one\na.go-11-ctx after one\n--\na.go:20:hit two\na.go-21-ctx after two\n--\na.go:30:hit three\na.go-31-ctx\n--\na.go:40:hit four\n"
	out, ok := CapSearchOutput(in, 2, 0, false)
	if !ok || !strings.Contains(out, "a.go: +2 more matches") || strings.Contains(out, "hit three") {
		t.Errorf("got ok=%v\n%s", ok, out)
	}
}

// Anything that is not a hit listing, or has nothing over the cap, must come back untouched.
func TestCapSearchOutputLeavesOthersAlone(t *testing.T) {
	for name, in := range map[string]string{
		"prose":     strings.Repeat("the quick brown fox jumps over the lazy dog\n", 20),
		"under cap": hits("a.go", 5) + hits("b.go", 5),
		"mixed":     hits("a.go", 3) + strings.Repeat("some unrelated build output line\n", 30),
	} {
		if out, ok := CapSearchOutput(in, 15, 0, true); ok || out != in {
			t.Errorf("%s: changed (ok=%v)", name, ok)
		}
	}
}

// The whole point: the stashed original is exactly what the agent gets back from the marker.
func TestSearchcapIsReversible(t *testing.T) {
	in := hits("pkg/a.go", 60) + hits("pkg/b.go", 40)
	comp := func() components.Offload {
		c, err := newSearchcap([]byte("per_file: 5"))
		if err != nil {
			t.Fatal(err)
		}
		return c.(components.Offload)
	}()
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(in)}}
	var rep components.Report
	st := store.NewMemory(store.Options{})
	c := &components.Ctx{Session: "s", Store: st}
	keys, err := comp.Offload(req, &rep, c)
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys=%v err=%v gates=%v", keys, err, rep.Gates)
	}
	out := schema.MessageText(req.Input[0])
	if !expand.HasPlaceholder(out) || schema.TextTokens(out) >= schema.TextTokens(in) {
		t.Fatalf("no marker or no saving:\n%s", out)
	}
	got, ok := st.Get(keys[0])
	if !ok || string(got) != in {
		t.Fatal("stash does not restore the original byte-for-byte")
	}
	// A message that already carries the marker is not capped again.
	req.Input[0] = tool(out)
	var rep2 components.Report
	if k, _ := comp.Offload(req, &rep2, c); len(k) != 0 {
		t.Error("re-capped a marked message")
	}
}

// Searchcap runs before searchfold in a pipeline: what it keeps must still fold.
func TestSearchcapOutputStillFolds(t *testing.T) {
	out, _ := CapSearchOutput(hits("pkg/deep/dir/a.go", 40), 5, 0, true)
	if f := reformat.FoldSearchOutput(out); len(f) >= len(out) {
		t.Errorf("capped output no longer folds: %d -> %d", len(out), len(f))
	}
}

// A file with exactly perFile+1 hits has nothing to elide once the last is kept: no "+0" line.
func TestCapSearchOutputExactlyOneOverCap(t *testing.T) {
	in := hits("a.go", 4) + hits("b.go", 30)
	out, ok := CapSearchOutput(in, 3, 0, true)
	if !ok || strings.Contains(out, "+0 more") || !strings.Contains(out, "a.go:40:") || !strings.Contains(out, "b.go: +26 more matches") {
		t.Errorf("got ok=%v\n%s", ok, out)
	}
}
