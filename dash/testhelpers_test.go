package dash

import (
	"strings"
	"testing"
	"time"
)

// stripComments drops comment lines so an assertion about RENDERED text cannot be satisfied by
// prose explaining the very thing that is missing. An earlier version of these checks passed on
// a file header while the explanation had been deleted from the view.
func stripComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "*") || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "/*") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// daysAgo is a timestamp n days before now, as epoch ms — transcripts are dated in real time, so
// the fixtures have to be too. Named around the existing `day` helper in this package.
func daysAgo(n int) int64 {
	return time.Now().AddDate(0, 0, -n).UnixMilli()
}

// insertUse records one invocation in tool_uses, keyed by the table's real primary key (tenant,
// session, project, name, skill), adding to an existing row's calls. Through the key, not a plain
// INSERT: a test that `project` keeps two folders apart must see them collapse if it were not in it.
func insertUse(t *testing.T, db *DB, session, name, skill, project string, ts int64) {
	t.Helper()
	if _, err := db.sql.Exec(`INSERT INTO tool_uses(tenant_id, session_id, name, server, skill, calls,
		first_ts, last_ts, project) VALUES ('', ?, ?, '', ?, 1, ?, ?, ?)
		ON CONFLICT(tenant_id, session_id, project, name, skill) DO UPDATE SET calls = calls + 1,
		last_ts = MAX(last_ts, excluded.last_ts)`, session, name, skill, ts, ts, project); err != nil {
		t.Fatal(err)
	}
}
