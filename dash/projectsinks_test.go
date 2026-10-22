package dash

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A credential-shaped launch directory must not survive on ANY sink: the requests table, the
// tool_uses table, or the live feed. Sanitising only the INSERT argument left the other two raw.
const canaryDir = "/home/u/sk-ant-api03-ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"

func TestRedactSanitisesTheProject(t *testing.T) {
	e := &Event{Project: canaryDir}
	e.Redact()
	if strings.Contains(e.Project, "sk-ant") {
		t.Fatalf("Event.Redact left the credential in Project: %q", e.Project)
	}
}

func TestACredentialDirectoryNeverReachesTheFeedOrToolUses(t *testing.T) {
	rec, err := NewRecorder(Options{DBPath: filepath.Join(t.TempDir(), "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	c, _ := rec.Hub().subscribe("", true)
	rec.Record(&Event{TS: 1000, SessionID: "s1", Model: "m", Project: canaryDir})
	inv := ScanInventory("anthropic", ccBody(t, []string{tool("Bash", "b")}, skillsReminder,
		[]map[string]any{{"name": "Bash", "input": map[string]any{}}}))
	rec.RecordInventory("", "s1", 1000, inv, false, canaryDir)

	select {
	case e := <-c.ch:
		if strings.Contains(e.Project, "sk-ant") {
			t.Errorf("the feed carried the credential: %q", e.Project)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event published")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		rec.DB().sql.QueryRow(`SELECT COUNT(*) FROM tool_uses`).Scan(&n)
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no tool_uses row written")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var bad int
	rec.DB().sql.QueryRow(`SELECT COUNT(*) FROM tool_uses WHERE project LIKE '%sk-ant%'`).Scan(&bad)
	if bad != 0 {
		t.Errorf("%d tool_uses rows carry the credential", bad)
	}
}
