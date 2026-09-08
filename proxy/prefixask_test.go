package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/adjudicate"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/internal/extract"
)

// The prefix ask exists for ONE reason: to read the provider's prompt cache instead of paying fresh
// for the transcript. Every test here is about the construction that makes the read possible, because
// a construction that quietly does not read looks identical to one that does — except on the bill.

// capturePrefixed stands in for the provider and records the body it was sent, so a test can assert
// what actually reached the wire rather than what the caller intended.
type capturePrefixed struct {
	mu   sync.Mutex
	body []byte
	srv  *httptest.Server
}

// forwarded is what the fixture last received. The handler runs on the test server's own
// goroutine and the HTTP round trip is not a happens-before edge, so the capture goes
// through c.mu -- the shape tenancy_test.go's hostedFixture uses.
func (c *capturePrefixed) forwarded() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.body
}

func newCapturePrefixed(t *testing.T, cacheRead int) *capturePrefixed {
	t.Helper()
	c := &capturePrefixed{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		c.mu.Lock()
		c.body = b
		c.mu.Unlock()
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"text":"[]"}],"usage":{"input_tokens":12,` +
			`"output_tokens":3,"cache_creation_input_tokens":0,"cache_read_input_tokens":` +
			itoa(cacheRead) + `}}`))
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}

// The prefix body a real turn would have forwarded: a transcript, the agent's tools, and the agent's
// own cache_control marks.
const prefixBodyFixture = `{"model":"claude-sonnet-5","stream":true,` +
	`"system":[{"type":"text","text":"you are a coding agent","cache_control":{"type":"ephemeral"}}],` +
	`"tools":[{"name":"Bash","description":"run a command"}],` +
	`"messages":[{"role":"user","content":"find the flaky test"},` +
	`{"role":"assistant","content":"I will run the suite."}]}`

// THE APPENDED MESSAGE IS THE ONLY EDIT INSIDE THE MESSAGE LIST, and everything before it is
// untouched. Every byte before the appended message is prefix; any edit to it costs the cache read
// this whole mechanism exists for.
func TestCompletePrefixedAppendsWithoutDisturbingThePrefix(t *testing.T) {
	srv := newCapturePrefixed(t, 19595)
	cli := cheapmodel.Anthropic{BaseURL: srv.srv.URL, Model: "claude-sonnet-5", APIKey: "k"}
	reply, u, err := cli.CompletePrefixed(context.Background(), []byte(prefixBodyFixture), "ASK-TEXT")
	if err != nil {
		t.Fatalf("CompletePrefixed: %v", err)
	}
	// PRECONDITION: the call reached the stub at all, or every assertion below is about nothing.
	if len(srv.forwarded()) == 0 {
		t.Fatal("no body reached the provider")
	}
	if reply != "[]" {
		t.Errorf("reply = %q, want the model's text", reply)
	}
	if u.CacheRead != 19595 {
		t.Errorf("CacheRead = %d, want the provider's figure — the caller gates on it", u.CacheRead)
	}

	var sent struct {
		Tools      []map[string]any `json:"tools"`
		System     []map[string]any `json:"system"`
		ToolChoice map[string]any   `json:"tool_choice"`
		Stream     *bool            `json:"stream"`
		Messages   []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(srv.forwarded(), &sent); err != nil {
		t.Fatalf("the body sent is not JSON: %v\n%s", err, srv.forwarded())
	}
	// The ask is the LAST message and a USER one. The route rejects assistant prefill, which this
	// satisfies by construction — and it is why the prefix must not be extended any other way.
	if n := len(sent.Messages); n != 3 {
		t.Fatalf("expected 3 messages (2 prefix + the ask), got %d", n)
	}
	last := sent.Messages[2]
	if last.Role != "user" {
		t.Errorf("the ask was appended as role %q; this route rejects assistant prefill", last.Role)
	}
	if last.Content != "ASK-TEXT" {
		t.Errorf("the ask text did not reach the wire: %v", last.Content)
	}
	// The two prefix messages are byte-for-byte what they were.
	if sent.Messages[0].Content != "find the flaky test" || sent.Messages[1].Content != "I will run the suite." {
		t.Error("a prefix message was altered, which costs the cache read")
	}
	// TOOLS ARE PART OF THE CACHE KEY. Dropping them read a different, smaller entry (19,129) —
	// a separate cache line and a fresh write.
	if len(sent.Tools) != 1 || sent.Tools[0]["name"] != "Bash" {
		t.Errorf("tools were not preserved exactly; they are part of the cache key: %v", sent.Tools)
	}
	// The agent's own cache_control marks are what make the transcript cacheable at all. We place
	// none of our own here, so these must survive untouched.
	if len(sent.System) != 1 || sent.System[0]["cache_control"] == nil {
		t.Errorf("the prefix's cache_control was lost, so there is no entry to read: %v", sent.System)
	}
	// NO TOOL_CHOICE AT ALL, and this reverses what this test used to assert. Two separate findings on
	// the same prefix with only tool_choice varying. On the CACHE: {"type":"tool",name} MISSED the entry
	// and wrote 8,378 tokens against the 8,268 already there, so naming a tool DOES participate in the
	// key, while omitting tool_choice read that entry for free. On the ANSWER: with the tool declared
	// and no tool_choice, unparseable replies ran 9.1% (7 of 77 replied asks) against 30.0% (6 of 20)
	// under main's {"type":"none"}, Fisher two-tailed p = 0.0245 — setting `none` to PREVENT a tool_use
	// is what drove the model into prose, which the caller then scored as an unparseable failure.
	// The "0 of 6 / 6 of 6" verdict counts this comment used to cite came from a six-item hand pass and
	// are retracted; main returns verdicts on 71.5% of the items it asks about, not none of them.
	if sent.ToolChoice != nil {
		t.Errorf("tool_choice was set (%v); `none` drives the model into prose and a named tool costs "+
			"a fresh cache write", sent.ToolChoice)
	}
	// stream is the one deliberate removal: the caller wants a single JSON answer.
	if sent.Stream != nil {
		t.Error("stream survived, so the reply would arrive as SSE rather than one JSON answer")
	}
}

// THE TOOL INPUT IS PREFERRED OVER TEXT. With the tool declared in the prefix the model answers by
// calling it, and that input is already schema-shaped — which is the whole point: it removes prose,
// partial batches, and an array truncated by the output budget. The raw input is returned so the
// caller's existing parser reads it unchanged, and a `Read` call sitting alongside must NOT be
// mistaken for the answer.
func TestCompletePrefixedPrefersOurToolInputOverText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"content":[` +
			`{"type":"thinking","thinking":"weighing the outputs"},` +
			`{"type":"text","text":"I think output 1 is still needed."},` +
			`{"type":"tool_use","id":"t0","name":"Read","input":{"path":"a.py"}},` +
			`{"type":"tool_use","id":"t1","name":"` + adjudicate.ToolName + `",` +
			`"input":{"verdicts":[{"i":1,"needed_by":"none","verdict":"drop"}]}}],` +
			`"usage":{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":0,` +
			`"cache_read_input_tokens":8268}}`))
	}))
	t.Cleanup(srv.Close)
	cli := cheapmodel.Anthropic{BaseURL: srv.URL, Model: "claude-sonnet-5", APIKey: "k"}
	reply, u, err := cli.CompletePrefixed(context.Background(), []byte(prefixBodyFixture), "ASK")
	if err != nil {
		t.Fatalf("CompletePrefixed: %v", err)
	}
	// REPORTED, not just used. The caller counts which reply shape it got, and it cannot infer this
	// one: extract.ParseVerdicts reads a tool_use input and a JSON array in text identically, so a
	// run whose declared tool is never touched looks exactly like one where it always is. That
	// ambiguity is what left the reviewer's "0 of 5 asks used the tool" and this PR's "6 of 6"
	// un-adjudicable against each other. See components.PrefixUsage.ViaTool.
	if !u.ViaTool {
		t.Error("the answer arrived as a tool_use but was not reported as one")
	}
	if strings.Contains(reply, "I think output 1") {
		t.Errorf("the prose text was returned in preference to the tool input: %q", reply)
	}
	if strings.Contains(reply, "a.py") {
		t.Errorf("the AGENT's own Read call was mistaken for the answer: %q", reply)
	}
	if !strings.Contains(reply, `"verdicts"`) || !strings.Contains(reply, `"drop"`) {
		t.Errorf("the tool input did not reach the caller verbatim: %q", reply)
	}
	// The existing text parser must read that input unchanged — it scans for an array that decodes,
	// and the tool's `verdicts` array is one. This is what keeps the fix additive.
	vs, ok := extract.ParseVerdicts(reply)
	if !ok || len(vs) != 1 || vs[0].Label != 1 || vs[0].Verdict != "drop" {
		t.Errorf("extract.ParseVerdicts could not read the tool input: ok=%v verdicts=%+v", ok, vs)
	}
}

// With no tool_use in the reply the TEXT path must still work: main's parse path and its fallback are
// untouched by this change, and a model that answers in prose anyway is read exactly as before.
func TestCompletePrefixedStillReadsTextWhenNoToolWasCalled(t *testing.T) {
	srv := newCapturePrefixed(t, 8268)
	cli := cheapmodel.Anthropic{BaseURL: srv.srv.URL, Model: "m", APIKey: "k"}
	reply, u, err := cli.CompletePrefixed(context.Background(), []byte(prefixBodyFixture), "ASK")
	// And the shape is reported as PROSE, so the two counters cannot both be inflated by the same
	// reply. A ViaTool that defaulted to true would make every prose answer look like a tool answer,
	// which is the exact confusion the field exists to remove.
	if u.ViaTool {
		t.Error("a text-only reply was reported as having arrived via the tool")
	}
	if err != nil {
		t.Fatalf("CompletePrefixed: %v", err)
	}
	if reply != "[]" {
		t.Errorf("reply = %q; the text path must survive unchanged", reply)
	}
}

// A prefix body with no messages array cannot be appended to, and must fail loudly rather than
// sending something that reads no cache.
func TestCompletePrefixedRefusesABodyWithNoMessages(t *testing.T) {
	srv := newCapturePrefixed(t, 0)
	cli := cheapmodel.Anthropic{BaseURL: srv.srv.URL, Model: "m", APIKey: "k"}
	if _, _, err := cli.CompletePrefixed(context.Background(), []byte(`{"model":"m"}`), "ask"); err == nil {
		t.Fatal("a body with no messages array was accepted")
	}
	if len(srv.forwarded()) != 0 {
		t.Error("a malformed prefix was still sent to the provider")
	}
}

// THE STASH HOLDS WHAT WAS FORWARDED, keyed by the SCOPED session id. A first turn has nothing, and
// that must surface as ErrNoPrefix rather than as a silent empty answer — the caller has to be able
// to tell "there was no prefix" from "the model declined".
func TestAskWithoutAStashedBodyReportsNoPrefix(t *testing.T) {
	h := &Handler{sent: newSentStash()}
	a := prefixAsker{stash: h.sent, cli: cheapmodel.Anthropic{Model: "m"}}
	_, u, err := a.Ask(context.Background(), "never-seen", "ask")
	if err == nil {
		t.Fatal("a missing prefix was not reported as an error")
	}
	if err != components.ErrNoPrefix {
		t.Errorf("err = %v, want components.ErrNoPrefix so a caller can tell it from a transport failure", err)
	}
	if u.CacheRead != 0 {
		t.Errorf("usage was reported for a call that never happened: %+v", u)
	}
}

// And with a body stashed, the ask goes out over exactly those bytes.
func TestAskUsesTheStashedBodyForThatSession(t *testing.T) {
	srv := newCapturePrefixed(t, 4242)
	h := &Handler{sent: newSentStash()}
	h.sent.put("scoped-session-1", []byte(prefixBodyFixture))
	a := prefixAsker{stash: h.sent,
		cli: cheapmodel.Anthropic{BaseURL: srv.srv.URL, Model: "claude-sonnet-5", APIKey: "k"}}
	_, u, err := a.Ask(context.Background(), "scoped-session-1", "THE-ASK")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if u.CacheRead != 4242 {
		t.Fatalf("CacheRead = %d; the caller cannot gate on a figure that does not arrive", u.CacheRead)
	}
	if !strings.Contains(string(srv.forwarded()), "THE-ASK") {
		t.Error("the ask did not reach the wire")
	}
	if !strings.Contains(string(srv.forwarded()), "find the flaky test") {
		t.Error("the stashed transcript did not reach the wire, so there was no prefix to read")
	}
	// A DIFFERENT session must not read this one's prefix: it is another cache namespace, and
	// appending to it would read nothing while looking like a hit.
	if _, _, err := a.Ask(context.Background(), "scoped-session-2", "x"); err != components.ErrNoPrefix {
		t.Errorf("session 2 got session 1's prefix (err=%v)", err)
	}
}

// The bounds are real, and the failure is a forgone opportunity rather than a wrong answer: a body
// over the per-body cap is not stashed at all.
func TestAnOversizedBodyIsNotStashed(t *testing.T) {
	s := newSentStash()
	s.put("sess", []byte(strings.Repeat("x", maxSentBody+1)))
	if got := s.get("sess"); got != nil {
		t.Fatalf("a %d-byte body was stashed past the %d cap", len(got), maxSentBody)
	}
	// Under the cap it is held.
	s.put("sess", []byte(prefixBodyFixture))
	if got := s.get("sess"); len(got) == 0 {
		t.Fatal("a body under the cap was not stashed, so no session could ever ask")
	}
}

// A non-Anthropic route gets no asker: the appended-message construction and the tool_choice/tools
// cache-key facts were measured on that dialect, and guessing another provider's cache semantics is
// how a claimed cache read becomes a silent 10x bill.
func TestPrefixAskerIsAnthropicOnlyAndNeedsTheIncomingClient(t *testing.T) {
	h := &Handler{sent: newSentStash()}
	incoming := components.ModelSpec{Incoming: cheapmodel.Anthropic{Model: "claude-sonnet-5"}}
	if a := h.prefixAskerFor(bschemas.Anthropic, incoming); a == nil {
		t.Fatal("an Anthropic route with an incoming client got no asker")
	}
	if a := h.prefixAskerFor(bschemas.OpenAI, incoming); a != nil {
		t.Error("an OpenAI route got an asker; its cache semantics are not the measured ones")
	}
	// No incoming client means the component would get the STATIC cheap model, which lives in a
	// different cache namespace and could not read this prefix at all.
	if a := h.prefixAskerFor(bschemas.Anthropic, components.ModelSpec{}); a != nil {
		t.Error("an asker was built with no incoming client, so it would read another namespace")
	}
}
