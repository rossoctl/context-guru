package proxy

import (
	"context"
	"sync"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/apply"
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

// sentEntry is one session's stashed body, plus what it takes to answer
// components.PrefixCoverage.CoversSpan for it later without re-parsing on every put: the
// provider the body was forwarded under (normalizeMessages needs it to parse the right wire
// shape) is kept so a later CoversSpan call can re-derive the same message boundaries a
// components/offload caller computed over its own, freshly-normalized request. The body is
// re-normalized lazily, on the (rare, off the hot forwarding path) call to CoversSpan, rather
// than eagerly on every put — put runs on every single forwarded request, CoversSpan only when
// a component is actually about to commission a summary through this stash.
type sentEntry struct {
	body     []byte
	provider bschemas.ModelProvider
}

// sentStash holds the last body forwarded upstream per session.
type sentStash struct {
	mu    sync.Mutex
	m     map[string]sentEntry
	bytes int
}

func newSentStash() *sentStash { return &sentStash{m: map[string]sentEntry{}} }

// put records this session's forwarded body, replacing any previous one.
//
// A body over maxSentBody is NOT stashed, and this now DELETES whatever was stashed for the
// session before — see the package comment's "what happens when a bound is hit". Keeping the
// OLD body used to be the behavior, on the theory that a forgone opportunity now beats one
// later; it is not, for PrefixAsker: Ask would still find something and answer from a
// transcript that is missing everything since, with no signal to the caller that happened,
// unless the caller checks CoversSpan first. A stale body is worse than none — none makes Ask
// return ErrNoPrefix, which every caller already handles; a stale one makes Ask succeed with an
// answer about the wrong conversation. CoversSpan is the primary fix (a caller that checks it
// never gets fooled by a stale body either way), but deleting here means a caller that does NOT
// check also gets the safer failure mode.
func (s *sentStash) put(session string, provider bschemas.ModelProvider, body []byte) {
	if s == nil || session == "" || len(body) == 0 {
		return
	}
	if len(body) > maxSentBody {
		s.mu.Lock()
		if old, ok := s.m[session]; ok {
			s.bytes -= len(old.body)
			delete(s.m, session)
		}
		s.mu.Unlock()
		return
	}
	cp := make([]byte, len(body))
	copy(cp, body)
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.m[session]; ok {
		s.bytes -= len(old.body)
	}
	if len(s.m) >= maxSentSessions || s.bytes+len(cp) > maxSentBytes {
		s.m = map[string]sentEntry{}
		s.bytes = 0
	}
	s.m[session] = sentEntry{body: cp, provider: provider}
	s.bytes += len(cp)
}

func (s *sentStash) get(session string) []byte {
	if s == nil || session == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[session].body
}

// CoversSpan implements components.PrefixCoverage: it reports whether the stashed body for
// session, re-normalized the same way the forwarding path itself normalizes a wire body
// (apply.NormalizeMessages), has AT LEAST `count` messages and those first `count` messages
// hash (components.SpanHash) to exactly `hash`.
//
// A MISSING session reports true — covered — rather than false. That looks backwards until
// you read what Ask already does with it: with nothing stashed, Ask returns components.
// ErrNoPrefix, which every existing caller already treats as the ordinary, once-per-session
// "nothing to read yet" case (cacheAwareNoPrefix), not a failure. This method exists to catch a
// DIFFERENT, newer failure: a stash that HOLDS something, but not enough of it, or not the right
// bytes — put's own oversized-body path now deletes a session's entry rather than leaving a
// stale one behind (see put's comment), specifically so that case also degrades to "missing",
// not "wrong". So false here is reserved for the one case that is actually dangerous: a PRESENT
// stash whose coverage falls short — see cache_aware_summarizer's own stale-prefix guard, which
// is the only caller.
func (s *sentStash) CoversSpan(session string, count int, hash string) bool {
	if s == nil || session == "" || count < 0 {
		return true
	}
	s.mu.Lock()
	e, ok := s.m[session]
	s.mu.Unlock()
	if !ok {
		return true
	}
	norm := apply.NormalizeMessages(e.provider, e.body)
	if count > len(norm) {
		return false
	}
	return components.SpanHash(norm[:count]) == hash
}

// prefixAsker is the components.PrefixAsker the pipeline receives. Created per request and holds no
// lock; the stash owns its own.
type prefixAsker struct {
	stash *sentStash
	cli   cheapmodel.Anthropic
}

type responsesPrefixAsker struct {
	stash *sentStash
	cli   cheapmodel.OpenAI
}

func (p responsesPrefixAsker) Ask(ctx context.Context, session, ask string) (string, components.PrefixUsage, error) {
	body := p.stash.get(session)
	if len(body) == 0 {
		return "", components.PrefixUsage{}, components.ErrNoPrefix
	}
	reply, u, err := p.cli.CompletePrefixedResponses(ctx, body, ask)
	return reply, components.PrefixUsage(u), err
}

// CoversSpan implements components.PrefixCoverage by delegating to the stash this asker reads
// through Ask — the asker value itself is what a caller type-asserts
// (c.PrefixAsk.(components.PrefixCoverage)), so the method has to live here, not only on
// *sentStash, or the assertion always fails and the stale-prefix guard never actually runs.
func (p responsesPrefixAsker) CoversSpan(session string, count int, hash string) bool {
	return p.stash.CoversSpan(session, count, hash)
}

// Ask appends the question to this session's last forwarded body and returns the model's text plus
// what it actually cost. A missing stash is an ERROR rather than a silent empty answer, so the caller
// can tell "there was no prefix to read" from "the model declined to act" — the distinction the
// counters exist to preserve.
func (p prefixAsker) Ask(ctx context.Context, session, ask string) (string, components.PrefixUsage, error) {
	body := p.stash.get(session)
	if len(body) == 0 {
		return "", components.PrefixUsage{}, components.ErrNoPrefix
	}
	reply, u, err := p.cli.CompletePrefixed(ctx, body, ask)
	return reply, components.PrefixUsage(u), err
}

// CoversSpan implements components.PrefixCoverage by delegating to the stash this asker reads
// through Ask — see responsesPrefixAsker.CoversSpan's own comment for why this cannot live only
// on *sentStash.
func (p prefixAsker) CoversSpan(session string, count int, hash string) bool {
	return p.stash.CoversSpan(session, count, hash)
}

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
	// THE REPLY BUDGET, set here because this is where the prefix-ask client is built and the default
	// is sized for something else entirely. Without it the reply is capped at DefaultMaxTokens and a
	// verdict array over a long candidate list is cut off mid-flight — which parses as nothing and is
	// indistinguishable from a model that declined to act. See PrefixAskMaxTokens.
	cli.MaxTokens = cheapmodel.PrefixAskMaxTokens
	return prefixAsker{stash: h.sent, cli: cli}
}

// Responses differs only in the request/response wire shape: the sweep's
// selection, read-verification and fallback decisions remain in the component.
func (h *Handler) prefixAskerForAPI(provider bschemas.ModelProvider, api string, models components.ModelSpec) components.PrefixAsker {
	if api != "responses" {
		return h.prefixAskerFor(provider, models)
	}
	cli, ok := models.Incoming.(cheapmodel.OpenAI)
	if !ok {
		return nil
	}
	cli.MaxTokens = cheapmodel.PrefixAskMaxTokens
	return responsesPrefixAsker{stash: h.sent, cli: cli}
}
