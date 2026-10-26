package dash

// Claude Code 2.1.310 defers MCP tools. Its `tools` array then holds the built-ins and one
// DeferredToolPlaceholder stub, and the mcp__* names travel as plain text in a user message:
// a header line, one name per line, a blank line. The fixtures in testdata/deferred are that
// text copied byte for byte from real requests, and the placeholder's own tools element.

import (
	"encoding/json"
	"hash/maphash"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/internal/tokens"
	"github.com/tidwall/gjson"
)

func deferredFixture(t testing.TB, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/deferred/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// deferredBody builds a body in the measured shape: the tools array, then one user message
// per entry of userTexts (each one text block), then optional tool calls.
func deferredBody(t testing.TB, tools []string, userTexts []string, calls []map[string]any) []byte {
	t.Helper()
	var msgs []any
	for _, s := range userTexts {
		msgs = append(msgs, map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": s},
		}})
	}
	if calls != nil {
		var blocks []any
		for _, c := range calls {
			blocks = append(blocks, map[string]any{"type": "tool_use", "id": "tu_1",
				"name": c["name"], "input": map[string]any{}})
		}
		msgs = append(msgs, map[string]any{"role": "assistant", "content": blocks})
	}
	m, err := json.Marshal(map[string]any{"model": "claude", "messages": msgs})
	if err != nil {
		t.Fatal(err)
	}
	return []byte(`{"tools":[` + strings.Join(tools, ",") + `],` + string(m)[1:])
}

func placeholderTool(t testing.TB) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(deferredFixture(t, "placeholder-tool.json")), &v); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// mcpByServer counts the KindMCPTool rows per server.
func mcpByServer(decls []Decl) map[string]int {
	out := map[string]int{}
	for _, d := range decls {
		if d.Kind == KindMCPTool {
			out[d.Server]++
		}
	}
	return out
}

var wantServers = map[string]int{
	"context7": 2, "memory": 9, "playwright": 25, "sequential-thinking": 1, "web-search": 2,
}

func assertServers(t *testing.T, decls []Decl) {
	t.Helper()
	got := mcpByServer(decls)
	if len(got) != len(wantServers) {
		t.Fatalf("servers = %v, want %v", got, wantServers)
	}
	for s, n := range wantServers {
		if got[s] != n {
			t.Errorf("server %s: %d tools, want %d", s, got[s], n)
		}
	}
}

// Session start: the listing shares a system-role block with hook text and the skills
// listing, as in the real request.
func TestDeferredReminderRecordsEveryMCPTool(t *testing.T) {
	text := "SessionStart hook additional context: hello\n\n" +
		deferredFixture(t, "deferred-reminder-full-39mcp.txt") + "\n" + skillsReminder
	body := ccBody(t, []string{tool("Bash", "b"), placeholderTool(t)}, text, nil)

	inv := ScanInventory("anthropic", body)

	assertServers(t, inv.Decls)
	for _, d := range inv.Decls {
		if d.Kind == KindMCPTool && d.Tokens <= 0 {
			t.Errorf("%s weighs %d tokens, want a share of the listing", d.Name, d.Tokens)
		}
	}
}

// Claude Code sends one reminder per change. The current set is all of them together.
func TestDeferredRemindersCombine(t *testing.T) {
	body := deferredBody(t, []string{tool("Bash", "b"), placeholderTool(t)}, []string{
		deferredFixture(t, "deferred-reminder-delta1-2mcp.txt"),
		"a user turn",
		deferredFixture(t, "deferred-reminder-delta2-37mcp.txt"),
	}, nil)

	assertServers(t, ScanInventory("anthropic", body).Decls)
}

// The wire form may wrap the list in a system-reminder. The closing tag ends the list; it
// must not read as a line that is not a name and so void the list.
func TestDeferredReminderInsideSystemReminderTag(t *testing.T) {
	text := "<system-reminder>\n" +
		strings.TrimSuffix(deferredFixture(t, "deferred-reminder-full-39mcp.txt"), "\n") +
		"\n</system-reminder>"
	body := deferredBody(t, []string{placeholderTool(t)}, []string{text}, nil)

	assertServers(t, ScanInventory("anthropic", body).Decls)
}

// The listing's measured cost is split equally over every name in it (60 here), and the 39
// MCP tools carry their shares: removing one gives back its own line, not the header or the
// built-in names. The placeholder is a marker, not a declaration.
func TestDeferredReminderCostMovesOffThePlaceholder(t *testing.T) {
	listing := deferredFixture(t, "deferred-reminder-full-39mcp.txt")
	body := deferredBody(t, []string{placeholderTool(t)}, []string{listing}, nil)
	total := tokens.Count(strings.TrimSuffix(listing, "\n"))
	share := total / 60

	inv := ScanInventory("anthropic", body)

	n := 0
	for _, d := range inv.Decls {
		if d.Name == "DeferredToolPlaceholder" {
			t.Errorf("placeholder recorded as %+v", d)
		}
		if d.Kind != KindMCPTool {
			t.Errorf("unexpected non-MCP row %s:%s", d.Kind, d.Name)
			continue
		}
		n++
		if d.Tokens != share && d.Tokens != share+1 {
			t.Errorf("%s = %d tokens, want %d or %d (listing %d / 60 names)", d.Name, d.Tokens, share, share+1, total)
		}
	}
	if n != 39 {
		t.Errorf("MCP rows = %d, want 39", n)
	}
}

// A tool loaded through ToolSearch is in `tools` (defer_loading, 0 tokens) AND in the
// listing. It is one declaration, charged once, and its call counts as a use of it.
func TestDeferredToolLoadedAndCalledIsCountedOnce(t *testing.T) {
	const name = "mcp__context7__resolve-library-id"
	loaded := `{"name":"` + name + `","description":"resolve","input_schema":{"type":"object"},"defer_loading":true}`
	body := deferredBody(t, []string{placeholderTool(t), loaded},
		[]string{deferredFixture(t, "deferred-reminder-full-39mcp.txt")},
		[]map[string]any{{"name": name}})

	inv := ScanInventory("anthropic", body)

	var rows []Decl
	for _, d := range inv.Decls {
		if d.Name == name {
			rows = append(rows, d)
		}
	}
	if len(rows) != 1 || rows[0].Kind != KindMCPTool || rows[0].Server != "context7" || rows[0].Tokens <= 0 {
		t.Fatalf("rows for %s = %+v, want one context7 row with a share of the listing", name, rows)
	}
	if len(inv.Used) != 1 || inv.Used[0].Name != name || inv.Used[0].Server != "context7" || inv.Used[0].Calls != 1 {
		t.Errorf("used = %+v, want one call of %s", inv.Used, name)
	}
}

// The user's own prompt quoted the header twice, followed by prose. Text that is not a
// bare list of names is not a declaration.
func TestQuotedDeferredHeaderIsNotADeclaration(t *testing.T) {
	quoted := "   messages, as a system reminder:\n" +
		"     <system-reminder>\n" +
		"     The following deferred tools are now available via ToolSearch. Their schemas are NOT loaded — ...\n" +
		"     <one tool name per line>\n" +
		"     </system-reminder>\n" +
		"1. Find each message containing \"The following deferred tools are now available via ToolSearch.\" and\n" +
		"   record every mcp__<server>__<tool> line as KindMCPTool with its Server.\n" +
		"The following deferred tools are now available via ToolSearch.\nmcp__evil__x\nnot a name at all\n\n"
	body := deferredBody(t, []string{tool("Bash", "b")}, []string{quoted}, nil)

	inv := ScanInventory("anthropic", body)

	if got := mcpByServer(inv.Decls); len(got) != 0 {
		t.Errorf("quoted header produced MCP rows %v", got)
	}
}

// A tool result is model input, not Claude Code's own declaration: printing a fixture with
// cat must not declare its tools.
func TestDeferredListingInToolResultIsIgnored(t *testing.T) {
	m, _ := json.Marshal(map[string]any{"model": "claude", "messages": []any{
		map[string]any{"role": "user", "content": []any{map[string]any{
			"type": "tool_result", "tool_use_id": "tu_1",
			"content": deferredFixture(t, "deferred-reminder-full-39mcp.txt"),
		}}},
	}})
	body := []byte(`{"tools":[` + tool("Bash", "b") + `],` + string(m)[1:])

	if got := mcpByServer(ScanInventory("anthropic", body).Decls); len(got) != 0 {
		t.Errorf("tool result produced MCP rows %v", got)
	}
}

// The listing feeds the digest: a new reminder is a new declaration set, and is parsed.
func TestDeferredReminderChangeIsReparsed(t *testing.T) {
	tools := []string{tool("Bash", "b"), placeholderTool(t)}
	first := ScanInventory("anthropic", deferredBody(t, tools,
		[]string{deferredFixture(t, "deferred-reminder-delta1-2mcp.txt")}, nil))
	second := ScanInventory("anthropic", deferredBody(t, tools, []string{
		deferredFixture(t, "deferred-reminder-delta1-2mcp.txt"),
		deferredFixture(t, "deferred-reminder-delta2-37mcp.txt"),
	}, nil))

	if first.Digest == second.Digest {
		t.Fatal("a new deferred-tools reminder did not change the digest")
	}
	if got := mcpByServer(first.Decls); len(got) != 1 || got["web-search"] != 2 {
		t.Errorf("first set = %v, want web-search only", got)
	}
	assertServers(t, second.Decls)
}

// The header line holds escaped quotes in the raw body. A list whose names change but whose
// size does not must still change the digest.
func TestDeferredNameChangeIsReparsed(t *testing.T) {
	listing := deferredFixture(t, "deferred-reminder-delta1-2mcp.txt")
	renamed := strings.Replace(listing, "mcp__web-search__WebSearch", "mcp__web-search__WebSearcH", 1)
	tools := []string{placeholderTool(t)}

	first := ScanInventory("anthropic", deferredBody(t, tools, []string{listing}, nil))
	second := ScanInventory("anthropic", deferredBody(t, tools, []string{renamed}, nil))

	if first.Digest == second.Digest {
		t.Fatal("a renamed deferred tool did not change the digest")
	}
	found := false
	for _, d := range second.Decls {
		found = found || d.Name == "mcp__web-search__WebSearcH"
	}
	if !found {
		t.Error("the renamed tool was not parsed")
	}
}

// Without the header nothing changes: the digest is the one the old code computed, so every
// stored declaration set keeps its identity.
func TestNoDeferredReminderKeepsTheDigest(t *testing.T) {
	body := ccBody(t, []string{tool("Bash", "b"), tool("mcp__pw__click", "c")}, skillsReminder, nil)
	var h maphash.Hash
	h.SetSeed(declSeed)
	h.WriteString(gjson.GetBytes(body, "tools").Raw)
	h.WriteString("\x00")
	h.WriteString(skillRegion(body))

	inv := ScanInventory("anthropic", body)

	if want := strconv.FormatUint(h.Sum64(), 16); inv.Digest != want {
		t.Errorf("digest = %s, want the unchanged %s", inv.Digest, want)
	}
	var names []string
	for _, d := range inv.Decls {
		names = append(names, d.Kind+":"+d.Name)
	}
	sort.Strings(names)
	want := "mcp_tool:mcp__pw__click,skill:apps/web:deploy,skill:dataviz,skill:superpowers:brainstorming,skill_listing:,tool:Bash"
	if got := strings.Join(names, ","); got != want {
		t.Errorf("decls = %s, want %s", got, want)
	}
}

// BenchmarkScanInventoryDeferred times the scan on a fixture-built body: warm is every
// request after a session's first, cold is the first (parse and tokenize).
func BenchmarkScanInventoryDeferred(b *testing.B) {
	tools := []string{tool("Bash", "run"), tool("Read", "read"), placeholderTool(b)}
	for _, c := range []struct {
		name string
		text []string
	}{
		{"none", []string{"hello"}},
		{"listing", []string{deferredFixture(b, "deferred-reminder-full-39mcp.txt")}},
	} {
		body := deferredBody(b, tools, c.text, nil)
		b.Run(c.name+"/warm", func(b *testing.B) {
			ScanInventory("anthropic", body)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ScanInventory("anthropic", body)
			}
		})
		b.Run(c.name+"/cold", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				declMu.Lock()
				declCache, declBytes = map[uint64][]Decl{}, 0
				declMu.Unlock()
				ScanInventory("anthropic", body)
			}
		})
	}
}

// The digest covers what the parser reads and nothing after it. A list wrapped in
// <system-reminder> with the closing tag right after the last name, then more text in the same
// block, used to put that text in the digest too: changing only the date gave a new digest, a cold
// parse and a new declaration set.
func TestDeferredDigestStopsAtTheClosingTag(t *testing.T) {
	list := strings.TrimSuffix(deferredFixture(t, "deferred-reminder-delta1-2mcp.txt"), "\n")
	block := func(date string) string {
		return "<system-reminder>\n" + list + "\n</system-reminder>\n<system-reminder>Today's date is " +
			date + ".</system-reminder>"
	}
	tools := []string{placeholderTool(t)}

	a := ScanInventory("anthropic", deferredBody(t, tools, []string{block("2026-10-25")}, nil))
	b := ScanInventory("anthropic", deferredBody(t, tools, []string{block("2026-10-26")}, nil))

	if a.Digest != b.Digest {
		t.Errorf("changing only the text after </system-reminder> changed the digest: %s -> %s", a.Digest, b.Digest)
	}
	if got := mcpByServer(a.Decls); got["web-search"] != 2 {
		t.Errorf("servers = %v, want web-search with 2 tools", got)
	}
}

// Claude Code writes the tag literally in its JSON, not as \u003c…\u003e as Go's encoder does. The
// digest must stop at it in that spelling too. The body is built as raw JSON, as a client sends it.
func TestDeferredDigestStopsAtALiteralClosingTag(t *testing.T) {
	list := strings.TrimSuffix(deferredFixture(t, "deferred-reminder-delta1-2mcp.txt"), "\n")
	enc, err := json.Marshal(list) // a JSON string; escape only what JSON needs, as Claude Code does
	if err != nil {
		t.Fatal(err)
	}
	inner := strings.NewReplacer(`\u003c`, "<", `\u003e`, ">").Replace(string(enc[1 : len(enc)-1]))
	raw := func(date string) []byte {
		text := `<system-reminder>\n` + inner + `\n</system-reminder>\n<system-reminder>Today's date is ` +
			date + `.</system-reminder>`
		return []byte(`{"tools":[` + placeholderTool(t) + `],"model":"claude","messages":[{"role":"user",` +
			`"content":[{"type":"text","text":"` + text + `"}]}]}`)
	}
	if !strings.Contains(string(raw("x")), `\n</system-reminder>`) {
		t.Fatal("the body does not carry the literal tag; the test proves nothing")
	}

	a, b := ScanInventory("anthropic", raw("2026-10-25")), ScanInventory("anthropic", raw("2026-10-26"))

	if a.Digest != b.Digest {
		t.Errorf("a date after a literal </system-reminder> changed the digest: %s -> %s", a.Digest, b.Digest)
	}
	if got := mcpByServer(a.Decls); got["web-search"] != 2 {
		t.Errorf("servers = %v, want web-search with 2 tools (the raw body did not parse)", got)
	}
}

// A client that sends CRLF line endings must not lose the whole list: every name line would end in
// \r, fail isToolName, and void the list with no signal ("0 MCP servers" again).
func TestDeferredListWithCRLFLineEndings(t *testing.T) {
	crlf := strings.ReplaceAll(deferredFixture(t, "deferred-reminder-delta1-2mcp.txt"), "\n", "\r\n")

	inv := ScanInventory("anthropic", deferredBody(t, []string{placeholderTool(t)}, []string{crlf}, nil))

	if got := mcpByServer(inv.Decls); got["web-search"] != 2 {
		t.Errorf("servers = %v, want web-search with 2 tools", got)
	}
}

// KNOWN GAP, pinned so it is visible rather than silent (#728 review). Claude Code also sends
// removals, in wording not captured yet, so the set is everything ADDED: a server removed mid-session
// stays declared until the session ends, and the report keeps offering `claude mcp remove` for it.
// When the removal wording is captured, this test changes to expect the server gone.
//
// Compaction is the other side: if a client stops resending an old list, the set shrinks and the
// digest changes. That is a correct new declaration set, not churn, and it only ever under-reports.
func TestDeferredRemovalIsNotYetSubtracted(t *testing.T) {
	added := deferredFixture(t, "deferred-reminder-delta1-2mcp.txt")
	// A later reminder in a wording this parser does not know (a placeholder for the removal form).
	removed := "The following deferred tools are no longer available:\nmcp__web-search__WebSearch\n\n"

	inv := ScanInventory("anthropic", deferredBody(t, []string{placeholderTool(t)}, []string{added, removed}, nil))

	if got := mcpByServer(inv.Decls); got["web-search"] != 2 {
		t.Errorf("servers = %v; this pins today's behaviour (removals are not subtracted): want web-search 2", got)
	}
}

// The model may repeat the list in its own reply. That is not Claude Code declaring tools: only a
// user or system message's own text counts.
func TestDeferredListInAnAssistantMessageIsIgnored(t *testing.T) {
	m, _ := json.Marshal(map[string]any{"model": "claude", "messages": []any{
		map[string]any{"role": "user", "content": "go"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": deferredFixture(t, "deferred-reminder-full-39mcp.txt")},
		}},
	}})
	body := []byte(`{"tools":[` + tool("Bash", "b") + `],` + string(m)[1:])

	if got := mcpByServer(ScanInventory("anthropic", body).Decls); len(got) != 0 {
		t.Errorf("an assistant message declared MCP rows %v", got)
	}
}
