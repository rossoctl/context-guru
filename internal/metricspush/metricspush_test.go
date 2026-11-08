package metricspush

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestPushSendsTokenAndPath(t *testing.T) {
	var gotAuth, gotPath atomic.Value
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		gotPath.Store(r.URL.Path)
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p, shutdown, err := Start(context.Background(), Config{
		URL: srv.URL, Token: "secret", Interval: 20 * time.Millisecond,
		Proxy: "cortex", Machine: "m1", Version: "v", Logger: quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := p.Meter("t").Int64Counter("llm.test")
	c.Add(context.Background(), 1)
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if hits.Load() == 0 {
		t.Fatal("the collector received no push")
	}
	if gotAuth.Load() != "Bearer secret" {
		t.Errorf("Authorization = %v, want Bearer secret", gotAuth.Load())
	}
	if gotPath.Load() != "/v1/metrics" {
		t.Errorf("path = %v, want /v1/metrics", gotPath.Load())
	}
}

func TestDisabledMakesNoConnection(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()

	p, shutdown, err := Start(context.Background(), Config{URL: srv.URL, Disabled: true, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := p.Meter("t").Int64Counter("llm.test")
	c.Add(context.Background(), 1)
	_ = shutdown(context.Background())
	if hits.Load() != 0 {
		t.Fatalf("a disabled push made %d connection(s)", hits.Load())
	}
}

func TestEmptyURLWithDisabledIsOff(t *testing.T) {
	_, shutdown, err := Start(context.Background(), Config{Disabled: true, Logger: quiet})
	if err != nil || shutdown(context.Background()) != nil {
		t.Fatalf("err=%v", err)
	}
}

func TestPlainHTTPToARemoteHostIsRefused(t *testing.T) {
	for _, u := range []string{"http://collector.example.com", "ftp://x", "not a url", "https://"} {
		if _, _, err := Start(context.Background(), Config{URL: u, Logger: quiet}); err == nil {
			t.Errorf("Start(%q) accepted the URL", u)
		}
	}
	for _, u := range []string{"https://collector.example.com", "http://127.0.0.1:4318", "http://localhost:4318"} {
		_, shutdown, err := Start(context.Background(), Config{URL: u, Interval: time.Hour, Logger: quiet})
		if err != nil {
			t.Errorf("Start(%q) = %v, want ok", u, err)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_ = shutdown(ctx)
		cancel()
	}
}

// A collector that refuses must not delay the code that records a metric (R16, R20).
func TestRefusingCollectorDoesNotBlockRecording(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // now nothing listens there

	p, shutdown, err := Start(context.Background(), Config{URL: url, Interval: 10 * time.Millisecond, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := p.Meter("t").Int64Counter("llm.test")
	start := time.Now()
	for i := 0; i < 100000; i++ {
		c.Add(context.Background(), 1)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("100000 records took %v with a refusing collector", d)
	}
	time.Sleep(50 * time.Millisecond) // let a few failed pushes happen
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = shutdown(ctx)
}

// A collector that hangs must not delay recording either.
func TestHangingCollectorDoesNotBlockRecording(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)

	p, shutdown, err := Start(context.Background(), Config{URL: srv.URL, Interval: 10 * time.Millisecond, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := p.Meter("t").Int64Counter("llm.test")
	time.Sleep(30 * time.Millisecond) // a push is now stuck
	start := time.Now()
	for i := 0; i < 100000; i++ {
		c.Add(context.Background(), 1)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("100000 records took %v with a hanging collector", d)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = shutdown(ctx)
}

func TestFromEnv(t *testing.T) {
	t.Setenv("METRICS_PUSH_URL", "https://c.example.com")
	t.Setenv("METRICS_PUSH_TOKEN", "tok")
	t.Setenv("METRICS_PUSH", "off")
	t.Setenv("METRICS_PUSH_INTERVAL", "5s")
	got := FromEnv(Config{Proxy: "cortex"})
	if got.URL != "https://c.example.com" || got.Token != "tok" || !got.Disabled || got.Interval != 5*time.Second || got.Proxy != "cortex" {
		t.Fatalf("FromEnv = %+v", got)
	}
}
