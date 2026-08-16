package logging

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// The credentials a request carries once the proxy forwards the CALLER's own
// provider key. Each is a shape that really arrives: the Anthropic header, the
// OpenAI bearer, an AWS key id, a line out of a `.env` a tool printed, a userinfo
// credential in an upstream URL, and a session JWT.
//
// `secret` is what gets logged; `fragment` is the distinctive middle of the secret
// itself, asserted separately because a HALF-redacting pattern is the failure mode
// that matters — the old `Authorization: Bearer \S+` rule redacted the scheme and
// published the token, and an assertion on the full string alone would have passed.
var reqSecrets = []struct{ name, secret, fragment string }{
	{"anthropic x-api-key", "sk-ant-api03-Zt7QwLm4Xy8vBn2KpR6sTuVwXyZ0123456789abcdefgh", "Zt7QwLm4Xy8vBn2KpR6s"},
	{"openai bearer", "sk-proj-9fJk2LmN4pQr6StU8vWx0YzA1bC3dE5fG7hI9jK1lM3nO5pQ", "9fJk2LmN4pQr6StU8vWx"},
	{"aws access key", "AKIAIOSFODNN7EXAMPLE", "IOSFODNN7EXAMPLE"},
	{"env file value", "ANTHROPIC_API_KEY=sk-ant-oat01-QQQQwwwweeeerrrrttttyyyy1234", "QQQQwwwweeeerrrrtttt"},
	{"upstream userinfo", "https://svc:s3cr3t-p4ssw0rd@gateway.internal.example.com/v1", "s3cr3t-p4ssw0rd"},
	{"jwt session", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ0ZW5hbnQtNyJ9.4Xy8vBn2KpR6sTuVwXyZ0", "eyJzdWIiOiJ0ZW5hbnQtNyJ9"},
}

// secretByName is the lookup the test body reads from.
func secretByName(name string) string {
	for _, s := range reqSecrets {
		if s.name == name {
			return s.secret
		}
	}
	panic("no such secret: " + name)
}

// TestDebugLoggingALiveRequestLeaksNoCredential is the test the whole scrubber
// exists for. It logs a realistic request at DEBUG — headers, the upstream URL, an
// error string, a `.env` line, and a couple of deliberately careless call shapes —
// and asserts that no credential appears anywhere in the output.
//
// It deliberately includes the careless shapes (a secret in the MESSAGE, a whole
// http.Header handed to one attr, a credential under an innocent key name) because
// those are what a future call site will actually look like. A test that only
// covered the disciplined shapes would pass while the leak stayed open.
func TestDebugLoggingALiveRequestLeaksNoCredential(t *testing.T) {
	var buf bytes.Buffer
	for _, asJSON := range []bool{false, true} {
		buf.Reset()
		lg := slog.New(New(&buf, slog.LevelDebug, asJSON))

		hdr := http.Header{
			"Authorization":       {"Bearer " + secretByName("openai bearer")},
			"X-Api-Key":           {secretByName("anthropic x-api-key")},
			"Content-Type":        {"application/json"},
			"X-Context-Guru-Auth": {secretByName("jwt session")},
		}

		// A per-request logger, exactly as proxy builds one — the With() attrs must be
		// scrubbed too, or every subsequent line on this request republishes the secret.
		req := lg.With("tenant", "t-42", "session", "s-9",
			"api_key", secretByName("openai bearer"), // key name says credential
			"upstream", secretByName("upstream userinfo"))

		// A whole http.Header handed to one attr. A printed header map is ONE line, and the
		// auth-header rule matches each auth header's value up to the next field rather than
		// to the end of the line — so the credentials go and Content-Type survives. What it
		// does NOT cover is an auth header this codebase has never heard of holding an
		// opaque value: that is a denylist's structural limit (see internal/redact), not
		// something this attr shape gets away with.
		req.Debug("forwarding", "header", hdr, "auth", hdr.Get("Authorization"),
			"content_type", hdr.Get("Content-Type"))
		req.Debug("component declined", "component", "extract_llm", "gate", "economic_gate",
			"tokens_before", 8123, "tokens_after", 8123)
		// Careless shapes.
		req.Debug("upstream " + secretByName("upstream userinfo") + " refused")
		req.Debug("env dump", "line", secretByName("env file value"))
		req.Debug("aws", "value", secretByName("aws access key"))
		req.Debug("boom", "err", &urlish{secretByName("upstream userinfo")})
		req.WithGroup("outbound").Debug("retry", "x-api-key", secretByName("anthropic x-api-key"))

		out := buf.String()
		for _, s := range reqSecrets {
			for _, needle := range []string{s.secret, s.fragment} {
				if strings.Contains(out, needle) {
					t.Errorf("json=%v: %s leaked into the log output: %q\n--- output ---\n%s",
						asJSON, s.name, needle, out)
				}
			}
		}
		// And prove the log is still USEFUL — a scrubber that redacts everything would
		// pass the assertions above and be worthless.
		for _, keep := range []string{"t-42", "s-9", "extract_llm", "economic_gate", "8123",
			"gateway.internal.example.com", "application/json"} {
			if !strings.Contains(out, keep) {
				t.Errorf("json=%v: %q should have survived redaction but did not\n--- output ---\n%s",
					asJSON, keep, out)
			}
		}
	}
}

// TestASecretInAnAttributeKeyIsNotEmitted covers the half of scrubAttr that only
// looked at values. `slog.Info("x", k, v)` takes k from whatever the call site has in
// hand, and a `for k, v := range someMap` over parsed data puts data in the key slot —
// so the key is as untrusted as the value. It was emitted verbatim.
//
// No call site builds a key from data today; this is a latent trap, and it is cheaper
// to close than to keep true.
func TestASecretInAnAttributeKeyIsNotEmitted(t *testing.T) {
	secret := secretByName("openai bearer")
	for _, asJSON := range []bool{false, true} {
		var buf bytes.Buffer
		lg := slog.New(New(&buf, slog.LevelDebug, asJSON))
		// The careless shape: a key that came from data.
		lg.Info("x", secret, "present")
		lg.Info("y", "api_key="+secret, 1) // and one with a numeric value, which short-circuits
		if out := buf.String(); strings.Contains(out, secret) ||
			strings.Contains(out, "9fJk2LmN4pQr6StU8vWx") {
			t.Errorf("json=%v: a credential in the attribute KEY was emitted verbatim:\n%s", asJSON, out)
		}
	}
}

// urlish mimics *url.Error, whose Error() embeds the upstream URL.
type urlish struct{ u string }

func (e *urlish) Error() string { return `Post "` + e.u + `": dial tcp: refused` }

func TestLevelBelowThresholdIsNotEmitted(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(New(&buf, slog.LevelInfo, false))
	lg.Debug("expensive detail")
	lg.Info("lifecycle")
	if strings.Contains(buf.String(), "expensive detail") {
		t.Fatal("a DEBUG record was emitted at level INFO")
	}
	if !strings.Contains(buf.String(), "lifecycle") {
		t.Fatal("the INFO record went missing")
	}
	// The guard the hot path relies on must agree with the handler.
	ctx := With(context.Background(), lg)
	if Debugging(ctx) {
		t.Fatal("Debugging() reported true at level INFO; expensive payloads would be built")
	}
}

func TestFromFallsBackToTheDefaultLogger(t *testing.T) {
	if From(context.Background()) != slog.Default() {
		t.Fatal("From() must yield the default logger when no request logger is set")
	}
	//nolint:staticcheck // a nil context is exactly what a careless caller passes
	if From(nil) != slog.Default() {
		t.Fatal("From(nil) must not panic and must yield the default logger")
	}
	var buf bytes.Buffer
	lg := slog.New(New(&buf, slog.LevelDebug, false))
	if got := From(With(context.Background(), lg)); got != lg {
		t.Fatal("From() did not return the logger put in by With()")
	}
}
