package proxyapp

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// childBound is the safety bound for a child boot or exit. It is long on purpose: the laptop
// runs under heavy load, and a short bound turns a slow boot into a wrong verdict.
const childBound = 90 * time.Second

// childEnvKeep lists the only variables a test child inherits. The proxy reads dozens of
// variables as config (DASHBOARD_*, CG_*, CONTEXT_GURU_*, UPSTREAMS, CONTROL_DB, the API keys and
// more), so a block list goes stale. An allow list cannot. Each test then sets what it tests.
var childEnvKeep = []string{"PATH", "TMPDIR", "TZ", "LANG", "LC_ALL", "LC_CTYPE", "SYSTEMROOT", "GOCOVERDIR"}

// childEnv returns the environment of a child that runs Main: the kept variables, a private
// HOME, and extra. Nothing else leaks in from the shell that runs the tests.
func childEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	env := []string{"HOME=" + t.TempDir(), "CG_TEST_RUN_MAIN=1"}
	for _, k := range childEnvKeep {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return append(env, extra...)
}

// syncBuffer is a bytes.Buffer the subprocess's two pipes can write at once. watch reports when
// the output first contains a string, so a test waits on the output and not on a clock.
type syncBuffer struct {
	mu      sync.Mutex
	b       bytes.Buffer
	watches []*bufWatch
}

type bufWatch struct {
	substr string
	ch     chan struct{}
	fired  bool
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := s.b.Write(p)
	s.fire()
	return n, err
}

// fire closes the channel of each watch whose string is now in the output. The caller holds mu.
func (s *syncBuffer) fire() {
	for _, w := range s.watches {
		if !w.fired && strings.Contains(s.b.String(), w.substr) {
			w.fired = true
			close(w.ch)
		}
	}
}

// watch returns a channel that closes once the output contains substr.
func (s *syncBuffer) watch(substr string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := &bufWatch{substr: substr, ch: make(chan struct{})}
	s.watches = append(s.watches, w)
	s.fire()
	return w.ch
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// child is a started subprocess that runs Main.
type child struct {
	cmd    *exec.Cmd
	buf    *syncBuffer
	exited chan struct{}
}

// startChild starts the test binary as Main on LISTEN_ADDR=addr, with the scrubbed childEnv plus
// env. The cleanup kills the child and waits for it.
func startChild(t *testing.T, addr string, env []string) *child {
	t.Helper()
	cmd := exec.Command(os.Args[0]) //nolint:gosec // the test binary itself
	cmd.Env = childEnv(t, append([]string{"LISTEN_ADDR=" + addr}, env...)...)
	c := &child{cmd: cmd, buf: &syncBuffer{}, exited: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = c.buf, c.buf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait(); close(c.exited) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-c.exited })
	return c
}

type childEvent int

const (
	childListening childEvent = iota // the "listening" line appeared first
	childExited                      // the process exited first
	childTimedOut                    // neither happened within childBound
)

// waitListening waits for the first of: the child's "context-guru-proxy listening" line that also
// contains attr (any line when attr is empty), the child's exit, or childBound.
func (c *child) waitListening(t *testing.T, attr string) childEvent {
	t.Helper()
	ready := make(chan struct{})
	go func() {
		// The line has two parts. Wait for the second, then check the first is on the same line.
		select {
		case <-c.buf.watch("context-guru-proxy listening"):
			for _, line := range strings.Split(c.buf.String(), "\n") {
				if strings.Contains(line, "context-guru-proxy listening") && strings.Contains(line, attr) {
					close(ready)
					return
				}
			}
		case <-c.exited:
		}
	}()
	timer := time.NewTimer(childBound)
	defer timer.Stop()
	select {
	case <-ready:
		return childListening
	case <-c.exited:
		// A line written just before the exit still counts as listening.
		for _, line := range strings.Split(c.buf.String(), "\n") {
			if strings.Contains(line, "context-guru-proxy listening") && strings.Contains(line, attr) {
				return childListening
			}
		}
		return childExited
	case <-timer.C:
		return childTimedOut
	}
}
