package extract

import (
	"regexp"
	"strings"
)

// Deterministic, model-free projection: filter a parsed value to the parts that match
// the keep-set (plus the important-key "spine"). Every leaf it emits is an unchanged
// value, a string prefix, or a contiguous window, so its output always passes
// IsContained by construction and is never empty. Ported from the reference prototype's
// deterministic_project.

var importantKeyTokens = []string{"id", "status", "state", "name", "error", "reason", "date", "time"}

var jsonKeyRe = regexp.MustCompile(`"([A-Za-z_][A-Za-z0-9_\-]{1,})"\s*:`)

func isImportantKey(keyLower string) bool {
	for _, tok := range importantKeyTokens {
		if strings.Contains(keyLower, tok) {
			return true
		}
	}
	return false
}

func termMatches(term, keyLower string) bool {
	return term == keyLower || strings.Contains(keyLower, term) || strings.Contains(term, keyLower)
}

func truncateValue(value any, maxChars int) any {
	if value == nil || maxChars <= 0 {
		return value
	}
	switch v := value.(type) {
	case string:
		if r := []rune(v); len(r) > maxChars {
			return string(r[:maxChars])
		}
		return v
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = truncateValue(x, maxChars)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[k] = truncateValue(x, maxChars)
		}
		return out
	}
	return value
}

// extractTextWindow cuts a bounded window of a text body around the first matching term.
//
// It is LINE-ALIGNED and MARKED (windowLines), because the unaligned unmarked version was a
// silent data loss: a mid-line cut of an `ls -l` listing produced `-rw-r--r--@ 1 itayn staff
// 1498 Aug ` as its final record and dropped two of four directories with nothing to say so.
// A window is a legitimate reduction only when the reader can see it is a window — otherwise
// capTruncated refuses it downstream, which is the backstop for any strategy that cuts
// without saying so.
func extractTextWindow(text string, terms []string, maxChars int) string {
	if maxChars <= 0 || len([]rune(text)) <= maxChars {
		return text
	}
	return windowLines(text, terms, maxChars)
}

func projectMapping(m map[string]any, terms map[string]struct{}, maxChars int) map[string]any {
	out := map[string]any{}
	for key, value := range m {
		kl := strings.ToLower(key)
		keep := isImportantKey(kl)
		if !keep {
			for t := range terms {
				if termMatches(t, kl) {
					keep = true
					break
				}
			}
		}
		if !keep {
			if s, ok := value.(string); ok {
				vl := strings.ToLower(s)
				for t := range terms {
					if strings.Contains(vl, t) {
						keep = true
						break
					}
				}
			}
		}
		if keep {
			out[key] = truncateValue(value, maxChars)
		}
	}
	return out
}

// DeterministicProject filters a parsed value to the keep-set + important-key spine.
// Never empties: a value that projects to nothing falls back to a truncated copy.
func DeterministicProject(value any, keepIDs []string, maxChars int) any {
	terms := map[string]struct{}{}
	for _, k := range keepIDs {
		if k != "" {
			terms[strings.ToLower(k)] = struct{}{}
		}
	}
	switch v := value.(type) {
	case string:
		winTerms := append([]string{}, keepIDs...)
		for i, m := range jsonKeyRe.FindAllStringSubmatch(v, -1) {
			if i >= 16 {
				break
			}
			winTerms = append(winTerms, m[1])
		}
		return extractTextWindow(v, winTerms, maxChars)
	case map[string]any:
		if len(terms) > 0 {
			if p := projectMapping(v, terms, maxChars); len(p) > 0 {
				return p
			}
		}
		return truncateValue(v, maxChars)
	case []any:
		var kept []any
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				kept = append(kept, truncateValue(item, maxChars))
				continue
			}
			if len(terms) > 0 {
				if p := projectMapping(m, terms, maxChars); len(p) > 0 {
					kept = append(kept, p)
				}
			}
		}
		if len(kept) > 0 {
			return kept
		}
		return truncateValue(v, maxChars)
	}
	return truncateValue(value, maxChars)
}
