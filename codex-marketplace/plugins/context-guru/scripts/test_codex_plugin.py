import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

MODULE_PATH = Path(__file__).with_name("codex_plugin.py")
SPEC = importlib.util.spec_from_file_location("codex_plugin", MODULE_PATH)
PLUGIN = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PLUGIN)


class CodexPluginTest(unittest.TestCase):
    def test_session_start_hook_is_async(self):
        hooks_path = MODULE_PATH.parent.parent / "hooks.json"
        hooks = json.loads(hooks_path.read_text())
        handler = hooks["hooks"]["SessionStart"][0]["hooks"][0]
        self.assertEqual(handler["command"], "python3 ./scripts/codex_plugin.py serve")
        self.assertIs(handler["async"], True)

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        root = Path(self.temp.name)
        self.env = mock.patch.dict(os.environ, {
            "HOME": str(root / "home"),
            "CODEX_HOME": str(root / "codex"),
            "XDG_STATE_HOME": str(root / "state"),
        }, clear=False)
        self.env.start()

    def tearDown(self):
        self.env.stop()
        self.temp.cleanup()

    def test_profile_is_isolated_and_uses_responses_wire_api(self):
        PLUGIN.write_profile(8791)
        text = PLUGIN.profile().read_text()
        self.assertTrue(text.startswith(PLUGIN.MARKER))
        self.assertIn('base_url = "http://127.0.0.1:8791/openai/v1"', text)
        self.assertIn('wire_api = "responses"', text)
        self.assertFalse((PLUGIN.codex_home() / "config.toml").exists())

    def test_profile_and_proxy_preserve_custom_provider_route(self):
        provider = {
            "base_url": "https://gateway.example.test/",
            "requires_openai_auth": False,
            "experimental_bearer_token": "test-token",
        }
        PLUGIN.write_profile(8791, provider)
        text = PLUGIN.profile().read_text()
        self.assertIn('experimental_bearer_token = "test-token"', text)
        self.assertIn("requires_openai_auth = false", text)
        self.assertEqual(PLUGIN.profile().stat().st_mode & 0o777, 0o600)
        command = PLUGIN.proxy_command("/proxy", 8791, provider["base_url"])
        self.assertEqual(command[-2:], ["--openai-upstream", "https://gateway.example.test"])

    def test_reads_selected_base_provider_without_external_toml_dependency(self):
        PLUGIN.codex_home().mkdir(parents=True)
        (PLUGIN.codex_home() / "config.toml").write_text('''
model_provider = "gateway"
[model_providers.other]
base_url = "https://wrong.example"
[model_providers.gateway]
base_url = "https://right.example"
requires_openai_auth = false
experimental_bearer_token = "secret"
[tui]
screen_reader_detection_done = true
''')
        self.assertEqual(PLUGIN.base_provider(), {
            "base_url": "https://right.example",
            "requires_openai_auth": False,
            "experimental_bearer_token": "secret",
        })

    def test_refuses_unmanaged_profile(self):
        PLUGIN.profile().parent.mkdir(parents=True)
        PLUGIN.profile().write_text("model = 'mine'\n")
        with self.assertRaisesRegex(RuntimeError, "unmanaged profile"):
            PLUGIN.write_profile(8791)

    def test_uninstall_keeps_unmanaged_profile_and_unowned_process(self):
        PLUGIN.profile().parent.mkdir(parents=True)
        PLUGIN.profile().write_text("model = 'mine'\n")
        PLUGIN.state_dir().mkdir(parents=True)
        (PLUGIN.state_dir() / "install.json").write_text(json.dumps({
            "pid": 123, "binary": "/expected", "port": 8791
        }))
        args = type("Args", (), {"dry_run": False})()
        with mock.patch.object(PLUGIN.subprocess, "check_output", return_value="/other --flag"), \
             mock.patch.object(PLUGIN.os, "kill") as kill:
            self.assertEqual(PLUGIN.uninstall(args), 0)
            kill.assert_not_called()
        self.assertTrue(PLUGIN.profile().exists())
        self.assertTrue((PLUGIN.state_dir() / "install.json").exists())

    def test_escape_hatch_is_installed_outside_plugin(self):
        target = PLUGIN.install_escape_hatch()
        self.assertEqual(target, PLUGIN.state_dir() / "context-guru-reset")
        self.assertTrue(os.access(target, os.X_OK))
        self.assertIn("Managed by the context-guru Codex plugin", target.read_text())

    def test_update_check_is_read_only(self):
        args = type("Args", (), {"check": True, "install": False})()
        completed = mock.Mock(returncode=0, stdout="update_available=false\n", stderr="")
        with mock.patch.object(PLUGIN, "shared_installer", return_value=Path(__file__)), \
             mock.patch.object(PLUGIN.subprocess, "run", return_value=completed) as run:
            self.assertEqual(PLUGIN.update(args), 0)
        self.assertEqual(run.call_args.args[0][-1], "--check-latest")
        self.assertNotIn("CONTEXT_GURU_UPGRADE", run.call_args.kwargs["env"])

    def test_reset_dry_run_changes_nothing(self):
        PLUGIN.write_profile(8791)
        PLUGIN.state_dir().mkdir(parents=True, exist_ok=True)
        record = PLUGIN.state_dir() / "install.json"
        record.write_text(json.dumps({"port": 8791, "pid": 999999, "binary": "/missing"}))
        env = os.environ.copy()
        completed = subprocess.run(
            ["sh", str(Path(__file__).with_name("reset.sh")), "--dry-run"],
            env=env, text=True, capture_output=True,
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertIn("result=planned", completed.stdout)
        self.assertTrue(PLUGIN.profile().exists())
        self.assertTrue(record.exists())

    def test_reset_backup_is_private(self):
        PLUGIN.write_profile(8791, {"experimental_bearer_token": "secret"})
        completed = subprocess.run(
            ["sh", str(Path(__file__).with_name("reset.sh")), "--yes"],
            env=os.environ.copy(), text=True, capture_output=True,
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)
        backups = list((PLUGIN.state_dir() / "recovery").iterdir())
        self.assertEqual(len(backups), 1)
        self.assertEqual(backups[0].stat().st_mode & 0o777, 0o600)

    def test_explicit_update_sets_upgrade_gate(self):
        args = type("Args", (), {"check": False, "install": True})()
        completed = mock.Mock(returncode=1, stdout="result=error\n", stderr="")
        with mock.patch.object(PLUGIN, "shared_installer", return_value=Path(__file__)), \
             mock.patch.object(PLUGIN.subprocess, "run", return_value=completed) as run:
            self.assertEqual(PLUGIN.update(args), 1)
        self.assertEqual(run.call_args.kwargs["env"]["CONTEXT_GURU_UPGRADE"], "1")


if __name__ == "__main__":
    unittest.main()
