package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/tenant"
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
	if modelinfo.OpenAIDefaultKeepAliveIdle != time.Duration(config.DefaultKeepAliveOpenAIIdle)*time.Second {
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
		{"default", modelinfo.OpenAIDefaultKeepAliveIdle},
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
				k.record(&Tenancy{ID: "t1", Cache: pol}, "openai-session", "", at, body, up, req,
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

func TestOpenAIKeepAliveIgnoresAnthropicIntervalControls(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	now := clock.now()
	k.setStrategies([]tenant.Strategy{{
		ID: "short", Active: true, IdleSeconds: 60, MaxPings: 2,
		Windows: []tenant.Window{{Start: "00:00", End: "24:00"}},
		Target:  tenant.Target{Mode: tenant.TargetAll}, UpdatedAt: now,
	}})
	armOn(t, k, "openai-session", 60*time.Second, 2, now.Add(time.Hour))
	pol := kaPolicy()
	pol.KeepAlive = false // the manual arm must enable this one session
	pol.OpenAIIdle = 28 * time.Minute
	pol.MinPrefixTokens = 1000
	body := []byte(`{"model":"gpt-5.6","input":[{"role":"user","content":"hello"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	up := upstream{base: "http://up", path: "/v1/responses"}
	k.record(&Tenancy{ID: "t1", Cache: pol}, "openai-session", "", now, body, up, req,
		bschemas.OpenAI, up.path, http.StatusOK, Usage{CacheRead: 4000}, true)
	k.record(&Tenancy{ID: "t1", Cache: pol}, "openai-session", "", now.Add(time.Second), body, up, req,
		bschemas.OpenAI, up.path, http.StatusOK, Usage{CacheRead: 4000}, true)
	k.mu.Lock()
	e := k.live[kaKey("t1", "openai-session")]
	k.mu.Unlock()
	if e == nil {
		t.Fatal("eligible OpenAI entry was not retained")
	}
	if !e.pol.KeepAlive || e.pol.Idle != 28*time.Minute || e.pol.MaxPings != 1 ||
		e.pol.MinPrefixTokens != 0 || e.appliedStrategy != "" {
		t.Fatalf("manual OpenAI override was ineffective or imported an Anthropic cadence: policy=%+v strategy=%q",
			e.pol, e.appliedStrategy)
	}
	if hold := time.Duration(e.pol.MaxPings+1) * e.pol.Idle; hold > maxOverrideHold {
		t.Fatalf("OpenAI override retained credentials for %s, beyond %s", hold, maxOverrideHold)
	}
	k.disarm("t1", "openai-session")
	k.record(&Tenancy{ID: "t1", Cache: pol}, "openai-session", "", now.Add(2*time.Second), body, up, req,
		bschemas.OpenAI, up.path, http.StatusOK, Usage{CacheRead: 4000}, true)
	k.mu.Lock()
	e = k.live[kaKey("t1", "openai-session")]
	k.mu.Unlock()
	if e != nil {
		t.Fatal("disarmed OpenAI session remained retained on an account with keep-alive off")
	}
}
