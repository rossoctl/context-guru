package keepalive

import (
	"net/http"
	"testing"
)

func TestPingHeadersCarriesNoCredential(t *testing.T) {
	src := http.Header{}
	src.Set("Authorization", "Bearer secret")
	src.Set("x-api-key", "secret")
	src.Set("x-goog-api-key", "secret")
	src.Set("Cookie", "secret")
	src.Set("Anthropic-Version", "2023-06-01")
	src.Set("Anthropic-Beta", "prompt-caching-2024-07-31")
	src.Set("OpenAI-Organization", "org-1")
	src.Set("OpenAI-Project", "proj-1")
	got := PingHeaders(src)
	for name, vs := range got {
		for _, v := range vs {
			if v == "secret" || v == "Bearer secret" {
				t.Fatalf("PingHeaders copied %s", name)
			}
		}
	}
	want := map[string]string{
		"Content-Type": "application/json", "Anthropic-Version": "2023-06-01",
		"Anthropic-Beta": "prompt-caching-2024-07-31", "Openai-Organization": "org-1",
		"Openai-Project": "proj-1",
	}
	for k, v := range want {
		if got.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, got.Get(k), v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("PingHeaders returned %d headers, want %d: %v", len(got), len(want), got)
	}
}
