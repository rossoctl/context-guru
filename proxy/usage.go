package proxy

import (
	"github.com/rossoctl/context-guru/respfacts"
	"github.com/rossoctl/context-guru/thinkcount"
)

// Usage is one response's provider-billed token tiers. ok=false means the
// provider told us nothing, in which case a caller must report the request as
// partially accounted rather than pricing it as free.
type Usage struct {
	// Searches and SearchLoaded are the server-side tool searches a streamed response ran and the
	// tools they loaded, read off the sniffer window for tool deferral's ledger (#485).
	// SearchDropped is how many of those searches have no result (sent with another tool call).
	Searches      int
	SearchLoaded  []string
	SearchDropped int
	// Live is the request-side facts a learned keep-alive campaign reads at response end (predictive.go). nil on paths that do not set it.
	Live *LiveFacts
	// PrefixRewritten says the client rewrote the prefix this request sends: the agent's own compaction
	// came just before it, or the thread tracker found it forks or rewinds another thread's content
	// (capture.prefixRewritten). A cache miss on such a request has a cause other than the cache tier. Set
	// by serve beside Live; read by recordTouch.
	PrefixRewritten bool
	FreshInput      int64
	CacheRead       int64
	CacheWrite      int64
	Output          int64
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
	// tokens at the 1h tier. Absent-or-zero here with a 1h breakpoint on the wire means EITHER a
	// refusal OR that the request wrote nothing at that breakpoint (a cache read bills no
	// write): the head of a warm session is always a read. Read it only on a request that
	// wrote the entry. The grant varies with model, provider and date (#1243).
	CacheWrite1h int64
	// StopReason is the provider's terminal reason for this response, normalized across
	// dialects (see respfacts.StopReason). It rides on Usage because it comes off the same
	// buffered/sniffed response bytes and reaches the dashboard by the same path — a
	// second out-of-band channel for one string would be a second thing to keep in sync.
	//
	// It is filled even when the token tiers are absent, because the two facts are
	// independent: an OpenAI response without `stream_options` reports no usage at all and
	// still reports finish_reason, and "we could not price this, and it stopped because it
	// hit max_tokens" is exactly the pair worth having.
	StopReason string
	// ToolUseNames is every tool_use (Anthropic) / function call (OpenAI chat, Responses)
	// block name this response carried, in the order they appeared — read off the same
	// bytes StopReason comes from (see respfacts.ToolUseNames), for issue #424's
	// waitcause.ClassifyWaitingFor: a response that calls a tool is a thread about to wait
	// on ITS OWN tool call, not on a human or a background task, and the LAST name here is
	// what the dashboard keeps for it. nil when the response carried none, or when it was
	// never buffered/sniffed far enough to see one (a long streamed response's tool_use
	// block can fall outside the sniffer's bounded head window — see newSniffer — in which
	// case this is best-effort, not exhaustive, and the row reads a weaker signal
	// (background/teammate/human from open state) rather than own_tool, never a guess).
	ToolUseNames []string
	// ToolUseIncomplete is true when ToolUseNames may miss the true last call, because the bytes
	// read were a spliced head+tail sniff window. The terminal-tool rule never applies then (#857).
	ToolUseIncomplete bool
	// ToolParams is the two numeric/boolean parameters of the LAST tool call that say how long the
	// wait may last (#1198): `timeout` and `run_in_background`. Nothing else of the call's input is
	// kept. The zero value means neither was stated, or the call could not be read.
	ToolParams respfacts.ToolParams
	// Spawn is the subagent calls of this response (#1198): how many, and the type of the last.
	Spawn respfacts.Spawn
	// Backend is which of Bob's backends answered (bobBackend). Set on Bob's model route only.
	Backend string
	// Thinking is the signature of each thinking block the response made and its thinking_tokens. finish
	// records it in the accountant's count table (issue #1187). Empty when the response made no block.
	Thinking thinkcount.Response
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

// spliced reports whether the retained window DROPPED anything: more bytes were written than
// the head alone holds, so bytes() is a splice of head and tail with a middle missing. The usage
// classifier needs this because "no usage block in what we kept" then stops meaning "the
// provider sent none" — see responseUsageWhy.
func (s *sniffer) spliced() bool { return s.on && s.total > len(s.head) }

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

// disjoint is the retained window with no byte twice. For a response of sniffMax to 2*sniffMax
// bytes the head and the tail overlap, and bytes() holds the overlap twice: a reader that counts
// events (tool deferral's search count) would count one there twice. Here the tail is cut past
// the head, so an overlap gives the exact body; a gap keeps the newline between the halves.
func (s *sniffer) disjoint() []byte {
	if !s.on {
		return nil
	}
	if s.total <= len(s.head) {
		return s.head
	}
	start := len(s.head) - (s.total - len(s.tail)) // the tail's first byte the head does not hold
	if start < 0 {
		return s.bytes() // a gap: nothing is held twice
	}
	out := make([]byte, 0, len(s.head)+len(s.tail)-start)
	out = append(out, s.head...)
	return append(out, s.tail[start:]...)
}

// responseUsage reads a response's usage. The returned Usage carries the stop reason whatever
// `ok` says, see Usage.StopReason.
func responseUsage(contentType string, body []byte) (Usage, bool) {
	u, _, ok := responseUsageWhy(contentType, body, false)
	return u, ok
}

// responseUsageWhy reads a response through respfacts.Read and lays the facts into a Usage, which
// adds the fields only this host sets. spliced is the sniffer's own knowledge that body is a
// head+tail window (sn.spliced()).
func responseUsageWhy(contentType string, body []byte, spliced bool) (Usage, respfacts.Miss, bool) {
	f := respfacts.Read(contentType, body, spliced)
	return Usage{
		FreshInput: f.FreshInput, CacheRead: f.CacheRead, CacheWrite: f.CacheWrite,
		CacheWrite1h: f.CacheWrite1h, Output: f.Output,
		StopReason: f.StopReason, ToolUseNames: f.ToolUseNames, ToolUseIncomplete: f.ToolUseIncomplete,
		ToolParams: f.ToolParams, Spawn: f.Spawn,
	}, f.Miss, f.Miss == respfacts.MissNone
}
