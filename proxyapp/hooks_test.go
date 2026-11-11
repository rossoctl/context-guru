package proxyapp

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/proxy"
	"github.com/rossoctl/context-guru/tenant"
)

// testHooks is the hook set TestMain passes to Main when CG_TEST_HOOKS=1. Each hook prints one
// "hook:" line, so the test can read the order they ran in and what each one saw.
func testHooks() Hooks {
	var hookFlag *string
	return Hooks{
		Flags: func(fs *flag.FlagSet) {
			hookFlag = fs.String("hook-flag", "", "a host flag")
			fmt.Println("hook:Flags")
		},
		Defaults: Defaults{DashboardContent: true},
		UIPath:   "/hookdash/",
		BeforeControlDB: func(path string) error {
			fmt.Println("hook:BeforeControlDB", filepath.Base(path))
			return nil
		},
		TenantOptions: func(o *tenant.Options) {
			o.ManagerEmail = "boss@example.com"
			fmt.Println("hook:TenantOptions")
		},
		Hosted: func(app *App) error {
			fmt.Println("hook:Hosted", "flag="+*hookFlag, "registry", app.Registry != nil,
				"upstreams", len(app.Upstreams), "handler", app.Handler != nil)
			return nil
		},
		ProxyOptions: func(o *proxy.Options, app *App) {
			fmt.Println("hook:ProxyOptions", "tenants", o.Tenants != nil, "handler", app.Handler != nil)
		},
		DashAuth: func(core dash.Authenticator) dash.Authenticator {
			fmt.Println("hook:DashAuth")
			return core
		},
		Mount: func(mux *http.ServeMux, app *App) {
			fmt.Println("hook:Mount", "handler", app.Handler != nil, "mux", app.Mux == mux)
			mux.HandleFunc("GET /hooktest", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "mounted")
			})
		},
		Handler: func(next http.Handler, _ *App) http.Handler {
			fmt.Println("hook:Handler")
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Hook", "wrapped")
				next.ServeHTTP(w, r)
			})
		},
	}
}

// EVERY HOOK RUNS ONCE, AT ITS OWN POINT, AND TAKES EFFECT. The real Main boots in a subprocess in
// hosted mode with the dashboard on, and the test reads the hook lines, the log and one request.
func TestHooksRunInOrderAndTakeEffect(t *testing.T) {
	dir := t.TempDir()
	env := append(hostedEnv(t), "CG_TEST_HOOKS=1", "CG_TEST_MAIN_ARGS=--hook-flag=yes",
		"DASHBOARD=true", "DASHBOARD_DB="+filepath.Join(dir, "dash.db"))
	out, resp := bootAndGet(t, env, "/hooktest")

	var got []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "hook:") {
			got = append(got, strings.Fields(line)[0])
		}
	}
	want := []string{"hook:Flags", "hook:BeforeControlDB", "hook:TenantOptions", "hook:Hosted",
		"hook:ProxyOptions", "hook:DashAuth", "hook:Mount", "hook:Handler"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("hooks ran as %v, want %v\n%s", got, want, out)
	}
	for _, s := range []string{
		"hook:BeforeControlDB control.db",
		"hook:Hosted flag=yes registry true upstreams 1 handler false", // before proxy.New
		"hook:ProxyOptions tenants true handler false",
		"hook:Mount handler true mux true",
		"manager=true",         // TenantOptions reached the registry and its log line
		"/hookdash/",           // UIPath reached the dashboard's log line
		"content_capture=true", // Defaults.DashboardContent
	} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
	if resp.body != "mounted" || resp.header.Get("X-Hook") != "wrapped" {
		t.Errorf("GET /hooktest = %q, X-Hook %q; want the mounted route through the wrapper",
			resp.body, resp.header.Get("X-Hook"))
	}
}

// NO HOOKS IS THE BINARY. The zero Hooks keeps /dashboard/ and content capture off.
func TestNoHooksKeepsTheBinaryDefaults(t *testing.T) {
	env := []string{"DASHBOARD=true", "DASHBOARD_DB=" + filepath.Join(t.TempDir(), "dash.db")}
	out, resp := bootAndGet(t, env, "/hooktest")
	if !strings.Contains(out, "/dashboard/") || !strings.Contains(out, "content_capture=false") {
		t.Errorf("a boot with no hooks changed the dashboard defaults:\n%s", out)
	}
	if resp.status != http.StatusNotFound {
		t.Errorf("GET /hooktest = %d with no hooks, want 404", resp.status)
	}
}

type probe struct {
	status int
	body   string
	header http.Header
}

// bootAndGet starts Main in a subprocess on a free port, waits until it answers, sends one GET,
// then stops the process and returns everything it printed.
func bootAndGet(t *testing.T, env []string, path string) (string, probe) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0]) //nolint:gosec // the test binary itself
	cmd.Env = append(append(os.Environ(), "CG_TEST_RUN_MAIN=1", "LISTEN_ADDR="+addr), env...)
	var buf syncBuffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	var p probe
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		r, err := http.Get("http://" + addr + path)
		if err == nil {
			b, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			p = probe{status: r.StatusCode, body: string(b), header: r.Header}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the proxy never answered on %s:\n%s", addr, buf.String())
		}
	}
	return buf.String(), p
}

// syncBuffer is a bytes.Buffer the subprocess's two pipes can write at once.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
