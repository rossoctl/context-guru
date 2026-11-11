// Tests for #318: a machine-wide install left the plugin ENABLED in one project only.
//
// `/plugin` writes `enabledPlugins`, which decides whether this plugin's commands, skills and hooks
// exist in a session at all; install.sh writes routing, the port option and the status line. Nothing
// bridged the two, so `--scope user` run from the one project the plugin was enabled in routed every
// project on the machine and left `/context-guru:*` missing from all but that one — with the status
// line live and traffic proxied, which made it look like a broken install.
//
// The fix is the `installed_statusline` pattern applied to a second key we own conditionally: write
// `enabledPlugins[<plugin>] = true` into the file the machine-wide install is already editing, never
// over an explicit `false` (a user decision), and record that we added it so uninstall removes it only
// then. The tests below pin each of those three clauses, plus the project-scope boundary.
package plugin

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const pluginID = "context-guru@context-guru"

// enabledIn reads enabledPlugins[pluginID] from a settings file: (value, present).
func enabledIn(t *testing.T, path string) (any, bool) {
	t.Helper()
	ep, _ := readJSON(t, path)["enabledPlugins"].(map[string]any)
	v, ok := ep[pluginID]
	return v, ok
}

// recordedEnable reads the `$context-guru` record of an enablement we added.
func recordedEnable(t *testing.T, path string) any {
	t.Helper()
	meta, _ := readJSON(t, path)["$context-guru"].(map[string]any)
	return meta["installed_enabled_plugin"]
}

// userScopeInstall runs a consented machine-wide install from a project that has the plugin enabled
// in its own settings.local.json only — the exact state the issue was reported from. `seed` is
// merged into ~/.claude/settings.json first (the port option is always added).
func userScopeInstall(t *testing.T, seed map[string]any) (facts map[string]string, home, state, proj string) {
	t.Helper()
	home, state, proj = t.TempDir(), t.TempDir(), t.TempDir()
	port := freePort(t)
	writePluginOptions(t, home, map[string]any{"port": port})
	userFile := filepath.Join(home, ".claude", "settings.json")
	if len(seed) > 0 {
		data := readJSON(t, userFile)
		for k, v := range seed {
			data[k] = v
		}
		writeJSON(t, userFile, data)
	}
	if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(proj, ".claude", "settings.local.json"),
		map[string]any{"enabledPlugins": map[string]any{pluginID: true}})
	env := routeEnv(t, home, state, fakeProxyDir(t, port, true))
	t.Cleanup(func() { stopFakeProxy(t, state, port) })
	facts, code := runRoute(t, proj, env, "--scope", "user", "--i-understand-machine-wide",
		"--i-consent-to-traffic-interception")
	if code != 0 || facts["result"] != "routed" {
		t.Fatalf("the install itself failed, so enablement proves nothing: exit %d %v", code, facts)
	}
	return facts, home, state, proj
}

func TestUserScopeInstallEnablesThePluginMachineWide(t *testing.T) {
	facts, home, _, proj := userScopeInstall(t, map[string]any{
		// Somebody else's plugins, which must come through untouched.
		"enabledPlugins": map[string]any{"pyright-lsp@claude-plugins-official": true},
	})
	userFile := filepath.Join(home, ".claude", "settings.json")
	if v, _ := enabledIn(t, userFile); v != true {
		t.Errorf("enabledPlugins[%s] = %v in the machine-wide file, want true — the commands exist "+
			"only in the project the install was run from", pluginID, v)
	}
	ep, _ := readJSON(t, userFile)["enabledPlugins"].(map[string]any)
	if ep["pyright-lsp@claude-plugins-official"] != true {
		t.Errorf("another plugin's enablement was disturbed: %v", ep)
	}
	if got := recordedEnable(t, userFile); got != pluginID {
		t.Errorf("$context-guru.installed_enabled_plugin = %v, want %q: without the record uninstall "+
			"cannot tell our key from one the user set", got, pluginID)
	}
	// Reported as a key it set — the issue's other half: silence was the defect.
	if facts["plugin_enabled"] != "added" {
		t.Errorf("plugin_enabled=%q, want added: %v", facts["plugin_enabled"], facts)
	}
	// The project's own enablement is the user's, and stays exactly as it was.
	if v, _ := enabledIn(t, filepath.Join(proj, ".claude", "settings.local.json")); v != true {
		t.Errorf("the project's own enablement changed: %v", v)
	}
}

// An explicit `false` in the machine-wide file is a decision the user made in `/plugin`. The install
// must neither flip it nor stay silent about what it means: commands will not appear elsewhere.
func TestUserScopeInstallNeverFlipsAnExplicitFalse(t *testing.T) {
	facts, home, _, _ := userScopeInstall(t, map[string]any{
		"enabledPlugins": map[string]any{pluginID: false},
	})
	userFile := filepath.Join(home, ".claude", "settings.json")
	if v, ok := enabledIn(t, userFile); !ok || v != false {
		t.Errorf("enabledPlugins[%s] = %v (present=%v), want the user's explicit false", pluginID, v, ok)
	}
	if got := recordedEnable(t, userFile); got != nil {
		t.Errorf("recorded an enablement we did not write (%v), so uninstall would delete the "+
			"user's own false", got)
	}
	if facts["plugin_enabled"] != "explicitly_disabled" || facts["plugin_enabled_note"] == "" {
		t.Errorf("plugin_enabled=%q note=%q, want explicitly_disabled with a note",
			facts["plugin_enabled"], facts["plugin_enabled_note"])
	}
}

// Already enabled machine-wide by the user: nothing to write, nothing recorded, and so nothing for
// uninstall to take away.
func TestUserScopeInstallLeavesAnExistingEnableAloneAndUninstallKeepsIt(t *testing.T) {
	facts, home, state, proj := userScopeInstall(t, map[string]any{
		"enabledPlugins": map[string]any{pluginID: true},
	})
	userFile := filepath.Join(home, ".claude", "settings.json")
	if facts["plugin_enabled"] != "already" {
		t.Errorf("plugin_enabled=%q, want already: %v", facts["plugin_enabled"], facts)
	}
	if got := recordedEnable(t, userFile); got != nil {
		t.Errorf("recorded ownership of a key the user set: %v", got)
	}
	out, code := settingsInDir(t, state, home, proj, "remove", "--file", userFile, "--user-scope")
	if code != 0 || out["result"] != "removed" {
		t.Fatalf("uninstall failed: exit %d %v", code, out)
	}
	if v, _ := enabledIn(t, userFile); v != true {
		t.Errorf("uninstall removed an enablement the user set themselves: %v", v)
	}
	if out["plugin_enabled_removed"] != "false" {
		t.Errorf("plugin_enabled_removed=%q, want false", out["plugin_enabled_removed"])
	}
}

// Uninstall takes back exactly what we added, and nothing next to it.
func TestUserScopeUninstallRemovesOnlyTheEnableItAdded(t *testing.T) {
	_, home, state, proj := userScopeInstall(t, map[string]any{
		"enabledPlugins": map[string]any{"pyright-lsp@claude-plugins-official": true},
	})
	userFile := filepath.Join(home, ".claude", "settings.json")
	out, code := settingsInDir(t, state, home, proj, "remove", "--file", userFile, "--user-scope")
	if code != 0 || out["result"] != "removed" {
		t.Fatalf("uninstall failed: exit %d %v", code, out)
	}
	if out["plugin_enabled_removed"] != "true" {
		t.Errorf("plugin_enabled_removed=%q, want true: %v", out["plugin_enabled_removed"], out)
	}
	if _, ok := enabledIn(t, userFile); ok {
		t.Errorf("our enablement survived uninstall")
	}
	ep, _ := readJSON(t, userFile)["enabledPlugins"].(map[string]any)
	if ep["pyright-lsp@claude-plugins-official"] != true {
		t.Errorf("uninstall touched another plugin's enablement: %v", ep)
	}
	if got := recordedEnable(t, userFile); got != nil {
		t.Errorf("the record outlived the key it describes: %v", got)
	}
}

// If the user switched it to false after we enabled it, that is now their decision: keep it.
func TestUserScopeUninstallKeepsAFalseTheUserSetAfterInstall(t *testing.T) {
	_, home, state, proj := userScopeInstall(t, nil)
	userFile := filepath.Join(home, ".claude", "settings.json")
	data := readJSON(t, userFile)
	data["enabledPlugins"].(map[string]any)[pluginID] = false
	writeJSON(t, userFile, data)
	if out, code := settingsInDir(t, state, home, proj, "remove", "--file", userFile,
		"--user-scope"); code != 0 || out["result"] != "removed" {
		t.Fatalf("uninstall failed: exit %d %v", code, out)
	}
	if v, ok := enabledIn(t, userFile); !ok || v != false {
		t.Errorf("uninstall removed a false the user set after install: %v (present=%v)", v, ok)
	}
}

// Re-running the machine-wide install is how a user who installed before this fix repairs their
// machine, so the enablement must be written on the `unchanged` path too, and exactly once.
func TestUserScopeReinstallIsIdempotentAndRepairsAnOlderInstall(t *testing.T) {
	_, home, state, proj := userScopeInstall(t, nil)
	userFile := filepath.Join(home, ".claude", "settings.json")
	// The state an install from before this fix left behind: routed, not enabled, no record.
	data := readJSON(t, userFile)
	delete(data, "enabledPlugins")
	delete(data["$context-guru"].(map[string]any), "installed_enabled_plugin")
	writeJSON(t, userFile, data)

	port := readJSON(t, userFile)["pluginConfigs"].(map[string]any)[pluginID].(map[string]any)["options"].(map[string]any)["port"].(string)
	env := routeEnv(t, home, state, fakeProxyDir(t, port, true))
	for i, want := range []string{"added", "already"} {
		facts, code := runRoute(t, proj, env, "--scope", "user", "--i-understand-machine-wide",
			"--i-consent-to-traffic-interception")
		if code != 0 || facts["result"] != "routed" {
			t.Fatalf("re-run %d failed: exit %d %v", i, code, facts)
		}
		if facts["plugin_enabled"] != want {
			t.Errorf("re-run %d: plugin_enabled=%q, want %q", i, facts["plugin_enabled"], want)
		}
		if v, _ := enabledIn(t, userFile); v != true {
			t.Errorf("re-run %d: not enabled: %v", i, v)
		}
		if got := recordedEnable(t, userFile); got != pluginID {
			t.Errorf("re-run %d: the second run lost the first run's record: %v", i, got)
		}
	}
}

// A project-scope install writes nothing machine-wide — enablement included.
func TestProjectScopeInstallDoesNotEnableMachineWide(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	port := freePort(t)
	writePluginOptions(t, home, map[string]any{"port": port})
	env := routeEnv(t, home, state, fakeProxyDir(t, port, true))
	t.Cleanup(func() { stopFakeProxy(t, state, port) })
	facts, code := runRoute(t, proj, env, "--scope", "project", "--i-consent-to-traffic-interception")
	if code != 0 || facts["result"] != "routed" {
		t.Fatalf("exit %d: %v", code, facts)
	}
	if _, ok := enabledIn(t, filepath.Join(home, ".claude", "settings.json")); ok {
		t.Errorf("a project-scope install enabled the plugin machine-wide")
	}
	if _, ok := facts["plugin_enabled"]; ok {
		t.Errorf("plugin_enabled reported for a project-scope install: %v", facts)
	}
}

// The scope gate holds for this key as for routing: the machine-wide file needs --user-scope.
func TestEnablePluginRefusesTheUserFileWithoutTheFlag(t *testing.T) {
	home, state := t.TempDir(), t.TempDir()
	userFile := filepath.Join(home, ".claude", "settings.json")
	writePluginOptions(t, home, map[string]any{})
	out, code := settingsInDir(t, state, home, "", "enable-plugin", "--file", userFile)
	if code != 2 || out["reason"] != "user_scope_needs_flag" {
		t.Errorf("exit %d %v, want the scope refusal", code, out)
	}
	if _, ok := enabledIn(t, userFile); ok {
		t.Errorf("written despite the refusal")
	}
}

// seedUserFile writes a machine-wide settings file holding only the port option and `enabledPlugins`
// set to `ep` — any JSON value, since the malformed shapes are the point of some tests below.
func seedUserFile(t *testing.T, ep any) (home, state, userFile string) {
	t.Helper()
	home, state = t.TempDir(), t.TempDir()
	writePluginOptions(t, home, map[string]any{})
	userFile = filepath.Join(home, ".claude", "settings.json")
	data := readJSON(t, userFile)
	data["enabledPlugins"] = ep
	writeJSON(t, userFile, data)
	return home, state, userFile
}

// An emptied block is litter in a file that had none.
func TestUserScopeUninstallLeavesNoEmptyEnabledPluginsBlock(t *testing.T) {
	_, home, state, proj := userScopeInstall(t, nil)
	userFile := filepath.Join(home, ".claude", "settings.json")
	if out, code := settingsInDir(t, state, home, proj, "remove", "--file", userFile,
		"--user-scope"); code != 0 || out["plugin_enabled_removed"] != "true" {
		t.Fatalf("uninstall: exit %d %v", code, out)
	}
	if v, ok := readJSON(t, userFile)["enabledPlugins"]; ok {
		t.Errorf("uninstall left enabledPlugins=%v behind in a file that had none", v)
	}
}

func TestEnablePluginRefusesANonObjectEnabledPlugins(t *testing.T) {
	home, state, userFile := seedUserFile(t, []any{"x"})
	before, _ := os.ReadFile(userFile)
	out, code := settingsInDir(t, state, home, "", "enable-plugin", "--file", userFile, "--user-scope")
	if code != 3 || out["reason"] != "enabledPlugins_not_an_object" {
		t.Errorf("exit %d %v, want exit 3 enabledPlugins_not_an_object", code, out)
	}
	if after, _ := os.ReadFile(userFile); string(after) != string(before) {
		t.Errorf("file changed despite the refusal:\n%s", after)
	}
}

// `null` is absence, not a malformed block — and it used to crash with a traceback, which install.sh
// then reported as "could not enable ... ()".
func TestEnablePluginTreatsANullBlockAsAbsent(t *testing.T) {
	home, state, userFile := seedUserFile(t, nil)
	out, code := settingsInDir(t, state, home, "", "enable-plugin", "--file", userFile, "--user-scope")
	if code != 0 || out["plugin_enabled"] != "added" {
		t.Fatalf("exit %d %v, want plugin_enabled=added", code, out)
	}
	if v, _ := enabledIn(t, userFile); v != true {
		t.Errorf("not enabled: %v", v)
	}
}

// install.sh's own `skipped` branch, and the note that must name a reason rather than "()".
func TestUserScopeInstallReportsSkippedForANonObjectEnabledPlugins(t *testing.T) {
	facts, home, _, _ := userScopeInstall(t, map[string]any{"enabledPlugins": []any{"x"}})
	if facts["plugin_enabled"] != "skipped" || !strings.Contains(facts["plugin_enabled_note"], "(not_an_object)") {
		t.Errorf("plugin_enabled=%q note=%q, want skipped with a reasoned note", facts["plugin_enabled"],
			facts["plugin_enabled_note"])
	}
	if got := readJSON(t, filepath.Join(home, ".claude", "settings.json"))["enabledPlugins"]; !reflect.DeepEqual(got, []any{"x"}) {
		t.Errorf("enabledPlugins replaced: %v", got)
	}
}

func TestEnablePluginLeavesANonBooleanValueAlone(t *testing.T) {
	home, state, userFile := seedUserFile(t, map[string]any{pluginID: "yes"})
	out, code := settingsInDir(t, state, home, "", "enable-plugin", "--file", userFile, "--user-scope")
	if code != 0 || out["plugin_enabled"] != "present" {
		t.Errorf("exit %d %v, want plugin_enabled=present", code, out)
	}
	if v, _ := enabledIn(t, userFile); v != "yes" {
		t.Errorf("value overwritten: %v", v)
	}
	if got := recordedEnable(t, userFile); got != nil {
		t.Errorf("recorded ownership of a value we did not write: %v", got)
	}
}

func TestEnablePluginBackupFlag(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want int
	}{{nil, 1}, {[]string{"--no-backup"}, 0}} {
		home, state, userFile := seedUserFile(t, map[string]any{})
		args := append([]string{"enable-plugin", "--file", userFile, "--user-scope"}, tc.args...)
		out, code := settingsInDir(t, state, home, "", args...)
		if code != 0 || out["plugin_enabled"] != "added" {
			t.Fatalf("%v: exit %d %v", tc.args, code, out)
		}
		if got := len(backupsUnder(t, userFile)); got != tc.want {
			t.Errorf("%v: %d backups, want %d", tc.args, got, tc.want)
		}
	}
}

// A first install is covered by the routing write's own backup, so the enable step adds none: every
// backup left must predate routing.
func TestUserScopeInstallEnableStepTakesNoPostRoutingBackup(t *testing.T) {
	_, home, _, _ := userScopeInstall(t, nil)
	for _, b := range backupsUnder(t, filepath.Join(home, ".claude", "settings.json")) {
		raw, _ := os.ReadFile(b)
		if strings.Contains(string(raw), "ANTHROPIC_BASE_URL") {
			t.Errorf("%s is a post-routing checkpoint (enable-plugin ran without --no-backup)", b)
		}
	}
}

// ...but a re-run whose routing write was `unchanged` took no backup, so the enable step must take
// one itself rather than claim it was covered.
func TestUserScopeReinstallBacksUpBeforeEnablingWhenRoutingWasUnchanged(t *testing.T) {
	_, home, state, proj := userScopeInstall(t, nil)
	userFile := filepath.Join(home, ".claude", "settings.json")
	port := readJSON(t, userFile)["pluginConfigs"].(map[string]any)[pluginID].(map[string]any)["options"].(map[string]any)["port"].(string)
	env := routeEnv(t, home, state, fakeProxyDir(t, port, true))
	rerun := func() map[string]string {
		facts, code := runRoute(t, proj, env, "--scope", "user", "--i-understand-machine-wide",
			"--i-consent-to-traffic-interception")
		if code != 0 || facts["result"] != "routed" {
			t.Fatalf("re-run: exit %d %v", code, facts)
		}
		return facts
	}
	// One settling re-run first, so any other write a re-run makes (the port option) has been made
	// and backed up already, and the enable step below is the ONLY write left.
	rerun()
	data := readJSON(t, userFile)
	delete(data, "enabledPlugins")
	delete(data["$context-guru"].(map[string]any), "installed_enabled_plugin")
	writeJSON(t, userFile, data)
	n := len(backupsUnder(t, userFile))
	facts := rerun()
	if facts["settings_result"] != "unchanged" || facts["plugin_enabled"] != "added" {
		t.Fatalf("want settings_result=unchanged plugin_enabled=added: %v", facts)
	}
	if got := len(backupsUnder(t, userFile)); got <= n {
		t.Errorf("the enable write on an unchanged re-run took no backup (%d before, %d after)", n, got)
	}
}

// Routing removed some other way first: uninstall is then the no-routing branch of `remove`, and it
// must still take our enablement back, or the plugin stays enabled everywhere after a clean uninstall.
func TestUninstallRemovesTheEnableEvenWhenRoutingIsAlreadyGone(t *testing.T) {
	_, home, state, proj := userScopeInstall(t, nil)
	userFile := filepath.Join(home, ".claude", "settings.json")
	data := readJSON(t, userFile)
	delete(data["env"].(map[string]any), "ANTHROPIC_BASE_URL")
	writeJSON(t, userFile, data)
	out, code := settingsInDir(t, state, home, proj, "remove", "--file", userFile, "--user-scope")
	if code != 0 || out["result"] != "removed" || out["plugin_enabled_removed"] != "true" {
		t.Fatalf("exit %d %v, want removed with plugin_enabled_removed=true", code, out)
	}
	if _, ok := enabledIn(t, userFile); ok {
		t.Errorf("our enablement survived the uninstall")
	}
	if got := recordedEnable(t, userFile); got != nil {
		t.Errorf("record survived: %v", got)
	}
}

// Added by us, switched off by the user, install re-run (reports explicitly_disabled), then switched
// back on by the user: that last `true` is theirs, and uninstall must not take it.
func TestAReEnableByTheUserAfterTheyDisabledItIsTheirs(t *testing.T) {
	_, home, state, proj := userScopeInstall(t, nil)
	userFile := filepath.Join(home, ".claude", "settings.json")
	setTo := func(v bool) {
		data := readJSON(t, userFile)
		data["enabledPlugins"].(map[string]any)[pluginID] = v
		writeJSON(t, userFile, data)
	}
	setTo(false)
	out, code := settingsInDir(t, state, home, proj, "enable-plugin", "--file", userFile, "--user-scope")
	if code != 0 || out["plugin_enabled"] != "explicitly_disabled" {
		t.Fatalf("exit %d %v", code, out)
	}
	if got := recordedEnable(t, userFile); got != nil {
		t.Errorf("record kept over a false the user set: %v", got)
	}
	setTo(true)
	if out, code := settingsInDir(t, state, home, proj, "remove", "--file", userFile,
		"--user-scope"); code != 0 || out["plugin_enabled_removed"] != "false" {
		t.Errorf("exit %d %v, want plugin_enabled_removed=false", code, out)
	}
	if v, _ := enabledIn(t, userFile); v != true {
		t.Errorf("uninstall took the user's own re-enable: %v", v)
	}
}

// The enablement is a machine-wide write in the same file as the routing, so the sentence the user
// agrees to has to name it — consent to "route every project" is not consent to a second key. And a
// project install writes no enablement, so its question must not promise one.
func TestConsentQuestionNamesTheMachineWideEnable(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	env := routeEnv(t, home, state, "")
	const clause = "enable the context-guru plugin in every project"
	facts, code := runRoute(t, proj, env, "--plan", "--scope", "user", "--i-understand-machine-wide")
	if code != 0 || facts["result"] != "planned" {
		t.Fatalf("plan: exit %d result=%q: %v", code, facts["result"], facts)
	}
	if !strings.Contains(facts["consent_question"], clause) {
		t.Errorf("machine-wide consent_question does not mention the enablement it will write: %q",
			facts["consent_question"])
	}
	facts, code = runRoute(t, proj, env, "--plan", "--scope", "project")
	if code != 0 || facts["result"] != "planned" {
		t.Fatalf("project plan: exit %d result=%q: %v", code, facts["result"], facts)
	}
	if strings.Contains(facts["consent_question"], clause) {
		t.Errorf("project consent_question promises a machine-wide enablement it never writes: %q",
			facts["consent_question"])
	}
}

// A PROJECT uninstall walks ~/.claude/settings.json too, without --user-scope, and relies on `remove`
// refusing it. With the routing already gone from that file (a reset, a hand edit), the no-routing
// branch used to take our enablement anyway — one project's uninstall switching the plugin's commands
// off in every project, with no question asked. The machine-wide enablement goes only with the flag.
func TestAProjectUninstallNeverTakesTheMachineWideEnable(t *testing.T) {
	_, home, state, proj := userScopeInstall(t, nil)
	userFile := filepath.Join(home, ".claude", "settings.json")
	data := readJSON(t, userFile)
	delete(data["env"].(map[string]any), "ANTHROPIC_BASE_URL")
	writeJSON(t, userFile, data)
	out, code := settingsInDir(t, state, home, proj, "remove", "--file", userFile)
	if code != 0 || out["plugin_enabled_removed"] == "true" {
		t.Fatalf("exit %d %v: removing the machine-wide enablement needs --user-scope", code, out)
	}
	if v, _ := enabledIn(t, userFile); v != true {
		t.Errorf("a project uninstall disabled the plugin machine-wide: %v", v)
	}
	if got := recordedEnable(t, userFile); got != pluginID {
		t.Errorf("record dropped (%v), so the real machine-wide uninstall could no longer take it back", got)
	}
	// And the deliberate machine-wide uninstall still does.
	out, code = settingsInDir(t, state, home, proj, "remove", "--file", userFile, "--user-scope")
	if code != 0 || out["plugin_enabled_removed"] != "true" {
		t.Errorf("exit %d %v, want plugin_enabled_removed=true with --user-scope", code, out)
	}
}

// A record of ours over a value the user changed since is stale; `remove` drops it, and the drop has
// to reach the disk even when nothing else in the file changed, or it outlives the uninstall.
func TestUninstallDropsAStaleRecordEvenWhenNothingElseChanged(t *testing.T) {
	_, home, state, proj := userScopeInstall(t, nil)
	userFile := filepath.Join(home, ".claude", "settings.json")
	data := readJSON(t, userFile)
	delete(data["env"].(map[string]any), "ANTHROPIC_BASE_URL")
	delete(data, "statusLine")
	delete(data["$context-guru"].(map[string]any), "installed_statusline")
	data["enabledPlugins"].(map[string]any)[pluginID] = false
	writeJSON(t, userFile, data)
	if _, code := settingsInDir(t, state, home, proj, "remove", "--file", userFile,
		"--user-scope"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if v, _ := enabledIn(t, userFile); v != false {
		t.Errorf("the user's false was touched: %v", v)
	}
	if got := recordedEnable(t, userFile); got != nil {
		t.Errorf("stale record survived on disk: %v", got)
	}
}
