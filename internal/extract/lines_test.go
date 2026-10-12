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
