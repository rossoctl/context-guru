import http.server
import json
import pathlib
import re
import subprocess
import tempfile
import threading
import unittest

PHASES = pathlib.Path(__file__).resolve().parents[1] / "phases.sh"


def _fn(name: str) -> str:
    """One function's text out of phases.sh, which cannot be sourced outside the container."""
    lines = PHASES.read_text().splitlines()
    start = next(i for i, l in enumerate(lines) if l.startswith(f"{name}() {{"))
    end = next(i for i in range(start, len(lines)) if lines[i] == "}")
    return "\n".join(lines[start:end + 1])


class _Handler(http.server.BaseHTTPRequestHandler):
    routes: dict = {}  # path without query -> (status, X-Cache value or None)

    def do_GET(self):
        code, xc = self.routes.get(self.path.split("?")[0], (404, None))
        self.send_response(code)
        if xc:
            self.send_header("X-Cache", xc)
        self.send_header("Content-Length", "2")
        self.end_headers()
        self.wfile.write(b"{}")

    def log_message(self, *a):
        pass


class TestStressEndpointTimings(unittest.TestCase):
    """A stress timing is recorded only for an uncached 200; anything else crashes the phase instead of logging
    a timing that measures nothing (0 ms for a refused connection, a cache lookup for a hit)."""

    def _run(self, routes):
        handler = type("H", (_Handler,), {"routes": routes})
        with tempfile.TemporaryDirectory() as r:
            srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
            threading.Thread(target=srv.serve_forever, daemon=True).start()
            try:
                p = subprocess.run(["bash", "-c", f'R={r}; BASE=http://127.0.0.1:{srv.server_address[1]}\n{_fn("p_endpoints")}\np_endpoints'],
                                   capture_output=True, text=True, timeout=60)
            finally:
                srv.shutdown(); srv.server_close()
            out = pathlib.Path(r) / "stress-endpoints.jsonl"
            return p, [json.loads(l) for l in out.read_text().splitlines()] if out.exists() else []

    def test_every_endpoint_answering_200_is_timed(self):
        p, rows = self._run({"/api/history": (200, None), "/api/facets": (200, None), "/api/insights": (200, None)})
        self.assertEqual((p.returncode, [r["endpoint"] for r in rows]),
                         (0, ["/api/history", "/api/facets?measured=1", "/api/insights?timeline=1"]))
        self.assertTrue(all(isinstance(r["ms"], float) for r in rows))

    def test_a_non_200_crashes_the_phase_and_records_no_timing(self):
        p, rows = self._run({"/api/history": (200, None)})  # facets 404s
        self.assertEqual((p.returncode, [r["endpoint"] for r in rows]), (1, ["/api/history"]))
        self.assertIn("/api/facets?measured=1 answered '404'", p.stdout)

    def test_x_cache_is_recorded_when_present(self):
        p, rows = self._run({"/api/history": (200, "miss"), "/api/facets": (200, None), "/api/insights": (200, "stale")})
        self.assertEqual((p.returncode, [r.get("x_cache") for r in rows]), (0, ["miss", None, "stale"]))

    def test_a_cache_hit_crashes_the_phase_and_records_no_timing(self):
        p, rows = self._run({"/api/history": (200, "miss"), "/api/facets": (200, "HIT"), "/api/insights": (200, None)})
        self.assertEqual((p.returncode, [r["endpoint"] for r in rows]), (1, ["/api/history"]))
        self.assertIn("/api/facets?measured=1 answered from cache (X-Cache: hit)", p.stdout)


class TestProxyReadiness(unittest.TestCase):
    def test_the_readiness_probe_hits_no_endpoint_p_endpoints_times(self):
        # A probe on a timed endpoint warms the dashboard's response cache, so the timing would read a hit.
        probe = re.search(r'curl [^\n]*"\$BASE(/[^"]*)"', _fn("start_proxy")).group(1)
        timed = re.search(r"for ep in ([^;]+);", _fn("p_endpoints")).group(1).replace('"', "").split()
        self.assertEqual((probe, probe.split("?")[0] in {t.split("?")[0] for t in timed}), ("/dashboard/", False))


if __name__ == "__main__":
    unittest.main()
