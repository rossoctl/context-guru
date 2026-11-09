package proxy

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
)

// credMaskKey masks a caller's credential while the proxy holds it between requests (keep-alive,
// and the cheap-model refs that async work keeps). Be precise about what this buys: a heap or core
// dump, a crash report, a `/proc/<pid>/mem` read or a stray `strings` over a snapshot yields masked
// bytes instead of a working key — the accidental-capture class, the realistic one. It does NOT
// stop an attacker with code execution in this process: the key is in the same address space. It
// is obfuscation at rest, not encryption.
//
// The proxy's own: credential custody is each host's business, so this is not shared with the
// core or with other hosts.
var credMaskKey = func() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("context-guru: no entropy for the credential mask: " + err.Error())
	}
	return b
}()

// credMask masks or unmasks in place — XOR is its own inverse, so one function does both.
func credMask(b []byte) {
	for i := range b {
		b[i] ^= credMaskKey[i%len(credMaskKey)]
	}
}

// credZero overwrites a buffer. Dropping the reference is not enough: the bytes sit in the heap
// until a collection that may never come, and a dump taken in between still has them.
func credZero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// heldHeader is one auth header held masked. The NAME is not a secret; the value is.
type heldHeader struct {
	name string
	val  []byte
}

// clone returns a copy whose value can be zeroized without touching the original.
func (h heldHeader) clone() heldHeader {
	return heldHeader{name: h.name, val: append([]byte(nil), h.val...)}
}

// maskHeaders captures every value in hs, masked.
func maskHeaders(hs http.Header) []heldHeader {
	var out []heldHeader
	for name, vs := range hs {
		for _, v := range vs {
			b := []byte(v)
			credMask(b)
			out = append(out, heldHeader{name: name, val: b})
		}
	}
	return out
}

func zeroHeaders(hs []heldHeader) {
	for _, h := range hs {
		credZero(h.val)
	}
}

// heldAuth is the caller's credential as the keep-alive holds it: only the auth slots, only after
// the proxy's own token has been scrubbed out of them, and masked. nil when the upstream injects a
// server-held key, because then there is nothing of the caller's to hold.
func heldAuth(up upstream, reqHeaders http.Header) []heldHeader {
	if up.setKey != nil {
		return nil
	}
	slots := http.Header{}
	for _, name := range authHeaders {
		for _, v := range reqHeaders.Values(name) {
			slots.Add(name, v)
		}
	}
	scrubToken(slots)
	return maskHeaders(slots)
}

// credIDKey keys credKey's HMAC: random per process, in memory only, never logged or stored.
var credIDKey = func() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("context-guru: no entropy for the keep-alive credential id key: " + err.Error())
	}
	return b
}()

// credKey is a per-process id for the credential a hold pings with (keepalive.Record.Cred): an
// HMAC (credIDKey) over the FULL auth header values. They are hashed in their masked form, which
// is the whole value XORed with a fixed per-process mask, a bijection: two keys collide only if
// they are the same key, and no plaintext copy is made to compute it. "" when the upstream
// injects a server-held key. Unlike main's, the id carries no tenant prefix: package keepalive
// scopes it by the record's tenant itself.
func credKey(up upstream, reqHeaders http.Header) string {
	auth := heldAuth(up, reqHeaders)
	defer zeroHeaders(auth)
	if len(auth) == 0 {
		return ""
	}
	parts := make([]string, 0, len(auth))
	for _, a := range auth {
		parts = append(parts, a.name+"\x00"+string(a.val))
	}
	sort.Strings(parts)
	m := hmac.New(sha256.New, credIDKey)
	m.Write([]byte(strings.Join(parts, "\x01")))
	return hex.EncodeToString(m.Sum(nil)[:16])
}
