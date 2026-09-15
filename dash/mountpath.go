package dash

import "strings"

// DefaultUIPath is where the dashboard UI mounts when a host does not choose otherwise.
//
// Unchanged, and it has to be: it is in this project's README, in every existing bookmark, and in
// the refusal messages the API already emits. A deployment that never calls SetUIPath sees exactly
// what it saw before this existed.
const DefaultUIPath = "/dashboard/"

// SetUIPath moves the dashboard UI to a different prefix.
//
// WHY A HOST NEEDS THIS. A deployment that renames the dashboard — for a routing convention, to sit
// beside another tool, or simply because "/dashboard-stats/" reads better on its own service — has
// to fork dash/api.go today, because the prefix is written into the route table, the bare-path
// redirect and the sign-in refusal. Forking a file to change a string is how a deployment ends up
// maintaining a copy of the whole package.
//
// It must be called BEFORE Mount, and it is not safe to call once serving: the route table is read
// at mount time, and a prefix that changed underneath a running mux would leave the redirect and
// the mount disagreeing. Panics on a path that is not rooted and slash-terminated, because a
// half-formed prefix produces a dashboard whose relative asset references resolve one directory up
// — a blank page with no error, which is materially harder to diagnose than a panic at startup.
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
