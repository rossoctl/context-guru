#!/usr/bin/env python3
"""Writes cases/*.json: the eight worked examples of docs/counterfactual/examples.md as test cases.

Run `go generate ./...` in this directory after editing. Bodies use 4 bytes a token, and a body
hash is cumulative over the content ids, so two bodies with an equal prefix have equal hashes
there (design.md §5.1). Every figure is checked twice: opscases_test.go checks conservation and the
billed tokens against the bodies, and dash/counterfactual_play_test.go plays each case through the
current saving code.
"""
import json
import os
from datetime import datetime, timedelta, timezone

OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "cases")
T0 = datetime(2026, 3, 2, 9, 0, 0, tzinfo=timezone.utc)
MASK = (1 << 64) - 1

BSTAR_SNAP = ("design §5.1 snaps B* to the last client breakpoint at or before the previous body's end, "
              "and the client's breakpoints are the current request's own: system and the new last message. "
              "Read literally that gives the end of system, not the end of the previous body. The examples, and "
              "this case, take the end of the previous body.")


def ts(sec):
    return (T0 + timedelta(seconds=sec)).strftime("%Y-%m-%dT%H:%M:%SZ")


def body(tools, system, msgs):
    h, pos = 14695981039346656037, []
    for section in (tools, system, msgs):
        for cid, tok in section:
            for ch in (cid + "\0").encode():
                h = ((h ^ ch) * 1099511628211) & MASK
            pos.append({"hash": h, "bytes": 4 * tok})
    return {"start": [0, len(tools), len(tools) + len(system)], "pos": pos}


def tokens(*sections):
    return sum(t for s in sections for _, t in s)


def P(section, index):
    return {"section": section, "index": index}


def bps(msgs, ttl="5m", system=True):
    out = [{"pos": P("system", 0), "ttl": ttl}] if system else []
    out.append({"pos": P("messages", len(msgs) - 1), "ttl": ttl})
    return out


def U(read=0, write=0, inp=0, w1h=0):
    return {"input": inp, "cacheRead": read, "cacheWrite": write, "cacheWrite1h": w1h}


def TT(**kw):
    return kw  # input, cacheWrite5m, cacheWrite1h, cacheRead


def size(t):
    # Bytes only (design §4.2 rule 7). The fixtures use 4 bytes a token, so t tokens are 4 * t bytes.
    return {"bytes": 4 * t}


def rewrite(oid, operator, section, idx, before, after, change, spans=None, splice=None):
    op = {"id": oid, "kind": "rewrite", "operator": operator, "at": P(section, idx),
          "sizeBefore": size(before), "sizeAfter": size(after), "changeId": change}
    if spans:
        op["spans"] = spans
    if splice:
        op["messageSplice"] = splice
    return op


def eop(change, saved=None, caused=None, note=None, call=None):
    e = {"changeId": change, "saved": saved or {}, "caused": caused or {}}
    if not call:  # a rewrite's tokens are converted from bytes, so it is Estimated (§4.2 rule 7)
        e["estimated"] = True
    if note:
        e["note"] = note
    if call:
        e["call"] = call
    return e


def step(sec, rid, client, fwd, cbp, fbp, add, usage, exp, session="s1", model="agent-model",
         params=None, facts=None):
    meta = {"requestId": rid, "session": session, "model": model, "at": ts(sec), "billing": {"mode": "api"}}
    if params:
        meta["params"] = params
    return {
        "begin": {"meta": meta, "client": client, "clientBreakpoints": cbp},
        "add": add,
        "end": {"thread": {"id": "main"}, "forwarded": fwd, "breakpoints": fbp,
                "facts": facts or {"ttl": "5m", "lifetime": "exact"}},
        "usage": usage,
        "expected": exp,
    }


def effect(kind, change, saved=None, caused=None, frm=None, to=None, ttl=None, note=None):
    """A cache effect the library finds (design 4.4). It has no input form: only Compute writes one."""
    e = {"kind": kind, "changeId": change, "saved": saved or {}, "caused": caused or {}}
    if frm:
        e["from"] = frm
    if to:
        e["to"] = to
    if ttl:
        e["ttl"] = ttl
    if note:
        e["note"] = note
    return e


def expect(phase, baseline, ops, boundary=None, unpriced=None, effects=None, checks=None):
    e = {"phase": phase, "baselineUsage": baseline, "ops": ops}
    if phase == "cold" and not effects:  # §5.2 row 5: the expiry is on the record, and it moves no tokens
        effects = [{"kind": "break", "changeId": "", "saved": {}, "caused": {},
                    "note": "the entry expired; B* is 0, so the read extent to B* is empty"}]
    if effects:
        e["effects"] = effects
    if boundary:
        e["boundary"] = boundary
    if unpriced:
        e["unpriced"] = unpriced
    if checks:
        e["checks"] = checks
    return e


def hot_usage(prev_total, total):
    return U(read=prev_total, write=total - prev_total)


def case(cid, description, source, dims, steps, rates="fixture-v1", open_=None):
    c = {"id": cid, "description": description, "source": source, "dims": dims, "rates": rates}
    if open_:
        c["open"] = open_
    c["steps"] = steps
    return c


def write(c, name):
    with open(os.path.join(OUT, name + ".json"), "w") as f:
        json.dump(c, f, indent=1)
        f.write("\n")


# ---------------------------------------------------------------------------------------------
# Examples 1 and 2: one rewrite of a new message, on hot and cold requests.
SYS1 = [("sys1", 2000)]
M_BASE = [("m0", 4000)] + [(f"m{i}", 3000) for i in range(1, 9)]  # 30000 with the system block
CF = "cg:cmdfilter:c40e"


def cmdfilter_op():
    return rewrite("o1", "context-guru.cmdfilter", "messages", 9, 8000, 1000, CF)


def ex12():
    steps = [step(0, "r0", body([], SYS1, M_BASE), body([], SYS1, M_BASE), bps(M_BASE), bps(M_BASE), [],
                  U(write=30000), expect("unknown", U(write=30000), []))]
    c_msgs = M_BASE + [("m9", 8000)]
    f_msgs = M_BASE + [("m9f", 1000)]
    steps.append(step(10, "r1", body([], SYS1, c_msgs), body([], SYS1, f_msgs), bps(c_msgs), bps(f_msgs),
                      [cmdfilter_op()], U(read=30000, write=1000),
                      expect("hot", U(read=30000, write=8000),
                             [eop(CF, saved=TT(cacheWrite5m=7000), note="new content, would have been written")],
                             boundary=P("messages", 8))))
    one = case("rewrite/single/after-bstar/hit/5m",
               "One hot request. A new 8,000-token test output (message 9) is shortened to 1,000 tokens by "
               "cmdfilter. The cache holds messages 0 to 8, so B* is the end of message 8 and the 7,000 removed "
               "tokens would have been cache writes. Saves 7,000 x $3.75/M = $0.02625.",
               "examples.md#1", {"kind": "rewrite", "position": "afterBStarToL", "cache": "hit", "ttl": "5m",
                                 "lifetime": "exact"}, steps, open_=[BSTAR_SNAP])
    write(one, "rewrite-single-after-bstar")

    c2, f2 = c_msgs + [("m10", 500)], f_msgs + [("m10", 500)]
    steps.append(step(70, "r2", body([], SYS1, c2), body([], SYS1, f2), bps(c2), bps(f2), [],
                      U(read=31000, write=500),
                      expect("hot", U(read=38000, write=500),
                             [eop(CF, saved=TT(cacheRead=7000), note="applied again; the client's prefix would be cached")],
                             boundary=P("messages", 9))))
    c3, f3 = c2 + [("m11", 500)], f2 + [("m11", 500)]
    steps.append(step(1270, "r3", body([], SYS1, c3), body([], SYS1, f3), bps(c3), bps(f3), [],
                      U(write=32000),
                      expect("cold", U(write=39000),
                             [eop(CF, saved=TT(cacheWrite5m=7000), note="the entry expired; written again")])))
    write(case("rewrite/replay/hot-hot-cold/5m",
               "One rewrite, reported once on request 1 and applied again by Compute on requests 2 and 3. The "
               "same 7,000 tokens are priced three ways: write (hot, new content), read (hot, cached by request 1 "
               "without the change), write (cold, after a 20-minute gap). Total $0.05460.",
               "examples.md#2", {"kind": "rewrite", "position": "afterBStarToL", "cache": "hit-hit-miss",
                                 "ttl": "5m", "lifetime": "exact", "replay": "true"}, steps, open_=[BSTAR_SNAP]),
          "rewrite-replay-hot-hot-cold")


# ---------------------------------------------------------------------------------------------
# Tool declarations shared by examples 3, 6 and 7.
TOOL_SIZES = {2: 3500, 9: 2000, 15: 2500, 29: 300}
TOOLS_C = [(f"t{i}", TOOL_SIZES.get(i, 450)) for i in range(30)]  # 20000
TOOLS_F = [(i_t, t) for k, (i_t, t) in enumerate(TOOLS_C) if k not in (2, 9, 15)]  # 12000
TP = "tp:decl-set-7f3a"
SYS3 = [("sys3", 3000)]


def prune_op():
    spans = [{"section": "tools", "from": i, "to": i, "sizeBefore": size(t),
              "sizeAfter": size(0)} for i, t in ((2, 3500), (9, 2000), (15, 2500))]
    return rewrite("op1", "tool-prune", "tools", 2, 8000, 0, TP, spans=spans)


def ex3():
    msgs = [(f"m{i}", 5000) for i in range(4)]
    steps = []
    plan = [(0, "r0", [], "unknown", True), (60, "r1", [("m4", 2000)], "hot", False),
            (1260, "r2", [("m4", 2000), ("m5", 2000)], "cold", False)]
    prev_c = prev_f = 0
    for sec, rid, extra, phase, first in plan:
        m = msgs + extra
        c, f = body(TOOLS_C, SYS3, m), body(TOOLS_F, SYS3, m)
        ct, ft = tokens(TOOLS_C, SYS3, m), tokens(TOOLS_F, SYS3, m)
        if phase == "unknown":
            usage, base = U(write=ft), U(write=ct)
            ops = [eop(TP, saved=TT(cacheWrite5m=8000), note="first request: nothing is cached")]
            boundary = None
        elif phase == "hot":
            usage, base = hot_usage(prev_f, ft), hot_usage(prev_c, ct)
            ops = [eop(TP, saved=TT(cacheRead=8000), note="tools come first, so they are before B*")]
            boundary = P("messages", 3)
        else:
            usage, base = U(write=ft), U(write=ct)
            ops = [eop(TP, saved=TT(cacheWrite5m=8000), note="the entry expired")]
            boundary = None
        steps.append(step(sec, rid, c, f, bps(m), bps(m), [prune_op()] if first else [], usage,
                          expect(phase, base, ops, boundary=boundary)))
        prev_c, prev_f = ct, ft
    write(case("rewrite/tools-spans/first-hot-cold/5m",
               "tool-prune removes 3 tool declarations (3,500 + 2,000 + 2,500 = 8,000 tokens) on the first "
               "request of a thread, as one rewrite with three spans, and the removal applies again on later "
               "requests. The tools come first in the prefix, so the 8,000 tokens are read on a hot request "
               "($0.00240) and written on a cold one ($0.03000). The forwarded prefix never changes, so there is "
               "no cache break.",
               "examples.md#3", {"kind": "rewrite", "position": "tools", "cache": "unknown-hit-miss", "ttl": "5m",
                                 "lifetime": "exact", "spans": "true"}, steps, open_=[BSTAR_SNAP]),
          "rewrite-tools-spans")


# ---------------------------------------------------------------------------------------------
# Example 4: a summary that costs money first.
SU = "cg:summarize:5d21"


def ex4():
    tools, sysb = [("tools4", 4000)], [("sys4", 4000)]
    old = [("m0", 4000), ("m1", 3000), ("m2", 3000), ("m3", 1500), ("m4", 1500)]
    tail = old[3:]
    summary = [("summary", 500)]
    steps = [step(0, "r0", body(tools, sysb, old), body(tools, sysb, old), bps(old), bps(old), [],
                  U(write=21000), expect("unknown", U(write=21000), []))]
    spend = {"id": "op1", "kind": "spend", "operator": "context-guru.summarize", "changeId": SU,
             "call": {"model": "summarizer-model", "params": {"effort": "low"},
                      "usage": {"input": 10000, "cacheRead": 0, "cacheWrite": 0, "cacheWrite1h": 0,
                                "output": 500, "costUSD": 0.0125}}}
    rw = rewrite("op2", "context-guru.summarize", "messages", 0, 10000, 500, SU,
                 splice={"at": 0, "removed": 3, "inserted": 1})
    new = []
    prev_c, prev_f = 21000, 21000
    plan = [(60, "A", "hot"), (120, "B", "hot"), (1020, "C", "cold")]
    for k, (sec, rid, phase) in enumerate(plan):
        new.append((f"n{k}", 500))
        c, f = old + new, summary + tail + new
        ct, ft = tokens(tools, sysb, c), tokens(tools, sysb, f)
        if rid == "A":
            usage = U(read=8000, write=ft - 8000)  # the provider reads tools and system only
            base = hot_usage(prev_c, ct)
            ops = [eop(SU, call=spend["call"], note="the summarizer call: priced by the host"),
                   eop(SU, saved=TT(cacheRead=9500), note="would have been read")]
            effects = [effect("break", SU, saved=TT(cacheRead=3500), caused=TT(cacheWrite5m=3500),
                              frm=P("system", 0), to=P("messages", 2),
                              note="the summary and the tail are written, not read; to is in forwarded space (§6.2)")]
            add = [spend, rw]
            boundary = P("messages", 4)
        elif phase == "hot":
            usage, base = hot_usage(prev_f, ft), hot_usage(prev_c, ct)
            ops = [eop(SU, saved=TT(cacheRead=9500), note="applied again; the summary is cached now")]
            effects = []
            add, boundary = [], P("messages", 5)
        else:
            usage, base = U(write=ft), U(write=ct)
            ops = [eop(SU, saved=TT(cacheWrite5m=9500), note="the entry expired")]
            effects = []
            add, boundary = [], None
        steps.append(step(sec, rid, body(tools, sysb, c), body(tools, sysb, f), bps(c), bps(f), add,
                          usage, expect(phase, base, ops, boundary=boundary, effects=effects)))
        prev_c, prev_f = ct, ft
    write(case("rewrite+spend/summary/break-then-payback/5m",
               "A summarizer replaces messages 0 to 2 (10,000 tokens) with a 500-token summary. The call costs "
               "$0.01250 first (summarizer rates). Request A is hot, but the summary changes the start of the "
               "messages, so the provider reads only tools and system: the summary and the tail (3,500 tokens) are "
               "written, a break of 3,500 x ($3.75 - $0.30). A nets -$0.00923. B (hot) saves $0.00285, C (cold, 15 "
               "minutes later) saves $0.03563. The total after three requests, call included, is +$0.01675.",
               "examples.md#4", {"kind": "rewrite+spend", "position": "beforeBStar", "cache": "break-hit-miss",
                                 "ttl": "5m", "lifetime": "exact", "break": "plugin"}, steps, open_=[BSTAR_SNAP]),
          "rewrite-summary-break")


# ---------------------------------------------------------------------------------------------
# Examples 5a and 5b: a router changes one setting. Request 4 left 40,000 tokens cached (tools 5,000,
# system 3,000, messages 32,000). Request 5 adds one 1,000-token message.
def setting_case(cid, description, source, dims, change, model_to, params_to, settings, read, open_):
    tools, sysb = [("tools5", 5000)], [("sys5", 3000)]
    msgs = [(f"m{i}", 8000) for i in range(4)]
    model_from = "large-model"
    steps = [step(0, "req-4", body(tools, sysb, msgs), body(tools, sysb, msgs), bps(msgs), bps(msgs), [],
                  U(write=40000), expect("unknown", U(write=40000), []), session="s3", model=model_from,
                  params={"effort": "high"})]
    msgs5 = msgs + [("m4", 1000)]
    setting = {"id": "op1", "kind": "setting", "operator": "inference-router", "changeId": change,
               "settings": settings}
    moved = 40000 - read  # tokens the provider no longer reads
    start = P("system", 0) if read else None  # the provider still reads up to here
    steps.append(step(60, "req-5", body(tools, sysb, msgs5), body(tools, sysb, msgs5), bps(msgs5), bps(msgs5),
                      [setting], U(read=read, write=41000 - read),
                      expect("hot", U(read=40000, write=1000),
                             [],
                             effects=[effect("break", change, saved=TT(cacheRead=moved), caused=TT(cacheWrite5m=moved),
                                             frm=start, to=P("messages", 3),
                                             note=f"{moved:,} tokens move from read to write")],
                             boundary=P("messages", 3)),
                      session="s3", model=model_to, params=params_to))
    write(case(cid, description, source, dims, steps, open_=open_), cid.replace("/", "-"))


def ex5():
    settings_model = {"model": {"from": "large-model", "to": "small-model", "at": P("tools", 0)}}
    setting_case(
        "setting/model-change/break",
        "A router moves request 5 from large-model to small-model. The provider keeps a cache for each model, so "
        "request 5 reads 0 where request 4 left 40,000 cached. A model is rendered at the start of the prompt, so "
        "the setting is at tools[0] and the break covers the whole cached prefix: 40,000 tokens move from read to "
        "write. They are priced at the small model's rates (the proposed rule, design section 11 question 10): "
        "40,000 x ($1.00 - $0.08) = -$0.03680. The large model's rates would give -$0.13800. The saving of the "
        "cheaper model is not computed (R50).",
        "examples.md#5a", {"kind": "setting", "setting": "model", "position": "tools", "cache": "break",
                           "ttl": "5m", "lifetime": "exact", "break": "setting"},
        "rt:model-7c1a", "small-model", {"effort": "high"}, settings_model, read=0,
        open_=["design §11 question 10: which model's rates price a setting break", BSTAR_SNAP])
    settings_effort = {"effort": {"from": "high", "to": "low", "at": P("messages", 0)}}
    setting_case(
        "setting/effort-change/break-from-messages",
        "A router lowers the effort of request 5 from high to low, on the same large-model. On a model that does not "
        "render the effort ahead of the messages (cached-prefix.md), the tools and the system stay valid: the provider "
        "reads 8,000 tokens and writes the 32,000 tokens of messages 0 to 3 again. The two bodies are identical up to "
        "the new message, so a body comparison finds nothing; the setting carries its position, messages[0]. "
        "32,000 x ($3.75 - $0.30) = -$0.11040.",
        "examples.md#5b", {"kind": "setting", "setting": "effort", "position": "messages", "cache": "break",
                           "ttl": "5m", "lifetime": "exact", "break": "setting"},
        "rt:effort-3b9e", "large-model", {"effort": "low"}, settings_effort, read=8000,
        open_=[BSTAR_SNAP])


# ---------------------------------------------------------------------------------------------
# Examples 6 and 7: four operations on one request, and a change inside the cached prefix.
EXP = "cg:expand:91b0"
MK = "cg:mask:2a77"
COREF = "cg:coref:5e2d"
SIZES6 = {"m0": 4000, "m1": 3000, "m2": 3000, "m3": 4000, "m4": 1000, "m5": 20000, "m6": 2000, "m7": 1500,
          "m8": 1500, "m9": 8000, "m10": 1000, "m11": 1000, "m12": 1000, "m13": 1000}
ADDED = {1: ["m0", "m1"], 2: ["m2"], 3: ["m3", "m4"], 4: ["m5", "m6", "m7", "m8"], 5: ["m9"], 6: ["m10", "m11"],
         7: ["m12", "m13"]}
FWD_OVERRIDE = {"m3": ("m3f", 60), "m9": ("m9f", 900)}
OPS6 = {
    "prune": dict(change=TP, section="tools", idx=None, delta=8000),
    "expand": dict(change=EXP, section="tools", idx=None, delta=-300),
    "mask": dict(change=MK, section="messages", idx=3, delta=3940),
    "cmdfilter": dict(change=CF, section="messages", idx=9, delta=7100),
    "coref": dict(change=COREF, section="messages", idx=5, delta=19950),
}


def op_json(name):
    if name == "prune":
        return prune_op()
    if name == "expand":
        return rewrite("op2", "context-guru.expand", "tools", 27, 0, 300, EXP)
    if name == "mask":
        return rewrite("op3", "context-guru.mask", "messages", 3, 4000, 60, MK)
    if name == "cmdfilter":
        return rewrite("op4", "context-guru.cmdfilter", "messages", 9, 8000, 900, CF)
    return rewrite("op12", "context-guru.coref", "messages", 5, 20000, 50, COREF)


def thread6(last_turn):
    """The turns 1..last_turn of examples 6 and 7, with every expected figure."""
    reported_on = {1: ["prune", "expand"], 3: ["mask"], 5: ["cmdfilter"], 7: ["coref"]}
    expand_tool = [("expand", 300)]
    steps, active = [], []
    c_msgs, f_msgs = [], []
    prev_c = prev_f = prev_n = 0
    for k in range(1, last_turn + 1):
        for n in ADDED[k]:
            c_msgs.append((n, SIZES6[n]))
            f_msgs.append(FWD_OVERRIDE.get(n, (n, SIZES6[n])))
        reported = reported_on.get(k, [])
        now_active = [a for a in active] + reported
        if k == 7:  # coref reduces message 5 inside the cached prefix
            f_msgs[5] = ("m5f", 50)
        c, f = body(TOOLS_C, SYS3, c_msgs), body(TOOLS_F + expand_tool, SYS3, f_msgs)
        ct, ft = tokens(TOOLS_C, SYS3, c_msgs), tokens(TOOLS_F + expand_tool, SYS3, f_msgs)
        hot = k > 1
        phase = "hot" if hot else "unknown"
        ops = []
        for name in reported + active:
            spec = OPS6[name]
            read = hot and (spec["section"] == "tools" or spec["idx"] < prev_n)
            tier = "cacheRead" if read else "cacheWrite5m"
            if spec["delta"] >= 0:
                ops.append(eop(spec["change"], saved={tier: spec["delta"]}, note=name))
            else:
                ops.append(eop(spec["change"], caused={tier: -spec["delta"]}, note=name + ": an added declaration is a cost"))
        base = hot_usage(prev_c, ct) if hot else U(write=ct)
        usage = hot_usage(prev_f, ft) if hot else U(write=ft)
        effects = []
        if k == 7:
            usage = U(read=15300, write=ft - 15300)  # the provider reads tools and system only
            effects.append(effect("break", COREF, saved=TT(cacheRead=19010), caused=TT(cacheWrite5m=19010),
                                  frm=P("system", 0), to=P("messages", 11),
                                  note="19,010 tokens move from read to write"))
        boundary = P("messages", prev_n - 1) if hot else None
        steps.append(step(60 * (k - 1), f"req-{k}", c, f, bps(c_msgs), bps(f_msgs), [op_json(n) for n in reported],
                          usage, expect(phase, base, ops, boundary=boundary, effects=effects)))
        active = now_active
        prev_c, prev_f, prev_n = ct, ft, len(c_msgs)
    return steps


def ex67():
    write(case("rewrite/four-operations/hot/5m",
               "Turn 5 of one thread: cmdfilter is reported on this request, and tool-prune, expand and mask "
               "(reported on turns 1 and 3) are applied again. Turns 1 to 4 build the state. On turn 5 the three "
               "earlier operations change tokens before B* (read, and the added expand tool is a cost) and "
               "cmdfilter changes a new message (write). The four net $0.03012 = the client bill $0.04890 minus "
               "the forwarded bill $0.01878.",
               "examples.md#6", {"kind": "rewrite", "position": "mixed", "cache": "hit", "ttl": "5m",
                                 "lifetime": "exact", "operations": "4"}, thread6(5),
               open_=["design §11 question 12: how Compute knows the three earlier operations are still active", BSTAR_SNAP]),
          "rewrite-four-operations")
    write(case("rewrite/coref-break-inside-prefix/hot/5m",
               "Turn 7: coref reduces message 5 (20,000 tokens to 50), which is inside the prefix that turn 6 "
               "cached. The client did not change, so the change itself breaks the cache. The last breakpoint "
               "before message 5 is the end of system, so the provider reads 15,300 tokens and writes the rest. "
               "coref saves 19,950 x $0.30 = +$0.00599 and the break costs 19,010 x ($3.75 - $0.30) = -$0.06558, "
               "so coref nets -$0.05960 on this turn. The other four operations are applied again as before.",
               "examples.md#7", {"kind": "rewrite", "position": "beforeBStar", "cache": "break", "ttl": "5m",
                                 "lifetime": "exact", "break": "plugin"}, thread6(7),
               open_=["design §11 question 12: how Compute knows the earlier operations are still active", BSTAR_SNAP]),
          "rewrite-coref-break")


# ---------------------------------------------------------------------------------------------
# Example 8: a keep-alive ping and the request it rescues.
KA = "ka:main:w1"


def ex8():
    sysb = [("sys8", 2000)]
    msgs = [("m0", 3000), ("m1", 1000), ("m2", 1000), ("m3", 1000), ("m4", 1000), ("m5", 1000)]
    ping_usage = {"input": 10, "cacheRead": 10000, "cacheWrite": 0, "cacheWrite1h": 0, "output": 1}
    call = {"host": "api.anthropic.com", "model": "agent-model", "usage": ping_usage}
    steps = [step(0, "turn-9", body([], sysb, msgs), body([], sysb, msgs), bps(msgs), bps(msgs), [],
                  U(write=10000), expect("unknown", U(write=10000), []), session="s2")]
    ping = {"version": 1, "requestId": "ping-p1", "session": "s2", "thread": {"id": "main"}, "at": ts(240),
            "billing": {"mode": "api"},
            "ops": [{"id": "op1", "kind": "spend", "operator": "keeper", "changeId": KA, "call": call}]}
    steps.append({"standalone": ping, "usage": U(),
                  "expected": {"ops": [eop(KA, call=call, note="the ping: priced by the host, 3,045 micro-dollars")]}})
    msgs10 = msgs + [("m6", 500)]
    facts = {"ttl": "5m", "lifetime": "exact", "lastPing": ts(240), "pingRead": P("messages", 5), "pingWindow": KA}
    steps.append(step(480, "turn-10", body([], sysb, msgs10), body([], sysb, msgs10), bps(msgs10), bps(msgs10), [],
                      U(read=10000, write=500),
                      expect("hot", U(write=10500), [],
                             effects=[effect("rescue", KA, saved=TT(cacheWrite5m=10000), caused=TT(cacheRead=10000),
                                             to=P("messages", 5),
                                             note="without the ping the 5m entry would have expired; write becomes read")]),
                      session="s2", facts=facts))
    write(case("keepalive/ping-rescue/exact/5m",
               "Turn 9 caches 10,000 tokens at minute 20. A keep-alive ping at minute 24 re-reads them (10,000 read, "
               "10 input, 1 output: -$0.00305) and renews the entry. Turn 10 arrives at minute 28, 8 minutes after "
               "turn 9 and so past the 5m TTL, but 4 minutes after the ping and so inside it. Lifetime is exact, so "
               "the expiry without a ping is proven and the rescue is credited: 10,000 x ($3.75 - $0.30) = "
               "+$0.03450. The ping and the rescue share one ChangeID. Total +$0.03146.",
               "examples.md#8", {"kind": "rescue", "position": "beforeBStar", "cache": "rescue", "ttl": "5m",
                                 "lifetime": "exact", "keepalive": "true"}, steps),
          "keepalive-ping-rescue")


# ---------------------------------------------------------------------------------------------
# Example 9: the extract_llm_sweep story. The ask goes out in place of a keep-alive ping, on the warm
# cache; the answer is held; the removal is applied by the first request after the cache expired.
SW = "cg:extract_llm_sweep:7a1c"


def ex9():
    sysb = [("sys9", 2000)]
    base = [("m0", 3000), ("m1", 8000), ("m2", 1000), ("m3", 1000), ("m4", 1000), ("m5", 1000)]  # m1: a spent output
    steps = [step(0, "sw-0", body([], sysb, base), body([], sysb, base), bps(base), bps(base), [],
                  U(write=17000), expect("unknown", U(write=17000), []), session="s3")]
    m6 = base + [("m6", 500)]
    steps.append(step(100, "sw-1", body([], sysb, m6), body([], sysb, m6), bps(m6), bps(m6), [],
                      U(read=17000, write=500), expect("hot", U(read=17000, write=500), []), session="s3"))
    # The ask: the previous turn's forwarded bytes (17,500 tokens) plus a 400-token inventory question.
    # It reads the cache sw-1 left, so the entry is renewed; it replaces the ping. 280 s after sw-1.
    call = {"host": "api.anthropic.com", "model": "agent-model",
            "usage": {"input": 400, "cacheRead": 17500, "cacheWrite": 0, "cacheWrite1h": 0, "output": 200}}
    ask = {"version": 1, "requestId": "sw-ask-1", "session": "s3", "thread": {"id": "main"}, "at": ts(380),
           "billing": {"mode": "api"},
           "ops": [{"id": "op1", "kind": "spend", "operator": "context-guru.extract_llm_sweep", "changeId": SW,
                    "call": call}]}
    steps.append({"standalone": ask, "usage": U(),
                  "expected": {"ops": [eop(SW, call=call, note="the ask, sent in place of the ping: priced by the host, 9,450 micro-dollars")]}})
    # The user leaves; the keep-alive budget ends; the entry expires. sw-2 arrives 1,420 s after the ask.
    c2 = m6 + [("m7", 500)]
    f2 = [base[0], ("m1f", 100)] + base[2:] + [("m6", 500), ("m7", 500)]
    facts = {"ttl": "5m", "lifetime": "exact", "lastPing": ts(380), "pingRead": P("messages", 6)}
    rw = rewrite("op2", "context-guru.extract_llm_sweep", "messages", 1, 8000, 100, SW)
    steps.append(step(1800, "sw-2", body([], sysb, c2), body([], sysb, f2), bps(c2), bps(f2), [rw],
                      U(write=10100),
                      expect("cold", U(write=18000),
                             [eop(SW, saved=TT(cacheWrite5m=7900),
                                  note="the held verdict is applied on the cold request; the output would have been written")]),
                      session="s3", facts=facts))
    write(case("rewrite+spend/extract-llm-sweep/ask-then-cold-apply/5m",
               "A spent 8,000-token tool output (message 1) sits in a 17,500-token thread. At minute 6:20 the "
               "keep-alive ping is replaced by the sweep's ask: the previous turn's forwarded body plus a "
               "400-token question, so it reads 17,500 tokens from the warm cache and renews the entry (400 "
               "input, 17,500 read, 200 output: -$0.00945). The model says drop; the verdict is held and changes "
               "no request while the cache is warm, because a removal inside the cached prefix would be a break. "
               "The keep-alive ends and the entry expires. The next request arrives 23 minutes later, past the "
               "5m TTL of both the last request and the ask, so no rescue is credited and nothing is cached: "
               "the held verdict is applied and the 7,900 removed tokens (8,000 to a 100-token descriptor with a "
               "<<cg:HASH>> marker) are priced as cache writes, 7,900 x $3.75 = +$0.02963. The ask and the "
               "removal share one ChangeID. Total +$0.02018.",
               "advanced-offload.md#extract_llm_sweep",
               {"kind": "rewrite+spend", "position": "beforeBStar", "cache": "hit-ask-miss", "ttl": "5m",
                "lifetime": "exact", "keepalive": "true", "sweep": "true"}, steps),
          "rewrite-spend-sweep-ask-then-cold-apply")


# ---------------------------------------------------------------------------------------------
# Cases derived from issues (docs/counterfactual/issue_cases.md). Each id's source names the issue.
ISSUES = "issue_cases.md"


def sim(sec, rid, C, F, ops, phase, add=(), prev=(0, 0), effects=None, usage=None, base=None,
        boundary=None, cbp=None, fbp=None, unpriced=None, checks=None, **kw):
    """One request step. C and F are (tools, system, msgs) of the client and forwarded bodies.
    hot: the provider reads the previous body's tokens and writes the rest; unknown/cold: writes all."""
    ct, ft = tokens(*C), tokens(*F)
    if phase == "hot":
        u, b = hot_usage(prev[1], ft), hot_usage(prev[0], ct)
    else:
        u, b = U(write=ft), U(write=ct)
    if phase == "unknown" and unpriced is None:  # §5.2 row 6 and §6.4: a first request has no known boundary
        unpriced = [{"reason": "penalty_unknown"}]  # request-level: the boundary is unknown, no operation owns it
    e = expect(phase, base or b, ops, boundary=boundary, unpriced=unpriced, effects=effects, checks=checks)
    return step(sec, rid, body(*C), body(*F), bps(C[2]) if cbp is None else cbp, bps(F[2]) if fbp is None else fbp, list(add),
                usage or u, e, **kw)


def BP(*pairs, ttl="5m"):
    return [{"pos": P(sec, idx), "ttl": ttl} for sec, idx in pairs]


def ping_step(sec, session, change, call, window_unpriced=None):
    ping = {"version": 1, "requestId": f"ping-{sec}", "session": session, "thread": {"id": "main"}, "at": ts(sec),
            "billing": {"mode": "api"},
            "ops": [{"id": "op1", "kind": "spend", "operator": "keeper", "changeId": change, "call": call}]}
    exp = {"ops": [eop(change, call=call)]}
    if window_unpriced:
        exp["unpriced"] = window_unpriced
    return {"standalone": ping, "usage": U(), "expected": exp}


def DIMS(kind, position, cache, **kw):
    d = {"kind": kind, "position": position, "cache": cache, "ttl": "5m", "lifetime": "exact"}
    d.update(kw)
    return d


SNAP_CASES = {"rewrite/searchfold-inside-prefix/hot/5m", "rewrite/decision-flips-inside-prefix/hot/5m",
              "rewrite/replay-at-new-position/hot/5m", "rewrite/break-then-payback-turns/hot/5m",
              "rewrite/restore-in-place/hot/5m", "state/session-isolation", "state/tenant-collision",
              "split/marker-not-in-system", "check/changeid-shared-between-operators"}


def write_issue(cid, issue, description, dims, steps, open_=None):
    if cid in SNAP_CASES and BSTAR_SNAP not in (open_ or []):
        open_ = (open_ or []) + [BSTAR_SNAP]
    if cid == "break/client-edit-inside-prefix/hot/5m":  # B* is messages[9] under either reading
        open_ = [o for o in (open_ or []) if o != BSTAR_SNAP] or None
    write(case(cid, description, f"{ISSUES}#{issue}", dims, steps, open_=open_), cid.replace("/", "-").replace("+", "-"))


Q12 = "design §11 question 12: the rule that finds the end of a change is not defined"
NOTENANT = "RequestMeta has no tenant field, and ThreadKey is session + thread: the design does not say where a tenant lives"


# Case 1 (#580): a ping that missed and wrote the entry restarts the clock.
def c01():
    sysb, m0 = [("sys", 2000)], [("m0", 18000)]
    KAW = "ka:main:w2"
    call = {"host": "api.anthropic.com", "model": "agent-model",
            "usage": {"input": 10, "cacheRead": 0, "cacheWrite": 20000, "cacheWrite1h": 0, "output": 1}}
    m1 = m0 + [("m1", 500)]
    facts = {"ttl": "5m", "lifetime": "exact", "lastPing": ts(360), "pingRead": P("messages", 0), "pingWindow": KAW}
    steps = [sim(0, "r0", ([], sysb, m0), ([], sysb, m0), [], "unknown", session="s-kp"),
             ping_step(360, "s-kp", KAW, call),
             sim(540, "r1", ([], sysb, m1), ([], sysb, m1), [], "hot", prev=(20000, 20000),
                 usage=U(read=20000, write=500), base=U(write=20500), facts=facts, session="s-kp",
                 effects=[effect("rescue", KAW, saved=TT(cacheWrite5m=20000), caused=TT(cacheRead=20000),
                                 to=P("messages", 0),
                                 note="the ping missed and wrote the entry: the entry is alive from minute 6")])]
    write_issue("keepalive/ping-miss-restarts-clock/5m", 580,
                "The entry (20,000 tokens) expired before the ping at minute 6, so the ping read 0 and wrote it again. "
                "Turn 1 arrives at minute 9: past the TTL measured from turn 0, inside it measured from the ping. The "
                "entry gap runs from LastPing, so the phase is hot and the rescue is credited: 20,000 x ($3.75 - $0.30) "
                "= +$0.06900. The ping costs 20,000 x $3.75 + 10 x $3.00 + 1 x $15.00 = -$0.07505, so the window nets "
                "-$0.00605: a ping that missed pays for itself at the read price only.",
                DIMS("rescue", "beforeBStar", "rescue", keepalive="true", ping="missed-and-wrote"), steps,
                open_=["design §4.4 rule 4 takes the smaller of what the last ping READ and what the request read. A ping "
                       "that missed read 0 but re-created the entry, so read literally the rescue is 0. This case credits "
                       "the extent the ping covered (PingRead). The rule needs to say so",
                       "design §4.1 defines PingRead as the extent the ping READ, which is nil for a ping that missed; "
                       "this case sets it to messages[0] so that row 2 of the phase table matches. Both need amending"])


# Case 2 (#532): a ping whose answer never arrived.
def c02():
    sysb = [("sys", 2000)]
    m0, m1 = [("m0", 197103)], [("m0", 197103), ("m1", 387)]
    KAW = "ka:main:w3"
    call = {"host": "api.anthropic.com", "model": "agent-model",
            "usage": {"input": 0, "cacheRead": 0, "cacheWrite": 0, "cacheWrite1h": 0, "output": 0}}
    facts = {"ttl": "5m", "lifetime": "exact", "lastPing": ts(271), "pingRead": P("messages", 0), "pingWindow": KAW}
    steps = [sim(0, "r0", ([], sysb, m0), ([], sysb, m0), [], "unknown", session="s-slow"),
             ping_step(271, "s-slow", KAW, call, [{"opId": "op1", "reason": "ping_result_unknown"}]),
             sim(526, "r1", ([], sysb, m1), ([], sysb, m1), [], "hot", prev=(199103, 199103),
                 usage=U(read=199103, write=387), base=U(write=199490), facts=facts, session="s-slow",
                 effects=[effect("rescue", KAW, saved=TT(cacheWrite5m=199103), caused=TT(cacheRead=199103),
                                 to=P("messages", 0), note="proved by the read on this request")])]
    write_issue("keepalive/ping-timeout-unknown-result/5m", 532,
                "The proxy gave up on a ping after 60 seconds, so the host has no usage for it. The next turn comes 8 "
                "minutes 46 seconds after the last one and reads 199,103 of 199,490 tokens. The ping is reported as a "
                "spend with empty usage and the library marks it unpriced (ping_result_unknown). The rescue is credited "
                "from the read on the request, which proves the entry was alive. The window shows a rescue and an "
                "unpriced ping, never a rescue with a silent zero cost.",
                DIMS("rescue", "beforeBStar", "rescue", keepalive="true", ping="timed-out"), steps,
                open_=["new: the design has no form for a ping whose usage is unknown. This case assumes the host reports "
                       "an empty-usage spend and the library answers Unpriced ping_result_unknown (option c of issue_cases.md case 2)",
                       "design §4.4 rule 4 credits the smaller of what the last ping read and what the request read. The "
                       "ping's read is unknown here, so the credit of 199,103 tokens rests on the request's read alone"])


# Cases 3 and 4 (#496): the body changes inside the cached prefix on a warm request.
SF = "cg:searchfold:6b02"


def c03_04():
    tools, sysb = [("tools", 6000)], [("sys", 2000)]
    base = [(f"m{i}", 2500) for i in range(10)] + [("m10", 3000)] + [(f"m{i}", 2800) for i in range(11, 16)]
    bp = lambda last: BP(("system", 0), ("messages", 9), ("messages", last))
    s0 = sim(0, "r0", (tools, sysb, base), (tools, sysb, base), [], "unknown", cbp=bp(15), fbp=bp(15))
    tail = [("m16", 2000)]
    fwd = [m if m[0] != "m10" else ("m10f", 500) for m in base] + tail
    op = rewrite("o1", "context-guru.searchfold", "messages", 10, 3000, 500, SF)
    s1 = sim(40, "r1", (tools, sysb, base + tail), (tools, sysb, fwd), [eop(SF, saved=TT(cacheRead=2500))],
             "hot", add=[op], cbp=bp(16), fbp=bp(16), usage=U(read=33000, write=16500), prev=(50000, 50000),
             boundary=P("messages", 15),
             effects=[effect("break", SF, saved=TT(cacheRead=14500), caused=TT(cacheWrite5m=14500),
                             frm=P("messages", 9), to=P("messages", 15),
                             note="the marker and everything after it are written, not read")])
    write_issue("rewrite/searchfold-inside-prefix/hot/5m", 496,
                "A warm request (40 seconds after the last). searchfold shortens message 10, a 3,000-token output "
                "inside the cached prefix, to 500 tokens. The client breakpoints are the end of system, message 9 and "
                "the last message, so the provider reads through message 9 (33,000 tokens) and writes the other 16,500. "
                "searchfold saves 2,500 x $0.30 = +$0.00075 and breaks the cache on 14,500 tokens, 14,500 x ($3.75 - "
                "$0.30) = -$0.05003: net -$0.04928. The break carries searchfold's ChangeID, so the cost is visible "
                "where the dashboard in issue 496 showed a hit.",
                DIMS("rewrite", "beforeBStar", "break", **{"break": "plugin"}), [s0, s1])

    cl = [m if m[0] != "m10" else ("m10c", 500) for m in base] + tail
    s1b = sim(40, "r1", (tools, sysb, cl), (tools, sysb, cl), [], "hot", cbp=bp(16), fbp=bp(16),
              usage=U(read=33000, write=16500), base=U(read=33000, write=16500), boundary=P("messages", 9),
              effects=[effect("break", "", to=P("messages", 9),
                              note="the client changed message 10 itself: B* is the breakpoint at message 9, "
                                   "so the break moves no tokens")])
    write_issue("break/client-edit-inside-prefix/hot/5m", 496,
                "The same usage as the searchfold case (33,000 read, 16,500 written), but no context operation touched "
                "the cached part: the client shortened message 10 itself. The break is on the record with an empty "
                "ChangeID (R31) and, because B* is the breakpoint at message 9, moves no tokens (§5.1, §6.2): the "
                "baseline equals the bill and no operator gets a saving or a cost.",
                DIMS("none", "beforeBStar", "break", **{"break": "client"}), [s0, s1b], open_=[BSTAR_SNAP])


# Case 5 (#493): a decision flips on a cached message.
DD = "cg:dedup:4c18"


def c05():
    sysb = [("sys", 2000)]
    m = [("m0", 3000)] + [(f"m{i}", 2000) for i in range(1, 7)]
    bp = lambda last: BP(("system", 0), ("messages", 4), ("messages", last))
    s1 = sim(0, "r1", ([], sysb, m), ([], sysb, m), [], "unknown", cbp=bp(6), fbp=bp(6))
    c2 = m + [("m7", 1000)]
    f2 = [x if x[0] != "m5" else ("m5f", 100) for x in c2]
    s2 = sim(30, "r2", ([], sysb, c2), ([], sysb, f2), [eop(DD, saved=TT(cacheRead=1900))], "hot",
             add=[rewrite("o1", "context-guru.dedup", "messages", 5, 2000, 100, DD)], prev=(17000, 17000),
             usage=U(read=13000, write=3100), cbp=bp(7), fbp=bp(7), boundary=P("messages", 6),
             effects=[effect("break", DD, saved=TT(cacheRead=2100), caused=TT(cacheWrite5m=2100),
                             frm=P("messages", 4), to=P("messages", 6))])
    c3, f3 = c2 + [("m8", 1000)], f2 + [("m8", 1000)]
    s3 = sim(70, "r3", ([], sysb, c3), ([], sysb, f3), [eop(DD, saved=TT(cacheRead=1900))], "hot",
             prev=(18000, 16100), cbp=bp(8), fbp=bp(8), boundary=P("messages", 7))
    c4 = c3 + [("m9", 1000)]
    s4 = sim(100, "r4", ([], sysb, c4), ([], sysb, c4), [], "hot", prev=(19000, 17100),
             usage=U(read=13000, write=7000), cbp=bp(9), fbp=bp(9), boundary=P("messages", 8), checks=["attribution"],
             effects=[effect("break", DD, saved=TT(cacheRead=6000), caused=TT(cacheWrite5m=6000),
                             frm=P("messages", 4), to=P("messages", 8),
                             note="dedup stopped replacing message 5: the original returns inside the prefix")])
    write_issue("rewrite/decision-flips-inside-prefix/hot/5m", 493,
                "Message 5 repeats message 2. Request 1 runs with dedup off. On request 2 dedup turns on and replaces "
                "message 5 (2,000 to 100 tokens), which is inside the cached prefix: it saves 1,900 x $0.30 and breaks "
                "2,100 tokens, net -$0.00668. Request 3 replays the same replacement: no break, +$0.00057. On request 4 "
                "dedup stops (its settings changed), the original returns inside the cached prefix and 6,000 tokens move "
                "from read to write: -$0.02070, charged to dedup's ChangeID although dedup reported nothing.",
                DIMS("rewrite", "beforeBStar", "break-hit-break", **{"break": "plugin", "replay": "true"}), [s1, s2, s3, s4],
                open_=[Q12 + " (request 4: the change ended, and the break is charged to it)"])


# Case 6 (#199): a replay at a new position that was never compacted.
EL, EL2 = "cg:extract_llm:3d10", "cg:extract_llm:b9f1"


def c06():
    sysb = [("sys", 2000)]
    head = [("m0", 2000), ("m1", 2000), ("m2", 2000)]
    cm0 = head + [("m3", 6000)]
    fm0 = head + [("m3f", 500)]
    s0 = sim(0, "r0", ([], sysb, cm0), ([], sysb, fm0), [eop(EL, saved=TT(cacheWrite5m=5500))], "unknown",
             add=[rewrite("o1", "context-guru.extract_llm", "messages", 3, 6000, 500, EL)])
    mid = [(f"m{i}", 1000) for i in range(4, 12)]
    mid = [m if m[0] != "m8" else ("m8", 6000) for m in mid]  # the same 6,000-token output, second occurrence
    c1, f1 = cm0 + mid + [("m12", 1000)], fm0 + mid + [("m12", 1000)]
    bp = lambda last: BP(("system", 0), ("messages", 11), ("messages", last))
    # the provider reads only up to a breakpoint (§5.1), so the 12,500 tokens it read end at one (§6.2)
    bp7 = lambda last: BP(("system", 0), ("messages", 7), ("messages", 11), ("messages", last))
    s1 = sim(60, "r1", ([], sysb, c1), ([], sysb, f1), [eop(EL, saved=TT(cacheRead=5500))], "hot",
             prev=(14000, 8500), cbp=bp(12), fbp=bp(12), boundary=P("messages", 3))
    c2 = c1 + [("m13", 1000)]
    mid2 = [m if m[0] != "m8" else ("m8f", 500) for m in mid]
    f2 = fm0 + mid2 + [("m12", 1000), ("m13", 1000)]
    s2 = sim(120, "r2", ([], sysb, c2), ([], sysb, f2),
             [eop(EL2, saved=TT(cacheRead=5500)), eop(EL, saved=TT(cacheRead=5500))], "hot",
             add=[rewrite("o2", "context-guru.extract_llm", "messages", 8, 6000, 500, EL2)], prev=(28000, 22500),
             usage=U(read=12500, write=5500), cbp=bp7(13), fbp=bp7(13), boundary=P("messages", 12),
             effects=[effect("break", EL2, saved=TT(cacheRead=4500), caused=TT(cacheWrite5m=4500),
                             frm=P("messages", 7), to=P("messages", 12),
                             note="the break starts at the read extent (§6.2). Message 8 was never compacted, so the provider holds the original and "
                                  "everything after it is written again")])
    write_issue("rewrite/replay-at-new-position/hot/5m", 199,
                "The same 6,000-token output appears at message 3 and at message 8. extract_llm compacted message 3 "
                "when it arrived, so the provider holds the compacted form there and the replay is free of any break. "
                "Message 8 was not compacted on arrival (the operator was throttled). On request 2 the operator compacts "
                "it inside the cached prefix: a second change with its own ChangeID saves 5,500 x $0.30 = +$0.00165 and "
                "breaks the 4,500 tokens from message 8 to the end of the prefix, 4,500 x $3.45 = -$0.01553: the break "
                "is larger than the saving, which is the cost issue 199 hid. The replay of the first change stays "
                "break-free.",
                DIMS("rewrite", "beforeBStar", "hit-hit-break", **{"break": "plugin", "replay": "true"}), [s0, s1, s2])


# Case 7 (#197): two operators report one ChangeID.
def c07():
    sysb = [("sys", 2000)]
    m0 = [("m0", 2000)]
    CH = "cg:res:5c1d"
    s0 = sim(0, "r0", ([], sysb, m0), ([], sysb, m0), [], "unknown")
    c1 = m0 + [("m1", 6000), ("m2", 7000)]
    f1 = m0 + [("m1f", 500), ("m2f", 800)]
    s1 = sim(40, "r1", ([], sysb, c1), ([], sysb, f1),
             [eop(CH, saved=TT(cacheWrite5m=5500)), eop(CH, saved=TT(cacheWrite5m=6200))], "hot",
             add=[rewrite("o1", "context-guru.extract_llm", "messages", 1, 6000, 500, CH),
                  rewrite("o2", "context-guru.extract_llm_sweep", "messages", 2, 7000, 800, CH)],
             prev=(4000, 4000), boundary=P("messages", 0), checks=["unique_change"])
    write_issue("check/changeid-shared-between-operators", 197,
                "extract_llm and extract_llm_sweep share the cg:res: namespace, so both report the same ChangeID for "
                "different messages and different sizes. The library prices each operation by its own size (5,500 and "
                "6,200 tokens at the write tier) and the unique_change check (§6.5 check 3) fails and is reported. A failed check does not stop "
                "pricing.",
                DIMS("rewrite", "afterBStar", "hit", operations="2", check="unique_change"), [s0, s1])
    # The negative variant: one operator reports the same ChangeID on two requests. That is a replay.
    rc0, rf0 = m0 + [("m1", 6000)], m0 + [("m1f", 500)]
    r0 = sim(0, "r0", ([], sysb, rc0), ([], sysb, rf0), [eop(CH, saved=TT(cacheWrite5m=5500))], "unknown",
             add=[rewrite("o1", "context-guru.extract_llm", "messages", 1, 6000, 500, CH)])
    rc1, rf1 = rc0 + [("m3", 1000)], rf0 + [("m3", 1000)]
    r1 = sim(40, "r1", ([], sysb, rc1), ([], sysb, rf1), [eop(CH, saved=TT(cacheRead=5500))], "hot",
             prev=(10000, 4500), boundary=P("messages", 1))
    write_issue("check/changeid-same-operator-replay", 197,
                "The negative variant of the shared-ChangeID case: one operator reports the same ChangeID on two "
                "requests. That is a replay (§4.6), so the unique_change check does not fail and the second request "
                "saves the same 5,500 tokens at the read tier.",
                DIMS("rewrite", "beforeBStar", "hit", operations="1", replay="true"), [r0, r1])


# Case 8 (#214): model calls whose removal was refused.
def spend_call(inp, out, cost):
    return {"model": "summarizer-model", "params": {"effort": "low"},
            "usage": {"input": inp, "cacheRead": 0, "cacheWrite": 0, "cacheWrite1h": 0, "output": out, "costUSD": cost}}


def c08():
    sysb, m0 = [("sys", 2000)], [("m0", 4000)]
    CH = "cg:extract_llm:77e0"
    call = spend_call(6000, 400, 0.008)
    sp = lambda i, ch: {"id": f"op{i}", "kind": "spend", "operator": "context-guru.extract_llm", "changeId": ch, "call": call}
    c1 = m0 + [("m1", 1000)]
    s0 = sim(0, "r0", ([], sysb, m0), ([], sysb, m0), [], "unknown")
    s1 = sim(40, "r1", ([], sysb, c1), ([], sysb, c1), [eop(CH, call=call)], "hot", add=[sp(1, CH)],
             prev=(6000, 6000), boundary=P("messages", 0))
    write_issue("spend/without-rewrite/refused", 214,
                "extract_llm paid for one model call (6,000 input, 400 output: $0.00800 at summarizer rates) but the "
                "reserve was full, so the pipeline refused the removal. No rewrite is reported. The spend is priced, "
                "nothing is saved and nothing breaks: the request's net is exactly minus the call.",
                DIMS("spend", "none", "hit"), [s0, s1],
                open_=["design §4.1 says a spend's ChangeID names the change its call created, and R47 gives a spend that "
                       "creates no saving an empty ChangeID. The removal was refused, so the empty form may be the right "
                       "one. This case keeps a ChangeID; the numbers do not depend on it"])
    steps = []
    for k, sess in enumerate(("s-a", "s-b", "s-c")):
        steps.append(sim(k, f"r0-{sess}", ([], sysb, m0), ([], sysb, m0), [], "unknown", session=sess))
    for k, sess in enumerate(("s-a", "s-b", "s-c")):
        ch = CH  # rule 6: the same operator and content give the same ChangeID in every session
        steps.append(sim(40 + k, f"r1-{sess}", ([], sysb, c1), ([], sysb, c1),
                         [eop(ch, call=call), eop(ch, call=call)], "hot", add=[sp(1, ch), sp(2, ch)],
                         prev=(6000, 6000), boundary=P("messages", 0), session=sess))
    write_issue("spend/without-rewrite/refused-three-sessions", 214,
                "Three sessions share one process and one full reserve. Each pays two calls and each loses both. Every "
                "session's net is minus its own two calls ($0.01600) and no session carries another's cost.",
                DIMS("spend", "none", "hit", sessions="3"), steps)


# Case 9 (#232): a break that pays back over many turns.
CO = "cg:coref:7a03"


def c09():
    sysb = [("sys", 2000)]
    head = [("m0", 4000)]
    xo, tail = [("m1", 20000)], [("m2", 10000), ("m3", 10000)]
    bp = lambda last: BP(("system", 0), ("messages", 0), ("messages", last))
    c0 = head + xo + tail
    steps = [sim(0, "r0", ([], sysb, c0), ([], sysb, c0), [], "unknown", cbp=bp(3), fbp=bp(3))]
    fwd = head + [("m1f", 100)] + tail
    pc, pf = tokens(sysb, c0), tokens(sysb, fwd) + 19900 - 19900
    c, f = c0 + [("m4", 1000)], fwd + [("m4", 1000)]
    op = rewrite("o1", "context-guru.coref", "messages", 1, 20000, 100, CO)
    steps.append(sim(40, "r1", ([], sysb, c), ([], sysb, f), [eop(CO, saved=TT(cacheRead=19900))], "hot", add=[op],
                     prev=(46000, 46000), cbp=bp(4), fbp=bp(4), usage=U(read=6000, write=21100),
                     boundary=P("messages", 3),
                     effects=[effect("break", CO, saved=TT(cacheRead=20100), caused=TT(cacheWrite5m=20100),
                                     frm=P("messages", 0), to=P("messages", 3))]))
    pc, pf = tokens(sysb, c), tokens(sysb, f)
    for k in range(2, 13):  # 11 hot replays: the break is repaid on the 11th
        c, f = c + [(f"m{k + 3}", 1000)], f + [(f"m{k + 3}", 1000)]
        steps.append(sim(40 + 60 * (k - 1), f"r{k}", ([], sysb, c), ([], sysb, f),
                         [eop(CO, saved=TT(cacheRead=19900))], "hot", prev=(pc, pf), cbp=bp(len(c) - 1),
                         fbp=bp(len(f) - 1), boundary=P("messages", len(c) - 2)))
        pc, pf = tokens(sysb, c), tokens(sysb, f)
    c, f = c + [("m16", 1000)], f + [("m16", 1000)]
    steps.append(sim(40 + 60 * 11 + 1500, "r13", ([], sysb, c), ([], sysb, f),
                     [eop(CO, saved=TT(cacheWrite5m=19900))], "cold", cbp=bp(len(c) - 1), fbp=bp(len(f) - 1),
                     effects=[effect("break", "", note="a cold request: the break moves no tokens (§5.2 row 5)")]))
    write_issue("rewrite/break-then-payback-turns/hot/5m", 232,
                "coref reduces a 20,000-token output (message 1) inside the cached prefix with 20,000 tokens after it. "
                "Request 1 saves 19,900 x $0.30 = +$0.00597 and breaks 20,100 tokens, 20,100 x $3.45 = -$0.06935: net "
                "-$0.06338. Each of the next 11 hot requests saves +$0.00597. The cumulative net is still -$0.00368 after "
                "10 replays and turns positive on the 11th (+$0.00230). A cold request 25 minutes later saves 19,900 x "
                "$3.75 = +$0.07463, because the content would have been written.",
                DIMS("rewrite", "beforeBStar", "break-then-hits-then-miss", **{"break": "plugin", "replay": "true"}), steps)


# Case 10 (#47): the state is lost, the request already reads from the cache.
def c10():
    sysb = [("sys", 2000)]
    C = [("m0", 4000), ("m1", 6000), ("m2", 6000), ("m3", 6000), ("m4", 2000)]
    F = [("m0", 4000), ("m1f", 500), ("m2", 6000), ("m3", 6000), ("m4", 2000)]
    CH = "cg:extract:1a40"
    s = sim(0, "r0", ([], sysb, C), ([], sysb, F), [eop(CH, saved=TT(cacheRead=5500))], "hot",
            add=[rewrite("o1", "context-guru.extract", "messages", 1, 6000, 500, CH)],
            usage=U(read=18500, write=2000), base=U(read=24000, write=2000),
            unpriced=[{"reason": "penalty_unknown"}], session="s-restart")
    write_issue("boundary/state-lost-reads-cache", 47,
                "The proxy restarted: the Tracker has no history for the thread. The replacement of message 1 (6,000 to "
                "500 tokens) is replayed from the store at depth, and the provider reads 18,500 tokens, so it holds the "
                "compacted form. The usage proves the entry (§5.2), so the phase is hot and the saved tokens are priced "
                "as a read (5,500 x $0.30 = +$0.00165), never as a write. No break can be charged without a previous "
                "fingerprint, so penalty_unknown is reported: the net is not a measured figure.",
                DIMS("rewrite", "beforeBStar", "unknown-read-by-evidence", boundary="unknown"), [s],
                open_=["design §5.2 row 6 says the boundary is unknown, and the evidence rule says to use the extent the read "
                       "covers, which would make it known and §6.4 would not apply. This case takes row 6 and §6.4"])


# Case 11 (#555): the client body's size follows from the result.
def c11():
    sysb = [("sys", 2000)]
    C = [("m0", 4000), ("m1", 10000), ("m2", 10000), ("m3", 10000), ("m4", 2000)]
    F = [("m0", 4000), ("m1f", 1000), ("m2f", 1000), ("m3f", 6000), ("m4", 2000)]
    A, B, D = "cg:mask:aa01", "cg:mask:aa02", "cg:cmdfilter:aa03"
    s = sim(0, "r0", ([], sysb, C), ([], sysb, F),
            [eop(A, saved=TT(cacheWrite5m=9000)), eop(B, saved=TT(cacheWrite5m=9000)),
             eop(D, saved=TT(cacheWrite5m=4000))], "unknown",
            add=[rewrite("o1", "context-guru.mask", "messages", 1, 10000, 1000, A),
                 rewrite("o2", "context-guru.mask", "messages", 2, 10000, 1000, B),
                 rewrite("o3", "context-guru.cmdfilter", "messages", 3, 10000, 6000, D)])
    write_issue("conservation/client-size-from-result", 555,
                "Three operators remove 22,000 of the 38,000 tokens the client sent, so the provider bills 16,000. The "
                "fill a size gate wants (38,000) is not billed, but it follows from the result with no new field: billed "
                "+ sum of Saved - sum of Caused = 16,000 + 22,000 - 0. opscases_test.go pins that identity for every case.",
                DIMS("rewrite", "afterBStar", "miss", operations="3"), [s],
                open_=["new: whether Result should expose the client-side token total (issue 555) or leave the sum to the "
                       "trigger. The identity holds either way"])


# Case 12 (#175): one session's state does not reach another.
def c12():
    sysb = [("sys", 2000)]
    CH = "cg:extract:1a40"
    C0 = [("m0", 3000), ("m1", 8000)]
    F0 = [("m0", 3000), ("m1f", 500)]
    op = rewrite("o1", "context-guru.extract", "messages", 1, 8000, 500, CH)
    a0 = sim(0, "ra0", ([], sysb, C0), ([], sysb, F0), [eop(CH, saved=TT(cacheWrite5m=7500))], "unknown", add=[op],
             session="s-a")
    C1, F1 = C0 + [("m2", 1000)], F0 + [("m2", 1000)]
    a1 = sim(40, "ra1", ([], sysb, C1), ([], sysb, F1), [eop(CH, saved=TT(cacheRead=7500))], "hot",
             prev=(13000, 5500), boundary=P("messages", 1), session="s-a")
    b0 = sim(50, "rb0", ([], sysb, C0), ([], sysb, F0), [eop(CH, saved=TT(cacheWrite5m=7500))], "unknown", add=[op],
             session="s-b")
    write_issue("state/session-isolation", 175,
                "Session A and session B hold the same 8,000-token output and the operator derives the same ChangeID "
                "for it. A's change is active in A's Tracker. B's first request reports the change as new and must be "
                "priced alone: one op, a write-tier saving, no unique_change failure. ThreadKey includes the session, so "
                "nothing of A's state reaches B.",
                DIMS("rewrite", "afterBStar", "miss", sessions="2"), [a0, a1, b0],
                open_=[Q12 + " (issue 175: the change ends in A when the agent expands the output; this case keeps A's "
                       "change active, so it tests only that an active change in A does not reach B)"])


# Case 13 (#501): two tenants send the same session id.
def c13():
    sysb = [("sys", 2000)]
    A0, B0 = [("m0", 8000)], [("m0b", 20000)]
    A1 = A0 + [("m1", 1000)]
    a0 = sim(0, "ra0", ([], sysb, A0), ([], sysb, A0), [], "unknown", session="s-shared", params={"tenant": "A"})
    b0 = sim(10, "rb0", ([], sysb, B0), ([], sysb, B0), [], "unknown", session="s-shared", params={"tenant": "B"})
    a1 = sim(40, "ra1", ([], sysb, A1), ([], sysb, A1), [], "hot", prev=(10000, 10000), boundary=P("messages", 0),
             session="s-shared", params={"tenant": "A"})
    write_issue("state/tenant-collision", 501,
                "Tenant A and tenant B send requests under one session id and thread. A's second request must be priced "
                "against A's first: hot, a read of 10,000 tokens, no break. Under the current key (session + thread) B's "
                "request replaces A's ThreadState, so A's second request would be compared with B's 22,000-token body "
                "and show a break that is nobody's.",
                DIMS("none", "none", "hit", tenants="2"), [a0, b0, a1], open_=[NOTENANT])


# Cases 14 (#45) and 15 (#567): breakpoint operators that report success.
SPL = "cg:cachesplit:e4d1"


def c14():
    msgs0 = [("m0", 5000)]
    msgs1 = msgs0 + [("m1", 1000)]
    op = rewrite("o1", "context-guru.cachesplit", "system", 0, 3300, 3300, SPL)
    for cid, delivered in (("split/reported-not-delivered", False), ("split/delivered/stable-half-read", True)):
        fb_idx = 0 if delivered else 1
        s0 = sim(0, "r0", ([], [("sysblk", 3300)], msgs0), ([], [("sysstable", 3000), ("sysvol", 300)], msgs0), [],
                 "unknown", add=[op], cbp=BP(("system", 0), ("messages", 0)), fbp=BP(("system", fb_idx), ("messages", 0)))
        brk = effect("break", "", to=P("system", 0), note="the client changed its own system block: B* is before it, "
                     "so the break moves no tokens (§5.1, §6.2)")
        if delivered:
            usage = U(read=3000, write=6300)
            effects = [effect("split", SPL, saved=TT(cacheWrite5m=3000), caused=TT(cacheRead=3000), to=P("system", 0),
                              note="the stable half is read, not written"), brk]
            ops = []
        else:
            usage = U(write=9300)
            effects = [brk]
            ops = []
        s1 = sim(40, "r1", ([], [("sysblk2", 3300)], msgs1), ([], [("sysstable", 3000), ("sysvol2", 300)], msgs1), ops,
                 "hot", prev=(8300, 8300), usage=usage, base=U(write=9300),
                 cbp=BP(("system", 0), ("messages", 1)), fbp=BP(("system", fb_idx), ("messages", 1)),
                 effects=effects)
        if delivered:
            desc = ("On Anthropic the breakpoint follows the stable half: cachesplit moves it there. The client changes "
                    "the volatile 300 tokens, so the client body would write all 9,300 tokens, and the forwarded body "
                    "reads the stable 3,000 and writes 6,300. A split effect credits 3,000 x ($3.75 - $0.30) = +$0.01035. "
                    "The contrast case of reported-not-delivered.")
        else:
            desc = ("On Bedrock Converse the cachePoint is its own entry after the block, and the split put the volatile "
                    "half before it, so the breakpoint still covers the churn. cachesplit reported a rewrite of equal "
                    "size and shows as active. The volatile text changes: the forwarded body reads nothing, like the "
                    "client body. The library prices from usage and breakpoints: no split effect, no saving, and the "
                    "operator is credited nothing.")
        write_issue(cid, 45, desc, DIMS("setting-breakpoint", "system", "split" if delivered else "no-effect"),
                    [s0, s1],
                    open_=["design §5.2 rows 1 and 2: the client changed its own system block, and the split moved that "
                           "change one block later in forwarded space. Which row names the break (and so whether the "
                           "empty-ChangeID break is added beside the split effect) is not stated"])


def c15():
    sysb = [("s0", 200), ("s1", 800), ("s2", 1500)]
    m = [("m0", 5000), ("m1", 2000)]
    bp = lambda last: BP(("system", 1), ("system", 2), ("messages", last))
    s0 = sim(0, "r0", ([], sysb, m), ([], sysb, m), [], "unknown", cbp=bp(1), fbp=bp(1))
    m2 = m + [("m2", 1000)]
    s1 = sim(40, "r1", ([], sysb, m2), ([], sysb, m2), [], "hot", prev=(9500, 9500), cbp=bp(2), fbp=bp(2),
             boundary=P("messages", 1))
    write_issue("split/marker-not-in-system", 567,
                "On Claude Code 2.1.299 the git snapshot sits in the first user message, after both system breakpoints, "
                "and does not change during the session. cachesplit finds no marker and reports no operation. The thread "
                "reads the whole prefix on a hot turn: no operation, no effect, a zero total.",
                DIMS("none", "none", "hit"), [s0, s1])
    bp2 = lambda last: BP(("system", 2), ("messages", 0), ("messages", last))
    s0 = sim(0, "r0", ([], sysb, m), ([], sysb, m), [], "unknown", cbp=bp2(1), fbp=bp2(1))
    m3 = [("m0b", 5000), ("m1", 2000), ("m2", 1000)]
    s1 = sim(40, "r1", ([], sysb, m3), ([], sysb, m3), [], "hot", prev=(9500, 9500), cbp=bp2(2), fbp=bp2(2),
             usage=U(read=2500, write=8000), base=U(read=2500, write=8000), boundary=P("system", 2),
             effects=[effect("break", "", to=P("system", 2),
                             note="a volatile block inside the span a messages[0] breakpoint hashes: B* is the "
                                  "system breakpoint before it, so the break moves no tokens")])
    write_issue("split/volatile-in-first-message", 567,
                "The variant issue 567 could not measure: the volatile snapshot changes inside message 0, which a "
                "messages[0] breakpoint hashes. The provider reads through system (2,500 tokens) and writes the other "
                "8,000. The client made the change, so the break has an empty ChangeID and, with B* at the system "
                "breakpoint, no priced tokens. The 8,000 written tokens are the room an operator that moved that "
                "breakpoint could win back, but they are not a cost of any operation.",
                DIMS("none", "messages", "break", **{"break": "client"}), [s0, s1], open_=[BSTAR_SNAP])


# Case 16 (#481): a restore adds the original back.
MK16, RS = "cg:mask:8d11", "cg:restore:2c9e"


def c16():
    sysb = [("sys", 2000)]
    base_c = [("m0", 3000), ("m1", 5000)]
    base_f = [("m0", 3000), ("m1f", 500)]
    mask = rewrite("o1", "context-guru.mask", "messages", 1, 5000, 500, MK16)
    s0 = sim(0, "r0", ([], sysb, base_c), ([], sysb, base_f), [eop(MK16, saved=TT(cacheWrite5m=4500))], "unknown",
             add=[mask])
    c1, f1 = base_c + [("m2", 1000)], base_f + [("m2", 1000)]
    s1 = sim(40, "r1", ([], sysb, c1), ([], sysb, f1), [eop(MK16, saved=TT(cacheRead=4500))], "hot", prev=(10000, 5500),
             boundary=P("messages", 1))
    c2, f2 = c1 + [("m3", 500)], f1 + [("m3", 500), ("r1", 5000)]
    restore = rewrite("o2", "context-guru.expand_restore", "messages", 4, 0, 5000, RS,
                      splice={"at": 4, "removed": 0, "inserted": 1})
    s2 = sim(80, "r2", ([], sysb, c2), ([], sysb, f2),
             [eop(RS, caused=TT(cacheWrite5m=5000), note="the restored copy is new content: written once"),
              eop(MK16, saved=TT(cacheRead=4500))], "hot", add=[restore], prev=(11000, 6500), boundary=P("messages", 2))
    steps = [s0, s1, s2]
    pc, pf = tokens(sysb, c2), tokens(sysb, f2)
    c, f = c2, f2
    for k, t in ((3, 120), (4, 160)):
        c, f = c + [(f"n{k}", 500)], f + [(f"n{k}", 500)]
        steps.append(sim(t, f"r{k}", ([], sysb, c), ([], sysb, f),
                         [eop(MK16, saved=TT(cacheRead=4500)), eop(RS, caused=TT(cacheRead=5000))], "hot",
                         prev=(pc, pf), boundary=P("messages", len(c) - 2)))
        pc, pf = tokens(sysb, c), tokens(sysb, f)
    write_issue("rewrite/fixed-restore-cost/hot/5m", 481,
                "mask compacts a 5,000-token output to 500 (saves 4,500). On request 2 the model expands it, and "
                "fixed_restore appends the original as a new 5,000-token message while the marker stays. The restore "
                "grows the body, so it is a cost by its position: written once on request 2 (5,000 x $3.75 = -$0.01875) "
                "and read on every later hot request (-$0.00150), against the mask's +$0.00135. Each later request nets "
                "-$0.00015: the compaction's credit is never shown without the restore beside it.",
                DIMS("rewrite", "afterBStar", "hit", operations="2", restore="fixed"), steps,
                open_=[BSTAR_SNAP, "design §6.2 does not say how B* maps onto content inserted after it. The restored message "
                       "is forwarded messages[4], past client B* (messages[3]), so a literal reading prices it as a write; "
                       "this case prices it as a read on the later turns because only that conserves"])

    bpx = lambda last: BP(("system", 0), ("messages", 0), ("messages", last))
    s0 = sim(0, "r0", ([], sysb, base_c), ([], sysb, base_f), [eop(MK16, saved=TT(cacheWrite5m=4500))], "unknown",
             add=[mask], cbp=bpx(1), fbp=bpx(1))
    s1 = sim(40, "r1", ([], sysb, c1), ([], sysb, f1), [eop(MK16, saved=TT(cacheRead=4500))], "hot", prev=(10000, 5500),
             cbp=bpx(2), fbp=bpx(2), boundary=P("messages", 1))
    s2 = sim(80, "r2", ([], sysb, c2), ([], sysb, c2), [], "hot", prev=(11000, 6500), usage=U(read=5000, write=6500),
             cbp=bpx(3), fbp=bpx(3), boundary=P("messages", 2), checks=["attribution"],
             effects=[effect("break", MK16, saved=TT(cacheRead=6000), caused=TT(cacheWrite5m=6000),
                             frm=P("messages", 0), to=P("messages", 2),
                             note="the original goes back in place: the mask ended and the provider holds the marker")])
    write_issue("rewrite/restore-in-place/hot/5m", 481,
                "The variant with fixed_restore off: the expanded original goes back in place. The mask no longer "
                "applies, so it saves nothing, but the provider holds the marker form from the earlier requests, so "
                "6,000 tokens move from read to write: -$0.02070, charged to the ended change. The old figure kept "
                "crediting the mask here.",
                DIMS("rewrite", "beforeBStar", "hit-break", **{"break": "plugin", "restore": "in-place"}), [s0, s1, s2],
                open_=[Q12 + " (request 2: the mask ended and the break is charged to it)"])


# Case 17 (#584): the client shrinks its own body.
def c17():
    msgs = [("m0", 5000), ("m1", 5000)]
    s0 = sim(0, "r0", (TOOLS_C, SYS3, msgs), (TOOLS_C, SYS3, msgs), [], "unknown")
    m2 = msgs + [("m2", 2000)]
    s1 = sim(40, "r1", (TOOLS_F, SYS3, m2), (TOOLS_F, SYS3, m2), [], "hot", prev=(33000, 33000),
             usage=U(write=27000), base=U(write=27000),
             effects=[effect("break", "", to=P("tools", 0),
                             note="the client dropped tool declarations by itself: no client breakpoint precedes the "
                                  "change, so B* is 0 and the break moves no tokens")])
    write_issue("client-edit/shrink-no-operator", 584,
                "The client drops three tool declarations (8,000 tokens) by itself, with no context operator. The cache "
                "breaks from tools[2], so the provider reads nothing and writes all 27,000 tokens. Nothing a context "
                "operator did is involved: no operation, no operator gets a saving, and the break has an empty ChangeID. "
                "What the client removes is not counted as saved.",
                DIMS("none", "tools", "break", **{"break": "client"}), [s0, s1],
                open_=[BSTAR_SNAP, "Issue 584 option 2 (credit a removal our own "
                       "recommendation caused) needs a reporter for that and is not covered"])


# Case 18 (#696): two operations whose bytes the library converts to tokens.
def c18():
    msgs_c = [("m0", 4000), ("m1", 3000)]
    msgs_f = [("m0", 4000), ("m1f", 500)]
    MK18 = "cg:mask:be02"
    s = sim(0, "r0", (TOOLS_C, SYS3, msgs_c), (TOOLS_F, SYS3, msgs_f),
            [eop(TP, saved=TT(cacheWrite5m=8000)), eop(MK18, saved=TT(cacheWrite5m=2500))], "unknown",
            add=[prune_op(), rewrite("o2", "context-guru.mask", "messages", 1, 3000, 500, MK18)])
    write_issue("estimate/bytes-ratio-json-removal", 696,
                "tool-prune and mask report bytes only. The library converts them with the request ratio (4 bytes a "
                "token here), so both operations are marked Estimated. The library does not correct the conversion: "
                "the host passes the billed total and the conservation check compares it. Issue 696 (a per-family "
                "factor that over-credits a removal) is answered by the request ratio; this case pins the marking.",
                DIMS("rewrite", "mixed", "miss", operations="2", estimate="bytes-ratio"), [s],
                open_=["not tested: a removal denser in tokens than the request average (the other half of issue 696). "
                       "Sizes here are exactly 4 bytes per token, so the conservation check has nothing to report",
                       "design §11 question 3: whether Size should take a content class, or an optional Tokens field"])


# ---------------------------------------------------------------------------------------------
# Cases from docs/counterfactual/more_examples.md. The scenario ids (A1, B3, ...) are that document's.
# The document was drafted before any of them was priced; the ones that do not hold up are listed as
# skipped in its section 6, with the reason.
def write_more(cid, sid, description, dims, steps, open_=None):
    write(case(cid, description, f"more_examples.md#{sid}", dims, steps, open_=open_),
          cid.replace("/", "-").replace("+", "-"))


PING_USAGE = {"input": 10, "cacheRead": 10000, "cacheWrite": 0, "cacheWrite1h": 0, "output": 1}


def ping_call(**kw):
    u = dict(PING_USAGE)
    u.update(kw)
    return {"host": "api.anthropic.com", "model": "agent-model", "usage": u}


SYS10 = [("sys", 2000)]
MSG10 = [("m0", 3000)] + [(f"m{i}", 1000) for i in range(1, 6)]  # 8000 tokens, 10000 with the system block


# A1: the client edits a message inside the prefix and an operator shortens a new one.
def a1():
    CF1 = "cg:cmdfilter:a1c4"
    sysb = SYS10
    M = [(f"m{i}", 3000) for i in range(9)]
    bp = lambda last: BP(("system", 0), ("messages", 2), ("messages", last))
    s0 = sim(0, "r0", ([], sysb, M), ([], sysb, M), [], "unknown", cbp=bp(8), fbp=bp(8))
    edit = lambda m: [x if x[0] != "m3" else ("m3e", 3000) for x in m]
    s1 = sim(40, "r1", ([], sysb, edit(M) + [("m9", 8000)]), ([], sysb, edit(M) + [("m9f", 1000)]),
             [eop(CF1, saved=TT(cacheWrite5m=7000))], "hot",
             add=[rewrite("o1", "context-guru.cmdfilter", "messages", 9, 8000, 1000, CF1)],
             cbp=bp(9), fbp=bp(9), usage=U(read=11000, write=19000), base=U(read=11000, write=26000),
             boundary=P("messages", 2),
             effects=[effect("break", "", frm=P("messages", 2), to=P("messages", 2),
                             note="the client edited message 3: its last breakpoint before the edit is message 2, "
                                  "so B* is already where the provider stopped reading and no token moves")])
    write_more("break/client-edit-and-operator/hot/5m", "A1",
               "The client edits message 3, inside the cached prefix, and cmdfilter shortens a new 8,000-token output "
               "(message 9) to 1,000 tokens. The client's last breakpoint before the edit is the end of message 2, so "
               "B* is the end of message 2: the provider reads 11,000 tokens and writes the rest, in the client's bill "
               "as well as in the forwarded one. The edit is on the record as a break with no ChangeID and moves no "
               "token, because the client body would have stopped reading at the same place. cmdfilter's 7,000 tokens "
               "are after B*, so they are writes: +7,000 x $3.75 = +$0.02625. The edit does not reduce or raise that figure.",
               DIMS("rewrite", "afterBStarToL", "break", **{"break": "client"}), [s0, s1],
               open_=["design §5.2 row 2 and §6.2: B* snaps to the breakpoint before the edit, which is also where the "
                      "provider stopped reading, so the break moves no tokens. The case expects the effect with an empty "
                      "token count. If the library should add no effect at all when nothing moves, drop it here"])


# A2: equal bodies, a hot clock, and a read that is too low.
def a2():
    m = [(f"m{i}", 3000) for i in range(6)]
    s0 = sim(0, "r0", ([], SYS10, m), ([], SYS10, m), [], "unknown")
    m2 = m + [("m6", 1000)]
    s1 = sim(40, "r1", ([], SYS10, m2), ([], SYS10, m2), [], "hot", prev=(20000, 20000),
             usage=U(read=2000, write=19000), base=U(read=2000, write=19000), boundary=P("messages", 5),
             effects=[effect("break", "", saved=TT(cacheRead=18000), caused=TT(cacheWrite5m=18000),
                             frm=P("system", 0), to=P("messages", 5),
                             note="the entry was lost early; nobody caused it")])
    write_more("break/entry-lost-early/hot/5m", "A2",
               "Two requests 40 seconds apart with equal prefixes. The clock says hot, but the provider read only the "
               "2,000 tokens of the system block and wrote the other 19,000 (the lookback window, an eviction, or "
               "another reason the library cannot see). The break has no ChangeID (§5.2 row 4). It is in the client "
               "body's bill too, so the baseline equals the bill and the total is zero.",
               DIMS("none", "beforeBStar", "break", **{"break": "unknown"}), [s0, s1],
               open_=["design R51 and §6.3 conservation: a break nobody caused is in the baseline bill too, so it is on "
                      "the record and out of the sum (see break/client-edit-inside-prefix/hot/5m)", BSTAR_SNAP])


# A3: a cold request with equal bodies.
def a3():
    m = [(f"m{i}", 3000) for i in range(6)]
    s0 = sim(0, "r0", ([], SYS10, m), ([], SYS10, m), [], "unknown")
    s1 = sim(1500, "r1", ([], SYS10, m + [("m6", 1000)]), ([], SYS10, m + [("m6", 1000)]), [], "cold",
             effects=[effect("break", "", note="the entry expired; B* is 0, so the read extent to B* is empty")])
    write_more("break/expiry-moves-no-tokens/cold/5m", "A3",
               "A request 25 minutes after the last, with an equal prefix. The 5m entry expired, so the provider "
               "reads nothing and writes all 21,000 tokens, in both bills. The expiry is on the record as a break with "
               "no ChangeID (§5.2 row 5), but B* is 0 and every position is already a write, so it moves no token and "
               "the total is zero.",
               DIMS("none", "none", "miss", **{"break": "expiry"}), [s0, s1])


# A4 and B3: OpenAI GPT-5.6, a lifetime that is a minimum.
def a4_b3():
    MK4 = "cg:mask:4a90"
    facts = {"ttl": "30m", "lifetime": "minimum"}
    Cm = [("m0", 3000), ("m1", 6000), ("m2", 3000), ("m3", 3000)]
    Fm = [("m0", 3000), ("m1f", 60), ("m2", 3000), ("m3", 3000)]
    op = rewrite("o1", "context-guru.mask", "messages", 1, 6000, 60, MK4)
    for cid, sid, ping in (("keepalive/no-ping/minimum-lifetime/evidence-hot", "A4", False),
                           ("keepalive/ping/minimum-lifetime/no-rescue", "B3", True)):
        ses = "s-oa-ping" if ping else "s-oa"
        s0 = sim(0, "r0", ([], SYS10, Cm), ([], SYS10, Fm), [eop(MK4, saved=TT(cacheWrite5m=5940))], "unknown",
                 add=[op], facts=facts, session=ses, model="gpt-5.6")
        steps = [s0]
        f1 = dict(facts)
        at = 2400
        if ping:
            KAW = "ka:main:w-oa"
            steps.append(ping_step(1800, ses, KAW, ping_call(cacheRead=11060)))
            f1.update(lastPing=ts(1800), pingRead=P("messages", 3), pingWindow=KAW)
            at = 2100
        steps.append(sim(at, "r1", ([], SYS10, Cm + [("m4", 500)]), ([], SYS10, Fm + [("m4", 500)]),
                         [eop(MK4, saved=TT(cacheRead=5940), note="the read proves the entry, so the saving is a read")],
                         "hot", prev=(17000, 11060), boundary=P("messages", 3), facts=f1, session=ses, model="gpt-5.6"))
        if ping:
            desc = ("OpenAI GPT-5.6: the entry lifetime is a 30-minute minimum, not a proven expiry (R45). A ping at "
                    "minute 30 reads the 11,060 tokens, and the next request arrives at minute 35 and reads them too. "
                    "A gap past a minimum proves nothing, so the phase is unknown until the read in the usage proves "
                    "the entry: it becomes hot. A rescue claims the entry would have expired, which a minimum cannot "
                    "show, so none is credited and the window is only the ping's cost, 11,060 x $0.30 + 10 x $3.00 + "
                    "$15.00 per million = -$0.00336. mask's 5,940 tokens are read, not written: +$0.00178.")
        else:
            desc = ("OpenAI GPT-5.6, 40 minutes after the last request, past the 30-minute minimum. The gap proves "
                    "nothing, so the phase starts unknown. The provider read 11,060 tokens, which is evidence the entry "
                    "was alive, so the phase becomes hot and mask's replayed 5,940 tokens are priced as a read "
                    "(+$0.00178), not a write (+$0.02228). No ping was sent, so there is no rescue either way.")
        write_more(cid, sid, desc, DIMS("rewrite" if not ping else "rescue", "beforeBStar", "evidence",
                                         lifetime="minimum", keepalive=str(ping).lower()), steps,
                   open_=[BSTAR_SNAP])


# B1 and B2: a ping that does not earn a rescue.
def b1_b2():
    for cid, sid, ping_at, req_at, phase, desc in (
            ("keepalive/ping-inside-ttl/no-rescue/5m", "B1", 280, 290, "hot",
             "Turn 1 caches 10,000 tokens. The keeper pings at 280 seconds, and the next request arrives 10 seconds "
             "later, 290 seconds after turn 1: inside the 5m TTL. Without the ping the entry would have been alive, so "
             "no rescue is credited (§4.4 condition 2). The window holds only the ping, 10,000 x $0.30 + 10 x $3.00 + "
             "1 x $15.00 per million = -$0.00305: a ping that was not needed."),
            ("keepalive/ping-after-entry-expired/no-rescue/5m", "B2", 320, 700, "cold",
             "Turn 1 caches 10,000 tokens. The keeper pings at 320 seconds, after the entry expired at 300: the ping "
             "reads nothing and writes the 10,000 tokens again (10,000 x $3.75 + 10 x $3.00 + 1 x $15.00 per million = "
             "-$0.03755). The next request arrives at 700 seconds, 380 after the ping: past the TTL again. The entry "
             "gap is above the TTL, so the phase is cold, no rescue is credited, and the window is only the ping's cost. "
             "The expiry is on the record as a break that moves no token.")):
        KAW = f"ka:main:w-{sid.lower()}"
        ses = f"s-{sid.lower()}"
        wrote = phase == "cold"
        call = ping_call(cacheRead=0, cacheWrite=10000) if wrote else ping_call()
        s0 = sim(0, "r0", ([], SYS10, MSG10), ([], SYS10, MSG10), [], "unknown", session=ses)
        m2 = MSG10 + [("m6", 500)]
        facts = {"ttl": "5m", "lifetime": "exact", "lastPing": ts(ping_at), "pingRead": P("messages", 5), "pingWindow": KAW}
        s1 = sim(req_at, "r1", ([], SYS10, m2), ([], SYS10, m2), [], phase, prev=(10000, 10000), facts=facts, session=ses,
                 effects=[effect("break", "", note="the entry expired; moves no token")] if wrote else None)
        write_more(cid, sid, desc, DIMS("rescue", "beforeBStar", "no-rescue", keepalive="true"),
                   [s0, ping_step(ping_at, ses, KAW, call), s1])


# B4: a 1h entry is alive after 9 minutes.
def b4():
    KAW = "ka:main:w-b4"
    ses = "s-b4"
    m2 = MSG10 + [("m6", 500)]
    facts = {"ttl": "1h", "lifetime": "exact", "lastPing": ts(300), "pingRead": P("messages", 5), "pingWindow": KAW}
    s0 = sim(0, "r0", ([], SYS10, MSG10), ([], SYS10, MSG10), [], "unknown", session=ses, cbp=bps(MSG10, "1h"),
             fbp=bps(MSG10, "1h"), usage=U(write=10000, w1h=10000), base=U(write=10000, w1h=10000),
             facts={"ttl": "1h", "lifetime": "exact"})
    s1 = sim(540, "r1", ([], SYS10, m2), ([], SYS10, m2), [], "hot", prev=(10000, 10000), session=ses,
             cbp=bps(m2, "1h"), fbp=bps(m2, "1h"), usage=U(read=10000, write=500, w1h=500),
             base=U(read=10000, write=500, w1h=500), facts=facts)
    write_more("keepalive/ping-on-1h-entry/no-rescue/1h", "B4",
               "The whole prompt is cached at 1h. The keeper pings at 5 minutes and the next request arrives at 9. The "
               "entry lives 60 minutes, so without the ping it would have been alive: no rescue (§4.4 condition 2 uses the "
               "TTL of the breakpoint that wrote the entry, not 5m). The window is only the ping, -$0.00305.",
               DIMS("rescue", "beforeBStar", "no-rescue", ttl="1h", keepalive="true"),
               [s0, ping_step(300, ses, KAW, ping_call()), s1])


# B5: two pings in one window, then one rescue.
def b5():
    KAW = "ka:main:w-b5"
    ses = "s-b5"
    m2 = MSG10 + [("m6", 500)]
    facts = {"ttl": "5m", "lifetime": "exact", "lastPing": ts(560), "pingRead": P("messages", 5), "pingWindow": KAW}
    s0 = sim(0, "r0", ([], SYS10, MSG10), ([], SYS10, MSG10), [], "unknown", session=ses)
    s1 = sim(840, "r1", ([], SYS10, m2), ([], SYS10, m2), [], "hot", prev=(10000, 10000), session=ses, facts=facts,
             usage=U(read=10000, write=500), base=U(write=10500),
             effects=[effect("rescue", KAW, saved=TT(cacheWrite5m=10000), caused=TT(cacheRead=10000),
                             to=P("messages", 5), note="the smaller of what the last ping read and what the request read")])
    write_more("keepalive/two-pings/one-rescue/5m", "B5",
               "Turn 1 caches 10,000 tokens. The keeper pings at 280 and 560 seconds, then the request arrives at 840: "
               "past the TTL from turn 1, 280 seconds after the last ping. One rescue is credited, for the smaller of what "
               "the last ping read and what the request read, 10,000 tokens: 10,000 x ($3.75 - $0.30) = +$0.03450. Both "
               "pings cost (2 x $0.00305), so the window nets +$0.02841.",
               DIMS("rescue", "beforeBStar", "rescue", keepalive="true", pings="2"),
               [s0, ping_step(280, ses, KAW, ping_call()), ping_step(560, ses, KAW, ping_call()), s1],
               open_=[BSTAR_SNAP])


# C1: a rewrite after the last breakpoint is input.
def c1_():
    CF = "cg:cmdfilter:c1d0"
    M = [(f"m{i}", 3000) for i in range(9)]
    bpf = BP(("system", 0), ("messages", 8))
    s0 = sim(0, "r0", ([], SYS10, M), ([], SYS10, M), [], "unknown", cbp=bpf, fbp=bpf)
    s1 = sim(40, "r1", ([], SYS10, M + [("m9", 8000)]), ([], SYS10, M + [("m9f", 1000)]),
             [eop(CF, saved=TT(input=7000))], "hot", prev=(29000, 29000), cbp=bpf, fbp=bpf,
             add=[rewrite("o1", "context-guru.cmdfilter", "messages", 9, 8000, 1000, CF)],
             usage=U(read=29000, inp=1000), base=U(read=29000, inp=8000), boundary=P("messages", 8))
    write_more("rewrite/after-last-breakpoint/input/5m", "C1",
               "The client keeps its breakpoints at the end of system and at message 8. A new 8,000-token output "
               "(message 9) sits after the last breakpoint, so the provider bills it as input, not as a cache write. "
               "cmdfilter shortens it to 1,000 tokens and saves 7,000 x $3.00 = +$0.02100, not the $0.02625 of a write.",
               DIMS("rewrite", "afterL", "hit"), [s0, s1], open_=[BSTAR_SNAP])


# C2: no breakpoint anywhere.
def c2_():
    CF = "cg:cmdfilter:c2e7"
    M = [(f"m{i}", 3000) for i in range(9)]
    s0 = sim(0, "r0", ([], SYS10, M), ([], SYS10, M), [], "unknown", cbp=[], fbp=[], usage=U(inp=29000), base=U(inp=29000))
    s1 = sim(40, "r1", ([], SYS10, M + [("m9", 8000)]), ([], SYS10, M + [("m9f", 1000)]),
             [eop(CF, saved=TT(input=7000))], "hot", prev=(29000, 29000), cbp=[], fbp=[],
             add=[rewrite("o1", "context-guru.cmdfilter", "messages", 9, 8000, 1000, CF)],
             usage=U(inp=30000), base=U(inp=37000))
    write_more("rewrite/no-breakpoint/input/5m", "C2",
               "The body carries no cache_control, so the provider caches nothing and bills every token as input. "
               "cmdfilter shortens a new 8,000-token output to 1,000 tokens and saves 7,000 x $3.00 = +$0.02100. B* is "
               "0 because no client breakpoint is at or before the extent.",
               DIMS("rewrite", "afterL", "none", breakpoints="none"), [s0, s1])


# C5: mask at depth on a cold turn, then warm replays.
def c5():
    MK = "cg:mask:c5a2"
    C = [("m0", 3000), ("m1", 6000)] + [(f"m{i}", 3000) for i in range(2, 6)]
    F = [("m0", 3000), ("m1f", 60)] + [(f"m{i}", 3000) for i in range(2, 6)]
    s0 = sim(0, "r0", ([], SYS10, C), ([], SYS10, C), [], "unknown")
    s1 = sim(1500, "r1", ([], SYS10, C + [("m6", 500)]), ([], SYS10, F + [("m6", 500)]),
             [eop(MK, saved=TT(cacheWrite5m=5940), note="deep history on a cold turn: nothing is cached to break")],
             "cold", add=[rewrite("o1", "context-guru.mask", "messages", 1, 6000, 60, MK)])
    s2 = sim(1560, "r2", ([], SYS10, C + [("m6", 500), ("m7", 500)]), ([], SYS10, F + [("m6", 500), ("m7", 500)]),
             [eop(MK, saved=TT(cacheRead=5940), note="applied again; the client's prefix would be cached")],
             "hot", prev=(23500, 17560), boundary=P("messages", 6))
    write_more("rewrite/deep-on-cold/then-hot-replay/5m", "C5",
               "The entry expired, so on turn 2 (25 minutes later) mask removes 5,940 tokens from message 1, deep in "
               "history. Nothing is cached, so no break is possible and the saving is a write, +5,940 x $3.75 = +$0.02228. "
               "Turn 3, 60 seconds later, replays the change: the forwarded prefix is the one turn 2 cached, so there is "
               "no break, and the same tokens are now a read, +5,940 x $0.30 = +$0.00178. The total is +$0.02406.",
               DIMS("rewrite", "beforeBStar", "miss-hit", replay="true"), [s0, s1, s2], open_=[BSTAR_SNAP])


# D2: the expand tool is injected for the first time, on a warm request.
EXPD = "cg:expand:d2f1"


def d2():
    tools = [("tools", 5000)]
    ft = tools + [("expand", 300)]
    m = [("m0", 4000), ("m1", 4000), ("m2", 4000)]
    s0 = sim(0, "r0", (tools, SYS10, m), (tools, SYS10, m), [], "unknown")
    m1 = m + [("m3", 1000)]
    s1 = sim(60, "r1", (tools, SYS10, m1), (ft, SYS10, m1), [eop(EXPD, caused=TT(cacheRead=300))], "hot",
             add=[rewrite("o1", "context-guru.expand", "tools", 1, 0, 300, EXPD)], prev=(19000, 19000),
             usage=U(write=20300), boundary=P("messages", 2),
             effects=[effect("break", EXPD, saved=TT(cacheRead=19300), caused=TT(cacheWrite5m=19300), to=P("messages", 2),
                             note="the declaration is appended to tools: the provider reads nothing")])
    m2 = m1 + [("m4", 500)]
    s2 = sim(120, "r2", (tools, SYS10, m2), (ft, SYS10, m2), [eop(EXPD, caused=TT(cacheRead=300))], "hot",
             prev=(20000, 20300), boundary=P("messages", 3))
    write_more("rewrite/expand-first-injection/break-once/5m", "D2",
               "On turn 2 the engine appends the expand tool (300 tokens) to tools for the first time. tools come first in "
               "the prompt and the body has no breakpoint on them, so the provider reads nothing: the 19,300 tokens move "
               "from read to write, 19,300 x ($3.75 - $0.30) = -$0.06659, and the declaration itself is a cost of 300 x "
               "$0.30 = -$0.00009. The tool is sticky, so turn 3 carries it again with no break and costs -$0.00009. The "
               "total is -$0.06677.",
               DIMS("rewrite", "tools", "break", **{"break": "plugin", "sticky": "true"}), [s0, s1, s2],
               open_=["the expand tool is reported here as a rewrite with SizeAfter above SizeBefore (§4.2 rule 3, "
                      "cortex-wiring.md). Whether the engine reports it at all is not checked; if it does not, the break "
                      "on turn 2 has no ChangeID", BSTAR_SNAP])


# D3: toolschema is turned on mid-session.
def d3():
    TS = "cg:toolschema:d3b7"
    tc = [("t0", 5000), ("t1", 3000), ("t2", 4000)]
    tf = [("t0", 5000), ("t1f", 1000), ("t2", 4000)]
    m = [(f"m{i}", 4000) for i in range(3)]
    s0 = sim(0, "r0", (tc, SYS10, m), (tc, SYS10, m), [], "unknown")
    m1 = m + [("m3", 1000)]
    s1 = sim(60, "r1", (tc, SYS10, m1), (tf, SYS10, m1), [eop(TS, saved=TT(cacheRead=2000))], "hot",
             add=[rewrite("o1", "context-guru.toolschema", "tools", 1, 3000, 1000, TS)], prev=(26000, 26000),
             usage=U(write=25000), boundary=P("messages", 2),
             effects=[effect("break", TS, saved=TT(cacheRead=24000), caused=TT(cacheWrite5m=24000), to=P("messages", 2),
                             note="a tool declaration changed: the provider reads nothing")])
    m2 = m1 + [("m4", 500)]
    s2 = sim(120, "r2", (tc, SYS10, m2), (tf, SYS10, m2), [eop(TS, saved=TT(cacheRead=2000))], "hot",
             prev=(27000, 25000), boundary=P("messages", 3))
    write_more("rewrite/toolschema-mid-session/break-then-saving/5m", "D3",
               "toolschema is turned on at turn 2 and trims one tool declaration from 3,000 to 1,000 tokens. The "
               "declaration is part of the cached tools, so the provider reads nothing: 24,000 forwarded tokens move from "
               "read to write, -$0.08280, against a saving of 2,000 x $0.30 = +$0.00060. Turn 3 replays the change with no "
               "break and saves +$0.00060 again, and every later turn does too. The net after two turns is -$0.08160.",
               DIMS("rewrite", "tools", "break-hit", **{"break": "plugin", "replay": "true"}), [s0, s1, s2],
               open_=[BSTAR_SNAP])


# D4: a summary on a cold turn.
SUD4 = "cg:summarize:d4c9"


def summary_bodies():
    tools, sysb = [("tools4", 4000)], [("sys4", 4000)]
    old = [("m0", 4000), ("m1", 3000), ("m2", 3000), ("m3", 1500), ("m4", 1500)]
    return tools, sysb, old


def d4():
    tools, sysb, old = summary_bodies()
    summary, tail = [("summary", 500)], old[3:]
    spend = {"id": "op1", "kind": "spend", "operator": "context-guru.summarize", "changeId": SUD4,
             "call": {"model": "summarizer-model", "params": {"effort": "low"},
                      "usage": {"input": 10000, "cacheRead": 0, "cacheWrite": 0, "cacheWrite1h": 0, "output": 500, "costUSD": 0.0125}}}
    rw = rewrite("op2", "context-guru.summarize", "messages", 0, 10000, 500, SUD4, splice={"at": 0, "removed": 3, "inserted": 1})
    s0 = sim(0, "r0", (tools, sysb, old), (tools, sysb, old), [], "unknown")
    s1 = sim(1500, "r1", (tools, sysb, old + [("n0", 500)]), (tools, sysb, summary + tail + [("n0", 500)]),
             [eop(SUD4, call=spend["call"]), eop(SUD4, saved=TT(cacheWrite5m=9500))], "cold", add=[spend, rw])
    s2 = sim(1560, "r2", (tools, sysb, old + [("n0", 500), ("n1", 500)]),
             (tools, sysb, summary + tail + [("n0", 500), ("n1", 500)]),
             [eop(SUD4, saved=TT(cacheRead=9500), note="applied again; the summary is cached now")],
             "hot", prev=(21500, 12000), boundary=P("messages", 5))
    write_more("rewrite+spend/summary/cold-no-break/5m", "D4",
               "The contrast of the summary case that breaks the cache. The same summary (messages 0 to 2, 10,000 tokens "
               "to 500) is made on a request 25 minutes after the last, when nothing is cached. There is no prefix to "
               "break, so no break is charged: the 9,500 tokens are writes, +$0.03563, against the call, -$0.01250. "
               "Turn 3 replays it as a read, +$0.00285. The total is +$0.02598.",
               DIMS("rewrite+spend", "beforeBStar", "miss-hit", ttl="5m", replay="true"), [s0, s1, s2], open_=[BSTAR_SNAP])


# D5: the sweep when the ask cannot read the cached prefix.
def d5():
    SW = "cg:extract_llm_sweep:9d44"
    base = [("m0", 3000), ("m1", 8000), ("m2", 1000), ("m3", 2000), ("m4", 1000), ("m5", 1000)]
    s0 = sim(0, "r0", ([], SYS10[:1] and [("sys9", 2000)], base), ([], [("sys9", 2000)], base), [], "unknown", session="s-d5")
    m6 = base + [("m6", 500)]
    s1 = sim(100, "r1", ([], [("sys9", 2000)], m6), ([], [("sys9", 2000)], m6), [], "hot", prev=(18000, 18000), session="s-d5")
    call = {"host": "api.anthropic.com", "model": "agent-model",
            "usage": {"input": 18900, "cacheRead": 0, "cacheWrite": 0, "cacheWrite1h": 0, "output": 200}}
    ask = {"version": 1, "requestId": "sw-ask-fb", "session": "s-d5", "thread": {"id": "main"}, "at": ts(380),
           "billing": {"mode": "api"}, "ops": [{"id": "op1", "kind": "spend", "operator": "context-guru.extract_llm_sweep",
                                                 "changeId": SW, "call": call}]}
    spans = [{"section": "messages", "from": 1, "to": 1, "sizeBefore": size(8000), "sizeAfter": size(100)},
             {"section": "messages", "from": 3, "to": 3, "sizeBefore": size(2000), "sizeAfter": size(50)}]
    rw = rewrite("op2", "context-guru.extract_llm_sweep", "messages", 1, 10000, 150, SW, spans=spans)
    c2 = m6 + [("m7", 500)]
    f2 = [("m0", 3000), ("m1f", 100), ("m2", 1000), ("m3f", 50), ("m4", 1000), ("m5", 1000), ("m6", 500), ("m7", 500)]
    s2 = sim(1800, "r2", ([], [("sys9", 2000)], c2), ([], [("sys9", 2000)], f2),
             [eop(SW, saved=TT(cacheWrite5m=9850), note="two spans, one ChangeID")], "cold", add=[rw], session="s-d5")
    write_more("rewrite+spend/extract-llm-sweep/fallback-ask-fresh-input/5m", "D5",
               "The sweep's ask could not read the cached prefix (after a restart the proxy holds no previous body), so it "
               "falls back to a call that pays the whole 18,500-token transcript plus the 400-token question as fresh input: "
               "18,900 x $3.00 + 200 x $15.00 per million = -$0.05970. The ask drops two outputs, and the next cold request "
               "removes 7,900 + 1,950 = 9,850 tokens as writes, +$0.03694, as two spans of one change. The sweep nets "
               "-$0.02276: with the prefix the same ask would have cost -$0.00945 (see "
               "rewrite+spend/extract-llm-sweep/ask-then-cold-apply/5m).",
               DIMS("rewrite+spend", "beforeBStar", "hit-ask-miss", keepalive="true", sweep="fallback", spans="true"),
               [s0, s1, {"standalone": ask, "usage": U(), "expected": {"ops": [eop(SW, call=call)]}}, s2])


# D6: cache_aware_summarizer reads the prefix the agent's model holds.
def d6():
    SUC = "cg:cache_aware_summarizer:3e55"
    tools, sysb, old = summary_bodies()
    summary, tail = [("summary", 500)], old[3:]
    s0 = sim(0, "r0", (tools, sysb, old), (tools, sysb, old), [], "unknown", session="s-d6")
    call = {"host": "api.anthropic.com", "model": "agent-model",
            "usage": {"input": 300, "cacheRead": 21000, "cacheWrite": 0, "cacheWrite1h": 0, "output": 500}}
    ask = {"version": 1, "requestId": "cas-1", "session": "s-d6", "thread": {"id": "main"}, "at": ts(280),
           "billing": {"mode": "api"}, "ops": [{"id": "op1", "kind": "spend", "operator": "context-guru.cache_aware_summarizer",
                                                 "changeId": SUC, "call": call}]}
    rw = rewrite("op2", "context-guru.cache_aware_summarizer", "messages", 0, 10000, 500, SUC,
                 splice={"at": 0, "removed": 3, "inserted": 1})
    s1 = sim(1800, "r1", (tools, sysb, old + [("n0", 500)]), (tools, sysb, summary + tail + [("n0", 500)]),
             [eop(SUC, saved=TT(cacheWrite5m=9500))], "cold", add=[rw], session="s-d6")
    write_more("rewrite+spend/cache-aware-summarizer/prefix-read/5m", "D6",
               "cache_aware_summarizer in pre_expiry mode makes its call in place of the keep-alive ping, on the agent's "
               "own model, reading the 21,000-token cached prefix: 300 input, 21,000 read, 500 output = -$0.01470. "
               "Summarize (the case summary/break-then-payback) pays 10,000 tokens of fresh input at a cheaper model's "
               "rates, -$0.01250. The summary is held until the cache is cold: 25 minutes later it replaces messages 0 to 2 "
               "and the 9,500 tokens are writes, +$0.03563. The ask and the removal share one ChangeID. Total +$0.02093.",
               DIMS("rewrite+spend", "beforeBStar", "hit-ask-miss", keepalive="true", summarizer="cache-aware"),
               [s0, {"standalone": ask, "usage": U(), "expected": {"ops": [eop(SUC, call=call)]}}, s1])


# E2: cachesplit, the volatile tail unchanged.
def e2():
    SPL2 = "cg:cachesplit:e2a8"
    msgs0 = [("m0", 5000)]
    op = rewrite("o1", "context-guru.cachesplit", "system", 0, 3300, 3300, SPL2)
    fsys = [("sysstable", 3000), ("sysvol", 300)]
    s0 = sim(0, "r0", ([], [("sysblk", 3300)], msgs0), ([], fsys, msgs0), [], "unknown", add=[op],
             cbp=BP(("system", 0), ("messages", 0)), fbp=BP(("system", 0), ("messages", 0)))
    m1 = msgs0 + [("m1", 1000)]
    s1 = sim(40, "r1", ([], [("sysblk", 3300)], m1), ([], fsys, m1), [], "hot", prev=(8300, 8300),
             cbp=BP(("system", 0), ("messages", 1)), fbp=BP(("system", 0), ("messages", 1)), boundary=P("messages", 0))
    write_more("split/tail-unchanged/no-effect", "E2",
               "cachesplit moved the breakpoint to the end of the stable half of the system block, but the volatile tail "
               "does not change between the two requests. The client body would be read in full, and so is the "
               "forwarded one: no split effect, no saving, a zero total. The split costs nothing when it is not needed.",
               DIMS("setting-breakpoint", "system", "no-effect"), [s0, s1])


# E3, E4, E5: headttl.
def head_steps(second):
    HT = "headttl:main:sys3a1c"
    head = [("sys", 3000)]
    m0 = [("m0", 5000)]
    cb = BP(("system", 0), ("messages", 0))
    fb = [{"pos": P("system", 0), "ttl": "1h"}, {"pos": P("messages", 0), "ttl": "5m"}]
    op = rewrite("o1", "context-guru.headttl", "system", 0, 3000, 3000, HT)
    s0 = sim(0, "r0", ([], head, m0), ([], head, m0), [], "unknown", add=[op], cbp=cb, fbp=fb,
             usage=U(write=8000, w1h=3000), base=U(write=8000), facts={"ttl": "1h", "lifetime": "exact"},
             effects=[effect("ttlPremium", HT, saved=TT(cacheWrite5m=3000), caused=TT(cacheWrite1h=3000), ttl="1h",
                             note="the head is written at 1h, whether or not anyone reads it")])
    steps = [s0]
    if second is not None:
        sec, rescue = second
        m1 = m0 + [("m1", 500)]
        cb1, fb1 = BP(("system", 0), ("messages", 1)), [{"pos": P("system", 0), "ttl": "1h"}, {"pos": P("messages", 1), "ttl": "5m"}]
        if rescue:
            kw = dict(usage=U(read=3000, write=5500), base=U(write=8500),
                      effects=[effect("ttlRescue", HT, saved=TT(cacheWrite5m=3000), caused=TT(cacheRead=3000), ttl="1h",
                                      note="the 5m entry would have expired; the 1h head is read")])
        else:
            kw = dict(usage=U(read=8000, write=500), base=U(read=8000, write=500))
        steps.append(sim(sec, "r1", ([], head, m1), ([], head, m1), [], "hot", prev=(8000, 8000), cbp=cb1, fbp=fb1,
                         facts={"ttl": "1h", "lifetime": "exact"}, **kw))
    return steps


def e345():
    write_more("ttl/head-1h/read-after-20-minutes", "E3",
               "headttl holds the 3,000-token head of the request for 1h. Writing it costs the premium over a 5m write, "
               "3,000 x ($6.00 - $3.75) = -$0.00675 (ttlPremium). Twenty minutes later the client's 5m entry would have "
               "expired, but the 1h head is read: 3,000 x ($3.75 - $0.30) = +$0.01035 (ttlRescue). The total is +$0.00360.",
               DIMS("setting-breakpoint", "system", "premium-rescue", ttl="1h"), head_steps((1200, True)))
    write_more("ttl/head-1h/never-read", "E4",
               "headttl writes the 3,000-token head at 1h and no request reads it again. Only the cost leg exists: "
               "3,000 x ($6.00 - $3.75) = -$0.00675. The bet's total is negative and visible, as a keep-alive window that "
               "rescued nothing is.",
               DIMS("setting-breakpoint", "system", "premium-only", ttl="1h"), head_steps(None))
    write_more("ttl/head-1h/read-after-3-minutes", "E5",
               "headttl writes the head at 1h and the next request reads it 3 minutes later. The 5m entry would have been "
               "alive too, so the 1h read is worth nothing extra: no ttlRescue. The premium is paid, -$0.00675, and "
               "nothing pays it back.",
               DIMS("setting-breakpoint", "system", "premium-no-rescue", ttl="1h"), head_steps((180, False)))


# F2, F4, F5: settings.
def setting_steps(change, settings, usage1, effects, unpriced=None, cbp=None, model="agent-model", params=None,
                  facts=None, params_from=None):
    tools, sysb = [("tools5", 5000)], [("sys5", 3000)]
    msgs = [(f"m{i}", 8000) for i in range(4)]
    bp0 = cbp(3) if cbp else None
    s0 = sim(0, "r0", (tools, sysb, msgs), (tools, sysb, msgs), [], "unknown", cbp=bp0, fbp=bp0, session="s-set",
             model=model, params=params_from, facts=facts)
    m5 = msgs + [("m4", 1000)]
    bp1 = cbp(4) if cbp else None
    setting = {"id": "op1", "kind": "setting", "operator": "inference-router", "changeId": change, "settings": settings}
    s1 = sim(60, "r1", (tools, sysb, m5), (tools, sysb, m5), [], "hot", add=[setting], prev=(40000, 40000), cbp=bp1, fbp=bp1,
             usage=usage1, boundary=P("messages", 3), effects=effects, unpriced=unpriced, session="s-set", model=model,
             params=params, facts=facts)
    return [s0, s1]


def f245():
    # F2: the web-search toggle sits at system[0]. A breakpoint on tools keeps the tools readable.
    cb = lambda last: BP(("tools", 0), ("system", 0), ("messages", last))
    steps = setting_steps("rt:websearch-1d2f", {"web_search": {"from": "off", "to": "on", "at": P("system", 0)}},
                          U(read=5000, write=36000),
                          [effect("break", "rt:websearch-1d2f", saved=TT(cacheRead=35000), caused=TT(cacheWrite5m=35000),
                                  frm=P("tools", 0), to=P("messages", 3), note="35,000 tokens move from read to write")],
                          cbp=cb)
    write_more("setting/web-search-toggle/break-from-system", "F2",
               "A router turns web search on for request 5. Anthropic renders the toggle into the system prompt, so the "
               "tools stay valid and the system and messages do not: with a breakpoint on tools the provider reads 5,000 "
               "tokens and writes the other 36,000. The setting carries system[0]. 35,000 x ($3.75 - $0.30) = -$0.12075.",
               DIMS("setting", "system", "break", setting="web_search", **{"break": "setting"}), steps,
               open_=[BSTAR_SNAP, "design §4.2 setting rules: system[0] carries a breakpoint of its own, so the last breakpoint "
                      "strictly before it is the one on tools. The case adds that breakpoint; with only system and last-message "
                      "breakpoints the read would be 0"])
    # F4: OpenAI effort change; the usage shows the whole prefix read, so no break.
    steps = setting_steps("rt:effort-4c1a", {"effort": {"from": "high", "to": "low", "at": P("tools", 0)}},
                          U(read=40000, write=1000), [], model="gpt-5.6", params={"effort": "low"},
                          params_from={"effort": "high"}, facts={"ttl": "30m", "lifetime": "minimum"})
    write_more("setting/effort-change/read-intact-no-break", "F4",
               "A router lowers the effort of an OpenAI request. The setting names tools[0] as the point where a break "
               "could start, but the provider kept the entry: it read all 40,000 tokens. At is where a break would start, "
               "not a statement that one happened (§4.2 rule 2), and row 3 of §5.2 needs a read below the prediction. No "
               "effect, no cost, a zero total.",
               DIMS("setting", "tools", "hit", setting="effort", **{"break": "none"}), steps)
    # F5: a setting with no position.
    steps = setting_steps("rt:effort-5d70", {"effort": {"from": "high", "to": "low"}}, U(read=8000, write=33000),
                          [effect("break", "rt:effort-5d70", saved=TT(cacheRead=32000), caused=TT(cacheWrite5m=32000),
                                  frm=P("system", 0), to=P("messages", 3),
                                  note="from the read the usage shows; the position of the cause is unknown")],
                          unpriced=[{"opId": "op1", "reason": "penalty_unknown"}], params={"effort": "low"},
                          params_from={"effort": "high"})
    write_more("setting/nil-position/penalty-unknown", "F5",
               "The router lowered effort, but its reporter does not know where the provider renders it, so the setting "
               "has no position. The provider read 8,000 tokens and wrote 33,000. With no At the library takes the read "
               "the usage shows as the end of the valid prefix: the break covers 32,000 tokens, -$0.11040, and the "
               "operation is reported unpriced with penalty_unknown. The figure is a floor, so a host must show it as "
               "an estimate (§6.4).",
               DIMS("setting", "nil", "break", setting="effort", **{"break": "setting"}), steps, open_=[BSTAR_SNAP])


# H2: a summary splices the message list and a later operator reports in the spliced positions.
def h2():
    SUH, CFH = "cg:summarize:h2c3", "cg:cmdfilter:h2d6"
    tools, sysb, old = summary_bodies()
    summary, tail = [("summary", 500)], old[3:]
    spend = {"id": "op1", "kind": "spend", "operator": "context-guru.summarize", "changeId": SUH,
             "call": {"model": "summarizer-model", "params": {"effort": "low"},
                      "usage": {"input": 10000, "cacheRead": 0, "cacheWrite": 0, "cacheWrite1h": 0, "output": 500, "costUSD": 0.0125}}}
    rw = rewrite("op2", "context-guru.summarize", "messages", 0, 10000, 500, SUH, splice={"at": 0, "removed": 3, "inserted": 1})
    cm = rewrite("op3", "context-guru.cmdfilter", "messages", 3, 6000, 600, CFH)
    s0 = sim(0, "r0", (tools, sysb, old), (tools, sysb, old), [], "unknown")
    s1 = sim(1500, "r1", (tools, sysb, old + [("n0", 6000)]), (tools, sysb, summary + tail + [("n0f", 600)]),
             [eop(SUH, call=spend["call"]), eop(SUH, saved=TT(cacheWrite5m=9500)), eop(CFH, saved=TT(cacheWrite5m=5400))],
             "cold", add=[spend, rw, cm])
    write_more("rewrite+spend/summary-then-cmdfilter/positions/5m", "H2",
               "On a cold request the summary replaces messages 0 to 2 with one (a splice of 3 removed, 1 inserted), and "
               "cmdfilter, which runs after it, reports its 6,000-token output at index 3 of the summary's output: client "
               "message 5. The order check maps that index back through the splice. Both removals are writes: 9,500 and "
               "5,400 tokens, +$0.05588, against the summarizer's call -$0.01250. Total +$0.04338.",
               DIMS("rewrite+spend", "mixed", "miss", operations="2", splice="true"), [s0, s1], open_=[BSTAR_SNAP])


# H5: what the library cannot price, and what it must still notice.
def h5():
    sysb = [("sys", 2000)]
    m = [(f"m{i}", 3000) for i in range(4)]
    bogus = {"id": "op1", "kind": "quantum", "operator": "context-guru.quantum", "changeId": "cg:quantum:5a0e"}
    s0 = sim(0, "r0", ([], sysb, m), ([], sysb, m), [], "unknown", add=[bogus],
             unpriced=[{"opId": "op1", "reason": "unknown_kind"}])
    mc = m + [("m4", 6000)]
    mf = m + [("m4f", 1000)]
    s1 = sim(40, "r1", ([], sysb, mc), ([], sysb, mf), [], "hot", prev=(14000, 14000), checks=["sum", "conservation"])
    write_more("unpriced/unknown-kind-and-unreported-edit", "H5",
               "A newer operator reports an operation of a kind this library does not know: it is carried and reported "
               "unpriced (unknown_kind), and prices to nothing. On the next request a plugin shortens message 4 by 5,000 "
               "tokens and reports nothing. The client body is 5,000 tokens larger than the forwarded one with no "
               "operation to account for it, so the sum check fails, and the bill difference matches no operation, so "
               "the conservation check fails. Pricing goes on.",
               DIMS("none", "afterBStar", "hit", check="sum,conservation"), [s0, s1])


# C3: one failed_run change removes two runs, one inside the cached prefix and one new.
def c3_():
    FR = "cg:failed_run:c3f2"
    M = [("m0", 3000), ("m1", 4000)] + [(f"m{i}", 3000) for i in range(2, 7)]
    bp = lambda last: BP(("system", 0), ("messages", 6), ("messages", last))
    s0 = sim(0, "r0", ([], SYS10, M), ([], SYS10, M), [], "unknown", cbp=bp(6), fbp=bp(6))
    tail = [("m7", 5000)]
    F = [M[0], ("m1f", 100)] + M[2:] + [("m7f", 200)]
    spans = [{"section": "messages", "from": 1, "to": 1, "sizeBefore": size(4000), "sizeAfter": size(100)},
             {"section": "messages", "from": 7, "to": 7, "sizeBefore": size(5000), "sizeAfter": size(200)}]
    op = rewrite("o1", "context-guru.failed_run", "messages", 1, 9000, 300, FR, spans=spans)
    s1 = sim(40, "r1", ([], SYS10, M + tail), ([], SYS10, F), [eop(FR, saved=TT(cacheRead=3900, cacheWrite5m=4800))], "hot",
             add=[op], cbp=bp(7), fbp=bp(7), prev=(24000, 24000), usage=U(read=2000, write=18300),
             base=U(read=24000, write=5000), boundary=P("messages", 6),
             effects=[effect("break", FR, saved=TT(cacheRead=18100), caused=TT(cacheWrite5m=18100),
                             frm=P("system", 0), to=P("messages", 6), note="message 1 is inside the cached prefix")])
    write_more("rewrite/two-spans-two-tiers/hot/5m", "C3",
               "failed_run removes two failed runs as one change with two spans: message 1 (4,000 to 100 tokens), inside the "
               "prefix the last turn cached, and a new message 7 (5,000 to 200 tokens). The first span would have been "
               "read and the second written, so one change is priced at two tiers: 3,900 x $0.30 + 4,800 x $3.75 = "
               "+$0.01917. Message 1 is also inside the cached prefix, so the provider reads only the 2,000 tokens of the "
               "system block and writes the 18,300 after it: the break moves 18,100 tokens, -$0.06245. Net -$0.04328.",
               DIMS("rewrite", "mixed", "break", spans="true", **{"break": "plugin"}), [s0, s1],
               open_=["design §6.2: the break starts at the read the usage shows, here the end of system; the case takes the "
                      "client breakpoints as system, message 6 and the last message", BSTAR_SNAP])


if __name__ == "__main__":
    os.makedirs(OUT, exist_ok=True)
    for old in os.listdir(OUT):  # the generator owns this directory: drop cases that no longer exist
        if old.endswith(".json"):
            os.remove(os.path.join(OUT, old))
    for f in (ex12, ex3, ex4, ex5, ex67, ex8, ex9, c01, c02, c03_04, c05, c06, c07, c08, c09, c10, c11, c12, c13,
              c14, c15, c16, c17, c18, a1, a2, a3, a4_b3, b1_b2, b4, b5, c1_, c2_, c3_, c5, d2, d3, d4,
              d5, d6, e2, e345, f245, h2, h5):
        f()
