package dash

import "testing"

func TestObserveLifecycleGapAndTTL(t *testing.T) {
	r := &Recorder{life: newLifecycle()}
	q := LifecycleReq{ResultCount: 2, ResultBytes: 50, ResultError: true}

	a := r.ObserveLifecycle("t", "s", "", 1000, 1400, q, 0, 0)
	if a.SincePrevEndMs != -1 || a.GrantedTTL != "" || a.ResultCount != 2 || !a.ResultError {
		t.Errorf("first: %+v", a)
	}
	// Gap runs from the previous END (1400), not the previous start.
	b := r.ObserveLifecycle("t", "s", "", 61400, 62000, q, 100, 0)
	if b.SincePrevEndMs != 60000 || b.GrantedTTL != "5m" {
		t.Errorf("second: %+v", b)
	}
	// Another thread of the same session has its own clock; tier labels.
	c := r.ObserveLifecycle("t", "s", "sub", 63000, 64000, q, 0, 200)
	if c.SincePrevEndMs != -1 || c.GrantedTTL != "1h" {
		t.Errorf("other thread: %+v", c)
	}
	if d := r.ObserveLifecycle("t", "s", "", 70000, 71000, q, 5, 5); d.SincePrevEndMs != 8000 || d.GrantedTTL != "mixed" {
		t.Errorf("mixed: %+v", d)
	}
}

// Stale threads are dropped by age, live ones kept.
func TestLifecycleEvictsByAge(t *testing.T) {
	r := &Recorder{life: newLifecycle()}
	r.ObserveLifecycle("t", "old", "", 0, 1, LifecycleReq{}, 0, 0)
	live := int64(staleSession) + 10*int64(lifecycleSweepMs)
	r.ObserveLifecycle("t", "live", "", live-5, live-4, LifecycleReq{}, 0, 0)
	r.ObserveLifecycle("t", "x", "", live, live+1, LifecycleReq{}, 0, 0) // triggers the sweep
	if _, ok := r.life.end["t\x00old\x00"]; ok {
		t.Error("stale thread kept")
	}
	if _, ok := r.life.end["t\x00live\x00"]; !ok {
		t.Error("live thread evicted")
	}
}

func TestLifecycleOffAndPing(t *testing.T) {
	var r *Recorder
	if r.ObserveLifecycle("t", "s", "", 1, 2, LifecycleReq{}, 0, 0) != nil || r.PingLifecycle(1) != nil {
		t.Error("flag off must produce no row")
	}
	on := &Recorder{life: newLifecycle()}
	if p := on.PingLifecycle(2); p.PingN != 2 || p.SincePrevEndMs != -1 {
		t.Errorf("ping: %+v", p)
	}
}
