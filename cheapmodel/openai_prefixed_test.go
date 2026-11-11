package cheapmodel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/dispatch/dispatchtest"
)

func TestOpenAIPrefixedResponsesPreservesSentPrefix(t *testing.T) {
	var got []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("wrong prefixed request: path=%s auth=%s", r.URL.Path, r.Header.Get("Authorization"))
		}
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"{\"keep\":[]}"}]}],"usage":{"input_tokens":1200,"input_tokens_details":{"cached_tokens":1100,"cache_write_tokens":0},"output_tokens":20}}`))
	}))
	defer up.Close()
	body := []byte(`{"model":"gpt-5.6","stream":true,"input":[{"role":"user","content":"task"},{"type":"reasoning","encrypted_content":"opaque"}]}`)
	d, ref := dispatchtest.HTTP(up.URL, dispatchtest.BearerAuth("test-key"))
	cli := OpenAI{D: d, Ref: ref, Model: "gpt-5.6", MaxTokens: 100}
	reply, usage, err := cli.CompletePrefixedResponses(context.Background(), body, "judge")
	if err != nil || reply != `{"keep":[]}` || usage.CacheRead != 1100 || usage.Fresh != 100 || usage.Output != 20 {
		t.Fatalf("reply=%q usage=%+v err=%v", reply, usage, err)
	}
	if gjson.GetBytes(got, "input.1.encrypted_content").String() != "opaque" ||
		gjson.GetBytes(got, "input.2.content").String() != "judge" ||
		gjson.GetBytes(got, "stream").Exists() || gjson.GetBytes(got, "max_output_tokens").Int() != 100 {
		t.Fatalf("prefix ask changed the sent history: %s", got)
	}
}

func TestOpenAIPrefixedResponsesReadsAdjudicationTool(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":[{"type":"function_call","name":"context_guru_adjudicate",` +
			`"arguments":"{\"verdicts\":[{\"i\":0,\"verdict\":\"keep\"}]}"}],` +
			`"usage":{"input_tokens":1200,"input_tokens_details":{"cached_tokens":1100},"output_tokens":20}}`))
	}))
	defer up.Close()
	d, ref := dispatchtest.HTTP(up.URL, dispatchtest.BearerAuth("test-key"))
	cli := OpenAI{D: d, Ref: ref, Model: "gpt-5.6"}
	reply, usage, err := cli.CompletePrefixedResponses(context.Background(),
		[]byte(`{"model":"gpt-5.6","input":[{"role":"user","content":"task"}]}`), "judge")
	if err != nil || reply != `{"verdicts":[{"i":0,"verdict":"keep"}]}` || !usage.ViaTool {
		t.Fatalf("reply=%q usage=%+v err=%v", reply, usage, err)
	}
}

// Opt-in live contract test. It seeds a real implicit-cache entry, then uses
// exactly CompletePrefixedResponses to prove the appended ask reads that entry.
// The ordinary unit suite never needs an API key or network access.
func TestOpenAIPrefixedResponsesLiveCache(t *testing.T) {
	base, key, model := os.Getenv("CG_LIVE_OPENAI_URL"), os.Getenv("CG_LIVE_OPENAI_KEY"), os.Getenv("CG_LIVE_OPENAI_MODEL")
	if base == "" || key == "" || model == "" {
		t.Skip("set CG_LIVE_OPENAI_URL, CG_LIVE_OPENAI_KEY and CG_LIVE_OPENAI_MODEL to run")
	}
	var transcript strings.Builder
	for i := 0; i < 1300; i++ {
		fmt.Fprintf(&transcript, "Record %04d: inspect the stable tool-output prefix and retain the original ordering.\n", i)
	}
	body, err := json.Marshal(map[string]any{
		"model": model, "input": []map[string]string{{"role": "user", "content": transcript.String()}},
		"max_output_tokens": 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(base, "/")+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	seed, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Body.Close()
	if seed.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(seed.Body)
		t.Fatalf("seed status %d: %s", seed.StatusCode, clipErr(b))
	}
	var seeded struct {
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(seed.Body).Decode(&seeded); err != nil {
		t.Fatal(err)
	}
	d, ref := dispatchtest.HTTP(base, dispatchtest.BearerAuth(key))
	cli := OpenAI{D: d, Ref: ref, Model: model, MaxTokens: 32}
	var u PrefixUsage
	for attempt := 0; attempt < 3; attempt++ {
		_, u, err = cli.CompletePrefixedResponses(context.Background(), body, "Answer with one word: ready")
		if err != nil {
			t.Fatal(err)
		}
		if u.CacheRead > 0 {
			break
		}
	}
	if seeded.Usage.InputTokens < 1024 || u.CacheRead < 1024 || u.CacheRead < u.Fresh {
		t.Fatalf("prefixed ask did not prove a dominant cache read: seed_input=%d ask_usage=%+v",
			seeded.Usage.InputTokens, u)
	}
	t.Logf("verified live Responses cache read: seed_input=%d cached=%d fresh=%d write=%d output=%d",
		seeded.Usage.InputTokens, u.CacheRead, u.Fresh, u.CacheWrite, u.Output)
}

func chatServer(t *testing.T, status int, reply string, got *[]byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		*got, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
}

func TestCompletePrefixedChatAppendsOneMessageAndKeepsTheRest(t *testing.T) {
	var got []byte
	up := chatServer(t, 200, `{"choices":[{"message":{"content":"{\"verdicts\":[]}"}}],`+
		`"usage":{"prompt_tokens":1200,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":1100}}}`, &got)
	defer up.Close()
	body := []byte(`{"model":"m","stream":true,"stream_options":{"include_usage":true},"max_completion_tokens":50,` +
		`"tool_choice":"auto","tools":[{"type":"function","function":{"name":"Read"}}],` +
		`"messages":[{"role":"system","content":"S"},{"role":"user","content":"task"}]}`)
	d, ref := dispatchtest.HTTP(up.URL, dispatchtest.BearerAuth("k"))
	reply, u, err := OpenAI{D: d, Ref: ref, Model: "m", MaxTokens: 100}.CompletePrefixedChat(context.Background(), body, "judge")
	if err != nil || reply != `{"verdicts":[]}` || u.CacheRead != 1100 || u.Fresh != 100 || u.Output != 20 || u.ViaTool {
		t.Fatalf("reply=%q usage=%+v err=%v", reply, u, err)
	}
	if gjson.GetBytes(got, "messages.#").Int() != 3 || gjson.GetBytes(got, "messages.2.content").String() != "judge" ||
		gjson.GetBytes(got, "messages.0.content").String() != "S" || gjson.GetBytes(got, "messages.1.content").String() != "task" ||
		gjson.GetBytes(got, "stream").Exists() || gjson.GetBytes(got, "stream_options").Exists() ||
		gjson.GetBytes(got, "max_completion_tokens").Int() != 100 || gjson.GetBytes(got, "max_tokens").Exists() ||
		gjson.GetBytes(got, "tool_choice").String() != "auto" || gjson.GetBytes(got, "tools.0.function.name").String() != "Read" {
		t.Fatalf("prefix changed: %s", got)
	}
}

func TestCompletePrefixedChatDefaultsToMaxCompletionTokens(t *testing.T) {
	var got []byte
	up := chatServer(t, 200, `{"choices":[{"message":{"content":"x"}}]}`, &got)
	defer up.Close()
	d, ref := dispatchtest.HTTP(up.URL, dispatchtest.BearerAuth("k"))
	_, _, err := OpenAI{D: d, Ref: ref, Model: "m", MaxTokens: 77}.CompletePrefixedChat(context.Background(),
		[]byte(`{"model":"m","messages":[{"role":"user","content":"t"}]}`), "q")
	if err != nil || gjson.GetBytes(got, "max_completion_tokens").Int() != 77 || gjson.GetBytes(got, "max_tokens").Exists() {
		t.Fatalf("a client with neither field must get max_completion_tokens: err=%v body=%s", err, got)
	}
}

func TestCompletePrefixedChatKeepsTheClientsMaxTokens(t *testing.T) {
	var got []byte
	up := chatServer(t, 200, `{"choices":[{"message":{"content":"x"}}]}`, &got)
	defer up.Close()
	d, ref := dispatchtest.HTTP(up.URL, dispatchtest.BearerAuth("k"))
	_, _, err := OpenAI{D: d, Ref: ref, Model: "m", MaxTokens: 77}.CompletePrefixedChat(context.Background(),
		[]byte(`{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"t"}]}`), "q")
	if err != nil || gjson.GetBytes(got, "max_tokens").Int() != 77 || gjson.GetBytes(got, "max_completion_tokens").Exists() {
		t.Fatalf("err=%v body=%s", err, got)
	}
}

func TestCompletePrefixedChatJoinsContentParts(t *testing.T) {
	var got []byte
	up := chatServer(t, 200, `{"choices":[{"message":{"content":[{"type":"text","text":"ab"},{"type":"text","text":"cd"}]}}]}`, &got)
	defer up.Close()
	d, ref := dispatchtest.HTTP(up.URL, dispatchtest.BearerAuth("k"))
	r, _, err := OpenAI{D: d, Ref: ref, Model: "m"}.CompletePrefixedChat(context.Background(),
		[]byte(`{"messages":[{"role":"user","content":"t"}]}`), "q")
	if err != nil || r != "abcd" {
		t.Fatalf("reply=%q err=%v", r, err)
	}
}

func TestCompletePrefixedChatReadsAdjudicationToolAmongOthers(t *testing.T) {
	var got []byte
	up := chatServer(t, 200, `{"choices":[{"message":{"content":"prose","tool_calls":[`+
		`{"id":"1","function":{"name":"Read","arguments":"{\"p\":1}"}},`+
		`{"id":"2","function":{"name":"context_guru_adjudicate","arguments":"{\"verdicts\":[{\"i\":0}]}"}}]}}],`+
		`"usage":{"prompt_tokens":10,"completion_tokens":1}}`, &got)
	defer up.Close()
	d, ref := dispatchtest.HTTP(up.URL, dispatchtest.BearerAuth("k"))
	reply, u, err := OpenAI{D: d, Ref: ref, Model: "m"}.CompletePrefixedChat(context.Background(),
		[]byte(`{"messages":[{"role":"user","content":"t"}]}`), "q")
	if err != nil || reply != `{"verdicts":[{"i":0}]}` || !u.ViaTool {
		t.Fatalf("reply=%q usage=%+v err=%v", reply, u, err)
	}
}

func TestCompletePrefixedChatRefusals(t *testing.T) {
	ok := []byte(`{"messages":[{"role":"user","content":"t"}]}`)
	for name, tc := range map[string]struct {
		body   []byte
		status int
		reply  string
	}{
		"no messages array": {[]byte(`{"model":"m"}`), 200, `{}`},
		"non-200":           {ok, 401, `{"error":"no"}`},
		"not json":          {ok, 200, `<html>`},
	} {
		var got []byte
		up := chatServer(t, tc.status, tc.reply, &got)
		d, ref := dispatchtest.HTTP(up.URL, dispatchtest.BearerAuth("k"))
		reply, _, err := OpenAI{D: d, Ref: ref, Model: "m"}.CompletePrefixedChat(context.Background(), tc.body, "q")
		up.Close()
		if err == nil || reply != "" {
			t.Errorf("%s: reply=%q err=%v", name, reply, err)
		}
	}
	// No dispatcher: fail open with an error.
	if _, _, err := (OpenAI{Model: "m"}).CompletePrefixedChat(context.Background(), ok, "q"); err == nil {
		t.Error("no dispatcher did not error")
	}
	// An empty choices list is an empty reply, not a panic.
	var got []byte
	up := chatServer(t, 200, `{"choices":[]}`, &got)
	defer up.Close()
	d, ref := dispatchtest.HTTP(up.URL, dispatchtest.BearerAuth("k"))
	if r, _, err := (OpenAI{D: d, Ref: ref, Model: "m"}).CompletePrefixedChat(context.Background(), ok, "q"); err != nil || r != "" {
		t.Errorf("empty choices: %q %v", r, err)
	}
}

func TestCompletePrefixedChatPrefersMaxCompletionTokensWhenBothAreSent(t *testing.T) {
	var got []byte
	up := chatServer(t, 200, `{"choices":[{"message":{"content":"x"}}]}`, &got)
	defer up.Close()
	d, ref := dispatchtest.HTTP(up.URL, dispatchtest.BearerAuth("k"))
	_, _, err := OpenAI{D: d, Ref: ref, Model: "m", MaxTokens: 77}.CompletePrefixedChat(context.Background(),
		[]byte(`{"model":"m","max_tokens":5,"max_completion_tokens":6,"messages":[{"role":"user","content":"t"}]}`), "q")
	if err != nil || gjson.GetBytes(got, "max_completion_tokens").Int() != 77 {
		t.Fatalf("err=%v body=%s", err, got)
	}
}

// The prefixed Chat ask (the sweep's question) uses the client's Path too, so it reaches a route
// with its own path (Bob) and not only /v1/chat/completions.
func TestOpenAIPrefixedChatPostsToItsPath(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"", "/v1/chat/completions"},
		{"/inference/v1/chat/completions", "/inference/v1/chat/completions"},
	} {
		var got string
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":1}}`))
		}))
		d, ref := dispatchtest.HTTP(up.URL, nil)
		_, _, err := OpenAI{D: d, Ref: ref, Model: "m", Path: tc.path}.CompletePrefixedChat(context.Background(),
			[]byte(`{"model":"m","messages":[{"role":"user","content":"task"}]}`), "judge")
		up.Close()
		if err != nil {
			t.Fatalf("Path %q: err %v", tc.path, err)
		}
		if got != tc.want {
			t.Errorf("Path %q: posted to %q, want %q", tc.path, got, tc.want)
		}
	}
}
