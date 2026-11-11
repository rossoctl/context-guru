import importlib.util
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

MODULE_PATH = Path(__file__).with_name("insights.py")
SPEC = importlib.util.spec_from_file_location("codex_insights", MODULE_PATH)
INSIGHTS = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(INSIGHTS)


class InsightsTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.env = mock.patch.dict(os.environ, {"XDG_STATE_HOME": self.temp.name})
        self.env.start()
        INSIGHTS.state_dir().mkdir(parents=True)
        (INSIGHTS.state_dir() / "install.json").write_text('{"port": 8791}')

    def tearDown(self):
        self.env.stop()
        self.temp.cleanup()

    def test_negative_keepalive_is_ranked_first_with_exact_fix(self):
        stats = {"requests": 9, "savings": {"all": {
            "total_saved_usd": 2.5, "keepalive_net_usd": -0.25}},
            "saved_tokens_unique": 1200}
        with mock.patch.object(INSIGHTS, "get", return_value=stats):
            result = INSIGHTS.report()
        self.assertEqual([f["id"] for f in result["findings"]],
                         ["keepalive-net", "measured-net-savings", "saved-tokens"])
        self.assertEqual(result["findings"][0]["fix"],
                         "python3 ../../scripts/codex_plugin.py configure --cache-strategy none")

    def test_missing_or_malformed_measurements_are_not_invented_as_zero(self):
        with mock.patch.object(INSIGHTS, "get", return_value={"requests": 1,
                                                              "saved_tokens": "unknown"}):
            result = INSIGHTS.report()
        self.assertEqual(result["findings"], [])

    def test_legacy_top_level_savings_remain_supported(self):
        with mock.patch.object(INSIGHTS, "get", return_value={
                "total_saved_usd": 1.0, "keepalive_net_usd": 0.2}):
            result = INSIGHTS.report()
        self.assertEqual({f["id"] for f in result["findings"]},
                         {"measured-net-savings", "keepalive-net"})
        net = next(f for f in result["findings"] if f["id"] == "measured-net-savings")
        self.assertEqual(net["severity"], "ok")
        self.assertEqual(net["fix"], "none")

    def test_focused_capabilities_report_is_deterministic(self):
        responses = {
            "/api/stats": {"requests": 3},
            "/api/tools": {"tools": [
                {"name": "used", "calls": 2},
                {"name": "unused", "calls": 0, "fix": "Disable unused."},
            ]},
        }
        with mock.patch.object(INSIGHTS, "get", side_effect=lambda _port, path: responses[path]):
            result = INSIGHTS.report("capabilities")
        self.assertEqual([f["id"] for f in result["findings"]],
                         ["unused-capability-unused"])
        self.assertEqual(result["findings"][0]["fix"], "Disable unused.")

    def test_null_capabilities_are_an_empty_measurement_not_a_crash(self):
        responses = {"/api/stats": {"requests": 0}, "/api/tools": {"tools": None}}
        with mock.patch.object(INSIGHTS, "get", side_effect=lambda _port, path: responses[path]):
            self.assertEqual(INSIGHTS.report("capabilities")["findings"], [])

    def test_focused_component_report_uses_measured_tokens(self):
        stats = {"requests": 3, "components": {
            "dedup": {"saved_tokens_unique": 42},
            "unknown": {"saved_tokens": "missing"},
        }}
        with mock.patch.object(INSIGHTS, "get", return_value=stats):
            result = INSIGHTS.report("components")
        self.assertEqual([f["id"] for f in result["findings"]], ["component-dedup"])
        self.assertEqual(result["findings"][0]["tokens"], 42)


if __name__ == "__main__":
    unittest.main()
