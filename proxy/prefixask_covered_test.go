package proxy

import (
	"context"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/cheapmodel"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/dispatch/dispatchtest"
)

// AskCovered checks coverage and asks on ONE read of the stash, so the body that goes out is the
// body that was checked even when the thread's next request replaces the stash afterwards.
func TestAskCoveredAsksOnTheBodyItChecked(t *testing.T) {
	srv := newCapturePrefixed(t, 1)
	stash := newSentStash()
	const session = "covered-1"
	stash.put(session, bschemas.Anthropic, []byte(prefixBodyFixture), []byte(prefixBodyFixture))
	norm := apply.NormalizeMessages(bschemas.Anthropic, []byte(prefixBodyFixture))
	hash := components.SpanHash(norm[:1])
	a := prefixAsker{stash: stash, cli: newCoveredCli(srv)}

	if _, _, err := a.AskCovered(context.Background(), session, 1, hash, "THE-ASK"); err != nil {
		t.Fatalf("AskCovered over a covering stash: %v", err)
	}
	if got := string(srv.forwarded()); !strings.Contains(got, "THE-ASK") || !strings.Contains(got, "find the flaky test") {
		t.Fatalf("the ask did not go out over the checked body: %q", got)
	}
}

func TestAskCoveredDeclinesStaleAndMissingWithoutACall(t *testing.T) {
	srv := newCapturePrefixed(t, 1)
	stash := newSentStash()
	a := prefixAsker{stash: stash, cli: newCoveredCli(srv)}

	if _, _, err := a.AskCovered(context.Background(), "never-seen", 1, "h", "ask"); err != components.ErrNoPrefix {
		t.Errorf("missing stash: err = %v, want ErrNoPrefix", err)
	}
	const session = "covered-2"
	stash.put(session, bschemas.Anthropic, []byte(prefixBodyFixture), []byte(prefixBodyFixture))
	if _, _, err := a.AskCovered(context.Background(), session, 1, "not-the-span-hash", "ask"); err != components.ErrStalePrefix {
		t.Errorf("wrong span hash: err = %v, want ErrStalePrefix", err)
	}
	if _, _, err := a.AskCovered(context.Background(), session, 99, "h", "ask"); err != components.ErrStalePrefix {
		t.Errorf("span longer than the stash: err = %v, want ErrStalePrefix", err)
	}
	if n := len(srv.forwarded()); n != 0 {
		t.Errorf("a declined ask still sent %d bytes to the provider", n)
	}
}

// The stash replaced by a different body after a span hash was computed: the old two-step
// (CoversSpan, then Ask) could pair that hash with the new body; AskCovered refuses it.
func TestAskCoveredRefusesAHashFromAReplacedBody(t *testing.T) {
	srv := newCapturePrefixed(t, 1)
	stash := newSentStash()
	const session = "covered-3"
	stash.put(session, bschemas.Anthropic, []byte(prefixBodyFixture), []byte(prefixBodyFixture))
	oldHash := components.SpanHash(apply.NormalizeMessages(bschemas.Anthropic, []byte(prefixBodyFixture))[:1])
	other := strings.Replace(prefixBodyFixture, "find the flaky test", "write the release notes", 1)
	stash.put(session, bschemas.Anthropic, []byte(other), []byte(other))
	a := prefixAsker{stash: stash, cli: newCoveredCli(srv)}

	if _, _, err := a.AskCovered(context.Background(), session, 1, oldHash, "ask"); err != components.ErrStalePrefix {
		t.Fatalf("err = %v, want ErrStalePrefix for a hash computed over the replaced body", err)
	}
	if len(srv.forwarded()) != 0 {
		t.Error("the ask went out over the replacement body")
	}
}

// The body snapshotCovering returns is the entry it read and checked, not a second read of the
// stash: replace the stash at the point right after the read and the original still comes back.
func TestSnapshotCoveringReturnsTheEntryItChecked(t *testing.T) {
	stash := newSentStash()
	const session = "covered-4"
	stash.put(session, bschemas.Anthropic, []byte(prefixBodyFixture), []byte(prefixBodyFixture))
	hash := components.SpanHash(apply.NormalizeMessages(bschemas.Anthropic, []byte(prefixBodyFixture))[:1])
	other := strings.Replace(prefixBodyFixture, "find the flaky test", "write the release notes", 1)
	stash.afterEntryRead = func() { stash.put(session, bschemas.Anthropic, []byte(other), []byte(other)) }

	body, err := stash.snapshotCovering(session, 1, hash)
	if err != nil {
		t.Fatalf("snapshotCovering: %v", err)
	}
	if !strings.Contains(string(body), "find the flaky test") || strings.Contains(string(body), "release notes") {
		t.Fatalf("the snapshot is not the entry that was checked: %q", body)
	}
}

func newCoveredCli(srv *capturePrefixed) cheapmodel.Anthropic {
	d, ref := dispatchtest.HTTP(srv.srv.URL, dispatchtest.AnthropicAuth("k", ""))
	return cheapmodel.Anthropic{D: d, Ref: ref, Model: "m"}
}

// AskRead returns the text of the body the model read, from the same snapshot it asked over. A later put
// (the thread's next request) must not change what a running ask reports, and a stale span is declined.
func TestAskReadReportsTheTextOfTheBodyItAskedOver(t *testing.T) {
	srv := newCapturePrefixed(t, 1)
	stash := newSentStash()
	const session = "read-1"
	stash.put(session, bschemas.Anthropic, []byte(prefixBodyFixture), []byte(prefixBodyFixture))
	norm := apply.NormalizeMessages(bschemas.Anthropic, []byte(prefixBodyFixture))
	hash := components.SpanHash(norm[:1])
	a := prefixAsker{stash: stash, cli: newCoveredCli(srv)}

	_, _, read, err := a.AskRead(context.Background(), session, 1, hash, "THE-ASK")
	if err != nil {
		t.Fatalf("AskRead over a covering stash: %v", err)
	}
	if !strings.Contains(read, "find the flaky test") || strings.Contains(read, "THE-ASK") {
		t.Fatalf("read text = %q, want the stashed messages and not the ask", read)
	}
	// count < 0 skips the coverage check.
	if _, _, _, err := a.AskRead(context.Background(), session, -1, "", "THE-ASK"); err != nil {
		t.Fatalf("AskRead with no coverage check: %v", err)
	}
	if _, _, _, err := a.AskRead(context.Background(), session, 1, "not-the-span-hash", "ask"); err != components.ErrStalePrefix {
		t.Fatalf("stale span: err = %v, want ErrStalePrefix", err)
	}
	if _, _, _, err := a.AskRead(context.Background(), "never-seen", 1, "h", "ask"); err != components.ErrNoPrefix {
		t.Fatalf("missing stash: err = %v, want ErrNoPrefix", err)
	}
}
