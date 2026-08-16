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
	"os"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/modes"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/session"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// debugTraffic, when CONTEXT_GURU_DEBUG is set, logs each inbound tool output's
// token count + first line so we can analyze why components did/didn't fire on
// real agent traffic. Diagnostic only.
var debugTraffic = os.Getenv("CONTEXT_GURU_DEBUG") != ""

func dumpToolOutputs(norm []bschemas.ChatMessage) {
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
		slog.Info("cg.debug.toolout", "tokens", schema.TextTokens(t), "lines", strings.Count(t, "\n")+1, "head", head)
	}
	slog.Info("cg.debug.request", "tool_outputs", tools, "total_tool_tokens", schema.MessagesTokens(&bschemas.BifrostChatRequest{Input: norm}))
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
	kind     slotKind
	path     string // sjson path: "messages.<i>" (whole) or "messages.<i>.content.<b>.content" (tool text)
	pre      []byte // wholeMessage: canonical marshal of the original message
	preText  string // anthropicToolText: original tool-output text (change detection)
	lossless bool   // wholeMessage: does bifrost round-trip this message without dropping fields
}

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

// metaSessionKeys are the request-body fields carrying the agent's OWN session id,
// in precedence order behind the header: `metadata.user_id` is Claude Code's (a JSON
// object string, unwrapped by session.ExplicitID) and `metadata.taskId` is Bob
// Shell's (a bare randomUUID, re-rolled only by /clear). Both survive the agent's
// context compaction, which the derived sha256(system+firstUser) does not — see
// session.ExplicitID for what breaks without them. A request carrying both resolves
// to user_id, by this order.
var metaSessionKeys = [...]string{"metadata.user_id", "metadata.taskId"}

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
func metaSessionIDs(body []byte) []string {
	out := make([]string, 0, len(metaSessionKeys))
	for _, k := range metaSessionKeys {
		if r := gjson.GetBytes(body, k); r.Type == gjson.String {
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
			slog.Error("context-guru: recovered from panic in BodyOpts; forwarding original request", "panic", r)
			res = Result{Body: body}
		}
	}()
	mode := o.Mode
	if mode == "" {
		mode = components.ModeSync
	}
	models := o.Models
	msgsRaw := gjson.GetBytes(body, "messages")
	if !msgsRaw.Exists() || !msgsRaw.IsArray() {
		// Assign rather than return a fresh Result: res already carries the trace fields
		// set above, and a bypassed request that also lacks a messages array must still
		// report itself as bypassed rather than as "no messages".
		res.Body = body
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
	if !bypass && pipe != nil && (pipe.Has("cachesplit") || pipe.Has("cacheinject")) {
		body, systemSplit = splitVolatileTail(body, provider)
	}

	norm, slots := normalize(provider, msgsRaw.Array())
	if len(norm) == 0 {
		res.Body, res.Changed = body, systemSplit // keep the split even with nothing to compact
		return res
	}

	if debugTraffic {
		dumpToolOutputs(norm)
	}
	chat := &bschemas.BifrostChatRequest{Provider: provider, Input: norm}
	sys, firstUser := schema.SessionHead(norm)
	sessionID := session.Scoped(o.Tenant, explicitSession(o.Session, body), sys, firstUser)
	cacheAware := resolveCacheAware(o.CacheMode, provider, body)
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
			maxCachedIdx = o.Tracker.Turn(sessionID, len(norm)) - 1
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
			o.Tracker.Turn(sessionID, len(norm))
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
		CacheAware:   cacheAware,
		MaxCachedIdx: maxCachedIdx,
		// Every breakpoint already on the wire — including the ones no component can
		// see (`system`, `tools`, and the marks our own normalize drops). The
		// provider's cap of four counts them all (issue #32, defect 2).
		ExistingBreakpoints: bps.Total(),
		Mode:                mode,
	}
	tr.Session, tr.CacheAware, tr.MaxCachedIdx, tr.Messages = sessionID, cacheAware, maxCachedIdx, len(norm)
	tr.Breakpoints = bps
	// The eligible (attempted) denominator: what age/supersession offloaders were
	// allowed to touch. Everything before MaxCachedIdx is frozen for cache safety —
	// the cost of that mechanism, reported next to its benefit.
	tr.AttemptedTokens = attemptedTokens(norm, c)

	// Canonical form of each normalized message BEFORE the pipeline, so a
	// count-changing component (summarize) can be mapped back to the body.
	normPre := make([][]byte, len(norm))
	for i := range norm {
		normPre[i], _ = json.Marshal(norm[i])
	}

	rr := pipe.Run(chat, c)
	tr.Run = rr
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
		nb, ok := rebuildCountChanged(body, msgsRaw.Array(), normPre, slots, chat.Input)
		if !ok && systemSplit {
			res.Body, res.Changed = body, true // keep the split even when the rebuild declined
			return res
		}
		res.Body, res.Changed = nb, ok || systemSplit
		return res
	}

	out := body
	// The tail split already rewrote `body`, so the result must be forwarded even
	// if no component changes a message.
	changed := systemSplit
	var changes []Change
	// Exact attribution for the diff view: which components rewrote each message, in
	// order. Built once from the run report the pipeline already produced.
	touched := touchedBy(rr)
	// Per-message count of changes this writeback threw away, attributed back to the
	// components that made them once the loop is done.
	discarded := map[int]int{}
	for i := range chat.Input {
		s := slots[i]
		switch s.kind {
		case anthropicToolText:
			newText := schema.MessageText(chat.Input[i])
			if newText == s.preText {
				continue
			}
			var err error
			if out, err = sjson.SetBytes(out, s.path, newText); err != nil {
				res.Body = body
				return res
			}
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
			if !s.lossless {
				// bifrost can't round-trip this message; splicing our re-marshal would
				// drop provider fields it doesn't model. But if the ONLY change is added
				// `cache_control` — metadata, not content — write those keys at their exact
				// paths on the original raw bytes: nothing else is read or rewritten, so no
				// provider field can be dropped. See metawrite.go (issue #32).
				if w, ok := metadataOnlyWrites(s.pre, post); ok {
					if nb, ok := applyMetaWrites(out, s.path, len(chat.Input[i].Content.ContentBlocks), w); ok {
						out = nb
						changed = true
						continue
					}
				}
				// Anything else: discard the change, keep the original bytes.
				// ponytail: correctness over the marginal saving here.
				discarded[i]++
				continue
			}
			if out, err = sjson.SetRawBytes(out, s.path, post); err != nil {
				res.Body = body
				return res
			}
			changed = true
			var pm bschemas.ChatMessage
			_ = json.Unmarshal(s.pre, &pm)
			changes = append(changes,
				mkChange(s.path, schema.MessageText(pm), schema.MessageText(chat.Input[i]), touched[i]))
		}
	}
	pipe.RecordDiscards(rr, discarded)
	tr.Changes = changes
	if changed && dumpPath != "" {
		dumpChanges(c.Session, changes)
	}
	// A cap breach WE caused is a bug and must be loud. A request that arrived already
	// over the cap is the client's to fix — we forward it as-is (fail open), and an ERROR
	// blaming context-guru for it would be a false alarm. So compare against the inbound
	// count and only shout when we added to an over-cap total.
	if n := wireBreakpoints(out); n > maxWireBreakpoints && n > c.ExistingBreakpoints {
		slog.Error("context-guru: cache breakpoint count exceeds the provider cap",
			"breakpoints", n, "inbound", c.ExistingBreakpoints,
			"cap", maxWireBreakpoints, "session", c.Session)
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
			m.Get("role").String() == string(bschemas.ChatMessageRoleUser) &&
			m.Get("content").IsArray() {
			handled := false
			for b, blk := range m.Get("content").Array() {
				if blk.Get("type").String() != "tool_result" {
					continue
				}
				add := func(text, path string) {
					handled = true
					norm = append(norm, toolMessage(text, blk.Get("tool_use_id").String()))
					slots = append(slots, slot{kind: anthropicToolText, path: path, preText: text})
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
		// Default: whole-message slot. Unmarshal via bifrost and record whether that
		// round-trips losslessly.
		var cm bschemas.ChatMessage
		if err := json.Unmarshal([]byte(m.Raw), &cm); err != nil {
			continue // unparseable message — leave it in the body untouched
		}
		preMarshal, _ := json.Marshal(cm)
		norm = append(norm, cm)
		slots = append(slots, slot{
			kind:     wholeMessage,
			path:     "messages." + strconv.Itoa(i),
			pre:      preMarshal,
			lossless: jsonEqual([]byte(m.Raw), preMarshal),
		})
	}
	return norm, slots
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
func rebuildCountChanged(body []byte, orig []gjson.Result, normPre [][]byte, slots []slot, out []bschemas.ChatMessage) ([]byte, bool) {
	// The rebuild emits ONLY slot-mapped messages, so a body message normalize skipped
	// (unparseable — it has no slot) would be silently DELETED from the forwarded
	// request. Deleting a message is an ALTERED request, not a fail-open one, so decline
	// the rebuild entirely and let the caller forward the original.
	covered := map[int]bool{}
	for _, s := range slots {
		if bi, ok := bodyIndexOf(s.path); ok {
			covered[bi] = true
		}
	}
	if len(covered) != len(orig) {
		return body, false
	}
	used := make([]bool, len(normPre))
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
		for k := range normPre {
			if !used[k] && bytes.Equal(mb, normPre[k]) {
				matched = k
				break
			}
		}
		if matched < 0 {
			parts = append(parts, mb) // new message (e.g. the summary) — fresh, lossless (plain text)
			continue
		}
		used[matched] = true
		bi, ok := bodyIndexOf(slots[matched].path)
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

// bodyIndexOf extracts the leading messages.<i> index from a slot path.
func bodyIndexOf(path string) (int, bool) {
	s := strings.TrimPrefix(path, "messages.")
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		s = s[:dot]
	}
	i, err := strconv.Atoi(s)
	return i, err == nil
}
