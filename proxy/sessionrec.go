package proxy

// sessionRecorder records one OTLP span per proxied turn, for --record and --record-otlp.
//
// WHY THIS LIVES IN package proxy AND NOT internal/record. internal/record is a pure
// translation library: no I/O, no proxy imports, unit-testable on byte slices alone, and
// its Turn doc says outright that "the proxy fills it, the recorder writes it". Putting
// the writer there would invert that dependency (record importing proxy for Usage, recTee
// and expand.AggregateSSE) or force a parallel copy of all of it. So the split stays:
// record translates, this file fills and writes.
//
// THE ONE RULE THIS FILE OBEYS. CLAUDE.md: "Any component error/panic reverts that
// component only; the original request is always forwarded as a valid fallback." Recording
// is a component. It may not alter a byte the client receives, may not return an error to
// the client, and may not block the request goroutine on I/O. Every expensive step --
// SSE aggregation, JSON translation, redaction, file writes -- happens on the writer
// goroutine for that reason, and the handoff is a single non-blocking channel send.
//
// This is not a preference, it is measured: dashcapture.go records that `finish` runs in
// serve's defer, which fires BEFORE the handler returns, so anything expensive there is
// paid by the next request on a keep-alive connection -- "by every real agent". Redaction
// alone is ~53 ms/request against ~175 ns for the channel send.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/record"
	"github.com/rossoctl/context-guru/session"
)

// Defaults for the --record flags, applied when a caller passes a non-positive value so the
// recorder is never constructed in a degenerate state.
//
// The two caps are easy to confuse and bound different things. defaultRecordTeeCap is bytes
// RETAINED PER IN-FLIGHT RESPONSE, so the memory ceiling is roughly this times the number of
// concurrent requests. defaultRecordBodyCap is per body AFTER redaction, i.e. what reaches
// disk. Tee larger than body on purpose: a response truncated at the tee is gone, where one
// truncated at the body cap at least marks itself via Turn.Redact.
const (
	defaultRecordBodyCap = 1 << 20 // 1 MB per body, post-redaction
	defaultRecordTeeCap  = 8 << 20 // 8 MB retained per in-flight response
	defaultRecordQueue   = 256     // turns buffered before Record starts dropping
)

// recEvent is one turn, handed to the writer as RAW BYTES AND SCALARS ONLY.
//
// Nothing here is parsed, translated or redacted: that is the whole latency design. The
// request goroutine copies fields and sends; the writer does the work.
type recEvent struct {
	traceID  string
	tenantID string // "" single-tenant; otherwise the file goes under <dir>/<tenantID>/
	provider bschemas.ModelProvider

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

// recSession is the one thing the writer keeps per session: the last span id, the parent of the
// next turn, which is what threads a session together in a trace view. No spans are kept --
// each is written once, as it is built -- so memory no longer grows with the length of a
// session, as it did with the file writer this replaces.
type recSession struct {
	lastSpan string
}

type sessionRecorder struct {
	dir     string     // local OTLP JSON files, one per session; "" when only exporting
	exp     *recExport // OTLP/HTTP exporter; nil when only writing files
	bodyCap int
	teeCap  int
	log     *slog.Logger

	ch   chan *recEvent
	done chan struct{}
	wg   sync.WaitGroup

	sessions map[string]*recSession // writer-goroutine-owned

	captured, dropped, errors, written atomic.Int64
	warnedDrop                         atomic.Bool
}

// newSessionRecorder returns nil when both dir and endpoint are empty, matching newCapture's
// nil-means-off contract so every call site is one nil check on the default path.
//
// Two outputs, either or both:
//
//   - endpoint (--record-otlp): OTLP/HTTP to a collector such as Phoenix, Jaeger or an OTel
//     collector. Each span is sent once, batched, off the writer goroutine.
//   - dir (--record): the same spans as OTLP JSON lines, one file per session. That is the
//     OTel collector file exporter's format, so the offline converter (cmd/cg-record-convert)
//     reads either. It is what a laptop run without a collector uses.
//
// MkdirAll and the endpoint check happen HERE rather than at first write: a bad path or URL must
// fail loudly at startup, not silently per request for the length of a session someone believed
// was being recorded.
func newSessionRecorder(dir, endpoint string, bodyCap, teeCap, queue int, log *slog.Logger) (*sessionRecorder, error) {
	dir, endpoint = strings.TrimSpace(dir), strings.TrimSpace(endpoint)
	if dir == "" && endpoint == "" {
		return nil, nil
	}
	var abs string
	if dir != "" {
		var err error
		if abs, err = filepath.Abs(dir); err != nil {
			return nil, fmt.Errorf("record dir %q: %w", dir, err)
		}
		if err := os.MkdirAll(abs, 0o700); err != nil {
			return nil, fmt.Errorf("record dir %q: %w", abs, err)
		}
	}
	var exp *recExport
	if endpoint != "" {
		var err error
		if exp, err = newRecExport(endpoint); err != nil {
			return nil, err
		}
	}
	if bodyCap <= 0 {
		bodyCap = defaultRecordBodyCap
	}
	if teeCap <= 0 {
		teeCap = defaultRecordTeeCap
	}
	if queue <= 0 {
		queue = defaultRecordQueue
	}
	if log == nil {
		log = slog.Default()
	}
	r := &sessionRecorder{
		dir: abs, exp: exp, bodyCap: bodyCap, teeCap: teeCap, log: log,
		ch:       make(chan *recEvent, queue),
		done:     make(chan struct{}),
		sessions: make(map[string]*recSession),
	}
	r.wg.Add(1)
	go r.run()
	return r, nil
}

// Record hands one turn to the writer. Nil-safe, never blocks, never fails: a full queue
// drops and counts, exactly as dash.Recorder.Record does.
//
// The caller must not touch the event afterwards -- the writer owns it.
func (r *sessionRecorder) Record(e *recEvent) {
	if r == nil || e == nil {
		return
	}
	r.captured.Add(1)
	select {
	case r.ch <- e:
	default:
		r.dropped.Add(1)
		// Once, loudly. A file silently missing turns is worse than no file at all, and
		// nothing else reads this counter until Close.
		if r.warnedDrop.CompareAndSwap(false, true) {
			r.log.Warn("cg.record: queue full, dropping turns; the trace will be incomplete",
				"queue", cap(r.ch))
		}
	}
}

// Close drains what is queued, sends what the exporter still holds, and reports the counters.
func (r *sessionRecorder) Close() error {
	if r == nil {
		return nil
	}
	close(r.done)
	r.wg.Wait()
	// After the writer has stopped, so every span it built is in the exporter's queue.
	if err := r.exp.Shutdown(); err != nil {
		r.log.Warn("cg.record: could not send every span to the collector", "err", err)
	}
	r.log.Info("cg.record: stopped", "dir", r.dir,
		"turns_captured", r.captured.Load(), "turns_written", r.written.Load(),
		"turns_dropped", r.dropped.Load(), "errors", r.errors.Load())
	return nil
}

// run is the writer goroutine.
//
// defer wg.Done() is registered FIRST, before the recover, so a panic here can never leave
// Close blocked on wg.Wait() forever. A recording bug must not wedge proxy shutdown.
func (r *sessionRecorder) run() {
	defer r.wg.Done()
	defer func() {
		if rec := recover(); rec != nil {
			r.errors.Add(1)
			r.log.Error("cg.record: writer goroutine panicked; recording has stopped "+
				"for the rest of this process", "panic", rec)
		}
	}()
	for {
		select {
		case e := <-r.ch:
			r.handle(e)
		case <-r.done:
			// Drain what is already queued: these are turns that really happened, and the
			// process is going away.
			for {
				select {
				case e := <-r.ch:
					r.handle(e)
				default:
					return
				}
			}
		}
	}
}

// handle translates one event into a span. Its own recover, so ONE malformed body loses
// that turn and nothing else -- the goroutine, and the rest of the session, survive.
func (r *sessionRecorder) handle(e *recEvent) {
	defer func() {
		if rec := recover(); rec != nil {
			r.errors.Add(1)
			r.log.Error("cg.record: dropped one turn after a panic while translating it",
				"panic", rec)
		}
	}()
	if e == nil {
		return
	}
	// Keyed by tenant AND session: two tenants never share a parent chain or a file, even if a
	// client sent another tenant's session UUID.
	key := e.tenantID + "\x00" + e.traceID
	s := r.sessions[key]
	if s == nil {
		s = &recSession{}
		r.sessions[key] = s
	}
	turn, err := r.buildTurn(e, s.lastSpan)
	if err != nil {
		r.errors.Add(1)
		r.log.Warn("cg.record: could not translate a turn", "err", err, "trace_id", e.traceID)
		return
	}
	turn.Redact(r.bodyCap)
	span := turn.ToOTLP()
	if r.exp != nil {
		if err := r.exp.export(e.tenantID, span); err != nil {
			r.errors.Add(1)
			r.log.Warn("cg.record: could not export a turn", "err", err, "trace_id", e.traceID)
		}
	}
	if r.dir != "" {
		if err := r.appendLocal(e.tenantID, e.traceID, span); err != nil {
			r.errors.Add(1)
			r.log.Warn("cg.record: could not write a turn", "err", err, "trace_id", e.traceID)
		}
	}
	s.lastSpan = turn.SpanID
	r.written.Add(1)
}

// buildTurn fills record.Turn from one event. Everything expensive is here, on the writer.
func (r *sessionRecorder) buildTurn(e *recEvent, parent string) (record.Turn, error) {
	spanID, err := randomSpanID()
	if err != nil {
		return record.Turn{}, err
	}
	turn := record.Turn{
		SessionID: e.traceID,
		SpanID:    spanID,
		ParentID:  parent,
		System:    string(e.provider),
		Start:     e.start,
		End:       e.end,
		// Usage comes from the PROXY's own parse, never record.UsageFromResponse: an
		// aggregated SSE body carries no usage key at all (expand/sse.go never emits one),
		// so reading it back would report zeros for every streamed turn -- i.e. for every
		// turn of a real Claude Code session.
		// int64 -> int: proxy.Usage counts in int64, record.Turn in int. Safe on every
		// platform this builds for (64-bit int), and a token count that overflowed int32
		// would be a 2-billion-token request.
		FreshInput: int(e.usage.FreshInput),
		CacheRead:  int(e.usage.CacheRead),
		CacheWrite: int(e.usage.CacheWrite),
		Output:     int(e.usage.Output),
		StopReason: e.usage.StopReason,
		UpstreamMS: e.upstreamMS,
		// FirstByteMS only when it was actually observed. serve deliberately leaves
		// sseFirstByte zero on a buffered response, and msSince falls back to NOW for a
		// zero time -- correct for the dashboard's SSE accounting, and in a trace it would
		// report the entire request duration as the time to first byte.
		FirstByteMS:  e.firstByteMS,
		Rounds:       e.rounds,
		Incomplete:   e.incomplete,
		ResponseSize: e.respSize,
		Err:          e.errMsg,
	}
	if meta, err := record.RequestMeta(e.reqBody); err == nil {
		turn.Model, turn.MaxTokens = meta.Model, meta.MaxTokens
		turn.Effort, turn.OutputTy = meta.Effort, meta.OutputType
		turn.OutputSchema = meta.OutputSchema
	}
	if in, err := record.InputMessages(e.reqBody); err == nil {
		if body, err := record.MarshalMessages(in); err == nil {
			turn.RequestBody = body
		}
	}
	if defs, err := record.ToolDefinitions(e.reqBody); err == nil {
		turn.ToolDefs = defs
	}
	r.fillResponse(&turn, e)
	// Zero tokens and unknown tokens are DIFFERENT FACTS, and ToSpan cannot tell them apart:
	// it writes the cache split unconditionally, because "this turn did no cache reads" is real
	// data worth recording. So an unparsed usage block has to be marked here, or the corpus
	// silently reports a measured zero for a turn nobody measured.
	if !e.usageOK {
		turn.MarkIncomplete(record.IncompleteNoUsage)
	}
	return turn, nil
}

// fillResponse translates the response body, aggregating SSE first when needed.
//
// An SSE body cannot be translated directly: record.OutputMessages unmarshals a single
// Anthropic message object, and the tee holds an event stream. AggregateSSE rebuilds that
// object -- for Anthropic only, by design -- so a non-Anthropic stream has no translatable
// body and the turn says so rather than presenting an empty answer as a real one.
func (r *sessionRecorder) fillResponse(turn *record.Turn, e *recEvent) {
	if len(e.respBody) == 0 {
		turn.MarkIncomplete(record.IncompleteNoBody)
		return
	}
	body := e.respBody
	if e.respIsSSE {
		agg, ok := expand.AggregateSSE(string(e.provider), body)
		if !ok {
			turn.MarkIncomplete(record.IncompleteNoBody)
			return
		}
		body = agg
	}
	out, err := record.OutputMessages(body)
	if err != nil {
		turn.MarkIncomplete(record.IncompleteNoBody)
		return
	}
	if s, err := record.MarshalMessages(out); err == nil && s != "" {
		turn.ResponseBody = s
		return
	}
	turn.MarkIncomplete(record.IncompleteNoBody)
}

// appendLocal appends one span to the session's local OTLP JSON file, as one line.
//
// APPEND, NOT REWRITE. The file writer this replaces rewrote the whole session file every 10 s,
// which costs O(N^2) bytes over a long session. Each span is now written once. A crash can lose
// at most the line being written, and the converter reports a broken line rather than skipping
// it silently.
//
// In hosted mode each tenant's files go in their own subdirectory, created owner-only, so one
// person's traces are never mixed with another's and can be handed over, or deleted, as a unit.
// A proxy restart in the middle of a session appends to the same file, so nothing an earlier
// process wrote is ever overwritten.
func (r *sessionRecorder) appendLocal(tenantID, traceID string, span record.OTLPSpan) error {
	dir := r.dir
	if tenantID != "" {
		dir = filepath.Join(r.dir, sanitizeTraceID(tenantID))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	line, err := record.EncodeOTLPLine(record.Resource(tenantID), span)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, sanitizeTraceID(traceID)+".otlp.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// sanitizeTraceID makes a session id safe as a filename.
//
// THE TRAP IS NTFS-SPECIFIC AND SILENT. apply.SessionIDFor returns a tenant-scoped
// "tenant:base" form, and on NTFS "foo:bar.otlp.jsonl" does not create a file called that --
// it creates an alternate data stream on a file called "foo". No error, no visible file, no
// clue. Claude Code's own UUIDs are already safe; the fallback ids are what make this
// necessary. The rule lives in record.SafeFileName, which the offline converter shares, so
// a session's input and output files are always named alike.
func sanitizeTraceID(id string) string { return record.SafeFileName(id) }

func randomSpanID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("span id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
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
// keyed on a real session.
func (h *Handler) newRecCtx(r *http.Request, provider bschemas.ModelProvider, orig []byte, tn *Tenancy) recCtx {
	if !h.recordFor(tn) {
		return recCtx{}
	}
	id := session.ExplicitID(gjson.GetBytes(orig, "metadata.user_id").String())
	if id == "" {
		// Second choice: the proxy's own resolver, computed off the PRISTINE body. Tenant-
		// prefixed and possibly a content hash, so it can flip at compaction -- acceptable as
		// a fallback, not as the primary.
		if tn != nil {
			id = apply.SessionIDFor(tn.ID, r.Header.Get("x-context-guru-session"), provider, orig)
		}
		if id != "" {
			id = "cg-derived-" + id
		}
	}
	if id == "" {
		// Never empty: an empty id yields a hidden ".otlp.jsonl" file, and merges every unkeyed
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

// NewSessionRecorder is newSessionRecorder for main, which assembles Options from flags.
//
// The type stays unexported: it is proxy-internal machinery reached only through
// Options.Record, and main needs exactly two things from it -- construction and the resolved
// directory for the startup log.
func NewSessionRecorder(dir, endpoint string, bodyCap, teeCap, queue int, log *slog.Logger) (*sessionRecorder, error) {
	return newSessionRecorder(dir, endpoint, bodyCap, teeCap, queue, log)
}

// Dir is the absolute output directory, for the startup log. Resolved in the constructor, so
// what this returns is the path files actually land in and not the flag string.
func (r *sessionRecorder) Dir() string {
	if r == nil {
		return ""
	}
	return r.dir
}
