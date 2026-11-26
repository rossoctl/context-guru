# Logging forwarder: proxy -> tap -> plain gateway. Records per-request counts only. Test only.
# Flag files in D: capture (save request bodies to cap/), inject400 (one 400 for the first request with defer_loading tools).
import json, os, sys, hashlib, urllib.request, urllib.error, http.server, threading
UP=os.environ["TD871_GATEWAY_URL"].rstrip("/")
PORT=int(sys.argv[1]); D=os.environ.get("TD871_ROOT") or os.path.dirname(os.path.abspath(__file__)); SEQ=[0]; L=threading.Lock()
JL=open(D+"/logs/tap.jsonl","a")
def strip(o):
    if isinstance(o,dict): return {k:strip(v) for k,v in o.items() if k!="cache_control"}
    if isinstance(o,list): return [strip(x) for x in o]
    return o
def h(o): return hashlib.sha256(json.dumps(o,separators=(",",":")).encode()).hexdigest()[:12]
def usage_of(raw, sse):
    u={}; stu=[]; refs=[]; tus=[]; mid=None; stop=None
    def blk(cb):
        if cb["type"]=="server_tool_use": stu.append(cb.get("name"))
        if cb["type"]=="tool_search_tool_result":
            c=cb.get("content") or {}
            refs.extend(r.get("tool_name") for r in (c.get("tool_references") or []))
        if cb["type"]=="tool_use": tus.append(cb.get("name"))
    if sse:
        for ev in raw.decode("utf-8","replace").split("\n"):
            if not ev.startswith("data:"): continue
            try: d=json.loads(ev[5:])
            except Exception: continue
            t=d.get("type")
            if t=="message_start": u.update(d["message"].get("usage",{})); mid=d["message"].get("id")
            elif t=="message_delta": u.update(d.get("usage",{})); stop=d.get("delta",{}).get("stop_reason")
            elif t=="content_block_start": blk(d["content_block"])
    else:
        try:
            m=json.loads(raw); u=m.get("usage",{}) or {}; mid=m.get("id"); stop=m.get("stop_reason")
            for b in m.get("content",[]) or []: blk(b)
        except Exception: pass
    return u, stu, refs, tus, mid, stop
def req_refs(b):
    out=[]
    try:
        last=b["messages"][-1]
        for c in last.get("content",[]) if isinstance(last.get("content"),list) else []:
            if c.get("type")=="tool_result" and isinstance(c.get("content"),list):
                out+=[x.get("tool_name") for x in c["content"] if x.get("type")=="tool_reference"]
    except Exception: pass
    return out
class H(http.server.BaseHTTPRequestHandler):
    protocol_version="HTTP/1.1"
    def log_message(self,*a): pass
    def do_POST(self):
        n=int(self.headers.get("content-length",0)); raw=self.rfile.read(n)
        with L: SEQ[0]+=1; seq=SEQ[0]
        hdr={k:v for k,v in self.headers.items() if k.lower() not in ("host","content-length","accept-encoding","connection")}
        sess=self.headers.get("X-Claude-Code-Session-Id") or self.headers.get("x-claude-code-session-id")
        try: b=json.loads(raw)
        except Exception: b={}
        if not isinstance(b,dict): b={}
        tools=b.get("tools",[]) or []
        deferred=sorted(t.get("name") for t in tools if t.get("defer_loading"))
        if os.path.exists(D+"/capture"): open(f"{D}/cap/{seq:05d}-req.json","wb").write(raw)
        inj=False
        if deferred and "tool_search_tool_bm25" in [t.get("name") for t in tools] and os.path.exists(D+"/inject400"):
            try: os.remove(D+"/inject400"); inj=True
            except FileNotFoundError: pass
        if inj:
            st=400; rh={"Content-Type":"application/json"}
            rb=json.dumps({"type":"error","error":{"type":"invalid_request_error","message":"tools.3.defer_loading: injected test refusal (tool_search not available)"}}).encode()
        else:
            req=urllib.request.Request(UP+self.path,data=raw,headers=hdr,method="POST")
            try: r=urllib.request.urlopen(req,timeout=600); st=r.status; rh=dict(r.headers); rb=r.read()
            except urllib.error.HTTPError as e: st=e.code; rh=dict(e.headers); rb=e.read()
            except Exception as e: st=599; rh={"Content-Type":"text/plain"}; rb=str(e).encode()
        sse="event-stream" in rh.get("Content-Type",rh.get("content-type",""))
        u,stu,refs,tus,mid,stop=usage_of(rb,sse)
        msgs=b.get("messages",[]) or []
        rec={"seq":seq,"sess":sess,"path":self.path,"status":st,"injected":inj,"model":b.get("model"),"mid":mid,"stop":stop,
             "ntools":len(tools),"deferred":deferred,"search_tool":any(t.get("name")=="tool_search_tool_bm25" for t in tools),
             "tools_h":h(tools),"m1_h":h(msgs[1]) if len(msgs)>1 else None,"m1_hn":h(strip(msgs[1])) if len(msgs)>1 else None,"tools_hn":h(strip(tools)),"m1_len":len(json.dumps(msgs[1])) if len(msgs)>1 else 0,"m1_role":msgs[1].get("role") if len(msgs)>1 else None,
             "nmsgs":len(msgs),"probe":any(str(t.get("name","")).startswith("cg_probe") for t in tools),
             "thinking":(b.get("thinking") or {}).get("type"),
             "in":u.get("input_tokens"),"cw":u.get("cache_creation_input_tokens"),"cr":u.get("cache_read_input_tokens"),"out":u.get("output_tokens"),
             "stu":stu,"refs":refs,"req_refs":req_refs(b),"tool_use":tus,"err":None if st==200 else rb[:300].decode("utf-8","replace")}
        with L: JL.write(json.dumps(rec)+"\n"); JL.flush()
        self.send_response(st)
        for k,v in rh.items():
            if k.lower() in ("content-type","request-id"): self.send_header(k,v)
        self.send_header("content-length",str(len(rb))); self.end_headers(); self.wfile.write(rb)
http.server.ThreadingHTTPServer(("127.0.0.1",PORT),H).serve_forever()
