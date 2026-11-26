package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/accountant"

	"github.com/rossoctl/context-guru/agent"
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
	_, u, _, _ := h.streamRecOn(rec, resp, nil, agent.Client{}, agent.Base{}, h.thinkCollector(bschemas.Anthropic, resp))
	if rec.Body.String() != stream {
		t.Fatalf("the client got other bytes")
	}
	if th := u.Thinking; len(th.Opaques) != 1 || th.Opaques[0] != sig || th.Tokens != 62 || !th.HasTokens {
		t.Fatalf("thinking = %d opaques, %d tokens, %v", len(th.Opaques), th.Tokens, th.HasTokens)
	}
	// The catch-all route and /count_tokens use stream, which calls no finish: nothing is collected there
	// (review of #1190, round 2). Nor with no accountant, or for another provider.
	resp2, err := http.Get(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, u2, _, _ := h.stream(httptest.NewRecorder(), resp2); len(u2.Thinking.Opaques) != 0 {
		t.Fatalf("the catch-all path collected signatures")
	}
	if (&Handler{}).thinkCollector(bschemas.Anthropic, resp) != nil || h.thinkCollector(bschemas.OpenAI, resp) != nil {
		t.Fatalf("a collector was made with no accountant, or for OpenAI")
	}
}

// The whole path, through the handler: a served response's blocks reach the table, on the JSON path and the
// stream path. A rejected response records nothing (review of #1190, round 1).
func TestServedResponseRecordsThinkingCounts(t *testing.T) {
	sig := strings.Repeat(thinkSig, 2000)
	jsonBody := `{"id":"m","type":"message","role":"assistant","model":"m","content":[{"type":"thinking","thinking":"",` +
		`"signature":"` + sig + `"},{"type":"text","text":"ok"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":20,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":90,` +
		`"output_tokens_details":{"thinking_tokens":80}}}`
	sseBody := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":20,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0,\"output_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\",\"signature\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"" + sig + "\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":20,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0,\"output_tokens\":90,\"output_tokens_details\":{\"thinking_tokens\":80}}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	for _, c := range []struct {
		name, contentType, body string
		status                  int
		want                    bool
	}{
		{"json", "application/json", jsonBody, http.StatusOK, true},
		{"stream", "text/event-stream", sseBody, http.StatusOK, true},
		{"rejected", "application/json", jsonBody, http.StatusBadRequest, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", c.contentType)
				w.WriteHeader(c.status)
				w.Write([]byte(c.body))
			}))
			defer up.Close()
			h, rec, _ := accHandler(t, up.URL)
			accPost(t, h, "sess-think-"+c.name)
			waitForRows(t, rec, 1)
			n, ok := h.savings.thinking.Lookup(accountant.OpaqueKey(sig))
			if ok != c.want || (c.want && n != 80) {
				t.Fatalf("table has %d %v for the block, want recorded=%v at 80", n, ok, c.want)
			}
		})
	}
}

// The counts outlive the snapshot: a file past the snapshot's hour, or one whose snapshot is refused, still
// gives back its counts (review of #1190, round 1).
func TestThinkingCountsRestoreOnTheirOwn(t *testing.T) {
	sig := strings.Repeat(thinkSig, 1000)
	path := filepath.Join(t.TempDir(), "state.json")
	s := newSavingsTracker()
	s.recordThinking(thinkcount.Response{Opaques: []string{sig}, Tokens: 1000, HasTokens: true})
	if err := s.save(path); err != nil {
		t.Fatal(err)
	}
	old := newSavingsTracker()
	if old.restore(path, time.Now().Add(accStateMaxAge+time.Minute)) {
		t.Fatal("a snapshot older than an hour was restored")
	}
	if _, ok := old.thinking.Lookup(accountant.OpaqueKey(sig)); !ok {
		t.Fatal("the counts of a 1-hour-old file were not restored")
	}
	if r := newSavingsTracker(); r.restore(path, time.Now().Add(accThinkingMaxAge+time.Minute)) || r.thinking.Len() != 0 {
		t.Fatal("a file older than the counts' limit gave something back")
	}
	// A refused snapshot: the counts are still read.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f map[string]json.RawMessage
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	f["snapshot"] = json.RawMessage(`{"version":999}`) // a library snapshot version Restore refuses
	bad, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	r := newSavingsTracker()
	if r.restore(path, time.Now()) {
		t.Fatal("a refused snapshot was reported restored")
	}
	if _, ok := r.thinking.Lookup(accountant.OpaqueKey(sig)); !ok {
		t.Fatal("the counts were dropped with the refused snapshot")
	}
}
