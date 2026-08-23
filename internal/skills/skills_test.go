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
