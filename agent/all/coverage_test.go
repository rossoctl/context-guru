package all_test

import (
	"os"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/agent"
	_ "github.com/rossoctl/context-guru/agent/all"
)

// docs/client-coverage.md says, for each client, whether each client-specific feature is covered
// or falls back (#1321). A registered agent without a row fails. A row for a client that is
// neither registered nor listed in pendingRows fails. The table for clients is the first one in
// the page, under "By client".
const coverageDoc = "../../docs/client-coverage.md"

// pendingRows are clients that have a row but no registered agent yet.
// TODO(#1312): remove an entry when its agent package registers. Then an unregistered row fails.
var pendingRows = map[string]bool{
	"claude code": true,
	"codex":       true,
	"opencode":    true,
	"pi":          true,
}

// clientRows returns the lower-cased first cell of each row of the first table in the page.
func clientRows(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(coverageDoc)
	if err != nil {
		t.Fatalf("read %s: %v", coverageDoc, err)
	}
	rows := map[string]bool{}
	inTable := false
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "|") {
			if inTable {
				break
			}
			continue
		}
		inTable = true
		cells := strings.Split(strings.Trim(line, "|"), "|")
		name := strings.ToLower(strings.TrimSpace(cells[0]))
		if name == "client" || strings.HasPrefix(name, "---") {
			continue
		}
		rows[name] = true
	}
	return rows
}

func TestClientCoverageHasARowForEveryAgent(t *testing.T) {
	rows := clientRows(t)
	if len(rows) == 0 {
		t.Fatalf("%s: no client rows", coverageDoc)
	}
	registered := map[string]bool{}
	for _, a := range agent.All() {
		name := strings.ToLower(a.Name())
		registered[name] = true
		if !rows[name] {
			t.Errorf("agent %q is registered but has no row in %s", a.Name(), coverageDoc)
		}
	}
	for name := range rows {
		if !registered[name] && !pendingRows[name] {
			t.Errorf("%s: row %q is not a registered agent (add it to pendingRows with an issue)", coverageDoc, name)
		}
	}
	for name := range pendingRows {
		if !rows[name] {
			t.Errorf("pendingRows lists %q but %s has no such row", name, coverageDoc)
		}
	}
}
