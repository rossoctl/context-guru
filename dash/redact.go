package dash

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/rossoctl/context-guru/internal/redact"
)

// Redaction happens BEFORE anything reaches the database. This is the whole
// design: a secret that lands in a row is a secret on disk forever, and a
// redact-on-read filter is one forgotten code path away from leaking it. So
// nothing sensitive is ever stored, and the API has no redaction step at all.
//
// Two surfaces, one mechanism each:
//
//   - Config: allowlist the KEYS we render. context-guru's effective config is
//     structured and finite, so naming the safe keys is tractable and safe.
//   - Request HEADERS are not a surface at all: no capture path records them. There is
//     no header allowlist here on purpose — one used to exist with no production caller,
//     which read as a live protection and was not. If headers ever DO reach the recorder,
//     the allowlist has to come back with it (git history has the previous one); the
//     denylist alternative fails the moment a gateway invents a new auth header.
//
// Content (transcript before/after text) cannot be allowlisted — it is arbitrary
// agent output — so it gets pattern-based scrubbing of the shapes that are
// unambiguously credentials, plus a hard size cap.
//
// The credential vocabulary itself — the patterns, and the "this key NAME names a
// credential" test — lives in internal/redact, because internal/logging needs the
// same thing for log records and two copies of a denylist is one copy that goes
// stale. The allowlists below stay here: they describe the dashboard's own data
// shapes, not what a credential looks like.

// Redacted is the placeholder written in place of any redacted value. It is
// deliberately visible: a blank would read as "the field was empty".
const Redacted = redact.Redacted

// RedactContent scrubs credential-shaped substrings from captured transcript text
// and caps its length. cap<=0 means no cap. The cap is applied AFTER scrubbing so
// a secret near the end cannot survive by being truncated into place.
func RedactContent(s string, cap int) string { return redact.Content(s, cap) }

// metaEnumShape is the shape every stored request-metadata enum has to fit: a short
// identifier. `high`, `adaptive`, `end_turn`, `tool_calls` and `model_context_window_
// exceeded` all fit; a credential does not, because credentials are long and carry
// characters this does not admit.
//
// An allowlist of the values we know would be tighter still, and it is the wrong trade
// here: a provider that ships a new effort level or a new stop reason would have every
// row of it recorded as «redacted», which reads as "we caught a secret" — a dashboard
// lying about its own data. A shape check keeps an unrecognized-but-harmless value
// visible while still refusing anything credential-sized.
var metaEnumShape = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]{0,31}$`)

// metaEnum sanitizes one client-supplied metadata enum before it is inserted.
//
// Two gates, and both matter. RedactContent catches the credential SHAPES (sk-…, ghp_…,
// AKIA…, a JWT) that would otherwise fit inside the length bound, and the shape check
// then refuses everything else that is not a plain short identifier. Values are dropped
// wholesale rather than partially scrubbed because a partial value in an aggregated
// column is worse than none: it would open its own GROUP BY bucket.
func metaEnum(s string) string {
	if s == "" {
		return ""
	}
	if RedactContent(s, 0) != s || !metaEnumShape.MatchString(s) {
		return Redacted
	}
	return s
}

// projectPathShape is the shape a stored project key has to fit: an absolute POSIX path, no
// interior whitespace, no control characters.
//
// A shape check rather than an allowlist, and here that is forced rather than chosen — nobody can
// enumerate the directories a user may work in. It is also why the check has to be structural: the
// one thing we can say about a legitimate value is what it looks like.
var projectPathShape = regexp.MustCompile(`^/[^\s\x00-\x1f]{0,511}$`)

// SourceImported marks a row recovered from a Claude Code transcript rather than measured by the
// proxy. A measured row stores "" — see Event.Source for why.
const SourceImported = "imported"

// sourceOf allow-lists the source marker.
//
// ALLOW-LISTED, not stored as given, because every honesty rule downstream keys off this field:
// a report decides whether it may show a waste verdict by asking whether the window contains
// imported rows. An unrecognised value would quietly become a third kind of row that no rule
// covers. Anything but the known marker records as measured, which is the safe direction — it
// claims LESS about what the row can support, so a mistake here under-reports rather than
// inventing a finding.
func sourceOf(s string) string {
	if s == SourceImported {
		return SourceImported
	}
	return ""
}

// ProjectPath sanitizes an agent's launch directory for storage.
//
// THE TRUST BOUNDARY FOR THE PROJECT DIMENSION. A launch directory is a value the CLIENT chose, so
// it is attacker-influenced in exactly the way Meta.redact() describes: a request whose working
// directory happens to be named after a token would otherwise write that token into the requests
// table and onto the SSE feed, permanently and unredactably.
//
// Same composition as metaEnum — scan for credential shapes, then insist on a shape — against a
// different shape, since a path is neither short nor an identifier and so cannot reuse
// metaEnumShape. The order matters and is the same as there: RedactContent first, because a
// credential embedded in an otherwise path-shaped string must be caught before the shape test
// passes it.
//
// Returns Redacted rather than "" for a value it refuses. "" already means "this request did not
// state a project", and a refusal silently collapsing into that would be indistinguishable from
// the dimension not working — the same reasoning metaEnumShape's comment gives for preferring a
// visible placeholder.
func ProjectPath(s string) string {
	if s == "" {
		return ""
	}
	if RedactContent(s, 0) != s || !projectPathShape.MatchString(s) {
		return Redacted
	}
	return s
}

// configAllowlist names the effective-config keys the /api/config view may
// render. Everything else in the resolved configuration is withheld, because a
// component's config block is free-form YAML and could carry an endpoint
// credential (e.g. a component's model: block).
var configAllowlist = map[string]bool{
	"preset": true, "pipeline": true, "mode": true, "cache_mode": true,
	"inject_expand": true, "store": true, "components": true,
	"store_enabled": true, "store_ttl_seconds": true, "store_max_entries": true,
	"store_stash_max_bytes": true, "store_stash_ttl_seconds": true,
	"listen_addr": true, "openai_upstream": true, "anthropic_upstream": true,
	"bob_upstream": true, "force_model": true, "cheap_model": true,
	"cheap_model_provider": true, "cheap_model_base": true,
	"dashboard": true, "db_path": true, "retention": true, "capture_content": true,
	"trusted_cidrs": true, "build_version": true, "build_commit": true,
	"max_tokens": true, "min_tokens": true, "head_lines": true, "tail_lines": true,
	"strategy": true, "source": true, "model": true, "trigger": true,
	"min_request_tokens": true, "llm_every_n_requests": true, "llm_max_per_request": true,
	"marker_mode": true, "min_items": true, "keep_first": true, "keep_last": true,
	"enabled": true, "ttl_seconds": true, "max_entries": true, "stash_max_bytes": true,
	"stash_ttl_seconds": true,
}

// openKeys name subtrees whose immediate child keys are USER-CHOSEN and therefore
// cannot be allowlisted: `components` is keyed by component name, and a plugin can
// register any name it likes. Their children pass through by name, and the
// allowlist then applies one level deeper to the block's own fields — otherwise the
// effective-config view redacts every component's configuration and shows nothing,
// which defeats the point of the view.
var openKeys = map[string]bool{"components": true}

// RedactConfig walks a decoded configuration tree and returns a copy in which
// only allowlisted keys survive with their values; everything else is replaced by
// the placeholder. Maps and slices are walked; scalars are passed through.
func RedactConfig(v any) any {
	return redactConfig(v, false)
}

func redactConfig(v any, openNames bool) any {
	switch t := v.(type) {
	case string:
		// An allowlisted KEY does not make its VALUE safe. `anthropic_upstream` is on the
		// allowlist by name and an upstream URL is exactly where a `user:password@`
		// credential lives, so the value is still checked. Only the userinfo is replaced,
		// leaving the host visible — a wholly redacted upstream would make the config view
		// useless for the thing it exists to answer ("where is this pointing?").
		if strings.Contains(t, "://") {
			return redact.URLCredentials(t)
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			lk := strings.ToLower(k)
			switch {
			case redact.IsSecretKey(lk):
				out[k] = Redacted
			case openNames:
				// This level's key is a user-chosen name (a component id); keep it and
				// resume allowlisting inside its block.
				out[k] = redactConfig(val, false)
			case openKeys[lk]:
				out[k] = redactConfig(val, true)
			case configAllowlist[lk]:
				out[k] = redactConfig(val, false)
			default:
				out[k] = Redacted
			}
		}
		return out
	case map[any]any: // yaml can produce this when a key is not a string
		conv := make(map[string]any, len(t))
		for k, val := range t {
			conv[fmt.Sprint(k)] = val
		}
		return redactConfig(conv, openNames)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = redactConfig(e, openNames)
		}
		return out
	default:
		return v
	}
}
