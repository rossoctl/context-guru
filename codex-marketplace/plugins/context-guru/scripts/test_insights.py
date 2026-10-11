import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

HERE = Path(__file__).resolve().parent
CLAUDE_ORIGINAL = HERE.parents[3] / "context-guru-plugin/scripts/insights.py"


class Stub(BaseHTTPRequestHandler):
    def do_GET(self):
        body = b"ok" if self.path == "/healthz" else json.dumps({"requests": 0}).encode()
        self.send_response(200 if self.path in ("/healthz", "/api/stats") else 404)
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


class InsightsTest(unittest.TestCase):
    @unittest.skipUnless(CLAUDE_ORIGINAL.is_file(), "sparse checkout: Claude plugin not present")
    def test_core_is_a_verbatim_copy_of_the_claude_insights(self):
        # Refresh with: cp context-guru-plugin/scripts/insights.py <this dir>/insights_core.py
        self.assertEqual((HERE / "insights_core.py").read_bytes(), CLAUDE_ORIGINAL.read_bytes())

    def test_empty_store_reports_in_codex_terms_only(self):
        server = HTTPServer(("127.0.0.1", 0), Stub)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        with tempfile.TemporaryDirectory() as state:
            root = Path(state) / "context-guru-codex"
            root.mkdir()
            (root / "install.json").write_text(json.dumps({"port": server.server_port}))
            env = dict(os.environ, XDG_STATE_HOME=state, HOME=state,
                       ANTHROPIC_BASE_URL="https://unrelated.example")
            out = subprocess.run([sys.executable, str(HERE / "insights.py"), "all"], env=env,
                                 capture_output=True, text=True, timeout=60)
        server.shutdown()
        self.assertEqual(out.returncode, 0, out.stderr)
        self.assertIn("proxy_up=true", out.stdout)
        self.assertIn("finding.1.id=no-traffic", out.stdout)
        self.assertIn("session_routed=false", out.stdout)
        for claude_only in ("/context-guru:", "CLAUDE_PLUGIN", "Claude Code", "ANTHROPIC", "unrelated"):
            self.assertNotIn(claude_only, out.stdout)

    def test_not_installed_makes_no_request_and_says_so(self):
        with tempfile.TemporaryDirectory() as state:
            env = dict(os.environ, XDG_STATE_HOME=state, HOME=state)
            out = subprocess.run([sys.executable, str(HERE / "insights.py"), "all"], env=env,
                                 capture_output=True, text=True, timeout=60)
        self.assertIn("finding.1.id=no-install", out.stdout)
        self.assertIn("$context-guru-setup", out.stdout)


if __name__ == "__main__":
    unittest.main()
