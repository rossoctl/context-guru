package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// maxLinesShown bounds the numbered body the `lines` strategy shows the model. Lines past it
// cannot be kept, so an output longer than this loses its tail; the acceptance checks still run.
const maxLinesShown = 1500

const linesRules = `Reply with JSON only, no prose: {"keep": [[first, last], ...]}, inclusive line numbers as shown.
Keep every line the agent may need next: errors and tracebacks, failing test names and assertion
messages, file paths, function, class and variable names it is working on, and anything containing
an identifier listed above. Drop repetition, progress bars, passing-test noise, boilerplate and
unrelated code. Keep whole logical units (a function, a traceback, a test failure), not fragments.
Keep at most half of the lines; if nothing can be dropped, reply {"keep": []}.`

// runLines asks the model which line ranges of the body to keep and returns exactly those lines,
// in order. The result is a subsequence of the body by construction, so the deletion-only contract
// cannot be broken by paraphrase; it can only fail by keeping too much or too little.
func runLines(ctx context.Context, body, goal string, keepIDs []string, model Model) (string, string) {
	if model == nil {
		return "", "no model"
	}
	lines := strings.Split(body, "\n")
	shown := lines
	if len(shown) > maxLinesShown {
		shown = shown[:maxLinesShown]
	}
	var sb strings.Builder
	for i, l := range shown {
		fmt.Fprintf(&sb, "%d| %s\n", i+1, l)
	}
	g := strings.TrimSpace(goal)
	if g == "" {
		g = "(no explicit goal stated)"
	}
	keepBlock := ""
	if len(keepIDs) > 0 {
		k := keepIDs
		if len(k) > 60 {
			k = k[:60]
		}
		kb, _ := json.Marshal(k)
		keepBlock = "IDENTIFIERS THE AGENT REFERENCED RECENTLY (keep lines containing them):\n" + string(kb) + "\n\n"
	}
	prompt := "You trim ONE tool output for a coding agent by choosing which of its lines to keep.\n\n" +
		"WHAT THE AGENT IS DOING NOW:\n" + clipGoal(g) + "\n\n" + keepBlock +
		"THE OUTPUT, one numbered line each:\n" + sb.String() + "\n" + linesRules
	out, err := model.Complete(ctx, prompt)
	if err != nil {
		return "", "model call failed: " + err.Error()
	}
	var reply struct {
		Keep [][2]int `json:"keep"`
	}
	if err := json.Unmarshal([]byte(stripFences(out)), &reply); err != nil {
		return "", "reply is not the ranges JSON"
	}
	keep := make([]bool, len(shown))
	for _, r := range reply.Keep {
		for i := max(r[0], 1); i <= min(r[1], len(shown)); i++ {
			keep[i-1] = true
		}
	}
	var kept []string
	for i, k := range keep {
		if k {
			kept = append(kept, lines[i])
		}
	}
	return strings.Join(kept, "\n"), ""
}
