package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// installSH runs script with deploy/service/install.sh sourced, so a test exercises the
// real functions instead of a copy of them. The file's dispatcher is cut off first —
// sourcing it whole would run a subcommand. args arrive as $1, $2, …
func installSH(t *testing.T, script string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	src, err := filepath.Abs("../../deploy/service/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	const cut = `case "${1:-preflight}" in`
	if !strings.Contains(string(b), cut) {
		t.Fatalf("install.sh no longer dispatches on %q, so this test cannot source it", cut)
	}
	prelude := `set -euo pipefail
source <(sed '/^` + cut + `/,$d' "$CG_INSTALL_SH")
`
	cmd := exec.Command("bash", append([]string{"-c", prelude + script, "install.sh"}, args...)...)
	cmd.Env = append(os.Environ(), "CG_INSTALL_SH="+src)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash: %v\n%s", err, out)
	}
	return string(out)
}

// Preflight reported `credential for SOME_VAR … missing or empty` on a healthy host.
// SOME_VAR was never configured anywhere: it comes from the paragraph in upstreams.yaml
// that DOCUMENTS key_env. That comment ships with the file, so preflight cried wolf on
// every host — and a preflight that cries wolf is the one people learn to ignore.
func TestPreflightReadsKeyEnvFromConfigNotComments(t *testing.T) {
	// The shipped example carries the documenting comment verbatim. Two real entries and
	// a trailing comment are appended, because the case that must STILL be checked is
	// the configured one.
	example, err := os.ReadFile("../../deploy/service/upstreams.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(example), "key_env: SOME_VAR") {
		t.Fatal("upstreams.example.yaml no longer documents key_env in a comment; " +
			"keep this fixture honest or drop the test")
	}
	yaml := filepath.Join(t.TempDir(), "upstreams.yaml")
	if err := os.WriteFile(yaml, append(example, []byte(`
  - name: gateway
    dialect: anthropic
    base_url: https://example.invalid
    key_env: UPSTREAM_GATEWAY_KEY
  - name: other
    dialect: openai
    base_url: https://example.invalid
    key_env: UPSTREAM_OTHER_KEY  # one shared budget
  - name: quoted
    dialect: anthropic  # unset, key_env: UPSTREAM_NEVER_CONFIGURED
    key_env: "UPSTREAM_QUOTED_KEY"
  - name: single
    dialect: openai
    key_env: 'UPSTREAM_SINGLE_KEY'
  - name: quotedkey
    dialect: openai
    "key_env": UPSTREAM_QUOTED_NAME_KEY
`)...), 0o600); err != nil {
		t.Fatal(err)
	}

	// Every shape that the loader accepts has to be extracted. A name that is NOT
	// extracted is the dangerous direction: config/upstreams.go refuses to boot on an
	// unset variable, so preflight would print PASSED and the service would then fail to
	// start on exactly the credential this checks. Quoting is the shape that shipped
	// broken; a quoted mapping KEY (`"key_env": X`) is legal YAML for the same field and
	// was missed for the same reason, so it is fixtured here too.
	//
	// UPSTREAM_NEVER_CONFIGURED is the other direction, on the same line as a real field:
	// comments are stripped wherever they start, not only on whole-comment lines. The
	// exact-match assertion below is what enforces it — an extra name fails the compare.
	// Its comma is load-bearing: comments have to be stripped BEFORE the line is split on
	// commas, or the split hands the tail of a comment back as configuration.
	got := strings.Fields(installSH(t, `configured_key_envs "$1"`, yaml))
	want := "UPSTREAM_GATEWAY_KEY UPSTREAM_OTHER_KEY UPSTREAM_QUOTED_KEY UPSTREAM_QUOTED_NAME_KEY UPSTREAM_SINGLE_KEY"
	if strings.Join(got, " ") != want {
		t.Errorf("configured_key_envs = %v; want [%s] — a commented key_env is prose, a real one is config, quoted or not", got, want)
	}

	// Flow style on ONE physical line, which is where the extraction used to lose names
	// wholesale. Two failures at once: only the LAST key_env on a line was taken, and the
	// `#` inside the quoted base_url was treated as a comment and erased the key_env after
	// it — so this line yielded NOTHING, and the loader demands both names.
	flow := filepath.Join(t.TempDir(), "upstreams.yaml")
	if err := os.WriteFile(flow, []byte(
		`upstreams: [{name: a, dialect: openai, base_url: 'https://x.invalid/#frag', key_env: UPSTREAM_FLOW_A_KEY}, `+
			`{name: b, dialect: openai, base_url: 'https://x.invalid', key_env: UPSTREAM_FLOW_B_KEY}]`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got = strings.Fields(installSH(t, `configured_key_envs "$1"`, flow))
	want = "UPSTREAM_FLOW_A_KEY UPSTREAM_FLOW_B_KEY"
	if strings.Join(got, " ") != want {
		t.Errorf("configured_key_envs on flow style = %v; want [%s] — every entry on the line configures a credential, not just the last", got, want)
	}

	// An allow-list that configures none is the normal case (caller-pays), and must be
	// silent rather than a pipeline failure.
	only := filepath.Join(t.TempDir(), "upstreams.yaml")
	if err := os.WriteFile(only, example, 0o600); err != nil {
		t.Fatal(err)
	}
	if out := installSH(t, `configured_key_envs "$1"; echo "rc=$?"`, only); strings.TrimSpace(out) != "rc=0" {
		t.Errorf("a comment-only allow-list produced %q; want no variables and a clean status", out)
	}
}

// The rclone check used bare `command -v`, which resolves against sudo's secure_path —
// /sbin:/bin:/usr/sbin:/usr/bin, no /usr/local/bin. rclone lives in /usr/local/bin, and
// the nightly backup unit — which inherits systemd's PATH — had been uploading to Box
// successfully for weeks while preflight declared it missing. The check has to resolve
// the way the unit does.
func TestPreflightResolvesToolsOnTheServicePATH(t *testing.T) {
	dir := t.TempDir()
	// A tool that exists ONLY on the service's PATH — rclone's situation on the host.
	svcBin := filepath.Join(dir, "svcbin")
	fake := filepath.Join(dir, "fake")
	callerBin := filepath.Join(dir, "callerbin")
	for _, d := range []string{svcBin, fake, callerBin} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(svcBin, "cg-only-on-service-path"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// And a tool that exists only on the CALLER's PATH. The two PATHs are genuinely
	// different sets — install.sh's own header documents the direction that bit us,
	// /usr/local/bin being in systemd's PATH and absent from sudo's secure_path — and a
	// set difference has two directions. The reverse one produces a false PASS: a tool
	// preflight can see but the unit cannot must be reported MISSING, or the check passes
	// and the unit then fails on the tool it was supposed to vouch for. Asserting only
	// that a present tool is found cannot catch that.
	if err := os.WriteFile(filepath.Join(callerBin, "cg-only-on-caller-path"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A stand-in systemctl that answers show-environment the way this host's does.
	if err := os.WriteFile(filepath.Join(fake, "systemctl"),
		[]byte("#!/bin/sh\necho LANG=en_US.UTF-8\necho PATH="+svcBin+":/usr/bin:/bin\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	// PATH here stands in for sudo's: the fake systemctl and the basics, NOT svcBin.
	out := installSH(t, `export PATH="`+fake+`:`+callerBin+`:/usr/bin:/bin"
command -v cg-only-on-caller-path >/dev/null && echo caller-tool-is-runnable-here=yes || echo caller-tool-is-runnable-here=NO
in_service_path cg-only-on-service-path && echo on-service-path=found || echo on-service-path=MISSED
in_service_path cg-only-on-caller-path && echo caller-only=FOUND || echo caller-only=missing
in_service_path cg-nowhere-at-all && echo nowhere=FOUND || echo nowhere=missing`,
	)
	// The first line keeps the third honest: without it, a typo in the tool name would
	// also report caller-only=missing and the assertion would pass for the wrong reason.
	for _, want := range []string{"caller-tool-is-runnable-here=yes", "on-service-path=found", "caller-only=missing", "nowhere=missing"} {
		if !strings.Contains(out, want) {
			t.Errorf("want %q in:\n%s", want, out)
		}
	}
}
