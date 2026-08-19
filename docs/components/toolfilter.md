# `toolfilter`

Drops the tool and MCP-server declarations an account has explicitly opted to stop sending.

| | |
|---|---|
| Kind | Reformat (marker: the rewrite lives in `apply`, on the top-level `tools` array) |
| Lossy? | Yes, deliberately and by name — a removed declaration is not sent |
| Reversible? | Yes: delete the name from `remove`. Re-anchors the prefix once (~$0.70) |
| In any preset? | **No.** It has no effect without an explicit list, and the list is a decision only the account can make |
| Config | `remove: [<declaration name> ...]`, plus `mcp__<server>` for a whole MCP server |

```yaml
components:
  toolfilter:
    remove: [CronCreate, Workflow, mcp__playwright]
```

Measured value, the safety rules (the prose gate, the determinism argument, the sufficiency
rule behind a suggestion) and the realized-vs-projected accounting are all in
[Stop carrying tools you never use](../how-to/declaration-removal.md). The mechanism is
`apply/toolfilter.go`; the suggestions and the accounting are `dash/toolsuggest.go`, served by
`GET /api/toolfilter` (read) and `POST /api/toolfilter` (write, control plane).

Never removed, whatever the list says: a declaration whose name still appears in the system
prompt's prose, the tool `tool_choice` forces, a provider-side tool declared by `type`, and
the last remaining tool.
