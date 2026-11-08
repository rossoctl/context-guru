import json, sys, collections, os
D=os.environ.get("TD871_ROOT") or os.path.dirname(os.path.abspath(__file__))
filt=sys.argv[1] if len(sys.argv)>1 else ""
tap=collections.defaultdict(list); mids=collections.Counter()
for l in open(D+"/logs/tap.jsonl"):
    r=json.loads(l); tap[r["sess"]].append(r)
    if r["mid"]: mids[r["mid"]]+=1
hook=collections.defaultdict(list)
for l in open(D+"/logs/hook.jsonl"):
    r=json.loads(l); hook[r["sid"]].append(r)
rows=[]
for l in open(D+"/logs/runs.jsonl"):
    r=json.loads(l)
    if filt and filt not in json.dumps(r): continue
    want=r["want"].split(",")
    lines=open(D+"/runs/"+r["out"]).read().strip().split("\n")
    res={}; cc_uses=[]; ts_calls=[]; ts_loaded=[]; errs=0
    for x in lines:
        try: m=json.loads(x)
        except Exception: continue
        if m.get("type")=="result": res=m
        if m.get("type")=="assistant":
            for b in m["message"].get("content",[]):
                if b.get("type")=="tool_use":
                    cc_uses.append(b["name"])
                    if b["name"]=="ToolSearch": ts_calls.append(json.dumps(b.get("input"))[:60])
                if b.get("type")=="text" and "API Error" in b.get("text",""): errs+=1
        if m.get("type")=="user":
            c=m["message"].get("content")
            if isinstance(c,list):
                for b in c:
                    if b.get("type")=="tool_result" and isinstance(b.get("content"),list):
                        ts_loaded+=[y.get("tool_name") for y in b["content"] if y.get("type")=="tool_reference"]
    hk=[h["tool"] for h in hook.get(r["sid"],[])]
    tr=tap.get(r["sid"],[])
    srv=[s for t in tr for s in t["stu"] if s=="tool_search_tool_bm25"]
    srv_loaded=[x for t in tr for x in t["refs"]]
    nonok=[t["status"] for t in tr if t["status"]!=200]
    first=next((t for t in tr if t["ntools"]>5),None)
    wd=bool(first and any(w in first["deferred"] for w in want))
    right=any(w in hk or w in cc_uses for w in want)
    searches=len(ts_calls)+len(srv)
    loaded=ts_loaded+srv_loaded
    wrong=sum(1 for x in loaded if x not in want)
    api_err=res.get("api_error_status")
    four=1 if (api_err or errs or res.get("is_error")) else 0
    rows.append(dict(setup=r["setup"],model=r["model"],rep=r["rep"],task=r["task"],kind=r["kind"],want=r["want"],right=right,searches=searches,
        wrong=wrong,turns=res.get("num_turns"),client4xx=four,api_err=api_err,cost=res.get("total_cost_usd",0),deferred_first=wd,
        ndeferred=len(first["deferred"]) if first else None,upstream_non200=nonok,used=[u for u in hk if u not in want][:5],loaded=loaded[:4],
        ts=ts_calls[:2],nreq=len(tr),cr=sum((t["cr"] or 0) for t in tr),cw=sum((t["cw"] or 0) for t in tr),sid=r["sid"]))
dups=[m for m,c in mids.items() if c>1]
if "-v" in sys.argv:
    for x in rows: print(x)
agg=collections.OrderedDict()
for x in rows:
    k=(x["model"],x["setup"]); a=agg.setdefault(k,collections.Counter())
    a["runs"]+=1; a["right"]+=x["right"]; a["searches"]+=x["searches"]; a["wrong"]+=x["wrong"]; a["turns"]+=x["turns"] or 0
    a["4xx"]+=x["client4xx"]; a["cost"]+=x["cost"]; a["def_ok"]+=x["deferred_first"]; a["up_non200"]+=len(x["upstream_non200"])
print("model setup runs right searches wrong turns 4xx cost deferred_first upstream_non200")
for (m,s),a in sorted(agg.items()):
    print(m,s,a["runs"],a["right"],a["searches"],a["wrong"],a["turns"],a["4xx"],"%.2f"%a["cost"],a["def_ok"],a["up_non200"])
print("duplicate response ids:",len(dups))
json.dump(rows,open(D+"/logs/rows.json","w"))
