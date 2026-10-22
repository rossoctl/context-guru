package record

// Identity redaction: removing who recorded a session, not just their credentials.
//
// Content's credential pass leaves the recording person in plain sight. Measured on one real
// dual-recorded session (f987eccb, 2026-10-14): the OS username appeared 47,817 times (every
// C:\Users\<name> path, every mangled C--Users-<name> project dir), the surname 56,528 times,
// the work email 917 times and github.ibm.com/<owner>/<repo> URLs 312 times. A trace meant to
// be shared outside the team cannot carry those, so this file removes them.
//
// The rules are a port of claude-record's redactor.py identity pass (collector PR #38), kept
// in the same order so the two recorders remove the same things:
//
//  1. owner/repo in github.com and GitHub Enterprise URLs (site pages and anthropics/ kept,
//     because Claude Code's own system prompt names them)
//  2. the git user.email, then any email address (noreply and git@ kept)
//  3. the git user.name: full name and its login forms (dana-levi, dana.levi, danalevi) and
//     the last name in any case; any other part (a first name) only Capitalized or UPPER, so
//     a first name that is also a word ("Will") keeps the word
//  4. the local OS username, as a whole word in any case
//  5. extra terms -- logins that cannot be found automatically -- from CG_RECORD_REDACT_TERMS
//     (comma-separated) and ~/.claude-record/redact-terms.txt, the file claude-record reads,
//     so one person keeps one list for both recorders
//
// Emails and URLs go before names because a name is usually inside them.
//
// Every replacement is FIXED TEXT, never a hash or counter: two calls that share a prefix
// must still share it after redaction, or the trace is useless for prefix-cache replay.
//
// What is NOT ported: claude-record's user.id alias. claude-record exports Claude Code's
// telemetry, which carries user.id on every span; this recorder writes only what the proxy
// saw, and the one per-person id in a request (metadata.user_id) is read for the session
// UUID and never written. Measured on the same session: 0 device_id, 0 account_uuid.
//
// WHOSE IDENTITY. The local one: the OS user and git config of the machine the proxy runs
// on. That is right for a local proxy, which is how --record is run today. A hosted proxy
// would redact its own service account, which is harmless but useless, and would need the
// per-person terms supplied per tenant instead -- an open item, not something to guess at.

import (
	"bufio"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Markers for removed identity. Placeholder's «» family, with the category kept so a reader
// can tell a removed name from a removed key.
const (
	RedactedName     = "«redacted:name»"
	RedactedUsername = "«redacted:username»"
	RedactedEmail    = "«redacted:email»"
	RedactedTerm     = "«redacted:term»"
	RedactedOwner    = "«redacted:owner»"
	RedactedRepo     = "«redacted:repo»"
)

// Usernames and name parts too generic to redact as words: they would hit ordinary text.
var genericNames = map[string]bool{
	"admin": true, "dev": true, "home": true, "root": true, "runner": true, "test": true,
	"ubuntu": true, "user": true, "users": true, "vscode": true,
}

// GitHub site sections that look like owner/repo but name no one.
var githubSitePaths = map[string]bool{
	"about": true, "apps": true, "codespaces": true, "code-security": true, "collections": true,
	"customer-stories": true, "enterprise": true, "explore": true, "features": true, "login": true,
	"marketplace": true, "new": true, "notifications": true, "open-source": true, "orgs": true,
	"pricing": true, "readme": true, "resources": true, "security": true, "settings": true,
	"solutions": true, "sponsors": true, "topics": true, "trending": true,
	// Public, and in Claude Code's own system prompt; redacting it would only rewrite that.
	"anthropics": true,
}

var (
	emailRe = regexp.MustCompile(`([A-Za-z0-9._%+-]+)@([A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,})`)
	// github.com/<owner>/<repo>, also git@github.com:<owner>/<repo>.git and GitHub Enterprise.
	// The repo is optional: a bare profile URL (github.ibm.com/<login>, which gh api output
	// carries for every author) names a person just as well.
	githubRepoRe = regexp.MustCompile(`((?:https?://)?(?:www\.)?github(?:\.ibm)?\.com[/:])([A-Za-z0-9][A-Za-z0-9-]*)(?:/([\w.-]+))?`)
)

// Identity is who to remove from a recording. The zero value removes emails and repo URLs
// only.
type Identity struct {
	Usernames []string // local OS login names
	Names     []string // personal names, e.g. the git user.name
	Emails    []string // addresses always removed, even ones the generic rule would keep
	Terms     []string // anything else, e.g. GitHub logins
}

// identityRules is an Identity compiled into word patterns.
type identityRules struct {
	emails, anyCaseNames, casedNames, usernames, terms *regexp.Regexp
}

var (
	identityMu  sync.RWMutex
	identitySet *identityRules
	identityOnc sync.Once
)

// SetIdentity replaces the identity Content removes. The proxy never needs it -- the local
// identity is loaded on first use -- but tests do, so a result never depends on who runs them.
func SetIdentity(id Identity) {
	r := compileIdentity(id)
	identityOnc.Do(func() {}) // a later first use must not overwrite this
	identityMu.Lock()
	identitySet = r
	identityMu.Unlock()
}

func currentIdentity() *identityRules {
	identityOnc.Do(func() {
		r := compileIdentity(LocalIdentity())
		identityMu.Lock()
		identitySet = r
		identityMu.Unlock()
	})
	identityMu.RLock()
	defer identityMu.RUnlock()
	return identitySet
}

// LocalIdentity reads the identity of the person running the proxy: the OS login and home
// directory names, the git user.name and user.email, and the extra-terms list.
func LocalIdentity() Identity {
	var id Identity
	if u, err := user.Current(); err == nil {
		name := u.Username
		if i := strings.LastIndexAny(name, `\/`); i >= 0 {
			name = name[i+1:] // Windows reports DOMAIN\login
		}
		id.Usernames = append(id.Usernames, name)
	}
	home, err := os.UserHomeDir()
	if err == nil {
		id.Usernames = append(id.Usernames, filepath.Base(home))
	}
	if v := gitConfig("user.name"); v != "" {
		id.Names = append(id.Names, v)
	}
	if v := gitConfig("user.email"); v != "" {
		id.Emails = append(id.Emails, v)
	}
	for _, t := range strings.Split(os.Getenv("CG_RECORD_REDACT_TERMS"), ",") {
		id.Terms = append(id.Terms, strings.TrimSpace(t))
	}
	recHome := os.Getenv("CLAUDE_RECORD_HOME")
	if recHome == "" && home != "" {
		recHome = filepath.Join(home, ".claude-record")
	}
	if f, err := os.Open(filepath.Join(recHome, "redact-terms.txt")); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" && !strings.HasPrefix(line, "#") {
				id.Terms = append(id.Terms, line)
			}
		}
		f.Close()
	}
	return id
}

// gitConfig returns one git setting, or "" when git or the setting is missing. Bounded,
// because it runs once at the first recorded turn and a hung git must not stall the writer.
func gitConfig(key string) string {
	cmd := exec.Command("git", "config", "--get", key)
	done := make(chan []byte, 1)
	go func() {
		out, err := cmd.Output()
		if err != nil {
			out = nil
		}
		done <- out
	}()
	select {
	case out := <-done:
		return strings.TrimSpace(string(out))
	case <-time.After(5 * time.Second):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return ""
	}
}

func compileIdentity(id Identity) *identityRules {
	r := &identityRules{}
	var emails []string
	for _, e := range id.Emails {
		if strings.Contains(e, "@") {
			emails = append(emails, e)
		}
	}
	r.emails = wordsPattern(emails, true)

	anyCase, cased := map[string]bool{}, map[string]bool{}
	for _, name := range id.Names {
		parts := strings.FieldsFunc(strings.TrimSpace(name), func(c rune) bool {
			return c == ' ' || c == '\t' || c == '.' || c == '-' || c == '_'
		})
		if len(parts) >= 2 {
			for _, sep := range []string{" ", "-", ".", "_", ""} {
				anyCase[strings.Join(parts, sep)] = true
			}
			anyCase[parts[len(parts)-1]] = true
		}
		for _, p := range parts {
			// By rune, not byte: a first letter like É is two bytes.
			r, n := utf8.DecodeRuneInString(p)
			cased[string(unicode.ToUpper(r))+strings.ToLower(p[n:])] = true
			cased[strings.ToUpper(p)] = true
		}
	}
	r.anyCaseNames = wordsPattern(keys(anyCase), true)
	r.casedNames = wordsPattern(keys(cased), false)
	r.usernames = wordsPattern(id.Usernames, true)
	r.terms = wordsPattern(id.Terms, true)
	return r
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// wordsPattern matches any of words, longest first. Words shorter than 3 characters or in
// genericNames are dropped. Whole-word matching is done by isWord at replace time, since RE2
// has no lookaround.
func wordsPattern(words []string, ignoreCase bool) *regexp.Regexp {
	seen := map[string]bool{}
	var alts []string
	for _, w := range words {
		k := w
		if ignoreCase {
			k = strings.ToLower(w)
		}
		if len(w) < 3 || genericNames[strings.ToLower(w)] || seen[k] {
			continue
		}
		seen[k] = true
		alts = append(alts, regexp.QuoteMeta(w))
	}
	if len(alts) == 0 {
		return nil
	}
	sort.SliceStable(alts, func(i, j int) bool { return len(alts[i]) > len(alts[j]) })
	flags := ""
	if ignoreCase {
		flags = "(?i)"
	}
	return regexp.MustCompile(flags + "(?:" + strings.Join(alts, "|") + ")")
}

func isAlnum(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// replaceWords replaces each match of re that stands as a whole word.
func replaceWords(s string, re *regexp.Regexp, repl string) string {
	if re == nil {
		return s
	}
	return replaceMatches(s, re, func(s string, m []int) (string, bool) {
		// A literal `\n` or `\t` (escaped text inside tool output) is a boundary, not the
		// letter before a word.
		escaped := m[0] >= 2 && s[m[0]-2] == '\\' && (s[m[0]-1] == 'n' || s[m[0]-1] == 't')
		if m[0] > 0 && isAlnum(s[m[0]-1]) && !escaped || m[1] < len(s) && isAlnum(s[m[1]]) {
			return "", false
		}
		return repl, true
	})
}

// replaceMatches rebuilds s, replacing each match for which f says so. f gets the submatch
// indexes, so a rule can keep part of what it matched.
func replaceMatches(s string, re *regexp.Regexp, f func(s string, m []int) (string, bool)) string {
	ms := re.FindAllStringSubmatchIndex(s, -1)
	if ms == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range ms {
		out, ok := f(s, m)
		if !ok {
			continue
		}
		b.WriteString(s[last:m[0]])
		b.WriteString(out)
		last = m[1]
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

func redactRepoURL(s string, m []int) (string, bool) {
	// Not part of a longer host, e.g. notgithub.com.
	if m[0] > 0 {
		if c := s[m[0]-1]; isAlnum(c) || c == '_' || c == '.' || c == '-' {
			return "", false
		}
	}
	prefix, owner := s[m[2]:m[3]], s[m[4]:m[5]]
	if githubSitePaths[strings.ToLower(owner)] {
		return "", false
	}
	if m[6] < 0 {
		return prefix + RedactedOwner, true
	}
	repo := s[m[6]:m[7]]
	stripped := strings.TrimRight(repo, ".")
	tail := repo[len(stripped):]
	if strings.HasSuffix(stripped, ".git") {
		tail = ".git" + tail
	}
	return prefix + RedactedOwner + "/" + RedactedRepo + tail, true
}

func redactEmail(s string, m []int) (string, bool) {
	// user:password@host inside a URL is userinfo, not an address. The credential pass that
	// runs next removes the password and keeps the host; matching it here would lose the host.
	if start := strings.LastIndexAny(s[:m[0]], " \t\r\n\"'<>()"); strings.Contains(s[start+1:m[0]], "://") {
		return "", false
	}
	local, domain := strings.ToLower(s[m[2]:m[3]]), strings.ToLower(s[m[4]:m[5]])
	if local == "git" || strings.Contains(local, "noreply") || strings.Contains(local, "no-reply") ||
		strings.Contains(domain, "noreply") {
		return "", false
	}
	return RedactedEmail, true
}

// identityContent removes the recording person from s. Called by Content before the
// credential pass.
func identityContent(s string) string {
	r := currentIdentity()
	s = replaceMatches(s, githubRepoRe, redactRepoURL)
	s = replaceWords(s, r.emails, RedactedEmail)
	s = replaceMatches(s, emailRe, redactEmail)
	s = replaceWords(s, r.anyCaseNames, RedactedName)
	s = replaceWords(s, r.casedNames, RedactedName)
	s = replaceWords(s, r.usernames, RedactedUsername)
	return replaceWords(s, r.terms, RedactedTerm)
}
