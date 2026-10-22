# History tab: per-project usage, and the cross-project waste list

Status: PLAN, awaiting approval. Supersedes the machine-wide-tiles decision taken in the
previous iteration, which the user reported as broken ("nothing changes when I select a project").

## What this screen is for

One sentence, because every decision below follows from it:

> **Show the user which installed capabilities they have actually used in their own work, and
> which have never once been used — because the latter are pure waste.**

No dollar figures. They are not recoverable from this data (a transcript records what a request
spent in total, never which tools it declared) and inventing one here would be the single most
damaging thing this page could do. The finding is categorical, and it is strong enough without
money: *installed, carried in the prompt on every request, never invoked in forty days.*

## The two scopes, and why they differ

| Scope | Question it answers | Claim strength |
|---|---|---|
| **One project** | What did I use *here*, and what did I not use *here*? | Descriptive. "Unused here" is NOT waste — a skill used in another project is being used correctly. |
| **All projects** | What have I never used *anywhere*? | **A finding.** Installed, carried on every request, never once invoked. This is the waste list. |

This asymmetry is the design. The cross-project view deliberately shows ONLY the never-used-
anywhere set, because a pooled "used" list is just the union of the per-project ones and tells the
reader nothing they cannot get by selecting a project.

## Three defects this fixes

Each was reported against the running build and each is reproduced below from the live store.

### 1. Per-project numbers are wrong (session bleed)

`tool_uses` is keyed `(tenant, session, name, skill)` and has **no project column**. The used list
is therefore built by selecting whole sessions that touched the project:

```sql
WHERE u.session_id IN (SELECT session_id FROM requests WHERE project = ?)
```

Measured on the live store: **8 sessions span more than one folder, and one spans 14.** Selecting
`/private/tmp/cgzip` — 6 requests — returns that session's whole 2,626-request tool list. Every
folder a session touched reports nearly the same numbers, which is exactly the symptom reported.

The information is not merely mis-queried; for live rows it was never stored. For imported rows it
IS recoverable: `TranscriptTurn.Project` already carries the per-turn `cwd` and the importer
discards it.

### 2. The dropdown lists directories, not projects

40 entries, of which ~5 are projects. The rest are subdirectories of real repos
(`notes/server`, `ai-native-company/docs/adr`, `context-guru/docs/superpowers/specs`) and 18
throwaway `/private/tmp/*` folders. Each is a distinct `cwd` Claude Code was launched in.

### 3. The tiles do not respond to the picker

Deliberate in the previous iteration — scoped to avoid calling a skill unused when it is used
elsewhere — but identical numbers across every project reads as a broken page, and did.

## Design

### Data model: `project` on `tool_uses`

```sql
ALTER TABLE tool_uses ADD COLUMN project TEXT NOT NULL DEFAULT '';
```

Additive, via `additiveColumns` in `dash/schema.go` — the route `project` and `source` took on
`requests`, and for the same reason: a `schemaVersion` bump renames the database aside and
discards every row, which would throw away the 5,379 rows just imported.

**The primary key must change** to `(tenant_id, session_id, project, name, skill)`. Without
`project` in the key, one session working in two folders collapses into a single row and the
column records whichever folder was written last. SQLite cannot add a column to a primary key, so
this is a table rebuild: create, copy, drop, rename, inside one transaction. Existing rows copy
with `project = ''`, which reads as "folder unknown" — correct for live rows written before the
column existed, and NOT the same as "no folder".

| Row source | How `project` is filled |
|---|---|
| Imported | `TranscriptTurn.Project`, already per-turn. Full fidelity. |
| Live, new | The request's own project, available on the event. |
| Live, pre-existing | `''`. Unknown, and reported as unknown rather than guessed. |

### Project roots: ask Claude Code, which has already decided

**Claude Code groups its own sessions by project.** Every transcript lives under
`~/.claude/projects/<dir>/`, one directory per project, and sessions that worked in a project's
subfolders are filed together. Measured: 10 such directories hold every session on this machine,
and the `-Users-vitalyz-Research-notes` directory contains `notes`, `notes/server`,
`notes/ops/compose` and `notes/ops/backup` — folded exactly as wanted, by Claude Code, with no
inference from us.

This replaces an earlier git-root proposal, which was wrong twice over: it read the filesystem as
it is today (so a deleted project would vanish, when its history is precisely what the user wants
kept), and it answered a question Claude Code had already answered better.

**The rule.** For each transcript directory, the project is the `cwd` recorded on the most turns
in it. Every session in that directory belongs to that project.

Two things that rule gets right, and one measured non-obvious consequence:

- **The directory NAME is never decoded.** `-Users-vitalyz-OpenSource-context-guru` is ambiguous:
  a literal `-` in a folder name encodes identically to a separator, so decoding it yields
  `/Users/vitalyz/OpenSource/context/guru`. The recorded `cwd` inside the file is unambiguous and
  is used instead.
- **Dominant, not shallowest.** `/private/tmp` has fewer path segments than any real project, so
  a shallowest-path rule resolved every project to `/private/tmp`. Measured and rejected.
- **A session that wanders out of its project keeps its project.** Sessions do work in
  `/private/tmp` mid-flight; measured, one project's sessions touched 13 outside folders. Those
  turns are attributed to the project whose session they belong to, because that is whose work it
  was. This is a deliberate choice, not an artefact: the alternative files a scratch directory as
  a project of its own and splits one piece of work across two rows.

Two transcript directories can resolve to the same project (measured: two both resolve to
`OpenSource/context-guru`). They merge, which is correct.

**Projects that no longer exist on disk are listed, with their history intact.** Nothing in this
rule touches the filesystem — it reads only the recorded statistics — which is what the user asked
for: *"If it deleted now and we still have statistics it's fine, we need only statistics to
understand what skills tools waste money."*

### Where the project is stored

Resolved ONCE, at import, and stored on the row — unlike the git-root proposal, which had to be
resolved per query because `git init` could change the answer. The transcript grouping cannot
change retroactively: the directory a session was filed under is a fact about the past.

| Row source | How `project` is filled |
|---|---|
| Imported | The transcript directory's dominant `cwd`. Full fidelity, every turn. |
| Live, new | The request's own project. A live session has one directory, so no grouping needed. |
| Live, pre-existing | `''`. Unknown, reported as unknown, never guessed. |

### Low-traffic folders: listed, never hidden

No threshold. Measured, the rule already reduces 40 recorded directories to 10 projects, so the
dropdown is short without one. A project with 28 turns is a project the reader worked in, and
dropping it means a dropdown whose contents cannot be reconciled with what they remember.

## The UI

### Picker

```
Project  [ all projects (waste only)  ▾ ]   What was never used in any project.
         ────────────────────────────
         all projects (waste only)
         OpenSource/context-guru      7,156 turns
         Projects/context-guru        4,912 turns
         Research/notes               2,916 turns
         /Users/vitalyz                 694 turns
         Projects/ai-native-company     227 turns
         Projects/cg                    196 turns
         /private/tmp/sotest             70 turns
         /private/tmp/dtest              56 turns
         /private/tmp/cg-probe           28 turns
```

The default label says what the pooled view IS, because "all projects" alone implies a pooled
average and this view is not one. Every project Claude Code ever recorded is listed, including
ones since deleted from disk — the statistics are the point, not the current filesystem.

### All-projects view: the waste list

One claim, stated once, with nothing to distract from it.

```
  ┌─────────────────────────────────────────────────────────┐
  │  40 of 46 installed skills have never been used         │
  │  in any project, in 40 days of history.                 │
  │                                                         │
  │  Each is carried in the prompt on every request.        │
  └─────────────────────────────────────────────────────────┘

  Never used anywhere                              40 skills
  ─────────────────────────────────────────────────────────
  superpowers                                     12 skills
    ├ condition-based-waiting          never
    ├ defense-in-depth                 never
    └ ... 10 more
  context-guru-connect                             6 skills
  ...

  Used somewhere (6)                                  ▸ fold
```

- **Grouped by owner**, because removal operates there: a plugin's skills go together, so twelve
  rows from one plugin are one decision.
- **The used set is present but folded.** It is the control that makes the headline checkable — a
  reader who cannot see what DID get used cannot judge the claim about what did not.
- No cost column anywhere, and a line saying why.

### Per-project view

```
  Research/notes · 1,067 requests · 4 folders folded in

  ┌──────────────┐ ┌──────────────┐ ┌──────────────┐
  │ 3 of 46      │ │ 43           │ │ 38 days      │
  │ used here    │ │ not used here│ │ of history   │
  │ 6 anywhere   │ │ 40 never any-│ │ 31 measured  │
  │              │ │ where        │ │ 673 imported │
  └──────────────┘ └──────────────┘ └──────────────┘

  Used in this project (3 skills, 9 tools)
  ──────────────────────────────────────────────────────
  superpowers:brainstorming    skill    4 calls   9 Sep – 5 Oct
  Bash                         tool   465 calls   9 Sep – 18 Oct
  ...

  Not used in this project (43)                       ▸ fold
    40 of these were never used in ANY project — see all projects
     3 are used in other projects, which is not waste
```

**The second line under "not used here" is the one that keeps this view honest.** Without it, 43
reads as a waste figure, when 3 of those skills are being used correctly elsewhere. With it, the
reader is pointed at the pooled view for the real finding.

Every tile carries both scopes: the project figure as the headline, the machine figure beneath it.
That is what makes the picker feel responsive without the numbers becoming a false accusation.

## Verification

TDD per the project's rules; each test seen to fail for its intended reason, with a mutation check
wherever a fixture could pass vacuously.

1. **Session bleed is gone.** A session working in two projects, invoking skill A in one and B in
   the other: selecting the first returns A and not B. *Mutation: restore the `session_id IN (...)`
   query and this must fail.* This is the reported bug and the test that owns it.
2. **Subdirectories fold.** Two sessions in one transcript directory, one working in
   `<proj>` and one in `<proj>/docs/x`, both report under `<proj>`.
3. **A deleted project is still listed** with its history. *Mutation: resolve the root by
   checking `.git` on disk and this must fail, because the fixture's directory does not exist.*
4. **The directory name is never decoded.** A project whose folder name contains a literal `-`
   resolves to its real path, not to one with the hyphen turned into a separator.
5. **Dominant cwd, not shallowest.** A session whose turns are mostly in `<proj>` but which also
   touched `/private/tmp` resolves to `<proj>`.
6. **The pooled view shows only never-used-anywhere.** A skill used in exactly one project must
   NOT appear in the pooled waste list. *Mutation: scope `calledAnywhere` to the filter and this
   must fail.*
7. **Per-project "not used here" states the machine-wide split**, so 43 is never readable as 43
   wasted.
8. **The rebuild preserves every row.** Seed `tool_uses`, migrate, assert the counts and the
   call totals are unchanged and that old rows carry `project = ''`.
9. **Re-import stays idempotent** with the project column in the key — the existing
   `TestReimportingDoesNotInflateInvocationCounts`, extended to a multi-folder session.
10. **No cost anywhere in the view.** The existing `TestTheHistoryViewComputesNoCost` holds.
11. **Against the live store**, after migrating: `/private/tmp/cgzip` reports its own 6 requests'
   invocations and not the 2,626 of the session that passed through it.

## Sequence

| # | Step | Why this order |
|---|---|---|
| 1 | `project` column + PK rebuild, with the preservation test | Everything else needs the column |
| 2 | Importer writes it; re-import the 5,379 rows | The bulk of the history; proves the column end to end |
| 3 | Live capture writes it | New traffic stops accruing the defect |
| 4 | Query by project, not by session | The reported bug, now fixable |
| 5 | Project resolved from the transcript grouping + the picker | The dropdown |
| 6 | The two views | What the reader actually sees |

Steps 1–4 are the correctness fix and are worth landing on their own; 5–6 are the presentation.

## Out of scope

- **Dollar figures per project.** Not recoverable; see the top of this document.
- **Hiding low-traffic folders.** A dropdown the reader cannot reconcile with their own memory is
  worse than a long one.
- **Reading the filesystem to identify a project.** A deleted project's statistics are exactly
  what this page is for; a rule that depends on today's disk would discard them.
- **Decoding the transcript directory name.** Ambiguous for any folder containing a literal `-`.
