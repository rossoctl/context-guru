package proxy

import (
	"context"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
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
	a := prefixAsker{stash: stash, cli: cheapmodel.Anthropic{BaseURL: srv.srv.URL, Model: "m", APIKey: "k"}}

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
	a := prefixAsker{stash: stash, cli: cheapmodel.Anthropic{BaseURL: srv.srv.URL, Model: "m", APIKey: "k"}}

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
	a := prefixAsker{stash: stash, cli: cheapmodel.Anthropic{BaseURL: srv.srv.URL, Model: "m", APIKey: "k"}}

	if _, _, err := a.AskCovered(context.Background(), session, 1, oldHash, "ask"); err != components.ErrStalePrefix {
		t.Fatalf("err = %v, want ErrStalePrefix for a hash computed over the replaced body", err)
	}
	if len(srv.forwarded()) != 0 {
		t.Error("the ask went out over the replacement body")
	}
}
