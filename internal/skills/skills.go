// Package skills parses Claude Code's skills listing — the prose block that declares which
// skills a request carries.
//
// It exists as a shared package for one reason: two places need the SAME parse and they must
// not drift. The dashboard reads the listing to price each entry (dash/toolinventory.go); the
// declaration filter cuts entries out of it (apply/toolfilter.go). A filter that disagreed with
// the inventory about where an entry ENDS would leave an orphaned description line behind in a
// real request — prose with no heading, attached to the wrong skill — while the page that
// authorised the removal reported a clean cut. This repo has shipped that class of bug before
// (two cost columns priced from different maps, 31.6% apart), so the rule lives once.
//
// The listing is not JSON and cannot be treated as though it were. It is prose inside a
// `<system-reminder>` in a role:"system" MESSAGE — measured on real traffic: messages[1], a
// plain string, 6,867 bytes — so everything here is line-oriented and every function fails
// closed: a line that does not clearly parse as an entry is not one.
package skills

import "strings"

// Header anchors the listing, and the anchor is load-bearing rather than cosmetic: the
// agent-types listing sitting immediately above it in the SAME message uses the identical
// `- name: description` shape, so an unanchored scrape counts subagents as skills.
const Header = "The following skills are available for use with the Skill tool:"

// RemovePrefix marks a declaration-removal entry as a SKILL rather than a tool.
//
// A skill needs its own namespace in that list because the two are removed from different places
// and a bare name cannot say which is meant: a tool is an element of the top-level `tools` array,
// a skill is a line of prose inside a transcript message. They can also collide — nothing stops a
// skill being called `Monitor` — and the wrong mechanism on a name is a silent no-op, so the
// prefix is required rather than inferred. It mirrors `mcp__<server>`, which the same list
// already uses for "a whole MCP server, not one of its tools".
//
// It lives HERE, and not beside the rewrite that acts on it, because the component that validates
// the list (components/reformat) and the code that applies it (apply) cannot import each other —
// apply imports the components. A shared constant beats two spellings of the same string.
const RemovePrefix = "skill__"

// ReminderEnd bounds the listing. Both this and Header appear literally in the JSON-escaped
// body (neither contains an escapable character), which is what lets a caller find the region
// with a byte search and no unescaping.
const ReminderEnd = "</system-reminder>"

// Entry is one skill in the listing, as a LINE RANGE rather than a byte span.
//
// Lines, because the two callers want different bytes from the same range and a span would have
// forced one of them to adjust it: the inventory joins the range with "\n" and stores that as
// the entry's text (so the entry's measured weight excludes its trailing newline), while the
// filter drops the lines and re-joins what is left. Both are exact on the same Listing.Lines.
type Entry struct {
	Name string
	// From / To are the half-open line range this entry occupies in Listing.Lines.
	From, To int
}

// Listing is a parsed listing body: its lines, and one Entry per skill.
type Listing struct {
	Lines   []string
	Entries []Entry
}

// Parse splits a listing BODY — the text after Header and before ReminderEnd — into entries.
//
// An entry runs from its own `- name:` line to the next line that parses as a name, not to the
// next blank line and not to the next line starting with "- ": a description containing its own
// "\n- " bullet would otherwise truncate the entry, and several real skill descriptions do
// exactly that.
func Parse(body string) Listing {
	l := Listing{Lines: strings.Split(body, "\n")}
	for n, ln := range l.Lines {
		name, ok := EntryName(ln)
		if !ok {
			continue
		}
		if k := len(l.Entries) - 1; k >= 0 {
			l.Entries[k].To = n
		}
		l.Entries = append(l.Entries, Entry{Name: name, From: n, To: len(l.Lines)})
	}
	return l
}

// Text is one entry's own text, joined the way the inventory stores and measures it.
func (l Listing) Text(e Entry) string { return strings.Join(l.Lines[e.From:e.To], "\n") }

// Without returns the listing body with the named entries' lines removed, and how many entries
// went. The result is byte-exact when nothing is dropped, because Parse split on "\n" and this
// re-joins on "\n" — which is what lets a caller treat "removed nothing" as "changed nothing"
// rather than having to compare the strings.
func (l Listing) Without(drop map[string]bool) (string, int) {
	cut := make([]bool, len(l.Lines))
	n := 0
	for _, e := range l.Entries {
		if !drop[e.Name] {
			continue
		}
		for i := e.From; i < e.To && i < len(cut); i++ {
			cut[i] = true
		}
		n++
	}
	// No early return for n == 0: Parse split on "\n" and this joins on "\n", so the
	// nothing-dropped path already reproduces the input byte for byte. A guard here would be a
	// second code path with nothing to do, and mutation testing found it — the fast path could be
	// deleted outright without any test noticing, which is the definition of code that is not
	// carrying its weight.
	kept := make([]string, 0, len(l.Lines))
	for i, ln := range l.Lines {
		if !cut[i] {
			kept = append(kept, ln)
		}
	}
	return strings.Join(kept, "\n"), n
}

// EntryName reads a listing line's skill name, or reports that the line is not an entry.
func EntryName(line string) (string, bool) {
	if !strings.HasPrefix(line, "- ") {
		return "", false
	}
	rest := line[2:]
	i := strings.Index(rest, ":")
	if i <= 0 {
		return "", false
	}
	name := rest[:i]
	// A plugin skill is `plugin:skill` and a directory-scoped one `apps/web:deploy`, so one
	// colon may be part of the name: take the longer candidate when the first colon is not
	// followed by a space.
	if !strings.HasPrefix(rest[i:], ": ") {
		j := strings.Index(rest[i+1:], ": ")
		if j < 0 {
			return "", false
		}
		name = rest[:i+1+j]
	}
	if name == "" || len(name) > 128 {
		return "", false
	}
	if !ValidName(name) {
		return "", false
	}
	return name, true
}

// ValidName reports whether s is drawn from the charset a skill name uses. Anything else is not
// a name this package recognises, and is skipped rather than guessed at — the listing is the
// authority for what may be removed from a real request.
func ValidName(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == ':' || r == '-' || r == '/':
		default:
			return false
		}
	}
	return true
}
