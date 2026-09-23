#!/usr/bin/env python3
"""Measure CO-REFERENCE density in real captured requests: how much tool-output mass is
ever referenced again by a later model turn, how far back those references reach, and what
a co-reference-driven cut would COST in cache-writes vs save in cache-reads.

Substrate for docs/proposals/coref-compaction.md. Answers, from traffic rather than
argument, the three questions that decide whether the `coref` component is worth building:

  1. How much tool-output mass does no later model turn ever touch?  (the free, safe cut)
  2. Of the mass that IS referenced: last referenced long ago and only once or twice (a
     "closed" reference — the model lifted a value out, and that value survives in the
     assistant turn that lifted it), or referenced recently / repeatedly (an "open"
     reference — still load-bearing)? Recency is measured from the HEAD of the transcript,
     which is the axis the A/B distinction actually rests on.
  3. Does the arithmetic work? A cut breaks the provider's cached prefix at its shallowest
     mutated index, so the suffix is cache-WRITTEN once (11.5x a cache-read) and only then
     starts saving reads. Prints the turns-remaining `T` each cut set would need to break
     even, against the `T` the session actually had.

METHOD, and its one non-negotiable guard. A reference is only counted for tokens the tool
output ITSELF INTRODUCED. If the agent calls Read(src/auth.py), the path is in the tool_use
argument, echoed by the tool_result, and used again by a later Edit(src/auth.py) — exact
matching would score that as a reference, but the value was in context before the output
existed. So every token already present at or before the producing turn is excluded as
echo, and only NOVEL tokens count. Without this the numbers are meaningless (they trend
toward "everything is referenced").

Reference-bearing surfaces are MODEL turns only — assistant text and tool_use arguments.
A later tool_result echoing a token is the environment repeating itself, not the model
using the value.

Tier-1 (exact) matching ONLY. Mass reported as `unreferenced` means "no later EXACT use";
it necessarily includes any use that arrived transformed (summed, unit-converted, reworded).
That gap is reported as `derived-value evidence`, never folded into `unreferenced`.

Usage:
  coref.py <capture.jsonl> [more.jsonl ...] [key=value ...]

  min_later=8      an output with fewer model turns after it is never cut (no opportunity yet)
  closed_dist=12   a reference is "closed" once its last reference is this many messages AGO
                   (measured from the head of the transcript, not from the output)
  open_reps=3      ...unless referenced at least this many times (then it stays "open")
  min_output=300   ignore tool outputs smaller than this (tokens) — below any cut floor
  window=200000    model context window, for the trigger-crossing / agent-compact estimates
  fire_frac=0.6    cut fires when a request reaches this fraction of the window
  sweep=1          also print a threshold sensitivity sweep
  json=1           emit machine-readable totals instead of the report
"""
import json, sys, re, hashlib
from collections import defaultdict

TOK = lambda s: max(1, len(s) // 4)  # cheap ~4 chars/token proxy (as analyze_content.py)
CACHE_WRITE_X = 11.5  # ($2.50 - $0.20) / $0.20 — one cache-write in cache-read-equivalents

# An identifier-ish token: the things a model actually carries forward (paths, symbols,
# ids, hashes, error codes). Prose is filtered out by DISTINCTIVE below rather than by a
# stopword list, which does not survive a change of domain.
IDENT = re.compile(r"[A-Za-z_][A-Za-z0-9_./:\-]{2,63}|\b\d{3,}\b|\b[0-9a-f]{7,40}\b")
NUMERIC = re.compile(r"^\d[\d,._]*$")
CAMEL = re.compile(r"[a-z][A-Z]")


def distinctive(t):
    """Keep tokens that look like an identifier rather than an English word: after trimming
    surrounding punctuation they carry INTERIOR structure (_ . / : -), a digit, or CamelCase.

    Three rules here were measured, not guessed — an earlier version accepted any token of
    10+ characters, any token containing punctuation, and any number of 3+ digits, and on
    real traffic the top "references" it found were `description`, `transparency`,
    `efficiency`, `conditions`, `e.g.`, `try:`, `None:` and `2026`. Prose scored as
    identifiers inflated referenced mass from 51% to 71%, and a spurious reference is the
    expensive direction: it makes an output look load-bearing and suppresses the cut silently.

      - no bare length rule: long English words are still English words. A real identifier
        almost always carries structure, a digit, or camelCase (`src/auth.py`, `session_id`,
        `GraphStore`); one that carries none is indistinguishable from prose, so it is
        dropped rather than guessed at.
      - punctuation must be INTERIOR: `e.g.` / `try:` / `memory.` are prose plus a sentence
        mark. Trimming the ends first leaves `e.g` / `try` / `memory`, which fail on their
        own merits, while `src/auth.py` and `v1/messages` are untouched.
      - a bare number needs 5+ digits or a separator: `2026` is a year and `447` is a line
        number or a count, and both recur everywhere. Hashes, ids and versions survive.
    """
    t = t.strip("._:-/")  # trim first, so the test sees the token and not its punctuation
    if len(t) < 4:
        return False
    if NUMERIC.match(t):
        return sum(c.isdigit() for c in t) >= 5 or bool(set(",._") & set(t))
    return bool(set("_./:-") & set(t)) or any(c.isdigit() for c in t) or bool(CAMEL.search(t))


def idents(s):
    """Distinctive tokens, trimmed of surrounding punctuation — so `memory.` and `memory`
    are one token rather than two that never match each other."""
    out = set()
    for t in IDENT.findall(s or ""):
        if distinctive(t):
            out.add(t.strip("._:-/"))
    return out


# --- normalize a provider body into one flat message list -----------------------------
#
# Each entry: {"role", "texts": [model-authored text], "results": [(tool_use_id, text)]}
# `texts` is the reference-bearing surface (assistant prose + tool_use arguments);
# `results` is the mass under evaluation.


def normalize(body, provider):
    out = []
    if provider == "anthropic":
        for m in body.get("messages", []):
            role, c = m.get("role"), m.get("content")
            e = {"role": role, "texts": [], "results": []}
            if isinstance(c, str):
                e["texts"].append(c)
            elif isinstance(c, list):
                for b in c:
                    if not isinstance(b, dict):
                        continue
                    t = b.get("type")
                    if t == "text":
                        e["texts"].append(b.get("text", ""))
                    elif t == "tool_use":
                        e["texts"].append(b.get("name", "") + " " + json.dumps(b.get("input", {})))
                    elif t == "tool_result":
                        rc = b.get("content")
                        if isinstance(rc, str):
                            txt = rc
                        elif isinstance(rc, list):
                            txt = "".join(x.get("text", "") for x in rc if isinstance(x, dict))
                        else:
                            txt = json.dumps(rc) if rc is not None else ""
                        e["results"].append((b.get("tool_use_id", ""), txt))
            out.append(e)
    else:  # openai
        for m in body.get("messages", []):
            role = m.get("role")
            e = {"role": role, "texts": [], "results": []}
            c = m.get("content")
            if role == "tool":
                e["results"].append((m.get("tool_call_id", ""), c if isinstance(c, str) else json.dumps(c)))
            else:
                if isinstance(c, str):
                    e["texts"].append(c)
                for tc in m.get("tool_calls") or []:
                    f = tc.get("function") or {}
                    e["texts"].append(f.get("name", "") + " " + str(f.get("arguments", "")))
            out.append(e)
    return out


# --- the measurement ------------------------------------------------------------------


def analyze_session(msgs, min_output):
    """One record per tool output: its size, where it sits, and how the model referred
    back to it afterwards. Only tokens the output INTRODUCED are eligible (see module
    docstring), and only model turns count as referring."""
    ref_tokens = [idents(" ".join(m["texts"])) for m in msgs]  # model-authored surfaces
    res_tokens = [{} for _ in msgs]
    for i, m in enumerate(msgs):
        for tid, txt in m["results"]:
            res_tokens[i][tid] = idents(txt)

    # Tokens present at or before message i, from ANY surface — the echo exclusion set.
    # Built as a running union so a 150k-token transcript stays one pass.
    prior = [set() for _ in msgs]
    acc = set()
    for i, m in enumerate(msgs):
        prior[i] = set(acc)
        acc |= ref_tokens[i]
        for toks in res_tokens[i].values():
            acc |= toks

    # A token echoed by many outputs is boilerplate, not a carried value.
    spread = defaultdict(int)
    for i in range(len(msgs)):
        for toks in res_tokens[i].values():
            for t in toks:
                spread[t] += 1
    n_out = sum(len(m["results"]) for m in msgs) or 1
    common = {t for t, c in spread.items() if c > max(5, n_out // 4)}

    recs = []
    for i, m in enumerate(msgs):
        for tid, txt in m["results"]:
            size = TOK(txt)
            if size < min_output:
                continue
            # Novel = introduced here: not in prior context, not in a sibling block of
            # this same message (the producing tool_use lands in the turn before, but a
            # batched turn can carry several), and not session boilerplate.
            siblings = set()
            for otid, otoks in res_tokens[i].items():
                if otid != tid:
                    siblings |= otoks
            novel = res_tokens[i][tid] - prior[i] - siblings - common - ref_tokens[i]
            hits = []  # (message index, how many novel tokens that turn reused)
            used = set()
            later = 0
            for j in range(i + 1, len(msgs)):
                if not msgs[j]["texts"]:
                    continue
                later += 1  # a model turn that COULD have referenced this output
                inter = novel & ref_tokens[j]
                if inter:
                    hits.append(j)
                    used |= inter
            recs.append({
                "idx": i,
                "size": size,
                "novel": len(novel),
                "refs": len(hits),
                # ref_age: how many messages ago the LAST reference was, measured from the
                # head of the transcript. This — not the output's own depth — is the A/B
                # discriminator: B is "referenced recently", A is "referenced long ago".
                "ref_age": (len(msgs) - hits[-1]) if hits else None,
                # consume_lag: how long AFTER the output the model first/last used it. A
                # separate signal: a long lag means the output stayed live for many turns.
                "consume_lag": (hits[-1] - i) if hits else None,
                "used_frac": (len(used) / len(novel)) if novel else 0.0,
                # later_turns is the OPPORTUNITY to be referenced; near the tail it goes to
                # zero and an output that never had a chance must not be scored as unused.
                "later_turns": later,
            })
    return recs


def classify(r, closed_dist, open_reps, min_later=8):
    """OPAQUE       — the output introduced NO trackable identifier, so there is no evidence
                      either way. Absence of evidence, not evidence of deadness: a tool
                      returning records of human-readable values
                      ([{"name":"david","id":123,"address":"foobarbaz"}]) yields no
                      distinctive tokens at all, because short lowercase words and 3-digit
                      numbers are what the precision rules in distinctive() exclude. Folded
                      into `unreferenced` it is a silent vote to delete. Never cut.
    UNREFERENCED — introduced identifiers, and no later exact use (safest cut on Tier 1,
                   blind to Tier 2).
    OPEN         — referenced recently or repeatedly (still load-bearing), OR too new to
                   have had the chance: an output with fewer than min_later model turns
                   after it has no opportunity to be referenced, and scoring it as unused
                   would preferentially cut the most RECENT context.
    CLOSED       — referenced once/twice and not for a long time; whatever the model took
                   survives in the assistant turn that took it. The large-cut candidate."""
    if r["novel"] == 0:
        return "opaque"
    if min_later > 0 and r["later_turns"] < min_later:
        return "open"
    if r["refs"] == 0:
        return "unreferenced"
    if r["refs"] >= open_reps or r["ref_age"] < closed_dist:
        return "open"
    return "closed"


def derived_evidence(msgs):
    """Tier-2 proxy: numeric tokens the model states that appear NOWHERE earlier. A value
    it computed (summed, converted) rather than copied — a reference an exact matcher is
    structurally unable to see. Reported as a caveat on `unreferenced`, never subtracted."""
    seen, derived, total = set(), 0, 0
    for m in msgs:
        mine = idents(" ".join(m["texts"]))
        nums = {t for t in mine if NUMERIC.match(t)}
        if m["texts"]:
            total += 1
            if nums - seen:
                derived += 1
        seen |= mine
        for _, txt in m["results"]:
            seen |= idents(txt)
    return derived, total


def session_turns(reqs, provider, window, fire_frac):
    """Turns remaining after the cut would fire (T in the break-even inequality), plus the
    session's peak request size. Append-only traffic means one capture per turn, so the
    request whose message-count first crosses fire_frac*window dates the trigger."""
    sizes = []
    for r in reqs:
        # A pre-sized record (cc_capture.py: a converted Claude Code transcript, where a full
        # prefix per turn would be O(n^2) bytes). Only the SIZE of a non-final turn is ever
        # read here, so carrying it directly is equivalent. A real capture never sets this
        # field, so the capture path below is unchanged.
        if "turn_tokens" in r:
            sizes.append(int(r["turn_tokens"]))
            continue
        b = r["body"]
        n = 0
        for m in normalize(b, provider):
            n += TOK(" ".join(m["texts"])) + sum(TOK(t) for _, t in m["results"])
        sizes.append(n)
    sizes.sort()
    fire_at = fire_frac * window
    crossed = [k for k, v in enumerate(sizes) if v >= fire_at]
    t_remaining = (len(sizes) - crossed[0]) if crossed else 0
    return t_remaining, (sizes[-1] if sizes else 0)


def main():
    files = [a for a in sys.argv[1:] if "=" not in a]
    opt = dict(a.split("=", 1) for a in sys.argv[1:] if "=" in a)
    closed_dist = int(opt.get("closed_dist", 12))
    open_reps = int(opt.get("open_reps", 3))
    min_output = int(opt.get("min_output", 300))
    min_later = int(opt.get("min_later", 8))
    window = int(opt.get("window", 200000))
    fire_frac = float(opt.get("fire_frac", 0.6))
    if not files:
        print(__doc__)
        return 2

    agg = {}
    for f in files:
        recs = [json.loads(l) for l in open(f) if l.strip()]
        by_conv = defaultdict(list)
        for r in recs:
            # An explicit conversation id, when the producer knows one (cc_capture.py, which
            # splits one long transcript into several window-sized sessions). Inferring it from
            # the first user message is right for a real capture — every request of a session
            # opens with the same task statement — but a mid-transcript segment opens on a
            # tool_result, and those collide: 31 segments grouped down to 24, silently
            # discarding the rest, because only the largest member of a group is analyzed.
            key = r.get("conv")
            # A REAL capture carries the agent's own session id in metadata.user_id (the
            # Anthropic clients pack {device_id, account_uuid, session_id} in there). Prefer it:
            # the first-user-message fallback below is catastrophically wrong on benchmark
            # traffic, where every task's instruction opens with the SAME long preamble. On
            # capture-swebench.jsonl the 200-char prefix has 19 distinct values and the top one
            # covers 1,771 of 1,795 requests -- so 18 of 19 groups held nothing but stray
            # single-message calls, and because only the largest member of a group is analyzed,
            # the measurement silently reported ONE session's worth of data. The session id
            # recovers 50 sessions from the same file.
            if key is None:
                md = (r["body"].get("metadata") or {}).get("user_id")
                if md:
                    try:
                        key = (json.loads(md) or {}).get("session_id") or md
                    except (ValueError, TypeError):
                        key = md
            if key is None:
                for m in r["body"].get("messages", []):
                    if m.get("role") == "user":
                        key = hashlib.sha1(json.dumps(m.get("content"))[:200].encode()).hexdigest()[:8]
                        break
            by_conv[key].append(r)

        mass = defaultdict(int)          # bucket -> tokens
        count = defaultdict(int)
        dist_hist = defaultdict(int)     # last-reference RECENCY bucket -> tokens
        reps_hist = defaultdict(int)     # reference count bucket -> tokens
        used_fracs = []
        lags = []
        breakeven = []                   # (needed_T, actual_T, cut_tokens, suffix_tokens)
        derived_n = derived_d = 0
        peaks = []
        for conv, rs in by_conv.items():
            rs.sort(key=lambda r: len(r["body"].get("messages", [])))
            top = rs[-1]  # append-only: the largest request holds the whole transcript
            prov = top.get("provider", "anthropic")
            msgs = normalize(top["body"], prov)
            recs_s = analyze_session(msgs, min_output)
            if not recs_s:
                continue
            dn, dd = derived_evidence(msgs)
            derived_n += dn
            derived_d += dd
            t_rem, peak = session_turns(rs, prov, window, fire_frac)
            peaks.append(peak)

            cut_idx, cut_tok = [], 0
            for r in recs_s:
                b = classify(r, closed_dist, open_reps, min_later)
                mass[b] += r["size"]
                count[b] += 1
                # `open` now also covers "too new to have been referenced yet", which has no
                # last reference at all — so bucket on whether a reference EXISTS, not on the
                # verdict, or the histogram indexes None.
                if r["ref_age"] is None:
                    dist_hist["never"] += r["size"]
                    reps_hist["0"] += r["size"]
                else:
                    d = r["ref_age"]
                    key = "1-3" if d <= 3 else "4-11" if d < 12 else "12-39" if d < 40 else "40+"
                    dist_hist[key] += r["size"]
                    reps_hist["1" if r["refs"] == 1 else "2" if r["refs"] == 2 else "3+"] += r["size"]
                    used_fracs.append(r["used_frac"])
                    lags.append(r["consume_lag"])
                if b in ("unreferenced", "closed"):
                    cut_idx.append(r["idx"])
                    cut_tok += r["size"]

            # Break-even for ONE batched pass: the rewrite starts at the shallowest cut.
            if cut_idx and cut_tok:
                start = min(cut_idx)
                suffix = 0
                for m in msgs[start:]:
                    suffix += TOK(" ".join(m["texts"])) + sum(TOK(t) for _, t in m["results"])
                breakeven.append((CACHE_WRITE_X * suffix / cut_tok, t_rem, cut_tok, suffix))

        total = sum(mass.values()) or 1
        if opt.get("json"):
            agg[f] = {"mass": dict(mass), "count": dict(count), "total": total,
                      "dist": dict(dist_hist), "reps": dict(reps_hist),
                      "breakeven": breakeven, "derived": [derived_n, derived_d]}
            continue

        print(f"\n===== {f} =====")
        print(f"sessions {len(by_conv)}  requests {len(recs)}  "
              f"tool outputs >={min_output} tok: {sum(count.values()):,}  "
              f"mass {total:,} tok   (peak request: median {sorted(peaks)[len(peaks)//2]:,} tok)"
              if peaks else f"sessions {len(by_conv)}  requests {len(recs)}")
        print("\n  bucket        outputs      tokens   share   verdict")
        verdict = {
            "opaque":       "introduced no trackable identifier -> NO EVIDENCE, never cut",
            "unreferenced": "no later exact use -> free deterministic cut (Tier-1 blind spot applies)",
            "closed":       "value taken, survives in an assistant turn -> large-cut candidate",
            "open":         "recent or repeated -> KEEP",
        }
        for b in ("opaque", "unreferenced", "closed", "open"):
            print(f"  {b:13s} {count[b]:7,} {mass[b]:11,}  {100*mass[b]//total:4d}%   {verdict[b]}")

        print(f"\n  last reference, messages AGO (recency from the head — the A/B axis)"
              f"   [open if <{closed_dist} ago or >={open_reps} refs]")
        for k in ("1-3", "4-11", "12-39", "40+"):
            print(f"    {k:>6s} msgs ago  {dist_hist[k]:11,}  ({100*dist_hist[k]//total:2d}%)")
        if lags:
            sl = sorted(lags)
            print(f"    consume lag (output -> its last use), median {sl[len(sl)//2]} msgs "
                  f"— how long the output stayed live")
        print("\n  reference count")
        for k in ("1", "2", "3+"):
            print(f"    {k:>6s}x          {reps_hist[k]:11,}  ({100*reps_hist[k]//total:2d}%)")
        if used_fracs:
            uf = sorted(used_fracs)
            print(f"\n  novel tokens actually reused, median {uf[len(uf)//2]:.1%} "
                  f"— how little of an output a reference consumes (low => 'took a value, "
                  f"dropped the rest' is the real pattern)")

        if breakeven:
            need = sorted(b[0] for b in breakeven)
            ok = sum(1 for n, t, _, _ in breakeven if t >= n)
            med_cut = sorted(b[2] for b in breakeven)[len(breakeven)//2]
            med_suf = sorted(b[3] for b in breakeven)[len(breakeven)//2]
            print(f"\n  break-even, one batched pass per session (S*T > {CACHE_WRITE_X}*W)")
            print(f"    median cut S {med_cut:,} tok   median rewritten suffix W {med_suf:,} tok")
            print(f"    turns needed T: p25 {need[len(need)//4]:.0f}  median "
                  f"{need[len(need)//2]:.0f}  p75 {need[3*len(need)//4]:.0f}")
            print(f"    sessions where the observed T actually clears it: {ok}/{len(breakeven)}")
            print(f"    (a session whose T falls short is NOT a reason to skip the cut — it is a "
                  f"reason to justify it on steps / deferred agent-compaction, not on tokens)")

        if derived_d:
            print(f"\n  Tier-2 caveat: {100*derived_n//derived_d}% of model turns state a numeric "
                  f"value absent from all prior context (computed, not copied). Exact matching "
                  f"cannot see those references, so `unreferenced` is an UPPER bound on safe cuts.")

        if opt.get("sweep"):
            print("\n  sensitivity (closed_dist x open_reps -> closed mass share)")
            hdr = "     reps>=  " + "".join(f"{d:>8d}" for d in (4, 8, 12, 24, 40))
            print(hdr + "   <- closed_dist")
            for reps in (2, 3, 4, 6):
                row = f"     {reps:>6d}  "
                for d in (4, 8, 12, 24, 40):
                    s = 0
                    for conv, rs in by_conv.items():
                        rs.sort(key=lambda r: len(r["body"].get("messages", [])))
                        top = rs[-1]
                        for r in analyze_session(normalize(top["body"], top.get("provider", "anthropic")), min_output):
                            if classify(r, d, reps, min_later) == "closed":
                                s += r["size"]
                    row += f"{100*s//total:7d}%"
                print(row)

    if agg:
        print(json.dumps(agg, indent=2))
    return 0


if __name__ == "__main__":
    sys.exit(main())
