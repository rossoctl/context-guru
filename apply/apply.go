// Package apply is the one place the pipeline meets a raw wire request, shared
// by every host adapter (the bifrost proxy and the AuthBridge plugin). It
// extracts the messages array, runs the pipeline on it, and splices the result
// back into the original body — byte-lossless for every other field (headroom
// invariant I1). This is what makes "one implementation behind both
// integrations" concrete: hosts differ only in how they obtain the body,
// provider, and session id.
//
// Provider normalization. Components operate on OpenAI-shaped tool outputs
// (role=="tool" messages with string content). The Anthropic Messages API
// instead carries tool outputs as `tool_result` content blocks INSIDE user
// messages — a shape bifrost's ChatContentBlock cannot even represent (it drops
// the payload on unmarshal). So for Anthropic requests we expand each
// tool_result block into a synthetic role=tool message the existing components
// already know how to shrink, run the pipeline, then splice each rewritten
// tool output back into its exact source block via sjson. Everything the
// pipeline did not touch stays byte-identical.
package apply

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/components/reformat"
	"github.com/rossoctl/context-guru/internal/logging"
	"github.com/rossoctl/context-guru/modes"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/session"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// dumpToolOutputs logs each inbound tool output's token count + first line so we can
// analyze why components did or didn't fire on real agent traffic.
//
// This used to log at INFO behind its own CONTEXT_GURU_DEBUG env var. It is a
// per-tool-output diagnostic, which is the definition of DEBUG, and every caller is
// now gated on the level instead — CONTEXT_GURU_DEBUG=1 still works, it just means
// CG_LOG_LEVEL=debug (see internal/logging).
func dumpToolOutputs(lg *slog.Logger, norm []bschemas.ChatMessage) {
	tools := 0
	for _, m := range norm {
		if m.Role != bschemas.ChatMessageRoleTool {
			continue
		}
		tools++
		t := schema.MessageText(m)
		head := strings.TrimSpace(t)
		if i := strings.IndexByte(head, '\n'); i >= 0 {
			head = head[:i]
		}
		if len(head) > 160 {
			head = head[:160]
		}
		lg.Debug("cg.toolout", "tokens", schema.TextTokens(t), "lines", strings.Count(t, "\n")+1, "head", head)
	}
	lg.Debug("cg.toolouts", "tool_outputs", tools, "total_tool_tokens", schema.MessagesTokens(&bschemas.BifrostChatRequest{Input: norm}))
}

// logDecisions writes one DEBUG line per component saying what it DECIDED and on
// what numbers — which is the whole reason this exists. "acted: 0" is the one number
// a diagnosis cannot use: it cannot tell a component with nothing to do from one
// whose guard is misfiring. So each line carries the verdict, the token delta, and
// the gates that turned candidates away.
//
// The gate names and counts here are read straight off the SAME Report.Gates map the
// /stats gate histogram (components.<name>.gates) is summed from, so a log line and
// the metric can never disagree — they are one source.
//
// Gates are rendered as one `name=n name=n` STRING rather than one attr per gate on
// purpose: an attribute key is checked against the credential-name denylist, so a
// future gate called `no_auth` or `bad_token` would have its count silently replaced
// by «redacted». As a value it is scrubbed as content, where a short integer after
// `=` matches nothing.
func logDecisions(lg *slog.Logger, rr *components.RunReport) {
	for _, rep := range rr.Components {
		verdict := "acted"
		switch {
		case rep.Reverted:
			verdict = "reverted" // error, panic, or the never-worse guard
		case rep.Skipped:
			verdict = "declined" // ran, chose not to act
		case rep.Saved() == 0:
			verdict = "no_change"
		}
		attrs := []any{
			"component", rep.Component, "kind", rep.Kind, "verdict", verdict,
			"tokens_before", rep.TokensBefore, "tokens_after", rep.TokensAfter,
			"saved", rep.Saved(), "duration_ms", rep.DurationMs,
			"changed_msgs", len(rep.ChangedIdx), "stashed", len(rep.CacheKeys),
		}
		if len(rep.Gates) > 0 {
			attrs = append(attrs, "gates", formatGates(rep.Gates))
		}
		if rep.Irreversible {
			attrs = append(attrs, "irreversible", true)
		}
		if rep.Err != nil {
			attrs = append(attrs, "err", rep.Err)
		}
		lg.Debug("cg.component", attrs...)
	}
	lg.Debug("cg.run", "components", len(rr.Components), "tokens_before", rr.TokensBefore,
		"tokens_after", rr.TokensAfter, "saved", rr.Saved(), "duration_ms", rr.DurationMs)
}

// formatGates renders a gate histogram as `name=n name=n`, sorted so two runs of the
// same traffic produce comparable lines.
func formatGates(g map[string]int) string {
	var b strings.Builder
	for i, name := range slices.Sorted(maps.Keys(g)) {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(name)
		b.WriteByte('=')
		b.WriteString(strconv.Itoa(g[name]))
	}
	return b.String()
}

// slotKind is how a normalized message maps back to the raw body.
type slotKind int

const (
	// wholeMessage: norm message i corresponds 1:1 to messages.<msgIdx>; a change
	// re-marshals the whole message (guarded by lossless round-trip).
	wholeMessage slotKind = iota
	// anthropicToolText: norm message is a synthetic role=tool extracted from an
	// Anthropic tool_result block; a change rewrites only that block's `content`
	// string field, which is byte-lossless for the rest of the message.
	anthropicToolText
)

// slot records how one normalized message writes back to the raw body.
type slot struct {
	kind    slotKind
	path    string // sjson path: "messages.<i>" (whole) or "messages.<i>.content.<b>.content" (tool text)
	pre     []byte // canonical marshal of the original (normalized) message
	preText string // anthropicToolText: original tool-output text (change detection)
	// raw is the BODY message's original bytes — for a tool-text slot, the whole user
	// message the tool_result block lives in, so several slots can share one raw.
	//
	// Two readers. The lossless check (jsonEqual: two unmarshals into `any` plus a
	// DeepEqual) only matters for a message a component actually CHANGED — a handful per
	// request — so it is computed on demand rather than for all ~70 messages up front,
	// where it cost ~12% of the rewrite path. And the writeback edits these bytes rather
	// than the whole body, so it needs no second parse to find them.
	raw string
}

// lossless reports whether bifrost round-trips this message without dropping fields,
// i.e. whether our re-marshal is safe to splice over the original bytes.
func (s slot) lossless() bool { return jsonEqual([]byte(s.raw), s.pre) }

// Trace is the per-request record of what BodyFull actually did: the resolved
// session, the pipeline's own run report (per-component accounting), the
// before/after text of every rewritten message, and the cache-awareness facts
// that decided which messages were even eligible. It is the dashboard's capture
// input — the same material CONTEXT_GURU_DUMP writes to a file, handed to a
// caller instead. Purely observational: nothing on it affects the rewrite.
type Trace struct {
	Session      string
	Bypassed     bool
	CacheAware   bool
	MaxCachedIdx int
	// Messages is the normalized message count this request carried.
	Messages int
	// AttemptedTokens is the token count of the messages age/supersession
	// offloaders were ALLOWED to touch (the uncached tail when cache-aware, the
	// whole request otherwise). It is the honest denominator for
	// "saved / attempted-to-compress"; TokensBefore−AttemptedTokens is the
	// compaction our own cache-safety mechanism deliberately gave up.
	AttemptedTokens int
	// FrozenTokens is TokensBefore−AttemptedTokens: the cost of cache safety.
	FrozenTokens int
	// Breakpoints is where this request's prompt-cache breakpoints sat ON ARRIVAL,
	// split by location. Observational, and free: the pipeline already counts them to
	// respect the provider's cap of four.
	Breakpoints Breakpoints
	// SplitTailHash identifies the VOLATILE half the split moved the breakpoint off. Compared
	// against the same session's previous request to decide whether the snapshot moved, which
	// is the turn on which the split is worth anything: with the block unsplit, a moved
	// snapshot re-creates the whole thing. 0 when nothing split.
	SplitTailHash uint64
	// SplitStableTokens is the token count of the half the volatile-tail split moved the
	// breakpoint onto — the tokens it moved out of the cache-creation tier. 0 when nothing
	// split. It is the honest numerator for the prefix-cache saving: the alternative,
	// crediting the request's whole cache_read, over-credits by whatever the agent's OTHER
	// breakpoints were already matching — 7.5x in dollars on a measured session.
	SplitStableTokens int
	// Run is the pipeline's aggregate report (nil when the pipeline never ran).
	Run *components.RunReport
	// Changes lists each rewritten message's before/after text (clipped).
	Changes []Change
}

// Body runs the pipeline with no LLM clients available (deterministic components
// only). See BodyWithModel to supply model clients for LLM-based components.
func Body(ctx context.Context, pipe *components.Pipeline, st store.Store, provider bschemas.ModelProvider, body []byte, explicitSession string, bypass bool) ([]byte, bool) {
	return BodyWithModel(ctx, pipe, st, provider, body, explicitSession, bypass, components.ModelSpec{})
}

// BodyWithModel runs the pipeline over the request body's messages and returns
// the rewritten body. changed=false means "forward the original unchanged" (no
// messages array, unparseable, or a re-serialization problem) — always fail
// open. explicitSession is the host-supplied session id ("" -> content hash).
// models carries the LLM clients that NeedsModel components may call.
func BodyWithModel(ctx context.Context, pipe *components.Pipeline, st store.Store, provider bschemas.ModelProvider, body []byte, explicitSession string, bypass bool, models components.ModelSpec) ([]byte, bool) {
	return BodyWithModelWindow(ctx, pipe, st, provider, body, explicitSession, bypass, models, 0)
}

// BodyWithModelWindow is BodyWithModel plus the model's resolved context window
// (max input tokens, 0 = unknown) so fraction-based Trigger thresholds can scale
// with the model. Hosts that resolve the window (the proxy, via internal/modelinfo)
// call this; window=0 reproduces the pre-D3 behavior exactly.
func BodyWithModelWindow(ctx context.Context, pipe *components.Pipeline, st store.Store, provider bschemas.ModelProvider, body []byte, explicitSession string, bypass bool, models components.ModelSpec, window int) ([]byte, bool) {
	return BodyFull(ctx, pipe, st, provider, body, explicitSession, bypass, models, window, "auto")
}

// BodyFull is BodyWithModelWindow plus the cache mode ("auto"|"on"|"off",
// default "auto") controlling cache-aware compaction. "auto" turns on
// cache-awareness when the backend is a prompt-caching provider or the request
// already carries cache_control breakpoints; "on" forces it; "off" restores the
// legacy compact-everything behavior (correct for confirmed non-caching backends).
func BodyFull(ctx context.Context, pipe *components.Pipeline, st store.Store, provider bschemas.ModelProvider, body []byte, explicitSession string, bypass bool, models components.ModelSpec, window int, cacheMode string) ([]byte, bool) {
	r := BodyOpts(ctx, pipe, st, Opts{
		Provider: provider, Body: body, Session: explicitSession, Bypass: bypass,
		Models: models, Window: window, CacheMode: cacheMode,
	})
	return r.Body, r.Changed
}

// metaSessionKeys are the `metadata` fields carrying the agent's OWN session id,
// in precedence order behind the header: `metadata.user_id` is Claude Code's (a JSON
// object string, unwrapped by session.ExplicitID) and `metadata.taskId` is Bob
// Shell's (a bare randomUUID, re-rolled only by /clear). Both survive the agent's
// context compaction, which the derived sha256(system+firstUser) does not — see
// session.ExplicitID for what breaks without them. A request carrying both resolves
// to user_id, by this order.
var metaSessionKeys = [...]string{"user_id", "taskId"}

// explicitSession resolves the explicit session id for one request: the header wins,
// then each metaSessionKeys field in order, then "" (Scoped's derived-hash fallback).
func explicitSession(header string, body []byte) string {
	cands := make([]string, 0, 1+len(metaSessionKeys))
	cands = append(cands, header)
	return session.ExplicitID(append(cands, metaSessionIDs(body)...)...)
}

// metaSessionIDs reads those fields off the raw body.
//
// gjson scans, so this costs no second unmarshal of the body on the request path — the
// same reason every other body-level read here (messages, wireBreakpoints) uses it. The
// type check is the whole robustness story: a `metadata` that is absent, null, a scalar
// or an array yields a non-existent result, and a value that is a number, object or
// array is not gjson.String — all of which yield "" and fall back to the derived hash
// rather than stringifying into a key like `map[]`.
//
// `metadata` is fetched ONCE and both fields read off it, rather than a full-body query
// per field: a gjson scan is cheap but not free, and on a real 600 KB request `metadata`
// sits behind `messages`, so each query walked the whole transcript to reach it.
func metaSessionIDs(body []byte) []string {
	md := gjson.GetBytes(body, "metadata")
	out := make([]string, 0, len(metaSessionKeys))
	for _, k := range metaSessionKeys {
		if r := md.Get(k); r.Type == gjson.String {
			out = append(out, r.Str)
		}
	}
	return out
}

// BodyOpts is the full entry point: everything BodyFull takes plus the operating mode
// (#31), the per-session boundary tracker, and the observational Trace the dashboard's
// capture path reads. Hosts that support modes call this; BodyFull is the positional
// shim every other caller keeps using.
//
// The rewrite is byte-identical whether or not anyone reads the trace: every trace
// field is filled from a value the rewrite already computed, and nothing branches on it.
func BodyOpts(ctx context.Context, pipe *components.Pipeline, st store.Store, o Opts) (res Result) {
	body, provider, bypass := o.Body, o.Provider, o.Bypass
	tr := &res.Trace
	tr.Bypassed = bypass
	// Top-level fail-open backstop: the per-component recover in pipeline.runOne only
	// covers component code. A panic anywhere else on the rewrite path (normalize, the
	// sjson splice, rebuildCountChanged, a marshal) must NOT 500 the client — forward
	// the original body unchanged. This makes CLAUDE.md's fail-open invariant hold for
	// the whole entry point, not just inside components.
	defer func() {
		if r := recover(); r != nil {
			// From(ctx), not the default logger: a panic is the line you most want attributed
			// to a tenant, and this runs before the session-bearing logger below exists.
			logging.From(ctx).Error("context-guru: recovered from panic in BodyOpts; "+
				"forwarding original request", "panic", r)
			res = Result{Body: body}
		}
	}()
	mode := o.Mode
	if mode == "" {
		mode = components.ModeSync
	}
	// Every line from here on carries the mode — the per-component decisions, the run
	// summary, whatever a component logs, and the panic recovery above (its closure reads
	// this same ctx variable, so the reassignment reaches it). One stamp on the logger
	// rather than an attr per call site: an observe-mode run is a PROJECTION of what
	// enforcing WOULD have saved, and a panel summing `saved` across the two without a way
	// to tell them apart is exactly the confusion the potential_* namespace exists to
	// prevent, reproduced in the logs. cg.cache_boundary used to pass mode as an attr of
	// its own; it does not any more, because that would now render the key twice.
	ctx = logging.With(ctx, logging.From(ctx).With("mode", string(mode)))
	models := o.Models
	// Tool-schema annotation strip: a top-level field the pipeline never sees (it
	// operates on `messages`). Done FIRST, before any byte offset into the body is
	// taken, because `tools` may be serialized either side of `messages` and a rewrite
	// after the fact would move every offset the writeback relies on.
	// See components/reformat/toolschema.go for the mechanism and the break-even.
	toolSchema := false
	if !bypass && pipe != nil && pipe.Has("toolschema") {
		body, toolSchema = reformat.CompactToolSchemas(body)
	}

	msgsRaw := gjson.GetBytes(body, "messages")
	if !msgsRaw.Exists() || !msgsRaw.IsArray() {
		// Assign rather than return a fresh Result: res already carries the trace fields
		// set above, and a bypassed request that also lacks a messages array must still
		// report itself as bypassed rather than as "no messages".
		res.Body, res.Changed = body, toolSchema
		return res
	}

	// Volatile-tail split, before anything else touches the body. This is a
	// body-level concern rather than a component one: the pipeline operates on
	// `messages`, but the block that needs splitting lives in the top-level `system`
	// array, which components never see. See prefixsplit.go for what it splits and why.
	//
	// Gated on `cachesplit` OR `cacheinject`. The two were coupled only because the split
	// had to hang off some existing config entry, but they are independent mechanisms
	// with very different evidence: the split is measured (−34.1% cost, 0% → 96.7% hit in
	// an isolated A/B), while breakpoint PLACEMENT has never been measured — so #32 drops
	// cacheinject from the default presets and puts `cachesplit` there instead. Without
	// its own gate the split would have gone down with cacheinject, turning "disable an
	// unproven component" into a real cost regression. It stays opt-in rather than
	// unconditional so `off` remains a true passthrough control for A/B runs.
	systemSplit := false
	// shiftAt/shift carry the split's effect on the rest of the body's byte offsets, so
	// the writeback can reuse the offsets in msgs (taken from the PRE-split body) rather
	// than re-parse the whole messages array to find each message again — that parse
	// measured ~3x the cost of the whole-body copy it was meant to save.
	shiftAt, shift := 0, 0
	// splitStableTokens is what the split rescued from the cache-creation tier; the
	// dashboard prices it (see dash.Event.cachesplitSavedUSD). 0 when nothing split.
	splitStableTokens := 0
	// splitTailHash identifies the volatile half, so a later turn can tell whether the
	// snapshot MOVED. Only on such a turn would the unsplit block have been re-created.
	var splitTailHash uint64
	if !bypass && pipe != nil && (pipe.Has("cachesplit") || pipe.Has("cacheinject")) {
		body, systemSplit, shiftAt, shift, splitStableTokens, splitTailHash =
			splitVolatileTail(body, provider)
	}

	// Parsed ONCE and shared: normalize, the count-changed rebuild and the writeback
	// splice all want the same array, and each Array() call re-walks the whole request.
	msgs := msgsRaw.Array()
	norm, slots := normalize(provider, msgs)
	if len(norm) == 0 {
		res.Body, res.Changed = body, systemSplit || toolSchema // keep the envelope rewrites even with nothing to compact
		return res
	}

	chat := &bschemas.BifrostChatRequest{Provider: provider, Input: norm}
	sys, firstUser := schema.SessionHead(norm)
	sessionID := session.Scoped(o.Tenant, explicitSession(o.Session, body), sys, firstUser)
	cacheAware := resolveCacheAware(o.CacheMode, provider, body)
	nowMs := o.nowMs()
	coldCache := false
	idleMs := int64(0)
	maxCachedIdx := -1
	if cacheAware && !bypass {
		// Messages present on the previous turn of this session are already committed
		// to the provider cache; only the new tail is being cache-written this turn.
		// Restrict supersession/age offloaders to that tail so they never mutate the
		// cached prefix. Growth-based (dialect-agnostic; needs no cache_control mapping).
		//
		// With a Tracker the boundary is read and this turn's recorded in ONE locked call.
		// The legacy path below read it from the store and wrote it back in a `defer`, so
		// two concurrent turns of one session raced on it — see the modes package comment.
		// Callers without a tracker (library users, /compact) keep the legacy path: same
		// numbers, same race, no behavior change for them.
		//
		// Both paths route the boundary through modes.Boundary, so a compaction (the
		// transcript SHRANK under a now-stable session id) restarts the prefix instead of
		// declaring the whole new, shorter transcript already-cached and freezing the rest
		// of the session. The store path needed it too: putLen writes len(norm)
		// unconditionally, so it self-healed on the FOLLOWING turn but still froze the
		// post-compaction one.
		if o.Tracker != nil {
			var prevAt int64
			maxCachedIdx, prevAt = o.Tracker.TurnAt(sessionID, len(norm), nowMs)
			maxCachedIdx--
			coldCache = cacheIsCold(prevAt, nowMs, cacheTTL(provider, body))
			if prevAt > 0 && nowMs > prevAt {
				idleMs = nowMs - prevAt
			}
		} else {
			maxCachedIdx = modes.Boundary(prevLen(st, sessionID), len(norm)) - 1
			defer putLen(st, sessionID, len(norm))
		}
	} else if cacheAware && bypass {
		// A bypassed turn still has to RECORD its length, even though it reads no
		// boundary and rewrites nothing.
		//
		// The provider caches whatever we forward, and a bypassed request is forwarded in
		// full — so those messages are committed to the cache exactly as a compacted
		// turn's would be. Skipping the record leaves the boundary at the last
		// non-bypassed turn's length, and the NEXT turn then treats the messages the
		// bypass passed through as mutable tail and rewrites them, diverging from the
		// prefix the provider just cached and forcing a full cache-write of the suffix.
		//
		// So the cost of a bypass is not confined to the bypassed request, which is what
		// the anti-latching argument in proxy/agentcompaction.go used to claim. It lands
		// on the following turn, and it is a cache-write rather than lost savings. That
		// matters because a false-positive bypass is reachable: the detector's phrase is
		// quoted verbatim in this repo's own docs/how-to/agent-compaction.md, so an agent
		// that reads that page gets it into a trailing tool_result.
		//
		// Recording is safe in both directions. On a genuine agent compaction the next
		// turn is shorter, so modes.Boundary resets anyway and this record is discarded.
		if o.Tracker != nil {
			// nowMs, not Turn(): a bypassed request is forwarded IN FULL, so the provider
			// cached it exactly as a compacted turn's would be — it is activity on this
			// session's prefix. Leaving the timestamp at the last non-bypassed turn made a
			// session that had been bypassing for ten minutes look ten minutes IDLE, and a
			// cold sweep would then rewrite a prefix the provider had just cached: the 1.25x
			// suffix re-write this whole design exists to avoid.
			o.Tracker.TurnAt(sessionID, len(norm), nowMs)
		} else {
			defer putLen(st, sessionID, len(norm))
		}
	}
	// Counted ONCE and used twice: the pipeline needs the total to respect the
	// provider's cap, and the dashboard records the per-location split. Two calls would
	// be two scans of the body on the request path for one fact.
	bps := CountBreakpoints(body)
	c := &components.Ctx{
		Ctx:          ctx,
		Session:      sessionID,
		Store:        st,
		Model:        models,
		Bypass:       bypass,
		CtxWindow:    o.Window,
		ModelName:    gjson.GetBytes(body, "model").String(),
		SelfRates:    o.SelfRates,
		CacheAware:   cacheAware,
		ColdCache:    coldCache,
		IdleMs:       idleMs,
		MaxCachedIdx: maxCachedIdx,
		// Every breakpoint already on the wire — including the ones no component can
		// see (`system`, `tools`, and the marks our own normalize drops). The
		// provider's cap of four counts them all (issue #32, defect 2).
		ExistingBreakpoints: bps.Total(),
		Mode:                mode,
		// Set BEFORE the run, so cachesplit's own report is right at the source and every
		// consumer of it agrees. Amending the report afterwards fixed the dashboard and
		// left /stats and the Prometheus component counters still saying "skipped",
		// because the pipeline emits each report to them as it goes.
		SystemSplit: systemSplit,
		ToolSchema:  toolSchema,
	}
	tr.Session, tr.CacheAware, tr.MaxCachedIdx, tr.Messages = sessionID, cacheAware, maxCachedIdx, len(norm)
	tr.Breakpoints = bps
	tr.SplitStableTokens, tr.SplitTailHash = splitStableTokens, splitTailHash
	// The eligible (attempted) denominator: what age/supersession offloaders were
	// allowed to touch. Everything before MaxCachedIdx is frozen for cache safety —
	// the cost of that mechanism, reported next to its benefit.
	tr.AttemptedTokens = attemptedTokens(norm, c)

	// From here on, everything this request logs carries the RESOLVED session. The
	// caller (the proxy) already put tenant and route on the context logger; the session
	// is only knowable here, after explicitSession + session.Scoped have run, which is
	// exactly why the logger travels in the context instead of being passed in whole.
	lg := logging.From(ctx).With("session", sessionID)
	debug := logging.Debugging(ctx)
	if debug {
		// The cached-prefix boundary decision, which is the single most common reason a
		// component "did nothing" on a request that obviously had something to compact:
		// everything at or below max_cached_idx is frozen, and frozen_tokens is what that
		// cost. Without this line the only visible symptom is a small saving.
		lg.Debug("cg.cache_boundary", "cache_aware", cacheAware, "max_cached_idx", maxCachedIdx,
			"messages", len(norm), "attempted_tokens", tr.AttemptedTokens,
			"existing_breakpoints", c.ExistingBreakpoints, "system_split", systemSplit,
			"bypassed", bypass, "ctx_window", o.Window,
			"cache_mode", o.CacheMode, "tracked", o.Tracker != nil)
		dumpToolOutputs(lg, norm)
	}

	// The canonical form of each normalized message BEFORE the pipeline — which a
	// count-changing component (summarize) needs to map survivors back to the body — is
	// already in slot.pre, filled by normalize. Marshalling every message a second time
	// here was pure duplicate work on every request, for a path only summarize takes.

	rr := pipe.Run(chat, c)
	tr.Run = rr
	if debug && rr != nil {
		logDecisions(lg, rr)
	}
	if rr != nil {
		tr.FrozenTokens = rr.TokensBefore - tr.AttemptedTokens
		if tr.FrozenTokens < 0 {
			tr.FrozenTokens = 0
		}
	}

	// A component changed the message count (summarize restructures the transcript
	// to [msg0, <summary>, last-K]). Rebuild the messages array preserving each
	// retained message's ORIGINAL raw bytes (byte-lossless, incl. Anthropic
	// tool_result) and marshaling only genuinely new messages (the summary).
	if len(chat.Input) != len(norm) {
		nb, ok := rebuildCountChanged(body, msgs, slots, chat.Input)
		if !ok && systemSplit {
			res.Body, res.Changed = body, true // keep the split even when the rebuild declined
			return res
		}
		res.Body, res.Changed = nb, ok || systemSplit || toolSchema
		return res
	}

	// The envelope rewrites (tail split, tool-schema strip) already rewrote `body`, so
	// the result must be forwarded even if no component changes a message.
	changed := systemSplit || toolSchema
	// Each changed message's new bytes, keyed by its index in the body's messages array,
	// spliced into the body in ONE pass after the loop (spliceMessages). The loop used to
	// sjson.SetBytes into the whole body per changed message, which copies the entire
	// request every time: 30 rewritten tool outputs on a 600 KB body meant ~18 MB of
	// copying to produce one 600 KB result. Editing each message's own bytes and splicing
	// once is O(body) instead of O(k*body), and byte-identical — sjson's splice is local.
	edits := map[int][]byte{}
	var changes []Change
	// Exact attribution for the diff view: which components rewrote each message, in
	// order. Built once from the run report the pipeline already produced.
	touched := touchedBy(rr)
	// Per-message count of changes this writeback threw away, attributed back to the
	// components that made them once the loop is done.
	discarded := map[int]int{}
	// The message's bytes as edited so far, and the rest of the slot path relative to it.
	// Resolved only for a message that actually CHANGED: several slots can share one body
	// message (an Anthropic user message with several tool_result blocks), and applying
	// their writes in order to the accumulating bytes is what the sequential whole-body
	// writes did. Doing this per changed message rather than per message matters — the
	// loop runs over all ~70, of which one or two change.
	editOf := func(path, raw string) (bi int, rel string, cur []byte, ok bool) {
		if bi, rel, ok = splitSlotPath(path); !ok {
			return 0, "", nil, false
		}
		if cur = edits[bi]; cur == nil {
			cur = []byte(raw)
		}
		return bi, rel, cur, true
	}
	for i := range chat.Input {
		s := slots[i]
		switch s.kind {
		case anthropicToolText:
			newText := schema.MessageText(chat.Input[i])
			if newText == s.preText {
				continue
			}
			bi, rel, cur, ok := editOf(s.path, s.raw)
			if !ok {
				res.Body = body
				return res
			}
			nb, err := sjson.SetBytes(cur, rel, newText)
			if err != nil {
				res.Body = body
				return res
			}
			edits[bi] = nb
			changed = true
			changes = append(changes, mkChange(s.path, s.preText, newText, touched[i]))
		default: // wholeMessage
			post, err := json.Marshal(chat.Input[i])
			if err != nil {
				res.Body = body
				return res
			}
			if bytes.Equal(post, s.pre) {
				continue // unmodified — keep the original bytes verbatim (I1)
			}
			bi, _, cur, ok := editOf(s.path, s.raw)
			if !ok {
				res.Body = body
				return res
			}
			if !s.lossless() {
				// bifrost can't round-trip this message; splicing our re-marshal would
				// drop provider fields it doesn't model. But if the ONLY change is added
				// `cache_control` — metadata, not content — write those keys at their exact
				// paths on the original raw bytes: nothing else is read or rewritten, so no
				// provider field can be dropped. See metawrite.go (issue #32).
				if w, ok := metadataOnlyWrites(s.pre, post); ok {
					if nb, ok := applyMetaWrites(cur, len(chat.Input[i].Content.ContentBlocks), w); ok {
						edits[bi] = nb
						changed = true
						continue
					}
				}
				// Anything else: discard the change, keep the original bytes.
				// ponytail: correctness over the marginal saving here.
				discarded[i]++
				continue
			}
			// The whole message value is replaced, so the edit IS the new bytes — the
			// sjson.SetRawBytes(body, "messages.<i>", post) this replaces did exactly
			// that splice, at the cost of copying the whole body.
			edits[bi] = post
			changed = true
			var pm bschemas.ChatMessage
			_ = json.Unmarshal(s.pre, &pm)
			changes = append(changes,
				mkChange(s.path, schema.MessageText(pm), schema.MessageText(chat.Input[i]), touched[i]))
		}
	}
	pipe.RecordDiscards(rr, discarded)
	if debug && len(discarded) > 0 {
		// The component worked and the request went out unchanged anyway, because bifrost
		// cannot round-trip that message and splicing our re-marshal would drop provider
		// fields. This is the issue-#32 class of silent misfire.
		//
		// DEBUG rather than WARN even though it IS a degradation: it is a property of the
		// provider's message shape, so on some upstreams it fires on every request, and a
		// warning on every request is a warning nobody reads. The alerting path is the
		// per-component `discarded_changes` counter, which /stats already exports.
		lg.Debug("cg.writeback_discarded", "messages", len(discarded))
	}
	// One splice for every edited message, over the post-split body. Fail-open: if the
	// body's own bytes do not confirm a message's span, forward the request unchanged
	// rather than splice at a guessed offset.
	out := body
	if len(edits) > 0 {
		nb, ok := spliceMessages(body, msgs, edits, shiftAt, shift)
		if !ok {
			res.Body = body
			return res
		}
		out = nb
	}
	tr.Changes = changes
	if changed && dumpPath != "" {
		dumpChanges(c.Session, changes)
	}
	// A cap breach WE caused is a bug and must be loud. A request that arrived already
	// over the cap is the client's to fix — we forward it as-is (fail open), and an ERROR
	// blaming context-guru for it would be a false alarm. So compare against the inbound
	// count and only shout when we added to an over-cap total.
	// Recounted only when something was actually rewritten: an untouched body still
	// carries exactly the inbound count, and `n > inbound` is then false by construction —
	// so the check is unchanged and the second full count is skipped.
	if changed {
		if n := wireBreakpoints(out); n > maxWireBreakpoints && n > c.ExistingBreakpoints {
			// lg already carries the session, so this line does not repeat it.
			lg.Error("context-guru: cache breakpoint count exceeds the provider cap",
				"breakpoints", n, "inbound", c.ExistingBreakpoints, "cap", maxWireBreakpoints)
		}
	}
	res.Body, res.Changed = out, changed
	return res
}

// resolveCacheAware decides whether cache-aware compaction is active for this
// request. "off" disables it; "on" forces it; "auto" (default) enables it when the
// backend is a prompt-caching provider OR the request already carries cache_control
// breakpoints (so we assume caching even when the provider isn't in the static set —
// covers "backend caches but we can't see it from the provider name").
func resolveCacheAware(mode string, provider bschemas.ModelProvider, body []byte) bool {
	switch mode {
	case "off":
		return false
	case "on":
		return true
	default: // "auto" / ""
		switch provider {
		case bschemas.Anthropic, bschemas.Bedrock, bschemas.BedrockMantle, bschemas.Vertex:
			return true
		}
		return hasCacheBreakpoint(body)
	}
}

// Prompt-cache lifetimes, per provider, for deciding whether a session that has been idle
// still has a cached prefix at all.
//
// THE SAFE DIRECTION IS TO OVER-ESTIMATE. Believing a cache is cold when it is still warm
// is the expensive mistake: a component that then rewrites deep history invalidates a live
// prefix and forces a cache-WRITE of the whole suffix at 1.25x the fresh rate — precisely
// the churn the tail gate exists to prevent. Believing it is warm when it has actually
// expired only forgoes an opportunity. So every number here is an UPPER bound.
const (
	// anthropicDefaultTTL is the implicit lifetime of a bare {"type":"ephemeral"} mark.
	// Every real captured Claude Code breakpoint is exactly that shape — no ttl field in
	// any of ~5,000 captured requests — so this is the common case, not the fallback.
	anthropicDefaultTTL = 5 * time.Minute
	// extendedTTL is the lifetime an explicit ttl:"1h" asks for, and also the outer bound
	// used where a provider caches automatically and declares no lifetime at all
	// (OpenAI-shaped backends: documented as clearing after minutes of inactivity and
	// always within the hour).
	extendedTTL = time.Hour
	// coldMargin is added to the TTL before calling a prefix cold, covering clock skew
	// between this box and the provider and the gap between when a request was recorded
	// here and when the provider last touched the entry.
	coldMargin = time.Minute
)

// cacheTTL returns how long this request's prompt cache should be assumed to live.
//
// For the Anthropic family the request itself declares it, so this is exact rather than a
// guess: the LONGEST ttl among the breakpoints wins, because any one of them being 1h means
// part of the prefix may still be warm.
func cacheTTL(provider bschemas.ModelProvider, body []byte) time.Duration {
	switch provider {
	case bschemas.Anthropic, bschemas.Bedrock, bschemas.BedrockMantle, bschemas.Vertex:
		if bodyAsksExtendedTTL(body) {
			return extendedTTL
		}
		return anthropicDefaultTTL
	default:
		return extendedTTL
	}
}

// bodyAsksExtendedTTL reports whether any cache_control on the wire asks for the 1h tier.
// Structural, for the same reason hasCacheBreakpoint is: a tool output containing the text
// "1h" must not extend our idea of the cache lifetime.
func bodyAsksExtendedTTL(body []byte) bool {
	for _, p := range []string{
		"messages.#.content.#.cache_control.ttl",
		"messages.#.cache_control.ttl",
		"system.#.cache_control.ttl",
		"tools.#.cache_control.ttl",
	} {
		found := false
		gjson.GetBytes(body, p).ForEach(func(_, v gjson.Result) bool {
			if v.IsArray() {
				v.ForEach(func(_, vv gjson.Result) bool {
					if vv.String() == "1h" {
						found = true
					}
					return !found
				})
			}
			if v.String() == "1h" {
				found = true
			}
			return !found
		})
		if found {
			return true
		}
	}
	return false
}

// cacheIsCold reports whether this session has been idle long enough that its prompt cache
// is certainly gone.
//
// Returns FALSE when there is no previous turn on record (prevAtMs == 0). That is a new
// session, or one the tracker evicted, or the first turn after a proxy restart — all
// "unknown", and treating unknown as cold would invalidate a warm prefix on every restart.
func cacheIsCold(prevAtMs, nowMs int64, ttl time.Duration) bool {
	if prevAtMs <= 0 || nowMs <= prevAtMs {
		return false
	}
	return time.Duration(nowMs-prevAtMs)*time.Millisecond >= ttl+coldMargin
}

// hasCacheBreakpoint reports whether the request carries a REAL prompt-cache
// breakpoint — a structural cache_control / cachePoint field on a message, content
// block, system block, or tool. This is deliberately structural (gjson path queries),
// not a substring scan of the whole body: a tool output whose text merely contains the
// string "cache_control" must NOT flip the request into cache-aware mode (that would
// wrongly restrict offloaders to the tail on a non-caching backend). A key inside a
// JSON string value never matches these paths.
func hasCacheBreakpoint(body []byte) bool {
	paths := []string{
		"messages.#.content.#.cache_control",
		"messages.#.cache_control",
		"system.#.cache_control",
		"tools.#.cache_control",
		"messages.#.content.#.cachePoint",
	}
	for _, p := range paths {
		r := gjson.GetBytes(body, p)
		if !r.Exists() {
			continue
		}
		found := false
		r.ForEach(func(_, v gjson.Result) bool {
			// nested arrays (content-of-messages) surface as arrays here; recurse one level
			if v.IsArray() {
				v.ForEach(func(_, vv gjson.Result) bool {
					if vv.IsObject() {
						found = true
						return false
					}
					return true
				})
			} else if v.IsObject() {
				found = true
			}
			return !found
		})
		if found {
			return true
		}
	}
	return false
}

// prevLen / putLen track, per session, how many normalized messages the previous
// turn carried — the boundary between the already-cached prefix and this turn's
// uncached tail. Stored in the same Store as offload state (TTL+LRU); a miss (first
// turn / expired) yields 0 so the whole request is treated as tail.
func prevLen(st store.Store, session string) int {
	b, ok := st.Get("cg:len:" + session)
	if !ok || len(b) == 0 {
		return 0
	}
	n := 0
	for _, ch := range b {
		if ch < '0' || ch > '9' {
			return 0
		}
		n = n*10 + int(ch-'0')
	}
	return n
}

func putLen(st store.Store, session string, n int) {
	st.Put("cg:len:"+session, []byte(strconv.Itoa(n)))
}

// attemptedTokens sums the tokens of the messages an age/supersession offloader
// was allowed to touch this turn (Ctx.TailOnly). With cache-awareness off it is
// the whole request; with it on it is the uncached tail, and the difference is
// what cache safety cost us in foregone compaction.
func attemptedTokens(norm []bschemas.ChatMessage, c *components.Ctx) int {
	n := 0
	for i := range norm {
		if c.TailOnly(i) {
			n += schema.TextTokens(schema.MessageText(norm[i]))
		}
	}
	return n
}

// Change is one rewritten message, captured for the CONTEXT_GURU_DUMP trace and
// for the dashboard's before/after diff view, so a human can see exactly what
// context-guru did to the wire.
type Change struct {
	Path         string `json:"path"`
	BeforeTokens int    `json:"before_tokens"`
	AfterTokens  int    `json:"after_tokens"`
	Before       string `json:"before"`
	After        string `json:"after"`
	// Components names which components rewrote this message, IN THE ORDER THEY
	// TOUCHED IT. A LIST, not a single id, and that is not a hedge: several components
	// routinely rewrite the same message in sequence (a reformatter then an offloader),
	// and the before/after pair here is their cumulative result — a single field would
	// have to name a winner and would be wrong for every other toucher.
	//
	// A REVERTED component is absent. The pipeline only records indices for a run that
	// survived its guards (see components.runOne), so a component rolled back by an
	// error, a panic, or the never-worse rule is never credited with the change.
	Components []string `json:"components,omitempty"`
}

func mkChange(path, before, after string, comps []string) Change {
	return Change{
		Path: path, BeforeTokens: schema.TextTokens(before), AfterTokens: schema.TextTokens(after),
		Before: clip(before, 4000), After: clip(after, 4000), Components: comps,
	}
}

// touchedBy inverts the run report into "normalized message index -> the components
// that changed it, in pipeline order". Report.ChangedIdx is already computed by the
// pipeline for the discard attribution, so this costs one pass over indices that
// already exist — no extra comparison of message text on the hot path.
func touchedBy(rr *components.RunReport) map[int][]string {
	if rr == nil {
		return nil
	}
	var out map[int][]string
	for _, rep := range rr.Components {
		for _, i := range rep.ChangedIdx {
			if out == nil {
				out = map[int][]string{}
			}
			out[i] = append(out[i], rep.Component)
		}
	}
	return out
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// truncate on a rune boundary so the trace stays valid UTF-8
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…[+" + strconv.Itoa(len(s)-n) + " bytes]"
}

var dumpPath = os.Getenv("CONTEXT_GURU_DUMP")

// dumpChanges appends one JSON line describing this request's rewrites.
func dumpChanges(session string, changes []Change) {
	f, err := os.OpenFile(dumpPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	before, after := 0, 0
	for _, c := range changes {
		before += c.BeforeTokens
		after += c.AfterTokens
	}
	rec := map[string]any{
		"session": session, "n_changed": len(changes),
		"tokens_before": before, "tokens_after": after, "saved": before - after,
		"changes": changes,
	}
	if b, err := json.Marshal(rec); err == nil {
		f.Write(append(b, '\n'))
	}
}

// normalize builds the message slice the pipeline runs on plus a write-back slot
// per message. For OpenAI (and any non-Anthropic dialect) every message maps
// 1:1 to a whole-message slot — the request is already in the shape components
// expect. For Anthropic, each user-message `tool_result` block with string
// content becomes a synthetic role=tool message with a text-field write-back
// slot; the block's siblings and every other message are left for the raw body
// to carry verbatim.
func normalize(provider bschemas.ModelProvider, arr []gjson.Result) (norm []bschemas.ChatMessage, slots []slot) {
	for i, m := range arr {
		if provider == bschemas.Anthropic &&
			m.Get("role").String() == string(bschemas.ChatMessageRoleUser) {
			// Fetched once, not twice (IsArray, then Array): every Get re-scans the whole
			// message, and these messages carry the real tool_result payloads (tens of KB).
			blocks := m.Get("content")
			handled := false
			// IsArray stays a guard, not just a shape hint: Array() on a bare object
			// would yield that object as element 0 and mint a `content.0.content`
			// write-back path the body has no such index for.
			blks := []gjson.Result(nil)
			if blocks.IsArray() {
				blks = blocks.Array()
			}
			for b, blk := range blks {
				if blk.Get("type").String() != "tool_result" {
					continue
				}
				add := func(text, path string) {
					handled = true
					tm := toolMessage(text, blk.Get("tool_use_id").String())
					pre, _ := json.Marshal(tm)
					norm = append(norm, tm)
					slots = append(slots, slot{
						kind: anthropicToolText, path: path, preText: text, pre: pre, raw: m.Raw,
					})
				}
				base := "messages." + strconv.Itoa(i) + ".content." + strconv.Itoa(b) + ".content"
				content := blk.Get("content")
				if content.Type == gjson.String {
					add(content.String(), base)
					continue
				}
				// The Messages API also permits an ARRAY of content blocks, and many clients
				// emit that shape. Extract each TEXT block as its own synthetic tool message
				// with a write-back slot one level deeper — otherwise the whole message fell
				// to the whole-message slot, which bifrost cannot model, and 100% of that
				// request's tool output was silently uncompactable. Non-text blocks (images,
				// …) are skipped and left in the body untouched: never lose one.
				if content.IsArray() {
					for k, cb := range content.Array() {
						if cb.Get("type").String() != "text" || cb.Get("text").Type != gjson.String {
							continue
						}
						add(cb.Get("text").String(), base+"."+strconv.Itoa(k)+".text")
					}
				}
			}
			if handled {
				continue // this user message contributed its tool_result blocks; body carries the rest
			}
		}
		// Default: whole-message slot. Unmarshal via bifrost, keeping the raw bytes for
		// the (lazy) lossless check.
		//
		// UnmarshalJSON is called DIRECTLY rather than through encoding/json.Unmarshal:
		// bifrost's ChatMessage implements json.Unmarshaler, so encoding/json's only
		// contribution is a full checkValid re-scan of bytes gjson has already parsed —
		// ~40% of the unmarshal cost, and a third of it on the biggest messages in the
		// request. A malformed message still fails (sonic reports it) and is still left in
		// the body untouched.
		var cm bschemas.ChatMessage
		if err := cm.UnmarshalJSON([]byte(m.Raw)); err != nil {
			continue // unparseable message — leave it in the body untouched
		}
		attachToolUse(&cm, m)
		preMarshal, _ := json.Marshal(cm)
		norm = append(norm, cm)
		slots = append(slots, slot{
			kind: wholeMessage,
			path: "messages." + strconv.Itoa(i),
			pre:  preMarshal,
			raw:  m.Raw,
		})
	}
	return norm, slots
}

// attachToolUse lifts an Anthropic assistant turn's `tool_use` blocks into bifrost's
// OpenAI-shaped ToolCalls, so the normalized transcript exposes WHICH tool produced
// each tool_result in both dialects (schema.ToolCalls pairs them by id).
//
// It is needed because bifrost's chat schema does not model `tool_use`: unmarshalling
// `{"type":"tool_use","id":…,"name":…,"input":{…}}` keeps the block's TYPE and drops
// its id, name and input entirely. Every capture of real Claude Code traffic is this
// dialect, so without the lift a command-keyed filter would only ever fire on
// OpenAI-shaped requests.
//
// Purely additive to the NORMALIZED view: the raw body is untouched, and a message
// that no component changes is still emitted from its original bytes (the writeback
// compares the post-pipeline marshal against slot.pre, which is taken after this
// runs). Such a message is already not round-trip-lossless — the dropped tool_use
// fields are why — so this changes no writeback decision.
func attachToolUse(cm *bschemas.ChatMessage, m gjson.Result) {
	if cm.Role != bschemas.ChatMessageRoleAssistant {
		return
	}
	content := m.Get("content")
	if !content.IsArray() {
		return
	}
	var calls []bschemas.ChatAssistantMessageToolCall
	for _, blk := range content.Array() {
		if blk.Get("type").String() != "tool_use" {
			continue
		}
		id, name := blk.Get("id").String(), blk.Get("name").String()
		fn := bschemas.ChatAssistantMessageToolCallFunction{Arguments: blk.Get("input").Raw}
		if name != "" {
			fn.Name = &name
		}
		call := bschemas.ChatAssistantMessageToolCall{Index: uint16(len(calls)), Function: fn}
		if id != "" {
			call.ID = &id
		}
		calls = append(calls, call)
	}
	if len(calls) == 0 {
		return
	}
	if cm.ChatAssistantMessage == nil {
		cm.ChatAssistantMessage = &bschemas.ChatAssistantMessage{}
	}
	cm.ChatAssistantMessage.ToolCalls = append(cm.ChatAssistantMessage.ToolCalls, calls...)
}

// toolMessage builds a synthetic OpenAI-shaped tool message from an Anthropic
// tool_result so the (provider-agnostic) components can process it.
func toolMessage(text, toolUseID string) bschemas.ChatMessage {
	m := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleTool}
	schema.SetMessageText(&m, text)
	if toolUseID != "" {
		id := toolUseID
		m.ChatToolMessage = &bschemas.ChatToolMessage{ToolCallID: &id}
	}
	return m
}

// jsonEqual reports whether two JSON documents are semantically equal (ignoring
// key order and whitespace). Used to decide whether bifrost's schema round-trips
// a message without dropping fields.
func jsonEqual(a, b []byte) bool {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// rebuildCountChanged reconstructs the messages array after a component changed
// the message count. Each output message that byte-matches a pre-pipeline
// normalized message (a survivor) is emitted as its ORIGINAL body raw bytes
// (byte-lossless); genuinely new messages (the summary) are marshaled fresh.
// Fail-open (returns body,false) if any survivor can't be mapped to the body.
func rebuildCountChanged(body []byte, orig []gjson.Result, slots []slot, out []bschemas.ChatMessage) ([]byte, bool) {
	// The rebuild emits ONLY slot-mapped messages, so a body message normalize skipped
	// (unparseable — it has no slot) would be silently DELETED from the forwarded
	// request. Deleting a message is an ALTERED request, not a fail-open one, so decline
	// the rebuild entirely and let the caller forward the original.
	covered := map[int]bool{}
	for _, s := range slots {
		if bi, _, ok := splitSlotPath(s.path); ok {
			covered[bi] = true
		}
	}
	if len(covered) != len(orig) {
		return body, false
	}
	used := make([]bool, len(slots))
	var parts [][]byte
	// emitted guards against emitting one body message TWICE: several normalized
	// messages can share a body index (an Anthropic user message with several
	// tool_result blocks), and a count-changing component may leave them
	// non-contiguous — which duplicated the raw message, and with it its tool_use_id.
	emitted := map[int]bool{}
	for i := range out {
		mb, err := json.Marshal(out[i])
		if err != nil {
			return body, false
		}
		matched := -1
		for k := range slots {
			if !used[k] && bytes.Equal(mb, slots[k].pre) {
				matched = k
				break
			}
		}
		if matched < 0 {
			parts = append(parts, mb) // new message (e.g. the summary) — fresh, lossless (plain text)
			continue
		}
		used[matched] = true
		bi, _, ok := splitSlotPath(slots[matched].path)
		if !ok || bi < 0 || bi >= len(orig) {
			return body, false
		}
		if emitted[bi] {
			continue // several normalized messages share one body message — emit it once
		}
		emitted[bi] = true
		parts = append(parts, []byte(orig[bi].Raw))
	}
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, p := range parts {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(p)
	}
	buf.WriteByte(']')
	res, err := sjson.SetRawBytes(body, "messages", buf.Bytes())
	if err != nil {
		return body, false
	}
	return res, true
}

// splitSlotPath splits a slot path into the body message index and the remainder of
// the path RELATIVE to that message: "messages.3.content.2.content" -> 3,
// "content.2.content". A whole-message slot has an empty remainder.
func splitSlotPath(path string) (idx int, rel string, ok bool) {
	s, found := strings.CutPrefix(path, "messages.")
	if !found {
		return 0, "", false
	}
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		s, rel = s[:dot], s[dot+1:]
	}
	i, err := strconv.Atoi(s)
	if err != nil {
		return 0, "", false
	}
	return i, rel, true
}

// spliceMessages rebuilds body with each edited message's bytes swapped in at that
// message's exact byte span, in a single pass.
//
// This replaces one sjson.SetBytes over the WHOLE body per changed message. That was
// O(k * body): sjson allocates and copies the entire request for every write, so 30
// rewritten tool outputs on a 600 KB body copied ~18 MB to produce one 600 KB result.
//
// The output is byte-identical to those sequential writes, not merely equivalent JSON,
// because sjson's splice is purely local: appendRawPaths (sjson@v1.2.5) emits
// json[:res.Index], then recurses into that subtree's own Raw, then appends
// json[res.Index+len(res.Raw):]. Editing a message's bytes and putting them back at the
// message's span therefore produces the same bytes as editing at "messages.<i>.<rest>"
// on the whole body. Everything outside the edited spans — key order, whitespace, every
// untouched message — is copied verbatim, which is what invariant I1 (unmodified
// messages keep their original bytes) requires.
//
// gjson documents Index as "index of raw value in original json, zero means index
// unknown", and it is relative to whatever json the Result was parsed from. So the span
// is never trusted on its word: the body's own bytes at that span must equal the
// message's Raw, or we decline (ok=false) and the caller forwards the original request.
func spliceMessages(body []byte, msgs []gjson.Result, edits map[int][]byte, shiftAt, shift int) ([]byte, bool) {
	idx := slices.Sorted(maps.Keys(edits))
	out := make([]byte, 0, len(body))
	prev := 0
	for _, i := range idx {
		if i < 0 || i >= len(msgs) {
			return nil, false
		}
		m := msgs[i]
		start := m.Index
		if start >= shiftAt {
			start += shift // the volatile-tail split moved this message
		}
		end := start + len(m.Raw)
		// start <= prev also rejects an unset (0) Index and any overlap, so the spans are
		// strictly increasing and disjoint — the messages array is walked once.
		if start <= prev || end > len(body) || string(body[start:end]) != m.Raw {
			return nil, false
		}
		out = append(out, body[prev:start]...)
		out = append(out, edits[i]...)
		prev = end
	}
	return append(out, body[prev:]...), true
}
