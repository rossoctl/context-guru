package apply

import (
	"strings"
	"testing"
)

// TestSplitTrailingReminders pins which text counts as a trailing reminder (#556): only a run
// of complete <system-reminder> blocks at the very end, and never the whole text.
func TestSplitTrailingReminders(t *testing.T) {
	const r1 = "<system-reminder>\n<total_tokens>14989288 tokens left</total_tokens>\n</system-reminder>"
	const r2 = "<system-reminder>\nThe file changed.\n</system-reminder>"
	const hdr = "HEADER"
	big := strings.Repeat("x", 50<<10)
	atCap := reminderOpen + strings.Repeat("y", maxReminderBytes-len(reminderOpen)-len(reminderClose)) + reminderClose
	cases := []struct {
		name, in, body, tail string
	}{
		{"no reminder", "line 1\nline 2\n", "line 1\nline 2\n", ""},
		{"one trailing", "[1,2]\n\n" + r1, "[1,2]", "\n\n" + r1},
		{"two trailing", "out\n" + r2 + "\n" + r1, "out", "\n" + r2 + "\n" + r1},
		{"whitespace after", "out\n\n" + r1 + "\n", "out", "\n\n" + r1 + "\n"},
		{"in the middle is content", "a\n" + r1 + "\nb", "a\n" + r1 + "\nb", ""},
		{"reminder only is left alone", r1, r1, ""},
		{"reminder and whitespace only", "\n\n" + r1, "\n\n" + r1, ""},
		{"unclosed tag", "out\n<system-reminder>\nno end", "out\n<system-reminder>\nno end", ""},
		{"stray end tag", "out\n</system-reminder>", "out\n</system-reminder>", ""},
		{"nested open in body", "a <system-reminder> b\n\n" + r1, "a <system-reminder> b", "\n\n" + r1},
		// A stray close tag at the end, after output that quotes an open tag: the "block" holds the
		// output, so nothing is split.
		{"stray end tag after a quoted block", hdr + "\n" + r2 + big + "\n</system-reminder>",
			hdr + "\n" + r2 + big + "\n</system-reminder>", ""},
		{"stray end tag after a short quoted block", "out\n" + r2 + "\nmore output\n</system-reminder>",
			"out\n" + r2 + "\nmore output\n</system-reminder>", ""},
		{"block over the cap", "out\n\n<system-reminder>" + big + "</system-reminder>",
			"out\n\n<system-reminder>" + big + "</system-reminder>", ""},
		{"block at the cap", "out\n\n" + atCap, "out", "\n\n" + atCap},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body, tail := splitTrailingReminders(c.in)
			if body != c.body || tail != c.tail {
				t.Fatalf("splitTrailingReminders(%q)\n got body=%q tail=%q\nwant body=%q tail=%q", c.in, body, tail, c.body, c.tail)
			}
			if body+tail != c.in {
				t.Fatalf("body+tail must rebuild the input exactly: %q", body+tail)
			}
		})
	}
}
