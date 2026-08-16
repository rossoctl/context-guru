// Package session resolves the conversation key that state is scoped to.
//
// Per the design (D4): each host supplies an explicit id when it has one
// (bifrost proxy: x-context-guru-session header, else the agent's own id off the
// request body — Anthropic metadata.user_id or Bob Shell's metadata.taskId;
// AuthBridge: pctx.Session A2A id; eval-containers: gateway-stamped). When none
// is available we fall back to a deterministic content hash of the system
// prompt + first user message, which needs no host cooperation.
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// Resolve returns the session key. If explicit is non-empty it wins; otherwise
// a stable hash of (system, firstUser) is used, so two turns of the same
// conversation land on the same key.
//
// That property depends entirely on the CALLER passing conversation-HEAD strings.
// Pass the concatenation of every system-role message and it breaks: the Claude
// Agent SDK appends a new system-role message per turn, which made every request
// derive its own key. Callers get the head via schema.SessionHead.
func Resolve(explicit, system, firstUser string) string {
	return Scoped("", explicit, system, firstUser)
}

// MaxExplicitLen caps a client-supplied session id. The derived-hash path produces
// 16 characters, so this is generous by an order of magnitude.
const MaxExplicitLen = 128

// safeExplicit reports whether a client-supplied id may be used as a session key.
//
// The key is not only an in-memory map key: it becomes a store key, a `session_id`
// column in DELETE statements, and — hosted — a path component of a Box object
// (`archive/<tenant>/<yyyy>/<mm>/<session>.<kind>.jsonl.gz`). An id of
// `../../../../backup/x` therefore escapes the tenant's folder and can name the
// operator's control-database snapshots. So the charset is an allow-list: no
// separators of any kind, no unicode, nothing a path or a shell would interpret.
func safeExplicit(s string) bool {
	if s == "" || len(s) > MaxExplicitLen {
		return false
	}
	// "." and ".." are inside the charset below and are precisely the two names a path
	// DOES interpret. No traversal follows from them today — every consumer appends a
	// suffix, so ".." becomes a file called "..<kind>.jsonl.gz" rather than a parent
	// directory — but that is a property of today's callers, not of the id, and the
	// point of this guard is to not depend on that. Rejecting an all-dots id costs one
	// comparison and removes the whole question.
	if strings.Trim(s, ".") == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == ':' || c == '-':
		default:
			return false
		}
	}
	return true
}

// maxMetadataLen bounds the metadata.user_id value we are willing to LOOK at. The
// real thing is ~170 bytes (see ExplicitID); the cap exists so a client sending a
// 4 MB user_id costs one length comparison rather than a JSON parse.
const maxMetadataLen = 1024

// ExplicitID returns the first usable id from the candidates a host can offer, in
// the caller's precedence order — the proxy passes the x-context-guru-session header
// first, then Anthropic's `metadata.user_id` (Claude Code), then `metadata.taskId`
// (Bob Shell). It returns "" when none is usable, which makes Scoped fall back to
// the derived content hash — i.e. today's behaviour.
//
// A host-supplied id matters because of COMPACTION. The derived hash covers the
// system prompt plus the first user message, and when Claude Code auto-compacts it
// replaces the transcript with a synthetic first user message ("This session is
// being continued from a previous conversation…"). The hash therefore flips
// mid-conversation, splitting one conversation into two sessions: the dashboard's
// before/after view breaks, the extract cache, the frozen offload set and the
// summarize checkpoint all reset, /expand 404s for pre-compaction ids, and — the
// expensive one — the cached-prefix boundary (modes.Tracker.Turn) resets to 0, so
// the offloaders are let loose over a prefix the provider has already cached and the
// next turn pays a full cache write. Bob Shell is worse: its `system` head changes
// at compaction too (it regenerates environment boilerplate including a recursive
// listing of the working directory), so its derived key flips for two independent
// reasons. Both agents' OWN session ids are stable across a compaction, so reading
// one off the request fixes all of that in one place.
//
// Claude Code does not send a bare id. Real captured traffic carries a JSON object
// STRING:
//
//	"metadata":{"user_id":"{\"device_id\":\"<64 hex>\",\"account_uuid\":\"\",\"session_id\":\"<uuid>\"}"}
//
// which safeExplicit rejects wholesale (braces, quotes, commas, ~170 bytes). So the
// object's session_id — the half that survives compaction — is pulled out. Anything
// that is not that object is passed through unchanged, for safeExplicit to judge, so
// a host sending a plain id (Bob's `metadata.taskId` is a bare randomUUID) works with
// no special case.
func ExplicitID(cands ...string) string {
	for _, cand := range cands {
		if s := strings.TrimSpace(metaSessionID(cand)); safeExplicit(s) {
			return s
		}
	}
	return ""
}

// metaSessionID unwraps Claude Code's JSON-object user_id, or returns v unchanged.
// Every malformed shape (truncated JSON, an object without session_id, a nested
// non-string session_id) yields "", which ExplicitID treats as "no candidate".
func metaSessionID(v string) string {
	s := strings.TrimSpace(v)
	if !strings.HasPrefix(s, "{") {
		return v
	}
	if len(s) > maxMetadataLen {
		return ""
	}
	var m struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return ""
	}
	return m.SessionID
}

// Scoped is Resolve namespaced by tenant, for a hosted multi-tenant deployment. An
// empty tenant returns exactly what Resolve does, so single-tenant keys are
// unchanged.
//
// The namespace is not decoration. Both halves of the key need it for a different
// reason:
//
//   - The content-hash fallback is identical for two tenants running the same agent
//     against the same repository — same system prompt, same first message, same
//     hash. Unscoped, they would silently share one session: one sticky offload set,
//     one cached-prefix boundary, one another's state.
//   - The EXPLICIT id is client-supplied (the x-context-guru-session header), so
//     unscoped it lets any caller name another tenant's session by simply sending
//     its id. Prefixing means a tenant can only ever name keys inside its own
//     namespace, whatever it puts in the header.
//
// The explicit half is also validated here (see safeExplicit): this is the single
// point every caller routes through, so one guard here is worth a guard in the
// archive path plus one in the store plus one in every DELETE.
func Scoped(tenant, explicit, system, firstUser string) string {
	var base string
	// An unusable explicit id FALLS BACK to the content hash rather than failing the
	// request. Deliberate: the session id is not identity — the token is, and that
	// still fails closed — so a malformed one is a component-level input error, and
	// the project invariant there is FAIL OPEN. Falling back also degrades to exactly
	// the behaviour of a host that supplies no id at all, which is a supported mode,
	// whereas rejecting would let a header a user cannot see take their agent offline.
	// The cost is that two bad ids from one tenant share a session; that is inside a
	// tenant, never across one.
	if s := strings.TrimSpace(explicit); safeExplicit(s) {
		base = s
	} else {
		h := sha256.Sum256([]byte(system + "\x00" + firstUser))
		base = hex.EncodeToString(h[:])[:16]
	}
	if t := strings.TrimSpace(tenant); t != "" {
		return t + ":" + base
	}
	return base
}
