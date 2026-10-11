package offload

import (
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

// A stale checkpoint (the new tail since it is past resummarize_tokens) must still be SENT while
// the fresh summary that replaces it is being made. Before the fix, the commission path returned
// the full transcript instead, so with large tool outputs every turn paid for a new summary and
// no summary was ever applied (live, integration check on main 2f6bd96: high committed 3, spliced 0).
func TestCacheAwareReplaysAStaleCheckpointWhileTheFreshOneIsMade(t *testing.T) {
	s := newCacheAware(t, "keep_last_turns: 2\nmin_tokens: 10\nresummarize_tokens: 50\n"+
		"trigger:\n  min_messages: 4\n  min_request_tokens: 10\n  min_request_frac: 0\n"+
		"instruction_role: user\n")
	s.modelClient = &capturingModel{out: "<summary>explored the handler.</summary>"}

	in := caFixture()
	c := caCtx("ca-stale-replay")
	if t1, _ := caTurn(t, s, c, in); len(t1.Input) != len(in) {
		t.Fatal("turn 1 must forward untouched")
	}
	if !WaitForSummaryForTest(c.Session, 5*time.Second) {
		t.Fatal("the commissioned summary never landed")
	}
	if _, ok := loadCheckpoint(c); !ok {
		t.Fatal("no checkpoint after turn 1: the path under test cannot run")
	}

	// Turn 2: a large new tool exchange, far past resummarize_tokens, so the checkpoint is stale.
	body := strings.Repeat("go test ./... output line with a failure in pkg/x\n", 60)
	in2 := append(append([]bschemas.ChatMessage(nil), in...),
		caToolPair("running the suite", "bash", `{"c":"go test"}`, "call_9", body)...)
	in2 = append(in2, caMsg(bschemas.ChatMessageRoleUser, "and now?"))
	startedBefore, _, _, _ := CacheAwareAsyncStats()
	t2, _ := caTurn(t, s, c, in2)
	if len(t2.Input) >= len(in2) {
		t.Fatalf("turn 2 forwarded %d messages, the full %d: the stale checkpoint was not replayed",
			len(t2.Input), len(in2))
	}
	if started, _, _, _ := CacheAwareAsyncStats(); started == startedBefore {
		t.Fatal("no fresh summary was commissioned for the stale checkpoint")
	}
	WaitForSummaryForTest(c.Session, 5*time.Second)
}
