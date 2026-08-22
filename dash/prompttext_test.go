package dash

// The prompt-text feature's two load-bearing behaviours: the consent gate, and the
// coverage count that stands in for the rows written before the column existed.
//
// Both are here rather than folded into toolinventory_test.go because both are about a
// NEGATIVE — what is NOT stored, and what a reader is told when nothing is — and that is
// the half of a capture feature that rots silently. A test that only checks the happy path
// would pass just as well if the gate were deleted.

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sysBody is ccBody plus a top-level system prompt, which is what a real Claude Code
// request carries and what the marker row is built from.
func sysBody(t *testing.T, system string, tools []string, reminders string) []byte {
	t.Helper()
	body := ccBody(t, tools, reminders, nil)
	sys, err := json.Marshal([]any{map[string]any{"type": "text", "text": system}})
	if err != nil {
		t.Fatal(err)
	}
	return []byte(`{"system":` + string(sys) + `,` + string(body)[1:])
}

// recWithInventory runs one inventory through the real recorder path and waits for the
// writer goroutine to land it, which is the only way to test the writer's own gate.
func recWithInventory(t *testing.T, inv *Inventory, withText bool) *DB {
	t.Helper()
	rec, err := NewRecorder(Options{DBPath: filepath.Join(t.TempDir(), "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	rec.RecordInventory("t1", "s1", 1000, inv, withText)
	db := rec.DB()
	deadline := time.Now().Add(10 * time.Second)
	for {
		d, _, err := db.countInventoryRows()
		if err != nil {
			t.Fatal(err)
		}
		if d > 0 {
			return db
		}
		if time.Now().After(deadline) {
			t.Fatal("inventory rows never appeared")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// textRows counts the stored / unstored split straight off the table, because that is the
// fact the gate is about and every read path derives its answer from it.
//
// Through declHasText, the same fragment the read paths use, so a test cannot keep passing by
// asking about a column the reader has stopped looking at — which is what happened when the
// text moved to declaration_text.
func textRows(t *testing.T, db *DB) (rows, withText int) {
	t.Helper()
	err := db.sql.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN `+declHasText+` THEN 1 ELSE 0 END), 0)
		FROM tool_declarations d`).Scan(&rows, &withText)
	if err != nil {
		t.Fatal(err)
	}
	return rows, withText
}

// storedTexts is every piece of prompt text the database holds, read THROUGH the join the
// reveal uses. Any test that asserts about what reached disk goes through here: reading the
// declaration row's own blob would assert about a column nothing serves any more.
func storedTexts(t *testing.T, db *DB) []string {
	t.Helper()
	rows, err := db.sql.Query(`SELECT ` + declTextCol + ` FROM tool_declarations d ` +
		declTextJoin + ` WHERE ` + declHasText)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var gz []byte
		if err := rows.Scan(&gz); err != nil {
			t.Fatal(err)
		}
		out = append(out, gunzipText(gz))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPromptTextIsStoredOnlyWithContentConsent(t *testing.T) {
	body := sysBody(t, "You are Claude Code.", []string{tool("Bash", "run a command")}, skillsReminder)
	inv := ScanInventory("anthropic", body)
	if inv == nil {
		t.Fatal("no inventory scanned")
	}
	// The scan ALWAYS carries the text: it is already in the body, and gating it here would
	// poison declCache, whose entries are shared by every tenant declaring the same set.
	var sawText bool
	for _, d := range inv.Decls {
		if d.Text != "" {
			sawText = true
		}
	}
	if !sawText {
		t.Error("scan carried no declaration text; the writer has nothing to gate")
	}
	if inv.System == nil || inv.System.Text == "" || inv.System.Tokens == 0 {
		t.Fatalf("system prompt not scanned: %+v", inv.System)
	}

	// Consent absent: every row is still written, and not one of them has text.
	db := recWithInventory(t, inv, false)
	rows, with := textRows(t, db)
	if rows == 0 {
		t.Fatal("consent absent dropped the whole inventory; only the TEXT is gated")
	}
	if with != 0 {
		t.Errorf("%d of %d rows stored prompt text without consent", with, rows)
	}
	// The weights survive, which is the point of gating the text rather than the feature:
	// an account that declined transcript capture still gets its inventory.
	var tok int
	if err := db.sql.QueryRow(`SELECT MAX(tokens) FROM tool_declarations
		WHERE kind = ?`, KindSystemPrompt).Scan(&tok); err != nil {
		t.Fatal(err)
	}
	if tok == 0 {
		t.Error("the system prompt's token weight was gated too; only its text is content")
	}

	// Consent present: the same inventory stores text.
	db2 := recWithInventory(t, inv, true)
	rows2, with2 := textRows(t, db2)
	if with2 == 0 {
		t.Fatalf("consent present stored no text at all (%d rows)", rows2)
	}
}

func TestPromptViewReportsNotCapturedRatherThanAnEmptyPrompt(t *testing.T) {
	inv := ScanInventory("anthropic", sysBody(t, "You are Claude Code.",
		[]string{tool("Bash", "run a command")}, skillsReminder))
	db := recWithInventory(t, inv, false)
	// A request row, so the scope subquery selects the session at all.
	seedRequest(t, db, "t1", "s1")

	v, err := db.PromptViewFor(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if v.Captured {
		t.Error("Captured is true with no stored text")
	}
	if v.Rows == 0 || v.TextRows != 0 {
		t.Errorf("coverage count wrong: rows=%d text_rows=%d, want rows>0 and text_rows=0",
			v.Rows, v.TextRows)
	}
	// The WEIGHTS are still served, so the panel can say "12,400 tokens, text not captured"
	// instead of rendering nothing and looking broken.
	if v.Tokens == 0 || len(v.Regions) == 0 {
		t.Fatalf("no regions served without text: %d tokens, %d regions", v.Tokens, len(v.Regions))
	}
	for _, r := range v.Regions {
		if r.HasText || r.Text != "" {
			t.Errorf("region %q carried text with no consent", r.Name)
		}
	}
}

func TestPromptViewServesTheSystemPromptAsItsOwnRegion(t *testing.T) {
	const system = "You are Claude Code, Anthropic's official CLI."
	inv := ScanInventory("anthropic", sysBody(t, system,
		[]string{tool("Bash", "run a command"), tool("Read", "read a file")}, skillsReminder))
	db := recWithInventory(t, inv, true)
	seedRequest(t, db, "t1", "s1")

	v, err := db.PromptViewFor(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if !v.Captured {
		t.Fatal("Captured false with consent and stored text")
	}
	// The system prompt is pinned first: it is the region every tool schema sits beside, and
	// a reader looking for "where did my prompt go" must not have to find it in the list.
	if len(v.Regions) == 0 || v.Regions[0].Kind != KindSystemPrompt {
		t.Fatalf("first region is %q, want %q", v.Regions[0].Kind, KindSystemPrompt)
	}
	if v.Regions[0].Text != system {
		t.Errorf("system region text = %q, want %q", v.Regions[0].Text, system)
	}
	var share float64
	byName := map[string]PromptRegion{}
	for _, r := range v.Regions {
		share += r.Share
		byName[r.Name] = r
	}
	if share < 99.9 || share > 100.1 {
		t.Errorf("region shares sum to %.2f%%, want 100%%", share)
	}
	if got := byName["Bash"].Text; !strings.Contains(got, "run a command") {
		t.Errorf("Bash region text = %q, want the declaration's own JSON", got)
	}
}

func TestSystemPromptRowIsNotCountedAsADeclaration(t *testing.T) {
	// A system prompt heavy enough that folding it into the declaration set would be
	// unmissable — which is the bug: share_pct would be a share of the wrong whole, and a
	// "tool" named by the prompt's own hash would top the removal list.
	inv := ScanInventory("anthropic", sysBody(t, strings.Repeat("system prompt prose. ", 400),
		[]string{tool("Bash", "run a command")}, skillsReminder))
	db := recWithInventory(t, inv, true)
	seedRequest(t, db, "t1", "s1")

	rep, err := db.ToolReportFor(Filter{TenantAll: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, ts := range rep.Tools {
		if ts.Kind == KindSystemPrompt || ts.Name == inv.System.Hash {
			t.Fatalf("the system prompt was reported as a removable declaration: %+v", ts)
		}
	}
	if rep.Prompt.Tokens != inv.System.Tokens {
		t.Errorf("Prompt.Tokens = %d, want the measured %d", rep.Prompt.Tokens, inv.System.Tokens)
	}
	if rep.Prompt.Sessions != 1 {
		t.Errorf("Prompt.Sessions = %d, want 1", rep.Prompt.Sessions)
	}
	// declared_set_tokens is the whole every share is a part of, so the system prompt must
	// not be inside it.
	if rep.Totals.DeclaredSetTokens >= inv.System.Tokens {
		t.Errorf("declared_set_tokens = %d, which has swallowed the %d-token system prompt",
			rep.Totals.DeclaredSetTokens, inv.System.Tokens)
	}
	if rep.Prompt.TextRows == 0 || rep.Prompt.Rows == 0 {
		t.Errorf("coverage count absent: rows=%d text_rows=%d", rep.Prompt.Rows, rep.Prompt.TextRows)
	}
}

func TestStoredPromptTextIsScrubbedAndCapped(t *testing.T) {
	// A credential-shaped string inside a tool description, which is exactly how one gets
	// into a prompt: somebody documents their API with a real key in the example.
	const secret = "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	inv := ScanInventory("anthropic", sysBody(t, "auth with "+secret,
		[]string{tool("Bash", "call it with "+secret)}, skillsReminder))
	db := recWithInventory(t, inv, true)

	texts := storedTexts(t, db)
	if len(texts) == 0 {
		t.Fatal("no text rows to check")
	}
	for _, got := range texts {
		if strings.Contains(got, secret) {
			t.Fatalf("a credential reached disk in stored prompt text: %q", got)
		}
	}

	// The cap, on the one field a caller sizes. Only just over it: the scan BPE-tokenizes
	// what it measures, so a 200 KB fixture costs two minutes of test time to prove the same
	// thing an 80 KB one proves.
	big := ScanInventory("anthropic", sysBody(t, strings.Repeat("x", 80<<10),
		[]string{tool("Bash", strings.Repeat("y", 80<<10))}, skillsReminder))
	db2 := recWithInventory(t, big, true)
	for _, got := range storedTexts(t, db2) {
		// The cap plus RedactContent's own truncation marker, which it appends after
		// cutting — the same overshoot request_content.before_gz has, and the marker is the
		// point: a silently shortened prompt would read as the whole prompt.
		if len(got) > maxDeclTextBytes+64 {
			t.Errorf("stored region is %d bytes, over the %d cap", len(got), maxDeclTextBytes)
		}
	}
}

func TestSessionSystemPromptRowsAreBounded(t *testing.T) {
	// A caller whose system prompt carries a clock: without the cap this writes one
	// multi-kilobyte blob per request forever.
	rec, err := NewRecorder(Options{DBPath: filepath.Join(t.TempDir(), "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	tools := []string{tool("Bash", "run a command")}
	for i := 0; i < 20; i++ {
		inv := ScanInventory("anthropic", sysBody(t,
			fmt.Sprintf("You are Claude Code. It is %d.", i), tools, skillsReminder))
		rec.RecordInventory("t1", "s1", int64(1000+i), inv, true)
	}
	db := rec.DB()
	deadline := time.Now().Add(10 * time.Second)
	var n int
	for {
		if err := db.sql.QueryRow(`SELECT COUNT(*) FROM tool_declarations WHERE kind = ?`,
			KindSystemPrompt).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Give the writer a moment to over-write if it is going to.
	time.Sleep(3 * invFlushGap)
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM tool_declarations WHERE kind = ?`,
		KindSystemPrompt).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 || n > maxSessionSystemRows {
		t.Errorf("%d system-prompt rows for one session, want 1..%d", n, maxSessionSystemRows)
	}
}

// seedRequest gives a session one request row, so the scope subquery every read path uses
// (`session_id IN (SELECT ... FROM requests WHERE ... AND tools > 0)`) selects it.
func seedRequest(t *testing.T, db *DB, tenant, session string) {
	t.Helper()
	if _, err := db.sql.Exec(`INSERT INTO requests(ts, tenant_id, session_id, model, tools)
		VALUES (?,?,?,'claude',1)`, time.Now().UnixMilli(), tenant, session); err != nil {
		t.Fatal(err)
	}
}

// TestSystemPromptIsRecordedForEveryDeclarationSet guards the gap between how the writer
// dedups and how the reader selects.
//
// PromptViewFor picks ONE (session, digest) pair and shows its rows. The writer used to dedup
// the system prompt per SESSION, so a session that changed its declaration set mid-way — 34 of
// 135 production sessions do — filed its system prompt only under the FIRST digest, and a view
// that landed on a later one rendered a prefix with the largest region silently missing while
// the tile above it reported that a system prompt exists.
func TestSystemPromptIsRecordedForEveryDeclarationSet(t *testing.T) {
	rec, err := NewRecorder(Options{DBPath: filepath.Join(t.TempDir(), "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	const system = "You are Claude Code."
	// The SAME system prompt under two different declaration sets, which is the ordinary case:
	// a session that adds an MCP server keeps its system prompt.
	sets := [][]string{
		{tool("Bash", "run a command")},
		{tool("Bash", "run a command"), tool("mcp__srv__thing", "an added server")},
	}
	var digests []string
	for i, set := range sets {
		inv := ScanInventory("anthropic", sysBody(t, system, set, skillsReminder))
		if inv == nil {
			t.Fatal("no inventory scanned")
		}
		digests = append(digests, inv.Digest)
		rec.RecordInventory("t1", "s1", int64(1000+i), inv, true)
	}
	if digests[0] == digests[1] {
		t.Fatal("the two declaration sets hashed the same; the fixture proves nothing")
	}
	db := rec.DB()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := db.sql.QueryRow(`SELECT COUNT(DISTINCT digest) FROM tool_declarations
			WHERE kind = ?`, KindSystemPrompt).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == len(sets) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d declaration sets recorded the session's system prompt; a view "+
				"landing on the one without it would show a prefix missing its largest region",
				n, len(sets))
		}
		time.Sleep(10 * time.Millisecond)
	}
	// And whichever set the reader lands on serves a system region.
	seedRequest(t, db, "t1", "s1")
	v, err := db.PromptViewFor(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	var sys bool
	for _, r := range v.Regions {
		if r.Kind == KindSystemPrompt {
			sys = true
		}
	}
	if !sys {
		t.Errorf("the selected prefix (digest %s) has no system region", v.Digest)
	}
}
