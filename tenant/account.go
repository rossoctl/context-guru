package tenant

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Email + password accounts for the DASHBOARD.
//
// This is a second, separate credential from the cg_live_ proxy token, and the split
// is the whole design:
//
//	proxy token  → agents authenticate to the PROXY with it. Machine-readable, lives
//	               in CI config and agent settings, 128 random bits, no second factor
//	               because there is no human at the keyboard to give one.
//	email+password → a human signs in to the DASHBOARD with it, plus a mailed code.
//	               Never accepted by the proxy, never spends money.
//
// A leaked token must not be a dashboard login and a leaked password must not be an
// inference credential, which is only true while nothing here can be exchanged for
// the other.
//
// The registration flow is two steps for a reason. RegisterAccount creates the
// account with NO token and email_verified_at = 0; the first token is minted only by
// VerifyRegistration, once a code we mailed to the address comes back. So an
// unverified account is inert: it cannot sign in and it cannot send traffic.

// RegisterAccount creates an unverified account with a password and no token. The
// caller is expected to follow immediately with IssueCode + a mail send.
//
// Re-registering an existing address is refused unless the account behind it has NEVER
// been usable: no password set and not one token row, live or revoked. "Unverified"
// alone is not that test and must never be used as one — migration v3 added
// email_verified_at with DEFAULT 0, so every account created before email auth is
// unverified while being in daily use. Gating on unverified would make this endpoint a
// password reset for those accounts: type a colleague's address, get their row back with
// your own password installed, and the owner is locked out of both doors at once
// (loginWithToken refuses an account that has a password; they do not know it).
//
// The cost of the strict test is that an abandoned half-registration reserves the
// address until an operator clears it. That is the right way round: a squatted address
// is a support ticket, a claimable live account is a takeover.
func (r *Registry) RegisterAccount(label, email, password string) (*Tenant, error) {
	t, err := r.newTenant(label, email)
	if err != nil {
		return nil, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	err = insertTenant(r.db, t, hash)
	if err == nil {
		return t, nil
	}
	if !errors.Is(err, ErrEmailTaken) {
		return nil, err
	}
	// Taken: claimable only by an account that has never been usable. The conditions are
	// in the UPDATE rather than checked first, so a token minted or a password set
	// between the read and the write loses the race instead of being overwritten.
	existing, err := r.ByEmail(t.Email)
	if err != nil {
		return nil, err
	}
	if existing.Verified() {
		return nil, ErrEmailTaken
	}
	res, err := r.db.Exec(`UPDATE tenants SET password_hash = ?, label = ?
	  WHERE id = ? AND email_verified_at = 0 AND password_hash = ''
	    AND NOT EXISTS (SELECT 1 FROM tenant_tokens WHERE tenant_id = tenants.id)`,
		hash, t.Label, existing.ID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrEmailTaken
	}
	r.clearCache()
	existing.Label, existing.HasPassword = t.Label, true
	return existing, nil
}

// VerifyLogin is the FIRST factor: it checks an email and password and nothing else.
// It opens no session — the caller must still put a mailed code in front of one.
//
// Every failure returns ErrWrongPass, whether the address is unknown, the password is
// wrong, or no password has been set. One error for all three, so this endpoint cannot
// be used to learn which addresses have accounts. ErrDisabled and ErrNotVerified are
// the two exceptions, because a user who cannot sign in needs to be told which of the
// two things to do about it, and both require having got the password right first.
//
// The argon2 verify runs even for an unknown address, against a throwaway hash. Not
// doing so makes "unknown email" answer in microseconds and "wrong password" in ~50
// ms, which turns the deliberately vague error message into a precise account
// enumeration oracle.
func (r *Registry) VerifyLogin(email, password string) (*Tenant, error) {
	var id, hash string
	err := r.db.QueryRow(`SELECT id,password_hash FROM tenants WHERE email = ?`,
		strings.ToLower(strings.TrimSpace(email))).Scan(&id, &hash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if hash == "" {
		hash = decoyHash
	}
	if !VerifyPassword(hash, password) || id == "" {
		return nil, ErrWrongPass
	}
	t, err := r.Get(id)
	if err != nil {
		return nil, err
	}
	if t.Disabled {
		return nil, ErrDisabled
	}
	if !t.Verified() {
		return nil, ErrNotVerified
	}
	return t, nil
}

// decoyHash is a valid argon2id encoding of a value nobody knows, used to spend the
// same ~50 ms on an unknown address as on a real one. Generated at init from random
// bytes rather than being a literal in the source, so it is not a credential anyone
// could type and not a constant to explain to a scanner.
var decoyHash = func() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("tenant: crypto/rand unavailable: " + err.Error())
	}
	h, err := HashPassword(tokenEnc.EncodeToString(b[:]))
	if err != nil {
		panic("tenant: " + err.Error())
	}
	return h
}()

// SetPassword replaces an account's password. Used by the change-password path and by
// a verified user who registered before passwords existed.
func (r *Registry) SetPassword(tenantID, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	res, err := r.db.Exec(`UPDATE tenants SET password_hash = ? WHERE id = ?`, hash, tenantID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	r.clearCache()
	return nil
}

// VerifyRegistration completes a registration: it consumes the mailed code, marks the
// address verified, and mints the account's FIRST proxy token, returned once.
//
// One function rather than three calls at the HTTP layer, because "verified" and "has
// a usable token" must not be able to disagree — a handler that verified and then
// failed to mint would leave an account that can sign in but cannot send traffic, and
// its retry would mint a second token. Idempotent-ish on the token: an already
// verified account gets no new one.
func (r *Registry) VerifyRegistration(tenantID, code string) (*Tenant, string, error) {
	if err := r.VerifyCode(tenantID, PurposeRegister, code); err != nil {
		return nil, "", err
	}
	t, err := r.Get(tenantID)
	if err != nil {
		return nil, "", err
	}
	if t.Verified() {
		return t, "", nil
	}
	if _, err := r.db.Exec(`UPDATE tenants SET email_verified_at = ? WHERE id = ?`,
		time.Now().UnixMilli(), tenantID); err != nil {
		return nil, "", err
	}
	r.clearCache()
	plain, err := r.MintToken(tenantID, t.Label)
	if err != nil {
		return nil, "", err
	}
	t, err = r.Get(tenantID)
	if err != nil {
		return nil, "", err
	}
	return t, plain, nil
}
