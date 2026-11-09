package dash

import (
	"strings"
	"testing"
)

// jsStringLiterals returns every JavaScript string literal in src, of any quote style, with the
// quotes kept.
//
// WHY A SCANNER AND NOT A REGEX. These UI checks used to match single-quoted strings alone, which
// breaks the moment a string of ANOTHER kind contains an apostrophe: insights.js has
// "...does not reach a plugin\'s skills..." inside double quotes, and that one apostrophe flipped
// the pairing for every literal after it. Measured: not one literal in the rest of the file was
// extracted correctly, so every assertion about rendered text below that line was checking
// garbage — and one of them failed on a phrase that was plainly on the page.
//
// Walking the source once, left to right, and consuming whichever string starts first means a
// quote inside one kind of string is part of it and cannot open another. Template literals are
// returned whole, interpolations included, which is enough for a substring check.
func jsStringLiterals(src string) []string {
	var out []string
	for i := 0; i < len(src); i++ {
		q := src[i]
		if q != '\'' && q != '"' && q != '`' {
			continue
		}
		j := i + 1
		for j < len(src) && src[j] != q {
			if src[j] == '\\' {
				j++ // skip the escaped character, whatever it is
			} else if src[j] == '\n' && q != '`' {
				break // an unterminated '' or "" string ends at the line, as in JS
			}
			j++
		}
		if j >= len(src) || src[j] != q {
			continue // not a real literal; resume scanning after the stray quote
		}
		out = append(out, src[i:j+1])
		i = j
	}
	return out
}

// jsLiteralText is every string literal in src, joined and lowercased, for asserting that some
// phrase reaches the reader as rendered text.
func jsLiteralText(src string) string {
	return strings.ToLower(strings.Join(jsStringLiterals(src), " "))
}

// The scanner's own contract. Every rendered-text assertion in the UI tests stands on it, so it is
// pinned against the exact input that broke its regex predecessor.
func TestJSStringLiteralsSurvivesAnApostropheInsideDoubleQuotes(t *testing.T) {
	src := `lines.push("a plugin's skills");` + "\n" +
		`el('p', { text: 'open the History tab' });` + "\n" +
		"const x = `tpl ${a} 'quoted'`;" + "\n" +
		`el('p', { text: 'it\'s escaped' });`
	got := jsStringLiterals(src)
	// 'p' twice: the tag name passed to el() is a literal too.
	want := []string{
		`"a plugin's skills"`,
		`'p'`,
		`'open the History tab'`,
		"`tpl ${a} 'quoted'`",
		`'p'`,
		`'it\'s escaped'`,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d literals %q; want %d %q", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("literal %d = %q; want %q", i, got[i], want[i])
		}
	}
}
