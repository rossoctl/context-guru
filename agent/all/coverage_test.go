package all_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/agent"
	_ "github.com/rossoctl/context-guru/agent/all"
)

// docs/client-coverage.md says, for each client, whether each client-specific feature is covered
// or falls back (#1321). A registered agent without a row fails. A row for a client that is
// neither registered nor listed in pendingRows fails. The table for clients is the first one under
// the "## By client" heading.
const coverageDoc = "../../docs/client-coverage.md"

// pendingRows are clients that have a row but no registered agent yet. Each maps to the issue that
// registers it. agent/codex exists but is a helper, not a registered agent. Remove an entry when its
// agent registers. Then an unregistered row fails.
var pendingRows = map[string]string{
	"claude code": "#1312",
	"codex":       "#1331",
	"opencode":    "#1331",
	"pi":          "#1331",
}

// clientHeading starts the table the test reads. A page without it fails loudly.
const clientHeading = "## By client"

// parseClientRows returns the lower-cased first cell of each row of the first table after the
// "## By client" heading. A missing heading or an empty table is an error, never an empty result.
func parseClientRows(doc string) (map[string]bool, error) {
	_, after, found := strings.Cut("\n"+doc, "\n"+clientHeading+"\n")
	if !found {
		return nil, fmt.Errorf("no %q heading", clientHeading)
	}
	rows := map[string]bool{}
	inTable := false
	for _, line := range strings.Split(after, "\n") {
		if !strings.HasPrefix(line, "|") {
			if inTable {
				break
			}
			if strings.HasPrefix(line, "## ") {
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
	if len(rows) == 0 {
		return nil, fmt.Errorf("no client rows under %q", clientHeading)
	}
	return rows, nil
}

func TestParseClientRowsFailsLoudly(t *testing.T) {
	table := "| Client | A |\n|---|---|\n| Bob | x |\n"
	if _, err := parseClientRows("# T\n\n| Other | A |\n|---|---|\n| X | y |\n"); err == nil {
		t.Error("a page without the heading must fail")
	}
	if _, err := parseClientRows("# T\n\n## By client\n\nno table\n\n## By host\n\n" + table); err == nil {
		t.Error("a heading without a table must fail, not read the next table")
	}
	if rows, err := parseClientRows("## By client\n\n" + table); err != nil || !rows["bob"] {
		t.Errorf("a heading at the start of the page: rows = %v, err = %v; want bob", rows, err)
	}
	rows, err := parseClientRows("| Early | A |\n|---|---|\n| X | y |\n\n## By client\n\n" + table)
	if err != nil || !rows["bob"] || rows["x"] {
		t.Errorf("rows = %v, err = %v; want only bob, from the table under the heading", rows, err)
	}
}

func clientRows(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(coverageDoc)
	if err != nil {
		t.Fatalf("read %s: %v", coverageDoc, err)
	}
	rows, err := parseClientRows(string(b))
	if err != nil {
		t.Fatalf("%s: %v", coverageDoc, err)
	}
	return rows
}

func TestClientCoverageHasARowForEveryAgent(t *testing.T) {
	rows := clientRows(t)
	registered := map[string]bool{}
	for _, a := range agent.All() {
		name := strings.ToLower(a.Name())
		registered[name] = true
		if !rows[name] {
			t.Errorf("agent %q is registered but has no row in %s", a.Name(), coverageDoc)
		}
	}
	for name := range rows {
		if !registered[name] && pendingRows[name] == "" {
			t.Errorf("%s: row %q is not a registered agent (add it to pendingRows with its issue)", coverageDoc, name)
		}
	}
	for name := range pendingRows {
		if !rows[name] {
			t.Errorf("pendingRows lists %q but %s has no such row", name, coverageDoc)
		}
	}
}
