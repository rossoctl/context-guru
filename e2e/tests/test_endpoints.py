import http.server
import json
import pathlib
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


class TestStressEndpointTimings(unittest.TestCase):
    """A stress timing is recorded only for a 200; anything else crashes the phase instead of logging 0 ms."""

    def _run(self, files):
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as r:
            for f in files:
                (pathlib.Path(root) / f).parent.mkdir(parents=True, exist_ok=True)
                (pathlib.Path(root) / f).write_text("{}")
            h = lambda *a, **k: http.server.SimpleHTTPRequestHandler(*a, directory=root, **k)
            srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), h)
            srv.RequestHandlerClass.log_message = lambda *a: None
            threading.Thread(target=srv.serve_forever, daemon=True).start()
            try:
                p = subprocess.run(["bash", "-c", f'R={r}; BASE=http://127.0.0.1:{srv.server_address[1]}\n{_fn("p_endpoints")}\np_endpoints'],
                                   capture_output=True, text=True, timeout=60)
            finally:
                srv.shutdown(); srv.server_close()
            out = pathlib.Path(r) / "stress-endpoints.jsonl"
            return p, [json.loads(l) for l in out.read_text().splitlines()] if out.exists() else []

    def test_every_endpoint_answering_200_is_timed(self):
        p, rows = self._run(["api/history", "api/facets", "api/insights"])
        self.assertEqual((p.returncode, [r["endpoint"] for r in rows]),
                         (0, ["/api/history", "/api/facets?measured=1", "/api/insights?timeline=1"]))
        self.assertTrue(all(isinstance(r["ms"], float) for r in rows))

    def test_a_non_200_crashes_the_phase_and_records_no_timing(self):
        p, rows = self._run(["api/history"])  # facets 404s
        self.assertEqual((p.returncode, [r["endpoint"] for r in rows]), (1, ["/api/history"]))
        self.assertIn("/api/facets?measured=1 answered '404'", p.stdout)


if __name__ == "__main__":
    unittest.main()
