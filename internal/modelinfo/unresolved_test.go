package modelinfo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// THE DEFECT THESE EXIST TO CATCH: a fetch that failed was silent. The error was assigned and then
// discarded in refreshIfStale, the HTTP status was never examined, and the resolver chain answered the
// resulting emptiness from its embedded table. So a proxy could run for six hours against an
// unreachable model-window document, resolve every context window to 1,000,000, evaluate every
// fraction-based trigger against the wrong denominator, and print nothing.
//
// Each test was checked by reverting the change it covers.

// A 404 is not a malformed document, and the error must say which one happened.
// Mutation that must fail this: remove the resp.StatusCode check from fetch.
func TestFetchDistinguishesANotFoundFromABadDocument(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // body: "404 page not found", which decodes as a json error
	}))
	defer srv.Close()

	l := NewLiteLLM(srv.URL, nil, 0)
	err := l.Load(context.Background())
	if err == nil {
		t.Fatal("a 404 loaded successfully")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("the error does not name the status, so a wrong URL is indistinguishable from a "+
			"changed document: %v", err)
	}
}

// Load is SYNCHRONOUS, which is the property a startup check needs. Window() cannot serve that purpose:
// it starts a background fetch and returns unknown, so a caller verifying a configured document would
// see failure on the first call and success a few milliseconds later.
// Mutation that must fail this: implement Load by calling refreshIfStale.
func TestLoadIsSynchronous(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sample))
	}))
	defer srv.Close()

	l := NewLiteLLM(srv.URL, nil, 0)
	if err := l.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	// No polling, no sleep: the very next lookup must already resolve.
	if w, ok := l.Window(context.Background(), "gpt-4o"); !ok || w != 128000 {
		t.Fatalf("Load returned before the map was usable: window=%d ok=%v", w, ok)
	}
}

// The failure has to be RECOVERABLE BY A CALLER, not merely logged, so a run can record it in the
// artifact it already collects instead of relying on someone noticing a warning in a debug log.
// Mutation that must fail this: drop the lastErr/failures assignment in refreshIfStale's else branch.
func TestUnresolvedReportsWhyAndHowOften(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	l := NewLiteLLM(srv.URL, nil, 0)
	if err, n := l.Unresolved(); err != nil || n != 0 {
		t.Fatalf("a fresh resolver already reports a failure: %v (%d)", err, n)
	}
	for i := 1; i <= 2; i++ {
		if err := l.Load(context.Background()); err == nil {
			t.Fatal("a 500 loaded successfully")
		}
		err, n := l.Unresolved()
		if err == nil {
			t.Fatal("Unresolved reports no error after a failed Load")
		}
		if n != i {
			t.Fatalf("failure count = %d after %d failed attempts", n, i)
		}
	}
	// And it clears once a document does load, so a transient failure does not read as a permanent one.
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sample))
	}))
	defer ok.Close()
	l2 := NewLiteLLM(ok.URL, nil, 0)
	if err := l2.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err, _ := l2.Unresolved(); err != nil {
		t.Fatalf("Unresolved reports an error after a successful load: %v", err)
	}
}

// A document that fetches cleanly but names no windows is a failure too — that is the shape an
// operator's typo takes when the file is valid JSON.
func TestEmptyDocumentIsAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"sample_spec": {"max_input_tokens": 1}}`))
	}))
	defer srv.Close()

	l := NewLiteLLM(srv.URL, nil, 0)
	if err := l.Load(context.Background()); err == nil {
		t.Fatal("a document with no usable entries loaded successfully")
	}
}

// THE CHAIN IS THE ACTUAL HAZARD, and this pins it: an unreachable document does NOT produce "unknown",
// it produces the embedded table's answer. The test exists so nobody reads the package comment's
// "fails open" as "a failed fetch is safe" — it is safe only for a caller that has no authoritative
// document, which is why an explicit MODEL_INFO_URL is probed at startup in cmd/context-guru-proxy.
func TestAnUnreachableDocumentResolvesFromTheFallbackNotToUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	l := NewLiteLLM(srv.URL, nil, 0)
	_ = l.Load(context.Background())
	chain := Chain{l, DefaultStatic()}
	w, ok := chain.Window(context.Background(), "claude-sonnet-5")
	if !ok || w != 1000000 {
		t.Fatalf("the fallback stopped answering for an unreachable document (window=%d ok=%v); if "+
			"this is now an intentional change, the startup probe in modelWindows and the package "+
			"comment both describe the old behaviour", w, ok)
	}
}

// THE BACKGROUND PATH IS THE ONE THAT ACTUALLY RUNS IN PRODUCTION. Load() is the startup probe; every
// subsequent refresh goes through refreshIfStale's goroutine, and that is where the error used to be
// discarded. A test that only exercises Load would leave the real failure path uncovered — Load sets
// lastErr itself, so a mutation that re-broke the goroutine would still pass.
// Mutation that must fail this: drop the else branch in refreshIfStale.
func TestTheBackgroundRefreshRecordsItsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	l := NewLiteLLM(srv.URL, nil, 0)
	// Window, NOT Load: this is the path a serving process takes.
	if _, ok := l.Window(context.Background(), "claude-sonnet-5"); ok {
		t.Fatal("an unreachable document resolved a window")
	}
	for i := 0; i < 400; i++ {
		if err, n := l.Unresolved(); err != nil && n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	err, n := l.Unresolved()
	t.Fatalf("the background refresh failed silently: Unresolved() = (%v, %d) after two seconds", err, n)
}

// THE PROXY HOLDS A CHAIN, not a *LiteLLM, so the question has to be answerable through the Chain or
// the stats field is always zero and the run's artifact says "healthy" for a failed load.
// Mutation that must fail this: delete Chain.Unresolved.
func TestChainForwardsUnresolved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	l := NewLiteLLM(srv.URL, nil, 0)
	_ = l.Load(context.Background())
	var chain Resolver = Chain{l, DefaultStatic()}
	u, ok := chain.(interface{ Unresolved() (error, int) })
	if !ok {
		t.Fatal("a Chain cannot be asked whether the document loaded; the stats field will read 0 " +
			"for every failed load")
	}
	err, n := u.Unresolved()
	if err == nil || n == 0 {
		t.Fatalf("Chain.Unresolved reports healthy for a 404: (%v, %d)", err, n)
	}
}
