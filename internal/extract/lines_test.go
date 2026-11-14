package extract

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type linesReply string

func (r linesReply) Complete(context.Context, string) (string, error) { return string(r), nil }

// The result is exactly the named lines, in order, so it is a subsequence of the body whatever the
// model says; ranges past the end are clamped and a reply that is not the ranges JSON produces nothing.
func TestLinesKeepsOnlyTheNamedLinesVerbatim(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&b, "2026-10-13 worker-%d step %d ok\n", i%3, i)
	}
	b.WriteString("Traceback: ValueError in parse_config at line 12")
	body := b.String()
	cfg := DefaultCfg()
	cfg.Mode, cfg.Floor, cfg.AllowDeterministic = "lines", 10, false
	res, _, strat, why := RunExtractionDetail(context.Background(), body, "fix parse_config", nil, 400, cfg,
		linesReply("```json\n{\"keep\": [[3, 4], [41, 99]]}\n```"))
	want := "2026-10-13 worker-0 step 3 ok\n2026-10-13 worker-1 step 4 ok\nTraceback: ValueError in parse_config at line 12"
	if strat != "lines" || res != want {
		t.Fatalf("got strategy %q reason %q result %q", strat, why, res)
	}
	if res, _, _, why := RunExtractionDetail(context.Background(), body, "x", nil, 400, cfg, linesReply("keep lines 3-4")); res != "" || !strings.Contains(why, "ranges JSON") {
		t.Fatalf("a prose reply was accepted: %q / %q", res, why)
	}
}

// promptLines records the prompt and keeps the line numbers it is told to.
type promptLines struct {
	reply, prompt string
}

func (m *promptLines) Complete(_ context.Context, p string) (string, error) {
	m.prompt = p
	return m.reply, nil
}

// An output longer than the cap is shown as its head and its tail under their real line numbers,
// so the last line (a failure summary, the final traceback) can be kept; the hidden middle cannot.
func TestLinesShowsTheTailOfALongOutput(t *testing.T) {
	n := maxLinesShown + 500
	var b strings.Builder
	for i := 1; i < n; i++ {
		fmt.Fprintf(&b, "test_case_%d PASSED\n", i)
	}
	b.WriteString("FAILED tests/test_parse.py::test_parse_config - ValueError: bad key")
	m := &promptLines{reply: fmt.Sprintf(`{"keep": [[1, 1], [%d, %d]]}`, maxLinesShown/2+1, n)}
	res, why := runLines(context.Background(), b.String(), "fix parse_config", nil, m)
	if !strings.Contains(m.prompt, fmt.Sprintf("%d| FAILED", n)) {
		t.Fatalf("the last line was not shown under its own number (%s)", why)
	}
	// The range starts inside the hidden middle: only the visible tail of it is kept.
	if want := 1 + maxLinesShown/2; len(strings.Split(res, "\n")) != want || !strings.HasSuffix(res, "ValueError: bad key") {
		t.Fatalf("kept %d lines, want %d ending in the failure line: %q", len(strings.Split(res, "\n")), want, res[max(0, len(res)-80):])
	}
}

// keep_identifiers: false changes what is accepted, so it must not share a cached result with a
// config that checks the keep-list.
func TestNoKeepIDsIsPartOfTheResultKey(t *testing.T) {
	cfg := DefaultCfg()
	off := cfg
	off.NoKeepIDs = true
	if ResultKey("k", "m", cfg) == ResultKey("k", "m", off) {
		t.Fatal("a result derived without a keep-list would be served to a config that checks one")
	}
}
