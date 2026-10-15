package keepalive

import (
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

// The name check is for relays only: native Anthropic, Bedrock and Vertex are eligible by upstream
// type whatever the deployment is called; a relay on the Anthropic route must serve a Claude name.
// Port of main's TestKeepAliveNameCheckIsForRelaysOnly (67c5f758:proxy/keepalive_inflight_test.go),
// merged with the brief's own cases.
func TestKeepAliveNameCheckIsForRelaysOnly(t *testing.T) {
	arn := "arn:aws:bedrock:us-east-1:123456789012:application-inference-profile/a1b2c3"
	cases := []struct {
		p           bschemas.ModelProvider
		base, model string
		want        bool
	}{
		{bschemas.Anthropic, "https://api.anthropic.com", "my-team-alias", true},
		{bschemas.Anthropic, "https://api.anthropic.com", "anything", true},
		{bschemas.Anthropic, "https://eu.anthropic.com", "anything", true},
		{bschemas.Bedrock, "https://bedrock-runtime.us-east-1.amazonaws.com", arn, true},
		{bschemas.BedrockMantle, "https://bedrock.example", arn, true},
		{bschemas.Vertex, "https://us-east5-aiplatform.googleapis.com", "publishers/anthropic/models/x", true},
		{bschemas.Anthropic, "http://litellm:4000", "aws/claude-sonnet-5", true},
		{bschemas.Anthropic, "http://litellm:4000", "vertex_ai/claude-opus-4-5@20251101", true},
		{bschemas.Anthropic, "http://litellm:4000", "us.anthropic.claude-sonnet-4-5-20250929-v1:0", true},
		{bschemas.Anthropic, "http://litellm:4000", "opus5-5", true},
		{bschemas.Anthropic, "https://gw.example.com", "aws/claude-haiku-4-5", true},
		{bschemas.Anthropic, "https://gw.example.com", "us.anthropic.claude-sonnet", true},
		{bschemas.Anthropic, "http://litellm:4000", "gpt-5-mini-2025-08-07", false},
		{bschemas.Anthropic, "https://gw.example.com", "gpt-5-mini", false},
		{bschemas.Anthropic, "http://litellm:4000", arn, false},
		{bschemas.Anthropic, "https://evil-anthropic.com.example", "gpt-5", false},
		{bschemas.Anthropic, "https://evilanthropic.com", "gpt-5", false},
		{bschemas.Anthropic, "https://anthropic.com.evil.example", "gpt-5-mini", false},
	}
	for _, c := range cases {
		if got := relayServesClaude(c.p, c.base, c.model); got != c.want {
			t.Errorf("relayServesClaude(%s, %q, %q) = %v, want %v", c.p, c.base, c.model, got, c.want)
		}
	}
	if !relayServesClaude(bschemas.OpenAI, "https://gw.example.com", "gpt-5-mini") {
		t.Error("the check is for the Anthropic route only")
	}
}

// CONTEXT_GURU_KEEPALIVE_MODELS adds names a relay serves under aliases that do not say what they
// are. Port of main's TestKeepAliveModelAliasesFromTheEnvironment, merged with the brief's case.
func TestKeepAliveModelAliasesFromTheEnvironment(t *testing.T) {
	t.Setenv("CONTEXT_GURU_KEEPALIVE_MODELS", " application-inference-profile/a1b2c3 , team-model ")
	cases := map[string]bool{
		"arn:aws:bedrock:us-east-1:123456789012:application-inference-profile/a1b2c3": true,
		"Team-Model-Large":      true,
		"gpt-5-mini-2025-08-07": false,
	}
	for model, want := range cases {
		if got := relayServesClaude(bschemas.Anthropic, "http://litellm:4000", model); got != want {
			t.Errorf("%s: %v, want %v", model, got, want)
		}
	}
}

// TestKeepAliveModelAliasesFromTheEnvironmentSimpleCase is the brief's own smaller example, kept
// alongside the merged test above since it exercises a different alias shape (a bare substring
// match on "house-model" via CONTEXT_GURU_KEEPALIVE_MODELS, not an ARN).
func TestKeepAliveModelAliasesFromTheEnvironmentSimpleCase(t *testing.T) {
	t.Setenv("CONTEXT_GURU_KEEPALIVE_MODELS", "house-model,other")
	if !relayServesClaude(bschemas.Anthropic, "https://gw.example.com", "team/house-model-v2") {
		t.Fatal("a word from CONTEXT_GURU_KEEPALIVE_MODELS must count as a Claude model")
	}
}
