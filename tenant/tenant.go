// Package tenant is the control plane for a hosted, multi-user context-guru: who
// the users are, which token identifies them, and what configuration their traffic
// runs under.
//
// Two properties shape every decision here.
//
// First, this data is NOT derived. dash's request store is a view — it renames
// itself aside and starts fresh on a schema change, because losing observability
// history beats refusing to boot. Accounts cannot work that way, so tenants live in
// their own database with real forward migrations (PRAGMA user_version), never in
// the dashboard file, and the retention janitor never touches them.
//
// Second, we hold no user's provider credential. A user authenticates with a token
// WE mint and forwards their OWN provider key to the upstream; that key is read from
// the request and never stored. So the only secrets in this database are sha256
// digests — of a token, and (for agents that cannot set a custom header) of a
// provider key — and the only thing ever displayed is a token's 8-character prefix.
// There is no code path that can print either, because after Register/MintToken
// return the process no longer holds a token, and BindAgentKey hashes its argument
// before anything else.
package tenant

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, same as dash: no extra C toolchain
)

// Errors callers are expected to branch on. Authentication fails CLOSED (an
// unknown token is never treated as a new tenant); configuration fails open
// elsewhere, in the resolver that reads it.
var (
	ErrUnknownToken = errors.New("tenant: unknown or revoked token")
	ErrDisabled     = errors.New("tenant: account disabled")
	ErrNotFound     = errors.New("tenant: no such tenant")
	ErrEmailTaken   = errors.New("tenant: email already registered")
	ErrEmailDomain  = errors.New("tenant: email domain not allowed")
	ErrBadEmail     = errors.New("tenant: malformed email")
	ErrBadLabel     = errors.New("tenant: label must be 1-64 printable characters")
	ErrForbidden    = errors.New("tenant: not permitted")
)

// Role decides what a token may see beyond its own rows. Exactly two, because a
// permission matrix nobody asked for is the classic thing to regret.
type Role string

const (
	RoleUser    Role = "user"
	RoleManager Role = "manager"
)

// DefaultConfigYAML is the pipeline every new tenant starts on: codesmart with the
// LLM extractor removed and no blind collapse. Deterministic all the way through,
// which on a SHARED box is the property that matters — it makes no cheap-model
// calls, so it adds no upstream spend, contends on no shared LLM budget, and costs
// near-zero added latency on someone else's agent. A tenant can opt into
// extract_llm on their settings page and see the tradeoff there.
//
// Expressed as a literal pipeline rather than a registered preset name: this is one
// default value, not a new vocabulary word.
const DefaultConfigYAML = `pipeline: [format, toon, dedup, failed_run, cmdfilter, extract, cachesplit]
components:
  extract:
    min_tokens: 400
mode: sync
`

// Tenant is one user of the service.
type Tenant struct {
	ID    string
	Label string
	Email string
	Role  Role
	// ConfigYAML is the tenant's context-guru configuration, validated on write by
	// the same strict loader the proxy uses. Empty means "use the server default".
	ConfigYAML string
	// Upstream selections, by name into the server's allow-list — never a URL. A
	// user-supplied upstream URL would make this proxy an SSRF hop out of the
	// network, which is not a tradeoff, just a hole.
	UpAnthropic string
	UpOpenAI    string
	UpBob       string
	// CaptureContent opts this tenant into storing before/after transcript text.
	// Off by default and per-tenant by design: the redactor is a best-effort
	// denylist, so this is consent, not a switch.
	CaptureContent bool
	// MaxRows caps this tenant's retained request rows so one heavy user cannot
	// evict everyone else's history. 0 = the server default
	// (--dashboard-max-rows-per-tenant). Enforced by the dashboard janitor, which reads
	// it through dash.Recorder.SetTenantQuota — wired in cmd/context-guru-proxy.
	MaxRows    int64
	Disabled   bool
	CreatedAt  time.Time
	LastSeenAt time.Time
}

// IsManager reports whether this tenant may read and write other tenants.
func (t *Tenant) IsManager() bool { return t != nil && t.Role == RoleManager }

// TracksDefault reports whether this tenant follows the server default rather than a
// configuration of their own. It is the ONE test for that, used by Config to resolve
// the document and by the API to tell the UI which state to draw — two copies of the
// same predicate is how a settings page ends up disagreeing with the proxy.
func (t *Tenant) TracksDefault() bool { return t == nil || strings.TrimSpace(t.ConfigYAML) == "" }

// Token is the public view of a credential: enough to recognise and revoke one,
// never enough to use it.
type Token struct {
	Prefix     string // first 8 chars of the random part, for display
	Label      string
	TenantID   string
	CreatedAt  time.Time
	LastUsedAt time.Time
	RevokedAt  time.Time
}

// Revoked reports whether this token has been revoked.
func (t Token) Revoked() bool { return !t.RevokedAt.IsZero() }

// Options configures the registry.
type Options struct {
	// DefaultConfig is what a tenant's traffic runs under until they store a
	// configuration of their own — read live on every resolve, never copied into a
	// tenant's row, so changing it changes what every tracking tenant runs.
	// Empty = DefaultConfigYAML.
	DefaultConfig string
	// ManagerEmail, when it matches a registering email (case-insensitively), makes
	// that tenant a manager. This is how the first manager exists at all; an
	// interactive bootstrap step is a thing to forget and then work around.
	ManagerEmail string
	// EmailDomains restricts who may self-register (e.g. ["ibm.com"]). Empty means
	// anyone who can reach the port, which is only right on loopback.
	EmailDomains []string
	// DefaultUpstreams are the allow-list names a new tenant starts with.
	DefaultUpAnthropic string
	DefaultUpOpenAI    string
	DefaultUpBob       string
	// CacheTTL bounds how long a resolved token is trusted from memory. Mutations
	// through this registry clear the cache immediately, so the TTL only bounds
	// staleness from an EXTERNAL edit of the database. 0 = DefaultCacheTTL.
	CacheTTL time.Duration
	// Validate checks a configuration document before it is stored. nil skips
	// validation, which is right for tests and wrong for a server.
	Validate func([]byte) error
}

// DefaultCacheTTL is deliberately short: it exists to keep a token lookup
// off the writer's path under agent-rate traffic, not to be a session store.
const DefaultCacheTTL = 30 * time.Second

// Registry is the control-plane store. Safe for concurrent use.
type Registry struct {
	db   *sql.DB
	path string
	opts Options

	mu    sync.RWMutex
	cache map[string]cacheEntry // sha256(token) hex -> resolved tenant
}

type cacheEntry struct {
	t   *Tenant
	exp time.Time
}

// Open opens (creating if needed) the control database and runs migrations.
// An empty path opens a private in-memory database, for tests.
func Open(path string, o Options) (*Registry, error) {
	if o.DefaultConfig == "" {
		o.DefaultConfig = DefaultConfigYAML
	}
	if o.CacheTTL == 0 {
		o.CacheTTL = DefaultCacheTTL
	}
	dsn := memDSN()
	if path != "" {
		if dir := filepath.Dir(path); dir != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, err
			}
		}
		// WAL so the dashboard's reads never block on a config write; foreign keys
		// for the ON DELETE CASCADE from tenants to tokens. synchronous(FULL), unlike
		// dash's NORMAL: losing the tail of the request log to a power cut is
		// acceptable, losing an account is not.
		dsn = "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)" +
			"&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("tenant: migrate: %w", err)
	}
	return &Registry{db: db, path: path, opts: o, cache: map[string]cacheEntry{}}, nil
}

// Close releases the database.
func (r *Registry) Close() error { return r.db.Close() }

// Register creates a tenant and its first token. The returned string is the ONLY
// time the plaintext token exists outside the caller's machine: it is not stored,
// logged, or recoverable. Show it once.
func (r *Registry) Register(label, email string) (*Tenant, string, error) {
	label = strings.TrimSpace(label)
	if !validLabel(label) {
		return nil, "", ErrBadLabel
	}
	email, err := r.checkEmail(email)
	if err != nil {
		return nil, "", err
	}
	role := RoleUser
	if r.opts.ManagerEmail != "" && strings.EqualFold(email, strings.TrimSpace(r.opts.ManagerEmail)) {
		role = RoleManager
	}
	now := time.Now()
	t := &Tenant{
		ID: newID(), Label: label, Email: email, Role: role,
		// ConfigYAML is deliberately left EMPTY: a new tenant TRACKS the server
		// default (see Config) instead of getting a copy of it. Copying it in froze
		// every tenant on the default as it existed on their registration day —
		// adding a component to the default reached nobody who had already
		// registered, and there was no way to ask for "just follow the default".
		// Storing a config is what CUSTOMISING means; registering is not that.
		UpAnthropic: r.opts.DefaultUpAnthropic,
		UpOpenAI:    r.opts.DefaultUpOpenAI,
		UpBob:       r.opts.DefaultUpBob,
		// Capture the before/after text by default, so the diff view — the thing that
		// makes the savings inspectable rather than a number to take on faith — works
		// on a new account's first request instead of after they find a setting.
		//
		// This is a deliberate reversal of the safer default, and the tradeoff is real:
		// this is the one path that writes ARBITRARY agent output to disk. It is scrubbed
		// of known credential shapes and size-capped first, but that scrubbing is a
		// pattern denylist, and a review of 22 realistic credential shapes found 11 got
		// through. The operator gate (--dashboard-content) still has to be on as well, and
		// a tenant can turn this off on their settings page at any time. What makes it
		// acceptable here: content is visible ONLY to the tenant that produced it — a
		// manager sees everyone's metrics and nobody's transcripts.
		CaptureContent: true,
		CreatedAt:      now,
	}
	plain, hash, prefix, err := mintToken()
	if err != nil {
		return nil, "", err
	}
	tx, err := r.db.Begin()
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO tenants
	  (id,label,email,role,config_yaml,up_anthropic,up_openai,up_bob,
	   capture_content,max_rows,disabled,created_at,last_seen_at)
	  VALUES (?,?,?,?,?,?,?,?,?,0,0,?,0)`,
		t.ID, t.Label, t.Email, string(t.Role), t.ConfigYAML,
		t.UpAnthropic, t.UpOpenAI, t.UpBob, boolInt(t.CaptureContent),
		now.UnixMilli()); err != nil {
		if isUniqueViolation(err) {
			return nil, "", ErrEmailTaken
		}
		return nil, "", err
	}
	if _, err := tx.Exec(`INSERT INTO tenant_tokens
	  (token_hash,prefix,tenant_id,label,created_at,last_used_at,revoked_at)
	  VALUES (?,?,?,?,?,0,0)`, hash, prefix, t.ID, label, now.UnixMilli()); err != nil {
		return nil, "", err
	}
	if err := tx.Commit(); err != nil {
		return nil, "", err
	}
	return t, plain, nil
}

// MintToken issues an additional token for a tenant, so a machine can be added or
// a credential rotated without downtime. The plaintext is returned once.
func (r *Registry) MintToken(tenantID, label string) (string, error) {
	label = strings.TrimSpace(label)
	if !validLabel(label) {
		return "", ErrBadLabel
	}
	if _, err := r.Get(tenantID); err != nil {
		return "", err
	}
	plain, hash, prefix, err := mintToken()
	if err != nil {
		return "", err
	}
	if _, err := r.db.Exec(`INSERT INTO tenant_tokens
	  (token_hash,prefix,tenant_id,label,created_at,last_used_at,revoked_at)
	  VALUES (?,?,?,?,?,0,0)`, hash, prefix, tenantID, label, time.Now().UnixMilli()); err != nil {
		return "", err
	}
	return plain, nil
}

// RevokeToken revokes one of a tenant's tokens by its public prefix. Revocation is
// a timestamp, not a delete, so a leaked credential stays visible in the audit
// trail instead of quietly disappearing.
func (r *Registry) RevokeToken(tenantID, prefix string) error {
	res, err := r.db.Exec(
		`UPDATE tenant_tokens SET revoked_at = ? WHERE tenant_id = ? AND prefix = ? AND revoked_at = 0`,
		time.Now().UnixMilli(), tenantID, prefix)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	r.clearCache() // a revoked token must stop working now, not in 30 seconds
	return nil
}

// Tokens lists a tenant's tokens, including revoked ones.
func (r *Registry) Tokens(tenantID string) ([]Token, error) {
	rows, err := r.db.Query(`SELECT prefix,label,tenant_id,created_at,last_used_at,revoked_at
	  FROM tenant_tokens WHERE tenant_id = ? ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		var created, used, revoked int64
		if err := rows.Scan(&t.Prefix, &t.Label, &t.TenantID, &created, &used, &revoked); err != nil {
			return nil, err
		}
		t.CreatedAt = msTime(created)
		t.LastUsedAt = msTime(used)
		t.RevokedAt = msTime(revoked)
		out = append(out, t)
	}
	return out, rows.Err()
}

// Resolve maps a plaintext token to its tenant. This is on the proxy's hot path,
// so a hit is served from memory; a miss reads the database and stamps
// last_used_at, which bounds that write to once per CacheTTL per token rather than
// once per request.
//
// There is no secret-dependent comparison to make constant-time: the sha256 IS the
// lookup key, and computing it requires already holding the token.
func (r *Registry) Resolve(token string) (*Tenant, error) {
	// Reject anything that is not shaped like one of our tokens before hashing or
	// touching the database — a wrong Authorization header is the common case.
	if !strings.HasPrefix(token, tokenPrefix) || len(token) != tokenLen {
		return nil, ErrUnknownToken
	}
	sum := sha256.Sum256([]byte(token))
	key := hex.EncodeToString(sum[:])

	r.mu.RLock()
	e, ok := r.cache[key]
	r.mu.RUnlock()
	if ok && time.Now().Before(e.exp) {
		if e.t.Disabled {
			return nil, ErrDisabled
		}
		return e.t, nil
	}

	t, err := r.loadByTokenHash(sum[:])
	if err != nil {
		return nil, err
	}
	now := time.Now()
	// Best-effort: a failed timestamp update must not fail the request.
	_, _ = r.db.Exec(`UPDATE tenant_tokens SET last_used_at = ? WHERE token_hash = ?`,
		now.UnixMilli(), sum[:])
	_, _ = r.db.Exec(`UPDATE tenants SET last_seen_at = ? WHERE id = ?`, now.UnixMilli(), t.ID)

	r.mu.Lock()
	r.cache[key] = cacheEntry{t: t, exp: now.Add(r.opts.CacheTTL)}
	r.mu.Unlock()

	if t.Disabled {
		return nil, ErrDisabled
	}
	return t, nil
}

func (r *Registry) loadByTokenHash(hash []byte) (*Tenant, error) {
	row := r.db.QueryRow(`SELECT `+tenantCols+` FROM tenants t
	  JOIN tenant_tokens k ON k.tenant_id = t.id
	  WHERE k.token_hash = ? AND k.revoked_at = 0`, hash)
	t, err := scanTenant(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnknownToken
	}
	return t, err
}

// Get reads one tenant by id.
func (r *Registry) Get(id string) (*Tenant, error) {
	t, err := scanTenant(r.db.QueryRow(`SELECT `+tenantCols+` FROM tenants t WHERE t.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// ByEmail reads one tenant by email (lowercased).
func (r *Registry) ByEmail(email string) (*Tenant, error) {
	t, err := scanTenant(r.db.QueryRow(`SELECT `+tenantCols+` FROM tenants t WHERE t.email = ?`,
		strings.ToLower(strings.TrimSpace(email))))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// List returns every tenant, newest first. Manager-only at the API layer.
func (r *Registry) List() ([]*Tenant, error) {
	rows, err := r.db.Query(`SELECT ` + tenantCols + ` FROM tenants t ORDER BY t.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Tenant
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Patch is a sparse update: a nil field is left alone. One method with pointers
// beats eight single-field setters, and it lets the audit trail record exactly the
// fields that changed.
type Patch struct {
	Label          *string
	Role           *Role
	ConfigYAML     *string
	UpAnthropic    *string
	UpOpenAI       *string
	UpBob          *string
	CaptureContent *bool
	MaxRows        *int64
	Disabled       *bool
}

// Update applies a patch to a tenant, recording each changed field in the audit
// log. actor must be the target tenant or a manager; only a manager may change
// role, row quota, or disabled state — the fields a user would otherwise raise on
// themselves.
func (r *Registry) Update(actor *Tenant, targetID string, p Patch) error {
	if actor == nil {
		return ErrForbidden
	}
	if actor.ID != targetID && !actor.IsManager() {
		return ErrForbidden
	}
	privileged := p.Role != nil || p.MaxRows != nil || p.Disabled != nil
	if privileged && !actor.IsManager() {
		return ErrForbidden
	}
	cur, err := r.Get(targetID)
	if err != nil {
		return err
	}
	if p.Label != nil && !validLabel(strings.TrimSpace(*p.Label)) {
		return ErrBadLabel
	}
	if p.Role != nil && *p.Role != RoleUser && *p.Role != RoleManager {
		return fmt.Errorf("tenant: unknown role %q", *p.Role)
	}
	if p.ConfigYAML != nil && r.opts.Validate != nil && strings.TrimSpace(*p.ConfigYAML) != "" {
		if err := r.opts.Validate([]byte(*p.ConfigYAML)); err != nil {
			return fmt.Errorf("tenant: config rejected: %w", err)
		}
	}

	type change struct{ field, before, after string }
	var changes []change
	set := func(field, before, after string, differs bool) {
		if differs {
			changes = append(changes, change{field, before, after})
		}
	}
	next := *cur
	if v := p.Label; v != nil {
		next.Label = strings.TrimSpace(*v)
		set("label", cur.Label, next.Label, next.Label != cur.Label)
	}
	if v := p.Role; v != nil {
		next.Role = *v
		set("role", string(cur.Role), string(next.Role), next.Role != cur.Role)
	}
	if v := p.ConfigYAML; v != nil {
		next.ConfigYAML = *v
		set("config_yaml", cur.ConfigYAML, next.ConfigYAML, next.ConfigYAML != cur.ConfigYAML)
	}
	if v := p.UpAnthropic; v != nil {
		next.UpAnthropic = *v
		set("up_anthropic", cur.UpAnthropic, next.UpAnthropic, next.UpAnthropic != cur.UpAnthropic)
	}
	if v := p.UpOpenAI; v != nil {
		next.UpOpenAI = *v
		set("up_openai", cur.UpOpenAI, next.UpOpenAI, next.UpOpenAI != cur.UpOpenAI)
	}
	if v := p.UpBob; v != nil {
		next.UpBob = *v
		set("up_bob", cur.UpBob, next.UpBob, next.UpBob != cur.UpBob)
	}
	if v := p.CaptureContent; v != nil {
		next.CaptureContent = *v
		set("capture_content", boolStr(cur.CaptureContent), boolStr(next.CaptureContent),
			next.CaptureContent != cur.CaptureContent)
	}
	if v := p.MaxRows; v != nil {
		if *v < 0 {
			return fmt.Errorf("tenant: max rows must not be negative")
		}
		next.MaxRows = *v
		set("max_rows", fmt.Sprint(cur.MaxRows), fmt.Sprint(next.MaxRows), next.MaxRows != cur.MaxRows)
	}
	if v := p.Disabled; v != nil {
		next.Disabled = *v
		set("disabled", boolStr(cur.Disabled), boolStr(next.Disabled), next.Disabled != cur.Disabled)
	}
	if len(changes) == 0 {
		return nil
	}

	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE tenants SET label=?,role=?,config_yaml=?,up_anthropic=?,
	  up_openai=?,up_bob=?,capture_content=?,max_rows=?,disabled=? WHERE id=?`,
		next.Label, string(next.Role), next.ConfigYAML, next.UpAnthropic, next.UpOpenAI,
		next.UpBob, boolInt(next.CaptureContent), next.MaxRows,
		boolInt(next.Disabled), targetID); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	for _, c := range changes {
		if _, err := tx.Exec(`INSERT INTO tenant_config_audit
		  (ts,actor_tenant,target_tenant,field,before,after) VALUES (?,?,?,?,?,?)`,
			now, actor.ID, targetID, c.field, c.before, c.after); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Every mutation clears the whole cache. Mutations are rare and resolves are
	// hot, so precise invalidation buys nothing and can be wrong in one direction
	// that matters (a disabled tenant still being served).
	r.clearCache()
	// Disabling an account must also close its dashboard logins. Blocking the proxy
	// while leaving a live cookie behind would let a disabled user keep reading their
	// history for up to the session TTL, which is not what "disabled" means to
	// whoever clicked it.
	if p.Disabled != nil && *p.Disabled {
		if err := r.EndAllWebSessions(targetID); err != nil {
			return err
		}
	}
	return nil
}

// AuditEntry is one recorded configuration change.
type AuditEntry struct {
	At            time.Time
	Actor, Target string
	Field         string
	Before, After string
}

// Audit returns the most recent changes to a tenant, newest first. A shared
// service needs "which config change broke my agent, and who made it" to be
// answerable.
func (r *Registry) Audit(targetID string, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q := `SELECT ts,actor_tenant,target_tenant,field,before,after FROM tenant_config_audit`
	args := []any{}
	if targetID != "" {
		q += ` WHERE target_tenant = ?`
		args = append(args, targetID)
	}
	q += ` ORDER BY ts DESC LIMIT ?`
	args = append(args, limit)
	rows, err := r.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var ts int64
		if err := rows.Scan(&ts, &e.Actor, &e.Target, &e.Field, &e.Before, &e.After); err != nil {
			return nil, err
		}
		e.At = msTime(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Config returns the configuration document to run this tenant's traffic under,
// falling back to the server default when the tenant has none. Resolution fails
// OPEN: a user's agent must not break because their configuration row is empty.
func (r *Registry) Config(t *Tenant) string {
	if !t.TracksDefault() {
		return t.ConfigYAML
	}
	return r.opts.DefaultConfig
}

func (r *Registry) clearCache() {
	r.mu.Lock()
	r.cache = map[string]cacheEntry{}
	r.mu.Unlock()
}

func (r *Registry) checkEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	at := strings.IndexByte(email, '@')
	if at <= 0 || at == len(email)-1 || strings.Count(email, "@") != 1 ||
		len(email) > 254 || strings.ContainsAny(email, " \t\r\n") {
		return "", ErrBadEmail
	}
	if len(r.opts.EmailDomains) == 0 {
		return email, nil
	}
	domain := email[at+1:]
	for _, d := range r.opts.EmailDomains {
		d = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(d, "@")))
		if d != "" && (domain == d || strings.HasSuffix(domain, "."+d)) {
			return email, nil
		}
	}
	return "", ErrEmailDomain
}

// --- agent keys -------------------------------------------------------------

// ErrNoAgentKey is returned when a provider key's digest is bound to no tenant.
var ErrNoAgentKey = errors.New("tenant: provider key is not bound to any account")

// agentKeyHash is the ONLY thing this package ever does with a provider key:
// digest it and drop it. Nothing below this line holds the plaintext.
func agentKeyHash(key string) []byte {
	sum := sha256.Sum256([]byte(strings.TrimSpace(key)))
	return sum[:]
}

// BindAgentKey records that a provider key belongs to a tenant, by digest, so an
// agent that can only send that key is still identified. Idempotent, and re-binding
// a key moves it to the calling tenant — which is safe precisely because holding the
// key is what proves the claim.
func (r *Registry) BindAgentKey(tenantID, key string) error {
	if strings.TrimSpace(key) == "" {
		return ErrUnknownToken
	}
	if _, err := r.Get(tenantID); err != nil {
		return err
	}
	_, err := r.db.Exec(`INSERT INTO tenant_agent_keys (key_hash,tenant_id,created_at)
	  VALUES (?,?,?) ON CONFLICT(key_hash) DO UPDATE SET tenant_id = excluded.tenant_id`,
		agentKeyHash(key), tenantID, time.Now().UnixMilli())
	if err == nil {
		r.clearCache()
	}
	return err
}

// UnbindAgentKeys drops every agent key bound to a tenant. There is no per-key
// variant: the digests are not displayable, so "which one" is not a question the
// user can answer.
func (r *Registry) UnbindAgentKeys(tenantID string) error {
	if _, err := r.db.Exec(`DELETE FROM tenant_agent_keys WHERE tenant_id = ?`, tenantID); err != nil {
		return err
	}
	r.clearCache()
	return nil
}

// AgentKeyCount reports how many keys a tenant has bound, for the settings page.
func (r *Registry) AgentKeyCount(tenantID string) (int, error) {
	var n int
	err := r.db.QueryRow(`SELECT COUNT(*) FROM tenant_agent_keys WHERE tenant_id = ?`,
		tenantID).Scan(&n)
	return n, err
}

// ResolveAgentKey maps a caller's own provider key to the tenant that bound it. Same
// caching and last-used bookkeeping as Resolve, and the same closed failure: an
// unbound key is not a new tenant.
func (r *Registry) ResolveAgentKey(key string) (*Tenant, error) {
	if strings.TrimSpace(key) == "" {
		return nil, ErrNoAgentKey
	}
	hash := agentKeyHash(key)
	// Namespaced so a digest can never be confused with a token digest, even though
	// the two are computed over different inputs.
	ck := "ak:" + hex.EncodeToString(hash)

	r.mu.RLock()
	e, ok := r.cache[ck]
	r.mu.RUnlock()
	if ok && time.Now().Before(e.exp) {
		if e.t.Disabled {
			return nil, ErrDisabled
		}
		return e.t, nil
	}

	t, err := scanTenant(r.db.QueryRow(`SELECT `+tenantCols+` FROM tenants t
	  JOIN tenant_agent_keys k ON k.tenant_id = t.id WHERE k.key_hash = ?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoAgentKey
	}
	if err != nil {
		return nil, err
	}
	now := time.Now()
	_, _ = r.db.Exec(`UPDATE tenant_agent_keys SET last_used_at = ? WHERE key_hash = ?`,
		now.UnixMilli(), hash)
	_, _ = r.db.Exec(`UPDATE tenants SET last_seen_at = ? WHERE id = ?`, now.UnixMilli(), t.ID)

	r.mu.Lock()
	r.cache[ck] = cacheEntry{t: t, exp: now.Add(r.opts.CacheTTL)}
	r.mu.Unlock()

	if t.Disabled {
		return nil, ErrDisabled
	}
	return t, nil
}

// --- tokens -----------------------------------------------------------------

// TokenPrefix is how a context-guru token is recognised on sight. Exported because
// the proxy now accepts the caller's OWN provider key in the Authorization slot, so
// it has to be able to tell our token apart from a credential it must forward.
const TokenPrefix = "cg_live_"

const (
	tokenPrefix = TokenPrefix
	// 16 random bytes as unpadded base32 = 26 characters, 128 bits of entropy.
	tokenBody = 26
	tokenLen  = len(tokenPrefix) + tokenBody
	// prefixLen is how much of the body is public, for display and revocation.
	prefixLen = 8
)

var tokenEnc = base32.StdEncoding.WithPadding(base32.NoPadding)

// LooksLikeToken reports whether s is shaped like one of our tokens. It says nothing
// about whether the token exists — only that it is ours rather than a provider
// credential, which is the distinction the proxy needs before deciding what to
// forward.
func LooksLikeToken(s string) bool {
	return strings.HasPrefix(s, TokenPrefix) && len(s) == tokenLen
}

// mintToken returns the plaintext token, its sha256, and its public prefix.
func mintToken() (plain string, hash []byte, prefix string, err error) {
	var b [16]byte
	if _, err = rand.Read(b[:]); err != nil {
		return "", nil, "", err
	}
	body := tokenEnc.EncodeToString(b[:])
	plain = tokenPrefix + body
	sum := sha256.Sum256([]byte(plain))
	return plain, sum[:], body[:prefixLen], nil
}

// --- schema -----------------------------------------------------------------

// migrations is append-only: index+1 is the schema version it produces. Unlike
// dash's disposable view, this database is migrated forward forever — never
// renamed aside, never rebuilt.
//
// There is deliberately NO data migration for the rows Register used to stamp with a
// copy of the server default (see Register). Clearing a stored config would have to
// recognise a byte-identical PREVIOUS default to be safe, and this build cannot: the
// previous defaults were never recorded anywhere — no constant, no schema row, and
// this package predates its first commit — so the match list would be a guess.
// A guess that misses does nothing; a guess that hits deletes a configuration
// somebody chose. Existing rows therefore keep exactly what they have, and the
// settings page offers "Track the server default" as a one-click, audited way out,
// pointing out when a saved config is identical to the current default. If a future
// change to DefaultConfigYAML wants to sweep tracking tenants forward, add the
// OUTGOING literal to a list here at the same commit that changes it, while it is
// still known to be exactly what Register wrote.
var migrations = []string{
	`CREATE TABLE tenants (
	   id              TEXT PRIMARY KEY,
	   label           TEXT    NOT NULL,
	   email           TEXT    NOT NULL UNIQUE,
	   role            TEXT    NOT NULL DEFAULT 'user',
	   config_yaml     TEXT    NOT NULL DEFAULT '',
	   up_anthropic    TEXT    NOT NULL DEFAULT '',
	   up_openai       TEXT    NOT NULL DEFAULT '',
	   up_bob          TEXT    NOT NULL DEFAULT '',
	   capture_content INTEGER NOT NULL DEFAULT 0,
	   max_rows        INTEGER NOT NULL DEFAULT 0,
	   disabled        INTEGER NOT NULL DEFAULT 0,
	   created_at      INTEGER NOT NULL,
	   last_seen_at    INTEGER NOT NULL DEFAULT 0
	 );
	 -- token_hash is sha256(token). The plaintext is never stored, so a dump of
	 -- this table cannot be replayed against the proxy.
	 CREATE TABLE tenant_tokens (
	   token_hash   BLOB    PRIMARY KEY,
	   prefix       TEXT    NOT NULL,
	   tenant_id    TEXT    NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
	   label        TEXT    NOT NULL DEFAULT '',
	   created_at   INTEGER NOT NULL,
	   last_used_at INTEGER NOT NULL DEFAULT 0,
	   revoked_at   INTEGER NOT NULL DEFAULT 0
	 );
	 CREATE INDEX idx_tokens_tenant ON tenant_tokens(tenant_id);
	 -- Browser logins. Separate from tokens so revoking a proxy credential does
	 -- not log someone out of the dashboard, and vice versa.
	 CREATE TABLE dash_sessions (
	   id         BLOB    PRIMARY KEY,
	   tenant_id  TEXT    NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
	   created_at INTEGER NOT NULL,
	   expires_at INTEGER NOT NULL
	 );
	 CREATE INDEX idx_dash_sessions_tenant ON dash_sessions(tenant_id);
	 CREATE TABLE tenant_config_audit (
	   ts            INTEGER NOT NULL,
	   actor_tenant  TEXT    NOT NULL,
	   target_tenant TEXT    NOT NULL,
	   field         TEXT    NOT NULL,
	   before        TEXT    NOT NULL DEFAULT '',
	   after         TEXT    NOT NULL DEFAULT ''
	 );
	 CREATE INDEX idx_audit_target ON tenant_config_audit(target_tenant, ts DESC);`,
	// Agent keys. Some agents (Bob/BobShell) can set no request header we do not
	// already occupy: their client builds Authorization, User-Agent, x-instance-id and
	// x-team-id itself and offers no hook for another one. Such an agent can still be
	// identified by the credential it DOES send â its own provider key â so a tenant
	// binds the sha256 of that key once and their traffic resolves by it thereafter.
	//
	// key_hash only. The key itself is never inserted, selected, or logged, so this
	// table is exactly as replayable as tenant_tokens: not at all.
	`CREATE TABLE tenant_agent_keys (
	   key_hash     BLOB    PRIMARY KEY,
	   tenant_id    TEXT    NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
	   created_at   INTEGER NOT NULL,
	   last_used_at INTEGER NOT NULL DEFAULT 0
	 );
	 CREATE INDEX idx_agent_keys_tenant ON tenant_agent_keys(tenant_id);`,
}

func migrate(db *sql.DB) error {
	var have int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&have); err != nil {
		return err
	}
	if have > len(migrations) {
		return fmt.Errorf("control database is version %d, this build knows %d "+
			"(a newer context-guru wrote it; downgrade is not supported)", have, len(migrations))
	}
	for i := have; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("step %d: %w", i+1, err)
		}
		// PRAGMA does not accept a bound parameter; i+1 is a loop index, not input.
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// --- small helpers ----------------------------------------------------------

const tenantCols = `t.id,t.label,t.email,t.role,t.config_yaml,t.up_anthropic,t.up_openai,
	t.up_bob,t.capture_content,t.max_rows,t.disabled,t.created_at,t.last_seen_at`

// scanner is what *sql.Row and *sql.Rows have in common.
type scanner interface{ Scan(...any) error }

func scanTenant(s scanner) (*Tenant, error) {
	var t Tenant
	var role string
	var capture, disabled int
	var created, seen int64
	if err := s.Scan(&t.ID, &t.Label, &t.Email, &role, &t.ConfigYAML, &t.UpAnthropic,
		&t.UpOpenAI, &t.UpBob, &capture, &t.MaxRows, &disabled,
		&created, &seen); err != nil {
		return nil, err
	}
	t.Role = Role(role)
	t.CaptureContent = capture != 0
	t.Disabled = disabled != 0
	t.CreatedAt = msTime(created)
	t.LastSeenAt = msTime(seen)
	return &t, nil
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not a condition to paper over with a weak id.
		panic("tenant: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// validLabel keeps labels short and printable: they end up in the UI, in log
// lines, and in the audit trail.
func validLabel(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// msTime converts epoch milliseconds to a time, mapping 0 to the zero time so
// "never used" is distinguishable from "used at the epoch".
func msTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique constraint")
}

// memSeq gives each in-memory registry its own database. `cache=shared` is
// required because database/sql pools connections and a private in-memory
// database exists per connection — but under cache=shared the NAME identifies the
// database, so a single fixed name would silently merge two registries (and leak
// rows between tests). dash learned this the hard way; same fix here.
var memSeq int64
var memMu sync.Mutex

func memDSN() string {
	memMu.Lock()
	memSeq++
	n := memSeq
	memMu.Unlock()
	return fmt.Sprintf("file:tenantmem%d?mode=memory&cache=shared&_pragma=foreign_keys(1)", n)
}
