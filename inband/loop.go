package inband

import (
	"bytes"
	"io"
	"net/http"

	"github.com/rossoctl/context-guru/adjudicate"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/thread"
	"github.com/rossoctl/context-guru/tooldefer"
)

// The in-band loop. When the model calls one of our own tools (expand, adjudicate, tool
// search), the host does not pass the call to its client. It asks this package for the answer
// and sends the follow-up request upstream itself.
//
// The host keeps the transport: the upstream call, its headers and credential, the bytes
// written to the client, and the dashboard and recorder bookkeeping. This package keeps every
// decision: what a call resolves to, what the follow-up body is, and when to give up.
// Fail open: any "no" (Result.Bail) means the host hands the upstream response over unchanged.

// MaxExpandRounds caps the continuation loop (headroom's default).
const MaxExpandRounds = 3

// Sinks are the host's accounting hooks for an expand. Any nil hook is skipped.
type Sinks struct {
	// Bounce counts an expand call process-wide (offload had to come back).
	Bounce func(tokens int)
	// NoteExpand books the restored tokens on the request's dashboard row.
	NoteExpand func(tokens int)
	// NoteSweepExpand charges a restored sweep marker to the sweep (#404 point 7).
	NoteSweepExpand func(tokens int, note offload.SweepExpandNote)
}

func (s Sinks) bounce(n int) {
	if s.Bounce != nil {
		s.Bounce(n)
	}
}

func (s Sinks) noteExpand(n int) {
	if s.NoteExpand != nil {
		s.NoteExpand(n)
	}
}

func (s Sinks) noteSweep(n int, note offload.SweepExpandNote) {
	if s.NoteSweepExpand != nil {
		s.NoteSweepExpand(n, note)
	}
}

// RepairExpandErrors resolves every expand tool_result the CLIENT had to answer itself, so
// `No such tool available: context_guru_expand` never reaches the model. It is the request
// half of interception: the loop withholds the call from the client wherever it can, and this
// covers what it structurally cannot — a batched client tool, the round cap, an event stream
// that will not reconstruct, a non-Anthropic stream, a bypassed turn carrying older markers.
// See expand.RepairToolResults.
//
// Restored content is marked kept-verbatim: an offloader that re-compacted it would trigger
// the same expand call again next turn. That mark is also what keeps the accounting honest —
// the tokens are charged to the dashboard row once, on the turn the original first comes back,
// because the client keeps its own copy of the error and the repair therefore runs again on
// every later turn. Nothing is charged to the process-wide bounce counter, which counts expand
// CALLS, and ten repairs of one stale error are one call.
func RepairExpandErrors(wire string, body []byte, st store.Store, sessionOf func(body []byte) string, s Sinks) ([]byte, int) {
	out, restored := expand.RepairToolResults(wire, body, func(hashID string) (string, bool) {
		return expand.Resolve(st, hashID)
	})
	if len(restored) == 0 {
		return out, 0
	}
	// Lazy: the derivation re-parses the transcript, and a repair is rare. Derived from the repaired
	// body, because that is what the pipeline will see.
	session := sessionOf(out)
	for _, orig := range restored {
		// Charge the row only the FIRST time this original comes back. Repairing the same
		// stale error on ten later turns is one recovery, not ten.
		if !offload.KeptVerbatim(st, session, orig) {
			s.noteExpand(schema.TextTokens(orig))
			// A sweep marker restored by the repair is charged to the sweep too (#404 point 7). Same
			// first-time rule as the line above: the repair runs again on every later turn.
			s.noteSweep(schema.TextTokens(orig), offload.NoteSweepExpand(st, offload.SweepMarkerID(orig), schema.TextTokens(orig), ""))
		}
		offload.MarkKeptVerbatim(st, session, orig)
	}
	return out, len(restored)
}

// Search is the host's side of our own tool search (#825) for one answer.
type Search struct {
	// Orig is the request before deferral. Loaded tools are added back from it.
	Orig []byte
	// Load adds the named tools back to the thread and says which were deferred (set) and which
	// went in now (now).
	Load func(names []string) (set map[string]bool, now []string)
	// Mixed keeps the searches the client receives together with its own tool call.
	Mixed func(calls []tooldefer.SearchCall)
}

// Turn is one upstream round the loop must answer.
type Turn struct {
	Wire string
	// Body is the request that produced this round. The follow-up extends it.
	Body []byte
	// Msg is the round's assistant message (an event stream is aggregated first).
	Msg      []byte
	Store    store.Store
	Session  string
	ThreadID string
	// Anchor, when non-nil, is where this turn's expands are restored on later turns
	// (expand.fixed_restore, #407).
	Anchor *expand.Anchor
	// InBandBreakpoint moves the client's last message breakpoint onto the follow-up (#784).
	InBandBreakpoint bool
	// Search is nil unless the request declares our search and deferral ran.
	Search *Search
	Sinks  Sinks
}

// Result is what the loop decided, with the counts the host logs and books. The counts are set
// on every path, also on a bail, because the side effects they describe already happened.
type Result struct {
	// Next is the follow-up request. Nil when Bail is true.
	Next []byte
	// Bail: the loop cannot answer. The host hands the upstream response over unchanged.
	Bail bool

	// Our search (#825).
	Searches     int      // calls to our search in the round
	SearchLoaded []string // tools added back now
	SearchClient bool     // the turn also holds a client tool call
	// Expand. Reached is true once the round held a call to answer: the host logs then.
	Reached  bool
	Calls    int // expand calls
	Resolved int // of those, resolved from the store
	// Strays is the number of adjudication calls answered in band. Set only when Next is set.
	Strays int
}

// Answer decides what to do with one round. It answers every call to our tools in the round,
// or says Bail when it cannot, and it never answers half a round.
func Answer(t Turn) Result {
	var res Result
	wire, msg := t.Wire, t.Msg

	// Calls to our own tool search (#825). Together with a client tool call, the turn goes to the
	// client whole (the loop cannot run the client's tool): the tools are added back now, and the
	// next request answers our call (tooldefer.ApplySearch). Alone, they are answered below.
	var searches []tooldefer.SearchCall
	var searchSet map[string]bool
	var searchNow []string
	if t.Search != nil {
		var client bool
		searches, client = tooldefer.SearchCalls(wire, msg, expand.ToolName, adjudicate.ToolName)
		if len(searches) > 0 {
			var names []string
			for _, c := range searches {
				names = append(names, c.Names...)
			}
			searchSet, searchNow = t.Search.Load(names)
			res.Searches = len(searches)
			res.SearchLoaded = searchNow
			res.SearchClient = client
			if client || searchSet == nil {
				if client && t.Search.Mixed != nil {
					t.Search.Mixed(searches)
				}
				res.Bail = true
				return res
			}
		}
	}

	calls, otherTools := expand.ResponseCalls(wire, msg, adjudicate.ToolName, tooldefer.ClientSearchName)
	// Calls the AGENT made to the adjudication tool. Answered HERE, in band, whenever this turn
	// called PROXY tools only: by the time the request-path repair runs, the client has already
	// received a tool_use for a tool it does not implement, already answered "not found", and
	// already lost the turn.
	//
	// NOT on every path, and the exception is worth more than the claim it replaces. When the model
	// calls this tool AND a client tool in the same assistant turn, otherTools is true, the bail
	// below hands the round to the client whole, and our tool_use reaches it raw. That is a
	// deliberate deferral, not an oversight: answering in band means continuing the turn upstream,
	// and this loop cannot continue a turn whose other tool_use only the CLIENT can execute. It
	// would have to invent a result for the client's tool or drop the client's call, and both are
	// worse than one lost turn. adjudicate.AnswerStrayCalls repairs it on the NEXT request instead:
	// the substitute answer lands, is_error clears, the client's own tool_result is untouched, and
	// the stray is counted — so fail-open holds and the price is one agent turn, not a broken
	// session. Pinned by TestAdjudicateStrayCoCalledWithClientToolLeaks.
	//
	// So AnswerStrayCalls backstops TWO different things: this co-call path, which the loop DOES
	// see and declines, and a round the loop genuinely cannot reconstruct (SSE aggregation failed,
	// or MaxExpandRounds is spent).
	strays := adjudicate.ResponseCallIDs(wire, msg)
	if (len(calls) == 0 && len(strays) == 0 && len(searches) == 0) || otherTools {
		res.Bail = true // normal answer (or a CLIENT tool) — hand it over unchanged
		return res
	}
	res.Reached = true
	// Build a tool_result for EVERY expand call — the provider requires one per
	// tool_call_id or the continuation is malformed. Expired/unknown ids get an
	// explicit placeholder rather than being omitted.
	resolved := map[string]string{}
	// Every stray adjudication call gets the same definite, uninteresting answer, so the model can
	// finish its turn instead of waiting on a tool nobody will run.
	for _, id := range strays {
		resolved[id] = adjudicate.StrayAnswer
	}
	for _, c := range searches {
		resolved[c.ID] = tooldefer.SearchAnswer(c.Names, searchSet)
	}
	got := 0
	for _, c := range calls {
		if orig, ok := expand.Resolve(t.Store, c.HashID); ok {
			resolved[c.CallID] = orig
			got++
			// The agent needed this content back — don't re-compact it on later turns
			// (that would loop it straight back into another expand). Keep it verbatim.
			offload.MarkKeptVerbatim(t.Store, t.Session, orig)
			// And, under fixed restore, where to give it back from now on. The mark above stays:
			// it is the fail-open behaviour for any turn on which the anchor cannot be honoured.
			if t.Anchor != nil {
				expand.RecordAnchor(t.Store, t.Session, expand.Anchor{ID: c.HashID, At: t.Anchor.At, Fp: t.Anchor.Fp})
			}
			back := schema.TextTokens(orig)
			t.Sinks.bounce(back)     // bounce: offload had to come back
			t.Sinks.noteExpand(back) // and on the dashboard row, or SavedAdjusted over-reports
			// And, when the marker is one the sweep made, charged to the sweep (#404 point 7): the
			// false-drop signal, and what comes off the sweep's credit.
			t.Sinks.noteSweep(back, offload.NoteSweepExpand(t.Store, c.HashID, back, thread.Key(t.Session, t.ThreadID)))
		} else {
			// Classified as well as counted: the response loop and the request-path repair
			// both reach here, and an id this proxy could have minted with nothing behind it
			// is our own broken reversibility promise rather than the model inventing one.
			// See expand/unresolved.go.
			expand.NoteUnresolved(c.HashID)
			resolved[c.CallID] = expand.Unavailable(c.HashID)
		}
	}
	res.Calls, res.Resolved = len(calls), got
	next, ok := expand.Continuation(wire, t.Body, msg, resolved)
	if ok && len(searchNow) > 0 && t.Search != nil {
		// The loaded tools go at the end of `tools`, as on every later request of the thread
		// (tooldefer.RewriteSearch), taken from the request before our rewrite.
		next, ok = tooldefer.AppendTools(wire, next, t.Search.Orig, searchNow)
	}
	if !ok {
		res.Bail = true // malformed shapes — fail open, hand the response over unchanged
		return res
	}
	if t.InBandBreakpoint {
		// The follow-up ends with content the client never sent. The client's last message
		// breakpoint moves onto it, so the provider caches it (#784). Off by default: the
		// client's next request does not carry this content, so nothing reads the entry.
		// No-op off Anthropic.
		next = apply.MarkInBandFollowUp(wire, next)
	}
	// got == 0 CONTINUES. `resolved` carries a placeholder for every unresolved id, so the
	// continuation is well formed whether anything came back or not, and the model finishes its
	// turn by reading "that id is no longer available". Replaying the raw response instead
	// would hand the CLIENT a bare tool_use for a tool it does not implement.
	res.Next, res.Strays = next, len(strays)
	return res
}

// Refused is a round-0 request that tool deferral changed and the provider refused.
type Refused struct {
	// Orig is the request before deferral.
	Orig []byte
	// Search: the original declares our search, so the repair below does not apply (it reads
	// Anthropic shapes, not our search's OpenAI wires).
	Search bool
	// Do sends one request upstream.
	Do func(body []byte) (*http.Response, error)
	// Book records the refusal: the refused status, the provider's error body, and whether the
	// original was accepted.
	Book func(status int, errBody []byte, originalOK bool)
}

// RetryRefused sends the request without deferral, once, when the provider refused it with 400
// or 422: the client must never see that 400 (Claude Code would turn its own search off for the
// session). It returns retried=false, and leaves resp as it is, when the response is not such a
// refusal. When retried, body is what went upstream last, and the host must drop deferral for
// the rest of the request.
func RetryRefused(resp *http.Response, err error, body []byte, r Refused) (_ *http.Response, _ error, _ []byte, retried bool) {
	if err != nil || resp == nil || (resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusUnprocessableEntity) {
		return resp, err, body, false
	}
	errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	resp.Body.Close()
	refused := resp.StatusCode
	body = r.Orig
	resp, err = r.Do(body)
	stubbed := false
	// The missing-reference repair reads Anthropic shapes: not on our search's OpenAI wires.
	if err == nil && !r.Search {
		if fixed, ok := RepairOnMissingRef(resp, body); ok {
			// The original failed the same way: it names a tool `tools` lacks. Send it once
			// more with a stub for that tool, so the client never sees our 400 (#871). The
			// refusal was then not deferral's, so deferral stays on (originalOK false).
			body, stubbed = fixed, true
			resp, err = r.Do(body)
		}
	}
	ok := !stubbed && err == nil && resp != nil && resp.StatusCode < 400
	if r.Book != nil {
		r.Book(refused, errBody, ok)
	}
	return resp, err, body, true
}

// RepairOnMissingRef reads a refused response. When it is a 400 that names a tool_reference to a
// tool body does not declare, it closes resp and returns body with a deferred stub for that tool.
// Otherwise resp is left readable, with the same bytes, and ok is false. The decision is
// tooldefer.RepairFromError's; this keeps the HTTP response.
func RepairOnMissingRef(resp *http.Response, body []byte) (fixed []byte, ok bool) {
	if resp == nil || resp.StatusCode != http.StatusBadRequest {
		return nil, false
	}
	errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if fixed, ok = tooldefer.RepairFromError(errBody, body); !ok {
		// Put back what was read: LimitReader stops at 16 KiB, so the bytes read and the unread
		// rest of resp.Body are the original stream, in order.
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(errBody), resp.Body), resp.Body}
		return nil, false
	}
	resp.Body.Close()
	return fixed, true
}
