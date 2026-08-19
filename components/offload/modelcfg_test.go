package offload

import "testing"

// TestModelAloneDoesNotPinThePublicAPI pins the trap that made every extraction call fail
// authentication on a gateway deployment.
//
// `model: {source: incoming, model: claude-haiku-4-5}` is the shape that names the cheap model
// to compact WITH. Client() used to treat any `model:` as a pinned endpoint, so with no base_url
// it built a client at the provider's PUBLIC API and presented whatever credential the
// environment held as x-api-key. Measured result: `authentication_error: API key is invalid.`
// on every call, with `source: incoming` never consulted.
func TestModelAloneDoesNotPinThePublicAPI(t *testing.T) {
	prev := AllowEnvModelKey
	AllowEnvModelKey = true // single-tenant: the env-key fallback is what made this dangerous
	defer func() { AllowEnvModelKey = prev }()
	t.Setenv("ANTHROPIC_API_KEY", "sk-env-key-that-must-not-be-sent-to-a-default-endpoint")

	if c := (modelConfig{Source: "incoming", Model: "claude-haiku-4-5"}).Client(); c != nil {
		t.Fatalf("a bare model: must not pin a client, got %#v", c)
	}
	// An endpoint or a credential is what makes it a pin.
	if c := (modelConfig{Model: "m", BaseURL: "https://gw.example"}).Client(); c == nil {
		t.Fatal("base_url must still pin")
	}
	if c := (modelConfig{Model: "m", APIKey: "sk-explicit"}).Client(); c == nil {
		t.Fatal("api_key must still pin")
	}
	// And no model id is still no client at all.
	if c := (modelConfig{BaseURL: "https://gw.example"}).Client(); c != nil {
		t.Fatal("without a model id there is nothing to call")
	}
}
