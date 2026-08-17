package tenant

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
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
	cookie, err := r.openSession(t.ID, SessionMeta{Label: t.Label}, ttl)
	if err != nil {
		return nil, "", err
	}
	return t, cookie, nil
}

// SessionMeta is what a new login records about the machine it came from, so the
// owner can tell their sessions apart well enough to revoke the right one. All of it
// is UNTRUSTED — client-supplied text (User-Agent) or a network fact (IP). It is
// display material for the account's owner, never an authorisation input.
type SessionMeta struct {
	// Label is a short human name for the machine ("laptop"). Falls back to
	// "browser" when empty or unprintable.
	Label string
	// UserAgent is truncated on write: a real browser string is bounded, an
	// attacker's is not.
	UserAgent string
	IP        string
}

// Session is one signed-in machine, as its owner sees it.
type Session struct {
	// SID is the public handle: the first 16 hex characters of sha256(cookie). It is
	// DERIVED rather than stored, so there is no second column to keep in step with
	// the key, and it is safe to show because it is a truncation of a hash of the
	// only thing that authenticates — holding the SID proves nothing and unlocks
	// nothing.
	SID        string
	Label      string
	UserAgent  string
	IP         string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	// Current marks the session making the request, so the UI can label it and warn
	// before it signs itself out.
	Current bool
}

// maxUAStored bounds the User-Agent text kept per session. Long enough for every real
// browser string, short enough that a login cannot write a megabyte to this database.
const maxUAStored = 200

// sidLen is the length of the public session handle, in hex characters. 16 hex = 64
// bits of the id's hash: not guessable, and not enough to reconstruct anything.
const sidLen = 16

// OpenWebSession signs a tenant in on one machine WITHOUT checking a credential — the
// caller has already done that (password plus emailed code, or a proxy token).
// Returns the cookie value once.
//
// Sessions ACCUMULATE rather than replace: signing in on a desktop must not sign the
// laptop out. Several concurrent logins per account are the expected state, which is
// what the per-row metadata is for — a list of hashes is not something a user can
// revoke from.
func (r *Registry) OpenWebSession(tenantID string, m SessionMeta, ttl time.Duration) (string, error) {
	return r.openSession(tenantID, m, ttl)
}

func (r *Registry) openSession(tenantID string, m SessionMeta, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = DefaultWebSessionTTL
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	cookie := tokenEnc.EncodeToString(raw[:])
	sum := sha256.Sum256([]byte(cookie))
	if len(m.UserAgent) > maxUAStored {
		m.UserAgent = m.UserAgent[:maxUAStored]
	}
	if !validLabel(m.Label) {
		m.Label = "browser"
	}
	now := time.Now()
	if _, err := r.db.Exec(`INSERT INTO dash_sessions
	  (id,tenant_id,created_at,expires_at,label,user_agent,ip,last_seen_at)
	  VALUES (?,?,?,?,?,?,?,?)`, sum[:], tenantID, now.UnixMilli(),
		now.Add(ttl).UnixMilli(), m.Label, m.UserAgent, m.IP, now.UnixMilli()); err != nil {
		return "", err
	}
	return cookie, nil
}

// SID returns the public handle for a cookie, so a handler can mark which row in the
// list is the browser it is answering — without a query, and without the cookie
// leaving the request.
func SID(cookie string) string {
	if cookie == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(cookie))
	return hex.EncodeToString(sum[:])[:sidLen]
}

// Sessions lists a tenant's live logins, most recently active first. Expired rows are
// filtered in the QUERY, for the same reason WebSession filters there: the sweeper is
// a disk job, never the access control.
func (r *Registry) Sessions(tenantID, currentCookie string) ([]Session, error) {
	rows, err := r.db.Query(`SELECT id,label,user_agent,ip,created_at,last_seen_at,expires_at
	  FROM dash_sessions WHERE tenant_id = ? AND expires_at > ? ORDER BY last_seen_at DESC`,
		tenantID, time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cur := SID(currentCookie)
	var out []Session
	for rows.Next() {
		var id []byte
		var s Session
		var created, seen, exp int64
		if err := rows.Scan(&id, &s.Label, &s.UserAgent, &s.IP, &created, &seen, &exp); err != nil {
			return nil, err
		}
		s.SID = hex.EncodeToString(id)[:sidLen]
		s.CreatedAt, s.LastSeenAt, s.ExpiresAt = msTime(created), msTime(seen), msTime(exp)
		s.Current = cur != "" && s.SID == cur
		out = append(out, s)
	}
	return out, rows.Err()
}

// EndWebSessionBySID revokes ONE of a tenant's logins by its public handle.
//
// Scoped by tenant_id inside the WHERE clause rather than checked after the fact, so
// "revoke session X" cannot reach another account's row even if a handle is guessed.
//
// ponytail: matches on substr(hex(id)) so the handle needs no stored column, at the
// cost of a scan over this tenant's own sessions. A user has a handful; add an indexed
// sid column if that ever stops being true.
func (r *Registry) EndWebSessionBySID(tenantID, sid string) error {
	if len(sid) != sidLen {
		return ErrNoSession
	}
	res, err := r.db.Exec(`DELETE FROM dash_sessions
	  WHERE tenant_id = ? AND substr(hex(id),1,?) = ?`, tenantID, sidLen, strings.ToUpper(sid))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSession
	}
	return nil
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
	// Stamp "last seen", THROTTLED to once a minute per session by the WHERE clause.
	// This runs on every authenticated dashboard request, and the session list is only
	// useful if it shows which machine is actually active — but a write per request
	// would make reading the dashboard a writer on the control database. Best-effort:
	// a failed stamp is a stale column, not a failed sign-in.
	now := time.Now().UnixMilli()
	_, _ = r.db.Exec(`UPDATE dash_sessions SET last_seen_at = ?
	  WHERE id = ? AND last_seen_at < ?`, now, sum[:], now-60_000)
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
