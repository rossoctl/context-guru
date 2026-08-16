package tenant

import (
	"errors"
	"strings"
	"testing"
)

// Registry-level manager control: deletion and its cascade, the manager-only fields, and
// the password paths that used to be dead code.

func mgrRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := Open("", Options{ManagerEmail: "boss@example.test"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// Deleting an account takes every control-plane row that hangs off it. The cascade is what
// removes them, so this is really a test that foreign keys are on and the references are
// declared — get either wrong and a deleted account leaves live tokens behind.
func TestDeleteCascadesEveryCredential(t *testing.T) {
	r := mgrRegistry(t)
	boss, _, err := r.Register("laptop", "boss@example.test")
	if err != nil {
		t.Fatal(err)
	}
	victim, tok, err := r.Register("laptop", "user@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.BindAgentKey(victim.ID, strings.Repeat("k", MinAgentKeyLen)); err != nil {
		t.Fatal(err)
	}
	cookie, err := r.OpenWebSession(victim.ID, SessionMeta{Label: "laptop"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.IssueCode(victim.ID, PurposeLogin); err != nil {
		t.Fatal(err)
	}

	if err := r.Delete(boss, victim.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Get(victim.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("the account row survived: %v", err)
	}
	// The token stops resolving IMMEDIATELY — the resolve cache is cleared by Delete, so a
	// deleted account cannot keep sending traffic for the cache TTL.
	if _, err := r.Resolve(tok); !errors.Is(err, ErrUnknownToken) {
		t.Errorf("a deleted account's token still resolves: %v", err)
	}
	if _, err := r.WebSession(cookie); err == nil {
		t.Error("a deleted account's dashboard session still resolves")
	}
	if n, err := r.AgentKeyCount(victim.ID); err != nil || n != 0 {
		t.Errorf("agent-key bindings survived: %d %v", n, err)
	}
	for _, table := range []string{"tenant_tokens", "dash_sessions", "tenant_agent_keys", "email_codes"} {
		var n int
		if err := r.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE tenant_id = ?`,
			victim.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s kept %d rows for a deleted account", table, n)
		}
	}
	// The trail OUTLIVES the account. tenant_config_audit has no foreign key on
	// target_tenant precisely so it can, and a deletion nobody can look up afterwards is
	// the one change that most needs a record.
	entries, err := r.Audit(victim.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range entries {
		if e.Field == "account" && e.After == "deleted" && e.Actor == boss.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("the deletion is not in the audit trail: %+v", entries)
	}
}

// Deletion is manager-only, and a manager may not delete themselves: these routes are the
// only way to appoint another manager, so the last one leaving locks the deployment out of
// its own administration.
func TestDeleteIsManagerOnlyAndNeverSelf(t *testing.T) {
	r := mgrRegistry(t)
	boss, _, err := r.Register("laptop", "boss@example.test")
	if err != nil {
		t.Fatal(err)
	}
	user, _, err := r.Register("laptop", "user@example.test")
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := r.Register("laptop", "other@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(user, other.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("a plain user deleting somebody = %v, want ErrForbidden", err)
	}
	if err := r.Delete(user, user.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("a plain user deleting themselves = %v, want ErrForbidden", err)
	}
	if err := r.Delete(boss, boss.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("a manager deleting themselves = %v, want ErrForbidden", err)
	}
	if err := r.Delete(nil, other.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("delete with no actor = %v, want ErrForbidden", err)
	}
	if _, err := r.Get(other.ID); err != nil {
		t.Errorf("a refused delete removed the account anyway: %v", err)
	}
}

// The A/B group and the disabled note are manager fields, and the variant name is
// validated: it is a grouping key that ends up in a chart legend and a query string, not
// free text.
func TestVariantAndReasonAreManagerOnlyAndValidated(t *testing.T) {
	r := mgrRegistry(t)
	boss, _, err := r.Register("laptop", "boss@example.test")
	if err != nil {
		t.Fatal(err)
	}
	user, _, err := r.Register("laptop", "user@example.test")
	if err != nil {
		t.Fatal(err)
	}
	arm := "arm-a"
	if err := r.Update(user, user.ID, Patch{Variant: &arm}); !errors.Is(err, ErrForbidden) {
		t.Errorf("a user assigning their own variant = %v, want ErrForbidden", err)
	}
	if err := r.Update(boss, user.ID, Patch{Variant: &arm}); err != nil {
		t.Fatalf("manager assigning a variant: %v", err)
	}
	got, err := r.Get(user.ID)
	if err != nil || got.Variant != arm {
		t.Fatalf("variant = %q (%v), want %q", got.Variant, err, arm)
	}
	for _, bad := range []string{"arm a", "arm/b", "../x", strings.Repeat("v", 33)} {
		v := bad
		if err := r.Update(boss, user.ID, Patch{Variant: &v}); !errors.Is(err, ErrBadVariant) {
			t.Errorf("variant %q = %v, want ErrBadVariant", bad, err)
		}
	}
	// Clearing it is legal: "" means no variant.
	empty := ""
	if err := r.Update(boss, user.ID, Patch{Variant: &empty}); err != nil {
		t.Errorf("clearing a variant: %v", err)
	}
	// The reason is bounded, because it travels into an HTTP body.
	long := strings.Repeat("x", 201)
	if err := r.Update(boss, user.ID, Patch{DisabledReason: &long}); !errors.Is(err, ErrBadReason) {
		t.Errorf("an over-long reason = %v, want ErrBadReason", err)
	}
}

// A disabled account's refusal carries the manager's reason, and unwraps to ErrDisabled so
// every existing branch on that sentinel is unaffected.
func TestDisabledErrorCarriesTheReasonAndUnwraps(t *testing.T) {
	r := mgrRegistry(t)
	boss, _, err := r.Register("laptop", "boss@example.test")
	if err != nil {
		t.Fatal(err)
	}
	user, tok, err := r.Register("laptop", "user@example.test")
	if err != nil {
		t.Fatal(err)
	}
	yes, reason := true, "paused pending review"
	if err := r.Update(boss, user.ID, Patch{Disabled: &yes, DisabledReason: &reason}); err != nil {
		t.Fatal(err)
	}
	_, err = r.Resolve(tok)
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("Resolve on a disabled account = %v, want ErrDisabled", err)
	}
	if !strings.Contains(err.Error(), reason) {
		t.Errorf("the refusal does not carry the reason: %v", err)
	}
	// An account with no reason answers exactly as it always did.
	other, otherTok, err := r.Register("laptop", "other@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Update(boss, other.ID, Patch{Disabled: &yes}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(otherTok); err != ErrDisabled { //nolint:errorlint // identity is the point
		t.Errorf("a reasonless refusal = %v, want the bare sentinel", err)
	}
}

// The password paths. SetPassword was dead code whose comment claimed a change-password
// flow that did not exist; these are the two callers that now make the claim true.
func TestChangePasswordChecksTheOldOneAndKeepsThisSession(t *testing.T) {
	r := mgrRegistry(t)
	const first = "first-password-here"
	const second = "second-password-here"
	tn, err := r.RegisterAccount("laptop", "user@example.test", first)
	if err != nil {
		t.Fatal(err)
	}
	mine, err := r.OpenWebSession(tn.ID, SessionMeta{Label: "mine"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := r.OpenWebSession(tn.ID, SessionMeta{Label: "elsewhere"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ChangePassword(tn.ID, mine, "wrong-password", second); !errors.Is(err, ErrWrongPass) {
		t.Fatalf("change with a wrong old password = %v, want ErrWrongPass", err)
	}
	if err := r.ChangePassword(tn.ID, mine, first, "short"); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("change to a short password = %v, want ErrBadPassword", err)
	}
	if err := r.ChangePassword(tn.ID, mine, first, second); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	// The session that made the change survives; the other one does not.
	if _, err := r.WebSession(mine); err != nil {
		t.Errorf("the changing browser was signed out: %v", err)
	}
	if _, err := r.WebSession(theirs); err == nil {
		t.Error("another machine stayed signed in through a password change")
	}
	// An account with no password cannot use this route at all: there is nothing to check.
	tokenOnly, _, err := r.Register("laptop", "old@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ChangePassword(tokenOnly.ID, "", "", second); !errors.Is(err, ErrNoPassword) {
		t.Errorf("change on a passwordless account = %v, want ErrNoPassword", err)
	}
}

// A reset spends a code of its OWN purpose and nothing else: that separation is what stops
// a login code being spent as a reset, and a reset code being spent as a second factor.
func TestResetPasswordIsPurposeSeparatedAndSingleUse(t *testing.T) {
	r := mgrRegistry(t)
	const first = "first-password-here"
	const next = "recovered-password-x"
	tn, err := r.RegisterAccount("laptop", "user@example.test", first)
	if err != nil {
		t.Fatal(err)
	}
	// Verified, because VerifyLogin refuses an unverified account whatever its password is
	// — and this test is about the password, not about that gate.
	reg, err := r.IssueCode(tn.ID, PurposeRegister)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.VerifyRegistration(tn.ID, reg.Plain); err != nil {
		t.Fatal(err)
	}
	live, err := r.OpenWebSession(tn.ID, SessionMeta{Label: "somewhere"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	login, err := r.IssueCode(tn.ID, PurposeLogin)
	if err != nil {
		t.Fatal(err)
	}
	// A LOGIN code is not a reset code, even though both are six digits for the same account.
	if err := r.ResetPassword(tn.ID, login.Plain, next); !errors.Is(err, ErrNoCode) {
		t.Errorf("a login code spent as a reset = %v, want ErrNoCode", err)
	}
	reset, err := r.IssueCode(tn.ID, PurposeReset)
	if err != nil {
		t.Fatal(err)
	}
	// A too-short password is refused BEFORE the code is spent, so a user does not lose
	// their one code to a typo.
	if err := r.ResetPassword(tn.ID, reset.Plain, "short"); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("reset to a short password = %v, want ErrBadPassword", err)
	}
	if err := r.ResetPassword(tn.ID, reset.Plain, next); err != nil {
		t.Fatalf("ResetPassword after a rejected password: %v (the code was spent anyway)", err)
	}
	if _, err := r.VerifyLogin("user@example.test", next); err != nil {
		t.Errorf("the reset password does not sign in: %v", err)
	}
	if _, err := r.VerifyLogin("user@example.test", first); !errors.Is(err, ErrWrongPass) {
		t.Errorf("the old password still signs in: %v", err)
	}
	// Single use, and every session goes: a recovery exists to evict whoever else was in.
	if err := r.ResetPassword(tn.ID, reset.Plain, next); err == nil {
		t.Error("a reset code was reusable")
	}
	if _, err := r.WebSession(live); err == nil {
		t.Error("a session survived a password reset")
	}
}

// The v5 migration is additive: an existing account gains "no variant, no reason" and
// nothing else changes. Guarding it here because the number will shift when another
// migration lands beside it.
func TestVariantMigrationIsAdditive(t *testing.T) {
	r := mgrRegistry(t)
	tn, _, err := r.Register("laptop", "user@example.test")
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Get(tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Variant != "" || got.DisabledReason != "" {
		t.Errorf("a new account starts with variant %q reason %q, want both empty",
			got.Variant, got.DisabledReason)
	}
	var version int
	if err := r.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != len(migrations) {
		t.Errorf("user_version = %d, want %d", version, len(migrations))
	}
}
