package proxy

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
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

// MountControl registers the control-plane routes. Called only in hosted mode; without
// a tenant registry there are no accounts to manage.
func (h *Handler) MountControl(m *http.ServeMux) {
	if h.opts.Tenants == nil {
		return
	}
	m.HandleFunc("POST /api/register", h.ctlRegister)
	m.HandleFunc("POST /api/login", h.ctlLogin)
	m.HandleFunc("POST /api/verify", h.ctlVerify)
	m.HandleFunc("POST /api/logout", h.ctlLogout)
	m.HandleFunc("GET /api/me/sessions", h.ctlSessions)
	m.HandleFunc("DELETE /api/me/sessions/{id}", h.ctlRevokeSession)
	m.HandleFunc("GET /api/me", h.ctlMe)
	m.HandleFunc("PUT /api/me", h.ctlUpdateMe)
	m.HandleFunc("POST /api/me/tokens", h.ctlMintToken)
	m.HandleFunc("DELETE /api/me/tokens/{prefix}", h.ctlRevokeToken)
	m.HandleFunc("GET /api/me/audit", h.ctlAudit)
	m.HandleFunc("GET /api/options", h.ctlOptions)
	m.HandleFunc("GET /api/tenants", h.ctlTenants)
	m.HandleFunc("PATCH /api/tenants/{id}", h.ctlPatchTenant)
	m.HandleFunc("POST /api/tenants/{id}/tokens", h.ctlManagerMintToken)
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
			return nil, errTenantOff
		}
		return nil, errNoToken
	}
	return t, nil
}

// readJSON decodes a bounded, strict JSON body. Strict because a typo'd field in a
// settings save should be a visible error, not a silently ignored change the user
// believes they made.
func readJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxControlBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
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
	EffectiveConfigYAML string  `json:"effective_config_yaml"`
	ConfigInherited     bool    `json:"config_inherited"`
	UpAnthropic         string  `json:"up_anthropic"`
	UpOpenAI            string  `json:"up_openai"`
	UpBob               string  `json:"up_bob"`
	CaptureContent      bool    `json:"capture_content"`
	MonthlyCapUSD       float64 `json:"monthly_cap_usd"`
	MaxRows             int64   `json:"max_rows"`
	Disabled            bool    `json:"disabled"`
	CreatedAt           int64   `json:"created_at"`
	LastSeenAt          int64   `json:"last_seen_at"`
	SpentUSD            float64 `json:"spent_usd"`
}

func (h *Handler) view(t *tenant.Tenant) tenantView {
	v := tenantView{
		ID: t.ID, Label: t.Label, Email: t.Email, Role: string(t.Role),
		ConfigYAML:          t.ConfigYAML,
		EffectiveConfigYAML: h.registry().Config(t),
		ConfigInherited:     t.TracksDefault(),
		UpAnthropic:         t.UpAnthropic, UpOpenAI: t.UpOpenAI, UpBob: t.UpBob,
		CaptureContent: t.CaptureContent, MonthlyCapUSD: t.MonthlyCapUSD,
		MaxRows: t.MaxRows, Disabled: t.Disabled,
		CreatedAt: msOrZero(t.CreatedAt), LastSeenAt: msOrZero(t.LastSeenAt),
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
		ctlErr(w, http.StatusBadRequest, "malformed request: "+err.Error())
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
			ctlErr(w, http.StatusConflict,
				"that email is already registered — sign in instead, or reset it with the operator")
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
	writeJSON(w, http.StatusCreated, map[string]any{
		"next":              "verify",
		"email":             t.Email,
		"code_expires_at":   msOrZero(exp),
		"code_valid_secs":   int(tenant.CodeTTL.Seconds()),
		"code_max_attempts": tenant.MaxCodeAttempts,
	})
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
		// The error is logged WITHOUT the address's code and without the recipient's
		// mailbox contents; the relay's own message is the useful half.
		slog.Warn("context-guru: verification mail failed", "err", err.Error())
		if _, ok := err.(StatusError); ok {
			return time.Time{}, err
		}
		return time.Time{}, statusError{http.StatusBadGateway,
			"could not send the verification email; try again shortly"}
	}
	return c.ExpiresAt, nil
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
		ctlErr(w, http.StatusBadRequest, "malformed request")
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
			ctlErr(w, http.StatusForbidden, "this account is disabled")
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
			ctlErr(w, http.StatusForbidden, "this account is disabled")
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
		ctlErr(w, http.StatusBadRequest, "malformed request")
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
		ctlErr(w, http.StatusForbidden, "this account is disabled")
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
		ctlErr(w, http.StatusBadRequest, "malformed request: "+err.Error())
		return
	}
	// Upstream names are validated against the operator's allow-list HERE as well as at
	// request time: a settings page that accepts a name the proxy will later refuse is
	// a page that lets someone break their own agent and not find out until they use it.
	for _, name := range []*string{in.UpAnthropic, in.UpOpenAI, in.UpBob} {
		if name == nil || *name == "" {
			continue
		}
		if _, ok := h.opts.Upstreams[*name]; !ok {
			ctlErr(w, http.StatusBadRequest, "unknown upstream "+*name+
				"; pick one of the names from /api/options")
			return
		}
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
		ctlErr(w, http.StatusBadRequest, "malformed request")
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
		Label          *string  `json:"label"`
		Role           *string  `json:"role"`
		ConfigYAML     *string  `json:"config_yaml"`
		UpAnthropic    *string  `json:"up_anthropic"`
		UpOpenAI       *string  `json:"up_openai"`
		UpBob          *string  `json:"up_bob"`
		CaptureContent *bool    `json:"capture_content"`
		MonthlyCapUSD  *float64 `json:"monthly_cap_usd"`
		MaxRows        *int64   `json:"max_rows"`
		Disabled       *bool    `json:"disabled"`
	}
	if err := readJSON(w, r, &in); err != nil {
		ctlErr(w, http.StatusBadRequest, "malformed request: "+err.Error())
		return
	}
	patch := tenant.Patch{Label: in.Label, ConfigYAML: in.ConfigYAML,
		UpAnthropic: in.UpAnthropic, UpOpenAI: in.UpOpenAI, UpBob: in.UpBob,
		CaptureContent: in.CaptureContent, MonthlyCapUSD: in.MonthlyCapUSD,
		MaxRows: in.MaxRows, Disabled: in.Disabled}
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
	// A raised cap must take effect now, not after the spend cache expires — otherwise
	// a manager helps someone and they stay blocked for another minute with no
	// explanation.
	if in.MonthlyCapUSD != nil {
		h.InvalidateSpend(target)
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
		ctlErr(w, http.StatusBadRequest, "malformed request")
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
