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
import unittest
import urllib.error
from unittest import mock

MODULE_PATH = Path(__file__).with_name("codex_plugin.py")
SPEC = importlib.util.spec_from_file_location("codex_plugin", MODULE_PATH)
PLUGIN = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PLUGIN)


class CodexPluginTest(unittest.TestCase):
    def test_session_hook_is_advisory_only(self):
        hooks = json.loads((MODULE_PATH.parent.parent / "hooks.json").read_text())["hooks"]
        self.assertEqual(list(hooks), ["SessionStart"])
        command = hooks["SessionStart"][0]["hooks"][0]["command"]
        self.assertIn("session-start-hook", command)

    def test_plugin_wires_onboarding_and_doctor_skills(self):
        root = MODULE_PATH.parent.parent
        manifest = json.loads((root / ".codex-plugin/plugin.json").read_text())
        self.assertTrue((root / manifest["onboardingSkill"]).is_file())
        self.assertTrue((root / "skills/doctor/SKILL.md").is_file())

    def test_skills_cover_sandbox_boundaries(self):
        skills = MODULE_PATH.parent.parent / "skills"
        for name in ("status", "insights", "insights-capabilities",
                     "insights-components", "insights-idle"):
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
        self.assertIn("stdout verbatim", status)
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

    def tearDown(self):
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
        text = PLUGIN.profile().read_text()
        self.assertIn('experimental_bearer_token = "test-token"', text)
        self.assertIn("requires_openai_auth = false", text)
        self.assertEqual(PLUGIN.profile().stat().st_mode & 0o777, 0o600)
        command = PLUGIN.proxy_command("/proxy", 8791, provider["base_url"])
        self.assertEqual(command[3:5], ["--config", str(PLUGIN.proxy_config())])
        self.assertEqual(command[-2:], ["--openai-upstream", "https://gateway.example.test"])

    def test_proxy_config_uses_off_preset_and_cache_keepalive(self):
        path = PLUGIN.write_proxy_config()
        self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        self.assertEqual(path.read_text(),
                         f"{PLUGIN.CONFIG_MARKER}\npreset: off\ncache:\n  keepalive: true\n")

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
        args = type("Args", (), {"plan": False, "port": 0, "i_consent_to_traffic_interception": True})()
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

    def test_status_stats_prints_exact_response_body(self):
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
        self.assertEqual(stdout.getvalue(), body.decode())
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
        self.assertEqual(routed, original)  # config.toml is not touched at all
        profile = PLUGIN.profile().read_text()
        self.assertIn('model_provider = "context-guru"', profile)
        self.assertFalse(PLUGIN.is_default_routed(path))
        self.assertIn('base_url = "http://127.0.0.1:8791/openai/v1"', profile)
        self.assertTrue(PLUGIN.is_routed(path))
        self.assertEqual(PLUGIN.restore_default_route(PLUGIN.routing_state()), "restored")
        self.assertEqual(path.read_text(), original)
        self.assertFalse(PLUGIN.profile().exists())

    def test_install_migrates_a_legacy_default_provider_lock_in(self):
        path = PLUGIN.main_config()
        path.parent.mkdir(parents=True)
        original = 'model_provider = "gateway"\n'
        path.write_text(original)
        state = PLUGIN.routing_state()
        state.parent.mkdir(parents=True)
        state.write_text(json.dumps({"config": str(path), "existed": True,
                                     "original_model_provider_line": 'model_provider = "gateway"'}))
        path.write_text('model_provider = "context-guru"\n' + "# context-guru: begin managed provider" + "\n"
                        "[model_providers.context-guru]\nbase_url = \"http://127.0.0.1:1/openai/v1\"\n"
                        + "# context-guru: end managed provider" + "\n")
        self.assertTrue(PLUGIN.is_default_routed(path))
        PLUGIN.install_default_route(path, state, 8791, {})
        self.assertFalse(PLUGIN.is_default_routed(path))
        self.assertIn('model_provider = "gateway"', path.read_text())
        self.assertTrue(PLUGIN.is_routed(path))

    def test_launch_falls_back_to_plain_codex_when_proxy_cannot_start(self):
        path = PLUGIN.main_config()
        path.parent.mkdir(parents=True)
        PLUGIN.install_default_route(path, PLUGIN.routing_state(), 8791, {})
        args = type("Args", (), {"codex_args": ["--", "resume"]})()
        with mock.patch.dict(os.environ, {"CODEX_BIN": "/bin/codex"}), \
             mock.patch.object(PLUGIN, "revive", return_value=False), \
             mock.patch.object(PLUGIN.os, "execv") as execv, \
             mock.patch("sys.stderr", io.StringIO()) as err:
            PLUGIN.launch(args)
        execv.assert_called_once_with("/bin/codex", ["/bin/codex", "resume"])
        self.assertIn("NOT routed", err.getvalue())

    def test_launch_routes_through_profile_when_proxy_is_up(self):
        path = PLUGIN.main_config()
        path.parent.mkdir(parents=True)
        PLUGIN.install_default_route(path, PLUGIN.routing_state(), 8791, {})
        args = type("Args", (), {"codex_args": ["--", "hi"]})()
        with mock.patch.dict(os.environ, {"CODEX_BIN": "/bin/codex"}), \
             mock.patch.object(PLUGIN, "revive", return_value=True), \
             mock.patch.object(PLUGIN.os, "execv") as execv:
            PLUGIN.launch(args)
        execv.assert_called_once_with("/bin/codex", ["/bin/codex", "--profile", "context-guru", "hi"])

    def test_session_hook_warns_only_on_a_real_problem(self):
        path = PLUGIN.main_config()
        path.parent.mkdir(parents=True)
        PLUGIN.install_default_route(path, PLUGIN.routing_state(), 8791, {})
        PLUGIN.state_dir().joinpath("install.json").write_text('{"port": 8791, "binary": "/p"}')
        with mock.patch.dict(os.environ, {"CONTEXT_GURU_ROUTED": "1"}), \
             mock.patch.object(PLUGIN, "revive", return_value=True), \
             mock.patch("sys.stdout", io.StringIO()) as out:
            PLUGIN.session_start_hook(None)
        self.assertEqual(out.getvalue(), "")
        with mock.patch.dict(os.environ, {"CONTEXT_GURU_ROUTED": "1"}), \
             mock.patch.object(PLUGIN, "revive", return_value=False), \
             mock.patch("sys.stdout", io.StringIO()) as out:
            PLUGIN.session_start_hook(None)
        self.assertIn("down", json.loads(out.getvalue())["systemMessage"])

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
        self.assertEqual(len(backups), 2)  # the profile (holds the bearer token) and config.toml
        self.assertTrue(all(b.stat().st_mode & 0o777 == 0o600 for b in backups))
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
        PLUGIN.save_proxy_options("off", False)
        args = type("Args", (), {"show": False, "preset": "high", "cache_strategy": None})()
        with mock.patch.object(PLUGIN, "restart_proxy", return_value=True) as restart:
            self.assertEqual(PLUGIN.configure(args), 0)
        self.assertEqual(PLUGIN.read_proxy_options(), ("high", False))
        restart.assert_called_once()

    def test_configure_cache_preserves_preset(self):
        PLUGIN.routing_state().parent.mkdir(parents=True)
        PLUGIN.routing_state().write_text("{}")
        PLUGIN.save_proxy_options("medium", False)
        args = type("Args", (), {"show": False, "preset": None,
                                  "cache_strategy": "30-min-ping"})()
        with mock.patch.object(PLUGIN, "restart_proxy", return_value=True):
            self.assertEqual(PLUGIN.configure(args), 0)
        self.assertEqual(PLUGIN.read_proxy_options(), ("medium", True))

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
        PLUGIN.save_proxy_options("xhigh", True)
        before = PLUGIN.proxy_config().read_bytes()
        args = type("Args", (), {"show": True, "preset": None, "cache_strategy": None})()
        with mock.patch.object(PLUGIN, "restart_proxy") as restart:
            self.assertEqual(PLUGIN.configure(args), 0)
        self.assertEqual(PLUGIN.proxy_config().read_bytes(), before)
        restart.assert_not_called()


if __name__ == "__main__":
    unittest.main()
