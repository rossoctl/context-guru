package dash

import (
	"fmt"
	"regexp"
	"strings"
)

// Redaction happens BEFORE anything reaches the database. This is the whole
// design: a secret that lands in a row is a secret on disk forever, and a
// redact-on-read filter is one forgotten code path away from leaking it. So
// nothing sensitive is ever stored, and the API has no redaction step at all.
//
// Two mechanisms, matching gateway's (correct) choice of default:
//
//   - Headers: blanket-redact by KEY. Every header is dropped unless it is on a
//     short allowlist of headers known to be non-secret. A denylist of "the auth
//     headers we thought of" fails the moment a gateway invents a new one.
//   - Config: allowlist the KEYS we render. context-guru's effective config is
//     structured and finite, so naming the safe keys is tractable and safe.
//
// Content (transcript before/after text) cannot be allowlisted — it is arbitrary
// agent output — so it gets pattern-based scrubbing of the shapes that are
// unambiguously credentials, plus a hard size cap.

// Redacted is the placeholder written in place of any redacted value. It is
// deliberately visible: a blank would read as "the field was empty".
const Redacted = "«redacted»"

// headerAllowlist is the set of request headers safe to store verbatim. Anything
// not listed here is redacted by key, value unseen.
var headerAllowlist = map[string]bool{
	"content-type":                true,
	"content-length":              true,
	"user-agent":                  true,
	"accept":                      true,
	"accept-encoding":             true,
	"anthropic-version":           true,
	"anthropic-beta":              true,
	"x-stainless-lang":            true,
	"x-stainless-os":              true,
	"x-stainless-arch":            true,
	"x-stainless-package-version": true,
	"x-stainless-runtime":         true,
	"x-stainless-runtime-version": true,
	"x-app":                       true,
}

// RedactHeaders returns a storable copy of a request's headers: allowlisted keys
// keep their value, every other key is present (so you can see WHAT was sent)
// with its value replaced.
func RedactHeaders(h map[string][]string) map[string]string {
	out := make(map[string]string, len(h))
	for k, vs := range h {
		lk := strings.ToLower(k)
		if headerAllowlist[lk] && len(vs) > 0 {
			out[lk] = vs[0]
			continue
		}
		out[lk] = Redacted
	}
	return out
}

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

// configAllowlist names the effective-config keys the /api/config view may
// render. Everything else in the resolved configuration is withheld, because a
// component's config block is free-form YAML and could carry an endpoint
// credential (e.g. a component's model: block).
var configAllowlist = map[string]bool{
	"preset": true, "pipeline": true, "mode": true, "cache_mode": true,
	"inject_expand": true, "store": true, "components": true,
	"store_enabled": true, "store_ttl_seconds": true, "store_max_entries": true,
	"listen_addr": true, "openai_upstream": true, "anthropic_upstream": true,
	"bob_upstream": true, "force_model": true, "cheap_model": true,
	"cheap_model_provider": true, "cheap_model_base": true,
	"dashboard": true, "db_path": true, "retention": true, "capture_content": true,
	"trusted_cidrs": true, "build_version": true, "build_commit": true,
	"max_tokens": true, "min_tokens": true, "head_lines": true, "tail_lines": true,
	"strategy": true, "source": true, "model": true, "trigger": true,
	"min_request_tokens": true, "llm_every_n_requests": true, "llm_max_per_request": true,
	"marker_mode": true, "min_items": true, "keep_first": true, "keep_last": true,
	"enabled": true, "ttl_seconds": true, "max_entries": true,
}

// secretishKey matches config keys that are credentials BY NAME, whatever the
// allowlist says — so a component block nesting an api_key under an otherwise
// allowlisted name cannot leak.
//
// It is deliberately anchored on whole words rather than substrings. A naive
// `(key|token|...)` substring match also swallows `max_tokens`, `min_tokens` and
// `min_request_tokens`, redacting every threshold in the config view and making it
// useless — the same "safety that destroys the feature" trap as redacting whole
// component blocks. `cache_key` and `api_key` still match; `max_tokens` does not.
var secretishKey = regexp.MustCompile(`(?i)(^|_)(api_?key|access_?key|secret_?key|private_?key|` +
	`auth_?token|access_?token|refresh_?token|id_?token|session_?token|bearer_?token|` +
	`key|token|secret|password|passwd|passphrase|credential|credentials|auth|authorization|bearer|cookie)($|_)`)

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
			return urlUserinfo.ReplaceAllString(t, `${1}:`+Redacted+`@`)
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			lk := strings.ToLower(k)
			switch {
			case secretishKey.MatchString(lk):
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

// credentialWord is the credential vocabulary shared by the content assignment
// pattern and the config key check. Written once so the two cannot drift.
//
// Anchoring note, and it is load-bearing: callers append this to a `[A-Za-z0-9_.-]*`
// prefix and require a delimiter immediately after, so the key must END with one of
// these words. That is what stops `max_tokens=3000` and `min_request_tokens` from
// being read as credentials and redacting every threshold in the config view — the
// "safety that destroys the feature" trap. `api_key` matches; `max_tokens` does not.
const credentialWord = `api[_-]?key|access[_-]?key|secret[_-]?key|account[_-]?key|` +
	`private[_-]?key(?:[_-]?id)?|client[_-]?secret|` +
	`auth[_-]?token|access[_-]?token|refresh[_-]?token|id[_-]?token|session[_-]?token|` +
	`bearer[_-]?token|api[_-]?token|secret|password|passwd|passphrase|credential|token`

// contentSecrets are the credential shapes that appear verbatim in agent output (a
// leaked env dump, a curl command in a shell transcript, a `cat .env`) and always run.
// Each is anchored on a literal prefix, which RE2 can prefilter internally, so these
// are the cheap ones.
//
// This is a DENYLIST, and a denylist over arbitrary text is structurally incomplete —
// a review of 22 realistic shapes found 11 passing through. The patterns here and in
// contentSecretsDelimited close those 11 and are pinned by a table-driven test, but
// the conclusion drawn from that review is not "the list is now complete", it is that
// content capture must be opt-in. See --dashboard-content.
var contentSecrets = []*regexp.Regexp{
	// Well-known credential prefixes, one alternation rather than seven separate passes
	// over the blob. `sk-ant-` is covered by `sk-`; GitLab, Stripe, HuggingFace and
	// Google were missing entirely.
	regexp.MustCompile(`(?i)\b(?:sk-[A-Za-z0-9_-]{16,}|sk_live_[A-Za-z0-9]{16,}|` +
		`rk_live_[A-Za-z0-9]{16,}|ghp_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|` +
		`glpat-[A-Za-z0-9_-]{16,}|hf_[A-Za-z0-9]{16,}|xox[baprs]-[A-Za-z0-9-]{10,}|` +
		`ya29\.[A-Za-z0-9_-]{16,}|AIza[A-Za-z0-9_-]{30,})`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),                                           // AWS access key id: case-SENSITIVE
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`), // JWT
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
}

// contentSecretsDelimited are the shapes that CANNOT match without a `:` or `=` in the
// blob, so RedactContent skips them entirely on text that contains neither. They are
// also the expensive ones (an unanchored `[A-Za-z0-9_.-]*` prefix scan), which is why
// the one-pass ContainsAny check in front of them is worth having.
//
// Ordering matters: the header and URL rules come before the generic assignment rule
// so the more specific replacement wins.
var contentSecretsDelimited = []*regexp.Regexp{
	// Auth headers, matched to end of LINE rather than with `\S+`.
	//
	// This is the bug the review called most alarming: `\S+` stops at the space after
	// the scheme, so `Authorization: Bearer <token>` redacted the word "Bearer" and left
	// the credential in the diff view. A scheme plus a token is two words.
	regexp.MustCompile(`(?i)\b(authorization|proxy-authorization|x-api-key|x-auth-token|api-key)\s*:[^\r\n]*`),

	// Credentials in a URL's userinfo (`scheme://user:pass@host`). Its own rule because
	// the password, not the whole URL, is the secret: redacting the host would tell the
	// reader nothing about what leaked. Replaced via submatch, so scheme and user live.
	regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^\s/:@]+):[^\s/@]+@`),

	// `NAME=value` / `"name": "value"` where the NAME says credential.
	//
	// The optional quote before the name is the one character that was missing: the old
	// `\b[A-Z0-9_]*` could not cross the `"` in `{"api_key": "..."}`, so every
	// JSON-shaped credential (including GCP service-account keys) passed through. The
	// `;` in the value terminator set is what catches Azure connection strings, where
	// `AccountKey=...` is one field among several on a line.
	regexp.MustCompile(`(?i)(["']?[A-Za-z0-9_.\-]*(?:` + credentialWord + `)["']?\s*[:=]\s*)["']?[^\s"',;}]{8,}`),
}

// urlUserinfo is the URL rule above, applied with a submatch replacement so only the
// password is replaced.
var urlUserinfo = contentSecretsDelimited[1]

// redactValue replaces a matched credential, keeping the assignment's or header's NAME
// so a diff still shows WHAT was set.
func redactValue(m string) string {
	if i := strings.IndexAny(m, ":="); i > 0 {
		return m[:i+1] + " " + Redacted
	}
	return Redacted
}

// RedactContent scrubs credential-shaped substrings from captured transcript text
// and caps its length. cap<=0 means no cap. The cap is applied AFTER scrubbing so
// a secret near the end cannot survive by being truncated into place.
func RedactContent(s string, cap int) string {
	for _, re := range contentSecrets {
		s = re.ReplaceAllStringFunc(s, redactValue)
	}
	// The delimited rules cannot match without one of these two bytes, and they are the
	// expensive half of the pass, so a blob with neither (most prose, most code bodies)
	// skips them after one scan of the string. Correctness does not depend on the
	// prefilter agreeing with the regexes by eye: every pattern below literally
	// requires `:` or `=`.
	if strings.ContainsAny(s, ":=") {
		for _, re := range contentSecretsDelimited {
			if re == urlUserinfo {
				s = re.ReplaceAllString(s, `${1}:`+Redacted+`@`)
				continue
			}
			s = re.ReplaceAllStringFunc(s, redactValue)
		}
	}
	if cap > 0 && len(s) > cap {
		for cap > 0 && !isRuneStart(s[cap]) {
			cap--
		}
		return s[:cap] + "\n…[truncated: content capture cap reached]"
	}
	return s
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
