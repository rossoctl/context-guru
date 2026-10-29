#!/usr/bin/env python3
"""Synthetic demo DB for dashboard screenshots. No real data. usage: gen_demo_db.py SCHEMA.sql OUT.db"""
import random, sqlite3, sys, time, hashlib
schema, out = sys.argv[1], sys.argv[2]
random.seed(437)
db = sqlite3.connect(out); db.executescript(open(schema).read())
now = int(time.time()*1000); DAY = 86400000
P = {'claude-sonnet-4-5': (3,.3,3.75,15), 'claude-haiku-4-5': (1,.1,1.25,5)}
cols = None
def ins(table, d):
    k = ','.join(d); q = ','.join('?'*len(d))
    return db.execute(f'insert into {table}({k}) values({q})', list(d.values())).lastrowid
for s in range(48):
    sid = 'demo-session-%03d' % s
    model = 'claude-haiku-4-5' if s % 5 == 0 else 'claude-sonnet-4-5'
    pin, prd, pwr, pout = P[model]
    t = now - random.randint(0, 13)*DAY - random.randint(0, DAY//2)
    # Large enough that some sessions pass 100k tokens: the Overview measures cache latency there.
    ctx = random.randint(60000, 110000)
    dig = hashlib.md5(sid.encode()).hexdigest()[:12]  # ties requests to their tool declarations
    for i in range(random.randint(15, 60)):
        t += random.randint(8000, 90000)
        gap = random.random() < .07
        if gap: t += random.randint(310000, 900000)
        ctx += random.randint(300, 3500)
        before = ctx; unique = int(random.random()*random.choice([0, 0, 800, 2500]))
        saved_gross = unique * random.randint(1, 4)
        after = before - saved_gross
        out_t = random.randint(150, 1500)
        miss = 'ttl_expiry' if gap else ('cold_start' if i == 0 else ('prefix_change' if random.random() < .04 else 'hit'))
        if miss == 'hit': rd, wr, fr = int(after*.93), int(after*.05), int(after*.02)
        else: rd, wr, fr = 0, int(after*.97), int(after*.03)
        cost = (fr*pin + rd*prd + wr*pwr + out_t*pout)/1e6
        base = cost + (unique*pwr + (saved_gross-unique)*prd)/1e6
        ka = 1 if (not gap and random.random() < .12) else 0
        # The tools and skills filter left some declarations out of most prompts.
        fdt = random.randint(4000, 12000) if random.random() < .8 else 0
        # Large-context latency: a write is slower than a read.
        big = (fr + rd + wr) >= 100000
        up = (random.uniform(9000, 16000) if miss != 'hit' else random.uniform(5000, 9000)) if big else random.uniform(900, 6000)
        kasv = round(random.uniform(.02, .35), 4) if ka and miss == 'hit' and i > 3 else 0
        rid = ins('requests', dict(ts=t, tenant_id='', session_id=sid, model=model, provider='anthropic', agent='claude-code',
            preset='general', mode='active', route='/v1/messages', status=200, messages=i*2+1, tokens_before=before, tokens_after=after,
            attempted_tokens=int(before*.5), frozen_tokens=int(before*.45), saved_unique=unique, fresh_input=fr, cache_read=rd, cache_write=wr,
            output_tokens=out_t, cost_usd=cost, baseline_cost_usd=base, cg_llm_cost_usd=(0.004 if unique > 2000 and random.random() < .5 else 0),
            cg_latency_ms=random.uniform(5, 60), upstream_ms=up, token_accounting='complete', filtered_decl_tokens=fdt, decl_digest=dig, cache_saved_usd=rd*(pin-prd)/1e6,
            cache_miss_reason=miss, stream=1, tools=38, keepalive_pings=ka, keepalive_saved_usd=kasv, cache_ttl='5m',
            keepalive_strategy_id=('warm-240' if kasv else None)))
        if saved_gross:
            ins('request_components', dict(request_id=rid, component=random.choice(['extract', 'dedup', 'searchfold', 'format']), kind='compaction',
                acted=1, mutated=1, saved_gross=saved_gross, saved_unique=unique, saved_usd=(unique*pwr+(saved_gross-unique)*prd)/1e6))
        if ka and random.random() < .5:  # a ping row
            ins('requests', dict(ts=t+240000, tenant_id='', session_id=sid, model=model, provider='anthropic', agent='claude-code', mode='active',
                status=200, cache_read=int(after*.95), cost_usd=int(after*.95)*prd/1e6, keepalive=1, token_accounting='complete', cache_miss_reason='hit',
                keepalive_strategy_id='warm-240'))
        if random.random() < .03:  # the user pressed Ping
            warm = random.random() < .85
            ins('requests', dict(ts=t+120000, tenant_id='', session_id=sid, model=model, provider='anthropic', agent='claude-code', mode='active',
                status=200, cache_read=int(after*.95) if warm else 0, cache_write=0 if warm else int(after*.95),
                cost_usd=(int(after*.95)*(prd if warm else pwr))/1e6, keepalive=1, manual_ping=1, keepalive_strategy_id='manual',
                manual_saved_usd=(int(after*.95)*pwr/1e6) if warm else 0, token_accounting='complete', cache_miss_reason='hit'))
    # tool inventory: 4 MCP servers, many tools, few used
    servers = {'docs': 6, 'browser': 14, 'issues': 9, 'analytics': 17}
    for sv, n in servers.items():
        for k in range(n):
            nm = f'mcp__{sv}__tool_{k}'
            ins('tool_declarations', dict(tenant_id='', session_id=sid, digest=dig, kind='mcp_tool', name=nm, server=sv, tokens=random.randint(250, 900), ts=t))
            if sv in ('docs',) and k < 2 and s % 3 == 0:
                ins('tool_uses', dict(tenant_id='', session_id=sid, name=nm, server=sv, calls=random.randint(1, 9), first_ts=t, last_ts=t))
    for nm in ['Bash', 'Read', 'Edit', 'Grep', 'Glob', 'Write']:
        ins('tool_declarations', dict(tenant_id='', session_id=sid, digest=dig, kind='tool', name=nm, tokens=random.randint(200, 700), ts=t))
        ins('tool_uses', dict(tenant_id='', session_id=sid, name=nm, calls=random.randint(5, 80), first_ts=t, last_ts=t))
db.commit(); print(db.execute('select count(*) from requests').fetchone())
# Recipe (synthetic data only, no real tenant):
#   sqlite3 <(any dashboard db) .schema | grep -v 'sqlite_stat\|sqlite_sequence' > schema.sql
#   python3 -I gen_demo_db.py schema.sql demo.db
#   DASHBOARD=true DASHBOARD_DB=$PWD/demo.db DASHBOARD_RETENTION=87600h ./context-guru-proxy --listen 127.0.0.1:4152 --anthropic-upstream http://127.0.0.1:9
#   (MODEL_PRICES=prices.yaml with the two demo models priced, so every dollar figure has a value)
#   chrome-headless-shell --remote-debugging-port=9334 & ; node shot.mjs OUTDIR light|dark WIDTH PREFIX
#   (needs pages.json: [[name, "index.html#/view", waitMs, optionalSetupJS], ...])
