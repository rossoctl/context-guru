package main

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
)

// capture collects every log message slog emits, so a test can assert on what was NOT said.
type capture struct {
	mu    sync.Mutex
	lines []string
}

func (c *capture) Enabled(context.Context, slog.Level) bool { return true }
func (c *capture) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *capture) WithGroup(string) slog.Handler            { return c }
func (c *capture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, r.Message)
	return nil
}

func (c *capture) says(sub string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// TestListeningIsNotAnnouncedWhenTheBindFails is the whole point of listenAndAnnounce: on a port
// already owned by another instance the process must NOT log the line every readiness probe,
// healthcheck and operator watches for. It used to, and then exited — so the one failure the
// probe exists to catch read as a successful start, while the sibling that owned the port
// answered /healthz 200 with its own data.
func TestListeningIsNotAnnouncedWhenTheBindFails(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	c := &capture{}
	slog.SetDefault(slog.New(c))

	ln, err := listenAndAnnounce(held.Addr().String(), "pipeline", "p", "logs", "stderr")
	if err == nil {
		ln.Close()
		t.Fatal("binding a port another listener already owns succeeded")
	}
	if c.says("listening") {
		t.Error("announced it was listening after the bind failed")
	}
}

// And the success path still announces, with its fields intact.
func TestListeningIsAnnouncedOnceBound(t *testing.T) {
	c := &capture{}
	slog.SetDefault(slog.New(c))

	ln, err := listenAndAnnounce("127.0.0.1:0", "pipeline", "p", "logs", "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if !c.says("context-guru-proxy listening") {
		t.Error("bound the port without announcing it")
	}
}
