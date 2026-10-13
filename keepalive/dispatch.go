package keepalive

import "net/http"

// PingHeaders copies the non-secret headers a ping needs from the recorded request.
func PingHeaders(src http.Header) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	for _, name := range []string{"Anthropic-Version", "Anthropic-Beta", "OpenAI-Organization", "OpenAI-Project"} {
		if v := src.Get(name); v != "" {
			h.Set(name, v)
		}
	}
	return h
}
