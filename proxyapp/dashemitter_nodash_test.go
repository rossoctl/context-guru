package proxyapp

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	acmetrics "github.com/rossoctl/accountant/metrics"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/proxy"
)

type recObserver struct {
	mu  sync.Mutex
	got []acmetrics.Observation
}

func (o *recObserver) Observe(x acmetrics.Observation) {
	o.mu.Lock()
	o.got = append(o.got, x)
	o.mu.Unlock()
}

// With the dashboard off, a ping still books its spend into the savings counters and stores its facts for the
// thread's next request (#1457). A manual ping does the same (#1460).
func TestAPingBooksSavingsWithNoDashboard(t *testing.T) {
	for _, manual := range []bool{false, true} {
		obs := &recObserver{}
		store := proxy.NewPingStore()
		em := &dashEmitter{pings: store} // rec is nil: the dashboard is off
		em.metrics.Store(&metricsSinks{savings: obs})
		r := pingReport()
		r.Manual = manual
		em.KeepAlivePing(r)
		if len(obs.got) != 1 || len(obs.got[0].Result.Ops) != 1 || obs.got[0].Result.Ops[0].Call == nil {
			t.Fatalf("manual=%v: observations=%+v, want one spend", manual, obs.got)
		}
		if store.Priced() != 1 {
			t.Errorf("manual=%v: priced = %d, want 1", manual, store.Priced())
		}
	}
}

// With the dashboard on, a scheduled ping books the same observation as before: one, with the same result as the row.
func TestAPingBooksTheSameSavingsWithTheDashboard(t *testing.T) {
	rec, err := dash.NewRecorder(dash.Options{DBPath: filepath.Join(t.TempDir(), "d.db"), BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	obs := &recObserver{}
	em := &dashEmitter{rec: rec, pings: proxy.NewPingStore()}
	em.metrics.Store(&metricsSinks{savings: obs})
	em.KeepAlivePing(pingReport())
	if len(obs.got) != 1 || len(obs.got[0].Result.Ops) != 1 || obs.got[0].Usage.CacheRead != int(pingReport().CacheRead) {
		t.Fatalf("observations = %+v, want one spend", obs.got)
	}
}
