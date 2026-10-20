import json
import threading
import unittest
import urllib.request

from e2e import upstream


class TestUpstream(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.srv = upstream.serve(0)
        cls.port = cls.srv.server_address[1]
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()

    def _post(self, body, headers=None):
        req = urllib.request.Request(f"http://127.0.0.1:{self.port}/v1/messages",
                                     data=json.dumps(body).encode(), method="POST",
                                     headers={"content-type": "application/json", **(headers or {})})
        with urllib.request.urlopen(req, timeout=5) as r:
            return r.headers.get("content-type"), r.read().decode()

    def test_plain_reply_carries_the_requested_usage(self):
        ct, body = self._post({"model": "m", "messages": []}, {"X-E2E-Usage": "7,100,20,33"})
        self.assertIn("json", ct)
        u = json.loads(body)["usage"]
        self.assertEqual((u["input_tokens"], u["cache_read_input_tokens"],
                          u["cache_creation_input_tokens"], u["output_tokens"]), (7, 100, 20, 33))

    def test_stream_reply_is_sse_and_ends_with_message_stop(self):
        ct, body = self._post({"model": "m", "messages": [], "stream": True}, {"X-E2E-Usage": "1,2,3,4"})
        self.assertIn("event-stream", ct)
        self.assertTrue(body.rstrip().endswith('{"type": "message_stop"}'))
        self.assertIn('"output_tokens": 4', body)

    def test_upstream_can_truncate_a_stream(self):
        _, body = self._post({"model": "m", "messages": [], "stream": True},
                             {"X-E2E-Usage": "1,2,3,4", "X-E2E-Truncate": "1"})
        self.assertNotIn("message_stop", body)
        self.assertIn("content_block_delta", body)


if __name__ == "__main__":
    unittest.main()
