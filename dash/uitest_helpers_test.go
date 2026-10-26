package dash

import "strings"

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
