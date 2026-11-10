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
import unittest
from unittest import mock

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import models_route  # noqa: E402
import pi_plugin  # noqa: E402


class Sandbox(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        root = Path(self.tmp.name)
        self.env = mock.patch.dict(os.environ, {
            "HOME": str(root), "XDG_STATE_HOME": str(root / "state"),
            "XDG_CONFIG_HOME": str(root / "config"), "PI_CODING_AGENT_DIR": str(root / "agent")})
        self.env.start()
        self.models = root / "agent/models.json"
        self.state = root / "state/context-guru-pi/routing.json"

    def tearDown(self):
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
        self.assertIn("after 4 minutes idle", question)
        self.assertIn("after 28 minutes idle", question)

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

    def setup_on(self, port, taken=()):
        """Run setup with the service mocked, on `port`, while the ports in `taken` belong to someone else."""
        with mock.patch.object(pi_plugin, "find_binary", return_value="/bin/true"), \
                mock.patch.object(pi_plugin, "service_kind", return_value="launchd"), \
                mock.patch.object(pi_plugin, "start_service", return_value=True), \
                mock.patch.object(pi_plugin, "healthy", return_value=False), \
                mock.patch.object(pi_plugin, "port_free", side_effect=lambda p: p not in taken), \
                mock.patch.object(pi_plugin, "first_free_port", return_value=port):
            return self.run_main("setup", "--i-consent-to-traffic-interception")

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
        self.assertIn("  keepalive: true\n", text)
        self.assertIn("  keepalive_max_usd_per_ping: 0.5\n", text)
        self.assertNotIn("cache:", pi_plugin.render_config("off", "none"))

    def test_config_refuses_a_file_it_did_not_write(self):
        path = pi_plugin.proxy_config()
        path.parent.mkdir(parents=True)
        path.write_text("preset: high\n")
        with self.assertRaises(RuntimeError):
            pi_plugin.write_proxy_config("off", "auto")
        self.assertEqual(path.read_text(), "preset: high\n")

    def test_strategies_match_the_claude_code_plugin(self):
        """One name, one behaviour: each shared strategy sets the same cache keys on both clients."""
        claude = HERE.parents[1] / "context-guru-plugin/scripts/settings.py"
        if not claude.exists():
            self.skipTest("Claude Code plugin not in this checkout")
        script = ("import json, runpy, sys; m = runpy.run_path(sys.argv[1]); "
                  "print(json.dumps({k: v['cache'] for k, v in m['STRATEGIES'].items()}))")
        theirs = json.loads(subprocess.check_output([sys.executable, "-c", script, str(claude)], text=True))
        for name, spec in pi_plugin.STRATEGIES.items():
            self.assertEqual(spec["cache"], theirs[name], name)

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
        code, facts = self.restart("launchd", lambda port: False)
        self.assertEqual((code, facts["result"]), (2, "not_installed"))
        self.assertEqual(self.calls, [])

    def test_a_proxy_that_answers_is_not_restarted(self):
        self.our_plist()
        code, facts = self.restart("launchd", lambda port: True)
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
        code, facts = self.restart("launchd", lambda port: False)
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
        code, facts = self.restart("launchd", lambda port: False)
        self.assertEqual((code, facts["result"], facts["service"]), (1, "down", "not_owned"))
        self.assertEqual(self.calls, [])

    def test_no_service_file_means_no_restart(self):
        code, facts = self.restart("launchd", lambda port: False)
        self.assertEqual((code, facts["service"]), (1, "no_service"))
        self.assertEqual(self.calls, [])

    def test_systemd_restarts_our_unit(self):
        self.our_unit()
        code, facts = self.restart("systemd", [False, True])
        self.assertEqual((code, facts["result"]), (0, "up"))
        self.assertEqual(self.calls, [["systemctl", "--user", "restart", "context-guru-pi.service"]])

    def test_a_unit_we_do_not_own_is_left_alone(self):
        self.our_unit("[Service]\n")
        code, facts = self.restart("systemd", lambda port: False)
        self.assertEqual(facts["service"], "not_owned")
        self.assertEqual(self.calls, [])

    def test_still_down_names_the_reset_hatch_and_the_log(self):
        self.our_unit()
        self.fail = {"--user"}
        code, facts = self.restart("systemd", lambda port: False)
        self.assertEqual((code, facts["result"], facts["service"]), (1, "down", "failed"))
        self.assertEqual(facts["reset_hatch"], str(pi_plugin.reset_hatch()))
        self.assertEqual(facts["log"], str(pi_plugin.state_dir() / "proxy.log"))
        self.assertEqual(facts["port"], "8800")

    def test_an_unsupported_platform_restarts_nothing(self):
        code, facts = self.restart(None, lambda port: False)
        self.assertEqual((code, facts["service"]), (1, "unsupported"))
        self.assertEqual(self.calls, [])


if __name__ == "__main__":
    unittest.main()
