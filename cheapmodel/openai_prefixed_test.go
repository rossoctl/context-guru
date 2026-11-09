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
