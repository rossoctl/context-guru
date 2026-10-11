package dash

import (
	"fmt"
	"testing"
	"time"
)

func TestAutoEligibleDecls(t *testing.T) {
	db := openTestDB(t)
	day := int64(24 * time.Hour / time.Millisecond)
	decl := func(tenant, name, kind string, sessions int, spanDays int64) {
		for i := 0; i < sessions; i++ {
			ts := int64(1_000_000_000_000) + int64(i)*spanDays*day/int64(max(sessions-1, 1))
			if _, err := db.sql.Exec(`INSERT INTO tool_declarations(tenant_id, session_id, digest, kind, name, ts) VALUES(?,?,?,?,?,?)`,
				tenant, fmt.Sprintf("%s-%s-%d", tenant, name, i), "d", kind, name, ts); err != nil {
				t.Fatal(err)
			}
		}
	}
	decl("a", "Old", "tool", 5, 8)           // eligible
	decl("a", "Few", "tool", 4, 8)           // too few sessions
	decl("a", "Short", "tool", 6, 3)         // span too short
	decl("a", "Used", "tool", 6, 8)          // called
	decl("a", "mcp__s__t", "mcp_tool", 5, 9) // eligible
	decl("a", "Skill", "skill", 6, 8)        // skills never
	decl("b", "OtherTenant", "tool", 6, 8)   // other tenant
	if _, err := db.sql.Exec(`INSERT INTO tool_uses(tenant_id, session_id, name) VALUES('a','s','Used')`); err != nil {
		t.Fatal(err)
	}
	got, err := db.AutoEligibleDecls("a", AutoMinSessions, AutoMinSpan)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[Old mcp__s__t]" {
		t.Fatalf("got %v", got)
	}
}
