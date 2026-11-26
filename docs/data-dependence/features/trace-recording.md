---
name: **Trace recording** (`--record`, `--record-otlp`)
kind: content
off: ❌ Off
metadata_only: ❌ **Cannot work.** A trace without bodies cannot be replayed.
everything: ✅ Works, only for accounts that opted in. Off for every account until its owner turns it on.
order: 280
---

It records the full request and response of each call, redacted, so a replay tool can send the real prompts again and reproduce real prefix-cache behaviour. It is all content, so it is a deliberate exception to the metadata-only rule. Decided by David Amid, #649 (2026-11-01).

Request headers are written by allowlist (#1466). Only names known to carry no credential are kept, such as content-type, anthropic-version and the client's session ids. Every other header is dropped, so a credential under an unexpected name never reaches the file. This lowers what the feature stores. It adds no new data dependence.
