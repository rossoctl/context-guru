"""Replay the manifest's post-install turns through the proxy, as Claude Code would send them.

Each request declares the full inventory (MCP and built-in tools in `tools`, skills in a system
message), names its working directory with the same marker Claude Code writes, and carries the
turn's invocations as tool_use blocks, so the proxy's capture path is exercised for real."""
from __future__ import annotations

import argparse
import json
import sys
import urllib.error
import urllib.request

from e2e.gen import manifest as mf

SKILL_KINDS = ("plugin_skill", "marketplace_skill", "bundled_skill")


def _tool(c: mf.Capability) -> dict:
    return {"name": c.name, "description": f"e2e fixture {c.name} " + "x" * (c.tokens // 4),
            "input_schema": {"type": "object", "properties": {}}}


def body_for(t: mf.Turn, s: mf.Session, m: mf.Manifest, with_marker: bool = True) -> dict:
    caps = {c.name: c for c in m.capabilities}
    declared = [c for c in m.capabilities if c.declared]
    tools = [_tool(c) for c in declared if c.kind not in SKILL_KINDS]
    tools.append({"name": "Skill", "description": "Invoke a skill",
                  "input_schema": {"type": "object", "properties": {"skill": {"type": "string"}}}})
    listing = "\n".join(f"- {c.name}: e2e fixture skill {c.name}" for c in declared if c.kind in SKILL_KINDS)
    reminder = ("<system-reminder>\nThe following skills are available for use with the Skill tool:\n\n"
                + listing + "\n</system-reminder>")
    env = f"# Environment\nPrimary working directory: {t.cwd}\n" if with_marker else "# Environment\n"
    uses = []
    for i, inv in enumerate(t.invocations):
        c = caps[inv.cap]
        if c.kind in SKILL_KINDS:
            uses.append({"type": "tool_use", "id": f"tu{i}", "name": "Skill", "input": {"skill": c.name}})
        else:
            uses.append({"type": "tool_use", "id": f"tu{i}", "name": c.name, "input": {}})
    # Claude Code sends the skills listing as its own system-role message inside `messages`
    # (dash/toolinventory.go), not inside the user turn.
    msgs = [{"role": "user", "content": [{"type": "text", "text": env}, {"type": "text", "text": "continue"}]},
            {"role": "system", "content": [{"type": "text", "text": reminder}]}]
    if uses:
        msgs.append({"role": "assistant", "content": uses})
        msgs.append({"role": "user", "content": [{"type": "tool_result", "tool_use_id": u["id"], "content": "ok"}
                                                 for u in uses]})
    return {"model": "claude-opus-5", "max_tokens": 1024, "stream": t.stream,
            "system": [{"type": "text", "text": "You are Claude Code."}],
            "tools": tools, "messages": msgs,
            # Claude Code's real shape: user_id is a JSON OBJECT carrying session_id. session.ExplicitID
            # unwraps it; a plain string would skip that path.
            "metadata": {"user_id": json.dumps({"session_id": s.id})}}


def _turns(m: mf.Manifest, which: str):
    post = [(s, t) for s in m.sessions for t in s.turns if mf.measured(t, m)]
    post.sort(key=lambda st: st[1].ts)
    half = len(post) // 2
    return {"post": post, "first_half": post[:half], "second_half": post[half:]}[which]


def run(m: mf.Manifest, base_url: str, which: str = "post", limit=None) -> dict:
    sent = ok = failed = trunc = 0
    for s, t in _turns(m, which)[:limit]:
        body = json.dumps(body_for(t, s, m)).encode()
        h = {"content-type": "application/json", "x-api-key": "sk-e2e-fake",
             "anthropic-version": "2023-06-01",
             "X-E2E-Usage": f"{t.fresh},{t.cache_read},{t.cache_write},{t.output}"}
        if t.truncate:
            h["X-E2E-Truncate"] = "1"
            trunc += 1
        sent += 1
        req = urllib.request.Request(base_url.rstrip("/") + "/v1/messages", data=body, headers=h, method="POST")
        try:
            # HTTPError (4xx/5xx, e.g. the upstream's 400 on malformed usage) is a URLError: counted failed.
            with urllib.request.urlopen(req, timeout=30) as r:
                r.read()
                ok += 1
        except (urllib.error.URLError, ConnectionError, TimeoutError) as e:
            if isinstance(e, urllib.error.HTTPError):
                e.close()
            failed += 1
    return {"sent": sent, "ok": ok, "failed": failed, "truncated": trunc}


def main(argv=None) -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--tier", required=True)
    ap.add_argument("--base", required=True)
    ap.add_argument("--which", default="post", choices=("post", "first_half", "second_half"))
    ap.add_argument("--home", default="/sandbox")
    a = ap.parse_args(argv)
    r = run(mf.build(a.tier, home=a.home), a.base, a.which)
    print(json.dumps(r))
    return 1 if r["failed"] else 0


if __name__ == "__main__":
    sys.exit(main())
