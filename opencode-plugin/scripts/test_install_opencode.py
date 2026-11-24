"""Tests for install_opencode.py. The terminal and the programs it runs (npm, opencode, the plugin's scripts) are
faked at their boundary; the files (opencode.json, tui.json, the plugin copy, the record) are real, in a
temporary HOME."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

import install_opencode  # noqa: E402

SOURCE = HERE.parent  # the real opencode-plugin folder: index.js and tui.js carry the marker


class FakeTerminal:
    """Answers in order. Each question and each line said is kept, to check what the user saw."""

    def __init__(self, answers=()):
        self.answers, self.shown = list(answers), []

    def say(self, text):
        self.shown.append(text)

    def ask(self, question, default=""):
        self.shown.append(question)
        if not self.answers:
            raise AssertionError(f"unexpected question: {question}")
        return self.answers.pop(0) or default

    def text(self):
        return "\n".join(self.shown)


class FakeShell:
    """Answers per program, and keeps every argv."""

    def __init__(self):
        self.calls = []
        self.opencode, self.latest = "1.19.6", "1.19.6"
        self.npm_view_error = None
        self.has_npm = True
        self.npm_install_error = None
        self.proxy = "up"
        self.run_fails = False  # "exit": exit 1 with an error; "error-exit-0": the error with exit 0
        self.npm_shadowed = False  # an older opencode comes first on PATH, so npm's new one does not run
        self.opencode_version_text = None

    def which(self, name):
        if name == "opencode":
            return "/usr/local/bin/opencode" if self.opencode else None
        if name == "npm":
            return "/usr/local/bin/npm" if self.has_npm else None
        return f"/usr/bin/{name}"

    def run(self, cmd, cwd=None):
        self.calls.append(list(cmd))
        def ok(out=""):
            return subprocess.CompletedProcess(cmd, 0, out, "")

        if cmd[:2] == ["opencode", "--version"]:
            return ok((self.opencode_version_text or self.opencode) + "\n")
        if cmd[:2] == ["npm", "view"]:
            if self.npm_view_error:
                return subprocess.CompletedProcess(cmd, 1, "", self.npm_view_error)
            return ok(self.latest + "\n")
        if cmd[:2] == ["npm", "install"]:
            if self.npm_install_error:
                return subprocess.CompletedProcess(cmd, 1, "", self.npm_install_error)
            if not self.npm_shadowed:
                self.opencode = cmd[-1].rsplit("@", 1)[1]
            return ok()
        if cmd[:2] == ["npm", "uninstall"]:
            self.opencode = None
            return ok()
        if cmd[:2] == ["opencode", "run"]:
            # The plugin starts in the folder OpenCode runs in, and writes its scripts there.
            state = install_opencode.state_root() / hashlib.sha256(cwd.encode()).hexdigest()[:16]
            state.mkdir(parents=True, exist_ok=True)
            (state / "context-guru-status").write_text("#!/bin/sh\n")
            (state / "context-guru-reset").write_text("#!/bin/sh\n")
            if self.run_fails == "exit":
                return subprocess.CompletedProcess(cmd, 1, "", "Error: Anthropic API key is missing.\n")
            if self.run_fails == "error-exit-0":
                return ok("Error: Anthropic API key is missing.\n")
            return ok("ok\n")
        if cmd[0] == "sh" and cmd[1].endswith("context-guru-status"):
            return ok(f"install=present\nport=8787\nproxy={self.proxy}\n")
        if cmd[0] == "sh" and cmd[1].endswith("context-guru-reset"):
            return ok("context-guru: proxy stopped\n")
        raise AssertionError(f"unexpected command: {cmd}")


class InstallerTest(unittest.TestCase):
    def setUp(self):
        self.home = tempfile.TemporaryDirectory()
        root = Path(self.home.name)
        env = {"HOME": str(root), "XDG_CONFIG_HOME": str(root / "config"), "XDG_DATA_HOME": str(root / "data"),
               "XDG_STATE_HOME": str(root / "state")}
        patcher = mock.patch.dict(os.environ, env)
        patcher.start()
        self.addCleanup(patcher.stop)
        os.environ.pop("CONTEXT_GURU_STATE", None)
        os.environ.pop("CG_OC_OPENCODE_VERSION", None)
        self.addCleanup(self.home.cleanup)
        self.shell = FakeShell()
        self.config = root / "config/opencode"
        self.plugin_copy = root / "data/context-guru-opencode/plugin"

    def installer(self, *answers):
        self.terminal = FakeTerminal(answers)
        return install_opencode.Installer(self.terminal, self.shell, SOURCE)

    def read(self, name):
        path = self.config / name
        return json.loads(path.read_text()) if path.exists() else None

    def write(self, name, value):
        self.config.mkdir(parents=True, exist_ok=True)
        (self.config / name).write_text(json.dumps(value, indent=2) + "\n")

    def npm_calls(self):
        return [call for call in self.shell.calls if call[0] == "npm"]

    # ------------------------------------------------------------------------------- install
    def test_install_adds_the_stable_copy_to_both_config_files(self):
        code = self.installer("y", "n").install()

        self.assertEqual(code, 0)
        self.assertEqual(self.read("opencode.json"), {"$schema": "https://opencode.ai/config.json",
                                                      "plugin": [str(self.plugin_copy / "index.js")]})
        self.assertEqual(self.read("tui.json"), {"$schema": "https://opencode.ai/tui.json",
                                                 "plugin": [str(self.plugin_copy / "tui.js")]})
        self.assertTrue((self.plugin_copy / "index.js").is_file())
        self.assertFalse((self.plugin_copy / "test").exists())

    def test_install_keeps_other_plugins_and_keys(self):
        self.write("opencode.json", {"model": "anthropic/claude-sonnet-5-5", "plugin": ["opencode-foo"]})

        self.installer("y", "n").install()

        self.assertEqual(self.read("opencode.json"), {"model": "anthropic/claude-sonnet-5-5",
                                                      "plugin": ["opencode-foo", str(self.plugin_copy / "index.js")]})

    def test_install_replaces_a_cloned_copy_and_keeps_its_options(self):
        clone = Path(self.home.name) / "src/context-guru/opencode-plugin"
        clone.mkdir(parents=True)
        (clone / "index.js").write_text('console.log("opencode-context-guru: x")\n')
        (clone / "tui.js").write_text('// opencode-context-guru\n')
        self.write("opencode.json", {"plugin": [[str(clone / "index.js"), {"preset": "medium"}]]})
        self.write("tui.json", {"plugin": [str(clone / "tui.js")]})

        self.installer("y", "n").install()

        self.assertEqual(self.read("opencode.json")["plugin"], [[str(self.plugin_copy / "index.js"),
                                                                 {"preset": "medium"}]])
        self.assertEqual(self.read("tui.json")["plugin"], [str(self.plugin_copy / "tui.js")])

    def test_second_install_adds_no_second_entry(self):
        self.installer("y", "n").install()
        self.installer("y", "n").install()

        self.assertEqual(self.read("opencode.json")["plugin"], [str(self.plugin_copy / "index.js")])
        self.assertEqual(self.read("tui.json")["plugin"], [str(self.plugin_copy / "tui.js")])

    def test_no_consent_changes_nothing(self):
        code = self.installer("n").install()

        self.assertEqual(code, 0)
        self.assertFalse(self.config.exists())
        self.assertFalse(self.plugin_copy.exists())
        self.assertIn("context-guru stays off", self.terminal.text())

    def test_consent_text_names_routing_and_keep_alive_on_the_users_credential(self):
        self.installer("n").install()

        text = self.terminal.text()
        self.assertIn("Anthropic and OpenAI requests", text)
        self.assertIn("your own credential", text)

    def test_jsonc_config_stops_with_the_entries_to_add_by_hand(self):
        self.config.mkdir(parents=True)
        (self.config / "opencode.json").write_text('{\n  // my comment\n  "plugin": []\n}\n')

        code = self.installer().install()

        self.assertEqual(code, 1)
        self.assertEqual((self.config / "opencode.json").read_text(), '{\n  // my comment\n  "plugin": []\n}\n')
        self.assertIn("is not plain JSON", self.terminal.text())
        self.assertFalse(self.plugin_copy.exists())

    def test_an_opencode_jsonc_file_stops_the_install(self):
        self.config.mkdir(parents=True)
        (self.config / "opencode.jsonc").write_text("{}\n")

        code = self.installer().install()

        self.assertEqual(code, 1)
        self.assertIn("opencode.jsonc", self.terminal.text())
        self.assertFalse((self.config / "opencode.json").exists())

    def test_backs_up_an_existing_config_before_writing(self):
        self.write("opencode.json", {"plugin": ["opencode-foo"]})

        self.installer("y", "n").install()

        backup = install_opencode.state_root() / "installer-backup/opencode.json"
        self.assertEqual(json.loads(backup.read_text()), {"plugin": ["opencode-foo"]})

    # ------------------------------------------------------------------------------ OpenCode
    def npm_installs(self):
        return [call for call in self.npm_calls() if call[1] == "install"]

    def test_installs_the_latest_opencode_when_missing_after_a_yes(self):
        self.shell.opencode = None

        code = self.installer("y", "y", "n").install()

        self.assertEqual(code, 0)
        self.assertEqual(self.npm_calls(), [
            ["npm", "view", "opencode-ai", "version", "--registry=https://registry.npmjs.org/"],
            ["npm", "install", "-g", "--registry=https://registry.npmjs.org/", "opencode-ai@1.19.6"]])
        self.assertTrue(json.loads((install_opencode.state_root() / "installer.json").read_text())
                        ["opencode_installed"])

    def test_no_to_installing_opencode_stops_and_changes_nothing(self):
        self.shell.opencode = None

        code = self.installer("n").install()

        self.assertEqual(code, 1)
        self.assertEqual(self.npm_installs(), [])
        self.assertFalse(self.config.exists())

    def test_missing_opencode_and_no_npm_names_the_install_page(self):
        self.shell.opencode, self.shell.has_npm = None, False

        code = self.installer().install()

        self.assertEqual(code, 1)
        self.assertIn("https://opencode.ai/docs", self.terminal.text())

    def test_an_installed_opencode_without_npm_stays(self):
        self.shell.opencode, self.shell.has_npm = "1.19.5", False

        code = self.installer("y", "n").install()

        self.assertEqual(code, 0)
        self.assertEqual(self.npm_calls(), [])
        self.assertIn("OpenCode 1.19.5 is installed", self.terminal.text())

    def test_npm_that_cannot_read_the_latest_version_names_the_registry_setting(self):
        self.shell.npm_view_error = "E401 Unable to authenticate"

        code = self.installer().install()

        self.assertEqual(code, 1)
        self.assertIn("E401 Unable to authenticate", self.terminal.text())
        self.assertIn("CG_OC_NPM_REGISTRY", self.terminal.text())
        self.assertFalse(self.config.exists())

    def test_an_npm_failure_shows_npms_message_and_never_uses_sudo(self):
        self.shell.opencode, self.shell.npm_install_error = None, "EACCES: permission denied"

        code = self.installer("y").install()

        self.assertEqual(code, 1)
        self.assertIn("EACCES: permission denied", self.terminal.text())
        self.assertNotIn("sudo", [arg for call in self.shell.calls for arg in call])
        self.assertFalse(self.config.exists())

    def test_an_older_opencode_is_updated_to_the_latest_after_a_yes(self):
        self.shell.opencode = "1.19.5"

        self.installer("y", "y", "n").install()

        self.assertEqual(self.npm_installs()[0][-1], "opencode-ai@1.19.6")
        record = json.loads((install_opencode.state_root() / "installer.json").read_text())
        self.assertNotIn("opencode_installed", record)  # it was there before: uninstall must not offer to remove it

    def test_no_to_the_update_keeps_a_supported_opencode(self):
        self.shell.opencode = "1.19.5"

        code = self.installer("n", "y", "n").install()

        self.assertEqual(code, 0)
        self.assertEqual(self.npm_installs(), [])
        self.assertIn("OpenCode 1.19.5 stays", self.terminal.text())

    def test_no_to_the_update_of_a_too_old_opencode_stops_the_install(self):
        self.shell.opencode = "1.17.0"

        code = self.installer("n").install()

        self.assertEqual(code, 1)
        self.assertIn("1.18.29", self.terminal.text())
        self.assertFalse(self.config.exists())

    def test_the_latest_opencode_stays_and_names_the_tested_version(self):
        code = self.installer("y", "n").install()

        self.assertEqual(code, 0)
        self.assertEqual(self.npm_installs(), [])
        self.assertIn("OpenCode 1.19.6 is installed", self.terminal.text())
        self.assertIn("tested on OpenCode 1.19.5", self.terminal.text())

    def test_a_newer_pre_release_is_never_downgraded(self):
        self.shell.opencode = "1.20.0-beta.1"

        code = self.installer("y", "n").install()  # no OpenCode question

        self.assertEqual(code, 0)
        self.assertEqual(self.npm_installs(), [])
        self.assertIn("OpenCode 1.20.0 is installed", self.terminal.text())

    def test_an_opencode_whose_version_cannot_be_read_stays_after_a_yes(self):
        self.shell.opencode_version_text = "opencode dev build"

        code = self.installer("y", "y", "n").install()  # go on with it, consent, no test prompt

        self.assertEqual(code, 0)
        self.assertEqual(self.npm_installs(), [])
        self.assertIn("cannot read the version", self.terminal.text())
        self.assertNotIn("opencode_installed", json.loads(install_opencode.record_path().read_text()))

    def test_an_opencode_whose_version_cannot_be_read_and_a_no_stops(self):
        self.shell.opencode_version_text = "opencode dev build"

        code = self.installer("n").install()

        self.assertEqual(code, 1)
        self.assertFalse(self.config.exists())

    # -------------------------------------------------------------------------- test prompt
    def test_test_prompt_reports_the_proxy_up(self):
        code = self.installer("y", "y").install()

        self.assertEqual(code, 0)
        self.assertIn("✓ The proxy started, and OpenCode answered", self.terminal.text())

    def test_test_prompt_with_the_proxy_down_names_the_login_command(self):
        self.shell.proxy = "down"

        self.installer("y", "y").install()

        self.assertIn("opencode auth login", self.terminal.text())

    def test_test_prompt_that_fails_is_not_reported_as_working(self):
        for failure in ("exit", "error-exit-0"):
            with self.subTest(failure=failure):
                self.shell.run_fails = failure

                self.installer("y", "y").install()

                text = self.terminal.text()
                self.assertNotIn("✓ The proxy started, and OpenCode answered", text)
                self.assertIn("The proxy started, but OpenCode could not answer", text)
                self.assertIn("Anthropic API key is missing", text)
                self.assertIn("opencode auth login", text)

    def test_test_prompt_stops_its_own_proxy_and_removes_its_state(self):
        self.installer("y", "y").install()

        resets = [call for call in self.shell.calls if call[0] == "sh" and call[1].endswith("context-guru-reset")]
        self.assertEqual(len(resets), 1)
        project = Path(resets[0][1]).parent
        self.assertEqual(project.parent, install_opencode.state_root())
        self.assertFalse(project.exists())

    def test_an_opencode_first_on_path_that_is_still_old_stops_before_consent(self):
        self.shell.opencode, self.shell.npm_shadowed = "1.17.0", True

        code = self.installer("y").install()  # one question only: the update

        self.assertEqual(code, 1)
        self.assertIn("still runs OpenCode 1.17.0", self.terminal.text())
        self.assertIn("/usr/local/bin/opencode", self.terminal.text())
        self.assertFalse(self.config.exists())

    def test_carried_options_with_a_preset_change_the_consent_text(self):
        clone = Path(self.home.name) / "src/opencode-plugin"
        clone.mkdir(parents=True)
        (clone / "index.js").write_text("// opencode-context-guru\n")
        self.write("opencode.json", {"plugin": [[str(clone / "index.js"), {"preset": "medium"}]]})

        self.installer("n").install()

        self.assertIn("The preset is medium", self.terminal.text())
        self.assertNotIn("The preset is off", self.terminal.text())

    def test_install_keeps_the_config_file_mode(self):
        self.write("opencode.json", {"plugin": []})
        (self.config / "opencode.json").chmod(0o644)

        self.installer("y", "n").install()

        self.assertEqual((self.config / "opencode.json").stat().st_mode & 0o777, 0o644)

    # ----------------------------------------------------------------------------- uninstall
    def test_uninstall_removes_only_what_install_added(self):
        self.write("opencode.json", {"model": "x", "plugin": ["opencode-foo"]})
        self.installer("y", "n").install()

        code = self.installer("y").uninstall()

        self.assertEqual(code, 0)
        self.assertEqual(self.read("opencode.json"), {"model": "x", "plugin": ["opencode-foo"]})
        self.assertFalse((self.config / "tui.json").exists())  # the installer created it
        self.assertFalse(self.plugin_copy.parent.exists())  # no empty folder is left
        self.assertFalse(install_opencode.state_root().exists())

    def test_uninstall_stops_each_projects_proxy(self):
        self.installer("y", "n").install()
        project = install_opencode.state_root() / "0123456789abcdef"
        project.mkdir(parents=True)
        (project / "context-guru-reset").write_text("#!/bin/sh\n")

        self.installer("y").uninstall()

        self.assertIn(["sh", str(project / "context-guru-reset")], self.shell.calls)

    def test_uninstall_keeping_local_data_deletes_only_the_installers_files(self):
        self.installer("y", "n").install()
        log = install_opencode.state_root() / "0123456789abcdef/proxy.log"
        log.parent.mkdir(parents=True)
        log.write_text("log\n")

        self.installer("n").uninstall()

        self.assertTrue(log.exists())
        self.assertFalse((install_opencode.state_root() / "installer.json").exists())
        self.assertFalse((install_opencode.state_root() / "installer-backup").exists())

    def test_uninstall_offers_to_remove_opencode_only_when_it_installed_it(self):
        self.shell.opencode = None
        self.installer("y", "y", "n").install()

        self.installer("y", "y").uninstall()

        self.assertEqual(self.npm_calls()[-1], ["npm", "uninstall", "-g", "opencode-ai"])

    def test_uninstall_leaves_an_opencode_it_did_not_install(self):
        self.installer("y", "n").install()

        self.installer("y").uninstall()  # one question only: the local data

        self.assertEqual([call for call in self.npm_calls() if call[1] == "uninstall"], [])

    def test_uninstall_with_a_jsonc_file_still_stops_proxies_and_removes_the_copy(self):
        self.installer("y", "n").install()
        project = install_opencode.state_root() / "0123456789abcdef"
        project.mkdir(parents=True)
        (project / "context-guru-reset").write_text("#!/bin/sh\n")
        jsonc = '{\n  // mine\n  "plugin": ["%s"]\n}\n' % (self.plugin_copy / "index.js")
        (self.config / "opencode.json").write_text(jsonc)

        code = self.installer("y").uninstall()

        self.assertEqual(code, 1)  # not finished: one entry is for the user to remove
        self.assertIn(["sh", str(project / "context-guru-reset")], self.shell.calls)
        self.assertEqual((self.config / "opencode.json").read_text(), jsonc)
        self.assertFalse(self.read("tui.json"))  # the plain file is done
        self.assertFalse(self.plugin_copy.exists())
        text = self.terminal.text()
        self.assertIn(f"remove this entry by hand: \"{self.plugin_copy / 'index.js'}\"", text)
        self.assertNotIn("To install it by hand", text)

    def test_uninstall_removes_the_stable_entry_when_the_copy_is_already_gone(self):
        self.write("opencode.json", {"plugin": ["opencode-foo"]})
        self.installer("y", "n").install()
        shutil.rmtree(self.plugin_copy)

        self.installer("y").uninstall()

        self.assertEqual(self.read("opencode.json"), {"plugin": ["opencode-foo"]})

    def test_uninstall_removes_a_cloned_copy_entry_too(self):
        clone = Path(self.home.name) / "src/opencode-plugin"
        clone.mkdir(parents=True)
        (clone / "index.js").write_text("// opencode-context-guru\n")
        self.write("opencode.json", {"plugin": ["opencode-foo", str(clone / "index.js")]})

        self.installer("y").uninstall()

        self.assertEqual(self.read("opencode.json"), {"plugin": ["opencode-foo"]})


if __name__ == "__main__":
    unittest.main()
