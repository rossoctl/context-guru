package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/config"
)

// writeRound writes a synthetic round: one user prompt, then three tool calls — a Read of a large file, a
// grep with repeated output, and an Edit of the file read first — with a call after each result.
func writeRound(t *testing.T, dir string) string {
	t.Helper()
	var file strings.Builder
	for i := 0; i < 400; i++ {
		file.WriteString("func f")
		file.WriteString(strings.Repeat("x", i%7))
		file.WriteString("() { return compute(a, b, c) + other(d, e) }\n")
	}
	var grep strings.Builder
	for i := 0; i < 60; i++ {
		grep.WriteString("src/pkg/file.go:12: match here\n")
	}
	tu := func(id, name, input string) string {
		return `{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":` + input + `}`
	}
	tr := func(id, content string) string {
		b, _ := json.Marshal(content)
		return `{"type":"tool_result","tool_use_id":"` + id + `","content":` + string(b) + `}`
	}
	msgs := []string{
		`{"role":"user","content":[{"type":"text","text":"fix the bug"}]}`,
		`{"role":"assistant","content":[` + tu("t1", "Read", `{"file_path":"/w/a.go"}`) + `]}`,
		`{"role":"user","content":[` + tr("t1", file.String()) + `]}`,
		`{"role":"assistant","content":[` + tu("t2", "Bash", `{"command":"grep -rn match src"}`) + `]}`,
		`{"role":"user","content":[` + tr("t2", grep.String()) + `]}`,
		`{"role":"assistant","content":[` + tu("t3", "Edit", `{"file_path":"/w/a.go","old_string":"a","new_string":"b"}`) + `]}`,
		`{"role":"user","content":[` + tr("t3", "ok") + `]}`,
		`{"role":"assistant","content":[{"type":"text","text":"done"}]}`,
	}
	round := `{"messages":[` + strings.Join(msgs, ",") + `],"calls":[` +
		`{"n":1,"ts_ms":1000},{"n":3,"ts_ms":11000},{"n":5,"ts_ms":21000},{"n":7,"ts_ms":31000}]}`
	p := filepath.Join(dir, "round.json")
	if err := os.WriteFile(p, []byte(round), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func runDoc(t *testing.T, doc string) []callOut {
	t.Helper()
	dir := t.TempDir()
	in := writeRound(t, dir)
	cfg, err := config.LoadBytes([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.jsonl")
	if err := replay(cfg, "claude-opus-5", in, out); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var calls []callOut
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var c callOut
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, c)
	}
	return calls
}

// The off preset is the control: it must report no saving, no change and no cache break on every call.
// A replay that reports a saving here is measuring its own rebuild, not the pipeline.
func TestOffPresetReportsNothing(t *testing.T) {
	calls := runDoc(t, "preset: \"off\"\n")
	if len(calls) != 4 {
		t.Fatalf("got %d calls, want 4", len(calls))
	}
	for _, c := range calls {
		if c.OrigTok != c.OutTok || c.Broken || len(c.ByComp) != 0 || len(c.ByTool) != 0 {
			t.Fatalf("call %d: off preset changed the request: %+v", c.K, c)
		}
	}
}

// collapse cuts the large file body; the saving must be attributed to that component and to that tool_use_id,
// and must agree with the token difference of the whole request.
func TestCollapseIsAttributedToItsToolResult(t *testing.T) {
	calls := runDoc(t, "pipeline: [collapse]\n")
	last := calls[len(calls)-1]
	removed := last.OrigTok - last.OutTok
	if removed <= 0 {
		t.Fatalf("collapse removed nothing from a 400-line file: %+v", last)
	}
	if last.ByComp["collapse"] <= 0 {
		t.Fatalf("saving not attributed to collapse: %+v", last.ByComp)
	}
	if last.ByTool["t1"] <= 0 || last.ByTool["t3"] != 0 {
		t.Fatalf("saving attributed to the wrong tool result: %+v", last.ByTool)
	}
	for _, c := range calls {
		if c.Broken {
			t.Fatalf("call %d: collapse on the tail broke the cached prefix", c.K)
		}
	}
}

// A rewrite of a message the previous request already sent is a cache break, and the replay must say so.
// readlifecycle with stale_at_depth offloads the Read once the Edit arrives, when the Read is already cached.
func TestRewriteOfSentMessageIsABreak(t *testing.T) {
	calls := runDoc(t, "pipeline: [readlifecycle]\ncomponents:\n  readlifecycle:\n    stale_at_depth: true\n")
	broken := 0
	for _, c := range calls {
		if c.Broken {
			broken++
			if c.CommonTok >= c.PrevOut {
				t.Fatalf("call %d: a break must keep less than the previous body: %+v", c.K, c)
			}
		}
	}
	if broken == 0 {
		t.Fatalf("readlifecycle rewrote a cached Read but no break was reported: %+v", calls)
	}
}
