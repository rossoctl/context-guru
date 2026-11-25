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

    def __init__(self, answers=(), eof=False):
        self.answers, self.shown, self.eof = list(answers), [], eof

    def say(self, text):
        self.shown.append(text)

    def ask(self, question, default=""):
        self.shown.append(question)
        if not self.answers and self.eof:
            raise install_opencode.Stop("no answer (the input ended). Nothing more was changed.")
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
        self.requests = 1  # what the proxy's /stats counted, as the status script prints it
        self.run_fails = False  # "exit": exit 1 with an error; "error-exit-0": the error with exit 0
        self.npm_shadowed = False  # an older opencode comes first on PATH, so npm's new one does not run
        self.opencode_version_text = None
        self.logins = {"anthropic", "anthropic-gateway"}  # provider ids with an OpenCode login
        self.login_adds, self.login_fails = "anthropic-gateway", False  # what the user's login does
        self.interactive_calls = []

    def interactive(self, cmd):
        self.interactive_calls.append(list(cmd))
        if self.login_fails:
            return 1
        self.logins.add(self.login_adds)
        return 0

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
        if cmd[:3] == ["opencode", "auth", "list"]:
            names = {"anthropic": "Anthropic", "openai": "OpenAI"}
            return ok("\x1b[0m┌  Credentials /x/auth.json\n│\n" + "".join(
                f"●  {names.get(login, login)} api\n" for login in sorted(self.logins)) + "└  done\n")
        if cmd[:3] == ["opencode", "auth", "logout"]:
            self.logins.discard(cmd[3])
            return ok("Logout successful\n")
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
            stats = f"stats_requests={self.requests}\n" if self.proxy == "up" else ""
            return ok(f"install=present\nport=8787\nproxy={self.proxy}\n{stats}")
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
        for name in ("CONTEXT_GURU_STATE", "CG_OC_GATEWAY_URL", "CG_OC_API", "CG_OC_MODEL", "CG_OC_SMALL_MODEL"):
            os.environ.pop(name, None)
        self.addCleanup(self.home.cleanup)
        self.shell = FakeShell()
        self.config = root / "config/opencode"
        self.plugin_copy = root / "data/context-guru-opencode/plugin"

    def installer(self, *answers, eof=False):
        self.terminal = FakeTerminal(answers, eof)
        return install_opencode.Installer(self.terminal, self.shell, SOURCE)

    def read(self, name):
        path = self.config / name
        return json.loads(path.read_text()) if path.exists() else None

    def write(self, name, value):
        self.config.mkdir(parents=True, exist_ok=True)
        (self.config / name).write_text(json.dumps(value, indent=2) + "\n")

    def npm_calls(self):
        return [call for call in self.shell.calls if call[0] == "npm"]

    # --------------------------------------------------------------------------- your models
    def test_choice_3_changes_nothing_and_exits_0(self):
        code = self.installer("3").install()

        self.assertEqual(code, 0)
        self.assertIsNone(self.read("opencode.json"))
        self.assertEqual(self.npm_calls(), [])
        self.assertIn("Nothing was changed. context-guru supports Anthropic and OpenAI models.", self.terminal.text())

    def test_end_of_input_at_the_first_question_changes_nothing_and_exits_0(self):
        code = self.installer(eof=True).install()

        self.assertEqual(code, 0)
        self.assertIsNone(self.read("opencode.json"))
        self.assertEqual(self.npm_calls(), [])
        self.assertIn("Nothing was changed. context-guru supports Anthropic and OpenAI models.", self.terminal.text())

    def test_the_first_question_names_the_supported_providers_first(self):
        self.installer("3").install()

        text = self.terminal.text()
        self.assertIn("context-guru works with Anthropic and OpenAI models only.", text)
        self.assertIn("3) Another provider: exit, change nothing", text)
        self.assertLess(text.index("works with Anthropic and OpenAI models only"),
                        text.index("How do you reach your models?"))

    def test_gateway_url_strips_v1_and_slash(self):
        for text in ("https://gw.example/v1", "https://gw.example/v1/", "https://gw.example/", "https://gw.example"):
            with self.subTest(text=text):
                self.assertEqual(install_opencode.gateway_url(text), "https://gw.example/v1")

    def test_gateway_url_refuses_http_to_a_remote_host(self):
        for text in ("http://gw.example", "ftp://gw.example", "gw.example", ""):
            with self.subTest(text=text), self.assertRaises(install_opencode.Stop):
                install_opencode.gateway_url(text)
        self.assertEqual(install_opencode.gateway_url("http://127.0.0.1:4000"), "http://127.0.0.1:4000/v1")
        self.assertEqual(install_opencode.gateway_url("http://localhost:4000/v1"), "http://localhost:4000/v1")

    def test_path_1_gives_an_empty_connection(self):
        connection = self.installer("1").ask_connection({"opencode.json": {}})

        self.assertEqual(connection, install_opencode.Connection(None, None, None, None))

    def test_path_2_asks_url_format_model_and_small_model(self):
        connection = self.installer("2", "https://gw.example", "", "aws/claude-sonnet-5-5",
                                    "aws/claude-haiku-5-5").ask_connection({"opencode.json": {}})

        self.assertEqual(connection, install_opencode.Connection(
            "anthropic-gateway", "https://gw.example/v1", "aws/claude-sonnet-5-5", "aws/claude-haiku-5-5"))

    def test_path_2_small_model_defaults_to_the_model(self):
        connection = self.installer("2", "https://gw.example", "", "m", "").ask_connection({"opencode.json": {}})

        self.assertEqual(connection.small_model, "m")

    def test_path_2_openai_format_gives_openai_gateway(self):
        connection = self.installer("2", "https://gw.example", "openai", "m", "").ask_connection(
            {"opencode.json": {}})

        self.assertEqual(connection.provider, "openai-gateway")

    def test_path_2_model_has_no_default_and_asks_again(self):
        connection = self.installer("2", "https://gw.example", "", "", "m", "").ask_connection(
            {"opencode.json": {}})

        self.assertEqual(connection.model, "m")

    def test_env_variables_answer_the_questions(self):
        os.environ.update(CG_OC_GATEWAY_URL="https://gw.example", CG_OC_API="openai", CG_OC_MODEL="m",
                          CG_OC_SMALL_MODEL="s")

        connection = self.installer().ask_connection({"opencode.json": {}})

        self.assertEqual(connection, install_opencode.Connection("openai-gateway", "https://gw.example/v1", "m", "s"))

    def test_another_provider_with_its_own_base_url_stops_before_any_change(self):
        before = {"provider": {"anthropic": {"options": {"baseURL": "https://other.example/v1"}}}}
        self.write("opencode.json", before)

        code = self.installer("2", "https://gw.example", "", "m", "").install()

        self.assertEqual(code, 1)
        self.assertEqual(self.read("opencode.json"), before)
        self.assertEqual(self.npm_calls(), [])
        self.assertIn("https://other.example/v1", self.terminal.text())

    def test_another_provider_of_the_same_format_without_a_base_url_stops_too(self):
        # The plugin routes it to the default upstream, so a gateway would be a second anthropic upstream.
        before = {"provider": {"anthropic": {"models": {"x": {}}}}}
        self.write("opencode.json", before)

        code = self.installer("2", "https://gw.example", "", "m", "").install()

        self.assertEqual(code, 1)
        self.assertEqual(self.read("opencode.json"), before)
        self.assertIn('the anthropic provider "anthropic"', self.terminal.text())

    def test_a_provider_of_the_other_format_does_not_stop_the_gateway(self):
        self.write("opencode.json", {"provider": {"openai": {"options": {"baseURL": "https://other.example/v1"}}}})

        connection = self.installer("2", "https://gw.example", "", "m", "").ask_connection(
            {"opencode.json": self.read("opencode.json")})

        self.assertEqual(connection.provider, "anthropic-gateway")

    # ---------------------------------------------------------------------- the connection
    GATEWAY = ("2", "https://gw.example", "", "m", "s")  # path, URL, format (default), model, small model

    def test_path_2_writes_the_gateway_provider_and_both_models(self):
        code = self.installer(*self.GATEWAY, "y", "n").install()  # consent, test prompt

        self.assertEqual(code, 0)
        document = self.read("opencode.json")
        self.assertEqual(document["provider"]["anthropic-gateway"], {
            "npm": "@ai-sdk/anthropic", "name": "Anthropic gateway", "options": {"baseURL": "https://gw.example/v1"},
            "models": {"m": {"name": "m"}, "s": {"name": "s"}}})
        self.assertEqual((document["model"], document["small_model"]), ("anthropic-gateway/m", "anthropic-gateway/s"))

    def test_path_1_writes_no_provider_and_no_model(self):
        self.installer("1", "y", "n").install()

        self.assertEqual(set(self.read("opencode.json")), {"$schema", "plugin"})

    def test_uninstall_puts_back_the_old_model_and_removes_the_gateway(self):
        self.write("opencode.json", {"model": "anthropic/claude-x"})
        self.installer(*self.GATEWAY, "y", "n").install()

        code = self.installer("y").uninstall()  # delete local data

        self.assertEqual(code, 0)
        self.assertEqual(self.read("opencode.json"), {"model": "anthropic/claude-x"})

    def test_uninstall_keeps_an_empty_provider_object_the_user_had(self):
        self.write("opencode.json", {"provider": {}})
        self.installer(*self.GATEWAY, "y", "n").install()

        self.installer("y").uninstall()

        self.assertEqual(self.read("opencode.json"), {"provider": {}})

    def test_uninstall_of_a_created_config_removes_the_file(self):
        self.installer(*self.GATEWAY, "y", "n").install()

        self.installer("y").uninstall()

        self.assertIsNone(self.read("opencode.json"))

    def test_uninstall_keeps_a_model_the_user_changed_after_install(self):
        self.installer(*self.GATEWAY, "y", "n").install()
        document = self.read("opencode.json")
        document["model"] = "openai/gpt-x"
        self.write("opencode.json", document)

        self.installer("y").uninstall()

        self.assertEqual(self.read("opencode.json"), {"$schema": "https://opencode.ai/config.json",
                                                      "model": "openai/gpt-x"})

    def test_a_second_install_with_the_same_answers_changes_nothing(self):
        self.write("opencode.json", {"model": "anthropic/claude-x"})
        self.installer(*self.GATEWAY, "y", "n").install()
        first_file = (self.config / "opencode.json").read_text()
        first_before = json.loads(install_opencode.record_path().read_text())["connection"]["before"]

        self.installer(*self.GATEWAY, "y", "n").install()

        self.assertEqual((self.config / "opencode.json").read_text(), first_file)
        self.assertEqual(json.loads(install_opencode.record_path().read_text())["connection"]["before"], first_before)
        self.assertEqual(first_before["model"], "anthropic/claude-x")

    def test_an_existing_gateway_with_another_url_asks_before_replacing(self):
        before = {"provider": {"anthropic-gateway": {"options": {"baseURL": "https://old.example/v1"}}}}
        self.write("opencode.json", before)

        code = self.installer(*self.GATEWAY, "n").install()  # replace: no

        self.assertEqual(code, 1)
        self.assertEqual(self.read("opencode.json"), before)
        self.assertEqual(self.npm_calls(), [])
        self.assertIn("https://old.example/v1", self.terminal.text())

        self.installer(*self.GATEWAY, "y", "y", "n").install()  # replace: yes, consent, test prompt

        self.assertEqual(self.read("opencode.json")["provider"]["anthropic-gateway"]["options"]["baseURL"],
                         "https://gw.example/v1")

    # ---------------------------------------------------------------------------- the login
    def logouts(self):
        return [call for call in self.shell.calls if call[:3] == ["opencode", "auth", "logout"]]

    def test_logged_in_reads_credentials_and_environment(self):
        text = ("\x1b[0m┌  Credentials /x/auth.json\n│\n●  Anthropic api\n●  anthropic-gateway api\n└  2 credentials\n"
                "┌  Environment\n│\n●  \x1b[1mOpenAI\x1b[0m OPENAI_API_KEY\n└  1 environment variable\n")

        self.assertEqual(install_opencode.logged_in(text), {"anthropic", "anthropic-gateway", "openai"})

    def test_path_2_starts_the_login_and_names_the_gateway_id(self):
        self.shell.logins = {"anthropic"}

        code = self.installer(*self.GATEWAY, "y", "n").install()

        self.assertEqual(code, 0)
        self.assertEqual(self.shell.interactive_calls, [["opencode", "auth", "login"]])
        self.assertIn("pick Other (the last choice), then type: anthropic-gateway", self.terminal.text())
        self.assertEqual(json.loads(install_opencode.record_path().read_text())["login_started"], "anthropic-gateway")

    def test_path_2_with_a_direct_anthropic_login_never_touches_that_login(self):
        self.shell.logins = {"anthropic"}
        self.installer(*self.GATEWAY, "y", "n").install()

        self.installer("y").uninstall()

        self.assertEqual(self.logouts(), [["opencode", "auth", "logout", "anthropic-gateway"]])
        self.assertEqual(self.shell.logins, {"anthropic"})

    def test_an_existing_login_skips_the_login(self):
        code = self.installer(*self.GATEWAY, "y", "n").install()

        self.assertEqual(code, 0)
        self.assertEqual(self.shell.interactive_calls, [])
        self.assertNotIn("login_started", json.loads(install_opencode.record_path().read_text()))

    def test_uninstall_does_not_log_out_a_login_the_installer_did_not_start(self):
        self.installer(*self.GATEWAY, "y", "n").install()

        self.installer("y").uninstall()

        self.assertEqual(self.logouts(), [])

    def test_a_failed_login_offers_undo_and_exits_1(self):
        self.shell.logins, self.shell.login_fails = {"anthropic"}, True

        code = self.installer(*self.GATEWAY, "y", "y").install()  # consent, undo

        self.assertEqual(code, 1)
        self.assertIsNone(self.read("opencode.json"))
        self.assertIsNone(self.read("tui.json"))
        self.assertFalse(self.plugin_copy.exists())
        self.assertFalse(install_opencode.record_path().exists())
        self.assertNotIn("✓ The proxy started", self.terminal.text())

    def test_a_failed_login_without_undo_says_how_to_finish(self):
        self.shell.logins, self.shell.login_fails = {"anthropic"}, True

        code = self.installer(*self.GATEWAY, "y", "n").install()  # consent, undo: no

        self.assertEqual(code, 1)
        self.assertIn("anthropic-gateway", self.read("opencode.json")["provider"])
        self.assertIn("opencode auth login", self.terminal.text())

    def test_path_1_with_a_provider_it_cannot_route_offers_undo_and_exits_1(self):
        self.shell.logins, self.shell.login_adds = set(), "groq"

        code = self.installer("1", "y", "y").install()  # path, consent, undo

        self.assertEqual(code, 1)
        self.assertIn("context-guru cannot route", self.terminal.text())
        self.assertIsNone(self.read("opencode.json"))

    def test_undo_keeps_an_opencode_this_run_installed_and_says_so(self):
        self.shell.opencode = None
        self.shell.logins, self.shell.login_fails = set(), True

        code = self.installer("1", "y", "y", "y").install()  # path, install OpenCode, consent, undo

        self.assertEqual(code, 1)
        self.assertEqual([call for call in self.npm_calls() if call[1] == "uninstall"], [])
        self.assertIn("OpenCode stays", self.terminal.text())
        self.assertEqual(json.loads(install_opencode.record_path().read_text()), {"opencode_installed": True})

    # ------------------------------------------------------------------------------ the check
    def test_the_check_is_default_yes(self):
        self.installer("1", "y", "").install()  # path, consent, test prompt: Enter

        self.assertIn(["opencode", "run", "Reply with the single word: ok"], self.shell.calls)

    def test_the_check_passes_with_one_request_through_the_proxy(self):
        code = self.installer("1", "y", "y").install()

        self.assertEqual(code, 0)
        self.assertIn("✓ The proxy started, and OpenCode answered through it.", self.terminal.text())

    def test_zero_requests_fails_with_exit_1(self):
        self.shell.requests = 0

        code = self.installer("1", "y", "y").install()

        self.assertEqual(code, 1)
        self.assertNotIn("✓ The proxy started", self.terminal.text())
        self.assertIn("the proxy counted 0 requests", self.terminal.text())

    def test_no_answer_fails_with_exit_1_and_shows_the_error(self):
        for failure in ("exit", "error-exit-0"):
            with self.subTest(failure=failure):
                self.shell.run_fails = failure

                code = self.installer("1", "y", "y").install()

                self.assertEqual(code, 1)
                self.assertIn("Anthropic API key is missing", self.terminal.text())
                self.assertNotIn("✓ The proxy started", self.terminal.text())

    def test_proxy_down_fails_with_exit_1_and_names_the_log(self):
        self.shell.proxy = "down"

        code = self.installer("1", "y", "y").install()

        self.assertEqual(code, 1)
        self.assertIn(f"The proxy did not start. Its log is under {install_opencode.state_root()}",
                      self.terminal.text())

    def test_no_to_the_check_says_not_checked_and_exits_0(self):
        code = self.installer("1", "y", "n").install()

        self.assertEqual(code, 0)
        self.assertIn("Not checked.", self.terminal.text())
        self.assertNotIn(["opencode", "run", "Reply with the single word: ok"], self.shell.calls)

    def test_consent_names_the_connection(self):
        self.installer(*self.GATEWAY, "y", "n").install()
        self.assertIn("through the gateway at gw.example", self.terminal.text())

        self.installer("1", "y", "n").install()
        self.assertIn("OpenCode reaches Anthropic or OpenAI directly", self.terminal.text())

    # ------------------------------------------------------------------------------- install
    def test_install_adds_the_stable_copy_to_both_config_files(self):
        code = self.installer("1", "y", "n").install()

        self.assertEqual(code, 0)
        self.assertEqual(self.read("opencode.json"), {"$schema": "https://opencode.ai/config.json",
                                                      "plugin": [str(self.plugin_copy / "index.js")]})
        self.assertEqual(self.read("tui.json"), {"$schema": "https://opencode.ai/tui.json",
                                                 "plugin": [str(self.plugin_copy / "tui.js")]})
        self.assertTrue((self.plugin_copy / "index.js").is_file())
        self.assertFalse((self.plugin_copy / "test").exists())

    def test_install_keeps_other_plugins_and_keys(self):
        self.write("opencode.json", {"model": "anthropic/claude-sonnet-5-5", "plugin": ["opencode-foo"]})

        self.installer("1", "y", "n").install()

        self.assertEqual(self.read("opencode.json"), {"model": "anthropic/claude-sonnet-5-5",
                                                      "plugin": ["opencode-foo", str(self.plugin_copy / "index.js")]})

    def test_install_replaces_a_cloned_copy_and_keeps_its_options(self):
        clone = Path(self.home.name) / "src/context-guru/opencode-plugin"
        clone.mkdir(parents=True)
        (clone / "index.js").write_text('console.log("opencode-context-guru: x")\n')
        (clone / "tui.js").write_text('// opencode-context-guru\n')
        self.write("opencode.json", {"plugin": [[str(clone / "index.js"), {"preset": "medium"}]]})
        self.write("tui.json", {"plugin": [str(clone / "tui.js")]})

        self.installer("1", "y", "n").install()

        self.assertEqual(self.read("opencode.json")["plugin"], [[str(self.plugin_copy / "index.js"),
                                                                 {"preset": "medium"}]])
        self.assertEqual(self.read("tui.json")["plugin"], [str(self.plugin_copy / "tui.js")])

    def test_second_install_adds_no_second_entry(self):
        self.installer("1", "y", "n").install()
        self.installer("1", "y", "n").install()

        self.assertEqual(self.read("opencode.json")["plugin"], [str(self.plugin_copy / "index.js")])
        self.assertEqual(self.read("tui.json")["plugin"], [str(self.plugin_copy / "tui.js")])

    def test_no_consent_changes_nothing(self):
        code = self.installer("1", "n").install()

        self.assertEqual(code, 0)
        self.assertFalse(self.config.exists())
        self.assertFalse(self.plugin_copy.exists())
        self.assertIn("context-guru stays off", self.terminal.text())

    def test_consent_text_names_routing_and_keep_alive_on_the_users_credential(self):
        self.installer("1", "n").install()

        text = self.terminal.text()
        self.assertIn("Anthropic and OpenAI requests", text)
        self.assertIn("your own credential", text)

    def test_jsonc_config_stops_with_the_entries_to_add_by_hand(self):
        self.config.mkdir(parents=True)
        (self.config / "opencode.json").write_text('{\n  // my comment\n  "plugin": []\n}\n')

        code = self.installer("1").install()

        self.assertEqual(code, 1)
        self.assertEqual((self.config / "opencode.json").read_text(), '{\n  // my comment\n  "plugin": []\n}\n')
        self.assertIn("is not plain JSON", self.terminal.text())
        self.assertFalse(self.plugin_copy.exists())

    def test_an_opencode_jsonc_file_stops_the_install(self):
        self.config.mkdir(parents=True)
        (self.config / "opencode.jsonc").write_text("{}\n")

        code = self.installer("1").install()

        self.assertEqual(code, 1)
        self.assertIn("opencode.jsonc", self.terminal.text())
        self.assertFalse((self.config / "opencode.json").exists())

    def test_backs_up_an_existing_config_before_writing(self):
        self.write("opencode.json", {"plugin": ["opencode-foo"]})

        self.installer("1", "y", "n").install()

        backup = install_opencode.state_root() / "installer-backup/opencode.json"
        self.assertEqual(json.loads(backup.read_text()), {"plugin": ["opencode-foo"]})

    # ------------------------------------------------------------------------------ OpenCode
    def npm_installs(self):
        return [call for call in self.npm_calls() if call[1] == "install"]

    def test_installs_the_latest_opencode_when_missing_after_a_yes(self):
        self.shell.opencode = None

        code = self.installer("1", "y", "y", "n").install()

        self.assertEqual(code, 0)
        self.assertEqual(self.npm_calls(), [
            ["npm", "view", "opencode-ai", "version", "--registry=https://registry.npmjs.org/"],
            ["npm", "install", "-g", "--registry=https://registry.npmjs.org/", "opencode-ai@1.19.6"]])
        self.assertTrue(json.loads((install_opencode.state_root() / "installer.json").read_text())
                        ["opencode_installed"])

    def test_no_to_installing_opencode_stops_and_changes_nothing(self):
        self.shell.opencode = None

        code = self.installer("1", "n").install()

        self.assertEqual(code, 1)
        self.assertEqual(self.npm_installs(), [])
        self.assertFalse(self.config.exists())

    def test_missing_opencode_and_no_npm_names_the_install_page(self):
        self.shell.opencode, self.shell.has_npm = None, False

        code = self.installer("1").install()

        self.assertEqual(code, 1)
        self.assertIn("https://opencode.ai/docs", self.terminal.text())

    def test_an_installed_opencode_without_npm_stays(self):
        self.shell.opencode, self.shell.has_npm = "1.19.5", False

        code = self.installer("1", "y", "n").install()

        self.assertEqual(code, 0)
        self.assertEqual(self.npm_calls(), [])
        self.assertIn("OpenCode 1.19.5 is installed", self.terminal.text())

    def test_npm_that_cannot_read_the_latest_version_names_the_registry_setting(self):
        self.shell.opencode, self.shell.npm_view_error = None, "E401 Unable to authenticate"

        code = self.installer("1").install()

        self.assertEqual(code, 1)
        self.assertIn("E401 Unable to authenticate", self.terminal.text())
        self.assertIn("CG_OC_NPM_REGISTRY", self.terminal.text())
        self.assertFalse(self.config.exists())

    def test_npm_that_cannot_read_the_latest_version_keeps_a_supported_opencode(self):
        self.shell.opencode, self.shell.npm_view_error = "1.19.5", "E401 Unable to authenticate"

        code = self.installer("1", "y", "n").install()  # no OpenCode question

        self.assertEqual(code, 0)
        self.assertEqual(self.npm_installs(), [])
        text = self.terminal.text()
        self.assertIn("cannot check for a newer OpenCode", text)
        self.assertIn("CG_OC_NPM_REGISTRY", text)
        self.assertIn("OpenCode 1.19.5 stays", text)

    def test_an_npm_failure_shows_npms_message_and_never_uses_sudo(self):
        self.shell.opencode, self.shell.npm_install_error = None, "EACCES: permission denied"

        code = self.installer("1", "y").install()

        self.assertEqual(code, 1)
        self.assertIn("EACCES: permission denied", self.terminal.text())
        self.assertNotIn("sudo", [arg for call in self.shell.calls for arg in call])
        self.assertFalse(self.config.exists())

    def test_an_older_opencode_is_updated_to_the_latest_after_a_yes(self):
        self.shell.opencode = "1.19.5"

        self.installer("1", "y", "y", "n").install()

        self.assertEqual(self.npm_installs()[0][-1], "opencode-ai@1.19.6")
        record = json.loads((install_opencode.state_root() / "installer.json").read_text())
        self.assertNotIn("opencode_installed", record)  # it was there before: uninstall must not offer to remove it

    def test_no_to_the_update_keeps_a_supported_opencode(self):
        self.shell.opencode = "1.19.5"

        code = self.installer("1", "n", "y", "n").install()

        self.assertEqual(code, 0)
        self.assertEqual(self.npm_installs(), [])
        self.assertIn("OpenCode 1.19.5 stays", self.terminal.text())

    def test_no_to_the_update_of_a_too_old_opencode_stops_the_install(self):
        self.shell.opencode = "1.17.0"

        code = self.installer("1", "n").install()

        self.assertEqual(code, 1)
        self.assertIn("1.18.29", self.terminal.text())
        self.assertFalse(self.config.exists())

    def test_the_install_uses_the_latest_version_as_npm_prints_it(self):
        self.shell.opencode, self.shell.latest = None, "1.21.0-rc.1"

        self.installer("1", "y", "y", "n").install()

        self.assertEqual(self.npm_installs()[0][-1], "opencode-ai@1.21.0-rc.1")

    def test_an_older_opencode_first_on_path_is_not_reported_as_updated(self):
        self.shell.opencode, self.shell.npm_shadowed = "1.19.5", True

        code = self.installer("1", "y", "n").install()  # update yes, consent no

        self.assertEqual(code, 0)
        text = self.terminal.text()
        self.assertNotIn("✓ OpenCode 1.19.5 updated", text)
        self.assertIn("still runs OpenCode 1.19.5", text)

    def test_the_tested_opencode_line_has_no_trailing_space(self):
        self.shell.opencode = self.shell.latest = "1.19.5"

        self.installer("1", "n").install()

        self.assertIn("OpenCode 1.19.5 is installed.", self.terminal.shown)

    def test_the_latest_opencode_stays_and_names_the_tested_version(self):
        code = self.installer("1", "y", "n").install()

        self.assertEqual(code, 0)
        self.assertEqual(self.npm_installs(), [])
        self.assertIn("OpenCode 1.19.6 is installed", self.terminal.text())
        self.assertIn("tested on OpenCode 1.19.5", self.terminal.text())

    def test_a_newer_pre_release_is_never_downgraded(self):
        self.shell.opencode = "1.20.0-beta.1"

        code = self.installer("1", "y", "n").install()  # no OpenCode question

        self.assertEqual(code, 0)
        self.assertEqual(self.npm_installs(), [])
        self.assertIn("OpenCode 1.20.0 is installed", self.terminal.text())

    def test_an_opencode_whose_version_cannot_be_read_stays_after_a_yes(self):
        self.shell.opencode_version_text = "opencode dev build"

        code = self.installer("1", "y", "y", "n").install()  # go on with it, consent, no test prompt

        self.assertEqual(code, 0)
        self.assertEqual(self.npm_installs(), [])
        self.assertIn("cannot read the version", self.terminal.text())
        self.assertNotIn("opencode_installed", json.loads(install_opencode.record_path().read_text()))

    def test_an_opencode_whose_version_cannot_be_read_and_a_no_stops(self):
        self.shell.opencode_version_text = "opencode dev build"

        code = self.installer("1", "n").install()

        self.assertEqual(code, 1)
        self.assertFalse(self.config.exists())

    # -------------------------------------------------------------------------- test prompt
    def test_test_prompt_reports_the_proxy_up(self):
        code = self.installer("1", "y", "y").install()

        self.assertEqual(code, 0)
        self.assertIn("✓ The proxy started, and OpenCode answered", self.terminal.text())

    def test_test_prompt_that_fails_is_not_reported_as_working(self):
        for failure in ("exit", "error-exit-0"):
            with self.subTest(failure=failure):
                self.shell.run_fails = failure

                self.installer("1", "y", "y").install()

                text = self.terminal.text()
                self.assertNotIn("✓ The proxy started, and OpenCode answered", text)
                self.assertIn("The proxy started, but OpenCode could not answer", text)
                self.assertIn("Anthropic API key is missing", text)
                self.assertIn("opencode auth login", text)

    def test_test_prompt_stops_its_own_proxy_and_removes_its_state(self):
        self.installer("1", "y", "y").install()

        resets = [call for call in self.shell.calls if call[0] == "sh" and call[1].endswith("context-guru-reset")]
        self.assertEqual(len(resets), 1)
        project = Path(resets[0][1]).parent
        self.assertEqual(project.parent, install_opencode.state_root())
        self.assertFalse(project.exists())

    def test_an_opencode_first_on_path_that_is_still_old_stops_before_consent(self):
        self.shell.opencode, self.shell.npm_shadowed = "1.17.0", True

        code = self.installer("1", "y").install()  # one question only: the update

        self.assertEqual(code, 1)
        self.assertIn("still runs OpenCode 1.17.0", self.terminal.text())
        self.assertIn("/usr/local/bin/opencode", self.terminal.text())
        self.assertFalse(self.config.exists())

    def test_carried_options_with_a_preset_change_the_consent_text(self):
        clone = Path(self.home.name) / "src/opencode-plugin"
        clone.mkdir(parents=True)
        (clone / "index.js").write_text("// opencode-context-guru\n")
        self.write("opencode.json", {"plugin": [[str(clone / "index.js"), {"preset": "medium"}]]})

        self.installer("1", "n").install()

        self.assertIn("The preset is medium", self.terminal.text())
        self.assertNotIn("The preset is off", self.terminal.text())

    def test_install_writes_through_a_symlinked_config_and_keeps_the_link(self):
        dotfiles = Path(self.home.name) / "dotfiles/opencode.json"
        dotfiles.parent.mkdir(parents=True)
        dotfiles.write_text('{"plugin": ["opencode-foo"]}\n')
        self.config.mkdir(parents=True)
        (self.config / "opencode.json").symlink_to(dotfiles)

        self.installer("1", "y", "n").install()

        self.assertTrue((self.config / "opencode.json").is_symlink())
        self.assertEqual(json.loads(dotfiles.read_text())["plugin"],
                         ["opencode-foo", str(self.plugin_copy / "index.js")])

    def test_install_keeps_the_config_file_mode(self):
        self.write("opencode.json", {"plugin": []})
        (self.config / "opencode.json").chmod(0o644)

        self.installer("1", "y", "n").install()

        self.assertEqual((self.config / "opencode.json").stat().st_mode & 0o777, 0o644)

    # ----------------------------------------------------------------------------- uninstall
    def test_uninstall_removes_only_what_install_added(self):
        self.write("opencode.json", {"model": "x", "plugin": ["opencode-foo"]})
        self.installer("1", "y", "n").install()

        code = self.installer("y").uninstall()

        self.assertEqual(code, 0)
        self.assertEqual(self.read("opencode.json"), {"model": "x", "plugin": ["opencode-foo"]})
        self.assertFalse((self.config / "tui.json").exists())  # the installer created it
        self.assertFalse(self.plugin_copy.parent.exists())  # no empty folder is left
        self.assertFalse(install_opencode.state_root().exists())

    def test_uninstall_stops_each_projects_proxy(self):
        self.installer("1", "y", "n").install()
        project = install_opencode.state_root() / "0123456789abcdef"
        project.mkdir(parents=True)
        (project / "context-guru-reset").write_text("#!/bin/sh\n")

        self.installer("y").uninstall()

        self.assertIn(["sh", str(project / "context-guru-reset")], self.shell.calls)

    def test_uninstall_keeping_local_data_deletes_only_the_installers_files(self):
        self.installer("1", "y", "n").install()
        log = install_opencode.state_root() / "0123456789abcdef/proxy.log"
        log.parent.mkdir(parents=True)
        log.write_text("log\n")

        self.installer("n").uninstall()

        self.assertTrue(log.exists())
        self.assertFalse((install_opencode.state_root() / "installer.json").exists())
        self.assertFalse((install_opencode.state_root() / "installer-backup").exists())

    def test_uninstall_offers_to_remove_opencode_only_when_it_installed_it(self):
        self.shell.opencode = None
        self.installer("1", "y", "y", "n").install()

        self.installer("y", "y").uninstall()

        self.assertEqual(self.npm_calls()[-1], ["npm", "uninstall", "-g", "opencode-ai"])

    def test_uninstall_leaves_an_opencode_it_did_not_install(self):
        self.installer("1", "y", "n").install()

        self.installer("y").uninstall()  # one question only: the local data

        self.assertEqual([call for call in self.npm_calls() if call[1] == "uninstall"], [])

    def test_uninstall_with_a_jsonc_file_still_stops_proxies_and_removes_the_copy(self):
        self.installer("1", "y", "n").install()
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
        self.installer("1", "y", "n").install()
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
