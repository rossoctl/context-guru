package classifier

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/routing"
)

// req builds a request body. Each option changes one part of the classifier shape.
func req(mut func(m map[string]any)) []byte {
	m := map[string]any{
		"model":          "claude-sonnet-5-5",
		"max_tokens":     64,
		"stream":         false,
		"stop_sequences": []string{"</block>"},
		"system": []map[string]any{
			{"type": "text", "text": "s1"}, {"type": "text", "text": "s2"}, {"type": "text", "text": "s3"}},
		"messages": []map[string]any{
			{"role": "user", "content": []map[string]any{{"type": "text", "text": "ctx"}}}},
	}
	if mut != nil {
		mut(m)
	}
	b, _ := json.Marshal(m)
	return b
}

func TestDetectShape(t *testing.T) {
	tools := []map[string]any{{"name": "x"}}
	cases := []struct {
		name string
		mut  func(map[string]any)
		want bool
	}{
		{"exact shape", nil, true},
		{"sonnet other id", func(m map[string]any) { m["model"] = "aws/claude-sonnet-4-6" }, true},
		{"opus", func(m map[string]any) { m["model"] = "claude-opus-5-5" }, false},
		{"tools > 0", func(m map[string]any) { m["tools"] = tools }, false},
		{"stream true", func(m map[string]any) { m["stream"] = true }, false},
		{"max_tokens 65", func(m map[string]any) { m["max_tokens"] = 65 }, false},
		{"max_tokens 63", func(m map[string]any) { m["max_tokens"] = 63 }, false},
		{"opus 2112 shape", func(m map[string]any) { m["model"] = "claude-opus-5-5"; m["max_tokens"] = 2112 }, false},
		{"no stop_sequences", func(m map[string]any) { delete(m, "stop_sequences") }, false},
		{"empty stop_sequences", func(m map[string]any) { m["stop_sequences"] = []string{} }, false},
		{"2 messages", func(m map[string]any) {
			m["messages"] = []map[string]any{{"role": "user", "content": "a"}, {"role": "user", "content": "b"}}
		}, false},
		{"3 messages", func(m map[string]any) {
			m["messages"] = []map[string]any{{"role": "user", "content": "a"}, {"role": "assistant", "content": "b"}, {"role": "user", "content": "c"}}
		}, false},
		{"2 system blocks", func(m map[string]any) {
			m["system"] = []map[string]any{{"type": "text", "text": "a"}, {"type": "text", "text": "b"}}
		}, false},
		{"4 system blocks", func(m map[string]any) {
			m["system"] = []map[string]any{{"type": "text", "text": "a"}, {"type": "text", "text": "b"}, {"type": "text", "text": "c"}, {"type": "text", "text": "d"}}
		}, false},
		{"string system", func(m map[string]any) { m["system"] = "one" }, false},
	}
	for _, c := range cases {
		if got := Detect(http.Header{}, req(c.mut)); got != c.want {
			t.Errorf("%s: Detect=%v want %v (shape %+v)", c.name, got, c.want, ShapeOf(req(c.mut)))
		}
	}
}

func TestDetectHeader(t *testing.T) {
	h := http.Header{}
	h.Set(RequestClassHeader, "Classifier")
	// A header match does not need the shape...
	if !Detect(h, req(func(m map[string]any) { m["max_tokens"] = 200 })) {
		t.Error("header match must qualify")
	}
	// ...but a streaming request is never redirected: the answer is one JSON body.
	if Detect(h, req(func(m map[string]any) { m["stream"] = true })) {
		t.Error("stream must not be redirected")
	}
	// "auxiliary" alone is not a classifier.
	h.Set(RequestClassHeader, "auxiliary")
	if Detect(h, req(func(m map[string]any) { m["max_tokens"] = 200 })) {
		t.Error("auxiliary must not match by header")
	}
}

func TestDetectReadsNoText(t *testing.T) {
	s := ShapeOf(req(nil))
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), "ctx") || strings.Contains(string(b), "action?") {
		t.Fatalf("shape carries text: %s", b)
	}
}

// fake is a Dispatcher with a scripted sequence of answers.
type fake struct {
	calls  atomic.Int32
	script []func() (dispatch.Response, error)
	last   dispatch.Request
}

func (f *fake) Do(ctx context.Context, r dispatch.Request) (dispatch.Response, error) {
	i := int(f.calls.Add(1)) - 1
	f.last = r
	if i >= len(f.script) {
		i = len(f.script) - 1
	}
	return f.script[i]()
}
func (f *fake) Release(dispatch.Ref) {}
func (f *fake) Price(context.Context, string, string, int64) (dispatch.Price, bool) {
	return dispatch.Price{}, false
}

func ok(body string) func() (dispatch.Response, error) {
	return func() (dispatch.Response, error) { return dispatch.Response{Status: 200, Body: []byte(body)}, nil }
}
func status(c int) func() (dispatch.Response, error) {
	return func() (dispatch.Response, error) { return dispatch.Response{Status: c, Body: []byte(`{}`)}, nil }
}

var bobRoute = routing.Route{Feature: Feature, Model: "bob-fast", Ref: "r9", Dialect: "bob",
	Path: "/inference/v1/chat/completions"}

const answer = `{"choices":[{"message":{"content":"<block>no</block>"},"finish_reason":"stop"}],
"usage":{"prompt_tokens":1000,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":900}}}`

var fast = Options{Backoff: -1}

func TestRedirectConvertsBothWays(t *testing.T) {
	f := &fake{script: []func() (dispatch.Response, error){ok(answer)}}
	res, err := Redirect(context.Background(), f, bobRoute, req(nil), fast)
	if err != nil {
		t.Fatal(err)
	}
	// Outbound: OpenAI shape, one system message, text blocks flattened, routed model.
	sent := f.last
	if sent.Path != bobRoute.Path || sent.Ref != "r9" || sent.Purpose != dispatch.PurposeRouted {
		t.Fatalf("request=%+v", sent)
	}
	b := sent.Body
	if gjson.GetBytes(b, "model").String() != "bob-fast" || gjson.GetBytes(b, "messages.#").Int() != 2 ||
		gjson.GetBytes(b, "messages.0.role").String() != "system" ||
		gjson.GetBytes(b, "messages.0.content").String() != "s1\n\ns2\n\ns3" ||
		gjson.GetBytes(b, "messages.1.content").String() != "ctx" ||
		gjson.GetBytes(b, "max_tokens").Int() != 64 || gjson.GetBytes(b, "stop").Exists() {
		t.Fatalf("body=%s", b)
	}
	// Inbound: Anthropic shape, cut at the stop sequence, usage tiers split.
	r := res.Body
	if gjson.GetBytes(r, "type").String() != "message" ||
		gjson.GetBytes(r, "content.0.text").String() != "<block>no" ||
		gjson.GetBytes(r, "stop_reason").String() != "stop_sequence" ||
		gjson.GetBytes(r, "stop_sequence").String() != "</block>" ||
		gjson.GetBytes(r, "usage.input_tokens").Int() != 100 ||
		gjson.GetBytes(r, "usage.cache_read_input_tokens").Int() != 900 ||
		gjson.GetBytes(r, "usage.output_tokens").Int() != 9 {
		t.Fatalf("response=%s", r)
	}
	if res.Usage != (Usage{Fresh: 100, CacheRead: 900, Output: 9}) || res.Attempts != 1 {
		t.Fatalf("usage=%+v attempts=%d", res.Usage, res.Attempts)
	}
}

func TestEndTurnWithoutStopSequence(t *testing.T) {
	f := &fake{script: []func() (dispatch.Response, error){
		ok(`{"choices":[{"message":{"content":"allow"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)}}
	res, err := Redirect(context.Background(), f, bobRoute, req(nil), fast)
	if err != nil || gjson.GetBytes(res.Body, "stop_reason").String() != "end_turn" ||
		gjson.GetBytes(res.Body, "stop_sequence").Type != gjson.Null {
		t.Fatalf("err=%v body=%s", err, res.Body)
	}
}

func TestVerdictIsNeverAFailure(t *testing.T) {
	for _, verdict := range []string{"block", "allow", "<block>yes</block>"} {
		raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": verdict}}}})
		f := &fake{script: []func() (dispatch.Response, error){ok(string(raw))}}
		if _, err := Redirect(context.Background(), f, bobRoute, req(nil), fast); err != nil || f.calls.Load() != 1 {
			t.Errorf("%q: err=%v calls=%d", verdict, err, f.calls.Load())
		}
	}
}

func TestInaccessibleThreeTimesThenFallback(t *testing.T) {
	bad := map[string]func() (dispatch.Response, error){
		"transport": func() (dispatch.Response, error) { return dispatch.Response{}, errors.New("dial") },
		"500":       status(500), "503": status(503), "401": status(401), "403": status(403),
		"404": status(404), "429": status(429),
		"empty text":    ok(`{"choices":[{"message":{"content":""}}]}`),
		"no choices":    ok(`{"choices":[]}`),
		"not json":      ok(`<html>`),
		"empty body":    ok(``),
		"whitespace":    ok(`{"choices":[{"message":{"content":"  \n"}}]}`),
		"unparseable":   ok(`{"choices":`),
		"not held ref":  func() (dispatch.Response, error) { return dispatch.Response{}, dispatch.ErrNotHeld },
		"only 2xx rest": status(204),
	}
	for name, step := range bad {
		f := &fake{script: []func() (dispatch.Response, error){step}}
		_, err := Redirect(context.Background(), f, bobRoute, req(nil), fast)
		if !errors.Is(err, ErrInaccessible) || f.calls.Load() != 3 {
			t.Errorf("%s: err=%v calls=%d, want ErrInaccessible after 3", name, err, f.calls.Load())
		}
	}
}

func TestRecoversOnSecondAttempt(t *testing.T) {
	f := &fake{script: []func() (dispatch.Response, error){status(503), ok(answer)}}
	res, err := Redirect(context.Background(), f, bobRoute, req(nil), fast)
	if err != nil || res.Attempts != 2 {
		t.Fatalf("err=%v attempts=%d", err, res.Attempts)
	}
}

func TestTotalBudgetStopsRetries(t *testing.T) {
	slow := func() (dispatch.Response, error) {
		time.Sleep(60 * time.Millisecond)
		return dispatch.Response{Status: 503}, nil
	}
	f := &fake{script: []func() (dispatch.Response, error){slow}}
	start := time.Now()
	_, err := Redirect(context.Background(), f, bobRoute, req(nil),
		Options{Budget: 100 * time.Millisecond, Backoff: -1, Attempts: 10})
	if !errors.Is(err, ErrInaccessible) {
		t.Fatalf("err=%v", err)
	}
	if time.Since(start) > 500*time.Millisecond || f.calls.Load() >= 10 {
		t.Fatalf("budget not enforced: %v, %d calls", time.Since(start), f.calls.Load())
	}
}

// A blocked Do must be cut by the budget, not wait for the transport.
func TestBudgetCancelsAHungCall(t *testing.T) {
	hang := func() (dispatch.Response, error) { time.Sleep(2 * time.Second); return dispatch.Response{}, nil }
	f := &hangDisp{}
	_ = hang
	start := time.Now()
	_, err := Redirect(context.Background(), f, bobRoute, req(nil), Options{Budget: 80 * time.Millisecond, Backoff: -1})
	if !errors.Is(err, ErrInaccessible) || time.Since(start) > time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}

type hangDisp struct{ fake }

func (h *hangDisp) Do(ctx context.Context, _ dispatch.Request) (dispatch.Response, error) {
	<-ctx.Done()
	return dispatch.Response{}, ctx.Err()
}

func TestFailOpen(t *testing.T) {
	// Non-text block: cannot be carried, so no call is made and the host falls back.
	f := &fake{script: []func() (dispatch.Response, error){ok(answer)}}
	body := req(func(m map[string]any) {
		m["messages"] = []map[string]any{{"role": "user", "content": []map[string]any{{"type": "image"}}},
			{"role": "user", "content": "x"}}
	})
	if _, err := Redirect(context.Background(), f, bobRoute, body, fast); err == nil || f.calls.Load() != 0 {
		t.Fatalf("err=%v calls=%d", err, f.calls.Load())
	}
	// A nil dispatcher and a panicking dispatcher both end as an error, never a panic.
	if _, err := Redirect(context.Background(), nil, bobRoute, req(nil), fast); err == nil {
		t.Fatal("nil dispatcher")
	}
	if _, err := Redirect(context.Background(), &panicDisp{}, bobRoute, req(nil), fast); err == nil {
		t.Fatal("panic must become an error")
	}
}

type panicDisp struct{ fake }

func (*panicDisp) Do(context.Context, dispatch.Request) (dispatch.Response, error) { panic("boom") }

func TestAnthropicDialectKeepsShape(t *testing.T) {
	route := routing.Route{Feature: Feature, Model: "claude-haiku-5-5", Ref: "r2", Dialect: "anthropic", Path: "/v1/messages"}
	f := &fake{script: []func() (dispatch.Response, error){ok(
		`{"content":[{"type":"text","text":"yes"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":2}}`)}}
	res, err := Redirect(context.Background(), f, route, req(nil), fast)
	if err != nil || gjson.GetBytes(f.last.Body, "model").String() != "claude-haiku-5-5" ||
		gjson.GetBytes(f.last.Body, "stop_sequences.0").String() != "</block>" ||
		gjson.GetBytes(res.Body, "content.0.text").String() != "yes" || res.Usage.Fresh != 7 {
		t.Fatalf("err=%v sent=%s got=%s", err, f.last.Body, res.Body)
	}
}

// TestDetectsTheCapturedWireShape pins the shape of a classifier request captured on the wire
// from Claude Code 2.1.305 (field names and types only, no text): 1 user message of 5 text
// blocks, 3 system blocks with cache_control on the second, thinking disabled, metadata.user_id,
// no stream field at all. A detector written from the dashboard's columns expected 2 messages
// and matched nothing real.
func TestDetectsTheCapturedWireShape(t *testing.T) {
	text := func() map[string]any { return map[string]any{"type": "text", "text": "x"} }
	b, _ := json.Marshal(map[string]any{
		"model":      "claude-sonnet-5",
		"max_tokens": 64,
		"system": []map[string]any{
			text(),
			{"type": "text", "text": "x", "cache_control": map[string]any{"type": "ephemeral"}},
			text()},
		"messages": []map[string]any{
			{"role": "user", "content": []map[string]any{text(), text(), text(), text(), text()}}},
		"stop_sequences": []string{"</block>"},
		"thinking":       map[string]any{"type": "disabled"},
		"metadata":       map[string]any{"user_id": "u"},
	})
	if !Detect(http.Header{}, b) {
		t.Fatalf("captured wire shape not detected: %+v", ShapeOf(b))
	}
}
