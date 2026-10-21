package dash

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func msgLine(sess, id, cwd string, tsMs, cacheRead, out int64, blocks ...string) string {
	ts := time.UnixMilli(tsMs).UTC().Format(time.RFC3339Nano)
	return `{"type":"assistant","sessionId":"` + sess + `","cwd":"` + cwd + `","timestamp":"` + ts +
		`","message":{"id":"` + id + `","model":"m","usage":{"input_tokens":1,` +
		`"cache_creation_input_tokens":0,"cache_read_input_tokens":` + strconv.FormatInt(cacheRead, 10) +
		`,"output_tokens":` + strconv.FormatInt(out, 10) + `},"content":[` + strings.Join(blocks, ",") + `]}}`
}

// B3: one streamed message is written as several lines sharing an id and a usage block.
func TestOneMessageIDIsOneBilledTurn(t *testing.T) {
	home := t.TempDir()
	path := writeJSONL(t, home, "-p", "s1",
		msgLine("s1", "msg_A", "/p", 1000, 500, 10, `{"type":"text","text":"hi"}`),
		msgLine("s1", "msg_A", "/p", 1040, 500, 10, `{"type":"tool_use","name":"Bash","input":{}}`),
		msgLine("s1", "msg_A", "/p", 1090, 500, 10, `{"type":"tool_use","name":"Read","input":{}}`),
		msgLine("s1", "msg_B", "/p", 5000, 700, 20))
	turns, err := ReadTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("turns = %d; want 2 (msg_A once, msg_B once)", len(turns))
	}
	var cr int64
	for _, tn := range turns {
		cr += tn.CacheRead
	}
	if cr != 1200 {
		t.Errorf("cache_read = %d; want 1200, not triple-counted", cr)
	}
	if got := strings.Join(turns[0].Called, ","); got != "Bash,Read" {
		t.Errorf("Called = %q; want the union Bash,Read across the id's lines", got)
	}
}

// N1: a line longer than the buffer must be reported, not swallowed.
func TestAnOversizedLineIsReportedAsTruncation(t *testing.T) {
	home := t.TempDir()
	long := `{"type":"user","x":"` + strings.Repeat("a", transcriptMaxLine+10) + `"}`
	path := writeJSONL(t, home, "-p", "s1",
		msgLine("s1", "m1", "/p", 1000, 5, 5), long, msgLine("s1", "m2", "/p", 2000, 5, 5))
	turns, truncated, err := ReadTranscriptChecked(path)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Error("truncated = false; the rest of the file was lost silently")
	}
	if len(turns) != 1 {
		t.Errorf("turns = %d; want the 1 read before the oversize line", len(turns))
	}
}

// S5: neither a symlinked file nor a symlinked directory is read.
func TestTranscriptFilesSkipSymlinks(t *testing.T) {
	home := t.TempDir()
	real := writeJSONL(t, home, "-real", "s1", msgLine("s1", "m1", "/p", 1000, 5, 5))
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "x.jsonl"), []byte(msgLine("x", "m", "/q", 1000, 5, 5)+"\n"), 0o644)
	proj := filepath.Join(home, ".claude", "projects")
	os.Symlink(real, filepath.Join(proj, "-real", "link.jsonl"))
	os.Symlink(outside, filepath.Join(proj, "-linkdir"))
	files := TranscriptFiles(home)
	if len(files) != 1 || files[0] != real {
		t.Errorf("files = %v; want only %s", files, real)
	}
}
