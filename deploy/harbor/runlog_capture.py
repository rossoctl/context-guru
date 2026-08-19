#!/usr/bin/env python3
"""Convert benchmark-harness run logs into CONTEXT_GURU_CAPTURE JSONL, so coref.py can
measure co-reference density on real LONG-HORIZON BENCHMARK traffic.

Companion to cc_capture.py, which does the same for Claude Code transcripts. That corpus is
real but it is interactive research work; this one is what the acceptance criteria in
docs/proposals/coref-compaction.md §8 actually care about — agent trajectories from
UltraHorizon, LOCA-bench and the Momento/travel harnesses, at 200+ turns with hard context
wipes. Still zero API dollars: the runs already happened and left their logs on disk.

Three input shapes, auto-detected:

  llm_calls.jsonl   (loopb / UltraHorizon)  {model, messages, tools, response} per line.
                    Already a request body per model turn, append-only — a capture in all
                    but name. OpenAI dialect (role "tool" + tool_calls).
  trace.jsonl       (litellm)               {request: {messages, model}, ...} per line.
                    Same, one line per upstream call.
  all_trajectories.json (LOCA-bench)        {env: {state: {claude_messages: [...]}}}.
                    A finished conversation rather than per-turn bodies, so the turn
                    prefixes are reconstructed from it. Anthropic dialect.

Two behaviours worth knowing, because both change what gets measured:

  CONTEXT WIPES ARE SESSION BOUNDARIES. UltraHorizon clears the agent's context mid-run
  ("The context has been cleared. Please check your notes"), so the message count DROPS. A
  drop starts a new session here. Measuring across a wipe would look for references from
  before a boundary the model cannot see across, and would score every one of them as
  absent — inventing cuttable mass out of the harness's own reset.

  ONLY THE LARGEST BODY IS EMITTED IN FULL. coref.py indexes the largest request per session
  and reads the others only for their token size (the T estimate), so every other turn is
  emitted as a `turn_tokens` record. That is what keeps this tractable: the UltraHorizon logs
  alone are 9 GB of growing prefixes. A real capture sets no such field and is unaffected.

Usage:
  runlog_capture.py <out.jsonl> <log> [log ...] [--min-turns N] [--label NAME]

  --min-turns 8   drop sessions with fewer model turns than this
  --label NAME    prefix for the conversation ids (defaults to the file's parent dirs)
"""
import json
import os
import sys

TOK = lambda s: max(1, len(s) // 4)  # same proxy as coref.py / analyze_content.py


# --- sizing (must match coref.py's normalize + session_turns) -----------------------------

def _openai_tokens(m):
    texts, results = [], []
    c = m.get("content")
    if m.get("role") == "tool":
        results.append(c if isinstance(c, str) else json.dumps(c))
    else:
        if isinstance(c, str):
            texts.append(c)
        for tc in m.get("tool_calls") or []:
            f = tc.get("function") or {}
            texts.append(f.get("name", "") + " " + str(f.get("arguments", "")))
    return TOK(" ".join(texts)) + sum(TOK(r) for r in results)


def _anthropic_tokens(m):
    texts, results = [], []
    c = m.get("content")
    if isinstance(c, str):
        texts.append(c)
    elif isinstance(c, list):
        for b in c:
            if not isinstance(b, dict):
                continue
            t = b.get("type")
            if t == "text":
                texts.append(b.get("text", ""))
            elif t == "thinking":
                texts.append(b.get("thinking", ""))
            elif t == "tool_use":
                texts.append(b.get("name", "") + " " + json.dumps(b.get("input", {})))
            elif t == "tool_result":
                rc = b.get("content")
                if isinstance(rc, str):
                    results.append(rc)
                elif isinstance(rc, list):
                    results.append("".join(x.get("text", "") for x in rc if isinstance(x, dict)))
                elif rc is not None:
                    results.append(json.dumps(rc))
    return TOK(" ".join(texts)) + sum(TOK(r) for r in results)


def body_tokens(msgs, provider):
    f = _anthropic_tokens if provider == "anthropic" else _openai_tokens
    return sum(f(m) for m in msgs)


# --- readers: each yields a list of per-turn message lists ------------------------------

def read_llm_calls(path):
    """loopb / UltraHorizon: one request body per line, already append-only."""
    out, model = [], "unknown"
    with open(path, errors="replace") as f:
        for line in f:
            if not line.strip():
                continue
            try:
                d = json.loads(line)
            except ValueError:
                continue
            msgs = d.get("messages")
            if isinstance(msgs, list) and msgs:
                out.append(msgs)
                model = d.get("model") or model
    return out, model, "openai"


def read_litellm_trace(path):
    """litellm trace: one upstream call per line, body under `request`."""
    out, model = [], "unknown"
    with open(path, errors="replace") as f:
        for line in f:
            if not line.strip():
                continue
            try:
                d = json.loads(line)
            except ValueError:
                continue
            req = d.get("request") or {}
            msgs = req.get("messages")
            if isinstance(msgs, list) and msgs:
                out.append(msgs)
                model = d.get("model") or req.get("model") or model
    return out, model, "openai"


def read_loca(path):
    """LOCA-bench all_trajectories.json: a FINISHED conversation per env/state, so the turn
    prefixes are reconstructed by cutting it after each model turn. Uses claude_messages
    (the wire conversation) over full_messages_history, which omits the opening task."""
    with open(path, errors="replace") as f:
        doc = json.load(f)
    runs, model = [], "claude-sonnet-5"
    for env, states in (doc or {}).items():
        if not isinstance(states, dict):
            continue
        for state, node in states.items():
            if not isinstance(node, dict):
                continue
            msgs = node.get("claude_messages") or node.get("full_messages_history")
            if not isinstance(msgs, list) or not msgs:
                continue
            prefixes = [msgs[: i + 1] for i, m in enumerate(msgs) if m.get("role") == "assistant"]
            if prefixes:
                runs.append((f"{env}/{state}", prefixes))
    return runs, model, "anthropic"


def detect(path):
    base = os.path.basename(path)
    if base == "all_trajectories.json":
        return "loca"
    if base.endswith(".jsonl"):
        with open(path, errors="replace") as f:
            for line in f:
                if not line.strip():
                    continue
                try:
                    d = json.loads(line)
                except ValueError:
                    continue
                if "messages" in d:
                    return "llm_calls"
                if isinstance(d.get("request"), dict) and "messages" in d["request"]:
                    return "trace"
                return None
    return None


# --- emit -------------------------------------------------------------------------------

def wipe_split(prefixes):
    """Split where the message count DROPS — a harness context wipe (see docstring)."""
    segs, cur = [], []
    for msgs in prefixes:
        if cur and len(msgs) < len(cur[-1]):
            segs.append(cur)
            cur = []
        cur.append(msgs)
    if cur:
        segs.append(cur)
    return segs


def emit(prefixes, provider, model, conv, out, min_turns):
    if len(prefixes) < min_turns:
        return 0
    sizes = [body_tokens(m, provider) for m in prefixes]
    top = max(range(len(prefixes)), key=lambda i: (len(prefixes[i]), sizes[i]))
    for i, n in enumerate(sizes):
        if i == top:
            continue
        out.write(json.dumps({"provider": provider, "model": model, "conv": conv,
                              "turn_tokens": n,
                              "body": {"model": model, "messages": prefixes[top][:1]}}) + "\n")
    out.write(json.dumps({"provider": provider, "model": model, "conv": conv,
                          "body": {"model": model, "messages": prefixes[top]}}) + "\n")
    return len(prefixes)


def main():
    args = sys.argv[1:]
    if not args:
        print(__doc__)
        return 2
    min_turns, label, files, out_path = 8, None, [], None
    i = 0
    while i < len(args):
        a = args[i]
        if a == "--min-turns":
            i += 1
            min_turns = int(args[i])
        elif a == "--label":
            i += 1
            label = args[i]
        elif a.startswith("--"):
            print(f"unknown flag {a}")
            return 2
        elif out_path is None:
            out_path = a
        else:
            files.append(a)
        i += 1
    if out_path is None or not files:
        print(__doc__)
        return 2

    n_sess = n_turn = n_src = 0
    skipped = 0
    with open(out_path, "w") as out:
        for path in files:
            kind = detect(path)
            if kind is None:
                skipped += 1
                continue
            stem = label or "/".join(path.rstrip("/").split("/")[-4:-1])
            tag = f"{stem}:{os.path.basename(path)[:12]}"
            try:
                if kind == "loca":
                    runs, model, provider = read_loca(path)
                    groups = [(f"{tag}#{name}", pref) for name, pref in runs]
                else:
                    prefixes, model, provider = (read_llm_calls if kind == "llm_calls"
                                                 else read_litellm_trace)(path)
                    groups = [(f"{tag}#{k}", seg) for k, seg in enumerate(wipe_split(prefixes))]
            except (ValueError, OSError):
                skipped += 1
                continue
            used = 0
            for conv, pref in groups:
                t = emit(pref, provider, model, conv, out, min_turns)
                if t:
                    n_sess += 1
                    n_turn += t
                    used += 1
            if used:
                n_src += 1
    print(f"wrote {out_path}: {n_src} logs -> {n_sess} sessions, {n_turn} turn records "
          f"({os.path.getsize(out_path) / 1e6:.1f} MB)"
          + (f"; {skipped} unrecognized" if skipped else ""))
    return 0


if __name__ == "__main__":
    sys.exit(main())
