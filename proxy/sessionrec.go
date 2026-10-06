package proxy

// sessionRecorder writes claude-record-shaped trace files for --record.
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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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

// recordFlushEvery is how often a dirty session is rewritten to disk.
//
// Neither extreme works. Rewriting per turn is O(N^2) bytes over an N-turn session, and a
// long agent session is exactly the case this exists to capture. Buffering until Close
// loses everything to a kill -9, which on Windows is how a proxy normally stops -- and the
// session we most want is the long one that ends that way. claude-record also writes
// periodically, so matching it keeps the comparison honest about durability too.
const recordFlushEvery = 10 * time.Second

// recordFlushSpans forces a flush mid-interval once a session has this many unwritten
// spans, so a burst does not sit in memory for the rest of the tick.
const recordFlushSpans = 8

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

// recSession accumulates one session's spans. Owned exclusively by the writer goroutine,
// so it needs no mutex.
type recSession struct {
	traceID  string
	file     string // resolved once, so a restart collision is decided once
	spans    []record.Span
	lastSpan string // parent of the next turn: what threads a session together
	dirty    int    // spans added since the last flush
}

type sessionRecorder struct {
	dir     string
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

// newSessionRecorder returns nil when dir is empty, matching newCapture's nil-means-off
// contract so every call site is one nil check on the default path.
//
// MkdirAll happens HERE rather than at first write: a bad --record path must fail loudly at
// startup, not silently per request for the length of a session someone believed was being
// recorded.
func newSessionRecorder(dir string, bodyCap, teeCap, queue int, log *slog.Logger) (*sessionRecorder, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("record dir %q: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("record dir %q: %w", abs, err)
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
		dir: abs, bodyCap: bodyCap, teeCap: teeCap, log: log,
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

// Close drains what is queued, flushes every dirty session, and reports the counters.
func (r *sessionRecorder) Close() error {
	if r == nil {
		return nil
	}
	close(r.done)
	r.wg.Wait()
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
	tick := time.NewTicker(recordFlushEvery)
	defer tick.Stop()
	for {
		select {
		case e := <-r.ch:
			r.handle(e)
		case <-tick.C:
			r.flushAll(false)
		case <-r.done:
			// Drain what is already queued before the final flush: these are turns that
			// really happened, and the process is going away.
			for {
				select {
				case e := <-r.ch:
					r.handle(e)
				default:
					r.flushAll(true)
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
	s := r.sessions[e.traceID]
	if s == nil {
		s = &recSession{traceID: e.traceID, file: r.fileFor(e.traceID)}
		r.sessions[e.traceID] = s
	}
	turn, err := r.buildTurn(e, s.lastSpan)
	if err != nil {
		r.errors.Add(1)
		r.log.Warn("cg.record: could not translate a turn", "err", err, "trace_id", e.traceID)
		return
	}
	turn.Redact(r.bodyCap)
	s.spans = append(s.spans, turn.ToSpan())
	s.lastSpan = turn.SpanID
	s.dirty++
	r.written.Add(1)
	if s.dirty >= recordFlushSpans {
		r.flush(s)
	}
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

// flush writes one session's file. Atomic: a reader (or a crash) sees either the previous
// complete file or the new complete one, never a half-written JSON document.
func (r *sessionRecorder) flush(s *recSession) {
	if s == nil || s.dirty == 0 || len(s.spans) == 0 {
		return
	}
	b, err := record.NewFile(s.traceID, r.spansWithDerivedTurns(s), time.Now()).Marshal()
	if err != nil {
		r.errors.Add(1)
		r.log.Warn("cg.record: could not marshal a trace file", "err", err, "trace_id", s.traceID)
		return
	}
	tmp := s.file + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		r.errors.Add(1)
		r.log.Warn("cg.record: could not write a trace file", "err", err, "path", s.file)
		return
	}
	// Windows will not rename onto an existing file.
	_ = os.Remove(s.file)
	if err := os.Rename(tmp, s.file); err != nil {
		r.errors.Add(1)
		r.log.Warn("cg.record: could not rename a trace file", "err", err, "path", s.file)
		_ = os.Remove(tmp)
		return
	}
	s.dirty = 0
}

// spansWithDerivedTurns returns the session's observed spans followed by one derived-turn span
// per reconstructed human turn.
//
// WHY THIS IS COMPOSED AT WRITE TIME AND NOT STORED. ReconstructTurns needs EVERY call in the
// session at once: it groups calls by advertised tool set, drops roots below RootShare, and
// de-duplicates turns that later calls replay. None of that is decidable per-turn, so there is
// nothing correct to append as turns arrive. Recomputing from s.spans on each flush also makes
// the periodic flush idempotent by construction -- derived spans are never stored, so they can
// never be duplicated or left stale by a later flush.
//
// Span ids are DERIVED FROM THE SEQUENCE NUMBER, not random. A flush every 10 s with random ids
// would give the same turn a different id on every write, so a consumer diffing two reads of a
// live file would see churn where nothing changed.
//
// Fails open, like everything else here: on any trouble the observed spans are returned
// unchanged. A trace file missing its derived turns is a degraded file; a missing file is a
// lost session.
func (r *sessionRecorder) spansWithDerivedTurns(s *recSession) []record.Span {
	defer func() {
		if v := recover(); v != nil {
			r.errors.Add(1)
			r.log.Warn("cg.record: turn reconstruction panicked; wrote observed spans only",
				"panic", v, "trace_id", s.traceID)
		}
	}()
	if len(s.spans) == 0 {
		return s.spans
	}
	calls := make([]record.Call, 0, len(s.spans))
	for _, sp := range s.spans {
		if sp.Attributes == nil {
			continue
		}
		raw, _ := sp.Attributes[record.AttrInputMsgs].(string)
		if raw == "" {
			continue
		}
		var msgs []record.Message
		if err := json.Unmarshal([]byte(raw), &msgs); err != nil {
			continue
		}
		defs, _ := sp.Attributes[record.AttrToolDefs].(string)
		// Order must sort chronologically: the span's own start time does, and it is also the
		// closest thing to a timestamp a reconstructed turn can honestly carry.
		calls = append(calls, record.Call{Order: sp.StartTime, Messages: msgs, ToolDefs: defs})
	}
	turns := record.ReconstructTurns(calls)
	if len(turns) == 0 {
		return s.spans
	}
	out := make([]record.Span, 0, len(s.spans)+len(turns))
	out = append(out, s.spans...)
	for _, t := range turns {
		start, err := time.Parse(time.RFC3339Nano, t.FirstSeen)
		if err != nil {
			// FirstSeen is a span start time this function wrote, so a parse failure means the
			// format changed underneath us. Skip the turn rather than invent a timestamp.
			continue
		}
		out = append(out, record.DerivedTurnSpan(
			s.traceID, derivedSpanID(s.traceID, t.Sequence, t.Root), "", t, start))
	}
	return out
}

// derivedSpanID is a stable 8-byte id for one reconstructed turn. Deterministic in the session,
// the root and the sequence number, so a turn keeps its id across flushes.
func derivedSpanID(traceID string, seq int, root string) string {
	sum := sha256.Sum256([]byte(traceID + "\x00" + root + "\x00" + strconv.Itoa(seq)))
	return hex.EncodeToString(sum[:8])
}

func (r *sessionRecorder) flushAll(final bool) {
	for _, s := range r.sessions {
		r.flush(s)
	}
	if final {
		r.sessions = make(map[string]*recSession)
	}
}

// fileFor resolves a session's path, once.
//
// It REFUSES TO OVERWRITE a file this process did not write. A session that spans a proxy
// restart would otherwise be truncated silently: the new process starts with an empty span
// list and its first flush replaces a complete file with a one-span one. The analysis side
// globs *.otel.json, so a second file for the same session still loads; a clobbered one is
// unrecoverable.
func (r *sessionRecorder) fileFor(traceID string) string {
	safe := sanitizeTraceID(traceID)
	base := filepath.Join(r.dir, safe+".otel.json")
	if _, err := os.Stat(base); err != nil {
		return base
	}
	alt := filepath.Join(r.dir, fmt.Sprintf("%s.%d.otel.json", safe, os.Getpid()))
	r.log.Info("cg.record: a trace file for this session already exists (a restart, most likely); "+
		"writing alongside it rather than over it", "existing", base, "writing", alt)
	return alt
}

// sanitizeTraceID makes a session id safe as a filename.
//
// THE TRAP IS NTFS-SPECIFIC AND SILENT. apply.SessionIDFor returns a tenant-scoped
// "tenant:base" form, and on NTFS "foo:bar.otel.json" does not create a file called that --
// it creates an alternate data stream on a file called "foo". No error, no visible file, no
// clue. Claude Code's own UUIDs are already safe; the fallback ids are what make this
// necessary.
func sanitizeTraceID(id string) string {
	id = strings.TrimSpace(id)
	var b strings.Builder
	b.Grow(len(id))
	for _, ch := range id {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9',
			ch == '.', ch == '_', ch == '-':
			b.WriteRune(ch)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	// "." and ".." are directory entries, not names; an empty id would yield ".otel.json",
	// a hidden file that also merges every unkeyed session into one.
	if s == "" || strings.TrimLeft(s, ".") == "" {
		return "cg-unkeyed"
	}
	return s
}

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
	if h == nil || h.recorder == nil {
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
		// Never empty: an empty id yields ".otel.json", a hidden file that also merges every
		// unkeyed session into one.
		id = "cg-unkeyed-" + unkeyedSuffix()
	}
	// bytes.Clone, not an alias: the pipeline owns `body` from here and a future refactor that
	// mutated the backing array in place would silently corrupt every recorded request. ~200 KB
	// on an opt-in path buys the invariant outright.
	return recCtx{pristine: bytes.Clone(orig), traceID: id}
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
func NewSessionRecorder(dir string, bodyCap, teeCap, queue int, log *slog.Logger) (*sessionRecorder, error) {
	return newSessionRecorder(dir, bodyCap, teeCap, queue, log)
}

// Dir is the absolute output directory, for the startup log. Resolved in the constructor, so
// what this returns is the path files actually land in and not the flag string.
func (r *sessionRecorder) Dir() string {
	if r == nil {
		return ""
	}
	return r.dir
}
