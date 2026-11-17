package bob

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// Real-shaped ids from Bob's model route (2026-11-09). The characters are made up; the shape is not.
const (
	bobIDA = "chatcmpl-0f3c9a1e-5b7d-4e2a-9c1f-8d6b2a4e7c10" // backend A: chatcmpl-, 45 chars
	bobIDB = "nb3xQ7kLm2PzR8tYv4WcJ1s"                       // backend B: 23 chars of [A-Za-z0-9]
	// The canaries must never reach a log line.
	bobCanaryID   = "chatcmpl-CANARY0ID0abcdefghijklmnopqrstuvwxyz"
	bobCanaryText = "CANARY-ANSWER-TEXT-7731"
	// Backend B's usage block: no cache writes, no cache read, some reasoning tokens.
	bobBUsage = `{"completion_tokens":50,"completion_tokens_details":{"reasoning_tokens":30},` +
		`"prompt_tokens":12440,"prompt_tokens_details":{"cached_tokens":0,"text_tokens":12440},"total_tokens":12490}`
)

func TestBobIDShapesAreRealShaped(t *testing.T) {
	if len(bobIDA) != 45 || len(bobIDB) != 23 || len(bobCanaryID) != 45 {
		t.Fatalf("id lengths = %d %d %d, want 45 23 45", len(bobIDA), len(bobIDB), len(bobCanaryID))
	}
}

func TestBobBackendClassifier(t *testing.T) {
	a := bobMarks{id: bobIDA, usage: true, cacheWrite: true, creationDetails: true, providerFields: true}
	b := bobMarks{id: bobIDB, usage: true}
	for name, tc := range map[string]struct {
		m    bobMarks
		want string
	}{
		"A":                               {a, "A"},
		"B":                               {b, "B"},
		"A without provider fields":       {bobMarks{id: bobIDA, usage: true, cacheWrite: true}, "A"},
		"router call: empty id, no usage": {bobMarks{}, ""},
		"empty id with usage":             {bobMarks{usage: true, cacheWrite: true}, ""},
		"id with no usage block":          {bobMarks{id: bobIDA}, ""},
		// The two marks disagree.
		"chatcmpl- without cache writes":   {bobMarks{id: bobIDA, usage: true}, "other"},
		"23-char id with cache writes":     {bobMarks{id: bobIDB, usage: true, cacheWrite: true}, "other"},
		"23-char id with creation details": {bobMarks{id: bobIDB, usage: true, creationDetails: true}, "other"},
		// New shapes.
		"chatcmpl without the dash": {bobMarks{id: "chatcmpl" + bobIDA[9:], usage: true, cacheWrite: true}, "other"},
		"uppercase CHATCMPL-":       {bobMarks{id: "CHATCMPL-" + bobIDA[9:], usage: true, cacheWrite: true}, "other"},
		// Backend B ids vary in length and characters. Seen on real traffic on 2026-11-10: a 22-char
		// id, and 23-char ids with characters other than letters and digits. They are B.
		"22-char id":                      {bobMarks{id: bobIDB[:22], usage: true}, "B"},
		"24-char id":                      {bobMarks{id: bobIDB + "x", usage: true}, "B"},
		"23 chars, not alphanumeric":      {bobMarks{id: "nb3x-7kLm2PzR8tYv4WcJ_s", usage: true}, "B"},
		"23-char id with chatcmpl- start": {bobMarks{id: "chatcmpl-" + bobIDB[:14], usage: true}, "other"},
		"stream chunks with mixed ids":    {bobMarks{id: bobIDA, mixedIDs: true, usage: true, cacheWrite: true}, "other"},
		// Bob's helper calls (model "security", "router") are not the premium model: no label, no
		// alarm. On 2026-11-10 a "security" reply had a chatcmpl- id and no cache fields.
		"security helper call": {bobMarks{id: bobIDA, usage: true, helper: true}, ""},
		"premium model reply":  {bobMarks{id: bobIDA, usage: true, cacheWrite: true, helper: false}, "A"},
	} {
		if got := bobBackend(tc.m); got != tc.want {
			t.Errorf("%s: bobBackend = %q, want %q", name, got, tc.want)
		}
	}
}

// A must be the chatcmpl- id with cache writes, and B the short id without. Fails if they swap.
func TestBobBackendAAndBAreNotSwapped(t *testing.T) {
	if got := bobBackend(bobMarks{id: bobIDA, usage: true, cacheWrite: true}); got != "A" {
		t.Errorf("chatcmpl- with cache writes = %q, want A", got)
	}
	if got := bobBackend(bobMarks{id: bobIDB, usage: true}); got != "B" {
		t.Errorf("23-char id without cache writes = %q, want B", got)
	}
}

func TestBobMarksReadsJSONAndStreams(t *testing.T) {
	ct := map[bool]string{false: "application/json", true: "text/event-stream"}
	for _, sse := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[sse], func(t *testing.T) {
			_, m := bobMarksOf(ct[sse], bobReplyBytes(sse, bobIDA, coldUsage, true))
			want := bobMarks{id: bobIDA, usage: true, cacheWrite: true, creationDetails: true, providerFields: true}
			if m != want {
				t.Errorf("A marks = %+v, want %+v", m, want)
			}
			_, m = bobMarksOf(ct[sse], bobReplyBytes(sse, bobIDB, bobBUsage, false))
			if want := (bobMarks{id: bobIDB, usage: true}); m != want {
				t.Errorf("B marks = %+v, want %+v", m, want)
			}
		})
	}
	mixed := `data: {"id":"` + bobIDA + `","choices":[]}` + "\n\n" + `data: {"id":"` + bobIDB + `","choices":[],"usage":` + coldUsage + "}\n\n"
	if _, m := bobMarksOf("text/event-stream", []byte(mixed)); m.id != bobIDA || !m.mixedIDs {
		t.Errorf("mixed-id stream marks = %+v, want the first id and mixedIDs", m)
	}
	for name, body := range map[string]string{"not json": "not json", "empty": "", "array": "[1]"} {
		if _, m := bobMarksOf("application/json", []byte(body)); m != (bobMarks{}) {
			t.Errorf("%s: marks = %+v, want none", name, m)
		}
	}
}

// The WARN never logs a whole id, even a short one, and never a raw non-ASCII byte.
func TestBobBackendWarnPrefixIsNeverTheWholeID(t *testing.T) {
	for id, want := range map[string]string{
		"chatcmpl-0123456789abcdefghijklmnopqrstuvwxyz": "chat",
		"abcd": "abc",
		"ab":   "a",
		"a":    "",
	} {
		if got := bobWarnPrefix(id); got != want {
			t.Errorf("bobWarnPrefix(%q) = %q, want %q", id, got, want)
		}
	}
	// A non-ASCII id: the prefix is plain ASCII (escaped), and shorter than the id.
	got := bobWarnPrefix("é123456")
	for i := 0; i < len(got); i++ {
		if got[i] >= 0x80 {
			t.Fatalf("bobWarnPrefix logged a raw non-ASCII byte: %q", got)
		}
	}
	if got == "" || strings.Contains(got, "123456") {
		t.Fatalf("bobWarnPrefix(é123456) = %q, want a short escaped prefix", got)
	}
}

// The alarm logs once per KIND of shape, not once per id. Backend B ids are random, so keying on
// their first letters made every changed-B reply a "new" shape: 8 of them used up every slot, and a
// later, really different change logged nothing.
func TestBobBackendAlarmIsOncePerKindOfShapeNotPerID(t *testing.T) {
	logs := bobLogs(t)
	// IBM changes backend B: it now sends cache_creation_token_details. 20 replies, 20 random ids,
	// of varying length and characters, as real B ids have (22 and 23 chars, some with - or _).
	letters := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_"
	for i := 0; i < 20; i++ {
		id := make([]byte, 22+i%3)
		for j := range id {
			id[j] = letters[(i*7+j*13)%len(letters)]
		}
		bobNoteBackend(bobMarks{id: string(id), usage: true, creationDetails: true})
	}
	if n := strings.Count(logs.String(), "cg.bob_backend_unknown"); n != 1 {
		t.Fatalf("20 replies of one changed shape logged %d WARNs, want 1", n)
	}
	// A different change later still gets its own WARN: a chatcmpl- id with no cache writes (the
	// marks disagree).
	bobNoteBackend(bobMarks{id: "chatcmpl-0123456789abcdefghijklmnopqrstuvwxyz", usage: true})
	if n := strings.Count(logs.String(), "cg.bob_backend_unknown"); n != 2 {
		t.Fatalf("a second, different shape: %d WARNs in all, want 2", n)
	}
	// A mixed-id stream must be told apart from a good A reply in the log.
	bobNoteBackend(bobMarks{id: "chatcmpl-0123456789abcdefghijklmnopqrstuvwxyz", usage: true,
		cacheWrite: true, creationDetails: true, providerFields: true, mixedIDs: true})
	if !strings.Contains(logs.String(), "mixed_ids=true") {
		t.Fatalf("a mixed-id reply's WARN does not say mixed_ids=true:\n%s", logs.String())
	}
}

// Only Bob's named helper models are helpers. A coding call on any tier keeps its label, so a user
// on standard or fast is not silently left without one.
func TestBobMarksTreatOnlyTheNamedHelpersAsHelpers(t *testing.T) {
	for model, helper := range map[string]bool{
		"security": true, "router": true,
		"premium-ide": false, "premium": false, "standard": false, "standard-ide": false,
		"fast": false, "fast-ide": false, "": false,
	} {
		_, m := bobMarksOf("application/json", []byte(`{"id":"x","model":"`+model+`","usage":{"prompt_tokens":1}}`))
		if m.helper != helper {
			t.Errorf("model %q: helper = %v, want %v", model, m.helper, helper)
		}
	}
}

// bobReplyBytes is a reply from Bob's model route. psf adds the top-level provider_specific_fields
// that backend A sends. The answer text is the canary.
func bobReplyBytes(sse bool, id, usage string, psf bool) []byte {
	extra := ""
	if psf {
		extra = `,"provider_specific_fields":{}`
	}
	if !sse {
		return []byte(`{"id":"` + id + `","object":"chat.completion","model":"premium-ide","system_fingerprint":""` + extra +
			`,"choices":[{"index":0,"message":{"role":"assistant","content":"` + bobCanaryText + `"},"finish_reason":"stop"}],"usage":` + usage + `}`)
	}
	return []byte(`data: {"id":"` + id + `","object":"chat.completion.chunk","model":"premium-ide"` + extra +
		`,"choices":[{"index":0,"delta":{"role":"assistant","content":"` + bobCanaryText + `"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"` + id + `","object":"chat.completion.chunk","model":"premium-ide"` + extra +
		`,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"id":"` + id + `","object":"chat.completion.chunk","model":"premium-ide"` + extra +
		`,"choices":[],"usage":` + usage + `}` + "\n\n" +
		"data: [DONE]\n\n")
}

// bobLogs sends every log record to a buffer for the test.
func bobLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	logs := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	ResetBackendForTest()
	return logs
}
