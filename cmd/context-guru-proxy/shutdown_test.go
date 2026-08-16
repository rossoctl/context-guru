package main

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/dash"
)

// nopRemote is a cold-storage stand-in, so the archiver goroutine is running during
// the shutdown under test (it is one of the things shutdown must not hang on).
type nopRemote struct{}

func (nopRemote) Put(context.Context, string, []byte) error   { return nil }
func (nopRemote) Get(context.Context, string) ([]byte, error) { return nil, dash.ErrRemoteMissing }
func (nopRemote) Size(context.Context, string) (int64, error) { return 0, dash.ErrRemoteMissing }
func (nopRemote) Delete(context.Context, string) error        { return nil }
func (nopRemote) Describe() string                            { return "test:" }

// TestGracefulShutdownWithSSEClient pins the property the graceful path exists for:
// with the dashboard on, a live SSE viewer attached and an archive remote configured,
// shutdown finishes in milliseconds and the capture writer's pending row reaches the
// database. Before armShutdown, the SSE stream held http.Server.Shutdown for its
// entire deadline — 25 s in production.
func TestGracefulShutdownWithSSEClient(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "dash.db")
	rec, err := dash.NewRecorder(dash.Options{
		DBPath:              dbPath,
		Remote:              nopRemote{},
		ArchiveContentAfter: time.Hour,
		ArchiveSessionAfter: time.Hour,
		ArchiveInterval:     time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	dash.NewAPI(rec).Mount(mux)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln) //nolint:errcheck // returns ErrServerClosed on Shutdown

	// A real SSE client: connected, headers read, and left open — exactly what a
	// dashboard tab is when the operator restarts the service.
	resp, err := http.Get("http://" + ln.Addr().String() + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	go bufio.NewReader(resp.Body).ReadString('\n') // keep reading; never finishes
	for i := 0; rec.Hub().Clients() == 0; i++ {
		if i > 200 {
			t.Fatal("SSE client never registered with the hub")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// One captured request still sitting in the writer's batch at shutdown.
	rec.Record(&dash.Event{SessionID: "s1", Model: "m", Route: "/compact", Status: 200})

	armShutdown(srv, rec)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	start := time.Now()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("shutdown took %v; an SSE stream is holding it open", d)
	}
	if err := rec.Close(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("recorder close: %v", err)
	}

	// Reopened after Close, so this asserts the row is on DISK — a restart's worth of
	// durability, not just an in-process buffer.
	db, err := dash.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	page, err := db.Requests(dash.Filter{TenantAll: true}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Requests) != 1 {
		t.Fatalf("capture writer did not flush on shutdown: %d rows in the database, want 1",
			len(page.Requests))
	}
}
