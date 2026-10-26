package keepalive

import (
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
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

// Eligibility: only the Responses route, and only models with the documented 30-minute cache.
// The OpenAI ping CADENCE is resolved by the host (the proxy's record), and tested there.
func TestOpenAIKeepAliveOnlyKnownModelsAreEligible(t *testing.T) {
	if !eligible(bschemas.OpenAI, "azure/gpt-5.6-luna", "/v1/responses") ||
		eligible(bschemas.OpenAI, "gpt-5.5", "/v1/responses") ||
		eligible(bschemas.OpenAI, "gpt-5.6", "/v1/chat/completions") {
		t.Fatal("OpenAI keep-alive eligibility did not match the tested Responses shape")
	}
}
