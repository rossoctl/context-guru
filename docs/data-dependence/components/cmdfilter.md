---
name: cmdfilter
kind: content
where: Store: the filtered text (a speed-up) and the original
off: ✅ Works, no `expand`. Same bytes every turn.
metadata_only: ✅ Works, no `expand`
everything: ✅ Works
decided_by: Osher Elhadad, first commit a91d6f4; memo in #188
order: 140
---

It applies a filter for each known command, and the same output always gives the same text. It keeps the original only so the agent can get dropped lines back.
