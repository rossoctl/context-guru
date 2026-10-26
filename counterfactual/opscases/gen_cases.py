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


def size(t, tok="o200k_base"):
    return {"bytes": 4 * t, "tokens": t, "tokenizer": tok}


def rewrite(oid, operator, section, idx, before, after, change, tok="o200k_base", spans=None, splice=None):
    op = {"id": oid, "kind": "rewrite", "operator": operator, "at": P(section, idx),
          "sizeBefore": size(before, tok), "sizeAfter": size(after, tok), "changeId": change}
    if spans:
        op["spans"] = spans
    if splice:
        op["messageSplice"] = splice
    return op


def eop(change, saved=None, caused=None, note=None, call=None):
    e = {"changeId": change, "saved": saved or {}, "caused": caused or {}}
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


def expect(phase, baseline, ops, boundary=None, unpriced=None, effects=None):
    e = {"phase": phase, "baselineUsage": baseline, "ops": ops}
    if effects:
        e["effects"] = effects
    if boundary:
        e["boundary"] = boundary
    if unpriced:
        e["unpriced"] = unpriced
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
    spans = [{"section": "tools", "from": i, "to": i, "sizeBefore": size(t, "bytes-estimate"),
              "sizeAfter": size(0, "bytes-estimate")} for i, t in ((2, 3500), (9, 2000), (15, 2500))]
    return rewrite("op1", "tool-prune", "tools", 2, 8000, 0, TP, tok="bytes-estimate", spans=spans)


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
                              frm=P("system", 0), to=P("messages", 4),
                              note="the summary and the tail are written, not read")]
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


if __name__ == "__main__":
    os.makedirs(OUT, exist_ok=True)
    for old in os.listdir(OUT):  # the generator owns this directory: drop cases that no longer exist
        if old.endswith(".json"):
            os.remove(os.path.join(OUT, old))
    for f in ex12, ex3, ex4, ex5, ex67, ex8:
        f()
