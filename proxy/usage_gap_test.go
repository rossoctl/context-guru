package proxy

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// #200: `ok=false` from the usage parser meant five different things and nothing distinguished
// them. Two are benign; three mean TOKEN ACCOUNTING IS OFFLINE on a request that otherwise looks
// perfect — a healthy 200, correct savings counters, correct latency, and fresh_input_tokens /
// cache_read_tokens / cache_write_tokens quietly at 0. It ran that way for 4,015 of 4,015
// requests in one benchmark iteration and was found two iterations later, in a post-mortem
// chasing a different question.
//
// The table is the classification's whole contract, and the two shapes marked with the issue's
// name are the ones that were indistinguishable from "the provider said nothing".
func TestEveryUsageOutcomeIsDistinguishable(t *testing.T) {
	const content = "SECRET-TRANSCRIPT-CONTENT"
	for _, tc := range []struct {
		name string
		ct   string
		body string
		want usageMiss
	}{
		{"anthropic snake_case", "application/json",
			`{"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":9}}`, usageMissNone},
		{"openai snake_case", "application/json",
			`{"usage":{"prompt_tokens":10,"completion_tokens":2}}`, usageMissNone},
		{"output-only delta", "application/json",
			`{"usage":{"output_tokens":7}}`, usageMissNone},

		// The gap this issue was filed for. aws/claude-* through Bedrock Converse speaks
		// camelCase, and a block that is RIGHT THERE read as "the provider reported nothing".
		{"bedrock converse camelCase", "application/json",
			`{"usage":{"inputTokens":10,"outputTokens":2},"text":"` + content + `"}`, usageMissUnparsed},
		{"nested under response", "application/json",
			`{"response":{"usage":{"input_tokens":10}}}`, usageMissUnparsed},

		// Benign, and they must NOT reach the same signal as the two above.
		{"no usage anywhere", "application/json",
			`{"content":[{"type":"text","text":"` + content + `"}],"stop_reason":"end_turn"}`, usageMissAbsent},
		{"recognised but all zero", "application/json",
			`{"usage":{"input_tokens":0,"output_tokens":0}}`, usageMissZero},
		{"empty response", "application/json", "", usageMissNoBody},

		// A THIRD failure the issue did not list, and the reason it needs its own value: over
		// sniffMax each way, sniffer.bytes returns head+"\n"+tail, which is not a whole document.
		// Reported as an unrecognised dialect it would send someone hunting for a field name that
		// is not missing — the remedy is in the sniffer, not in any parser.
		{"spliced sniffer window", "application/json",
			`{"content":[{"text":"` + content + `"` + "\n" + `"usage":{"input_tokens":10}}`, usageMissUnreadable},

		{"sse anthropic", "text/event-stream",
			"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10}}}\n", usageMissNone},
		{"sse in an unknown dialect", "text/event-stream",
			"data: {\"usage\":{\"inputTokens\":10}}\n", usageMissUnparsed},
		{"sse with no usage event", "text/event-stream",
			"data: {\"type\":\"ping\"}\n", usageMissAbsent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, why, ok := responseUsageWhy(tc.ct, []byte(tc.body))
			if why != tc.want {
				t.Fatalf("classified %v, want %v — %q", why, tc.want, tc.body)
			}
			if ok != (tc.want == usageMissNone) {
				t.Fatalf("ok=%v disagrees with the reason %v", ok, why)
			}
		})
	}
}

// The counter must not report a benign outcome and a broken promise at once — the rule
// expand/unresolved.go and #188 both state locally, and the reason this issue exists at all. So:
// the two alertable cases move their own counter and nothing else, and the three benign ones move
// neither.
func TestOnlyAnAccountingOutageMovesTheUsageCounters(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		ct, body              string
		wantUnp, wantUnreadab int64
	}{
		{"an unrecognised dialect", "application/json", `{"usage":{"inputTokens":10}}`, 1, 0},
		// The window has to be spliced in a way that actually HIDES the block: gjson scans rather
		// than walking, so a truncated document whose top-level `usage` survives is still read
		// correctly and is no problem at all. That is why this case is narrower than "the bytes
		// were truncated" — see the same shape in TestEveryUsageOutcomeIsDistinguishable.
		{"an unreadable window", "application/json",
			`{"content":[{"text":"x"` + "\n" + `"usage":{"input_tokens":1}}`, 0, 1},
		{"a provider that said nothing", "application/json", `{"stop_reason":"end_turn"}`, 0, 0},
		{"a recognised all-zero block", "application/json", `{"usage":{"input_tokens":0}}`, 0, 0},
		{"an empty body", "application/json", "", 0, 0},
		{"a healthy response", "application/json", `{"usage":{"input_tokens":10}}`, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unpBefore, unrBefore := UsageGaps()
			responseUsageWhy(tc.ct, []byte(tc.body))
			unp, unr := UsageGaps()
			if got := unp - unpBefore; got != tc.wantUnp {
				t.Errorf("usage_unparsed moved by %d, want %d: the counter whose whole meaning is "+
					"'a dialect is missing' is being driven by something else", got, tc.wantUnp)
			}
			if got := unr - unrBefore; got != tc.wantUnreadab {
				t.Errorf("usage_unreadable moved by %d, want %d", got, tc.wantUnreadab)
			}
		})
	}
}

// The shape record is what makes the dialect fix possible WITHOUT a captured body: it turns
// "usage_reported is false" into "the provider is sending camelCase". Two properties, and the
// second is the one that lets it ship at all.
func TestTheShapeRecordNamesTheDialectAndCarriesNoContent(t *testing.T) {
	const content = "SECRET-TRANSCRIPT-CONTENT-THAT-MUST-NOT-BE-LOGGED"
	body := `{"id":"msg_1","type":"message","content":[{"type":"text","text":"` + content +
		`"}],"usage":{"inputTokens":10,"outputTokens":2,"cacheReadInputTokens":9}}`

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	ResetUsageShapeRecordForTest()

	responseUsageWhy("application/json", []byte(body))
	got := buf.String()

	// It names WHERE the block is and WHAT the provider calls the fields — the answer the dialect
	// fix has been blocked on.
	for _, want := range []string{"cg.usage_unaccounted", "usage_at=usage", "inputTokens",
		"cacheReadInputTokens", "valid_json=true"} {
		if !strings.Contains(got, want) {
			t.Errorf("the record does not contain %q, so it does not answer which dialect this is:\n%s",
				want, got)
		}
	}
	// KEY NAMES ONLY. A body dump on this workload writes kilobytes of transcript to disk per
	// response; this record cannot, by construction, and that is why it needs no capture rig and
	// no operator's permission to turn on.
	if strings.Contains(got, content) {
		t.Fatalf("the shape record leaked response CONTENT, which is the one thing it must never "+
			"do — a body dump was rejected for exactly this reason:\n%s", got)
	}
	// Values are not logged either, only names: 10 and 9 are token counts, but a value here is a
	// precedent for logging the next one, which will be text.
	for _, leak := range []string{"inputTokens:10", `"inputTokens":10`} {
		if strings.Contains(got, leak) {
			t.Errorf("the record carries a VALUE (%q), not just key names:\n%s", leak, got)
		}
	}

	// One per process. A run with this gap has it on EVERY request — 4,015 of 4,015 in the
	// iteration that motivated this — so a per-response record is a log line per request.
	buf.Reset()
	responseUsageWhy("application/json", []byte(body))
	if strings.Contains(buf.String(), "cg.usage_unaccounted") {
		t.Fatalf("a second unaccounted response logged a second shape record:\n%s", buf.String())
	}
}

// The record must be useful for the SPLICED-WINDOW case too, and must say so — that is the
// distinction between "add a dialect" and "stop truncating", which is the whole reason the two
// counters are separate.
func TestTheShapeRecordSaysWhenTheBytesWereNotAWholeDocument(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	ResetUsageShapeRecordForTest()

	// The splice has to hide the block, or it is simply read correctly — see the note in
	// TestOnlyAnAccountingOutageMovesTheUsageCounters.
	responseUsageWhy("application/json",
		[]byte(`{"content":[{"text":"x"`+"\n"+`"usage":{"input_tokens":10}}`))
	got := buf.String()
	if !strings.Contains(got, "valid_json=false") {
		t.Fatalf("the record does not say the bytes would not parse, so it reads as a dialect "+
			"problem when the fix is in the sniffer:\n%s", got)
	}
}
