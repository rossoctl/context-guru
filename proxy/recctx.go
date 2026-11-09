package proxy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/internal/record"
	"github.com/rossoctl/context-guru/session"
)

// The request-path half of the session recorder. It has no heavy dependency, so it is in
// every build; the recorder itself (sessionrec.go) is only in a -tags cg_record build.
// Without a recorder, h.recorder is nil, recordFor says no, and none of this runs.

// recEvent is one turn, handed to the writer as RAW BYTES AND SCALARS ONLY.
//
// Nothing here is parsed, translated or redacted: that is the whole latency design. The
// request goroutine copies fields and sends; the writer does the work.
type recEvent struct {
	traceID  string
	tenantID string // "" single-tenant; otherwise the file goes under <dir>/<tenantID>/
	provider bschemas.ModelProvider
	path     string      // the API path, as the client called it; selects the wire dialect
	headers  http.Header // the client's request headers; credentials are removed when written
	status   int         // the HTTP status the client received

	reqBody   []byte // PRISTINE inbound body (pre-pipeline)
	respBody  []byte // raw: SSE event stream, or a JSON body, or nil
	respIsSSE bool
	respSize  int

	start, end  time.Time
	upstreamMS  int64
	firstByteMS int64 // 0 means "not knowable", never "instant" -- see serve's collection
	rounds      int

	usage   Usage
	usageOK bool

	incomplete string
	errMsg     string
}

// recCtx is what the request path hands `serve` for recording: the pristine body and the
// session key, both resolved in `chat` before the pipeline rewrites anything.
//
// It is ONE parameter rather than two because serve already takes ten and the plan for
// splitting them into a request struct is a separate refactor this change deliberately does
// not bundle. Zero value means "not recording", so every field access stays valid.
type recCtx struct {
	pristine []byte // the inbound body as the client sent it, pre-pipeline
	traceID  string // Claude Code's own session UUID where available -- see newRecCtx
	tenantID string // the consenting tenant in hosted mode, "" single-tenant
}

// recordFor decides whether THIS request may be recorded. The same two gates as
// captureContentFor, both of which must pass: the operator's --record flag (the recorder
// exists at all) and, in hosted mode, the tenant's own consent.
//
// The difference is the default. Content capture is ON for a new account; trace recording
// is OFF (tenant.Tenant.RecordTraces), because a recorded trace is the full, replayable
// body of every call -- the most a proxy can keep -- and is for research, not for the
// person using CG. An operator turning --record on a shared instance records nobody until
// each person opts in for themselves.
func (h *Handler) recordFor(tn *Tenancy) bool {
	if h == nil || h.recorder == nil {
		return false
	}
	if h.opts.Tenants == nil {
		return true
	}
	return tn != nil && tn.RecordTraces
}

// newRecCtx resolves the recording context, or the zero value when recording is off.
//
// THE TRACE ID IS THE WHOLE POINT OF THIS FUNCTION, and it must not come from tr.Session.
// In observe mode -- the mode a dual-recorded session runs in -- tr.Session is EMPTY, because
// observe's byte-identity guarantee is structural: the enforced path never calls BodyOpts, so
// it has no Trace and no session id (apply.go:1220-1236). Keying on it would produce one file
// named for the empty string holding every session's spans.
//
// So: Claude Code's own UUID from metadata.user_id first. That is what claude-record names its
// files by, which is what makes the two recorders joinable per session, and session.ExplicitID
// documents it as stable across compaction where the content-hash fallback is not. The derived
// key is the fallback, and it is PREFIXED so an analyst can see at a glance that a file is not
// keyed on a real session. explicitSess is the chat handler's explicit id (explicitSessionOf).
// So a Bob IDE file is keyed on its X-Task-Id, like the pipeline's session.
func (h *Handler) newRecCtx(provider bschemas.ModelProvider, orig []byte, tn *Tenancy, explicitSess string) recCtx {
	if !h.recordFor(tn) {
		return recCtx{}
	}
	id := session.ExplicitID(gjson.GetBytes(orig, "metadata.user_id").String())
	if id == "" {
		// Second choice: the proxy's own resolver, computed off the PRISTINE body. Tenant-
		// prefixed and possibly a content hash, so it can flip at compaction -- acceptable as
		// a fallback, not as the primary.
		if tn != nil {
			id = apply.SessionIDFor(tn.ID, explicitSess, provider, orig)
		}
		if id != "" {
			id = "cg-derived-" + id
		}
	}
	if id == "" {
		// Never empty: an empty id yields a hidden ".wire.jsonl" file, and merges every unkeyed
		// session into one trace.
		id = "cg-unkeyed-" + unkeyedSuffix()
	}
	// bytes.Clone, not an alias: the pipeline owns `body` from here and a future refactor that
	// mutated the backing array in place would silently corrupt every recorded request. ~200 KB
	// on an opt-in path buys the invariant outright.
	var tenantID string
	if h.opts.Tenants != nil {
		tenantID = tn.ID // recordFor guarantees tn != nil here
	}
	return recCtx{pristine: bytes.Clone(orig), traceID: id, tenantID: tenantID}
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

// teeBody reads the tee out for the recorder.
//
// The bool from recTee.bytes() means "whole", not "present": a response longer than the tee
// cap yields the first teeCap bytes and false. Those bytes are still worth keeping -- a
// truncated SSE stream usually aggregates to a usable partial message -- but the turn must
// say it is partial, which is what IncompleteTeeCap is for.
func teeBody(t *recTee, isSSE bool, incomplete *string) (body []byte, sse bool) {
	if t == nil {
		return nil, false
	}
	b, whole := t.bytes()
	if !whole {
		*incomplete = joinIncomplete(*incomplete, record.IncompleteTeeCap)
	}
	return b, isSSE
}

// joinIncomplete composes reasons the way record.markIncomplete does -- first reason wins,
// distinct reasons append comma-separated -- for the proxy-side string that is assembled
// before a Turn exists.
func joinIncomplete(existing, reason string) string {
	if reason == record.IncompleteNone {
		return existing
	}
	if existing == record.IncompleteNone {
		return reason
	}
	if strings.Contains(existing, reason) {
		return existing
	}
	return existing + "," + reason
}

// firstByteOrZero returns the observed time to first byte, or 0 when none was observed.
//
// It exists to NOT call msSince with a zero time. msSince treats zero as "now" (proxy.go),
// which is correct for the dashboard's SSE accounting and wrong in a trace file: it would
// record the entire request duration as the time to first byte on every buffered response.
// Zero here means "not knowable", and Turn.FirstByteMS is documented as omitted-when-zero.
func firstByteOrZero(start, first time.Time) int64 {
	if first.IsZero() {
		return 0
	}
	// Round, not truncate: msSince returns fractional milliseconds, and a sub-millisecond
	// first byte truncating to 0 would be indistinguishable from "not observed".
	return int64(math.Round(msSince(start, first)))
}
