package keepalive

import (
	"net/url"
	"os"
	"strings"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

// relayServesClaude: the Anthropic route forwarded to anything but Anthropic itself is a relay
// (LiteLLM, a corporate gateway) that may serve another family there, an OpenAI / Azure model
// with automatic caching or none, where a ping buys nothing. Only those are name-checked; native
// Anthropic, Bedrock and Vertex upstreams are eligible by type (their ARNs, inference profiles and
// aliases often do not say "claude").
// ponytail: name test; a Claude-named alias of another family is caught by the route breaker.
//
// base may be a full URL (https://api.anthropic.com), a bare host
// (api.anthropic.com), or a bare host with a port (api.anthropic.com:443) —
// Cortex passes the bare-host form (Record.Endpoint, from pctx.Host).
func relayServesClaude(p bschemas.ModelProvider, base, model string) bool {
	if p != bschemas.Anthropic {
		return true
	}
	if h := relayHost(base); h == "anthropic.com" || strings.HasSuffix(h, ".anthropic.com") {
		return true
	}
	return claudeModel(model)
}

// relayHost extracts the lowercased hostname from base, accepting any of
// the forms relayServesClaude is handed: a scheme-qualified URL, a bare
// host, or a bare host:port. url.Parse treats an unqualified string as a
// path rather than an authority, so a scheme-free base is parsed as
// "//" + base to force authority parsing; Hostname() then strips the port.
func relayHost(base string) string {
	b := base
	if !strings.Contains(b, "://") {
		b = "//" + b
	}
	u, err := url.Parse(b)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// claudeModel matches provider-prefixed ids (aws/..., vertex_ai/..., us.anthropic.claude-...,
// Bedrock ARNs naming a Claude model) by substring. CONTEXT_GURU_KEEPALIVE_MODELS adds more,
// comma-separated, for a deployment alias whose name says nothing (an application inference profile).
func claudeModel(model string) bool {
	m := strings.ToLower(model)
	words := []string{"claude", "opus", "sonnet", "haiku"}
	for _, w := range strings.Split(os.Getenv("CONTEXT_GURU_KEEPALIVE_MODELS"), ",") {
		if w = strings.ToLower(strings.TrimSpace(w)); w != "" {
			words = append(words, w)
		}
	}
	for _, w := range words {
		if strings.Contains(m, w) {
			return true
		}
	}
	return false
}
