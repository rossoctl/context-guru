package proxy_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/proxy"
	"github.com/tidwall/gjson"
)

// The ceilings are lowered for these tests (tokenizing a 33 MiB body took over 100 s under
// -race and the tests only need a body over the ceiling, not a body of a particular size).
// The shipped values are pinned by TestShippedRequestCeilings, so a change to either must not
// pass by construction.
const (
	testMaxRequestBytes           = 1 << 20
	testMaxCompactionRequestBytes = 4 << 20
)

// useSmallCeilings lowers the two ceilings for one test and restores them after. These tests
// are serial, so the swap is not observed by a running handler of another test.
func useSmallCeilings(t *testing.T) {
	t.Helper()
	prevReq, prevCompact := proxy.SetRequestCeilings(testMaxRequestBytes, testMaxCompactionRequestBytes)
	t.Cleanup(func() { proxy.SetRequestCeilings(prevReq, prevCompact) })
}

func TestShippedRequestCeilings(t *testing.T) {
	req, compact := proxy.SetRequestCeilings(0, 0)
	proxy.SetRequestCeilings(req, compact)
	if req != 32<<20 || compact != 128<<20 {
		t.Fatalf("shipped ceilings = %d / %d, want 32 MiB / 128 MiB", req, compact)
	}
}

// repeatBannerToSize builds the realistic shape that actually triggers #278: a repeated,
// low-entropy line (the cgssh2 SSH MOTD banner, in the real incident) rather than a single
// repeated character. A single repeated byte is a known BPE-tokenizer worst case (pathological
// merge behavior on a run of one character) and made an earlier version of this test hang for
// minutes on a size a real transcript reaches routinely — using realistic filler avoids that
// AND matches the actual bug shape more closely.
const bannerLine = "Authorized uses only. All activity may be monitored and reported.\n"

func repeatBannerToSize(atLeast int) string {
	return strings.Repeat(bannerLine, atLeast/len(bannerLine)+1)
}

// oversizedBody builds a syntactically valid chat request whose total size is just over
// `over` bytes past testMaxRequestBytes, with an ordinary last message (no compaction
// phrase) unless lastMsg is supplied.
func oversizedBody(t *testing.T, over int, lastMsg map[string]any) []byte {
	t.Helper()
	if lastMsg == nil {
		lastMsg = map[string]any{"role": "user", "content": "an ordinary turn"}
	}
	filler := repeatBannerToSize(testMaxRequestBytes + over)
	body, err := json.Marshal(map[string]any{
		"model": "gpt-x",
		"messages": []map[string]any{
			{"role": "user", "content": filler},
			lastMsg,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// TestOversizedNonCompactionRequestGets413: today this returned a plain 400, identical to
// a malformed body — the user-visible symptom in rossoctl/context-guru#278 (Claude Code
// rendered it as "Request too large (max 32MB)" with no distinguishable status from the
// proxy at all). An ordinary oversized request must still be refused, but honestly.
func TestOversizedNonCompactionRequestGets413(t *testing.T) {
	useSmallCeilings(t)
	var upstreamHit bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	h, _ := buildHandler(t, "pipeline: [dedup]\n", upstream.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	body := oversizedBody(t, 1<<20, nil) // 1 MiB over the default ceiling
	resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d (413) — got %s", resp.StatusCode, http.StatusRequestEntityTooLarge,
			mustReadAll(t, resp.Body))
	}
	if upstreamHit {
		t.Fatal("an oversized, non-compaction request must never reach the upstream")
	}
}

// TestOversizedAgentCompactionRequestSucceeds is the fix for #278 itself: the agent's own
// compaction request — the one whose whole purpose is carrying a transcript already larger
// than testMaxRequestBytes — must reach the existing, already-correct bypass path instead
// of dying at the read. Forwarded byte-identical, exactly as a small compaction request
// already is (see agentcompaction_test.go's TestAgentCompactionIsBypassed) — this test
// only proves the SAME thing still holds once the body is oversized.
func TestOversizedAgentCompactionRequestSucceeds(t *testing.T) {
	useSmallCeilings(t)
	var up upstreamCapture
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.record(r)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	h, _ := buildHandler(t, "pipeline: [dedup]\n", upstream.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	compactionMsg := map[string]any{"role": "user", "content": ccCompactPromptForSizeTest}
	body := oversizedBody(t, 1<<20, compactionMsg) // 1 MiB over the default ceiling
	resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — got %s", resp.StatusCode, mustReadAll(t, resp.Body))
	}
	got := up.last().body
	if !bytes.Equal(got, body) {
		t.Fatalf("an oversized compaction request must reach the upstream byte-identical "+
			"(len sent=%d, len got=%d)", len(body), len(got))
	}
}

// TestOverHardCeilingIsRefusedEvenIfCompaction: the compaction exemption raises the
// ceiling, it does not remove it. A body over testMaxCompactionRequestBytes is refused
// regardless of content — memory is still bounded, just at the higher tier.
func TestOverHardCeilingIsRefusedEvenIfCompaction(t *testing.T) {
	useSmallCeilings(t)
	var upstreamHit bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	h, _ := buildHandler(t, "pipeline: [dedup]\n", upstream.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	compactionMsg := map[string]any{"role": "user", "content": ccCompactPromptForSizeTest}
	over := (testMaxCompactionRequestBytes - testMaxRequestBytes) + (1 << 20) // 1 MiB past the hard ceiling
	body := oversizedBody(t, over, compactionMsg)
	resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d (413) — got %s", resp.StatusCode, http.StatusRequestEntityTooLarge,
			mustReadAll(t, resp.Body))
	}
	if upstreamHit {
		t.Fatal("a request over the hard ceiling must never reach the upstream, compaction or not")
	}
}

// TestCompactEndpointAcceptsOversizedBody: /compact's whole job is to receive and shrink a
// body that is, by definition, already large — it gets the unconditional override, with no
// need for the agent-compaction phrase at all. Unlike chat, the pipeline DOES run here (see
// proxy.go's own doc comment on compact()), so this also proves the oversized body actually
// reaches and is shrunk by the pipeline, not merely accepted.
func TestCompactEndpointAcceptsOversizedBody(t *testing.T) {
	useSmallCeilings(t)
	h, _ := buildHandler(t, "pipeline: [dedup]\ncomponents:\n  dedup: {min_tokens: 20}\n", "http://unused.invalid")
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	dup := strings.Repeat("a duplicated tool output line\n", 200)
	filler := repeatBannerToSize(testMaxRequestBytes + (1 << 20))
	body, err := json.Marshal(map[string]any{
		"model": "gpt-x",
		"messages": []map[string]any{
			{"role": "user", "content": filler},
			{"role": "tool", "tool_call_id": "a", "content": dup},
			{"role": "tool", "tool_call_id": "b", "content": dup},
			{"role": "user", "content": "carry on"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(srv.URL+"/compact", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := mustReadAll(t, resp.Body)
	second := gjson.GetBytes([]byte(got), "messages.2.content").String()
	if !strings.Contains(second, "repeat of") {
		t.Fatalf("dedup did not run on the oversized body: %s", truncate(second, 200))
	}
}

// TestOversizedToolResultQuotingCompactionPhraseStillGetsThroughAtTheHigherCeiling pins a
// KNOWN, DOCUMENTED tradeoff rather than leaving it a surprise: isAgentCompaction's own doc
// comment (agentcompaction.go) already accepts that a trailing tool_result quoting the
// compaction phrase verbatim is a reachable false positive — an agent that reads
// docs/how-to/agent-compaction.md lands the phrase in its own last message. Before this
// fix, that cost a skipped pipeline pass on one request. After it, it additionally unlocks
// reading up to testMaxCompactionRequestBytes instead of testMaxRequestBytes for that one
// request — still bounded, not unlimited, but a real, larger blast radius worth a named
// test rather than an implicit one.
func TestOversizedToolResultQuotingCompactionPhraseStillGetsThroughAtTheHigherCeiling(t *testing.T) {
	useSmallCeilings(t)
	var up upstreamCapture
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.record(r)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	h, _ := buildHandler(t, "pipeline: [dedup]\n", upstream.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	// The false-positive shape: role "user" (a tool_result IS user-role in the Anthropic
	// dialect), quoting the phrase, as the LAST message.
	falsePositiveMsg := map[string]any{"role": "user", "content": ccCompactPromptForSizeTest}
	body := oversizedBody(t, 1<<20, falsePositiveMsg)
	resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (documenting the known false-positive letting this "+
			"through) — got %s", resp.StatusCode, mustReadAll(t, resp.Body))
	}
	if got := up.last().body; !bytes.Equal(got, body) {
		t.Fatalf("expected the false positive to forward byte-identical, same as a real " +
			"compaction request would")
	}
}

func mustReadAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ccCompactPromptForSizeTest is the same verbatim phrase agentcompaction_test.go uses
// (ccCompactPrompt), duplicated here rather than imported across test files so this file
// reads standalone and a rename in one does not silently break the other's coverage.
const ccCompactPromptForSizeTest = "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.\n\n" +
	"- Tool calls will be REJECTED and will waste your only turn — you will fail the task.\n\n" +
	"Your task is to create a detailed summary of the conversation so far, paying close " +
	"attention to the user's explicit requests and your previous actions.\n" +
	"3. Files and Code Sections: ... include full code snippets where applicable"

// TestRefusalIsParseableByTheAgent pins the SHAPE of the 413, not just its status.
//
// This is the second half of rossoctl/context-guru#278, and the half that kept the symptom
// alive after the ceiling itself was fixed. Claude Code (2.1.x) decides what to tell the
// user by parsing the refusal body for `error.type == "request_too_large"`; only then does it
// classify the transcript it tried to send and say whether /compact, removing attachments, or
// starting fresh is the way out. `http.Error`'s plain-text "request too large" does not parse,
// so a proxy refusal reached the user as an opaque failure quoting a 32MB limit the deployment
// had never applied, with advice ("remove accumulated images and attachments") aimed at media
// the session did not contain.
//
// Asserting on the parsed field rather than the whole string deliberately: the prose may be
// reworded, but `error.type` is a wire contract with the client and changing it silently
// re-breaks the diagnosis.
func TestRefusalIsParseableByTheAgent(t *testing.T) {
	useSmallCeilings(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("an oversized, non-compaction request must never reach the upstream")
	}))
	defer upstream.Close()

	h, _ := buildHandler(t, "pipeline: [dedup]\n", upstream.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json",
		bytes.NewReader(oversizedBody(t, 1<<20, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json — a body the client cannot "+
			"identify as JSON is not parsed at all", ct)
	}
	raw := mustReadAll(t, resp.Body)
	if !json.Valid([]byte(raw)) {
		t.Fatalf("refusal body is not valid JSON: %q", raw)
	}
	if got := gjson.Get(raw, "error.type").String(); got != "request_too_large" {
		t.Errorf("error.type = %q, want %q — this exact value is what the agent matches "+
			"to tell the user how to recover; body was %s", got, "request_too_large", raw)
	}
	if got := gjson.Get(raw, "error.limit_bytes").Int(); got != testMaxRequestBytes {
		t.Errorf("error.limit_bytes = %d, want %d (the tier that actually refused)",
			got, testMaxRequestBytes)
	}
}

// TestRefusalOverHardCeilingNamesTheHigherTier: a body over the 128 MiB hard ceiling is
// refused whatever it is, and must say so with the ceiling that actually applied. Reporting
// the 32 MiB tier here would tell a caller its compaction request was refused by a limit it
// was in fact allowed to exceed.
func TestRefusalOverHardCeilingNamesTheHigherTier(t *testing.T) {
	useSmallCeilings(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a body over the hard ceiling must never reach the upstream")
	}))
	defer upstream.Close()

	h, _ := buildHandler(t, "pipeline: [dedup]\n", upstream.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	// Over the HARD ceiling and a genuine compaction request: refused on size alone.
	filler := repeatBannerToSize(testMaxCompactionRequestBytes + (1 << 20))
	body, err := json.Marshal(map[string]any{
		"model": "gpt-x",
		"messages": []map[string]any{
			{"role": "user", "content": filler},
			{"role": "user", "content": "Your task is to create a detailed summary of the conversation"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	raw := mustReadAll(t, resp.Body)
	if got := gjson.Get(raw, "error.type").String(); got != "request_too_large" {
		t.Errorf("error.type = %q, want request_too_large; body %s", got, raw)
	}
	if got := gjson.Get(raw, "error.limit_bytes").Int(); got != testMaxCompactionRequestBytes {
		t.Errorf("error.limit_bytes = %d, want %d (the hard ceiling, not the ordinary tier)",
			got, testMaxCompactionRequestBytes)
	}
}
