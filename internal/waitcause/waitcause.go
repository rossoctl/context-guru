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

type threadState struct {
	lastStatus   int
	lastMsgCount int
	// firstSeen maps a stable, in-memory-only key (never persisted, never logged — see the
	// package doc's privacy note) to the wall-clock time this package first observed it open.
	// "bg:<task id>" for a background task, "tm:<teammate name, or an anonymous key>" for a
	// SendMessage. Entries for ids that close are deleted so this cannot grow without bound
	// over a long-lived thread with many short-lived tasks; it is NOT bounded against a thread
	// that keeps a great many tasks open forever, which issue #424's own open-teammate finding
	// says is realistic — maxFirstSeenPerThread guards that case.
	firstSeen map[string]time.Time
	last      time.Time
}

// Bounds mirroring internal/thread.Tracker's own (issue #423's package, which this one keys
// alongside): a long-lived deployment must not let either map grow forever.
const (
	maxThreads            = 20000
	staleAfter            = 2 * time.Hour
	maxFirstSeenPerThread = 512
)

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
// state as of that body. Fails open: any panic (a malformed body, an unexpected shape) yields
// the zero Observation (WakeCause "", read by callers as unknown) rather than escaping.
func (t *Tracker) Observe(tenant, session, threadID string, body []byte) (obs Observation) {
	defer func() { recover() }() //nolint:errcheck // fail open; see package doc
	if t == nil {
		return Observation{}
	}
	return t.observe(tenant, session, threadID, body)
}

func (t *Tracker) observe(tenant, session, threadID string, body []byte) Observation {
	entries := messageEntries(body)
	n := len(entries)
	if n == 0 {
		return Observation{}
	}
	key := Key(tenant, session, threadID)
	now := t.now()

	t.mu.Lock()
	ts, ok := t.threads[key]
	if !ok {
		if len(t.threads) >= maxThreads {
			t.pruneLocked(now)
		}
		ts = &threadState{firstSeen: map[string]time.Time{}}
		t.threads[key] = ts
	}
	prevStatus, prevMsgCount := ts.lastStatus, ts.lastMsgCount
	ts.last = now
	t.mu.Unlock()

	obs := Observation{MsgCount: n}

	// The retry rule (issue #424, section 1): "A previous request on the same thread with a
	// non-200 status and the same message count means retry." Structural, and checked before
	// any content read — a resend after an upstream failure carries whatever content was
	// already there, so classifying it by content would read the ORIGINAL wait's cause, not
	// the retry.
	isRetry := prevStatus != 0 && prevStatus/100 != 2 && prevMsgCount == n

	open, anonBG, openTeammates := scanOpenState(entries)
	obs.OpenBackground, obs.MonitorExpiryMs = t.resolveBackground(key, open, anonBG, now)
	obs.OpenTeammate = t.resolveTeammates(key, openTeammates, now)

	if isRetry {
		obs.WakeCause = WakeRetry
	} else {
		obs.WakeCause = classifyWake(entries)
	}
	return obs
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
	ts, ok := t.threads[key]
	if !ok {
		if len(t.threads) >= maxThreads {
			t.pruneLocked(now)
		}
		ts = &threadState{firstSeen: map[string]time.Time{}}
		t.threads[key] = ts
	}
	ts.lastStatus, ts.lastMsgCount, ts.last = status, msgCount, now
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

// resolveBackground turns this request's open background-task set (keyed by task id, "" value
// meaning no stated expiry) into a count and a latest-expiry timestamp, using each task's
// first-seen wall time (recorded here, the first time this package ever saw it open) to turn a
// Monitor's relative "expires in Xm" into an absolute deadline. A task whose deadline has
// already passed is NOT counted and does not contribute to the expiry — see the package doc on
// why the body alone cannot supply "how long ago".
func (t *Tracker) resolveBackground(key string, open map[string]time.Duration, anon int, now time.Time) (count int, expiryMs int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ts := t.threads[key]
	if ts == nil {
		return anon, 0
	}
	var latest time.Time
	for id, dur := range open {
		fs, seen := ts.firstSeen["bg:"+id]
		if !seen {
			fs = now
			t.rememberLocked(ts, "bg:"+id, now)
		}
		if dur > 0 {
			exp := fs.Add(dur)
			if !exp.After(now) {
				continue // this Monitor's own stated expiry has already passed
			}
			if exp.After(latest) {
				latest = exp
			}
		}
		count++
	}
	// Anonymous opens (a background Skill launch: no id in the body text at all, only in a
	// transcript's own toolUseResult.agentId — see the package doc) have no expiry to check,
	// and are already known-open-right-now by construction (scanOpenState only reports one as
	// still anonymous when it saw no finish signal for it at all).
	count += anon
	if !latest.IsZero() {
		expiryMs = latest.UnixMilli()
	}
	return count, expiryMs
}

// resolveTeammates applies teammateWaitCap to this request's open-teammate set, using each
// one's first-seen wall time the same way resolveBackground does for a Monitor's expiry.
func (t *Tracker) resolveTeammates(key string, open map[string]bool, now time.Time) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	ts := t.threads[key]
	if ts == nil {
		return 0
	}
	count := 0
	for name := range open {
		fs, seen := ts.firstSeen["tm:"+name]
		if !seen {
			fs = now
			t.rememberLocked(ts, "tm:"+name, now)
		}
		if now.Sub(fs) < teammateWaitCap {
			count++
		}
	}
	// Drop first-seen entries for anything no longer in the open set at all (closed, or a
	// background task that is gone) so the map cannot grow for the life of a long thread.
	for k := range ts.firstSeen {
		if strings.HasPrefix(k, "tm:") && !open[strings.TrimPrefix(k, "tm:")] {
			delete(ts.firstSeen, k)
		}
	}
	return count
}

// rememberLocked records key's first-seen time, bounding the map the same crude way
// internal/thread bounds its thread list: a single-entry eviction of something arbitrary
// rather than a size-based policy, because this map is for memory safety under a pathological
// thread, not for correctness — losing one entry early just means that one item's age is
// measured from one request later than it should be.
func (t *Tracker) rememberLocked(ts *threadState, key string, now time.Time) {
	if len(ts.firstSeen) >= maxFirstSeenPerThread {
		for k := range ts.firstSeen {
			delete(ts.firstSeen, k)
			break
		}
	}
	ts.firstSeen[key] = now
}

// messageEntries returns the request body's message/input array, the same two paths
// internal/thread.fingerprint reads (`messages`, falling back to `input`). nil when the body
// carries no such list — a Responses request continuing server-held history, or a body this
// package cannot parse — in which case every field reads unknown/0 rather than a guess.
func messageEntries(body []byte) []gjson.Result {
	root := gjson.ParseBytes(body)
	if !root.IsObject() || root.Get("previous_response_id").String() != "" {
		// A Responses request continuing SERVER-HELD history: `input` here is only the
		// newest turn, not the full conversation, so scanning it would undercount every
		// open background task and teammate wait, and misread "the last entry" as the
		// whole wake signal when a wake could have arrived several turns ago server-side.
		// Same bailout as internal/thread.fingerprint, for the same reason.
		return nil
	}
	items := root.Get("messages")
	if !items.IsArray() {
		items = root.Get("input")
	}
	if !items.IsArray() {
		return nil
	}
	return items.Array()
}

// blockKind is one content block's shape, told apart across the three dialects issue #424
// names: Anthropic Messages, OpenAI chat, and OpenAI Responses.
type blockKind int

const (
	kindOther blockKind = iota
	kindText
	kindToolUse
	kindToolResult
)

type block struct {
	kind      blockKind
	text      string
	toolName  string // kindToolUse only
	toolUseID string // the call's own id (kindToolUse) or the id it answers (kindToolResult)
	input     gjson.Result
}

// blocksOf returns one message/input entry's content blocks, uniformly across dialects:
//
//	Anthropic Messages  {role, content: [{type: text|tool_use|tool_result|thinking, ...}]}
//	                     (content may also be a bare string: one text block)
//	OpenAI chat          {role, content, tool_calls: [{id, function: {name, arguments}}]}
//	                     and a bare {role: "tool", tool_call_id, content} result message
//	OpenAI Responses     an `input` item directly: {type: function_call, call_id, name,
//	                     arguments} or {type: function_call_output, call_id, output}, or a
//	                     message-shaped item with role + content parts (input_text/output_text)
//
// Blocks this package has no use for (thinking/reasoning, images, redacted content) are
// skipped rather than guessed at.
func blocksOf(e gjson.Result) []block {
	var out []block
	switch e.Get("type").String() {
	case "function_call": // OpenAI Responses
		out = append(out, block{kind: kindToolUse,
			toolName: e.Get("name").String(), toolUseID: e.Get("call_id").String(), input: e.Get("arguments")})
		return out
	case "function_call_output": // OpenAI Responses
		out = append(out, block{kind: kindToolResult,
			toolUseID: e.Get("call_id").String(), text: e.Get("output").String()})
		return out
	}
	e.Get("tool_calls").ForEach(func(_ gjson.Result, tc gjson.Result) bool { // OpenAI chat, assistant
		out = append(out, block{kind: kindToolUse,
			toolName: tc.Get("function.name").String(), toolUseID: tc.Get("id").String(), input: tc.Get("function.arguments")})
		return true
	})
	if e.Get("role").String() == "tool" { // OpenAI chat, a bare tool result message
		out = append(out, block{kind: kindToolResult,
			toolUseID: e.Get("tool_call_id").String(), text: e.Get("content").String()})
		return out
	}
	content := e.Get("content")
	if content.Type == gjson.String {
		out = append(out, block{kind: kindText, text: content.String()})
		return out
	}
	content.ForEach(func(_ gjson.Result, c gjson.Result) bool {
		switch c.Get("type").String() {
		case "tool_use":
			out = append(out, block{kind: kindToolUse,
				toolName: c.Get("name").String(), toolUseID: c.Get("id").String(), input: c.Get("input")})
		case "tool_result":
			out = append(out, block{kind: kindToolResult,
				toolUseID: c.Get("tool_use_id").String(), text: toolResultText(c)})
		case "text", "input_text", "output_text":
			out = append(out, block{kind: kindText, text: c.Get("text").String()})
		}
		return true
	})
	return out
}

// toolResultText reads a tool_result block's own content, which is either a bare string or an
// array of {type: text, text} parts (Anthropic's dialect for a multi-part tool answer).
func toolResultText(c gjson.Result) string {
	content := c.Get("content")
	switch {
	case content.Type == gjson.String:
		return content.String()
	case content.IsArray():
		var sb strings.Builder
		content.ForEach(func(_ gjson.Result, v gjson.Result) bool {
			if v.IsObject() {
				sb.WriteString(v.Get("text").String())
			} else {
				sb.WriteString(v.String())
			}
			sb.WriteByte('\n')
			return true
		})
		return sb.String()
	default:
		return ""
	}
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

// scanOpenState scans a request's entire message/input list, front to back, and returns the
// background tasks and teammate waits still open AS OF THE END of that list — the "waiting_on"
// state issue #424 asks be tracked per thread. Returned fresh from a full scan every call
// (issue #424: "incrementally if a full rescan of the body per request is too slow" — see
// internal/waitcause_test.go's benchmark and the PR body for the measured cost on a ~600 KB
// body) rather than maintained incrementally: a full rescan is self-correcting from any earlier
// request this proxy process never saw (a restart, a dropped event), where incremental state
// would stay wrong forever.
//
// open maps a background task's id to its stated duration (0 = no stated expiry: a backgrounded
// Bash, an async agent, or a "persistent"/unparseable-detail Monitor). anon counts background
// Skill launches, whose id lives only in a transcript's own structured field (see blocksOf's
// doc) and so cannot be matched to any later finish signal by content — each is treated as open
// until SOME finish signal arrives (closed oldest-first), the same imprecise but non-silent
// choice scripts/analysis/wait_causes.py makes for the same case.
func scanOpenState(entries []gjson.Result) (open map[string]time.Duration, anon int, openTeammates map[string]bool) {
	open = map[string]time.Duration{}
	openTeammates = map[string]bool{}
	var anonQueue []string // FIFO: oldest unmatched background-Skill open closes first

	closeOldestAnon := func() {
		if len(anonQueue) == 0 {
			return
		}
		delete(open, anonQueue[0])
		anonQueue = anonQueue[1:]
	}
	closeByID := func(id string) {
		if _, ok := open[id]; ok {
			delete(open, id)
			return
		}
		closeOldestAnon()
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
					openTeammates[to] = true
				case "TaskStop":
					if id := b.input.Get("task_id").String(); id != "" {
						closeByID(id)
					}
				}
			case kindToolResult:
				text := b.text
				if m := monitorStartRE.FindStringSubmatch(text); m != nil {
					open[m[1]] = parseMonitorDuration(m[2])
				} else if m := bgBashStartRE.FindStringSubmatch(text); m != nil {
					open[m[1]] = 0
				} else if m := asyncAgentStartRE.FindStringSubmatch(text); m != nil {
					open[m[1]] = 0
				} else if bgSkillStartRE.MatchString(text) {
					key := "anon:" + b.toolUseID
					open[key] = 0
					anonQueue = append(anonQueue, key)
				}
			case kindText:
				text := b.text
				for _, m := range teammateIDFinishRE.FindAllStringSubmatch(text, -1) {
					delete(openTeammates, m[1])
				}
				for _, m := range crossSessionNameRE.FindAllStringSubmatch(text, -1) {
					delete(openTeammates, m[1])
				}
				for _, m := range agentMessageFromRE.FindAllStringSubmatch(text, -1) {
					closeByID(m[1])
				}
				for _, blk := range taskNotifBlockRE.FindAllStringSubmatch(text, -1) {
					inner := blk[1]
					finishes := !monitorEventRE.MatchString(inner) || monitorExpiredRE.MatchString(inner)
					if !finishes {
						continue // a Monitor's own mid-watch event report, not a finish
					}
					if idm := taskIDInBlockRE.FindStringSubmatch(inner); idm != nil {
						closeByID(idm[1])
					} else {
						closeOldestAnon()
					}
				}
			}
		}
	}
	// Anonymous (Skill) opens that this scan never saw close are reported through anon,
	// keyed by nothing: whatever is left in open under an "anon:" key IS one of them (it was
	// never removed from open itself, only from anonQueue, which only tracks match order).
	for k := range open {
		if strings.HasPrefix(k, "anon:") {
			delete(open, k)
			anon++
		}
	}
	return open, anon, openTeammates
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
// classifyWake must skip it rather than classify it as the wake. A role this explicit needs
// no content guess, unlike the `<system-reminder>`-style text a `user`-role entry can carry
// (see systemMarkers): this is simply never a human and never a tool answering a call.
func isNoiseEntry(e gjson.Result) bool {
	return e.Get("role").String() == "system"
}

// lastSubstantiveIndex returns the index of the last entry that is not isNoiseEntry, or -1
// if entries holds nothing else.
func lastSubstantiveIndex(entries []gjson.Result) int {
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
func classifyWake(entries []gjson.Result) string {
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
			case taskNotifMarkerRE.MatchString(b.text):
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
