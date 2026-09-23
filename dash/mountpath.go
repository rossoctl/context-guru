package dash

import "strings"

// DefaultUIPath is where the dashboard UI mounts when a host does not choose otherwise.
//
// Unchanged, and it has to be: it is in this project's README, in every existing bookmark, and in
// the refusal messages the API already emits. A deployment that never calls SetUIPath sees exactly
// what it saw before this existed.
const DefaultUIPath = "/dashboard/"

// SetUIPath serves the dashboard UI at a different prefix.
//
// WHY A HOST NEEDS THIS. The prefix used to be written into the route table, the bare-path redirect
// and the sign-in refusal, so a deployment serving the dashboard anywhere else had to fork
// dash/api.go — and forking a file to change a string is how a deployment ends up maintaining a
// copy of the whole package.
//
// IT MOVES THE MOUNT, IT DOES NOT ADD ONE. The default prefix stops existing: a host that chooses
// its own path gets exactly one, and the old one 404s. That is deliberate — two live prefixes for
// one page means a bookmark, a runbook and a link each naming a different URL, and no way to tell
// which is canonical. A host that wants the old path to redirect can register that itself in three
// lines; a host that wants it gone gets that by default.
//
// It must be called BEFORE Mount, and it is not safe once serving: the route table is read at mount
// time, and a prefix that changed underneath a running mux would leave the redirect and the mount
// disagreeing. Panics on a path that is not rooted and slash-terminated, because a half-formed
// prefix produces a dashboard whose relative asset references resolve one directory up — a blank
// page with no error, materially harder to diagnose than a panic at startup.
//
// The UI itself is prefix-agnostic by construction: index.html and the scripts reference siblings
// relatively, /api/* is a sibling of the mount rather than a child, and the asset-version rewriter
// matches bare quoted names. So moving the mount needs no change to any asset.
func (a *API) SetUIPath(prefix string) {
	if !strings.HasPrefix(prefix, "/") || !strings.HasSuffix(prefix, "/") || prefix == "/" {
		panic("dash: UI path must be rooted and slash-terminated, e.g. \"/dashboard-stats/\", got " +
			prefix)
	}
	a.uiPath = prefix
}

// uiPrefix is the mount, defaulting to DefaultUIPath so a zero-value API behaves as before.
func (a *API) uiPrefix() string {
	if a.uiPath == "" {
		return DefaultUIPath
	}
	return a.uiPath
}

// uiBare is the prefix without its trailing slash — the path the redirect answers on.
func (a *API) uiBare() string { return strings.TrimSuffix(a.uiPrefix(), "/") }
