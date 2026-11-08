package compactionpoint

import (
	"math"
	"strconv"
	"strings"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// ClaudeCode is what the user set to move Claude Code's own auto-compact point (#948). A host
// fills it once, at start-up, from its env and the user's settings files (proxy.ReadClaudeCompact).
// Every field is the RAW text of the setting, so the
// parsing rules below live in one place, next to the client rules they copy.
//
// WHERE EACH SETTING IS FROM. Read from the shipped client, `claude` 2.1.319 (its bundled
// JavaScript, `strings` on the binary), not from memory:
//
//   - CLAUDE_CODE_AUTO_COMPACT_WINDOW (env): the window the client counts against, in tokens.
//     Accepts `200000`, `200k`, `1m`. Below 100,000 it is raised to 100,000. Above 1,000,000 it
//     is lowered to 1,000,000. Not a number: ignored. The effective window is
//     min(model window, this).
//   - `autoCompactWindow` (settings.json, top level) and `modelSettings.<model>.autoCompactWindow`
//     (what `/autocompact` writes): the same, in tokens, or "auto" for the client default. The
//     env var wins over both. A model entry wins over the top-level one.
//   - CLAUDE_AUTOCOMPACT_PCT_OVERRIDE (env): a percent, 0 < p <= 100, of the effective window
//     less the output reserve. It can only LOWER the threshold: the client takes the smaller of
//     it and the default.
//   - DISABLE_AUTO_COMPACT and DISABLE_COMPACT (env; the values 1, true, yes, on), and
//     `autoCompactEnabled: false` (settings.json or ~/.claude.json): auto-compact is off.
//
// THE THRESHOLD. With W the effective window, the client compacts when its token count reaches
//
//	T = (W - 20,000) - 13,000                         (the output reserve, then the buffer)
//	T = min(floor((W - 20,000) x pct / 100), T)       (when the percent override is set)
//
// 20,000 is min(model max output, 20,000); every current model has a larger maximum. These
// are constants in the client (AZr and the L4 function). Nothing is guessed from max_tokens.
type ClaudeCode struct {
	// Env values, raw: CLAUDE_CODE_AUTO_COMPACT_WINDOW, CLAUDE_AUTOCOMPACT_PCT_OVERRIDE,
	// DISABLE_AUTO_COMPACT, DISABLE_COMPACT.
	EnvWindow, EnvPct, EnvDisableAuto, EnvDisable string
	// Settings values, raw, already merged across the settings files by the host.
	SettingsWindow string
	// SettingsEnabled is "true", "false" or "" (not set).
	SettingsEnabled string
	// ModelWindows maps a model name from modelSettings to its autoCompactWindow, raw.
	ModelWindows map[string]string
}

// ClientToBilled converts the client's own token count to provider-billed tokens. 1.0 means the
// threshold is taken as billed tokens.
//
// NOT MEASURED AS A CONVERSION. The two data points disagree:
//   - 13 real Claude Code sessions (#940) auto-compacted at 164k to 167k billed, which is the
//     client threshold itself (W - 33k for a 200k window): a factor of about 1.0.
//   - One haiku session (the table above) billed 199,184 when the client's own count read about
//     168,000: a factor of 1.186.
//
// 1.0 is the lower, so it makes C smaller. A smaller C is the conservative direction for the
// replay cap and for the summarize trigger (see THE STATISTIC above). Change this one constant
// when a measurement settles it.
const ClientToBilled = 1.0

const (
	minWindow     = 100_000
	maxWindow     = 1_000_000
	outputReserve = 20_000
	buffer        = 13_000
)

// Resolve gives C for the model when the user changed a setting. ok is false when nothing was
// changed or nothing is readable: the caller then uses For(model), which is the old behavior.
// window is the model's context window (0 = unknown).
func (c ClaudeCode) Resolve(model string, window int) (p Point, ok bool) {
	if truthy(c.EnvDisable) || truthy(c.EnvDisableAuto) || strings.EqualFold(strings.TrimSpace(c.SettingsEnabled), "false") {
		// Auto-compact is off. The conversation grows to the window.
		return Point{Frac: 1.00, Source: Configured,
			Note: "the user turned off Claude Code's auto-compact (DISABLE_AUTO_COMPACT, DISABLE_COMPACT or " +
				"autoCompactEnabled=false), so C is the whole window. The client's hard limit sits a little below it."}, true
	}
	w, wsrc := c.configuredWindow(model)
	pct := parsePct(c.EnvPct)
	if w == 0 && pct == 0 {
		return Point{}, false
	}
	eff := window
	if w > 0 && (eff <= 0 || w < eff) {
		eff = w
	}
	if eff <= 0 {
		return Point{}, false // a percent alone, and no window to apply it to
	}
	ure := eff - outputReserve
	if ure <= buffer {
		return Point{}, false
	}
	t := ure - buffer
	if pct > 0 {
		if v := int(math.Floor(float64(ure) * pct / 100)); v < t {
			t = v
		}
	}
	abs := int(float64(t) * ClientToBilled)
	if abs <= 0 {
		return Point{}, false
	}
	note := "the user's own Claude Code setting (" + wsrc + "), threshold = (window - 20,000) - 13,000 in the " +
		"client's token count, taken as billed tokens by a factor of 1.0 that is NOT measured. "
	if pct > 0 {
		note += "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE applies. "
	}
	if window > 0 {
		return Point{Frac: float64(abs) / float64(window), Abs: abs, Source: Configured, Note: note}, true
	}
	return Point{Abs: abs, Source: Configured, Note: note}, true
}

// configuredWindow returns the window the user set (0 = none) and where it came from.
func (c ClaudeCode) configuredWindow(model string) (int, string) {
	if v := parseWindow(c.EnvWindow); v > 0 {
		return v, "CLAUDE_CODE_AUTO_COMPACT_WINDOW"
	}
	id := modelinfo.NormalizeID(model)
	best, bestLen := "", 0
	for name, raw := range c.ModelWindows {
		n := modelinfo.NormalizeID(name)
		if n != "" && len(n) > bestLen && strings.Contains(id, n) {
			best, bestLen = raw, len(n)
		}
	}
	if v := parseWindow(best); v > 0 {
		return v, "modelSettings autoCompactWindow"
	}
	if v := parseWindow(c.SettingsWindow); v > 0 {
		return v, "autoCompactWindow"
	}
	return 0, ""
}

// parseWindow copies the client's rule: 1.5k, 200k, 1m or a bare number; below the minimum is
// raised, above the maximum is lowered, anything else is 0 (ignored).
func parseWindow(raw string) int {
	r := strings.TrimSpace(raw)
	if r == "" || len(r) > 16 {
		return 0
	}
	mult := 1.0
	switch strings.ToLower(r[len(r)-1:]) {
	case "k":
		mult, r = 1000, r[:len(r)-1]
	case "m":
		mult, r = 1e6, r[:len(r)-1]
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(r), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	f *= mult
	switch {
	case f < 1:
		return 0
	case f < minWindow:
		return minWindow
	case f > maxWindow:
		return maxWindow
	}
	return int(math.Round(f))
}

func parsePct(raw string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(f) || f <= 0 || f > 100 {
		return 0
	}
	return f
}

func truthy(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
