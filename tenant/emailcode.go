package tenant

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// Emailed one-time codes: the second factor for signing in, and the proof of address
// for registering.
//
// A 6-digit code is 20 bits. That is only a factor at all because of the three
// bounds enforced here and in the HTTP layer, and it is worth being blunt about
// which one does the work:
//
//   - EXPIRY (5 minutes) bounds how long a guess is worth making.
//   - ATTEMPTS (MaxCodeAttempts, counted on the row) is the real control. Five
//     guesses out of a million is 1-in-200,000 per code, and the sixth wrong guess
//     destroys the code rather than merely refusing it — so an attacker cannot keep
//     grinding one live challenge.
//   - RATE LIMITS per email and per client address (proxy/control.go) bound how fast
//     an attacker can request fresh codes to spend those five guesses against.
//
// Remove any one of the three and the code stops being a factor. Without the attempt
// cap in particular, 10^6 with unlimited tries is a formality, not a check.
//
// One pending code per (tenant, purpose): issuing a new one REPLACES the old, so a
// resend invalidates the previous mail rather than widening the set of valid codes.

// CodeTTL is how long an emailed code stays valid. The owner asked for 5 minutes;
// the API returns the absolute expiry so the UI can count down against it rather
// than assuming this constant.
const CodeTTL = 5 * time.Minute

// MaxCodeAttempts is how many wrong guesses a single code tolerates before it is
// destroyed. Low on purpose: a human reading a code out of their mail does not need
// five tries, and every extra try is 1-in-200,000 handed to someone who is not them.
const MaxCodeAttempts = 5

// CodePurpose separates the two flows so a registration code cannot be spent as a
// login second factor (or the reverse) — same tenant, same table, different meaning.
type CodePurpose string

const (
	PurposeRegister CodePurpose = "register"
	PurposeLogin    CodePurpose = "login"
)

// Errors callers branch on. ErrBadCode and ErrCodeExpired are deliberately different
// so the UI can say "that code has expired, here is a new one" instead of "wrong" —
// this is not an oracle worth protecting: the attacker already knows the clock.
var (
	ErrBadCode      = errors.New("tenant: wrong verification code")
	ErrCodeExpired  = errors.New("tenant: verification code expired")
	ErrCodeAttempts = errors.New("tenant: too many wrong codes; that code is now void")
	ErrNoCode       = errors.New("tenant: no verification code pending")
)

// Code is a freshly issued challenge. The Plain field is the ONLY place the digits
// exist outside the user's mailbox: it is not stored (only its hash is), and the
// caller's single job is to hand it to the mailer and drop it.
type Code struct {
	Plain     string
	ExpiresAt time.Time
}

// IssueCode mints a code for a tenant and purpose, replacing any pending one.
func (r *Registry) IssueCode(tenantID string, p CodePurpose) (Code, error) {
	digits, err := sixDigits()
	if err != nil {
		return Code{}, err
	}
	now := time.Now()
	exp := now.Add(CodeTTL)
	sum := sha256.Sum256([]byte(string(p) + ":" + digits))
	// REPLACE on the (tenant_id,purpose) primary key: one live code per flow.
	if _, err := r.db.Exec(`INSERT OR REPLACE INTO email_codes
	  (tenant_id,purpose,code_hash,attempts,created_at,expires_at) VALUES (?,?,?,0,?,?)`,
		tenantID, string(p), sum[:], now.UnixMilli(), exp.UnixMilli()); err != nil {
		return Code{}, err
	}
	return Code{Plain: digits, ExpiresAt: exp}, nil
}

// VerifyCode consumes a code. Success DELETES the row, which is what makes a code
// one-time: replaying it a second later finds nothing and fails.
//
// Runs in a transaction because the attempt counter is the security control, and an
// increment that races with a concurrent guess is an increment an attacker gets for
// free.
func (r *Registry) VerifyCode(tenantID string, p CodePurpose, code string) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var stored []byte
	var attempts int
	var exp int64
	err = tx.QueryRow(`SELECT code_hash,attempts,expires_at FROM email_codes
	  WHERE tenant_id = ? AND purpose = ?`, tenantID, string(p)).Scan(&stored, &attempts, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNoCode
	}
	if err != nil {
		return err
	}
	drop := func(ret error) error {
		if _, err := tx.Exec(`DELETE FROM email_codes WHERE tenant_id = ? AND purpose = ?`,
			tenantID, string(p)); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return ret
	}
	if exp <= time.Now().UnixMilli() {
		return drop(ErrCodeExpired)
	}
	if attempts >= MaxCodeAttempts {
		// Belt and braces: the row is destroyed on the Nth failure below, so reaching
		// here means something else wrote it. Treat it as void either way.
		return drop(ErrCodeAttempts)
	}
	sum := sha256.Sum256([]byte(string(p) + ":" + code))
	if subtle.ConstantTimeCompare(sum[:], stored) != 1 {
		if attempts+1 >= MaxCodeAttempts {
			return drop(ErrCodeAttempts)
		}
		if _, err := tx.Exec(`UPDATE email_codes SET attempts = attempts + 1
		  WHERE tenant_id = ? AND purpose = ?`, tenantID, string(p)); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return ErrBadCode
	}
	return drop(nil)
}

// SweepCodes deletes lapsed codes. Disk reclaim only — VerifyCode refuses an expired
// row on its own, so a sweep that never runs costs space, never access. Same contract
// as SweepWebSessions, and called from the same place.
func (r *Registry) SweepCodes() (int64, error) {
	res, err := r.db.Exec(`DELETE FROM email_codes WHERE expires_at <= ?`, time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// sixDigits returns a uniformly random 6-digit code, leading zeros kept.
//
// crypto/rand.Int rather than a masked byte read: `uint32 % 1000000` is biased
// towards the low 967,296 codes, and "which third of the keyspace to guess first" is
// exactly the hint not to give away on a 20-bit secret.
func sixDigits() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}
