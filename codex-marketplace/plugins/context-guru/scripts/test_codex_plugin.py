import importlib.util
import hashlib
import io
import json
import os
from pathlib import Path
import plistlib
import subprocess
import tarfile
import tempfile
import time
import unittest
import urllib.error
from unittest import mock

MODULE_PATH = Path(__file__).with_name("codex_plugin.py")
SPEC = importlib.util.spec_from_file_location("codex_plugin", MODULE_PATH)
PLUGIN = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PLUGIN)


REAL_LOAD_STRATEGY_TABLE = PLUGIN.load_strategy_table
TABLE = {
    "default": "auto", "aliases": {"5-min-ping": "auto"},
    "presets": ["off", "conservative", "medium", "high", "xhigh", "ultrahigh", "house"],
    "strategies": [
        {"name": "none", "spends": False, "desc": "no pings", "providers": ["any"], "cache": {}},
        {"name": "auto", "spends": True, "desc": "anthropic pings", "providers": ["anthropic"],
         "cache": {"keepalive": True}},
        {"name": "30-min-ping", "spends": True, "desc": "openai pings", "providers": ["openai"],
         "cache": {"keepalive": True}},
    ],
}


def write_script(path, body):
    path.write_text("#!/bin/sh\n" + body)
    path.chmod(0o755)
    return str(path)


class CodexPluginTest(unittest.TestCase):
    def test_proxy_lifecycle_does_not_depend_on_a_sandboxed_session_hook(self):
        hooks_path = MODULE_PATH.parent.parent / "hooks.json"
        hooks = json.loads(hooks_path.read_text())["hooks"]
        self.assertEqual(sorted(hooks), ["SessionEnd", "SessionStart"])
        commands = [h["command"] for groups in hooks.values() for g in groups for h in g["hooks"]]
        self.assertTrue(all(c.endswith("codex_plugin.py\" signal") for c in commands), commands)

    def test_signal_hook_sends_only_the_closed_fields(self):
        PLUGIN.state_dir().mkdir(parents=True)
        PLUGIN.state_dir().joinpath("install.json").write_text('{"port": 8791}')
        payload = {"session_id": "019a-thread", "hook_event_name": "SessionEnd", "reason": "other",
                   "cwd": "/home/u/secret", "transcript_path": "/home/u/t.jsonl"}
        with mock.patch("sys.stdin", io.StringIO(json.dumps(payload))), \
             mock.patch.object(PLUGIN.urllib.request, "urlopen") as send:
            self.assertEqual(PLUGIN.signal_hook(None), 0)
        request = send.call_args.args[0]
        self.assertEqual(request.full_url, "http://127.0.0.1:8791/signal")
        self.assertEqual(json.loads(request.data),
                         {"session": "019a-thread", "event": "SessionEnd", "kind": "other"})
        for bad in ({"session_id": "../x", "hook_event_name": "SessionEnd"},
                    {"session_id": "s", "hook_event_name": "Stop"}):
            with mock.patch("sys.stdin", io.StringIO(json.dumps(bad))), \
                 mock.patch.object(PLUGIN.urllib.request, "urlopen") as send:
                self.assertEqual(PLUGIN.signal_hook(None), 0)
            send.assert_not_called()
        with mock.patch("sys.stdin", io.StringIO(json.dumps(payload))), \
             mock.patch.object(PLUGIN.urllib.request, "urlopen", side_effect=OSError("refused")):
            self.assertEqual(PLUGIN.signal_hook(None), 0)

    def test_skills_cover_sandbox_boundaries(self):
        # insights-capabilities/-components/-idle folded into `insights` (README: "still work for
        # one release"); their folders are gone, so naming them here made this test error before it
        # reached the assertions below — which hid a wrong assertion for two releases.
        skills = MODULE_PATH.parent.parent / "skills"
        for name in ("status", "insights"):
            text = (skills / name / "SKILL.md").read_text()
            self.assertIn('sandbox_permissions="require_escalated"', text, name)
            self.assertIn("unverified", text, name)
        for name in ("setup", "preset-picker", "cache-strategy-picker", "update"):
            text = (skills / name / "SKILL.md").read_text()
            self.assertIn('sandbox_permissions="require_escalated"', text, name)
        update = (skills / "update" / "SKILL.md").read_text()
        self.assertIn("does not run this check automatically", update)
        self.assertIn("periodically", update)
        self.assertIn("Do not install the update from inside Codex", update)
        self.assertIn("context-guru-update", update)
        uninstall = (skills / "uninstall" / "SKILL.md").read_text()
        self.assertIn("Do not run the mutating uninstall command from inside Codex", uninstall)
        self.assertIn("context-guru-reset --yes", uninstall)
        status = (skills / "status" / "SKILL.md").read_text()
        self.assertIn("status --stats", status)
        self.assertIn("file path", status)  # the #411 contract: the path, not the JSON
        self.assertIn("do not print the JSON", status)
        self.assertIn("no narration", status)

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        root = Path(self.temp.name)
        self.env = mock.patch.dict(os.environ, {
            "HOME": str(root / "home"),
            "CODEX_HOME": str(root / "codex"),
            "XDG_STATE_HOME": str(root / "state"),
        }, clear=False)
        self.env.start()
        self.table = mock.patch.object(PLUGIN, "load_strategy_table", return_value=TABLE)
        self.table.start()

    def tearDown(self):
        self.table.stop()
        self.env.stop()
        self.temp.cleanup()

    def test_profile_and_proxy_preserve_custom_provider_route(self):
        provider = {
            "base_url": "https://gateway.example.test/",
            "requires_openai_auth": False,
            "experimental_bearer_token": "test-token",
        }
        PLUGIN.main_config().parent.mkdir(parents=True)
        PLUGIN.main_config().write_text('model_provider = "gateway"\n\n[model_providers.gateway]\n'
                                        'base_url = "https://gateway.example.test/"\n'
                                        'requires_openai_auth = false\n'
                                        'experimental_bearer_token = "test-token"\n')
        PLUGIN.install_default_route(PLUGIN.main_config(), PLUGIN.routing_state(), 8791, provider)
        text = PLUGIN.main_config().read_text()
        self.assertIn('experimental_bearer_token = "test-token"', text)
        self.assertIn("requires_openai_auth = false", text)
        self.assertEqual(PLUGIN.main_config().stat().st_mode & 0o777, 0o600)
        command = PLUGIN.proxy_command("/proxy", 8791, provider["base_url"])
        self.assertEqual(command[3:5], ["--config", str(PLUGIN.proxy_config())])
        self.assertEqual(command[-2:], ["--openai-upstream", "https://gateway.example.test"])

    def test_proxy_config_uses_off_preset_and_the_30_min_ping_strategy(self):
        path = PLUGIN.write_proxy_config()
        self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        self.assertEqual(path.read_text(),
                         f"{PLUGIN.CONFIG_MARKER}\npreset: off\ncache:\n  strategy: 30-min-ping\n")

    def test_linux_proxy_is_started_by_user_service_not_child_process(self):
        with mock.patch.object(PLUGIN.platform, "system", return_value="Linux"), \
             mock.patch.object(PLUGIN.subprocess, "run") as run, \
             mock.patch.object(PLUGIN.subprocess, "check_output", return_value="4321\n"), \
             mock.patch.object(PLUGIN.subprocess, "Popen") as popen, \
             mock.patch.object(PLUGIN, "healthy", return_value=True):
            process = PLUGIN.start_proxy("/opt/context guru/proxy", 8791)
        self.assertEqual(process.pid, 4321)
        popen.assert_not_called()
        unit = PLUGIN.systemd_unit()
        self.assertTrue(unit.read_text().startswith(PLUGIN.CONFIG_MARKER))
        self.assertIn('ExecStart="/opt/context guru/proxy"', unit.read_text())
        self.assertIn(["systemctl", "--user", "enable", "--now", str(unit)],
                      [call.args[0] for call in run.call_args_list])

    def test_stop_owned_service_does_not_trust_recorded_pid(self):
        unit = PLUGIN.systemd_unit()
        unit.parent.mkdir(parents=True)
        unit.write_text(PLUGIN.CONFIG_MARKER + "\n[Service]\n")
        with mock.patch.object(PLUGIN.platform, "system", return_value="Linux"), \
             mock.patch.object(PLUGIN.subprocess, "run") as run, \
             mock.patch.object(PLUGIN.os, "kill") as kill:
            self.assertEqual(PLUGIN.stop_owned({"service": "systemd", "pid": 22}), "stopped")
        kill.assert_not_called()
        self.assertFalse(unit.exists())
        self.assertIn(["systemctl", "--user", "disable", "--now", unit.name],
                      [call.args[0] for call in run.call_args_list])

    def test_macos_proxy_is_started_by_launch_agent(self):
        with mock.patch.object(PLUGIN.platform, "system", return_value="Darwin"), \
             mock.patch.object(PLUGIN.subprocess, "run") as run, \
             mock.patch.object(PLUGIN.subprocess, "Popen") as popen, \
             mock.patch.object(PLUGIN, "healthy", return_value=True):
            process = PLUGIN.start_proxy("/opt/context guru/proxy", 8791)
        self.assertEqual(process.pid, 0)
        popen.assert_not_called()
        with open(PLUGIN.launchd_plist(), "rb") as handle:
            data = plistlib.load(handle)
        self.assertEqual(data["ProgramArguments"][0], "/opt/context guru/proxy")
        self.assertTrue(data["KeepAlive"])
        self.assertIn("bootstrap", [part for call in run.call_args_list for part in call.args[0]])

    def test_refuses_to_overwrite_unmanaged_user_service(self):
        unit = PLUGIN.systemd_unit()
        unit.parent.mkdir(parents=True)
        unit.write_text("[Service]\nExecStart=/mine\n")
        with mock.patch.object(PLUGIN.platform, "system", return_value="Linux"), \
             mock.patch.object(PLUGIN.subprocess, "run") as run:
            self.assertIsNone(PLUGIN.start_proxy("/proxy", 8791))
        self.assertEqual(unit.read_text(), "[Service]\nExecStart=/mine\n")
        run.assert_not_called()

    def test_refuses_unmanaged_proxy_config(self):
        PLUGIN.proxy_config().parent.mkdir(parents=True)
        PLUGIN.proxy_config().write_text("preset: mine\n")
        with self.assertRaisesRegex(RuntimeError, "unmanaged proxy config"):
            PLUGIN.write_proxy_config()

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

    def test_setup_bootstraps_release_when_binary_is_missing(self):
        args = type("Args", (), {"plan": False, "i_consent_to_traffic_interception": True})()
        process = mock.Mock(pid=4321)
        with mock.patch.object(PLUGIN, "find_binary", return_value=None), \
             mock.patch.object(PLUGIN, "install_release_binary",
                               return_value=("/managed/context-guru-proxy", "v1.2.3")) as install, \
             mock.patch.object(PLUGIN, "first_free_port", return_value=8791), \
             mock.patch.object(PLUGIN, "start_proxy", return_value=process), \
             mock.patch.object(PLUGIN, "install_default_route") as route, \
             mock.patch.object(PLUGIN, "install_escape_hatch", return_value=Path("/reset")):
            self.assertEqual(PLUGIN.setup(args), 0)
        install.assert_called_once_with()
        route.assert_called_once()
        record = json.loads((PLUGIN.state_dir() / "install.json").read_text())
        self.assertEqual(record["binary"], "/managed/context-guru-proxy")

    def test_setup_refuses_without_explicit_consent(self):
        args = type("Args", (), {"plan": False, "i_consent_to_traffic_interception": False})()
        with mock.patch.object(PLUGIN, "install_release_binary") as install:
            self.assertEqual(PLUGIN.setup(args), 2)
        install.assert_not_called()

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

    def test_uninstall_removes_managed_binary_after_safe_cleanup(self):
        binary = PLUGIN.managed_binary()
        binary.parent.mkdir(parents=True)
        binary.write_bytes(b"proxy")
        PLUGIN.state_dir().joinpath("install.json").write_text("{}")
        args = type("Args", (), {"dry_run": False})()
        with mock.patch.object(PLUGIN, "stop_owned", return_value="gone"):
            self.assertEqual(PLUGIN.uninstall(args), 0)
        self.assertFalse(binary.exists())
        self.assertFalse(binary.parent.exists())

    def test_uninstall_preserves_managed_binary_when_process_is_not_owned(self):
        binary = PLUGIN.managed_binary()
        binary.parent.mkdir(parents=True)
        binary.write_bytes(b"proxy")
        PLUGIN.state_dir().joinpath("install.json").write_text("{}")
        args = type("Args", (), {"dry_run": False})()
        with mock.patch.object(PLUGIN, "stop_owned", return_value="not_owned"):
            self.assertEqual(PLUGIN.uninstall(args), 0)
        self.assertTrue(binary.exists())

    def test_status_stats_writes_exact_response_body_and_prints_the_path(self):
        PLUGIN.state_dir().mkdir(parents=True)
        PLUGIN.state_dir().joinpath("install.json").write_text('{"port": 8791}')
        body = b'{"requests":3,"tokens_saved":17}\n'
        stdout = io.StringIO()
        stderr = io.StringIO()
        args = type("Args", (), {"stats": True})()
        with mock.patch.object(PLUGIN.urllib.request, "urlopen",
                               return_value=io.BytesIO(body)) as fetch, \
             mock.patch("sys.stdout", stdout), mock.patch("sys.stderr", stderr):
            self.assertEqual(PLUGIN.status(args), 0)
        fetch.assert_called_once_with("http://127.0.0.1:8791/stats", timeout=2)
        # The #411 contract: stdout holds only the snapshot's path, and the file holds the exact
        # bytes the proxy sent, unreadable by anyone else (mkstemp's 0600).
        path = stdout.getvalue().strip()
        self.assertNotIn("tokens_saved", stdout.getvalue())
        self.assertTrue(path.endswith(".json"), path)
        self.addCleanup(os.unlink, path)
        with open(path, "rb") as file:
            self.assertEqual(file.read(), body)
        self.assertEqual(os.stat(path).st_mode & 0o777, 0o600)
        self.assertEqual(stderr.getvalue(), "")

    def test_status_stats_without_install_keeps_stdout_empty(self):
        stdout = io.StringIO()
        stderr = io.StringIO()
        args = type("Args", (), {"stats": True})()
        with mock.patch("sys.stdout", stdout), mock.patch("sys.stderr", stderr):
            self.assertEqual(PLUGIN.status(args), 1)
        self.assertEqual(stdout.getvalue(), "")
        self.assertIn("no installed proxy", stderr.getvalue())

    def test_status_stats_fetch_failure_keeps_stdout_empty(self):
        PLUGIN.state_dir().mkdir(parents=True)
        PLUGIN.state_dir().joinpath("install.json").write_text('{"port": 8791}')
        stdout = io.StringIO()
        stderr = io.StringIO()
        args = type("Args", (), {"stats": True})()
        error = urllib.error.URLError("connection refused")
        with mock.patch.object(PLUGIN.urllib.request, "urlopen", side_effect=error), \
             mock.patch("sys.stdout", stdout), mock.patch("sys.stderr", stderr):
            self.assertEqual(PLUGIN.status(args), 1)
        self.assertEqual(stdout.getvalue(), "")
        self.assertIn("connection refused", stderr.getvalue())

    def test_escape_hatch_is_installed_outside_plugin(self):
        target = PLUGIN.install_escape_hatch()
        self.assertEqual(target, PLUGIN.state_dir() / "context-guru-reset")
        self.assertTrue(os.access(target, os.X_OK))
        self.assertIn("Managed by the context-guru Codex plugin", target.read_text())
        self.assertTrue((PLUGIN.state_dir() / "config_route.py").is_file())
        self.assertTrue(os.access(PLUGIN.state_dir() / "context-guru-update", os.X_OK))
        self.assertTrue(os.access(PLUGIN.state_dir() / "codex_plugin.py", os.X_OK))

    def test_default_route_preserves_and_restores_original_provider(self):
        path = PLUGIN.main_config()
        path.parent.mkdir(parents=True)
        original = ('model = "gpt-test"\nmodel_provider = "gateway"\n\n'
                    '[model_providers.gateway]\nbase_url = "https://gateway.test/v1"\n')
        path.write_text(original)
        provider = {"base_url": "https://gateway.test/v1", "requires_openai_auth": False}
        PLUGIN.install_default_route(path, PLUGIN.routing_state(), 8791, provider)
        routed = path.read_text()
        self.assertIn('model_provider = "context-guru"', routed)
        self.assertIn('base_url = "http://127.0.0.1:8791/openai/v1"', routed)
        self.assertIn('[model_providers.gateway]', routed)
        self.assertTrue(PLUGIN.is_routed(path))
        self.assertEqual(PLUGIN.restore_default_route(PLUGIN.routing_state()), "restored")
        self.assertEqual(path.read_text(), original)

    def test_restore_does_not_overwrite_a_later_provider_choice(self):
        path = PLUGIN.main_config()
        path.parent.mkdir(parents=True)
        path.write_text('model_provider = "gateway"\n')
        PLUGIN.install_default_route(path, PLUGIN.routing_state(), 8791, {})
        path.write_text(path.read_text().replace('model_provider = "context-guru"',
                                                 'model_provider = "new-choice"'))
        self.assertEqual(PLUGIN.restore_default_route(PLUGIN.routing_state()),
                         "routing_already_changed")
        self.assertIn('model_provider = "new-choice"', path.read_text())
        self.assertNotIn(PLUGIN.CONFIG_MARKER, path.read_text())

    def test_release_binary_is_checksum_verified_and_installed_privately(self):
        payload = b"released proxy"
        archive_buffer = io.BytesIO()
        with tarfile.open(fileobj=archive_buffer, mode="w:gz") as bundle:
            info = tarfile.TarInfo("context-guru_1.2.3_linux_amd64/context-guru-proxy")
            info.size = len(payload)
            bundle.addfile(info, io.BytesIO(payload))
        archive = archive_buffer.getvalue()
        checksum = hashlib.sha256(archive).hexdigest()

        def fake_fetch(url):
            if url.endswith("checksums.txt"):
                return f"{checksum}  context-guru_1.2.3_linux_amd64.tar.gz\n".encode()
            return archive

        with mock.patch.object(PLUGIN, "release_platform", return_value=("linux", "amd64")), \
             mock.patch.object(PLUGIN, "fetch", side_effect=fake_fetch):
            path, version = PLUGIN.install_release_binary("v1.2.3")
        self.assertEqual(version, "v1.2.3")
        self.assertEqual(Path(path), PLUGIN.managed_binary().resolve())
        self.assertEqual(PLUGIN.managed_binary().read_bytes(), payload)
        self.assertTrue(os.access(path, os.X_OK))
        with mock.patch.object(PLUGIN.shutil, "which", return_value=None):
            self.assertEqual(PLUGIN.find_binary(), path)

    def test_release_binary_rejects_checksum_mismatch(self):
        with mock.patch.object(PLUGIN, "release_platform", return_value=("linux", "amd64")), \
             mock.patch.object(PLUGIN, "fetch", side_effect=[b"archive", b"0" * 64 + b"  context-guru_1.2.3_linux_amd64.tar.gz\n"]):
            with self.assertRaisesRegex(RuntimeError, "checksum mismatch"):
                PLUGIN.install_release_binary("v1.2.3")
        self.assertFalse(PLUGIN.managed_binary().exists())

    def test_release_binary_rejects_pre_codex_release(self):
        with mock.patch.object(PLUGIN, "fetch") as fetch:
            with self.assertRaisesRegex(RuntimeError, "predates Codex Responses support"):
                PLUGIN.install_release_binary("v0.3.4")
        fetch.assert_not_called()

    def test_update_check_is_read_only(self):
        args = type("Args", (), {"check": True, "install": False})()
        with mock.patch.object(PLUGIN, "latest_release_tag", return_value="v1.2.3"), \
             mock.patch.object(PLUGIN, "find_binary", return_value=None), \
             mock.patch.object(PLUGIN, "install_release_binary") as install:
            self.assertEqual(PLUGIN.update(args), 0)
        install.assert_not_called()

    def test_reset_dry_run_changes_nothing(self):
        PLUGIN.main_config().parent.mkdir(parents=True)
        PLUGIN.main_config().write_text('model_provider = "openai"\n')
        PLUGIN.install_default_route(PLUGIN.main_config(), PLUGIN.routing_state(), 8791, {})
        PLUGIN.write_proxy_config()
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
        self.assertTrue(PLUGIN.is_routed(PLUGIN.main_config()))
        self.assertTrue(PLUGIN.proxy_config().exists())
        self.assertTrue(record.exists())

    def test_reset_backup_is_private(self):
        PLUGIN.main_config().parent.mkdir(parents=True)
        PLUGIN.main_config().write_text('model_provider = "openai"\n')
        PLUGIN.install_default_route(PLUGIN.main_config(), PLUGIN.routing_state(), 8791, {})
        PLUGIN.write_proxy_config()
        PLUGIN.install_escape_hatch()
        completed = subprocess.run(
            ["sh", str(Path(__file__).with_name("reset.sh")), "--yes"],
            env=os.environ.copy(), text=True, capture_output=True,
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)
        backups = list((PLUGIN.state_dir() / "recovery").iterdir())
        self.assertEqual(len(backups), 1)
        self.assertEqual(backups[0].stat().st_mode & 0o777, 0o600)
        self.assertFalse(PLUGIN.is_routed(PLUGIN.main_config()))
        self.assertFalse(PLUGIN.proxy_config().exists())

    def test_reset_removes_managed_binary(self):
        binary = PLUGIN.managed_binary()
        binary.parent.mkdir(parents=True)
        binary.write_bytes(b"proxy")
        completed = subprocess.run(
            ["sh", str(Path(__file__).with_name("reset.sh")), "--yes"],
            env=os.environ.copy(), text=True, capture_output=True,
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertIn("binary_removed=true", completed.stdout)
        self.assertFalse(binary.exists())
        self.assertFalse(binary.parent.exists())

    def test_reset_preserves_managed_binary_when_recorded_process_is_not_owned(self):
        binary = PLUGIN.managed_binary()
        binary.parent.mkdir(parents=True)
        binary.write_bytes(b"proxy")
        PLUGIN.state_dir().joinpath("install.json").write_text(json.dumps({
            "pid": os.getpid(), "binary": "/definitely/not/this/test-process"
        }))
        completed = subprocess.run(
            ["sh", str(Path(__file__).with_name("reset.sh")), "--yes"],
            env=os.environ.copy(), text=True, capture_output=True,
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertIn("reason=process_not_owned", completed.stdout)
        self.assertNotIn("binary_removed=true", completed.stdout)
        self.assertTrue(binary.exists())
        self.assertTrue(PLUGIN.state_dir().joinpath("install.json").exists())

    def test_explicit_update_sets_upgrade_gate(self):
        args = type("Args", (), {"check": False, "install": True})()
        with mock.patch.object(PLUGIN, "latest_release_tag", return_value="v1.2.3"), \
             mock.patch.object(PLUGIN, "find_binary", return_value=None), \
             mock.patch.object(PLUGIN, "install_release_binary",
                               side_effect=RuntimeError("download failed")):
            self.assertEqual(PLUGIN.update(args), 1)

    def test_configure_preset_preserves_cache_and_restarts(self):
        PLUGIN.routing_state().parent.mkdir(parents=True)
        PLUGIN.routing_state().write_text("{}")
        PLUGIN.save_proxy_options("off", "none")
        args = type("Args", (), {"show": False, "preset": "high", "cache_strategy": None})()
        with mock.patch.object(PLUGIN, "restart_proxy", return_value=True) as restart:
            self.assertEqual(PLUGIN.configure(args), 0)
        self.assertEqual(PLUGIN.read_proxy_options(), ("high", "none"))
        restart.assert_called_once()

    def test_configure_cache_preserves_preset(self):
        PLUGIN.routing_state().parent.mkdir(parents=True)
        PLUGIN.routing_state().write_text("{}")
        PLUGIN.save_proxy_options("medium", "none")
        args = type("Args", (), {"show": False, "preset": None,
                                  "cache_strategy": "30-min-ping"})()
        with mock.patch.object(PLUGIN, "restart_proxy", return_value=True):
            self.assertEqual(PLUGIN.configure(args), 0)
        self.assertEqual(PLUGIN.read_proxy_options(), ("medium", "30-min-ping"))

    def test_configure_refuses_unmanaged_options(self):
        PLUGIN.routing_state().parent.mkdir(parents=True)
        PLUGIN.routing_state().write_text("{}")
        PLUGIN.proxy_config().write_text("preset: mine\n")
        args = type("Args", (), {"show": False, "preset": "off", "cache_strategy": None})()
        with mock.patch.object(PLUGIN, "restart_proxy") as restart:
            self.assertEqual(PLUGIN.configure(args), 2)
        restart.assert_not_called()

    def test_configure_show_is_read_only(self):
        PLUGIN.routing_state().parent.mkdir(parents=True)
        PLUGIN.routing_state().write_text("{}")
        PLUGIN.save_proxy_options("xhigh", "30-min-ping")
        before = PLUGIN.proxy_config().read_bytes()
        args = type("Args", (), {"show": True, "preset": None, "cache_strategy": None})()
        with mock.patch.object(PLUGIN, "restart_proxy") as restart:
            self.assertEqual(PLUGIN.configure(args), 0)
        self.assertEqual(PLUGIN.proxy_config().read_bytes(), before)
        restart.assert_not_called()

    def test_configure_ultrahigh_asks_for_a_probe(self):
        PLUGIN.routing_state().parent.mkdir(parents=True)
        PLUGIN.routing_state().write_text("{}")
        PLUGIN.save_proxy_options("xhigh", "30-min-ping")
        args = type("Args", (), {"show": False, "preset": "ultrahigh", "cache_strategy": None})()
        with mock.patch.object(PLUGIN, "restart_proxy", return_value=True), \
                mock.patch.object(PLUGIN, "read_record", return_value={"port": 8799}), \
                mock.patch.object(PLUGIN, "ask_defer_probe", return_value="asked") as ask, \
                mock.patch("sys.stdout", new_callable=io.StringIO) as out:
            self.assertEqual(PLUGIN.configure(args), 0)
        ask.assert_called_once_with(8799)
        self.assertIn("probe=asked", out.getvalue())
        args.preset = "high"
        with mock.patch.object(PLUGIN, "restart_proxy", return_value=True), \
                mock.patch.object(PLUGIN, "ask_defer_probe") as ask, \
                mock.patch("sys.stdout", new_callable=io.StringIO):
            PLUGIN.configure(args)
        ask.assert_not_called()

    def test_ask_defer_probe_returns_at_once_without_a_proxy(self):
        start = time.monotonic()
        with mock.patch.object(PLUGIN, "healthy", return_value=True):
            self.assertEqual(PLUGIN.ask_defer_probe(1), "unavailable")
        self.assertLess(time.monotonic() - start, 2)


    def real_table(self, binary):
        with mock.patch.object(PLUGIN, "load_strategy_table", REAL_LOAD_STRATEGY_TABLE):
            return PLUGIN.load_strategy_table(binary)

    def test_reads_the_table_from_the_binary(self):
        binary = write_script(Path(self.temp.name) / "ok", "cat <<'EOF'\n" + json.dumps(TABLE) + "\nEOF\n")
        self.assertEqual(self.real_table(binary), TABLE)

    def test_old_binary_that_serves_gets_the_update_error_within_the_timeout(self):
        # v0.7.0 given `strategies` starts serving and prints no JSON.
        binary = write_script(Path(self.temp.name) / "old", "echo 'listening on 127.0.0.1:0'\nexec sleep 30\n")
        start = time.monotonic()
        with mock.patch.object(PLUGIN, "STRATEGIES_TIMEOUT", 1), \
             self.assertRaisesRegex(RuntimeError, "update the proxy to v0.7.1 or later"):
            self.real_table(binary)
        self.assertLess(time.monotonic() - start, 5)

    def test_old_binary_that_exits_with_an_error_gets_the_update_error(self):
        binary = write_script(Path(self.temp.name) / "err", "echo 'flag provided but not defined' >&2\nexit 2\n")
        with self.assertRaisesRegex(RuntimeError, "update the proxy to v0.7.1 or later"):
            self.real_table(binary)
        with self.assertRaisesRegex(RuntimeError, "update the proxy to v0.7.1 or later"):
            self.real_table(None)

    def test_strategies_command_gets_a_loopback_port_and_no_config(self):
        out = Path(self.temp.name) / "env.txt"
        binary = write_script(Path(self.temp.name) / "env",
                              f'echo "$LISTEN_ADDR|$CONFIG|$1" > {out}\ncat <<\'EOF\'\n' + json.dumps(TABLE) + "\nEOF\n")
        with mock.patch.dict(os.environ, {"CONFIG": "/etc/mine.yaml"}):
            self.real_table(binary)
        self.assertEqual(out.read_text().strip(), "127.0.0.1:0||strategies")

    def test_a_missing_preset_is_a_clear_error(self):
        table = dict(TABLE, presets=["off", "conservative", "medium", "high", "xhigh"])
        binary = write_script(Path(self.temp.name) / "short", "cat <<'EOF'\n" + json.dumps(table) + "\nEOF\n")
        with self.assertRaisesRegex(RuntimeError, "does not know preset.*ultrahigh"):
            self.real_table(binary)

    def test_only_openai_and_any_strategies_are_offered(self):
        self.assertEqual([r["name"] for r in PLUGIN.offered_strategies(TABLE)], ["none", "30-min-ping"])
        self.assertEqual(PLUGIN.resolve_strategy(TABLE, "none"), "none")
        with self.assertRaisesRegex(RuntimeError, "not offered for Codex"):
            PLUGIN.resolve_strategy(TABLE, "auto")
        with self.assertRaisesRegex(RuntimeError, "not offered for Codex"):
            PLUGIN.resolve_strategy(TABLE, "5-min-ping")

    def test_both_write_forms_read_back(self):
        PLUGIN.proxy_config().parent.mkdir(parents=True)
        cases = [
            ("cache:\n  keepalive: true\n", "30-min-ping"),
            ("cache:\n  keepalive: false\n", "none"),
            ("cache:\n  strategy: 30-min-ping\n", "30-min-ping"),
            ("cache:\n  strategy: none\n", "none"),
            ("cache:\n  strategy: \"none\"\n", "none"),
            ("", "30-min-ping"),
        ]
        for body, want in cases:
            PLUGIN.proxy_config().write_text(f"{PLUGIN.CONFIG_MARKER}\npreset: high\n{body}")
            self.assertEqual(PLUGIN.read_proxy_options(), ("high", want), body)
        for name in ("none", "30-min-ping"):
            PLUGIN.save_proxy_options("medium", name)
            self.assertEqual(PLUGIN.read_proxy_options(), ("medium", name))

    def test_an_old_keepalive_file_keeps_its_meaning_through_configure(self):
        PLUGIN.routing_state().parent.mkdir(parents=True)
        PLUGIN.routing_state().write_text("{}")
        PLUGIN.proxy_config().write_text(f"{PLUGIN.CONFIG_MARKER}\npreset: high\ncache:\n  keepalive: false\n")
        args = type("Args", (), {"show": False, "preset": "medium", "cache_strategy": None})()
        with mock.patch.object(PLUGIN, "restart_proxy", return_value=True):
            self.assertEqual(PLUGIN.configure(args), 0)
        self.assertEqual(PLUGIN.read_proxy_options(), ("medium", "none"))

    def test_there_is_one_preset_list(self):
        source = MODULE_PATH.read_text()
        self.assertEqual(source.count('"conservative"'), 1)
        self.assertEqual(PLUGIN.PRESETS, ("off", "conservative", "medium", "high", "xhigh", "ultrahigh"))
        with self.assertRaisesRegex(RuntimeError, "unknown preset"):
            PLUGIN.save_proxy_options("house", "none")

    def test_configure_show_lists_the_choices_from_the_proxy(self):
        PLUGIN.routing_state().parent.mkdir(parents=True)
        PLUGIN.routing_state().write_text("{}")
        args = type("Args", (), {"show": True, "preset": None, "cache_strategy": None})()
        with mock.patch("sys.stdout", new_callable=io.StringIO) as out:
            self.assertEqual(PLUGIN.configure(args), 0)
        text = out.getvalue()
        self.assertIn("preset=off", text)
        self.assertIn("cache_strategy=30-min-ping", text)
        self.assertIn('"name":"none"', text)
        self.assertIn('"desc":"openai pings"', text)
        self.assertNotIn("anthropic pings", text)

    def test_configure_with_an_old_binary_says_to_update(self):
        PLUGIN.routing_state().parent.mkdir(parents=True)
        PLUGIN.routing_state().write_text("{}")
        args = type("Args", (), {"show": True, "preset": None, "cache_strategy": None})()
        error = RuntimeError("x cannot list cache strategies; update the proxy to v0.7.1 or later")
        with mock.patch.object(PLUGIN, "load_strategy_table", side_effect=error), \
             mock.patch("sys.stdout", new_callable=io.StringIO) as out:
            self.assertEqual(PLUGIN.configure(args), 2)
        self.assertIn("result=refused", out.getvalue())
        self.assertIn("v0.7.1", out.getvalue())

    def test_skills_read_the_choices_from_show(self):
        skills = MODULE_PATH.parent.parent / "skills"
        for name in ("cache-strategy-picker", "preset-picker"):
            self.assertIn("configure --show", (skills / name / "SKILL.md").read_text())


    def test_setup_on_a_running_proxy_checks_the_recorded_binary(self):
        # The record names an old binary. A new binary comes first in find_binary. The proxy runs.
        # Every restart uses the old binary, so setup must refuse and must not write the new form.
        PLUGIN.state_dir().mkdir(parents=True)
        (PLUGIN.state_dir() / "install.json").write_text(json.dumps({"port": 8791, "binary": "/old/proxy"}))
        PLUGIN.main_config().parent.mkdir(parents=True)
        PLUGIN.main_config().write_text('model_provider = "openai"\n')
        args = type("Args", (), {"plan": False, "i_consent_to_traffic_interception": True})()

        def load(binary):
            if binary == "/old/proxy":
                raise RuntimeError("cannot list cache strategies; update the proxy to v0.7.1 or later")
            return TABLE
        with mock.patch.object(PLUGIN, "find_binary", return_value="/new/proxy"), \
             mock.patch.object(PLUGIN, "healthy", return_value=True), \
             mock.patch.object(PLUGIN, "load_strategy_table", side_effect=load), \
             mock.patch.object(PLUGIN, "install_default_route") as route, \
             mock.patch("sys.stdout", new_callable=io.StringIO) as out:
            self.assertEqual(PLUGIN.setup(args), 2)
        self.assertIn("result=strategies_unavailable", out.getvalue())
        self.assertIn("v0.7.1", out.getvalue())
        self.assertFalse(PLUGIN.proxy_config().exists())
        route.assert_not_called()

    def test_setup_on_a_running_proxy_with_a_new_recorded_binary_writes_the_file(self):
        PLUGIN.state_dir().mkdir(parents=True)
        (PLUGIN.state_dir() / "install.json").write_text(json.dumps({"port": 8791, "binary": "/new/proxy"}))
        PLUGIN.main_config().parent.mkdir(parents=True)
        PLUGIN.main_config().write_text('model_provider = "openai"\n')
        args = type("Args", (), {"plan": False, "i_consent_to_traffic_interception": True})()
        with mock.patch.object(PLUGIN, "find_binary", return_value="/new/proxy"), \
             mock.patch.object(PLUGIN, "healthy", return_value=True), \
             mock.patch.object(PLUGIN, "install_default_route"), \
             mock.patch.object(PLUGIN, "install_escape_hatch", return_value=Path("/reset")), \
             mock.patch("sys.stdout", new_callable=io.StringIO) as out:
            self.assertEqual(PLUGIN.setup(args), 0)
        self.assertIn("result=already_running", out.getvalue())
        self.assertIn("strategy: 30-min-ping", PLUGIN.proxy_config().read_text())

    def test_setup_reports_a_write_error_as_facts_not_a_traceback(self):
        args = type("Args", (), {"plan": False, "i_consent_to_traffic_interception": True})()
        with mock.patch.object(PLUGIN, "find_binary", return_value="/new/proxy"), \
             mock.patch.object(PLUGIN.Path, "mkdir", side_effect=PermissionError("read-only")), \
             mock.patch("sys.stdout", new_callable=io.StringIO) as out:
            self.assertEqual(PLUGIN.setup(args), 2)
        self.assertIn("result=config_conflict", out.getvalue())

    def test_configure_show_ignores_cache_strategy(self):
        PLUGIN.routing_state().parent.mkdir(parents=True)
        PLUGIN.routing_state().write_text("{}")
        PLUGIN.save_proxy_options("high", "30-min-ping")
        before = PLUGIN.proxy_config().read_bytes()
        args = type("Args", (), {"show": True, "preset": "off", "cache_strategy": "none"})()
        with mock.patch.object(PLUGIN, "restart_proxy") as restart, \
             mock.patch("sys.stdout", new_callable=io.StringIO) as out:
            self.assertEqual(PLUGIN.configure(args), 0)
        self.assertIn("cache_strategy=30-min-ping", out.getvalue())
        self.assertIn("preset=high", out.getvalue())
        self.assertEqual(PLUGIN.proxy_config().read_bytes(), before)
        restart.assert_not_called()


    def test_setup_with_the_proxy_down_keeps_the_record_and_the_file_in_step(self):
        # Old recorded binary, new binary first in find_binary, proxy down, setup fails after the
        # write. A later ensure runs the recorded binary: it must be the one that can read the file.
        PLUGIN.state_dir().mkdir(parents=True)
        (PLUGIN.state_dir() / "install.json").write_text(
            json.dumps({"port": 8791, "binary": "/old/proxy"}))
        PLUGIN.main_config().parent.mkdir(parents=True)
        PLUGIN.main_config().write_text('model_provider = "openai"\n')
        args = type("Args", (), {"plan": False, "i_consent_to_traffic_interception": True})()
        with mock.patch.object(PLUGIN, "find_binary", return_value="/new/proxy"), \
             mock.patch.object(PLUGIN, "healthy", return_value=False), \
             mock.patch.object(PLUGIN, "first_free_port", return_value=8791), \
             mock.patch.object(PLUGIN, "start_proxy", return_value=None), \
             mock.patch("sys.stdout", new_callable=io.StringIO):
            self.assertEqual(PLUGIN.setup(args), 1)
        self.assertIn("strategy: 30-min-ping", PLUGIN.proxy_config().read_text())
        self.assertEqual(PLUGIN.read_record()["binary"], "/new/proxy")
        # ensure now starts the binary that validated the file, never the old one.
        with mock.patch.object(PLUGIN, "is_routed", return_value=True), \
             mock.patch.object(PLUGIN, "healthy", return_value=False), \
             mock.patch.object(PLUGIN, "start_proxy", return_value=mock.Mock(pid=1)) as start, \
             mock.patch("sys.stdout", new_callable=io.StringIO):
            PLUGIN.ensure(None)
        self.assertEqual(start.call_args[0][0], "/new/proxy")

    def test_setup_with_the_proxy_down_and_a_new_binary_that_cannot_list_writes_nothing(self):
        PLUGIN.state_dir().mkdir(parents=True)
        (PLUGIN.state_dir() / "install.json").write_text(
            json.dumps({"port": 8791, "binary": "/old/proxy"}))
        args = type("Args", (), {"plan": False, "i_consent_to_traffic_interception": True})()
        error = RuntimeError("cannot list cache strategies; update the proxy to v0.7.1 or later")
        with mock.patch.object(PLUGIN, "find_binary", return_value="/other/proxy"), \
             mock.patch.object(PLUGIN, "healthy", return_value=False), \
             mock.patch.object(PLUGIN, "load_strategy_table", side_effect=error), \
             mock.patch("sys.stdout", new_callable=io.StringIO):
            self.assertEqual(PLUGIN.setup(args), 2)
        self.assertFalse(PLUGIN.proxy_config().exists())
        self.assertEqual(PLUGIN.read_record()["binary"], "/old/proxy")


if __name__ == "__main__":
    unittest.main()
