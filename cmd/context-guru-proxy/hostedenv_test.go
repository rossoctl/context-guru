package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMain doubles as the harness for booting the REAL main() in a subprocess.
// Startup refusals are log.Fatalf, so nothing short of a process can observe them —
// and a unit test on an extracted helper would not prove that the refusal is wired
// into the hosted-mode path, which is the whole property under test.
func TestMain(m *testing.M) {
	if os.Getenv("CG_TEST_RUN_MAIN") == "1" {
		// Set before flag.Parse: the test binary's own -test.* flags would otherwise
		// make main() reject its command line for the wrong reason.
		os.Args = append([]string{"context-guru-proxy"},
			strings.Fields(os.Getenv("CG_TEST_MAIN_ARGS"))...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runMain boots main() in a subprocess with env, and returns its combined output.
// exited is false when the process was still running at the deadline — i.e. it booted
// and started serving, which for a refusal test is the failure.
func runMain(t *testing.T, wait time.Duration, env ...string) (out string, code int, exited bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0]) //nolint:gosec // the test binary itself
	cmd.Env = append(os.Environ(), "CG_TEST_RUN_MAIN=1", "LISTEN_ADDR=127.0.0.1:0")
	cmd.Env = append(cmd.Env, env...)
	b, _ := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return string(b), 0, false // killed at the deadline: it was serving happily
	}
	return string(b), cmd.ProcessState.ExitCode(), true
}

// hostedEnv is the minimum a hosted boot needs: an allow-list naming no key_env (so
// no upstream credential has to exist) and private database paths.
func hostedEnv(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	ups := filepath.Join(dir, "upstreams.yaml")
	if err := os.WriteFile(ups, []byte(
		"upstreams:\n  - name: up\n    dialect: anthropic\n    base_url: https://upstream.invalid\n"),
		0o600); err != nil {
		t.Fatal(err)
	}
	return []string{
		"UPSTREAMS=" + ups,
		"CONTROL_DB=" + filepath.Join(dir, "control.db"),
		"DASHBOARD=false",
	}
}

// Finding A. CONTEXT_GURU_DUMP / CONTEXT_GURU_CAPTURE append every request body to one
// process-wide file with no tenant attribution, on a path that never runs the redactor.
// In hosted mode that is a cross-tenant transcript leak that bypasses per-tenant capture
// consent, so the boot must REFUSE rather than warn — one careless Environment= line
// must not be able to turn it on.
func TestHostedModeRefusesCaptureEnv(t *testing.T) {
	for _, v := range []string{"CONTEXT_GURU_DUMP", "CONTEXT_GURU_CAPTURE"} {
		t.Run(v, func(t *testing.T) {
			env := append(hostedEnv(t), v+"="+filepath.Join(t.TempDir(), "leak.jsonl"))
			out, code, exited := runMain(t, 15*time.Second, env...)
			if !exited {
				t.Fatalf("hosted mode BOOTED with %s set; it must refuse to start.\noutput:\n%s", v, out)
			}
			if code == 0 {
				t.Errorf("exit code 0 with %s set; want a startup refusal.\noutput:\n%s", v, out)
			}
			if !strings.Contains(out, v) {
				t.Errorf("the refusal does not name %s, so an operator cannot act on it:\n%s", v, out)
			}
		})
	}
}

// The same hooks stay available in single-tenant/local mode, where there is one
// tenant and the file is the operator's own traffic.
func TestSingleTenantKeepsCaptureEnv(t *testing.T) {
	dump := filepath.Join(t.TempDir(), "dump.jsonl")
	// A short wait on purpose: the property is "it did NOT refuse", and a refusal
	// happens in the first moments of boot. Still serving at the deadline is the pass.
	out, code, exited := runMain(t, 4*time.Second, "CONTEXT_GURU_DUMP="+dump, "CONTEXT_GURU_CAPTURE="+dump)
	if exited && code != 0 {
		t.Fatalf("single-tenant mode refused to start with the capture hooks set (exit %d):\n%s", code, out)
	}
}

// Finding C, part 1: the shipped default for the per-tenant row quota. `flag` prints a
// default only when it is not the zero value, so "(default N)" on this line is exactly
// the assertion "the quota is in force out of the box".
func TestRowQuotaDefaultIsNonZero(t *testing.T) {
	out, _, _ := runMain(t, 20*time.Second, "CG_TEST_MAIN_ARGS=-h")
	// One flag's usage block: its own line plus the indented description, up to the
	// next flag. Scoped so a LATER flag's default cannot satisfy the assertion.
	block := regexp.MustCompile(`(?s)\n  -dashboard-max-rows-per-tenant.*?(\n  -|$)`).FindString(out)
	if block == "" {
		t.Fatalf("no -dashboard-max-rows-per-tenant flag in the usage output:\n%s", out)
	}
	m := regexp.MustCompile(`\(default (\d+)\)`).FindStringSubmatch(block)
	if m == nil {
		t.Fatalf("the per-tenant row quota still defaults to 0, so one tenant can fill the "+
			"database and the global byte rule then evicts everyone else's history:\n%s", block)
	}
	if n, _ := strconv.Atoi(m[1]); n <= 0 {
		t.Errorf("row quota default = %d; want a positive cap", n)
	}
}

// Finding C, part 2: the shipped unit must set it too — a default in the binary that the
// deployed unit overrides back to 0 would be no defence at all.
func TestShippedUnitSetsRowQuota(t *testing.T) {
	b, err := os.ReadFile("../../deploy/service/context-guru.service")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^Environment=DASHBOARD_MAX_ROWS_PER_TENANT=(\d+)\s*$`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("context-guru.service does not set DASHBOARD_MAX_ROWS_PER_TENANT")
	}
	if n, _ := strconv.Atoi(m[1]); n <= 0 {
		t.Errorf("the unit sets DASHBOARD_MAX_ROWS_PER_TENANT=%d, disabling the quota", n)
	}
}
