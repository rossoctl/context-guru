// Package dash is context-guru's persistent observability layer: the durable
// per-request store behind the dashboard, the off-hot-path capture pipeline that
// fills it, the JSON/SSE API the UI reads, and the embedded single-file UI.
//
// Layering, and why it is boring on purpose:
//
//   - metrics.Aggregator stays the fast in-process counter behind /stats. dash
//     never replaces it and never changes its shape — the benchmark harnesses
//     (deploy/harbor/*.py) parse that payload.
//   - Capture is strictly out of band. A request handler builds one Event and
//     hands it to a buffered channel; when the channel is full the event is
//     DROPPED and counted. Observability can never add latency to, or fail, a
//     request — the property gateway gets right and the one worth keeping.
//   - One writer goroutine owns the database. It batches inserts in a
//     transaction and fans a summary row out to SSE clients. Nothing else writes.
//   - Percentages are derived at read time; COST is computed at write time, so
//     history does not silently reprice when a model's published rate changes.
//   - No rollup tables. Time series are bucketed in SQL at query time
//     (ts/bucket*bucket GROUP BY 1). SQLite handles millions of rows; a
//     pre-aggregation layer is the speculative complexity to skip until a query
//     is measurably slow.
//
// The driver is modernc.org/sqlite (pure Go), so a dashboard build needs no C
// toolchain beyond the one tree-sitter already forces.
package dash

import (
	"database/sql"
	"fmt"
	"strings"
)

// schemaVersion is bumped whenever the DDL below changes incompatibly. On a
// mismatch Open PRESERVES the old file (renamed with its version suffix) and
// starts a fresh database: the METRICS are a derived view, so discarding them beats
// refusing to boot, and keeping the file beats deleting a user's data.
//
// That reasoning does NOT extend to the whole file any more. archived_sessions and
// tenant_spend hold facts nothing can recompute — the cold-storage index and the
// monthly spend rollup — so Open carries them across from the preserved file (see
// carryNonDerived in store.go, and the comments on those two tables below). Adding
// another table that traffic cannot rebuild means adding it to nonDerivedTables.
const schemaVersion = 6

// ddl is the whole schema. Timestamps are epoch MILLISECONDS everywhere — never
// a formatted locale string, which cannot be range-queried, sorted portably, or
// bucketed (gateway's mistake).
const ddl = `
CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

-- One row per proxied request. Cost columns are USD, priced at write time from
-- the model's rates; NULL-equivalent (0) with token_accounting<>'complete' means
-- "we could not price this", which the UI must render as unknown, not as free.
CREATE TABLE IF NOT EXISTS requests (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT,
  ts                 INTEGER NOT NULL,          -- epoch ms
  -- Owning tenant; '' in single-tenant deployments. EVERY dashboard query filters
  -- on this, forced from the authenticated principal rather than merged from the
  -- request, so a crafted ?tenant= cannot widen a view.
  tenant_id          TEXT    NOT NULL DEFAULT '',
  session_id         TEXT    NOT NULL DEFAULT '',
  model              TEXT    NOT NULL DEFAULT '',
  provider           TEXT    NOT NULL DEFAULT '',
  agent              TEXT    NOT NULL DEFAULT '', -- client user-agent family (claude-code, codex, …)
  preset             TEXT    NOT NULL DEFAULT '',
  mode               TEXT    NOT NULL DEFAULT '', -- operating mode (active|bypass|observe)
  route              TEXT    NOT NULL DEFAULT '',
  status             INTEGER NOT NULL DEFAULT 0,  -- upstream HTTP status (0 = no upstream)
  bypassed           INTEGER NOT NULL DEFAULT 0,
  cache_aware        INTEGER NOT NULL DEFAULT 0,
  messages           INTEGER NOT NULL DEFAULT 0,
  tokens_before      INTEGER NOT NULL DEFAULT 0,
  tokens_after       INTEGER NOT NULL DEFAULT 0,
  attempted_tokens   INTEGER NOT NULL DEFAULT 0, -- denominator: what we were allowed to compact
  frozen_tokens      INTEGER NOT NULL DEFAULT 0, -- cost of cache safety: what we deliberately did not touch
  saved_unique       INTEGER NOT NULL DEFAULT 0, -- this request's NEW (not re-sent) savings
  fresh_input        INTEGER NOT NULL DEFAULT 0,
  cache_read         INTEGER NOT NULL DEFAULT 0,
  cache_write        INTEGER NOT NULL DEFAULT 0,
  output_tokens      INTEGER NOT NULL DEFAULT 0,
  cost_usd           REAL    NOT NULL DEFAULT 0,
  baseline_cost_usd  REAL    NOT NULL DEFAULT 0, -- what the same request would have cost uncompacted
  cg_llm_cost_usd    REAL    NOT NULL DEFAULT 0, -- context-guru's OWN model spend attributable here
  -- What the PROVIDER's prompt cache saved on this request: its cache-read tokens priced
  -- at the fresh rate, minus what they were billed. A measurement of their mechanism, kept
  -- because it is what collapses when a pipeline rewrites deep history. Never reported as
  -- our saving.
  cache_saved_usd    REAL    NOT NULL DEFAULT 0,
  -- What OUR volatile-tail split saved: nonzero only where the split ran on the request, the
  -- provider read at least that much from cache while writing less, and the snapshot had MOVED
  -- since this session's previous request (Event.TailChanged) — the turn that would otherwise
  -- have missed. Priced against a cache MISS (creation rate), not fresh input.
  -- See Event.cachesplitSavedUSD, which is the authority on the condition; the
  -- session's-first-request test it once used was REPLACED because it reported ~$0, and three
  -- descriptions of this column went on stating it for a while after.
  cachesplit_saved_usd REAL  NOT NULL DEFAULT 0,
  -- The size of the prefix half cachesplit moved the breakpoint onto: the numerator of the
  -- column above, stored so a dollar figure can be checked against a token count.
  split_stable_tokens INTEGER NOT NULL DEFAULT 0,
  -- The volatile half's identity on this request. Compared against the same session's previous
  -- request to decide whether the snapshot MOVED, which is the turn the split is worth
  -- anything on. Stored so the figure survives a restart and can be recomputed from the table.
  split_tail_hash    INTEGER NOT NULL DEFAULT 0,
  -- The idle keep-alive's three columns. keepalive marks a row that IS a ping — a request
  -- context-guru sent on its own initiative, with no agent behind it — so every query that
  -- reports agent traffic can exclude it and the ledger can price it. It is the audit trail
  -- for a mechanism that spends the caller's money while they are away from the keyboard,
  -- and without it that spend would be indistinguishable from the agent's own.
  -- The part of cache_write the provider billed at the ONE-HOUR tier (a subset of it, from
  -- usage.cache_creation.ephemeral_1h_input_tokens). Two jobs: cost_usd needs it, because a 1h
  -- write is 2.0x base input where a 5m write is 1.25x and pricing one as the other understates
  -- the row by 0.75x of its written prefix; and it is the only evidence that a requested
  -- ttl:"1h" was honoured rather than silently downgraded, which on this gateway depends on the
  -- model (granted on aws/claude-haiku-4-5, refused on aws/claude-sonnet-5).
  cache_write_1h     INTEGER NOT NULL DEFAULT 0,
  keepalive          INTEGER NOT NULL DEFAULT 0,
  -- On a REAL request: how many pings preceded it during the idle span it just ended. It is
  -- what makes the saving attributable — a request that hits after a 20-minute gap could
  -- have been rescued by our ping or by any other session touching the same content, and
  -- only this column tells the two apart.
  keepalive_pings    INTEGER NOT NULL DEFAULT 0,
  -- What the ping avoided on THIS row: its cache-read tokens priced at the difference
  -- between the creation tier it would have paid and the read tier it did pay. Nonzero only
  -- where a ping preceded it, the idle gap exceeded the provider lifetime, and the provider
  -- read more than it wrote. Priced against a cache MISS, not fresh input, for the same
  -- reason cachesplit_saved_usd is: those tokens carry cache_control, so a miss bills them
  -- as creation at 1.25x rather than at 1.0x.
  keepalive_saved_usd REAL   NOT NULL DEFAULT 0,
  -- What the declaration filter stopped carrying on this request: the token weight of the
  -- tools[] elements it removed under the account's opt-in list. Written ONLY by the filter
  -- itself (apply.Trace.FilteredDeclTokens), never derived from the tool inventory — the
  -- inventory reads the PRISTINE body, so a figure computed from it would keep reporting a
  -- saving for a request the filter had declined to touch. 0 therefore means "nothing was
  -- removed here", which is the honest reading on every row that predates the feature.
  filtered_decl_tokens INTEGER NOT NULL DEFAULT 0,
  -- Which prompt-cache TTL TIER this request asked for: 'ephemeral_5m', 'ephemeral_1h', or ''
  -- for a body carrying no cache_control at all. The pipeline always computed this and threw
  -- it away, so nothing could group by it and a 1h prompt was priced at the 5m write rate.
  -- '' is a THIRD answer, never folded into 5m: an uncached request is not a 5-minute one.
  cache_ttl          TEXT    NOT NULL DEFAULT '',
  -- Whether the client's stream was BUFFERED instead of streamed through, and the measured
  -- time to its first byte. A third of streamed responses are buffered and those wait ~21 s
  -- longer to start — far more user-visible delay than the pipeline itself adds, and until
  -- now visible only as a counter on /stats that reset on restart. ttfb_ms is 0 on a
  -- non-streamed response; on a BUFFERED one it is the TOTAL response time rather than a
  -- first-byte time, because nothing reaches the client until the whole response has arrived.
  -- Never average the two populations together.
  sse_buffered       INTEGER NOT NULL DEFAULT 0,
  ttfb_ms            REAL    NOT NULL DEFAULT 0,
  cg_latency_ms      REAL    NOT NULL DEFAULT 0,
  upstream_ms        REAL    NOT NULL DEFAULT 0,
  expands            INTEGER NOT NULL DEFAULT 0,
  expand_tokens      INTEGER NOT NULL DEFAULT 0, -- restoration: content we offloaded and had to serve back
  reverts            INTEGER NOT NULL DEFAULT 0,
  token_accounting   TEXT    NOT NULL DEFAULT 'missing', -- complete|partial|missing
  cache_miss_reason  TEXT    NOT NULL DEFAULT '',        -- cold_start|ttl_expiry|prefix_change|unknown|hit
  uncompressed_reason TEXT   NOT NULL DEFAULT '',        -- why we did not compact: '' = we did
  -- Request metadata: the knobs the CLIENT chose, normalized across the Anthropic and
  -- OpenAI dialects at the capture site, plus the provider's terminal stop reason. Real
  -- columns rather than one JSON blob because every one of them is GROUPED BY in an
  -- aggregate or shown on the row — see the Meta type in event.go for the full argument.
  -- Every TEXT column here is client-supplied and is passed through metaEnum BEFORE the
  -- insert (dash/redact.go), never on read.
  reasoning_effort   TEXT    NOT NULL DEFAULT '',        -- output_config.effort | reasoning_effort
  thinking_mode      TEXT    NOT NULL DEFAULT '',        -- thinking.type: adaptive|enabled|disabled
  thinking_budget    INTEGER NOT NULL DEFAULT 0,         -- thinking.budget_tokens
  -- NULLABLE, unlike every other column in this table: "unset" and "0" are different
  -- facts for a sampling parameter, and 0 is a legitimate value, so no sentinel can mean
  -- absent. NULL is the only honest encoding.
  temperature        REAL,
  top_p              REAL,
  max_tokens         INTEGER NOT NULL DEFAULT 0,
  stream             INTEGER NOT NULL DEFAULT 0,
  tool_choice        TEXT    NOT NULL DEFAULT '',        -- auto|any|none|required|tool
  tools              INTEGER NOT NULL DEFAULT 0,         -- declared tool count
  system_blocks      INTEGER NOT NULL DEFAULT 0,         -- blocks in the top-level system array
  -- Prompt-cache breakpoints on arrival, BY LOCATION. The tools and system arrays render
  -- ahead of messages, so where a breakpoint sits decides how much prefix it protects; a
  -- single total cannot distinguish good placement from bad. Sum = the provider's cap of 4.
  cache_bp_system    INTEGER NOT NULL DEFAULT 0,
  cache_bp_tools     INTEGER NOT NULL DEFAULT 0,
  cache_bp_messages  INTEGER NOT NULL DEFAULT 0,
  cache_bp_blocks    INTEGER NOT NULL DEFAULT 0,
  stop_reason        TEXT    NOT NULL DEFAULT ''         -- end_turn|max_tokens|tool_use|refusal|…
);
CREATE INDEX IF NOT EXISTS idx_requests_ts       ON requests(ts DESC);
CREATE INDEX IF NOT EXISTS idx_requests_session  ON requests(session_id, ts);
CREATE INDEX IF NOT EXISTS idx_requests_model    ON requests(model, ts);
-- Tenant-leading indexes: in a hosted deployment every query is tenant-scoped, so
-- a tenant-leading index is the difference between a seek and a full scan once the
-- table holds every user's traffic.
-- archived_sessions is NOT DERIVED: it is the only index of what now lives in cold
-- storage, so a schema bump carries it across rather than discarding it
-- (nonDerivedTables in store.go). It is small
-- (one row per session), permanent, and deliberately local: the dashboard must be
-- able to show a user their whole history — including the archived part — without
-- Box being reachable, and only fetch an object when someone actually opens it.
--
-- Two paths, not one, because content and metrics leave at different times: a
-- session's transcripts are the bulk of the bytes and go early, while its numbers
-- are small and stay queryable locally for much longer.
CREATE TABLE IF NOT EXISTS archived_sessions (
  session_id    TEXT PRIMARY KEY,
  tenant_id     TEXT    NOT NULL DEFAULT '',
  first_ts      INTEGER NOT NULL DEFAULT 0,
  last_ts       INTEGER NOT NULL DEFAULT 0,
  requests      INTEGER NOT NULL DEFAULT 0,
  content_path  TEXT    NOT NULL DEFAULT '',  -- transcripts only; metrics still local
  content_bytes INTEGER NOT NULL DEFAULT 0,
  full_path     TEXT    NOT NULL DEFAULT '',  -- whole session; local rows deleted
  full_bytes    INTEGER NOT NULL DEFAULT 0,
  archived_at   INTEGER NOT NULL DEFAULT 0,
  remote        TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_archived_tenant ON archived_sessions(tenant_id, last_ts DESC);
CREATE INDEX IF NOT EXISTS idx_requests_tenant   ON requests(tenant_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_requests_tenant_session ON requests(tenant_id, session_id, ts);

-- One row per component per request: the answer to "which components earn their
-- place". saved_gross is what the component removed THIS turn (re-counted every
-- turn the agent re-sends the transcript); saved_unique counts each distinct
-- compaction once.
CREATE TABLE IF NOT EXISTS request_components (
  request_id   INTEGER NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
  component    TEXT    NOT NULL,
  kind         TEXT    NOT NULL DEFAULT '',
  acted        INTEGER NOT NULL DEFAULT 0,
  mutated      INTEGER NOT NULL DEFAULT 0,
  reverted     INTEGER NOT NULL DEFAULT 0,
  skipped      INTEGER NOT NULL DEFAULT 0,
  saved_gross  INTEGER NOT NULL DEFAULT 0,
  saved_unique INTEGER NOT NULL DEFAULT 0,
  -- This component's share of the request's baseline delta, in DOLLARS, priced at write
  -- time from the request's own model and the cache tier the request itself paid (see
  -- Event.Price). Stored rather than derived on read because the rate has to be the one
  -- that was in force, and because the UI was otherwise left improvising a dollar value
  -- from hardcoded rates.
  saved_usd    REAL    NOT NULL DEFAULT 0,
  duration_ms  REAL    NOT NULL DEFAULT 0,
  err          TEXT    NOT NULL DEFAULT '',
  -- Why this component turned candidates away, as {"gate_name":count} — the only answer
  -- to "nothing happened, why?", and the answer a user of a non-Claude agent needs most.
  -- JSON rather than a row per gate: it is read whole, never joined on, and json_each
  -- aggregates it in SQL when the components view needs totals.
  gates        TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_rc_request ON request_components(request_id);
CREATE INDEX IF NOT EXISTS idx_rc_comp    ON request_components(component);

-- Before/after text of each rewritten message — the diff view's data. Stored
-- gzip-compressed and size-capped, and skippable entirely (content capture is
-- opt-out). Redaction happens BEFORE the insert, never on read.
-- components names WHICH components rewrote this message, comma-separated, IN THE
-- ORDER THEY TOUCHED IT. A list rather than a single id because several components
-- routinely rewrite the same message in sequence, and the diff shown is their
-- cumulative result — one id would have to pick a winner and be wrong. A reverted
-- component is absent: the pipeline only records surviving changes.
--
-- Comma-joined text rather than a join table: component names are short lowercase
-- identifiers, the list is read only when a human opens one request's diff, and a
-- second table would be a row per component per message on the write path.
CREATE TABLE IF NOT EXISTS request_content (
  request_id    INTEGER NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
  seq           INTEGER NOT NULL,
  path          TEXT    NOT NULL DEFAULT '',
  before_tokens INTEGER NOT NULL DEFAULT 0,
  after_tokens  INTEGER NOT NULL DEFAULT 0,
  before_gz     BLOB,
  after_gz      BLOB,
  components    TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_content_request ON request_content(request_id);

-- Monthly per-tenant spend, incremented as rows are written and NEVER deleted by
-- retention — nor discarded by a schema bump, which carries it across
-- (nonDerivedTables in store.go): this table is the only copy of the figure, so
-- losing it resets every tenant's month-to-date spend to zero.
-- The one rollup table in this schema (see the package comment), and it
-- earns its place for a reason that is not performance: a SUM over the requests table
-- silently SHRINKS as retention evicts history, so a tenant that generates enough
-- traffic to evict its own oldest rows would watch its month-to-date spend fall. The
-- figure is reported, never enforced — there is no spend cap — and a reported number
-- that goes down while money goes up is the kind of number nobody trusts again.
CREATE TABLE IF NOT EXISTS tenant_spend (
  tenant_id TEXT NOT NULL,
  month     TEXT NOT NULL,             -- 'YYYY-MM', UTC, matching the calendar invoice
  usd       REAL NOT NULL DEFAULT 0,   -- cost_usd + cg_llm_cost_usd of every row written
  PRIMARY KEY (tenant_id, month)
);

-- Ingested benchmark runs (deploy/harbor's summary.json + rows-*.json).
CREATE TABLE IF NOT EXISTS bench_runs (
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  name     TEXT NOT NULL UNIQUE,   -- run directory name, so re-ingesting replaces
  ts       INTEGER NOT NULL,
  dataset  TEXT NOT NULL DEFAULT '',
  model    TEXT NOT NULL DEFAULT '',
  summary  TEXT NOT NULL DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS bench_tasks (
  run_id            INTEGER NOT NULL REFERENCES bench_runs(id) ON DELETE CASCADE,
  arm               TEXT    NOT NULL,   -- config name: off|codesmart|headroom|rtk|…
  task              TEXT    NOT NULL,
  reward            REAL    NOT NULL DEFAULT 0,
  steps             INTEGER NOT NULL DEFAULT 0,
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read        INTEGER NOT NULL DEFAULT 0,
  cache_write       INTEGER NOT NULL DEFAULT 0,
  fresh_input       INTEGER NOT NULL DEFAULT 0,
  cost_usd          REAL    NOT NULL DEFAULT 0,
  norm_cost_usd     REAL    NOT NULL DEFAULT 0,
  wall_s            REAL    NOT NULL DEFAULT 0,
  exception         INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_bt_run ON bench_tasks(run_id, arm);
`

// additiveDDL holds tables that can be created on an EXISTING database without touching
// what is already there — so adding one does not need a schemaVersion bump.
//
// The bump is otherwise unavoidable and expensive: a version mismatch renames the file aside
// and starts fresh, discarding every requests / request_components / request_content row
// (only archived_sessions and tenant_spend are carried). For a purely new table that cost
// buys nothing, so this runs unconditionally on every open and is idempotent.
//
// STRICTLY new tables only. Anything that alters an existing table — a new column, a changed
// index, a different type — still requires a version bump, because an old file would
// otherwise be read with the wrong shape.
const additiveDDL = `
-- One row per LLM call an expensive component made, with what it cost and what it bought.
--
-- Separate from request_components because the relationship is one-to-many and the columns
-- are about a MODEL CALL, not a component run: which candidate it looked at, the tokens and
-- cache tiers of the call itself, its own latency, whether the result was accepted, and what
-- the economic gate concluded (including when that conclusion was overridden). Before this,
-- the whole record of an extraction call was one dollar figure per request, priced at the
-- agent model's rate.
--
-- before_gz/after_gz are transcript content and are written ONLY with the same per-account
-- capture consent that governs the diff view.
CREATE TABLE IF NOT EXISTS extraction_calls (
  -- FK + cascade, matching request_components and request_content: retention, session
  -- eviction, cold-storage migration and a manager purge all delete from requests, and
  -- every one of them would otherwise leave orphans here (foreign_keys is on, see dsn()).
  request_id   INTEGER NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
  seq          INTEGER NOT NULL,
  component    TEXT NOT NULL,
  tenant_id    TEXT NOT NULL DEFAULT '',
  session_id   TEXT NOT NULL DEFAULT '',
  ts           INTEGER NOT NULL,
  cold         INTEGER NOT NULL DEFAULT 0,
  escalated    INTEGER NOT NULL DEFAULT 0,
  aggressiveness TEXT NOT NULL DEFAULT '',
  strategy     TEXT NOT NULL DEFAULT '',
  model        TEXT NOT NULL DEFAULT '',
  candidate_tokens INTEGER NOT NULL DEFAULT 0,
  saved_tokens INTEGER NOT NULL DEFAULT 0,
  prompt_tokens INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read   INTEGER NOT NULL DEFAULT 0,
  cache_write  INTEGER NOT NULL DEFAULT 0,
  cost_usd     REAL NOT NULL DEFAULT 0,
  latency_ms   REAL NOT NULL DEFAULT 0,
  accepted     INTEGER NOT NULL DEFAULT 0,
  gate_reason  TEXT NOT NULL DEFAULT '',
  rejection    TEXT NOT NULL DEFAULT '',
  summary      TEXT NOT NULL DEFAULT '',
  before_gz    BLOB,
  after_gz     BLOB,
  PRIMARY KEY (request_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_xcalls_request ON extraction_calls(request_id);
CREATE INDEX IF NOT EXISTS idx_xcalls_tenant_ts ON extraction_calls(tenant_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_xcalls_session ON extraction_calls(session_id, ts);

-- What a session DECLARED it could do: tool, MCP tool, provider-side tool and skill
-- NAMES, each with the token weight of carrying it. The requests table has only a
-- COUNT (requests.tools), which can find a heavy user but cannot name what they carry
-- — so "how much of what you carry do you actually use" had no data behind it.
--
-- Names, never descriptions: a tool/server/skill name is an IDENTIFIER of the user's
-- own configuration, the same class as tool_choice, so these rows are gated on the
-- tenant_id scoping every query already applies and NOT on transcript-capture consent
-- (which would deny the feature to the accounts that declined it, over data that is
-- not their transcript).
--
-- Keyed by SESSION plus a digest of the declaration set, not by request. The set is
-- constant for a whole session unless the client changes it — measured: 34 of 135
-- production sessions ever change it — so a per-request table would be ~45 identical
-- rows x 65 requests per session saying the same thing. A session that changes its set
-- simply gains a second digest, which is also how the change itself becomes visible.
-- ts is when the set was first seen.
--
-- kind='skill_listing' is ONE marker row per set: name is empty, tokens is the prose
-- listing's own cost, and server holds the PARSE STATE ('ok' | 'unknown'). It exists so
-- a listing whose format moved reads as "not known" rather than "no skills declared" —
-- a wrong-but-confident empty inventory is the one output that could later authorise
-- stripping something real.
CREATE TABLE IF NOT EXISTS tool_declarations (
  tenant_id  TEXT    NOT NULL DEFAULT '',
  session_id TEXT    NOT NULL DEFAULT '',
  digest     TEXT    NOT NULL DEFAULT '',   -- identity of the declaration SET
  kind       TEXT    NOT NULL DEFAULT '',   -- tool|mcp_tool|server_tool|skill|skill_listing
  name       TEXT    NOT NULL DEFAULT '',
  server     TEXT    NOT NULL DEFAULT '',   -- MCP server half of mcp__<server>__<tool>
  tokens     INTEGER NOT NULL DEFAULT 0,    -- BPE cost of carrying this declaration
  ts         INTEGER NOT NULL DEFAULT 0,
  -- text_gz is the declaration's own slice of the prompt, gzipped: the tool's JSON
  -- element, the skill's listing entry, the whole listing on the marker row, the whole
  -- system prompt on a kind='system_prompt' row.
  --
  -- This is the ONE column in this table that is CONTENT rather than an identifier, and it
  -- is therefore the one column written only under transcript-capture consent — the
  -- operator's --dashboard-content AND the tenant's own opt-in, the same pair that gates
  -- request_content.before_gz. NULL means "not stored", which is a THIRD state next to
  -- "empty" and "absent": a row written before this column existed, and a row written by
  -- an account that has not opted in, both read NULL, and the report counts them rather
  -- than rendering either as a prompt with nothing in it.
  text_gz    BLOB,
  -- tenant_id LEADS the key. A session id is CLIENT-SUPPLIED, so two accounts can present
  -- the same one (by accident or on purpose), and a key without the tenant makes the second
  -- account's rows collide with the first's: ON CONFLICT DO NOTHING silently discards them,
  -- so one account's inventory disappears and the other's weights stand in for it. The same
  -- shape as archived_sessions.session_id, which is why it is not repeated here.
  PRIMARY KEY (tenant_id, session_id, digest, kind, name)
);
CREATE INDEX IF NOT EXISTS idx_tooldecl_tenant ON tool_declarations(tenant_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_tooldecl_name   ON tool_declarations(name);

-- What a session actually INVOKED: one row per distinct tool name (and, for the Skill
-- tool, per skill — input.skill is the only place a skill invocation is identifiable,
-- since the Skill tool's schema carries no enum). Counted from the LAST tool-using turn
-- of each request, deduped by that turn's fingerprint, because the agent resends the
-- whole transcript every request.
CREATE TABLE IF NOT EXISTS tool_uses (
  tenant_id  TEXT    NOT NULL DEFAULT '',
  session_id TEXT    NOT NULL DEFAULT '',
  name       TEXT    NOT NULL DEFAULT '',
  server     TEXT    NOT NULL DEFAULT '',
  skill      TEXT    NOT NULL DEFAULT '',   -- '' for anything but a Skill call
  calls      INTEGER NOT NULL DEFAULT 0,
  first_ts   INTEGER NOT NULL DEFAULT 0,
  last_ts    INTEGER NOT NULL DEFAULT 0,
  -- tenant_id LEADS the key, as on tool_declarations: without it the upsert below
  -- (calls = calls + excluded.calls) ADDS one account's call counts to another account's
  -- row whenever a session id is shared, which is a cross-account write.
  PRIMARY KEY (tenant_id, session_id, name, skill)
);
CREATE INDEX IF NOT EXISTS idx_tooluses_tenant ON tool_uses(tenant_id, last_ts DESC);

`

// additiveDDL2 is the additive phase that must run AFTER `ddl`, because it references a
// table `ddl` creates. On a fresh database `additiveDDL` runs first of all — before
// `requests` exists — and SQLite resolves a trigger's body at CREATE time, so a trigger
// on `requests` can only be created here. Executed on EVERY open and on both paths
// (fresh and already-at-version), like additiveDDL, and idempotent for the same reason.
const additiveDDL2 = `
-- Lifetime. These two tables are keyed by session rather than by request_id, so they
-- have no FK to hang an ON DELETE CASCADE on — and every deletion path in this package
-- (retention eviction, a disk-pressure sweep, cold-storage migration, a manager's
-- tenant purge) works by deleting from the requests table. This trigger is how the inventory
-- follows: when a session's LAST request row goes, its inventory goes with it. The WHEN
-- clause is one indexed lookup (idx_requests_session) per deleted row, the same order
-- of cost as the cascades already on that path, and it keeps a live session's inventory
-- while a quota trim removes only its oldest rows.
--
-- A trigger, not a table, so this stays inside the additive rule: it creates a new
-- object and alters nothing about the shape of the requests table.
--
-- Every clause is scoped by TENANT as well as by session, because a session id is
-- client-supplied and two accounts can present the same one. Unscoped, the trigger is wrong
-- in both directions: it would delete another account's inventory, and it would DECLINE to
-- delete this account's (the WHEN clause still sees the other account's request rows), so a
-- purged tenant's tool names would outlive its request rows.
CREATE TRIGGER IF NOT EXISTS trg_tool_inventory_gc AFTER DELETE ON requests
WHEN NOT EXISTS (SELECT 1 FROM requests
  WHERE session_id = OLD.session_id AND tenant_id = OLD.tenant_id)
BEGIN
  DELETE FROM tool_declarations
    WHERE session_id = OLD.session_id AND tenant_id = OLD.tenant_id;
  DELETE FROM tool_uses
    WHERE session_id = OLD.session_id AND tenant_id = OLD.tenant_id;
END;
`

// additiveColumns are columns added to an EXISTING table without a version bump.
//
// The same argument as additiveDDL, one step further: `ALTER TABLE … ADD COLUMN` with a
// NOT NULL DEFAULT is non-destructive and instant in SQLite, every read of the column on
// an old row yields the default, and every query that does not name it is unaffected. The
// alternative is a version bump, which renames the file aside and discards every request
// row — on the live service, thousands of them — to gain one column.
//
// So the rule additiveDDL states is narrowed, not broken: a NEW column with a default may
// go here; a column without a default, a changed type, a renamed or dropped column, or a
// changed index still needs a bump, because those cannot be reconciled with an old file.
// Every entry must ALSO be present in `ddl` above, or a fresh database and an upgraded one
// end up with different shapes.
var additiveColumns = []struct{ table, column, ddl string }{
	{"requests", "cache_saved_usd", "REAL NOT NULL DEFAULT 0"},
	{"requests", "cachesplit_saved_usd", "REAL NOT NULL DEFAULT 0"},
	{"requests", "split_stable_tokens", "INTEGER NOT NULL DEFAULT 0"},
	{"requests", "split_tail_hash", "INTEGER NOT NULL DEFAULT 0"},
	{"requests", "filtered_decl_tokens", "INTEGER NOT NULL DEFAULT 0"},
	{"requests", "cache_ttl", "TEXT NOT NULL DEFAULT ''"},
	{"requests", "sse_buffered", "INTEGER NOT NULL DEFAULT 0"},
	{"requests", "ttfb_ms", "REAL NOT NULL DEFAULT 0"},
	{"requests", "cache_write_1h", "INTEGER NOT NULL DEFAULT 0"},
	{"requests", "keepalive", "INTEGER NOT NULL DEFAULT 0"},
	{"requests", "keepalive_pings", "INTEGER NOT NULL DEFAULT 0"},
	{"requests", "keepalive_saved_usd", "REAL NOT NULL DEFAULT 0"},
	{"tool_declarations", "text_gz", "BLOB"},
	{"request_components", "gates", "TEXT NOT NULL DEFAULT ''"},
	{"request_components", "saved_usd", "REAL NOT NULL DEFAULT 0"},
}

// migrate creates the schema and validates its version. A version mismatch is
// reported to the caller, which renames the old file aside and retries — see Open.
func migrate(db *sql.DB) error {
	// Additive tables first, and on every open: an existing database at the current version
	// returns early below, so a new table added to `ddl` would never appear on it.
	if _, err := db.Exec(additiveDDL); err != nil {
		return err
	}
	if err := addColumns(db); err != nil {
		return err
	}
	var have string
	err := db.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&have)
	switch {
	case err == sql.ErrNoRows || isMissingTable(err):
		// Fresh (or pre-meta) database: create everything and stamp the version.
	case err != nil:
		return err
	case have != fmt.Sprint(schemaVersion):
		return &versionMismatch{have: have}
	default:
		// Already at this version: `ddl` is skipped, but the late additive phase still runs.
		_, err = db.Exec(additiveDDL2)
		return err
	}
	if _, err := db.Exec(ddl); err != nil {
		return err
	}
	if _, err := db.Exec(additiveDDL2); err != nil {
		return err
	}
	_, err = db.Exec(`INSERT OR REPLACE INTO meta(key,value) VALUES('schema_version',?)`, fmt.Sprint(schemaVersion))
	return err
}

// addColumns applies additiveColumns idempotently. It probes pragma table_info rather
// than swallowing the "duplicate column name" error, so a genuine failure is still an
// error; a table that does not exist yet is skipped, because `ddl` below will create it
// with the column already in place.
func addColumns(db *sql.DB) error {
	for _, c := range additiveColumns {
		rows, err := db.Query(`SELECT 1 FROM pragma_table_info(?) WHERE name = ?`, c.table, c.column)
		if err != nil {
			return err
		}
		has := rows.Next()
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if has {
			continue
		}
		// No such table yet (fresh database): nothing to alter.
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, c.table).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE ` + c.table + ` ADD COLUMN ` + c.column + ` ` + c.ddl); err != nil {
			return fmt.Errorf("dash: add %s.%s: %w", c.table, c.column, err)
		}
	}
	return nil
}

// versionMismatch signals that the file on disk was written by another schema.
type versionMismatch struct{ have string }

func (e *versionMismatch) Error() string {
	return fmt.Sprintf("dash: database schema version %s, want %d", e.have, schemaVersion)
}

func isMissingTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}
