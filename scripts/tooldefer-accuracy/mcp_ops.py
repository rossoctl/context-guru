# Second tiny stdio MCP server for the ultrahigh accuracy test. Fake tools. Not production code.
import json, sys
TOOLS=[
 {"name":"create_ticket","description":"Creates a ticket in the team's issue tracker. Use it to file a bug, a task or a request for the on-call team. Returns the new ticket id.","inputSchema":{"type":"object","properties":{"title":{"type":"string","description":"One-line summary of the ticket."},"body":{"type":"string","description":"Full description."},"priority":{"type":"string","enum":["low","medium","high","urgent"],"description":"How urgent the ticket is."}},"required":["title","priority"]}},
 {"name":"page_oncall","description":"Pages the engineer who is on call now for a service, by phone and chat. Use it only for an outage or an incident that needs a person at once.","inputSchema":{"type":"object","properties":{"service":{"type":"string","description":"Service name, for example payments or search."},"message":{"type":"string","description":"What is wrong."}},"required":["service","message"]}},
 {"name":"search_runbooks","description":"Searches the operations runbooks (how-to guides for incidents, deploys and maintenance) and returns the best matching runbook titles with a short extract.","inputSchema":{"type":"object","properties":{"query":{"type":"string","description":"Words to search for."},"limit":{"type":"integer","description":"How many results, default 3."}},"required":["query"]}},
]
ANS={"create_ticket":lambda a:"Created ticket OPS-4417: %s"%a.get("title"),
     "page_oncall":lambda a:"Paged on-call for %s (probe 9981)."%a.get("service"),
     "search_runbooks":lambda a:"1. Rotating TLS certificates (probe 3302): run certctl rotate --service <name>.\n2. Draining a node."}
for line in sys.stdin:
    try: m=json.loads(line)
    except Exception: continue
    mid=m.get("id"); meth=m.get("method")
    if mid is None: continue
    if meth=="initialize": r={"protocolVersion":m["params"].get("protocolVersion","2024-11-05"),"capabilities":{"tools":{}},"serverInfo":{"name":"ops","version":"0.1"}}
    elif meth=="tools/list": r={"tools":TOOLS}
    elif meth=="tools/call":
        p=m["params"]; r={"content":[{"type":"text","text":ANS[p["name"]](p.get("arguments",{}))}]}
    else:
        sys.stdout.write(json.dumps({"jsonrpc":"2.0","id":mid,"error":{"code":-32601,"message":"no"}})+"\n"); sys.stdout.flush(); continue
    sys.stdout.write(json.dumps({"jsonrpc":"2.0","id":mid,"result":r})+"\n"); sys.stdout.flush()
