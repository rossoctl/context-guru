package plugin

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Processes this suite starts and must stop.
//
// The scripts under test start proxies detached on purpose (setsid, nohup, disown), so a fake proxy
// outlives the script that started it and, unless the test stops it, the test too. They piled up:
// hundreds of `python3 -c ... http.server` and `sleep 300` fakes on a shared box, each holding a port.
//
// Every child the suite starts inherits two marks through the environment: the run's id (set once, in
// TestMain) and, for commands built with sandboxEnv, the test's name. sandboxEnv registers a cleanup
// that kills whatever carries its test's name, process group included; TestMain then FAILS the run if
// anything carrying the run's id is still alive, and kills it, so a test that starts a process some
// other way cannot leak silently either. Linux only (it reads /proc); elsewhere it does nothing.

const (
	leakRunVar  = "CG_PLUGIN_TEST_RUN"
	leakTestVar = "CG_PLUGIN_TEST_NAME"
)

var leakRun = fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())

func TestMain(m *testing.M) {
	os.Setenv(leakRunVar, leakRun) //nolint:errcheck
	code := m.Run()
	if left := markedProcs(""); len(left) > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: the suite left %d process(es) running (stop them in t.Cleanup):\n", len(left))
		for _, p := range left {
			fmt.Fprintf(os.Stderr, "  pid %d  %s  (started by %s)\n", p.pid, p.cmd, p.test)
		}
		killProcs(left)
		code = 1
	}
	os.Exit(code)
}

type markedProc struct {
	pid       int
	cmd, test string
}

// markedProcs lists the live processes carrying this run's mark and, if test is not "", that test's.
func markedProcs(test string) []markedProc {
	dirs, _ := filepath.Glob("/proc/[0-9]*")
	var out []markedProc
	for _, d := range dirs {
		pid, _ := strconv.Atoi(filepath.Base(d))
		if pid == os.Getpid() {
			continue
		}
		run, name, ok := procMarks(pid)
		if !ok || run != leakRun || (test != "" && name != test) {
			continue
		}
		if st, err := os.ReadFile(filepath.Join(d, "stat")); err == nil && strings.Contains(string(st), ") Z ") {
			continue // a zombie is already dead; its parent reaps it
		}
		cmd, _ := os.ReadFile(filepath.Join(d, "cmdline"))
		out = append(out, markedProc{pid, strings.TrimSpace(string(bytes.ReplaceAll(cmd, []byte{0}, []byte{' '}))), name})
	}
	return out
}

// procMarks reads a process's two marks, each a whole NUL-separated entry of its environment.
func procMarks(pid int) (run, name string, ok bool) {
	env, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil {
		return "", "", false
	}
	for _, kv := range bytes.Split(env, []byte{0}) {
		if v, found := bytes.CutPrefix(kv, []byte(leakRunVar+"=")); found {
			run, ok = string(v), true
		}
		if v, found := bytes.CutPrefix(kv, []byte(leakTestVar+"=")); found {
			name = string(v)
		}
	}
	return run, name, ok
}

// killProcs stops each process and the process group it leads or belongs to (the scripts start
// proxies with setsid, so a fake's children share its group), never this test binary's own group.
func killProcs(ps []markedProc) {
	own := syscall.Getpgrp()
	for _, p := range ps {
		// Re-read the marks right before the signal: a pid that exited and was reused since the scan
		// belongs to something else now.
		if run, name, ok := procMarks(p.pid); !ok || run != leakRun || name != p.test {
			continue
		}
		if g, err := syscall.Getpgid(p.pid); err == nil && g != own && g > 1 {
			syscall.Kill(-g, syscall.SIGKILL) //nolint:errcheck
		}
		syscall.Kill(p.pid, syscall.SIGKILL) //nolint:errcheck
	}
}

var leakCleanups sync.Map // *testing.T -> struct{}: one cleanup per test, however often sandboxEnv runs

// leakMark is the environment entry that names t as the owner of what a command starts, and makes sure
// t stops all of it when it ends.
func leakMark(t *testing.T) string {
	t.Helper()
	if _, seen := leakCleanups.LoadOrStore(t, struct{}{}); !seen {
		name := t.Name()
		t.Cleanup(func() {
			killProcs(markedProcs(name))
			// A child caught mid-execve reads an empty environ, so the first scan can miss it: look again.
			time.Sleep(20 * time.Millisecond)
			killProcs(markedProcs(name))
			leakCleanups.Delete(t)
		})
	}
	return leakTestVar + "=" + t.Name()
}

// The guard itself: a process started under a test's mark is gone once that test's cleanups ran.
func TestLeakGuardStopsWhatATestStarted(t *testing.T) {
	if _, err := os.Stat("/proc/self/environ"); err != nil {
		t.Skip("no /proc on this platform")
	}
	var pid int
	t.Run("starter", func(t *testing.T) {
		cmd := exec.Command("setsid", "sleep", "300")
		cmd.Env = sandboxEnv(t)
		if err := cmd.Start(); err != nil {
			t.Skip("setsid unavailable: ", err)
		}
		pid = cmd.Process.Pid
		go cmd.Wait()                                      //nolint:errcheck
		for i := 0; len(markedProcs(t.Name())) == 0; i++ { // its environ reads empty until execve is done
			if i == 50 {
				t.Fatal("the started process does not carry the test's mark")
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	for i := 0; i < 50; i++ {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("pid %d outlived the subtest that started it", pid)
}

// The 15 s the installer waits for a proxy it just started is the product's default; the tests above
// shorten it only for fakes that never answer. A changed default must be a deliberate edit here too.
func TestTheHealthWaitDefaultsToFifteenSeconds(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("scripts", "start-proxy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `BUDGET="${CONTEXT_GURU_HEALTH_BUDGET:-15}"`) {
		t.Error("start-proxy.sh no longer waits 15 s by default for the proxy it started")
	}
}
