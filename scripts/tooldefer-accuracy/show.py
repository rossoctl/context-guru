import json,sys,os
start=int(sys.argv[1]) if len(sys.argv)>1 else 0
for i,l in enumerate(open((os.environ.get("TD871_ROOT") or os.path.dirname(os.path.abspath(__file__)))+"/logs/tap.jsonl")):
    r=json.loads(l)
    if i<start: continue
    print(r['seq'],(r['sess'] or '-')[:8],r['model'][7:18] if r['model'] else '-',r['status'],'nt',r['ntools'],'def',len(r['deferred']),'srch' if r['search_tool'] else '','probe' if r['probe'] else '',
      'in',r['in'],'cw',r['cw'],'cr',r['cr'],'out',r['out'],r['stu'] or '',r['refs'] or '',r['tool_use'] or '',r['tools_h'],r['m1_h'],(r['err'] or '')[:100])
