package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A nil tee is the shipped default (recording off) and must be inert, not a panic.
func TestNilRecTeeIsInert(t *testing.T) {
	var tee *recTee
	tee.write([]byte("anything"))
	if b, whole := tee.bytes(); b != nil || whole {
		t.Fatalf("nil tee returned %q whole=%v, want nil/false", b, whole)
	}
	if tee.size() != 0 {
		t.Fatalf("nil tee size = %d, want 0", tee.size())
	}
}

func TestRecTeeKeepsBytesInOrder(t *testing.T) {
	tee := newRecTee(1 << 20)
	for _, s := range []string{"event: a\n", "data: 1\n\n", "event: b\n"} {
		tee.write([]byte(s))
	}
	got, whole := tee.bytes()
	want := "event: a\ndata: 1\n\nevent: b\n"
	if string(got) != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if !whole {
		t.Error("whole = false on a body well under the cap")
	}
}

// The cap must TRUNCATE AND SAY SO. A silently short body is the corpus defect this
// return value exists to prevent.
func TestRecTeeTruncationIsReported(t *testing.T) {
	tee := newRecTee(10)
	tee.write([]byte("0123456789ABCDEF"))
	got, whole := tee.bytes()
	if len(got) != 10 {
		t.Errorf("kept %d bytes, want 10", len(got))
	}
	if whole {
		t.Error("whole = true after truncation; the trace would claim a complete turn")
	}
	if tee.size() != 16 {
		t.Errorf("size = %d, want the full 16 seen", tee.size())
	}
}

func TestRecTeeTruncationAcrossWrites(t *testing.T) {
	tee := newRecTee(8)
	tee.write([]byte("1234"))
	tee.write([]byte("5678"))
	tee.write([]byte("9")) // entirely past the cap
	got, whole := tee.bytes()
	if string(got) != "12345678" {
		t.Errorf("body = %q, want %q", got, "12345678")
	}
	if whole {
		t.Error("whole = true although a later write was dropped")
	}
}

func TestNewRecTeeZeroCapIsNil(t *testing.T) {
	if newRecTee(0) != nil || newRecTee(-1) != nil {
		t.Fatal("a non-positive cap must yield a nil (inert) tee")
	}
}

// THE PROPERTY THE CG TEAM WILL CARE ABOUT MOST: the bytes the client receives are
// identical whether or not a tee is attached, and the tee sees exactly those bytes.
func TestStreamRecForwardsIdenticalBytesWithAndWithoutTee(t *testing.T) {
	const body = "event: message_start\ndata: {\"x\":1}\n\nevent: message_stop\ndata: {}\n\n"
	serve := func(tee *recTee) string {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.Write([]byte(body))
		}))
		defer up.Close()
		resp, err := http.Get(up.URL)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		h := &Handler{}
		h.streamRec(rec, resp, tee)
		return rec.Body.String()
	}
	without := serve(nil)
	tee := newRecTee(1 << 20)
	with := serve(tee)
	if without != with {
		t.Fatalf("client bytes differ with a tee attached:\n off=%q\n on =%q", without, with)
	}
	if without != body {
		t.Fatalf("client did not receive the upstream body verbatim: %q", without)
	}
	got, whole := tee.bytes()
	if !whole {
		t.Error("tee reported truncation on a small body")
	}
	if !bytes.Equal(got, []byte(body)) {
		t.Errorf("tee captured %q, want the whole body %q", got, body)
	}
}

// A response larger than the cap must still reach the client INTACT. Recording may lose
// bytes; forwarding may never.
func TestStreamRecNeverTruncatesTheClient(t *testing.T) {
	big := strings.Repeat("data: xxxxxxxx\n\n", 4000) // ~64 KB
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(big))
	}))
	defer up.Close()
	resp, err := http.Get(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	tee := newRecTee(1024) // deliberately far too small
	h := &Handler{}
	h.streamRec(rec, resp, tee)
	if rec.Body.String() != big {
		t.Fatalf("client got %d bytes, want the full %d", rec.Body.Len(), len(big))
	}
	if _, whole := tee.bytes(); whole {
		t.Error("tee claimed a whole body despite a 1 KiB cap")
	}
	if tee.size() != len(big) {
		t.Errorf("tee size = %d, want %d (it must count what it saw)", tee.size(), len(big))
	}
}
