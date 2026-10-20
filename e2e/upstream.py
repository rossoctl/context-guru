"""A fake Anthropic /v1/messages for the e2e review: deterministic replies, no model, no key.

Usage comes from a header the replay driver sets, so token counts are the manifest's and repeat
exactly between runs. Streams can be cut short to exercise the proxy's capture of an incomplete SSE."""
from __future__ import annotations

import argparse
import http.server
import json
import sys


def _usage(h) -> tuple:
    raw = h.get("X-E2E-Usage", "1,0,0,1")
    a, b, c, d = (int(x) for x in raw.split(","))
    return a, b, c, d


class _H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):  # quiet; the phase log captures the proxy, not this
        pass

    def do_POST(self):
        n = int(self.headers.get("content-length", 0))
        req = json.loads(self.rfile.read(n) or b"{}")
        fresh, cread, cwrite, out = _usage(self.headers)
        usage = {"input_tokens": fresh, "cache_read_input_tokens": cread,
                 "cache_creation_input_tokens": cwrite, "output_tokens": out}
        msg = {"id": "msg_e2e", "type": "message", "role": "assistant", "model": req.get("model", "m"),
               "content": [{"type": "text", "text": "ok"}], "stop_reason": "end_turn", "usage": usage}
        if not req.get("stream"):
            body = json.dumps(msg).encode()
            self.send_response(200)
            self.send_header("content-type", "application/json")
            self.send_header("content-length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        events = [
            ("message_start", {"type": "message_start", "message": {**msg, "content": [],
                               "usage": {**usage, "output_tokens": 0}}}),
            ("content_block_start", {"type": "content_block_start", "index": 0,
                                     "content_block": {"type": "text", "text": ""}}),
            ("content_block_delta", {"type": "content_block_delta", "index": 0,
                                     "delta": {"type": "text_delta", "text": "ok"}}),
            ("content_block_stop", {"type": "content_block_stop", "index": 0}),
            ("message_delta", {"type": "message_delta", "delta": {"stop_reason": "end_turn"},
                               "usage": {"output_tokens": out}}),
            ("message_stop", {"type": "message_stop"}),
        ]
        if self.headers.get("X-E2E-Truncate") == "1":
            events = events[:3]
        payload = "".join(f"event: {e}\ndata: {json.dumps(d)}\n\n" for e, d in events).encode()
        self.send_response(200)
        self.send_header("content-type", "text/event-stream")
        self.send_header("content-length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


def serve(port: int) -> http.server.ThreadingHTTPServer:
    return http.server.ThreadingHTTPServer(("127.0.0.1", port), _H)


def main(argv=None) -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=9000)
    serve(ap.parse_args(argv).port).serve_forever()
    return 0


if __name__ == "__main__":
    sys.exit(main())
