package cheapmodel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAnthropicSkipsNonTextLeadingBlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"content":[{"type":"thinking","text":""},{"type":"text","text":"RESULT"}]}`)
	}))
	defer srv.Close()
	got, err := Anthropic{BaseURL: srv.URL, Model: "m"}.Complete(context.Background(), "p")
	if err != nil || got != "RESULT" {
		t.Fatalf("got %q err %v", got, err)
	}
}

// authHeaders is what a fixture upstream saw on the wire. Handlers run on the test server's
// own goroutine, so the values travel back over a buffered channel rather than through a
// captured variable: the HTTP round trip is not a happens-before edge, and a plain capture
// read by the test goroutine is a data race whether or not -race happens to observe it.
type authHeaders struct{ auth, key, version string }

func TestAnthropicBearerAuth(t *testing.T) {
	seen := make(chan authHeaders, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- authHeaders{r.Header.Get("Authorization"), r.Header.Get("x-api-key"),
			r.Header.Get("anthropic-version")}
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"OK"}]}`)
	}))
	defer srv.Close()
	_, err := Anthropic{BaseURL: srv.URL, APIKey: "tok", Model: "m", AuthScheme: "bearer"}.Complete(context.Background(), "p")
	if err != nil {
		t.Fatalf("err %v", err)
	}
	got := <-seen
	gotAuth, gotKey, gotVersion := got.auth, got.key, got.version
	if gotAuth != "Bearer tok" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer tok")
	}
	if gotKey != "" {
		t.Fatalf("x-api-key = %q, want empty", gotKey)
	}
	if gotVersion != "2023-06-01" {
		t.Fatalf("anthropic-version = %q, want 2023-06-01", gotVersion)
	}
}

func TestAnthropicDefaultAuthUsesAPIKey(t *testing.T) {
	seen := make(chan authHeaders, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- authHeaders{auth: r.Header.Get("Authorization"), key: r.Header.Get("x-api-key")}
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"OK"}]}`)
	}))
	defer srv.Close()
	_, err := Anthropic{BaseURL: srv.URL, APIKey: "tok", Model: "m"}.Complete(context.Background(), "p")
	if err != nil {
		t.Fatalf("err %v", err)
	}
	got := <-seen
	gotAuth, gotKey := got.auth, got.key
	if gotKey != "tok" {
		t.Fatalf("x-api-key = %q, want %q", gotKey, "tok")
	}
	if gotAuth != "" {
		t.Fatalf("Authorization = %q, want empty", gotAuth)
	}
}

func TestOpenAIComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"OUT"}}]}`)
	}))
	defer srv.Close()
	got, err := OpenAI{BaseURL: srv.URL, Model: "m"}.Complete(context.Background(), "p")
	if err != nil || got != "OUT" {
		t.Fatalf("got %q err %v", got, err)
	}
}
