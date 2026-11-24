"""Tests for install_pi.py. The terminal, the programs it runs and the network are faked at their boundary;
the files (models.json, settings.json, the package folder, the record) are real, in a temporary HOME."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

import install_pi  # noqa: E402

CANARY = "sk-ant-CANARY-0123456789-never-print-me"
PROXY = "http://127.0.0.1:8787/anthropic"
PLUGIN = str(HERE / "pi_plugin.py")


class FakeTerminal:
    """Answers in order. Each question and each line said is kept, to check what the user saw."""

    def __init__(self, answers=(), secrets=()):
        self.answers, self.secrets, self.shown = list(answers), list(secrets), []

    def say(self, text):
        self.shown.append(text)

    def ask(self, question, default=""):
        self.shown.append(question)
        answer = self.answers.pop(0) if self.answers else ""
        return answer or default

    def secret(self, question):
        self.shown.append(question)
        return self.secrets.pop(0) if self.secrets else ""


class FakeShell:
    """Answers per program. Every argv and every stdin is kept, to check that no key is in them."""

    def __init__(self, test):
        self.test, self.calls = test, []
        self.node, self.pi_version, self.latest = "v24.2.0", "1.5.2", "1.6.0"
        self.npm_install_error = None
        self.setup_results = ["routed"]
        self.plan = "planned"
        self.plan_preset, self.plan_strategy, self.restore = "off", "auto", "restored"
        self.keychain_has_item = True
        self.realistic, self.routing = False, None

    def which(self, name):
        if name == "pi":
            return "/usr/local/bin/pi" if self.pi_version else None
        return f"/usr/bin/{name}"

    def run(self, cmd, stdin=None):
        self.calls.append((list(cmd), stdin))
        ok = lambda out="": subprocess.CompletedProcess(cmd, 0, out, "")
        if cmd[:2] == ["node", "--version"]:
            return ok(self.node + "\n")
        if cmd[:2] == ["pi", "--version"]:
            return ok(self.pi_version + "\n")
        if cmd[:2] == ["npm", "view"]:
            return ok(self.latest + "\n")
        if cmd[:2] == ["npm", "uninstall"]:
            self.pi_version = None
            return ok()
        if cmd[:2] == ["npm", "install"]:
            if self.npm_install_error:
                return subprocess.CompletedProcess(cmd, 1, "", self.npm_install_error)
            self.pi_version = cmd[-1].rsplit("@", 1)[1]
            return ok()
        if cmd[:2] == ["pi", "install"]:
            self.test.settings_packages(add=cmd[2])
            return ok(f"Installed {cmd[2]}\n")
        if cmd[:2] == ["pi", "remove"]:
            self.test.settings_packages(remove=cmd[2])
            return ok(f"Removed {cmd[2]}\n")
        if cmd[:2] == ["security", "find-generic-password"]:
            return subprocess.CompletedProcess(cmd, 0 if self.keychain_has_item else 44, "", "")
        if cmd[0] in ("security", "secret-tool"):
            return ok()
        if cmd[:2] == ["python3", PLUGIN]:
            return ok(self.plugin(cmd[2:]))
        if cmd[0] == "pi" and "-p" in cmd:
            return ok("ok\n")
        raise AssertionError(f"unexpected command: {cmd}")

    def route(self):
        """Like models_route.install: the proxy URL in models.json, the user's value kept as `previous`."""
        models = self.test.models() or {}
        entry = models.setdefault("providers", {}).setdefault("anthropic", {})
        current = entry.get("baseUrl")
        if current and current != PROXY:
            self.routing = {"previous": current, "upstream": current}  # a clash: chain behind it
        elif not self.routing:
            self.routing = {"previous": None, "upstream": "https://api.anthropic.com"}
        # else: no clash, and pi_plugin keeps its recorded upstream (recorded_upstreams), like the real one
        entry["baseUrl"] = PROXY
        self.test.write_models(models)

    def unroute(self):
        if not self.routing:
            return "no_record"
        models = self.test.models() or {}
        entry = models.get("providers", {}).get("anthropic", {})
        if self.routing["previous"] is None:
            entry.pop("baseUrl", None)
        else:
            entry["baseUrl"] = self.routing["previous"]
        self.test.write_models(models)
        self.routing = None
        return "restored"

    def plugin(self, args):
        if self.realistic and args[:2] == ["setup", "--plan"]:
            current = ((self.test.models() or {}).get("providers", {}).get("anthropic", {}) or {}).get("baseUrl")
            if current and current != PROXY:
                return "result=needs_decision\nconsent_question_chain=CHAIN-CONSENT-TEXT\npreset=off\ncache_strategy=auto\n"
            return "result=planned\nconsent_question=PLAN-CONSENT-TEXT\npreset=off\ncache_strategy=auto\n"
        if self.realistic and args[0] == "setup":
            self.route()
            return "result=routed\nport=8787\npreset=off\n"
        if self.realistic and args[0] == "uninstall":
            return f"result=removing\nmodels_restore={self.unroute()}\nservice=stopped\nresult=removed\n"
        if args[:2] == ["setup", "--plan"]:
            if self.plan == "needs_decision":
                return ("result=needs_decision\nconsent_question_chain=CHAIN-CONSENT-TEXT\n"
                        "consent_question_replace=REPLACE-TEXT\n")
            if self.plan == "refused":
                return "result=refused\nreason=binary_named_missing\ndetail=CONTEXT_GURU_BIN names /nope\n"
            return f"result=planned\npreset={self.plan_preset}\ncache_strategy={self.plan_strategy}\nconsent_question=PLAN-CONSENT-TEXT\n"
        if args[0] == "setup":
            result = self.setup_results.pop(0) if self.setup_results else "routed"
            return (f"result={result}\nport=8787\npreset=off\n" if result == "routed"
                    else "result=error\nreason=health_check_failed\nlog=/state/proxy.log\n")
        if args[0] == "uninstall":
            return f"result=removing\nmodels_restore={self.restore}\nservice=stopped\nresult=removed\n"
        if args[0] == "configure":
            return f"result=configured\npreset={args[-1]}\n"
        if args[0] == "status":
            return "result=ok\nport=8787\nproxy_up=true\npreset=off\ncache_strategy=auto\n"
        raise AssertionError(f"unexpected plugin call: {args}")

    def argv_and_stdin(self):
        return [(" ".join(cmd), stdin or "") for cmd, stdin in self.calls]

    def ran(self, *prefix):
        return [cmd for cmd, _ in self.calls if cmd[:len(prefix)] == list(prefix)]


class Sandbox(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        env = {"HOME": str(root), "XDG_STATE_HOME": str(root / "state"), "XDG_DATA_HOME": str(root / "data"),
               "XDG_CONFIG_HOME": str(root / "config"), "PI_CODING_AGENT_DIR": str(root / "agent"),
               "USER": "tester"}
        patch = mock.patch.dict(os.environ, env)
        patch.start()
        self.addCleanup(patch.stop)
        self.root = root
        self.agent = root / "agent"
        self.agent.mkdir()
        self.source = root / "download/pi-package"  # what the bootstrap unpacked
        (self.source / "scripts").mkdir(parents=True)
        (self.source / "package.json").write_text(json.dumps({"name": "@rossoctl/context-guru-pi", "version": "0.1.6"}))
        (self.source / "scripts/pi_plugin.py").write_text("# fake\n")
        self.shell = FakeShell(self)
        self.http_codes, self.http_seen = [200], []
        self.platform = "Darwin"

    # --- files -----------------------------------------------------------------------------
    def models(self):
        path = self.agent / "models.json"
        return json.loads(path.read_text()) if path.exists() else None

    def settings(self):
        path = self.agent / "settings.json"
        return json.loads(path.read_text()) if path.exists() else None

    def write_models(self, document):
        (self.agent / "models.json").write_text(json.dumps(document))

    def settings_packages(self, add=None, remove=None):
        current = self.settings() or {}
        packages = current.get("packages", [])
        if add and add not in packages:
            packages.append(add)
        if remove:
            packages = [p for p in packages if p != remove]
        current["packages"] = packages
        (self.agent / "settings.json").write_text(json.dumps(current))

    # --- run -------------------------------------------------------------------------------
    def http(self, url, headers, body):
        self.http_seen.append((url, dict(headers), body))
        return self.http_codes.pop(0) if len(self.http_codes) > 1 else self.http_codes[0]

    def installer(self, terminal, confirm_folders=False):
        # The sandbox always sets PI_CODING_AGENT_DIR; only the Folders tests ask about it.
        return install_pi.Installer(terminal=terminal, shell=self.shell, http=self.http, source=self.source,
                                    plugin=PLUGIN, platform=self.platform, sleep=lambda s: None,
                                    confirm_folders=confirm_folders)

    def install(self, answers=(), secrets=()):
        terminal = FakeTerminal(answers, secrets)
        with mock.patch.object(install_pi, "PLUGIN_SCRIPT", PLUGIN):
            code = self.installer(terminal).install()
        return code, terminal

    def everything_printed(self, terminal):
        return "\n".join(terminal.shown)


class Checks(Sandbox):
    def test_node_older_than_22_19_stops_before_any_change(self):
        self.shell.node = "v22.18.0"
        code, terminal = self.install()
        self.assertEqual(code, 1)
        self.assertIn("Node.js 22.19 or later", self.everything_printed(terminal))
        self.assertEqual(self.shell.ran("npm", "install"), [])
        self.assertIsNone(self.models())


class PiItself(Sandbox):
    def test_missing_pi_is_installed_after_a_yes_from_the_public_registry(self):
        self.shell.pi_version = None
        code, terminal = self.install(answers=["y", "", "", "n"], secrets=[CANARY])  # install Pi, API, model, consent no
        self.assertIn(["npm", "install", "-g", "--registry=https://registry.npmjs.org/",
                       "@earendil-works/pi-coding-agent@1.6.0"], self.shell.ran("npm", "install"))
        self.assertIn("Install Pi 1.6.0?", self.everything_printed(terminal))

    def test_missing_pi_and_no_stops_and_changes_nothing(self):
        self.shell.pi_version = None
        code, terminal = self.install(answers=["n"])
        self.assertEqual(code, 1)
        self.assertEqual(self.shell.ran("npm", "install"), [])
        self.assertEqual(self.shell.ran("pi", "install"), [])

    def test_older_pi_is_updated_after_a_yes(self):
        code, terminal = self.install(answers=["y"])
        self.assertIn("Update Pi 1.5.2 to 1.6.0?", self.everything_printed(terminal))
        self.assertEqual(len(self.shell.ran("npm", "install")), 1)

    def test_older_pi_and_no_keeps_it(self):
        code, terminal = self.install(answers=["n"])
        self.assertEqual(self.shell.ran("npm", "install"), [])

    def test_current_pi_asks_nothing_about_pi(self):
        self.shell.pi_version = "1.6.0"
        _, terminal = self.install()
        self.assertNotIn("Update Pi", self.everything_printed(terminal))
        self.assertNotIn("Install Pi", self.everything_printed(terminal))

    def test_a_registry_can_be_chosen(self):
        with mock.patch.dict(os.environ, {"CG_PI_NPM_REGISTRY": "https://npm.example.com/"}):
            self.install(answers=["y"])
        self.assertIn("--registry=https://npm.example.com/", self.shell.ran("npm", "view")[0])

    def test_an_npm_error_stops_with_its_message_and_never_uses_sudo(self):
        self.shell.npm_install_error = "npm error code EACCES\nnpm error path /usr/local/lib/node_modules"
        code, terminal = self.install(answers=["y"])
        self.assertEqual(code, 1)
        self.assertIn("EACCES", self.everything_printed(terminal))
        self.assertFalse(any(cmd[0] == "sudo" for cmd, _ in self.shell.calls))


class Package(Sandbox):
    def stable(self):
        return self.root / "data/context-guru-pi/package"

    def test_the_package_goes_to_one_stable_path_and_into_pi(self):
        self.shell.pi_version = "1.6.0"
        self.install()
        self.assertTrue((self.stable() / "package.json").exists())
        self.assertEqual(self.shell.ran("pi", "install"), [["pi", "install", str(self.stable())]])

    def test_other_copies_of_the_package_are_removed_first(self):
        self.shell.pi_version = "1.6.0"
        clone = self.root / "clone/pi-package"
        clone.mkdir(parents=True)
        (clone / "package.json").write_text(json.dumps({"name": "@rossoctl/context-guru-pi"}))
        other = self.root / "other-pkg"
        other.mkdir()
        (other / "package.json").write_text(json.dumps({"name": "some-other-pi-package"}))
        self.settings_packages(add=str(clone))
        self.settings_packages(add=str(other))
        self.install()
        self.assertEqual(self.shell.ran("pi", "remove"), [["pi", "remove", str(clone)]])
        self.assertEqual(self.settings()["packages"], [str(other), str(self.stable())])

    def test_a_second_install_does_not_add_a_second_entry(self):
        self.shell.pi_version = "1.6.0"
        self.install()
        self.install()
        self.assertEqual(self.settings()["packages"], [str(self.stable())])


class Connection(Sandbox):
    """No connection yet: the installer asks, checks the key, then writes. Answers after the Pi question."""

    def gateway_answers(self):
        # Pi is current; gateway URL; bearer; model id; turn context-guru on; test prompt.
        return ["https://gw.example.com/", "", "aws/claude-haiku-5-5", "y", "n"]

    def test_a_gateway_connection_is_checked_then_written_with_a_keychain_command(self):
        self.shell.pi_version = "1.6.0"
        code, terminal = self.install(answers=self.gateway_answers(), secrets=[CANARY])
        self.assertEqual(code, 0, self.everything_printed(terminal))
        url, headers, body = self.http_seen[0]
        self.assertEqual(url, "https://gw.example.com/v1/messages")
        self.assertEqual(headers["authorization"], f"Bearer {CANARY}")
        self.assertEqual(json.loads(body)["max_tokens"], 1)
        anthropic = self.models()["providers"]["anthropic"]
        self.assertEqual(anthropic["baseUrl"], "https://gw.example.com")
        self.assertTrue(anthropic["authHeader"])
        self.assertEqual(anthropic["apiKey"],
                         "!security find-generic-password -s context-guru-pi-anthropic -a tester -w")
        self.assertEqual([m["id"] for m in anthropic["models"]], ["aws/claude-haiku-5-5"])
        self.assertEqual(self.settings()["defaultProvider"], "anthropic")
        self.assertEqual(self.settings()["defaultModel"], "aws/claude-haiku-5-5")

    def test_the_key_goes_to_the_keychain_on_stdin_never_in_argv(self):
        self.shell.pi_version = "1.6.0"
        self.install(answers=self.gateway_answers(), secrets=[CANARY])
        stored = [(cmd, stdin) for cmd, stdin in self.shell.calls if cmd[0] == "security"]
        self.assertEqual(stored[0][0], ["security", "-i"])
        self.assertIn(CANARY, stored[0][1])
        self.assertIn('add-generic-password -U -s context-guru-pi-anthropic -a "tester" -w', stored[0][1])

    def test_linux_uses_secret_tool_on_stdin(self):
        self.platform = "Linux"
        self.shell.pi_version = "1.6.0"
        self.install(answers=self.gateway_answers(), secrets=[CANARY])
        stored = [(cmd, stdin) for cmd, stdin in self.shell.calls if cmd[0] == "secret-tool"]
        self.assertEqual(stored[0][0][:2], ["secret-tool", "store"])
        self.assertEqual(stored[0][1], CANARY)
        self.assertEqual(self.models()["providers"]["anthropic"]["apiKey"],
                         "!secret-tool lookup service context-guru-pi-anthropic user tester")

    def test_linux_without_secret_tool_uses_a_private_file(self):
        self.platform = "Linux"
        self.shell.pi_version = "1.6.0"
        with mock.patch.object(self.shell, "which", side_effect=lambda n: None if n == "secret-tool" else f"/usr/bin/{n}"):
            self.install(answers=self.gateway_answers(), secrets=[CANARY])
        key_file = self.root / "config/context-guru-pi/anthropic-key"
        self.assertEqual(key_file.read_text(), CANARY)
        self.assertEqual(oct(key_file.stat().st_mode & 0o777), "0o600")
        self.assertEqual(oct(key_file.parent.stat().st_mode & 0o777), "0o700")
        self.assertEqual(self.models()["providers"]["anthropic"]["apiKey"], f"!cat {key_file}")

    def test_the_anthropic_api_needs_no_url_and_uses_x_api_key(self):
        self.shell.pi_version = "1.6.0"
        # URL empty; model id (default kept); turn on; no test prompt.
        code, terminal = self.install(answers=["", "", "y", "n"], secrets=[CANARY])
        self.assertEqual(code, 0, self.everything_printed(terminal))
        url, headers, _ = self.http_seen[0]
        self.assertEqual(url, "https://api.anthropic.com/v1/messages")
        self.assertEqual(headers["x-api-key"], CANARY)
        anthropic = self.models()["providers"]["anthropic"]
        self.assertNotIn("baseUrl", anthropic)
        self.assertNotIn("models", anthropic)

    def test_a_rejected_key_is_asked_again_and_three_rejections_change_nothing(self):
        self.shell.pi_version = "1.6.0"
        self.http_codes = [401, 401, 401]
        code, terminal = self.install(answers=self.gateway_answers(), secrets=[CANARY, CANARY, CANARY])
        self.assertEqual(code, 1)
        self.assertEqual(len(self.http_seen), 3)
        self.assertIn("not accepted", self.everything_printed(terminal))
        self.assertIsNone(self.models())
        self.assertFalse(any(cmd[0] == "security" for cmd, _ in self.shell.calls))

    def test_an_existing_connection_is_kept_and_no_key_is_asked(self):
        self.shell.pi_version = "1.6.0"
        self.write_models({"providers": {"anthropic": {"baseUrl": "https://gw.example.com", "apiKey": "!my-own-cmd"}}})
        before = self.models()
        code, terminal = self.install(answers=["", "y", "n"])  # keep (default yes); turn on; no prompt
        self.assertEqual(code, 0, self.everything_printed(terminal))
        self.assertIn("gw.example.com", self.everything_printed(terminal))
        self.assertNotIn("!my-own-cmd", self.everything_printed(terminal))
        self.assertEqual(self.models(), before)
        self.assertEqual(self.http_seen, [])

    def test_existing_settings_keep_their_default_model(self):
        self.shell.pi_version = "1.6.0"
        (self.agent / "settings.json").write_text(json.dumps({"defaultProvider": "openai", "defaultModel": "gpt-x"}))
        self.install(answers=self.gateway_answers(), secrets=[CANARY])
        self.assertEqual(self.settings()["defaultModel"], "gpt-x")

    def test_both_files_are_backed_up_before_they_change(self):
        self.shell.pi_version = "1.6.0"
        self.write_models({"providers": {"openai": {"apiKey": "!x"}}})
        self.install(answers=self.gateway_answers(), secrets=[CANARY])
        backup = self.root / "state/context-guru-pi/installer-backup/models.json"
        self.assertEqual(json.loads(backup.read_text()), {"providers": {"openai": {"apiKey": "!x"}}})


class Setup(Sandbox):
    def answers(self, consent="y"):
        return ["https://gw.example.com", "", "aws/claude-haiku-5-5", consent, "n"]

    def test_the_consent_text_is_shown_and_a_yes_runs_setup_with_chain_behind_a_gateway(self):
        self.shell.pi_version = "1.6.0"
        self.shell.plan = "needs_decision"
        code, terminal = self.install(answers=self.answers(), secrets=[CANARY])
        self.assertEqual(code, 0, self.everything_printed(terminal))
        self.assertIn("CHAIN-CONSENT-TEXT", self.everything_printed(terminal))
        self.assertIn(["python3", PLUGIN, "setup", "--i-consent-to-traffic-interception", "--on-conflict", "chain"],
                      self.shell.ran("python3", PLUGIN))

    def test_no_consent_runs_no_setup_and_says_how_to_turn_it_on(self):
        self.shell.pi_version = "1.6.0"
        code, terminal = self.install(answers=self.answers(consent="n"), secrets=[CANARY])
        self.assertEqual(code, 0)
        self.assertFalse(any("--i-consent-to-traffic-interception" in cmd for cmd in self.shell.ran("python3", PLUGIN)))
        self.assertIn("/context-guru setup", self.everything_printed(terminal))

    def test_a_cold_first_start_is_tried_once_more_after_an_uninstall(self):
        self.shell.pi_version = "1.6.0"
        self.shell.setup_results = ["failed", "routed"]
        code, terminal = self.install(answers=self.answers(), secrets=[CANARY])
        self.assertEqual(code, 0, self.everything_printed(terminal))
        calls = [cmd[2] for cmd in self.shell.ran("python3", PLUGIN)]
        self.assertEqual(calls, ["setup", "setup", "uninstall", "setup", "setup", "status"])

    def test_two_failed_setups_stop_and_say_pi_still_works(self):
        self.shell.pi_version = "1.6.0"
        self.shell.setup_results = ["failed", "failed"]
        code, terminal = self.install(answers=self.answers(), secrets=[CANARY])
        self.assertEqual(code, 1)
        self.assertIn("Pi works without it", self.everything_printed(terminal))

    def test_the_test_prompt_runs_only_after_a_yes(self):
        self.shell.pi_version = "1.6.0"
        answers = self.answers()
        answers[-1] = "y"
        self.install(answers=answers, secrets=[CANARY])
        self.assertTrue(any(cmd[0] == "pi" and "-p" in cmd for cmd, _ in self.shell.calls))


class Uninstall(Sandbox):
    def install_gateway(self, before=None):
        self.shell.pi_version = "1.6.0"
        if before is not None:
            self.write_models(before)
        code, terminal = self.install(answers=["https://gw.example.com", "", "aws/m", "y", "n"], secrets=[CANARY])
        self.assertEqual(code, 0, self.everything_printed(terminal))

    def uninstall(self, answers=()):
        terminal = FakeTerminal(answers)
        code = self.installer(terminal).uninstall()
        return code, terminal

    def test_uninstall_puts_models_json_back_exactly(self):
        before = {"providers": {"openai": {"apiKey": "!mine"}}, "other": 1}
        self.install_gateway(before)
        code, terminal = self.uninstall(answers=["y"])
        self.assertEqual(code, 0, self.everything_printed(terminal))
        self.assertEqual(self.models(), before)

    def test_uninstall_without_an_earlier_file_leaves_no_models_json(self):
        self.install_gateway()
        self.uninstall(answers=["y"])
        self.assertIsNone(self.models())  # it was absent before, so it is absent again
        self.assertNotIn("defaultModel", self.settings())

    def test_a_file_the_user_filled_after_the_install_stays(self):
        self.install_gateway()
        models = self.models()
        models["providers"]["openai"] = {"apiKey": "!added-later"}
        self.write_models(models)
        self.uninstall(answers=["y"])
        self.assertEqual(self.models(), {"providers": {"openai": {"apiKey": "!added-later"}}})

    def test_a_value_the_user_changed_stays(self):
        self.install_gateway()
        models = self.models()
        models["providers"]["anthropic"]["baseUrl"] = "https://my-own.example.com"
        self.write_models(models)
        self.uninstall(answers=["y"])
        self.assertEqual(self.models()["providers"]["anthropic"]["baseUrl"], "https://my-own.example.com")

    def test_uninstall_turns_context_guru_off_removes_the_package_and_the_key(self):
        self.install_gateway()
        stable = str(self.root / "data/context-guru-pi/package")
        self.uninstall(answers=["y"])
        self.assertIn(["python3", PLUGIN, "uninstall"], self.shell.ran("python3", PLUGIN))
        self.assertIn(["pi", "remove", stable], self.shell.ran("pi", "remove"))
        self.assertFalse(Path(stable).exists())
        self.assertIn(["security", "delete-generic-password", "-s", "context-guru-pi-anthropic", "-a", "tester"],
                      self.shell.ran("security", "delete-generic-password"))
        self.assertEqual(self.settings()["packages"], [])

    def test_uninstall_deletes_the_local_data_after_a_yes_and_keeps_it_after_a_no(self):
        self.install_gateway()
        state = self.root / "state/context-guru-pi"
        (state / "dashboard.db").write_text("x")
        self.uninstall(answers=["n"])
        self.assertTrue((state / "dashboard.db").exists())
        self.assertFalse((state / "installer.json").exists())
        self.install_gateway()
        self.uninstall(answers=["y"])
        self.assertFalse(state.exists())

    def test_pi_is_offered_for_removal_only_when_the_installer_installed_it(self):
        self.install_gateway()
        _, terminal = self.uninstall(answers=["y"])
        self.assertNotIn("Uninstall Pi", self.everything_printed(terminal))
        self.shell.pi_version = None
        self.install(answers=["y", "https://gw.example.com", "", "aws/m", "y", "n"], secrets=[CANARY])
        _, terminal = self.uninstall(answers=["y", "y"])
        self.assertIn("Uninstall Pi too?", self.everything_printed(terminal))
        self.assertIn(["npm", "uninstall", "-g", "@earendil-works/pi-coding-agent"], self.shell.ran("npm", "uninstall"))

    def test_linux_file_key_is_deleted(self):
        self.platform = "Linux"
        with mock.patch.object(self.shell, "which", side_effect=lambda n: None if n == "secret-tool" else f"/usr/bin/{n}"):
            self.install_gateway()
            self.uninstall(answers=["y"])
        self.assertFalse((self.root / "config/context-guru-pi/anthropic-key").exists())

    def test_uninstall_with_no_record_still_turns_context_guru_off(self):
        clone = self.root / "clone/pi-package"
        clone.mkdir(parents=True)
        (clone / "package.json").write_text(json.dumps({"name": "@rossoctl/context-guru-pi"}))
        self.settings_packages(add=str(clone))
        code, _ = self.uninstall(answers=["y"])
        self.assertEqual(code, 0)
        self.assertIn(["pi", "remove", str(clone)], self.shell.ran("pi", "remove"))
        self.assertEqual(self.shell.ran("security", "delete-generic-password"), [])


class ReviewRound1(Sandbox):
    """Findings of the first review of #1294."""

    OLD = {"baseUrl": "https://old-gw.example.com", "authHeader": True, "apiKey": "!old-cmd",
           "headers": {"x-team": "a"}, "models": [{"id": "old-model"}]}

    def test_a_connection_not_kept_is_replaced_and_uninstall_puts_it_back(self):
        self.shell.pi_version = "1.6.0"
        before = {"providers": {"anthropic": dict(self.OLD)}}
        self.write_models(before)
        # Do not keep; the Anthropic API (no URL); default model; consent; no test prompt.
        code, terminal = self.install(answers=["n", "", "", "y", "n"], secrets=[CANARY])
        self.assertEqual(code, 0, self.everything_printed(terminal))
        anthropic = self.models()["providers"]["anthropic"]
        for stale in ("baseUrl", "authHeader", "headers", "models"):
            self.assertNotIn(stale, anthropic)  # the new key must not go to the old gateway
        self.assertTrue(anthropic["apiKey"].startswith("!security "))
        done = self.installer(FakeTerminal(["y"])).uninstall()
        self.assertEqual(done, 0)
        self.assertEqual(self.models(), before)

    def test_the_retry_keeps_the_strategy_and_the_preset_the_consent_named(self):
        self.shell.pi_version = "1.6.0"
        self.shell.plan_preset, self.shell.plan_strategy = "medium", "none"
        self.shell.setup_results = ["failed", "routed"]
        code, terminal = self.install(answers=["https://gw.example.com", "", "m", "y", "n"], secrets=[CANARY])
        self.assertEqual(code, 0, self.everything_printed(terminal))
        setups = [cmd for cmd in self.shell.ran("python3", PLUGIN) if cmd[2] == "setup" and "--plan" not in cmd]
        self.assertEqual(len(setups), 2)
        self.assertEqual(setups[1][setups[1].index("--cache-strategy") + 1], "none")
        self.assertIn(["python3", PLUGIN, "configure", "--preset", "medium"], self.shell.ran("python3", PLUGIN))

    def test_a_refused_plan_stops_with_its_reason_and_asks_no_consent(self):
        self.shell.pi_version = "1.6.0"
        self.shell.plan = "refused"
        code, terminal = self.install(answers=["https://gw.example.com", "", "m"], secrets=[CANARY])
        self.assertEqual(code, 1)
        self.assertIn("binary_named_missing", self.everything_printed(terminal))
        self.assertNotIn("Turn context-guru on?", self.everything_printed(terminal))

    def test_a_broken_models_json_stops_with_the_file_name(self):
        self.shell.pi_version = "1.6.0"
        (self.agent / "models.json").write_text("{not json")
        code, terminal = self.install()
        self.assertEqual(code, 1)
        self.assertIn("models.json is not valid JSON", self.everything_printed(terminal))

    def test_uninstall_keeps_the_local_data_when_the_route_was_not_put_back(self):
        self.shell.pi_version = "1.6.0"
        self.install(answers=["https://gw.example.com", "", "m", "y", "n"], secrets=[CANARY])
        self.shell.restore = "models_unreadable"
        terminal = FakeTerminal(["y"])
        code = self.installer(terminal).uninstall()
        self.assertEqual(code, 1)
        self.assertTrue((self.root / "state/context-guru-pi").exists())
        self.assertIn("context-guru-reset", self.everything_printed(terminal))

    def test_a_routed_connection_names_the_real_upstream_not_the_proxy(self):
        self.shell.pi_version = "1.6.0"
        self.write_models({"providers": {"anthropic": {"baseUrl": "http://127.0.0.1:8787/anthropic", "apiKey": "!x"}}})
        state = self.root / "state/context-guru-pi"
        state.mkdir(parents=True)
        (state / "install.json").write_text(json.dumps({"upstreams": {"anthropic": "https://gw.example.com"}}))
        _, terminal = self.install(answers=["", "y", "n"])
        self.assertIn("Pi already connects to Anthropic at gw.example.com", self.everything_printed(terminal))

    def test_the_keychain_item_is_read_back_and_a_missing_one_changes_nothing(self):
        self.shell.pi_version = "1.6.0"
        self.shell.keychain_has_item = False
        code, terminal = self.install(answers=["https://gw.example.com", "", "m", "y", "n"], secrets=[CANARY])
        self.assertEqual(code, 1)
        self.assertIsNone(self.models())

    def test_a_user_name_with_a_space_is_quoted_in_the_key_command(self):
        self.shell.pi_version = "1.6.0"
        with mock.patch.dict(os.environ, {"USER": "first last"}):
            self.install(answers=["https://gw.example.com", "", "m", "y", "n"], secrets=[CANARY])
        self.assertEqual(self.models()["providers"]["anthropic"]["apiKey"],
                         "!security find-generic-password -s context-guru-pi-anthropic -a 'first last' -w")

    def test_a_second_install_keeps_the_note_that_the_first_one_created_models_json(self):
        self.shell.pi_version = "1.6.0"
        self.install(answers=["https://gw.example.com", "", "m", "y", "n"], secrets=[CANARY])
        self.install(answers=["n", "https://gw.example.com", "", "m", "y", "n"], secrets=[CANARY])
        self.installer(FakeTerminal(["y"])).uninstall()
        self.assertIsNone(self.models())


class Steps(Sandbox):
    """Each phase shows "Step N of M · <name>" and one line on what it does; a skipped step says why."""

    def steps(self, terminal):
        return [line.split("\n")[0] for line in terminal.shown if line.startswith("Step ")]

    def test_install_shows_nine_numbered_steps_in_order_after_the_download(self):
        self.shell.pi_version = "1.6.0"
        terminal = FakeTerminal(["https://gw.example.com", "", "m", "y", "n"], [CANARY])
        installer = self.installer(terminal)
        installer.downloaded = True
        self.assertEqual(installer.install(), 0)
        self.assertEqual(self.steps(terminal), [
            "Step 1 of 9 · Download context-guru for Pi", "Step 2 of 9 · Check this computer", "Step 3 of 9 · Pi",
            "Step 4 of 9 · The context-guru package for Pi", "Step 5 of 9 · Pi's connection to Anthropic",
            "Step 6 of 9 · Check the key", "Step 7 of 9 · Save the key and the connection",
            "Step 8 of 9 · Turn context-guru on", "Step 9 of 9 · Check that it works"])
        shown = self.everything_printed(terminal)
        self.assertIn("It adds the /context-guru command to Pi", shown)
        self.assertIn("1 token", shown)

    def test_without_the_bootstrap_there_are_eight_steps(self):
        self.shell.pi_version = "1.6.0"
        _, terminal = self.install(answers=["https://gw.example.com", "", "m", "y", "n"], secrets=[CANARY])
        self.assertEqual(self.steps(terminal)[0], "Step 1 of 8 · Check this computer")
        self.assertEqual(self.steps(terminal)[-1], "Step 8 of 8 · Check that it works")

    def test_a_kept_connection_skips_the_key_steps_and_says_so(self):
        self.shell.pi_version = "1.6.0"
        self.write_models({"providers": {"anthropic": {"baseUrl": "https://gw.example.com", "apiKey": "!mine"}}})
        _, terminal = self.install(answers=["", "y", "n"])
        shown = self.everything_printed(terminal)
        self.assertIn("Step 5 of 8 · Check the key — skipped: you kept your connection.", shown)
        self.assertIn("Step 6 of 8 · Save the key and the connection — skipped: you kept your connection.", shown)

    def test_uninstall_shows_its_numbered_steps(self):
        self.shell.pi_version = "1.6.0"
        self.install(answers=["https://gw.example.com", "", "m", "y", "n"], secrets=[CANARY])
        terminal = FakeTerminal(["y"])
        self.installer(terminal).uninstall()
        self.assertEqual(self.steps(terminal)[:2], ["Step 1 of 6 · Turn context-guru off",
                                                     "Step 2 of 6 · Remove the context-guru package"])
        self.assertEqual(self.steps(terminal)[-1].split(" — ")[0], "Step 6 of 6 · Pi itself")


class ReviewRound2(Sandbox):
    """A second run while context-guru routes Pi, with "n" to "Keep this connection?" (review 2 of #1294)."""

    def setUp(self):
        super().setUp()
        self.shell.pi_version = "1.6.0"
        self.shell.realistic = True

    def first_install(self, gateway):
        answers = [gateway, "", "m", "y", "n"] if gateway else ["", "", "y", "n"]
        code, terminal = self.install(answers=answers, secrets=[CANARY])
        self.assertEqual(code, 0, self.everything_printed(terminal))
        self.assertEqual(self.models()["providers"]["anthropic"]["baseUrl"], PROXY)

    def test_a_new_gateway_on_a_routed_pi_turns_context_guru_off_first(self):
        self.first_install("https://old-gw.example.com")
        code, terminal = self.install(answers=["n", "https://new-gw.example.com", "", "m", "y", "n"], secrets=[CANARY])
        self.assertEqual(code, 0, self.everything_printed(terminal))
        self.assertEqual(self.shell.routing["upstream"], "https://new-gw.example.com")
        self.installer(FakeTerminal(["y"])).uninstall()
        self.assertNotIn("127.0.0.1", json.dumps(self.models()))  # never a dead proxy URL after uninstall

    def test_the_anthropic_api_on_a_routed_pi_does_not_keep_the_old_gateway(self):
        self.first_install("https://old-gw.example.com")
        code, terminal = self.install(answers=["n", "", "", "y", "n"], secrets=[CANARY])
        self.assertEqual(code, 0, self.everything_printed(terminal))
        self.assertEqual(self.shell.routing["upstream"], "https://api.anthropic.com")
        self.assertEqual(self.http_seen[-1][0], "https://api.anthropic.com/v1/messages")
        self.installer(FakeTerminal(["y"])).uninstall()
        self.assertNotIn("127.0.0.1", json.dumps(self.models() or {}))

    def test_a_second_install_from_the_api_to_a_gateway_leaves_no_proxy_url(self):
        self.first_install("")
        self.install(answers=["n", "https://gw.example.com", "", "m", "y", "n"], secrets=[CANARY])
        self.installer(FakeTerminal(["y"])).uninstall()
        self.assertNotIn("127.0.0.1", json.dumps(self.models() or {}))

    def test_the_retry_plans_again_after_its_uninstall(self):
        self.shell.realistic = False
        self.shell.setup_results = ["failed", "routed"]
        self.install(answers=["https://gw.example.com", "", "m", "y", "n"], secrets=[CANARY])
        calls = [cmd[2:] for cmd in self.shell.ran("python3", PLUGIN)]
        self.assertEqual([c[0] + (" --plan" if "--plan" in c else "") for c in calls],
                         ["setup --plan", "setup", "uninstall", "setup --plan", "setup", "status"])
        self.assertNotIn("--cache-strategy", calls[1])  # the first setup reads the strategy itself
        self.assertIn("--cache-strategy", calls[4])     # the retry passes the one the consent named


class Folders(Sandbox):
    """#1319: the installer names the folders it uses, and asks when PI_CODING_AGENT_DIR moves Pi's settings."""

    def run_install(self, answers, secrets=()):
        terminal = FakeTerminal(answers, secrets)
        return self.installer(terminal, confirm_folders=True).install(), terminal

    def test_a_moved_pi_folder_is_named_and_no_stops_before_any_change(self):
        code, terminal = self.run_install(["n"])
        shown = self.everything_printed(terminal)
        self.assertEqual(code, 1)
        self.assertIn(f"PI_CODING_AGENT_DIR is set: Pi's settings are in {self.agent}", shown)
        self.assertIn("Use these folders? [y/N]", shown)
        self.assertIn(f"XDG_STATE_HOME is set too: {self.root / 'state'}", shown)
        self.assertIn("open a new terminal", shown)
        self.assertNotIn("unset", shown)  # unset does not fix a PATH that a test changed
        self.assertEqual(self.shell.ran("npm", "view"), [])  # nothing ran after the check
        self.assertEqual(self.shell.ran("pi", "install"), [])

    def test_a_yes_goes_on_with_those_folders(self):
        self.shell.pi_version = "1.6.0"
        code, terminal = self.run_install(["y", "https://gw.example.com", "", "m", "y", "n"], [CANARY])
        self.assertEqual(code, 0, self.everything_printed(terminal))
        self.assertTrue((self.agent / "models.json").exists())

    def test_without_the_variable_there_is_no_question_and_the_folders_are_named(self):
        self.shell.pi_version = "1.6.0"
        home_agent = self.root / ".pi/agent"
        home_agent.mkdir(parents=True)
        with mock.patch.dict(os.environ):
            del os.environ["PI_CODING_AGENT_DIR"]
            code, terminal = self.run_install(["https://gw.example.com", "", "m", "y", "n"], [CANARY])
        shown = self.everything_printed(terminal)
        self.assertEqual(code, 0, shown)
        self.assertNotIn("Use these folders?", shown)
        self.assertIn(f"Pi's settings: {home_agent}", shown)
        self.assertIn(f"context-guru's data: {self.root / 'state/context-guru-pi'}", shown)

    def test_the_end_names_the_pi_program_and_what_to_do_if_the_terminal_finds_another(self):
        self.shell.pi_version = "1.6.0"
        _, terminal = self.run_install(["y", "https://gw.example.com", "", "m", "y", "n"], [CANARY])
        shown = self.everything_printed(terminal)
        self.assertIn("Start Pi with: pi (/usr/local/bin/pi)", shown)
        self.assertIn("open a new terminal", shown)

    def test_the_default_folder_named_by_the_variable_asks_nothing(self):
        self.shell.pi_version = "1.6.0"
        default = self.root / ".pi/agent"
        default.mkdir(parents=True)
        with mock.patch.dict(os.environ, {"PI_CODING_AGENT_DIR": str(default)}):
            code, terminal = self.run_install(["https://gw.example.com", "", "m", "y", "n"], [CANARY])
        self.assertEqual(code, 0, self.everything_printed(terminal))
        self.assertNotIn("Use these folders?", self.everything_printed(terminal))

    def test_the_last_line_has_no_path_when_pi_is_not_on_path(self):
        self.shell.pi_version = "1.6.0"
        real_which = self.shell.which
        with mock.patch.object(self.shell, "which", side_effect=lambda n: None if n == "pi" and self.shell.calls and
                               any(c[0][:2] == ["python3", PLUGIN] and c[0][2] == "status" for c in self.shell.calls)
                               else real_which(n)):
            _, terminal = self.run_install(["y", "https://gw.example.com", "", "m", "y", "n"], [CANARY])
        self.assertIn("Done. Start Pi with: pi.", self.everything_printed(terminal))
        self.assertNotIn("pi (pi)", self.everything_printed(terminal))

    def test_main_asks_about_the_folders(self):
        with mock.patch.object(install_pi, "Installer") as made:
            made.return_value.install.return_value = 0
            install_pi.main(["install", "--source", str(self.source)])
        self.assertTrue(made.call_args.kwargs["confirm_folders"])

    def test_uninstall_asks_the_same_question_so_a_test_folder_install_can_be_removed(self):
        self.shell.pi_version = "1.6.0"
        self.run_install(["y", "https://gw.example.com", "", "m", "y", "n"], [CANARY])
        terminal = FakeTerminal(["n"])
        self.assertEqual(self.installer(terminal, confirm_folders=True).uninstall(), 1)
        self.assertEqual(self.shell.ran("python3", PLUGIN, "uninstall"), [])
        terminal = FakeTerminal(["y", "y"])
        self.assertEqual(self.installer(terminal, confirm_folders=True).uninstall(), 0)
        self.assertIn(["python3", PLUGIN, "uninstall"], self.shell.ran("python3", PLUGIN))


class TheKeyStaysSecret(Sandbox):
    """The canary key is in the secret store's stdin and in the check request's header, nowhere else."""

    def test_the_key_is_in_no_argv_no_output_no_file(self):
        self.shell.pi_version = "1.6.0"
        _, terminal = self.install(answers=["https://gw.example.com", "", "m", "y", "y"], secrets=[CANARY])
        for argv, stdin in self.shell.argv_and_stdin():
            self.assertNotIn(CANARY, argv)
            if not argv.startswith("security"):
                self.assertNotIn(CANARY, stdin)
        self.assertNotIn(CANARY, self.everything_printed(terminal))
        for path in self.root.rglob("*"):
            if path.is_file():
                self.assertNotIn(CANARY, path.read_text(errors="replace"), str(path))


if __name__ == "__main__":
    unittest.main()
