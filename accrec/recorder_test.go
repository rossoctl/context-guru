package accrec

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
)

func recMsg(role, text string) (string, int) {
	s := fmt.Sprintf(`{"role":%q,"content":%q}`, role, text)
	return s, len(s)
}

func recBody(msgs ...string) []byte { return []byte(`{"messages":[` + strings.Join(msgs, ",") + `]}`) }

var recFacts = accountant.EntryFacts{TTL: 5 * time.Minute, Lifetime: accountant.LifetimeExact}

func recMeta(n int, session string) accountant.RequestMeta {
	return accountant.RequestMeta{RequestID: fmt.Sprint("r", n), Session: session, At: time.Unix(int64(1000+n*10), 0),
		Billing: accountant.Billing{Mode: accountant.BillingAPI}}
}

// one request that masks a big message: it books a saving.
func recOneRequest(r *Recorder, n int, th accountant.Thread) *accountant.Request {
	big, nBig := recMsg("user", strings.Repeat("tool result ", 400))
	small, nSmall := recMsg("user", "[masked]")
	m0, _ := recMsg("user", "first")
	m2, _ := recMsg("assistant", "answer")
	op := accountant.Operation{Kind: accountant.KindRewrite, Operator: "context-guru.mask", ChangeID: "cg:mask:a1",
		At:         accountant.Position{Section: accountant.SectionMessages, Index: 1},
		SizeBefore: accountant.Size{Bytes: nBig}, SizeAfter: accountant.Size{Bytes: nSmall}}
	req := r.Begin(recMeta(n, "s"), recBody(m0, big, m2))
	r.End(req, th, recBody(m0, small, m2), []accountant.Operation{op}, recFacts)
	return req
}

func TestRecorderPricesAMaskedMessage(t *testing.T) {
	r := NewRecorder()
	req := recOneRequest(r, 1, ThreadOf("main", "", false))
	res := r.Compute(req, Usage{FreshInput: 10, CacheRead: 100})
	saved := 0
	for _, o := range res.Ops {
		saved += o.Saved.Input + o.Saved.CacheWrite5m + o.Saved.CacheWrite1h + o.Saved.CacheRead
	}
	if len(res.Checks) != 0 || saved == 0 {
		t.Fatalf("checks %+v, saved %d tokens; want none and a positive saving", res.Checks, saved)
	}
}

func TestRecorderEndForgetsTheClientHashAndDropForgetsAnOpenOne(t *testing.T) {
	held := func(r *Recorder) (n int) { r.Clients.Range(func(any, any) bool { n++; return true }); return }
	r := NewRecorder()
	recOneRequest(r, 1, ThreadOf("main", "", false))
	if held(r) != 0 {
		t.Fatal("End must remove the client hash")
	}
	req := r.Begin(recMeta(2, "s"), recBody(mustRecMsg("user", "x")))
	if held(r) != 1 {
		t.Fatal("Begin must keep the client hash")
	}
	r.Drop(req)
	if held(r) != 0 {
		t.Fatal("Drop must remove the client hash")
	}
	var nilRec *Recorder
	nilRec.Drop(req) // nil-safe
}

func mustRecMsg(role, text string) string { s, _ := recMsg(role, text); return s }

func TestComputeSafeReportsAPanicAsNoResult(t *testing.T) {
	r := NewRecorder()
	if _, ok := r.ComputeSafe(nil, Usage{CacheRead: 10}); ok {
		t.Fatal("a nil request must report ok=false, not panic")
	}
	req := recOneRequest(r, 1, ThreadOf("main", "", false))
	if _, ok := r.ComputeSafe(req, Usage{FreshInput: 10, CacheRead: 100}); !ok {
		t.Fatal("a normal request must price")
	}
}

func TestThreadOfCarriesTheParent(t *testing.T) {
	if got := ThreadOf("a", "p", true); got.ID != ThreadID("a") || got.Parent != ThreadID("p") {
		t.Fatalf("%+v", got)
	}
	if got := ThreadOf("a", "p", false); got.Parent != "" {
		t.Fatalf("parent %q set without hasParent", got.Parent)
	}
}

func TestPrice(t *testing.T) {
	usage := Usage{FreshInput: 10, CacheRead: 100}
	t.Run("prices and spends the ping facts", func(t *testing.T) {
		r, pings := NewRecorder(), NewPingStore()
		pings.Put(PingKey("s", "main"), accountant.PingFacts{PingWindow: "w1"})
		req := recOneRequest(r, 1, ThreadOf("main", "", false))
		lowered := 0
		out := r.Price(req, PriceIn{Status: 200, Rounds: 1, Usage: usage, BeforeCompute: func() { lowered++ },
			Session: "s", Thread: "main", PingWindow: "w1", Pings: pings})
		if !out.Priced || out.NoResult != "" || lowered != 1 {
			t.Fatalf("priced=%v noResult=%q beforeCompute=%d", out.Priced, out.NoResult, lowered)
		}
		if _, ok := pings.Peek(PingKey("s", "main")); ok {
			t.Fatal("the ping facts the request read must be spent")
		}
	})
	t.Run("a rejected status books nothing and keeps the ping facts", func(t *testing.T) {
		r, pings := NewRecorder(), NewPingStore()
		pings.Put(PingKey("s", "main"), accountant.PingFacts{PingWindow: "w1"})
		req := recOneRequest(r, 1, ThreadOf("main", "", false))
		out := r.Price(req, PriceIn{Status: 429, Rounds: 1, Usage: usage, BeforeCompute: func() { t.Fatal("must not run") },
			Session: "s", Thread: "main", PingWindow: "w1", Pings: pings})
		if out.Priced || out.NoResult != RejectedStatus {
			t.Fatalf("priced=%v noResult=%q", out.Priced, out.NoResult)
		}
		if _, ok := pings.Peek(PingKey("s", "main")); !ok {
			t.Fatal("a request that books nothing leaves the ping facts for the next one")
		}
	})
	t.Run("several rounds book nothing and keep the earlier reason", func(t *testing.T) {
		r := NewRecorder()
		out := r.Price(recOneRequest(r, 1, ThreadOf("main", "", false)), PriceIn{Status: 200, Rounds: 2, Usage: usage})
		if out.Priced || out.NoResult != "" {
			t.Fatalf("priced=%v noResult=%q", out.Priced, out.NoResult)
		}
	})
	t.Run("a panic in the library is no result", func(t *testing.T) {
		r := NewRecorder()
		out := r.Price(nil, PriceIn{Status: 200, Rounds: 1, Usage: usage})
		if out.Priced || out.NoResult != ComputePanicked {
			t.Fatalf("priced=%v noResult=%q", out.Priced, out.NoResult)
		}
	})
	t.Run("pings with no facts are counted", func(t *testing.T) {
		r := NewRecorder()
		r.Price(recOneRequest(r, 1, ThreadOf("main", "", false)), PriceIn{Status: 200, Rounds: 1, Usage: usage, KaPings: 2})
		r.Price(recOneRequest(r, 2, ThreadOf("main", "", false)), PriceIn{Status: 200, Rounds: 1, Usage: usage, KaPings: 2, PingWindow: "w"})
		if got := r.RescueFactsMissing(); got != 1 {
			t.Fatalf("RescueFactsMissing = %d, want 1", got)
		}
	})
}

func TestStateRoundTripAndRefusals(t *testing.T) {
	r := NewRecorder()
	recOneRequest(r, 1, ThreadOf("main", "", false))
	req := recOneRequest(r, 2, ThreadOf("main", "", false))
	r.Compute(req, Usage{FreshInput: 10, CacheRead: 100})
	data, err := r.SnapshotState()
	if err != nil {
		t.Fatal(err)
	}
	if res := NewRecorder().RestoreState(data, time.Minute); !res.Restored || res.Err != nil || res.TooOld {
		t.Fatalf("a fresh snapshot must restore: %+v", res)
	}
	if res := NewRecorder().RestoreState(data, StateMaxAge+time.Minute); res.Restored || !res.TooOld {
		t.Fatalf("an old snapshot must be refused as too old: %+v", res)
	}
	var f StateFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	f.Version = StateVersion - 1
	old, _ := json.Marshal(f)
	if res := NewRecorder().RestoreState(old, time.Minute); res.Restored || res.Err == nil {
		t.Fatalf("another version must be refused: %+v", res)
	}
	if res := NewRecorder().RestoreState([]byte("{not json"), time.Minute); res.Restored || res.Err == nil {
		t.Fatalf("a bad file must be refused: %+v", res)
	}
}
