// Package redact holds the credential-scrubbing primitives: the patterns that
// recognise a credential in arbitrary text, and the key-name test that recognises
// one by the name it is stored under.
//
// It lives here, below everything, because there are now TWO consumers with the
// same requirement and they must not drift: `dash` scrubs before writing a captured
// request to disk, and `internal/logging` scrubs before writing a log record to
// stderr or to Loki. A second copy of these patterns would be a second thing to
// keep current, and the first one forgotten is a leak. `dash` is the historical
// home (dash/redact.go) and keeps its allowlists — those are about the dashboard's
// own data shapes; only the credential vocabulary moved.
//
// The two mechanisms, and the reasoning behind each, are documented where they are
// used: see dash/redact.go for why headers are allowlisted by KEY and config keys
// by name, and why content — arbitrary agent output — can only get pattern-based
// scrubbing plus a size cap.
package redact

import (
	"regexp"
	"strings"
)

// Redacted is the placeholder written in place of any redacted value. It is
// deliberately visible: a blank would read as "the field was empty".
const Redacted = "«redacted»"

// secretishKey matches keys that are credentials BY NAME, whatever an allowlist
// says — so a component block nesting an api_key under an otherwise allowlisted
// name cannot leak, and neither can `slog.Info("...", "api_key", k)`.
//
// It is deliberately anchored on whole words rather than substrings. A naive
// `(key|token|...)` substring match also swallows `max_tokens`, `min_tokens` and
// `min_request_tokens`, redacting every threshold in the config view and making it
// useless — the same "safety that destroys the feature" trap as redacting whole
// component blocks. `cache_key` and `api_key` still match; `max_tokens` does not.
var secretishKey = regexp.MustCompile(`(?i)(^|_)(api_?key|access_?key|secret_?key|private_?key|` +
	`auth_?token|access_?token|refresh_?token|id_?token|session_?token|bearer_?token|` +
	`key|token|secret|password|passwd|passphrase|credential|credentials|auth|authorization|bearer|cookie)($|_)`)

// IsSecretKey reports whether a key NAME says "the value under me is a credential".
// Case-insensitive; callers need not normalise.
func IsSecretKey(k string) bool { return secretishKey.MatchString(k) }

// credentialWord is the credential vocabulary shared by the content assignment
// pattern and the key check. Written once so the two cannot drift.
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
// blob, so Content skips them entirely on text that contains neither. They are
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

// URLCredentials replaces only the password in a `scheme://user:pass@host` URL,
// leaving the scheme, the user and the host visible — a wholly redacted URL would
// make an upstream field useless for the thing it exists to answer ("where is this
// pointing?").
func URLCredentials(s string) string {
	return urlUserinfo.ReplaceAllString(s, `${1}:`+Redacted+`@`)
}

// redactValue replaces a matched credential, keeping the assignment's or header's NAME
// so a diff still shows WHAT was set.
func redactValue(m string) string {
	if i := strings.IndexAny(m, ":="); i > 0 {
		return m[:i+1] + " " + Redacted
	}
	return Redacted
}

// Content scrubs credential-shaped substrings from arbitrary text and caps its
// length. cap<=0 means no cap. The cap is applied AFTER scrubbing so a secret near
// the end cannot survive by being truncated into place.
func Content(s string, cap int) string {
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
