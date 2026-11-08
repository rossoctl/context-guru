# Accuracy-test driver. Lanes s1, s2 run in parallel threads; s3 runs serially (it restores the
# seeded snapshot before each run). Resumable: a run with a row in logs/runs.jsonl is skipped.
# usage: driver.py <model> <reps> <task-ids,|all> <setups e.g. s1,s2,s3> <seconds budget>
import json, os, subprocess, sys, threading, time, uuid, shutil
D=os.environ.get("TD871_ROOT") or os.path.dirname(os.path.abspath(__file__)); MODEL=sys.argv[1]; REPS=int(sys.argv[2]); IDS=sys.argv[3]; SETUPS=sys.argv[4].split(","); BUDGET=float(sys.argv[5])
T0=time.time(); LOCK=threading.Lock()
tasks=[l.split("\t") for l in open(D+"/tasks.tsv").read().splitlines() if l]
if IDS!="all": tasks=[t for t in tasks if t[0] in IDS.split(",")]
done=set()
if os.path.exists(D+"/logs/runs.jsonl"):
    for l in open(D+"/logs/runs.jsonl"):
        r=json.loads(l); done.add((r["setup"],r["model"],r["rep"],r["task"]))
def reset(wd):
    if not os.path.isdir(wd+"/.git"):
        shutil.copytree(D+"/tmpl",wd)
    subprocess.run(["git","-C",wd,"worktree","prune"],capture_output=True)
    for w in subprocess.run(["git","-C",wd,"worktree","list","--porcelain"],capture_output=True,text=True).stdout.split("\n"):
        if w.startswith("worktree ") and w[9:]!=os.path.realpath(wd): subprocess.run(["git","-C",wd,"worktree","remove","--force",w[9:]],capture_output=True)
    subprocess.run(["git","-C",wd,"checkout","-q","-f","main"],capture_output=True)
    subprocess.run(["git","-C",wd,"reset","-q","--hard"],capture_output=True)
    subprocess.run(["git","-C",wd,"clean","-qfdx"],capture_output=True)
    for b in subprocess.run(["git","-C",wd,"branch","--format=%(refname:short)"],capture_output=True,text=True).stdout.split():
        if b!="main": subprocess.run(["git","-C",wd,"branch","-D",b],capture_output=True)
import urllib.request
def snap_stats(n,port):
    try:
        d=json.load(urllib.request.urlopen("http://127.0.0.1:%s/stats"%port,timeout=3)).get("tool_defer") or {}
        with LOCK: open(D+"/logs/stats.jsonl","a").write(json.dumps({"proxy":n,"t":time.time(),"td":d})+"\n")
    except Exception: pass
def restore_s3(w,setup="s3"):
    off=0 if setup=="s3" else 10
    n="px%s-%d"%(setup[1:],w); port=str(18990+off+w); dd=D+"/%sdb-%d"%(setup,w)
    snap_stats(n,port)
    subprocess.run([D+"/stopproxy.sh",n])
    for f in ("dash.db","dash.db-wal","dash.db-shm","context-guru-tooldefer.db"):
        p=dd+"/"+f
        if os.path.exists(p): os.remove(p)
    os.makedirs(dd,exist_ok=True); shutil.copy(D+"/snap/dash.db",dd+"/dash.db")
    subprocess.run([D+"/startproxy.sh",n,port,"ultrahigh",dd+"/dash.db"])
    subprocess.run([D+"/warm.sh",port],capture_output=True)
    if setup=="s3b":
        # The client probe must pass before a thread starts, or the thread takes the server-side path. A throwaway run starts it.
        lg=D+"/logs/%s.log"%n
        subprocess.run([D+"/runcc.sh","warm-%s-%d.jsonl"%(setup,w),"acc-%s-%d"%(setup,w),"%s-%d"%(setup,w),port,MODEL,D+"/mcp.json","-","--","Reply with the word OK. (Warm-up id "+str(uuid.uuid4())+".)","--max-turns","1"],capture_output=True,text=True)
        for _ in range(60):
            if "probe=client" in open(lg).read() and "probe=client model=%s route=/v1/messages verdict=pass"%MODEL in open(lg).read(): break
            time.sleep(0.5)
def one(setup,rep,t,w):
    tid,grp,kind,want,prompt=t
    nonce=str(uuid.uuid4())
    p=prompt+"\n\n(Test run id "+nonce+": ignore it.)"
    port=18981 if setup not in ("s3","s3b") else 18990+w+(10 if setup=="s3b" else 0)
    extra="-" if setup in ("s2","s3b") else "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1"
    out=f"acc-{setup}-{MODEL[7:]}-{rep}-{tid}.jsonl"
    wd=D+"/work/%s-%d"%(setup,w); reset(wd)
    if setup in ("s3","s3b"): restore_s3(w,setup)
    t1=time.time()
    r=subprocess.run([D+"/runcc.sh",out,"acc-%s-%d"%(setup,w),"%s-%d"%(setup,w),str(port),MODEL,D+"/mcp.json",extra,"--",p,"--max-turns","8"],capture_output=True,text=True)
    sid=r.stdout.strip().rsplit("sid=",1)[-1]
    row={"setup":setup,"model":MODEL,"rep":rep,"task":tid,"group":grp,"kind":kind,"want":want,"sid":sid,"nonce":nonce,"out":out,"secs":round(time.time()-t1,1),"rc":r.stdout.strip()}
    with LOCK: open(D+"/logs/runs.jsonl","a").write(json.dumps(row)+"\n")
    print(setup,rep,tid,row["rc"],row["secs"],flush=True)
import queue
NW={"s1":int(os.environ.get("NW1",2)),"s2":int(os.environ.get("NW2",2)),"s3":int(os.environ.get("NW3",4)),"s3b":int(os.environ.get("NW3",4))}
qs={s:queue.Queue() for s in SETUPS}
for rep in range(1,REPS+1):
    for t in tasks:
        for s in SETUPS:
            if (s,MODEL,rep,t[0]) not in done: qs[s].put((rep,t))
def worker(s,w):
    while time.time()-T0<BUDGET:
        try: rep,t=qs[s].get_nowait()
        except queue.Empty: return
        one(s,rep,t,w)
th=[threading.Thread(target=worker,args=(s,w)) for s in SETUPS for w in range(NW[s])]
for x in th: x.start()
for x in th: x.join()
for w in range(4):
    for nm,off in (("px3-%d",0),("px3b-%d",10)):
        snap_stats(nm%w,18990+off+w)
        subprocess.run([D+"/stopproxy.sh",nm%w])
print("elapsed",round(time.time()-T0),"left",{s:qs[s].qsize() for s in SETUPS})
