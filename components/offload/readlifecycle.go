package offload

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
)

func init() { components.Register("readlifecycle", newReadLifecycle) }

// ReadLifecycle offloads a file Read whose body no longer describes the file, judged
// from the transcript itself (after headroom's read_lifecycle.py). Two classes, and
// only these two:
//
//   - STALE — an Edit/Write/MultiEdit/NotebookEdit on the same path appears LATER in
//     the transcript. What sits in context is then FACTUALLY WRONG, so removing it is
//     a correctness argument before it is a token argument: the model is otherwise
//     reasoning about line numbers and content that no longer exist.
//   - SUPERSEDED — the same path is Read again later with the SAME range, so the later
//     Read is authoritative and the earlier body is redundant.
//
// A FRESH Read — no later edit, no later re-read — is never touched. That is the
// safety property the component stands on: it removes only what the transcript itself
// proves is out of date, and the marker says only what is true (the file was modified /
// was read again), never "identical to", which a re-indent would already falsify.
//
// Deliberately conservative, in three places that each cost measurable savings:
//
//   - Supersession keys on (path, offset, limit), not path alone. Claude Code Reads
//     ranges; a later Read of lines 500-600 does not supersede an earlier read of the
//     whole file, and calling it superseded would delete content nothing replaced.
//     Containment (a later full read covering an earlier partial one) is NOT modelled —
//     measured at 867 tokens on the whole corpus on this box, which does not pay for
//     the reasoning.
//   - An Edit anywhere in the file staleizes every prior Read of it, including a Read
//     of a different range: an edit shifts the line numbers the Read printed, so the
//     rest of the body is wrong even where the text is not.
//   - Bash commands count as edits only under `bash_edits`, off by default, and only
//     for the narrow write forms listed in bashEditPaths. A false positive here deletes
//     CORRECT context, which is the one failure this component must not have.
//
// Cache safety. The classification is MONOTONE: it is a function of strictly later
// events, and events only ever accumulate, so a Read goes fresh → stale/superseded
// exactly once and never flaps back. The replacement text is a pure function of
// (content, class, path, config) — the marker key is sha256(content) — so it is
// byte-identical every time it is re-derived, from any process, with no map iteration
// on the path that produces it. That is what makes the freeze/reapply machinery this
// component shares with mask and failed_run correct here: a NEW offload happens only
// in the uncached tail (or at depth on a provably cold turn, `cold_cache`), and once
// frozen it is REPLAYED on every later turn regardless of depth, so the forwarded
// prefix is byte-stable. Determinism, not "only touch the delta", is the argument —
// see TestFrozenPrefixIsByteStable and TestDeterministicAcrossProcesses.
// atDepth (config `stale_at_depth`) is the one knob that can make this component pay on
// a WARM turn, and it is off by default because it costs a cache-write to buy one.
//
// The measured reason it exists: with the tail gate on, a real SWE-bench session declined
// 1,125 stale-Read candidates as `cached_prefix` and removed ZERO tokens — structurally,
// not accidentally. A Read only becomes stale when a LATER Edit appears, and by that turn
// the Read is already in the provider's cached prefix, so the gate can essentially never
// let this component act warm.
//
// Lifting it costs one re-anchor: rewriting the Read at index i forces a cache-write of
// everything after i, once, at the turn the Read goes stale. That is cheap exactly when
// the Read is near the tail at that moment — which the Read→Edit loop makes the common
// case — and the freeze then replays the decision on every later turn, so the body is
// gone for the rest of the session. See TestStaleAtDepthAmortizes for the measured
// distance, and the doc for the dollars.
type ReadLifecycle struct {
	minTokens  int
	mode       markerMode
	stale      bool
	superseded bool
	bashEdits  bool
	coldCache  bool
	atDepth    bool
}

type readLifecycleConfig struct {
	MinTokens int `yaml:"min_tokens"`
	// Stale/Superseded enable the two classes independently, so an operator who only
	// trusts the correctness argument can run stale detection alone.
	Stale      *bool  `yaml:"stale"`
	Superseded *bool  `yaml:"superseded"`
	BashEdits  bool   `yaml:"bash_edits"`
	MarkerMode string `yaml:"marker_mode"` // full (default) | summary | off
	ColdCache  bool   `yaml:"cold_cache"`
	// StaleAtDepth lifts the cache-tail gate for the STALE class on any turn, warm or
	// cold. See the amortization argument on ReadLifecycle.atDepth.
	StaleAtDepth bool `yaml:"stale_at_depth"`
}

func newReadLifecycle(raw []byte) (components.Component, error) {
	cfg := readLifecycleConfig{MinTokens: 100}
	if err := components.Decode(raw, &cfg); err != nil {
		return nil, err
	}
	on := func(p *bool) bool { return p == nil || *p }
	return &ReadLifecycle{
		minTokens: cfg.MinTokens, mode: parseMarkerMode(cfg.MarkerMode),
		stale: on(cfg.Stale), superseded: on(cfg.Superseded),
		bashEdits: cfg.BashEdits, coldCache: cfg.ColdCache, atDepth: cfg.StaleAtDepth,
	}, nil
}

func (ReadLifecycle) Name() string                 { return "readlifecycle" }
func (ReadLifecycle) Enabled(*components.Ctx) bool { return true }

// readClass is what the transcript proves about one Read.
type readClass int

const (
	readFresh readClass = iota
	readSuperseded
	readStale
)

// fileEvent is one file touch, at the index of the tool RESULT that reported it.
// Ordinals are absolute req.Input indices and comparisons are strictly-earlier-only,
// which is what makes appending a turn unable to reclassify an earlier Read
// (prefix-monotonicity; see headroom cross_turn_dedup invariant 1).
type fileEvent struct {
	idx   int
	edit  bool
	path  string
	rng   string // read range key: offset:limit; "" for an edit
	extra []string
}

func (rl *ReadLifecycle) Offload(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	events := rl.fileEvents(req)
	if len(events) == 0 {
		rep.Gate("no_file_reads")
		rep.Skipped = true
		return nil, nil
	}
	var keys []string
	changed := 0
	for k, ev := range events {
		if ev.edit {
			continue
		}
		msg := &req.Input[ev.idx]
		if !schema.Rewritable(*msg) {
			rep.Gate("non_text_blocks") // an image Read: a text rewrite would drop it
			continue
		}
		content := schema.MessageText(*msg)
		if content == "" {
			continue
		}
		// Replay a frozen decision on EVERY turn, at any depth: the agent re-sends the
		// original each turn, so not re-offloading it would flip the message
		// offloaded→full→offloaded and churn the provider's KV cache.
		if fk, _, ok := reapplyFrozen(c, rep, rl.Name(), msg); ok {
			changed++
			keys = append(keys, fk...)
			continue
		}
		class := rl.classify(events, k)
		if class == readFresh {
			rep.Gate("fresh_read") // no later edit, no later re-read: never touched
			continue
		}
		if skipReduce(c, content) {
			rep.Gate("marker_or_kept_verbatim")
			continue
		}
		if schema.TextTokens(content) < rl.minTokens {
			rep.Gate("below_min_tokens")
			continue
		}
		// A NEW offload only in the uncached tail (or at any depth on a provably cold
		// turn, opt-in), plus the lost-freeze repair: there the provider already holds
		// the offloaded bytes, so re-deriving them preserves the cache and leaving the
		// body verbatim is what destroys it.
		// stale_at_depth lifts the gate unconditionally for the stale class (see atDepth);
		// cold_cache lifts it only on a turn whose cache has provably expired.
		if rl.atDepth && class == readStale {
			// fall through: act at any depth
		} else if !c.TailOnlyCold(ev.idx, rl.coldCache) && !repairLostFreeze(c, rl.Name(), content) {
			rep.Gate("cached_prefix")
			continue
		}
		prefix := readMarkerPrefix(class, ev.path)
		newText, key, eff, ok := tryMark(c, rl.mode, content, " [full output: call "+expand.ToolName+"]",
			func(tok string) string { return prefix + tok })
		if !ok {
			rep.Gate("marker_no_win")
			continue
		}
		if !commitMark(c, rep, eff, key, content) {
			continue // the store cannot back the marker; leave this message verbatim
		}
		schema.SetMessageText(msg, newText)
		freeze(c, rl.Name(), content, newText)
		changed++
		if key != "" {
			keys = append(keys, key)
		}
	}
	if changed == 0 {
		rep.Skipped = true
	}
	return keys, nil
}

// classify decides what the transcript proves about events[k] (a read), looking ONLY
// at strictly later events. Stale wins over superseded: "was modified" is the stronger
// and truer statement about a body that is now wrong.
func (rl *ReadLifecycle) classify(events []fileEvent, k int) readClass {
	me := events[k]
	sup := false
	for _, ev := range events[k+1:] {
		if ev.edit {
			if rl.stale && touches(ev, me.path) {
				return readStale
			}
			continue
		}
		if rl.superseded && ev.path == me.path && ev.rng == me.rng {
			sup = true
		}
	}
	if sup {
		return readSuperseded
	}
	return readFresh
}

// touches reports whether an edit event modified path (its own target, or one of the
// paths a bash write form named).
func touches(ev fileEvent, path string) bool {
	if ev.path == path {
		return true
	}
	for _, p := range ev.extra {
		if p == path {
			return true
		}
	}
	return false
}

func readMarkerPrefix(class readClass, path string) string {
	if class == readStale {
		return "[stale file read: " + path + " was modified later in this conversation, so this content is out of date] "
	}
	return "[superseded file read: " + path + " was read again later in this conversation; that later read is authoritative] "
}

// editTools are the tools whose call MEANS the file changed. Explicit and closed —
// a proxy reading a tool name is certain, a proxy guessing at a shell command is not.
var editTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

// fileEvents walks the transcript in index order and records every file touch, using
// schema.ToolCalls to recover the tool NAME and ARGS behind each tool result. No map
// is iterated: `pairs` is only ever looked up by an ascending index, so the event list
// (and therefore every rewrite) is identical across processes despite Go's randomized
// map order.
func (rl *ReadLifecycle) fileEvents(req *bschemas.BifrostChatRequest) []fileEvent {
	pairs := schema.ToolCalls(req)
	var out []fileEvent
	for i := range req.Input {
		if req.Input[i].Role != bschemas.ChatMessageRoleTool {
			continue
		}
		tc, ok := pairs[i]
		if !ok {
			continue // no pairing: we cannot say what produced this output, so we say nothing
		}
		switch {
		case tc.Name == "Read":
			if p, rng := readArgs(tc); p != "" {
				out = append(out, fileEvent{idx: i, path: p, rng: rng})
			}
		case editTools[tc.Name]:
			if p, _ := readArgs(tc); p != "" {
				out = append(out, fileEvent{idx: i, edit: true, path: p})
			}
		case rl.bashEdits && tc.Name == "Bash":
			if ps := bashEditPaths(tc.Command()); len(ps) > 0 {
				out = append(out, fileEvent{idx: i, edit: true, path: ps[0], extra: ps[1:]})
			}
		}
	}
	return out
}

// readArgs pulls the path and the line window out of a call's raw arguments. It decodes
// the arguments itself rather than reusing schema.ToolCall's string accessor because
// `offset`/`limit` arrive as JSON NUMBERS, and the range key wants them as the wire
// carried them (raw bytes, so no formatting choice of ours can make two turns disagree).
func readArgs(tc schema.ToolCall) (path, rng string) {
	if tc.Args == "" {
		return "", ""
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(tc.Args), &obj) != nil {
		return "", ""
	}
	for _, k := range []string{"file_path", "notebook_path"} {
		if raw, ok := obj[k]; ok {
			if json.Unmarshal(raw, &path) != nil {
				return "", ""
			}
			break
		}
	}
	return path, string(obj["offset"]) + ":" + string(obj["limit"])
}

// bashWriteProgs is the whole of the Bash edit heuristic's program list, plus `>`/`>>`
// redirection handled below. Each entry is a program whose ONLY purpose is to write the
// files it is given:
//
//	tee [-a] path…              write stdin to a named file
//	sed -i[.suf] … path…        in-place stream edit (NOT plain `sed`, which only reads)
//	patch … path                apply a diff to a named file
//	truncate … path
//
// Everything else is NOT counted — `git apply`, `python script.py`, a Makefile, a code
// generator — because the file they write is not in the command text, and guessing would
// delete CORRECT context. `bash_edits` is off by default for the same reason.
var bashWriteProgs = map[string]bool{"tee": true, "patch": true, "truncate": true}

// bashEditPaths returns the paths a command plainly writes. For a program in
// bashWriteProgs it returns every non-flag operand rather than trying to know which
// operand is the file: a Read's file_path is absolute, so a `sed` expression or a `-p1`
// can never collide with one, and the caller only ever compares for EXACT equality with
// a path already read. That makes "all operands" narrow in effect while staying honest
// about not parsing shell.
func bashEditPaths(cmd string) []string {
	var out []string
	add := func(p string) {
		p = strings.Trim(p, `'"`)
		if p == "" || strings.HasPrefix(p, "-") || strings.HasPrefix(p, "/dev/") {
			return
		}
		out = append(out, p)
		if cl := filepath.Clean(p); cl != p {
			out = append(out, cl)
		}
	}
	for _, seg := range splitShell(cmd, "|;&\n") {
		toks := shellTokens(seg)
		if j := indexOf(toks, "<"); j >= 0 {
			toks = toks[:j] // an input redirect names a file we READ, not one we write
		}
		prog, inPlace := "", false
		for i, tk := range toks {
			if (tk == ">" || tk == ">>") && i+1 < len(toks) {
				add(toks[i+1])
			}
		}
		for _, tk := range toks {
			if strings.HasPrefix(tk, "-") {
				if prog == "sed" && (tk == "-i" || strings.HasPrefix(tk, "-i.")) {
					inPlace = true
				}
				continue
			}
			if strings.Contains(tk, "=") && prog == "" {
				continue // leading VAR=value environment assignment
			}
			if prog == "" {
				prog = filepath.Base(tk)
				continue
			}
			if bashWriteProgs[prog] || (prog == "sed" && inPlace) {
				add(tk)
			}
		}
	}
	return out
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}

// splitShell splits on any of seps, which is enough here: the operands this heuristic
// looks at are plain paths, and a separator inside quotes would at worst make it see
// FEWER write targets (declining to act), never more.
func splitShell(s, seps string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune(seps, r) })
}

// shellTokens splits one segment into words, keeping a single/double-quoted run together
// and separating `>`/`>>`/`<` from an adjacent word so `foo>bar` tokenizes.
func shellTokens(seg string) []string {
	seg = strings.ReplaceAll(seg, ">>", " >> ")
	seg = regexp.MustCompile(`([^>])>([^>])`).ReplaceAllString(seg, "$1 > $2")
	seg = strings.ReplaceAll(seg, "<", " < ")
	var toks []string
	var cur strings.Builder
	quote := rune(0)
	flush := func() {
		if cur.Len() > 0 {
			toks = append(toks, cur.String())
			cur.Reset()
		}
	}
	for _, r := range seg {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return toks
}

func init() {
	components.RegisterFields("readlifecycle", readLifecycleConfig{}, []components.Field{
		{Key: "min_tokens", Type: components.FieldInt, Default: 100, Min: 1,
			Hint: "Only offload a Read above this many tokens."},
		{Key: "stale", Type: components.FieldBool, Default: true,
			Hint: "Offload a Read whose file was later edited (Edit/Write/MultiEdit/NotebookEdit). The content in context is factually wrong once that happens."},
		{Key: "superseded", Type: components.FieldBool, Default: true,
			Hint: "Offload a Read of the same file and same line range that is read again later; the later read is authoritative."},
		{Key: "stale_at_depth", Type: components.FieldBool, Default: false,
			Hint: "Offload a stale Read even inside the already-cached prefix. Costs one cache-write of the suffix at the turn the Read goes stale, and saves the body on every turn after; whether that pays depends on how close to the tail the Read is when its file is edited (see docs/components/readlifecycle.md)."},
		{Key: "bash_edits", Type: components.FieldBool, Default: false,
			Hint: "Also treat narrow shell write forms (> file, >> file, tee, sed -i, patch, truncate) as edits. Off by default: a false positive here deletes correct context."},
		markerModeField(),
		coldCacheFieldDefault(false), // not a pure function of (content, config): see coldCacheDefault
	})
}
