package offload

import (
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

func blobMaskFor(t *testing.T, extraYAML string) *BlobMask {
	t.Helper()
	comp, err := newBlobMask([]byte("min_tokens: 5\n" + extraYAML))
	if err != nil {
		t.Fatal(err)
	}
	return comp.(*BlobMask)
}

// bigDataURI is a real-shaped tool result: a small JSON envelope wrapped around a large
// inline screenshot, the shape measured in production (browser-automation `tool_result`s).
func bigDataURI() string {
	payload := strings.Repeat("QUJDREVGR0hJSkw=", 40) // 640 base64 chars, well past the 200 floor
	return `{"session":{"id":"abc","status":"ready","screenshotDataUrl":"data:image/jpeg;base64,` + payload + `"}}`
}

// tailReq puts a small, innocuous cached message at index 0 and the message under test at
// index 1 — the uncached tail under coldGateCtx's MaxCachedIdx: 0 — so a NEW mask is legal
// there without invoking cold_cache.
func tailReq(body string) *bschemas.BifrostChatRequest {
	return &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool("cached filler"), tool(body)}}
}

func TestBlobMaskCollapsesADominantEmbeddedPayload(t *testing.T) {
	b := blobMaskFor(t, "")
	original := bigDataURI()
	req := tailReq(original)
	var rep components.Report
	if _, err := b.Offload(req, &rep, coldGateCtx(store.NewMemory(store.Options{}), false)); err != nil {
		t.Fatal(err)
	}
	got := schema.MessageText(req.Input[1])
	if !strings.Contains(got, "embedded binary payload masked") {
		t.Fatalf("expected a mask marker in the tail message, got %q", got)
	}
	if strings.Contains(got, "QUJDREVGR0hJSkw=") {
		t.Fatal("the base64 payload leaked into the rewritten message")
	}
	if schema.TextTokens(got) >= schema.TextTokens(original) {
		t.Fatal("masked message must be smaller than the original")
	}
}

// TestBlobMaskExpandRoundTrips is the exactness gate every lossy offloader must pass: the
// original bytes must be recoverable byte-for-byte from whatever the marker's key points at.
func TestBlobMaskExpandRoundTrips(t *testing.T) {
	b := blobMaskFor(t, "")
	original := bigDataURI()
	req := tailReq(original)
	st := store.NewMemory(store.Options{})
	var rep components.Report
	keys, err := b.Offload(req, &rep, coldGateCtx(st, false))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected exactly one stash key, got %d", len(keys))
	}
	stashed, ok := st.Get(keys[0])
	if !ok {
		t.Fatal("stash key not found in the store")
	}
	if string(stashed) != original {
		t.Fatalf("expand would not reproduce the original byte-for-byte\nwant: %s\ngot:  %s", original, stashed)
	}
}

func TestBlobMaskLeavesASmallIncidentalPayloadAlone(t *testing.T) {
	b := blobMaskFor(t, "")
	// A short base64 value (an id/hash, not a payload) beside a lot of real text.
	body := `{"note":"` + strings.Repeat("this is a real explanatory sentence. ", 30) +
		`","token":"YWJjZGVmZ2hpams="}`
	req := tailReq(body)
	var rep components.Report
	if _, err := b.Offload(req, &rep, coldGateCtx(store.NewMemory(store.Options{}), false)); err != nil {
		t.Fatal(err)
	}
	got := schema.MessageText(req.Input[1])
	if got != body {
		t.Fatalf("a short incidental token must not mask the whole message, got %q", got)
	}
	if rep.Gates["no_embedded_blob"] == 0 && rep.Gates["below_min_tokens"] == 0 {
		t.Fatalf("expected a decline gate, got %v", rep.Gates)
	}
}

func TestBlobMaskRespectsTheCachedPrefix(t *testing.T) {
	b := blobMaskFor(t, "cold_cache: false\n")
	original := bigDataURI()
	// deepReq's FIRST message sits inside the cached prefix (MaxCachedIdx = 0 in coldGateCtx).
	req := deepReq(original)
	var rep components.Report
	if _, err := b.Offload(req, &rep, coldGateCtx(store.NewMemory(store.Options{}), false)); err != nil {
		t.Fatal(err)
	}
	got := schema.MessageText(req.Input[0])
	if got != original {
		t.Fatalf("a NEW mask must not touch content already inside the cached prefix, got %q", got)
	}
	if rep.Gates["cached_prefix"] == 0 {
		t.Fatalf("expected the cached_prefix gate, got %v", rep.Gates)
	}
}
