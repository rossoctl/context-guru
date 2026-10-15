package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/internal/cheapmodel"
)

// The ask must go out under the same anthropic-beta header as the session request whose body it
// appends to, or it can miss the session's cache (or 400 on a long-context prefix).
func TestPrefixAskReplaysTheSessionsBetaHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("anthropic-beta")
		w.Write([]byte(`{"content":[{"type":"text","text":"[]"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()
	st := newSentStash()
	body := []byte(`{"messages":[{"role":"user","content":"a"}]}`)
	st.put("s", bschemas.Anthropic, body, body, "context-1m-2025-08-07")
	p := prefixAsker{stash: st, cli: cheapmodel.Anthropic{BaseURL: srv.URL, APIKey: "k", Model: "m"}}
	if _, _, err := p.AskAs(context.Background(), "s", "ask", "user"); err != nil {
		t.Fatal(err)
	}
	if got != "context-1m-2025-08-07" {
		t.Errorf("AskAs sent anthropic-beta %q", got)
	}
	got = "unset"
	if _, _, err := p.Ask(context.Background(), "s", "ask"); err != nil || got != "context-1m-2025-08-07" {
		t.Errorf("Ask sent anthropic-beta %q (err %v)", got, err)
	}
}
