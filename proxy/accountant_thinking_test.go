package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/accountant"

	"github.com/rossoctl/context-guru/thinkcount"
)

// Issue #1187: a thinking block the client sends back is priced at the thinking tokens of the response that
// made it, not at the accountant's fixed price of 1 token for 250 bytes.

const thinkSig = "SIG" // repeated to make a signature of the size a real one has

func thinkingMsg(sig string) string {
	return `{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"` + sig +
		`"},{"type":"text","text":"done"}]}`
}

// dropThinking runs one request where the operator drops the thinking block of message 1, and returns the
// tokens the drop saved.
func dropThinking(t *testing.T, s *savingsTracker, sig string) int {
	t.Helper()
	m0 := mustMsg("user", strings.Repeat("question ", 300))
	withBlock, without := thinkingMsg(sig), mustMsg("assistant", "done")
	m2 := mustMsg("user", "next")
	client, fwd := body(m0, withBlock, m2), body(m0, without, m2)
	const id = "cg:thinking:1"
	op := rewriteOp(id, 1, len(withBlock), len(without))
	c := accRun{s, accountant.Thread{ID: "main"}}
	// The provider bills the forwarded body's text, about 4 bytes a token.
	res := c.do(1, client, fwd, Usage{CacheWrite: int64(len(fwd) / 4)}, op)
	_, saved := countChange(res, id)
	return saved
}

func TestSavings_DroppedThinkingBlockSavesItsCount(t *testing.T) {
	sig := strings.Repeat(thinkSig, 1000) // 3,000 bytes
	if saved := dropThinking(t, newSavingsTracker(), sig); saved > 100 {
		t.Fatalf("precondition: with no count the drop saved %d tokens; the fixed price is about 12", saved)
	}
	s := newSavingsTracker()
	s.recordThinking(thinkcount.FromJSON([]byte(`{"content":[{"type":"thinking","thinking":"","signature":"` + sig +
		`"}],"usage":{"output_tokens":1100,"output_tokens_details":{"thinking_tokens":1000}}}`)))
	// The block's 1,000 tokens, and the few tokens of its JSON wrapper at the text ratio.
	if saved := dropThinking(t, s, sig); saved < 1000 || saved > 1050 {
		t.Fatalf("with the count the drop saved %d tokens, want 1,000 and its wrapper", saved)
	}
}

// The counts are saved with the state, so a block that comes back after a restart keeps its count.
func TestSavings_ThinkingCountsSurviveARestart(t *testing.T) {
	sig := strings.Repeat(thinkSig, 1000)
	path := filepath.Join(t.TempDir(), "state.json")
	s := newSavingsTracker()
	s.recordThinking(thinkcount.Response{Opaques: []string{sig}, Tokens: 1000, HasTokens: true})
	if err := s.save(path); err != nil {
		t.Fatal(err)
	}
	r := newSavingsTracker()
	if !r.restore(path, time.Now()) {
		t.Fatal("nothing restored")
	}
	if n, ok := r.thinking.Lookup(accountant.OpaqueKey(sig)); !ok || n != 1000 {
		t.Fatalf("after the restore the count is %d %v, want 1000", n, ok)
	}
}

// On the stream path the signatures arrive in the middle, outside the sniffer's window. The collector reads
// them as the bytes go by, and the client gets the same bytes.
func TestStreamReadsThinkingSignatures(t *testing.T) {
	sig := strings.Repeat(thinkSig, 100000) // 300 KB, far past the sniffer's head and tail
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\",\"signature\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"" + sig + "\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":5,\"output_tokens\":70,\"output_tokens_details\":{\"thinking_tokens\":62}}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(stream))
	}))
	defer up.Close()
	resp, err := http.Get(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h := &Handler{savings: newSavingsTracker()}
	_, u, _, _ := h.streamRec(rec, resp, nil)
	if rec.Body.String() != stream {
		t.Fatalf("the client got other bytes")
	}
	if th := u.Thinking; len(th.Opaques) != 1 || th.Opaques[0] != sig || th.Tokens != 62 || !th.HasTokens {
		t.Fatalf("thinking = %d opaques, %d tokens, %v", len(th.Opaques), th.Tokens, th.HasTokens)
	}
	// With no savings tracker there is nothing to record, and nothing is collected.
	resp2, err := http.Get(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, u2, _, _ := (&Handler{}).streamRec(httptest.NewRecorder(), resp2, nil); len(u2.Thinking.Opaques) != 0 {
		t.Fatalf("a proxy with no accountant collected signatures")
	}
}
