package offload

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
	_ "modernc.org/sqlite"
)

func dedupFor(t *testing.T) *Dedup {
	t.Helper()
	comp, err := newDedup([]byte("min_tokens: 5\n"))
	if err != nil {
		t.Fatal(err)
	}
	return comp.(*Dedup)
}

// runDedup offloads msgs and returns the rewritten text of each message, plus the report.
func runDedup(t *testing.T, d *Dedup, msgs []bschemas.ChatMessage) ([]string, *components.Report) {
	t.Helper()
	req := &bschemas.BifrostChatRequest{Input: msgs}
	c := &components.Ctx{Session: "s", Store: store.NewMemory(store.Options{})}
	var rep components.Report
	if _, err := d.Offload(req, &rep, c); err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(req.Input))
	for i := range req.Input {
		out[i] = schema.MessageText(req.Input[i])
	}
	return out, &rep
}

// #281: an agent that edits a file and re-runs the inspection command gets a dedup
// marker back, and from the marker alone could not tell "your edit changed nothing,
// here is proof" from "this is a caching artifact". The marker must therefore carry
// BOTH halves of what makes that decidable: that it is a pointer at all, and how to
// get the bytes it points at. The recovery instruction is the half that was missing —
// dedup was the only offloader of eleven that passed an empty hint to tryMark, so its
// duplicates went out as a bare <<cg:HASH>> with nothing saying the content was
// retrievable, while every sibling said "[full output: call context_guru_expand]". The
// cue here is folded INSIDE the note's bracket ("; expand <<cg:HASH>>]") rather than
// appended as a second trailer, which buys the same signal for three tokens instead of ten.
func TestTheDedupMarkerSaysHowToGetTheOutputBack(t *testing.T) {
	dump := strings.Repeat("verbose tool output line\n", 30)
	msgs := []bschemas.ChatMessage{tool(dump), tool(dump)}
	got, _ := runDedup(t, dedupFor(t), msgs)

	if got[0] != dump {
		t.Fatal("the first copy must be left verbatim; only later duplicates collapse")
	}
	dup := got[1]
	// "expand" adjacent to the marker, rather than the tool's full name: expand.Inject puts
	// the definition in `tools` on every turn that carries a marker, so repeating
	// context_guru_expand in the note costs ten tokens to restate what is already in front
	// of the model. The cue plus a resolvable marker is what has to survive.
	if !strings.Contains(dup, "expand") {
		t.Fatalf("the marker gives the model no cue that the output is retrievable, so it is "+
			"told this is a duplicate but not that it can recover it — the #281 trust gap: %q", dup)
	}
	if !strings.Contains(dup, "<<cg:") {
		t.Fatalf("no resolvable marker, so there is nothing to recover: %q", dup)
	}
	// The expand tool must actually be reachable under the name the note alludes to.
	if expand.ToolName != "context_guru_expand" {
		t.Fatalf("the expand tool was renamed to %q; the note's bare \"expand\" cue relies on "+
			"the injected tool definition being the thing the model reaches for", expand.ToolName)
	}
}

// The other half of #281: WHICH earlier output. "an earlier tool output" on its own
// asserts an equivalence the model cannot locate or check. Naming the producing command
// makes it checkable — and disbelievable, which is the point, since only the model can
// initiate recovery.
func TestTheDedupMarkerNamesTheCallTheOutputCameFrom(t *testing.T) {
	dump := strings.Repeat("parsed 41 records\n", 30)
	cmd := `python -c "import extract_team; print(extract_team.parse())"`
	var msgs []bschemas.ChatMessage
	msgs = callMsgs(msgs, "Bash", map[string]string{"command": cmd}, dump)
	msgs = callMsgs(msgs, "Bash", map[string]string{"command": cmd}, dump)

	got, _ := runDedup(t, dedupFor(t), msgs)
	dup := got[3] // assistant, tool, assistant, tool
	if !strings.Contains(dup, "python -c") {
		t.Fatalf("the marker does not name the command that produced the original, so the "+
			"model cannot tell which earlier output this claims to equal: %q", dup)
	}
}

// A tool result with no paired tool_use still has to say it is a pointer. The note
// degrades to the unqualified wording rather than inventing a source or emitting an
// empty pair of backticks.
func TestTheDedupMarkerStillIdentifiesItselfWithNoPairedCall(t *testing.T) {
	dump := strings.Repeat("verbose tool output line\n", 30)
	got, _ := runDedup(t, dedupFor(t), []bschemas.ChatMessage{tool(dump), tool(dump)})
	dup := got[1]
	if !strings.Contains(dup, "an earlier tool output") {
		t.Fatalf("an unpaired duplicate must still announce itself as a pointer: %q", dup)
	}
	if strings.Contains(dup, "``") {
		t.Fatalf("empty command rendered as backticks: %q", dup)
	}
}

// The note goes inside the request, so a pathological command must not make the pointer
// unbounded (or invalid UTF-8, which a naive byte slice through a multibyte rune emits).
func TestTheDedupNoteBoundsAndSanitizesTheCommand(t *testing.T) {
	for _, tc := range []struct{ name, cmd string }{
		{"long ascii", strings.Repeat("x", 4000)},
		{"multibyte straddling the cut", strings.Repeat("a", 59) + strings.Repeat("日", 40)},
		{"embedded newlines", "line one\nline two\nline three"},
		// A Write payload's `content` reaches Command() as one argument, so megabyte inputs
		// are reachable in normal operation, not a contrived case. Counting tokens on the
		// whole thing before clamping bytes made this hang rather than truncate.
		{"megabyte of punctuation", strings.Repeat("/.;:-_", 200000)},
		{"megabyte of CJK", strings.Repeat("日本語", 300000)},
		{"emoji run", strings.Repeat("🎉", 50000)},
		{"mixed scripts", strings.Repeat("a/日🎉_", 100000)},
		{"single multibyte rune", "日"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			note := dedupNote(schema.ToolCall{Name: "Bash", Args: `{"command":` + jsonQuote(tc.cmd) + `}`})
			if !utf8.ValidString(note) {
				t.Fatalf("note is not valid UTF-8, which corrupts the outgoing request: %q", note)
			}
			if strings.ContainsAny(note, "\n\r") {
				t.Fatalf("a multi-line note breaks the one-line pointer shape: %q", note)
			}
			// The binding cap is on tokens (see maxCmdTokens), so that is what is asserted
			// here; the byte length is only loosely bounded, because a run of dense
			// single-byte-per-token text legitimately fits more bytes into the same budget
			// than prose does. TestTheDedupPointerStaysWithinItsTokenBudget covers the cost.
			if got := schema.TextTokens(note); got > maxCmdTokens+12 {
				t.Fatalf("note costs %d tokens: %q\nthe command cap stopped binding, so a "+
					"pathological command can cost more than the output it replaces", got, note)
			}
			if len(note) > 400 {
				t.Fatalf("note is %d bytes, which is past any plausible pointer even at "+
					"one token per byte: %q", len(note), note)
			}
		})
	}
}

// The pointer is only worth writing if it is small, and every word in it is paid for on
// every deduped output in every request. The note therefore has a token budget, and this
// is where it is enforced: the pre-#281 note was 10 tokens and said neither which output
// it matched nor that it could be expanded, so the whole point is to buy both of those for
// close to nothing. Measured with the same tokenizer tryMark's never-worse guard uses.
//
// The budget also protects the floor. tryMark compares marker-inclusive, so each extra
// token raises the size at which a duplicate stops being worth collapsing and silently
// pushes outputs just above min_tokens into marker_no_win.
func TestTheDedupPointerStaysWithinItsTokenBudget(t *testing.T) {
	const marker = "<<cg:6b8b51fac0c9be86>>" // a real 16-hex key, the shape on the wire
	for _, tc := range []struct {
		name, cmd string
		budget    int
	}{
		// No paired call: must cost essentially nothing over the old wording, while
		// gaining the expand cue the old note did not have.
		{"unpaired", "", 26},
		{"short command", "pytest -q", 27},
		{"typical command", `python -c "import extract_team; print(extract_team.parse())"`, 39},
		// The cap binds here; without it this case ran away with the request.
		{"pathological command", strings.Repeat("some/long/path/", 40), 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var call schema.ToolCall
			if tc.cmd != "" {
				call = schema.ToolCall{Name: "Bash", Args: `{"command":` + jsonQuote(tc.cmd) + `}`}
			}
			got := schema.TextTokens(dedupNote(call) + marker + "]")
			if got > tc.budget {
				t.Fatalf("the pointer costs %d tokens, budget %d: %q\nevery token here is paid "+
					"on every deduped output, and it raises the floor at which collapsing a "+
					"duplicate stops paying (tryMark is marker-inclusive)",
					got, tc.budget, dedupNote(call)+marker+"]")
			}
		})
	}
}

// The marker's bytes must be a pure function of content: dedup is one of the offloaders
// Ctx.CacheAware exempts from the cache-tail gate on the grounds that it is "already
// byte-stable on the unchanged prefix". This is why #281's own suggestion — annotate the
// marker with the turn index it matches — is NOT what this fix implements: the index
// shifts as the transcript grows, so the same content would render different bytes inside
// the provider's cached prefix on a later turn and force a full-suffix cache write.
// Re-running with the pair pushed deeper must produce the identical replacement.
func TestTheDedupMarkerIsByteStableAsTheTranscriptGrows(t *testing.T) {
	dump := strings.Repeat("parsed 41 records\n", 30)
	cmd := "pytest -q"
	build := func(lead int) []bschemas.ChatMessage {
		var msgs []bschemas.ChatMessage
		for i := 0; i < lead; i++ {
			msgs = append(msgs, bschemas.ChatMessage{Role: bschemas.ChatMessageRoleUser,
				Content: &bschemas.ChatMessageContent{ContentStr: ptr("turn " + strings.Repeat("x", i))}})
		}
		msgs = callMsgs(msgs, "Bash", map[string]string{"command": cmd}, dump)
		return callMsgs(msgs, "Bash", map[string]string{"command": cmd}, dump)
	}

	shallow, _ := runDedup(t, dedupFor(t), build(0))
	deep, _ := runDedup(t, dedupFor(t), build(5))
	if shallow[len(shallow)-1] != deep[len(deep)-1] {
		t.Fatalf("the replacement moved with position:\n shallow %q\n deep    %q\n"+
			"a position-dependent marker rewrites itself inside the provider's cached prefix "+
			"as the transcript grows, forcing a full-suffix cache write", shallow[len(shallow)-1], deep[len(deep)-1])
	}
}

func ptr(s string) *string { return &s }

// #1097: dedup leaves an expanded output in full (GateKeptVerbatim), also when the mark comes from
// another store that was carried over (a rebuilt tenant pipeline) or reloaded from session state
// (a restart).
func dedupGates(t *testing.T, st store.Store, dump string) map[string]int {
	t.Helper()
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(dump), tool(dump)}}
	c := &components.Ctx{Session: "s", Store: st}
	var rep components.Report
	if _, err := dedupFor(t).Offload(req, &rep, c); err != nil {
		t.Fatal(err)
	}
	return rep.Gates
}

func TestDedupKeepsAnExpandedOutputAfterTheMarkIsCarriedToANewStore(t *testing.T) {
	dump := strings.Repeat("verbose tool output line\n", 30)
	old := store.NewMemory(store.Options{})
	MarkKeptVerbatim(old, "s", dump)
	fresh := store.NewMemory(store.Options{})
	if g := dedupGates(t, fresh, dump); g[GateKeptVerbatim] != 0 {
		t.Fatalf("a fresh thread must compact as before: %v", g)
	}
	fresh = store.NewMemory(store.Options{})
	if n := store.CarryPrefix(old, fresh, store.KeptPrefix); n != 1 {
		t.Fatalf("carried %d marks, want 1", n)
	}
	if g := dedupGates(t, fresh, dump); g[GateKeptVerbatim] == 0 {
		t.Fatalf("the carried mark must keep the output in full: %v", g)
	}
}

func TestDedupKeepsAnExpandedOutputAfterARestart(t *testing.T) {
	dump := strings.Repeat("verbose tool output line\n", 30)
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	open := func() (*store.Memory, *store.Disk) {
		d, err := store.OpenDisk(db, store.DiskOptions{MetadataOnly: true, FlushEvery: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		m := store.NewMemory(store.Options{})
		m.AttachDisk(d)
		return m, d
	}
	m1, d1 := open()
	MarkKeptVerbatim(m1, "s", dump)
	d1.Flush()
	// The restart: a new memory store over the same database.
	m2, d2 := open()
	defer d2.Close()
	m2.LoadSession("s")
	if g := dedupGates(t, m2, dump); g[GateKeptVerbatim] == 0 {
		t.Fatalf("the mark must survive a restart, even with metadata-only session state: %v", g)
	}
}
