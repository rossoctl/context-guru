package record

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/session"
)

// This file holds the rules a host needs to turn one proxied call into a Turn: the trace id, how
// incomplete reasons combine, and how a ProxiedCall becomes a Turn. A host does the transport (it reads
// the bytes, times the call) and the file writes. It does not repeat these rules.

// ProxiedCall is one proxied call as a host saw it: raw bytes and scalars. Nothing here is parsed.
type ProxiedCall struct {
	TraceID  string // the session key, see TraceID
	SpanID   string // the host makes it, it is not a rule
	ParentID string
	Provider string // the provider the call went to, such as "anthropic"

	ReqBody   []byte // the PRISTINE inbound body, before the pipeline
	RespBody  []byte // the SSE event stream, a JSON body, or nil
	RespIsSSE bool
	RespSize  int

	Start, End  time.Time
	UpstreamMS  int64
	FirstByteMS int64 // 0 means "not knowable", never "instant"
	Rounds      int

	// The provider-billed tiers, from the host's own parse of the response. They never come from
	// UsageFromResponse: an aggregated SSE body carries no usage key, so reading it back would
	// report zeros for every streamed turn.
	FreshInput, CacheRead, CacheWrite, Output int64
	StopReason                                string
	UsageOK                                   bool

	Incomplete string // the reasons the host knows, see JoinIncomplete
	Err        string
}

// TurnFromCall fills a Turn from one call. It is the expensive part: it parses, translates and
// aggregates, so a host runs it on its writer goroutine and never on the request path.
func TurnFromCall(c ProxiedCall) Turn {
	turn := Turn{
		SessionID: c.TraceID,
		SpanID:    c.SpanID,
		ParentID:  c.ParentID,
		System:    c.Provider,
		Start:     c.Start,
		End:       c.End,
		// int64 -> int: a token count that overflowed int32 would be a 2-billion-token request.
		FreshInput:   int(c.FreshInput),
		CacheRead:    int(c.CacheRead),
		CacheWrite:   int(c.CacheWrite),
		Output:       int(c.Output),
		StopReason:   c.StopReason,
		UpstreamMS:   c.UpstreamMS,
		FirstByteMS:  c.FirstByteMS,
		Rounds:       c.Rounds,
		Incomplete:   c.Incomplete,
		ResponseSize: c.RespSize,
		Err:          c.Err,
	}
	if meta, err := RequestMeta(c.ReqBody); err == nil {
		turn.Model, turn.MaxTokens = meta.Model, meta.MaxTokens
		turn.Effort, turn.OutputTy = meta.Effort, meta.OutputType
		turn.OutputSchema = meta.OutputSchema
	}
	if in, err := InputMessages(c.ReqBody); err == nil {
		if body, err := MarshalMessages(in); err == nil {
			turn.RequestBody = body
		}
	}
	if defs, err := ToolDefinitions(c.ReqBody); err == nil {
		turn.ToolDefs = defs
	}
	fillResponse(&turn, c)
	// Zero tokens and unknown tokens are DIFFERENT FACTS, and ToSpan cannot tell them apart:
	// it writes the cache split unconditionally, because "this turn did no cache reads" is real
	// data worth recording. So an unparsed usage block has to be marked here, or the corpus
	// silently reports a measured zero for a turn nobody measured.
	if !c.UsageOK {
		turn.MarkIncomplete(IncompleteNoUsage)
	}
	return turn
}

// fillResponse translates the response body, aggregating SSE first when needed.
//
// An SSE body cannot be translated directly: OutputMessages unmarshals a single Anthropic
// message object, and the tee holds an event stream. AggregateSSE rebuilds that object -- for
// Anthropic only, by design -- so a non-Anthropic stream has no translatable body and the turn
// says so rather than presenting an empty answer as a real one.
func fillResponse(turn *Turn, c ProxiedCall) {
	if len(c.RespBody) == 0 {
		turn.MarkIncomplete(IncompleteNoBody)
		return
	}
	body := c.RespBody
	if c.RespIsSSE {
		// Anthropic only: OutputMessages reads an Anthropic message object.
		if c.Provider != schema.WireAnthropic {
			turn.MarkIncomplete(IncompleteNoBody)
			return
		}
		agg, ok := expand.AggregateSSE(c.Provider, body)
		if !ok {
			turn.MarkIncomplete(IncompleteNoBody)
			return
		}
		body = agg
	}
	out, err := OutputMessages(body)
	if err != nil {
		turn.MarkIncomplete(IncompleteNoBody)
		return
	}
	if s, err := MarshalMessages(out); err == nil && s != "" {
		turn.ResponseBody = s
		return
	}
	turn.MarkIncomplete(IncompleteNoBody)
}

// JoinIncomplete composes incomplete reasons: the first reason wins, and each distinct later
// reason appends, comma-separated. A host uses it for the string it builds before a Turn exists,
// and Turn.MarkIncomplete uses it too, so the two cannot differ.
func JoinIncomplete(existing, reason string) string {
	if reason == IncompleteNone {
		return existing
	}
	if existing == IncompleteNone {
		return reason
	}
	if strings.Contains(existing, reason) {
		return existing
	}
	return existing + "," + reason
}

// TraceID chooses the id a recorded call is filed under. It must not come from the pipeline's
// session: in observe mode that is EMPTY, because observe's byte-identity guarantee means the
// enforced path never sets one. Keying on it would write one file, named for the empty string,
// holding every session's spans.
//
// So: Claude Code's own UUID from metadata.user_id first. That is what claude-record names its
// files by, which makes the two recorders joinable per session, and session.ExplicitID
// documents it as stable across compaction where the content-hash fallback is not.
//
// derived is the host's own session resolver, run on the PRISTINE body. It is the second choice:
// possibly a content hash, so it can flip at compaction. It may be nil. It runs only when the
// first choice fails. Its answer is PREFIXED, so an analyst can see at a glance that a file is
// not keyed on a real session.
//
// The result is never empty: an empty id yields a hidden ".wire.jsonl" file and merges every
// unkeyed session into one trace.
func TraceID(body []byte, derived func() string) string {
	if id := session.ExplicitID(gjson.GetBytes(body, "metadata.user_id").String()); id != "" {
		return id
	}
	if derived != nil {
		if id := derived(); id != "" {
			return "cg-derived-" + id
		}
	}
	return "cg-unkeyed-" + unkeyedSuffix()
}

// unkeyedProcess is minted once so every unkeyed session in one process shares a file, rather
// than scattering one file per request.
var unkeyedProcess struct {
	once sync.Once
	hex  string
}

func unkeyedSuffix() string {
	unkeyedProcess.once.Do(func() {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			unkeyedProcess.hex = "0"
			return
		}
		unkeyedProcess.hex = hex.EncodeToString(b[:])
	})
	return unkeyedProcess.hex
}
