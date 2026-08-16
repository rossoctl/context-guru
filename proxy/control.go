package proxy

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/tenant"
)

// The control plane's HTTP surface: registration, sign-in, a tenant's own settings and
// tokens, and the manager's view of everyone.
//
// It mounts under /api/ alongside the dashboard's read-only routes, because they are
// one product to the person using them — but the two have different auth: the dashboard
// reads with a COOKIE, and the proxy forwards traffic with a TOKEN. Keeping those
// separate is deliberate. Revoking a leaked CI token must not sign someone out of their
// browser, and signing in on a laptop must not mint something that can spend money.
//
// Every write here is a POST/PUT/PATCH/DELETE, and every one requires the cookie. A
// proxy token is deliberately NOT accepted for these routes: a token lives in agent
// config and CI environments, so accepting it for account changes would mean anything
// that can send traffic can also rewrite the account's configuration.

const (
	// dashCookie holds the browser session id.
	dashCookie = "cg_dash"
	// maxControlBody bounds a control-plane request. Generous for a config document,
	// small enough that an unauthenticated POST cannot cost us memory.
	maxControlBody = 256 << 10
)

// ctlScope is the authentication decision a control-plane route has made.
//
// It exists for the same reason dash's scopeClass does, and for a reason this file
// specifically needed: a review found the control plane had NO table-driven scope test
// while the dashboard's routes did, so a manager route added without its
// `if !t.IsManager()` line would have shipped silently. The class is now DATA — gate
// enforces it before the handler runs, and TestEveryControlRouteEnforcesItsScope walks
// this same table.
type ctlScope int

const (
	// ctlPublic is reachable with no session: registration, sign-in, and the
	// password-reset flow, which by definition is for someone who cannot sign in.
	ctlPublic ctlScope = iota
	// ctlTenant needs a browser session and acts on the caller's own account.
	ctlTenant
	// ctlManager additionally needs the manager role.
	ctlManager
)

// ctlRoute is one mounted control-plane endpoint plus its scope.
type ctlRoute struct {
	pattern string
	scope   ctlScope
	h       http.HandlerFunc
}

// ctlRoutes is the single mounted route table, read by MountControl and by the scope test.
func (h *Handler) ctlRoutes() []ctlRoute {
	return []ctlRoute{
		{"POST /api/register", ctlPublic, h.ctlRegister},
		{"POST /api/login", ctlPublic, h.ctlLogin},
		{"POST /api/verify", ctlPublic, h.ctlVerify},
		{"POST /api/logout", ctlPublic, h.ctlLogout},
		{"GET /api/me/sessions", ctlTenant, h.ctlSessions},
		{"DELETE /api/me/sessions/{id}", ctlTenant, h.ctlRevokeSession},
		{"GET /api/me", ctlTenant, h.ctlMe},
		{"PUT /api/me", ctlTenant, h.ctlUpdateMe},
		{"POST /api/me/tokens", ctlTenant, h.ctlMintToken},
		{"DELETE /api/me/tokens/{prefix}", ctlTenant, h.ctlRevokeToken},
		{"POST /api/me/agent-key", ctlTenant, h.ctlBindAgentKey},
		{"DELETE /api/me/agent-key", ctlTenant, h.ctlUnbindAgentKeys},
		{"GET /api/me/audit", ctlTenant, h.ctlAudit},
		{"GET /api/options", ctlTenant, h.ctlOptions},
		{"GET /api/tenants", ctlManager, h.ctlTenants},
		{"PATCH /api/tenants/{id}", ctlManager, h.ctlPatchTenant},
		{"POST /api/tenants/{id}/tokens", ctlManager, h.ctlManagerMintToken},
		// Feedback (feedback.go). Submitting is any signed-in account's own; reading is
		// manager-only INCLUDING the aggregate — "you said 2, the average is 4.4" is a
		// disclosure about other people's answers, so a user reads none of it.
		{"POST /api/feedback", ctlTenant, h.ctlSubmitFeedback},
		{"GET /api/feedback", ctlManager, h.ctlFeedback},
		// Manager control: everything below this line. See the section at the end of this
		// file.
		{"POST /api/me/password", ctlTenant, h.ctlChangePassword},
		{"POST /api/password-reset", ctlPublic, h.ctlRequestReset},
		{"POST /api/password-reset/verify", ctlPublic, h.ctlCompleteReset},
		{"POST /api/tenants/{id}/password-reset", ctlManager, h.ctlManagerReset},
		{"POST /api/tenants/{id}/purge", ctlManager, h.ctlPurgeTenant},
		{"DELETE /api/tenants/{id}", ctlManager, h.ctlDeleteTenant},
		{"GET /api/variants", ctlManager, h.ctlVariants},
	}
}

// MountControl registers the control-plane routes. Called only in hosted mode; without
// a tenant registry there are no accounts to manage.
func (h *Handler) MountControl(m *http.ServeMux) {
	if h.opts.Tenants == nil {
		return
	}
	for _, rt := range h.ctlRoutes() {
		m.HandleFunc(rt.pattern, h.gate(rt.scope, rt.h))
	}
}

// gate refuses a request that does not meet its route's declared scope, before the
// handler runs.
//
// The handlers still resolve the principal themselves — they need the tenant, for the
// audit trail and to act on. That is deliberate duplication rather than an oversight: the
// gate makes a route's class ENFORCED by the table rather than by whether whoever wrote
// the handler remembered, and a handler's own check keeps it correct if it is ever called
// from somewhere else. The cost is one extra cookie lookup per control-plane call, which
// is not a hot path.
func (h *Handler) gate(scope ctlScope, next http.HandlerFunc) http.HandlerFunc {
	if scope == ctlPublic {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		t, err := h.webPrincipal(r)
		if err != nil {
			code, msg := statusOf(err)
			ctlErr(w, code, msg)
			return
		}
		// The role, and ONLY the role. A crafted ?tenant= cannot reach this decision:
		// nothing here reads the query string, and the routes that act on another account
		// take its id from the PATH, which the registry then checks against the actor.
		if scope == ctlManager && !t.IsManager() {
			ctlErr(w, http.StatusForbidden, "manager only")
			return
		}
		next(w, r)
	}
}

// registry is the control-plane store, or nil in single-tenant mode.
func (h *Handler) registry() *tenant.Registry {
	if h.opts.Tenants == nil {
		return nil
	}
	return h.opts.Tenants.reg
}

// DashWhoami returns the body for /api/whoami: whether this is a hosted deployment and,
// if the caller is signed in, everything the UI needs to render itself — so the mode
// probe and the first data fetch are one round trip rather than two.
func (h *Handler) DashWhoami() func(*http.Request) any {
	return func(r *http.Request) any {
		// register tells the gate which of the three registration modes is in force, so
		// it can show the invite-code field only when a code is actually checked and
		// show no form at all when self-registration is closed. Sent unauthenticated
		// too — it is exactly the case that needs it — and it names the mode only,
		// never the code.
		out := map[string]any{
			"hosted": true, "authenticated": false, "register": registerMode(),
		}
		t, err := h.webPrincipal(r)
		if err != nil {
			return out
		}
		out["authenticated"] = true
		out["tenant"] = h.view(t)
		out["base_url"] = externalBase(r)
		if toks, err := h.registry().Tokens(t.ID); err == nil {
			out["tokens"] = tokenViews(toks)
		}
		return out
	}
}

// DashAuth returns the authenticator to hand dash.API.SetAuth, so the dashboard's
// read routes and these write routes agree on who the caller is. One resolver, so
// there is no way for the two halves to disagree about identity.
func (h *Handler) DashAuth() dash.Authenticator {
	return func(r *http.Request) (dash.Principal, bool) {
		t, err := h.webPrincipal(r)
		if err != nil {
			return dash.Principal{}, false
		}
		return dash.Principal{TenantID: t.ID, Manager: t.IsManager()}, true
	}
}

// webPrincipal resolves the browser cookie to a tenant.
//
// Cookie only, deliberately. Accepting a proxy token here would mean a token pasted
// into a CI environment could also read that account's transcripts and rewrite its
// settings — a token is for spending money on inference, not for administering an
// account.
func (h *Handler) webPrincipal(r *http.Request) (*tenant.Tenant, error) {
	reg := h.registry()
	if reg == nil {
		return nil, errNoToken
	}
	c, err := r.Cookie(dashCookie)
	if err != nil || c.Value == "" {
		return nil, errNoToken
	}
	t, err := reg.WebSession(c.Value)
	if err != nil {
		if errors.Is(err, tenant.ErrDisabled) {
			// Carrying the manager's reason: a user who was disabled mid-session finds out
			// here first, and "disabled" with no why is a support ticket.
			return nil, tenantOff(err)
		}
		return nil, errNoToken
	}
	return t, nil
}

// readJSON decodes a bounded, strict JSON body. Strict because a typo'd field in a
// settings save should be a visible error, not a silently ignored change the user
// believes they made.
//
// It also carries the cross-site guard, because every write route funnels through it —
// see checkOrigin. One guard in the shared decoder rather than one per handler, so a
// route added later is covered by construction.
func readJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if err := checkOrigin(r); err != nil {
		return err
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxControlBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// errCrossOrigin refuses a browser-driven cross-site write.
//
// Every write here is authenticated by the COOKIE, and the cookie's SameSite=Lax is not
// the boundary it looks like: SameSite's unit is the REGISTRABLE DOMAIN, so on a
// deployment under ibm.com any colleague's host under ibm.com is "same site" and the
// browser attaches the cookie. A form post needs no preflight either, and a
// `text/plain` body reaches a JSON decoder unimpeded (DisallowUnknownFields is happy as
// long as the form's `=` lands inside a string value). Nothing else stood in the way: a
// cross-origin post could mint a token on the victim's account or sign them out.
//
// The Origin header is the boundary, and it is enough on its own: a browser sets it on
// every write it makes, page script cannot forge it, and no CSRF token has to be
// plumbed through the UI to use it.
var errCrossOrigin = statusError{http.StatusForbidden,
	"cross-origin request refused: control-plane writes must come from this deployment's own pages"}

// checkOrigin rejects a request whose Origin is not this deployment's own.
//
// Present-and-different, NOT required-and-matching. A non-browser caller — curl, an
// agent, the tests — sends no Origin at all, and demanding one would break every one of
// them while adding nothing: the attack this closes is a BROWSER carrying someone
// else's cookie, and a browser always tells us where the page came from.
//
// The comparison is against externalBase(r), the same reconstruction /api/me hands the
// user as their base URL, so it is right on loopback, right behind nginx (which sets
// X-Forwarded-Proto and Host — see deploy/service/nginx.conf) and right if the hostname
// changes. An Origin of "null" (a sandboxed frame) matches nothing and is refused.
func checkOrigin(r *http.Request) error {
	o := r.Header.Get("Origin")
	if o == "" || strings.EqualFold(o, externalBase(r)) {
		return nil
	}
	return errCrossOrigin
}

// readErr writes the reply for a rejected request body: the status the error names — 403
// for a cross-site refusal — or 400 for a body that simply did not decode. One mapping
// for every route, so a refusal cannot present as a parse error on one endpoint and a
// 403 on the next.
func readErr(w http.ResponseWriter, err error) {
	if _, ok := err.(StatusError); ok {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	ctlErr(w, http.StatusBadRequest, "malformed request: "+err.Error())
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	// No caching of control-plane responses: they carry account state, and a cached
	// /api/me is how a shared browser shows one person another person's settings.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// msOrZero renders a time as epoch milliseconds, mapping the ZERO time to 0.
//
// time.Time{}.UnixMilli() is -62135596800000 (year 1), not 0 — so shipping it raw makes
// every "not set yet" timestamp a large negative number that reads as truthy in the UI.
// The symptom was every token rendering as revoked and "never used" showing a year-1
// date; the cause is this one conversion. dash's msTime does the same mapping on the way
// in, so this is the matching half.
func msOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func ctlErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// tenantView is what the UI is allowed to see about an account. Explicitly built
// rather than marshalling tenant.Tenant, so a field added to the struct is never
// exposed by accident.
type tenantView struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Email string `json:"email"`
	Role  string `json:"role"`
	// ConfigYAML is what this tenant has STORED — empty when they track the server
	// default. It is the value PUT /api/me and PATCH /api/tenants/{id} write back, so
	// a round trip through the settings page cannot accidentally turn tracking into a
	// frozen copy of today's default.
	ConfigYAML string `json:"config_yaml"`
	// EffectiveConfigYAML is what their traffic actually runs under, straight from
	// Registry.Config — the same resolver the proxy uses, not a second guess at it.
	// ConfigInherited says which of the two it came from, so the UI can show a
	// tracking tenant their real pipeline without claiming they chose it.
	EffectiveConfigYAML string `json:"effective_config_yaml"`
	ConfigInherited     bool   `json:"config_inherited"`
	UpAnthropic         string `json:"up_anthropic"`
	UpOpenAI            string `json:"up_openai"`
	UpBob               string `json:"up_bob"`
	CaptureContent      bool   `json:"capture_content"`
	MaxRows             int64  `json:"max_rows"`
	Disabled            bool   `json:"disabled"`
	// DisabledReason is the manager's note, shown to the account's owner as well as to a
	// manager — it is written to be read by the person whose agent stopped.
	DisabledReason string `json:"disabled_reason"`
	// Variant is the A/B group this account is in, "" for none.
	Variant string `json:"variant"`
	// HasPassword tells the settings page whether to ask for the CURRENT password. An
	// account that predates passwords has none to check, so it has to go through the
	// emailed reset instead — and a form demanding an old password it cannot have is a
	// dead end. The hash itself is never on this struct or in this payload.
	HasPassword bool    `json:"has_password"`
	CreatedAt   int64   `json:"created_at"`
	LastSeenAt  int64   `json:"last_seen_at"`
	SpentUSD    float64 `json:"spent_usd"`
	// AgentKeys is how many provider keys this account has bound (a COUNT, never a
	// digest): the settings page needs to say "bound" or "not bound", nothing more.
	AgentKeys int `json:"agent_keys"`
}

func (h *Handler) view(t *tenant.Tenant) tenantView {
	v := tenantView{
		ID: t.ID, Label: t.Label, Email: t.Email, Role: string(t.Role),
		ConfigYAML:          t.ConfigYAML,
		EffectiveConfigYAML: h.registry().Config(t),
		ConfigInherited:     t.TracksDefault(),
		UpAnthropic:         t.UpAnthropic, UpOpenAI: t.UpOpenAI, UpBob: t.UpBob,
		CaptureContent: t.CaptureContent,
		MaxRows:        t.MaxRows, Disabled: t.Disabled,
		DisabledReason: t.DisabledReason, Variant: t.Variant, HasPassword: t.HasPassword,
		CreatedAt: msOrZero(t.CreatedAt), LastSeenAt: msOrZero(t.LastSeenAt),
	}
	if n, err := h.registry().AgentKeyCount(t.ID); err == nil {
		v.AgentKeys = n
	}
	if h.opts.Spend != nil {
		// Best-effort: the settings page showing no spend is a cosmetic problem, and
		// failing the whole request over it would not be.
		if usd, err := h.opts.Spend.MonthToDateUSD(t.ID); err == nil {
			v.SpentUSD = usd
		}
	}
	return v
}

type tokenView struct {
	Prefix     string `json:"prefix"`
	Label      string `json:"label"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at"`
	RevokedAt  int64  `json:"revoked_at"`
}

func tokenViews(ts []tenant.Token) []tokenView {
	out := make([]tokenView, 0, len(ts))
	for _, t := range ts {
		out = append(out, tokenView{Prefix: t.Prefix, Label: t.Label,
			CreatedAt: msOrZero(t.CreatedAt), LastUsedAt: msOrZero(t.LastUsedAt),
			RevokedAt: msOrZero(t.RevokedAt)})
	}
	return out
}

// setSession issues the browser cookie.
func setSession(w http.ResponseWriter, r *http.Request, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:  dashCookie,
		Value: value,
		Path:  "/",
		// HttpOnly so no script can read it, SameSite=Lax so a cross-site form post
		// cannot act as this user. Secure whenever the request arrived over TLS —
		// checked via X-Forwarded-Proto too, because in the real deployment nginx
		// terminates TLS and the proxy itself only ever sees plain HTTP on loopback.
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
		Expires:  time.Now().Add(tenant.DefaultWebSessionTTL),
		MaxAge:   int(tenant.DefaultWebSessionTTL / time.Second),
	})
}

func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func clearSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: dashCookie, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: isHTTPS(r), MaxAge: -1,
	})
}

// --- handlers ---------------------------------------------------------------

// Registration gating.
//
// This gate used to default to CLOSED, and the reason it did no longer holds. Both
// halves of it changed:
//
//   - An account was a spending credential against the OPERATOR's upstream key, so N
//     accounts were N × cap of someone else's money. Users now forward their OWN
//     provider key, so a new account spends nothing that is not its owner's.
//   - "The domain check is not a control at all against anyone who can guess a valid
//     address" was true when nothing checked the address. Registration now REQUIRES a
//     code mailed to it, so an account exists only for someone who can read mail at
//     that address — which, combined with --register-domains, is the actual control
//     the old comment said was missing.
//
// So the default is now `open`: any colleague can self-serve, which is the point of a
// hosted service. `invite` and `closed` remain for a public port or a maintenance
// window. Read from the environment per request, like the upstream credentials, so an
// operator can change it without a restart.
//
//	CG_REGISTER=open   (default) — anyone whose email passes --register-domains, plus
//	                               a per-IP rate limit and mandatory email verification
//	CG_REGISTER=invite           — additionally requires the exact CG_REGISTER_CODE
//	CG_REGISTER=closed           — nothing can create an account. There is no
//	                               manager-side create route either, so a deployment
//	                               that starts closed is bootstrapped by opening
//	                               `invite` briefly.
//
// RESIDUAL RISK, stated plainly: an attacker with several real addresses in an allowed
// domain can still create several accounts — verification proves an address is
// reachable, never that its owner is entitled. What that costs is now bounded by rows
// on disk (--dashboard-max-rows-per-tenant) rather than by money. In `invite` mode the
// code is a shared secret with no per-use accounting: once leaked it is `open` until
// rotated.
const (
	envRegisterMode = "CG_REGISTER"
	envRegisterCode = "CG_REGISTER_CODE"
	// registrationsPerMinute bounds one address's attempts in `open` mode. Low
	// deliberately: a human registers once.
	registrationsPerMinute = 3
	// passwordAttemptsPerMinute bounds sign-in attempts. Applied to BOTH the email and
	// the client address, because either one alone is trivially sidestepped: per-email
	// only lets one host grind every account in the directory, per-IP only lets a
	// botnet grind one account.
	//
	// 5/min is the lockout. Deliberately not a sticky "account locked for 30 minutes":
	// that turns a rate limit into a denial-of-service anyone can aim at a colleague by
	// typing their address. A rolling window costs an attacker the same and costs the
	// real user one minute.
	passwordAttemptsPerMinute = 5
	// codeAttemptsPerMinute bounds code submissions. The primary control on a 6-digit
	// code is the per-code attempt cap in tenant.VerifyCode (5, then the code is
	// destroyed); this is the second layer, bounding how fast an attacker can burn
	// through fresh codes to get more attempts.
	codeAttemptsPerMinute = 10
)

// registerMode resolves the configured mode to one of "closed", "invite" or "open".
// One resolver, read by both the gate check below and /api/whoami, so the form the UI
// draws cannot disagree with the rule the server enforces — anything unrecognised is
// closed, which is also the default.
func registerMode() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envRegisterMode))) {
	case "invite":
		return "invite"
	case "closed":
		return "closed"
	default:
		// Unset, or anything unrecognised, is `open`. Unrecognised used to fall to
		// `closed`; it now falls to the default like every other setting in this
		// project, because "CG_REGISTER=Opne" silently disabling accounts is the same
		// class of bug as the banner/enforcement mismatch this resolver exists to fix.
		return "open"
	}
}

// RegisterMode is registerMode, exported for the startup banner.
//
// The banner used to switch on os.Getenv("CG_REGISTER") directly, which meant
// CG_REGISTER=Open (or " open", or "OPEN") logged "self-registration is off" while
// this file happily accepted registrations — a log line that disagrees with the
// enforcement is worse than none. One resolver, both callers.
func RegisterMode() string { return registerMode() }

// registrationAllowed reports whether this request may create an account.
func (h *Handler) registrationAllowed(r *http.Request, code string) error {
	switch registerMode() {
	case "open":
		// Bound per client address: registrantIP, not the raw RemoteAddr, so every port
		// a host connects from does not get its own budget — and so a deployment behind
		// its own nginx does not put every client in one bucket. See registrantIP.
		if _, err := h.regLim.Acquire(regBucket(registrantIP(r))); err != nil {
			return err
		}
		return nil
	case "invite":
		want := os.Getenv(envRegisterCode)
		if want == "" {
			// Configured for invites with no invite to check: refuse rather than fall
			// through to open, which would turn a half-finished configuration into the
			// hole this exists to close.
			return statusError{http.StatusForbidden,
				"registration is invite-only but no invite code is configured; ask the operator"}
		}
		if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(code)), []byte(want)) != 1 {
			return statusError{http.StatusForbidden, "a valid invite code is required to register"}
		}
		return nil
	default:
		return statusError{http.StatusForbidden,
			"self-registration is disabled on this deployment; ask the operator for an account"}
	}
}

// clientIP is the transport peer's address: RemoteAddr with the port removed, never a
// header. This is the address the loopback checks use (/stats, /metrics), and it is
// the only address that cannot be forged.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// registrantIP is the address to rate-limit a registration by.
//
// X-Forwarded-For is trusted from a LOOPBACK peer only, and from nobody else. The
// reasoning both ways:
//
//   - A remote caller cannot make RemoteAddr loopback, so it cannot reach this branch;
//     for it the header is ignored entirely and the bucket is its own address. Honouring
//     a remote client's header would hand it a fresh bucket per request.
//   - A loopback peer IS the reverse proxy on this host (deploy/service/nginx.conf,
//     which proxies to 127.0.0.1 and sets X-Forwarded-For). Ignoring the header there
//     put EVERY client of the real deployment in one bucket, which is both a
//     registration DoS for legitimate users at 3/min service-wide and no per-attacker
//     control at all — the limit failed at both of its jobs.
//
// nginx uses $proxy_add_x_forwarded_for, which APPENDS the peer it saw to whatever the
// client sent, so the LAST element is the one our proxy wrote and the earlier ones are
// client-supplied noise.
//
// Residual: a process on this host can forge the header and get unlimited buckets.
// Anything with local access can already read the control database, so that is not the
// boundary this defends.
func registrantIP(r *http.Request) string {
	host := clientIP(r)
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return host
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if last := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(last) != nil {
			return last
		}
	}
	return host
}

// regBucket is the rate-limit key for a client address: the address itself for IPv4,
// but the /64 PREFIX for IPv6.
//
// Per-address limiting is meaningless against IPv6: the smallest routed allocation
// anyone gets is a /64, which is 2^64 source addresses and therefore 2^64 buckets for
// free. Keying the prefix makes one allocation one budget, which is what "per client"
// was supposed to mean. Not used for the loopback checks (see clientIP callers in
// tenancy.go) — those need the literal address.
//
// ponytail: /64 for IPv6 and /32 exact for IPv4; if a single ISP's IPv4 range is ever
// the abuse source, widen to a /24 rather than adding a general CIDR config.
func regBucket(host string) string {
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() != nil {
		return host
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// Two-phase authentication, both flows.
//
//	REGISTER  POST /api/register {email, password}  → account created, code mailed
//	          POST /api/verify   {email, code}      → verified, first token, signed in
//	LOGIN     POST /api/login    {email, password}  → password checked, code mailed
//	          POST /api/verify   {email, code}      → signed in
//
// The two phases exist for different reasons, which is why they are not one endpoint.
// On registration the code proves the ADDRESS is real and reachable. On login it is a
// second FACTOR: something the user receives, on top of something they know. In both
// cases phase one issues NO cookie — a session appears only after phase two, so
// knowing a password is never by itself a signed-in browser.
//
// /api/verify handles both because the pending row's purpose already says which flow
// this is; letting the CLIENT name the purpose would let it ask for the register path
// (which mints a token) while holding a login code.

// codeSent is phase one's reply. It carries the absolute expiry so the UI can render a
// countdown against the server's clock rather than assuming five minutes from whenever
// its own timer started, and so a slow mail delivery does not show a wrong number.
func codeSent(w http.ResponseWriter, email string, exp time.Time, next string) {
	writeJSON(w, http.StatusOK, map[string]any{
		"next":              next,
		"email":             email,
		"code_expires_at":   msOrZero(exp),
		"code_valid_secs":   int(tenant.CodeTTL.Seconds()),
		"code_max_attempts": tenant.MaxCodeAttempts,
	})
}

// ctlRegister creates an UNVERIFIED account and mails it a code. No token, no session:
// see VerifyRegistration for why those wait.
func (h *Handler) ctlRegister(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Label    string `json:"label"`
		Email    string `json:"email"`
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := readJSON(w, r, &in); err != nil {
		readErr(w, err)
		return
	}
	// After the body is read (the invite code is in it), before anything is created.
	if err := h.registrationAllowed(r, in.Code); err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	if in.Label == "" {
		in.Label = "laptop"
	}
	t, err := h.registry().RegisterAccount(in.Label, in.Email, in.Password)
	if err != nil {
		switch {
		case errors.Is(err, tenant.ErrEmailTaken):
			// NOT a 409. This route is unauthenticated, so an answer that depends on
			// whether the address already has an account tells anyone who asks which of
			// their colleagues are signed up — the one channel ctlLogin and ctlVerify were
			// deliberately built to close (one message for unknown/wrong/no-password, and
			// VerifyLogin spends argon2 against a decoy hash so even the timings match).
			//
			// So the reply is the same 201 "check your mail" as a fresh registration, and
			// the difference goes into the MAILBOX, which only the owner can read: they get
			// a notice that someone tried, and no code. The timing matches too, because
			// RegisterAccount hashes the password before it discovers the address is taken.
			if mErr := mailRegisterNotice(normalEmail(in.Email)); mErr != nil {
				code, msg := statusOf(mErr)
				ctlErr(w, code, msg)
				return
			}
			// The expiry is the one the code we did NOT issue would have carried. A zero
			// here would be the tell the status no longer is.
			registered(w, normalEmail(in.Email), time.Now().Add(tenant.CodeTTL))
		case errors.Is(err, tenant.ErrEmailDomain):
			ctlErr(w, http.StatusForbidden, "that email domain is not allowed to register here")
		case errors.Is(err, tenant.ErrBadPassword):
			ctlErr(w, http.StatusBadRequest, fmt.Sprintf(
				"choose a password of at least %d characters", tenant.MinPasswordLen))
		case errors.Is(err, tenant.ErrBadEmail), errors.Is(err, tenant.ErrBadLabel):
			ctlErr(w, http.StatusBadRequest, err.Error())
		default:
			ctlErr(w, http.StatusInternalServerError, "could not register")
		}
		return
	}
	exp, err := h.mailCode(t, tenant.PurposeRegister)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	registered(w, t.Email, exp)
}

// registered is ctlRegister's reply, and the reason it is a function is that it must be
// byte-identical on both paths through that handler — a taken address and a free one
// (see the ErrEmailTaken branch above).
func registered(w http.ResponseWriter, email string, exp time.Time) {
	writeJSON(w, http.StatusCreated, map[string]any{
		"next":              "verify",
		"email":             email,
		"code_expires_at":   msOrZero(exp),
		"code_valid_secs":   int(tenant.CodeTTL.Seconds()),
		"code_max_attempts": tenant.MaxCodeAttempts,
	})
}

// normalEmail is the address as the registry stores it, so the reply and the notice
// name it the same way the success path does (which echoes tenant.Email).
func normalEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// mailRegisterNotice tells an address's OWNER that someone tried to register it. There
// is no code and nothing to act on: the point is that the "this address has an account"
// fact appears only in a mailbox the person asking cannot read.
func mailRegisterNotice(to string) error {
	err := sendMail(to, "Someone tried to register your context-guru account",
		`Someone just tried to create a context-guru account with this email address.

Nothing was created and no verification code was issued — this address already has an
account. If it was you, sign in instead; if you have lost the password, ask the operator.

If it was not you, there is nothing to do. Whoever tried learned nothing about this
account, including whether it exists.
`)
	if err == nil {
		return nil
	}
	// Mapped exactly as mailCode maps a send failure, because a mail outage must not
	// answer differently on the two paths either — that would put the oracle back.
	return mailFailed(err)
}

// mailCode issues a code and hands it to the mailer. The plaintext code exists only
// inside this function's callee — it is not returned, so no handler above can leak it
// into a response body, and it is not logged.
func (h *Handler) mailCode(t *tenant.Tenant, p tenant.CodePurpose) (time.Time, error) {
	c, err := h.registry().IssueCode(t.ID, p)
	if err != nil {
		return time.Time{}, statusError{http.StatusInternalServerError, "could not issue a code"}
	}
	if err := sendCode(t.Email, p, c); err != nil {
		return time.Time{}, mailFailed(err)
	}
	return c.ExpiresAt, nil
}

// mailFailed maps a send failure to the status the caller should see. One mapping, two
// callers (mailCode and mailRegisterNotice) — see the ErrEmailTaken branch of
// ctlRegister for why the two must agree.
func mailFailed(err error) error {
	// Logged WITHOUT the address's code and without the recipient's mailbox contents;
	// the relay's own message is the useful half.
	slog.Warn("context-guru: verification mail failed", "err", err.Error())
	if _, ok := err.(StatusError); ok {
		return err
	}
	return statusError{http.StatusBadGateway,
		"could not send the verification email; try again shortly"}
}

// ctlLogin is phase one of signing in: email + password, then a code in the mail.
//
// It also still accepts a bare proxy TOKEN, which is how every account created before
// passwords existed signs in. That path is unchanged and single-factor by necessity —
// there is nothing to mail a code to that the token holder has not already proved.
// Accounts with a password cannot use it; see below.
func (h *Handler) ctlLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token    string `json:"token"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := readJSON(w, r, &in); err != nil {
		readErr(w, err)
		return
	}
	if tok := strings.TrimSpace(in.Token); tok != "" {
		h.loginWithToken(w, r, tok)
		return
	}

	// Rate limit BEFORE the argon2 verify, not after: the KDF costs 64 MiB and ~50 ms
	// by design, so an unbounded endpoint that runs it is a memory-and-CPU amplifier as
	// well as a password oracle. Keyed on the email and on the client address, and both
	// are charged even when the address is unknown — a limiter that only counts real
	// accounts tells an attacker which addresses are real.
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if err := h.spendAuthAttempt(h.pwLim, email, r); err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	t, err := h.registry().VerifyLogin(email, in.Password)
	if err != nil {
		switch {
		case errors.Is(err, tenant.ErrDisabled):
			ctlErr(w, http.StatusForbidden, disabledMsg(err))
		case errors.Is(err, tenant.ErrNotVerified):
			// Correct password on an unverified account: re-send the REGISTRATION code
			// rather than telling them to start over. Safe to do here and nowhere else —
			// this branch is only reachable by someone who got the password right.
			exp, mErr := h.mailCode(t, tenant.PurposeRegister)
			if mErr != nil {
				code, msg := statusOf(mErr)
				ctlErr(w, code, msg)
				return
			}
			codeSent(w, t.Email, exp, "verify")
		default:
			// One message for unknown address, wrong password, and no-password-set, so
			// this endpoint cannot enumerate accounts.
			ctlErr(w, http.StatusUnauthorized, "wrong email or password")
		}
		return
	}
	exp, err := h.mailCode(t, tenant.PurposeLogin)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	codeSent(w, t.Email, exp, "verify")
}

// loginWithToken is the legacy token → session exchange, kept because agents' tokens
// are also how a pre-password account gets into the dashboard at all.
func (h *Handler) loginWithToken(w http.ResponseWriter, r *http.Request, token string) {
	t, err := h.registry().Resolve(token)
	if err != nil {
		if errors.Is(err, tenant.ErrDisabled) {
			ctlErr(w, http.StatusForbidden, disabledMsg(err))
			return
		}
		// One message for both "no such token" and "revoked", so this endpoint cannot
		// be used to enumerate which tokens exist.
		ctlErr(w, http.StatusUnauthorized, "that token is not valid")
		return
	}
	// An account WITH a password must use it. Otherwise the second factor is optional
	// for anyone holding a token — which is to say there is no second factor, since a
	// token is the credential most likely to be sitting in a CI log.
	if t.HasPassword {
		ctlErr(w, http.StatusForbidden,
			"this account has a password: sign in with your email and password instead")
		return
	}
	cookie, err := h.registry().OpenWebSession(t.ID, h.sessionMeta(r, t.Label), 0)
	if err != nil {
		ctlErr(w, http.StatusInternalServerError, "could not open a session")
		return
	}
	setSession(w, r, cookie)
	writeJSON(w, http.StatusOK, map[string]any{"tenant": h.view(t)})
}

// ctlVerify is phase two of both flows: it spends the mailed code and opens the
// session. Which flow it is depends on the pending code's purpose, not on anything the
// client says.
func (h *Handler) ctlVerify(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string `json:"email"`
		Code  string `json:"code"`
	}
	if err := readJSON(w, r, &in); err != nil {
		readErr(w, err)
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if err := h.spendAuthAttempt(h.codeLim, email, r); err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	t, err := h.registry().ByEmail(email)
	if err != nil {
		// Same shape and status as a wrong code: whether an address has a pending
		// challenge is not something this endpoint should confirm.
		ctlErr(w, http.StatusUnauthorized, "that code is not valid")
		return
	}
	code := strings.TrimSpace(in.Code)

	// Registration first: an account that is not verified yet has no login purpose to
	// serve, and this is the branch that mints its token.
	if !t.Verified() {
		vt, plain, err := h.registry().VerifyRegistration(t.ID, code)
		if err != nil {
			writeCodeErr(w, err)
			return
		}
		h.signIn(w, r, vt)
		// The ONLY time a plaintext token crosses this boundary. It is not stored and
		// cannot be recovered; the UI must show it once and say so.
		writeJSON(w, http.StatusCreated, map[string]any{
			"tenant": h.view(vt),
			"token":  plain,
			"warning": "This token is shown once and cannot be recovered. " +
				"Store it now; if you lose it, mint a new one from Settings.",
		})
		return
	}
	if err := h.registry().VerifyCode(t.ID, tenant.PurposeLogin, code); err != nil {
		writeCodeErr(w, err)
		return
	}
	if t.Disabled {
		ctlErr(w, http.StatusForbidden, disabledMsg(&tenant.DisabledError{Reason: t.DisabledReason}))
		return
	}
	h.signIn(w, r, t)
	writeJSON(w, http.StatusOK, map[string]any{"tenant": h.view(t)})
}

// writeCodeErr maps a code failure to a status and a message the user can act on.
// "expired" and "void" are distinguished from "wrong" deliberately: both mean START
// AGAIN, and a user who is told only "wrong code" retypes the same dead code until
// they give up. This leaks nothing an attacker does not already know — they can see
// the clock and they counted their own guesses.
func writeCodeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, tenant.ErrCodeExpired):
		ctlErr(w, http.StatusUnauthorized,
			"that code has expired — sign in again to get a new one")
	case errors.Is(err, tenant.ErrCodeAttempts):
		ctlErr(w, http.StatusUnauthorized,
			"too many wrong codes; that code is now void — sign in again to get a new one")
	case errors.Is(err, tenant.ErrNoCode), errors.Is(err, tenant.ErrBadCode):
		ctlErr(w, http.StatusUnauthorized, "that code is not valid")
	default:
		ctlErr(w, http.StatusInternalServerError, "could not verify that code")
	}
}

// signIn opens a session for a tenant and sets the cookie.
func (h *Handler) signIn(w http.ResponseWriter, r *http.Request, t *tenant.Tenant) {
	cookie, err := h.registry().OpenWebSession(t.ID, h.sessionMeta(r, t.Label), 0)
	if err != nil {
		ctlErr(w, http.StatusInternalServerError, "could not open a session")
		return
	}
	setSession(w, r, cookie)
}

// sessionMeta records what machine a login came from. registrantIP, not RemoteAddr, so
// the address shown is the client's rather than nginx's on every row.
func (h *Handler) sessionMeta(r *http.Request, label string) tenant.SessionMeta {
	return tenant.SessionMeta{
		Label:     label,
		UserAgent: r.UserAgent(),
		IP:        registrantIP(r),
	}
}

// spendAuthAttempt charges one attempt against BOTH the email bucket and the client
// address bucket. Either alone is trivially sidestepped — see
// passwordAttemptsPerMinute — so a refusal from either one refuses the request.
//
// The limiter's own message says "for this account", which is wrong for an IP bucket
// and would also confirm that an address IS an account, so it is replaced here with
// one message for both cases.
func (h *Handler) spendAuthAttempt(lim *Limiter, email string, r *http.Request) error {
	for _, key := range []string{"email:" + email, "ip:" + regBucket(registrantIP(r))} {
		if _, err := lim.Acquire(key); err != nil {
			return statusError{http.StatusTooManyRequests,
				"too many attempts; wait a minute and try again"}
		}
	}
	return nil
}

// ctlSessions lists the caller's signed-in machines.
func (h *Handler) ctlSessions(w http.ResponseWriter, r *http.Request) {
	t, err := h.webPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	cookie, _ := r.Cookie(dashCookie)
	var cur string
	if cookie != nil {
		cur = cookie.Value
	}
	ss, err := h.registry().Sessions(t.ID, cur)
	if err != nil {
		ctlErr(w, http.StatusInternalServerError, "could not list sessions")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessionViews(ss)})
}

type sessionView struct {
	// ID is the public handle, the only thing needed to revoke a session — and not
	// enough to use one.
	ID         string `json:"id"`
	Label      string `json:"label"`
	UserAgent  string `json:"user_agent"`
	IP         string `json:"ip"`
	CreatedAt  int64  `json:"created_at"`
	LastSeenAt int64  `json:"last_seen_at"`
	ExpiresAt  int64  `json:"expires_at"`
	Current    bool   `json:"current"`
}

func sessionViews(ss []tenant.Session) []sessionView {
	out := make([]sessionView, 0, len(ss))
	for _, s := range ss {
		out = append(out, sessionView{ID: s.SID, Label: s.Label, UserAgent: s.UserAgent,
			IP: s.IP, CreatedAt: msOrZero(s.CreatedAt), LastSeenAt: msOrZero(s.LastSeenAt),
			ExpiresAt: msOrZero(s.ExpiresAt), Current: s.Current})
	}
	return out
}

// ctlRevokeSession signs ONE machine out — including, deliberately, the one asking.
// "Sign out everywhere except here" is a thing users want, and a user who revokes
// their current session has simply signed out.
func (h *Handler) ctlRevokeSession(w http.ResponseWriter, r *http.Request) {
	t, err := h.webPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	sid := r.PathValue("id")
	if err := h.registry().EndWebSessionBySID(t.ID, sid); err != nil {
		if errors.Is(err, tenant.ErrNoSession) {
			ctlErr(w, http.StatusNotFound, "no such session")
			return
		}
		ctlErr(w, http.StatusInternalServerError, "could not revoke that session")
		return
	}
	if c, cErr := r.Cookie(dashCookie); cErr == nil && tenant.SID(c.Value) == sid {
		clearSession(w, r)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func (h *Handler) ctlLogout(w http.ResponseWriter, r *http.Request) {
	// The one write that reads no body, so readJSON's cross-site guard does not cover it
	// — and a forced sign-out is exactly what a cross-origin form post can aim for.
	if err := checkOrigin(r); err != nil {
		readErr(w, err)
		return
	}
	if c, err := r.Cookie(dashCookie); err == nil {
		_ = h.registry().EndWebSession(c.Value)
	}
	clearSession(w, r)
	writeJSON(w, http.StatusOK, map[string]string{"status": "signed out"})
}

// ctlMe returns the caller's account, tokens, and the setup snippets for their agents.
func (h *Handler) ctlMe(w http.ResponseWriter, r *http.Request) {
	t, err := h.webPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	toks, err := h.registry().Tokens(t.ID)
	if err != nil {
		ctlErr(w, http.StatusInternalServerError, "could not list tokens")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant": h.view(t),
		"tokens": tokenViews(toks),
		// The base URL the user should configure. Derived from the request rather than
		// configured, so it is right behind nginx, right on loopback, and right if the
		// hostname changes — one fewer thing to keep in step with reality.
		"base_url": externalBase(r),
	})
}

// externalBase reconstructs the URL a user's agent should point at, honouring the
// reverse proxy's forwarded headers.
func externalBase(r *http.Request) string {
	scheme := "http"
	if isHTTPS(r) {
		scheme = "https"
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

// checkUpstreams validates upstream NAMES against the operator's allow-list, at WRITE
// time as well as at request time. Not an SSRF boundary — a name is never a URL, and
// upstreamFor looks it up in this same map — but a stored name the proxy will later
// refuse breaks someone's agent with no feedback at the point the change was made, and
// the person who broke it may not be the person whose agent stops working (see
// ctlPatchTenant, which is a manager editing somebody else).
//
// One check, both writers. The user's own save had it; the manager's patch did not.
func (h *Handler) checkUpstreams(names ...*string) error {
	for _, name := range names {
		if name == nil || *name == "" {
			continue
		}
		if _, ok := h.opts.Upstreams[*name]; !ok {
			return statusError{http.StatusBadRequest, "unknown upstream " + *name +
				"; pick one of the names from /api/options"}
		}
	}
	return nil
}

// ctlUpdateMe saves the caller's own settings.
func (h *Handler) ctlUpdateMe(w http.ResponseWriter, r *http.Request) {
	t, err := h.webPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	// Pointers so "not sent" and "set to empty/false" are different things — a settings
	// form that omits a field must not silently clear it.
	var in struct {
		Label          *string `json:"label"`
		ConfigYAML     *string `json:"config_yaml"`
		UpAnthropic    *string `json:"up_anthropic"`
		UpOpenAI       *string `json:"up_openai"`
		UpBob          *string `json:"up_bob"`
		CaptureContent *bool   `json:"capture_content"`
	}
	if err := readJSON(w, r, &in); err != nil {
		readErr(w, err)
		return
	}
	if err := h.checkUpstreams(in.UpAnthropic, in.UpOpenAI, in.UpBob); err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	patch := tenant.Patch{Label: in.Label, ConfigYAML: in.ConfigYAML,
		UpAnthropic: in.UpAnthropic, UpOpenAI: in.UpOpenAI, UpBob: in.UpBob,
		CaptureContent: in.CaptureContent}
	if err := h.registry().Update(t, t.ID, patch); err != nil {
		if errors.Is(err, tenant.ErrForbidden) {
			ctlErr(w, http.StatusForbidden, "not permitted")
			return
		}
		// A rejected configuration is the common case here, and its message names the
		// offending key — worth passing through verbatim.
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := h.registry().Get(t.ID)
	if err != nil {
		ctlErr(w, http.StatusInternalServerError, "saved, but could not re-read the account")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenant": h.view(updated)})
}

func (h *Handler) ctlMintToken(w http.ResponseWriter, r *http.Request) {
	t, err := h.webPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	var in struct {
		Label string `json:"label"`
	}
	if err := readJSON(w, r, &in); err != nil {
		readErr(w, err)
		return
	}
	if in.Label == "" {
		in.Label = "new-token"
	}
	plain, err := h.registry().MintToken(t.ID, in.Label)
	if err != nil {
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":   plain,
		"warning": "Shown once and not recoverable.",
	})
}

func (h *Handler) ctlRevokeToken(w http.ResponseWriter, r *http.Request) {
	t, err := h.webPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	if err := h.registry().RevokeToken(t.ID, r.PathValue("prefix")); err != nil {
		ctlErr(w, http.StatusNotFound, "no such live token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// ctlBindAgentKey binds the sha256 of the caller's own provider key to their account,
// so an agent that can set no custom header (Bob/BobShell: its client builds every
// header itself) is still identified by the credential it does send.
//
// The key arrives in an AUTH HEADER, not the body — it is the same slot the agent
// itself uses, so the value can be piped straight from the environment
// (-H "Authorization: Bearer $BOBSHELL_API_KEY") instead of being pasted somewhere it
// gets logged. It is hashed by the registry and never stored, echoed, or logged.
func (h *Handler) ctlBindAgentKey(w http.ResponseWriter, r *http.Request) {
	t, err := h.webPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	key := CallerKey(r)
	if key == "" {
		ctlErr(w, http.StatusBadRequest,
			"send the provider key you want bound in Authorization or x-api-key")
		return
	}
	if err := h.registry().BindAgentKey(t.ID, key); err != nil {
		// A registry refusal here is usually the USER's to fix, and answering 500 for all
		// of them tells them nothing and points them at the operator instead.
		//
		// ErrForbidden is the anti-hijack case: this digest is already bound to a DIFFERENT
		// account. Binding it used to transfer it silently, which handed the new binder the
		// other account's traffic — so the refusal has to say who can undo it.
		//
		// ErrBadAgentKey is the length floor, and the message must not imply the key is
		// malformed: a short key can be perfectly valid at its provider and merely too
		// short for us to accept as an IDENTITY, because identity here is the key's
		// digest — so a guessable key is a guessable account.
		switch {
		case errors.Is(err, tenant.ErrBadAgentKey):
			ctlErr(w, http.StatusBadRequest, fmt.Sprintf("that provider key is shorter than %d "+
				"characters; context-guru identifies this agent by the digest of its key, so a "+
				"short key would be guessable. Use a longer key, or send the "+
				"x-context-guru-token header from an agent that can set one",
				tenant.MinAgentKeyLen))
		case errors.Is(err, tenant.ErrForbidden):
			ctlErr(w, http.StatusForbidden, "that provider key is already bound to another "+
				"account; its owner has to unbind it first")
		case errors.Is(err, tenant.ErrNotFound):
			ctlErr(w, http.StatusNotFound, "no such account")
		default:
			ctlErr(w, http.StatusInternalServerError, "could not bind the key")
		}
		return
	}
	n, _ := h.registry().AgentKeyCount(t.ID)
	writeJSON(w, http.StatusOK, map[string]any{"status": "bound", "agent_keys": n})
}

// ctlUnbindAgentKeys drops every key bound to the account. All of them, because the
// digests are not displayable and "which one" is not a question the user can answer.
func (h *Handler) ctlUnbindAgentKeys(w http.ResponseWriter, r *http.Request) {
	t, err := h.webPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	if err := h.registry().UnbindAgentKeys(t.ID); err != nil {
		ctlErr(w, http.StatusInternalServerError, "could not unbind")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "unbound", "agent_keys": 0})
}

func (h *Handler) ctlAudit(w http.ResponseWriter, r *http.Request) {
	t, err := h.webPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	target := t.ID
	// A manager may read anyone's trail, including the service-wide one.
	if t.IsManager() {
		if q := r.URL.Query().Get("tenant"); q != "" {
			target = q
			if q == "*" {
				target = ""
			}
		}
	}
	entries, err := h.registry().Audit(target, 200)
	if err != nil {
		ctlErr(w, http.StatusInternalServerError, "could not read the audit log")
		return
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{
			"at": msOrZero(e.At), "actor": e.Actor, "target": e.Target,
			"field": e.Field, "before": e.Before, "after": e.After,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"audit": out})
}

// ctlOptions tells the UI what choices exist: which upstreams the operator allows and
// which presets and components are registered. Served so the settings page can never
// offer something the server would reject.
func (h *Handler) ctlOptions(w http.ResponseWriter, r *http.Request) {
	if _, err := h.webPrincipal(r); err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	type up struct {
		Name    string `json:"name"`
		Dialect string `json:"dialect"`
	}
	ups := make([]up, 0, len(h.opts.Upstreams))
	for name, u := range h.opts.Upstreams {
		// The base URL and the credential's env var are the operator's business, not a
		// tenant's: a name is all anyone needs to choose one.
		ups = append(ups, up{Name: name, Dialect: u.Dialect})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"upstreams":      ups,
		"presets":        h.opts.PresetNames,
		"components":     h.opts.ComponentNames,
		"default_config": tenant.DefaultConfigYAML,
	})
}

// ctlTenants is the manager's roster.
func (h *Handler) ctlTenants(w http.ResponseWriter, r *http.Request) {
	t, err := h.webPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	if !t.IsManager() {
		ctlErr(w, http.StatusForbidden, "manager only")
		return
	}
	all, err := h.registry().List()
	if err != nil {
		ctlErr(w, http.StatusInternalServerError, "could not list tenants")
		return
	}
	out := make([]tenantView, 0, len(all))
	for _, x := range all {
		out = append(out, h.view(x))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenants": out})
}

// ctlPatchTenant is the manager's edit. The registry enforces which fields a manager
// may change; this handler only translates.
func (h *Handler) ctlPatchTenant(w http.ResponseWriter, r *http.Request) {
	actor, err := h.webPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	var in struct {
		Label          *string `json:"label"`
		Role           *string `json:"role"`
		ConfigYAML     *string `json:"config_yaml"`
		UpAnthropic    *string `json:"up_anthropic"`
		UpOpenAI       *string `json:"up_openai"`
		UpBob          *string `json:"up_bob"`
		CaptureContent *bool   `json:"capture_content"`
		MaxRows        *int64  `json:"max_rows"`
		Disabled       *bool   `json:"disabled"`
		// The A/B group, and the note the account's owner reads when they are shut off.
		// Both manager-only in the registry, like role and quota.
		Variant        *string `json:"variant"`
		DisabledReason *string `json:"disabled_reason"`
	}
	if err := readJSON(w, r, &in); err != nil {
		readErr(w, err)
		return
	}
	// Same allow-list check as PUT /api/me: a manager who parks a tenant on an upstream
	// name the proxy refuses breaks that tenant's agent, and the tenant is the one who
	// finds out.
	if err := h.checkUpstreams(in.UpAnthropic, in.UpOpenAI, in.UpBob); err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	patch := tenant.Patch{Label: in.Label, ConfigYAML: in.ConfigYAML,
		UpAnthropic: in.UpAnthropic, UpOpenAI: in.UpOpenAI, UpBob: in.UpBob,
		CaptureContent: in.CaptureContent,
		MaxRows:        in.MaxRows, Disabled: in.Disabled,
		Variant: in.Variant, DisabledReason: in.DisabledReason}
	if in.Role != nil {
		role := tenant.Role(*in.Role)
		patch.Role = &role
	}
	target := r.PathValue("id")
	if err := h.registry().Update(actor, target, patch); err != nil {
		switch {
		case errors.Is(err, tenant.ErrForbidden):
			ctlErr(w, http.StatusForbidden, "manager only")
		case errors.Is(err, tenant.ErrNotFound):
			ctlErr(w, http.StatusNotFound, "no such tenant")
		default:
			ctlErr(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	updated, err := h.registry().Get(target)
	if err != nil {
		ctlErr(w, http.StatusInternalServerError, "saved, but could not re-read the tenant")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenant": h.view(updated)})
}

// ctlManagerMintToken reissues a token for a tenant who lost theirs — the recovery
// path, since tokens are stored hashed and cannot be recovered.
func (h *Handler) ctlManagerMintToken(w http.ResponseWriter, r *http.Request) {
	actor, err := h.webPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	if !actor.IsManager() {
		ctlErr(w, http.StatusForbidden, "manager only")
		return
	}
	var in struct {
		Label string `json:"label"`
	}
	if err := readJSON(w, r, &in); err != nil {
		readErr(w, err)
		return
	}
	if in.Label == "" {
		in.Label = "reissued"
	}
	plain, err := h.registry().MintToken(r.PathValue("id"), in.Label)
	if err != nil {
		if errors.Is(err, tenant.ErrNotFound) {
			ctlErr(w, http.StatusNotFound, "no such tenant")
			return
		}
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":   plain,
		"warning": "Shown once. Hand it to the user over a channel you trust.",
	})
}

// --- manager control -------------------------------------------------------
//
// A manager can already read everyone's metrics and edit everyone's configuration
// (ctlTenants, ctlPatchTenant). This section adds the rest of what "full control of all
// users" means, and the boundary it does NOT cross.
//
// What a manager gets: every account's configuration, an A/B grouping over the metrics
// that already exist, disable/enable with a reason the user can read, storage purge,
// account deletion across both databases and cold storage, and the ability to START a
// password reset.
//
// What a manager never gets: a tenant's captured transcript text (dash's request and
// archive routes strip Content for anyone who is not the row's owner), and a tenant's
// password. The reset route mails the OWNER a code and returns nothing — a manager who
// could set a password could read that account's transcripts by signing in as them, which
// is the boundary this whole design exists to keep.

// disabledMsg renders the sign-in refusal for a disabled account, with the manager's
// reason when there is one. Shares tenantOff's wording so an agent's 403 and a browser's
// refusal say the same thing.
func disabledMsg(err error) string {
	var de *tenant.DisabledError
	if errors.As(err, &de) && de.Reason != "" {
		return "this account is disabled: " + de.Reason
	}
	return "this account is disabled"
}

// ctlChangePassword changes the caller's OWN password. The current one is required — see
// tenant.ChangePassword for why a live session is not enough.
func (h *Handler) ctlChangePassword(w http.ResponseWriter, r *http.Request) {
	t, err := h.webPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	var in struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := readJSON(w, r, &in); err != nil {
		readErr(w, err)
		return
	}
	// Bounded like every other route that runs argon2: this one verifies the old password,
	// so unbounded it is both a 64 MiB-per-attempt amplifier and an offline-strength guess
	// oracle for anyone holding a stolen cookie.
	if err := h.spendAuthAttempt(h.pwLim, t.Email, r); err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	// The caller's own cookie is kept signed in; every other machine is signed out.
	var cur string
	if c, cErr := r.Cookie(dashCookie); cErr == nil {
		cur = c.Value
	}
	switch err := h.registry().ChangePassword(t.ID, cur, in.OldPassword, in.NewPassword); {
	case err == nil:
	case errors.Is(err, tenant.ErrWrongPass):
		ctlErr(w, http.StatusUnauthorized, "that is not your current password")
		return
	case errors.Is(err, tenant.ErrNoPassword):
		// An account from before passwords existed. There is nothing to check the old value
		// against, so point at the flow that proves the ADDRESS instead of the one that
		// proves a password they never set.
		ctlErr(w, http.StatusBadRequest, "this account has no password yet — use "+
			"\"Forgot your password\" on the sign-in page to set one by email")
		return
	case errors.Is(err, tenant.ErrBadPassword):
		ctlErr(w, http.StatusBadRequest, fmt.Sprintf(
			"choose a password of at least %d characters", tenant.MinPasswordLen))
		return
	default:
		ctlErr(w, http.StatusInternalServerError, "could not change the password")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "changed",
		"note":   "Your other signed-in machines have been signed out.",
	})
}

// ctlRequestReset starts the emailed password reset. Unauthenticated by necessity: the
// person who needs it is the person who cannot sign in, and the absence of any
// self-service recovery is what made an earlier lockout bug unrecoverable.
//
// The reply is IDENTICAL whether or not the address has an account — same status, same
// fields, same expiry (computed rather than read, exactly as ctlRegister's taken-address
// branch does) — because this endpoint is otherwise a directory of who works here.
//
// RESIDUAL RISK, stated plainly: an existing address makes an SMTP round trip and an
// unknown one does not, so the two differ in LATENCY. Closing that would mean either
// sending a pointless message to every address typed here or faking a delay, and both are
// worse than documenting it. The rate limit bounds how many samples an attacker gets.
func (h *Handler) ctlRequestReset(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string `json:"email"`
	}
	if err := readJSON(w, r, &in); err != nil {
		readErr(w, err)
		return
	}
	email := normalEmail(in.Email)
	if err := h.spendAuthAttempt(h.codeLim, email, r); err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	// Mailed only for an account that exists, has proved this address, and is not disabled.
	// A disabled account is deliberately excluded: recovering a password it cannot sign in
	// with achieves nothing, and mailing its owner a code would read as reinstatement.
	if t, err := h.registry().ByEmail(email); err == nil && t.Verified() && !t.Disabled {
		if _, mErr := h.mailCode(t, tenant.PurposeReset); mErr != nil {
			// Swallowed on purpose: surfacing it here would answer differently for an address
			// that exists, which is the oracle this handler is shaped to avoid. mailFailed has
			// already logged it at WARN with the relay's own message.
			_ = mErr
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"next":            "reset",
		"email":           email,
		"code_expires_at": msOrZero(time.Now().Add(tenant.CodeTTL)),
		"code_valid_secs": int(tenant.CodeTTL.Seconds()),
	})
}

// ctlCompleteReset spends a reset code and installs the new password.
//
// The PURPOSE is fixed by this route rather than named by the client, which is what keeps
// the three code flows separate: a login code cannot be spent here and a reset code cannot
// be spent at /api/verify, because the purpose is mixed into the hash.
//
// It opens no session. Whoever completes a reset then signs in normally, which puts the
// password and a fresh emailed code back in front of the account — a reset that also
// signed you in would make one code worth two factors.
func (h *Handler) ctlCompleteReset(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email       string `json:"email"`
		Code        string `json:"code"`
		NewPassword string `json:"new_password"`
	}
	if err := readJSON(w, r, &in); err != nil {
		readErr(w, err)
		return
	}
	email := normalEmail(in.Email)
	if err := h.spendAuthAttempt(h.codeLim, email, r); err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	t, err := h.registry().ByEmail(email)
	if err != nil {
		// Same answer as a wrong code: whether an address has a pending reset is not
		// something this endpoint should confirm.
		ctlErr(w, http.StatusUnauthorized, "that code is not valid")
		return
	}
	switch err := h.registry().ResetPassword(t.ID, strings.TrimSpace(in.Code), in.NewPassword); {
	case err == nil:
	case errors.Is(err, tenant.ErrBadPassword):
		// Reported BEFORE the code is spent (see tenant.ResetPassword), so a too-short
		// password does not cost the user their one code.
		ctlErr(w, http.StatusBadRequest, fmt.Sprintf(
			"choose a password of at least %d characters", tenant.MinPasswordLen))
		return
	default:
		writeCodeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "changed",
		"next":   "signin",
		"note":   "Every signed-in machine on this account has been signed out.",
	})
}

// ctlManagerReset starts a reset FOR someone else. The manager learns nothing and sets
// nothing: the code goes to the account's own address, and only its owner can finish.
//
// This is the recovery path a manager actually needs — "I cannot sign in" — without
// becoming the ability to sign in AS a user, which would hand a manager that account's
// transcripts and defeat the one boundary this service promises its users.
func (h *Handler) ctlManagerReset(w http.ResponseWriter, r *http.Request) {
	actor, err := h.webPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	if !actor.IsManager() {
		ctlErr(w, http.StatusForbidden, "manager only")
		return
	}
	// No body to read, so readJSON's cross-site guard does not cover it: check directly.
	if err := checkOrigin(r); err != nil {
		readErr(w, err)
		return
	}
	target, err := h.registry().Get(r.PathValue("id"))
	if err != nil {
		ctlErr(w, http.StatusNotFound, "no such tenant")
		return
	}
	if !target.Verified() {
		ctlErr(w, http.StatusBadRequest, "that account has never confirmed its email address, "+
			"so there is nowhere to send a reset — it can register again, or you can reissue its token")
		return
	}
	if _, err := h.mailCode(target, tenant.PurposeReset); err != nil {
		// A manager gets the real failure. There is no oracle to protect here: they can
		// already list every account.
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	// On the record, with who started it. Not because the code is sensitive — it is not
	// ours to see either — but because "who caused my password to be reset" has to be
	// answerable.
	if err := h.registry().AuditWrite(actor.ID, target.ID, "password_reset", "", "code mailed"); err != nil {
		slog.Warn("context-guru: could not record a manager-initiated password reset",
			"target", target.ID, "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "mailed",
		"email":  target.Email,
		"note": "A reset code has been sent to that address. You cannot see it and cannot " +
			"set their password — only they can finish this.",
	})
}

// purgeTimeout bounds a purge. Generous: it may delete a few hundred objects from cold
// storage, each an rclone subprocess.
const purgeTimeout = 5 * time.Minute

// confirmed reports whether the manager typed this account's identity back.
//
// Deliberately not a checkbox. Both of these are irreversible and both are aimed BY ID at
// somebody else's data, so the one mistake worth engineering against is acting on the
// wrong row — and typing the address back is the check that catches it. Either the email
// or the id is accepted: the id is what the API deals in, the address is what a human
// recognises.
func confirmed(in string, t *tenant.Tenant) bool {
	in = strings.TrimSpace(in)
	return in != "" && (strings.EqualFold(in, t.Email) || in == t.ID)
}

// purgeBody is the confirmation both destructive routes require.
type purgeBody struct {
	Confirm string `json:"confirm"`
}

// ctlPurgeTenant erases a tenant's observability data and LEAVES THE ACCOUNT WORKING:
// their tokens, agent-key bindings, sessions and configuration are untouched, so their
// next request is captured as usual. This is the "clean their storage" case — a tenant who
// captured transcripts they should not have, or one whose history is filling the disk.
func (h *Handler) ctlPurgeTenant(w http.ResponseWriter, r *http.Request) {
	actor, target, ok := h.destructiveTarget(w, r)
	if !ok {
		return
	}
	res, err := h.purgeTenantData(target.ID)
	if err != nil {
		// 502 rather than 500 when cold storage is what failed: the local rows are gone or
		// still there as reported, and the part that did not work is a remote system.
		ctlErr(w, http.StatusBadGateway, "purge incomplete: "+err.Error())
		return
	}
	if err := h.registry().AuditWrite(actor.ID, target.ID, "storage_purged", "",
		fmt.Sprintf("%d requests, %d components, %d transcripts, %d archives",
			res.Requests, res.Components, res.Content, res.Archives)); err != nil {
		slog.Warn("context-guru: could not record a storage purge", "target", target.ID, "err", err)
	}
	slog.Warn("context-guru: manager purged a tenant's stored data",
		"actor", actor.ID, "tenant", target.ID, "requests", res.Requests,
		"archives", res.Archives, "objects", res.Objects)
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "purged", "email": target.Email, "purged": res,
		"note": "The account is untouched and still works. Its next request is captured as usual.",
	})
}

// ctlDeleteTenant removes an account AND its data, from both databases and cold storage.
//
// The order is the design, and it is not the obvious one:
//
//  1. Purge the metrics database and cold storage FIRST. A failure here is a 502 with the
//     account still intact, which is retryable. Deleting the account first would leave
//     rows owned by an id that no longer answers to anybody — invisible in every view,
//     unreachable by any retry, and still on disk.
//  2. Delete the account row. Tokens, sessions, agent keys and pending codes go with it
//     by ON DELETE CASCADE.
//  3. Purge AGAIN. Capture is asynchronous (a 250 ms flush), so a request that was in
//     flight during step 1 can land between the two. After step 2 nothing can
//     authenticate as this tenant, so this pass is final — which is what makes "no
//     orphans" a property rather than a hope.
func (h *Handler) ctlDeleteTenant(w http.ResponseWriter, r *http.Request) {
	actor, target, ok := h.destructiveTarget(w, r)
	if !ok {
		return
	}
	// Refused before anything is deleted. The registry refuses it too — this is the copy
	// that keeps the data intact, since the purge runs first.
	if actor.ID == target.ID {
		ctlErr(w, http.StatusForbidden, "a manager cannot delete their own account: "+
			"the manager routes are the only way to appoint another one")
		return
	}
	res, err := h.purgeTenantData(target.ID)
	if err != nil {
		ctlErr(w, http.StatusBadGateway, "nothing was deleted: their stored data could not be "+
			"removed first, and deleting the account would have orphaned it — "+err.Error())
		return
	}
	if err := h.registry().Delete(actor, target.ID); err != nil {
		switch {
		case errors.Is(err, tenant.ErrForbidden):
			ctlErr(w, http.StatusForbidden, "manager only")
		case errors.Is(err, tenant.ErrNotFound):
			ctlErr(w, http.StatusNotFound, "no such tenant")
		default:
			ctlErr(w, http.StatusInternalServerError,
				"their stored data was purged, but the account could not be deleted: "+err.Error())
		}
		return
	}
	// The in-memory tenancy holds this account's pipeline and state store; nothing can
	// authenticate as them now, so it is dead weight keyed by a live id.
	h.opts.Tenants.Forget(target.ID)
	// Step 3: the tail. Best-effort by construction — the account is already gone, so
	// there is nothing left to fail back to.
	if tail, tErr := h.purgeTenantData(target.ID); tErr != nil {
		slog.Warn("context-guru: a deleted tenant's in-flight rows could not be swept",
			"tenant", target.ID, "err", tErr)
	} else if tail.Removed() {
		slog.Info("context-guru: swept rows captured while a tenant was being deleted",
			"tenant", target.ID, "requests", tail.Requests)
		res.Requests += tail.Requests
		res.Components += tail.Components
		res.Content += tail.Content
	}
	slog.Warn("context-guru: manager deleted a tenant and their data",
		"actor", actor.ID, "tenant", target.ID, "requests", res.Requests, "objects", res.Objects)
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "deleted", "email": target.Email, "purged": res,
		"note": "The account, its tokens, its sessions and its stored data are gone. " +
			"The audit trail of this deletion is kept.",
	})
}

// destructiveTarget resolves the manager, the target account and the typed confirmation
// for the two irreversible routes. One place, so purge and delete cannot disagree about
// what counts as confirmation.
func (h *Handler) destructiveTarget(w http.ResponseWriter, r *http.Request) (*tenant.Tenant, *tenant.Tenant, bool) {
	actor, err := h.webPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return nil, nil, false
	}
	if !actor.IsManager() {
		ctlErr(w, http.StatusForbidden, "manager only")
		return nil, nil, false
	}
	var in purgeBody
	if err := readJSON(w, r, &in); err != nil {
		readErr(w, err)
		return nil, nil, false
	}
	target, err := h.registry().Get(r.PathValue("id"))
	if err != nil {
		ctlErr(w, http.StatusNotFound, "no such tenant")
		return nil, nil, false
	}
	if !confirmed(in.Confirm, target) {
		ctlErr(w, http.StatusBadRequest,
			"this cannot be undone: send confirm with that account's email address to proceed")
		return nil, nil, false
	}
	return actor, target, true
}

// purgeTenantData runs the dashboard-side purge under its own deadline.
//
// The context is deliberately NOT the request's. A manager whose browser gives up
// mid-purge must not leave half a tenant's data behind — this is a destructive operation
// that has to run to completion once it has started, and its result is reported afterwards
// either way.
func (h *Handler) purgeTenantData(tenantID string) (dash.PurgeResult, error) {
	if h.rec == nil {
		// No dashboard on this deployment: there is no stored traffic to purge, which is a
		// complete success rather than a failure.
		return dash.PurgeResult{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), purgeTimeout)
	defer cancel()
	return h.rec.PurgeTenant(ctx, tenantID)
}

// --- A/B variants ----------------------------------------------------------
//
// A variant is a NAME a manager puts on a set of accounts. It selects nothing and changes
// nothing on the request path: the configuration each account runs is the one on its own
// row, exactly as before. What the name buys is a way to group the metrics that already
// exist — so "give half the team the new pipeline and see what it did" becomes a label
// plus a GROUP BY rather than a new subsystem.
//
// Deliberately NOT built: a stats engine. No p-values, no confidence intervals, no
// significance test. The reason is in abCaveats — the assignment is not random, the
// workloads are not comparable, and a test statistic computed over those inputs would give
// a number that looks like evidence and is not. This project has already been misled once
// by comparing arms whose STEP COUNTS differed; the panel therefore always reports its
// denominators and says what it cannot show.

// abCaveats is what the comparison cannot tell you. Served with the data, not buried in a
// doc, because a cost delta with no confounds named is worse than no cost delta: it gets
// quoted.
var abCaveats = []string{
	"Assignment is not randomised. A manager chose who is in each variant, so any " +
		"difference may be a difference between the PEOPLE rather than the configurations.",
	"Workloads are not held constant. Variants differ in agent, model, task mix and how " +
		"much anyone worked this week — compare the request and session counts before " +
		"reading anything into the money.",
	"Per-request cost is not per-task cost. An earlier study in this repo was misled " +
		"exactly here: two arms with the same reward differed in STEP COUNT, so the arm " +
		"with cheaper requests spent more overall. Nothing on this panel can see task " +
		"outcomes.",
	"incomplete_rows are requests the provider gave us no usage for. Where that number " +
		"approaches the request count, the money figures for that variant are unknown " +
		"rather than low.",
	"saved_usd is a counterfactual: what the same traffic was priced at uncompacted, minus " +
		"what it actually cost including context-guru's own model spend. It is an estimate " +
		"of a request that was never sent.",
	"One variant can hold several different configurations. The configs list says how many " +
		"— if it is more than one, the variant is not a single treatment.",
}

// abComponent is one component's economics inside a variant: the answer to WHICH change
// did it, which the totals cannot give.
type abComponent struct {
	Component   string  `json:"component"`
	Runs        int64   `json:"runs"`
	Acted       int64   `json:"acted"`
	Reverted    int64   `json:"reverted"`
	SavedUnique int64   `json:"saved_unique"`
	ActRate     float64 `json:"act_rate"`
}

// abVariant is one row of the comparison.
type abVariant struct {
	// Variant is the assigned name; "" is the unassigned group, which is included on
	// purpose — how much traffic is OUTSIDE the experiment is part of reading it.
	Variant string `json:"variant"`
	// Tenants and Emails describe who is in it. A manager may see this; it is account
	// metadata, not traffic content.
	Tenants int      `json:"tenants"`
	Emails  []string `json:"emails"`
	// Configs is the DISTINCT effective configurations in this variant. More than one
	// means the variant is not one treatment — see abCaveats.
	Configs []string `json:"configs"`
	// Reporting is how many of those accounts have any traffic in the window at all. A
	// variant of six accounts where one produced every request is not six samples.
	Reporting       int64   `json:"reporting"`
	Requests        int64   `json:"requests"`
	Sessions        int64   `json:"sessions"`
	TokensBefore    int64   `json:"tokens_before"`
	TokensAfter     int64   `json:"tokens_after"`
	Saved           int64   `json:"saved"`
	SavedUnique     int64   `json:"saved_unique"`
	FreshInput      int64   `json:"fresh_input"`
	CacheRead       int64   `json:"cache_read"`
	CacheWrite      int64   `json:"cache_write"`
	OutputTokens    int64   `json:"output_tokens"`
	SpentUSD        float64 `json:"spent_usd"`
	SavedUSD        float64 `json:"saved_usd"`
	BaselineCostUSD float64 `json:"baseline_cost_usd"`
	Incomplete      int64   `json:"incomplete_rows"`
	// Components is per-component acted/reverted/saved, folded across this variant's
	// accounts.
	Components []abComponent `json:"components"`
}

// ctlVariants serves the A/B comparison: one row per variant, folded from the per-tenant
// aggregates the dashboard already computes.
//
// Folding is a SUM of sums, which is why there is no new storage and no new schema behind
// this. A variant is a set of tenants; dash groups by tenant (breakdown dim "tenant"); the
// rows add up. The alternative — stamping the variant onto every captured request — would
// have meant a metrics schema bump, which in this project renames the whole database aside
// and starts fresh. A label a manager can change at any time has no business costing
// anybody their history.
//
// Honest limitation of folding rather than storing: the variant is read as it is TODAY and
// applied to the whole window, so moving an account between variants retro-labels its past
// traffic. The audit log records when that happened; this panel cannot.
func (h *Handler) ctlVariants(w http.ResponseWriter, r *http.Request) {
	actor, err := h.webPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	if !actor.IsManager() {
		ctlErr(w, http.StatusForbidden, "manager only")
		return
	}
	if h.rec == nil {
		ctlErr(w, http.StatusServiceUnavailable,
			"the dashboard is not enabled on this deployment, so there are no metrics to compare")
		return
	}
	all, err := h.registry().List()
	if err != nil {
		ctlErr(w, http.StatusInternalServerError, "could not list tenants")
		return
	}
	// The same window the dashboard's filter bar is on, so the panel and the charts above
	// it agree. Unparseable is 0, which means unbounded — a filter is a view.
	since, until := atoi64(r.URL.Query().Get("since")), atoi64(r.URL.Query().Get("until"))
	window := dash.Filter{TenantAll: true, Since: since, Until: until}

	// One query for every account's totals. TenantAll is correct and checked: this handler
	// is manager-only, and Breakdown returns aggregates — never transcript content.
	groups, err := h.rec.DB().Breakdown(window, "tenant")
	if err != nil {
		ctlErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	byTenant := make(map[string]*dash.GroupRow, len(groups))
	for _, g := range groups {
		byTenant[g.Key] = g
	}

	// Ordered by first appearance of each variant in the roster (newest account first),
	// with the unassigned group last: it is context, not a contender.
	rows := map[string]*abVariant{}
	var order []string
	for _, t := range all {
		v, ok := rows[t.Variant]
		if !ok {
			v = &abVariant{Variant: t.Variant}
			rows[t.Variant] = v
			order = append(order, t.Variant)
		}
		v.Tenants++
		v.Emails = append(v.Emails, t.Email)
		if cfg := h.registry().Config(t); cfg != "" && !hasString(v.Configs, cfg) {
			v.Configs = append(v.Configs, cfg)
		}
		if g := byTenant[t.ID]; g != nil {
			v.Reporting++
			v.Requests += g.Requests
			v.Sessions += g.Sessions
			v.TokensBefore += g.TokensBefore
			v.TokensAfter += g.TokensAfter
			v.Saved += g.Saved
			v.SavedUnique += g.SavedUnique
			v.FreshInput += g.FreshInput
			v.CacheRead += g.CacheRead
			v.CacheWrite += g.CacheWrite
			v.OutputTokens += g.OutputTokens
			v.SpentUSD += g.SpentUSD
			v.SavedUSD += g.SavedUSD
			v.BaselineCostUSD += g.BaselineCostUSD
			v.Incomplete += g.Incomplete
		}
		// Per-component rows are only available per tenant, so they are folded one account
		// at a time.
		//
		// ponytail: one query per ACCOUNT, which is fine for an internal deployment's
		// roster and would not be for thousands. The next rung is a variant column on the
		// requests table, and that costs a metrics schema bump — not worth it until this
		// endpoint is measurably slow.
		comps, cErr := h.rec.DB().Components(dash.Filter{Tenant: t.ID, Since: since, Until: until})
		if cErr != nil {
			continue // a missing component breakdown must not lose the totals above
		}
		foldComponents(v, comps)
	}
	out := make([]*abVariant, 0, len(order))
	for _, name := range order {
		if name == "" {
			continue
		}
		out = append(out, rows[name])
	}
	if unassigned, ok := rows[""]; ok {
		out = append(out, unassigned)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"variants": out,
		"caveats":  abCaveats,
		"description": "Per-variant totals, folded from each account's own aggregates over the " +
			"selected window. A variant is a label a manager assigned, NOT a randomised arm: " +
			"read the caveats before quoting a difference. There is deliberately no " +
			"significance test here — the inputs do not support one.",
	})
}

// foldComponents adds one account's component rows into a variant's, keeping the list
// sorted by unique savings so the component that did the work is first.
func foldComponents(v *abVariant, comps []*dash.ComponentRow) {
	for _, c := range comps {
		var dst *abComponent
		for i := range v.Components {
			if v.Components[i].Component == c.Component {
				dst = &v.Components[i]
				break
			}
		}
		if dst == nil {
			v.Components = append(v.Components, abComponent{Component: c.Component})
			dst = &v.Components[len(v.Components)-1]
		}
		dst.Runs += c.Runs
		dst.Acted += c.Acted
		dst.Reverted += c.Reverted
		dst.SavedUnique += c.SavedUnique
		if dst.Runs > 0 {
			dst.ActRate = float64(dst.Acted) / float64(dst.Runs)
		}
	}
	sort.SliceStable(v.Components, func(i, j int) bool {
		return v.Components[i].SavedUnique > v.Components[j].SavedUnique
	})
}

func hasString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// atoi64 parses an epoch-millisecond query parameter, 0 for anything unparseable.
func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}
