# Hosted and local: where they differ, and why

A hosted proxy (multi-tenant, started with `--upstreams`) behaves the same as a local install. It differs only where a stated reason requires it.
A new hosted-only branch must add its row to this page, with its reason.

Hosted mode is the mode where `Options.Tenants` is set. Local mode is the single-tenant proxy.
Line numbers point at the code on `main` when this page was written.

## A. Needed for tenant safety or credentials

| What | Local | Hosted | Why | Data stored |
|---|---|---|---|---|
| `CONTEXT_GURU_DUMP` and `CONTEXT_GURU_CAPTURE` ([main_run.go:531](https://github.com/rossoctl/context-guru/blob/main/proxyapp/main_run.go#L531)) | Write the proxy's own request bodies to one file | The proxy refuses to start | "a shared plaintext transcript of every tenant's source code ... bypassing per-tenant content-capture consent" | Stops raw bodies from being stored |
| Content capture and trace recording ([proxy.go:2877](https://github.com/rossoctl/context-guru/blob/main/proxy/proxy.go#L2877), [recctx.go:68](https://github.com/rossoctl/context-guru/blob/main/proxy/recctx.go#L68)) | The operator flag is enough | The operator flag and the tenant's consent | "the operator enabling a feature is not the same act as a user agreeing to have their transcripts retained" | Decides whether content is stored |
| Server-key compaction model ([proxy.go:1123](https://github.com/rossoctl/context-guru/blob/main/proxy/proxy.go#L1123), [proxy.go:1136](https://github.com/rossoctl/context-guru/blob/main/proxy/proxy.go#L1136), [form.go:189](https://github.com/rossoctl/context-guru/blob/main/config/form.go#L189)) | A model from the source config works | No server model. `config` means no model. `incoming` uses the caller's model and key | "a server-held key must not fund a tenant's compaction" | None |
| Upstream choice ([tenancy.go:715](https://github.com/rossoctl/context-guru/blob/main/proxy/tenancy.go#L715)) | One fixed upstream | The tenant picks a name from the operator's allow-list. The call uses the tenant's own key | "a tenant never supplies a URL, so this stays an allow-list and not an SSRF hop" | None |
| `/stats` and `/metrics` access ([proxy.go:2905](https://github.com/rossoctl/context-guru/blob/main/proxy/proxy.go#L2905), [promexport.go:334](https://github.com/rossoctl/context-guru/blob/main/proxy/promexport.go#L334)) | Open | Loopback or a token | "one tenant's traffic mixed with everyone else's" | None |
| `/stats` leaves out pipeline, preset and savings ([proxy.go:3016](https://github.com/rossoctl/context-guru/blob/main/proxy/proxy.go#L3016), [proxy.go:3043](https://github.com/rossoctl/context-guru/blob/main/proxy/proxy.go#L3043) for the store block, [proxy/savings.go:142](https://github.com/rossoctl/context-guru/blob/main/proxy/savings.go#L142)) | Shown | Left out | "/stats aggregates every tenant and h.pipe describes none of them" | None |
| Bob `count_tokens` and the `/` catch-all ([counttokens.go:37](https://github.com/rossoctl/context-guru/blob/main/proxy/counttokens.go#L37), [proxy.go:671](https://github.com/rossoctl/context-guru/blob/main/proxy/proxy.go#L671)) | Pass through | Authenticate, meter, and inject the tenant's key | "an unmetered open forwarder that also leaked OUR token to the upstream in place of the tenant's" | None |
| Bob model-info forwarding ([proxy.go:710](https://github.com/rossoctl/context-guru/blob/main/proxy/proxy.go#L710)) | The proxy reads the response | Not done | Bob does not run on hosted for now (owner, 2026-11-12) | None |
| Process-wide counters on the dashboard ([dash/api.go:753](https://github.com/rossoctl/context-guru/blob/main/dash/api.go#L753), [app.js:4643](https://github.com/rossoctl/context-guru/blob/main/dash/ui/app.js#L4643)) | Everyone sees them | Managers only | "they leak other tenants' traffic volume" | None |
| Manager-only tabs ([app.js:6141](https://github.com/rossoctl/context-guru/blob/main/dash/ui/app.js#L6141)) | Config and Components open, the rest locked | Hidden from non-managers | "a permissions boundary over somebody else's data" | None |
| `campaigns.js` ([app.js:6188](https://github.com/rossoctl/context-guru/blob/main/dash/ui/app.js#L6188)) | Loaded as before | Loaded for managers only | A plain account must not see manager code in its network tab | None |
| Cheap-model usage per request ([proxy/dashcapture.go:703](https://github.com/rossoctl/context-guru/blob/main/proxy/dashcapture.go#L703), [cheapmodel/usage.go:40](https://github.com/rossoctl/context-guru/blob/main/cheapmodel/usage.go#L40)) | Same in both modes | Same in both modes | A shared counter delta would "let a tenant infer other tenants' compaction activity from its own rows" | Metadata only |
| Per-tenant limits, row quota, and `--idle-exit` ([limits.go](https://github.com/rossoctl/context-guru/blob/main/proxy/limits.go), [main_run.go:237](https://github.com/rossoctl/context-guru/blob/main/proxyapp/main_run.go#L237), [idleexit.go:277](https://github.com/rossoctl/context-guru/blob/main/proxyapp/idleexit.go#L277)) | No limits. `--idle-exit` allowed | Limits and a row quota. `--idle-exit` refused | "a gateway serving other people's agents must not self-terminate" | The row quota changes how much history is kept |
| Session recorder directory ([sessionrec.go:366](https://github.com/rossoctl/context-guru/blob/main/proxy/sessionrec.go#L366)) | One flat directory | One owner-only (0700) directory per tenant | Tenant isolation | Recorded bodies, one directory per tenant |

`--idle-exit` is also refused together with `--bob-upstream`. Bob does not run on hosted for now, so this rule also hits local Bob.

## B. Product choices with a written reason

| What | Local | Hosted | Why | Data stored |
|---|---|---|---|---|
| Start preset for new accounts ([tenant/tenant.go:102](https://github.com/rossoctl/context-guru/blob/main/tenant/tenant.go#L102)) | The preset the user chose | `house` | "on a SHARED box ... it makes no cheap-model calls, so it adds no upstream spend ... and costs near-zero added latency" | None |
| Keep-alive strategies ([keepalivestrategy.go:44](https://github.com/rossoctl/context-guru/blob/main/proxy/keepalivestrategy.go#L44)) | Not read | Read from the account registry | Each account has its own strategies | Strategy rows |
| Manual-ping ceiling ([keepalivectl.go:180](https://github.com/rossoctl/context-guru/blob/main/proxy/keepalivectl.go#L180)) | The default ceiling | The account's own ceiling | Each account has its own cache setting | None |
| Tool deferral file ([tooldefer.go:121](https://github.com/rossoctl/context-guru/blob/main/proxy/tooldefer.go#L121)) | Same in both modes | Same in both modes | Use a file when the session store is not durable (#871) | Tool names |
| Restart grace for observe tenants ([tenancy.go:857](https://github.com/rossoctl/context-guru/blob/main/proxy/tenancy.go#L857)) | Not needed | A key that looks like an observe tenant's gets the start-time rule | #766: "over-protection, the safe direction" | None |
| `/api/adapt` ([adaptapply.go:364](https://github.com/rossoctl/context-guru/blob/main/proxy/adaptapply.go#L364)) | Saved to an override file | Saved in the account registry with an audit row | Each account has its own change history | A change history row |

## Being made the same

These differences have no safety reason. Work is open to remove them.

- **Session-state table.** Local keeps it in the dashboard database. Hosted keeps memory only ([main_run.go:682](https://github.com/rossoctl/context-guru/blob/main/proxyapp/main_run.go#L682)). The cause is wiring, not safety. Tracked in #1104.
- **Caller-pays comment, frozen counters, `/stats` store block.** The comment in `limits.go` still calls the credential "the OPERATOR's". Hosted does not export the frozen `dropped` and `repaired` counters ([promexport.go:488](https://github.com/rossoctl/context-guru/blob/main/proxy/promexport.go#L488)) or the `/stats` store block. Tracked in #1116.
- **Client compaction settings.** Local reads the Claude Code, Bob and Codex settings ([compactioncfg.go:153](https://github.com/rossoctl/context-guru/blob/main/proxy/compactioncfg.go#L153), [claudecompact.go:11](https://github.com/rossoctl/context-guru/blob/main/proxyapp/claudecompact.go#L11)). Hosted uses its table. Kept for now: a tenant has no way to supply these settings. A per-account setting would change behavior and needs the owner's decision.
