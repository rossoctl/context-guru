package dash

// The CIDR content gate, asserted over the WHOLE route table rather than per route.
//
// Two content surfaces have now reached production on the aggregates' side of this
// boundary — /api/prompt here, and before it the benchmark and capture routes for the
// tenant boundary. The per-route tests did not catch either, because each one tests the
// route its author was thinking about. This walks the table Mount itself uses, so a new
// route either applies the gate or fails here.

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// Markers are distinct per surface so a failure names WHICH content came back.
const (
	transcriptMarker = "SECRET-OF-tenant-a" // written by newScopeFixture as request content
	systemMarker     = "SYSTEM-PROMPT-MARKER-9f3a1c"
	declMarker       = "TOOL-SCHEMA-MARKER-9f3a1c"
)

// newContentFixture is the scope fixture in SINGLE-TENANT mode (auth == nil) with every
// kind of stored content present: a request transcript, a tool schema and a system prompt.
//
// A nil principal is single-tenant, the mode with the hole in it: scope() returns ok/Manager
// for any caller that can reach the port, so the address is the ONLY content gate there is.
func newContentFixture(t *testing.T, principal func(*http.Request) (Principal, bool)) *scopeFixture {
	t.Helper()
	f := newScopeFixture(t, principal)
	// tools > 0 is what puts a session inside PromptViewFor's scope subquery.
	if _, err := f.rec.DB().sql.Exec(`UPDATE requests SET tools = 1`); err != nil {
		t.Fatal(err)
	}
	inv := ScanInventory("anthropic", sysBody(t, systemMarker,
		[]string{tool("Bash", declMarker)}, skillsReminder))
	if inv == nil {
		t.Fatal("no inventory scanned")
	}
	f.rec.RecordInventory("tenant-a", "tenant-a:sess", time.Now().UnixMilli(), inv, true)
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, withText := textRows(t, f.rec.DB())
		if withText > 0 {
			return f
		}
		if time.Now().After(deadline) {
			t.Fatal("declaration text never landed; the fixture has nothing to gate")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// No mounted route hands content text to an address outside the trusted set.
//
// This is the assertion that was missing when /api/prompt shipped: TestAPIScopesEveryRouteToTheCaller
// walks the same table but asks a different question (whose tenant's rows), and nothing
// exercised a.prompt as an HTTP handler at all — the prompt-text tests all sit at
// PromptViewFor or at the writer, i.e. below the layer where this decision is made.
func TestNoRouteServesContentTextFromAnUntrustedAddress(t *testing.T) {
	f := newContentFixture(t, nil)
	markers := []string{transcriptMarker, systemMarker, declMarker}

	// First prove the fixture HOLDS all three, from loopback. Without this the test below
	// passes just as well against a fixture that recorded nothing, which is the failure mode
	// that makes a negative assertion worthless.
	for path, want := range map[string]string{
		"/api/requests/" + itoa(f.ids["tenant-a"]): transcriptMarker,
		"/api/prompt": systemMarker,
	} {
		if _, body := f.getFrom(t, path, "127.0.0.1:1234"); !strings.Contains(body, want) {
			t.Fatalf("fixture is empty: loopback %s did not return %s:\n%s", path, want, body)
		}
	}
	if _, body := f.getFrom(t, "/api/prompt", "127.0.0.1:1234"); !strings.Contains(body, declMarker) {
		t.Fatalf("fixture is empty: loopback /api/prompt returned no tool schema:\n%s", body)
	}

	for _, rt := range f.api.routes() {
		path := f.probe(rt.pattern)
		_, body := f.get(t, path) // an untrusted peer
		for _, m := range markers {
			if strings.Contains(body, m) {
				t.Errorf("%s served content text (%s) to an untrusted address:\n%s", path, m, body)
			}
		}
	}
}

// And the weights survive the gate, which is the reason it strips the text rather than
// refusing the route: a proxy bound to 0.0.0.0 is still an observability tool, and an
// operator who cannot see their own token weights has no page left.
func TestPromptRouteStillServesWeightsFromAnUntrustedAddress(t *testing.T) {
	f := newContentFixture(t, nil)
	code, body := f.get(t, "/api/prompt")
	if code != 200 {
		t.Fatalf("/api/prompt from an untrusted address = %d: %s", code, body)
	}
	for _, want := range []string{`"kind":"system_prompt"`, `"tokens":`, `"rows":`, `"text_rows":`} {
		if !strings.Contains(body, want) {
			t.Errorf("the gate took %s with the text:\n%s", want, body)
		}
	}
	if strings.Contains(body, `"has_text":true`) {
		t.Errorf("has_text is true with no text served:\n%s", body)
	}
}

// Hosted, the address is NOT the gate: ownership is, and scope() already applied it. A
// tenant reading their own prompt over the network must still get the text, or the fix
// above silently takes the feature away from every hosted account.
func TestPromptRouteServesTheOwningTenantOverTheNetwork(t *testing.T) {
	f := newContentFixture(t, asTenant("tenant-a", false))
	code, body := f.get(t, "/api/prompt") // untrusted peer, but the owner
	if code != 200 {
		t.Fatalf("/api/prompt for its own tenant = %d: %s", code, body)
	}
	if !strings.Contains(body, systemMarker) {
		t.Errorf("hosted: the owning tenant cannot read its own system prompt:\n%s", body)
	}
}

// The system prompt's PARTS are content too, and the gate has to take them whole.
//
// This is the same hole /api/prompt shipped with, one field narrower. The decomposition adds a
// list of section titles and a slice of text per section; a strip that only cleared Region.Text
// would leave the titles behind, and a heading in somebody's CLAUDE.md names their project, their
// employer or their incident. So the assertion is on a marker that lives in a HEADING rather than
// in a body, because a strip that misses the titles passes a body-only test.
func TestPromptPartsAreStrippedForAnUntrustedCaller(t *testing.T) {
	const headingMarker = "SECRET-PROJECT-HEADING-9f3a1c"
	f := newScopeFixture(t, nil)
	if _, err := f.rec.DB().sql.Exec(`UPDATE requests SET tools = 1`); err != nil {
		t.Fatal(err)
	}
	sys := "You are an agent.\n\n# " + headingMarker + "\n\nDeploy on Fridays.\n\n# Environment\n\ncwd\n"
	inv := ScanInventory("anthropic", sysBody(t, sys, []string{tool("Bash", declMarker)}, skillsReminder))
	if inv == nil {
		t.Fatal("no inventory scanned")
	}
	f.rec.RecordInventory("tenant-a", "tenant-a:sess", time.Now().UnixMilli(), inv, true)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, withText := textRows(t, f.rec.DB()); withText > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("declaration text never landed; the fixture has nothing to gate")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The fixture HOLDS the parts, from loopback. Without this the negative below passes just as
	// well against a report that never decomposed anything.
	_, trusted := f.getFrom(t, "/api/prompt", "127.0.0.1:1234")
	for _, want := range []string{`"parts"`, headingMarker, `"parts_tokens"`} {
		if !strings.Contains(trusted, want) {
			t.Fatalf("fixture is empty: loopback /api/prompt has no %s:\n%s", want, trusted)
		}
	}
	// And an untrusted peer gets neither the parts nor their titles.
	_, body := f.get(t, "/api/prompt")
	for _, leak := range []string{headingMarker, `"parts"`, `"parts_tokens"`} {
		if strings.Contains(body, leak) {
			t.Errorf("/api/prompt served %s to an untrusted address:\n%s", leak, body)
		}
	}
	// The weights still come through, which is why this strips rather than refuses.
	if !strings.Contains(body, `"kind":"system_prompt"`) {
		t.Errorf("the strip took the system prompt's weight with its text:\n%s", body)
	}
}
