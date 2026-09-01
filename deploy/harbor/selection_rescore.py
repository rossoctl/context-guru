"""Re-score the held-out selection experiment under two corrections it documented but never made.

The experiment (docs/results/coref-selection-experiment.md) concluded that the deterministic
co-reference index beats every model arm, and that "no combination of index and model beat it".
Its own limitations section names two reasons that conclusion is not final, and this script
addresses both. **It makes no model calls** -- all 8,105 decisions were recorded, so every number
here is a re-score of data already on disk. Cost: $0.

CORRECTION 1 -- FLOOR SYMMETRY. From the experiment's limitations: "An asymmetry that flatters the
deterministic arms: min_later_turns is a hard structural guard present only in them. Model arms
received later_turns as information with no enforced floor. A floor-symmetric re-run is cheap and
has not been done." Confirmed in the code: score.py's deterministic() returns "keep" outright when
later_turns < min_later, while runner.py passes later_turns to the model arms as prompt text only.
So the index gets a guarantee and the models get a suggestion. Corrected in BOTH directions --
floor added to the models, floor removed from the index -- because fixing only the flattering
direction would be its own bias.

CORRECTION 2 -- TIER-2 GROUND TRUTH, AND WHY IT IS REPORTED AS A BRACKET. `future_referenced` is
exact identifier matching, so it sees Tier-1 reuse only. But the argument for spending a model call
is that a model catches Tier-2/3 reuse an exact matcher structurally cannot; when a model correctly
keeps an output the agent later restated in transformed form, the scorer records "not referenced"
and credits it nothing. The metric is blind to the capability under test.

Widening it deterministically turns out to be mostly impossible, and finding that out is a result:

  STRICT (numeric reformatting, case, substring for structured/long tokens) is defensible and
  barely moves the ground truth -- the deterministic slice of Tier-2 is nearly empty on this
  corpus.

  LOOSE additionally matches path BASENAMES. Reported separately as an UPPER bound because
  basename equality across different directories is genuinely ambiguous reuse.

An earlier version folded a third rule -- the basename's STEM -- into a single "Tier-2" number, and
it was wrong in the expensive direction: the stem key `yaml` matched 42 distinct identifiers, so a
later mention of ANY yaml file scored as reuse of a DIFFERENT one, and hyphenated tokens leaked
plain English (`Append-only` -> `only`, `Cost-based` -> `based`, plus `agent`, `memory`, `context`,
`session`, `task`). That one rule supplied 237 of 250 gains and drove the index's measured
false-drop from 11% to 53%. A false MATCH indicts the deterministic arms for reuse that never
happened, so it is the error that most needs guarding against here. See struct_keys.

Tier-3 (semantic paraphrase) is deliberately NOT addressed: it needs a judge, and an LLM-judged
ground truth reintroduces the noise that ruled out UltraHorizon in
docs/results/measurement-limits.md section 6. **So even the loose column is a lower bound on true
reuse, and any residual gap between index and model arms stays a bounded claim, not a clean one.**

VERIFICATION IS THE POINT. Two hard gates, because a silent mismatch would produce
authoritative-looking nonsense:
  1. the as-published scoring must reproduce the published table row for row -- which requires
     score.py's exact conventions, not merely similar ones (see metrics);
  2. the candidate re-derivation must reproduce candidates.json field for field before its added
     Tier-2 columns are trusted at all.
Gate 2 exists because this script recomputes novel-identifier sets prep.py already computed; the
assertion, not the similarity of the code, is what makes that safe. A silent partial join is how
deploy/harbor/coref.py's session-key collision discarded 98% of requests.
"""
import importlib.util
import json
import os
import re
import sys
from collections import defaultdict

EXP = os.environ.get("CG_EXP_DIR", "/tmp/cg-exp")
CAPTURES = [("/tmp/cg-loca.jsonl", "LOCA"), ("/tmp/cg-uh.jsonl", "UltraHorizon"),
            ("/tmp/cg-cc-all.jsonl", "ClaudeCode")]
HERE = os.path.dirname(os.path.abspath(__file__))


def _load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


cf = _load("cf", os.path.join(HERE, "coref.py"))        # idents/normalize/distinctive/TOK
score = _load("score", os.path.join(EXP, "score.py"))    # deterministic() + PRICE + TOK
MIN_OUTPUT = 300
MIN_LATER = 8

_NUM = re.compile(r"^[\d,._]*\d[\d,._]*$")
_SUFFIXED = re.compile(r"^([\d.,]+)\s*([kKmMbB])$")


def num_key(t):
    """Canonical numeric value, or None. Collapses 1200 / 1,200 / 1200.0 / 1.2k onto one key, so a
    count restated in a different format still counts as reuse."""
    s = t.strip()
    m = _SUFFIXED.match(s)
    mult = 1
    if m:
        s, suf = m.group(1), m.group(2).lower()
        mult = {"k": 1_000, "m": 1_000_000, "b": 1_000_000_000}[suf]
    bare = s.replace(",", "").replace("_", "")
    if not _NUM.match(bare or "x"):
        return None
    try:
        v = float(bare) * mult
    except ValueError:
        return None
    return ("num", int(v)) if v == int(v) else ("num", round(v, 6))


def struct_keys(t):
    """Path BASENAME only, and only when the basename is itself distinctive by coref.py's own rule.

    Split on path separators alone -- never on `.`, `:` or `-`. Splitting on those is what produced
    the stem rule described in the module docstring, which manufactured reuse at scale. Reusing
    cf.distinctive() rather than inventing a length threshold is deliberate: it is the repo's own
    measured filter for "identifier, not prose", and `yaml` / `only` / `auth` all fail it for
    carrying no interior structure, digit, or CamelCase.
    """
    out = set()
    last = re.split(r"[/\\]", t)[-1]
    if last != t and cf.distinctive(last):
        out.add(("seg", last.lower()))
    return out


def norm_keys(t, loose=False):
    """Normalized keys a token may be matched by. `loose` adds basename matching, kept opt-in so
    the two ground truths bracket the answer instead of one of them deciding it by fiat."""
    keys = {("lc", t.lower())}
    n = num_key(t)
    if n:
        keys.add(n)
    if loose:
        keys |= struct_keys(t)
    return keys


def substr_eligible(t):
    """Only structured or long tokens may match by substring. A short bare token would hit
    incidentally -- `1200` inside `112009` -- and manufacture reuse."""
    return len(t) >= 8 or any(ch in t for ch in "._:-/")


SUBSTR_BUDGET = 200   # per candidate; reported when it binds rather than silently truncating


def future_ref_t2(novel, idx_strict, idx_loose, fut_text_lc):
    """(strict, loose, rule) for one candidate's novel identifier set."""
    rule = ""
    strict = False
    for t in novel:
        for k in norm_keys(t):
            if k in idx_strict:
                return True, True, k[0]
    checked = 0
    for t in novel:
        if not substr_eligible(t):
            continue
        checked += 1
        if checked > SUBSTR_BUDGET:
            rule = "budget"
            break
        if t.lower() in fut_text_lc:
            return True, True, "substr"
    for t in novel:
        if struct_keys(t) & idx_loose:
            return strict, True, rule or "seg"
    return strict, False, rule


def rederive(path, label):
    """Recompute prep.py's candidates for one capture, adding both Tier-2 columns.

    The exact-match field is recomputed identically so it can be checked against candidates.json;
    that assertion is what licenses trusting the added columns.
    """
    recs = [json.loads(l) for l in open(path) if l.strip()]
    by = defaultdict(list)
    for r in recs:
        by[r.get("conv")].append(r)
    out = []
    for conv, rows in by.items():
        rows.sort(key=lambda r: len(r["body"].get("messages", [])))
        top = rows[-1]
        msgs = cf.normalize(top["body"], top.get("provider", "anthropic"))
        turns = [j for j, m in enumerate(msgs) if m["texts"]]
        if len(turns) < 10:
            continue
        F = turns[int(len(turns) * 0.6)]

        ref = [cf.idents(" ".join(m["texts"])) for m in msgs]
        res = [{t: cf.idents(x) for t, x in ((b[0], b[1]) for b in m["results"])} for m in msgs]
        first = {}
        for i, m in enumerate(msgs):
            for s in [ref[i]] + list(res[i].values()):
                for t in s:
                    first.setdefault(t, i)
        spread = defaultdict(int)
        for i in range(len(msgs)):
            for tk in res[i].values():
                for t in tk:
                    spread[t] += 1
        n_out = sum(len(m["results"]) for m in msgs) or 1
        common = {t for t, c in spread.items() if c > max(5, n_out // 4)}

        # Index the held-out future once per conversation rather than once per candidate.
        idx_strict, idx_loose, fut_chunks = set(), set(), []
        for j in range(F + 1, len(msgs)):
            if not msgs[j]["texts"]:
                continue
            for t in ref[j]:
                idx_strict |= norm_keys(t)
                idx_loose |= struct_keys(t)
            fut_chunks.append(" ".join(msgs[j]["texts"]).lower())
        fut_text_lc = " ".join(fut_chunks)
        future_msgs = [j for j in range(F + 1, len(msgs)) if msgs[j]["texts"]]

        for i, m in enumerate(msgs):
            if i >= F:
                break
            for tid, txt in m["results"]:
                if cf.TOK(txt) < MIN_OUTPUT:
                    continue
                sib = set()
                for o, ot in res[i].items():
                    if o != tid:
                        sib |= ot
                novel = {t for t in res[i][tid]
                         if first.get(t, i) >= i and t not in sib and t not in common
                         and t not in ref[i]}
                exact = any(novel & ref[j] for j in future_msgs)
                strict, loose, rule = future_ref_t2(novel, idx_strict, idx_loose, fut_text_lc)
                out.append({"key": f"{label}|{conv}|{i}|{tid}",
                            "future_referenced": exact,
                            "t2_strict": exact or strict,
                            "t2_loose": exact or loose,
                            "rule": "" if exact else rule})
    return out


def verdicts_from(path):
    """(verdict, kept) per key. `kept` is needed because score.py credits a trim with partial
    removal, and omitting that is what made an earlier version of this script fail gate 1."""
    out = {}
    for l in open(path):
        try:
            r = json.loads(l)
        except ValueError:
            continue
        p = r.get("parsed") or {}
        v = p.get("verdict")
        out[r["key"]] = (v if v in ("drop", "trim", "keep") else None, p.get("kept"))
    return out


def metrics(cands, verdict_of, gt="future_referenced", floor=False):
    """removed% / false-drop% / live-kept%, using score.py's conventions EXACTLY:

      - mass counts every joined candidate, including ones whose verdict failed to parse, so an
        arm is charged for the mass it declined to decide about;
      - a `trim` removes max(0, size - TOK(kept)), not the whole output;
      - `live-kept` credits only a literal `keep`, never a trim;
      - percentages floor-divide.

    Matching these is what makes gate 1 meaningful. Approximating them produced numbers that
    looked plausible and disagreed with the published table by up to 17 points.
    """
    mass = removed = dropped = fdrop = 0
    live = live_kept = 0
    overridden = 0
    for c in cands:
        v, kept = verdict_of(c)
        mass += c["size"]
        ref = c[gt]
        # The live denominator counts every joined candidate, INCLUDING ones whose verdict failed
        # to parse. score.py does this, and it is defensible: an arm that returned unparseable
        # output for a live output did not keep it. Skipping them instead inflated sonnet/bulk's
        # live-kept from 57% to 68% -- it has 149 parse failures -- which is precisely the kind of
        # plausible-looking disagreement gate 1 exists to catch.
        if ref:
            live += 1
        if v is None:
            continue
        if floor and c["later_turns"] < MIN_LATER and v != "keep":
            v, kept = "keep", None
            overridden += 1
        if v == "drop":
            dropped += 1
            removed += c["size"]
            if ref:
                fdrop += 1
        elif v == "trim":
            removed += max(0, c["size"] - score.TOK(kept or ""))
        elif ref:
            live_kept += 1
    return {"removed": 100.0 * removed / mass if mass else 0.0,
            "fdrop": 100 * fdrop // max(1, dropped),
            "live_kept": 100 * live_kept // max(1, live),
            "dropped": dropped, "live": live, "overridden": overridden}


def main():
    corpus = sys.argv[1] if len(sys.argv) > 1 else "ClaudeCode"
    cands_all = json.load(open(os.path.join(EXP, "candidates.json")))
    for c in cands_all:
        c["key"] = f'{c["corpus"]}|{c["conv"]}|{c["idx"]}|{c["id"]}'
    cands = [c for c in cands_all if c["corpus"] == corpus]
    print(f"=== {corpus}: {len(cands)} candidates (of {len(cands_all)} total), "
          f"exact-referenced-after-F={sum(c['future_referenced'] for c in cands)}")

    print("\n-- GATE 2: re-derive candidates from the captures --")
    t2 = {}
    for path, label in CAPTURES:
        if label == corpus and os.path.exists(path):
            for r in rederive(path, label):
                t2[r["key"]] = r
    missing = [c["key"] for c in cands if c["key"] not in t2]
    mismatch = [c["key"] for c in cands
                if c["key"] in t2 and t2[c["key"]]["future_referenced"] != c["future_referenced"]]
    print(f"   re-derived={len(t2)} joined={len(cands)-len(missing)}/{len(cands)} "
          f"exact-mismatches={len(mismatch)}")
    have_t2 = not (missing or mismatch)
    if not have_t2:
        print(f"   !! DOES NOT REPRODUCE candidates.json -- Tier-2 columns WITHHELD "
              f"(missing={len(missing)} mismatch={len(mismatch)})")
    else:
        print("   OK: reproduces candidates.json exactly.")
        for c in cands:
            c["t2_strict"] = t2[c["key"]]["t2_strict"]
            c["t2_loose"] = t2[c["key"]]["t2_loose"]
        rules = defaultdict(int)
        for c in cands:
            if c["t2_loose"] and not c["future_referenced"]:
                rules[t2[c["key"]]["rule"] or "seg"] += 1
        e = sum(c["future_referenced"] for c in cands)
        print(f"   referenced: exact={e}  strict={sum(c['t2_strict'] for c in cands)}  "
              f"loose={sum(c['t2_loose'] for c in cands)}   gains by rule: {dict(rules)}")
        if rules.get("budget"):
            print(f"   NOTE: substring budget bound on {rules['budget']} candidates -- "
                  f"under-counted, not over-counted.")

    arms = []
    for cc in (False, True):
        arms.append((f"deterministic (cut_closed={cc})",
                     lambda c, k=cc: (score.deterministic(c, k), None), "internal"))
        arms.append((f"  same, NO opportunity floor",
                     lambda c, k=cc: (score.deterministic(c, k, min_later=0), None), "none"))
    for f in sorted(os.listdir(EXP)):
        if not f.endswith(".jsonl") or f.startswith("smoke"):
            continue
        model = f.rsplit("-", 1)[0]
        if model not in score.PRICE:
            continue
        vs = verdicts_from(os.path.join(EXP, f))
        arms.append((f"{model} / {f.rsplit('-',1)[1][:-6]}",
                     lambda c, v=vs: v.get(c["key"], (None, None)), "none"))

    gts = [("EXACT (as published)", "future_referenced")]
    if have_t2:
        gts += [("TIER-2 STRICT (num/case/substr)", "t2_strict"),
                ("TIER-2 LOOSE (+basename; UPPER bound)", "t2_loose")]
    for gtname, gtfield in gts:
        print(f"\n{'='*100}\nGROUND TRUTH: {gtname}\n{'='*100}")
        print(f"{'arm':40s} {'removed':>8s} {'f-drop':>7s} {'live-kept':>10s}    "
              f"{'FLOOR-SYMMETRIC':>16s} {'f-drop':>7s} {'live-kept':>10s} {'ovr':>4s}")
        for label, vf, floorkind in arms:
            a = metrics(cands, vf, gtfield)
            row = (f"{label:40s} {a['removed']:7.1f}% {a['fdrop']:6d}% {a['live_kept']:9d}%")
            if floorkind == "internal":
                print(row + "     (floor already enforced internally)")
            else:
                b = metrics(cands, vf, gtfield, floor=True)
                print(row + f"    {b['removed']:15.1f}% {b['fdrop']:6d}% "
                            f"{b['live_kept']:9d}% {b['overridden']:4d}")


if __name__ == "__main__":
    main()
