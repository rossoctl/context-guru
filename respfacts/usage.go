package respfacts

import (
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/agent"
	"github.com/rossoctl/context-guru/providerusage"
	"github.com/rossoctl/context-guru/schema"
)

// Miss says WHY no billed tiers came back, because "ok=false" meant five different things
// and nothing distinguished them (#200). Only two of the five are benign, and the other three
// call for THREE DIFFERENT REMEDIES — which is the whole argument for classifying rather than
// counting: a shared signal reports "nothing to account" and "token accounting is offline" with
// one value, and the second is silent by construction (a healthy 200, correct savings counters,
// correct latency, and three token fields quietly at 0).
//
// It ran for 4,015 of 4,015 requests in one benchmark iteration and was found two iterations
// later, in a post-mortem chasing a different question.
type Miss uint8

// THE ORDER IS PART OF THE TYPE, ascending in severity: ParseSSEUsage compares two reasons
// with `>` to keep the worst one any event of a stream produced. A new value goes at its severity
// position, not at the end.
const (
	MissNone       Miss = iota // tiers were read; not a miss
	MissNoBody                 // nothing to look at (empty response). BENIGN
	MissAbsent                 // no usage block anywhere we know to look. BENIGN
	MissZero                   // a recognised block, every tier zero. LEGITIMATE
	MissUnparsed               // a block IS there, no recognised spelling read it
	MissUnreadable             // the bytes are not parseable, so nothing could be sought
)

// String is the reason as it appears in the lifecycle log line, so an operator reading
// `usage_reported=false` can see which of the five it was without a body dump.
func (m Miss) String() string {
	switch m {
	case MissNone:
		return "parsed"
	case MissNoBody:
		return "no_body"
	case MissAbsent:
		return "absent"
	case MissZero:
		return "all_zero"
	case MissUnparsed:
		return "unparsed_dialect"
	case MissUnreadable:
		return "unreadable_body"
	}
	return "unknown"
}

// alertable reports whether this miss means TOKEN ACCOUNTING IS OFFLINE, as opposed to a
// provider that legitimately said nothing. The two benign cases must never share a counter with
// these, or the number an operator watches to confirm accounting is healthy is incremented by it
// being broken.
func (m Miss) alertable() bool {
	_, ok := usageCounters[m]
	return ok
}

// usageCounters is the SINGLE SOURCE OF TRUTH for which reasons are alertable and which counter each
// one moves. alertable() derives from it and Read increments through it, so the two
// cannot drift.
//
// They were two independent lists — a predicate and a switch — and that is the same "two labels, one
// condition" shape this whole change is about. Widening only the predicate, which is exactly the edit
// someone adding a 6th Miss value would make, passed the entire suite: the shape record started
// firing on the new case while no counter moved and nothing caught it. Found in review.
//
// A value ABSENT from this map is benign by definition: no counter, no shape record. That is the
// decision a new value has to face, and TestEveryUsageMissIsClassifiedAsBenignOrCounted makes it
// face it rather than defaulting.
var usageCounters = map[Miss]*atomic.Int64{
	MissUnparsed:   &usageUnparsed,
	MissUnreadable: &usageUnreadable,
}

// nestedUsagePaths are places a usage block is known to live OTHER than the top level. They are
// probed for EXISTENCE ONLY — never read — which is what keeps this dialect-agnostic: knowing
// that a block is there is enough to say the parser has a gap, and guessing at the field names
// inside would produce a parser that looks correct and reads zero, which is the failure this
// classification exists to expose, reproduced in the code meant to report it.
var nestedUsagePaths = [...]string{"response.usage", "usageMetadata", "data.usage", "message.usage"}

// ParseUsage reads the four billed token tiers from a buffered response body, in whichever
// dialect it is written, and says why when it cannot (#200). The field names are in
// providerusage.Parse.
//
// Anthropic's `input_tokens` already EXCLUDES the cached tiers, so it is the fresh figure
// directly. OpenAI's `prompt_tokens` INCLUDES its cached_tokens, so fresh is the difference.
// Getting this backwards double-counts the whole transcript on every turn.
//
// ParseSSEUsage calls this per event and must not have its skips classified. Only Read, which
// sees the whole response, is in a position to say anything about it.
func ParseUsage(body []byte) (providerusage.Tiers, Miss) {
	u := gjson.GetBytes(body, "usage")
	if !providerusage.Present(u) {
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
			if providerusage.Present(gjson.GetBytes(body, p)) {
				return providerusage.Tiers{}, MissUnparsed
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
			return providerusage.Tiers{}, MissUnreadable
		}
		return providerusage.Tiers{}, MissAbsent
	}
	tiers, known := providerusage.Parse(u)
	if !known {
		// A usage block IS here and this parser read nothing out of it — the Bedrock Converse
		// camelCase shape (`usage.inputTokens`) is the known instance. The DIALECT is deliberately
		// not added here: getting the field names wrong (`cacheWriteInputTokens` vs
		// `cacheCreationInputTokens`) yields a parser that looks correct and reads zero. What
		// closes that gap is the shape record in Read, which needs no guess.
		return providerusage.Tiers{}, MissUnparsed
	}
	if tiers.Zero() {
		// Recognised and genuinely zero, which is legitimate on some responses. Kept out of the
		// unparsed counter, or every such response would inflate the one number that means a
		// dialect is missing.
		return providerusage.Tiers{}, MissZero
	}
	return tiers, MissNone
}

// ParseSSEUsage pulls usage out of a streamed response, with #200's classification. Both dialects
// report it in terminal events (Anthropic: message_start carries the input tiers and message_delta
// the output; OpenAI: a final chunk with `usage`).
//
// The INPUT tiers (fresh, cache read, cache write) are taken as ONE set from the last event that carries an
// input count, so a later event replaces an earlier one's tiers (#745): a litellm gateway puts the TOTAL
// prompt in message_start and the true split in message_delta, and a per-field maximum counted the read
// twice. An event with no input count (a real Anthropic message_delta) leaves the earlier tiers alone.
// Output keeps the maximum, because the last event carries the final count and a repeat must not double it.
//
// An event stream that carried a usage block that no event of it could read is the streamed form of the dialect gap, and it must
// not be reported as "the provider streamed no usage" — the remedies differ.
// The third return is the PAYLOAD OF THE EVENT THAT SET THE REASON, for the shape record.
//
// The record used to describe the stream's LAST data: event, which on the Anthropic-family
// transport is `message_stop` — usage arrives in `message_start`. So a streamed unrecognised
// dialect produced a record of the terminal event: no usage_at, no usage_keys, nothing about the
// dialect, and it consumed one of the bounded record slots to say it. That is exactly the shape a
// streamed Bedrock aws/claude-* response has, which is where the gap this record exists to
// diagnose would sit. Returning it from here also collapses two walks over the transport into one.
func ParseSSEUsage(raw []byte) (providerusage.Tiers, Miss, string) {
	var out providerusage.Tiers
	found := false
	// The WORST reason any event produced, which is what makes a streamed miss as precise as a
	// buffered one. The predecessor kept a bare sawBlock flag and reported every rejected block as
	// a dialect gap, so a streamed all-zero block — legitimate — read as an outage. Per-event
	// ParseUsage costs nothing extra (the old boolean form called it anyway) and each event's own
	// reason is already exact.
	// THE TWO PARSERS ARE NOT SYMMETRIC, and the asymmetry is here: this one can never return
	// MissUnreadable. It only calls ParseUsage with a block providerusage.Present already accepted,
	// and that function reaches its ValidBytes branch only when no block was found anywhere — so
	// `worst` ranges over absent/all_zero/unparsed_dialect and nothing else. A `data:` line cut
	// mid-JSON yields no present block, so the stream reads as "the provider streamed no usage".
	//
	// The corner that leaves open — a head+tail window hiding the block a whole response carried
	// — is closed one frame up, in Read, from the sniffer's own knowledge that it
	// spliced: an `absent` verdict over a spliced window is upgraded to unreadable there (#207),
	// because "no block in what we kept" and "no block anywhere" are different facts.
	worst := MissAbsent
	worstPayload := "" // the event `worst` came from, so the record describes the right one
	schema.SSEData(raw, func(d []byte) bool {
		// Anthropic nests the first usage under message.usage; OpenAI puts it at the top.
		for _, path := range []string{"usage", "message.usage", "response.usage"} {
			u := gjson.GetBytes(d, path)
			// providerusage.Present, not Exists: `"usage": null` on every chunk is what OpenAI-dialect
			// streaming sends absent stream_options.include_usage, and it is not a block.
			if !providerusage.Present(u) {
				continue
			}
			one, why := ParseUsage([]byte(`{"usage":` + u.Raw + `}`))
			if why > worst {
				worst = why // Miss is ordered benign -> alertable; see its constants
				worstPayload = string(d)
			}
			if why != MissNone {
				continue
			}
			found = true
			// THE INPUT TIERS COME FROM THE LAST EVENT THAT CARRIES AN INPUT COUNT, as one set. They were
			// merged field by field with max(), which is wrong when two events report the input in different
			// units. A litellm gateway streaming an Anthropic-shaped response puts the TOTAL prompt in
			// message_start (`input_tokens` 7373, no cache fields) and the true split in message_delta
			// (`input_tokens` 717, `cache_read_input_tokens` 6656). max() took 7373 fresh AND 6656 read, so
			// billed input counted the read twice (14,029 for a prompt of 7,373) and PrevBilledInput, which
			// the summarizer trigger and the warm gate read, was about 2x too large.
			//
			// Replacing is safe for the other shapes: a real Anthropic stream has the tiers in message_start
			// and an output-only message_delta, so no later event carries an input count and message_start's
			// values stay; and where a delta repeats the same input fields, the replacement is the same
			// numbers. A cache MISS reports the total again in the delta with zero cache fields, which gives
			// the same answer. Output keeps max(): the delta carries the final count.
			if hasInputCount(u) {
				out.FreshInput, out.CacheRead = one.FreshInput, one.CacheRead
				out.CacheWrite, out.CacheWrite1h = one.CacheWrite, one.CacheWrite1h
			}
			out.Output = max(out.Output, one.Output)
		}
		return true
	})
	if found {
		return out, MissNone, ""
	}
	// Every block that appeared was rejected, and `worst` says why — an unrecognised spelling is
	// alertable, an all-zero block is not, and no block at all is `absent` because that is what
	// `worst` starts as.
	return out, worst, worstPayload
}

// hasInputCount reports whether a usage block carries an input count at all (Anthropic or Responses
// `input_tokens`, OpenAI `prompt_tokens`), as opposed to an output-only block such as Anthropic's streaming
// message_delta.
func hasInputCount(u gjson.Result) bool {
	return u.Get("input_tokens").Exists() || u.Get("prompt_tokens").Exists()
}

// Facts is everything one response says, read from its bytes. Tiers is the zero value when Miss
// is not MissNone: a caller must report the request as partially accounted, not price it as free.
// The facts other than the tiers are filled whatever Miss says, because they are independent: an
// OpenAI response without `stream_options` reports no usage at all and still reports finish_reason.
type Facts struct {
	providerusage.Tiers
	Miss Miss
	// StopReason is the provider's terminal reason, in its own vocabulary (see StopReason).
	StopReason string
	// ToolUseNames is every tool_use or function-call name, in order (see ToolUseNames).
	ToolUseNames []string
	// ToolUseIncomplete is true when ToolUseNames may miss the true last call, because the bytes
	// read were a spliced head+tail window. The terminal-tool rule never applies then (#857).
	ToolUseIncomplete bool
	// ToolParams is the `timeout` and `run_in_background` of the last tool call (#1198). Zero on a
	// spliced window.
	ToolParams ToolParams
	// Spawn is the subagent calls of the response (#1198). Zero on a spliced window.
	Spawn Spawn
}

// Read reads one response: usage, stop reason and tool calls. c is the client that sent the request
// (the zero Client when it is not known). It is the ONE place a miss is counted
// and recorded — the whole response is in scope here, which is what it takes to say anything
// about it (ParseUsage is also called per-event by the SSE path, where a skip means nothing).
//
// spliced says the bytes in body are a head+tail WINDOW rather than the whole response (the
// sniffer's own knowledge, sn.spliced()). It upgrades an `absent` verdict to unreadable: over a
// window, "no usage block in what we kept" is not "the provider sent none" — the middle of the
// response was dropped on the floor, and the remedy is upstream of any parser. Only `absent` is
// upgraded; a block that was present and rejected is a real dialect finding whatever the window
// did, and `found` wins over everything (#207).
//
// The reason reaches the lifecycle log line beside `usage_reported`, so a deployment that has
// quietly stopped accounting tokens says which of the five cases it is at the moment it happens,
// instead of being reconstructed two iterations later from a cost figure.
func Read(c agent.Client, contentType string, body []byte, spliced bool) Facts {
	if len(body) == 0 {
		return Facts{Miss: MissNoBody}
	}
	sse := strings.Contains(contentType, "event-stream")
	var f Facts
	// doc is the JSON document the shape record should DESCRIBE, which for a stream is the one
	// event that produced the reason rather than the whole transport or its last event.
	doc := body
	if sse {
		var judged string
		f.Tiers, f.Miss, judged = ParseSSEUsage(body)
		doc = []byte(judged)
	} else {
		f.Tiers, f.Miss = ParseUsage(body)
	}
	if spliced && f.Miss == MissAbsent {
		f.Miss = MissUnreadable
	}
	f.StopReason = StopReason(sse, body)
	calls := callsOf(sse, body) // one rebuild of the calls, for the three facts below
	f.ToolUseNames = namesOf(calls)
	f.ToolUseIncomplete = spliced
	if !spliced {
		f.ToolParams = lastToolParams(calls)
		f.Spawn = spawnsOf(c, calls)
	}
	// One lookup drives both the counter and the record, so a reason cannot be alertable for one and
	// not the other. See usageCounters.
	if counter, alertable := usageCounters[f.Miss]; alertable {
		counter.Add(1)
		// len(body) so the record still reports the whole response's size — that is what says a
		// window was spliced — while describing the document that actually failed to parse.
		recordUsageShape(sse, len(body), spliced, doc)
	}
	return f
}

// NoteMiss folds one round's reason into the request's, so `usage_miss` cannot contradict
// `usage_reported` on the log line.
//
// A request can drive several upstream rounds (the expand continuation loop). usageOK is sticky —
// it is only ever set true and never reset, because a request whose usage was read once IS
// accounted — but the reason was last-write-wins, so round 2 carrying no usage relabelled an
// accounted request `absent`. The rule that removes the contradiction: once anything accounted the
// request, the reason is `parsed`; while nothing has, keep the WORST reason seen, for the same
// severity-ordering reason ParseSSEUsage keeps one.
func NoteMiss(sofar, this Miss, accounted bool) Miss {
	if accounted {
		return MissNone
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
func recordUsageShape(sse bool, total int, spliced bool, doc []byte) {
	attrs := usageShapeAttrs(sse, total, spliced, doc)
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

// ResetShapeRecordForTest forgets the shapes recorded so far. For TESTS only: the record is
// the diagnostic this whole change exists to produce, so a test has to be able to observe it, and
// the per-process bound makes that a one-shot for the entire binary otherwise.
func ResetShapeRecordForTest() {
	usageShapeMu.Lock()
	defer usageShapeMu.Unlock()
	usageShapeSeen = map[string]struct{}{}
}

// usageShapeAttrs builds the shape record's key/value pairs. Separated from the logging so a test
// can assert what it does and — more to the point — what it does NOT contain.
//
// doc is the JSON document that failed to parse: the whole body for a buffered response, and for a
// stream the ONE EVENT the classifier judged (see ParseSSEUsage) rather than the transport or its
// last event. total is the whole response's size, and spliced says that size was seen through a
// head+tail window — the two together are what say "the bytes were dropped, not the dialect".
func usageShapeAttrs(sse bool, total int, spliced bool, doc []byte) []any {
	attrs := []any{"sse", sse, "bytes", total, "spliced", spliced,
		// Not parseable means the window was spliced rather than the dialect being strange —
		// stated in the record so it cannot be misread as a field-name gap.
		"valid_json", gjson.ValidBytes(doc),
		"top_level_keys", objectKeys(gjson.ParseBytes(doc))}
	for _, p := range append([]string{"usage"}, nestedUsagePaths[:]...) {
		// providerusage.Present, not Exists, and for the same reason the classifier uses it: a null or empty
		// block resolves but names no fields, so the record pointed at `usage` and printed
		// usage_keys=[] while a real nested block held the answer — and keyed as `usage|`, which
		// collides with a genuinely empty top-level block. The record must look where the
		// CLASSIFIER looked.
		if u := gjson.GetBytes(doc, p); providerusage.Present(u) {
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
