package skills

import (
	"strings"
	"testing"
)

// listing is the real shape: a blank line after the header, one bullet per skill, and a
// description that carries its OWN bullet on a continuation line — which is what makes the
// entry span rule load-bearing rather than decorative.
const listing = `

- alpha: does alpha things.
- beta: does beta things.
  - and this line is part of beta's description, not a new entry
  more of beta
- plugin:gamma: a plugin skill.
- apps/web:deploy: a directory-scoped one.
`

func names(l Listing) []string {
	out := make([]string, 0, len(l.Entries))
	for _, e := range l.Entries {
		out = append(out, e.Name)
	}
	return out
}

func TestParseFindsEveryEntryAndItsSpan(t *testing.T) {
	l := Parse(listing)
	got := names(l)
	want := []string{"alpha", "beta", "plugin:gamma", "apps/web:deploy"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("entries = %v, want %v", got, want)
	}
	// beta owns its continuation lines. A span that stopped at the next "- " line would hand
	// them to plugin:gamma, which is how an inventory and a filter come to disagree about what
	// removing beta actually removes.
	beta := l.Text(l.Entries[1])
	for _, s := range []string{"does beta things", "part of beta's description", "more of beta"} {
		if !strings.Contains(beta, s) {
			t.Errorf("beta's text is missing %q: %q", s, beta)
		}
	}
	if strings.Contains(beta, "plugin:gamma") {
		t.Error("beta's span ran into the next entry")
	}
}

// Without is byte-exact when it drops nothing. The listing sits in the cached prefix, so an
// equivalent-but-reserialized body re-anchors the whole prompt for a saving of zero.
func TestWithoutIsByteExactWhenItDropsNothing(t *testing.T) {
	for _, drop := range []map[string]bool{nil, {}, {"nope": true}, {"alpha": false}} {
		got, n := Parse(listing).Without(drop)
		if got != listing || n != 0 {
			t.Errorf("drop=%v changed the listing (%d dropped)", drop, n)
		}
	}
}

func TestWithoutRemovesTheWholeEntry(t *testing.T) {
	got, n := Parse(listing).Without(map[string]bool{"beta": true})
	if n != 1 {
		t.Fatalf("dropped %d, want 1", n)
	}
	for _, gone := range []string{"does beta things", "part of beta's description", "more of beta"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q survived", gone)
		}
	}
	for _, keep := range []string{"- alpha:", "- plugin:gamma:", "- apps/web:deploy:"} {
		if !strings.Contains(got, keep) {
			t.Errorf("%q was removed too", keep)
		}
	}
}

func TestEntryNameRefusesWhatIsNotAnEntry(t *testing.T) {
	for _, line := range []string{
		"",
		"alpha: no bullet",
		"- no colon here",
		"- : empty name",
		"-  leading space then colon: x",
		"- has space: in the name", // a space is not in the charset
		"- <script>: markup",       // nor is markup
		"- " + strings.Repeat("x", 200) + ": too long",
	} {
		if n, ok := EntryName(line); ok {
			t.Errorf("EntryName(%q) = %q, true — want a refusal", line, n)
		}
	}
	// And the shapes that ARE names, including the two-colon forms.
	for line, want := range map[string]string{
		"- alpha: x":           "alpha",
		"- plugin:gamma: x":    "plugin:gamma",
		"- apps/web:deploy: x": "apps/web:deploy",
		"- dash-ed_name.v2: x": "dash-ed_name.v2",
	} {
		if n, ok := EntryName(line); !ok || n != want {
			t.Errorf("EntryName(%q) = %q,%v; want %q,true", line, n, ok, want)
		}
	}
}

func TestValidNameMatchesTheListingCharset(t *testing.T) {
	for _, ok := range []string{"a", "plugin:skill", "apps/web:deploy", "a.b_c-d2"} {
		if !ValidName(ok) {
			t.Errorf("ValidName(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "has space", "semi;colon", "star*", "quote\"", strings.Repeat("x", 129)} {
		if ValidName(bad) {
			t.Errorf("ValidName(%q) = true", bad)
		}
	}
}

// nameOnly is the shape that was silently dropped: a skill whose description is empty is listed
// as a bare name, with nothing after it on that line OR the next. Two forms, both from a real
// captured prompt — a plain name, and a `plugin:skill` name whose colon is the plugin separator
// and not a delimiter. 15 of 39 real entries looked like this.
const nameOnly = `

- alpha: does alpha things.
- security-review
- ui-ux-pro-max:brand
- ponytail:ponytail-audit
- superpowers:writing-skills
- beta: does beta things.
`

func TestParseFindsNameOnlyEntries(t *testing.T) {
	l := Parse(nameOnly)
	got := names(l)
	want := []string{"alpha", "security-review", "ui-ux-pro-max:brand",
		"ponytail:ponytail-audit", "superpowers:writing-skills", "beta"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("entries = %v, want %v", got, want)
	}
	// The point of the fix: an unrecognised line does not vanish, it is absorbed into the entry
	// above it — so alpha would answer for four names that are not its own, and cutting alpha
	// would cut all five lines.
	if alpha := l.Text(l.Entries[0]); strings.Contains(alpha, "security-review") {
		t.Errorf("alpha's span swallowed the following entries: %q", alpha)
	}
}

// The removal a page's one-click switch authorises has to be the removal it reports.
func TestWithoutRemovesExactlyOneNameOnlyEntry(t *testing.T) {
	got, n := Parse(nameOnly).Without(map[string]bool{"alpha": true})
	if n != 1 {
		t.Fatalf("dropped %d, want 1", n)
	}
	for _, keep := range []string{"- security-review", "- ui-ux-pro-max:brand",
		"- ponytail:ponytail-audit", "- superpowers:writing-skills", "- beta:"} {
		if !strings.Contains(got, keep) {
			t.Errorf("%q was removed too", keep)
		}
	}
}

func TestEntryNameReadsANameOnlyLine(t *testing.T) {
	for line, want := range map[string]string{
		"- security-review":         "security-review",
		"- ui-ux-pro-max:brand":     "ui-ux-pro-max:brand",
		"- ponytail:ponytail-audit": "ponytail:ponytail-audit",
		"- apps/web:deploy":         "apps/web:deploy",
		"- security-review  ":       "security-review", // trailing space
		"- security-review:":        "security-review", // empty description
		"- ui-ux-pro-max:brand: x":  "ui-ux-pro-max:brand",
		"- a:b:c":                   "a:b:c",
	} {
		if n, ok := EntryName(line); !ok || n != want {
			t.Errorf("EntryName(%q) = %q,%v; want %q,true", line, n, ok, want)
		}
	}
	// A prose bullet inside a description still is not an entry: the charset is the only gate
	// left on a line with no delimiter, so it has to keep refusing anything with a space.
	for _, line := range []string{"- and this line is part of a description", "- see also"} {
		if n, ok := EntryName(line); ok {
			t.Errorf("EntryName(%q) = %q, true — a prose bullet is not an entry", line, n)
		}
	}
}

// TestParseRefusesAProseBulletFollowedByItsOwnContinuation is the discriminator that no charset
// can supply. 47 of 245 real skill names are bare words — report, loop, gate — so `- report` (a
// skill) and `- Note` (prose) are the same string shape. What tells them apart is position: a
// name-only entry has an empty description by definition, so the next line is another entry,
// blank, or the region end; a one-word bullet inside somebody else's description is followed by
// that description's own indented continuation.
//
// The body is the realistic case: a description ending in a colon, then a URL on its own line.
// ValidName admits ':' and '/' because `plugin:skill` and `apps/web:deploy` need them, which is
// exactly what lets a URL through.
func TestParseRefusesAProseBulletFollowedByItsOwnContinuation(t *testing.T) {
	body := "\n- alpha: does alpha things. See the docs:\n" +
		"- https://example.com/alpha/guide\n" +
		"  and then keep reading.\n" +
		"- beta: does beta things.\n"
	if got, want := names(Parse(body)), "alpha,beta"; strings.Join(got, ",") != want {
		t.Errorf("entries = %v, want [%s] — the URL bullet is prose inside alpha", got, want)
	}
	// And the whole of alpha goes when alpha goes, leaving no orphaned prose in the prompt.
	out, n := Parse(body).Without(map[string]bool{"alpha": true})
	if n != 1 {
		t.Fatalf("dropped %d, want 1", n)
	}
	for _, gone := range []string{"https://example.com", "and then keep reading"} {
		if strings.Contains(out, gone) {
			t.Errorf("%q was orphaned in the listing: %q", gone, out)
		}
	}
}

// The lookahead must not cost a real name-only entry. Every legal successor: another entry, a
// blank line, and the end of the region.
func TestParseKeepsNameOnlyEntriesWhateverFollowsThem(t *testing.T) {
	for name, body := range map[string]string{
		"next is an entry": "\n- security-review\n- beta: x\n",
		"next is blank":    "\n- security-review\n\n- beta: x\n",
		"end of region":    "\n- beta: x\n- security-review\n",
		"last line no NL":  "\n- beta: x\n- security-review",
	} {
		got := names(Parse(body))
		found := false
		for _, n := range got {
			if n == "security-review" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: entries = %v, want security-review among them", name, got)
		}
	}
}
