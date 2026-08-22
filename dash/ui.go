package dash

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The UI is ONE embedded directory: an HTML file, a stylesheet, and a script.
// No npm, no bundler, no build step, and — the part that matters for a tool that
// ships into VPCs and air-gapped clusters — no CDN. Every byte the page needs is
// in the binary, so the dashboard works with the network unplugged. headroom's
// Alpine/Tailwind/htmx script tags are exactly what we are not doing.
//
// Charts are hand-drawn SVG rather than a vendored chart library: the ladder's
// "native platform feature covers it" rung. SVG path/rect/text is a native
// browser feature, the series here are small, and 45 KB of vendored library would
// buy tooltips we can write in fifteen lines.
//
//go:embed ui
var uiFS embed.FS

// assetVersion is a content hash over every embedded UI asset. versionedUI holds each
// text asset with its references to sibling assets rewritten to carry that hash, so a
// new binary serves NEW asset URLs.
//
// Why: the HTML is no-cache and the assets were max-age=3600 with unversioned URLs, so
// a deploy that changed index.html and app.js together served NEW HTML against an
// HOUR-OLD app.js. New markup ran old code — buttons that rendered and did nothing, the
// previous refresh interval still ticking — and it healed itself when the cache expired,
// which makes it look like the deploy silently failed. Versioning the URLs makes that
// skew impossible: new bytes, new URL, and max-age=3600 is now correct rather than
// dangerous, because the old URL is never requested again.
//
// ONE token over all assets, not one per asset, because tools.css is referenced from
// tools.js rather than from the HTML. A per-asset hash of tools.js would not change when
// only tools.css did, so tools.js would stay cached at its old URL and keep pointing at
// the stale stylesheet — the same bug, moved. The cost of busting all five assets on any
// change is one extra ~550 KB fetch per deploy.
//
// Content, not buildinfo.Commit, so a dirty local rebuild at an unchanged commit also
// gets new URLs.
var assetVersion, versionedUI = versionAssets()

// rewritable lists the asset types that can name a sibling asset.
var rewritable = map[string]bool{".html": true, ".htm": true, ".js": true, ".css": true, ".svg": true}

func versionAssets() (string, map[string][]byte) {
	sub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		return "", nil
	}
	return versionFS(sub)
}

// versionFS hashes every file in fsys and returns that hash plus the rewritten assets.
// On any error it returns no assets, and the caller falls back to serving the embedded
// files verbatim — unversioned is the old behaviour, a blank dashboard is not.
func versionFS(fsys fs.FS) (string, map[string][]byte) {
	// WalkDir, not fs.Glob(fsys, "*"): Glob returns a SUBDIRECTORY as a name, fs.ReadFile
	// on it errors, and the whole function then bails to "no assets" — one nested file
	// would silently turn versioning off everywhere and bring the stale-asset bug back.
	// Walking covers assets in subdirectories instead, at their slash-separated names,
	// which is also what a reference to one looks like in the markup.
	var names []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			names = append(names, p)
		}
		return nil
	})
	if err != nil || len(names) == 0 {
		return "", nil
	}
	sort.Strings(names)

	sum := sha256.New()
	raw := make(map[string][]byte, len(names))
	for _, n := range names {
		b, err := fs.ReadFile(fsys, n)
		if err != nil {
			return "", nil
		}
		// Name and length go into the hash too, so renaming a file or moving bytes
		// between two of them also changes the token.
		fmt.Fprintf(sum, "%s\x00%d\x00", n, len(b))
		sum.Write(b)
		raw[n] = b
	}
	v := hex.EncodeToString(sum.Sum(nil))[:12]

	// Rewrite QUOTED references — href="style.css", src='app.js', and the
	// href: 'tools.css' that tools.js builds at runtime — rather than fixed lines of
	// known markup. Deliberately not a string replace against the shape of today's
	// index.html: this is blind to element, attribute, attribute order, whitespace and
	// quoting style, so reshaping the HTML cannot silently stop the versioning. The
	// alternation is built from the embedded directory itself, so an asset added later
	// is covered the day it lands. A reference that already carries ?v= does not match
	// (the closing quote no longer follows the name), so it is idempotent.
	//
	// ponytail: blind means blind — a quoted asset name that is NOT a URL is rewritten
	// too (a JS string literal, a CSP nonce, the inside of a percent-encoded data URI),
	// and that would fail silently. Today there is no collision: every quoted occurrence
	// of an asset name across the five assets is a real reference. Match on the enclosing
	// attribute if one ever appears.
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, regexp.QuoteMeta(n))
	}
	re := regexp.MustCompile(`(["'])(` + strings.Join(quoted, "|") + `)(["'])`)

	out := make(map[string][]byte, len(names))
	for n, b := range raw {
		if rewritable[strings.ToLower(path.Ext(n))] {
			b = re.ReplaceAll(b, []byte("${1}${2}?v="+v+"${3}"))
		}
		out[n] = b
	}
	return v, out
}

// uiHandler serves the embedded UI. Asset URLs carry a build content hash, so they are
// cacheable for an hour; the HTML is not cached, so a redeploy is picked up on reload
// and hands the browser the new asset URLs.
func uiHandler() http.Handler {
	sub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		return http.NotFoundHandler()
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// StripPrefix leaves "" for /dashboard/ and "index.html" for the explicit URL.
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		switch name {
		case "index.html":
			w.Header().Set("Cache-Control", "no-cache")
		default:
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		// The page loads nothing from the network beyond its own origin; say so, so a
		// stray CDN tag added later fails loudly in the browser instead of silently
		// breaking the air-gapped install.
		//
		// The last three do NOT fall back to default-src — each is a separate fetch
		// directive with its own default of "anything" — and nginx sets no
		// X-Frame-Options, so without them a self-only policy still allowed:
		// frame-ancestors, framing this page cross-site. The session cookie is
		//   SameSite=Lax, so a frame renders the UNAUTHENTICATED sign-in gate, which
		//   makes UI-redress phishing of the sign-in form the realistic abuse rather
		//   than riding an existing session. 'none' beats X-Frame-Options: DENY (which
		//   has no reliable ALLOW-FROM) and covers every embedding element.
		// base-uri, an injected <base> rebasing every relative URL on the page —
		//   which script-src cannot see, because the URLs stay same-origin-looking.
		// form-action, posting the sign-in form to another origin.
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; "+
				"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if b, ok := versionedUI[name]; ok {
			// Every request of this build serves identical bytes. The ETag turns the
			// no-cache HTML's revalidation into a 304 instead of a re-download.
			w.Header().Set("ETag", `"`+assetVersion+`"`)
			http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(b))
			return
		}
		files.ServeHTTP(w, r)
	})
}
