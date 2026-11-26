package keepalive

import "crypto/rand"

// maskKey masks the held ping bodies — the transcripts — while they sit idle in memory. A heap
// dump, a crash report or a stray `strings` then yields masked bytes instead of a readable
// conversation. It does NOT stop code running in this process: the key is right here. It is
// obfuscation at rest, not encryption.
//
// Private to the keeper on purpose: credentials, and how a host masks them, are each host's own
// business. This guards held DATA, which the keeper owns.
var maskKey = func() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("context-guru: no entropy for the keep-alive body mask: " + err.Error())
	}
	return b
}()

// mask masks or unmasks in place — XOR is its own inverse.
func mask(b []byte) {
	for i := range b {
		b[i] ^= maskKey[i%len(maskKey)]
	}
}

// zero overwrites a buffer. Dropping the reference is not enough: the bytes stay in the heap until
// a collection that may never come.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
