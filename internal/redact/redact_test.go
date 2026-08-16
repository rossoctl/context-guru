package redact

import (
	"strings"
	"testing"
)

// canary is the marker that must never survive. Opaque on purpose: a prefix-less
// 32-character credential is exactly the shape only the header rule can catch, so this
// test measures the header rule and not one of the prefix patterns.
const canary = "CANARYVALUE0123456789abcdefXYZ99"

// TestAuthHeaderRuleConsumesTheCredentialNotTheLine covers the auth-header rule from
// both sides at once, because the two failure modes are opposite.
//
// The rule used to match to END OF LINE. That is fail-safe for a log record — but
// Content also scrubs CAPTURED TRANSCRIPT CONTENT, which the dashboard shows users and
// which capture_content now enables by default, and a minified JSON tool result is one
// line. An `"authorization"` field anywhere in it took the whole remainder of the blob
// with it: silent data loss dressed up as redaction.
//
// So: the credential must go (a rule bounded too tightly stops at the quote before it
// has consumed anything), and everything after it must stay (a rule bounded at the
// newline eats the rest of the document). A fix that satisfies only one of these is
// the bug in the other direction.
func TestAuthHeaderRuleConsumesTheCredentialNotTheLine(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		survive []string
	}{
		{
			// The shape that loses data: a minified JSON tool result.
			name: "minified json tool result",
			in: `{"headers":{"authorization":"Bearer ` + canary + `","content-type":"application/json"},` +
				`"model":"claude-opus-4","input_tokens":8123,"stop_reason":"end_turn"}`,
			survive: []string{"application/json", "claude-opus-4", "8123", "end_turn", "content-type"},
		},
		{
			name:    "single-quoted json-ish",
			in:      `{'x-api-key': '` + canary + `', 'model': 'claude-opus-4'}`,
			survive: []string{"claude-opus-4"},
		},
		{
			// A printed http.Header map: one line, several headers. Each auth header is
			// matched on its own; the harmless ones after it survive.
			name:    "printed header map",
			in:      `map[Authorization:[Bearer ` + canary + `] Content-Type:[application/json] User-Agent:[claude-cli/2.0.0]]`,
			survive: []string{"application/json", "claude-cli/2.0.0"},
		},
		{
			// The LOG case, which must not be weakened: a bare header line, scheme and
			// token, nothing to stop at but the end of the line.
			name: "bare header line",
			in:   "Authorization: Bearer " + canary,
		},
		{"bare header line lowercase", "authorization: bearer " + canary, nil},
		{"proxy-authorization basic", "Proxy-Authorization: Basic " + canary, nil},
		{"x-auth-token", "x-auth-token: " + canary, nil},
		{
			// Two header lines: the second must be redacted on its own merits, not because
			// the first one's match ran on to meet it.
			name:    "multi-line header block",
			in:      "Host: gateway.internal.example.com\r\nAuthorization: Bearer " + canary + "\r\nAccept: application/json\r\n",
			survive: []string{"gateway.internal.example.com", "application/json"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Content(tc.in, 0)
			if strings.Contains(got, canary) {
				t.Errorf("the credential survived\n  in:  %s\n  out: %s", tc.in, got)
			}
			for _, s := range tc.survive {
				if !strings.Contains(got, s) {
					t.Errorf("%q was destroyed by the redaction (data loss, not redaction)\n  in:  %s\n  out: %s",
						s, tc.in, got)
				}
			}
		})
	}
}
