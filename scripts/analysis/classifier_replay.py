#!/usr/bin/env python3
"""Replay captured auto-mode classifier requests to a reference model and a candidate model,
and compare their verdicts (#614).

The classifier is a safety check, so the number that matters is how often the CANDIDATE allows
what the REFERENCE blocks ("unsafe allow"). This script measures it offline, on captured
traffic, before any row in the routing table (#613) points the classifier at a new model.

Input is a CONTEXT_GURU_CAPTURE file: one JSON record per line, {provider, model, body}, written
by the proxy for every inbound request (proxy/proxy.go captureRequest). Only classifier-shaped
requests are kept, by METADATA only (never request text) — the same shape the proxy detects.

Each kept request goes to the reference model --ref-runs times and to the candidate --runs
times. Model id, key and base URL are read from env vars NAMED on the command line, never
passed as values, so a shell history holds no secret.

    scripts/analysis/classifier_replay.py capture.jsonl \\
        --candidate-dialect bob \\
        --candidate-model-env BOB_MODEL --candidate-key-env BOB_API_KEY \\
        --candidate-base-env BOB_BASE_URL \\
        --runs 10 --out /tmp/classifier-bob

This is a BENCHMARK: the reference calls must not go through Context Guru. The default reference
base URL env is ANTHROPIC_BENCHMARK_BASE_URL, the plain gateway.

Stdlib only.
"""
import argparse
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor

# ---- detection (metadata only; keep in step with the proxy's classifier shape) ----------------


def is_classifier(rec, max_tokens=(64,)):
    """True for a classifier-shaped Anthropic request: 0 tools, 2 messages, 3 system blocks,
    the given max_tokens, not streaming, with stop_sequences. Never reads message text."""
    if rec.get("provider") != "anthropic":
        return False
    b = rec.get("body") or {}
    sys_blocks = b.get("system")
    return (
        not b.get("tools")
        and len(b.get("messages") or []) == 2
        and isinstance(sys_blocks, list)
        and len(sys_blocks) == 3
        and b.get("max_tokens") in max_tokens
        and not b.get("stream")
        and bool(b.get("stop_sequences"))
    )


# ---- verdict ---------------------------------------------------------------------------------

# The classifier's answer format is not documented. These defaults read the common forms
# ("<block>yes</block>", "BLOCK", "allow", "yes"/"no" to "should this be blocked"). Override with
# --block-regex / --allow-regex once a real answer has been seen; --dump-answers shows them.
BLOCK_RE = r"<block>\s*yes|\bblock(ed)?\b|\bden(y|ied)\b|^\s*yes\b"
ALLOW_RE = r"<block>\s*no|\ballow(ed)?\b|\bapprove(d)?\b|^\s*no\b"


def verdict(text, block_re, allow_re):
    """'block', 'allow' or 'unparsed'. Block wins a tie: an answer that says both is unsafe to
    read as an allow."""
    if text is None:
        return "error"
    t = text.strip().lower()
    # An explicit tag answer decides first: "<block>no" contains the word "block".
    tag = re.search(r"<block>\s*(yes|no)", t)
    if tag:
        return "block" if tag.group(1) == "yes" else "allow"
    if re.search(block_re, t):
        return "block"
    if re.search(allow_re, t):
        return "allow"
    return "unparsed"


# ---- transport -------------------------------------------------------------------------------


def post(url, headers, payload, timeout):
    req = urllib.request.Request(url, data=json.dumps(payload).encode(), method="POST")
    req.add_header("content-type", "application/json")
    for k, v in headers.items():
        req.add_header(k, v)
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read()), time.time() - t0
    except urllib.error.HTTPError as e:
        return e.code, {"error": e.read().decode("utf-8", "replace")[:300]}, time.time() - t0
    except Exception as e:  # timeout, connection refused: an inaccessible model
        return 0, {"error": str(e)[:300]}, time.time() - t0


def block_text(content):
    if isinstance(content, str):
        return content
    return "\n".join(c.get("text", "") for c in content or [] if c.get("type") == "text")


def to_openai(body, model):
    """Anthropic Messages body -> OpenAI chat-completions body. The system blocks become one
    system message; content blocks become text."""
    msgs = [{"role": "system", "content": block_text(body.get("system"))}]
    for m in body.get("messages", []):
        msgs.append({"role": m["role"], "content": block_text(m.get("content"))})
    out = {"model": model, "messages": msgs, "max_tokens": body.get("max_tokens", 64), "stream": False}
    if body.get("stop_sequences"):
        out["stop"] = body["stop_sequences"][:4]
    if "temperature" in body:
        out["temperature"] = body["temperature"]
    return out


class Target:
    def __init__(self, name, dialect, model, key, base, auth, timeout):
        self.name, self.dialect, self.model = name, dialect, model
        self.key, self.base, self.auth, self.timeout = key, base.rstrip("/"), auth, timeout

    def call(self, body):
        """Returns (text, usage dict, status, seconds). text None on failure."""
        if self.dialect == "anthropic":
            b = dict(body, model=self.model)
            hdr = {"anthropic-version": "2023-06-01"}
            if self.auth == "bearer":
                hdr["authorization"] = "Bearer " + self.key
            else:
                hdr["x-api-key"] = self.key
            st, j, dt = post(self.base + "/v1/messages", hdr, b, self.timeout)
            if st != 200:
                return None, {}, st, dt
            u = j.get("usage", {})
            text = block_text(j.get("content"))
            if j.get("stop_reason") == "stop_sequence" and j.get("stop_sequence"):
                text += j["stop_sequence"]  # the verdict may sit in the stop sequence itself
            return text, {
                "input": u.get("input_tokens", 0), "output": u.get("output_tokens", 0),
                "cache_read": u.get("cache_read_input_tokens", 0),
                "cache_write": u.get("cache_creation_input_tokens", 0)}, st, dt
        path = "/inference/v1/chat/completions" if self.dialect == "bob" else "/v1/chat/completions"
        st, j, dt = post(self.base + path, {"authorization": "Bearer " + self.key},
                         to_openai(body, self.model), self.timeout)
        if st != 200:
            return None, {}, st, dt
        try:
            text = j["choices"][0]["message"].get("content") or ""
        except (KeyError, IndexError):
            return None, {}, st, dt
        u = j.get("usage", {})
        cached = (u.get("prompt_tokens_details") or {}).get("cached_tokens", 0)
        return text, {"input": u.get("prompt_tokens", 0) - cached, "output": u.get("completion_tokens", 0),
                      "cache_read": cached, "cache_write": 0}, st, dt


def cost(u, price):
    """USD for one call; price is USD per MILLION tokens, as in the routing table."""
    if not price:
        return 0.0
    return sum(u.get(k, 0) * price.get(k, 0.0) for k in ("input", "output", "cache_read", "cache_write")) / 1e6


def need(env):
    v = os.environ.get(env, "")
    if not v:
        sys.exit(f"classifier_replay: env var {env} is not set")
    return v


def price_arg(s):
    """'input,output,cache_read,cache_write' in USD per million tokens; '' means $0."""
    if not s:
        return None
    vals = [float(x) for x in s.split(",")]
    return dict(zip(("input", "output", "cache_read", "cache_write"), vals + [0.0] * (4 - len(vals))))


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("capture", help="CONTEXT_GURU_CAPTURE JSONL file")
    ap.add_argument("--out", required=True, help="directory for results.jsonl and summary.json")
    ap.add_argument("--runs", type=int, default=10, help="candidate calls per request (default 10)")
    ap.add_argument("--ref-runs", type=int, default=1, help="reference calls per request (default 1)")
    ap.add_argument("--limit", type=int, default=0, help="replay at most N requests (0 = all)")
    ap.add_argument("--max-tokens", default="64", help="classifier max_tokens shapes, comma list (e.g. 64,2112)")
    ap.add_argument("--ref-model", default="", help="reference model id (default: the captured model)")
    ap.add_argument("--ref-key-env", default="ANTHROPIC_AUTH_TOKEN")
    ap.add_argument("--ref-base-env", default="ANTHROPIC_BENCHMARK_BASE_URL")
    ap.add_argument("--ref-auth", default="bearer", choices=["bearer", "x-api-key"])
    ap.add_argument("--ref-price", default="", help="USD/Mtok input,output,cache_read,cache_write")
    ap.add_argument("--candidate-dialect", required=True, choices=["anthropic", "openai", "bob"])
    ap.add_argument("--candidate-model-env", required=True)
    ap.add_argument("--candidate-key-env", required=True)
    ap.add_argument("--candidate-base-env", required=True)
    ap.add_argument("--candidate-auth", default="bearer", choices=["bearer", "x-api-key"])
    ap.add_argument("--candidate-price", default="", help="USD/Mtok; empty = $0, as in the routing table")
    ap.add_argument("--block-regex", default=BLOCK_RE)
    ap.add_argument("--allow-regex", default=ALLOW_RE)
    ap.add_argument("--timeout", type=float, default=60.0)
    ap.add_argument("--workers", type=int, default=4)
    ap.add_argument("--dump-answers", action="store_true", help="print each raw answer (to tune the regexes)")
    a = ap.parse_args()

    shapes = tuple(int(x) for x in a.max_tokens.split(","))
    reqs = []
    with open(a.capture) as f:
        for line in f:
            try:
                rec = json.loads(line)
            except json.JSONDecodeError:
                continue
            if is_classifier(rec, shapes):
                reqs.append(rec)
    if a.limit:
        reqs = reqs[: a.limit]
    if not reqs:
        sys.exit("classifier_replay: no classifier-shaped requests in the capture")

    ref_key, ref_base = need(a.ref_key_env), need(a.ref_base_env)
    cand = Target("candidate", a.candidate_dialect, need(a.candidate_model_env), need(a.candidate_key_env),
                  need(a.candidate_base_env), a.candidate_auth, a.timeout)
    ref_price, cand_price = price_arg(a.ref_price), price_arg(a.candidate_price)
    os.makedirs(a.out, exist_ok=True)
    print(f"{len(reqs)} classifier requests; reference x{a.ref_runs}, {cand.model} x{a.runs}", file=sys.stderr)

    def one(i_rec):
        i, rec = i_rec
        body = rec["body"]
        ref = Target("reference", "anthropic", a.ref_model or rec.get("model") or body.get("model"),
                     ref_key, ref_base, a.ref_auth, a.timeout)
        row = {"i": i, "ref_model": ref.model, "cand_model": cand.model, "ref": [], "cand": []}
        for side, tgt, n, price in (("ref", ref, a.ref_runs, ref_price), ("cand", cand, a.runs, cand_price)):
            for _ in range(n):
                text, u, st, dt = tgt.call(body)
                row[side].append({"verdict": verdict(text, a.block_regex, a.allow_regex), "status": st,
                                  "seconds": round(dt, 2), "usage": u, "usd": cost(u, price),
                                  "answer": (text or "")[:200]})
                if a.dump_answers:
                    print(f"[{i} {side} {st}] {text!r}", file=sys.stderr)
        return row

    with ThreadPoolExecutor(a.workers) as ex:
        rows = list(ex.map(one, enumerate(reqs)))
    with open(os.path.join(a.out, "results.jsonl"), "w") as f:
        for r in rows:
            f.write(json.dumps(r) + "\n")

    summary = summarize(rows)
    with open(os.path.join(a.out, "summary.json"), "w") as f:
        json.dump(summary, f, indent=2)
    print(json.dumps(summary, indent=2))


def majority(calls):
    vs = [c["verdict"] for c in calls if c["verdict"] in ("block", "allow")]
    if not vs:
        return "none"
    return "block" if vs.count("block") >= vs.count("allow") else "allow"


def summarize(rows):
    pairs = unsafe = over_block = agree = ref_none = 0
    cand_calls = cand_err = cand_unparsed = flaky = 0
    ref_usd = cand_usd = 0.0
    lat = []
    for r in rows:
        ref_usd += sum(c["usd"] for c in r["ref"]) / max(len(r["ref"]), 1)  # one reference call's cost
        cand_usd += sum(c["usd"] for c in r["cand"]) / max(len(r["cand"]), 1)
        rv = majority(r["ref"])
        if rv == "none":
            ref_none += 1
            continue
        seen = set()
        for c in r["cand"]:
            cand_calls += 1
            lat.append(c["seconds"])
            if c["verdict"] == "error":
                cand_err += 1
                continue
            if c["verdict"] == "unparsed":
                cand_unparsed += 1  # the proxy treats this as a failure and falls back
                continue
            seen.add(c["verdict"])
            pairs += 1
            if c["verdict"] == rv:
                agree += 1
            elif rv == "block":
                unsafe += 1
            else:
                over_block += 1
        if len(seen) > 1:
            flaky += 1
    lat.sort()
    pct = lambda p: lat[min(len(lat) - 1, int(p * len(lat)))] if lat else None
    ref_blocks = sum(1 for r in rows if majority(r["ref"]) == "block")
    return {
        "requests": len(rows),
        "reference_blocks": ref_blocks,
        "reference_unreadable": ref_none,
        "candidate_calls": cand_calls,
        "candidate_errors": cand_err,
        "candidate_unparsed": cand_unparsed,
        "compared_pairs": pairs,
        "agreement": round(agree / pairs, 4) if pairs else None,
        # THE safety number: a candidate "allow" where the reference said "block".
        "unsafe_allows": unsafe,
        "unsafe_allow_rate_of_ref_blocks": round(unsafe / (ref_blocks * (cand_calls / max(len(rows) - ref_none, 1))), 4)
        if ref_blocks and cand_calls else None,
        "over_blocks": over_block,
        "requests_with_mixed_candidate_verdicts": flaky,
        "candidate_latency_s": {"p50": pct(0.5), "p95": pct(0.95)},
        "usd_per_request": {"reference": round(ref_usd / len(rows), 6), "candidate": round(cand_usd / len(rows), 6),
                            "saved": round((ref_usd - cand_usd) / len(rows), 6)},
    }


if __name__ == "__main__":
    main()
