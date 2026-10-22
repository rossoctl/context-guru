# Waste

`/context-guru:insights tools` (was `/context-guru:waste`, which still works for one release). `/context-guru:insights` shows it together with the other two.

**Question: what do I carry and never use?**

Every MCP server, skill and tool you declare is sent with every request, whether or not you ever call it. This screen lists the ones you never called, what carrying them cost, and the command that removes each.

![/context-guru:insights tools](img/waste-dark.svg#only-dark)
![/context-guru:insights tools](img/waste-light.svg#only-light)

```text
What you carry (MCP servers, their tools and skills; this proxy reports only the part carried while unused)
  MCP server  playwright   22 tools  never called  $18.40
  MCP server  sentry        9 tools  never called   $7.10
  MCP server  figma        14 tools  never called   $6.20
  MCP server  github       41 tools      87 calls      $0
  skill       brand-voice            never called   $1.60
  skill       pdf                    never called   $1.10

5 things you carry were never used: $34.40 in 7 days, about $147.43 a month.

Top 5 of 5:
  MCP server  playwright   $18.40  ██████████
  MCP server  sentry        $7.10  ████░░░░░░
  MCP server  figma         $6.20  ███░░░░░░░
  skill       brand-voice   $1.60  █░░░░░░░░░
  skill       pdf           $1.10  █░░░░░░░░░

Remove the first:
  claude mcp remove playwright

Measured carry; the monthly figure is an estimate that assumes your usage stays the same.

More: /context-guru:insights tools details
```

## Reading it

- The first line counts them and gives the dollars carried in the window, and a monthly figure that assumes your usage stays the same (an estimate).
- The list shows the top five; `details` shows all of them, each with the command that removes it.
- Context Guru's own components and Claude Code's built-in tools are never offered for removal.
- If something was used recently the row says so, so check before you remove it.
