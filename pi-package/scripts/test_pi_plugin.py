#!/usr/bin/env python3
"""Tests for the Pi package's controller and models.json route. Run: python3 -m unittest discover -s scripts

Every test runs in a temporary HOME / XDG_STATE_HOME / PI_CODING_AGENT_DIR, so nothing on the machine
running the tests changes. Nothing here starts a service or opens the network.
"""

import contextlib
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest import mock

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import models_route  # noqa: E402
import pi_plugin  # noqa: E402


def _row(name, providers, spends=True, desc=None):
    return {"name": name, "providers": providers, "spends": spends, "desc": desc or f"text of {name}", "cache": {}}


# What the proxy prints for `strategies` once the tag fix for #1062 is in. Pi keeps no table of its own.
FAKE_TABLE = {
    "default": "auto", "aliases": {"5-min-ping": "auto"}, "presets": list(pi_plugin.PRESETS),
    "strategies": [_row("none", ["any"], False), _row("auto", ["anthropic", "openai"]),
                   _row("1-ping", ["anthropic", "openai"]), _row("2-ping", ["anthropic", "openai"]),
                   _row("1-hour-head", ["anthropic"], False), _row("30-min-ping", ["openai"])],
}


class Sandbox(unittest.TestCase):
    def setUp(self):
        # The proxy answers `strategies` and is a v0.7.1 release, unless a test says otherwise.
        self.table = mock.patch.object(pi_plugin, "load_strategy_table", side_effect=lambda binary: FAKE_TABLE)
        self.table.start()
        self.state_of_version = mock.patch.object(pi_plugin, "proxy_version_state", return_value="named")
        self.state_of_version.start()
        self.tmp = tempfile.TemporaryDirectory()
        root = Path(self.tmp.name)
        self.env = mock.patch.dict(os.environ, {
            "HOME": str(root), "XDG_STATE_HOME": str(root / "state"),
            "XDG_CONFIG_HOME": str(root / "config"), "PI_CODING_AGENT_DIR": str(root / "agent")})
        self.env.start()
        self.models = root / "agent/models.json"
        self.state = root / "state/context-guru-pi/routing.json"

    def tearDown(self):
        self.state_of_version.stop()
        self.table.stop()
        self.env.stop()
        self.tmp.cleanup()

    def run_main(self, *argv):
        out = io.StringIO()
        with mock.patch.object(sys, "argv", ["pi_plugin.py", *argv]), contextlib.redirect_stdout(out):
            code = pi_plugin.main()
        facts = dict(line.split("=", 1) for line in out.getvalue().splitlines() if "=" in line)
        return code, facts


class ModelsRoute(Sandbox):
    def test_install_on_a_missing_file_and_restore_removes_it(self):
        models_route.install(self.models, self.state, 8800, ["anthropic"])
        doc = json.loads(self.models.read_text())
        self.assertEqual(doc, {"providers": {"anthropic": {"baseUrl": "http://127.0.0.1:8800/anthropic"}}})
        self.assertTrue(models_route.is_routed(self.models, 8800))
        self.assertEqual(models_route.restore(self.state), "restored")
        self.assertFalse(self.models.exists())
        self.assertFalse(self.state.exists())

    def test_restore_keeps_everything_else_and_the_previous_url(self):
        self.models.parent.mkdir(parents=True)
        original = {"providers": {"anthropic": {"baseUrl": "http://127.0.0.1:8800/anthropic", "headers": {"a": "b"}},
                                  "ollama": {"baseUrl": "http://localhost:11434/v1", "api": "openai-completions"}}}
        # A previous URL that IS ours (a re-install) is not a conflict.
        self.models.write_text(json.dumps(original))
        self.assertEqual(models_route.conflicts(self.models, 8800, ["anthropic"]), {})
        original["providers"]["anthropic"]["baseUrl"] = "https://old.example/anthropic"
        self.models.write_text(json.dumps(original))
        self.assertEqual(models_route.conflicts(self.models, 8800, ["anthropic"]),
                         {"anthropic": "https://old.example/anthropic"})
        models_route.install(self.models, self.state, 8800, ["anthropic"])
        self.assertEqual(models_route.restore(self.state), "restored")
        self.assertEqual(json.loads(self.models.read_text()), original)

    def test_restore_leaves_a_key_the_user_changed(self):
        models_route.install(self.models, self.state, 8800, ["anthropic"])
        doc = json.loads(self.models.read_text())
        doc["providers"]["anthropic"]["baseUrl"] = "https://mine.example"
        self.models.write_text(json.dumps(doc))
        self.assertEqual(models_route.restore(self.state), "routing_already_changed")
        self.assertEqual(json.loads(self.models.read_text())["providers"]["anthropic"]["baseUrl"], "https://mine.example")

    def test_a_trailing_slash_on_our_url_is_still_ours(self):
        self.models.parent.mkdir(parents=True)
        self.models.write_text(json.dumps({"providers": {"anthropic": {"baseUrl": "http://127.0.0.1:8800/anthropic/"}}}))
        self.assertTrue(models_route.is_routed(self.models, 8800))
        self.assertEqual(models_route.conflicts(self.models, 8800, ["anthropic"]), {})

    def test_a_broken_file_is_refused_not_overwritten(self):
        self.models.parent.mkdir(parents=True)
        self.models.write_text("{not json")
        with self.assertRaises(ValueError):
            models_route.install(self.models, self.state, 8800, ["anthropic"])
        self.assertEqual(self.models.read_text(), "{not json")

    def test_reinstall_keeps_the_first_record(self):
        self.models.parent.mkdir(parents=True)
        self.models.write_text(json.dumps({"providers": {"anthropic": {"headers": {"x": "y"}}}}))
        models_route.install(self.models, self.state, 8800, ["anthropic"])
        models_route.install(self.models, self.state, 8800, ["anthropic"])
        self.assertEqual(models_route.restore(self.state), "restored")
        self.assertEqual(json.loads(self.models.read_text()), {"providers": {"anthropic": {"headers": {"x": "y"}}}})

    def test_openai_goes_to_the_proxy_responses_path(self):
        self.assertEqual(models_route.base_url(8800, "openai"), "http://127.0.0.1:8800/openai/v1")
        models_route.install(self.models, self.state, 8800, ["anthropic", "openai"])
        doc = json.loads(self.models.read_text())
        self.assertEqual(doc["providers"]["openai"], {"baseUrl": "http://127.0.0.1:8800/openai/v1"})
        self.assertTrue(models_route.is_routed(self.models, 8800, "openai"))

    def test_restore_undoes_each_provider_by_its_own_record(self):
        self.models.parent.mkdir(parents=True)
        self.models.write_text(json.dumps({"providers": {"openai": {"headers": {"h": "1"}}}}))
        models_route.install(self.models, self.state, 8800, ["anthropic", "openai"])
        doc = json.loads(self.models.read_text())
        doc["providers"]["openai"]["baseUrl"] = "https://mine.example/v1"
        self.models.write_text(json.dumps(doc))
        self.assertEqual(models_route.restore(self.state), "routing_already_changed")
        # Anthropic was ours and goes; OpenAI was changed by the user and stays.
        self.assertEqual(json.loads(self.models.read_text()),
                         {"providers": {"openai": {"headers": {"h": "1"}, "baseUrl": "https://mine.example/v1"}}})

    def test_adding_openai_to_an_anthropic_install_keeps_the_first_records(self):
        self.models.parent.mkdir(parents=True)
        original = {"providers": {"anthropic": {"headers": {"x": "y"}}, "openai": {"headers": {"o": "p"}}}}
        self.models.write_text(json.dumps(original))
        models_route.install(self.models, self.state, 8800, ["anthropic"])
        models_route.install(self.models, self.state, 8800, ["anthropic", "openai"])
        keys = json.loads(self.state.read_text())["keys"]
        self.assertEqual(keys["anthropic"], {"previous": None, "created_entry": False})
        self.assertEqual(keys["openai"], {"previous": None, "created_entry": False})
        self.assertEqual(models_route.restore(self.state), "restored")
        self.assertEqual(json.loads(self.models.read_text()), original)

    def test_an_old_port_url_we_wrote_is_ours_not_a_conflict(self):
        models_route.install(self.models, self.state, 8800, ["anthropic"])
        old = {"anthropic": models_route.base_url(8800, "anthropic")}
        self.assertEqual(models_route.conflicts(self.models, 8801, ["anthropic"]),
                         {"anthropic": "http://127.0.0.1:8800/anthropic"})
        self.assertEqual(models_route.conflicts(self.models, 8801, ["anthropic"], old), {})
        models_route.install(self.models, self.state, 8801, ["anthropic"], old)
        record = json.loads(self.state.read_text())
        self.assertEqual((record["port"], record["keys"]["anthropic"]["previous"]), (8801, None))
        self.assertEqual(models_route.restore(self.state), "restored")
        self.assertFalse(self.models.exists())

    def test_our_url_for_one_provider_is_not_ours_for_another(self):
        """A user who put our Anthropic URL into the OpenAI key set that key. It is a conflict, and
        it is never recorded as ours, so uninstall would not delete it."""
        self.models.parent.mkdir(parents=True)
        self.models.write_text(json.dumps({"providers": {"openai": {"baseUrl": "http://127.0.0.1:8800/anthropic"}}}))
        old = {"anthropic": "http://127.0.0.1:8800/anthropic"}
        self.assertEqual(models_route.conflicts(self.models, 8801, ["anthropic", "openai"], old),
                         {"openai": "http://127.0.0.1:8800/anthropic"})
        models_route.install(self.models, self.state, 8801, ["openai"], old)
        record = json.loads(self.state.read_text())
        self.assertEqual(record["keys"]["openai"]["previous"], "http://127.0.0.1:8800/anthropic")
        self.assertEqual(models_route.restore(self.state), "restored")
        self.assertEqual(json.loads(self.models.read_text())["providers"]["openai"]["baseUrl"],
                         "http://127.0.0.1:8800/anthropic")

    def test_without_a_record_our_old_url_is_never_saved_as_the_users_value(self):
        self.models.parent.mkdir(parents=True)
        self.models.write_text(json.dumps({"providers": {"anthropic": {"baseUrl": "http://127.0.0.1:8800/anthropic"}}}))
        models_route.install(self.models, self.state, 8801, ["anthropic"], {"anthropic": "http://127.0.0.1:8800/anthropic"})
        self.assertIsNone(json.loads(self.state.read_text())["keys"]["anthropic"]["previous"])


class Controller(Sandbox):
    def test_setup_waits_for_the_restart_lock_and_writes_nothing_when_busy(self):
        """Review note from #1049: setup writes the config and the record under the restart lock."""
        lock = pi_plugin.restart_lock()
        try:
            with mock.patch.object(pi_plugin, "LOCK_WAIT_SECONDS", 0.2), \
                    mock.patch.object(pi_plugin, "find_binary", return_value="/bin/true"), \
                    mock.patch.object(pi_plugin, "service_kind", return_value="launchd"), \
                    mock.patch.object(pi_plugin, "start_service", return_value=True) as start, \
                    mock.patch.object(pi_plugin, "healthy", return_value=False), \
                    mock.patch.object(pi_plugin, "first_free_port", return_value=8800):
                code, facts = self.run_main("setup", "--i-consent-to-traffic-interception")
        finally:
            lock.close()
        self.assertEqual((code, facts["result"], facts["reason"]), (2, "error", "busy"))
        start.assert_not_called()
        self.assertFalse(self.models.exists())
        self.assertFalse(pi_plugin.record_path().exists())
        self.assertFalse(pi_plugin.proxy_config().exists())

    def test_plan_writes_nothing_and_asks_one_question(self):
        code, facts = self.run_main("setup", "--plan")
        self.assertEqual(code, 0)
        self.assertEqual(facts["result"], "planned")
        self.assertIn("your own quota", facts["consent_question"])
        self.assertIn(str(self.models), facts["consent_question"])
        self.assertFalse(self.models.exists())
        self.assertFalse(pi_plugin.state_dir().exists())

    def test_the_one_question_names_both_providers_and_what_spends_quota(self):
        _, facts = self.run_main("setup", "--plan")
        question = facts["consent_question"]
        self.assertIn("Anthropic and OpenAI", question)
        self.assertIn("/anthropic, forwarding to api.anthropic.com", question)
        self.assertIn("/openai/v1, forwarding to api.openai.com", question)
        # The strategy text is the proxy's, word for word. Pi has no copy of it.
        self.assertIn("quota", question)
        self.assertIn("What it does, from the proxy: text of auto", question)
        self.assertNotIn("4 minutes", question)

    def test_plan_with_strategy_none_does_not_mention_spend(self):
        _, facts = self.run_main("setup", "--plan", "--cache-strategy", "none")
        self.assertNotIn("quota", facts["consent_question"])

    def test_plan_reports_a_foreign_base_url(self):
        self.models.parent.mkdir(parents=True)
        self.models.write_text(json.dumps({"providers": {"anthropic": {"baseUrl": "https://gw.example"}}}))
        code, facts = self.run_main("setup", "--plan")
        self.assertEqual((code, facts["result"], facts["reason"]), (0, "needs_decision", "base_url_already_set"))
        self.assertNotIn("reset_hatch", facts)  # no earlier install, so no hatch to name

    def test_a_refusal_names_the_reset_hatch_when_one_exists(self):
        self.models.parent.mkdir(parents=True)
        self.models.write_text(json.dumps({"providers": {"openai": {"baseUrl": "https://gw.example/v1"}}}))
        pi_plugin.reset_hatch().parent.mkdir(parents=True)
        pi_plugin.reset_hatch().write_text("#!/bin/sh\n")
        _, plan = self.run_main("setup", "--plan")
        self.assertEqual(plan["reset_hatch"], str(pi_plugin.reset_hatch()))
        self.assertIn("gw.example", plan["existing_base_url"])
        code, facts = self.run_main("setup", "--i-consent-to-traffic-interception")
        self.assertEqual((code, facts["reason"], facts["reset_hatch"]), (2, "base_url_already_set", str(pi_plugin.reset_hatch())))

    def test_setup_without_consent_refuses_and_writes_nothing(self):
        code, facts = self.run_main("setup")
        self.assertEqual((code, facts["result"], facts["reason"]), (2, "refused", "consent_required"))
        self.assertFalse(pi_plugin.state_dir().exists())
        self.assertFalse(self.models.exists())

    def test_no_route_is_written_when_the_proxy_does_not_come_up(self):
        with mock.patch.object(pi_plugin, "find_binary", return_value="/bin/true"), \
                mock.patch.object(pi_plugin, "service_kind", return_value="launchd"), \
                mock.patch.object(pi_plugin, "start_service", return_value=False):
            code, facts = self.run_main("setup", "--i-consent-to-traffic-interception")
        self.assertEqual((code, facts["reason"], facts["routing_written"]), (1, "health_check_failed", "false"))
        self.assertFalse(self.models.exists())

    def test_setup_routes_after_the_proxy_answers(self):
        with mock.patch.object(pi_plugin, "find_binary", return_value="/bin/true"), \
                mock.patch.object(pi_plugin, "service_kind", return_value="launchd"), \
                mock.patch.object(pi_plugin, "start_service", return_value=True):
            code, facts = self.run_main("setup", "--i-consent-to-traffic-interception")
        self.assertEqual((code, facts["result"]), (0, "routed"))
        port = int(facts["port"])
        self.assertTrue(models_route.is_routed(self.models, port))
        record = pi_plugin.read_record()
        self.assertTrue(models_route.is_routed(self.models, port, "openai"))
        self.assertEqual(record["base_urls"], {"anthropic": f"http://127.0.0.1:{port}/anthropic",
                                               "openai": f"http://127.0.0.1:{port}/openai/v1"})
        self.assertEqual(record["cache_strategy"], "auto")
        self.assertTrue(pi_plugin.reset_hatch().exists())
        self.assertTrue((pi_plugin.state_dir() / "models_route.py").exists())

    def setup_on(self, port, taken=(), *extra):
        """Run setup with the service mocked, on `port`, while the ports in `taken` belong to someone else.
        The upstreams the service was started with are kept in self.started."""
        self.started = None

        def start(executable, port, upstreams=None):
            self.started = dict(upstreams or {})
            return True

        with mock.patch.object(pi_plugin, "find_binary", return_value="/bin/true"), \
                mock.patch.object(pi_plugin, "service_kind", return_value="launchd"), \
                mock.patch.object(pi_plugin, "start_service", side_effect=start), \
                mock.patch.object(pi_plugin, "healthy", return_value=False), \
                mock.patch.object(pi_plugin, "port_free", side_effect=lambda p: p not in taken), \
                mock.patch.object(pi_plugin, "first_free_port", return_value=port):
            return self.run_main("setup", "--i-consent-to-traffic-interception", *extra)

    def uninstall(self):
        with mock.patch.object(pi_plugin, "stop_service", return_value="stopped"):
            return self.run_main("uninstall")

    def test_setup_on_an_anthropic_only_install_adds_openai(self):
        self.models.parent.mkdir(parents=True)
        original = {"providers": {"anthropic": {"headers": {"x": "y"}}}}
        self.models.write_text(json.dumps(original))
        # The install as the Anthropic-only version left it: one provider in each record.
        with mock.patch.object(pi_plugin, "PROVIDERS", ("anthropic",)):
            self.assertEqual(self.setup_on(8800)[0], 0)
        self.assertNotIn("openai", json.loads(self.models.read_text())["providers"])
        code, facts = self.setup_on(8800)
        self.assertEqual((code, facts["result"], facts["port"]), (0, "routed", "8800"))
        self.assertTrue(models_route.is_routed(self.models, 8800, "anthropic"))
        self.assertTrue(models_route.is_routed(self.models, 8800, "openai"))
        self.assertEqual(set(json.loads(self.state.read_text())["keys"]), {"anthropic", "openai"})
        self.assertEqual(self.uninstall()[1]["models_restore"], "restored")
        self.assertEqual(json.loads(self.models.read_text()), original)

    def test_a_taken_recorded_port_moves_the_route_without_a_refusal(self):
        self.models.parent.mkdir(parents=True)
        original = {"providers": {"anthropic": {"headers": {"x": "y"}}}, "other": 1}
        self.models.write_text(json.dumps(original))
        self.assertEqual(self.setup_on(8800)[1]["port"], "8800")
        code, facts = self.setup_on(8801, taken=(8800,))
        self.assertEqual((code, facts["result"], facts["port"]), (0, "routed", "8801"))
        for provider in pi_plugin.PROVIDERS:
            self.assertTrue(models_route.is_routed(self.models, 8801, provider))
        routing = json.loads(self.state.read_text())
        self.assertEqual(routing["port"], 8801)
        self.assertEqual({p: k["previous"] for p, k in routing["keys"].items()}, {"anthropic": None, "openai": None})
        self.assertEqual(pi_plugin.read_record()["base_urls"]["openai"], "http://127.0.0.1:8801/openai/v1")
        _, removed = self.uninstall()
        self.assertEqual(removed["models_restore"], "restored")
        self.assertEqual(json.loads(self.models.read_text()), original)

    def test_a_fact_never_spans_two_lines(self):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            pi_plugin.facts(detail="first line\nresult=routed")
        self.assertEqual(out.getvalue(), "detail=first line result=routed\n")

    def test_a_taken_recorded_port_is_replaced(self):
        with mock.patch.object(pi_plugin, "healthy", return_value=False), \
                mock.patch.object(pi_plugin, "port_free", side_effect=lambda p: p != 8800), \
                mock.patch.object(pi_plugin, "first_free_port", return_value=8801):
            self.assertEqual(pi_plugin.choose_port({"port": 8800}), 8801)
        with mock.patch.object(pi_plugin, "healthy", return_value=True):
            self.assertEqual(pi_plugin.choose_port({"port": 8800}), 8800)

    def test_config_render(self):
        text = pi_plugin.render_config("medium", "auto")
        self.assertTrue(text.startswith(pi_plugin.MARKER + "\n"))
        self.assertIn("preset: medium\n", text)
        self.assertIn("preset: medium\ncache:\n  strategy: auto\n", text.replace(
            "# The proxy fills the cache keys of this strategy from its own table.\n", ""))
        self.assertNotIn("keepalive", text)
        self.assertNotIn("cache:", pi_plugin.render_config("off", "none"))

    def test_config_refuses_a_file_it_did_not_write(self):
        path = pi_plugin.proxy_config()
        path.parent.mkdir(parents=True)
        path.write_text("preset: high\n")
        with self.assertRaises(RuntimeError):
            pi_plugin.write_proxy_config("off", "auto")
        self.assertEqual(path.read_text(), "preset: high\n")

    def test_reset_hatch_dry_run(self):
        out = subprocess.run(["sh", str(HERE / "reset.sh"), "--dry-run"], capture_output=True, text=True,
                             env=dict(os.environ))
        self.assertEqual(out.returncode, 0, out.stderr)
        self.assertIn("result=planned", out.stdout)

    def test_reset_hatch_with_closed_stdin_is_a_no(self):
        out = subprocess.run(["sh", str(HERE / "reset.sh")], capture_output=True, text=True,
                             stdin=subprocess.DEVNULL, env=dict(os.environ))
        self.assertEqual(out.returncode, 0, out.stderr)
        self.assertIn("result=cancelled", out.stdout)

    def test_reset_hatch_restores_routing(self):
        models_route.install(self.models, self.state, 8800, ["anthropic"])
        hatch_dir = pi_plugin.state_dir()
        (hatch_dir / "models_route.py").write_text((HERE / "models_route.py").read_text())
        out = subprocess.run(["sh", str(HERE / "reset.sh"), "--yes"], capture_output=True, text=True,
                             env=dict(os.environ))
        self.assertEqual(out.returncode, 0, out.stderr)
        self.assertIn("models_restore=restored", out.stdout)
        self.assertFalse(self.models.exists())


class Restart(Sandbox):
    """`restart`: the extension's one restart at session start. Only a service we own is touched."""

    def setUp(self):
        super().setUp()
        pi_plugin.write_record({"port": 8800})
        self.calls = []
        self.fail = set()  # first words of commands that fail, e.g. "kickstart"

        def run(cmd, **kwargs):
            self.calls.append(cmd)
            code = 113 if cmd[1] in self.fail else 0
            if code and kwargs.get("check"):
                raise subprocess.CalledProcessError(code, cmd)
            return subprocess.CompletedProcess(cmd, code, b"", b"")
        self.run_patch = mock.patch.object(pi_plugin.subprocess, "run", side_effect=run)
        self.run_patch.start()
        self.addCleanup(self.run_patch.stop)
        sleep = mock.patch.object(pi_plugin.time, "sleep")
        sleep.start()
        self.addCleanup(sleep.stop)

    def restart(self, kind, healthy):
        with mock.patch.object(pi_plugin, "service_kind", return_value=kind), \
                mock.patch.object(pi_plugin, "healthy", side_effect=healthy):
            return self.run_main("restart")

    def our_plist(self, managed=True):
        plist = pi_plugin.launchd_plist()
        plist.parent.mkdir(parents=True, exist_ok=True)
        with open(plist, "wb") as handle:
            pi_plugin.plistlib.dump({"Label": pi_plugin.LAUNCHD_LABEL, "ContextGuruManaged": managed}, handle)

    def our_unit(self, text=None):
        unit = pi_plugin.systemd_unit()
        unit.parent.mkdir(parents=True, exist_ok=True)
        unit.write_text(text if text is not None else pi_plugin.MARKER + "\n[Service]\n")

    def test_not_installed(self):
        pi_plugin.record_path().unlink()
        code, facts = self.restart("launchd", lambda port, timeout=1: False)
        self.assertEqual((code, facts["result"]), (2, "not_installed"))
        self.assertEqual(self.calls, [])

    def test_a_proxy_that_answers_is_not_restarted(self):
        self.our_plist()
        code, facts = self.restart("launchd", lambda port, timeout=1: True)
        self.assertEqual((code, facts["result"]), (0, "already_up"))
        self.assertEqual(self.calls, [])

    def test_launchd_kickstarts_our_label(self):
        self.our_plist()
        code, facts = self.restart("launchd", [False, False, True])
        self.assertEqual((code, facts["result"], facts["service"]), (0, "up", "restarted"))
        self.assertEqual(self.calls, [["launchctl", "kickstart", "-k", f"gui/{os.getuid()}/io.rossoctl.context-guru-pi"]])

    def test_launchd_bootstraps_a_plist_that_is_not_loaded(self):
        self.our_plist()
        self.fail = {"kickstart"}
        # The first kickstart fails, so the plist is loaded; the second kickstart then fails too here.
        code, facts = self.restart("launchd", lambda port, timeout=1: False)
        self.assertEqual([c[1] for c in self.calls], ["kickstart", "bootstrap", "kickstart"])
        self.assertEqual(self.calls[1], ["launchctl", "bootstrap", f"gui/{os.getuid()}", str(pi_plugin.launchd_plist())])
        self.assertEqual((code, facts["result"], facts["service"]), (1, "down", "failed"))

    def test_launchd_bootstrap_then_kickstart_brings_it_up(self):
        self.our_plist()
        first = [True]

        def run(cmd, **kwargs):
            self.calls.append(cmd)
            if cmd[1] == "kickstart" and first[0]:
                first[0] = False
                return subprocess.CompletedProcess(cmd, 113, b"", b"")
            return subprocess.CompletedProcess(cmd, 0, b"", b"")
        with mock.patch.object(pi_plugin.subprocess, "run", side_effect=run):
            code, facts = self.restart("launchd", [False, True])
        self.assertEqual([c[1] for c in self.calls], ["kickstart", "bootstrap", "kickstart"])
        self.assertEqual((code, facts["result"]), (0, "up"))

    def test_a_plist_we_do_not_own_is_left_alone(self):
        self.our_plist(managed=False)
        code, facts = self.restart("launchd", lambda port, timeout=1: False)
        self.assertEqual((code, facts["result"], facts["service"]), (1, "down", "not_owned"))
        self.assertEqual(self.calls, [])

    def test_no_service_file_means_no_restart(self):
        code, facts = self.restart("launchd", lambda port, timeout=1: False)
        self.assertEqual((code, facts["service"]), (1, "no_service"))
        self.assertEqual(self.calls, [])

    def test_systemd_restarts_our_unit(self):
        self.our_unit()
        code, facts = self.restart("systemd", [False, True])
        self.assertEqual((code, facts["result"]), (0, "up"))
        self.assertEqual(self.calls, [["systemctl", "--user", "restart", "context-guru-pi.service"]])

    def test_a_unit_we_do_not_own_is_left_alone(self):
        self.our_unit("[Service]\n")
        code, facts = self.restart("systemd", lambda port, timeout=1: False)
        self.assertEqual(facts["service"], "not_owned")
        self.assertEqual(self.calls, [])

    def test_still_down_names_the_reset_hatch_and_the_log(self):
        self.our_unit()
        pi_plugin.reset_hatch().write_text("#!/bin/sh\n")
        self.fail = {"--user"}
        code, facts = self.restart("systemd", lambda port, timeout=1: False)
        self.assertEqual((code, facts["result"], facts["service"]), (1, "down", "failed"))
        self.assertEqual(facts["reset_hatch"], str(pi_plugin.reset_hatch()))
        self.assertEqual(facts["log"], str(pi_plugin.state_dir() / "proxy.log"))
        self.assertEqual(facts["port"], "8800")

    LITERAL = (pi_plugin.MARKER + "\n# strategy=auto.\npreset: high\ncache:\n  keepalive: true\n"
               "  keepalive_max_pings: 2\n")

    def write_config(self, strategy="auto", literal=False):
        pi_plugin.write_record({"port": 8800, "preset": "high", "cache_strategy": strategy, "binary": "/bin/proxy"})
        if literal:  # what an older package wrote: the keys, not the name
            pi_plugin.proxy_config().parent.mkdir(parents=True, exist_ok=True)
            pi_plugin.proxy_config().write_text(self.LITERAL)
        else:
            pi_plugin.write_proxy_config("high", strategy)

    def test_an_old_literal_config_is_re_rendered_by_name_before_the_restart(self):
        self.our_unit()
        self.write_config(literal=True)
        self.assertIn("keepalive_max_pings: 2", pi_plugin.proxy_config().read_text())
        with mock.patch.object(pi_plugin, "names_strategies", return_value=True) as supports:
            code, facts = self.restart("systemd", [False, True])
        self.assertEqual((code, facts["result"], facts["config"]), (0, "up", "rendered"))
        supports.assert_called_once_with("/bin/proxy")
        text = pi_plugin.proxy_config().read_text()
        self.assertTrue(text.startswith(pi_plugin.MARKER))
        self.assertIn("preset: high\ncache:\n", text.replace(
            "# The proxy fills the cache keys of this strategy from its own table.\n", ""))
        self.assertIn("  strategy: auto\n", text)
        self.assertNotIn("keepalive", text)
        self.assertEqual(self.calls, [["systemctl", "--user", "restart", "context-guru-pi.service"]])

    def test_an_old_binary_keeps_the_file_as_it_is_and_the_restart_goes_on(self):
        """A proxy before v0.7.1 refuses the `strategy` key, so the file is never rewritten for it."""
        self.our_unit()
        self.write_config(literal=True)
        with mock.patch.object(pi_plugin, "names_strategies", return_value=False):
            code, first = self.restart("systemd", [False, True])
            _, second = self.restart("systemd", [False, True])
        self.assertEqual((code, first["result"], first["config"], second["config"]), (0, "up", "old_proxy", "old_proxy"))
        self.assertEqual(pi_plugin.proxy_config().read_text(), self.LITERAL)

    def test_none_is_written_on_any_binary(self):
        self.our_unit()
        self.write_config(strategy="none")
        pi_plugin.proxy_config().write_text(self.LITERAL)
        with mock.patch.object(pi_plugin, "names_strategies", return_value=False):
            _, facts = self.restart("systemd", [False, True])
        self.assertEqual(facts["config"], "rendered")
        self.assertNotIn("cache:", pi_plugin.proxy_config().read_text())

    def test_a_proxy_that_answers_gets_the_new_file_and_no_restart(self):
        self.our_plist()
        self.write_config(literal=True)
        with mock.patch.object(pi_plugin, "names_strategies", return_value=True):
            code, facts = self.restart("launchd", lambda port, timeout=1: True)
        self.assertEqual((code, facts["result"], facts["config"]), (0, "already_up", "rendered"))
        self.assertIn("  strategy: auto\n", pi_plugin.proxy_config().read_text())
        self.assertEqual(self.calls, [])

    def test_a_hand_written_config_is_left_alone_and_the_restart_goes_on(self):
        self.our_unit()
        pi_plugin.write_record({"port": 8800, "preset": "high", "cache_strategy": "auto", "binary": "/bin/proxy"})
        pi_plugin.proxy_config().write_text("preset: xhigh\n")
        with mock.patch.object(pi_plugin, "names_strategies") as supports:
            code, facts = self.restart("systemd", [False, True])
        self.assertEqual((code, facts["result"], facts["config"]), (0, "up", "not_owned"))
        supports.assert_not_called()
        self.assertEqual(pi_plugin.proxy_config().read_text(), "preset: xhigh\n")

    def test_restart_asks_find_binary_when_the_record_has_no_binary(self):
        self.our_unit()
        self.write_config(literal=True)
        record = pi_plugin.read_record()
        del record["binary"]
        pi_plugin.write_record(record)
        with mock.patch.object(pi_plugin, "find_binary", return_value="/found/proxy"), \
                mock.patch.object(pi_plugin, "names_strategies", return_value=True) as supports:
            code, facts = self.restart("systemd", [False, True])
        self.assertEqual((code, facts["config"]), (0, "rendered"))
        supports.assert_called_once_with("/found/proxy")

    def test_a_missing_config_is_not_created_by_restart(self):
        self.our_unit()
        code, facts = self.restart("systemd", [False, True])
        self.assertEqual((code, facts["config"]), (0, "missing"))
        self.assertFalse(pi_plugin.proxy_config().exists())

    def test_a_failed_re_render_does_not_stop_the_restart(self):
        self.our_unit()
        self.write_config(literal=True)
        before = pi_plugin.proxy_config().read_text()
        with mock.patch.object(pi_plugin, "names_strategies", return_value=True), \
                mock.patch.object(pi_plugin, "write_proxy_config", side_effect=OSError("disk full")):
            code, facts = self.restart("systemd", [False, True])
        self.assertEqual((code, facts["result"], facts["config"]), (0, "up", "failed"))
        self.assertEqual(pi_plugin.proxy_config().read_text(), before)

    def test_an_unsupported_platform_restarts_nothing(self):
        code, facts = self.restart(None, lambda port, timeout=1: False)
        self.assertEqual((code, facts["service"]), (1, "unsupported"))
        self.assertEqual(self.calls, [])


    def test_a_missing_reset_hatch_is_not_named(self):
        self.our_unit()
        code, facts = self.restart("systemd", lambda port, timeout=1: False)
        self.assertEqual(facts["result"], "down")
        self.assertNotIn("reset_hatch", facts)

    def test_the_first_health_check_has_the_extensions_5_second_limit(self):
        """A proxy that is slow but alive answers within 5 s, and is not force-restarted."""
        self.our_plist()
        limits = []

        def healthy(port, timeout=1):
            limits.append(timeout)
            return timeout >= 5  # answers only to a check that waits long enough
        code, facts = self.restart("launchd", healthy)
        self.assertEqual((code, facts["result"]), (0, "already_up"))
        self.assertEqual(limits, [pi_plugin.START_CHECK_SECONDS])
        self.assertEqual(pi_plugin.START_CHECK_SECONDS, 5)
        self.assertEqual(self.calls, [])

    def test_a_second_restart_waits_for_the_first_and_finds_the_proxy_up(self):
        self.our_plist()
        up = threading.Event()
        result = {}
        lock = pi_plugin.restart_lock()  # the first restart, still running
        try:
            with mock.patch.object(pi_plugin, "service_kind", return_value="launchd"), \
                    mock.patch.object(pi_plugin, "healthy", side_effect=lambda port, timeout=1: up.is_set()):
                second = threading.Thread(target=lambda: result.update(zip(("code", "facts"), self.run_main("restart"))))
                second.start()
                second.join(0.3)
                self.assertTrue(second.is_alive(), "the second caller waits for the lock")
                up.set()  # the first restart brought the proxy up
                lock.close()
                second.join(5)
        finally:
            lock.close()
        self.assertFalse(second.is_alive())
        self.assertEqual((result["code"], result["facts"]["result"]), (0, "already_up"))
        self.assertEqual(self.calls, [])

    def test_a_restart_that_waits_too_long_for_the_lock_reports_busy(self):
        """Review note from #1011: the lock wait has its own limit, inside the extension's 60 s."""
        self.our_plist()
        lock = pi_plugin.restart_lock()
        try:
            with mock.patch.object(pi_plugin, "LOCK_WAIT_SECONDS", 0.2):
                code, facts = self.restart("launchd", lambda port, timeout=1: False)
        finally:
            lock.close()
        self.assertEqual((code, facts["result"]), (1, "busy"))
        self.assertEqual(self.calls, [])
        self.assertLess(pi_plugin.LOCK_WAIT_SECONDS, 60)

    def test_a_restart_reads_the_record_again_after_the_lock(self):
        """Review note from #1049: a configure that ran during the wait is not undone."""
        self.our_plist()
        seen = []
        lock = pi_plugin.restart_lock()
        result = {}
        try:
            with mock.patch.object(pi_plugin, "service_kind", return_value="launchd"), \
                    mock.patch.object(pi_plugin, "healthy", return_value=True), \
                    mock.patch.object(pi_plugin, "rerender_config",
                                      side_effect=lambda record, binary: seen.append(dict(record)) or "rendered"):
                second = threading.Thread(target=lambda: result.update(zip(("code", "facts"), self.run_main("restart"))))
                second.start()
                second.join(0.3)
                self.assertTrue(second.is_alive(), "the restart waits for the lock")
                pi_plugin.write_record({"port": 8800, "cache_strategy": "none"})  # configure, during the wait
                lock.close()
                second.join(5)
        finally:
            lock.close()
        self.assertEqual(result["facts"]["result"], "already_up")
        self.assertEqual([r.get("cache_strategy") for r in seen], ["none"])

    def test_a_restart_checks_the_port_that_a_setup_moved_during_the_wait(self):
        """Review finding on #1058: the health check and the facts use the port read after the wait."""
        self.our_plist()
        checked, result = [], {}
        lock = pi_plugin.restart_lock()
        try:
            with mock.patch.object(pi_plugin, "service_kind", return_value="launchd"), \
                    mock.patch.object(pi_plugin, "rerender_config", return_value="unchanged"), \
                    mock.patch.object(pi_plugin, "healthy",
                                      side_effect=lambda port, timeout=1: checked.append(port) or port == 8801):
                second = threading.Thread(target=lambda: result.update(zip(("code", "facts"), self.run_main("restart"))))
                second.start()
                second.join(0.3)
                pi_plugin.write_record({"port": 8801})  # setup moved the port
                lock.close()
                second.join(5)
        finally:
            lock.close()
        self.assertEqual((result["facts"]["result"], result["facts"]["port"]), ("already_up", "8801"))
        self.assertEqual(checked, [8801])
        self.assertEqual(self.calls, [])

    def test_with_no_fcntl_restart_reports_unsupported_and_no_traceback(self):
        """Review note from #1011: a state directory copied to Windows has no fcntl."""
        with mock.patch.dict(sys.modules, {"fcntl": None}):
            code, facts = self.restart(None, lambda port, timeout=1: False)
        self.assertEqual((code, facts["result"], facts["service"]), (1, "down", "unsupported"))
        self.assertEqual(self.calls, [])


class ConfigByName(Sandbox):
    """The config names the strategy for a binary with #987 (v0.7.1+), and is re-rendered before
    each start, like the Claude Code plugin's `strategy sync` (#1044)."""

    def fake_binary(self, output, code=0):
        path = Path(self.tmp.name) / "fake-proxy"
        path.write_text(f"#!/bin/sh\nprintf '%s\\n' '{output}'\nexit {code}\n")
        path.chmod(0o755)
        return str(path)

    def test_a_new_binary_gets_the_name_and_no_literal_keys(self):
        text = pi_plugin.render_config("medium", "auto")
        self.assertTrue(text.startswith(pi_plugin.MARKER + "\n# strategy=auto."))
        self.assertIn("preset: medium\ncache:\n  strategy: auto\n", text.replace(
            "# The proxy fills the cache keys of this strategy from its own table.\n", ""))
        self.assertNotIn("keepalive", text)

    def test_pi_keeps_no_copy_of_the_strategy_table(self):
        self.assertFalse(hasattr(pi_plugin, "STRATEGIES"))
        self.assertNotIn("keepalive_max_pings", pi_plugin.render_config("medium", "2-ping"))

    def test_none_writes_no_cache_block(self):
        self.assertNotIn("cache:", pi_plugin.render_config("high", "none"))

    def test_the_upstream_comment_lines_stay_with_the_name(self):
        text = pi_plugin.render_config("off", "1-ping", {"openai": "https://gw.example/openai"})
        self.assertIn("# chained: openai -> proxy -> https://gw.example/openai (--openai-upstream", text)
        self.assertIn("  strategy: 1-ping\n", text)

    def test_support_comes_from_the_version_the_binary_prints(self):
        cases = {"context-guru-proxy 0.7.1 (commit ac68a7a0)": True, "context-guru-proxy v0.8.0 (commit x)": True,
                 "context-guru-proxy 0.7.2-rc1 (commit x)": True, "context-guru-proxy 0.7.1-rc1 (commit x)": False,
                 "context-guru-proxy v0.7.1-rc1 (commit x)": False, "context-guru-proxy 0.7.0 (commit x)": False,
                 "context-guru-proxy dev (commit x)": False, "Usage of context-guru-proxy:": False}
        # The version rule under test, so the suite-wide stub is off.
        self.state_of_version.stop()
        for output, want in cases.items():
            self.assertEqual(pi_plugin.names_strategies(self.fake_binary(output)), want, output)
        self.assertFalse(pi_plugin.names_strategies(self.fake_binary("flag provided but not defined", 2)))
        self.assertFalse(pi_plugin.names_strategies(str(Path(self.tmp.name) / "absent")))
        self.assertFalse(pi_plugin.names_strategies(None))

    def test_setup_and_configure_name_the_strategy_for_a_new_binary(self):
        with mock.patch.object(pi_plugin, "names_strategies", return_value=True):
            self.assertEqual(Controller.setup_on(self, 8800)[0], 0)
            self.assertIn("  strategy: auto\n", pi_plugin.proxy_config().read_text())
            with mock.patch.object(pi_plugin, "start_service", return_value=True):
                code, _ = self.run_main("configure", "--cache-strategy", "2-ping")
        self.assertEqual(code, 0)
        self.assertIn("  strategy: 2-ping\n", pi_plugin.proxy_config().read_text())

    def test_setup_still_refuses_a_hand_written_config_before_any_download(self):
        path = pi_plugin.proxy_config()
        path.parent.mkdir(parents=True)
        path.write_text("preset: high\n")
        with mock.patch.object(pi_plugin, "install_release_binary") as download, \
                mock.patch.object(pi_plugin, "find_binary", return_value=None), \
                mock.patch.object(pi_plugin, "service_kind", return_value="launchd"), \
                mock.patch.object(pi_plugin, "healthy", return_value=False), \
                mock.patch.object(pi_plugin, "port_free", return_value=True):
            code, facts = self.run_main("setup", "--i-consent-to-traffic-interception")
        self.assertEqual((code, facts["reason"]), (2, "config_conflict"))
        download.assert_not_called()
        self.assertEqual(path.read_text(), "preset: high\n")

    def installed(self, strategy="auto", binary="/bin/proxy"):
        pi_plugin.write_record({"port": 8800, "preset": "medium", "cache_strategy": strategy, "binary": binary,
                                "upstreams": {"anthropic": "https://gw.example/anthropic"}})
        pi_plugin.write_proxy_config("medium", strategy, {"anthropic": "https://gw.example/anthropic"})

    def test_an_invalid_record_leaves_the_file_alone(self):
        self.installed()
        before = pi_plugin.proxy_config().read_text()
        for record in ({"preset": "huge", "cache_strategy": "auto"}, {"preset": "medium", "cache_strategy": "Not A Name"}):
            with mock.patch.object(pi_plugin, "names_strategies") as supports:
                self.assertEqual(pi_plugin.rerender_config(record, "/bin/proxy"), "invalid_record")
            supports.assert_not_called()
        self.assertEqual(pi_plugin.proxy_config().read_text(), before)

    def test_update_check_reads_the_version_the_binary_prints(self):
        binary = self.fake_binary("context-guru-proxy 0.7.0 (commit abc)")
        pi_plugin.write_record({"port": 8800, "binary": binary})
        with mock.patch.object(pi_plugin, "latest_release_tag", return_value="v0.7.1"):
            code, facts = self.run_main("update", "--check")
        self.assertEqual((code, facts["installed_version"], facts["latest_version"], facts["update_available"]),
                         (0, "0.7.0", "v0.7.1", "true"))
        pi_plugin.write_record({"port": 8800, "binary": self.fake_binary("context-guru-proxy 0.7.1 (commit abc)")})
        with mock.patch.object(pi_plugin, "latest_release_tag", return_value="v0.7.1"):
            _, facts = self.run_main("update", "--check")
        self.assertEqual((facts["installed_version"], facts["update_available"]), ("0.7.1", "false"))
        pi_plugin.write_record({"port": 8800, "binary": self.fake_binary("Usage of context-guru-proxy:")})
        with mock.patch.object(pi_plugin, "latest_release_tag", return_value="v0.7.1"):
            _, facts = self.run_main("update", "--check")
        self.assertEqual((facts["installed_version"], facts["update_available"]), ("unknown", "true"))

    def test_configure_writes_the_config_and_the_record_under_the_restart_lock(self):
        self.installed()
        held = {}
        real_write = pi_plugin.write_proxy_config

        def write(*args, **kwargs):
            # A restart that tries to take the lock now must wait.
            import fcntl
            with open(pi_plugin.state_dir() / "restart.lock", "a") as other:
                try:
                    fcntl.flock(other, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    held["locked"] = False
                except BlockingIOError:
                    held["locked"] = True
            return real_write(*args, **kwargs)
        with mock.patch.object(pi_plugin, "write_proxy_config", side_effect=write), \
                mock.patch.object(pi_plugin, "names_strategies", return_value=True), \
                mock.patch.object(pi_plugin, "start_service", return_value=True):
            code, _ = self.run_main("configure", "--cache-strategy", "1-ping")
        self.assertEqual((code, held["locked"]), (0, True))
        self.assertEqual(pi_plugin.read_record()["cache_strategy"], "1-ping")
        self.assertIn("  strategy: 1-ping\n", pi_plugin.proxy_config().read_text())

    def test_update_re_renders_with_the_new_binary_before_the_start(self):
        self.installed(binary=str(pi_plugin.managed_binary()))
        pi_plugin.proxy_config().write_text(Restart.LITERAL)  # an old install: literal keys
        new = str(pi_plugin.managed_binary())
        asked, seen = [], {}

        def supports(executable):
            asked.append(executable)
            return True

        def start(executable, port, upstreams=None):
            seen["config"] = pi_plugin.proxy_config().read_text()
            return True
        with mock.patch.object(pi_plugin, "latest_release_tag", return_value="v0.7.1"), \
                mock.patch.object(pi_plugin, "binary_version_text", return_value="0.7.0"), \
                mock.patch.object(pi_plugin, "install_release_binary", return_value=(new, "v0.7.1")), \
                mock.patch.object(pi_plugin, "names_strategies", side_effect=supports), \
                mock.patch.object(pi_plugin, "start_service", side_effect=start):
            code, facts = self.run_main("update", "--install")
        self.assertEqual((code, facts["result"], facts["config"], facts["restart"]), (0, "updated", "rendered", "completed"))
        self.assertEqual(asked, [new])
        self.assertIn("preset: medium\n", seen["config"])
        self.assertIn("  strategy: auto\n", seen["config"])
        self.assertNotIn("keepalive", seen["config"])
        self.assertIn("# chained: anthropic -> proxy -> https://gw.example/anthropic", seen["config"])

    def during_the_lock_wait(self, argv, change, patches=()):
        """Run `argv` while the restart lock is held; run `change` while it waits; then free the lock."""
        result = {}
        lock = pi_plugin.restart_lock()
        try:
            with contextlib.ExitStack() as stack:
                for patch in patches:
                    stack.enter_context(patch)
                thread = threading.Thread(target=lambda: result.update(zip(("code", "facts"), self.run_main(*argv))))
                thread.start()
                thread.join(0.3)
                self.assertTrue(thread.is_alive(), "the command waits for the lock")
                change()
                lock.close()
                thread.join(5)
        finally:
            lock.close()
        self.assertFalse(thread.is_alive())
        return result["code"], result["facts"]

    def test_configure_reads_the_record_again_after_the_lock(self):
        """Review finding on #1058: a setup that ran during the wait is not undone."""
        self.installed()
        started = {}
        both = {"anthropic": "https://gw.example/anthropic", "openai": "https://gw.example/openai/v1"}

        def setup_ran():
            pi_plugin.write_record({**pi_plugin.read_record(), "upstreams": both, "base_urls": {"openai": "x"}})

        def start(executable, port, upstreams=None):
            started["upstreams"] = dict(upstreams or {})
            return True
        code, facts = self.during_the_lock_wait(
            ("configure", "--cache-strategy", "1-ping"), setup_ran,
            (mock.patch.object(pi_plugin, "names_strategies", return_value=True),
             mock.patch.object(pi_plugin, "start_service", side_effect=start)))
        self.assertEqual((code, facts["result"]), (0, "configured"))
        record = pi_plugin.read_record()
        self.assertEqual((record["cache_strategy"], record["upstreams"], record["base_urls"]),
                         ("1-ping", both, {"openai": "x"}))
        self.assertEqual(started["upstreams"], both)
        self.assertIn("# chained: openai -> proxy -> https://gw.example/openai/v1", pi_plugin.proxy_config().read_text())

    def test_update_after_an_uninstall_during_the_wait_skips_the_restart(self):
        """Review finding on #1058: no KeyError, and no new record holding only the binary."""
        self.installed(binary=str(pi_plugin.managed_binary()))
        new = str(pi_plugin.managed_binary())
        start = mock.Mock(return_value=True)
        code, facts = self.during_the_lock_wait(
            ("update", "--install"), lambda: pi_plugin.record_path().unlink(),
            (mock.patch.object(pi_plugin, "latest_release_tag", return_value="v0.7.1"),
             mock.patch.object(pi_plugin, "binary_version_text", return_value="0.7.0"),
             mock.patch.object(pi_plugin, "install_release_binary", return_value=(new, "v0.7.1")),
             mock.patch.object(pi_plugin, "start_service", start)))
        self.assertEqual((code, facts["result"], facts["restart"]), (0, "updated", "skipped"))
        self.assertFalse(pi_plugin.record_path().exists())
        start.assert_not_called()

    def test_configure_and_update_refuse_when_the_lock_stays_busy(self):
        """Review note on #1058: the busy paths of configure and update write nothing."""
        self.installed(binary=str(pi_plugin.managed_binary()))
        before = (pi_plugin.record_path().read_text(), pi_plugin.proxy_config().read_text())
        start = mock.Mock(return_value=True)
        lock = pi_plugin.restart_lock()
        try:
            with mock.patch.object(pi_plugin, "LOCK_WAIT_SECONDS", 0.2), \
                    mock.patch.object(pi_plugin, "names_strategies", return_value=True), \
                    mock.patch.object(pi_plugin, "start_service", start), \
                    mock.patch.object(pi_plugin, "latest_release_tag", return_value="v0.7.1"), \
                    mock.patch.object(pi_plugin, "binary_version_text", return_value="0.7.0"), \
                    mock.patch.object(pi_plugin, "install_release_binary",
                                      return_value=(str(pi_plugin.managed_binary()), "v0.7.1")):
                configured = self.run_main("configure", "--cache-strategy", "1-ping")
                updated = self.run_main("update", "--install")
        finally:
            lock.close()
        self.assertEqual((configured[0], configured[1]["result"], configured[1]["reason"]), (2, "refused", "busy"))
        self.assertIn("0.2 seconds", configured[1]["detail"])
        self.assertEqual((updated[0], updated[1]["result"], updated[1]["reason"]), (1, "error", "busy"))
        self.assertEqual((pi_plugin.record_path().read_text(), pi_plugin.proxy_config().read_text()), before)
        start.assert_not_called()


class StrategiesFromTheProxy(Sandbox):
    """#1062: Pi asks the proxy binary for its strategy table and keeps no copy of it."""

    def setUp(self):
        super().setUp()
        self.table.stop()                 # the real probe runs here, against fake binaries
        self.state_of_version.stop()
        pi_plugin._TABLES.clear()
        self.addCleanup(pi_plugin._TABLES.clear)
        self.count = 0

    def fake(self, body):
        self.count += 1
        path = Path(self.tmp.name) / f"proxy{self.count}"
        path.write_text("#!/bin/sh\n" + body)
        path.chmod(0o755)
        return str(path)

    def printing(self, document):
        out = Path(self.tmp.name) / f"table{self.count + 1}.json"
        out.write_text(json.dumps(document))
        return self.fake(f'[ "$1" = strategies ] || exit 9\ncat {out}\n')

    def test_the_probe_reads_the_table_the_binary_prints(self):
        table = pi_plugin.load_strategy_table(self.printing(FAKE_TABLE))
        self.assertEqual([r["name"] for r in table["strategies"]][:2], ["none", "auto"])
        self.assertEqual(table["aliases"], {"5-min-ping": "auto"})

    def test_the_probe_is_safe_a_scrubbed_env_a_fresh_dir_and_no_dashboard(self):
        seen = Path(self.tmp.name) / "seen.txt"
        binary = self.fake(f'{{ echo "cwd=$(pwd)"; echo "listen=$LISTEN_ADDR"; echo "config=[$CONFIG]"; '
                           f'echo "dashboard=$DASHBOARD"; echo "db=[$DASHBOARD_DB]"; echo "store=[$STORE]"; '
                           f'echo "state=[$SESSION_STATE_DIR]"; echo "rec=[$CG_RECORD_DIR]"; echo "arch=[$ARCHIVE_DIR]"; '
                           f'echo "ctl=[$CONTROL_DB]"; echo "ups=[$UPSTREAMS]"; echo "idle=[$IDLE_EXIT]"; '
                           f'}} > {seen}\nexit 1\n')
        env = {k: "x" for k in ("DASHBOARD_DB", "STORE", "SESSION_STATE_DIR", "CG_RECORD_DIR", "ARCHIVE_DIR",
                                "CONTROL_DB", "UPSTREAMS", "IDLE_EXIT")}
        with mock.patch.dict(os.environ, env), self.assertRaises(pi_plugin.StrategyError):
            pi_plugin.load_strategy_table(binary)
        got = dict(line.split("=", 1) for line in seen.read_text().splitlines())
        self.assertEqual((got["listen"], got["config"], got["dashboard"]), ("127.0.0.1:0", "[]", "0"))
        for key in ("db", "store", "state", "rec", "arch", "ctl", "ups", "idle"):
            self.assertEqual(got[key], "[]", key)
        self.assertTrue(Path(got["cwd"]).name.startswith("cg-strategies-"))
        self.assertFalse(Path(got["cwd"]).exists(), "the probe directory is removed")

    def test_an_old_binary_that_serves_is_stopped_at_the_time_limit(self):
        binary = self.fake("exec sleep 30\n")
        with mock.patch.object(pi_plugin, "STRATEGIES_TIMEOUT", 0.3), self.assertRaises(pi_plugin.StrategyError) as cm:
            pi_plugin.load_strategy_table(binary)
        self.assertEqual(cm.exception.reason, "proxy_too_old")
        self.assertIn("v0.7.1 or later", str(cm.exception))

    def test_bad_output_gives_the_clean_error_not_a_traceback(self):
        bad = ["not json at all", "[1, 2]", "{}", '{"strategies": []}',
               json.dumps({**FAKE_TABLE, "aliases": []}), json.dumps({**FAKE_TABLE, "aliases": {"a": 1}}),
               json.dumps({"strategies": [{"name": "x", "providers": ["any"], "spends": "yes", "desc": "d"}]}),
               json.dumps({"strategies": [{"name": "x y", "providers": ["any"], "spends": True, "desc": "d"}]}),
               json.dumps({"strategies": [{"name": "x", "providers": "any", "spends": True, "desc": "d"}]})]
        for text in bad:
            pi_plugin._TABLES.clear()
            out = Path(self.tmp.name) / "bad.txt"
            out.write_text(text)
            with self.assertRaises(pi_plugin.StrategyError, msg=text):
                pi_plugin.load_strategy_table(self.fake(f"cat {out}\n"))
        with self.assertRaises(pi_plugin.StrategyError):       # bytes that are not UTF-8
            pi_plugin.load_strategy_table(self.fake("printf '\\377\\376'\n"))
        with self.assertRaises(pi_plugin.StrategyError):       # a non-zero exit
            pi_plugin.load_strategy_table(self.fake("echo '{}'\nexit 3\n"))
        with self.assertRaises(pi_plugin.StrategyError) as cm:
            pi_plugin.load_strategy_table(None)
        self.assertEqual(cm.exception.reason, "no_proxy_binary")

    def test_the_offer_is_the_strategies_that_serve_every_provider_pi_routes(self):
        names = [r["name"] for r in pi_plugin.offered_strategies(FAKE_TABLE)]
        self.assertEqual(names, ["none", "auto", "1-ping", "2-ping"])

    def test_none_works_with_no_binary_and_no_table(self):
        self.assertEqual(pi_plugin.resolve_strategy("none", None), ("none", None))

    def test_a_name_is_resolved_through_the_proxy_aliases_and_the_offer(self):
        binary = self.printing(FAKE_TABLE)
        with mock.patch.object(pi_plugin, "proxy_version_state", return_value="named"):
            self.assertEqual(pi_plugin.resolve_strategy("5-min-ping", binary)[0], "auto")
            self.assertEqual(pi_plugin.resolve_strategy("auto", binary)[1]["desc"], "text of auto")
            for name in ("1-hour-head", "30-min-ping", "nope"):
                with self.assertRaises(pi_plugin.StrategyError) as cm:
                    pi_plugin.resolve_strategy(name, binary)
                self.assertEqual(cm.exception.reason, "unknown_strategy")

    def test_a_proxy_older_than_v0_7_1_refuses_every_name_but_none_with_the_update_hint(self):
        binary = self.printing(FAKE_TABLE)       # even a table that answers does not rescue an old release
        with mock.patch.object(pi_plugin, "proxy_version_state", return_value="older"):
            self.assertEqual(pi_plugin.resolve_strategy("none", binary)[0], "none")
            with self.assertRaises(pi_plugin.StrategyError) as cm:
                pi_plugin.resolve_strategy("auto", binary)
        self.assertEqual(cm.exception.reason, "proxy_too_old")
        self.assertIn("update the proxy to v0.7.1 or later", str(cm.exception))

    def test_a_v0_7_0_binary_that_really_prints_its_version_is_refused(self):
        binary = self.fake('[ "$1" = --version ] && { echo "context-guru-proxy 0.7.0 (commit x)"; exit 0; }\nexec sleep 30\n')
        pi_plugin.write_record({"port": 8800, "binary": binary, "preset": "off", "cache_strategy": "none"})
        with mock.patch.object(pi_plugin, "STRATEGIES_TIMEOUT", 0.3):
            code, facts = self.run_main("configure", "--cache-strategy", "auto")
            self.assertEqual((code, facts["result"], facts["reason"]), (2, "refused", "proxy_too_old"))
            self.assertFalse(pi_plugin.proxy_config().exists())
            self.assertEqual(pi_plugin.read_record()["cache_strategy"], "none")
            with mock.patch.object(pi_plugin, "start_service", return_value=True):
                code, facts = self.run_main("configure", "--cache-strategy", "none")
        self.assertEqual((code, facts["result"]), (0, "configured"))
        self.assertNotIn("cache:", pi_plugin.proxy_config().read_text())

    def test_list_comes_from_the_proxy_and_an_old_proxy_lists_only_none(self):
        binary = self.printing(FAKE_TABLE)
        pi_plugin.write_record({"port": 8800, "binary": binary})
        with mock.patch.object(pi_plugin, "proxy_version_state", return_value="named"):
            _, facts = self.run_main("configure", "--list")
        self.assertEqual(facts["strategies"], "none,auto,1-ping,2-ping")
        self.assertEqual(facts["strategy_1_ping"], "text of 1-ping")
        with mock.patch.object(pi_plugin, "proxy_version_state", return_value="older"):
            _, facts = self.run_main("configure", "--list")
        self.assertEqual(facts["strategies"], "none")
        self.assertIn("v0.7.1 or later", facts["strategy_error"])

    def test_setup_on_a_proxy_that_cannot_list_refuses_a_named_strategy_and_writes_nothing(self):
        binary = self.fake('[ "$1" = --version ] && { echo "context-guru-proxy 0.7.0 (commit x)"; exit 0; }\nexit 1\n')
        pi_plugin.write_record({"binary": binary})
        for argv in (("setup", "--plan"), ("setup", "--i-consent-to-traffic-interception")):
            code, facts = self.run_main(*argv)
            self.assertEqual((code, facts["result"], facts["reason"]), (2, "refused", "proxy_too_old"))
        self.assertFalse(pi_plugin.proxy_config().exists())
        self.assertFalse(self.models.exists())
        code, facts = self.run_main("setup", "--plan", "--cache-strategy", "none")
        self.assertEqual((code, facts["result"]), (0, "planned"))
        self.assertNotIn("quota", facts["consent_question"])


GATEWAY = {"anthropic": "https://gw.example/anthropic", "openai": "https://gw.example/openai/v1"}


class ChainMode(Sandbox):
    """A models.json that already sends a provider to another endpoint (a company gateway)."""

    setup_on = Controller.setup_on
    uninstall = Controller.uninstall

    def write_models(self, providers):
        self.models.parent.mkdir(parents=True, exist_ok=True)
        self.document = {"providers": providers}
        self.models.write_text(json.dumps(self.document))

    def plan(self, *extra):
        with mock.patch.object(pi_plugin, "healthy", return_value=False), \
                mock.patch.object(pi_plugin, "port_free", return_value=True), \
                mock.patch.object(pi_plugin, "first_free_port", return_value=8800):
            return self.run_main("setup", "--plan", *extra)

    def test_the_upstream_reproduces_the_path_pi_sends_today(self):
        # Pi: anthropic baseUrl + /v1/messages; openai baseUrl + /responses.
        # Proxy: upstream + /v1/messages; upstream + /v1/responses.
        cases = [("anthropic", "https://gw.example/anthropic", "https://gw.example/anthropic"),
                 ("anthropic", "https://gw.example/", "https://gw.example"),
                 ("anthropic", "http://10.0.0.5:4000/v1", "http://10.0.0.5:4000/v1"),
                 ("openai", "https://gw.example/openai/v1", "https://gw.example/openai"),
                 ("openai", "https://gw.example/v1/", "https://gw.example")]
        for provider, url, want in cases:
            self.assertEqual(pi_plugin.chain_upstream(provider, url, 8800), (want, None), url)
            pi_path = {"anthropic": "/v1/messages", "openai": "/responses"}[provider]
            proxy_path = {"anthropic": "/v1/messages", "openai": "/v1/responses"}[provider]
            self.assertEqual(want + proxy_path, url.rstrip("/") + pi_path)

    def test_a_url_the_proxy_cannot_reproduce_is_refused(self):
        cases = [("openai", "https://gw.example/openai", "openai_path_not_v1"),
                 ("anthropic", "$GATEWAY", "not_an_http_url"),
                 ("anthropic", "ftp://gw.example", "not_an_http_url"),
                 ("anthropic", "https://user:secret@gw.example", "credentials_in_url"),
                 ("anthropic", "https://gw.example/a?key=1", "query_in_url"),
                 ("anthropic", "http://127.0.0.1:8800/anthropic", "points_at_this_proxy")]
        for provider, url, reason in cases:
            self.assertEqual(pi_plugin.chain_upstream(provider, url, 8800), (None, reason), url)

    def test_plan_on_a_conflict_prints_paired_answers_and_no_plain_question(self):
        self.write_models({"anthropic": {"baseUrl": GATEWAY["anthropic"], "headers": {"x-gw": "1"}}})
        code, facts = self.plan()
        self.assertEqual((code, facts["result"], facts["reason"], facts["conflicts"]),
                         (0, "needs_decision", "base_url_already_set", "anthropic"))
        self.assertNotIn("consent_question", facts)
        self.assertIn("forwarding to https://gw.example/anthropic", facts["consent_question_chain"])
        self.assertIn("forwarding to api.openai.com", facts["consent_question_chain"])
        self.assertIn("api.anthropic.com in place of https://gw.example/anthropic", facts["consent_question_replace"])
        self.assertIn("usually breaks the auth", facts["consent_question_replace"])
        self.assertTrue(facts["confirm_command_chain"].endswith("--i-consent-to-traffic-interception --on-conflict chain"))
        self.assertTrue(facts["confirm_command_replace"].endswith("--on-conflict replace"))
        self.assertEqual(json.loads(self.models.read_text()), self.document)  # the plan wrote nothing

    def test_plan_carries_the_cache_strategy_into_both_commands(self):
        self.write_models({"openai": {"baseUrl": GATEWAY["openai"]}})
        _, facts = self.plan("--cache-strategy", "none")
        for choice in ("chain", "replace"):
            self.assertIn(f"--on-conflict {choice} --cache-strategy none", facts[f"confirm_command_{choice}"])

    def test_plan_offers_no_chain_for_a_url_it_cannot_reproduce(self):
        self.write_models({"openai": {"baseUrl": "https://gw.example/openai"}})
        _, facts = self.plan()
        self.assertEqual(facts["chain_refused"], "openai:openai_path_not_v1")
        self.assertNotIn("consent_question_chain", facts)
        self.assertNotIn("confirm_command_chain", facts)
        self.assertIn("consent_question_replace", facts)

    def test_without_a_choice_setup_still_refuses_and_changes_nothing(self):
        self.write_models({"anthropic": {"baseUrl": GATEWAY["anthropic"]}})
        code, facts = self.setup_on(8800)
        self.assertEqual((code, facts["result"], facts["reason"]), (2, "refused", "base_url_already_set"))
        self.assertIsNone(self.started)
        self.assertEqual(json.loads(self.models.read_text()), self.document)
        self.assertFalse(pi_plugin.proxy_config().exists())

    def test_abort_changes_nothing(self):
        self.write_models({"anthropic": {"baseUrl": GATEWAY["anthropic"]}})
        code, facts = self.setup_on(8800, (), "--on-conflict", "abort")
        self.assertEqual((code, facts["result"]), (0, "unchanged"))
        self.assertIsNone(self.started)
        self.assertEqual(json.loads(self.models.read_text()), self.document)

    def test_chain_puts_the_gateway_behind_the_proxy_and_uninstall_puts_it_back(self):
        self.write_models({"anthropic": {"baseUrl": GATEWAY["anthropic"], "headers": {"x-gw": "1"}, "apiKey": "$GW_KEY"},
                           "openai": {"baseUrl": GATEWAY["openai"], "headers": {"x-gw": "2"}}})
        code, facts = self.setup_on(8800, (), "--on-conflict", "chain")
        self.assertEqual((code, facts["result"]), (0, "routed"))
        want = {"anthropic": "https://gw.example/anthropic", "openai": "https://gw.example/openai"}
        self.assertEqual(self.started, want)
        self.assertEqual(facts["routes"], "anthropic -> proxy -> https://gw.example/anthropic; "
                                          "openai -> proxy -> https://gw.example/openai/v1")
        providers = json.loads(self.models.read_text())["providers"]
        self.assertEqual(providers["anthropic"], {"baseUrl": "http://127.0.0.1:8800/anthropic",
                                                  "headers": {"x-gw": "1"}, "apiKey": "$GW_KEY"})
        self.assertEqual(providers["openai"], {"baseUrl": "http://127.0.0.1:8800/openai/v1", "headers": {"x-gw": "2"}})
        self.assertEqual(pi_plugin.read_record()["upstreams"], want)
        config = pi_plugin.proxy_config().read_text()
        self.assertIn("# chained: anthropic -> proxy -> https://gw.example/anthropic (--anthropic-upstream", config)
        self.assertIn("# chained: openai -> proxy -> https://gw.example/openai (--openai-upstream", config)
        command = pi_plugin.proxy_command("/bin/proxy", 8800, want)
        self.assertEqual(command[-4:], ["--anthropic-upstream", "https://gw.example/anthropic",
                                        "--openai-upstream", "https://gw.example/openai"])
        _, removed = self.uninstall()
        self.assertEqual(removed["models_restore"], "restored")
        self.assertEqual(json.loads(self.models.read_text()), self.document)

    def test_a_conflict_on_one_provider_chains_only_that_provider(self):
        self.write_models({"openai": {"baseUrl": GATEWAY["openai"]}})
        code, facts = self.setup_on(8800, (), "--on-conflict", "chain")
        self.assertEqual((code, facts["result"]), (0, "routed"))
        self.assertEqual(self.started, {"openai": "https://gw.example/openai"})
        self.assertIn("anthropic -> proxy -> api.anthropic.com", facts["routes"])
        for provider in pi_plugin.PROVIDERS:
            self.assertTrue(models_route.is_routed(self.models, 8800, provider))
        self.assertEqual(self.uninstall()[1]["models_restore"], "restored")
        self.assertEqual(json.loads(self.models.read_text()), self.document)

    def test_replace_goes_to_the_public_api_and_uninstall_puts_the_gateway_back(self):
        self.write_models({"anthropic": {"baseUrl": GATEWAY["anthropic"]}})
        code, facts = self.setup_on(8800, (), "--on-conflict", "replace")
        self.assertEqual((code, facts["result"]), (0, "routed"))
        self.assertEqual(self.started, {})
        self.assertEqual(pi_plugin.read_record()["upstreams"], {})
        self.assertNotIn("chained", pi_plugin.proxy_config().read_text())
        self.assertTrue(models_route.is_routed(self.models, 8800, "anthropic"))
        self.assertEqual(self.uninstall()[1]["models_restore"], "restored")
        self.assertEqual(json.loads(self.models.read_text()), self.document)

    def test_chain_refuses_a_url_it_cannot_reproduce_and_changes_nothing(self):
        self.write_models({"openai": {"baseUrl": "https://gw.example/openai"}})
        code, facts = self.setup_on(8800, (), "--on-conflict", "chain")
        self.assertEqual((code, facts["reason"], facts["chain_refused"]),
                         (2, "base_url_not_chainable", "openai:openai_path_not_v1"))
        self.assertIsNone(self.started)
        self.assertEqual(json.loads(self.models.read_text()), self.document)

    def test_a_rerun_and_a_configure_keep_the_upstream(self):
        self.write_models({"anthropic": {"baseUrl": GATEWAY["anthropic"]}})
        self.setup_on(8800, (), "--on-conflict", "chain")
        # A second setup sees our own URL, so no question, and the upstream stays.
        _, plan = self.plan()
        self.assertEqual(plan["result"], "planned")
        self.assertIn("forwarding to https://gw.example/anthropic", plan["consent_question"])
        code, _ = self.setup_on(8800)
        self.assertEqual((code, self.started), (0, {"anthropic": "https://gw.example/anthropic"}))
        with mock.patch.object(pi_plugin, "start_service", return_value=True) as start:
            code, facts = self.run_main("configure", "--preset", "medium")
        self.assertEqual((code, facts["result"]), (0, "configured"))
        self.assertEqual(start.call_args.args[2], {"anthropic": "https://gw.example/anthropic"})
        config = pi_plugin.proxy_config().read_text()
        self.assertIn("preset: medium\n", config)
        self.assertIn("# chained: anthropic -> proxy -> https://gw.example/anthropic", config)
        self.assertEqual(self.uninstall()[1]["models_restore"], "restored")
        self.assertEqual(json.loads(self.models.read_text()), self.document)

    def test_status_shows_the_chain(self):
        self.write_models({"anthropic": {"baseUrl": GATEWAY["anthropic"]}})
        self.setup_on(8800, (), "--on-conflict", "chain")
        with mock.patch.object(pi_plugin, "healthy", return_value=False):
            _, facts = self.run_main("status")
        self.assertEqual(facts["routes"], "anthropic -> proxy -> https://gw.example/anthropic; "
                                          "openai -> proxy -> api.openai.com")

    def test_a_gateway_set_after_our_route_is_what_uninstall_puts_back(self):
        """Routed first; then the user points the key at a gateway; then chain. Uninstall restores the
        gateway, not the value from before the first setup."""
        self.write_models({"anthropic": {"headers": {"x": "y"}}})
        self.setup_on(8800)
        doc = json.loads(self.models.read_text())
        doc["providers"]["anthropic"]["baseUrl"] = GATEWAY["anthropic"]
        self.models.write_text(json.dumps(doc))
        code, _ = self.setup_on(8800, (), "--on-conflict", "chain")
        self.assertEqual(code, 0)
        self.uninstall()
        self.assertEqual(json.loads(self.models.read_text())["providers"]["anthropic"],
                         {"headers": {"x": "y"}, "baseUrl": GATEWAY["anthropic"]})

    def test_an_old_port_of_ours_is_repointed_not_chained(self):
        """Review note from #977: our own recorded URL on an old port is ours, never a gateway."""
        self.write_models({})
        self.setup_on(8800)
        code, facts = self.setup_on(8801, (8800,))
        self.assertEqual((code, facts["result"], facts["port"]), (0, "routed", "8801"))
        self.assertEqual(self.started, {})

    def test_the_chain_question_warns_that_pings_lack_the_gateways_headers(self):
        self.write_models({"anthropic": {"baseUrl": GATEWAY["anthropic"], "headers": {"x-corp-tenant": "a"}},
                           "openai": {"baseUrl": GATEWAY["openai"]}})
        _, facts = self.plan()
        chain = facts["consent_question_chain"]
        self.assertIn("Anthropic sets headers in models.json", chain)
        self.assertIn("standard auth headers (Authorization, x-api-key)", chain)
        self.assertIn("keep-alive may stop for Anthropic", chain)
        self.assertIn("/context-guru cache-strategy none", chain)
        self.assertNotIn("OpenAI sets headers", chain)
        self.assertNotIn("sets headers", facts["consent_question_replace"])
        # Pings spend quota at the gateway, not at the provider.
        self.assertIn("at the endpoint it is chained to", chain)
        self.assertNotIn("your own quota on each provider", chain)
        self.assertIn("your own quota on each provider", facts["consent_question_replace"])
        # No pings, no warning.
        _, quiet = self.plan("--cache-strategy", "none")
        self.assertNotIn("sets headers", quiet["consent_question_chain"])

    def test_two_providers_with_headers_read_as_plural_and_are_named(self):
        """Review notes from #1010: "set", not "sets", and the chained wording names the providers."""
        self.write_models({"anthropic": {"baseUrl": GATEWAY["anthropic"], "headers": {"x-corp-tenant": "a"}},
                           "openai": {"baseUrl": GATEWAY["openai"], "headers": {"x-corp-tenant": "o"}}})
        _, facts = self.plan()
        chain = facts["consent_question_chain"]
        self.assertIn("Anthropic and OpenAI set headers in models.json", chain)
        self.assertIn("for Anthropic and OpenAI, at the endpoint it is chained to", chain)

    def test_systemd_quoting_keeps_percent_and_dollar_literal(self):
        self.assertEqual(pi_plugin._systemd_quote('https://gw.example/team%b/$HOME"\\'),
                         '"https://gw.example/team%%b/$$HOME\\"\\\\"')
        # Only Exec lines expand `$`; other settings resolve `%` specifiers alone.
        self.assertEqual(pi_plugin._systemd_quote("append:/a%h/$x", exec_line=False), '"append:/a%%h/$x"')

    def test_the_systemd_unit_carries_the_upstream_literally(self):
        self.write_models({"anthropic": {"baseUrl": "https://gw.example/team%b$x"}})
        with mock.patch.object(pi_plugin, "service_kind", return_value="systemd"), \
                mock.patch.object(pi_plugin.subprocess, "run"), \
                mock.patch.object(pi_plugin, "healthy", return_value=True):
            self.assertTrue(pi_plugin.start_service("/bin/proxy", 8800, {"anthropic": "https://gw.example/team%b$x"}))
        unit = pi_plugin.systemd_unit().read_text()
        self.assertIn('"--anthropic-upstream" "https://gw.example/team%%b$$x"', unit)

    def test_reset_hatch_puts_the_gateway_back(self):
        self.write_models({"anthropic": {"baseUrl": GATEWAY["anthropic"], "headers": {"x-gw": "1"}}})
        self.setup_on(8800, (), "--on-conflict", "chain")
        out = subprocess.run(["sh", str(pi_plugin.reset_hatch()), "--yes"], capture_output=True, text=True,
                             env=dict(os.environ))
        self.assertEqual(out.returncode, 0, out.stderr)
        self.assertIn("models_restore=restored", out.stdout)
        self.assertEqual(json.loads(self.models.read_text()), self.document)


if __name__ == "__main__":
    unittest.main()
