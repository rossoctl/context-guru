package main

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/dash"
)

// A keep-alive ping's answer carries the subscription limit headers, and the ping row records them
// with the request path's parser (#995). The newest values then come from the ping, so the share
// stays current while the user is idle. The auth kind still comes from the user's own request.
func TestDashEmitterRecordsPingLimitHeaders(t *testing.T) {
	rec, err := dash.NewRecorder(dash.Options{
		DBPath: filepath.Join(t.TempDir(), "dash.db"), BatchSize: 1, FlushInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	now := time.Now().UnixMilli()
	u30, r30 := 0.30, int64(1794249600)
	rec.Record(&dash.Event{TS: now - 60_000, TenantID: "t1", Provider: "anthropic", Status: 200,
		Model: "claude-sonnet-5", AuthSubscription: true,
		Limits: &dash.Limits{FiveHour: dash.LimitWindow{Utilization: &u30, ResetS: &r30, Status: "allowed"}}})

	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-utilization", "0.42")
	h.Set("anthropic-ratelimit-unified-5h-reset", "1794249600")
	h.Set("anthropic-ratelimit-unified-5h-status", "allowed")
	h.Set("anthropic-ratelimit-unified-7d-utilization", "0.09")
	em := &dashEmitter{rec: rec}
	em.KeepAlivePing(components.KeepAliveReport{
		Tenant: "t1", Session: "s1", Model: "claude-sonnet-5", Provider: "anthropic",
		Route: "/v1/messages", CacheRead: 48576, Status: 200, TS: now, Header: h,
	})
	// A ping without the headers (an API key) leaves its row's limit columns empty.
	em.KeepAlivePing(components.KeepAliveReport{
		Tenant: "t2", Session: "s2", Model: "claude-sonnet-5", Provider: "anthropic",
		Route: "/v1/messages", CacheRead: 48576, Status: 200, TS: now,
	})

	var page *dash.Page
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		page, err = rec.DB().Requests(dash.Filter{TenantAll: true, WithKeepAlive: true}, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Requests) == 3 {
			break
		}
	}
	if len(page.Requests) != 3 {
		t.Fatalf("rows = %d, want 3", len(page.Requests))
	}
	for _, e := range page.Requests {
		if !e.KeepAlive {
			continue
		}
		if e.AuthSubscription {
			t.Errorf("ping row %s has auth_subscription set; a ping carries no client auth kind", e.TenantID)
		}
		if e.TenantID == "t2" && e.Limits != nil {
			t.Errorf("a ping with no headers recorded limits %+v", e.Limits)
		}
	}

	l, at, err := rec.DB().LatestLimits(dash.Filter{Tenant: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if l == nil || l.FiveHour.Utilization == nil || *l.FiveHour.Utilization != 0.42 ||
		l.FiveHour.Status != "allowed" || l.SevenDay.Utilization == nil || *l.SevenDay.Utilization != 0.09 {
		t.Fatalf("latest limits = %+v, want the ping's 0.42 / 0.09", l)
	}
	if at != now {
		t.Errorf("latest limits at %d, want the ping's %d", at, now)
	}
	sub, err := rec.DB().LatestAuthSubscription(dash.Filter{Tenant: "t1"})
	if err != nil || !sub {
		t.Errorf("auth kind = %v (%v), want the user's subscription request to still decide it", sub, err)
	}
}
