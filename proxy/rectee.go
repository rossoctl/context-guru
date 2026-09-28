package proxy

// recTee retains a streamed response body for --record.
//
// WHY A SEPARATE TYPE FROM sniffer. sniffer keeps a bounded head+tail WINDOW, which is
// the right shape for finding a usage block and the wrong shape for a trace: a window
// spliced from two ends is not a reconstructable turn. recTee keeps the bytes in order,
// or says it could not.
//
// WHY BOUNDED ANYWAY. Unbounded retention on a proxy is a memory hazard an adversarial
// (or merely long) response can trigger, and the sniffer's own comment says so. The cap
// is therefore explicit and a hit is RECORDED rather than silently dropping bytes:
// a turn that says "truncated at N" is data, a turn that quietly lost its tail is a
// corpus defect that nobody discovers until analysis.
type recTee struct {
	buf       []byte
	cap       int
	total     int
	truncated bool
}

func newRecTee(capBytes int) *recTee {
	if capBytes <= 0 {
		return nil
	}
	return &recTee{cap: capBytes, buf: make([]byte, 0, min(capBytes, 64<<10))}
}

// write is nil-safe on purpose: the recording-off path calls it on every chunk of every
// streamed response, so "off" must cost one nil check and nothing else.
func (t *recTee) write(p []byte) {
	if t == nil {
		return
	}
	t.total += len(p)
	if len(t.buf) >= t.cap {
		t.truncated = true
		return
	}
	if n := t.cap - len(t.buf); len(p) > n {
		t.buf = append(t.buf, p[:n]...)
		t.truncated = true
		return
	}
	t.buf = append(t.buf, p...)
}

// bytes returns the retained body and whether it is the WHOLE body. A false second
// return must reach the trace as an explicit incompleteness marker.
func (t *recTee) bytes() ([]byte, bool) {
	if t == nil {
		return nil, false
	}
	return t.buf, !t.truncated
}

func (t *recTee) size() int {
	if t == nil {
		return 0
	}
	return t.total
}
