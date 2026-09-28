package record

import (
	"encoding/base64"
	"strings"
	"testing"
)

// canary is the value that must never survive. Opaque on purpose (entropy 4.64, no known
// prefix) so a test that passes did so because of the rule under test and not because some
// prefix pattern happened to fire.
const canary = "CANARYVALUE0123456789abcdefXYZ99"

// TestExtraShapesClosedTheMeasuredGap pins the seven credential shapes that
// internal/redact.Content was measured to miss. Each case here corresponds to one row of
// the comparison recorded in redact.go's doc comment, so if someone later decides these
// additions are redundant, this test is the evidence that they are not: delete the
// additions and these fail while dash's own tests keep passing.
func TestExtraShapesClosedTheMeasuredGap(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"aws sts session key", "ASIAIOSFODNN7EXAMPLE"},
		{"aws abia", "ABIAIOSFODNN7EXAMPLE"},
		{"databricks pat", "dapiabcdef1234567890abcdef1234567890"},
		{"gitlab deploy token", "gldt-abcdefghij1234567890"},
		{"npm token", "npm_abcdefghijklmnopqrstuvwxyz0123"},
		{"pypi token", "pypi-AgEIcHlwaS5vcmcabcdefghijklmnop"},
		{"sendgrid key", "SG.abcdefghijklmnopqrstuv.abcdefghijklmnopqrstuvwxyz0123456789"},
		{"slack webhook", "https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX"},
		{"pgp private key", "-----BEGIN PGP PRIVATE KEY BLOCK-----\nlQOYBF+fake\n-----END PGP PRIVATE KEY BLOCK-----"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Content("value=" + c.in)
			if strings.Contains(got, c.in) {
				t.Errorf("credential survived scrubbing\n in: %s\nout: %s", c.in, got)
			}
			if !strings.Contains(got, Placeholder) {
				t.Errorf("no placeholder written, so a reader cannot tell something was removed: %s", got)
			}
		})
	}
}

// TestUpstreamShapesStillCovered is the other half: the shapes internal/redact already
// handles must keep working THROUGH this wrapper. Composing could have broken them (a
// later pass mangling an earlier replacement), so it is checked rather than assumed.
func TestUpstreamShapesStillCovered(t *testing.T) {
	cases := []struct{ name, in, mustGo string }{
		{"anthropic key", "key: sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAA-BBBB", "sk-ant-api03"},
		{"aws akia", "AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE", "AKIAIOSFODNN7EXAMPLE"},
		{"github pat", "GITHUB_TOKEN=ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijkl", "ghp_ABCDEF"},
		{"jwt", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n", "eyJzdWIi"},
		{"cg own token", "cg_live_abcdefghijklmnopqrstuvwxyz", "cg_live_abcdef"},
		{"google oauth", "ya29.a0AfH6SMBabcdefghijklmnopqrstuvwxyz", "ya29.a0AfH6"},
		{"json shaped api key", `{"x-api-key": "` + canary + `", "model": "claude-opus-4"}`, canary},
		{"bearer header", "Authorization: Bearer " + canary, canary},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Content(c.in)
			if strings.Contains(got, c.mustGo) {
				t.Errorf("credential survived\n in: %s\nout: %s", c.in, got)
			}
		})
	}
}

// TestURLAndConnectionStringsKeepTheirShape checks that the password goes and the rest
// stays. A wholly redacted URL is data loss dressed up as safety: the field exists to
// answer "where was this pointing", and that answer is not the secret.
func TestURLAndConnectionStringsKeepTheirShape(t *testing.T) {
	cases := []struct {
		name, in string
		survive  []string
		mustGo   string
	}{
		{
			name:    "postgres",
			in:      "postgresql://admin:s3cr3t_pw@db.example.com:5432/mydb",
			survive: []string{"postgresql", "admin", "db.example.com", "mydb"},
			mustGo:  "s3cr3t_pw",
		},
		{
			name:    "mongodb srv",
			in:      "mongodb+srv://user:p4ssw0rd@cluster0.mongodb.net/test",
			survive: []string{"user", "cluster0.mongodb.net"},
			mustGo:  "p4ssw0rd",
		},
		{
			name:    "https userinfo",
			in:      "https://alice:hunter2hunter2@internal.example.com/path",
			survive: []string{"alice", "internal.example.com", "/path"},
			mustGo:  "hunter2hunter2",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Content(c.in)
			if strings.Contains(got, c.mustGo) {
				t.Errorf("password survived: %s", got)
			}
			for _, s := range c.survive {
				if !strings.Contains(got, s) {
					t.Errorf("lost %q, which is not the secret: %s", s, got)
				}
			}
		})
	}
}

// TestBase64WrappedSecretsAreCaught transfers the cases from claude-record's
// tests/test_redactor.py (TestBase64EncodedSecrets), which is the one capability the Python
// redactor had that internal/redact deliberately declined. redact.go explains why the
// trade-off flips for a recorded corpus.
func TestBase64WrappedSecretsAreCaught(t *testing.T) {
	cases := []struct{ name, plain string }{
		{"api key", "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAA-BBBBBBBBBBBBBBBBBBB"},
		{"private key", "-----BEGIN RSA PRIVATE KEY-----\nMIIBogIBAAJBAL+fake+key+data+here\n-----END RSA PRIVATE KEY-----"},
		{"connection string", "postgresql://admin:supersecretpassword@db.example.com:5432/mydb"},
		{"jwt", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4ifQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"},
		{"npm token in env dump", "NPM_TOKEN=npm_abcdefghijklmnopqrstuvwxyz0123456789"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			enc := base64.StdEncoding.EncodeToString([]byte(c.plain))
			got := Content("config_value=" + enc)
			if strings.Contains(got, enc) {
				t.Errorf("encoded secret survived\nencoded: %s\n    out: %s", enc, got)
			}
			if !strings.Contains(got, Placeholder) {
				t.Errorf("no placeholder: %s", got)
			}
		})
	}
}

// TestOrdinaryBase64Survives is the false-positive guard, and it is the case that makes the
// base64 rule defensible at all. internal/redact declined this rule because a decoder
// "would flag ordinary text"; that objection only holds if the rule replaces on shape
// rather than on decoded content. It replaces on decoded content, so ordinary base64 is
// untouched.
func TestOrdinaryBase64Survives(t *testing.T) {
	plain := "This is just a regular paragraph of text that happens to be long enough to trigger the base64 regex and nothing more."
	enc := base64.StdEncoding.EncodeToString([]byte(plain))
	if got := Content(enc); got != enc {
		t.Errorf("ordinary base64 was altered, which teaches readers to ignore the placeholder\n in: %s\nout: %s", enc, got)
	}
}

// TestNegativeControlsSurvive pins the "safety that destroys the feature" trap from the
// other direction. Every string here is one an over-eager redactor eats, and each would
// break a real analysis: max_tokens is a threshold someone filters on, a git sha is how a
// turn is tied to a commit, and a tool_use_id is how a tool result is joined to its call.
func TestNegativeControlsSurvive(t *testing.T) {
	cases := []struct{ name, in string }{
		{"token thresholds", "max_tokens=3000 min_request_tokens=500"},
		{"prose", "The quick brown fox jumps over the lazy dog repeatedly and at some length here."},
		{"git sha", "commit 32920fddde09aff43f03d876c1907c4f5a2835ff"},
		{"uuid", "4d6f1b2a-9c3e-4f5a-8b7d-1e2f3a4b5c6d"},
		{"anthropic tool id", "toolu_bdrk_01234567890123456789"},
		{"anthropic message id", "msg_01ABCdefGHIjklMNOpqrs"},
		{"code", "def compute(a, b):\n    return a * b + 42"},
		{"span id hex16", "065c1247752161c5"},
		{"model name", "claude-sonnet-5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Content(c.in); got != c.in {
				t.Errorf("harmless value was redacted\n in: %s\nout: %s", c.in, got)
			}
		})
	}
}

// TestContentIsIdempotent matters because Redact may be called twice on a retry path, and a
// second pass that mangled the first pass's placeholders would corrupt a row.
func TestContentIsIdempotent(t *testing.T) {
	in := "key=sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAA-BBBB and npm_abcdefghijklmnopqrstuvwxyz0123 and " +
		"postgresql://admin:pw123456@db.example.com/x"
	once := Content(in)
	twice := Content(once)
	if once != twice {
		t.Errorf("not idempotent\nonce : %s\ntwice: %s", once, twice)
	}
}

// TestRedactScrubsEveryFreeTextFieldOnTheTurn is the completeness check on Turn. A field
// added later and not scrubbed here is the leak this test exists to catch: every string
// below is seeded with the same canary, so a missed field is an explicit failure rather
// than something noticed in review.
func TestRedactScrubsEveryFreeTextFieldOnTheTurn(t *testing.T) {
	secret := "sk-ant-api03-" + canary + "-BBBBBBBBBBBB"
	turn := Turn{
		RequestBody:  `{"messages":[{"role":"user","content":"my key is ` + secret + `"}]}`,
		ResponseBody: `{"content":[{"text":"I see ` + secret + `"}]}`,
		OutputSchema: `{"type":"object","description":"` + secret + `"}`,
		Err:          "upstream 401: presented credential " + secret,
		StopReason:   secret,
	}
	turn.Redact(0)

	for name, got := range map[string]string{
		"RequestBody":  turn.RequestBody,
		"ResponseBody": turn.ResponseBody,
		"OutputSchema": turn.OutputSchema,
		"Err":          turn.Err,
		"StopReason":   turn.StopReason,
	} {
		if strings.Contains(got, canary) {
			t.Errorf("%s still carries the credential: %s", name, got)
		}
	}
}

// TestRedactReportsCapAsIncompleteness is the corpus-integrity property. A capped body that
// does not say it was capped is indistinguishable at analysis time from a short response,
// which silently biases anything computed over response length.
func TestRedactReportsCapAsIncompleteness(t *testing.T) {
	turn := Turn{ResponseBody: strings.Repeat("a", 5000)}
	turn.Redact(1000)

	if len(turn.ResponseBody) > 1000 {
		t.Errorf("cap not applied: len=%d", len(turn.ResponseBody))
	}
	if turn.Incomplete != IncompleteTeeCap {
		t.Errorf("cap hit not recorded as incompleteness: Incomplete=%q", turn.Incomplete)
	}
}

// TestRedactKeepsTheProxysMoreSpecificReason checks precedence. A body that never
// materialised (retain bound) is a more specific fact than a downstream cap hit, so the
// proxy's reason must not be overwritten by the redactor's.
func TestRedactKeepsTheProxysMoreSpecificReason(t *testing.T) {
	turn := Turn{ResponseBody: strings.Repeat("a", 5000), Incomplete: IncompleteRetainCap}
	turn.Redact(1000)

	if !strings.HasPrefix(turn.Incomplete, IncompleteRetainCap) {
		t.Errorf("proxy reason lost: %q", turn.Incomplete)
	}
	if !strings.Contains(turn.Incomplete, IncompleteTeeCap) {
		t.Errorf("cap hit not also recorded: %q", turn.Incomplete)
	}
}

// TestRedactOnUncappedTurnSetsNoIncompleteness guards the opposite error: marking a whole
// turn incomplete would make every row suspect and the marker useless.
func TestRedactOnUncappedTurnSetsNoIncompleteness(t *testing.T) {
	turn := Turn{RequestBody: "hello", ResponseBody: "world"}
	turn.Redact(1 << 20)
	if turn.Incomplete != IncompleteNone {
		t.Errorf("complete turn marked incomplete: %q", turn.Incomplete)
	}
}

// TestCapContentIsRuneSafe: a cap landing mid-rune would write invalid UTF-8 into a JSON
// file, and encoding/json would then replace it with U+FFFD -- corrupting the last
// character of every capped body.
func TestCapContentIsRuneSafe(t *testing.T) {
	s := strings.Repeat("é", 100) // 2 bytes each
	got, hit := CapContent(s, 51) // deliberately mid-rune
	if !hit {
		t.Fatal("expected cap to bite")
	}
	if !strings.ContainsRune(got, 'é') || strings.ContainsRune(got, '�') {
		t.Errorf("cap split a rune: %q", got)
	}
	if len(got) != 50 {
		t.Errorf("expected backup to rune start (50 bytes), got %d", len(got))
	}
}
