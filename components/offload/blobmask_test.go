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

// TestBlobMaskOnlyTrustsAnExplicitDataURI is the adversarial-exactness table: two classes of
// long base64-alphabet run that are NOT a binary payload and must not be masked, both found
// in review. A long hex digest chain (an SRI integrity block, a `sha256sum` listing) is a
// SUBSET of the base64 alphabet and would be masked by shape alone — throwing away a hash the
// agent may need to compare. Worse, a pagination/continuation token or a Relay-style cursor —
// standard base64, no dots, routinely 200+ chars — is a value the agent needs VERBATIM on its
// very next tool call; masking it is not just a lossy compaction, it breaks the agent's next
// step until it thinks to expand. Neither is a data URI, so `embeddedBlob`'s single arm
// correctly declines both; the real base64 image payload is the control that confirms the
// component still does its job.
func TestBlobMaskOnlyTrustsAnExplicitDataURI(t *testing.T) {
	b := blobMaskFor(t, "")
	cases := []struct {
		name       string
		body       string
		wantMasked bool
	}{
		{"256-char hex digest chain", jsonBody("sha256", strings.Repeat("a1b2c3d4e5f60789", 16)), false},
		{"10 concatenated sha256-shaped hex digests",
			jsonBody("sha256", strings.Repeat("deadbeefcafebabe0123456789abcdef0123456789abcdef0123456789abcd", 4)), false},
		{"pagination/continuation token (clean base64, no dots)",
			jsonBody("nextPageToken", strings.Repeat("QUJDREVGR0hJSkxNTk9QUVJTVFVWV1hZWjAxMjM0NTY3ODk", 45)), false},
		{"real base64 image payload (control)", bigDataURI(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := tailReq(tc.body)
			var rep components.Report
			if _, err := b.Offload(req, &rep, coldGateCtx(store.NewMemory(store.Options{}), false)); err != nil {
				t.Fatal(err)
			}
			got := schema.MessageText(req.Input[1])
			masked := got != tc.body
			if masked != tc.wantMasked {
				t.Fatalf("wantMasked=%v got masked=%v (gates=%v)", tc.wantMasked, masked, rep.Gates)
			}
		})
	}
}

// jsonBody wraps a value in a minimal JSON envelope under the given field name — the value
// dominates the message (well past MinBlobFrac's 0.6 default), matching how these values
// actually arrive: a lockfile integrity block, a `sha256sum` listing, or a pagination
// response is mostly the value itself, not surrounding prose.
func jsonBody(field, value string) string {
	return `{"note":"ok","` + field + `":"` + value + `"}`
}

// TestBlobMaskNeverMatchesAPlainTokenFieldUnderRealDefaults pins the resolution of a
// back-and-forth in review: does a large pagination/continuation token clear min_tokens and
// min_blob_frac before the regex even matters? Answer, with the ACTUAL shipped defaults
// (newBlobMask([]byte{}), not the test helper's lowered min_tokens:5) and a realistic
// 2,160-char token that easily clears both (min_tokens=500, min_blob_frac=0.6): yes, it
// clears both — and it is STILL not masked, because `embeddedBlob` requires an explicit
// `data:<mime>;base64,` declaration and a bare JSON string value never carries one. This is a
// structural exclusion, not a lucky gate ordering: the gate log below is `no_embedded_blob`,
// not `below_min_tokens` or `blob_not_dominant`.
func TestBlobMaskNeverMatchesAPlainTokenFieldUnderRealDefaults(t *testing.T) {
	comp, err := newBlobMask([]byte("")) // real shipped defaults: min_tokens=500, min_blob_frac=0.6
	if err != nil {
		t.Fatal(err)
	}
	b := comp.(*BlobMask)

	token := strings.Repeat("QUJDREVGR0hJSkxNTk9QUVJTVFVWV1hZWjAxMjM0NTY3ODk", 45) // 2,160 clean base64 chars
	body := jsonBody("nextPageToken", token)
	if got := schema.TextTokens(body); got < 500 {
		t.Fatalf("fixture must clear the real min_tokens default on its own; got %d tokens", got)
	}
	if frac := float64(len(token)) / float64(len(body)); frac < 0.6 {
		t.Fatalf("fixture must clear the real min_blob_frac default on its own; got %.3f", frac)
	}

	req := tailReq(body)
	var rep components.Report
	if _, err := b.Offload(req, &rep, coldGateCtx(store.NewMemory(store.Options{}), false)); err != nil {
		t.Fatal(err)
	}
	got := schema.MessageText(req.Input[1])
	if got != body {
		t.Fatalf("a bare token field must never be masked, regardless of size, got %q", got)
	}
	if rep.Gates["no_embedded_blob"] == 0 {
		t.Fatalf("expected the structural no_embedded_blob gate specifically, got %v", rep.Gates)
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
