#!/usr/bin/env python3
"""Ground-truth fixture for coref.py — four tool outputs whose correct classification is
known by construction, so the measurement can be checked rather than trusted.

The load-bearing case is #2: it must come out UNREFERENCED even though a later turn
mentions `src/config.py`, because that path entered context as the tool_use ARGUMENT, not
as something the output introduced. An implementation without the echo-exclusion guard
scores it as referenced, and then reports near-total reference density on any traffic.

  #1 src/auth.py read     -> CLOSED       (novel TOKEN_GRACE_SECONDS lifted out once, early)
  #2 src/config.py read   -> UNREFERENCED (only overlap is the echoed path == the argument)
  #3 ls -R listing        -> UNREFERENCED (nothing ever comes back to it)
  #4 pytest failure       -> OPEN         (novel error id reused 3x, most recently 1 msg ago)

Emits append-only capture records (one per model turn, each a prefix of the final
transcript) matching CONTEXT_GURU_CAPTURE's {provider, model, body} JSONL shape.

Usage: coref_fixture.py <out.jsonl>
"""
import json, sys


def filler(tag, n):
    """Per-output distinct lines: shared filler would be dropped as session boilerplate,
    which would mask whether the novel-token logic works."""
    return "\n".join(f"{i:4d}\t{tag}_line_{i} = compute_{tag}_{i}(arg_{i})" for i in range(n))


def build():
    msgs = []

    def user_text(t):
        msgs.append({"role": "user", "content": t})

    def asst(text=None, tool=None, tid=None, args=None):
        c = []
        if text:
            c.append({"type": "text", "text": text})
        if tool:
            c.append({"type": "tool_use", "id": tid, "name": tool, "input": args})
        msgs.append({"role": "assistant", "content": c})

    def result(tid, text):
        msgs.append({"role": "user", "content": [{"type": "tool_result", "tool_use_id": tid, "content": text}]})

    user_text("Fix the failing test test_auth_expiry in src/auth.py")

    # #1 CLOSED
    asst(text="Reading the auth module.", tool="Read", tid="t1", args={"path": "src/auth.py"})
    result("t1", "src/auth.py\n" + filler("auth", 240) + "\nTOKEN_GRACE_SECONDS = 0  # novel\n")
    asst(text="The bug is TOKEN_GRACE_SECONDS is 0; it must be 300.", tool="Edit", tid="t2",
         args={"path": "src/auth.py", "old": "TOKEN_GRACE_SECONDS = 0", "new": "TOKEN_GRACE_SECONDS = 300"})
    result("t2", "ok")

    # #2 UNREFERENCED — the echo confound
    asst(text="Checking config.", tool="Read", tid="t3", args={"path": "src/config.py"})
    result("t3", "src/config.py\n" + filler("config", 240))
    asst(text="Config is fine, adjusting anyway.", tool="Edit", tid="t4",
         args={"path": "src/config.py", "old": "a", "new": "b"})
    result("t4", "ok")

    # #3 UNREFERENCED
    asst(text="Surveying the tree.", tool="Bash", tid="t5", args={"cmd": "ls -R"})
    result("t5", filler("tree", 240))

    # #4 OPEN
    asst(text="Running the suite.", tool="Bash", tid="t6", args={"cmd": "pytest -q"})
    result("t6", "1 failed, 42 passed\n" + filler("pytest", 240) + "\nE   AssertionError: XPIRE_DRIFT_7f3a\n")
    for k in range(3):
        asst(text=f"XPIRE_DRIFT_7f3a again; attempt {k}.", tool="Bash", tid=f"r{k}", args={"cmd": "pytest -q"})
        result(f"r{k}", "1 failed\nE   AssertionError: XPIRE_DRIFT_7f3a\n")
    return msgs


def main():
    if len(sys.argv) != 2:
        print(__doc__)
        return 2
    msgs = build()
    with open(sys.argv[1], "w") as f:
        for n in range(2, len(msgs) + 1):
            if msgs[n - 1]["role"] != "assistant":
                continue  # capture fires on the request the agent sends, i.e. after a model turn
            f.write(json.dumps({"provider": "anthropic", "model": "claude-sonnet-5",
                                "body": {"model": "claude-sonnet-5", "messages": msgs[:n]}}) + "\n")
    print(f"wrote {sys.argv[1]}: {len(msgs)} messages, "
          f"{sum(1 for m in msgs if m['role'] == 'assistant')} model turns")
    return 0


if __name__ == "__main__":
    sys.exit(main())
