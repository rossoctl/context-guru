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
