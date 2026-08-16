package tenant

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"time"
)

// Browser logins for the dashboard.
//
// A dashboard session is a SEPARATE credential from a proxy token, and that is the
// point. Signing in must not require pasting a proxy token into a browser on every
// visit, and revoking a leaked CI token must not sign someone out of their dashboard.
//
// The cookie value is random and its sha256 is what the table holds — same rule as
// proxy tokens, for the same reason: a dump of this database cannot be replayed as a
// login.

// DefaultWebSessionTTL is how long a dashboard login lasts.
const DefaultWebSessionTTL = 30 * 24 * time.Hour

// ErrNoSession is returned when a cookie does not identify a live login.
var ErrNoSession = errors.New("tenant: no such dashboard session")

// NewWebSession authenticates a plaintext PROXY token and, if it is valid, opens a
// dashboard login for its tenant. Returning the cookie value once, like a token.
//
// Going through the proxy token means the browser never becomes a second place a
// long-lived credential has to be stored, and it means "can you prove you hold the
// token" is the only question the login flow has to answer.
func (r *Registry) NewWebSession(token string, ttl time.Duration) (*Tenant, string, error) {
	t, err := r.Resolve(token)
	if err != nil {
		return nil, "", err
	}
	if ttl <= 0 {
		ttl = DefaultWebSessionTTL
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, "", err
	}
	cookie := tokenEnc.EncodeToString(raw[:])
	sum := sha256.Sum256([]byte(cookie))
	now := time.Now()
	if _, err := r.db.Exec(`INSERT INTO dash_sessions (id,tenant_id,created_at,expires_at)
	  VALUES (?,?,?,?)`, sum[:], t.ID, now.UnixMilli(), now.Add(ttl).UnixMilli()); err != nil {
		return nil, "", err
	}
	return t, cookie, nil
}

// WebSession resolves a cookie value to its tenant, refusing an expired one.
//
// Expiry is enforced HERE, in the query, rather than relying on a sweeper. A sweep
// that fails or falls behind would otherwise silently extend every login, and a
// missed cleanup should cost disk, never access.
func (r *Registry) WebSession(cookie string) (*Tenant, error) {
	if cookie == "" {
		return nil, ErrNoSession
	}
	sum := sha256.Sum256([]byte(cookie))
	var id string
	err := r.db.QueryRow(`SELECT tenant_id FROM dash_sessions WHERE id = ? AND expires_at > ?`,
		sum[:], time.Now().UnixMilli()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, err
	}
	t, err := r.Get(id)
	if err != nil {
		return nil, err
	}
	if t.Disabled {
		return nil, ErrDisabled
	}
	return t, nil
}

// EndWebSession signs one browser out. Idempotent: signing out twice, or with a
// cookie that was already expired, is a success — the caller's intent is satisfied
// either way.
func (r *Registry) EndWebSession(cookie string) error {
	if cookie == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(cookie))
	_, err := r.db.Exec(`DELETE FROM dash_sessions WHERE id = ?`, sum[:])
	return err
}

// EndAllWebSessions signs a tenant out everywhere. Used when an account is
// disabled: leaving a live dashboard login behind would mean a disabled user could
// still read their history until the cookie lapsed.
func (r *Registry) EndAllWebSessions(tenantID string) error {
	_, err := r.db.Exec(`DELETE FROM dash_sessions WHERE tenant_id = ?`, tenantID)
	return err
}

// SweepWebSessions deletes expired logins. Purely a disk-reclaim job — WebSession
// already refuses an expired row, so skipping this costs space, not safety.
func (r *Registry) SweepWebSessions() (int64, error) {
	res, err := r.db.Exec(`DELETE FROM dash_sessions WHERE expires_at <= ?`, time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
