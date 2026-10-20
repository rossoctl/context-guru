package record

// Redaction for recorded traces.
//
// THE DESIGN DECISION, AND IT IS THE IMPORTANT ONE IN THIS FILE: this does NOT reimplement
// credential scrubbing. It calls internal/redact.Content first and then adds only the
// shapes that a measured comparison showed it misses. The alternative -- porting the
// existing Python redactor (claude-record's redactor.py, ~24 patterns) into Go -- was the
// original plan and is the wrong one, for a reason that is worth writing down rather than
// rediscovering:
//
// internal/redact already exists, is already the shared vocabulary for two consumers (dash
// and internal/logging), and its own package doc says a second copy of these patterns
// "would be a second thing to keep current, and the first one forgotten is a leak." A
// recorder that shipped its own parallel pattern list would be exactly that second copy,
// and it would be the copy nobody on the CG side maintains.
//
// MEASURED, not assumed. Both redactors were run over one corpus of 38 strings (32 real
// credential shapes plus 6 negative controls that must survive):
//
//	internal/redact : caught 21/32, 0 false positives
//	redactor.py     : caught 28/32, 0 false positives
//
// Neither is a superset. The seven that only redactor.py caught, and which the additions
// below therefore close: ASIA-prefixed AWS keys, base64-wrapped secrets (3 shapes),
// Databricks dapi, GitLab gldt, npm_, PyPI, SendGrid SG., and Slack webhook URLs. In the
// other direction internal/redact caught three that redactor.py misses, and those are left
// to it rather than duplicated: cg_live_ (CG's own token shape, which claude-record has
// never heard of), ya29. Google OAuth, and -- the one that matters most -- the
// JSON-SHAPED assignment `{"api_key": "..."}`.
//
// That last one is a live latent bug in redactor.py, not a difference of taste:
// its _KEYWORD_VALUE_PATTERN uses a backreference `(['"]?)value\1`, which cannot cross the
// quote BEFORE the key name, so `api_key="x"` is caught and `"api_key": "x"` is not --
// and the JSON shape is the shape every trace attribute actually has. internal/redact
// fixed precisely this and documents it. (Scanned: the gap has leaked nothing in the
// existing 67-file corpus -- 0 hits -- so it is a latent gap, not an incident.)
//
// So composing is not merely less work; it is strictly better coverage than either side
// alone, and it leaves the credential vocabulary in the one place CG already maintains.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/rossoctl/context-guru/internal/redact"
)

// Placeholder marks a removed value. It reuses internal/redact's visible-by-design
// placeholder so a reader sees one convention, not two.
const Placeholder = redact.Redacted

// extraSecrets are the credential shapes internal/redact does not cover, established by
// the measured comparison in this file's doc comment. Each one is here because a specific
// test case passed through, and each has a test pinning it.
//
// RE2 note: these are Go regexps, so there are no backreferences and no lookaround. Every
// pattern below is a plain prefix-anchored shape, which is also what makes them cheap --
// RE2 can prefilter on the literal prefix.
var extraSecrets = []*regexp.Regexp{
	// AWS keys that are not AKIA. internal/redact pins `\bAKIA[0-9A-Z]{16}\b` only;
	// ASIA (STS session), ABIA and ACCA are the same shape and the same severity.
	regexp.MustCompile(`\b(?:ASIA|ABIA|ACCA)[0-9A-Z]{16}\b`),

	// Databricks personal access token.
	regexp.MustCompile(`\bdapi[0-9a-f]{32}\b`),

	// GitLab deploy token (glpat is covered upstream; gldt is not).
	regexp.MustCompile(`\bgldt-[A-Za-z0-9_-]{16,}`),

	// npm automation token.
	regexp.MustCompile(`\bnpm_[A-Za-z0-9]{20,}`),

	// PyPI upload token.
	regexp.MustCompile(`\bpypi-[A-Za-z0-9_-]{20,}`),

	// SendGrid: fixed-width three-part shape, so it can be matched exactly.
	regexp.MustCompile(`\bSG\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}`),

	// Slack incoming webhook. The whole URL is the credential -- anyone holding it can
	// post to the channel -- so unlike a userinfo URL there is nothing worth preserving.
	regexp.MustCompile(`https://hooks\.slack\.com/services/T[A-Za-z0-9_]+/B[A-Za-z0-9_]+/[A-Za-z0-9_]+`),

	// PGP private key block. internal/redact covers the `-----BEGIN ... PRIVATE KEY-----`
	// family, but the PGP header ends in " BLOCK", which its `[A-Z ]*` prefix class cannot
	// match -- so this shape needs its own pattern.
	//
	// The HashiCorpIgnore marker below is required and is NOT a way of quietening a real
	// finding. The line has to contain the armour header verbatim in order to match it, which
	// makes the pattern that FINDS PGP keys look like a PGP key to a credential scanner: IBM
	// Vault Radar blocked the first commit of this file on exactly that, medium severity in
	// non-test code. The marker is the sanctioned inline exclusion for a single reviewed
	// false-positive line, which is narrower than a path exclusion (whole files, and
	// accountability for anything later hidden in them) and narrower than secret_types, which
	// would switch PGP detection off for this entire repository -- not ours to do. There is no
	// key material here; deleting the pattern would stop PGP private keys being redacted from
	// recorded traces, which is the opposite of the intent.
	regexp.MustCompile(`-----BEGIN PGP PRIVATE KEY BLOCK-----[\s\S]*?-----END PGP PRIVATE KEY BLOCK-----`), // HashiCorpIgnore.
}

// b64Candidate finds base64-looking runs long enough to hide a credential. 64 is the
// threshold redactor.py used and is kept: shorter runs are common in ordinary text
// (checksums, short ids) and decoding them all would be a lot of work for nothing.
var b64Candidate = regexp.MustCompile(`[A-Za-z0-9+/]{64,}={0,2}`)

// Content scrubs credentials, and the identity of the person recording, from arbitrary
// recorded text.
//
// Order is deliberate: identity first (see identity.go), then internal/redact.Content (it owns the vocabulary and handles
// the submatch-preserving URL and header rules), then the extra shapes, then the base64
// pass. cap<=0 means no cap, and the cap is NOT applied here -- see CapContent -- because
// a recorded trace wants the whole turn, and truncation for a trace is an incompleteness
// fact that belongs on the span (Turn.Incomplete), not a silent trim inside a redactor.
func Content(s string) string {
	if s == "" {
		return s
	}
	s = identityContent(s)
	s = redact.Content(s, 0)
	for _, re := range extraSecrets {
		s = re.ReplaceAllString(s, Placeholder)
	}
	return redactEncoded(s)
}

// ContentJSON scrubs a JSON document by scrubbing the STRINGS INSIDE IT, never its encoded
// text, and splices back only the strings that changed.
//
// WHY, measured on the first full day of recordings (2026-10-07): Content run over encoded
// JSON corrupted 63 of 314 bodies. The assignment rule's value class `[^\s"',;}]` admits a
// backslash, so in `?token=XYZ\"` the match took the `\` of the escaped quote with it and
// left a bare `"` behind -- invalid JSON. Turn reconstruction parses these bodies, so the
// first broken string silently ended turn derivation for the rest of the session (2 of 7
// turns found). Decoded text has no escapes for a pattern to eat, and a re-encoded string
// is always a valid literal, so the document stays valid whatever the patterns do.
//
// What the encoded pass caught ACROSS literals is kept: an object member is also checked
// as `"key": value`, the exact shape internal/redact's JSON-assignment rule exists for, and
// its value is replaced whole when that pair would have been scrubbed. Bytes outside a
// changed literal -- key order, whitespace, `<`-style escapes -- are untouched, so a
// body with nothing to redact comes back identical.
//
// Text that is not valid JSON falls back to Content: a capped or otherwise partial body is
// still scrubbed, just without the structure guarantee.
func ContentJSON(s string) string {
	if s == "" || !json.Valid([]byte(s)) {
		return Content(s)
	}
	var b strings.Builder
	last := 0
	for i := 0; i < len(s); {
		if s[i] != '"' {
			i++
			continue
		}
		end := stringEnd(s, i)
		vs := skipSpace(s, end)
		if vs >= len(s) || s[vs] != ':' {
			// A string that is not a key: an array element or the whole document.
			if out, ok := scrubLiteral(s[i:end]); ok {
				b.WriteString(s[last:i])
				b.WriteString(out)
				last = end
			}
			i = end
			continue
		}
		// An object key. Keys are scrubbed like any string; then the member is checked as
		// a pair when its value is a string or a scalar.
		if out, ok := scrubLiteral(s[i:end]); ok {
			b.WriteString(s[last:i])
			b.WriteString(out)
			last = end
		}
		key := decodeLiteral(s[i:end])
		vs = skipSpace(s, vs+1)
		var vend int
		switch {
		case s[vs] == '"':
			vend = stringEnd(s, vs)
		case s[vs] == '{' || s[vs] == '[':
			i = vs // a container: its contents are walked by the main loop
			continue
		default:
			vend = scalarEnd(s, vs)
		}
		raw := s[vs:vend]
		var out string
		var changed bool
		if pair := `"` + key + `": ` + pairValue(raw); redact.Content(pair, 0) != pair {
			out, changed = encodeLiteral(Placeholder), true
		} else if raw[0] == '"' {
			out, changed = scrubLiteral(raw)
		}
		if changed {
			b.WriteString(s[last:vs])
			b.WriteString(out)
			last = vend
		}
		i = vend
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// pairValue renders a member's value the way the assignment rule saw it in a flat
// document: a string as its decoded text in quotes, a scalar as its raw token.
func pairValue(raw string) string {
	if raw[0] == '"' {
		return `"` + decodeLiteral(raw) + `"`
	}
	return raw
}

// scrubLiteral runs Content over one decoded string literal and reports whether that
// changed it; the replacement is re-encoded, so it is always a valid literal.
func scrubLiteral(lit string) (string, bool) {
	v := decodeLiteral(lit)
	if c := Content(v); c != v {
		return encodeLiteral(c), true
	}
	return lit, false
}

func decodeLiteral(lit string) string {
	if !strings.ContainsRune(lit, '\\') {
		return lit[1 : len(lit)-1]
	}
	var v string
	_ = json.Unmarshal([]byte(lit), &v) // lit came from a document json.Valid accepted
	return v
}

func encodeLiteral(v string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep `<` and `&` as the client sent them
	_ = enc.Encode(v)
	return strings.TrimSuffix(buf.String(), "\n")
}

// stringEnd returns the index just past the literal that opens at s[i].
func stringEnd(s string, i int) int {
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}
	return len(s)
}

func scalarEnd(s string, i int) int {
	for i < len(s) && !strings.ContainsRune(",}] \t\r\n", rune(s[i])) {
		i++
	}
	return i
}

func skipSpace(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || s[i] == '\n') {
		i++
	}
	return i
}

// redactEncoded decodes base64-looking runs and replaces the whole run if what comes out
// contains a credential.
//
// WHY THIS IS HERE AT ALL, given that internal/redact explicitly declines to chase it:
// its stated reason is that a decoding rule "would flag ordinary text and teach readers to
// ignore the placeholder" -- a false-positive argument, and a correct one for the
// DASHBOARD, where a human reads every redaction. This is a different consumer. A recorded
// corpus is read by scripts, is retained, and is intended to be shared outside the team
// after a consent process, so a false positive costs one unreadable blob in a body while a
// false negative puts a live credential in a shared dataset. The trade runs the other way
// here, so the rule is enabled here and not there.
//
// The false-positive risk is contained by only replacing when the DECODED bytes match a
// credential pattern -- ordinary base64 prose decodes to prose, matches nothing, and is
// left exactly as it was. That is what the negative-control test pins.
func redactEncoded(s string) string {
	if len(s) < 64 {
		return s
	}
	return b64Candidate.ReplaceAllStringFunc(s, func(run string) string {
		dec, err := base64.StdEncoding.DecodeString(run)
		if err != nil {
			// Not valid base64 (a long hex string, a minified blob). Leave it alone: a
			// decode failure is evidence this was never encoded data.
			return run
		}
		if containsSecret(string(dec)) {
			return Placeholder
		}
		return run
	})
}

// containsSecret reports whether text holds something the scrubbers would remove. It is
// the decision function for the base64 pass, and it is deliberately written as "does
// scrubbing change this?" rather than as its own pattern list -- a separate list would be
// the third copy of the vocabulary and would drift from the two real ones.
func containsSecret(text string) bool {
	if redact.Content(text, 0) != text {
		return true
	}
	for _, re := range extraSecrets {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// CapContent applies a size cap AFTER scrubbing, and reports whether it bit.
//
// Separate from Content on purpose. internal/redact.Content folds the cap in and appends a
// human-readable truncation note, which is right for a dashboard field. For a trace the
// truncation is a fact about the TURN: it belongs in Turn.Incomplete where an analysis can
// filter on it, not buried in the body text where it can only be found by a substring
// search. Callers that cap must set Incomplete accordingly.
func CapContent(s string, cap int) (string, bool) {
	if cap <= 0 || len(s) <= cap {
		return s, false
	}
	for cap > 0 && !isRuneStart(s[cap]) {
		cap--
	}
	return s[:cap], true
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// Redact scrubs every free-text field on the turn in place, and records a cap hit as
// incompleteness rather than letting it pass silently.
//
// Idempotent, so a double call cannot corrupt a turn.
//
// GOROUTINE PLACEMENT IS THE SECURITY PROPERTY, and it is not this function's choice: this
// must be called on the WRITER goroutine, immediately before the bytes are written, never
// on the request goroutine. dash/event.go measured the cost of getting this wrong at
// ~53 ms, about 25% of a request, paid by the next request on a keep-alive connection.
// The ordering against the write is what guarantees no unscrubbed byte ever reaches disk.
func (t *Turn) Redact(bodyCap int) {
	// The four body fields hold translated JSON, so they go through ContentJSON: scrubbing
	// the encoded text instead is what broke 63 bodies on the 2026-10-07 recordings.
	t.RequestBody = ContentJSON(t.RequestBody)
	t.ResponseBody = ContentJSON(t.ResponseBody)
	t.OutputSchema = ContentJSON(t.OutputSchema)
	// Tool definitions are author-written descriptions and JSON schemas. They routinely
	// carry example values, and an example is exactly where a real endpoint or token gets
	// pasted, so they are scrubbed like any other free text.
	t.ToolDefs = ContentJSON(t.ToolDefs)
	// Err can carry an upstream error body verbatim, and a gateway that echoes the
	// presented credential in a 401 would otherwise put it on disk. Never capped: it is a
	// short diagnostic.
	t.Err = Content(t.Err)
	// StopReason is provider-chosen from a closed set, but it arrives as a client- or
	// upstream-influenced string, so it is scrubbed too rather than trusted.
	t.StopReason = Content(t.StopReason)

	if bodyCap > 0 {
		var hit bool
		if t.RequestBody, hit = CapContent(t.RequestBody, bodyCap); hit {
			t.markIncomplete(IncompleteTeeCap)
		}
		if t.ResponseBody, hit = CapContent(t.ResponseBody, bodyCap); hit {
			t.markIncomplete(IncompleteTeeCap)
		}
	}
}

// markIncomplete records a reason without overwriting one already set. An existing reason
// came from the proxy and describes WHY the body was never whole in the first place, which
// is more specific than a downstream cap hit, so the first reason wins.
func (t *Turn) markIncomplete(reason string) {
	if t.Incomplete == IncompleteNone {
		t.Incomplete = reason
		return
	}
	if !strings.Contains(t.Incomplete, reason) {
		t.Incomplete += "," + reason
	}
}

// MarkIncomplete is markIncomplete for the proxy, which fills a Turn from another package.
//
// It exists because Turn.Incomplete is not a plain assignment: a turn can be incomplete for
// several reasons at once (a retain bound AND a multi-round continuation), the FIRST reason
// is the most specific and must survive, and later distinct reasons append comma-separated
// so an analysis can search for any one of them. A caller setting the field directly would
// have to reimplement that rule, and would eventually get it wrong in a way no test here
// would catch.
func (t *Turn) MarkIncomplete(reason string) {
	if reason == IncompleteNone {
		return
	}
	t.markIncomplete(reason)
}
