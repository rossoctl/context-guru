package dash

import (
	"embed"
	"io/fs"
	"net/http"
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

// uiHandler serves the embedded UI. Assets are immutable per build, so they are
// cacheable; the HTML is not, so a redeploy is picked up on reload.
func uiHandler() http.Handler {
	sub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		return http.NotFoundHandler()
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "", "/", "index.html":
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
		files.ServeHTTP(w, r)
	})
}
