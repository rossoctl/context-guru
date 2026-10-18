package dash

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Reading Claude Code's own session transcripts.
//
// WHY THIS EXISTS. A user installs this proxy after months of working with Claude Code, and the
// dashboard starts from zero: everything before install day is invisible. But Claude Code writes
// a transcript of every session under ~/.claude/projects/, and those files predate any install of
// ours — measured on one real machine, 4,053 requests over 12 days that the proxy never saw,
// against 3,959 that it did. Roughly half the available history was being thrown away.
//
// THE SOURCE IS DELIBERATELY NOT OURS. Every figure the dashboard reports today comes from
// traffic that passed through the proxy; this comes from another program's append-only log. That
// is the whole point — it is the only way to see a request made before the proxy existed — and it
// is also why the reader has to be tolerant: the format is not ours to rely on, the file may be
// mid-write, and it contains entry types this code has never heard of.

// writeJSONL writes lines to a transcript file, creating the project directory.
func writeJSONL(t *testing.T, home, enc, sess string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", enc)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sess+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// turnLine builds one billed assistant turn, shaped as the real transcripts are.
func turnLine(sess, cwd string, tsMs int64, cacheWrite, out int64) string {
	return turnEntry(sess, cwd, tsMs, cacheWrite, out)
}

// turnLineCalling is turnLine with tool_use blocks, for the cases that care what was invoked.
func turnLineCalling(sess, cwd string, tsMs int64, blocks ...string) string {
	return turnEntry(sess, cwd, tsMs, 5000, 400, blocks...)
}

func turnEntry(sess, cwd string, tsMs, cacheWrite, out int64, blocks ...string) string {
	ts := time.UnixMilli(tsMs).UTC().Format(time.RFC3339Nano)
	return `{"type":"assistant","sessionId":"` + sess + `","cwd":"` + cwd + `",` +
		`"timestamp":"` + ts + `","message":{"model":"claude-opus-5","role":"assistant",` +
		`"usage":{"input_tokens":1,"cache_creation_input_tokens":` + strconv.FormatInt(cacheWrite, 10) +
		`,"cache_read_input_tokens":0,"output_tokens":` + strconv.FormatInt(out, 10) + `},"content":[` +
		strings.Join(blocks, ",") + `]}}`
}

// An assistant turn is one request: it carries the usage the provider billed, the model, the
// working directory and a timestamp. Those four are what a dashboard row is made of.
func TestATranscriptTurnYieldsTheUsageThatWasBilled(t *testing.T) {
	home := t.TempDir()
	// Shaped exactly as the real files are — verified against a 19 MB transcript on a real
	// machine rather than from documentation.
	line := `{"type":"assistant","sessionId":"s-1","cwd":"/Users/v/proj",` +
		`"timestamp":"2026-09-10T06:52:21.341Z","message":{"model":"claude-opus-5","role":"assistant",` +
		`"usage":{"input_tokens":2,"cache_creation_input_tokens":62783,"cache_read_input_tokens":0,` +
		`"output_tokens":442},"content":[{"type":"text","text":"hello"}]}}`
	path := writeJSONL(t, home, "-Users-v-proj", "s-1", line)

	turns, err := ReadTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("got %d turns; want 1", len(turns))
	}
	got := turns[0]
	if got.SessionID != "s-1" {
		t.Errorf("SessionID = %q; want s-1", got.SessionID)
	}
	if got.Model != "claude-opus-5" {
		t.Errorf("Model = %q; want claude-opus-5", got.Model)
	}
	if got.Project != "/Users/v/proj" {
		t.Errorf("Project = %q; want /Users/v/proj — the cwd IS the project", got.Project)
	}
	if got.FreshInput != 2 || got.CacheWrite != 62783 || got.CacheRead != 0 || got.Output != 442 {
		t.Errorf("usage = fresh %d, write %d, read %d, out %d; want 2/62783/0/442",
			got.FreshInput, got.CacheWrite, got.CacheRead, got.Output)
	}
	if got.TS == 0 {
		t.Error("TS is zero; a turn with no timestamp cannot be placed on any axis")
	}
}

// A SKILL INVOCATION IS NAMED IN THE TOOL'S INPUT, not by the tool.
//
// Claude Code invokes every skill through one built-in tool called `Skill`, with the skill's own
// name in `input.skill`. Reading the tool name instead records ten different skills as ten calls
// to "Skill" — which is exactly the mistake the first pass of this analysis made, and it matters
// because "which of my skills have I ever used" is the one question this import exists to answer.
func TestASkillCallIsAttributedToTheSkillNotToTheSkillTool(t *testing.T) {
	home := t.TempDir()
	line := `{"type":"assistant","sessionId":"s-2","cwd":"/Users/v/proj",` +
		`"timestamp":"2026-09-11T10:00:00.000Z","message":{"model":"claude-opus-5","role":"assistant",` +
		`"usage":{"input_tokens":1,"output_tokens":1},"content":[` +
		`{"type":"tool_use","id":"t1","name":"Skill","input":{"skill":"superpowers:brainstorming"}},` +
		`{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"ls"}},` +
		`{"type":"tool_use","id":"t3","name":"mcp__web-search__WebSearch","input":{"query":"x"}}]}}`
	path := writeJSONL(t, home, "-Users-v-proj", "s-2", line)

	turns, err := ReadTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("got %d turns; want 1", len(turns))
	}
	called := turns[0].Called
	want := map[string]bool{
		"superpowers:brainstorming":  true, // the SKILL, not "Skill"
		"Bash":                       true,
		"mcp__web-search__WebSearch": true, // an MCP tool keeps its wire name
	}
	if len(called) != len(want) {
		t.Fatalf("called = %v; want %d entries", called, len(want))
	}
	for _, n := range called {
		if !want[n] {
			t.Errorf("called includes %q, which is not one of the three invocations", n)
		}
	}
	for _, n := range called {
		if n == "Skill" {
			t.Error("a skill invocation was recorded against the `Skill` tool. Every skill goes " +
				"through that one tool, so this collapses every skill in the user's history into " +
				"a single meaningless name")
		}
	}
}

// A transcript is another program's append-only log: it holds entry types this code has never
// seen, and its last line is routinely half-written because the session is still running. A
// reader that fails on either is a reader that cannot be run.
func TestATranscriptReaderSurvivesWhatItDoesNotUnderstand(t *testing.T) {
	home := t.TempDir()
	path := writeJSONL(t, home, "-Users-v-proj", "s-3",
		`{"type":"user","sessionId":"s-3","message":{"role":"user","content":"hi"}}`,
		`{"type":"atis-latch","sessionId":"s-3"}`,
		`{"type":"file-history-snapshot","sessionId":"s-3","snapshot":{"x":1}}`,
		`not json at all`,
		`{"type":"assistant","sessionId":"s-3","cwd":"/Users/v/proj",`+
			`"timestamp":"2026-09-12T10:00:00.000Z","message":{"model":"m","role":"assistant",`+
			`"usage":{"input_tokens":5,"output_tokens":7},"content":[]}}`,
		`{"type":"assistant","sessionId":"s-3","message":{"usage":{"input_`) // truncated mid-write

	turns, err := ReadTranscript(path)
	if err != nil {
		t.Fatalf("a transcript with unknown types and a torn last line must still read: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("got %d turns; want the 1 complete assistant turn", len(turns))
	}
	if turns[0].FreshInput != 5 || turns[0].Output != 7 {
		t.Errorf("the surviving turn lost its usage: %+v", turns[0])
	}
}

// An assistant turn with no usage at all is not a billed request. Counting it would inflate the
// request total with entries that cost nothing — synthetic turns, interrupted ones.
func TestATurnWithNoUsageIsNotARequest(t *testing.T) {
	home := t.TempDir()
	path := writeJSONL(t, home, "-Users-v-proj", "s-4",
		`{"type":"assistant","sessionId":"s-4","cwd":"/Users/v/proj",`+
			`"timestamp":"2026-09-13T10:00:00.000Z","message":{"model":"<synthetic>","role":"assistant",`+
			`"content":[{"type":"text","text":"x"}]}}`,
		`{"type":"assistant","sessionId":"s-4","cwd":"/Users/v/proj",`+
			`"timestamp":"2026-09-13T10:00:01.000Z","message":{"model":"m","role":"assistant",`+
			`"usage":{"input_tokens":0,"output_tokens":0},"content":[]}}`)

	turns, err := ReadTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 0 {
		t.Errorf("got %d turns; want none — neither entry was billed anything", len(turns))
	}
}

// A BILLED turn with no usable timestamp is dropped, not dated to zero.
//
// time.Parse returns the zero Time on failure, so ignoring its error would place a real request
// at 1 January 1970 — on the axis, in the per-day totals, and in whatever window the reader
// selected. Spend that landed on a day it did not happen is worse than spend that is missing,
// because the missing kind is visible.
func TestATurnWithNoUsableTimestampIsDroppedRatherThanDatedToZero(t *testing.T) {
	home := t.TempDir()
	path := writeJSONL(t, home, "-Users-v-proj", "s-6",
		// Billed, so the usage gate passes it — only the timestamp is unusable.
		`{"type":"assistant","sessionId":"s-6","cwd":"/Users/v/proj",`+
			`"message":{"model":"m","role":"assistant",`+
			`"usage":{"input_tokens":900,"output_tokens":90},"content":[]}}`,
		`{"type":"assistant","sessionId":"s-6","cwd":"/Users/v/proj","timestamp":"not-a-date",`+
			`"message":{"model":"m","role":"assistant",`+
			`"usage":{"input_tokens":900,"output_tokens":90},"content":[]}}`,
		// One good turn, so a reader that drops everything also fails.
		`{"type":"assistant","sessionId":"s-6","cwd":"/Users/v/proj",`+
			`"timestamp":"2026-09-15T10:00:00.000Z","message":{"model":"m","role":"assistant",`+
			`"usage":{"input_tokens":1,"output_tokens":1},"content":[]}}`)

	turns, err := ReadTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("got %d turns; want only the 1 with a usable timestamp", len(turns))
	}
	if turns[0].TS == 0 {
		t.Error("a turn was returned with TS=0, which places it in 1970")
	}
}

// The working directory is a client-chosen string on disk, so it goes through the same gate a
// live request's does: an absolute path, no credential shapes, length-capped.
func TestATranscriptsWorkingDirectoryIsSanitised(t *testing.T) {
	home := t.TempDir()
	const canary = "sk-ant-api03-ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"
	path := writeJSONL(t, home, "-secret", "s-5",
		`{"type":"assistant","sessionId":"s-5","cwd":"/Users/v/`+canary+`",`+
			`"timestamp":"2026-09-14T10:00:00.000Z","message":{"model":"m","role":"assistant",`+
			`"usage":{"input_tokens":1,"output_tokens":1},"content":[]}}`)

	turns, err := ReadTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("got %d turns; want 1", len(turns))
	}
	if strings.Contains(turns[0].Project, canary) {
		t.Fatalf("a credential-shaped directory name reached the Project field: %q",
			turns[0].Project)
	}
}
