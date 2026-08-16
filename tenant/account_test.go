package tenant

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The password is a string this test invents and never stores anywhere but the
// in-memory database it also throws away. It is not a credential to anything.
const testPass = "correct-horse-battery"

func accountFixture(t *testing.T) *Registry {
	t.Helper()
	r, err := Open("", Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// --- password storage --------------------------------------------------------

// The stored value must be a memory-hard KDF output with a per-account salt, and it
// must not be, or contain, the password. A bare sha256 would pass a naive "is it
// different from the plaintext" check, so this asserts the ALGORITHM by name and that
// two accounts with the same password get different hashes.
func TestPasswordIsHashedWithArgon2idAndSalted(t *testing.T) {
	r := accountFixture(t)
	for _, e := range []string{"a@ibm.com", "b@ibm.com"} {
		if _, err := r.RegisterAccount("laptop", e, testPass); err != nil {
			t.Fatalf("RegisterAccount(%s): %v", e, err)
		}
	}
	var hashes []string
	for _, e := range []string{"a@ibm.com", "b@ibm.com"} {
		var h string
		if err := r.db.QueryRow(`SELECT password_hash FROM tenants WHERE email = ?`, e).Scan(&h); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(h, testPass) {
			t.Fatal("the stored hash contains the password")
		}
		if !strings.HasPrefix(h, "$argon2id$v=") {
			t.Errorf("stored hash is not argon2id: %q", firstField(h))
		}
		if !strings.Contains(h, "m=65536,t=3,p=2") {
			t.Errorf("unexpected argon2 parameters: %q", firstField(h))
		}
		hashes = append(hashes, h)
	}
	if hashes[0] == hashes[1] {
		t.Error("two accounts with the same password share a hash — the salt is not per-account")
	}
}

// firstField trims a hash down to its parameter prefix, so a failure message never
// prints the digest itself.
func firstField(h string) string {
	if i := strings.LastIndexByte(h, '$'); i > 0 {
		if j := strings.LastIndexByte(h[:i], '$'); j > 0 {
			return h[:j]
		}
	}
	return "(unparseable)"
}

func TestVerifyPasswordAcceptsOnlyTheRightPassword(t *testing.T) {
	h, err := HashPassword(testPass)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(h, testPass) {
		t.Fatal("the right password did not verify")
	}
	for _, wrong := range []string{"", testPass + "x", strings.ToUpper(testPass), "short"} {
		if VerifyPassword(h, wrong) {
			t.Errorf("a wrong password verified (len %d)", len(wrong))
		}
	}
	// A corrupt or truncated hash is a failed sign-in, never a panic and never a pass.
	for _, bad := range []string{"", "notahash", "$argon2id$v=19$m=1$x$y", h[:len(h)-4]} {
		if VerifyPassword(bad, testPass) {
			t.Error("a malformed stored hash verified")
		}
	}
}

// A stored hash carries its own cost parameters, which makes the row an instruction to
// allocate memory. Unbounded, one poisoned row turns every sign-in attempt against that
// account into a 1 GiB allocation, so anything far above our own parameters is treated
// as corrupt rather than obeyed.
func TestAbsurdArgonParametersAreRejectedNotObeyed(t *testing.T) {
	good, err := HashPassword(testPass)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(good, "$")
	for _, params := range []string{
		"m=1048576,t=3,p=2", // 1 GiB
		"m=65536,t=10,p=2",
		"m=65536,t=3,p=64",
	} {
		parts[3] = params
		if _, _, _, _, _, err := decodeHash(strings.Join(parts, "$")); !errors.Is(err, ErrBadPassHash) {
			t.Errorf("decodeHash accepted %s: err = %v", params, err)
		}
	}
	// Our own parameters must still decode, or this rejects every real password.
	if _, _, _, _, _, err := decodeHash(good); err != nil {
		t.Errorf("decodeHash rejected our own parameters: %v", err)
	}
}

func TestShortPasswordsAreRefused(t *testing.T) {
	r := accountFixture(t)
	if _, err := r.RegisterAccount("l", "a@ibm.com", strings.Repeat("x", MinPasswordLen-1)); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("a %d-character password was accepted: %v", MinPasswordLen-1, err)
	}
}

// --- registration + login ----------------------------------------------------

// A registered-but-unverified account is inert: no token, and VerifyLogin refuses it
// even with the right password. Otherwise "register" alone would be an account.
func TestUnverifiedAccountCannotSignIn(t *testing.T) {
	r := accountFixture(t)
	tn, err := r.RegisterAccount("laptop", "a@ibm.com", testPass)
	if err != nil {
		t.Fatal(err)
	}
	if tn.Verified() {
		t.Error("a fresh registration is already verified")
	}
	toks, err := r.Tokens(tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(toks) != 0 {
		t.Errorf("an unverified account holds %d proxy tokens, want 0", len(toks))
	}
	if _, err := r.VerifyLogin("a@ibm.com", testPass); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("unverified sign-in = %v, want ErrNotVerified", err)
	}
}

func TestVerifyRegistrationVerifiesAndMintsTheFirstToken(t *testing.T) {
	r := accountFixture(t)
	tn, err := r.RegisterAccount("laptop", "a@ibm.com", testPass)
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.IssueCode(tn.ID, PurposeRegister)
	if err != nil {
		t.Fatal(err)
	}
	vt, token, err := r.VerifyRegistration(tn.ID, c.Plain)
	if err != nil {
		t.Fatalf("VerifyRegistration: %v", err)
	}
	if !vt.Verified() {
		t.Error("the account is not marked verified")
	}
	if !strings.HasPrefix(token, tokenPrefix) {
		t.Errorf("no proxy token minted: %q", token)
	}
	if _, err := r.Resolve(token); err != nil {
		t.Errorf("the minted token does not resolve: %v", err)
	}
	if _, err := r.VerifyLogin("a@ibm.com", testPass); err != nil {
		t.Errorf("a verified account cannot sign in: %v", err)
	}
	if _, err := r.VerifyLogin("a@ibm.com", "wrong-password-here"); !errors.Is(err, ErrWrongPass) {
		t.Error("the wrong password signed in")
	}
	// One error for an unknown address too, so this cannot enumerate accounts.
	if _, err := r.VerifyLogin("nobody@ibm.com", testPass); !errors.Is(err, ErrWrongPass) {
		t.Error("an unknown address returns a distinguishable error")
	}
}

// Re-registering an address that already has a password is refused, verified or not:
// the password is the thing a claim would overwrite, so an account that has one is not
// free to take. Re-registering a VERIFIED one must not work either — that would be a
// password reset by anyone who knows a colleague's address.
func TestReregisterRefusesAnAddressThatAlreadyHasAPassword(t *testing.T) {
	r := accountFixture(t)
	first, err := r.RegisterAccount("laptop", "a@ibm.com", testPass)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RegisterAccount("desktop", "a@ibm.com", "second-password-x"); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("re-registering an address with a password = %v, want ErrEmailTaken", err)
	}

	c, _ := r.IssueCode(first.ID, PurposeRegister)
	if _, _, err := r.VerifyRegistration(first.ID, c.Plain); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RegisterAccount("attacker", "a@ibm.com", "third-password-xy"); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("a VERIFIED address was re-claimable: %v", err)
	}
	if _, err := r.VerifyLogin("a@ibm.com", testPass); err != nil {
		t.Errorf("the account's original password was overwritten: %v", err)
	}
}

// An account that is already IN USE must not be claimable by re-registration, even
// though it is unverified. Every account created before the email-auth work migrates to
// email_verified_at = 0 (see migration v3), so "unverified" alone cannot be the gate:
// an unauthenticated caller who types a colleague's address would otherwise install
// their own password on a live account and lock the owner out of both doors.
func TestReregisterCannotClaimAnAccountAlreadyInUse(t *testing.T) {
	r := accountFixture(t)
	victim, token, err := r.Register("laptop", "victim@ibm.com")
	if err != nil {
		t.Fatal(err)
	}
	// Exactly the state migration v3 leaves a pre-existing account in: a live token,
	// no password, unverified.
	if _, err := r.db.Exec(`UPDATE tenants SET email_verified_at = 0 WHERE id = ?`, victim.ID); err != nil {
		t.Fatal(err)
	}
	r.clearCache()

	if _, err := r.RegisterAccount("attacker", "victim@ibm.com", "attacker-chosen-pw"); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("an in-use account was claimable by re-registration: err = %v", err)
	}
	got, err := r.Get(victim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HasPassword {
		t.Error("a password was installed on an account the caller does not own")
	}
	if _, err := r.Resolve(token); err != nil {
		t.Errorf("the victim's token stopped working: %v", err)
	}
}

// An account holding no password AND no token has never been usable, so the address is
// still free to claim.
func TestReregisterStillClaimsANeverUsableAccount(t *testing.T) {
	r := accountFixture(t)
	stub, _, err := r.Register("laptop", "stub@ibm.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(`DELETE FROM tenant_tokens WHERE tenant_id = ?`, stub.ID); err != nil {
		t.Fatal(err)
	}
	r.clearCache()
	again, err := r.RegisterAccount("desktop", "stub@ibm.com", testPass)
	if err != nil {
		t.Fatalf("claiming a never-usable account: %v", err)
	}
	if again.ID != stub.ID {
		t.Error("claiming created a second account for one address")
	}
}

// --- emailed codes -----------------------------------------------------------

// The owner asked for five minutes, so five minutes is pinned here rather than left to
// whatever the constant happens to say.
func TestCodeExpiresAfterFiveMinutes(t *testing.T) {
	if CodeTTL != 5*time.Minute {
		t.Fatalf("CodeTTL = %v, want 5m", CodeTTL)
	}
	r := accountFixture(t)
	tn := verifiedAccount(t, r, "a@ibm.com")
	c, err := r.IssueCode(tn.ID, PurposeLogin)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(c.ExpiresAt); d > CodeTTL || d < CodeTTL-time.Minute {
		t.Errorf("code expires in %v, want ~%v", d, CodeTTL)
	}
	// Still good just before the deadline, dead just after. The clock is moved by
	// rewriting the row rather than by sleeping: a test that sleeps five minutes is a
	// test nobody runs.
	setExpiry(t, r, tn.ID, PurposeLogin, time.Now().Add(time.Second))
	if err := r.VerifyCode(tn.ID, PurposeLogin, c.Plain); err != nil {
		t.Fatalf("a code one second from expiry was refused: %v", err)
	}

	c, _ = r.IssueCode(tn.ID, PurposeLogin)
	setExpiry(t, r, tn.ID, PurposeLogin, time.Now().Add(-time.Millisecond))
	if err := r.VerifyCode(tn.ID, PurposeLogin, c.Plain); !errors.Is(err, ErrCodeExpired) {
		t.Fatalf("an expired code = %v, want ErrCodeExpired", err)
	}
	// And it is gone, not merely refused — an expired row must not be re-guessable.
	if n := countCodes(t, r, tn.ID); n != 0 {
		t.Errorf("%d code rows survived expiry", n)
	}
}

func TestCodeIsOneTimeUse(t *testing.T) {
	r := accountFixture(t)
	tn := verifiedAccount(t, r, "a@ibm.com")
	c, _ := r.IssueCode(tn.ID, PurposeLogin)
	if err := r.VerifyCode(tn.ID, PurposeLogin, c.Plain); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if err := r.VerifyCode(tn.ID, PurposeLogin, c.Plain); !errors.Is(err, ErrNoCode) {
		t.Fatalf("the same code verified twice: %v", err)
	}
}

// Issuing a new code must invalidate the previous one, or a resend widens the set of
// valid codes instead of replacing it.
func TestIssuingACodeReplacesThePendingOne(t *testing.T) {
	r := accountFixture(t)
	tn := verifiedAccount(t, r, "a@ibm.com")
	first, _ := r.IssueCode(tn.ID, PurposeLogin)
	second, _ := r.IssueCode(tn.ID, PurposeLogin)
	if first.Plain == second.Plain {
		t.Skip("the two codes collided (1 in a million); nothing to assert")
	}
	if err := r.VerifyCode(tn.ID, PurposeLogin, first.Plain); err == nil {
		t.Fatal("the superseded code still verified")
	}
	if err := r.VerifyCode(tn.ID, PurposeLogin, second.Plain); err != nil {
		t.Fatalf("the current code was refused: %v", err)
	}
}

// The attempt cap is what makes 20 bits a second factor at all. After
// MaxCodeAttempts wrong guesses the code must be VOID, not merely refused — otherwise
// an attacker keeps grinding the same live challenge.
func TestWrongCodesLockTheCodeOut(t *testing.T) {
	r := accountFixture(t)
	tn := verifiedAccount(t, r, "a@ibm.com")
	c, _ := r.IssueCode(tn.ID, PurposeLogin)
	wrong := "000000"
	if wrong == c.Plain {
		wrong = "111111"
	}
	for i := 1; i < MaxCodeAttempts; i++ {
		if err := r.VerifyCode(tn.ID, PurposeLogin, wrong); !errors.Is(err, ErrBadCode) {
			t.Fatalf("attempt %d = %v, want ErrBadCode", i, err)
		}
	}
	if err := r.VerifyCode(tn.ID, PurposeLogin, wrong); !errors.Is(err, ErrCodeAttempts) {
		t.Fatalf("attempt %d = %v, want ErrCodeAttempts", MaxCodeAttempts, err)
	}
	if n := countCodes(t, r, tn.ID); n != 0 {
		t.Errorf("%d code rows survived the attempt cap", n)
	}
	// And the RIGHT code no longer works either: the challenge is destroyed, so the
	// user has to start over rather than the attacker getting five more guesses.
	if err := r.VerifyCode(tn.ID, PurposeLogin, c.Plain); !errors.Is(err, ErrNoCode) {
		t.Fatalf("the correct code still worked after lockout: %v", err)
	}
}

// A login code must not be spendable as a registration code, or the second factor for
// signing in becomes a way to mint a token on an unverified account.
func TestCodePurposesDoNotCross(t *testing.T) {
	r := accountFixture(t)
	tn, err := r.RegisterAccount("l", "a@ibm.com", testPass)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := r.IssueCode(tn.ID, PurposeLogin)
	if _, _, err := r.VerifyRegistration(tn.ID, c.Plain); err == nil {
		t.Fatal("a login code completed a registration")
	}
}

func TestCodesAreSixDigitsAndNotStoredInTheClear(t *testing.T) {
	r := accountFixture(t)
	tn := verifiedAccount(t, r, "a@ibm.com")
	c, _ := r.IssueCode(tn.ID, PurposeLogin)
	if len(c.Plain) != 6 || strings.Trim(c.Plain, "0123456789") != "" {
		t.Fatalf("code is not six digits: %q", c.Plain)
	}
	var stored []byte
	if err := r.db.QueryRow(`SELECT code_hash FROM email_codes WHERE tenant_id = ?`,
		tn.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), c.Plain) {
		t.Error("the code is stored in a form that contains the digits")
	}
}

// --- sessions on several machines --------------------------------------------

// Signing in on a second machine must not sign the first one out, and each row has to
// carry enough to tell them apart.
func TestManyMachinesAtOnceAndPerSessionRevoke(t *testing.T) {
	r := accountFixture(t)
	tn := verifiedAccount(t, r, "a@ibm.com")

	laptop, err := r.OpenWebSession(tn.ID, SessionMeta{
		Label: "laptop", UserAgent: "Firefox/1", IP: "10.0.0.1"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	desktop, err := r.OpenWebSession(tn.ID, SessionMeta{
		Label: "desktop", UserAgent: "Chrome/2", IP: "10.0.0.2"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	phone, err := r.OpenWebSession(tn.ID, SessionMeta{Label: "phone", IP: "10.0.0.3"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if laptop == desktop || desktop == phone {
		t.Fatal("two logins share a cookie")
	}
	for _, c := range []string{laptop, desktop, phone} {
		if _, err := r.WebSession(c); err != nil {
			t.Fatalf("a concurrent session is not valid: %v", err)
		}
	}

	ss, err := r.Sessions(tn.ID, desktop)
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 3 {
		t.Fatalf("Sessions returned %d rows, want 3", len(ss))
	}
	byLabel := map[string]Session{}
	current := 0
	for _, s := range ss {
		byLabel[s.Label] = s
		if s.Current {
			current++
		}
		if s.CreatedAt.IsZero() || s.LastSeenAt.IsZero() || s.ExpiresAt.IsZero() {
			t.Errorf("session %q is missing a timestamp: %+v", s.Label, s)
		}
	}
	if current != 1 || !byLabel["desktop"].Current {
		t.Errorf("the current session is not marked exactly once (%d marked)", current)
	}
	if byLabel["laptop"].UserAgent != "Firefox/1" || byLabel["laptop"].IP != "10.0.0.1" {
		t.Errorf("the session did not record its machine: %+v", byLabel["laptop"])
	}
	if byLabel["laptop"].SID == "" || byLabel["laptop"].SID == laptop {
		t.Error("the public handle is empty, or IS the cookie")
	}

	// Revoke exactly one; the other two keep working.
	if err := r.EndWebSessionBySID(tn.ID, byLabel["laptop"].SID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := r.WebSession(laptop); !errors.Is(err, ErrNoSession) {
		t.Error("the revoked session still resolves")
	}
	for _, c := range []string{desktop, phone} {
		if _, err := r.WebSession(c); err != nil {
			t.Errorf("revoking one session broke another: %v", err)
		}
	}
	if err := r.EndWebSessionBySID(tn.ID, byLabel["laptop"].SID); !errors.Is(err, ErrNoSession) {
		t.Error("revoking an already-revoked session did not report it as missing")
	}
}

// One tenant must not be able to revoke another's session by guessing a handle.
func TestSessionRevokeIsScopedToItsOwner(t *testing.T) {
	r := accountFixture(t)
	mine := verifiedAccount(t, r, "a@ibm.com")
	theirs := verifiedAccount(t, r, "b@ibm.com")
	cookie, err := r.OpenWebSession(theirs.ID, SessionMeta{Label: "victim"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.EndWebSessionBySID(mine.ID, SID(cookie)); !errors.Is(err, ErrNoSession) {
		t.Fatalf("cross-tenant revoke = %v, want ErrNoSession", err)
	}
	if _, err := r.WebSession(cookie); err != nil {
		t.Fatalf("another tenant revoked this session: %v", err)
	}
}

func TestSessionUserAgentIsTruncated(t *testing.T) {
	r := accountFixture(t)
	tn := verifiedAccount(t, r, "a@ibm.com")
	cookie, err := r.OpenWebSession(tn.ID, SessionMeta{
		Label: "l", UserAgent: strings.Repeat("A", 5000)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	ss, _ := r.Sessions(tn.ID, cookie)
	if len(ss) != 1 || len(ss[0].UserAgent) != maxUAStored {
		t.Fatalf("stored a %d-character User-Agent", len(ss[0].UserAgent))
	}
}

// --- helpers -----------------------------------------------------------------

func verifiedAccount(t *testing.T, r *Registry, email string) *Tenant {
	t.Helper()
	tn, err := r.RegisterAccount("laptop", email, testPass)
	if err != nil {
		t.Fatalf("RegisterAccount: %v", err)
	}
	c, err := r.IssueCode(tn.ID, PurposeRegister)
	if err != nil {
		t.Fatal(err)
	}
	vt, _, err := r.VerifyRegistration(tn.ID, c.Plain)
	if err != nil {
		t.Fatalf("VerifyRegistration: %v", err)
	}
	return vt
}

// setExpiry rewrites a pending code's deadline, which is how these tests reach five
// minutes into the future without waiting five minutes.
func setExpiry(t *testing.T, r *Registry, tenantID string, p CodePurpose, at time.Time) {
	t.Helper()
	if _, err := r.db.Exec(`UPDATE email_codes SET expires_at = ? WHERE tenant_id = ? AND purpose = ?`,
		at.UnixMilli(), tenantID, string(p)); err != nil {
		t.Fatal(err)
	}
}

func countCodes(t *testing.T, r *Registry, tenantID string) int {
	t.Helper()
	var n int
	if err := r.db.QueryRow(`SELECT count(*) FROM email_codes WHERE tenant_id = ?`,
		tenantID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
