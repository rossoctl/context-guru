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


class Controller(Sandbox):
    def test_plan_writes_nothing_and_asks_one_question(self):
        code, facts = self.run_main("setup", "--plan")
        self.assertEqual(code, 0)
        self.assertEqual(facts["result"], "planned")
        self.assertIn("your own quota", facts["consent_question"])
        self.assertIn(str(self.models), facts["consent_question"])
        self.assertFalse(self.models.exists())
        self.assertFalse(pi_plugin.state_dir().exists())

    def test_plan_with_strategy_none_does_not_mention_spend(self):
        _, facts = self.run_main("setup", "--plan", "--cache-strategy", "none")
        self.assertNotIn("quota", facts["consent_question"])

    def test_plan_reports_a_foreign_base_url(self):
        self.models.parent.mkdir(parents=True)
        self.models.write_text(json.dumps({"providers": {"anthropic": {"baseUrl": "https://gw.example"}}}))
        code, facts = self.run_main("setup", "--plan")
        self.assertEqual((code, facts["result"], facts["reason"]), (0, "needs_decision", "base_url_already_set"))

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
        self.assertEqual(record["base_urls"], {"anthropic": f"http://127.0.0.1:{port}/anthropic"})
        self.assertEqual(record["cache_strategy"], "auto")
        self.assertTrue(pi_plugin.reset_hatch().exists())
        self.assertTrue((pi_plugin.state_dir() / "models_route.py").exists())

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


if __name__ == "__main__":
    unittest.main()
