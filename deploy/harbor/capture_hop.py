"""A capture-only hop that records the request context-guru actually SENT, when the provider rejects it.

WHY THIS EXISTS, AND WHY THE EXISTING SHIM COULD NOT DO IT. `loca_repair_shim.py` sits UPSTREAM of
context-guru (LOCA -> shim -> cg-proxy -> gateway), so it observes the request before compaction. When
the provider rejects a request because a component produced an invalid message list, the shim's copy is
the innocent one. It also *repairs* tool pairing, which would mask exactly the defect under
investigation.

So this hop goes on the other side:

    LOCA -> repair shim -> cg-proxy -> [THIS HOP] -> gateway

It repairs nothing and changes nothing. On a >=400 response it records a STRUCTURAL DIGEST of the
outgoing message list -- per message: index, role, the tool_use ids it declares, and the tool_result
ids it answers -- plus the provider's error. That digest is what identifies an orphaned pair; the raw
bodies are hundreds of kilobytes and mostly irrelevant, so only a bounded head is kept.

Provoked by: `summarize` producing `messages.1: tool_use ids were found without tool_result blocks
immediately after` on 28 of 75 live runs, a FOURTH shape defect in a component that already had three
fixed (docs/experiments/loca/iter011/). Reasoning from the source did not converge -- both splice sites
appear to align the span boundary -- so the message list itself has to be read.
"""
import json
import os
import sys
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

UPSTREAM = os.environ["CAPTURE_UPSTREAM"].rstrip("/")
FAILLOG = os.environ.get("CAPTURE_FAILLOG", "/tmp/cg-loca/capture-failures.jsonl")
PORT = int(os.environ.get("CAPTURE_PORT", "4270"))

# Same set as forever/http_utils.py REQUEST_STRIP, for the reason documented in
# loca_repair_shim.py: this hop re-frames with an explicit content-length, so forwarding
# transfer-encoding alongside it is a protocol violation.
_STRIP = {"connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te",
          "trailer", "trailers", "transfer-encoding", "upgrade", "host", "content-length",
          "accept-encoding"}


def digest(payload):
    """Per-message structure: what each message declares and what it answers."""
    out = []
    for i, m in enumerate(payload.get("messages") or []):
        uses, results, kinds = [], [], []
        c = m.get("content")
        blocks = c if isinstance(c, list) else ([] if c is None else [{"type": "text"}])
        for b in blocks:
            if not isinstance(b, dict):
                continue
            t = b.get("type")
            kinds.append(t)
            if t == "tool_use":
                uses.append(b.get("id"))
            elif t == "tool_result":
                results.append(b.get("tool_use_id"))
        out.append({"i": i, "role": m.get("role"), "types": kinds,
                    "tool_use": uses, "tool_result": results})
    return out


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        if (self.headers.get("transfer-encoding") or "").lower() == "chunked":
            chunks = []
            while True:
                line = self.rfile.readline(65536).strip()
                if not line:
                    continue
                n = int(line.split(b";")[0], 16)
                if n == 0:
                    while True:
                        if self.rfile.readline(65536) in (b"\r\n", b"\n", b""):
                            break
                    break
                chunks.append(self.rfile.read(n))
                self.rfile.read(2)
            raw = b"".join(chunks)
        else:
            raw = self.rfile.read(int(self.headers.get("content-length") or 0))

        req = urllib.request.Request(UPSTREAM + self.path, data=raw, method="POST")
        for k, v in self.headers.items():
            if k.lower() not in _STRIP:
                req.add_header(k, v)
        req.add_header("content-length", str(len(raw)))
        try:
            with urllib.request.urlopen(req, timeout=900) as r:
                data, status, hdrs = r.read(), r.status, dict(r.headers)
        except urllib.error.HTTPError as e:
            data, status, hdrs = e.read(), e.code, dict(e.headers)
        except Exception as e:
            data, status, hdrs = json.dumps({"error": str(e)}).encode(), 502, {}

        if status >= 400:
            try:
                payload = json.loads(raw)
                snap = {"status": status, "body_bytes": len(raw),
                        # Stamped by loca_repair_shim.py so this record can be joined to the
                        # request the proxy was GIVEN, not just the one it produced.
                        "rig_seq": self.headers.get("x-cg-rig-seq"),
                        "n_messages": len(payload.get("messages") or []),
                        "system_present": "system" in payload,
                        "digest": digest(payload),
                        "error": data[:600].decode("utf-8", "replace")}
                with open(FAILLOG, "a") as fh:
                    fh.write(json.dumps(snap) + "\n")
            except Exception as e:
                try:
                    with open(FAILLOG, "a") as fh:
                        fh.write(json.dumps({"status": status,
                                             "capture_error": f"{type(e).__name__}: {e}"}) + "\n")
                except Exception:
                    pass
        self.send_response(status)
        self.send_header("content-type", hdrs.get("Content-Type", "application/json"))
        self.send_header("content-length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.path == "/capture-stats":
            n = 0
            if os.path.exists(FAILLOG):
                with open(FAILLOG) as fh:
                    n = sum(1 for _ in fh)
            body = json.dumps({"captured": n}).encode()
            self.send_response(200)
            self.send_header("content-length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        else:
            self.send_response(404)
            self.end_headers()

    def log_message(self, *a):
        pass


if __name__ == "__main__":
    print(f"[capture] :{PORT} -> {UPSTREAM}  faillog={FAILLOG}", flush=True)
    sys.stdout.flush()
    ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
