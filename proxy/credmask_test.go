package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/dispatch"
)

const fakeSecret = "sk-test-SECRET-123"

// heldAuth keeps the auth slots only. Every other header, and the proxy's own token, stays out.
func TestHeldAuthKeepsOnlyAuthHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+fakeSecret)
	h.Set("X-Api-Key", fakeSecret)
	h.Set("X-Passwd", "other-secret")
	h.Set("X-Bob-Session", "other-session")
	h.Set("Content-Type", "application/json")
	got := heldAuth(upstream{}, h)
	if len(got) != 2 {
		t.Fatalf("heldAuth kept %d headers, want 2 (authorization, x-api-key): %+v", len(got), got)
	}
	for _, g := range got {
		switch http.CanonicalHeaderKey(g.name) {
		case "Authorization", "X-Api-Key":
		default:
			t.Errorf("heldAuth kept a non-auth header %q", g.name)
		}
	}
}

// A server-held key means nothing of the caller's is held.
func TestHeldAuthIsNilWhenTheUpstreamInjectsAKey(t *testing.T) {
	h := http.Header{"Authorization": {"Bearer " + fakeSecret}}
	if got := heldAuth(upstream{setKey: func(http.Header) {}}, h); got != nil {
		t.Errorf("heldAuth held %+v for a server-held key", got)
	}
}

// maskHeaders output differs from the plaintext and unmasks back to it.
func TestMaskHeadersMasksAndUnmasks(t *testing.T) {
	h := http.Header{"Authorization": {"Bearer " + fakeSecret}}
	held := maskHeaders(h)
	if len(held) != 1 {
		t.Fatalf("want 1 held header, got %d", len(held))
	}
	if bytes.Contains(held[0].val, []byte(fakeSecret)) || string(held[0].val) == "Bearer "+fakeSecret {
		t.Fatal("the held value is still plaintext")
	}
	c := held[0].clone()
	credMask(c.val) // XOR twice gives the plaintext back
	if string(c.val) != "Bearer "+fakeSecret {
		t.Errorf("unmask gave %q", c.val)
	}
	zeroHeaders([]heldHeader{c})
	if !bytes.Equal(c.val, make([]byte, len(c.val))) {
		t.Error("zeroHeaders left bytes behind")
	}
}

// target and Dispatcher hold a credential in memory only. Serialized, they must show none of it.
func TestTargetAndDispatcherSerializeToNoSecret(t *testing.T) {
	tg := &target{
		kind: kindSession,
		up:   upstream{base: "https://example.invalid", path: "/v1/messages"},
		auth: maskHeaders(http.Header{"Authorization": {"Bearer " + fakeSecret}, "X-Api-Key": {fakeSecret}}),
	}
	d := NewDispatcher(nil)
	d.held = map[dispatch.Ref]*target{"r1": tg}
	for name, v := range map[string]any{"target": tg, "dispatcher": d} {
		j, _ := json.Marshal(v) // an error or "{}" is fine: nothing secret is shown
		for form, s := range map[string]string{
			"json": string(j), "%+v": fmt.Sprintf("%+v", v), "%v": fmt.Sprintf("%v", v), "%#v": fmt.Sprintf("%#v", v),
		} {
			if strings.Contains(s, fakeSecret) {
				t.Errorf("%s under %s shows the secret", name, form)
			}
		}
	}
}

// CONTEXT_GURU_CAPTURE writes the body, the provider and the model. No header, so no credential.
func TestCaptureWritesNoHeaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.jsonl")
	old := capturePath
	capturePath = path
	defer func() { capturePath = old }()
	captureRequest("anthropic", []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), fakeSecret) {
		t.Fatal("the capture file holds the secret")
	}
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(b), &rec); err != nil {
		t.Fatal(err)
	}
	for k := range rec {
		if k != "provider" && k != "model" && k != "body" {
			t.Errorf("the capture record has an unexpected field %q", k)
		}
	}
}
