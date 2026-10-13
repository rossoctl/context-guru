package litellm

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// dirtySample is the shape that broke production, minimised: a `sample_spec`
// entry whose numeric fields hold prose, plus a model that spells an integer
// window as a float. A whole-map typed decode fails on the first of these and
// discards every good row with it.
const dirtySample = `{
  "sample_spec": {"max_input_tokens": "max input tokens, if the provider specifies it",
                  "max_tokens": "LEGACY parameter",
                  "deprecation_date": "date when the model becomes deprecated in the format YYYY-MM-DD"},
  "float-window-model": {"max_input_tokens": 128000.0},
  "aws/claude-sonnet-5": {"max_input_tokens": 1000000},
  "prose-window-model": {"max_input_tokens": "quite a lot, actually"}
}`

func TestLiteLLMSkipsMalformedEntries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(dirtySample))
	}))
	defer srv.Close()
	l := NewLiteLLM(srv.URL, srv.Client(), time.Hour)
	ctx := context.Background()
	waitFor(t, l, "float-window-model")

	// A prose-filled sibling entry must not poison the whole map.
	if w, ok := l.Window(ctx, "aws/claude-sonnet-5"); !ok || w != 1000000 {
		t.Fatalf("window(aws/claude-sonnet-5) = %d,%v; want 1000000,true (a malformed sibling entry poisoned the decode)", w, ok)
	}
	// A float-spelled integer must still resolve.
	if w, ok := l.Window(ctx, "float-window-model"); !ok || w != 128000 {
		t.Errorf("float-spelled window = %d,%v; want 128000,true", w, ok)
	}
}

// TestLiteLLMDecodesTheRealDocument runs the decode against a checked-in snapshot
// of the actual upstream document (gzipped; see testdata). This is the regression
// test the package never had: before the per-entry decode it resolved ZERO
// entries from this exact byte sequence.
func TestLiteLLMDecodesTheRealDocument(t *testing.T) {
	f, err := os.Open("testdata/litellm_prices.json.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	l := NewLiteLLM(srv.URL, srv.Client(), time.Hour)
	ctx := context.Background()
	waitFor(t, l, "gpt-4o")

	l.mu.Lock()
	n := len(l.byKey)
	l.mu.Unlock()
	if n < 2900 {
		t.Errorf("decoded %d keys from the real document; want >2900 (a strict whole-map decode yields 0)", n)
	}
	// The two models context-guru is actually deployed against must resolve a
	// non-zero window, or every fraction-based trigger is dead.
	for _, m := range []string{"aws/claude-sonnet-5", "aws/claude-haiku-4-5"} {
		w, ok := l.Window(ctx, m)
		if !ok || w == 0 {
			t.Errorf("window(%s) = %d,%v; want a non-zero window", m, w, ok)
		}
	}
	t.Logf("decoded %d window keys from the real LiteLLM document", n)
}

func waitFor(t *testing.T, l *LiteLLM, model string) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if _, ok := l.Window(context.Background(), model); ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("litellm map never loaded (%s never resolved)", model)
}
