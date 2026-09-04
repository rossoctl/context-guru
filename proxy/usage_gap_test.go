package proxy

import (
	"bytes"
	"log/slog"
	"strconv"
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

		// The transport axis was SAMPLED here rather than covered: usageMissZero had no streamed
		// row, so the one value that is benign-but-block-bearing was untested on the transport
		// where it was misclassified as a dialect gap. Same for a null field, which is what
		// OpenAI-dialect streaming sends on every chunk without stream_options.include_usage.
		{"sse recognised but all zero", "text/event-stream",
			"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":0}}\n", usageMissZero},
		{"sse with a null usage field", "text/event-stream",
			"data: {\"choices\":[{\"delta\":{}}],\"usage\":null}\n", usageMissAbsent},
		{"sse with an empty usage object", "text/event-stream",
			"data: {\"usage\":{}}\n", usageMissAbsent},
		{"a null usage field", "application/json", `{"usage":null}`, usageMissAbsent},
		{"an empty usage object", "application/json", `{"usage":{}}`, usageMissAbsent},
		// A stream that says nothing readable in one event and the real tiers in another must take
		// the GOOD outcome, not the worst — `worst` is only the answer when nothing parsed.
		{"sse null chunk then real usage", "text/event-stream",
			"data: {\"choices\":[{\"delta\":{}}],\"usage\":null}\n" +
				"data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n", usageMissNone},
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

		// THE TRANSPORT AXIS, which this table sampled rather than covered. Every row above is
		// application/json, so the rule was asserted only where parseUsageWhy runs and never where
		// parseSSEUsageWhy does — and the stream path is the half that broke it.
		//
		// `usage: null` is not a hypothetical shape. gjson's Exists() is `Type != Null ||
		// len(Raw) != 0`, and a JSON null has Raw == "null", so a null field looked present; and
		// OpenAI-dialect streaming sends exactly `"usage": null` on every chunk unless the caller
		// sets stream_options.include_usage. So this row is ordinary healthy traffic on the
		// openai_upstream route, and it was moving the counter that means "add a dialect".
		{"a streamed null usage field", "text/event-stream",
			"data: {\"choices\":[{\"delta\":{}}],\"usage\":null}\n", 0, 0},
		{"a streamed all-zero block", "text/event-stream",
			"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":0}}\n", 0, 0},
		{"a streamed empty usage object", "text/event-stream", "data: {\"usage\":{}}\n", 0, 0},
		{"a non-streamed null usage field", "application/json", `{"usage":null}`, 0, 0},
		{"a non-streamed empty usage object", "application/json", `{"usage":{}}`, 0, 0},
		// And the positive control for the transport, so "no benign row moves it" cannot pass by
		// the classifier having stopped counting on this path altogether.
		{"a streamed unrecognised dialect", "text/event-stream",
			"data: {\"usage\":{\"inputTokens\":10}}\n", 1, 0},
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

// ONE EARLIER RESPONSE MUST NOT SPEND THE DIAGNOSTIC.
//
// The record is the part of this change that unblocks the dialect fix, and it was gated on a single
// process-wide sync.Once shared by both alertable classes — so whichever response came first
// consumed it. A spliced window followed by the camelCase response produced a record about the
// window and left the dialect question, the one the record exists to answer, unanswered.
//
// Worse in combination with the null-usage defect that shared this review: a benign OpenAI-dialect
// chunk was alertable, so on any deployment carrying streamed OpenAI traffic the record was almost
// certain to be spent on it before an interesting response ever arrived.
func TestAnEarlierShapeDoesNotSpendTheRecordForALaterDialect(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	ResetUsageShapeRecordForTest()

	// First: a spliced window, which is alertable but says nothing about any dialect.
	responseUsageWhy("application/json",
		[]byte(`{"content":[{"text":"x"`+"\n"+`"usage":{"input_tokens":10}}`))
	// Then: the response whose shape somebody actually needs.
	responseUsageWhy("application/json",
		[]byte(`{"id":"msg_1","usage":{"inputTokens":10,"cacheReadInputTokens":9}}`))

	got := buf.String()
	if !strings.Contains(got, "inputTokens") || !strings.Contains(got, "cacheReadInputTokens") {
		t.Fatalf("the camelCase shape was never recorded — an earlier unrelated response spent "+
			"the one record, so the dialect question this record exists to answer is still open:\n%s", got)
	}

	// And still bounded: the SAME shape again writes nothing, so a run with the gap on every
	// request produces one line, not one per request.
	buf.Reset()
	responseUsageWhy("application/json",
		[]byte(`{"id":"msg_2","usage":{"inputTokens":11,"cacheReadInputTokens":8}}`))
	if strings.Contains(buf.String(), "cg.usage_unaccounted") {
		t.Fatalf("a second response of an ALREADY RECORDED shape logged again; a run with this gap "+
			"on 4,015 of 4,015 requests would write 4,015 lines:\n%s", buf.String())
	}

	// The bound is on distinct shapes, not on responses, and it holds.
	buf.Reset()
	for i := 0; i < usageShapeMax+4; i++ {
		responseUsageWhy("application/json",
			[]byte(`{"usage":{"tokensVariant`+strconv.Itoa(i)+`":1}}`))
	}
	if n := strings.Count(buf.String(), "cg.usage_unaccounted"); n > usageShapeMax {
		t.Fatalf("recorded %d shapes with a cap of %d: a provider varying its key names could "+
			"turn the diagnostic into a line per request", n, usageShapeMax)
	}
}

// The log line's two halves must not contradict each other across expand rounds. usageOK is
// sticky-true — a request whose usage was read once IS accounted — while the reason was
// last-write-wins, so a round 2 continuation carrying no usage relabelled an accounted request
// `absent`, and `usage_reported=true usage_miss=absent` is a line that undoes the reason for
// having the reason.
func TestTheMissReasonNeverContradictsUsageReported(t *testing.T) {
	// Accounted by any round => parsed, whatever a later round found.
	if got := noteUsageMiss(usageMissNone, usageMissAbsent, true); got != usageMissNone {
		t.Errorf("an accounted request reported %v; usage_reported=true with a miss reason is a "+
			"self-contradicting log line", got)
	}
	if got := noteUsageMiss(usageMissUnparsed, usageMissAbsent, true); got != usageMissNone {
		t.Errorf("a request accounted by a later round still reported %v", got)
	}
	// Unaccounted => keep the WORST reason seen, so a benign later round cannot mask a dialect gap
	// an earlier one found.
	if got := noteUsageMiss(usageMissUnparsed, usageMissAbsent, false); got != usageMissUnparsed {
		t.Errorf("reported %v, want unparsed_dialect: a later benign round masked the round that "+
			"found a dialect gap", got)
	}
	if got := noteUsageMiss(usageMissAbsent, usageMissUnparsed, false); got != usageMissUnparsed {
		t.Errorf("reported %v, want unparsed_dialect", got)
	}
}
