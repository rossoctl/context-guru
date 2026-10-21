# Permanent e2e harness — follow-up plan (not started)

Status: PLAN. Agreed with the repo owner on 2026-10-21. Starts after PR #578 merges into main.

## Decisions

| Question | Decision |
|---|---|
| Where it lands | Its own PR, rebased onto `main` after #578 merges. #578's diff stays as reviewed. |
| How it runs | Local Make targets only: `make e2e-smoke` (~5 min), `make e2e-full` (~45 min). No CI. |
| Known findings | `e2e/known-findings.txt` ships empty. Every future entry must cite a GitHub issue. |

## Work

1. Rebase `e2e/` (and only `e2e/`) onto main once #578 is in. Drop `docs/superpowers/` review artefacts
   from the PR, or keep only the spec, by the owner's choice at that point.
2. Generalise PR-specific names. The image tag `cg-e2e:pr578` becomes `cg-e2e:<git short sha>`. The
   report title becomes "context-guru e2e: <branch>@<sha>". The mutation round and summary stop naming #578.
3. The KNOWN baseline is the merge base with `main`, not literal `main`, so a feature branch is compared
   with where it started. Once History is on main, the baseline runs the whole tier, not just the Go suite.
4. Empty `known-findings.txt`. `report.py` / `run.sh` reject an entry without `issue:#NNN`, and a test
   pins that rule.
5. Add `make e2e-smoke`, `make e2e-full` and `make e2e-mutate` to the Makefile, plus a short
   `e2e/README.md`: prerequisites (Podman or Docker, ~8 GB disk, a Mac kept on power for the full
   tier), tiers, how to read summary.md, and how to add a known finding.
6. Add `e2e/results/` to the root `.gitignore`, not only `e2e/.gitignore`.
7. The fake-data manifest stays seeded; document how to extend it when a feature adds a capability kind.

## Out of scope

CI wiring (the owner chose local only); any change to product code.
