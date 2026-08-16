package tenant

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Password storage for dashboard accounts.
//
// A dashboard password is the ONE long-lived user secret this database holds, so it
// is the one thing a dump must not reveal. Proxy tokens get away with a bare sha256
// because they are 128 random bits — there is no dictionary to run against them. A
// human-chosen password is the opposite, so a fast hash is the same as plaintext
// with extra steps.
//
// argon2id, from golang.org/x/crypto/argon2: memory-hard, so an attacker with GPUs
// pays for RAM per guess rather than getting thousands of guesses per core for free.
// The "id" variant because it is the one the RFC recommends by default — Argon2i's
// side-channel resistance for the first pass, Argon2d's resistance to
// time-memory tradeoffs for the rest.
//
// Nothing here ever returns, logs, or formats a plaintext password. The encoded
// hash is the only value that leaves this file, and it is not a credential: it
// cannot be replayed as one.

// Errors callers branch on.
var (
	ErrBadPassword = errors.New("tenant: password must be at least 12 characters")
	ErrNoPassword  = errors.New("tenant: account has no password set")
	ErrWrongPass   = errors.New("tenant: wrong email or password")
	ErrNotVerified = errors.New("tenant: email not verified")
	ErrBadPassHash = errors.New("tenant: unreadable password hash")
)

// argon2id parameters.
//
// 64 MiB / 3 passes / 2 lanes. Chosen against the RFC 9106 "second recommended"
// profile (64 MiB, t=3, p=4) and trimmed to 2 lanes because this runs on a shared
// box next to a proxy on the hot path, and lanes buy parallel speed for the DEFENDER
// only — the attacker parallelises across guesses regardless. Measured cost is
// roughly 40-60 ms and 64 MiB per verify on this hardware, which is why the code and
// password endpoints are rate-limited: without a limit, 64 MiB per attempt is itself
// a memory-exhaustion lever.
//
// These live in the encoded hash (PHC string format), so raising them later does not
// invalidate a single existing password — VerifyPassword reads the parameters the
// hash was made with. Re-hashing on next sign-in is deliberately NOT implemented;
// add it when the parameters actually change.
const (
	argonMemoryKiB = 64 * 1024
	argonTime      = 3
	argonThreads   = 2
	argonKeyLen    = 32
	argonSaltLen   = 16

	// A stored hash carries the parameters it was made with, which means a row in the
	// database is an instruction to allocate memory and burn CPU. Reachable only with
	// write access to the control DB, so these caps are defence in depth: without them
	// one poisoned row (m=1048576) turns every sign-in attempt against that account
	// into a 1 GiB allocation on a box that also runs the proxy. Expressed as multiples
	// of our own parameters, so raising those raises these with them; anything above is
	// treated as a corrupt row, which VerifyPassword already reports as a failed
	// sign-in rather than an error.
	maxArgonMemoryKiB = 4 * argonMemoryKiB
	maxArgonTime      = 2 * argonTime
	maxArgonThreads   = 2 * argonThreads

	// MinPasswordLen is the only password rule. Length is the property that
	// actually buys entropy; composition rules ("one symbol!") push users to
	// Password1! and buy nothing, so there are none.
	MinPasswordLen = 12
	// maxPasswordLen bounds what we will hash. argon2's cost does not grow with
	// input length, so this is only about not accepting a megabyte-long field.
	maxPasswordLen = 256
)

// HashPassword returns a PHC-encoded argon2id hash with a fresh random salt.
func HashPassword(pw string) (string, error) {
	if len(pw) < MinPasswordLen {
		return "", ErrBadPassword
	}
	if len(pw) > maxPasswordLen {
		return "", ErrBadPassword
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sum := argon2.IDKey([]byte(pw), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(sum)), nil
}

// VerifyPassword reports whether pw produced encoded.
//
// Returns false — never an error — for a malformed or empty stored hash, so a
// corrupt row is a failed sign-in rather than a way to distinguish accounts.
func VerifyPassword(encoded, pw string) bool {
	if encoded == "" || pw == "" || len(pw) > maxPasswordLen {
		return false
	}
	salt, want, mem, t, threads, err := decodeHash(encoded)
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, mem, threads, uint32(len(want)))
	// Constant time: the comparison is between a value derived from attacker input
	// and a stored secret, which is exactly the shape a timing oracle needs.
	return subtle.ConstantTimeCompare(got, want) == 1
}

// b64 is the PHC standard alphabet: raw (unpadded) standard base64.
var b64 = base64.RawStdEncoding

func decodeHash(encoded string) (salt, sum []byte, mem, time uint32, threads uint8, err error) {
	// $argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return nil, nil, 0, 0, 0, ErrBadPassHash
	}
	var ver int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &ver); err != nil || ver != argon2.Version {
		return nil, nil, 0, 0, 0, ErrBadPassHash
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return nil, nil, 0, 0, 0, ErrBadPassHash
	}
	if m == 0 || t == 0 || p == 0 ||
		m > maxArgonMemoryKiB || t > maxArgonTime || p > maxArgonThreads {
		return nil, nil, 0, 0, 0, ErrBadPassHash
	}
	if salt, err = b64.DecodeString(parts[4]); err != nil {
		return nil, nil, 0, 0, 0, ErrBadPassHash
	}
	if sum, err = b64.DecodeString(parts[5]); err != nil {
		return nil, nil, 0, 0, 0, ErrBadPassHash
	}
	if len(salt) == 0 || len(sum) == 0 {
		return nil, nil, 0, 0, 0, ErrBadPassHash
	}
	return salt, sum, m, t, p, nil
}
