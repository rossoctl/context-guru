package store

import "testing"

// #1365. One read of a clock record, as the dashboard and the manual-ping refusal do per thread.
func BenchmarkPeekValueOfASeenRecord(b *testing.B) {
	m := NewMemory(Options{MaxEntries: 100000})
	for i := 0; i < 50000; i++ {
		m.Put(SeenPrefix+string(rune('a'+i%26))+string(rune(i)), []byte("1700000000000:1h"))
	}
	m.Put(SeenPrefix+"x", []byte("1700000000000:1h"))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := PeekValue(m, SeenPrefix+"x"); !ok {
			b.Fatal("missing")
		}
	}
}
