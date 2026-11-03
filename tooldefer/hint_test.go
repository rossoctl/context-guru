package tooldefer

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/internal/tokens"
	"github.com/tidwall/gjson"
)

// ── #871 part 4: the hinted names list ──────────────────────────────────

func TestHintIsTheFirstSentenceCutOnAWord(t *testing.T) {
	for in, want := range map[string]string{
		"Edits a notebook cell. Use it for .ipynb files.":                     "Edits a notebook cell.",
		"Fetches a URL\nand returns its content as markdown. Then more text.": "Fetches a URL and returns its content as markdown.",
		"No full stop here":   "No full stop here",
		"v1.2 is fine. Next.": "v1.2 is fine.",
		"":                    "",
		"# SendMessage\n\nSend a message to another agent.\n\nMore.": "Send a message to another agent.",
		"## Do a thing. Then more.":                                  "Do a thing.",
		strings.Repeat("word ", 40):                                  strings.TrimSpace(strings.Repeat("word ", 20)),
	} {
		if got := Hint(in); got != want {
			t.Errorf("Hint(%q) = %q, want %q", in, got, want)
		}
	}
	long := "Starts a background process and streams each line of its output back to you as a notification, so you can watch logs."
	h := Hint(long)
	if len(h) > maxHint || !strings.HasPrefix(long, h) || long[len(h)] != ' ' {
		t.Fatalf("Hint cut %q (len %d) not on a word boundary", h, len(h))
	}
	// One word longer than the bound: cut on a rune boundary, never inside one.
	w := Hint(strings.Repeat("é", 80))
	if len(w) > maxHint || !strings.HasPrefix(strings.Repeat("é", 80), w) || len(w)%2 != 0 {
		t.Fatalf("long word cut = %d bytes", len(w))
	}
}

func TestHintedListNamesAndHints(t *testing.T) {
	b := body([]string{decl("Bash"), decl("Monitor"), smallDecl("Tiny"), `{"name":"Bare","input_schema":{"type":"object"}}`}, "")
	got := HintedList(b, []string{"Bare", "Monitor", "Tiny"})
	if !strings.HasPrefix(got, hintHead+"\n") {
		t.Fatalf("head: %q", got)
	}
	lines := strings.Split(strings.TrimPrefix(got, hintHead+"\n"), "\n")
	want := []string{"Bare", "Monitor: Does the Monitor job.", "Tiny: tiny"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("lines = %q", lines)
	}
}

// The list is built once, at the decision, and stored: a later turn whose descriptions changed
// (a new Claude Code version mid-thread) still sends the same messages[1], and so does a restart.
func TestApplyHintedListIsStableAcrossTurnsAndRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	e, f := envAt(t, threeUnused(), path)
	passProbe(e, ProbeKey{"u", "m", "r"})
	first := Apply(e, req(body(catalogue(), "")))
	m1 := gjson.GetBytes(first.Body, "messages.1.content").String()
	if !strings.Contains(m1, "\nMonitor: Does the Monitor job.\n") {
		t.Fatalf("no hint in the names list: %q", m1)
	}
	rec, ok := Load(e.RecordStore(), "s", "")
	if !ok || rec.List == "" || !strings.HasSuffix(m1, rec.List) {
		t.Fatalf("the list is not the stored one: ok=%v", ok)
	}
	if first.ListTokens != tokens.Count(rec.List) {
		t.Fatalf("ListTokens = %d, want the hinted list's %d", first.ListTokens, tokens.Count(rec.List))
	}
	later := body(catalogue(), "", `{"role":"assistant","content":"ok"}`, `{"role":"user","content":"more"}`)
	later = []byte(strings.ReplaceAll(string(later), "Does the Monitor job.", "Watches a thing."))
	o2 := Apply(e, req(later))
	if gjson.GetBytes(o2.Body, "messages.1").Raw != gjson.GetBytes(first.Body, "messages.1").Raw {
		t.Fatal("the names list changed within the thread")
	}
	f.Close()
	e2, _ := envAt(t, hist{}, path)
	o3 := Apply(e2, req(later))
	if gjson.GetBytes(o3.Body, "messages.1").Raw != gjson.GetBytes(first.Body, "messages.1").Raw {
		t.Fatal("the names list changed across a restart")
	}
}

// A record written before hints (no List) keeps the bare list: its thread's cached prefix holds it.
func TestApplyOldRecordKeepsTheBareList(t *testing.T) {
	e := env(t, threeUnused())
	Save(e.RecordStore(), "s", "", Record{Deferred: []string{"Monitor", "Workflow"}})
	o := Apply(e, req(body(catalogue(), "", `{"role":"assistant","content":"ok"}`, `{"role":"user","content":"x"}`)))
	if !o.Changed || !strings.HasSuffix(gjson.GetBytes(o.Body, "messages.1.content").String(), NamesList([]string{"Monitor", "Workflow"})) {
		t.Fatalf("old record: %s", show(o))
	}
}
