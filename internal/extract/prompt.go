package extract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Prompt building. Because the model returns the filtered VALUE (which containment
// then verifies), it must SEE the values — so the prompt shows the actual JSON/text
// (truncated). For very large lists the RLM strategy chunks the body so each chunk is
// shown in full. The rule set is the reference prototype's "select, never summarize, recall-first"
// contract, retargeted from "write a function" to "return the JSON".

// sampleMarker precedes the body in the prompt; tests and the (future) model both
// locate the payload after it.
const sampleMarker = "INPUT (return a smaller value of this same shape):\n"

const rules = `Return ONLY a JSON value (or, for raw text input, the kept text): a SMALLER value
of the SAME shape, selecting only what the agent needs next. Rules, in priority order:
1. RECALL FIRST. When unsure whether a record/field is relevant, KEEP IT.
2. SELECT, NEVER SUMMARIZE. Return whole records/objects/values byte-for-byte. Never
   paraphrase, truncate, round, reformat, or invent values.
3. PRESERVE EXACTLY: ids, numbers, names, paths, timestamps, error messages, stack
   traces — and anything matching the KEEP list.
4. Only drop records that are CLEARLY irrelevant boilerplate, duplicates, or noise.
5. If you cannot identify clearly-irrelevant content, RETURN THE INPUT UNCHANGED.
6. Keep the natural shape and types. Output ONLY the value — no prose, no markdown.`

const example = `EXAMPLE
Goal: "Fix failing test test_auth_expiry; find the relevant hit."
KEEP: ["test_auth_expiry","auth/session.py"]
INPUT: [{"path":"auth/session.py","snippet":"def test_auth_expiry()..."},{"path":"README.md","snippet":"intro"}]
OUTPUT: [{"path":"auth/session.py","snippet":"def test_auth_expiry()..."}]`

func buildPrompt(bodyText, goal string, keepIDs []string) string {
	g := strings.TrimSpace(goal)
	if g == "" {
		g = "(no explicit goal stated)"
	}
	g = clipGoal(g)
	keep := keepIDs
	if len(keep) > 60 {
		keep = keep[:60]
	}
	keepBlock := ""
	if len(keep) > 0 {
		kb, _ := json.Marshal(keep)
		keepBlock = "IDENTIFIERS THE AGENT REFERENCED RECENTLY — keep every record or field\n" +
			"whose value matches any of these, verbatim:\n" + string(kb) + "\n\n"
	}

	// Show the actual value (pretty-printed JSON only if the whole body is a JSON
	// container — never mangle a numeric-prefixed text/file read), truncated.
	sample := bodyText
	if isJSONContainer(bodyText) {
		if v := parseBody(bodyText); !isRawString(v) {
			if b, err := json.MarshalIndent(v, "", "  "); err == nil {
				sample = string(b)
			}
		}
	}
	sample = truncate(sample, sampleChars)

	return "You filter ONE tool output down to only what the agent needs next.\n\n" +
		"WHAT THE AGENT IS DOING NOW (filter toward this):\n" + g + "\n\n" +
		keepBlock + sampleMarker + sample + "\n\n" + rules + "\n\n" + example
}

// codeContract is the shared preamble: the sandbox API and the always-safe
// defaults. Because the program runs inside a function body (top-level assignments
// are locals), OUTPUT may be reassigned as many times as you like — there is no
// "assign once" restriction.
const codeContract = `Write a Starlark program (a safe Python subset) that reduces THIS ONE tool output
(shown in full below) to only what the agent needs next. Be SPECIFIC to the content
you see — target its exact noise, not a generic filter. This is GENERAL: the output
may come from any tool for any agent (coding, tool/API use, research, ops, a task for
a user), and you do not know exactly what the agent does next, so RECALL FIRST — when
unsure whether something matters, keep it. Deleting a line the agent needed makes it
re-run the tool and redo work; keeping a borderline line costs only a few tokens.
Sandbox:
- INPUT (string) holds the FULL tool output, identical to what's shown below.
- OUTPUT (string) starts equal to INPUT; assign it the reduced content. You MAY
  reassign OUTPUT freely (build it up step by step).
- SUMMARY (string) starts empty; OPTIONALLY set it to ONE short line naming the gist
  of what you elided (e.g. "pytest: 3 failed, 710 passed" or "npm install: ok, 42
  pkgs"). It is shown to the agent inline next to the recovery marker. Leave it "" if
  a plain reduction needs no digest.
- Available: the ` + "`json`" + ` module and regex helpers
  re_sub(pattern, repl, s) -> s, re_findall(pattern, s) -> [str],
  re_split(pattern, s) -> [str], re_match(pattern, s) -> bool. RE2 syntax.
- NO imports (no load()), NO I/O, NO network.
STARLARK IS NOT PYTHON. These are the constructs models reach for that the sandbox
REJECTS outright — a rejected program means your whole reply is discarded:
- NO generator expressions. ` + "`any(k in ln for k in ids)`" + ` is a syntax error.
  Write a list comprehension instead: ` + "`any([k in ln for k in ids])`" + `.
- NO f-strings and no %-formatting. Use ` + "`str(x)`" + ` and ` + "`+`" + ` to build strings.
- NO while loops, NO try/except, NO lambda with statements, NO ` + "`global`" + `/` + "`nonlocal`" + `.
- NO set literals, NO ` + "`dict.setdefault`" + `, NO ` + "`sorted(key=...)`" + ` with a lambda closure
  over a mutable local.
- Strings have .strip()/.split()/.startswith()/.endswith()/.replace()/.lower(); lists
  have .append(). Use ` + "`for`" + ` over a list, ` + "`if/elif/else`" + `, and plain function defs.
When in doubt write the dumb, explicit loop — a plain program that runs beats a clever
one that does not.
Rule for SOURCE-CODE FILES (a file read: imports + function/class defs). A large
file read is the MOST important thing to reduce — produce a SKELETON. Do NOT return it
unchanged; a large file always has bodies to elide, and bodies are recoverable via
expand, so PREFER eliding. Concretely, go line by line and KEEP a line verbatim iff it
is one of:
  * an import / from / require / #include / package / using line;
  * a signature or structural line — contains "def ", "class ", "func ", "function ",
    "interface ", "type ", "struct ", "enum ", "public/private/protected", a decorator
    ("@..."), or ends with "{" / ":" at a low indent;
  * a module-level constant / assignment (low indent);
  * a docstring/comment line immediately under a kept signature;
  * ANY line containing a KEEP-list identifier or clearly central to the goal.
Otherwise the line is body detail: DROP it, and collapse each run of dropped lines
into ONE marker line that keeps the run's indentation, e.g.
  "        # … 14 lines elided (call context_guru_expand) …".
KEEP the FULL body (do NOT elide) when ANY of these hold — this protects the agent from
having to re-read (which wastes far more than it saves):
  * the definition's name or a line in it matches the KEEP list or the goal;
  * the body is SHORT (a rough rule: ≤ ~15 lines) — eliding it saves little but risks a
    re-read;
  * the definition is adjacent to a KEEP-matching one (the agent is likely working nearby).
Only elide the LONG bodies (> ~15 lines) of definitions with no KEEP/goal relevance.
When in doubt about a body, KEEP it. Kept lines must be BYTE-IDENTICAL (keep any leading
line numbers). A big file with many unrelated long defs should still shrink a lot; a
small or highly-relevant file may barely shrink — that is correct.
Always PRESERVE EXACTLY, verbatim: ids, numbers, names, paths, signatures,
timestamps, error messages, stack traces, and anything matching the KEEP list.
For NON-code output, if nothing is clearly reducible, leave OUTPUT = INPUT.`

// codeRules is the DEFAULT (powerful) contract: the program may delete OR rewrite
// via regex/string ops — collapse repeated blocks, strip progress columns/banners,
// keep only relevant records/lines — as long as the verbatim-preservation rule holds.
const codeRules = codeContract + `
You MAY delete AND rewrite: collapse runs, strip noise columns, drop irrelevant
records/lines, replace verbose boilerplate with a shorter form — provided every
preserved item above stays byte-for-byte intact.
Output ONLY the Starlark program — no prose, no markdown fences.`

// codeDeletionRules is the strict (rewrite:false) contract: deletion only, verified
// as a character subsequence of INPUT (no reorder/reword/fabrication).
const codeDeletionRules = codeContract + `
DELETION ONLY: you may only DELETE characters — never add, reorder, reword, renumber,
translate, or rephrase. The result MUST be obtainable by removing characters from
INPUT (verified as a subsequence; a rewrite is rejected and wastes the call). SUMMARY
is the ONE exception — it is separate from OUTPUT and may be free text.
Output ONLY the Starlark program — no prose, no markdown fences.`

const codeExample = `EXAMPLE A (JSON search hits) — keep only the relevant records:
  data = json.decode(INPUT)
  kept = [r for r in data if "col_insert" in r["match"] or "common.py" in r["path"]]
  OUTPUT = json.encode(kept)
  SUMMARY = "search: %d of %d hits kept" % (len(kept), len(data))
EXAMPLE B (pytest log) — drop passing/progress noise, strip the % progress column,
keep failures and the summary line:
  lines = INPUT.split("\n")
  kept = [ln for ln in lines if "PASSED" not in ln and not re_match("^\\s*$", ln)]
  OUTPUT = re_sub(" +\\[ *[0-9]+%\\]", "", "\n".join(kept))
  SUMMARY = "pytest failures + summary kept; passing lines elided"
EXAMPLE C (verbose install log) — collapse the "already satisfied" noise:
  lines = [ln for ln in INPUT.split("\n") if "already satisfied" not in ln]
  OUTPUT = "\n".join(lines)
EXAMPLE D (source-code FILE READ; goal/KEEP mentions "parse_config") — skeleton:
keep imports, every signature, and the relevant def; collapse each run of other body
lines into one indented marker. Kept lines stay byte-identical (line numbers kept).
  keep_ids = ["parse_config"]   # from KEEP / the goal
  out = []
  pending = 0    # consecutive elided body lines
  indent = ""
  for ln in INPUT.split("\n"):
    s = ln.strip()
    struct = ("def " in s or "class " in s or "func " in s or "function " in s or
              s.startswith("import ") or s.startswith("from ") or s.startswith("@") or
              s.endswith(":") or s.endswith("{"))
    keep = s == "" or struct or any([k in ln for k in keep_ids])
    if keep:
      if pending > 0:
        out.append(indent + "# ... " + str(pending) + " lines elided (call context_guru_expand) ...")
        pending = 0
      out.append(ln)
    else:
      if pending == 0:
        indent = ln[:len(ln) - len(s)]
      pending = pending + 1
  if pending > 0:
    out.append(indent + "# ... " + str(pending) + " lines elided (call context_guru_expand) ...")
  OUTPUT = "\n".join(out)
  SUMMARY = "skeleton: imports + signatures + parse_config kept; bodies elided"`

// --- Aggressiveness -------------------------------------------------------------
//
// How hard to compact is a JUDGEMENT, not a threshold, so it is taught rather than
// configured: the second system block carries a target and few-shot examples that
// demonstrate it. Three levels, because two is not enough to express "about like today"
// and four would be a distinction nobody can act on.
//
// Why this is a SEPARATE block from the general contract, and second: the general half is
// byte-identical for every tenant on the deployment, so as its own leading block it is one
// shared cacheable prefix no matter which level each tenant picked. Only the shorter second
// block differs between them.
//
// Every example sets SUMMARY. That is not decoration — the summary is what the agent reads
// next to the recovery marker, and it is the difference between "something was removed
// here" and "the 118 passing test lines were removed here, call expand if you need them".
// An example that omits it teaches the model to omit it.

// Aggressiveness names the compaction target. Empty means AggroMedium.
type Aggressiveness string

// The compaction targets. The percentages are a target for the MODEL, not a guarantee
// and not a gate: the acceptance checks (verbatim ids/paths/errors, strictly smaller,
// and in deletion-only mode the subsequence proof) are unchanged at every level, so a
// higher level asks for more and never buys it by weakening what must survive.
const (
	AggroLow    Aggressiveness = "low"
	AggroMedium Aggressiveness = "medium"
	AggroHigh   Aggressiveness = "high"
)

// ParseAggressiveness validates a configured level; empty means medium.
func ParseAggressiveness(s string) (Aggressiveness, error) {
	switch a := Aggressiveness(s); a {
	case "", AggroMedium:
		return AggroMedium, nil
	case AggroLow, AggroHigh:
		return a, nil
	default:
		return AggroMedium, fmt.Errorf("aggressiveness must be low|medium|high, got %q", s)
	}
}

// aggroBlock returns the second system block for a level.
func aggroBlock(a Aggressiveness) string {
	switch a {
	case AggroLow:
		return aggroLow
	case AggroHigh:
		return aggroHigh
	default:
		return aggroMedium
	}
}

const aggroLow = `COMPACTION TARGET: LOW. Remove only what is unambiguously redundant —
exact repetition, progress bars, banners, boilerplate. If you are not certain a line is
noise, KEEP it. A 10-25% reduction is a good result here; returning OUTPUT = INPUT is an
acceptable answer when nothing is clearly redundant. Prefer under-cutting: at this level the
agent has told us it would rather pay for tokens than risk re-running the tool.

EXAMPLE A (JSON search hits) — keep every record; drop only a field repeated identically
across all of them:
  data = json.decode(INPUT)
  for r in data:
    r.pop("repo_root")          # identical in every record, restated below
  OUTPUT = json.encode(data)
  SUMMARY = "search: all %d hits kept; constant repo_root field dropped" % len(data)
EXAMPLE B (bash / test log) — strip the progress column and collapse consecutive
duplicates, keep every distinct line including passes:
  out = []
  prev = ""
  for ln in INPUT.split("\n"):
    ln = re_sub(" +\\[ *[0-9]+%\\]", "", ln)
    if ln != prev:
      out.append(ln)
    prev = ln
  OUTPUT = "\n".join(out)
  SUMMARY = "log: progress column stripped, consecutive duplicate lines collapsed"
EXAMPLE C (prose / documentation output) — drop only the legal footer and nav chrome:
  parts = re_split("\n-{3,}\n", INPUT)
  OUTPUT = parts[0]
  SUMMARY = "doc: body kept; trailing footer/nav sections dropped"
EXAMPLE D (source-code FILE READ) — keep imports, all signatures, and every body up to
about 40 lines; elide only very long bodies with no KEEP/goal relevance:
  OUTPUT = INPUT   # a small or highly relevant file: nothing safe to elide
  SUMMARY = ""`

const aggroMedium = `COMPACTION TARGET: MEDIUM (the default). Remove clear noise and
irrelevant records, and skeletonize long source files, while keeping anything plausibly
related to the goal. A 25-50% reduction is a good result. Keep ids, paths, numbers,
timestamps, signatures and error text byte-identical.

` + codeExample + `
EXAMPLE E (prose / documentation output) — keep the sections that bear on the goal, drop
boilerplate, and say what went:
  paras = re_split("\n\n+", INPUT)
  drop = ["Copyright", "This page was generated", "Table of contents"]
  kept = [p for p in paras if not any([d in p for d in drop])]
  OUTPUT = "\n\n".join(kept)
  SUMMARY = "doc: %d of %d sections kept; boilerplate dropped" % (len(kept), len(paras))`

const aggroHigh = `COMPACTION TARGET: HIGH. Keep what the goal needs and little else, plus
every id, path, number, timestamp, signature, error line and KEEP-list identifier
BYTE-IDENTICAL — those are never negotiable at any level. A 50-80% reduction is a good
result. Where you remove a run, leave ONE marker line naming what went, and always set
SUMMARY: the agent can call context_guru_expand to get the original back, but only if the
summary tells it there is something worth getting.

EXAMPLE A (JSON search hits) — keep only matching records, and only the fields the goal
needs, with a count of what went:
  data = json.decode(INPUT)
  kept = [{"path": r["path"], "line": r["line"], "match": r["match"]}
          for r in data if "col_insert" in r["match"] or "common.py" in r["path"]]
  OUTPUT = json.encode(kept)
  SUMMARY = "search: %d of %d hits kept, other fields dropped" % (len(kept), len(data))
EXAMPLE B (bash / test log) — failures, the summary line, and nothing else:
  lines = INPUT.split("\n")
  keep = [ln for ln in lines
          if re_match("(FAIL|ERROR|Traceback|assert|error:)", ln) or
             re_match("^=+ .* =+$", ln) or re_match("[0-9]+ (passed|failed)", ln)]
  OUTPUT = "\n".join(keep) + "\n# ... %d other log lines elided (call context_guru_expand) ..." % (len(lines) - len(keep))
  SUMMARY = "pytest: failures + summary kept, %d passing/progress lines elided" % (len(lines) - len(keep))
EXAMPLE C (prose / documentation output) — keep only paragraphs mentioning the goal terms:
  terms = ["timeout", "auth"]        # from the goal / KEEP list
  paras = re_split("\n\n+", INPUT)
  kept = [p for p in paras if any([t in p.lower() for t in terms])]
  OUTPUT = "\n\n".join(kept) + "\n\n# ... %d unrelated paragraphs elided (call context_guru_expand) ..." % (len(paras) - len(kept))
  SUMMARY = "doc: %d of %d paragraphs kept (auth/timeout)" % (len(kept), len(paras))
EXAMPLE D (source-code FILE READ) — full skeleton: imports, signatures and KEEP-relevant
bodies only; elide every other body over about 5 lines, one indented marker per run:
  keep_ids = ["parse_config"]
  out = []
  pending = 0
  indent = ""
  for ln in INPUT.split("\n"):
    s = ln.strip()
    struct = ("def " in s or "class " in s or "func " in s or "function " in s or
              s.startswith("import ") or s.startswith("from ") or s.startswith("@") or
              s.endswith(":") or s.endswith("{"))
    keep = s == "" or struct or any([k in ln for k in keep_ids])
    if keep:
      if pending > 0:
        out.append(indent + "# ... " + str(pending) + " lines elided (call context_guru_expand) ...")
        pending = 0
      out.append(ln)
    else:
      if pending == 0:
        indent = ln[:len(ln) - len(s)]
      pending = pending + 1
  if pending > 0:
    out.append(indent + "# ... " + str(pending) + " lines elided (call context_guru_expand) ...")
  OUTPUT = "\n".join(out)
  SUMMARY = "skeleton: imports + signatures + parse_config kept, other bodies elided"`

// maxGoalChars is the BACKSTOP on the conversational context a prompt carries. The caller
// decides how much context to send (see the component's context: goal|recent|full) and
// clips to its own per-mode budget; this only stops a caller with no budget at all from
// producing an unbounded prompt. It is therefore set above the largest caller budget rather
// than at the old 8000, which would have silently truncated `context: full` to a few turns.
const maxGoalChars = 400_000

// clipGoal bounds the context on a rune boundary. The old code sliced bytes, which can
// split a multi-byte rune and hand the model invalid UTF-8 mid-prompt.
func clipGoal(g string) string {
	// Counted in RUNES, because that is what the trim below uses. Gating on len(g) in bytes
	// while trimming runes meant a multi-byte goal over the byte cap but under the rune cap
	// came back unclipped, so the backstop bounded ASCII input only.
	if utf8.RuneCountInString(g) <= maxGoalChars {
		return g
	}
	r := []rune(g)
	// Keep the TAIL: the newest turns say what the agent needs next.
	return string(r[len(r)-maxGoalChars:])
}

// maxCodeContentChars bounds the full output shown to the model. Big enough to be
// content-specific (~8k tokens), bounded so a giant output can't blow up the prompt;
// beyond it we show head+tail and note the truncation (the program still runs over
// the full INPUT at runtime).
const maxCodeContentChars = 32000

// PromptVersion identifies the extractor's prompt + acceptance semantics. The result cache
// key includes it, so a change MISSES every stale entry rather than serving an extraction
// derived under different rules.
//
// DERIVED, not hand-maintained. A manual constant only works if every future editor of the
// prompt remembers to bump it, and the one time someone forgets, the cache serves
// extractions produced under rules that no longer exist — exactly the failure the issue
// warned about, and one with no symptom to notice. Hashing the prompt text makes the version
// a consequence of the prompt instead of a promise about it.
var PromptVersion = promptFingerprint()

// semanticsVersion covers result-affecting changes OUTSIDE the prompt strings — the
// validation gate (validateExtraction / extractionIsSane) and the sandbox contract. Bump it
// when what gets ACCEPTED changes while the prompt text does not.
const semanticsVersion = "s1"

// promptFingerprint hashes every prompt constant that can change what the model returns.
// Add new prompt text here as it is introduced: anything omitted is invisible to the key.
func promptFingerprint() string {
	h := sha256.New()
	for _, part := range []string{
		semanticsVersion,
		codeContract, codeRules, codeDeletionRules, codeExample,
		aggroLow, aggroMedium, aggroHigh,
		rules, example, sampleMarker,
	} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return "p" + hex.EncodeToString(h.Sum(nil))[:12]
}

// codeSystemBlocks is the INVARIANT half of the code-strategy prompt, as TWO ordered
// blocks so each can be cached independently:
//
//	[0] the general contract — the sandbox API, the preservation rules, the source-file
//	    skeleton rule. Selected only by REWRITE MODE, so there are two possible values on
//	    the whole deployment and every tenant sending the same one shares a cache entry.
//	[1] the compaction target and its worked examples, selected by AGGRESSIVENESS.
//
// Ordered general-first on purpose: a provider caches a PREFIX, so the block shared by
// everyone has to come first or it is not shared at all. Anything that varies per call
// (the goal, the keep-list, the tool output) stays in the user message.
func codeSystemBlocks(rewrite bool, a Aggressiveness) []string {
	rules := codeRules
	if !rewrite {
		rules = codeDeletionRules
	}
	return []string{
		"You write a Starlark program that reduces ONE tool output to what the agent needs next.\n\n" + rules,
		aggroBlock(a),
	}
}

// buildCodePrompt builds the prompt for the Starlark code-writing strategy. It shows
// the model the FULL output (bounded) so it can write content-specific deletions
// rather than a blind generic filter. rewrite selects the (lossy, unverified) rewrite
// contract instead of the default deletion-only one.
//
// Deprecated in favor of buildCodePromptSplit; retained so the single-message shape
// stays testable and any caller without a system-capable client keeps working.
func buildCodePrompt(bodyText, goal string, keepIDs []string, rewrite bool) string {
	g := strings.TrimSpace(goal)
	if g == "" {
		g = "(no explicit goal stated)"
	}
	g = clipGoal(g)
	keep := keepIDs
	if len(keep) > 60 {
		keep = keep[:60]
	}
	keepBlock := ""
	if len(keep) > 0 {
		kb, _ := json.Marshal(keep)
		keepBlock = "IDENTIFIERS THE AGENT REFERENCED RECENTLY — keep every one verbatim:\n" +
			string(kb) + "\n\n"
	}

	// Show the FULL content. Pretty-print ONLY when the WHOLE body is a valid JSON
	// object/array — never for text that merely starts with a number (a line-numbered
	// file read like "55\t…" would otherwise be parsed as the JSON number 55 and shown
	// to the model as just "55", destroying the input).
	shown := bodyText
	if isJSONContainer(bodyText) {
		if v := parseBody(bodyText); !isRawString(v) {
			if b, err := json.MarshalIndent(v, "", "  "); err == nil {
				shown = string(b)
			}
		}
	}
	label := "FULL TOOL OUTPUT (INPUT is exactly this):"
	if len(shown) > maxCodeContentChars {
		half := maxCodeContentChars / 2
		shown = shown[:half] + "\n…[middle elided in this prompt; the real INPUT at runtime is the FULL output]…\n" + shown[len(shown)-half:]
		label = "TOOL OUTPUT (head+tail; the real INPUT at runtime is the FULL output):"
	}
	rules := codeRules
	if !rewrite {
		rules = codeDeletionRules
	}
	return "You write a Starlark program that reduces ONE tool output to what the agent needs next.\n\n" +
		"WHAT THE AGENT IS DOING NOW (reduce toward this):\n" + g + "\n\n" +
		keepBlock + label + "\n" + shown + "\n\n" + rules + "\n\n" + codeExample
}

// buildCodePromptSplit returns (systemBlocks, user): the invariant preamble as ordered
// cacheable blocks, and the per-call variable part. Same total content as
// buildCodePrompt, reordered so the stable half can be a cacheable prefix. Order matters
// — the cacheable blocks must come FIRST on the wire, which is exactly what a `system`
// array gives us.
// The GOAL is a system block, not part of the user message, and that placement is the
// whole economics of a multi-candidate request. The rendered conversation context is
// computed ONCE per request (it is the same for every candidate in it) and under
// `context: full` it is the overwhelming bulk of the prompt. Measured on production: five
// haiku calls on ONE request each sent ~138,000 prompt tokens with cache_read = 0 and
// cache_write = 0 — the same transcript re-sent fresh five times — because it sat in the
// uncacheable user half.
//
// As a trailing system block it falls inside the prefix CompleteBlocks marks, so calls
// 2..N of a request read it instead of re-sending it. It also lifts the prefix over the
// model's minimum cacheable size: the invariant preamble alone is ~1,463 tokens, below
// claude-haiku-4-5's 4,096 floor, so on haiku nothing was being cached at all.
func buildCodePromptSplit(bodyText, goal string, keepIDs []string, rewrite, cacheContext bool, a Aggressiveness) (system []string, user string) {
	blocks := codeSystemBlocks(rewrite, a)
	if !cacheContext {
		return blocks, buildCodeUserPart(bodyText, goal, keepIDs)
	}
	if g := goalBlock(goal); g != "" {
		blocks = append(blocks, g)
	}
	return blocks, buildCodeUserPart(bodyText, "", keepIDs)
}

// goalBlock renders the conversation context as its own cacheable system block. Empty when
// there is no goal, so the block list stays exactly as it was for callers that pass none.
func goalBlock(goal string) string {
	g := strings.TrimSpace(goal)
	if g == "" {
		return ""
	}
	return "WHAT THE AGENT IS DOING (context for judging relevance):\n" + clipGoal(g)
}

// buildCodeUserPart is the VARIABLE half: the goal, the keep-list, and the tool output.
func buildCodeUserPart(bodyText, goal string, keepIDs []string) string {
	g := strings.TrimSpace(goal)
	if g == "" {
		g = "(no explicit goal stated)"
	}
	g = clipGoal(g)
	keep := keepIDs
	if len(keep) > 60 {
		keep = keep[:60]
	}
	keepBlock := ""
	if len(keep) > 0 {
		kb, _ := json.Marshal(keep)
		keepBlock = "IDENTIFIERS THE AGENT REFERENCED RECENTLY — keep every one verbatim:\n" +
			string(kb) + "\n\n"
	}
	shown := bodyText
	if isJSONContainer(bodyText) {
		if v := parseBody(bodyText); !isRawString(v) {
			if b, err := json.MarshalIndent(v, "", "  "); err == nil {
				shown = string(b)
			}
		}
	}
	label := "FULL TOOL OUTPUT (INPUT is exactly this):"
	if len(shown) > maxCodeContentChars {
		half := maxCodeContentChars / 2
		shown = shown[:half] + "\n…[middle elided in this prompt; the real INPUT at runtime is the FULL output]…\n" + shown[len(shown)-half:]
		label = "TOOL OUTPUT (head+tail; the real INPUT at runtime is the FULL output):"
	}
	return "WHAT THE AGENT IS DOING NOW (reduce toward this):\n" + g + "\n\n" +
		keepBlock + label + "\n" + shown
}

func isRawString(v any) bool {
	_, ok := v.(string)
	return ok
}

// isJSONContainer reports whether the WHOLE trimmed body is a valid JSON object or
// array. Used to decide whether to pretty-print the body for the model — a bare
// number, a line-numbered file read, or a log that merely begins with a digit must
// NOT be treated as JSON (json.Decode would consume a leading number and mangle it).
func isJSONContainer(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" || (t[0] != '{' && t[0] != '[') {
		return false
	}
	return json.Valid([]byte(t))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
