// Golden tests for scripts/cgcost.py, the five cost commands (today, unused, cold, compact,
// trends). The inputs are a synthetic bundle of API responses (testdata/cost/bundle.json, no
// production data) so the output is fully determined; set UPDATE_GOLDEN=1 to rewrite the goldens
// after an intended change, and read the diff.
package plugin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runCost(t *testing.T, args ...string) string {
	t.Helper()
	requireTool(t, "python3")
	base := []string{filepath.Join("scripts", "cgcost.py")}
	args = append(base, append(args,
		"--fixture", filepath.Join("testdata", "cost", "bundle.json"),
		"--projects-dir", filepath.Join("testdata", "cost", "projects"),
		"--now", "2026-10-08T15:30:00Z", "--tz-offset-min", "0")...)
	cmd := exec.Command("python3", args...)
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cgcost.py %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestCostGoldens(t *testing.T) {
	for _, c := range []string{"today", "unused", "cold", "compact", "trends"} {
		t.Run(c, func(t *testing.T) {
			got := runCost(t, c, "--days", "14")
			if strings.Contains(got, "\x1b[") {
				t.Errorf("ANSI escapes in output without a tty / with NO_COLOR:\n%s", got)
			}
			golden := filepath.Join("testdata", "cost", c+".golden")
			if os.Getenv("UPDATE_GOLDEN") != "" {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("%s output changed (UPDATE_GOLDEN=1 to accept):\n--- want\n%s\n--- got\n%s", c, want, got)
			}
		})
	}
}

// The unused list must never offer Context Guru's own server or skills, whatever they cost, and
// must warn before suggesting removal of something used recently.
func TestCostUnusedSafety(t *testing.T) {
	got := runCost(t, "unused", "--days", "14")
	for _, own := range []string{"context-guru", "context-guru:status"} {
		if strings.Contains(got, own) {
			t.Errorf("unused offers Context Guru's own %q for removal:\n%s", own, got)
		}
	}
	if !strings.Contains(got, "figma") || !strings.Contains(got, "used 4d ago - check first") {
		t.Errorf("a server called 4 days ago must carry a caution:\n%s", got)
	}
	if strings.Contains(got, "github") {
		t.Errorf("a server that was called must not be listed:\n%s", got)
	}
}

// After the TTL the next turn is a rewrite whatever the user does, so the report must not claim
// that compacting saves it; before the TTL it must show the break-even.
func TestCostCompactIsHonestAboutExpiry(t *testing.T) {
	got := runCost(t, "compact")
	if !strings.Contains(got, "saves nothing on this turn") {
		t.Errorf("expired session must say compacting does not avoid the rewrite:\n%s", got)
	}
	if !strings.Contains(got, "chance you will be away") {
		t.Errorf("warm session must state the break-even:\n%s", got)
	}
}
