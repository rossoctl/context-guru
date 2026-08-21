#!/usr/bin/env python3
"""Rig-side tool-pairing repair, sitting between LOCA-bench and the context-guru proxy.

WHY THIS EXISTS. LOCA's native context trimmer fires when a request outgrows
--max-context-size and drops whole messages. Anthropic requires every assistant `tool_use`
block to be answered by a `tool_result` in the immediately following user message, so the
trim orphans pairs and the provider returns 400 (observed at the 128k band: 42 orphaned ids
at messages.13). This was predicted in docs/proposals/coref-compaction.md §8, which also
says to port the existing fix rather than rediscover it.

HISTORY -- READ BEFORE EDITING THE BODY-READING CODE. The first version of this shim read the
request body from `content-length` alone and forwarded every client header except a small
denylist. Both were wrong, and they compounded: a request sent with `Transfer-Encoding:
chunked` reached the gateway with an EMPTY body AND with `chunked` and `Content-Length: 0`
set simultaneously. The gateway answered `400 Bad request / Your browser sent an invalid
request` -- raw HTML, no Anthropic error body -- which was misattributed first to a
component under test and then to the benchmark, across two experiment iterations. A local
echo server accepted the contradictory framing with a 200, so a lenient stand-in would not
have caught it. See docs/experiments/loca/iter007/results.md.

`repair_tool_pairing` below is lifted VERBATIM from forever's
forever/benchmarks/_anthropic_auth_hop.py so the two rigs cannot drift.

WHERE IT SITS, AND WHY THAT MATTERS.

    LOCA -> [this shim] -> cg-proxy -> gateway

Before cg-proxy, deliberately: compaction should be measured on WELL-FORMED traffic. If the
shim sat after, coref would index a malformed message list and extract_llm would be handed
requests the provider would reject anyway.

WHAT IT IS NOT. This is not a context-guru feature and must not be mistaken for one. It
repairs the AGENT's malformed history. Note that coref structurally cannot cause this class
of bug: it rewrites a tool message's text in place and never removes a message, so pairing
is preserved. The repair count is reported so its rate is visible rather than silent.
"""
import json
import os
import sys
import threading
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

_TRIMMED = "[tool result unavailable: removed by context management]"


def repair_tool_pairing(messages: list) -> tuple[list, int]:
    """Repair a message history to satisfy Anthropic's tool-block pairing rules, returning the
    repaired list and the number of fixes applied.

    Anthropic requires every assistant ``tool_use`` block to be answered by a ``tool_result``
    (matching ``tool_use_id``) in the immediately following user message, and forbids a
    ``tool_result`` with no preceding ``tool_use``. Naive context management (trimming messages
    or clearing tool results) breaks both — it drops the results message while keeping the
    ``tool_use`` block, or the reverse — yielding an API 400. This restores validity:

    * **orphaned results** (a ``tool_result`` whose ``tool_use`` was removed) are dropped, and a
      message left empty by that removal is dropped too;
    * **unanswered ``tool_use`` blocks** get a synthetic placeholder ``tool_result`` in the next
      user message (created if the following message isn't a suitable user message).

    Pure and idempotent — a well-formed history returns unchanged with count 0. This is a
    rig-level fix applied by the auth hop; Forever's core proxy is never involved."""
    repairs = 0

    # Phase 1 — drop orphaned tool_result blocks and any message emptied by that.
    phase1: list = []
    for idx, m in enumerate(messages):
        if not isinstance(m, dict):
            phase1.append(m)
            continue
        m = dict(m)
        content = m.get("content")
        if isinstance(content, list):
            prior_use = set()
            prev = messages[idx - 1] if idx > 0 else None
            if isinstance(prev, dict) and isinstance(prev.get("content"), list):
                prior_use = {b.get("id") for b in prev["content"]
                             if isinstance(b, dict) and b.get("type") == "tool_use"}
            kept = []
            for b in content:
                if (isinstance(b, dict) and b.get("type") == "tool_result"
                        and b.get("tool_use_id") not in prior_use):
                    repairs += 1
                    continue
                kept.append(b)
            if content and not kept:          # message held only orphaned results → drop it
                continue
            m["content"] = kept
        phase1.append(m)

    # Phase 2 — ensure every assistant tool_use is answered in the next user message.
    out: list = []
    i = 0
    while i < len(phase1):
        m = phase1[i]
        out.append(m)
        content = m.get("content") if isinstance(m, dict) else None
        use_ids = ([b["id"] for b in content
                    if isinstance(b, dict) and b.get("type") == "tool_use" and b.get("id")]
                   if isinstance(content, list) else [])
        if use_ids:
            nxt = phase1[i + 1] if i + 1 < len(phase1) else None
            nxt_user = (isinstance(nxt, dict) and nxt.get("role") == "user"
                        and isinstance(nxt.get("content"), list))
            have = ({b.get("tool_use_id") for b in nxt["content"]
                     if isinstance(b, dict) and b.get("type") == "tool_result"}
                    if nxt_user else set())
            missing = [u for u in use_ids if u not in have]
            if missing:
                synth = [{"type": "tool_result", "tool_use_id": u, "content": _TRIMMED}
                         for u in missing]
                repairs += len(missing)
                if nxt_user:
                    nxt["content"] = synth + list(nxt["content"])
                else:
                    out.append({"role": "user", "content": synth})
        i += 1
    return out, repairs


# Lifted VERBATIM from forever/http_utils.py (HOP_BY_HOP | {host, content-length}), plus
# accept-encoding for the urllib reason documented at the use site.
_HOP_BY_HOP = frozenset({
    "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
    "te", "trailer", "trailers", "transfer-encoding", "upgrade",
})
_REQUEST_STRIP = _HOP_BY_HOP | {"host", "content-length", "accept-encoding"}

UPSTREAM = os.environ.get("SHIM_UPSTREAM", "http://localhost:4200/anthropic")
_repairs = 0
_requests = 0
_lock = threading.Lock()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):  # keep stdout clean; the counters are the output
        pass

    def do_GET(self):
        if self.path == "/shim-stats":
            body = json.dumps({"requests": _requests, "repairs": _repairs}).encode()
            self.send_response(200)
            self.send_header("content-type", "application/json")
            self.send_header("content-length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_error(404)

    def do_POST(self):
        global _repairs, _requests
        # Read the body under EITHER framing. Reading only content-length silently
        # produced an EMPTY body for every chunked request, and the client (httpx) chooses
        # chunked on its own terms -- which is why the resulting failures were intermittent.
        if (self.headers.get("transfer-encoding") or "").lower() == "chunked":
            chunks = []
            while True:
                line = self.rfile.readline(65536).strip()
                if not line:
                    continue
                size = int(line.split(b";")[0], 16)
                if size == 0:
                    while True:                       # consume trailers
                        t = self.rfile.readline(65536)
                        if t in (b"\r\n", b"\n", b""):
                            break
                    break
                chunks.append(self.rfile.read(size))
                self.rfile.read(2)                    # trailing CRLF
            raw = b"".join(chunks)
        else:
            raw = self.rfile.read(int(self.headers.get("content-length") or 0))
        try:
            payload = json.loads(raw)
            msgs = payload.get("messages")
            if isinstance(msgs, list):
                fixed, count = repair_tool_pairing(msgs)
                if count:
                    payload["messages"] = fixed
                    raw = json.dumps(payload).encode()
                    with _lock:
                        _repairs += count
            with _lock:
                _requests += 1
        except Exception as e:  # never break the run over a repair; forward untouched
            print(f"[shim] passthrough after {type(e).__name__}: {e}", file=sys.stderr)

        req = urllib.request.Request(UPSTREAM + self.path.split("/anthropic", 1)[-1]
                                    if "/anthropic" in self.path else UPSTREAM + self.path,
                                    data=raw, method="POST")
        for k, v in self.headers.items():
            # Hop-by-hop + transport-owned headers, lifted VERBATIM from forever's
            # forever/http_utils.py REQUEST_STRIP (= HOP_BY_HOP | {host, content-length}) so the
            # two rigs cannot drift -- the same reason repair_tool_pairing is copied verbatim.
            # My original ad-hoc list had four entries and omitted transfer-encoding, which is
            # what produced the empty-body 400s; see the HISTORY note above.
            #
            # DEVIATION, deliberate: accept-encoding is end-to-end and forever forwards it. This
            # hop strips it because urllib does not transparently decompress, so forwarding
            # `gzip` would relay compressed bytes as though they were plain. forever needs no
            # such deviation because httpx handles content-coding for it.
            if k.lower() not in _REQUEST_STRIP:
                req.add_header(k, v)
        req.add_header("content-length", str(len(raw)))
        try:
            with urllib.request.urlopen(req, timeout=900) as r:
                data, status, hdrs = r.read(), r.status, dict(r.headers)
        except urllib.error.HTTPError as e:
            data, status, hdrs = e.read(), e.code, dict(e.headers)
        except Exception as e:
            data, status, hdrs = json.dumps({"error": str(e)}).encode(), 502, {}

        # FAILURE CAPTURE. The `400 Bad request / Your browser sent an invalid request` HTML error
        # survived the chunked-body fix: 1 occurrence in 75 runs with no proxy in the path, 6 in 75
        # with one. It is intermittent and rare, so the only way to attribute it is to record the
        # request that caused it AT THE MOMENT it fails -- reconstructing afterwards is what made
        # this take three iterations. Body goes to a side file, not stderr, because it is large and
        # stderr is the run log.
        if status >= 400:
            try:
                snap = {"status": status, "path": self.path, "body_bytes": len(raw),
                        "sent_content_length": str(len(raw)),
                        "req_headers": {k: v for k, v in self.headers.items()
                                        if k.lower() not in ("authorization", "x-api-key")},
                        "resp_head": data[:400].decode("utf-8", "replace"),
                        "resp_headers": {k: v for k, v in hdrs.items()},
                        "upstream": UPSTREAM}
                with open(os.environ.get("SHIM_FAILLOG", "/tmp/cg-loca/shim-failures.jsonl"),
                          "a") as fh:
                    fh.write(json.dumps(snap) + "\n")
            except Exception:
                pass    # diagnostics must never break the run
        self.send_response(status)
        self.send_header("content-type", hdrs.get("Content-Type", "application/json"))
        self.send_header("content-length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


if __name__ == "__main__":
    port = int(os.environ.get("SHIM_PORT", "4260"))
    print(f"[shim] :{port} -> {UPSTREAM}", flush=True)
    ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
