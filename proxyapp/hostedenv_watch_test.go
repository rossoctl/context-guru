package proxyapp

import "testing"

// A listening line can reach the buffer in 2 writes. The watch must still fire on the second.
func TestWaitListeningSeesALineSplitAcrossWrites(t *testing.T) {
	var b syncBuffer
	ready := b.watch(func(out string) bool { return hasListeningLine(out, "addr=127.0.0.1:1") })
	_, _ = b.Write([]byte("level=INFO msg=\"context-guru-proxy listening\" "))
	select {
	case <-ready:
		t.Fatal("the watch fired before the addr part arrived")
	default:
	}
	_, _ = b.Write([]byte("addr=127.0.0.1:1 mode=sync\n"))
	select {
	case <-ready:
	default:
		t.Fatal("the watch did not fire after the second write")
	}
}

// A listening line for another addr does not count.
func TestWaitListeningIgnoresAnotherAddr(t *testing.T) {
	if hasListeningLine("msg=\"context-guru-proxy listening\" addr=127.0.0.1:2\n", "addr=127.0.0.1:1") {
		t.Fatal("matched a line for another addr")
	}
}
