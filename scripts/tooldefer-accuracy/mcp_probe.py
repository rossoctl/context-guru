# Tiny stdio MCP server for #485 tests. Not production code.
import json, sys
TOOLS=[
 {"name":"get_weather","description":"Returns the current weather and the forecast for the next three days for a city: temperature in Celsius, wind, rain chance and a one-line summary. Use it for any question about weather, temperature, rain or what to wear outside.","inputSchema":{"type":"object","properties":{"city":{"type":"string","description":"City name, for example Lisbon or New York."}},"required":["city"]}},
 {"name":"convert_currency","description":"Converts an amount of money from one currency to another at today's market exchange rate. Takes ISO 4217 codes such as USD, EUR, GBP, JPY. Use it for any question about prices in another currency or exchange rates.","inputSchema":{"type":"object","properties":{"amount":{"type":"number"},"from":{"type":"string"},"to":{"type":"string"}},"required":["amount","from","to"]}},
 {"name":"lookup_zip","description":"Looks up the city, county and state for a US ZIP code (five digits). Use it to find where a ZIP code is, or to check that an address has the right ZIP code.","inputSchema":{"type":"object","properties":{"zip":{"type":"string"}},"required":["zip"]}},
]
ANS={"get_weather":lambda a:"Weather in %s: sunny, 21C (probe value 7731)."%a.get("city"),
     "convert_currency":lambda a:"%s %s = %.2f %s (probe rate 1.10)"%(a.get("amount"),a.get("from"),float(a.get("amount",0))*1.1,a.get("to")),
     "lookup_zip":lambda a:"ZIP %s: Springfield, IL (probe 5512)"%a.get("zip")}
for line in sys.stdin:
    try: m=json.loads(line)
    except Exception: continue
    mid=m.get("id"); meth=m.get("method")
    if mid is None: continue
    if meth=="initialize": r={"protocolVersion":m["params"].get("protocolVersion","2024-11-05"),"capabilities":{"tools":{}},"serverInfo":{"name":"probe","version":"0.1"}}
    elif meth=="tools/list": r={"tools":TOOLS}
    elif meth=="tools/call":
        p=m["params"]; r={"content":[{"type":"text","text":ANS[p["name"]](p.get("arguments",{}))}]}
    else:
        sys.stdout.write(json.dumps({"jsonrpc":"2.0","id":mid,"error":{"code":-32601,"message":"no"}})+"\n"); sys.stdout.flush(); continue
    sys.stdout.write(json.dumps({"jsonrpc":"2.0","id":mid,"result":r})+"\n"); sys.stdout.flush()
