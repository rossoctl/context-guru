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

// BodyOpts is the full entry point: everything BodyFull takes plus the operating mode
// (#31) and the per-session generation snapshot async mode needs. Hosts that support
// modes call this; BodyFull is the positional shim every other caller keeps using.
func BodyOpts(ctx context.Context, pipe *components.Pipeline, st store.Store, o Opts) (res Result) {
	body, provider, bypass := o.Body, o.Provider, o.Bypass
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
	// Async, on the REQUEST path: replay only decisions that are already computed. The
	// expensive part of a compaction is the LLM call, which is the entire reason async
	// exists, so the inline pass gets no model clients and every NeedsModel component
	// degrades to its deterministic path or no-ops (that degradation is already a
	// documented contract). The off-path job (Deferred) gets the clients.
	if mode == components.ModeAsync && !o.Deferred {
		models = components.ModelSpec{}
	}
	msgsRaw := gjson.GetBytes(body, "messages")
	if !msgsRaw.Exists() || !msgsRaw.IsArray() {
		return Result{Body: body}
	}

	// Volatile-tail split, before anything else touches the body. This is a
	// body-level concern rather than a component one: the pipeline operates on
	// `messages`, but the block that needs splitting lives in the top-level `system`
	// array, which components never see. Gated on cacheinject being configured, so it
	// is opt-in via the same pipeline entry and adds no new config surface. See
	// prefixsplit.go for what it splits and why.
	systemSplit := false
	if !bypass && pipe != nil && pipe.Has("cacheinject") {
		body, systemSplit = splitVolatileTail(body, provider)
	}

	norm, slots := normalize(provider, msgsRaw.Array())
	if len(norm) == 0 {
		return Result{Body: body, Changed: systemSplit} // keep the split even with nothing to compact
	}

	if debugTraffic {
		dumpToolOutputs(norm)
	}
	chat := &bschemas.BifrostChatRequest{Provider: provider, Input: norm}
	sys, firstUser := systemAndFirstUser(norm)
	sessionID := session.Resolve(o.Session, sys, firstUser)
	cacheAware := resolveCacheAware(o.CacheMode, provider, body)
	// Turn accounting is independent of cache mode: the generation counts TURNS, and a
	// turn happens whether or not the backend caches. Deriving it inside the cache-aware
	// branch left every generation at 0 with cache_mode: off, which both disabled the
	// stale guard and collided with 0's use as "nothing pending".
	if o.Tracker != nil && !o.Deferred {
		pl, gen := o.Tracker.Turn(sessionID, len(norm))
		res.PrevLen, res.Generation = pl, gen
	}
	maxCachedIdx := -1
	if cacheAware && !bypass {
		// Messages present on the previous turn of this session are already committed
		// to the provider cache; only the new tail is being cache-written this turn.
		// Restrict supersession/age offloaders to that tail so they never mutate the
		// cached prefix. Growth-based (dialect-agnostic; needs no cache_control mapping).
		//
		// The boundary comes from the Tracker when the host supplies one: it reads the
		// previous length and records this turn's in ONE locked call, which is what
		// removes the concurrent-turn race the old read-then-deferred-write had
		// (#31/#25). Without a tracker (library callers, /compact) the legacy store path
		// stands — same numbers, same race, no behavior change for them.
		switch {
		case o.PrevLen != nil:
			maxCachedIdx = *o.PrevLen - 1
		case o.Tracker != nil:
			maxCachedIdx = res.PrevLen - 1 // recorded above, in one locked call
		default:
			maxCachedIdx = prevLen(st, sessionID) - 1
			defer putLen(st, sessionID, len(norm))
		}
	}
	// Async cache policy: while a compaction for this session is queued but not landed,
	// the un-compacted tail is about to be REPLACED, so no breakpoint may be committed
	// at or beyond it (see components.Ctx.NoCacheAtOrAfter). CacheUncompactedTail=true
	// is the escape hatch for a confirmed non-caching backend, where the protection buys
	// nothing.
	//
	// Three conditions beyond "async", each one a bug found in review:
	//
	//   - cacheAware. With cache_mode: off there is no cached prefix to protect and no
	//     boundary to protect it at, so blocking breakpoints would suppress caching
	//     forever for nothing (the two knobs interacted backwards).
	//   - a boundary that exists. On a session's FIRST turn prevLen is 0, so the whole
	//     request is "tail" and blocking it wrote zero breakpoints — on precisely the
	//     turn whose job is to write the prefix. There is also nothing to protect yet:
	//     no compaction is pending, because no earlier turn enqueued one.
	//   - the tail a pending job will actually replace. The job enqueued by the PREVIOUS
	//     turn targets that turn's tail, which by now sits at or below the boundary.
	//     Blocking from the boundary up protected this turn's new messages, which no
	//     pending job is going to touch — off by one turn, and it protected the wrong
	//     span. The doomed span starts where the previous turn's own tail started.
	tailPending, noCacheAt := false, 0
	if mode == components.ModeAsync && !o.Deferred && !bypass && !o.CacheUncompactedTail &&
		cacheAware && o.PendingFrom > 0 {
		tailPending = true
		noCacheAt = o.PendingFrom
	}
	c := &components.Ctx{
		Ctx:                    ctx,
		Session:                sessionID,
		Store:                  st,
		Model:                  models,
		Bypass:                 bypass,
		CtxWindow:              o.Window,
		CacheAware:             cacheAware,
		MaxCachedIdx:           maxCachedIdx,
		Mode:                   mode,
		Deferred:               o.Deferred,
		TailCachePending:       tailPending,
		NoCacheAtOrAfter:       noCacheAt,
		StripCallerBreakpoints: o.StripCallerBreakpoints,
	}
	res.Session = sessionID

	// Canonical form of each normalized message BEFORE the pipeline, so a
	// count-changing component (summarize) can be mapped back to the body.
	normPre := make([][]byte, len(norm))
	for i := range norm {
		normPre[i], _ = json.Marshal(norm[i])
	}

	res.Run = pipe.Run(chat, c)
	res.TailUnprotected = c.TailUnprotected()

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
	var changes []change
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
			changes = append(changes, mkChange(s.path, s.preText, newText))
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
				// drop provider fields it doesn't model. Discard the change, keep the
				// original bytes. ponytail: correctness over the marginal saving here.
				continue
			}
			if out, err = sjson.SetRawBytes(out, s.path, post); err != nil {
				res.Body = body
				return res
			}
			changed = true
			var pm bschemas.ChatMessage
			_ = json.Unmarshal(s.pre, &pm)
			changes = append(changes, mkChange(s.path, schema.MessageText(pm), schema.MessageText(chat.Input[i])))
		}
	}
	if changed && dumpPath != "" {
		dumpChanges(c.Session, changes)
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

// change is one rewritten message, captured for the CONTEXT_GURU_DUMP trace so a
// human can see exactly what context-guru did to the wire.
type change struct {
	Path         string `json:"path"`
	BeforeTokens int    `json:"before_tokens"`
	AfterTokens  int    `json:"after_tokens"`
	Before       string `json:"before"`
	After        string `json:"after"`
}

func mkChange(path, before, after string) change {
	return change{
		Path: path, BeforeTokens: schema.TextTokens(before), AfterTokens: schema.TextTokens(after),
		Before: clip(before, 4000), After: clip(after, 4000),
	}
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
func dumpChanges(session string, changes []change) {
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
				content := blk.Get("content")
				if content.Type != gjson.String {
					continue // array/structured tool_result content — skip (never lose non-text)
				}
				handled = true
				text := content.String()
				norm = append(norm, toolMessage(text, blk.Get("tool_use_id").String()))
				slots = append(slots, slot{
					kind:    anthropicToolText,
					path:    "messages." + strconv.Itoa(i) + ".content." + strconv.Itoa(b) + ".content",
					preText: text,
				})
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
	used := make([]bool, len(normPre))
	var parts [][]byte
	lastBodyIdx := -1
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
			lastBodyIdx = -1
			continue
		}
		used[matched] = true
		bi, ok := bodyIndexOf(slots[matched].path)
		if !ok || bi < 0 || bi >= len(orig) {
			return body, false
		}
		if bi == lastBodyIdx {
			continue // several normalized messages share one body message — emit it once
		}
		parts = append(parts, []byte(orig[bi].Raw))
		lastBodyIdx = bi
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

func systemAndFirstUser(msgs []bschemas.ChatMessage) (sys, firstUser string) {
	for _, m := range msgs {
		t := schema.MessageText(m)
		switch m.Role {
		case bschemas.ChatMessageRoleSystem:
			sys += t
		case bschemas.ChatMessageRoleUser:
			if firstUser == "" {
				firstUser = t
			}
		}
	}
	return sys, firstUser
}
