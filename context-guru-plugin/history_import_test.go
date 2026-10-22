package plugin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The pre-install history import runs ONCE per install, at the end of a successful one: install.sh calls
// `context-guru-proxy import-history -apply -db <this install's dashboard DB>` and writes a marker. These
// tests drive the real install.sh with a fake proxy that logs each import-history call it receives.

// importingProxyDir is fakeProxyDir's healthy proxy that also answers `import-history`: it appends its
// arguments to calls.log and exits with failRC (0 = success).
func importingProxyDir(t *testing.T, port string, failRC int) (dir, calls string) {
	t.Helper()
	dir = fakeProxyDir(t, port, true)
	calls = filepath.Join(t.TempDir(), "calls.log")
	bin := filepath.Join(dir, "context-guru-proxy")
	b, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	hook := "if [ \"$1\" = import-history ]; then echo \"$*\" >>" + calls +
		"; echo 'APPLIED — 3 requests imported'; exit " + strconv.Itoa(failRC) + "; fi\n"
	s := strings.Replace(string(b), "#!/usr/bin/env bash\n", "#!/usr/bin/env bash\n"+hook, 1)
	if err := os.WriteFile(bin, []byte(s), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, calls
}

// stopRoutedProxy kills the fake proxy an install started, by the PID in its pidfile (never by pattern).
func stopRoutedProxy(state, port string) {
	if b, e := os.ReadFile(filepath.Join(state, "context-guru", "proxy-"+port+".pid")); e == nil {
		exec.Command("kill", strings.TrimSpace(string(b))).Run() //nolint:errcheck
	}
}

func importCalls(t *testing.T, calls string) []string {
	t.Helper()
	b, err := os.ReadFile(calls)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestInstallImportsHistoryOnceIntoThisInstallsDatabase(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	port := freePort(t)
	writePluginOptions(t, home, map[string]any{"port": port})
	dir, calls := importingProxyDir(t, port, 0)
	env := routeEnv(t, home, state, dir)
	t.Cleanup(func() { stopRoutedProxy(state, port) })

	facts, code := runRoute(t, proj, env, consentOK()...)
	if code != 0 || facts["result"] != "routed" {
		t.Fatalf("install: exit %d %v", code, facts)
	}
	got := importCalls(t, calls)
	db := filepath.Join(state, "context-guru", "dashboard-"+port+".db")
	if len(got) != 1 || got[0] != "import-history -apply -db "+db {
		t.Fatalf("want exactly one import into %s, got %q", db, got)
	}
	if facts["history_import"] != "done" {
		t.Errorf("history_import=%q, want done; facts %v", facts["history_import"], facts)
	}

	// A re-install of the same port must not import again.
	facts, code = runRoute(t, proj, env, consentOK()...)
	if code != 0 {
		t.Fatalf("re-install: exit %d %v", code, facts)
	}
	if got := importCalls(t, calls); len(got) != 1 {
		t.Errorf("a re-install imported again: %q", got)
	}
	if facts["history_import"] != "already_done" {
		t.Errorf("re-install history_import=%q, want already_done", facts["history_import"])
	}
}

// The marker belongs to the database: delete the DB (uninstall's "delete the state directory") and the
// next install imports into the new one.
func TestDeletingTheDashboardDatabaseLetsTheNextInstallImportAgain(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	port := freePort(t)
	writePluginOptions(t, home, map[string]any{"port": port})
	dir, calls := importingProxyDir(t, port, 0)
	env := routeEnv(t, home, state, dir)
	t.Cleanup(func() { stopRoutedProxy(state, port) })

	if _, code := runRoute(t, proj, env, consentOK()...); code != 0 {
		t.Fatalf("install exit %d", code)
	}
	db := filepath.Join(state, "context-guru", "dashboard-"+port+".db")
	if err := os.Remove(db + ".history-imported"); err != nil {
		t.Fatalf("no marker beside the DB after a successful import: %v", err)
	}
	if _, code := runRoute(t, proj, env, consentOK()...); code != 0 {
		t.Fatalf("re-install exit %d", code)
	}
	if got := importCalls(t, calls); len(got) != 2 {
		t.Errorf("with the DB's marker gone the next install must import again, calls %q", got)
	}
}

func TestAFailedHistoryImportNeverFailsTheInstallAndIsRetriedNextTime(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	port := freePort(t)
	writePluginOptions(t, home, map[string]any{"port": port})
	dir, calls := importingProxyDir(t, port, 1)
	env := routeEnv(t, home, state, dir)
	t.Cleanup(func() { stopRoutedProxy(state, port) })

	facts, code := runRoute(t, proj, env, consentOK()...)
	if code != 0 || facts["result"] != "routed" {
		t.Fatalf("a failed import must not fail the install: exit %d %v", code, facts)
	}
	if facts["history_import"] != "failed" || facts["history_import_retry"] == "" {
		t.Errorf("want history_import=failed with a retry command, got %v", facts)
	}
	if _, err := os.Stat(filepath.Join(state, "context-guru", "dashboard-"+port+".db.history-imported")); !os.IsNotExist(err) {
		t.Errorf("a failed import left its done-marker (err=%v): the next install would never retry", err)
	}
	if _, code = runRoute(t, proj, env, consentOK()...); code != 0 {
		t.Fatalf("re-install exit %d", code)
	}
	if got := importCalls(t, calls); len(got) != 2 {
		t.Errorf("a failed import must be retried by the next install, calls %q", got)
	}
}
