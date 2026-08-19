#!/usr/bin/env python3
"""Convert Claude Code session transcripts into CONTEXT_GURU_CAPTURE JSONL, so coref.py
can measure co-reference density on REAL agent traffic without an eval-box run.

Why this exists. coref.py needs captured request bodies, and the corpus the improvement
plan cites (capture-tb / capture-swe / capture-swebench) lives on the eval box. But a
Claude Code transcript is the same traffic recorded one step earlier: the agent's own
append-only log of every message it sent. Converting it costs zero API dollars and yields
Tier-1-rich Read/Edit/Bash traffic — the substrate docs/proposals/coref-compaction.md §8
names as the right one for a deterministic reference detector.

Three things the transcript is NOT, and what this does about each:

  1. It is one entry per content BLOCK, not per message. Consecutive same-role entries are
     MERGED, because a real request body carries one assistant message with several blocks
     (and one user message with the batched tool_results). Message COUNT is the axis
     reference recency is measured on, so getting this wrong would rescale the whole A/B
     distinction.
  2. It spans many context windows. These sessions reach 3,000+ model turns, so no single
     request ever held the whole thing — the agent compacted repeatedly. Emitting it as one
     transcript would measure a request that never existed. So it is SEGMENTED at
     --segment-tokens, each segment treated as its own session. This approximates the real
     compaction boundaries (which the transcript does not record) by the budget that forces
     them.
  3. `thinking` blocks are model-authored, so they are mapped to text and count as a
     reference-bearing surface. That is deliberate, not a shortcut: thinking is exactly
     where a model restates the value it lifted out of a tool result, and the real wire
     body carries it too.

Output is standard {provider, model, body} JSONL, one record per model turn, as an actual
capture would be. One deviation, and it is the only reason coref.py needed a change: a
full prefix per turn is O(n^2) bytes (47 GB across this machine's transcripts), and
non-final records are read ONLY for their token size (session_turns, the T estimate). So
every record except a segment's last carries `turn_tokens` plus just the segment's first
user message — enough for coref.py to size the turn and to group the segment — and
coref.py uses that field when present. A real capture has no such field and is unaffected.

Usage:
  cc_capture.py <out.jsonl> [transcript.jsonl ...] [--segment-tokens N] [--min-turns N]
  cc_capture.py <out.jsonl> --all [--segment-tokens N]     # every local Claude Code session

  --segment-tokens 180000   split a transcript into window-sized sessions (0 = never split)
  --min-turns 8             drop segments with fewer model turns than this
  --all                     use ~/.claude/projects/*/*.jsonl
"""
import glob
import json
import os
import sys

TOK = lambda s: max(1, len(s) // 4)  # same proxy as coref.py / analyze_content.py


def load_entries(path):
    """Main-conversation user/assistant entries, in file order. Sidechain (subagent) entries
    are a DIFFERENT conversation with its own context, so mixing them in would invent
    references across two transcripts that never saw each other."""
    out = []
    with open(path, errors="replace") as f:
        for line in f:
            if not line.strip():
                continue
            try:
                e = json.loads(line)
            except ValueError:
                continue
            if e.get("type") not in ("user", "assistant") or e.get("isSidechain"):
                continue
            m = e.get("message") or {}
            if m.get("content") is None:
                continue
            out.append(e)
    return out


def blocks_of(entry):
    """Anthropic content blocks for one entry, with thinking mapped to text and anything
    non-textual dropped (an image carries no identifiers an exact matcher could use)."""
    c = (entry.get("message") or {}).get("content")
    if isinstance(c, str):
        return [{"type": "text", "text": c}] if c else []
    if not isinstance(c, list):
        return []
    out = []
    for b in c:
        if not isinstance(b, dict):
            continue
        t = b.get("type")
        if t == "text":
            out.append({"type": "text", "text": b.get("text", "")})
        elif t == "thinking":
            out.append({"type": "text", "text": b.get("thinking", "")})
        elif t == "tool_use":
            out.append({"type": "tool_use", "id": b.get("id", ""),
                        "name": b.get("name", ""), "input": b.get("input", {})})
        elif t == "tool_result":
            out.append({"type": "tool_result", "tool_use_id": b.get("tool_use_id", ""),
                        "content": b.get("content")})
    return out


def to_messages(entries):
    """Merge consecutive same-role entries into one message each (see docstring note 1)."""
    msgs = []
    for e in entries:
        role = "assistant" if e.get("type") == "assistant" else "user"
        bs = blocks_of(e)
        if not bs:
            continue
        if msgs and msgs[-1]["role"] == role:
            msgs[-1]["content"].extend(bs)
        else:
            msgs.append({"role": role, "content": bs})
    return msgs


def msg_tokens(m):
    """Token size of one message, counted the way coref.py's session_turns counts it: the
    reference-bearing surfaces and the tool-result bodies, not the JSON envelope."""
    texts, results = [], []
    c = m["content"]
    for b in c:
        t = b.get("type")
        if t == "text":
            texts.append(b.get("text", ""))
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


def segments(msgs, budget):
    """Split into sessions no larger than budget tokens (see docstring note 2). Splits only
    BEFORE a user turn, so a tool_result is never separated from the assistant turn that
    requested it — an orphaned pair would look like an output nobody asked for."""
    if budget <= 0:
        return [msgs]
    out, cur, acc = [], [], 0
    for m in msgs:
        n = msg_tokens(m)
        if cur and acc + n > budget and m["role"] == "user":
            out.append(cur)
            cur, acc = [], 0
        cur.append(m)
        acc += n
    if cur:
        out.append(cur)
    return out


def first_user(msgs):
    for m in msgs:
        if m["role"] == "user":
            return m
    return msgs[0] if msgs else {"role": "user", "content": []}


def emit(seg, model, conv, out):
    """One record per model turn, as an append-only capture would hold. Only a segment's
    last record carries the full transcript; earlier ones carry their size (see docstring).

    `conv` is stamped explicitly because coref.py otherwise groups a session by the hash of
    its first user message — correct for a real capture, where every request of a session
    opens with the same task statement, but a mid-transcript segment opens on a tool_result
    and those collide. Left inferred, 31 segments grouped down to 24 and the rest were
    silently discarded, since only the largest member of a group is analyzed."""
    turns = [i for i, m in enumerate(seg) if m["role"] == "assistant"]
    if not turns:
        return 0
    head, acc, sizes = first_user(seg), 0, []
    for i, m in enumerate(seg):
        acc += msg_tokens(m)
        if m["role"] == "assistant":
            sizes.append(acc)
    for n in sizes[:-1]:
        out.write(json.dumps({"provider": "anthropic", "model": model, "conv": conv,
                              "turn_tokens": n,
                              "body": {"model": model, "messages": [head]}}) + "\n")
    last = turns[-1]
    out.write(json.dumps({"provider": "anthropic", "model": model, "conv": conv,
                          "body": {"model": model, "messages": seg[:last + 1]}}) + "\n")
    return len(sizes)


def model_of(entries):
    for e in entries:
        m = (e.get("message") or {}).get("model")
        if m:
            return m
    return "claude-sonnet-5"


def main():
    args = sys.argv[1:]
    if not args:
        print(__doc__)
        return 2
    opt = {"--segment-tokens": "180000", "--min-turns": "8"}
    files, i, use_all, out_path = [], 0, False, None
    while i < len(args):
        a = args[i]
        if a == "--all":
            use_all = True
        elif a in opt:
            i += 1
            opt[a] = args[i]
        elif a.startswith("--"):
            print(f"unknown flag {a}")
            return 2
        elif out_path is None:
            out_path = a
        else:
            files.append(a)
        i += 1
    if out_path is None:
        print(__doc__)
        return 2
    if use_all:
        files += sorted(glob.glob(os.path.expanduser("~/.claude/projects/*/*.jsonl")))
    if not files:
        print("no transcripts given")
        return 2

    budget, min_turns = int(opt["--segment-tokens"]), int(opt["--min-turns"])
    n_seg = n_turn = n_src = 0
    with open(out_path, "w") as out:
        for path in files:
            entries = load_entries(path)
            msgs = to_messages(entries)
            if not msgs:
                continue
            model = model_of(entries)
            used = 0
            stem = os.path.basename(path)[:8]
            for k, seg in enumerate(segments(msgs, budget)):
                if sum(1 for m in seg if m["role"] == "assistant") < min_turns:
                    continue
                t = emit(seg, model, f"{stem}#{k}", out)
                if t:
                    n_seg += 1
                    n_turn += t
                    used += 1
            if used:
                n_src += 1
    print(f"wrote {out_path}: {n_src} transcripts -> {n_seg} sessions, {n_turn} turn records "
          f"({os.path.getsize(out_path) / 1e6:.1f} MB)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
