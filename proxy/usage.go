package proxy

import (
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/tidwall/gjson"
)

// Usage is one response's provider-billed token tiers. ok=false means the
// provider told us nothing, in which case a caller must report the request as
// partially accounted rather than pricing it as free.
type Usage struct {
	FreshInput int64
	CacheRead  int64
	CacheWrite int64
	Output     int64
	// CacheWrite1h is the part of CacheWrite the provider billed at the ONE-HOUR write
	// tier, from `usage.cache_creation.ephemeral_1h_input_tokens`. Zero on every response
	// that wrote only 5-minute entries, which on this deployment is all of them.
	//
	// It is here for two reasons, and both are about not being lied to. A 1h write costs
	// 2.0x base input where a 5m write costs 1.25x, so pricing a 1h write as a 5m one
	// understates that request by 0.75x of its whole written prefix. And it is the ONLY
	// signal that a requested `ttl: "1h"` was HONOURED. A provider that does not support the
	// tier for that model downgrades it silently: a normal 200 with a normal-looking
	// `cache_creation_input_tokens` and nothing to distinguish it from a granted 1h entry.
	// Measured live on this gateway: aws/claude-haiku-4-5 returned 36,251 of 36,574 written
	// tokens at the 1h tier, aws/claude-sonnet-5 returned 0 of 48,212. Absent-or-zero here,
	// with a 1h breakpoint on the wire, means the request was downgraded.
	CacheWrite1h int64
	// StopReason is the provider's terminal reason for this response, normalized across
	// dialects (see responseStopReason). It rides on Usage because it comes off the same
	// buffered/sniffed response bytes and reaches the dashboard by the same path — a
	// second out-of-band channel for one string would be a second thing to keep in sync.
	//
	// It is filled even when the token tiers are absent, because the two facts are
	// independent: an OpenAI response without `stream_options` reports no usage at all and
	// still reports finish_reason, and "we could not price this, and it stopped because it
	// hit max_tokens" is exactly the pair worth having.
	StopReason string
}

// usageMiss says WHY no billed tiers came back, because "ok=false" meant five different things
// and nothing distinguished them (#200). Only two of the five are benign, and the other three
// call for THREE DIFFERENT REMEDIES — which is the whole argument for classifying rather than
// counting: a shared signal reports "nothing to account" and "token accounting is offline" with
// one value, and the second is silent by construction (a healthy 200, correct savings counters,
// correct latency, and three token fields quietly at 0).
//
// It ran for 4,015 of 4,015 requests in one benchmark iteration and was found two iterations
// later, in a post-mortem chasing a different question.
type usageMiss uint8

// THE ORDER IS PART OF THE TYPE, ascending in severity: parseSSEUsageWhy compares two reasons
// with `>` to keep the worst one any event of a stream produced. A new value goes at its severity
// position, not at the end.
const (
	usageMissNone       usageMiss = iota // tiers were read; not a miss
	usageMissNoBody                      // nothing to look at (empty response). BENIGN
	usageMissAbsent                      // no usage block anywhere we know to look. BENIGN
	usageMissZero                        // a recognised block, every tier zero. LEGITIMATE
	usageMissUnparsed                    // a block IS there, no recognised spelling read it
	usageMissUnreadable                  // the bytes are not parseable, so nothing could be sought
)

// String is the reason as it appears in the lifecycle log line, so an operator reading
// `usage_reported=false` can see which of the five it was without a body dump.
func (m usageMiss) String() string {
	switch m {
	case usageMissNone:
		return "parsed"
	case usageMissNoBody:
		return "no_body"
	case usageMissAbsent:
		return "absent"
	case usageMissZero:
		return "all_zero"
	case usageMissUnparsed:
		return "unparsed_dialect"
	case usageMissUnreadable:
		return "unreadable_body"
	}
	return "unknown"
}

// alertable reports whether this miss means TOKEN ACCOUNTING IS OFFLINE, as opposed to a
// provider that legitimately said nothing. The two benign cases must never share a counter with
// these, or the number an operator watches to confirm accounting is healthy is incremented by it
// being broken.
func (m usageMiss) alertable() bool {
	return m == usageMissUnparsed || m == usageMissUnreadable
}

// nestedUsagePaths are places a usage block is known to live OTHER than the top level. They are
// probed for EXISTENCE ONLY — never read — which is what keeps this dialect-agnostic: knowing
// that a block is there is enough to say the parser has a gap, and guessing at the field names
// inside would produce a parser that looks correct and reads zero, which is the failure this
// classification exists to expose, reproduced in the code meant to report it.
var nestedUsagePaths = [...]string{"response.usage", "usageMetadata", "data.usage", "message.usage"}

// usagePresent reports whether a usage block is actually THERE — an object carrying at least one
// field — as opposed to merely resolving.
//
// gjson's Exists() is `Type != Null || len(Raw) != 0`, and a JSON null has Raw == "null", so
// `"usage": null` LOOKED like a present block that no recognised spelling could read: classified
// `unparsed_dialect`, counted as a measurement outage. OpenAI-dialect streaming sends exactly
// `"usage": null` on every chunk unless the caller sets stream_options.include_usage, so on the
// openai_upstream route the counter documented as "add the dialect the provider is speaking" was
// incremented by ordinary healthy traffic.
//
// That is #200's own defect with the sign flipped — instead of an outage reading as healthy,
// healthy traffic reads as an outage — and it costs the same thing: an operator who cannot tell
// from the counter which one they have. `{}` is treated the same way for the same reason: a
// provider that sent an empty object told us nothing, which is `absent`, not a missing spelling.
func usagePresent(v gjson.Result) bool {
	if !v.IsObject() {
		return false
	}
	empty := true
	v.ForEach(func(gjson.Result, gjson.Result) bool {
		empty = false
		return false // one field is enough
	})
	return !empty
}

// parseUsage extracts the four billed token tiers from a buffered response body,
// in whichever dialect it is written. This is the number that actually matters on
// this workload — the request is ~99.95% cached and a cache write bills ~11.5x a
// read, so content-token savings alone cannot express the economics.
//
// Dialects handled:
//
//	Anthropic  usage.{input_tokens, output_tokens,
//	           cache_read_input_tokens, cache_creation_input_tokens}
//	OpenAI     usage.{prompt_tokens, completion_tokens,
//	           prompt_tokens_details.cached_tokens}
//
// Anthropic's `input_tokens` already EXCLUDES the cached tiers, so it is the fresh
// figure directly. OpenAI's `prompt_tokens` INCLUDES its cached_tokens, so fresh is
// the difference — getting this backwards double-counts the whole transcript on
// every turn, which is exactly the kind of error a "savings" number hides.
func parseUsage(body []byte) (Usage, bool) {
	u, why := parseUsageWhy(body)
	return u, why == usageMissNone
}

// parseUsageWhy is parseUsage plus the classification. Split out rather than folded in because
// parseSSEUsage calls the boolean form per event and must not have its skips classified — only
// responseUsage, which sees the whole response, is in a position to say anything about it.
func parseUsageWhy(body []byte) (Usage, usageMiss) {
	u := gjson.GetBytes(body, "usage")
	if !usagePresent(u) {
		// A block SOMEWHERE ELSE is a parser gap; nothing anywhere is a provider that said
		// nothing. Distinguishing them is the point: the first is a measurement outage whose fix
		// is a dialect, the second needs no action at all.
		//
		// PRECEDENCE, and it is chosen rather than accidental: a found block wins over an
		// unparseable document. These probes run BEFORE ValidBytes below, so a spliced window that
		// hid the top-level `usage` while leaving `response.usage` scannable is reported as a
		// dialect gap, not as unreadable bytes. That is the right way round — a block we DID find
		// really does mean a spelling is missing, whatever else is wrong with the bytes — and
		// `valid_json` in the shape record tells the reader the document was also truncated. Do
		// not "fix" this by testing ValidBytes first: that would hide a real dialect behind a
		// transport problem.
		for _, p := range nestedUsagePaths {
			if usagePresent(gjson.GetBytes(body, p)) {
				return Usage{}, usageMissUnparsed
			}
		}
		// And bytes we cannot parse are a THIRD thing, with its own remedy: nothing could be
		// looked for, so "no usage block" is not a finding about the provider. This is what a
		// spliced sniffer window looks like — head+"\n"+tail is not valid JSON — so reporting it
		// as an unrecognised dialect would send someone hunting for a field-name gap that is not
		// there. See sniffer.bytes.
		//
		// Reachable only from the SNIFFED path — Handler.stream, taken when neither proxy-injected
		// tool is advertised. With one advertised (inject_expand: always makes that true from the
		// first turn) a non-streamed response is read whole and this cannot fire, which is worth
		// knowing before treating it as a competing explanation for a miss: that mistake has been
		// made twice, in both directions, from reading the call sites out of order.
		//
		// Narrower than "the response was truncated", and deliberately so: gjson SCANS rather
		// than walking a tree, so a spliced document whose top-level `usage` survived is read
		// correctly above and never reaches here. This fires only when the splice actually hid
		// the block, which is the only case where anything is lost.
		if !gjson.ValidBytes(body) {
			return Usage{}, usageMissUnreadable
		}
		return Usage{}, usageMissAbsent
	}
	var out Usage
	switch {
	case u.Get("input_tokens").Exists(): // Anthropic
		out.FreshInput = u.Get("input_tokens").Int()
		out.Output = u.Get("output_tokens").Int()
		out.CacheRead = u.Get("cache_read_input_tokens").Int()
		out.CacheWrite = u.Get("cache_creation_input_tokens").Int()
		// The per-TTL split, when the provider reports one. `cache_creation_input_tokens`
		// is the total across both tiers, so this is a SUBSET of CacheWrite and never an
		// addition to it.
		out.CacheWrite1h = u.Get("cache_creation.ephemeral_1h_input_tokens").Int()
	case u.Get("prompt_tokens").Exists(): // OpenAI
		prompt := u.Get("prompt_tokens").Int()
		out.Output = u.Get("completion_tokens").Int()
		out.CacheRead = u.Get("prompt_tokens_details.cached_tokens").Int()
		out.FreshInput = prompt - out.CacheRead
		if out.FreshInput < 0 {
			out.FreshInput = 0
		}
	case u.Get("output_tokens").Exists():
		// An OUTPUT-ONLY block. Anthropic's streaming message_delta carries exactly
		// this, and it holds the FINAL completion count — treating it as "no usage"
		// under-reports every streamed response's output tokens.
		out.Output = u.Get("output_tokens").Int()
	default:
		// A usage block IS here and this parser read nothing out of it — the Bedrock Converse
		// camelCase shape (`usage.inputTokens`) is the known instance. The DIALECT is deliberately
		// not added here: getting the field names wrong (`cacheWriteInputTokens` vs
		// `cacheCreationInputTokens`) yields a parser that looks correct and reads zero. What
		// closes that gap is the shape record in responseUsage, which needs no guess.
		return Usage{}, usageMissUnparsed
	}
	if out.FreshInput|out.CacheRead|out.CacheWrite|out.Output == 0 {
		// Recognised and genuinely zero, which is legitimate on some responses. Kept out of the
		// unparsed counter, or every such response would inflate the one number that means a
		// dialect is missing.
		return Usage{}, usageMissZero
	}
	return out, usageMissNone
}

// parseSSEUsage pulls usage out of a streamed response. Both dialects report it in
// terminal events (Anthropic: message_start carries the input tiers and
// message_delta the output; OpenAI: a final chunk with `usage`), so the tiers are
// merged across events, taking the maximum of each — a later event repeating a
// value must not double it.
func parseSSEUsage(raw []byte) (Usage, bool) {
	u, why, _ := parseSSEUsageWhy(raw)
	return u, why == usageMissNone
}

// parseSSEUsageWhy is parseSSEUsage plus #200's classification. An event stream that carried a
// usage block no event of it could be read is the streamed form of the dialect gap, and it must
// not be reported as "the provider streamed no usage" — the remedies differ.
// The third return is the PAYLOAD OF THE EVENT THAT SET THE REASON, for the shape record.
//
// The record used to describe the stream's LAST data: event, which on the Anthropic-family
// transport is `message_stop` — usage arrives in `message_start`. So a streamed unrecognised
// dialect produced a record of the terminal event: no usage_at, no usage_keys, nothing about the
// dialect, and it consumed one of the bounded record slots to say it. That is exactly the shape a
// streamed Bedrock aws/claude-* response has, which is where the gap this record exists to
// diagnose would sit. Returning it from here also collapses two walks over the transport into one.
func parseSSEUsageWhy(raw []byte) (Usage, usageMiss, string) {
	var out Usage
	found := false
	// The WORST reason any event produced, which is what makes a streamed miss as precise as a
	// buffered one. The predecessor kept a bare sawBlock flag and reported every rejected block as
	// a dialect gap, so a streamed all-zero block — legitimate — read as an outage. Per-event
	// parseUsageWhy costs nothing extra (the boolean form called it anyway) and each event's own
	// reason is already exact.
	// THE TWO PARSERS ARE NOT SYMMETRIC, and the asymmetry is here: this one can never return
	// usageMissUnreadable. It only calls parseUsageWhy with a block usagePresent already accepted,
	// and that function reaches its ValidBytes branch only when no block was found anywhere — so
	// `worst` ranges over absent/all_zero/unparsed_dialect and nothing else. A `data:` line cut
	// mid-JSON yields no present block, so the stream reads as "the provider streamed no usage".
	//
	// That leaves usage_unreadable reachable for a sniffed JSON response and structurally
	// unreachable for a sniffed STREAM, which is where a head+tail window would hide a block —
	// #200's own defect, in the corner this classification does not cover. Narrow in practice:
	// Anthropic puts input_tokens in message_start at the front of the head window, so `found` is
	// true and the answer is `parsed`. Closing it properly means passing the sniffer's own
	// knowledge that it spliced (s.total > len(s.head)) into responseUsageWhy, which would also
	// make the buffered case exact instead of inferred from ValidBytes. Filed rather than guessed
	// at here.
	worst := usageMissAbsent
	worstPayload := "" // the event `worst` came from, so the record describes the right one
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		// Anthropic nests the first usage under message.usage; OpenAI puts it at the top.
		for _, path := range []string{"usage", "message.usage"} {
			u := gjson.Get(payload, path)
			// usagePresent, not Exists: `"usage": null` on every chunk is what OpenAI-dialect
			// streaming sends absent stream_options.include_usage, and it is not a block.
			if !usagePresent(u) {
				continue
			}
			one, why := parseUsageWhy([]byte(`{"usage":` + u.Raw + `}`))
			if why > worst {
				worst = why // usageMiss is ordered benign -> alertable; see its constants
				worstPayload = payload
			}
			if why != usageMissNone {
				continue
			}
			found = true
			out.FreshInput = max64(out.FreshInput, one.FreshInput)
			out.CacheRead = max64(out.CacheRead, one.CacheRead)
			out.CacheWrite = max64(out.CacheWrite, one.CacheWrite)
			out.CacheWrite1h = max64(out.CacheWrite1h, one.CacheWrite1h)
			out.Output = max64(out.Output, one.Output)
		}
	}
	if found {
		return out, usageMissNone, ""
	}
	// Every block that appeared was rejected, and `worst` says why — an unrecognised spelling is
	// alertable, an all-zero block is not, and no block at all is `absent` because that is what
	// `worst` starts as.
	return out, worst, worstPayload
}

// responseUsage picks the right parser for a response's content type. The returned
// Usage carries the stop reason whatever `ok` says — see Usage.StopReason.
func responseUsage(contentType string, body []byte) (Usage, bool) {
	u, _, ok := responseUsageWhy(contentType, body)
	return u, ok
}

// responseUsageWhy is responseUsage plus the reason, and it is the ONE place a miss is counted
// and recorded — the whole response is in scope here, which is what it takes to say anything
// about it (parseUsage is also called per-event by the SSE path, where a skip means nothing).
//
// The reason reaches the lifecycle log line beside `usage_reported`, so a deployment that has
// quietly stopped accounting tokens says which of the five cases it is at the moment it happens,
// instead of being reconstructed two iterations later from a cost figure.
func responseUsageWhy(contentType string, body []byte) (Usage, usageMiss, bool) {
	if len(body) == 0 {
		return Usage{}, usageMissNoBody, false
	}
	sse := strings.Contains(contentType, "event-stream")
	var u Usage
	var why usageMiss
	// doc is the JSON document the shape record should DESCRIBE, which for a stream is the one
	// event that produced the reason rather than the whole transport or its last event.
	doc := body
	if sse {
		var judged string
		u, why, judged = parseSSEUsageWhy(body)
		doc = []byte(judged)
	} else {
		u, why = parseUsageWhy(body)
	}
	u.StopReason = responseStopReason(sse, body)
	if why.alertable() {
		switch why {
		case usageMissUnparsed:
			usageUnparsed.Add(1)
		case usageMissUnreadable:
			usageUnreadable.Add(1)
		}
		// len(body) so the record still reports the whole response's size — that is what says a
		// window was spliced — while describing the document that actually failed to parse.
		recordUsageShape(sse, len(body), doc)
	}
	return u, why, why == usageMissNone
}

// noteUsageMiss folds one round's reason into the request's, so `usage_miss` cannot contradict
// `usage_reported` on the log line.
//
// A request can drive several upstream rounds (the expand continuation loop). usageOK is sticky —
// it is only ever set true and never reset, because a request whose usage was read once IS
// accounted — but the reason was last-write-wins, so round 2 carrying no usage relabelled an
// accounted request `absent`. The rule that removes the contradiction: once anything accounted the
// request, the reason is `parsed`; while nothing has, keep the WORST reason seen, for the same
// severity-ordering reason parseSSEUsageWhy keeps one.
func noteUsageMiss(sofar, this usageMiss, accounted bool) usageMiss {
	if accounted {
		return usageMissNone
	}
	if this > sofar {
		return this
	}
	return sofar
}

// The two alertable misses, counted apart because their REMEDIES are opposite: `unparsed` means
// add the dialect the provider is speaking, `unreadable` means the bytes we looked at were not a
// whole document (a spliced sniffer window) and the fix is upstream of any parser. A single
// counter would have said "token accounting is offline" without saying which half of the stack
// to look at.
//
// Neither counts the two benign cases. Every operator-facing description of them would otherwise
// make a promise the dangerous case violates — the same reason #188 split stash_refused from
// stash_missing, and expand/unresolved.go splits malformed from missing.
var (
	usageUnparsed   atomic.Int64
	usageUnreadable atomic.Int64
)

// UsageGaps returns how many responses carried usage this proxy could not read (an unrecognised
// dialect) and how many carried bytes it could not parse at all. Non-zero on either means
// fresh_input_tokens / cache_read_tokens / cache_write_tokens are 0 for some route or provider
// while everything else about the request looks healthy.
//
// RESPONSES, not requests, and the distinction matters when reading them against traffic: one
// request can drive several upstream rounds through the expand continuation loop, so a single
// unaccounted request can move these more than once. The 4,015-of-4,015 figure this was filed
// against is a per-REQUEST count, so it is not directly comparable.
func UsageGaps() (unparsed, unreadable int64) {
	return usageUnparsed.Load(), usageUnreadable.Load()
}

// One record per DISTINCT SHAPE, bounded — not one per process.
//
// A single process-wide sync.Once was wrong in a way that defeated the diagnostic: both alertable
// classes shared it, so whichever response came first spent it. A spliced window followed by the
// camelCase response produced a record about the window and left the dialect question unanswered —
// and the dialect question is what this record exists for. A multi-provider deployment has more
// than one answer, so "one line per process" was the wrong quantity as well as the wrong one.
//
// Keyed on the record's own identity (where the block was, plus its sorted key names), which is
// exactly the granularity that distinguishes two dialects while collapsing a run that has the same
// gap on every request: 4,015 of 4,015 identical responses still produce ONE line. Capped so a
// pathological provider varying its keys cannot turn this into a line per request.
const usageShapeMax = 8

var (
	usageShapeMu   sync.Mutex
	usageShapeSeen = map[string]struct{}{}
)

// noteUsageShape reports whether this shape has not been recorded before and there is room for it.
// Under the mutex so the reset below is not a data race — the predecessor's
// `usageShapeOnce = sync.Once{}` raced any concurrent Do, which -race would have found the moment
// one of those tests was marked t.Parallel().
func noteUsageShape(key string) bool {
	usageShapeMu.Lock()
	defer usageShapeMu.Unlock()
	if _, dup := usageShapeSeen[key]; dup || len(usageShapeSeen) >= usageShapeMax {
		return false
	}
	usageShapeSeen[key] = struct{}{}
	return true
}

// recordUsageShape logs the response's SHAPE — the top-level key names, plus the key names under
// any usage block found — and nothing else.
//
// KEY NAMES ONLY, NEVER VALUES AND NEVER THE BODY. A body dump on this workload writes several
// kilobytes of transcript content to disk per response; a key-shape record cannot, by
// construction. And it is the datum that actually answers the question: it turns "usage_reported
// is false" into "the provider is sending camelCase", which is the difference between an open
// question and a one-line fix — without needing a captured body, an instrumented capture hop, or
// any extra request.
func recordUsageShape(sse bool, total int, doc []byte) {
	attrs := usageShapeAttrs(sse, total, doc)
	if !noteUsageShape(usageShapeKey(attrs)) {
		return
	}
	slog.Default().Debug("cg.usage_unaccounted", attrs...)
}

// usageShapeKey identifies a shape by WHERE the block was and what its fields are called — the two
// things that differ between dialects. Deliberately not the top-level keys: those vary with the
// response's content (an Anthropic body with a tool_use block has different top-level keys from one
// without), so keying on them would let one dialect burn the whole budget.
func usageShapeKey(attrs []any) string {
	var at, keys string
	for i := 0; i+1 < len(attrs); i += 2 {
		switch attrs[i] {
		case "usage_at":
			at, _ = attrs[i+1].(string)
		case "usage_keys":
			if ks, ok := attrs[i+1].([]string); ok {
				keys = strings.Join(ks, ",")
			}
		}
	}
	// No block found at all (the unreadable case) still deserves exactly one line, so it gets a
	// stable key of its own rather than colliding with a real dialect.
	return at + "|" + keys
}

// ResetUsageShapeRecordForTest forgets the shapes recorded so far. For TESTS only: the record is
// the diagnostic this whole change exists to produce, so a test has to be able to observe it, and
// the per-process bound makes that a one-shot for the entire binary otherwise.
func ResetUsageShapeRecordForTest() {
	usageShapeMu.Lock()
	defer usageShapeMu.Unlock()
	usageShapeSeen = map[string]struct{}{}
}

// usageShapeAttrs builds the shape record's key/value pairs. Separated from the logging so a test
// can assert what it does and — more to the point — what it does NOT contain.
//
// doc is the JSON document that failed to parse: the whole body for a buffered response, and for a
// stream the ONE EVENT the classifier judged (see parseSSEUsageWhy) rather than the transport or its
// last event. total is the whole response's size, which is the figure that says a window was
// spliced.
func usageShapeAttrs(sse bool, total int, doc []byte) []any {
	attrs := []any{"sse", sse, "bytes", total,
		// Not parseable means the window was spliced rather than the dialect being strange —
		// stated in the record so it cannot be misread as a field-name gap.
		"valid_json", gjson.ValidBytes(doc),
		"top_level_keys", objectKeys(gjson.ParseBytes(doc))}
	for _, p := range append([]string{"usage"}, nestedUsagePaths[:]...) {
		// usagePresent, not Exists, and for the same reason the classifier uses it: a null or empty
		// block resolves but names no fields, so the record pointed at `usage` and printed
		// usage_keys=[] while a real nested block held the answer — and keyed as `usage|`, which
		// collides with a genuinely empty top-level block. The record must look where the
		// CLASSIFIER looked.
		if u := gjson.GetBytes(doc, p); usagePresent(u) {
			return append(attrs, "usage_at", p, "usage_keys", objectKeys(u))
		}
	}
	return attrs
}

// objectKeys lists a JSON object's immediate key NAMES, sorted so two records compare, and
// bounded so a pathological object cannot write an unbounded log line. Values are never touched.
func objectKeys(v gjson.Result) []string {
	if !v.IsObject() {
		return nil
	}
	var keys []string
	v.ForEach(func(k, _ gjson.Result) bool {
		keys = append(keys, k.String())
		return len(keys) < 64
	})
	sort.Strings(keys)
	return keys
}

// stopReasonPaths are where a terminal reason lives, per dialect and per transport:
//
//	Anthropic  stop_reason              (non-streaming)
//	           delta.stop_reason        (message_delta, the streamed terminal event)
//	OpenAI     choices.0.finish_reason  (both; the streamed final chunk carries it too)
//
// The values are the providers' own vocabulary and are recorded verbatim rather than
// mapped onto one another: `end_turn` and `stop` mean the same thing, but flattening them
// would quietly assert that `pause_turn`, `refusal` and `content_filter` have equivalents
// on the other side, and they do not.
var stopReasonPaths = [...]string{"stop_reason", "delta.stop_reason", "choices.0.finish_reason"}

// responseStopReason reads the terminal reason off a response body. For SSE the events
// are scanned newest-first: the terminal reason is in the LAST event that carries one, and
// an earlier chunk's `finish_reason: null` must not win.
//
// Whatever this returns is still client-influenced text by the time it reaches the
// database (an upstream is configurable, so its response is not trusted input), so it is
// re-checked by dash.metaEnum before the insert.
func responseStopReason(sse bool, body []byte) string {
	if !sse {
		return firstStopReason(string(body))
	}
	lines := strings.Split(string(body), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if s := firstStopReason(payload); s != "" {
			return s
		}
	}
	return ""
}

// firstStopReason returns the first non-empty stop reason among the known paths.
func firstStopReason(payload string) string {
	for _, p := range stopReasonPaths {
		if r := gjson.Get(payload, p); r.Type == gjson.String && r.Str != "" {
			return r.Str
		}
	}
	return ""
}

// sniffMax bounds each half of the sniffer's window. Usage blocks are a few
// hundred bytes; 64 KiB each way is generous and hard-caps the memory an
// adversarially long response can make us hold per in-flight request.
const sniffMax = 64 << 10

// sniffer keeps a bounded head+tail window of a streamed response so its usage
// block can be read after the stream completes, without buffering the response.
// A disabled sniffer allocates nothing and does no work.
type sniffer struct {
	on    bool
	head  []byte
	tail  []byte
	total int // bytes written, so bytes() knows whether the head alone is the whole body
}

func newSniffer(on bool) *sniffer { return &sniffer{on: on} }

func (s *sniffer) write(p []byte) {
	if !s.on {
		return
	}
	s.total += len(p)
	if len(s.head) < sniffMax {
		n := min(len(p), sniffMax-len(s.head))
		s.head = append(s.head, p[:n]...)
	}
	s.tail = append(s.tail, p...)
	if len(s.tail) > sniffMax {
		// Keep the last sniffMax bytes, re-slicing into a fresh buffer so the old one
		// can be collected rather than growing forever behind the slice header.
		keep := append(make([]byte, 0, sniffMax), s.tail[len(s.tail)-sniffMax:]...)
		s.tail = keep
	}
}

// bytes returns the retained window: head, then tail, with a newline between so a
// truncated SSE line in the middle cannot glue two events into one bogus line.
func (s *sniffer) bytes() []byte {
	if !s.on {
		return nil
	}
	if s.total <= len(s.head) {
		return s.head // the whole response fit in the head window; head IS the body
	}
	out := make([]byte, 0, len(s.head)+len(s.tail)+1)
	out = append(out, s.head...)
	out = append(out, '\n')
	return append(out, s.tail...)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
