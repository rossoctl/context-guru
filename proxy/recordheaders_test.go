//go:build cg_record

package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
)

// sendWithSecretHeaders drives one call through a real Handler with the given recorder.
// It returns after the client has the response.
func sendWithSecretHeaders(t *testing.T, rec *sessionRecorder, hdr map[string]string) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(testSSE))
	}))
	defer up.Close()
	cfg, err := config.LoadBytes([]byte("preset: conservative\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{
		AnthropicUpstream: up.URL, Preset: cfg.Preset, Record: rec,
	})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages",
		strings.NewReader(string(ccBody("please fix the failing test"))))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func readAllFiles(t *testing.T, dir string) string {
	t.Helper()
	var sb strings.Builder
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			b, _ := os.ReadFile(p)
			sb.Write(b)
		}
		return nil
	})
	return sb.String()
}

// No credential reaches the wire file, whatever the header is called, and an allowed header is
// kept. The names X-Passwd, X-Signature and X-Bob-Session passed the old name denylist.
func TestRecordWritesHeadersByAllowlist(t *testing.T) {
	const secret = "sk-test-SECRET-123"
	r, dir := newTestRecorder(t)
	sendWithSecretHeaders(t, r, map[string]string{
		"Content-Type":      "application/json",
		"Anthropic-Version": "2023-06-01",
		"User-Agent":        "claude-cli/2.0.14 (external, cli)",
		"Authorization":     "Bearer " + secret,
		"X-Api-Key":         secret,
		"X-Passwd":          secret,
		"X-Signature":       secret,
		"X-Bob-Session":     secret,
	})
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	all := readAllFiles(t, dir)
	if all == "" {
		t.Fatal("the recorder wrote nothing")
	}
	if strings.Contains(all, secret) {
		t.Errorf("the secret reached the record directory")
	}
	for _, name := range []string{"x-passwd", "x-signature", "x-bob-session", "authorization", "x-api-key"} {
		if strings.Contains(strings.ToLower(all), `"`+name+`"`) {
			t.Errorf("header %q was written", name)
		}
	}
	var kept bool
	for _, lines := range wireLines(t, dir) {
		for _, l := range lines {
			hd, _ := l["headers"].(map[string]any)
			if hd["anthropic-version"] == "2023-06-01" {
				kept = true
			}
		}
	}
	if !kept {
		t.Error("the allowed header anthropic-version was not kept")
	}
}

// CONTEXT_GURU_CAPTURE, run through the handler, writes no header and so no credential.
func TestCaptureThroughTheHandlerWritesNoCredential(t *testing.T) {
	const secret = "sk-test-SECRET-123"
	path := filepath.Join(t.TempDir(), "capture.jsonl")
	old := capturePath
	capturePath = path
	defer func() { capturePath = old }()
	sendWithSecretHeaders(t, nil, map[string]string{
		"Authorization": "Bearer " + secret, "X-Api-Key": secret, "X-Passwd": secret,
	})
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		t.Fatalf("no capture written: %v", err)
	}
	if strings.Contains(string(b), secret) {
		t.Error("the capture file holds the secret")
	}
}
