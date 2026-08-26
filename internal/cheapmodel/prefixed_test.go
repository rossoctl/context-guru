package cheapmodel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// The wire construction is the whole mechanism: every byte before the appended message is prefix, and
// any edit to it costs the cache read this exists for. Measured facts being enforced here (see
// docs/experiments/loca/iter019/results.md §2): tools ARE part of the cache key, tool_choice is NOT,
// and this route rejects assistant prefill so the ask must be a trailing USER message.
func TestCompletePrefixedPreservesThePrefix(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"content":[{"type":"thinking","text":""},{"type":"text","text":"[{\"i\":2}]"}],
		  "usage":{"input_tokens":12,"output_tokens":7,"cache_read_input_tokens":19595,"cache_creation_input_tokens":0}}`)
	}))
	defer srv.Close()

	prefix := []byte(`{"model":"claude-x","stream":true,"system":"be terse",
	  "tools":[{"name":"Read","description":"d","input_schema":{"type":"object"}}],
	  "messages":[{"role":"user","content":"go"},
	              {"role":"assistant","content":[{"type":"text","text":"working"}]}]}`)

	a := Anthropic{BaseURL: srv.URL, Model: "claude-x", APIKey: "k", Client: srv.Client(), MaxTokens: 4000}
	reply, u, err := a.CompletePrefixed(context.Background(), prefix, "ADJUDICATE THIS")
	if err != nil {
		t.Fatalf("CompletePrefixed: %v", err)
	}
	if reply != `[{"i":2}]` {
		t.Errorf("reply = %q; a leading thinking block must be skipped, not returned", reply)
	}
	if u.CacheRead != 19595 || u.CacheWrite != 0 {
		t.Errorf("usage not surfaced: %+v -- the caller must be able to see whether the cache read "+
			"actually happened, since a miss costs ~10x and looks identical otherwise", u)
	}

	msgs := gjson.GetBytes(got, "messages").Array()
	if n := len(msgs); n != 3 {
		t.Fatalf("expected the ask appended as a 3rd message, got %d", n)
	}
	if r := msgs[2].Get("role").String(); r != "user" {
		t.Errorf("appended message role = %q, want user: this route rejects assistant prefill", r)
	}
	if c := msgs[2].Get("content").String(); c != "ADJUDICATE THIS" {
		t.Errorf("appended content = %q", c)
	}
	// The prefix must be untouched, byte-for-byte, or there is no cache hit.
	if msgs[0].Raw != gjson.GetBytes(prefix, "messages.0").Raw ||
		msgs[1].Raw != gjson.GetBytes(prefix, "messages.1").Raw {
		t.Error("an existing message was rewritten; every byte before the appended ask is prefix")
	}
	if gjson.GetBytes(got, "system").String() != "be terse" {
		t.Error("system was altered; it hashes before messages, so this misses from position zero")
	}
	if gjson.GetBytes(got, "tools").Raw != gjson.GetBytes(prefix, "tools").Raw {
		t.Error("tools were altered; tools ARE part of the cache key -- dropping them read a " +
			"different, smaller cache entry when measured")
	}
	// tool_choice must be ABSENT. This assertion used to demand {"type":"none"}, on the reasoning that
	// the prefix carries the agent's tools so a tool_use reply had to be suppressed. Measurement
	// reversed it: with "none" the model answered in PROSE and produced no verdicts at all (0 of 6
	// labels), while with tool_choice omitted it called the advertised structured-answer tool and
	// covered the whole batch (6 of 6, on 4 of 4 trials) at the same cache-read price. Forcing a
	// named tool is a third option and NOT free -- it wrote a separate cache entry.
	if tc := gjson.GetBytes(got, "tool_choice"); tc.Exists() {
		t.Errorf("tool_choice was set to %q; it must be omitted, or the model answers in prose "+
			"instead of calling the structured-answer tool", tc.Raw)
	}
	if gjson.GetBytes(got, "stream").Exists() {
		t.Error("stream survived; the caller wants one JSON answer, not a stream")
	}
	if mt := gjson.GetBytes(got, "max_tokens").Int(); mt != 4000 {
		t.Errorf("max_tokens = %d, want 4000", mt)
	}
}

// A prefix that is not a conversation must be refused rather than silently sent, because the caller's
// fallback (a plain completion) is correct and a malformed send is not.
func TestCompletePrefixedRejectsNonConversation(t *testing.T) {
	a := Anthropic{BaseURL: "http://127.0.0.1:1", Model: "m", APIKey: "k"}
	if _, _, err := a.CompletePrefixed(context.Background(), []byte(`{"model":"m"}`), "ask"); err == nil {
		t.Error("a body with no messages array was accepted")
	} else if !strings.Contains(err.Error(), "messages") {
		t.Errorf("unhelpful error: %v", err)
	}
	var js map[string]any
	if json.Unmarshal([]byte(`{"model":"m"}`), &js) != nil {
		t.Fatal("fixture is not JSON")
	}
}
