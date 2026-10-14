# Waste

**Question: what do I carry and never use?**

Every MCP server, skill and tool you declare is sent with every request, whether or not you ever call it. This screen lists the ones you never called, what carrying them cost, and the command that removes each.

![/context-guru:waste](img/waste-dark.svg#only-dark)
![/context-guru:waste](img/waste-light.svg#only-light)

```text
6 things you carry were never used: $37.70 in 7 days, about $161.57 a month.

Top 5 of 6:
  MCP server     playwright    $18.40  ██████████
  MCP server     sentry         $7.10  ████░░░░░░
  MCP server     figma          $6.20  ███░░░░░░░
  built-in tool  NotebookEdit   $3.30  ██░░░░░░░░
  skill          brand-voice    $1.60  █░░░░░░░░░

Remove the first:
  claude mcp remove playwright

Measured carry; the monthly figure is an estimate that assumes your usage stays the same.

More: /context-guru:waste details
```

## Reading it

- The first line counts them and gives the dollars carried in the window, and a monthly figure that assumes your usage stays the same (an estimate).
- The list shows the top five; `details` shows all of them, each with the command that removes it.
- Context Guru's own components and Claude Code's built-in tools are never offered for removal.
- If something was used recently the row says so, so check before you remove it.
