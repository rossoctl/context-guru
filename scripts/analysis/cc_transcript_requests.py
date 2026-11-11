"""Rebuild the Anthropic request sequence of a Claude Code transcript, for an offline replay.
usage: python3 -I cc_transcript_requests.py OUT_DIR NAME=TRANSCRIPT.jsonl ...
Writes OUT_DIR/NAME.json: {messages, calls:[{n, ts_ms, usage}], tools:{tool_use_id: {name, kind, path, edited_later}}}.
Call k was sent with messages[:calls[k].n]. Assumes one thread (no sidechains) and no compaction."""
import datetime, json, os, re, sys

EDITS = {"Edit", "Write", "MultiEdit", "NotebookEdit"}
CAT = re.compile(r"^\s*(cat|sed|grep|rg|head|tail|awk|nl|less)\b")

def ms(ts): return int(datetime.datetime.fromisoformat(ts.replace("Z", "+00:00")).timestamp() * 1000)

def kind(name, inp):
    if name == "Read": return "Read"
    if name == "Bash": return "Bash-view" if CAT.match(inp.get("command", "")) else "Bash-other"
    if name in EDITS: return "Edit"
    return "other"

def build(path):
    msgs, calls, tools, pending, seen = [], [], {}, [], {}
    def flush():
        nonlocal pending
        if not pending: return
        if msgs and msgs[-1]["role"] == "user": msgs[-1]["content"].extend(pending)
        else: msgs.append({"role": "user", "content": pending})
        pending = []
    for line in open(path):
        e = json.loads(line)
        t = e.get("type")
        if e.get("isSidechain"): raise SystemExit(f"{path}: sidechain entry, not supported")
        if t == "user":
            c = e["message"]["content"]
            pending.extend([{"type": "text", "text": c}] if isinstance(c, str) else c)
        elif t == "attachment" and e.get("rendered"):
            for r in e["rendered"]:
                c = r.get("content")
                if isinstance(c, str) and c: pending.append({"type": "text", "text": c})
                elif isinstance(c, list): pending.extend(b for b in c if isinstance(b, dict))
        elif t == "assistant":
            m = e["message"]; mid = m["id"]
            if mid not in seen:
                flush()
                seen[mid] = len(calls)
                calls.append({"n": len(msgs), "ts_ms": ms(e["timestamp"]), "usage": m.get("usage", {})})
                msgs.append({"role": "assistant", "content": []})
            else:
                calls[seen[mid]]["usage"] = m.get("usage", calls[seen[mid]]["usage"])
            for b in m["content"]:
                b = {k: v for k, v in b.items() if k != "caller"}
                msgs[-1]["content"].append(b)
                if b.get("type") == "tool_use":
                    inp = b.get("input", {})
                    tools[b["id"]] = {"name": b["name"], "kind": kind(b["name"], inp), "path": inp.get("file_path") or inp.get("notebook_path"), "pos": len(tools)}
    flush()
    # The Read/view of a path is "edited later" when an Edit/Write on that path follows it.
    order = sorted(tools.items(), key=lambda kv: kv[1]["pos"])
    for i, (tid, t) in enumerate(order):
        t["edited_later"] = bool(t["path"]) and any(u["kind"] == "Edit" and u["path"] == t["path"] for _, u in order[i + 1:])
    # Merge assistant messages that are not separated by a user turn (should not happen, but keep the wire valid).
    return {"messages": msgs, "calls": calls, "tools": tools}

if __name__ == "__main__":
    out = sys.argv[1]; os.makedirs(out, mode=0o700, exist_ok=True)
    for arg in sys.argv[2:]:
        name, path = arg.split("=", 1)
        d = build(path)
        with open(os.path.join(out, name + ".json"), "w") as f: json.dump(d, f)
        print(name, len(d["calls"]), "calls", len(d["messages"]), "messages")
