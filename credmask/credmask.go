// Package credmask holds a credential (or a transcript) masked while it is idle in memory.
//
// Be precise about what this buys, because it is easy to oversell. A heap or core dump, a crash
// report, a `/proc/<pid>/mem` read or a stray `strings` over a snapshot yields masked bytes
// instead of a working key — that is the accidental-capture class, and it is the realistic one.
// It does NOT stop an attacker with code execution in this process: the mask is right here in
// the same address space. It is obfuscation at rest, not encryption.
//
// Shared by every keep-alive host (cmd/context-guru-proxy and the Cortex plugin), so the one
// security-sensitive routine exists once.
package credmask

import "crypto/rand"

var key = func() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("context-guru: no entropy for the keep-alive credential mask: " + err.Error())
	}
	return b
}()

// Mask masks or unmasks in place — XOR is its own inverse, so one function does both.
func Mask(b []byte) {
	for i := range b {
		b[i] ^= key[i%len(key)]
	}
}

// Zero overwrites a buffer. Dropping the reference is not enough: the bytes sit in the heap
// until a collection that may never come, and a dump taken in between still has them.
func Zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// Header is one auth header held masked. The NAME is not a secret; the value is.
type Header struct {
	Name string
	Val  []byte
}

// Clone returns a copy whose value can be zeroized without touching the original.
func (h Header) Clone() Header {
	return Header{Name: h.Name, Val: append([]byte(nil), h.Val...)}
}
