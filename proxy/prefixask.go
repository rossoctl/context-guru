package proxy

import (
	"context"
	"sync"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
)

// PREFIX ASKS — putting a question to the request's own model with the previous turn's SENT body as
// the prefix, so the provider reads its prompt cache instead of being re-sent the transcript.
//
// WHY THE SENT BODY. The cache upstream was populated by what context-guru emitted, which is the
// compacted form. The incoming body is uncompacted, so it diverges from the cached bytes at the
// first thing any component removed and everything past that point is a fresh charge. Appending to
// the bytes actually sent is the only construction that reliably reads the cache — measured at
// 19,595 read / 0 written in docs/experiments/loca/iter019/results.md §2.
//
// WHY IT IS WORTH THE MACHINERY. A component that must judge whether a tool output is still needed
// cannot answer that from the output alone: need is relevance MINUS whatever has already been
// captured elsewhere in the transcript, and that second term lives in the later turns. Sending those
// turns fresh costs ~10x a cache read; on the cheap model the required verbatim quoting also
// degraded to 20.8% at the batch sizes a bulk mechanism needs, against 0 of 59 on the request
// model. So the judgement wants the agent's own model AND the whole transcript, and only a cache
// read makes that affordable.
//
// ON BY DEFAULT, WHICH IS A DELIBERATE INVERSION of how this shipped on the branch it comes from.
// There it was gated off behind CONTEXT_GURU_PREFIX_ASK, on the reasoning that "a feature whose
// benefit is a cache hit should not be on by default in a host that cannot verify the hit". We CAN
// verify it: PrefixUsage is returned rather than merely recorded, precisely so the caller can gate on
// it, and extract_llm_sweep declines when the read did not happen instead of falling back. A host
// that can see the hit has no reason to make the operator opt in to it.
//
// The other reason it was off — that it holds request bodies in memory — is a bound to state, not a
// reason to disable. See the caps below and what happens when they are hit.

// Bounds on the stash. These are deliberately small: the stash exists to serve the NEXT turn of an
// ACTIVE session, so retention beyond that is pure memory cost.
//
// WHAT HAPPENS WHEN A BOUND IS HIT, since "bounded" on its own says nothing about the failure:
//
//   - a body larger than maxSentBody is NOT stashed at all, rather than evicting others to hold it.
//     The next turn's ask then finds nothing, reports errNoPrefix, and the sweep declines. One
//     forgone opportunity, no wrong answer.
//   - past maxSentSessions or maxSentBytes the WHOLE stash is dropped. Crude on purpose: a missing
//     prefix costs one declined sweep, while an LRU here would be state to get wrong for no
//     measurable gain. The worst case is a busy proxy that never accumulates a usable prefix, which
//     is visible as sweep_no_prefix rather than as a silent cost.
//
// 64 sessions x up to 1.5 MB is ~96 MB, which is the outer bound and why maxSentBytes states it
// explicitly rather than leaving it to be multiplied out.
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

// prefixAsker is the components.PrefixAsker the pipeline receives. Created per request and holds no
// lock; the stash owns its own.
type prefixAsker struct {
	stash *sentStash
	cli   cheapmodel.Anthropic
}

// Ask appends the question to this session's last forwarded body and returns the model's text plus
// what it actually cost. A missing stash is an ERROR rather than a silent empty answer, so the caller
// can tell "there was no prefix to read" from "the model declined to act" — the distinction the
// counters exist to preserve.
func (p prefixAsker) Ask(ctx context.Context, session, ask string) (string, components.PrefixUsage, error) {
	body := p.stash.get(session)
	if len(body) == 0 {
		return "", components.PrefixUsage{}, ErrNoPrefix
	}
	reply, u, err := p.cli.CompletePrefixed(ctx, body, ask)
	return reply, components.PrefixUsage(u), err
}

// ErrNoPrefix is the first-turn case: nothing has been forwarded for this session yet. Exported so a
// component can tell it apart from a transport failure without string matching.
var ErrNoPrefix = errNoPrefixType{}

type errNoPrefixType struct{}

func (errNoPrefixType) Error() string { return "no stashed prefix for this session" }

// prefixAskerFor builds the asker for one request, or nil when a precondition is missing.
//
// ANTHROPIC ONLY: the appended-message construction and the tool_choice/tools cache-key facts were
// measured on that dialect, and guessing at another provider's cache semantics is how a claimed
// cache read becomes a silent 10x bill.
//
// NO PRE-FLIGHT STASH CHECK. The first turn of a session has nothing stashed, and that case must
// surface as an error from Ask — counted, and declined — rather than as a nil asker. A nil asker is
// indistinguishable from "the route cannot support this at all", which is exactly how a session-key
// mismatch would stay invisible.
func (h *Handler) prefixAskerFor(provider bschemas.ModelProvider, models components.ModelSpec) components.PrefixAsker {
	if provider != bschemas.Anthropic {
		return nil
	}
	cli, ok := models.Incoming.(cheapmodel.Anthropic)
	if !ok {
		// No incoming client means the component would get the STATIC cheap model, which lives in a
		// different cache namespace and could not read this prefix anyway.
		return nil
	}
	return prefixAsker{stash: h.sent, cli: cli}
}
