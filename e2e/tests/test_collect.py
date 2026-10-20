import json
import threading
import unittest
import http.server

from e2e import collect


class _Fake(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def do_GET(self):
        body = json.dumps({"path": self.path}).encode()
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


class TestCollect(unittest.TestCase):
    def test_queries_every_root_scoped_and_pooled(self):
        srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _Fake)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        try:
            got = collect.collect(f"http://127.0.0.1:{srv.server_address[1]}",
                                  {"cleanup": {"picker": ["/sandbox/work/alpha"]},
                                   "stats": {"requests": {"/sandbox/work/alpha": 3}}},
                                  skill_cmd=None)
        finally:
            srv.shutdown()
        self.assertIn("", got["insights"], "pooled view collected")
        self.assertIn("project=%2Fsandbox%2Fwork%2Falpha", got["insights"]["/sandbox/work/alpha"]["path"])
        self.assertIn("measured=1", got["facets_measured"]["path"])


if __name__ == "__main__":
    unittest.main()
