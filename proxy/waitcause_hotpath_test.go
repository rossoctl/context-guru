package proxy

import (
	"strings"
	"testing"
)

// BenchmarkNoteWaitBodyLargeBody pins issue #424's own requirement: the request path's own
// cost for issue #424's RECORD half must not depend on body size, because the actual scan
// (waitcause.Tracker.Observe, benchmarked at ~11ms for a 783 KB body in
// internal/waitcause's own BenchmarkObserveLargeBody) now runs from finish(), after the
// request has already gone upstream — never from here.
//
// noteWaitBody is nil-safe and does exactly one thing: store the slice header. This
// benchmark's whole point is a FLAT ns/op across body sizes, which a reviewer can compare
// directly against BenchmarkObserveLargeBody's ms/op for the same size body.
func BenchmarkNoteWaitBodyLargeBody(b *testing.B) {
	body := []byte(strings.Repeat(`{"role":"user","content":"x"},`, 20000))
	c := &capture{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.noteWaitBody(body)
	}
}
