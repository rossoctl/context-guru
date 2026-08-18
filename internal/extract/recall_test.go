package extract

import (
	"strings"
	"testing"
)

// The recall check must protect a NEEDLE and must not block compaction because a pervasive
// identifier vanished with the noise it lived in.
//
// MEASURED live before the exemption: an access log whose 900 routine lines each carried
// `handler=src/api/users.py` — the exact path the user asked about. Any reduction that kept
// only the ERROR line dropped that id and was refused, so 3 of 6 calls paid full price for
// nothing precisely BECAUSE the model compacted well.
func TestRecallCheckProtectsNeedlesNotBoilerplate(t *testing.T) {
	noise := strings.Repeat("2024-01-01 GET /users/1 200 12ms handler=src/api/users.py\n", 900)
	body := noise + "ERROR auth timeout on token refresh user=42\nTRACE-ID abc123def456\n"

	t.Run("a pervasive id may vanish with the noise", func(t *testing.T) {
		result := "ERROR auth timeout on token refresh user=42\nTRACE-ID abc123def456\n"
		if !extractionIsSane(body, result, []string{"src/api/users.py", "timeout"}, 0) {
			t.Fatal("a reduction that kept the error and dropped 900 identical handler= lines " +
				"was rejected; that is the compaction being blocked, not recall being protected")
		}
	})

	t.Run("a rare id must still survive", func(t *testing.T) {
		result := strings.Repeat("2024-01-01 GET /users/1 200 12ms handler=src/api/users.py\n", 5)
		// Drops the ERROR line and the trace id, both of which appear once.
		if extractionIsSane(body, result, []string{"abc123def456"}, 0) {
			t.Fatal("a reduction that dropped a once-mentioned trace id was accepted: the " +
				"recall check no longer protects a needle")
		}
	})

	t.Run("the boundary", func(t *testing.T) {
		id := "needle-identifier"
		atLimit := strings.Repeat(id+"\n", pervasiveIDOccurrences)
		overLimit := strings.Repeat(id+"\n", pervasiveIDOccurrences+1)
		if extractionIsSane(atLimit, "nothing kept", []string{id}, 0) {
			t.Fatalf("an id appearing exactly %d times must still be required",
				pervasiveIDOccurrences)
		}
		if !extractionIsSane(overLimit, "nothing kept", []string{id}, 0) {
			t.Fatalf("an id appearing %d times must be treated as boilerplate",
				pervasiveIDOccurrences+1)
		}
	})

	t.Run("an id absent from the body is not required", func(t *testing.T) {
		if !extractionIsSane("hello world", "hello", []string{"absent-identifier"}, 0) {
			t.Fatal("an id that was never in the body was required in the result")
		}
	})
}

// A line-numbered FILE READ must never be structurally parsed. parseBody turns
// `     1\timport json…` into the JSON number 1, and the deterministic projection of 1 is the
// string "1" — smaller than the input, so it was ACCEPTED and a whole source file became a
// single character.
//
// MEASURED live: a 3,598-token file came back as "1", by strategy "deterministic", with the
// acceptance check satisfied. The prompt builder already guards this case for the text it shows
// the model; the fallback did not.
func TestDeterministicNeverParsesALineNumberedFileRead(t *testing.T) {
	body := "     1\timport json\n     2\timport os\n     3\t\n" +
		"     4\tdef parse_config(path):\n     5\t    return json.load(open(path))\n"

	if got := deterministicInput(body); got != body {
		t.Fatalf("a line-numbered file read was parsed into %T (%v); it must stay raw text",
			got, got)
	}
	out := resultToText(DeterministicProject(deterministicInput(body), []string{"parse_config"}, 4000))
	if len(out) < 20 {
		t.Fatalf("the deterministic projection collapsed a source file to %q", out)
	}
	if !strings.Contains(out, "parse_config") {
		t.Fatalf("the projection dropped the identifier it was told to keep: %q", out)
	}

	// A genuine JSON body must still be projected structurally — that is the whole point of
	// the deterministic strategy.
	jsonBody := `[{"path":"a.py","match":"parse_config"},{"path":"b.py","match":"other"}]`
	if _, raw := deterministicInput(jsonBody).(string); raw {
		t.Fatal("a real JSON array was treated as raw text, disabling structural projection")
	}
}

// An identifier at the end of a sentence must not carry the period. identRe allows '.' inside
// an identifier so paths survive whole, which also swallowed the full stop in
// "fix parse_config." — and because the recall check skips ids absent from the body, the one
// identifier the agent actually named then protected nothing.
func TestHarvestIdentifiersTrimsTrailingPunctuation(t *testing.T) {
	got := HarvestIdentifiers("Find the auth timeout in src/api/users.py and fix parse_config.", 40)
	has := func(s string) bool {
		for _, g := range got {
			if g == s {
				return true
			}
		}
		return false
	}
	if !has("parse_config") {
		t.Fatalf("parse_config was not harvested cleanly: %q", got)
	}
	if has("parse_config.") {
		t.Fatalf("harvested an identifier with its sentence period: %q", got)
	}
	// A path must still survive whole, dots included.
	if !has("src/api/users.py") {
		t.Fatalf("a path was mangled: %q", got)
	}
}
