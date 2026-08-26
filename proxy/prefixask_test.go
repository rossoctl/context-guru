package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
)

// THE KEY MUST MATCH, and this test exists because it did not.
//
// Live symptom: prefix_ask_used = 0 with prefix_ask_failed = 0 -- the asker was never built, so the
// mechanism silently never ran and looked exactly like a feature that was switched off. Cause: the
// stash is written under the pipeline's RESOLVED, tenant-scoped session id, while the asker was being
// built from the caller's `x-context-guru-session` header, which this workload never sends. Two
// different keys, no error anywhere.
//
// The component-level tests all injected a fake asker, so none of them touched this wiring. That is
// the gap: a fake that satisfies the interface proves nothing about who supplies the key.
func TestPrefixAskerLooksUpByTheSessionItIsGiven(t *testing.T) {
	t.Setenv("CONTEXT_GURU_PREFIX_ASK", "1")
	var got int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got++
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"content":[{"type":"text","text":"[]"}],
		  "usage":{"input_tokens":5,"output_tokens":2,"cache_read_input_tokens":14651,"cache_creation_input_tokens":0}}`)
	}))
	defer srv.Close()

	h := &Handler{sent: newSentStash(), client: srv.Client()}
	models := components.ModelSpec{
		Incoming: cheapmodel.Anthropic{BaseURL: srv.URL, Model: "m", APIKey: "k", Client: srv.Client()},
	}
	a := h.prefixAskerFor(bschemas.Anthropic, models)
	if a == nil {
		t.Fatal("no asker built even though the feature is on and an incoming client exists")
	}

	// FIRST TURN: nothing stashed. This must be an ERROR the caller can count and fall back on, not a
	// nil asker and not an empty answer -- "no prefix yet" and "the feature is off" must not look alike.
	if _, _, err := a.Ask(context.Background(), "resolved-session-1", "ask"); err == nil {
		t.Error("Ask succeeded with an empty stash; the first turn of a session must report an error")
	}
	if got != 0 {
		t.Error("a request was sent upstream with no prefix to append to")
	}

	// The host stashes what it forwarded, under the RESOLVED id.
	h.sent.put("resolved-session-1", []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))

	_, u, err := a.Ask(context.Background(), "resolved-session-1", "ask")
	if err != nil {
		t.Fatalf("Ask failed for the session that was stashed: %v", err)
	}
	if u.CacheRead != 14651 {
		t.Errorf("cache read not surfaced: %+v", u)
	}

	// A DIFFERENT session must not read another session's transcript. This is the cross-session
	// leak the key mismatch could have become had the empty-string key ever matched a stash entry.
	if _, _, err := a.Ask(context.Background(), "resolved-session-2", "ask"); err == nil {
		t.Error("Ask returned another session's prefix; the stash must be keyed strictly by session")
	}
	if _, _, err := a.Ask(context.Background(), "", "ask"); err == nil {
		t.Error("an empty session id resolved to some stash entry; it must never match")
	}
}

// Off by default, and only where the cache semantics were measured.
func TestPrefixAskerPreconditions(t *testing.T) {
	h := &Handler{sent: newSentStash()}
	models := components.ModelSpec{Incoming: cheapmodel.Anthropic{BaseURL: "http://x", Model: "m", APIKey: "k"}}

	if a := h.prefixAskerFor(bschemas.Anthropic, models); a != nil {
		t.Error("built an asker with CONTEXT_GURU_PREFIX_ASK unset; it must be opt-in")
	}
	t.Setenv("CONTEXT_GURU_PREFIX_ASK", "1")
	if a := h.prefixAskerFor(bschemas.OpenAI, models); a != nil {
		t.Error("built an asker for OpenAI; the appended-message and tool_choice cache facts were " +
			"measured on Anthropic only, and guessing another provider's cache semantics is how a " +
			"claimed cache read becomes a silent 10x bill")
	}
	if a := h.prefixAskerFor(bschemas.Anthropic, components.ModelSpec{}); a != nil {
		t.Error("built an asker with no incoming client; ModelSpec.For would hand the component the " +
			"static cheap model, which is in a different cache namespace entirely")
	}
}
