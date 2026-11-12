package proxyapp

import (
	"flag"
	"net/http"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/proxy"
	"github.com/rossoctl/context-guru/tenant"
)

// Hooks are the points where a host adds its own parts to the standard proxy startup (Main).
//
// WHY THEY EXIST. Go cannot extend a package main, so a deployment that needed the standard proxy
// plus its own parts (SSO, a key store, its own routes) had to copy cmd/context-guru-proxy and
// edit the copy. Each later step added to that file then had to be copied by hand, and nothing
// warned when a copy was missed: one host booked $0 of keep-alive saving because it never called
// proxy.PingSavings. With the startup here, a new step reaches every host in one change, and a
// host keeps only its own parts.
//
// Every field is optional. The zero value is exactly the context-guru-proxy binary. A hook runs at
// one fixed point in the startup, named on its field; Main calls none of them more than once.
type Hooks struct {
	// Flags registers the host's own flags. It runs before flag.Parse, on the same flag set.
	Flags func(fs *flag.FlagSet)

	// Defaults changes the default of some shared flags. The flag and its environment variable
	// still win over it.
	Defaults Defaults

	// UIPath moves the dashboard from /dashboard/ to this path (dash.API.SetUIPath). "" keeps
	// /dashboard/. It must be rooted and slash-terminated; Main checks it before anything opens.
	UIPath string

	// BeforeControlDB runs in hosted mode, before the control database at path opens. An error
	// stops the start.
	BeforeControlDB func(path string) error

	// TenantOptions changes the registry options, just before the control database opens.
	TenantOptions func(opts *tenant.Options)

	// Hosted runs in hosted mode, just after the control database opens. app.Registry and
	// app.Upstreams are set; app.Tenants, app.Handler and app.Mux are not yet. An error stops the
	// start.
	Hosted func(app *App) error

	// ProxyOptions changes the proxy options, just before proxy.New.
	ProxyOptions func(opts *proxy.Options, app *App)

	// DashAuth wraps the dashboard authenticator in hosted mode.
	DashAuth func(core dash.Authenticator) dash.Authenticator

	// Mount adds routes to the proxy's mux, after it is built. Every App field is set.
	Mount func(mux *http.ServeMux, app *App)

	// Handler wraps the mux. It runs after Mount. The idle-exit activity stamp, when on, wraps
	// the result, and it still asks the mux which requests count as use: a request that matches
	// no mux route does not count, even when the wrapper answers it.
	Handler func(next http.Handler, app *App) http.Handler
}

// Defaults are the shared flag defaults a host may change.
type Defaults struct {
	// DashboardContent is the default of --dashboard-content (DASHBOARD_CONTENT).
	DashboardContent bool
}

// App is what the startup has built, for the hooks. A field is nil when the part is off: Recorder
// without --dashboard, Registry, Tenants and Upstreams outside hosted mode.
type App struct {
	Config    *config.Config
	Recorder  *dash.Recorder
	Registry  *tenant.Registry
	Tenants   *proxy.TenantSource
	Upstreams map[string]proxy.Upstream
	Windows   Resolver
	Prices    Pricer
	Handler   *proxy.Handler
	Mux       *http.ServeMux
}
