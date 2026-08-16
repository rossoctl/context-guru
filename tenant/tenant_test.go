package tenant

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T, o Options) *Registry {
	t.Helper()
	r, err := Open("", o)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func register(t *testing.T, r *Registry, email string) (*Tenant, string) {
	t.Helper()
	tn, tok, err := r.Register("laptop", email)
	if err != nil {
		t.Fatalf("Register(%q): %v", email, err)
	}
	return tn, tok
}

func TestRegisterResolveRoundTrip(t *testing.T) {
	r := open(t, Options{})
	tn, tok := register(t, r, "Someone@IBM.com")

	if !strings.HasPrefix(tok, tokenPrefix) || len(tok) != tokenLen {
		t.Fatalf("token %q is not the documented shape", tok)
	}
	if tn.Email != "someone@ibm.com" {
		t.Errorf("email not normalised: %q", tn.Email)
	}
	if tn.Role != RoleUser {
		t.Errorf("role = %q, want user", tn.Role)
	}
	// No stored config: a new tenant TRACKS the default rather than owning a copy.
	// See TestRegisterStoresNoConfigAndTracksTheDefault.
	if tn.ConfigYAML != "" {
		t.Errorf("new tenant got a copy of the default config: %q", tn.ConfigYAML)
	}

	got, err := r.Resolve(tok)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ID != tn.ID {
		t.Errorf("resolved %q, want %q", got.ID, tn.ID)
	}
	// Second call comes from cache; it must agree.
	if got2, err := r.Resolve(tok); err != nil || got2.ID != tn.ID {
		t.Errorf("cached Resolve = %v, %v", got2, err)
	}
}

// The security property worth a test: after registering, the plaintext token must
// not exist anywhere in the database file. This is what fails if someone ever adds
// a convenience column.
func TestTokenPlaintextNeverHitsDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control.db")
	r, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_, tok := register(t, r, "a@ibm.com")
	if _, err := r.MintToken(mustTenant(t, r, "a@ibm.com").ID, "ci"); err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Check the main file and any WAL/SHM sidecars.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(strings.TrimPrefix(tok, tokenPrefix))
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(tok)) {
			t.Errorf("%s contains the plaintext token", e.Name())
		}
		// The 8-char public prefix is stored on purpose; the rest of the body must
		// not be. Look for the tail past the prefix.
		if bytes.Contains(b, body[prefixLen:]) {
			t.Errorf("%s contains the secret part of the token body", e.Name())
		}
	}
}

func TestResolveRejectsJunkWithoutTouchingTheDB(t *testing.T) {
	r := open(t, Options{})
	register(t, r, "a@ibm.com")
	for _, bad := range []string{
		"", "sk-ant-api03-whatever", "cg_live_", "Bearer cg_live_AAAA",
		"cg_live_" + strings.Repeat("A", 25), // one char short
		"cg_live_" + strings.Repeat("A", 27), // one char long
	} {
		if _, err := r.Resolve(bad); !errors.Is(err, ErrUnknownToken) {
			t.Errorf("Resolve(%q) = %v, want ErrUnknownToken", bad, err)
		}
	}
	// A well-shaped token that was never issued is also unknown.
	if _, err := r.Resolve(tokenPrefix + strings.Repeat("A", tokenBody)); !errors.Is(err, ErrUnknownToken) {
		t.Errorf("unissued token: got %v, want ErrUnknownToken", err)
	}
}

func TestRevokeTakesEffectImmediately(t *testing.T) {
	r := open(t, Options{CacheTTL: time.Hour}) // long TTL: revocation must not wait for it
	tn, tok := register(t, r, "a@ibm.com")
	if _, err := r.Resolve(tok); err != nil {
		t.Fatalf("Resolve before revoke: %v", err)
	}
	toks, err := r.Tokens(tn.ID)
	if err != nil || len(toks) != 1 {
		t.Fatalf("Tokens = %v, %v", toks, err)
	}
	if err := r.RevokeToken(tn.ID, toks[0].Prefix); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if _, err := r.Resolve(tok); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("revoked token still resolves: %v", err)
	}
	if err := r.RevokeToken(tn.ID, toks[0].Prefix); !errors.Is(err, ErrNotFound) {
		t.Errorf("re-revoking: got %v, want ErrNotFound", err)
	}
	// Revocation is recorded, not deleted.
	toks, _ = r.Tokens(tn.ID)
	if len(toks) != 1 || !toks[0].Revoked() {
		t.Errorf("revoked token disappeared from the audit view: %+v", toks)
	}
}

func TestSecondTokenWorksAndIsIndependent(t *testing.T) {
	r := open(t, Options{CacheTTL: time.Hour})
	tn, first := register(t, r, "a@ibm.com")
	second, err := r.MintToken(tn.ID, "ci")
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	if second == first {
		t.Fatal("minted a duplicate token")
	}
	for _, tok := range []string{first, second} {
		if got, err := r.Resolve(tok); err != nil || got.ID != tn.ID {
			t.Fatalf("Resolve: %v, %v", got, err)
		}
	}
	toks, _ := r.Tokens(tn.ID)
	if len(toks) != 2 {
		t.Fatalf("want 2 tokens, got %d", len(toks))
	}
	// Revoking one must leave the other working — the point of having two.
	var ciPrefix string
	for _, k := range toks {
		if k.Label == "ci" {
			ciPrefix = k.Prefix
		}
	}
	if err := r.RevokeToken(tn.ID, ciPrefix); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(second); !errors.Is(err, ErrUnknownToken) {
		t.Errorf("revoked ci token still works: %v", err)
	}
	if _, err := r.Resolve(first); err != nil {
		t.Errorf("laptop token broke when ci was revoked: %v", err)
	}
}

func TestDisabledTenantFailsClosed(t *testing.T) {
	r := open(t, Options{ManagerEmail: "boss@ibm.com", CacheTTL: time.Hour})
	mgr, _ := register(t, r, "boss@ibm.com")
	tn, tok := register(t, r, "a@ibm.com")
	if _, err := r.Resolve(tok); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	yes := true
	if err := r.Update(mgr, tn.ID, Patch{Disabled: &yes}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := r.Resolve(tok); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled tenant resolves: %v", err)
	}
}

func TestEmailRules(t *testing.T) {
	r := open(t, Options{EmailDomains: []string{"ibm.com"}, ManagerEmail: "boss@ibm.com"})
	if _, _, err := r.Register("l", "someone@example.com"); !errors.Is(err, ErrEmailDomain) {
		t.Errorf("outside domain: got %v, want ErrEmailDomain", err)
	}
	// A subdomain of an allowed domain is allowed; a lookalike suffix is not.
	if _, _, err := r.Register("l", "someone@uk.ibm.com"); err != nil {
		t.Errorf("subdomain rejected: %v", err)
	}
	if _, _, err := r.Register("l", "someone@notibm.com"); !errors.Is(err, ErrEmailDomain) {
		t.Errorf("suffix lookalike accepted: %v", err)
	}
	for _, bad := range []string{"", "nope", "@ibm.com", "a@", "a b@ibm.com", "a@b@ibm.com"} {
		if _, _, err := r.Register("l", bad); !errors.Is(err, ErrBadEmail) && !errors.Is(err, ErrEmailDomain) {
			t.Errorf("Register(%q) = %v, want a rejection", bad, err)
		}
	}
	if _, _, err := r.Register("l", "dup@ibm.com"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Register("l", "DUP@ibm.com"); !errors.Is(err, ErrEmailTaken) {
		t.Errorf("duplicate email (different case): got %v, want ErrEmailTaken", err)
	}
	// The configured manager email becomes a manager; nobody else does.
	mgr, _ := register(t, r, "BOSS@ibm.com")
	if !mgr.IsManager() {
		t.Error("manager email did not get the manager role")
	}
	other, _ := register(t, r, "other@ibm.com")
	if other.IsManager() {
		t.Error("an ordinary registration became a manager")
	}
}

func TestBadLabelRejected(t *testing.T) {
	r := open(t, Options{})
	for _, bad := range []string{"", "  ", strings.Repeat("x", 65), "line\nbreak", "bell\x07"} {
		if _, _, err := r.Register(bad, "a@ibm.com"); !errors.Is(err, ErrBadLabel) {
			t.Errorf("Register(label=%q) = %v, want ErrBadLabel", bad, err)
		}
	}
}

func TestUpdatePrivilegeBoundaries(t *testing.T) {
	r := open(t, Options{ManagerEmail: "boss@ibm.com"})
	mgr, _ := register(t, r, "boss@ibm.com")
	a, _ := register(t, r, "a@ibm.com")
	b, _ := register(t, r, "b@ibm.com")

	cfg := "mode: observe\n"
	// A user may edit their own config.
	if err := r.Update(a, a.ID, Patch{ConfigYAML: &cfg}); err != nil {
		t.Fatalf("self config edit: %v", err)
	}
	// But not someone else's.
	if err := r.Update(a, b.ID, Patch{ConfigYAML: &cfg}); !errors.Is(err, ErrForbidden) {
		t.Errorf("cross-tenant edit: got %v, want ErrForbidden", err)
	}
	// Nor their own role, quota, or disabled flag.
	mgrRole, rows, off := RoleManager, int64(1<<40), false
	for name, p := range map[string]Patch{
		"role":     {Role: &mgrRole},
		"max_rows": {MaxRows: &rows},
		"disabled": {Disabled: &off},
	} {
		if err := r.Update(a, a.ID, p); !errors.Is(err, ErrForbidden) {
			t.Errorf("self-escalation via %s: got %v, want ErrForbidden", name, err)
		}
	}
	// A manager may do all of it.
	if err := r.Update(mgr, a.ID, Patch{Role: &mgrRole, MaxRows: &rows}); err != nil {
		t.Fatalf("manager update: %v", err)
	}
	got, _ := r.Get(a.ID)
	if !got.IsManager() || got.MaxRows != rows {
		t.Errorf("manager update did not apply: %+v", got)
	}
	if err := r.Update(nil, a.ID, Patch{ConfigYAML: &cfg}); !errors.Is(err, ErrForbidden) {
		t.Errorf("nil actor: got %v, want ErrForbidden", err)
	}
}

func TestUpdateValidatesConfigAndAudits(t *testing.T) {
	rejected := errors.New("nope")
	r := open(t, Options{Validate: func(b []byte) error {
		if strings.Contains(string(b), "bogus") {
			return rejected
		}
		return nil
	}})
	a, _ := register(t, r, "a@ibm.com")

	bad := "pipeline: [bogus]\n"
	if err := r.Update(a, a.ID, Patch{ConfigYAML: &bad}); !errors.Is(err, rejected) {
		t.Fatalf("invalid config accepted: %v", err)
	}
	if got, _ := r.Get(a.ID); got.ConfigYAML != "" {
		t.Errorf("a rejected config was still written: %q", got.ConfigYAML)
	}

	good := "mode: observe\n"
	if err := r.Update(a, a.ID, Patch{ConfigYAML: &good}); err != nil {
		t.Fatal(err)
	}
	entries, err := r.Audit(a.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Field != "config_yaml" ||
		entries[0].Before != "" || entries[0].After != good ||
		entries[0].Actor != a.ID {
		t.Fatalf("audit entry wrong: %+v", entries)
	}
	// A patch that changes nothing writes nothing.
	if err := r.Update(a, a.ID, Patch{ConfigYAML: &good}); err != nil {
		t.Fatal(err)
	}
	if entries, _ = r.Audit(a.ID, 10); len(entries) != 1 {
		t.Errorf("no-op patch produced %d audit rows, want 1", len(entries))
	}
}

func TestConfigFallsOpen(t *testing.T) {
	r := open(t, Options{DefaultConfig: "mode: sync\n"})
	a, _ := register(t, r, "a@ibm.com")
	blank := "   \n"
	if err := r.Update(a, a.ID, Patch{ConfigYAML: &blank}); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Get(a.ID)
	if cfg := r.Config(got); cfg != "mode: sync\n" {
		t.Errorf("blank config did not fall back to the default: %q", cfg)
	}
	if cfg := r.Config(nil); cfg != "mode: sync\n" {
		t.Errorf("nil tenant: %q", cfg)
	}
}

// Registration must store NO configuration: a tenant tracks the server default until
// they deliberately customise. Stamping a copy of the default into the row is what
// froze every tenant on the default of their registration day.
func TestRegisterStoresNoConfigAndTracksTheDefault(t *testing.T) {
	r := open(t, Options{})
	a, _ := register(t, r, "a@ibm.com")
	if a.ConfigYAML != "" {
		t.Errorf("Register stored a config: %q — a new tenant must track the default", a.ConfigYAML)
	}
	stored, err := r.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ConfigYAML != "" {
		t.Errorf("stored config = %q, want empty", stored.ConfigYAML)
	}
	if !stored.TracksDefault() {
		t.Error("a newly registered tenant does not report as tracking the default")
	}
	if got := r.Config(stored); got != DefaultConfigYAML {
		t.Errorf("Config = %q, want the current server default", got)
	}
}

// The point of tracking: an improvement to the default reaches a tenant who never
// customised, without anyone editing their row.
func TestChangingTheServerDefaultReachesATrackingTenant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	const before, after = "mode: sync\n", "pipeline: [format, toon]\nmode: sync\n"

	r, err := Open(path, Options{DefaultConfig: before})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := register(t, r, "a@ibm.com")
	if got := r.Config(a); got != before {
		t.Fatalf("Config = %q, want %q", got, before)
	}
	r.Close()

	// Same database, a server whose default has moved on.
	r2, err := Open(path, Options{DefaultConfig: after})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	got, err := r2.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cfg := r2.Config(got); cfg != after {
		t.Errorf("a tracking tenant still runs %q; the new default did not reach them", cfg)
	}
}

// The other half of the contract: someone who customised owns their document and a
// later change to the server default must not move them.
func TestACustomisedTenantIsNotMovedByADefaultChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	mine := "pipeline: [dedup]\nmode: observe\n"

	r, err := Open(path, Options{DefaultConfig: "mode: sync\n"})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := register(t, r, "a@ibm.com")
	if err := r.Update(a, a.ID, Patch{ConfigYAML: &mine}); err != nil {
		t.Fatal(err)
	}
	r.Close()

	r2, err := Open(path, Options{DefaultConfig: "pipeline: [format, toon]\nmode: sync\n"})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	got, err := r2.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TracksDefault() {
		t.Error("a tenant with a stored config reports as tracking the default")
	}
	if cfg := r2.Config(got); cfg != mine {
		t.Errorf("Config = %q, want the tenant's own %q", cfg, mine)
	}
}

// And it is reversible: clearing the stored config puts a tenant back on the default,
// including a default that has changed since they customised.
func TestClearingACustomisationReturnsToTracking(t *testing.T) {
	const def = "pipeline: [format, toon]\nmode: sync\n"
	r := open(t, Options{DefaultConfig: def})
	a, _ := register(t, r, "a@ibm.com")
	mine := "mode: observe\n"
	if err := r.Update(a, a.ID, Patch{ConfigYAML: &mine}); err != nil {
		t.Fatal(err)
	}
	empty := ""
	if err := r.Update(a, a.ID, Patch{ConfigYAML: &empty}); err != nil {
		t.Fatal(err)
	}
	got, err := r.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.TracksDefault() {
		t.Fatalf("stored config after clearing = %q, want empty", got.ConfigYAML)
	}
	if cfg := r.Config(got); cfg != def {
		t.Errorf("Config = %q, want the server default %q", cfg, def)
	}
	// Going back to tracking is a config change like any other, so it is auditable —
	// "who put me back on the default" has to be answerable too.
	entries, err := r.Audit(a.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	// Both changes land in the same millisecond, so ORDER BY ts cannot say which is
	// which — look for the clearing entry rather than assuming a position.
	var cleared bool
	for _, e := range entries {
		cleared = cleared || (e.Field == "config_yaml" && e.Before == mine && e.After == "")
	}
	if len(entries) != 2 || !cleared {
		t.Errorf("clearing was not audited as a config change: %+v", entries)
	}
}

func TestReopenPersistsAndMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	r, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, tok := register(t, r, "a@ibm.com")
	r.Close()

	r2, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer r2.Close()
	if _, err := r2.Resolve(tok); err != nil {
		t.Fatalf("token did not survive a restart: %v", err)
	}
	// A third open must also be clean — migrations are version-guarded, not
	// CREATE-IF-NOT-EXISTS-and-hope.
	r3, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("third open: %v", err)
	}
	r3.Close()
}

func TestMigrateRefusesNewerDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	r, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a future version having written this file.
	if _, err := r.db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if _, err := Open(path, Options{}); err == nil {
		t.Fatal("opened a database written by a newer build")
	}
}

func TestInMemoryRegistriesAreIsolated(t *testing.T) {
	a := open(t, Options{})
	b := open(t, Options{})
	register(t, a, "x@ibm.com")
	if got, err := b.List(); err != nil || len(got) != 0 {
		t.Fatalf("second in-memory registry saw %d tenants (%v) — shared cache name", len(got), err)
	}
}

func TestListAndLookups(t *testing.T) {
	r := open(t, Options{})
	a, _ := register(t, r, "a@ibm.com")
	register(t, r, "b@ibm.com")
	all, err := r.List()
	if err != nil || len(all) != 2 {
		t.Fatalf("List = %d, %v", len(all), err)
	}
	if got, err := r.ByEmail("A@IBM.COM"); err != nil || got.ID != a.ID {
		t.Errorf("ByEmail = %v, %v", got, err)
	}
	if _, err := r.ByEmail("nobody@ibm.com"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ByEmail(missing) = %v, want ErrNotFound", err)
	}
	if _, err := r.Get("deadbeef"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) = %v, want ErrNotFound", err)
	}
	if _, err := r.MintToken("deadbeef", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("MintToken(missing tenant) = %v, want ErrNotFound", err)
	}
}

func TestLastUsedIsStamped(t *testing.T) {
	r := open(t, Options{})
	tn, tok := register(t, r, "a@ibm.com")
	if toks, _ := r.Tokens(tn.ID); !toks[0].LastUsedAt.IsZero() {
		t.Fatal("a fresh token already has a last-used time")
	}
	if _, err := r.Resolve(tok); err != nil {
		t.Fatal(err)
	}
	toks, _ := r.Tokens(tn.ID)
	if toks[0].LastUsedAt.IsZero() {
		t.Error("last_used_at was not stamped on a cache miss")
	}
}

func mustTenant(t *testing.T, r *Registry, email string) *Tenant {
	t.Helper()
	tn, err := r.ByEmail(email)
	if err != nil {
		t.Fatalf("ByEmail(%q): %v", email, err)
	}
	return tn
}

// Agent keys: bound by digest, resolvable, never stored in plaintext. This is the path
// for an agent that can set no header of our choosing (Bob).
func TestAgentKeyBindResolveUnbind(t *testing.T) {
	r := open(t, Options{ManagerEmail: "boss@ibm.com"})
	tn, _, err := r.Register("laptop", "a@ibm.com")
	if err != nil {
		t.Fatal(err)
	}
	const key = "caller-provider-key-value"
	if _, err := r.ResolveAgentKey(key); !errors.Is(err, ErrNoAgentKey) {
		t.Fatalf("unbound key resolved: %v", err)
	}
	if err := r.BindAgentKey(tn.ID, key); err != nil {
		t.Fatal(err)
	}
	got, err := r.ResolveAgentKey(key)
	if err != nil || got.ID != tn.ID {
		t.Fatalf("ResolveAgentKey = %v, %v", got, err)
	}
	if n, err := r.AgentKeyCount(tn.ID); err != nil || n != 1 {
		t.Fatalf("AgentKeyCount = %d, %v", n, err)
	}
	// Binding twice is idempotent, not a second row.
	if err := r.BindAgentKey(tn.ID, key); err != nil {
		t.Fatal(err)
	}
	if n, _ := r.AgentKeyCount(tn.ID); n != 1 {
		t.Errorf("re-binding created %d rows", n)
	}
	// The plaintext must not be in the database anywhere.
	var hits int
	if err := r.db.QueryRow(
		`SELECT COUNT(*) FROM tenant_agent_keys WHERE CAST(key_hash AS TEXT) LIKE ?`,
		"%"+key+"%").Scan(&hits); err != nil {
		t.Fatal(err)
	}
	if hits != 0 {
		t.Error("the provider key was stored in plaintext")
	}
	if err := r.UnbindAgentKeys(tn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResolveAgentKey(key); !errors.Is(err, ErrNoAgentKey) {
		t.Fatalf("key still resolved after unbinding: %v", err)
	}
}

// A disabled account's bound key must stop working, exactly as its token does.
func TestAgentKeyRespectsDisabled(t *testing.T) {
	r := open(t, Options{ManagerEmail: "boss@ibm.com"})
	mgr, _, err := r.Register("m", "boss@ibm.com")
	if err != nil {
		t.Fatal(err)
	}
	tn, _, err := r.Register("laptop", "a@ibm.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.BindAgentKey(tn.ID, "k"); err != nil {
		t.Fatal(err)
	}
	off := true
	if err := r.Update(mgr, tn.ID, Patch{Disabled: &off}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResolveAgentKey("k"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled account's agent key = %v, want ErrDisabled", err)
	}
}
