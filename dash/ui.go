package dash

import (
	"bytes"
	"compress/gzip"
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
// Charts are hand-drawn SVG rather than a vendored chart library — with ONE exception, d3,
// vendored for the Cleanup view's brush. The rule and the exception are both here because a
// contradiction with no explanation is worse than either choice.
//
// THE RULE STILL HOLDS for everything else. SVG path/rect/text is a native browser feature, the
// series on the other tabs are small, and a library would buy tooltips we can write in fifteen
// lines. Every chart outside dash/ui/insights.js is still drawn by hand.
//
// THE EXCEPTION, and what changed. That reasoning was about tooltips. The Cleanup view needs a
// RANGE BRUSH over a capability-by-time heatmap — handles you can grab, drag-to-move, keyboard
// access, and a selection that survives leaving the plot. A hand-rolled version shipped first
// (forty lines of pointer events) and the reported failure was "it's not clear how to select":
// the gesture worked and the affordance did not, which is the part a library has actually solved.
//
// What it costs, measured rather than assumed:
//
//   - d3 7.9.0, ISC, 273 KB raw and 90 KB gzipped, against a 300 KB gzipped asset baseline.
//     app.js alone is 548 KB raw. The assets are already gzipped and content-hashed, so the
//     marginal cost is one 90 KB download per deploy.
//   - The per-module builds were tried first and do NOT work in a browser: d3-brush's UMD build
//     calls require('d3-dispatch'), so the sixteen modules this needs (119 KB, tempting) cannot
//     chain without a bundler. Verified. It is the full bundle or nothing.
//   - Vendored, not a CDN, because this page serves script-src 'self' and must keep working on
//     an air-gapped install.
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

// gzippedUI holds each versioned asset gzip-compressed, built once at process start.
//
// Why bother when the assets are already embedded: on loopback bytes are free, and the
// measured cost of the four scripts here is ~99 ms of MAIN-THREAD CPU against ~31 ms of
// DOMContentLoaded — so compression is NOT the local bottleneck and this is not a local
// speedup. It is for the hosted multi-tenant service, where the same 705 KB of JS and
// 91 KB of CSS cross a WAN on every cold load and drop ~70%.
//
// Compressed once rather than per request: the bytes never change for the life of the
// process, so a gzip.Writer per response would burn CPU to produce an identical body.
// An asset that does not shrink is left out of the map and served raw.
var gzippedUI = gzipAssets(versionedUI)

func gzipAssets(assets map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(assets))
	for n, b := range assets {
		var buf bytes.Buffer
		zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if err != nil {
			return nil
		}
		if _, err := zw.Write(b); err != nil {
			return nil
		}
		if err := zw.Close(); err != nil {
			return nil
		}
		// Already-compressed formats (png, woff2) grow. Serve those raw.
		if buf.Len() < len(b) {
			out[n] = buf.Bytes()
		}
	}
	return out
}

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
		// between two of them also changes the token. The length is what makes the
		// concatenation injective — without it {"a": "b.js\x00xy"} and {"a": "",
		// "b.js": "xy"} hash identically — so do not drop it as redundant.
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
	// of an asset name across the five assets is a real reference, four in total.
	//
	// Do NOT narrow this to an enclosing HTML attribute. References do not all live in
	// attributes: tools.js builds one in a JavaScript object literal, el('link', {href:
	// 'tools.css'}), so an attribute match would silently stop versioning tools.css and
	// hand it back its unversioned URL at max-age=3600 — the stale-stylesheet bug named
	// at the top of this file, reintroduced. The blindness is load-bearing. If a
	// collision ever does appear, make the failure LOUD rather than the match narrower:
	// the rewrite count is knowable, and assetversion_test.go asserts it against the
	// references it enumerates, so an unaccounted rewrite is a red test.
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
			etag := `"` + assetVersion + `"`
			// Vary on EVERY asset response, compressed or not: a shared cache that
			// stored the raw body under an unvaried key would later hand it to a
			// client that asked for gzip, and vice versa.
			w.Header().Set("Vary", "Accept-Encoding")
			if gz, ok := gzippedUI[name]; ok && acceptsGzip(r.Header.Get("Accept-Encoding")) {
				// A different Content-Encoding is a different representation, so it
				// needs a different validator — otherwise a cache revalidating with
				// this ETag can be told "304, use what you have" and serve a gzip
				// body to a client that did not ask for one.
				w.Header().Set("Content-Encoding", "gzip")
				etag = `"` + assetVersion + `-gzip"`
				b = gz
			}
			w.Header().Set("ETag", etag)
			// ServeContent still types the response from `name` (the .js/.css
			// extension) and answers Range requests against the bytes it is given —
			// which for a compressed response is the encoded body, as RFC 9110
			// requires.
			//
			// It does NOT set Content-Length once Content-Encoding is set, so the
			// compressed responses go out chunked and a browser shows no download
			// progress for them. Do not "fix" that by setting Content-Length here:
			// ServeContent answers a Range request with a 206 whose body is only the
			// requested slice, and it will not overwrite a Content-Length we already
			// set — so a full-body length on a 206 would be a broken response rather
			// than a missing progress bar.
			http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(b))
			return
		}
		files.ServeHTTP(w, r)
	})
}

// acceptsGzip reports whether the client will take gzip. Deliberately not a full
// Accept-Encoding parser: the one case that has to be honoured is an explicit
// `gzip;q=0` refusal, because a client that says no and gets gzip anyway is broken
// rather than slow.
func acceptsGzip(ae string) bool {
	for _, part := range strings.Split(ae, ",") {
		fields := strings.Split(strings.TrimSpace(part), ";")
		if name := strings.TrimSpace(fields[0]); name != "gzip" && name != "*" {
			continue
		}
		for _, p := range fields[1:] {
			if q := strings.TrimSpace(p); strings.HasPrefix(q, "q=") {
				if v := strings.TrimSpace(strings.TrimPrefix(q, "q=")); v == "0" || strings.HasPrefix(v, "0.0") || v == "0." {
					return false
				}
			}
		}
		return true
	}
	return false
}
