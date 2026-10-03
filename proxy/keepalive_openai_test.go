package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/config"
	"github.com/tidwall/gjson"
)

func TestOpenAIResponsesPingPreservesTheCachedPrefix(t *testing.T) {
	body := []byte(`{"model":"azure/gpt-5.6-luna","instructions":"stable head",` +
		`"prompt_cache_options":{"ttl":"30m"},"input":[{"role":"user","content":"stable text"}],` +
		`"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}],` +
		`"max_output_tokens":2048,"stream":true}`)
	out, ok := pingBody(body, "/v1/responses")
	if !ok {
		t.Fatal("Responses ping body was refused")
	}
	if got := gjson.GetBytes(out, "max_output_tokens").Int(); got != 16 {
		t.Errorf("max_output_tokens = %d, want the gateway minimum 16", got)
	}
	if gjson.GetBytes(out, "stream").Bool() {
		t.Error("ping still streams")
	}
	if gjson.GetBytes(out, "max_tokens").Exists() {
		t.Error("Anthropic max_tokens was added to a Responses request")
	}
	for _, path := range []string{"model", "instructions", "prompt_cache_options", "input", "tools"} {
		if got, want := gjson.GetBytes(out, path).Raw, gjson.GetBytes(body, path).Raw; got != want {
			t.Errorf("cached prefix field %s changed: %s != %s", path, got, want)
		}
	}
}

func TestOpenAIKeepAliveUsesItsOwnLifetimeAndOnlyKnownModels(t *testing.T) {
	if defaultOpenAIKeepAliveIdle != time.Duration(config.DefaultKeepAliveOpenAIIdle)*time.Second {
		t.Fatal("proxy and config disagree about the default OpenAI ping interval")
	}
	if !keepAliveEligible(bschemas.OpenAI, "azure/gpt-5.6-luna", "/v1/responses") ||
		keepAliveEligible(bschemas.OpenAI, "gpt-5.5", "/v1/responses") ||
		keepAliveEligible(bschemas.OpenAI, "gpt-5.6", "/v1/chat/completions") {
		t.Fatal("OpenAI keep-alive eligibility did not match the tested Responses shape")
	}
	for _, tc := range []struct {
		name string
		idle time.Duration
	}{
		{"default", defaultOpenAIKeepAliveIdle},
		{"configured", 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, _, clock := testKeeper(t, Limits{})
			pol := kaPolicy()
			pol.MinPrefixTokens = 1000
			pol.MaxPings = 1
			if tc.name == "configured" {
				pol.OpenAIIdle = tc.idle
			}
			body := []byte(`{"model":"azure/gpt-5.6-luna","input":[{"role":"user","content":"hello"}],"stream":true}`)
			req := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
			req.Header.Set("Authorization", "Bearer test-key")
			up := upstream{base: "http://up", path: "/v1/responses"}
			for _, at := range []time.Time{clock.now().Add(-time.Second), clock.now()} {
				k.record(&Tenancy{ID: "t1", Cache: pol}, "openai-session", at, body, up, req,
					bschemas.OpenAI, up.path, http.StatusOK,
					Usage{CacheRead: 4000, CacheWrite: 100}, true)
			}
			start := clock.now()
			if got := k.sweep(start.Add(tc.idle - time.Second)); got != 0 {
				t.Fatalf("ping fired early: %d", got)
			}
			if got := k.sweep(start.Add(tc.idle)); got != 1 {
				t.Fatalf("ping did not fire at %s: %d", tc.idle, got)
			}
		})
	}
}
