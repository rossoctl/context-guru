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
