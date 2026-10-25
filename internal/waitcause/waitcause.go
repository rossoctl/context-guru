// Package waitcause records WHY a thread's conversation went idle and WHY it woke up again —
// the signal issue #424 asks for so a later change can choose a keep-alive ping budget from
// real data, instead of today's fixed 2 pings per thread (see internal/thread and #426).
//
// This package does NOT choose a ping budget and does NOT change keep-alive or compaction
// behaviour. It only reads the request body the agent already sent (and the stop reason /
// tool calls of the response the proxy already parsed) and classifies two things:
//
//   - WaitingFor: what the thread is about to wait on, known the moment the response that
//     ENDS a turn finishes.
//   - WakeCause: what ended a wait, known the moment the NEXT request arrives.
//
// Every signal is a string match or a structural read of a tool_use/tool_result block — no
// LLM, no guessing. Where a marker Claude Code uses does not apply (another agent, another
// provider, a shape this package does not recognise), the fields read "unknown" rather than a
// guess. Every exported method recovers internally from a panic and returns the zero value
// (every field "unknown", every count 0): a bug here can degrade the data this package
// produces, and must never touch the request or its timing. See issue #424 and the
// scripts/analysis/wait_causes.py script (#425) this package's marker shapes come from,
// verified there against real Claude Code transcripts.
package waitcause

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/schema"
	"github.com/tidwall/gjson"
)

// WaitingFor values: what a thread is about to wait on, set on the row whose response ENDS a
// turn with nothing more for the agent to do right now.
const (
	WaitingOwnTool    = "own_tool"
	WaitingHuman      = "human"
	WaitingBackground = "background"
	WaitingTeammate   = "teammate"
	WaitingRetry      = "retry"
	WaitingUnknown    = "unknown"
)

// WakeCause values: what ended a wait, read off the request that ends it.
const (
	WakeToolResult = "tool_result"
	WakeBackground = "background"
	WakeTeammate   = "teammate"
	WakeHuman      = "human"
	WakeSystem     = "system"
	WakeRetry      = "retry"
	WakeUnknown    = "unknown"
)

// askHumanTools are tool_use blocks that are themselves a tool call, but one that waits on a
// HUMAN, not on the tool: the "exception" issue #424 section 1 names explicitly. A response
// that calls one of these — even alongside other tools — means WaitingHuman, never
// WaitingOwnTool; and a bare tool_result that answers one of these (the Messages API returns
// every tool's answer the same way, human or not) means WakeHuman, never WakeToolResult.
var askHumanTools = map[string]bool{"AskUserQuestion": true, "ExitPlanMode": true}

// teammateWaitCap is how long an open SendMessage wait is still counted as "open" for
// Observation.OpenTeammate. Issue #424: "An open teammate wait expires at the ping cap (about
// 56 min after the send)... Without an expiry, one unanswered SendMessage would mark every
// later wait in the thread as teammate." Chosen to match the ping cap in section 3's proposal
// (2 pings every ~4.67 min keeps a cache warm; a teammate reply realistically arriving within
// the keep-alive's own horizon is the case worth tracking at all — PR #425's own analysis found
// a third of teammate waits never close within a visible transcript, so an unbounded wait would
// make `teammate` stick for the rest of the thread's life).
const teammateWaitCap = 56 * time.Minute

// noExpiryCap bounds a background task that stated NO expiry at all (a backgrounded Bash, an
// async agent, a Skill launch — only a Monitor ever states one). Issue #424 (fix 6): once an
// item's finish signal and start marker have both scrolled out of a compacted body, the ONLY
// way it ever stops counting as open is a cap like this one — matched to teammateWaitCap for
// the same reason that one is 56 min (issue #424 section 3's own ping-cap horizon: past it, no
// ping budget this later work could choose would still be paying for itself).
const noExpiryCap = teammateWaitCap

// ClassifyWaitingFor turns one response's own facts into a WaitingFor value and, for
// WaitingOwnTool, the LAST tool_use block's name (issue #424: "also store the last tool_use's
// tool name"). toolUseNames is every tool_use/function-call name the response carried, in the
// order they appeared.
//
// failed means the call itself did not complete (a non-2xx upstream status, or a transport
// error) — the proxy's own, more precise analogue of the transcript's isApiErrorMessage.
// usageOK is responseUsageWhy's own success flag: when neither a stop reason nor any tool use
// nor a parsed usage block came back, there is nothing to classify from, so the answer is
// WaitingUnknown rather than a guess at WaitingHuman.
func ClassifyWaitingFor(toolUseNames []string, stopReason string, failed, usageOK bool, openBackground, openTeammate int) (waitingFor, lastTool string) {
	if failed {
		return WaitingRetry, ""
	}
	if len(toolUseNames) > 0 {
		for _, n := range toolUseNames {
			if askHumanTools[n] {
				return WaitingHuman, ""
			}
		}
		return WaitingOwnTool, toolUseNames[len(toolUseNames)-1]
	}
	if !usageOK && stopReason == "" {
		return WaitingUnknown, ""
	}
	switch {
	case openBackground > 0:
		return WaitingBackground, ""
	case openTeammate > 0:
		return WaitingTeammate, ""
	default:
		return WaitingHuman, ""
	}
}

// Observation is what Tracker.Observe reports for one request.
type Observation struct {
	// WakeCause is "" (callers should treat as WakeUnknown) only when the body carried no
	// usable message list; otherwise always one of the WakeXxx constants.
	WakeCause string
	// OpenBackground / OpenTeammate are the open-task and open-teammate counts AS OF this
	// request's own body — the "waiting_on" detail issue #424 asks for, and the input
	// ClassifyWaitingFor needs for the NEXT response that ends a turn.
	OpenBackground int
	OpenTeammate   int
	// MonitorExpiryMs is the latest known absolute expiry (epoch ms) among this thread's
	// currently open Monitor tasks that stated one; 0 when none is open or none stated an
	// expiry (a persistent Monitor, or a backgrounded Bash/agent/skill, has none).
	MonitorExpiryMs int64
	// MsgCount is the number of entries in the body's message/input list, carried back so the
	// caller can hand it to RecordOutcome once this request's own HTTP status is known.
	MsgCount int
}

// Tracker holds, per thread, the small amount of state a body alone cannot supply: how long
// ago an open item was first seen (the body carries no timestamps at all), and the previous
// request's status and message count (for the retry rule). Safe for concurrent use. The zero
// value is not usable; call New.
type Tracker struct {
	mu      sync.Mutex
	threads map[string]*threadState
	now     func() time.Time
}

// bgOpen is one open background task's state, kept in the thread's PERSISTENT map (see
// threadState) rather than rebuilt from the current body alone. Issue #424 (fix 6, found live
// against a real compacted session): once the agent compacts its history, a task's own start
// marker — "Monitor started (task X, ...)", "running in background with ID: X.", etc. — falls
// out of the body for good, but the task is still really running. A fresh-rescan-only design
// would read that as closed the moment compaction happened, which is wrong in exactly the
// case this field exists for: a long-running background task is the one case where the
// keep-alive's choice of budget matters most.
type bgOpen struct {
	dur       time.Duration // 0 = no stated expiry (a backgrounded Bash, an async agent, a Skill)
	firstSeen time.Time
}

type threadState struct {
	lastStatus   int
	lastMsgCount int
	// openBackground is keyed by the task's own id, or "anon:<tool_use id>" for a background
	// Skill launch (see blocksOf's doc on why that case has no real id to key on at all).
	// Persists across requests; an entry leaves ONLY on its own finish signal, a TaskStop, a
	// Monitor's stated expiry, or noExpiryCap for one with none — never because its start
	// marker is no longer in the body.
	openBackground map[string]*bgOpen
	// anonOrder is the FIFO order "anon:" keys OPENED in, for the Skill case's own
	// close-oldest-first rule (see scanOpenState's doc). Persistent for the same reason
	// openBackground is: an anonymous task must survive compaction exactly like a named one.
	anonOrder []string
	// openTeammates maps a SendMessage's target (or an anonymous key when the call named no
	// "to") to when this package first saw it open. Persistent for the same compaction reason.
	openTeammates map[string]time.Time
	// appliedCloses records close SIGNALS this package has already acted on, so replaying the
	// SAME historical close (every request resends the whole conversation, so a notification
	// that closed something on turn 10 is still right there in the body on turn 11, 12, ...)
	// cannot act on it a second time. This matters ONLY for a close that falls back to "close
	// the oldest open anonymous Skill task" (scanOpenState's doc): that fallback has a side
	// effect on an ENTRY OTHER than the one named in the signal, so firing it twice for one
	// real event would close two unrelated Skill tasks instead of one. A close that matches a
	// real, still-open id is already naturally idempotent (deleting an absent map key is a
	// no-op) and needs no entry here.
	appliedCloses map[string]bool
	last          time.Time
}

// Bounds mirroring internal/thread.Tracker's own (issue #423's package, which this one keys
// alongside): a long-lived deployment must not let any of these maps grow forever.
const (
	maxThreads     = 20000
	staleAfter     = 2 * time.Hour
	maxTrackedKeys = 512 // per map, per thread: openBackground, openTeammates, appliedCloses
)

func newThreadState() *threadState {
	return &threadState{
		openBackground: map[string]*bgOpen{},
		openTeammates:  map[string]time.Time{},
		appliedCloses:  map[string]bool{},
	}
}

// New returns an empty tracker.
func New() *Tracker {
	return &Tracker{threads: map[string]*threadState{}, now: time.Now}
}

// Key names one thread for Observe/RecordOutcome, matching internal/thread.Key's shape (tenant
// is folded in here because waitcause is tenant-scoped the same way thread identification is).
func Key(tenant, session, threadID string) string {
	return tenant + "\x00" + session + "\x00" + threadID
}

// Observe reads one request's body and returns its wake cause and the open-task/open-teammate
// state as of that body. provider picks the dialect apply.NormalizeMessages reads — the SAME
// normalization the compaction pipeline itself uses, so this package carries no second
// per-provider body parser (see the package doc). Fails open: any panic (a malformed body, an
// unexpected shape) yields the zero Observation (WakeCause "", read by callers as unknown)
// rather than escaping.
func (t *Tracker) Observe(tenant, session, threadID string, provider bschemas.ModelProvider, body []byte) (obs Observation) {
	defer func() { recover() }() //nolint:errcheck // fail open; see package doc
	if t == nil {
		return Observation{}
	}
	return t.observe(tenant, session, threadID, body, func() []bschemas.ChatMessage { return apply.NormalizeMessages(provider, body) })
}

// ObserveWith is Observe with the normalized messages supplied by the caller (norm is called
// at most once, and not at all for a previous_response_id body), so a caller that also needs
// the normalization can share one pass over the body.
func (t *Tracker) ObserveWith(tenant, session, threadID string, body []byte, norm func() []bschemas.ChatMessage) (obs Observation) {
	defer func() { recover() }() //nolint:errcheck // fail open; see package doc
	if t == nil {
		return Observation{}
	}
	return t.observe(tenant, session, threadID, body, norm)
}

func (t *Tracker) observe(tenant, session, threadID string, body []byte, norm func() []bschemas.ChatMessage) Observation {
	if gjson.GetBytes(body, "previous_response_id").String() != "" {
		// A Responses request continuing SERVER-HELD history: `input` here is only the
		// newest turn, not the full conversation. apply.NormalizeMessages does not special-
		// case this (normalizeResponses reads `input` as-is, which is right for the
		// PIPELINE's purposes), but for THIS package that one turn would undercount every
		// open background task and teammate wait, and could misread "the newest turn" as
		// the whole wake signal when the real one arrived several turns ago server-side.
		// Same bailout internal/thread.fingerprint makes, for the same reason, and the one
		// field check this package needs before trusting NormalizeMessages' output as "the
		// whole conversation" — not a second message parser.
		return Observation{}
	}
	entries := norm()
	n := len(entries)
	if n == 0 {
		return Observation{}
	}
	key := Key(tenant, session, threadID)
	now := t.now()

	obs := Observation{MsgCount: n}
	var isRetry bool
	func() {
		// The Tracker's lock is process-wide (one mutex for every thread, not one per
		// thread), so the critical section stays to exactly what touches ts: nothing here
		// needs to hold it while classifyWake (pure regex work over `entries`, no shared
		// state) runs below.
		t.mu.Lock()
		defer t.mu.Unlock()
		ts := t.threadLocked(key, now)
		prevStatus, prevMsgCount := ts.lastStatus, ts.lastMsgCount
		ts.last = now

		// The retry rule (issue #424, section 1): "A previous request on the same thread
		// with a non-200 status and the same message count means retry." Structural, and
		// checked before any content read — a resend after an upstream failure carries
		// whatever content was already there, so classifying it by content would read the
		// ORIGINAL wait's cause, not the retry.
		isRetry = prevStatus != 0 && prevStatus/100 != 2 && prevMsgCount == n

		// Apply this body's open/close signals onto the THREAD'S OWN PERSISTENT state (fix
		// for issue #424's own live finding, section 6): an item leaves ts.openBackground /
		// ts.openTeammates only on an explicit finish signal, a TaskStop, a stated Monitor
		// expiry, or a cap for one with none — never merely because its OWN start marker
		// has fallen out of a since-compacted body. Every request still rescans the WHOLE
		// current body (self-correcting from anything an earlier request missed), but
		// every write this scan makes to ts is naturally idempotent (an "if not already
		// open" guard on each open; a delete of an absent key is a no-op) except the
		// Skill-anonymous close fallback, which is explicitly guarded by ts.appliedCloses —
		// see scanOpenState's doc for why that ONE path needs it.
		scanOpenState(ts, entries, now)
		obs.OpenBackground, obs.MonitorExpiryMs = countBackgroundLocked(ts, now)
		obs.OpenTeammate = countTeammatesLocked(ts, now)
	}()

	if isRetry {
		obs.WakeCause = WakeRetry
	} else {
		obs.WakeCause = classifyWake(entries)
	}
	return obs
}

// OpenCounts reports the open background-task and open-teammate counts for a thread as of the last
// request Observe saw, with the same expiry rules Observe applies. It reads state and changes none
// except dropping an expired item, which Observe would drop on its next call anyway.
//
// It exists for the keep-alive, which needs the thread's wait cause at the moment a ping falls due,
// between requests, when no body is at hand to observe. A thread Observe has never seen reports 0, 0.
func (t *Tracker) OpenCounts(tenant, session, threadID string) (background, teammate int) {
	defer func() { recover() }() //nolint:errcheck // fail open; see package doc
	if t == nil {
		return 0, 0
	}
	key := Key(tenant, session, threadID)
	t.mu.Lock()
	defer t.mu.Unlock()
	ts, ok := t.threads[key]
	if !ok {
		return 0, 0
	}
	now := t.now()
	background, _ = countBackgroundLocked(ts, now)
	teammate = countTeammatesLocked(ts, now)
	return background, teammate
}

// RecordOutcome stores this request's own status and message count, for the NEXT request on
// the same thread's retry check. Call once this request's upstream status is known (status 0
// means "never reached an upstream" and is never read back as a retry predecessor).
func (t *Tracker) RecordOutcome(tenant, session, threadID string, msgCount, status int) {
	defer func() { recover() }() //nolint:errcheck // fail open; see package doc
	if t == nil {
		return
	}
	key := Key(tenant, session, threadID)
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	ts := t.threadLocked(key, now)
	ts.lastStatus, ts.lastMsgCount, ts.last = status, msgCount, now
}

// threadLocked returns key's thread state, creating it if needed. Caller holds t.mu.
func (t *Tracker) threadLocked(key string, now time.Time) *threadState {
	ts, ok := t.threads[key]
	if ok {
		return ts
	}
	if len(t.threads) >= maxThreads {
		t.pruneLocked(now)
	}
	ts = newThreadState()
	t.threads[key] = ts
	return ts
}

// pruneLocked drops threads silent longer than staleAfter, same policy as
// internal/thread.Tracker.pruneLocked. Caller holds t.mu.
func (t *Tracker) pruneLocked(now time.Time) {
	for k, ts := range t.threads {
		if now.Sub(ts.last) > staleAfter {
			delete(t.threads, k)
		}
	}
	if len(t.threads) >= maxThreads {
		t.threads = map[string]*threadState{}
	}
}

// countBackgroundLocked turns ts's persistent open-background set into a count and a latest
// absolute Monitor expiry, applying each item's cap along the way: a stated Monitor expiry
// (dur > 0) past its deadline, or an unbounded task (dur == 0, no stated expiry at all) past
// noExpiryCap. Either way the entry is DELETED here, not merely skipped — once something is
// given up on, counting on its finish signal arriving late is no better than never counting it
// at all, and keeping it would cost memory for the life of the thread for nothing. Caller
// holds t.mu.
func countBackgroundLocked(ts *threadState, now time.Time) (count int, expiryMs int64) {
	var latest time.Time
	for id, o := range ts.openBackground {
		window := noExpiryCap
		if o.dur > 0 {
			window = o.dur
		}
		exp := o.firstSeen.Add(window)
		if !exp.After(now) {
			delete(ts.openBackground, id)
			removeFromAnonOrder(ts, id)
			continue
		}
		if o.dur > 0 && exp.After(latest) {
			latest = exp // only a STATED expiry is reported; an assumed cap is not a promise
		}
		count++
	}
	if !latest.IsZero() {
		expiryMs = latest.UnixMilli()
	}
	return count, expiryMs
}

// countTeammatesLocked is countBackgroundLocked for ts.openTeammates, capped at
// teammateWaitCap. Caller holds t.mu.
func countTeammatesLocked(ts *threadState, now time.Time) int {
	count := 0
	for name, firstSeen := range ts.openTeammates {
		if now.Sub(firstSeen) >= teammateWaitCap {
			delete(ts.openTeammates, name)
			continue
		}
		count++
	}
	return count
}

// blockKind is one normalized message's shape, as this package needs to tell it apart.
type blockKind int

const (
	kindText blockKind = iota
	kindToolUse
	kindToolResult
)

// block is one signal this package reads off a bschemas.ChatMessage — the SAME normalized
// schema the compaction pipeline itself builds via apply.NormalizeMessages, for all three
// dialects issue #424 names (Anthropic Messages, OpenAI chat, OpenAI Responses). No second
// per-provider body parser lives in this package: NormalizeMessages already turns a tool_use
// (Anthropic) / function call (OpenAI chat, Responses) into ChatAssistantMessage.ToolCalls,
// and a tool_result / function_call_output into a synthetic role=tool ChatMessage — see
// apply.normalize and apply.normalizeResponses for exactly which text and which role each
// becomes.
type block struct {
	kind      blockKind
	text      string
	toolName  string // kindToolUse only
	toolUseID string // the call's own id (kindToolUse) or the id it answers (kindToolResult)
	input     gjson.Result
}

// blocksOf reads one normalized message's signals. A message is at most one kind here except
// an assistant turn, which can carry BOTH tool_use blocks and plain text in the same turn (a
// model that narrates before or alongside calling a tool) — so this returns every tool_use
// call AND, if the message also carries text, one more kindText block for it.
func blocksOf(m bschemas.ChatMessage) []block {
	var out []block
	if m.ChatAssistantMessage != nil {
		for _, tc := range m.ChatAssistantMessage.ToolCalls {
			b := block{kind: kindToolUse, input: gjson.Parse(tc.Function.Arguments)}
			if tc.Function.Name != nil {
				b.toolName = *tc.Function.Name
			}
			if tc.ID != nil {
				b.toolUseID = *tc.ID
			}
			out = append(out, b)
		}
	}
	if m.ChatToolMessage != nil && m.ChatToolMessage.ToolCallID != nil {
		// A tool-role message's whole content IS its result — never also plain text, in
		// either dialect — so there is nothing else to add for this message.
		return append(out, block{kind: kindToolResult,
			toolUseID: *m.ChatToolMessage.ToolCallID, text: schema.MessageText(m)})
	}
	if text := schema.MessageText(m); text != "" {
		out = append(out, block{kind: kindText, text: text})
	}
	return out
}

// The marker shapes below are exactly scripts/analysis/wait_causes.py's (issue #425), found by
// that script against real Claude Code transcripts rather than guessed. See that script's own
// comments and scripts/analysis/README.md's "Technical notes" for the shapes that did NOT work
// on the first try (a Monitor's own mid-watch notice looking like a finish; a background
// Skill's id living only in a structured field no API body carries).
var (
	teammateMarkerRE   = regexp.MustCompile(`<teammate-message|<cross-session-message`)
	teammateIDFinishRE = regexp.MustCompile(`<teammate-message\b[^>]*\bteammate_id="([^"]+)"`)
	crossSessionNameRE = regexp.MustCompile(`<cross-session-message\b[^>]*\bfrom-name="([^"]+)"`)
	taskNotifMarkerRE  = regexp.MustCompile(`<task-notification>`)
	taskNotifBlockRE   = regexp.MustCompile(`(?s)<task-notification>(.*?)</task-notification>`)
	taskIDInBlockRE    = regexp.MustCompile(`<task-id>([^<]+)</task-id>`)
	monitorEventRE     = regexp.MustCompile(`<summary>Monitor event:`)
	monitorExpiredRE   = regexp.MustCompile(`Monitor expired`)
	agentMessageFromRE = regexp.MustCompile(`<agent-message\s+from="([^"]+)"`)
	bgBashStartRE      = regexp.MustCompile(`running in background with ID:\s*(\S+?)\.`)
	monitorStartRE     = regexp.MustCompile(`Monitor started \(task\s+(\S+?),\s*([^)]*)\)`)
	monitorExpiresInRE = regexp.MustCompile(`expires in\s+([^;]+?);`)
	monitorTimeoutMsRE = regexp.MustCompile(`timeout\s+(\d+)ms`)
	asyncAgentStartRE  = regexp.MustCompile(`(?s)Async agent launched successfully.*?agentId:\s*(\S+)`)
	bgSkillStartRE     = regexp.MustCompile(`launched \(forked execution, running in the background\)`)
	durationPartsRE    = regexp.MustCompile(`(?:(\d+)\s*h)?\s*(?:(\d+)\s*m)?\s*(?:(\d+)\s*s)?`)
)

// systemMarkers are substrings that mark a block of text as something context-guru's own
// client (Claude Code) injected rather than something a human typed. The API body carries no
// `isMeta` flag the way a Claude Code transcript entry does (see the package doc), so these
// have to be told apart by CONTENT. Not exhaustive by design — see WakeSystem's doc: anything
// that is not one of these and not empty reads as human text, which is the safe direction (a
// human's own words never carry one of these strings by accident in practice; a system marker
// this list does not know about reads as human instead of system, which is a label on a
// row, not a decision this package or the keep-alive ever acts on today).
var systemMarkers = []string{
	"<system-reminder",
	"Base directory for this skill:",
	"<command-name>",
	"<local-command-stdout",
}

func hasSystemMarker(text string) bool {
	for _, m := range systemMarkers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// scanOpenState scans a request's entire message/input list, front to back, and APPLIES every
// open/close signal it finds onto ts's PERSISTENT state (ts.openBackground, ts.openTeammates) —
// the "waiting_on" state issue #424 asks be tracked per thread, and (issue #424 fix 6) tracked
// in a way that survives the agent compacting its own history. It still runs a full rescan of
// the WHOLE current body every call (issue #424 allows this when it is fast enough — see
// internal/waitcause_test.go's benchmark and the PR body for the measured cost on a ~600 KB
// body), which is what makes this self-correcting from anything an earlier request missed (a
// restart, a dropped event); the difference from an earlier version of this package is that the
// scan no longer DISCARDS an item just because ITS OWN start marker is no longer present —
// only an explicit finish signal, a TaskStop, a stated Monitor expiry, or a cap (see
// countBackgroundLocked/countTeammatesLocked) ever removes one.
//
// Every write here is naturally safe to repeat on the SAME historical signal across many
// requests (a full rescan sees it again on every later turn, since a request resends the whole
// conversation): opening an already-open id is a no-op (openBG/openAnon check first), and
// closing a NAMED id that is not open is already a no-op (closeByID's own delete). The one
// exception is the Skill case's "close the oldest open anonymous task" fallback — it has a
// side effect on an entry OTHER than the one the signal names, so firing it a second time for
// the SAME historical signal would wrongly close a second, unrelated task; see
// threadState.appliedCloses and closeOldestAnon's own idempotency guard.
func scanOpenState(ts *threadState, entries []bschemas.ChatMessage, now time.Time) {
	openBG := func(id string, dur time.Duration) {
		if _, exists := ts.openBackground[id]; exists {
			return // already open — do NOT reset firstSeen on a resend
		}
		if len(ts.openBackground) >= maxTrackedKeys {
			for k := range ts.openBackground {
				delete(ts.openBackground, k)
				removeFromAnonOrder(ts, k)
				break
			}
		}
		ts.openBackground[id] = &bgOpen{dur: dur, firstSeen: now}
	}
	openAnon := func(toolUseID string) {
		key := "anon:" + toolUseID
		if _, exists := ts.openBackground[key]; exists {
			return
		}
		openBG(key, 0)
		ts.anonOrder = append(ts.anonOrder, key)
	}
	// closeOldestAnon closes the longest-open anonymous (Skill) task, idempotent per sig: a
	// resent transcript carries the SAME historical close signal on every later request too
	// (see threadState.appliedCloses's own doc), and without the guard it would close a
	// DIFFERENT, unrelated Skill task every single time that one old signal is rescanned.
	closeOldestAnon := func(sig string) {
		if ts.appliedCloses[sig] {
			return
		}
		rememberApplied(ts, sig)
		if len(ts.anonOrder) == 0 {
			return
		}
		delete(ts.openBackground, ts.anonOrder[0])
		ts.anonOrder = ts.anonOrder[1:]
	}
	// closeByID closes a NAMED task. Deleting an absent key is already a no-op, so only the
	// anonymous fallback (an id this scan has no record of at all) needs the sig guard.
	closeByID := func(sigPrefix, id string) {
		if _, ok := ts.openBackground[id]; ok {
			delete(ts.openBackground, id)
			return
		}
		closeOldestAnon(sigPrefix + ":" + id)
	}

	for _, e := range entries {
		for _, b := range blocksOf(e) {
			switch b.kind {
			case kindToolUse:
				switch b.toolName {
				case "SendMessage":
					to := b.input.Get("to").String()
					if to == "" {
						to = "anon:" + b.toolUseID
					}
					if _, exists := ts.openTeammates[to]; !exists {
						if len(ts.openTeammates) >= maxTrackedKeys {
							for k := range ts.openTeammates {
								delete(ts.openTeammates, k)
								break
							}
						}
						ts.openTeammates[to] = now
					}
				case "TaskStop":
					if id := b.input.Get("task_id").String(); id != "" {
						closeByID("taskstop", id)
					}
				}
			case kindToolResult:
				text := b.text
				if m := monitorStartRE.FindStringSubmatch(text); m != nil {
					openBG(m[1], parseMonitorDuration(m[2]))
				} else if m := bgBashStartRE.FindStringSubmatch(text); m != nil {
					openBG(m[1], 0)
				} else if m := asyncAgentStartRE.FindStringSubmatch(text); m != nil {
					openBG(m[1], 0)
				} else if bgSkillStartRE.MatchString(text) {
					openAnon(b.toolUseID)
				}
			case kindText:
				text := b.text
				for _, m := range teammateIDFinishRE.FindAllStringSubmatch(text, -1) {
					delete(ts.openTeammates, m[1])
				}
				for _, m := range crossSessionNameRE.FindAllStringSubmatch(text, -1) {
					delete(ts.openTeammates, m[1])
				}
				for _, m := range agentMessageFromRE.FindAllStringSubmatch(text, -1) {
					closeByID("agentmsg", m[1])
				}
				for _, blk := range taskNotifBlockRE.FindAllStringSubmatch(text, -1) {
					inner := blk[1]
					finishes := !monitorEventRE.MatchString(inner) || monitorExpiredRE.MatchString(inner)
					if !finishes {
						continue // a Monitor's own mid-watch event report, not a finish
					}
					if idm := taskIDInBlockRE.FindStringSubmatch(inner); idm != nil {
						closeByID("notif", idm[1])
					} else {
						// No id anywhere in the block: the Skill case. The block's own text IS
						// the signature — the same historical notification is byte-identical on
						// every later resend, which is exactly what the idempotency guard needs.
						sig := "notif-raw:" + inner
						if len(sig) > 200 {
							sig = sig[:200]
						}
						closeOldestAnon(sig)
					}
				}
			}
		}
	}
}

// removeFromAnonOrder drops id from ts.anonOrder, if present. Only ever needed when an entry
// leaves ts.openBackground some way OTHER than closeOldestAnon (which already keeps the two in
// sync itself) — today, that is countBackgroundLocked's own expiry eviction.
func removeFromAnonOrder(ts *threadState, id string) {
	for i, k := range ts.anonOrder {
		if k == id {
			ts.anonOrder = append(ts.anonOrder[:i], ts.anonOrder[i+1:]...)
			return
		}
	}
}

// rememberApplied records a close signature, bounding the map the same crude way
// internal/thread bounds its own: a single arbitrary eviction rather than a size policy,
// because losing one old signature early just risks ONE extra (and harmless, since an
// anonymous Skill task closing slightly early is not a measurement anyone acts on today)
// close-oldest-anon firing a second time — not a correctness failure of the open/closed
// state itself.
func rememberApplied(ts *threadState, sig string) {
	if len(ts.appliedCloses) >= maxTrackedKeys {
		for k := range ts.appliedCloses {
			delete(ts.appliedCloses, k)
			break
		}
	}
	ts.appliedCloses[sig] = true
}

// parseMonitorDuration reads a Monitor's stated detail text and returns how long it keeps the
// task open, 0 meaning "no stated expiry" (a persistent Monitor, or a detail shape this
// package does not recognise — never a guess at a number).
func parseMonitorDuration(detail string) time.Duration {
	if m := monitorExpiresInRE.FindStringSubmatch(detail); m != nil {
		if d, ok := parseHMS(m[1]); ok {
			return d
		}
	}
	if m := monitorTimeoutMsRE.FindStringSubmatch(detail); m != nil {
		if ms, err := strconv.Atoi(m[1]); err == nil {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return 0
}

// parseHMS reads a "<N>h <N>m <N>s" duration (any subset of the three parts, issue #425's own
// three real shapes), e.g. "1m 30s" or "2h".
func parseHMS(s string) (time.Duration, bool) {
	m := durationPartsRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil || (m[1] == "" && m[2] == "" && m[3] == "") {
		return 0, false
	}
	var total time.Duration
	if m[1] != "" {
		h, _ := strconv.Atoi(m[1])
		total += time.Duration(h) * time.Hour
	}
	if m[2] != "" {
		mi, _ := strconv.Atoi(m[2])
		total += time.Duration(mi) * time.Minute
	}
	if m[3] != "" {
		sec, _ := strconv.Atoi(m[3])
		total += time.Duration(sec) * time.Second
	}
	return total, true
}

// isNoiseEntry reports whether a message/input entry is structurally never a wake signal: a
// bare `role: "system"` entry in the messages array itself (not the top-level `system`
// field — that one is a request's own metadata, never scanned here at all). Seen live, on
// real traffic, injected after both a human prompt and a tool_result alike, which is why
// classifyWake must skip it rather than classify it as the wake. apply.schema.SessionHead's
// own doc comment names the exact source: the Claude Agent SDK appends a fresh system-role
// message on every turn carrying its remaining-budget reminder
// (`<system-reminder><total_tokens>N tokens left</total_tokens>`). A role this explicit needs
// no content guess, unlike the `<system-reminder>`-style TEXT a `user`-role entry can carry
// (see systemMarkers): this is simply never a human and never a tool answering a call.
func isNoiseEntry(m bschemas.ChatMessage) bool {
	return m.Role == bschemas.ChatMessageRoleSystem
}

// lastSubstantiveIndex returns the index of the last entry that is not isNoiseEntry, or -1
// if entries holds nothing else.
func lastSubstantiveIndex(entries []bschemas.ChatMessage) int {
	for i := len(entries) - 1; i >= 0; i-- {
		if !isNoiseEntry(entries[i]) {
			return i
		}
	}
	return -1
}

// classifyWake reads the last SUBSTANTIVE entry of a request's message/input list and says
// what woke the thread, per issue #424 section 1's table (teammate beats background beats a
// bare tool result beats plain text beats a system marker). The AskUserQuestion/ExitPlanMode
// exception (SendMessage's own tool_result; see askHumanTools) is read off the entry before
// that — the response that STARTED this wait — the same call scripts/analysis/wait_causes.py's
// classify_wake_cause makes (its own PR #425 review finding 1).
//
// "Substantive" skips trailing `role: "system"` entries (see isNoiseEntry): one build of
// Claude Code seen live on this issue's own PR appends a bare `{"role":"system",
// "content":"<total_tokens>...tokens left</total_tokens>"}` entry after EVERY turn —
// including right after the very tool_result or human text that is the real wake signal.
// Unlike a `<system-reminder>` block, this is not something an API body ever asks us to
// detect by content: the role itself already says "not something a human or a tool wrote",
// which is a stronger, structural analogue of a transcript's `isMeta` flag.
func classifyWake(entries []bschemas.ChatMessage) string {
	idx := lastSubstantiveIndex(entries)
	if idx < 0 {
		return WakeUnknown
	}
	last := entries[idx]
	askedHuman := false
	if prev := lastSubstantiveIndex(entries[:idx]); prev >= 0 {
		for _, b := range blocksOf(entries[prev]) {
			if b.kind == kindToolUse && askHumanTools[b.toolName] {
				askedHuman = true
			}
		}
	}
	var hasTeammate, hasTaskNotif, hasToolResult, hasPlainText, hasSystem bool
	for _, b := range blocksOf(last) {
		switch b.kind {
		case kindToolResult:
			hasToolResult = true
		case kindText:
			switch {
			case teammateMarkerRE.MatchString(b.text):
				hasTeammate = true
			// <task-notification> (a backgrounded Bash/Monitor finishing) and
			// <agent-message from="X"> (an async agent or a background Skill finishing —
			// see scanOpenState's doc) are both the SAME wake: a background task's own
			// finish signal arriving, by itself, with nothing typed by a human. Found
			// live: a backgrounded Agent's own hand-back wakes the thread with exactly
			// this tag and nothing else, and reading it as hasPlainText (no recognized
			// marker) misclassified it as WakeHuman instead of WakeBackground.
			case taskNotifMarkerRE.MatchString(b.text), agentMessageFromRE.MatchString(b.text):
				hasTaskNotif = true
			case hasSystemMarker(b.text):
				hasSystem = true
			case strings.TrimSpace(b.text) != "":
				hasPlainText = true
			}
		}
	}
	switch {
	case hasTeammate:
		return WakeTeammate
	case hasTaskNotif:
		return WakeBackground
	case hasToolResult:
		if askedHuman {
			return WakeHuman
		}
		return WakeToolResult
	case hasPlainText:
		return WakeHuman
	case hasSystem:
		return WakeSystem
	default:
		return WakeUnknown
	}
}
