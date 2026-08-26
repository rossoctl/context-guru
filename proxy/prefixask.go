package proxy

import (
	"context"
	"os"
	"sync"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
)

// PREFIX ASKS — putting a question to the request's own model with the previous turn's SENT body as
// the prefix, so the provider reads its prompt cache instead of being re-sent the transcript.
//
// Why the SENT body. The cache upstream was populated by what context-guru emitted, which is the
// compacted form. The incoming body is uncompacted, so it diverges from the cached bytes at the
// first thing any component removed and everything past that point is a fresh charge. Appending to
// the bytes actually sent is the only construction that reliably reads the cache — measured at
// 19,595 read / 0 written in docs/experiments/loca/iter019/results.md §2.
//
// Why it is worth the machinery. A component that must judge whether a tool output is still needed
// cannot answer that from the output alone: need is relevance MINUS whatever has already been
// captured elsewhere in the transcript, and that second term lives in the later turns. Sending those
// turns fresh costs ~10x a cache read; on the cheap model the required verbatim quoting also
// degraded to 20.8% at the batch sizes the bulk mechanism needs, against 0 of 59 on the request
// model. So the judgement wants the agent's own model AND the whole transcript, and only a cache
// read makes that affordable.
//
// OFF BY DEFAULT. It holds request bodies in memory, and a feature whose benefit is a cache hit must
// not be enabled by a host that cannot verify the hit is happening.

// prefixAskEnabled gates the whole mechanism. Opt-in, and read once per call so a run can be
// configured without a rebuild.
func prefixAskEnabled() bool {
	v := os.Getenv("CONTEXT_GURU_PREFIX_ASK")
	return v == "1" || v == "true" || v == "on"
}

// Bounds on the stash. These are deliberately small: the stash exists to serve the NEXT turn of an
// ACTIVE session, so retention beyond that is pure memory cost. A body larger than the per-body cap
// is not stashed at all rather than evicting others to hold it.
const (
	maxSentSessions = 64
	maxSentBody     = 1_500_000
	maxSentBytes    = 96_000_000
)

// sentStash holds the last body forwarded upstream per session.
type sentStash struct {
	mu    sync.Mutex
	m     map[string][]byte
	bytes int
}

func newSentStash() *sentStash { return &sentStash{m: map[string][]byte{}} }

// put records this session's forwarded body, replacing any previous one.
//
// Eviction is crude on purpose: when either bound is hit the whole stash is dropped. A prefix ask
// that finds nothing simply falls back to a plain completion, so the cost of over-eviction is a
// cache miss on one call — whereas an LRU here would be state to get wrong for no measurable gain.
func (s *sentStash) put(session string, body []byte) {
	if s == nil || session == "" || len(body) == 0 || len(body) > maxSentBody {
		return
	}
	cp := make([]byte, len(body))
	copy(cp, body)
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.m[session]; ok {
		s.bytes -= len(old)
	}
	if len(s.m) >= maxSentSessions || s.bytes+len(cp) > maxSentBytes {
		s.m = map[string][]byte{}
		s.bytes = 0
	}
	s.m[session] = cp
	s.bytes += len(cp)
}

func (s *sentStash) get(session string) []byte {
	if s == nil || session == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[session]
}

// prefixAsker is the components.PrefixAsker the pipeline receives. It is created per request and
// holds no lock; the stash owns its own.
type prefixAsker struct {
	stash   *sentStash
	session string
	cli     cheapmodel.Anthropic
}

// Ask appends the question to this session's last forwarded body and returns the model's text plus
// what it actually cost. A missing stash is an error rather than a silent empty answer, so the
// caller falls back to a plain completion instead of treating "no prefix" as "no verdicts".
func (p prefixAsker) Ask(ctx context.Context, ask string) (string, components.PrefixUsage, error) {
	body := p.stash.get(p.session)
	if len(body) == 0 {
		return "", components.PrefixUsage{}, errNoPrefix
	}
	reply, u, err := p.cli.CompletePrefixed(ctx, body, ask)
	return reply, components.PrefixUsage(u), err
}

// errNoPrefix is the first-turn case: nothing has been forwarded for this session yet.
var errNoPrefix = errNoPrefixType{}

type errNoPrefixType struct{}

func (errNoPrefixType) Error() string { return "no stashed prefix for this session" }

// prefixAskerFor builds the asker for one request, or nil when any precondition is missing. Nil is
// the normal case on a first turn and whenever the feature is off, and callers must degrade.
//
// Anthropic only: the appended-message construction and the tool_choice/tools cache-key facts were
// measured on that dialect, and guessing at another provider's cache semantics is how a claimed
// cache read becomes a silent 10x bill.
func (h *Handler) prefixAskerFor(provider bschemas.ModelProvider, models components.ModelSpec, session string) components.PrefixAsker {
	if !prefixAskEnabled() || provider != bschemas.Anthropic || session == "" {
		return nil
	}
	cli, ok := models.Incoming.(cheapmodel.Anthropic)
	if !ok {
		// No incoming client means ModelSpec.For would hand the component the STATIC cheap model,
		// which lives in a different cache namespace and could not read this prefix anyway.
		return nil
	}
	if h.sent.get(session) == nil {
		return nil
	}
	return prefixAsker{stash: h.sent, session: session, cli: cli}
}
