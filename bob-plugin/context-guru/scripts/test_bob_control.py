"""Tests for phase P4 of the Bob plugin: configure, update and doctor. Temp HOME and fakes only.

Run: python3 -m unittest discover -s bob-plugin/context-guru/scripts -p 'test_*.py'
"""

import contextlib
import hashlib
import http.server
import io
import json
import os
from pathlib import Path
import plistlib
import shutil
import stat
import sys
import tarfile
import tempfile
import threading
import time
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
from test_bob_plugin import (  # noqa: E402
    HOOKS, PLUGIN, PLUGIN_ROOT, ROUTE, FakeProxy, TempHome, dead_port, fake_binary)

LOSSLESS = PLUGIN.CONFIG_MARKER + "\npipeline: [format, toon, cachesplit]\n"

# The shape of `context-guru-proxy strategies`. It has rows for other routes, so a test sees that
# Bob offers only the rows that fit its own route.
TABLE = {
    "default": "auto", "aliases": {"5-min-ping": "auto"},
    "presets": ["off", "conservative", "medium", "high", "xhigh"],
    "strategies": [
        {"name": "none", "spends": False, "desc": "no pings", "providers": ["any"], "cache": {}},
        {"name": "auto", "spends": True, "desc": "anthropic pings", "providers": ["anthropic"],
         "cache": {"keepalive": True}},
        {"name": "1-hour-head", "spends": True, "desc": "anthropic head",
         "providers": ["anthropic"], "cache": {"keepalive": True}},
        {"name": "30-min-ping", "spends": True, "desc": "openai pings", "providers": ["openai"],
         "cache": {"keepalive": True}},
    ],
}


def write_script(path, body):
    path.write_text("#!/bin/sh\n" + body)
    path.chmod(0o755)
    return str(path)


def table_binary(folder, table=TABLE):
    """A fake proxy that prints `table` for `strategies`."""
    return write_script(Path(folder) / "strategies-proxy",
                        "cat <<'EOF'\n" + json.dumps(table) + "\nEOF\n")


def with_row(name, providers):
    row = {"name": name, "spends": True, "desc": "new pings", "providers": providers, "cache": {}}
    return dict(TABLE, strategies=TABLE["strategies"] + [row])


class Installed(TempHome):
    """A temp HOME with a finished setup against a FakeProxy."""

    def setUp(self):
        super().setUp()
        self.rc.write_bytes(b"x\n")
        self.proxy = FakeProxy().__enter__()
        self.addCleanup(self.proxy.__exit__, None, None, None)
        with mock.patch.object(PLUGIN, "find_binary", return_value="/opt/cg/context-guru-proxy"), \
             mock.patch.object(PLUGIN, "first_free_port", return_value=self.proxy.port):
            code, out, _ = self.run_main("setup", "--i-consent-to-traffic-interception")
        self.assertEqual(code, 0, out)
        self.port = self.proxy.port
        self.config = PLUGIN.proxy_config(self.port)

    def bootstraps(self, since=0):
        return sum(c.startswith("bootstrap") for c in self.launchctl_calls()[since:])

    def record_binary(self, path):
        record = PLUGIN.read_record()
        record["binary"] = str(path)
        PLUGIN.write_record(record)

    def record_table_binary(self, table=TABLE):
        """Point the install record at a fake proxy that lists `table` as its strategies."""
        folder = self.root / "tablebin"
        folder.mkdir(exist_ok=True)
        self.record_binary(table_binary(folder, table))


# ---------------------------------------------------------------------------- configure

class ConfigureTest(Installed):
    def setUp(self):
        super().setUp()
        self.record_table_binary()

    def test_show_reports_lossless_and_none_and_changes_nothing(self):
        before = self.tree()
        code, out, _ = self.run_main("configure", "--show")
        self.assertEqual(code, 0, out)
        self.assertIn("preset=lossless\n", out)
        self.assertIn("cache_strategy=none\n", out)
        self.assertIn("allowed_presets=off,lossless\n", out)
        self.assertIn("allowed_cache_strategies=none\n", out)
        self.assertEqual(self.tree(), before)

    def test_preset_off_writes_passthrough_and_restarts(self):
        calls = len(self.launchctl_calls())
        code, out, _ = self.run_main("configure", "--preset", "off")
        self.assertEqual(code, 0, out)
        self.assertIn("result=configured\npreset=off\n", out)
        self.assertIn("proxy_restarted=true\n", out)
        self.assertEqual(self.config.read_text(), PLUGIN.CONFIG_MARKER + "\npreset: off\n")
        self.assertEqual(self.bootstraps(calls), 1)
        self.assertEqual(self.run_main("configure", "--show")[1].split("\n")[1], "preset=off")
        self.assertEqual(self.run_main("configure", "--preset", "lossless")[0], 0)
        self.assertEqual(self.config.read_text(), LOSSLESS)

    def test_lossy_presets_are_refused_and_nothing_changes(self):
        for name in ("conservative", "medium", "high", "xhigh", "coding", "house"):
            with self.subTest(preset=name):
                before, calls = self.tree(), len(self.launchctl_calls())
                code, out, _ = self.run_main("configure", "--preset", name)
                self.assertEqual(code, 2)
                self.assertIn("result=refused\nreason=preset_not_allowed_for_bob\n", out)
                self.assertIn(f"{name} can drop content that Bob cannot get back yet. Only off "
                              "and lossless are allowed for Bob until reversibility is proven.",
                              out)
                self.assertEqual(self.tree(), before)
                self.assertEqual(len(self.launchctl_calls()), calls)

    def test_keepalive_strategies_are_refused_with_the_reason(self):
        for name in ("5-min-ping", "1-hour-head", "30-min-ping"):
            with self.subTest(strategy=name):
                before = self.tree()
                code, out, _ = self.run_main("configure", "--cache-strategy", name)
                self.assertEqual(code, 2)
                self.assertIn("reason=cache_strategy_not_allowed_for_bob", out)
                self.assertIn("chat/completions", out)
                self.assertIn("SessionEnd", out)
                self.assertEqual(self.tree(), before)
        self.assertEqual(self.run_main("configure", "--cache-strategy", "none")[0], 0)

    def test_the_preset_allow_list_is_a_single_constant(self):
        self.assertEqual(PLUGIN.BOB_PRESETS, {"off": "preset: off\n",
                                              "lossless": "pipeline: [format, toon, cachesplit]\n"})

    def test_show_lists_only_strategies_that_fit_the_route_and_the_plugin_applies(self):
        self.record_table_binary(with_row("chat-ping", ["openai-chat"]))
        code, out, _ = self.run_main("configure", "--show")
        self.assertEqual(code, 0, out)
        self.assertIn("allowed_cache_strategies=none\n", out)

    def test_a_strategy_the_plugin_cannot_apply_is_refused_and_nothing_changes(self):
        self.record_table_binary(with_row("chat-ping", ["openai-chat"]))
        before, calls = self.tree(), len(self.launchctl_calls())
        code, out, _ = self.run_main("configure", "--cache-strategy", "chat-ping")
        self.assertEqual(code, 2)
        self.assertIn("result=refused\nreason=cache_strategy_not_supported_by_plugin\n", out)
        self.assertIn("cache_strategy=chat-ping\n", out)
        self.assertEqual(self.tree(), before)
        self.assertEqual(len(self.launchctl_calls()), calls)

    def old_binary(self):
        folder = self.root / "oldbin"
        folder.mkdir()
        self.record_binary(write_script(folder / "old",
                                        "echo 'flag provided but not defined' >&2\nexit 2\n"))

    def test_show_on_an_unreadable_table_still_reports_the_preset(self):
        self.old_binary()
        before = self.tree()
        code, out, err = self.run_main("configure", "--show")
        self.assertEqual(code, 0, out)
        self.assertIn("result=ok\npreset=lossless\ncache_strategy=none\n"
                      "allowed_presets=off,lossless\n", out)
        self.assertIn("cache_strategy_error=strategy_table_unreadable\n", out)
        self.assertIn("cache_strategy_detail=", out)
        self.assertIn("update the proxy to v0.7.1 or later", out)
        self.assertNotIn("allowed_cache_strategies", out)
        self.assertEqual(err, "")
        self.assertEqual(self.tree(), before)

    def test_an_unreadable_table_is_a_clean_error_and_changes_nothing(self):
        self.old_binary()
        before = self.tree()
        code, out, err = self.run_main("configure", "--cache-strategy", "30-min-ping")
        self.assertEqual(code, 1)
        self.assertIn("result=error\nreason=strategy_table_unreadable\n", out)
        self.assertIn("update the proxy to v0.7.1 or later", out)
        self.assertEqual(err, "")
        self.assertEqual(self.tree(), before)

    def test_cache_strategy_none_does_not_read_the_table(self):
        self.old_binary()
        code, out, _ = self.run_main("configure", "--preset", "off", "--cache-strategy", "none")
        self.assertEqual(code, 0, out)
        self.assertIn("result=configured\npreset=off\n", out)
        self.assertEqual(self.config.read_text(), PLUGIN.CONFIG_MARKER + "\npreset: off\n")

    def test_a_preset_change_does_not_read_the_table(self):
        self.record_binary("/nonexistent/context-guru-proxy")
        code, out, _ = self.run_main("configure", "--preset", "off")
        self.assertEqual(code, 0, out)
        self.assertIn("result=configured\npreset=off\n", out)

    def test_an_unmanaged_config_is_refused(self):
        self.config.write_text("preset: xhigh\n")
        code, out, _ = self.run_main("configure", "--preset", "off")
        self.assertEqual(code, 2)
        self.assertIn("unmanaged proxy config", out)
        self.assertEqual(self.config.read_text(), "preset: xhigh\n")

    def test_a_restart_failure_restores_the_old_config_and_service(self):
        plist = PLUGIN.launchd_plist().read_bytes()
        calls = len(self.launchctl_calls())
        with mock.patch.object(PLUGIN, "wait_healthy", side_effect=[False, True]):
            code, out, _ = self.run_main("configure", "--preset", "off")
        self.assertEqual(code, 1)
        self.assertIn("result=restart_failed", out)
        self.assertIn("rolled_back=true", out)
        self.assertEqual(self.config.read_text(), LOSSLESS)
        self.assertEqual(PLUGIN.launchd_plist().read_bytes(), plist)
        self.assertEqual(self.bootstraps(calls), 2)

    def test_a_failed_restore_says_so(self):
        with mock.patch.object(PLUGIN, "wait_healthy", return_value=False):
            code, out, _ = self.run_main("configure", "--preset", "off")
        self.assertEqual(code, 1)
        self.assertIn("rolled_back=false", out)
        self.assertIn("fix=", out)
        self.assertEqual(self.config.read_text(), LOSSLESS)

    def test_setup_again_keeps_the_chosen_preset(self):
        self.assertEqual(self.run_main("configure", "--preset", "off")[0], 0)
        with mock.patch.object(PLUGIN, "find_binary", return_value="/opt/cg/context-guru-proxy"):
            self.assertEqual(self.run_main("setup", "--i-consent-to-traffic-interception")[0], 0)
        self.assertEqual(self.config.read_text(), PLUGIN.CONFIG_MARKER + "\npreset: off\n")


class StrategyTableTest(unittest.TestCase):
    """The reader of `context-guru-proxy strategies` and the Bob offer. Fake binaries only."""

    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.dir = Path(temp.name)

    def test_reads_the_table_from_the_binary(self):
        self.assertEqual(PLUGIN.load_strategy_table(table_binary(self.dir)), TABLE)

    def test_bob_offers_exactly_none_from_a_table_with_other_routes(self):
        self.assertEqual(PLUGIN.offered_strategies(TABLE), ["none"])

    def test_the_offer_stays_none_when_the_proxy_adds_a_strategy_for_another_route(self):
        for provider in ("openai", "anthropic"):
            with self.subTest(provider=provider):
                table = with_row(f"new-{provider}-ping", [provider])
                self.assertEqual(PLUGIN.offered_strategies(table), ["none"])

    def test_a_strategy_tagged_openai_chat_is_offered(self):
        table = with_row("chat-ping", ["openai", "openai-chat"])
        self.assertEqual(PLUGIN.offered_strategies(table), ["none", "chat-ping"])

    def test_the_update_hint_covers_a_proxy_the_plugin_did_not_install(self):
        with self.assertRaises(RuntimeError) as caught:
            PLUGIN.load_strategy_table(None)
        hint = str(caught.exception)
        self.assertIn("run the context-guru-update skill", hint)
        self.assertIn("for a proxy the plugin did not install", hint)
        self.assertIn("remove it so setup downloads the latest release", hint)

    def test_old_binary_that_serves_gets_the_update_error_within_the_timeout(self):
        # v0.7.0 given `strategies` starts serving and prints no JSON.
        binary = write_script(self.dir / "old", "echo 'listening on 127.0.0.1:0'\nexec sleep 30\n")
        start = time.monotonic()
        with mock.patch.object(PLUGIN, "STRATEGIES_TIMEOUT", 1), \
             self.assertRaisesRegex(RuntimeError, "update the proxy to v0.7.1 or later"):
            PLUGIN.load_strategy_table(binary)
        self.assertLess(time.monotonic() - start, 5)

    def test_bad_output_is_the_update_error(self):
        bodies = {
            "exit": "echo 'flag provided but not defined' >&2\nexit 2\n",
            "text": "echo 'not json'\n",
            "utf8": "printf '\\377\\376 not utf8'\n",
            "list": "echo '[1, 2]'\n",
            "norows": "echo '{\"strategies\": []}'\n",
            "badrow": "echo '{\"strategies\": [{\"name\": \"none\"}]}'\n",
            "nonzero": "cat <<'EOF'\n" + json.dumps(TABLE) + "\nEOF\nexit 1\n",
        }
        for name, body in bodies.items():
            with self.subTest(output=name):
                with self.assertRaisesRegex(RuntimeError, "update the proxy to v0.7.1 or later"):
                    PLUGIN.load_strategy_table(write_script(self.dir / name, body))
        with self.assertRaisesRegex(RuntimeError, "no proxy binary found"):
            PLUGIN.load_strategy_table(None)

    def test_strategies_command_gets_a_loopback_port_and_no_config(self):
        out = self.dir / "env.txt"
        binary = write_script(self.dir / "env", f'echo "$LISTEN_ADDR|$CONFIG|$1" > {out}\n'
                              "cat <<'EOF'\n" + json.dumps(TABLE) + "\nEOF\n")
        with mock.patch.dict(os.environ, {"CONFIG": "/etc/mine.yaml"}):
            PLUGIN.load_strategy_table(binary)
        self.assertEqual(out.read_text().strip(), "127.0.0.1:0||strategies")

    def test_the_probe_scrubs_state_settings_and_runs_in_a_scratch_directory(self):
        out = self.dir / "probe.txt"
        names = "DASHBOARD|SESSION_STATE|CONTROL_DB|UPSTREAMS|STORE|IDLE_EXIT|CG_RECORD|ARCHIVE_"
        binary = write_script(
            self.dir / "probe",
            f'{{ env | grep -E "^({names})"; echo "PWD=$(pwd)"; }} > {out}\n'
            "cat <<'EOF'\n" + json.dumps(TABLE) + "\nEOF\n")
        parent = {"DASHBOARD": "1", "DASHBOARD_DB": "/x", "DASHBOARD_CONTENT": "1",
                  "SESSION_STATE": "1", "CONTROL_DB": "/c", "UPSTREAMS": "/u",
                  "CG_RECORD_DIR": "/r", "STORE": "/s", "IDLE_EXIT": "1s", "ARCHIVE_DIR": "/a"}
        with mock.patch.dict(os.environ, parent):
            PLUGIN.load_strategy_table(binary)
        lines = out.read_text().splitlines()
        self.assertIn("DASHBOARD=0", lines)
        self.assertEqual([l for l in lines if l.split("=")[0] != "PWD" and l != "DASHBOARD=0"], [])
        cwd = [l for l in lines if l.startswith("PWD=")][0][4:]
        self.assertIn("cg-strategies-", cwd)
        self.assertFalse(Path(cwd).exists())


class ConfigureNotInstalledTest(TempHome):
    def test_configure_needs_an_install(self):
        code, out, _ = self.run_main("configure", "--show")
        self.assertEqual(code, 2)
        self.assertIn("result=not_installed", out)


# ---------------------------------------------------------------------------- update

def release_archive(version, binary_body):
    data = io.BytesIO()
    with tarfile.open(fileobj=data, mode="w:gz") as bundle:
        payload = binary_body.encode()
        info = tarfile.TarInfo("context-guru-proxy")
        info.size, info.mode = len(payload), 0o755
        bundle.addfile(info, io.BytesIO(payload))
    return data.getvalue()


class FakeRelease:
    """A fake GitHub release server: /releases/latest and /releases/download/TAG/..."""

    def __init__(self, version, binary_body, bad_checksum=False):
        name = f"context-guru_{version.removeprefix('v')}_darwin_arm64.tar.gz"
        archive = release_archive(version, binary_body)
        digest = "0" * 64 if bad_checksum else hashlib.sha256(archive).hexdigest()
        files = {f"/download/{version}/{name}": archive,
                 f"/download/{version}/checksums.txt": f"{digest}  {name}\n".encode()}
        owner = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_HEAD(self):
                if self.path == "/latest":
                    self.send_response(302)
                    self.send_header("Location", f"http://127.0.0.1:{owner.port}/tag/{version}")
                    self.end_headers()
                    return
                self.send_response(200)
                self.end_headers()

            def do_GET(self):
                body = files.get(self.path)
                if self.path.startswith("/tag/"):
                    body = b"release page"
                if body is None:
                    self.send_response(404)
                    self.end_headers()
                    return
                self.send_response(200)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.port = self.server.server_address[1]
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.base = f"http://127.0.0.1:{self.port}"

    def close(self):
        self.server.shutdown()
        self.server.server_close()


GOOD_BINARY = ('#!/bin/sh\ncase "$1" in --version) echo "context-guru-proxy v9.9.9" ;; '
               '--help) echo "  -bob-upstream string" ;; esac\nexit 0\n')


class UpdateTest(TempHome):
    def setUp(self):
        super().setUp()
        self.rc.write_bytes(b"x\n")
        self.proxy = FakeProxy().__enter__()
        self.addCleanup(self.proxy.__exit__, None, None, None)
        managed = PLUGIN.managed_binary()
        managed.parent.mkdir(parents=True)
        fake_binary(managed.parent, "context-guru-proxy v0.5.1 (commit old)")
        self.old_bytes = managed.read_bytes()
        os.environ["CONTEXT_GURU_BIN"] = "/nonexistent/context-guru-proxy"
        with mock.patch.object(PLUGIN, "first_free_port", return_value=self.proxy.port):
            code, out, _ = self.run_main("setup", "--i-consent-to-traffic-interception")
        self.assertEqual(code, 0, out)

    def serve(self, version, body, **kwargs):
        release = FakeRelease(version, body, **kwargs)
        self.addCleanup(release.close)
        stack = [mock.patch.object(PLUGIN, "RELEASE_BASE", release.base),
                 mock.patch.object(PLUGIN, "release_platform", return_value=("darwin", "arm64"))]
        for patch in stack:
            patch.start()
            self.addCleanup(patch.stop)
        return release

    def test_check_reports_versions_and_changes_nothing(self):
        self.serve("v9.9.9", GOOD_BINARY)
        before = self.tree()
        code, out, _ = self.run_main("update", "--check")
        self.assertEqual(code, 0, out)
        self.assertIn("installed_version=v0.5.1\n", out)
        self.assertIn("latest_version=v9.9.9\n", out)
        self.assertIn("update_available=true\n", out)
        self.assertEqual(self.tree(), before)

    def test_update_installs_and_restarts(self):
        self.serve("v9.9.9", GOOD_BINARY)
        code, out, _ = self.run_main("update")
        self.assertEqual(code, 0, out)
        self.assertIn("result=updated\nversion=v9.9.9\n", out)
        self.assertEqual(PLUGIN.managed_binary().read_text(), GOOD_BINARY)
        self.assertEqual((PLUGIN.managed_binary().parent / "context-guru-proxy.previous")
                         .read_bytes(), self.old_bytes)

    def test_a_crashing_new_binary_rolls_back_to_the_old_one(self):
        self.serve("v9.9.9", GOOD_BINARY)
        calls = len(self.launchctl_calls())
        with mock.patch.object(PLUGIN, "wait_healthy", side_effect=[False, True]):
            code, out, _ = self.run_main("update")
        self.assertEqual(code, 1)
        self.assertIn("result=update_failed", out)
        self.assertIn("rolled_back=true", out)
        self.assertEqual(PLUGIN.managed_binary().read_bytes(), self.old_bytes)
        self.assertEqual(sum(c.startswith("bootstrap") for c in self.launchctl_calls()[calls:]), 2)

    def test_a_release_before_v0_7_1_is_refused_because_it_cannot_list_strategies(self):
        self.serve("v0.7.0", GOOD_BINARY)
        destination = self.root / "staged/context-guru-proxy"
        with self.assertRaisesRegex(RuntimeError, "v0.7.1 or newer is needed"):
            PLUGIN.install_release_binary("v0.7.0", destination)
        self.assertFalse(destination.exists())

    def test_release_v0_7_1_is_installed(self):
        self.serve("v0.7.1", GOOD_BINARY)
        destination = self.root / "staged/context-guru-proxy"
        PLUGIN.install_release_binary("v0.7.1", destination)
        self.assertEqual(destination.read_text(), GOOD_BINARY)

    def test_a_bad_checksum_changes_nothing(self):
        self.serve("v9.9.9", GOOD_BINARY, bad_checksum=True)
        before = self.tree()
        code, out, _ = self.run_main("update")
        self.assertEqual(code, 1)
        self.assertIn("checksum mismatch", out)
        self.assertEqual(self.tree(), before)

    def test_a_new_binary_without_the_bob_gateway_is_not_installed(self):
        self.serve("v9.9.9", '#!/bin/sh\necho "context-guru-proxy v9.9.9"\n')
        before = self.tree()
        code, out, _ = self.run_main("update")
        self.assertEqual(code, 1)
        self.assertIn("no --bob-upstream", out)
        self.assertEqual(self.tree(), before)

    def test_a_binary_on_path_is_not_replaced(self):
        folder = self.root / "brew"
        folder.mkdir()
        fake_binary(folder, "context-guru-proxy v0.5.1")
        record = PLUGIN.read_record()
        record["binary"] = str(folder / "context-guru-proxy")
        PLUGIN.write_record(record)
        self.serve("v9.9.9", GOOD_BINARY)
        code, out, _ = self.run_main("update")
        self.assertEqual(code, 2)
        self.assertIn("reason=binary_not_managed", out)


class SetupSkipsOldProxyTest(TempHome):
    """Setup never settles on a proxy that cannot list its cache strategies."""

    def test_an_old_proxy_on_path_is_skipped_and_setup_downloads_the_latest_release(self):
        self.rc.write_bytes(b"x\n")
        proxy = FakeProxy().__enter__()
        self.addCleanup(proxy.__exit__, None, None, None)
        folder = self.root / "brew"
        folder.mkdir()
        old = fake_binary(folder, "context-guru-proxy v0.7.0", lists_strategies=False)
        os.environ["CONTEXT_GURU_BIN"] = str(old)
        release = FakeRelease("v9.9.9", GOOD_BINARY)
        self.addCleanup(release.close)
        with mock.patch.object(PLUGIN, "RELEASE_BASE", release.base), \
             mock.patch.object(PLUGIN, "release_platform", return_value=("darwin", "arm64")), \
             mock.patch.object(PLUGIN, "first_free_port", return_value=proxy.port):
            code, out, _ = self.run_main("setup", "--i-consent-to-traffic-interception")
        self.assertEqual(code, 0, out)
        self.assertIn(f"binary_skipped={old} cannot list cache strategies "
                      "(needs v0.7.1 or later)\n", out)
        self.assertIn("binary=installed\nbinary_version=v9.9.9\n", out)
        self.assertEqual(PLUGIN.read_record()["binary"], str(PLUGIN.managed_binary()))
        self.assertEqual(PLUGIN.managed_binary().read_text(), GOOD_BINARY)

    def test_a_proxy_on_path_that_lists_strategies_is_used(self):
        self.rc.write_bytes(b"x\n")
        proxy = FakeProxy().__enter__()
        self.addCleanup(proxy.__exit__, None, None, None)
        folder = self.root / "brew"
        folder.mkdir()
        new = fake_binary(folder, "context-guru-proxy v0.7.1")
        os.environ["CONTEXT_GURU_BIN"] = str(new)
        with mock.patch.object(PLUGIN, "install_release_binary",
                               side_effect=AssertionError("no download expected")), \
             mock.patch.object(PLUGIN, "first_free_port", return_value=proxy.port):
            code, out, _ = self.run_main("setup", "--i-consent-to-traffic-interception")
        self.assertEqual(code, 0, out)
        self.assertNotIn("binary_skipped=", out)
        self.assertEqual(PLUGIN.read_record()["binary"], str(new.resolve()))


# ---------------------------------------------------------------------------- doctor

EXTENSION_JS = ('var a="VITE_GATEWAY_BASE_URL",b="region_domain",c="X-Task-Id",'
                'd="/inference/v1/chat/completions",'
                'e=["SessionStart","UserPromptSubmit","PreCompact","PostCompact","PreToolUse",'
                '"PostToolUse","PermissionRequest","PermissionResolved","Stop"],'
                'f="Tool does not exist";')


def fake_bob_app(root, js=EXTENSION_JS, version="2.3.0"):
    app = Path(root) / "IBM Bob.app"
    dist = app / "Contents/Resources/app/extensions/bob-code/dist"
    dist.mkdir(parents=True)
    (dist / "extension.js").write_text(js)
    with open(app / "Contents/Info.plist", "wb") as handle:
        plistlib.dump({"CFBundleIdentifier": "com.ibm.software.bob",
                       "CFBundleShortVersionString": version}, handle)
    return app


class DoctorTest(Installed):
    def setUp(self):
        super().setUp()
        folder = self.root / "binfolder"
        folder.mkdir()
        record = PLUGIN.read_record()
        record["binary"] = str(fake_binary(folder, "context-guru-proxy v0.5.1"))
        PLUGIN.write_record(record)
        os.environ["CONTEXT_GURU_BOB_APP"] = str(fake_bob_app(self.root))
        import time
        self.proxy_bob_ts = int(time.time() * 1000)

    def doctor(self, *extra):
        before = self.tree()
        calls = len(self.launchctl_calls())
        code, out, err = self.run_main("doctor", *extra)
        self.assertEqual(self.tree(), before, "doctor must change nothing")
        self.assertEqual([c for c in self.launchctl_calls()[calls:]
                          if not c.startswith("print")], [])
        return code, out

    def lines(self, out):
        return dict(line.split("=", 1) for line in out.splitlines() if "=" in line)

    def test_a_healthy_install(self):
        with FakeProxy(last_bob_ts_ms=self.proxy_bob_ts) as proxy:
            record = PLUGIN.read_record()
            record["port"] = proxy.port
            PLUGIN.write_record(record)
            ROUTE.install(self.rc, proxy.port)
            PLUGIN.start_service(record["binary"], proxy.port, record["upstream"])
            # Outside the temp root: the fake launchctl counts down in it, and doctor must not
            # be seen to change a file.
            loaded = Path(self.root.parent / f"{self.root.name}.loaded")
            self.addCleanup(loaded.unlink, missing_ok=True)
            loaded.write_text("1")
            os.environ["CG_TEST_PRINT_LOADED"] = str(loaded)
            code, out = self.doctor("--probe")
        facts = self.lines(out)
        self.assertEqual(code, 0, out)
        for name in ("proxy", "service", "rc_block", "rc_probe", "preset", "hooks", "binary",
                     "bob_bundle", "bob_traffic"):
            self.assertEqual(facts.get(name), "ok", f"{name}: {out}")
        self.assertEqual(facts["bob_version"], "2.3.0")
        self.assertNotIn("fix=", out)

    def test_a_missing_proxy_fails_with_a_fix(self):
        record = PLUGIN.read_record()
        record["port"] = dead_port()
        PLUGIN.write_record(record)
        code, out = self.doctor()
        self.assertEqual(code, 1)
        self.assertIn("proxy=fail\nfix=", out)

    def test_a_bundle_without_the_gateway_variable_fails_with_a_clear_fix(self):
        shutil.rmtree(self.root / "IBM Bob.app")
        os.environ["CONTEXT_GURU_BOB_APP"] = str(fake_bob_app(
            self.root, EXTENSION_JS.replace("VITE_GATEWAY_BASE_URL", "VITE_OTHER")))
        code, out = self.doctor()
        self.assertEqual(code, 1)
        self.assertIn("bob_bundle=fail\n", out)
        self.assertIn("no longer has: VITE_GATEWAY_BASE_URL (B1:", out)
        self.assertIn("Bob 2.3.0", out)
        self.assertIn("context-guru-reset", out)

    def test_a_bundle_missing_a_hook_event_warns(self):
        shutil.rmtree(self.root / "IBM Bob.app")
        os.environ["CONTEXT_GURU_BOB_APP"] = str(fake_bob_app(
            self.root, EXTENSION_JS.replace('"PostCompact",', "")))
        code, out = self.doctor()
        self.assertIn("bob_bundle=warn\n", out)
        self.assertIn("PostCompact", out)

    def test_the_model_path_built_from_two_parts_passes(self):
        """Bob 2.3.0 builds the path: m3e="/inference/v1" and "/chat/completions" apart."""
        shutil.rmtree(self.root / "IBM Bob.app")
        os.environ["CONTEXT_GURU_BOB_APP"] = str(fake_bob_app(self.root, EXTENSION_JS.replace(
            'd="/inference/v1/chat/completions"',
            'm3e="/inference/v1",x=u({path:"/chat/completions"})')))
        code, out = self.doctor()
        self.assertIn("bob_bundle=ok\n", out)
        shutil.rmtree(self.root / "IBM Bob.app")
        os.environ["CONTEXT_GURU_BOB_APP"] = str(fake_bob_app(self.root, EXTENSION_JS.replace(
            'd="/inference/v1/chat/completions"', 'x="/chat/completions"')))
        code, out = self.doctor()
        self.assertIn("bob_bundle=fail\n", out)
        self.assertIn("/inference/v1", out)

    def test_no_bob_installed_is_a_warning(self):
        os.environ["CONTEXT_GURU_BOB_APP"] = str(self.root / "nowhere/IBM Bob.app")
        code, out = self.doctor()
        self.assertIn("bob_bundle=warn\n", out)
        self.assertIn("bob_version=(not found)", out)

    def test_an_edited_rc_block_and_foreign_hooks_fail(self):
        self.rc.write_bytes(self.rc.read_bytes().replace(b"fi\n", b"fi\n# mine\n"))
        code, out = self.doctor()
        self.assertEqual(code, 1)
        self.assertIn("rc_block=fail\n", out)

    def test_an_old_binary_fails(self):
        folder = self.root / "old"
        folder.mkdir()
        record = PLUGIN.read_record()
        record["binary"] = str(fake_binary(folder, "context-guru-proxy v0.2.0", has_bob=False))
        PLUGIN.write_record(record)
        code, out = self.doctor()
        self.assertEqual(code, 1)
        self.assertIn("binary=fail\n", out)

    def test_doctor_not_installed(self):
        PLUGIN.record_path().unlink()
        code, out = self.doctor()
        self.assertEqual(code, 1)
        self.assertIn("install=fail\n", out)


# ---------------------------------------------------------------------------- skills

# ---------------------------------------------------------------------------- P4 review fixes

class SetupConfigUndoTest(Installed):
    """Finding 1: a failed setup re-run keeps the user's preset file byte for byte."""

    def chosen_off(self):
        self.assertEqual(self.run_main("configure", "--preset", "off")[0], 0)
        return self.config.read_bytes()

    def rerun(self, **patches):
        with mock.patch.object(PLUGIN, "find_binary", return_value="/opt/cg/context-guru-proxy"), \
             mock.patch.multiple(PLUGIN, **patches) if patches else mock.patch.object(
                 PLUGIN, "HEALTH_WAIT_S", PLUGIN.HEALTH_WAIT_S):
            return self.run_main("setup", "--i-consent-to-traffic-interception")

    def assert_config_kept(self, before):
        self.assertEqual(self.config.read_bytes(), before)
        with open(PLUGIN.launchd_plist(), "rb") as handle:
            arguments = plistlib.load(handle)["ProgramArguments"]
        self.assertEqual(arguments[arguments.index("--config") + 1], str(self.config))

    def test_start_failed_rerun_keeps_the_preset_file(self):
        before = self.chosen_off()
        code, out, _ = self.rerun(wait_healthy=mock.Mock(side_effect=[False, True]))
        self.assertEqual(code, 1)
        self.assertIn("result=start_failed", out)
        self.assert_config_kept(before)

    def test_hook_failure_rerun_keeps_the_preset_file(self):
        before = self.chosen_off()
        with mock.patch.object(PLUGIN.bob_hooks, "install", side_effect=OSError("disk")):
            code, out, _ = self.rerun()
        self.assertEqual(code, 1)
        self.assertIn("result=setup_failed", out)
        self.assert_config_kept(before)

    def test_a_first_setup_that_fails_removes_the_config_it_wrote(self):
        shutil.rmtree(PLUGIN.state_dir())
        PLUGIN.launchd_plist().unlink()
        with mock.patch.object(PLUGIN, "wait_healthy", return_value=False):
            code, out, _ = self.rerun()
        self.assertEqual(code, 1)
        self.assertFalse(self.config.exists())


class CustomConfigTest(Installed):
    """Finding 2: a hand-edited managed config is never rewritten by a run that did not ask."""

    CUSTOM = PLUGIN.CONFIG_MARKER + "\npipeline: [format, dedup]\n"

    def setUp(self):
        super().setUp()
        self.record_table_binary()
        self.config.write_text(self.CUSTOM)

    def test_cache_strategy_none_keeps_a_custom_file_and_does_not_restart(self):
        calls = len(self.launchctl_calls())
        code, out, _ = self.run_main("configure", "--cache-strategy", "none")
        self.assertEqual(code, 0, out)
        self.assertIn("preset=custom\n", out)
        self.assertEqual(self.config.read_text(), self.CUSTOM)
        self.assertEqual(self.bootstraps(calls), 0)

    def test_setup_rerun_keeps_a_custom_file_and_says_so(self):
        with mock.patch.object(PLUGIN, "find_binary", return_value="/opt/cg/context-guru-proxy"):
            code, out, _ = self.run_main("setup", "--i-consent-to-traffic-interception")
        self.assertEqual(code, 0, out)
        self.assertIn("preset=custom\n", out)
        self.assertEqual(self.config.read_text(), self.CUSTOM)

    def test_doctor_warns_about_a_custom_preset(self):
        os.environ["CONTEXT_GURU_BOB_APP"] = str(fake_bob_app(self.root))
        code, out, _ = self.run_main("doctor")
        self.assertIn("preset=warn\nfix=", out)
        self.assertIn("lossy", out)

    def test_naming_a_preset_still_replaces_a_custom_file(self):
        self.assertEqual(self.run_main("configure", "--preset", "lossless")[0], 0)
        self.assertEqual(self.config.read_text(), LOSSLESS)


class RestartSafetyTest(Installed):
    """Finding 3: Ctrl-C, a partial restore, and two runs at once."""

    def test_ctrl_c_during_wait_restores_config_and_binary(self):
        binary = self.root / "bin-under-test"
        binary.write_bytes(b"old")
        before = self.config.read_bytes()

        def change():
            self.config.write_text("changed")
            binary.write_bytes(b"new")

        with mock.patch.object(PLUGIN, "wait_healthy", side_effect=[KeyboardInterrupt, True]), \
             contextlib.redirect_stdout(io.StringIO()) as out:
            with self.assertRaises(KeyboardInterrupt):
                PLUGIN.change_and_restart(self.port, change, [self.config, binary])
        self.assertIn("rolled_back=true", out.getvalue())
        self.assertEqual(self.config.read_bytes(), before)
        self.assertEqual(binary.read_bytes(), b"old")

    def test_restore_tries_every_file_and_names_the_one_that_failed(self):
        first, second = self.root / "first", self.root / "second"
        first.write_bytes(b"1")
        second.write_bytes(b"2")
        real = PLUGIN._restore_one

        def flaky(path, data):
            if Path(path) == first:
                raise OSError("read-only")
            return real(path, data)

        def change():
            first.write_bytes(b"x")
            second.write_bytes(b"y")

        with mock.patch.object(PLUGIN, "_restore_one", side_effect=flaky), \
             mock.patch.object(PLUGIN, "wait_healthy", side_effect=[False, True]):
            values = PLUGIN.change_and_restart(self.port, change, [first, second])
        self.assertEqual(second.read_bytes(), b"2")
        self.assertEqual(values["rolled_back"], "false")
        self.assertIn(str(first), values["rollback_detail"])

    def test_a_second_run_while_the_lock_is_held_is_busy(self):
        import fcntl
        lock = PLUGIN.state_dir() / "lock"
        with open(lock, "a") as handle:
            fcntl.flock(handle, fcntl.LOCK_EX)
            for argv in (("configure", "--preset", "off"), ("update",),
                         ("setup", "--i-consent-to-traffic-interception")):
                with self.subTest(argv=argv):
                    before = self.tree()
                    code, out, _ = self.run_main(*argv)
                    self.assertEqual(code, 3)
                    self.assertIn("result=busy", out)
                    self.assertEqual(self.tree(), before)


class DoctorReviewTest(Installed):
    """Findings 4 and 5."""

    def setUp(self):
        super().setUp()
        os.environ["CONTEXT_GURU_BOB_APP"] = str(fake_bob_app(self.root))

    def test_the_probe_runs_only_with_probe(self):
        with mock.patch.object(PLUGIN.bob_route, "probe") as probe:
            code, out, _ = self.run_main("doctor")
            probe.assert_not_called()
            self.assertIn("rc_probe=skipped\n", out)
            self.assertIn("--probe", out)
            probe.return_value = {"ide": f"http://127.0.0.1:{self.port}", "terminal_ide": "",
                                  "terminal_shell": ""}
            code, out, _ = self.run_main("doctor", "--probe")
            probe.assert_called_once()
        self.assertIn("rc_probe=ok\n", out)

    def test_an_app_without_the_extension_is_a_layout_change(self):
        (self.root / "IBM Bob.app" / PLUGIN.BUNDLE_JS).unlink()
        code, out, _ = self.run_main("doctor")
        self.assertEqual(code, 1)
        self.assertIn("bob_bundle=fail\n", out)
        self.assertIn("Bob's layout changed; context-guru may not work with Bob 2.3.0", out)

    def test_an_unrelated_chat_completions_does_not_pass_b3(self):
        shutil.rmtree(self.root / "IBM Bob.app")
        js = EXTENSION_JS.replace('d="/inference/v1/chat/completions"',
                                  'doc="see /inference/v1 docs", note="/chat/completions"')
        os.environ["CONTEXT_GURU_BOB_APP"] = str(fake_bob_app(self.root, js))
        code, out, _ = self.run_main("doctor")
        self.assertIn("bob_bundle=fail\n", out)

    def test_the_real_bob_2_3_spelling_passes_b3(self):
        shutil.rmtree(self.root / "IBM Bob.app")
        js = EXTENSION_JS.replace(
            'd="/inference/v1/chat/completions"',
            'zI="/admin/v1",m3e="/inference/v1",oFt="/metrics"' + " " * 5000 +
            'await Iw({url:this.config.url({path:"/chat/completions",modelId:x})})')
        os.environ["CONTEXT_GURU_BOB_APP"] = str(fake_bob_app(self.root, js))
        code, out, _ = self.run_main("doctor")
        self.assertIn("bob_bundle=ok\n", out)

    def test_the_service_check_reads_the_config_path(self):
        with open(PLUGIN.launchd_plist(), "rb") as handle:
            data = plistlib.load(handle)
        arguments = data["ProgramArguments"]
        arguments[arguments.index("--config") + 1] = "/elsewhere/bob.yaml"
        PLUGIN._write_plist(plistlib.dumps(data))
        loaded = Path(self.root.parent / f"{self.root.name}.loaded")
        self.addCleanup(loaded.unlink, missing_ok=True)
        loaded.write_text("1")
        os.environ["CG_TEST_PRINT_LOADED"] = str(loaded)
        code, out, _ = self.run_main("doctor")
        self.assertIn("service=fail\n", out)
        self.assertIn("--config", out)


class VersionTest(unittest.TestCase):
    """Finding 6."""

    def test_release_tags_are_strict(self):
        self.assertEqual(PLUGIN.release_version("v0.5.1"), (0, 5, 1))
        self.assertEqual(PLUGIN.release_version("0.5.1-rc.1"), (0, 5, 1))
        for bad in ("v0.5", "v0.5.1.2", "v0.5.1;rm", "v0.5.1-", "latest", "v0.5.1 x", "vv0.5.1"):
            with self.subTest(tag=bad), self.assertRaises(RuntimeError):
                PLUGIN.release_version(bad)

    def test_update_available_compares_numbers(self):
        self.assertEqual(PLUGIN.compare_versions("v0.5.10", "v0.5.9"), 1)
        self.assertEqual(PLUGIN.compare_versions("v0.5.9", "v0.5.10"), -1)
        self.assertEqual(PLUGIN.compare_versions("v0.5.1", "0.5.1"), 0)
        self.assertIsNone(PLUGIN.compare_versions("unknown", "v0.5.1"))


class UpdateVersionTest(UpdateTest):
    def test_unknown_installed_counts_as_available_with_a_note(self):
        self.serve("v9.9.9", GOOD_BINARY)
        with mock.patch.object(PLUGIN, "binary_version", return_value="unknown"):
            code, out, _ = self.run_main("update", "--check")
        self.assertIn("update_available=true\n", out)
        self.assertIn("note=", out)

    def test_an_older_latest_is_not_a_downgrade_unless_asked(self):
        # The older release must still be v0.7.1 or newer, or install refuses it.
        fake_binary(PLUGIN.managed_binary().parent, "context-guru-proxy v0.8.0")
        self.serve("v0.7.1", GOOD_BINARY)
        code, out, _ = self.run_main("update", "--check")
        self.assertIn("update_available=false\n", out)
        before = self.tree()
        code, out, _ = self.run_main("update")
        self.assertEqual(code, 0)
        self.assertIn("result=up_to_date", out)
        self.assertEqual(self.tree(), before)
        code, out, _ = self.run_main("update", "--allow-downgrade")
        self.assertEqual(code, 0, out)
        self.assertIn("result=updated", out)


class ControlSkillsTest(unittest.TestCase):
    def test_the_p4_skills_exist_and_call_the_controller(self):
        expected = {"preset-picker": "configure", "cache-strategy-picker": "configure",
                    "update": "update", "doctor": "doctor"}
        for folder, sub in expected.items():
            text = (PLUGIN_ROOT / "skills" / folder / "SKILL.md").read_text()
            self.assertTrue(text.startswith(f"---\nname: context-guru-{folder}\n"), folder)
            self.assertIn(f'python3 "$HOME/.bob/plugins/context-guru/scripts/bob_plugin.py" {sub}',
                          text)
        cache = (PLUGIN_ROOT / "skills/cache-strategy-picker/SKILL.md").read_text()
        self.assertIn("SessionEnd", cache)
        preset = (PLUGIN_ROOT / "skills/preset-picker/SKILL.md").read_text()
        self.assertIn("lossless", preset)
        self.assertIn("only one bare word", preset)
        update = (PLUGIN_ROOT / "skills/update/SKILL.md").read_text()
        self.assertIn("one network call to GitHub", update)
        doctor = (PLUGIN_ROOT / "skills/doctor/SKILL.md").read_text()
        self.assertIn("--probe", doctor)
        self.assertNotIn("it changes nothing", doctor.lower().replace("only reads; ", ""))


if __name__ == "__main__":
    unittest.main()
