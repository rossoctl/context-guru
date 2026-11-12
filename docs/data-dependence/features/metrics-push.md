---
name: Metrics push to a central collector (`--metrics-push-url`, #886)
kind: metadata
off: ❌ Off. It is off until the operator sets a collector URL. With no URL it logs one warning and sends nothing. It also needs `--dashboard`, because the figures come from the dashboard's finished request. `--metrics-push=false` turns it off.
metadata_only: 🟡 Works, metadata. **Data leaves the machine.** It sends totals only, every 30 seconds: token counts by tier, dollar amounts, request counts by status class, times, the model and provider names, the component names, the preset, the proxy version, and one random install ID. It sends no message text, no tool output, no session id, no request id, no user name and no host name. The install ID is random and is kept in `machine-id` beside the dashboard database.
everything: 🟡 Works, metadata. The storage level changes nothing, because the push never reads stored content.
order: 290
---

A manager needs the savings of many machines in one place. Totals are enough for that, so the push sends no text. It holds no queue and stores nothing: a push that fails is lost, and the next push carries the new totals. The collector URL has no default, so no machine sends anything until its operator names a collector. The URL must use https. If the collector asks for a token, it travels in a header and is never logged. The central stack asks for none today (rossoctl/context-guru#1130). Decided by: not yet. **Needs confirmation from the person who owns this row before merge** (rossoctl/context-guru#974).
