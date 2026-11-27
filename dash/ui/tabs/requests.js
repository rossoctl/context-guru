// context-guru dashboard: the Requests tab, the request drawer and the session compaction diff.
// Moved out of app.js (#1089). app.js loads first and holds the shared DOM helpers,
// formatting, state and router that this file uses. See the file list in index.html.
'use strict';

// ── requests ───────────────────────────────────────────────────────────────
async function loadRequests() {
  const body = clear($('#requests-body'));
  const wide = showScopeCol('[data-testid=requests-table]', wideScope());
  const cols = wide ? 15 : 14;
  loadingRows(body, cols, 6);
  try {
    const page = await api('requests', { limit: 50, before: state.reqCursor });
    clear(body);
    if (!page.requests.length) {
      renderNoMatch(body, cols, 'requests');
    }
    for (const e of page.requests) {
      body.appendChild(el('tr', { class: 'click', 'data-testid': 'request-row', onclick: () => openRequest(e.id) },
        el('td', { text: e.id }),
        el('td', { text: whenShort(e.ts) }),
        el('td', {}, el('span', { class: 'trunc', title: e.session_id, text: e.session_id || '—' })),
        wide ? el('td', {}, el('span', { title: e.tenant_id, text: e.tenant_id ? e.tenant_id.slice(0, 6) : '—' })) : null,
        el('td', { class: 'more', text: e.model || '—' }),
        el('td', {}, el('span', { class: 'pill neutral', text: modeLabel(e.mode) })),
        el('td', { class: 'num more', text: compact(e.tokens_before) }),
        el('td', { class: 'num', text: compact(e.tokens_after) }),
        el('td', { class: 'num more', text: compact(e.tokens_before - e.tokens_after) }),
        el('td', { class: 'num more', text: compact(e.cache_read) + ' / ' + compact(e.cache_write) }),
        el('td', { class: 'num', text: e.token_accounting === 'complete' ? usd(e.cost_usd) : '—' }),
        el('td', { class: 'num more', text: ms(e.cg_latency_ms) }),
        // A keep-alive ping is not agent traffic, and the consent copy promises it is
        // identifiable here. Without the badge the row reads as a mysterious one-token request
        // the user did not make, which is worse than not showing it at all.
        el('td', {}, e.keepalive
          ? el('span', { class: 'pill neutral', title: 'a ping your cache strategy sent to '
              + 'refresh this session\u2019s cached prompt', text: 'cache strategy' })
          : el('span', { class: 'pill ' + (e.cache_miss_reason || 'neutral'), text: e.cache_miss_reason || '—' })),
        el('td', { class: 'more' }, el('span', { class: 'pill ' + e.token_accounting, text: e.token_accounting }))));
    }
    $('#req-page').textContent = `${num(page.requests.length)} shown of ${num(page.total)} matching`;
    $('#req-next').disabled = !page.next_cursor;
    $('#req-prev').disabled = state.reqStack.length === 0;
    state.nextCursor = page.next_cursor;
  } catch (err) {
    if (aborted(err)) return;
    tableMessage(body, cols, 'Could not load requests', String(err.message || err), { error: true });
  }
}

// ── request detail + diff ──────────────────────────────────────────────────
/**
 * Myers-style LCS diff over lines, then rendered Git-style. This is the view both
 * reference implementations carry the data for and neither built: it answers
 * "what did context-guru actually remove or rewrite?" instead of asserting a
 * token count.
 */
function diffLines(a, b) {
  const n = a.length, m = b.length;
  // Trim the common head/tail first: agent transcripts share long identical
  // stretches, so this cuts the DP table to the part that actually differs.
  let head = 0;
  while (head < n && head < m && a[head] === b[head]) head++;
  let tail = 0;
  while (tail < n - head && tail < m - head && a[n - 1 - tail] === b[m - 1 - tail]) tail++;
  const as = a.slice(head, n - tail), bs = b.slice(head, m - tail);

  const out = [];
  for (let i = 0; i < head; i++) out.push({ op: ' ', text: a[i], ai: i + 1, bi: i + 1 });

  // Guard the quadratic table: a huge rewrite renders as a whole-block replace
  // rather than hanging the tab.
  // ponytail: LCS is O(n·m); the cap below is the ceiling. Switch to a real Myers
  // O(nd) if multi-megabyte single-message diffs ever matter.
  const LIMIT = 1500;
  if (as.length > LIMIT || bs.length > LIMIT) {
    if (as.length) out.push({ op: 'gap', text: `… ${as.length} lines replaced (too large to line-diff) …` });
    for (let i = 0; i < as.length; i++) out.push({ op: '-', text: as[i], ai: head + i + 1 });
    for (let j = 0; j < bs.length; j++) out.push({ op: '+', text: bs[j], bi: head + j + 1 });
  } else {
    const dp = Array.from({ length: as.length + 1 }, () => new Uint32Array(bs.length + 1));
    for (let i = as.length - 1; i >= 0; i--) {
      for (let j = bs.length - 1; j >= 0; j--) {
        dp[i][j] = as[i] === bs[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
      }
    }
    let i = 0, j = 0;
    while (i < as.length && j < bs.length) {
      if (as[i] === bs[j]) { out.push({ op: ' ', text: as[i], ai: head + i + 1, bi: head + j + 1 }); i++; j++; }
      else if (dp[i + 1][j] >= dp[i][j + 1]) { out.push({ op: '-', text: as[i], ai: head + i + 1 }); i++; }
      else { out.push({ op: '+', text: bs[j], bi: head + j + 1 }); j++; }
    }
    while (i < as.length) { out.push({ op: '-', text: as[i], ai: head + i + 1 }); i++; }
    while (j < bs.length) { out.push({ op: '+', text: bs[j], bi: head + j + 1 }); j++; }
  }
  for (let k = 0; k < tail; k++) {
    out.push({ op: ' ', text: a[n - tail + k], ai: n - tail + k + 1, bi: m - tail + k + 1 });
  }
  return out;
}

/** Collapse runs of unchanged lines to CTX lines of context, Git-style. */
function withHunks(rows, ctx = 3) {
  const keep = new Array(rows.length).fill(false);
  rows.forEach((r, i) => {
    if (r.op === ' ') return;
    for (let k = Math.max(0, i - ctx); k <= Math.min(rows.length - 1, i + ctx); k++) keep[k] = true;
  });
  const out = [];
  let skipped = 0;
  rows.forEach((r, i) => {
    if (keep[i]) {
      if (skipped) { out.push({ op: 'gap', text: `… ${skipped} unchanged lines …` }); skipped = 0; }
      out.push(r);
    } else skipped++;
  });
  if (skipped) out.push({ op: 'gap', text: `… ${skipped} unchanged lines …` });
  return out;
}

/**
 * DIFF_MODES are the four ways to look at one compaction, and they are four views of
 * ONE diff rather than four renderers: `orig` and `raw` are the before/after sides on
 * their own, `git` interleaves them, `side` puts them in aligned columns. Sharing the
 * LCS output is what keeps the line tints and the line numbers agreeing between modes.
 */
const DIFF_MODES = [
  ['orig', 'Before', 'diff-mode-orig'],
  ['raw', 'After', 'diff-mode-raw'],
  ['git', 'Inline diff', 'diff-mode-git'],
  ['side', 'Side by side', 'diff-mode-side'],
];

/** Render one whole side as plain text, with the changed lines still tinted. */
function renderOneSide(host, rows, side) {
  const keepOp = side === 'a' ? '-' : '+';
  // Focusable for the same reason as the other scrollers: keyboard users scroll it.
  const pre = el('pre', { tabindex: '0' });
  let any = false;
  for (const r of rows) {
    // The elision has to be visible here too. Dropping the gap markers made this view
    // claim to be the whole before/after text while quietly omitting every unchanged
    // run — two adjacent lines that are 200 lines apart in the real message.
    if (r.op === 'gap') { pre.appendChild(el('span', { class: 'sl pad' }, (r.text || '') + '\n')); continue; }
    if (r.op !== ' ' && r.op !== keepOp) continue;
    any = true;
    pre.appendChild(el('span', {
      class: 'sl' + (r.op === '-' ? ' del' : r.op === '+' ? ' add' : ''),
    }, (r.text || '') + '\n'));
  }
  if (!any) { emptyState(host, 'Nothing on this side', 'This message is empty in that version.'); return; }
  host.appendChild(el('div', { class: 'oneside' }, pre));
}

/**
 * renderSide lays the two versions out in aligned columns.
 *
 * The alignment is the point, and it is why this cannot be two independent <pre>
 * blocks. Two of them drift for two separate reasons, and padding by LINE COUNT — which
 * is what this used to do — fixes neither:
 *
 *   - the lines WRAP, so a 1,961-character deletion is 540px tall beside an 18px
 *     addition. Equal counts, unequal heights, 522px of drift measured on a real block,
 *     with SKU-0032 sitting opposite SKU-0023.
 *   - each <pre> was its own scroll box, so scrolling one desynchronised them even where
 *     the heights did agree.
 *
 * So the two sides are cells of ONE grid, a row per line pair: the taller cell sets its
 * row's height, both cells top-align inside it, and a single scroller wraps the whole
 * thing. Alignment is then a property of the layout rather than something this function
 * has to compute and keep true — which also makes the count-based padding unnecessary
 * rather than merely insufficient.
 */
function renderSide(host, rows) {
  const grid = el('div', { class: 'side', 'data-testid': 'side-grid' });
  // Headers are the grid's first row so they sit above their own column.
  grid.appendChild(el('div', { class: 'side-h side-l', text: 'Before your preset' }));
  grid.appendChild(el('div', { class: 'side-h side-r', text: 'After your preset' }));
  let dels = [], adds = [];
  const cell = (side, cls, text) => el('div', { class: 'side-' + side + ' sl ' + cls, text: text });
  const pair = (l, r) => { grid.appendChild(l); grid.appendChild(r); };
  const flush = () => {
    const n = Math.max(dels.length, adds.length);
    for (let i = 0; i < n; i++) {
      pair(cell('l', i < dels.length ? 'del' : 'pad', i < dels.length ? dels[i] : ''),
        cell('r', i < adds.length ? 'add' : 'pad', i < adds.length ? adds[i] : ''));
    }
    dels = []; adds = [];
  };
  for (const r of rows) {
    if (r.op === '-') { dels.push(r.text); continue; }
    if (r.op === '+') { adds.push(r.text); continue; }
    flush();
    const cls = r.op === 'gap' ? 'pad' : 'ctx';
    pair(cell('l', cls, r.text || ''), cell('r', cls, r.text || ''));
  }
  flush();
  // The single scroller is focusable so it can be scrolled from the keyboard: it is the
  // only way to reach the rest of a long diff without a pointer.
  host.appendChild(el('div', { class: 'side-scroll', tabindex: '0' }, grid));
}

/** `rows` lets a caller hand in the diff it already computed. Every block used to run
 *  the LCS TWICE — once for the toolbar tally, once here — so a large session paid for
 *  two quadratic passes per block and got one rendering out of it. */
function renderDiff(host, before, after, mode, rows) {
  clear(host);
  rows = rows || withHunks(diffLines((before || '').split('\n'), (after || '').split('\n')));
  if (mode === 'orig') { renderOneSide(host, rows, 'a'); return; }
  if (mode === 'raw') { renderOneSide(host, rows, 'b'); return; }
  if (mode === 'side') { renderSide(host, rows); return; }
  if (!rows.length) { emptyState(host, 'Identical', 'This message was not changed.'); return; }
  const frag = document.createDocumentFragment();
  for (const r of rows) {
    if (r.op === 'gap') {
      frag.appendChild(el('div', { class: 'dl gap' },
        el('span', { class: 'ln' }), el('span', { class: 'ln' }), el('span', { class: 'tx', text: r.text })));
      continue;
    }
    const cls = r.op === '+' ? 'add' : r.op === '-' ? 'del' : 'ctx';
    frag.appendChild(el('div', { class: 'dl ' + cls },
      el('span', { class: 'ln', text: r.ai || '' }),
      el('span', { class: 'ln', text: r.bi || '' }),
      el('span', { class: 'tx', text: r.text })));
  }
  host.appendChild(frag);
}

/**
 * CLIPPED matches the truncation sentinel apply.clip() appends to a trace text:
 * "…[+2559 bytes]" at the very end of the LAST line. It means the pipeline rewrote the
 * whole message but only the first apply.TraceTextCap bytes were captured — so the token
 * counts in the block summary are for the full message while the text below is a prefix.
 */
const CLIPPED = /…\[\+(\d+) bytes\]$/;

/**
 * markClipped turns each truncation sentinel into its own `gap` row — the same op the
 * elided-unchanged-run marker already uses, so all three renderers style it as a notice
 * instead of tinting it like a line of transcript. Runs AFTER the diff, on the rows, so
 * the diff itself is byte-for-byte what it was before.
 *
 * Returns the bytes not captured per side, for the block-level note.
 */
function markClipped(rows) {
  const missing = { a: 0, b: 0 };
  for (let i = rows.length - 1; i >= 0; i--) {
    const r = rows[i];
    if (r.op === 'gap') continue;
    const m = CLIPPED.exec(r.text || '');
    if (!m) continue;
    const n = Number(m[1]);
    // op ' ' means the sentinel survived as an unchanged line, i.e. both sides carry it.
    if (r.op === '-' || r.op === ' ') missing.a = Math.max(missing.a, n);
    if (r.op === '+' || r.op === ' ') missing.b = Math.max(missing.b, n);
    r.text = r.text.slice(0, -m[0].length);
    rows.splice(i + 1, 0, { op: 'gap', text:
      `… truncated here: ${num(n)} more bytes were not captured (the diff shows the first ` +
      `${num(CONTENT_CAP)} bytes; token counts above are for the whole message) …` });
  }
  return missing;
}

/** The cap the server says actually binds, from any transcript/request answer. Defaults to
 *  apply.TraceTextCap so a notice reads correctly before the first answer lands. */
let CONTENT_CAP = 4000;

/** Count added/removed lines, for the toolbar's tally, from rows the caller already has.
 *
 *  It counts the LCS result rather than deriving anything from before_tokens/after_tokens:
 *  those are a different unit and do not track the line counts at all. Measured over 300
 *  content rows, 0 of 300 had `(lines removed − lines added) == (before_tokens −
 *  after_tokens)`, and the median ratio between the two is 0.0 — the common case is one
 *  long line replaced by a marker plus a note, i.e. +1 line net for several hundred tokens
 *  saved. So there is no arithmetic shortcut here; the saving is one pass instead of two. */
function diffTally(rows) {
  let add = 0, del = 0;
  for (const r of rows) {
    if (r.op === '+') add++;
    else if (r.op === '-') del++;
  }
  return { add, del };
}

/**
 * MARKER matches the offload sentinel context-guru leaves behind. Its PRESENCE in the
 * after-text is what distinguishes a reversible offload from a lossy rewrite, which is
 * the only per-span signal the capture actually records.
 */
const MARKER = /<<cg:[A-Za-z0-9_-]{1,64}>>/;

/**
 * attribute answers "which components changed this message?" and says whether the
 * answer is RECORDED or INFERRED — a distinction the reader has to be able to see.
 *
 * Recorded: `c.components` is the list the pipeline wrote down, in the order the
 * components touched the message; reverted and panicked ones are already excluded. The
 * diff is their cumulative result, so all of them are named, not a single winner. One
 * entry is not a special case — it is a recorded answer that happens to have one name.
 *
 * Inferred: rows written before that field existed have no list, and an absent list
 * means UNKNOWN, not "nothing". For those, attribution falls back to the change's
 * shape — an after-text carrying a <<cg:HASH>> marker was produced by an offloader, one
 * without by a reformatter — narrowed to the components that ran and were not reverted.
 * Those chips stay dashed, so an inferred list never reads as a recorded one.
 */
function attribute(c, components) {
  if (c.components && c.components.length) {
    // kind per name, for the chip's colour dot only; an unlisted component gets a
    // neutral dot rather than a colour that would assert a kind we do not have.
    const kinds = new Map((components || []).map((x) => [x.component, x.kind || '']));
    return {
      recorded: true, names: c.components, kinds, marked: MARKER.test(c.after || ''),
      why: 'Recorded by the pipeline: the components that rewrote this message, in the order they touched it. The diff is their combined result.',
    };
  }
  const ran = (components || []).filter((x) => x.mutated && !x.reverted);
  const kind = MARKER.test(c.after || '') ? 'offload' : 'reformat';
  let cands = ran.filter((x) => (x.kind || '') === kind);
  let fallback = false;
  let why = kind === 'offload'
    ? 'The replacement carries a <<cg:HASH>> marker, so an offload component produced it — the original is recoverable with the expand tool.'
    : 'The replacement carries no marker, so a reformat component rewrote this text in place.';
  if (!cands.length) {
    // No component of the inferred kind ran. Fall back to everything that did rather
    // than claiming nothing produced a change that plainly happened.
    cands = ran;
    fallback = true;
    why = 'No component of the expected kind ran on this request, so this lists every component that did.';
  }
  return { recorded: false, kind, fallback, names: cands.map((x) => x.component), why };
}

const KIND_HUE = { offload: SERIES[0], reformat: SERIES[1] };

/**
 * attributionChips renders the attribution beside a change.
 *
 * Recorded lists are solid chips in order, separated by arrows: "Changed by, in order:
 * a → b" is a different statement from the inferred "Offloaded by one of a, b", and the
 * two must not read alike. Inferred lists keep dashed chips and say they are inferred.
 *
 * The KIND is carried by the verb rather than by a chip of its own — "Offloaded by" vs
 * "Rewritten by". A chip reading "offload" sat in the same row as the component chips
 * and read as a third candidate component, which is the opposite of what it meant.
 */
function attributionChips(c, components) {
  const a = attribute(c, components);
  if (!a.names.length) return null;

  if (a.recorded) {
    const box = el('div', { class: 'attrib' }, el('span', {
      class: 'small muted', title: a.why,
      text: a.names.length > 1 ? 'Changed by, in order:' : 'Changed by:',
    }));
    a.names.forEach((n, i) => {
      if (i) box.appendChild(el('span', { class: 'small muted', text: '→' }));
      box.appendChild(el('span', { class: 'chip', title: a.why },
        el('i', { style: 'background:' + (KIND_HUE[a.kinds.get(n)] || 'var(--fg-faint)') }), n));
    });
    // A marker in the after-text is an observation about the text, not an inference
    // about the author, so it still belongs here — the reader wants to know the
    // original can be got back.
    if (a.marked) {
      box.appendChild(el('span', {
        class: 'small muted', text: '· the original is recoverable from the marker',
        title: 'The replacement carries a <<cg:HASH>> marker, so the original text is recoverable with the expand tool.',
      }));
    }
    return box;
  }

  const hue = KIND_HUE[a.kind];
  // In the fallback the kind could NOT be inferred, so the verb stays neutral rather
  // than asserting "rewritten" over a list that may well be offloaders.
  const verb = a.fallback ? 'Changed by' : a.kind === 'offload' ? 'Offloaded by' : 'Rewritten by';
  const box = el('div', { class: 'attrib' }, el('span', {
    class: 'small muted', title: a.why, text: verb + (a.names.length > 1 ? ' one of' : ''),
  }));
  for (const n of a.names) {
    box.appendChild(el('span', { class: 'chip guess', title: a.why },
      el('i', { style: 'background:' + hue }), n));
  }
  box.appendChild(el('span', { class: 'small muted', title: a.why, text: a.fallback
    ? '· inferred: this request recorded no per-message attribution'
    : a.kind === 'offload'
      ? '· inferred from the marker; the original is recoverable from it'
      : '· inferred from the absence of a marker' }));
  return box;
}

/**
 * kvUnset prints a value the client never set as the WORD "unset", styled as absent.
 *
 * The distinction is the whole reason temperature and top_p are nullable columns:
 * `temperature: 0` means "be deterministic" and a missing temperature means "you choose".
 * An em dash was already honest, but italic "unset" cannot be misread as a dash-shaped
 * zero — and it never renders 0 for an absent value, which is the invariant.
 */
function kvMaybe(k, v, fmt) {
  if (v === null || v === undefined || v === '') {
    return el('div', {}, el('div', { class: 'k', text: k }),
      el('div', { class: 'v unset', text: 'unset' }));
  }
  return kv(k, fmt ? fmt(v) : String(v));
}

/** kvBand groups a set of pairs under a caption. */
function kvBand(title, testid, ...pairs) {
  return el('div', { class: 'kv-band' },
    el('h3', { text: title }),
    el('div', { class: 'kv', 'data-testid': testid }, ...pairs.flat().filter(Boolean)));
}

/**
 * diffBlock builds one collapsible before/after block for a rewritten message: the
 * summary line, the view-mode toolbar, the attribution chips and the diff itself.
 *
 * Shared by the single-request drawer and the whole-session view so the two cannot
 * drift into rendering the same data two different ways. `mode` seeds the view, and
 * the returned element exposes setMode so a session-wide toolbar can drive every
 * block at once.
 */
function diffBlock(c, components, opts = {}) {
  const saved = c.before_tokens - c.after_tokens;
  const det = el('details', { class: 'diff', 'data-testid': 'diff-block' });
  det.appendChild(el('summary', {
    text: `${opts.prefix || ''}${c.path} — ${compact(c.before_tokens)} → ${compact(c.after_tokens)} tokens ` +
          (saved > 0 ? `(saved ${compact(saved)})` : '(rewritten, no token saving)'),
  }));

  let mode = opts.mode || 'git';
  let bodyHost = null, rows = null;
  const buttons = [];

  // Everything below the summary is built ON FIRST OPEN, not up front. A closed <details>
  // shows none of it — not the tally, not the chips, not the diff — so building it eagerly
  // bought nothing and cost one LCS pass plus a few hundred DOM nodes per block. On the
  // worst session in this corpus that was 31,425 blocks × (2 LCS + ~30 nodes each), which
  // killed the renderer in 1.9 s. Deferring it makes an unopened block a summary line.
  const build = () => {
    if (bodyHost) return;
    // The ONE LCS pass. The tally and every view mode read the same rows.
    rows = withHunks(diffLines((c.before || '').split('\n'), (c.after || '').split('\n')));
    const missing = markClipped(rows);
    const t = diffTally(rows);
    const bar = el('div', { class: 'difftoolbar' });
    for (const [m, label, testid] of DIFF_MODES) {
      const b = el('button', {
        class: 'ghost small', 'data-testid': testid, 'data-mode': m,
        'aria-pressed': String(m === mode), onclick: () => setMode(m),
      }, label);
      buttons.push(b);
      bar.appendChild(b);
    }
    bar.appendChild(el('span', { class: 'spacer' }));
    bar.appendChild(el('span', { class: 'tally' },
      el('span', { class: 'del', text: '−' + num(t.del) }), ' / ',
      el('span', { class: 'add', text: '+' + num(t.add) }), ' lines'));
    det.appendChild(bar);

    const chips = attributionChips(c, components);
    if (chips) det.appendChild(el('div', { class: 'difftoolbar' }, chips));

    // The header/body disagreement, said out loud: the token counts in the summary are the
    // whole message's, the text below is a prefix. Without this the only signal was a
    // sentinel tinted like an ordinary transcript line.
    if (missing.a || missing.b) {
      det.appendChild(el('div', { class: 'state blocked', 'data-testid': 'diff-truncated',
        role: 'note' },
        el('div', { class: 'state-body' },
          el('strong', { text: 'Only the first ' + num(CONTENT_CAP) + ' bytes were captured' }),
          el('span', { text:
            [missing.a && num(missing.a) + ' bytes of the before-text',
              missing.b && num(missing.b) + ' bytes of the after-text'].filter(Boolean).join(' and ')
            + ' are not in this diff. The token counts in the heading above cover the whole '
            + 'message, so they will not match the text you can see here. '
            + (MARKER.test(c.after || '')
              ? 'The after-text carries a <<cg:HASH>> marker, so the full original is still '
                + 'recoverable through the expand tool.'
              : 'This is a capture limit, not a rewrite: nothing was lost from what was sent '
                + 'upstream.') }))));
    }

    // tabindex: this box scrolls, and a scroll region a keyboard user cannot focus is a
    // part of the diff they cannot reach at all (axe scrollable-region-focusable). Same
    // reason .tblwrap carries one.
    bodyHost = el('div', { class: 'diffbody', tabindex: '0', role: 'group',
      'aria-label': 'Diff of ' + c.path });
    det.appendChild(bodyHost);
    renderDiff(bodyHost, c.before, c.after, mode, rows);
  };

  const setMode = (next) => {
    mode = next;
    // A session-wide toolbar sets .open and calls this in the same tick, and `toggle`
    // fires asynchronously — so build here rather than trusting the event to have landed.
    if (det.open) build();
    if (!bodyHost) return;
    for (const b of buttons) b.setAttribute('aria-pressed', String(b.dataset.mode === next));
    renderDiff(bodyHost, c.before, c.after, next, rows);
  };

  det.addEventListener('toggle', () => { if (det.open) build(); });
  if (opts.open) { det.open = true; build(); }
  det.setMode = setMode;
  return det;
}

/** openDrawer shows the side panel with a title and a body node. Factored out of
 *  openRequest so the archive view opens the same panel rather than inventing a second
 *  one that would need its own close handling and focus behaviour. */
function openDrawer(title, node) {
  const drawer = $('#drawer');
  // Remember where focus came from so closing returns it there rather than dumping the
  // caret at the top of the document — for a keyboard user that means losing their
  // place in a 500-row table every time they inspect a request.
  if (drawer.hidden) drawerReturnFocus = document.activeElement;
  drawer.hidden = false;
  $('#scrim').hidden = false;
  setBackgroundInert(true);
  $('#drawer-title').textContent = title;
  const body = clear($('#drawer-body'));
  if (node) body.appendChild(node);
  $('#drawer-close').focus();
  return body;
}
let drawerReturnFocus = null;

/**
 * trapTab keeps Tab inside the open drawer.
 *
 * The drawer is aria-modal, and the background is not inert, so without this a
 * keyboard user tabs straight out of the dialog into the table behind it — the focus
 * ring disappears under the scrim and the only way back is Shift+Tab thirteen times.
 * The list is recomputed per keypress because the drawer's contents are rebuilt on
 * every open and every diff-mode switch.
 */
function trapTab(ev) {
  if (ev.key !== 'Tab') return;
  const drawer = $('#drawer');
  if (drawer.hidden) return;
  const stops = $$('button, a[href], input, select, textarea, summary, [tabindex]:not([tabindex="-1"])', drawer)
    .filter((n) => !n.disabled && n.offsetParent !== null && n.id !== 'drawer-end');
  if (!stops.length) return;
  if (!drawer.contains(document.activeElement)) { ev.preventDefault(); stops[0].focus(); return; }
  // Backwards off the close button: there is nothing before it, so wrap to the end.
  if (ev.shiftKey && document.activeElement === stops[0]) {
    ev.preventDefault();
    stops[stops.length - 1].focus();
  }
}

async function openRequest(id, fromURL) {
  // The open drawer is part of the address (see urlFor): a request is linkable, and
  // Back dismisses the drawer instead of undoing the user's last filter change.
  if (!fromURL) { state.drawer = { req: Number(id) }; syncURL(false); }
  const body = openDrawer('Request #' + id, null);
  loadingState(body, 5);
  try {
    const res = await fetch('/api/requests/' + id);
    if (!res.ok) throw new Error(res.status + ' ' + res.statusText);
    // content_archived is the server's answer to "this session's text moved to cold
    // storage and I tried to fetch it": without reading it here, the branch below
    // referenced an undeclared `archived` and threw on every request that HAS content.
    // capture_blocked_by names WHICH party's gate is shut, so the empty-diff panel can
    // stop telling people to change a setting that is not theirs.
    const { request: e, content_visible: visible, content_captured: captured,
      content_archived: archived, capture_blocked_by: blockedBy,
      content_cap_bytes: cap } = await res.json();
    if (cap) CONTENT_CAP = cap;
    clear(body);

    // Four captioned bands rather than twenty-four pairs in one grid. Same data, same
    // testid on the first band (the checks and the docs screenshots read it), but "what
    // this request was" / "what it moved" / "what it cost" are three questions and the flat
    // wall answered them in one undifferentiated scan.
    const priced = e.token_accounting === 'complete';
    body.appendChild(kvBand('Request', 'detail-summary',
      kv('Session', e.session_id || '—'),
      kv('When', when(e.ts)),
      kv('Model', e.model || '—'),
      kv('Provider', e.provider || '—'),
      kv('Agent', e.agent || '—'),
      kv(term('Preset', 'A named bundle of settings for your preset steps.'), e.preset || '—'),
      kv(term('Mode', 'How context-guru handled this request: changed it, passed it through, or only measured it.'), modeLabel(e.mode)),
      kv(term('Upstream status', 'The HTTP status the AI provider returned.'), e.status || '—'),
      kv('Messages', num(e.messages))));

    body.appendChild(kvBand('Tokens', 'detail-tokens',
      kv(term('Before → after', 'Tokens received from the agent, then tokens actually sent to the provider.'), compact(e.tokens_before) + ' → ' + compact(e.tokens_after)),
      kv(term('Removed (this request / distinct text)', 'First: tokens removed from this request. Second: distinct text removed, counted once even if it was also removed from earlier requests.'), compact(e.tokens_before - e.tokens_after) + ' / ' + compact(e.saved_unique)),
      kv(term('Eligible to remove', 'Tokens context-guru was allowed to try to shrink.'), compact(e.attempted_tokens)),
      kv(term('Left alone for cache', 'Tokens not touched because changing them would break the provider prompt cache.'), compact(e.frozen_tokens)),
      kv(term('New / cache read / cache write / output', 'Tokens billed at full price / read from the prompt cache / written to it / generated by the model.'),
        [e.fresh_input, e.cache_read, e.cache_write, e.output_tokens].map(compact).join(' / ')),
      kv(term('Usage data', 'complete: the provider reported all token counts. partial or missing: some are unknown, so no dollar figure is shown.'), e.token_accounting)));

    body.appendChild(kvBand('Cost and latency', 'detail-cost',
      kv('Cost (actual / baseline)', priced ? usd(e.cost_usd) + ' / ' + usd(e.baseline_cost_usd) : 'not priced'),
      kv(term('context-guru model cost', 'Cost of any model calls context-guru made itself.'), priced ? usd(e.cg_llm_cost_usd) : '—'),
      // Directly beneath the cost, because the cost on its own is the figure that makes
      // readers conclude the product is worthless: baseline − actual − our own spend is
      // what this one turn was actually worth.
      // price_estimated: the accountant could not rule out a cache break (no history for this
      // thread yet), so the gross saving is right and the net may be too high. Marked "≈", the
      // same way this page marks every other estimate.
      kv(term('Net saving', 'Baseline cost minus actual cost minus context-guru model cost.'
          + (e.estimate_held ? ' Not counted: there was no history to tell whether the cache was already warm, so no saving is booked. The estimate is shown on the next line.' : '')),
        priced ? usd(e.baseline_cost_usd - e.cost_usd - e.cg_llm_cost_usd) : '—'),
      e.estimate_held ? kv(term('Estimated saving (not counted)', 'What the accountant would have booked with no history, shown as a range: the low end prices every removed token at the cache-read rate, the high end at the write rate. The true saving lies between them, so it is in no total. A negative figure is a cost.'),
        priced ? (e.estimated_saved_usd < 0 ? '' : '≈ ') + estRange(e.estimated_saved_low_usd || 0, e.estimated_saved_usd || 0) : '—') : null,
      // On a turn whose cache HIT this is usually the largest money figure on the row,
      // and it was not reported anywhere: the cache reads this request was billed for,
      // against the fresh rate they would have cost without the cache.
      kv(term('Provider cache saving', 'Dollars the provider prompt cache saved on this request.'), priced ? usd(e.cache_saved_usd) : '—'),
      kv('Delay added by context-guru', ms(e.cg_latency_ms)),
      kv('Provider response time', ms(e.upstream_ms)),
      kv(term('Restores', 'Times the agent asked for removed text to be put back.'), num(e.expands) + ' (' + compact(e.expand_tokens) + ' tok)'),
      kv(term('Reverts', 'Times a change was undone and the original request sent.'), num(e.reverts)),
      kv(term('Cache result', 'Why the provider cache did or did not hit.'), e.cache_miss_reason || '—'),
      kv(term('Why your preset left it alone', 'The reason nothing was removed, or "trimmed" if something was.'), e.uncompressed_reason || 'trimmed')));

    // The request's own metadata: what the client ASKED for. Separate from the bands above,
    // which are what happened to it. kvMaybe() prints an absent value as "unset" and a zero
    // as 0 — the API sends null for "not set" and 0 for "be deterministic", and collapsing
    // the two would misreport every deterministic request.
    body.appendChild(el('h2', { text: 'What the request asked for' }));
    body.appendChild(el('div', { class: 'kv', 'data-testid': 'detail-meta' },
      kvMaybe('Reasoning effort', e.reasoning_effort),
      kv('Thinking', e.thinking_mode
        ? e.thinking_mode + (e.thinking_budget ? ' (' + compact(e.thinking_budget) + ' token budget)' : '')
        : 'unset'),
      kvMaybe('Temperature', e.temperature),
      kvMaybe('top_p', e.top_p),
      kvMaybe('max_tokens', e.max_tokens || null, num),
      kv('Streaming', e.stream ? 'yes' : 'no'),
      kvMaybe('Tool choice', e.tool_choice),
      kv('Tools declared', num(e.tools)),
      kv('System blocks', num(e.system_blocks)),
      kvMaybe('Stop reason', e.stop_reason)));

    // Placement, not just a count: tools and system render ahead of messages, so a
    // breakpoint's LOCATION decides how much of the prefix it protects. Four labelled slots
    // rather than one run-on string that wrapped mid-clause and read as a sentence.
    const bpTotal = e.cache_bp_system + e.cache_bp_tools + e.cache_bp_messages + e.cache_bp_blocks + (e.cache_bp_root || 0);
    body.appendChild(el('div', { class: 'kv' },
      // Full width: four labelled slots do not fit a 184px grid cell, and wrapped over
      // three lines they stopped reading as four separate measurements.
      el('div', { class: 'kv-wide' },
        el('div', { class: 'k', text: 'Cache markers (cache_control) · ' + num(bpTotal) + ' of the provider’s 4' }),
        el('div', { class: 'v' }, el('div', { class: 'bp', 'data-testid': 'detail-breakpoints' },
          el('span', { class: e.cache_bp_system ? 'on' : '' }, 'system ', el('b', { text: num(e.cache_bp_system) })),
          el('span', { class: e.cache_bp_tools ? 'on' : '' }, 'tools ', el('b', { text: num(e.cache_bp_tools) })),
          el('span', { class: e.cache_bp_messages ? 'on' : '' }, 'messages ', el('b', { text: num(e.cache_bp_messages) })),
          el('span', { class: e.cache_bp_blocks ? 'on' : '' }, 'blocks ', el('b', { text: num(e.cache_bp_blocks) })),
          el('span', { class: e.cache_bp_root ? 'on' : '' }, 'top-level ', el('b', { text: num(e.cache_bp_root || 0) }))))),
      kv('Account id', e.tenant_id || '— (single account)')));

    // The log lines this request emitted. Text, not a link: Grafana binds loopback on the
    // box (see renderLogsHelp).
    if (e.session_id) body.appendChild(logQueryBlock(e.session_id, e.tenant_id));

    body.appendChild(el('h2', { text: 'Components, in the order they ran' }));
    if (!e.components || !e.components.length) {
      body.appendChild(el('div', { class: 'empty', text: 'No components ran on this request.' }));
    } else {
      const tbl = el('table', { class: 'tbl compact', 'data-testid': 'detail-components' },
        el('thead', {}, el('tr', {},
          el('th', { text: '#' }), el('th', { text: 'Component' }), el('th', { text: 'Kind' }),
          el('th', { class: 'num', text: 'Saved' }), el('th', { class: 'num', text: 'Unique' }),
          el('th', { class: 'num', text: 'Latency' }), el('th', { text: 'Outcome' }),
          el('th', { text: 'Why / what happened' }))));
      const tb = el('tbody');
      e.components.forEach((c, i) => {
        const outcome = c.reverted ? ['reverted', 'missing'] : c.skipped ? ['skipped', 'neutral']
          : c.acted ? ['acted', 'complete'] : ['mutated only', 'partial'];
        tb.appendChild(el('tr', {},
          el('td', { text: i + 1 }),
          el('td', {}, el('code', { text: c.component })),
          el('td', { text: c.kind || '—' }),
          el('td', { class: 'num', text: compact(c.saved_gross) }),
          el('td', { class: 'num', text: compact(c.saved_unique) }),
          el('td', { class: 'num', text: ms(c.duration_ms) }),
          el('td', {}, el('span', { class: 'pill ' + outcome[1], text: outcome[0] }),
            c.err ? el('div', { class: 's', text: c.err }) : null),
          el('td', {}, activitySummary(c.gates, c.events))));
      });
      tbl.appendChild(tb);
      body.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, tbl));
    }

    // Recorded model calls. This is the only place the cost of one extraction call is
    // visible: the request row carries a single rolled-up dollar figure, and the components
    // table has no dollars at all, so an expensive component could never be shown to be
    // underwater on a particular KIND of call.
    if (e.extractions && e.extractions.length) {
      body.appendChild(el('h2', { text: 'Preset model calls' }));
      const perTok = savedPerTok(e);
      const net = perTok === null ? null
        : e.extractions.reduce((a, x) => a + netUSD(x, perTok), 0);
      const spent = e.extractions.reduce((a, x) => a + (x.cost_usd || 0), 0);
      body.appendChild(el('div', { class: 'note' },
        `${e.extractions.length} call(s), spent ${usd(spent)}, net `,
        el('strong', { class: net !== null && net < 0 ? 'warn-text' : '', text: usd(net) }),
        net === null ? ' — this request is not fully priced, so the calls cannot be valued.'
          : net < 0 ? ' — these calls cost more than the tokens they removed were worth.' : '.'));
      const xt = el('table', { class: 'tbl compact', 'data-testid': 'detail-extractions' },
        el('thead', {}, el('tr', {},
          el('th', { text: '#' }), el('th', { text: 'Component' }), el('th', { text: 'Target' }),
          el('th', { class: 'num', text: 'Candidate' }), el('th', { class: 'num', text: 'Saved' }),
          el('th', { class: 'num', text: 'Prompt' }), el('th', { class: 'num', text: 'Cost' }),
          el('th', { class: 'num', text: 'Net' }), el('th', { class: 'num', text: 'Latency' }),
          el('th', { text: 'Outcome' }))));
      const xb = el('tbody');
      e.extractions.forEach((x, i) => {
        const n = netUSD(x, perTok);
        xb.appendChild(el('tr', {},
          el('td', { text: i + 1 }),
          el('td', {}, el('code', { text: x.component }),
            x.cold ? el('span', { class: 'pill complete', text: 'cold sweep' }) : null,
            x.escalated ? el('span', { class: 'pill neutral', text: 'escalated' }) : null),
          el('td', { text: x.aggressiveness || '—' }),
          el('td', { class: 'num', text: compact(x.candidate_tokens) }),
          el('td', { class: 'num', text: compact(x.saved_tokens) }),
          el('td', { class: 'num', text: compact(x.prompt_tokens) }),
          el('td', { class: 'num', text: usd(x.cost_usd) }),
          el('td', { class: 'num ' + (n !== null && n < 0 ? 'warn-text' : ''), text: usd(n) }),
          el('td', { class: 'num', text: ms(x.latency_ms) }),
          el('td', {},
            el('span', {
              class: 'pill ' + (x.accepted ? 'complete' : 'neutral'),
              text: x.accepted ? 'accepted' : 'no reduction',
            }),
            x.summary ? el('div', { class: 's', text: x.summary }) : null,
            x.rejection ? el('div', { class: 's warn-text', text: x.rejection }) : null,
            x.gate_reason ? el('div', { class: 's', text: x.gate_reason }) : null)));
      });
      xt.appendChild(xb);
      body.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, xt));
      // The before/after of each call, where the account stores transcripts at all.
      e.extractions.forEach((x, i) => {
        if (!x.before && !x.after) return;
        const d = el('details', { class: 'field' },
          el('summary', { text: `Call ${i + 1}: what the model removed` }));
        const host = el('div', {});
        renderDiff(host, x.before || '', x.after || '', 'unified');
        d.appendChild(host);
        body.appendChild(d);
      });
    }

    body.appendChild(el('h2', { text: 'What context-guru changed' }));
    body.appendChild(el('div', { class: 'note' },
      'Whole-session view: ',
      el('button', {
        class: 'subtle', 'data-testid': 'open-session-diff',
        onclick: () => openSessionDiff(e.session_id),
      }, 'every change in this session')));
    // The order matters, and so does what each branch is allowed to conclude. Absent
    // content means "we do not have the text", NEVER "there was no change" — only the
    // component rows and the compaction outcome may decide that.
    const hasContent = !!(e.content && e.content.length);
    const rewritten = wasRewritten(e);
    if (!visible) {
      contentBlockedState(body, 'not_permitted');
    } else if (!hasContent && archived) {
      // The server tried the fetch for this one request and could not complete it. Say
      // which of the two it was rather than showing an empty diff.
      contentBlockedState(body, 'unreachable', { session: e.session_id });
    } else if (!hasContent && !captured) {
      contentBlockedState(body, 'not_captured', { blockedBy, rewritten });
    } else if (!hasContent && rewritten) {
      // Capture is on NOW and this request was compacted, so the text is missing for a
      // reason of its own: it ran before capture was enabled, or it exceeded the cap.
      contentBlockedState(body, 'not_stored', { rewritten });
    } else if (!hasContent) {
      // Into its own box: emptyState CLEARS the host it is given, and given `body` it
      // deleted the metrics and the component table it was talking about — leaving a
      // drawer whose only content was the claim, with none of the evidence for it.
      emptyState(body.appendChild(el('div')), 'Nothing was rewritten',
        'This request passed through unchanged' + (e.uncompressed_reason ? ' (' + e.uncompressed_reason + ')' : '') + '.');
    } else {
      // Biggest saving first, and open that one: the point of the view is "what did
      // context-guru actually remove?", so leading with an unchanged block (and
      // collapsing the 2k-token rewrite below it) buries the answer.
      const blocks = e.content.slice().sort(
        (a, b) => (b.before_tokens - b.after_tokens) - (a.before_tokens - a.after_tokens));
      blocks.forEach((c, idx) => {
        body.appendChild(diffBlock(c, e.components, {
          open: idx === 0 && c.before_tokens > c.after_tokens,
        }));
      });
    }
  } catch (err) {
    errorState(clear(body), 'Could not load this request', err);
  }
}

/**
 * contentBlockedState renders the "why is there no transcript here" panel.
 *
 * Four distinct answers, never one blank panel, because they are four different
 * situations and only two of them are anything the reader can do something about:
 * capture is a setting they own, cold storage is a button, permission is neither.
 */
/** The one sentence every "no transcript" panel owes a reader whose request WAS
 *  compacted: the change happened, only its text is missing. */
function compactedAnywayNote() {
  return el('span', { class: 'warn-text', text:
    'This request was trimmed all the same — the component rows above are what ran and ' +
    'what each one removed. What is missing here is the stored before/after text, not the ' +
    'change itself.' });
}

function contentBlockedState(host, state, opts = {}) {
  const box = el('div', { class: 'state blocked' });
  const b = el('div', { class: 'state-body' });
  switch (state) {
    case 'not_permitted':
      box.className = 'state blocked';
      b.appendChild(el('strong', { text: 'Transcripts are not yours to read' }));
      b.appendChild(el('span', { text:
        'Metrics for this traffic are visible; its content is not. On a hosted deployment the ' +
        'owning account and the manager can read it. On a single-tenant proxy, content is ' +
        'served to loopback or a configured trusted CIDR only.' }));
      break;
    case 'not_captured':
      // WHOSE gate is shut decides what to say. Storing a transcript needs the operator's
      // service-wide gate AND the account's own consent, and the panel used to name only
      // the second — so a user whose consent was already on was told to switch on a
      // setting they had switched on, while the gate that was actually closed was one
      // they cannot reach.
      b.appendChild(el('strong', { text: 'Content was not captured' }));
      if (opts.blockedBy === 'operator') {
        b.appendChild(el('span', { text:
          'Storing transcripts is switched off service-wide on this proxy, so nothing was ' +
          'recorded for any account and your own consent setting makes no difference until ' +
          'that changes. Only whoever operates this proxy can turn it on ' +
          '(--dashboard-content); ask them. Capture is not retroactive, so even then this ' +
          'request stays empty.' }));
      } else if (opts.blockedBy === 'tenant') {
        b.appendChild(el('span', { text:
          'Storing transcripts is off by default and opt-in per account, so there is no ' +
          'before/after text to diff. Turn on "Store my transcripts for the diff view" in ' +
          'Settings to record it from the next request on. It is not retroactive.' }));
      } else {
        // capture_blocked_by is '' with content_captured false: more than one account is
        // in view, so there is no single consent setting to report — and guessing at one
        // would name the wrong party.
        b.appendChild(el('span', { text:
          'No transcripts are stored for this view. Storing them is decided per account, ' +
          'and this view spans more than one, so there is no single setting to report here. ' +
          "Open one account's traffic to see whether its transcripts are being stored." }));
      }
      if (opts.rewritten) b.appendChild(compactedAnywayNote());
      break;
    case 'not_stored':
      // Capture is on and this request still has no text. The component table above says
      // it WAS compacted, so the honest answer is about storage, not about change.
      b.appendChild(el('strong', { text: 'The text of this request was not stored' }));
      b.appendChild(el('span', { text:
        'Storing transcripts is on now, but it is not retroactive and it is size-capped: a ' +
        'request that ran before it was switched on, or whose messages were larger than the ' +
        'per-blob cap, has metrics and no text. There is nothing to recover for this one — ' +
        'requests from here on will have their before/after stored.' }));
      b.appendChild(compactedAnywayNote());
      break;
    case 'unknown_session':
      b.appendChild(el('strong', { text: 'No such session' }));
      b.appendChild(el('span', { text:
        'Nothing here has this session id: it was never captured, it has aged out of ' +
        'retention, or it belongs to another account. A link to a session that has since ' +
        'been pruned lands exactly here.' }));
      break;
    case 'capture_not_permitted':
      b.appendChild(el('strong', { text: 'Capture health is per-process, so it is manager-only' }));
      b.appendChild(el('span', { text:
        'The queue, drop and write counters cover every account this proxy serves, which ' +
        'makes them a read on other tenants\' traffic volume. One consequence is worth ' +
        'knowing: if the capture queue overflows, YOUR figures under-report and this page ' +
        'cannot tell you. Ask whoever operates the proxy — they see the counter.' }));
      break;
    case 'never_archived':
      b.appendChild(el('strong', { text: 'Never written to cold storage' }));
      b.appendChild(el('span', { text:
        'The index says this session has no archived object, so the transcripts were dropped by ' +
        'retention rather than moved. The metrics above are all that remains of it.' }));
      break;
    case 'unreachable':
      box.className = 'state failed';
      b.appendChild(el('strong', { text: 'Cold storage is unreachable' }));
      b.appendChild(el('span', { text:
        'The transcripts are safe where they are — this is a failure to reach the remote, not a ' +
        'loss. Try again shortly. ' + (opts.error || '') }));
      break;
    default:
      b.appendChild(el('strong', { text: 'No transcript' }));
  }
  box.appendChild(b);
  if (opts.action) box.appendChild(opts.action);
  host.appendChild(box);
  return box;
}

/** The page behind an open dialog is inert: not clickable, not tabbable, and not read
 *  out. Without it the drawer was aria-modal in name only — Tab walked straight into the
 *  table behind the scrim. */
function setBackgroundInert(on) {
  for (const sel of ['.topbar', '.filters', '#main', '.skip']) {
    const n = $(sel);
    if (n) n.inert = on;
  }
}

/** closeDrawer is what the ✕, Escape and the scrim call. It drops the drawer out of the
 *  URL by REPLACING the entry rather than going back one: a bookmark opened straight into
 *  a drawer has no previous entry of ours to return to, and history.back() there would
 *  leave the app. Back still closes the drawer — through popstate → syncDrawer. */
function closeDrawer() {
  if (state.drawer) { state.drawer = null; syncURL(true); }
  dismissDrawer();
}

function dismissDrawer() {
  setBackgroundInert(false);
  $('#drawer').hidden = true;
  $('#scrim').hidden = true;
  if (drawerReturnFocus && document.contains(drawerReturnFocus)) drawerReturnFocus.focus();
  drawerReturnFocus = null;
}

// ── session compaction diff ────────────────────────────────────────────────
//
// "Show me what compaction did to this conversation." The request drawer answers it for
// one turn; this answers it for the whole session, which is the level at which the
// question is actually asked.
//
// Two things it is careful about:
//
//   - It does NOT reconstruct a transcript. What was captured is the messages we
//     REWROTE, not the conversation around them, so presenting a stitched-together
//     "session before compaction" would be a fabrication wherever we touched nothing.
//     The before/after views are the before/after of every span we changed, in order,
//     and the heading says exactly that.
//   - Cold storage is never touched implicitly. The first load reads local data only
//     and reports state="cold"; the fetch happens on a button, once, per the user.

/** transcriptURL builds the route, carrying only the manager's tenant selector — never
 *  the view filters, which would silently drop turns out of the middle of a session. */
function transcriptURL(session, fetchCold, after) {
  const p = new URLSearchParams();
  if (state.filter.tenant) p.set('tenant', state.filter.tenant);
  if (fetchCold) p.set('fetch', '1');
  if (after) p.set('after', after);
  const q = p.toString();
  return '/api/sessions/' + encodeURIComponent(session) + '/transcript' + (q ? '?' + q : '');
}

/**
 * The diff drawer's pager. One session at a time, in memory.
 *
 * `pages[i]` is the keyset cursor for page i plus the 0-based turn number it starts at,
 * so "turn 3" keeps meaning the third turn of the CONVERSATION on page 7, and Previous
 * is a step back to a cursor already seen rather than a re-page from the start.
 *
 * ponytail: not in the URL. The router lives in another file, and a cursor pasted into a
 * shared link would pin the reader to a page boundary of a session that has since grown —
 * a link to a session is the honest thing to share. Move it into the hash if someone
 * actually asks to deep-link a page.
 */
let diffPager = { session: null, pages: [{ cursor: '', offset: 0 }], page: 0 };

async function openSessionDiff(session, fetchCold, fromURL, page) {
  // Linkable, like the request drawer: this view is the one people want to send someone.
  if (!fromURL) { state.drawer = { diff: session }; syncURL(false); }
  // A different session resets the pager; a re-open of the same one (the cold-storage
  // fetch button re-enters here) keeps the page you were on.
  if (diffPager.session !== session) diffPager = { session, pages: [{ cursor: '', offset: 0 }], page: 0 };
  if (page != null) diffPager.page = page;
  const at = diffPager.pages[diffPager.page] || diffPager.pages[0];
  const body = openDrawer('Preset diff · ' + (session || '(no session id)'), null);
  loadingState(body, 5);
  let out;
  try {
    const res = await fetch(transcriptURL(session, fetchCold, at.cursor), { headers: { accept: 'application/json' } });
    if (!res.ok) {
      let msg = res.status + ' ' + res.statusText;
      let j = null;
      try { j = await res.json(); } catch (_) { /* not json */ }
      // The 404 carries a state like every other answer, so a stale bookmark renders "no
      // such session" through the normal renderer instead of throwing an HTTP error at
      // someone who did nothing wrong.
      if (j && j.state) { renderSessionDiff(body, session, j); return; }
      const e = new Error(msg); e.status = res.status; throw e;
    }
    out = await res.json();
  } catch (err) {
    errorState(clear(body), 'Could not load this session', err);
    return;
  }
  // Learn the NEXT page's coordinates from the answer we just got: keyset pagination has
  // no page numbers, so the only way forward is the cursor the server handed back.
  if (out.next_cursor) {
    diffPager.pages[diffPager.page + 1] = {
      cursor: out.next_cursor, offset: at.offset + (out.requests || []).length,
    };
  }
  renderSessionDiff(body, session, out, at.offset);
}

function renderSessionDiff(body, session, out, offset = 0) {
  clear(body);
  if (out.content_cap_bytes) CONTENT_CAP = out.content_cap_bytes;
  if (out.state === 'unknown_session') {
    contentBlockedState(body, 'unknown_session');
    return;
  }
  const reqs = out.requests || [];

  // Totals from the rows themselves: an archived session has no local aggregate to ask
  // for, and recomputing here means the two paths agree.
  let before = 0, after = 0, unique = 0, changed = 0, cgMs = 0;
  const compTotals = new Map();
  for (const e of reqs) {
    before += e.tokens_before || 0;
    after += e.tokens_after || 0;
    unique += e.saved_unique || 0;
    cgMs += e.cg_latency_ms || 0;
    changed += (e.content || []).length;
    for (const c of e.components || []) {
      const t = compTotals.get(c.component) || { kind: c.kind, runs: 0, acted: 0, saved: 0, reverted: 0, ms: 0 };
      t.runs++;
      if (c.acted) t.acted++;
      if (c.reverted) t.reverted++;
      t.saved += c.saved_unique || 0;
      t.ms += c.duration_ms || 0;
      compTotals.set(c.component, t);
    }
  }

  // Three different populations, and the old summary called the first one by the second's
  // name: "Turns captured: 12" was the session's TOTAL turn count while only 4 turns had
  // any stored text, and "Messages rewritten" silently counted only those 4 turns'
  // messages even though components acted on the other 8.
  const rewrittenTurns = reqs.filter(wasRewritten).length;
  const storedTurns = reqs.filter((e) => (e.content || []).length).length;
  const partial = storedTurns < rewrittenTurns;

  // The route is paged, so every total above is a total over THIS PAGE. Saying so is not a
  // nicety: an unlabelled "Tokens before → after" over 50 of 1,310 turns is a wrong number
  // about the session, which is the level at which the reader is asking.
  const total = out.total || reqs.length;
  const paged = total > reqs.length;
  const from = reqs.length ? offset + 1 : 0;
  const to = offset + reqs.length;
  const per = paged ? ' (turns ' + num(from) + '–' + num(to) + ')' : '';

  body.appendChild(el('div', { class: 'kv', 'data-testid': 'session-diff-summary' },
    kv('Session', session || '(none)'),
    kv('Turns in this session', num(total)
      + (paged ? ' — showing ' + num(from) + '–' + num(to) : '')),
    kv('Turns rewritten' + per, num(rewrittenTurns)),
    kv('Turns with stored text' + per, storedTurns + ' of ' + rewrittenTurns + ' rewritten'),
    kv('Messages rewritten' + per + (partial ? ' (stored turns only)' : ''), num(changed)),
    kv('Tokens before → after' + per, compact(before) + ' → ' + compact(after)),
    kv('Saved (gross / unique)' + per, compact(before - after) + ' / ' + compact(unique)),
    kv('context-guru latency' + per, dur(cgMs)),
    kv('Window' + per, reqs.length
      ? when(reqs[0].ts) + ' → ' + when(reqs[reqs.length - 1].ts)
      : (out.archive ? when(out.archive.first_ts) + ' → ' + when(out.archive.last_ts) : '—'))));

  // The state strip decides whether there is anything to diff, and is the only place
  // the cold-storage fetch can be started from.
  const st = out.state;
  if (st === 'cold') {
    const btn = el('button', {
      class: 'primary', 'data-testid': 'fetch-transcript',
      onclick: async () => {
        btn.disabled = true;
        btn.textContent = 'fetching from cold storage…';
        // Re-enter through the same path so success and every failure mode render
        // through exactly one renderer.
        await openSessionDiff(session, true);
      },
    }, 'Fetch transcript');
    const box = el('div', { class: 'state cold', 'data-testid': 'state-cold' },
      el('div', { class: 'state-body' },
        el('strong', { text: 'Transcripts are in cold storage' }),
        el('span', { text:
          'The metrics above are local and complete. The before/after text moved to ' +
          (out.remote || 'the configured remote') + ' and is not read until you ask — it is a ' +
          'network round trip, so it never happens on page load or for a list. ' +
          (out.archive ? 'Archived ' + when(out.archive.archived_at) + ', ' +
            compact((out.archive.content_bytes || 0) + (out.archive.full_bytes || 0)) + 'B compressed.' : '') })),
      btn);
    body.appendChild(box);
  } else if (st === 'not_permitted' || st === 'not_captured' || st === 'never_archived') {
    contentBlockedState(body, st, { blockedBy: out.capture_blocked_by, rewritten: rewrittenTurns > 0 });
  } else if (st === 'unreachable') {
    const retry = el('button', {
      class: 'ghost', 'data-testid': 'retry-transcript',
      onclick: () => openSessionDiff(session, true),
    }, 'Try again');
    contentBlockedState(body, 'unreachable', { error: out.error, action: retry });
  } else if (st === 'fetched') {
    body.appendChild(el('div', { class: 'state', 'data-testid': 'state-fetched' },
      el('div', { class: 'state-body' },
        el('strong', { text: 'Fetched from cold storage' }),
        el('span', { text:
          'Read-only. Nothing was written back to local disk, so this does not re-trigger the ' +
          'eviction that archived it in the first place.' }))));
  }

  if (compTotals.size) {
    body.appendChild(el('h2', { text: paged
      ? 'Components, across turns ' + num(from) + '–' + num(to)
      : 'Components, across the session' }));
    const rows = Array.from(compTotals.entries()).sort((a, b) => b[1].saved - a[1].saved);
    const tbl = el('table', { class: 'tbl compact', 'data-testid': 'session-diff-components' },
      el('thead', {}, el('tr', {},
        el('th', { text: 'Component' }), el('th', { text: 'Kind' }),
        el('th', { class: 'num', text: 'Runs' }), el('th', { class: 'num', text: 'Acted' }),
        el('th', { class: 'num', text: 'Reverted' }), el('th', { class: 'num', text: 'Unique saved' }),
        el('th', { class: 'num', text: 'Own latency' }))));
    const tb = el('tbody');
    for (const [name, t] of rows) {
      tb.appendChild(el('tr', {},
        el('td', {}, el('code', { text: name })),
        el('td', { text: t.kind || '—' }),
        el('td', { class: 'num', text: num(t.runs) }),
        el('td', { class: 'num', text: num(t.acted) }),
        el('td', { class: 'num', text: num(t.reverted) }),
        el('td', { class: 'num', text: compact(t.saved) }),
        el('td', { class: 'num', text: dur(t.ms) })));
    }
    tbl.appendChild(tb);
    body.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, tbl));
  }

  const withContent = reqs.filter((e) => (e.content || []).length);
  if (!withContent.length) {
    if (st === 'hot' || st === 'fetched' || st === 'nothing_changed') {
      body.appendChild(el('h2', { text: 'What your preset changed' }));
      // Same rule as the single request: only the component rows may say "nothing was
      // rewritten". With no stored text and turns that WERE rewritten, the honest answer
      // is about storage.
      if (rewrittenTurns) contentBlockedState(body, 'not_stored', { rewritten: true });
      else {
        // Own box: emptyState clears its host, and given `body` it wiped the session
        // summary and the component table above it.
        emptyState(body.appendChild(el('div')), 'Nothing was rewritten in this session',
          'Every turn passed through unchanged. The per-request rows say why under "preset outcome".');
      }
    }
    return;
  }

  // Partial coverage is disclosed, not left to be inferred from a block list that starts
  // at "turn 9".
  if (partial) {
    body.appendChild(el('div', { class: 'state blocked', 'data-testid': 'partial-coverage' },
      el('div', { class: 'state-body' },
        el('strong', { text: 'Partial coverage: ' + (rewrittenTurns - storedTurns) + ' of ' +
          rewrittenTurns + ' rewritten turns have no stored text' }),
        el('span', { text:
          'The blocks below are the ' + storedTurns + ' turn(s) whose before/after was stored, so ' +
          '"messages rewritten" counts those only and undercounts the session. The missing ' +
          'turns ran before transcript storage was switched on, or exceeded its size cap — ' +
          'their metrics and component rows are complete either way.' }))));
  }

  body.appendChild(el('h2', { text: 'What your preset changed, oldest turn first' }));
  body.appendChild(el('p', { class: 'note', text:
    'Each block is one message context-guru rewrote. This is the set of spans we touched — ' +
    'not a reconstructed transcript: the messages we left alone were never captured, so ' +
    'stitching them into a whole conversation would be inventing the parts we did not store.' }));

  // Say the size BEFORE rendering it. A block count is the one number that predicts what
  // this panel is about to cost, and the page it belongs to is the answer to "where is the
  // rest of it" — a session in this corpus reaches 31,425 blocks, and the panel used to
  // open on all of them at once with no warning and no way out.
  const blockCount = withContent.reduce((n, e) => n + e.content.length, 0);
  body.appendChild(el('p', { class: 'note', 'data-testid': 'diff-scale', text:
    num(blockCount) + ' block(s) on this page, from ' + num(storedTurns) + ' stored turn(s)'
    + (paged ? ' of ' + num(total) + ' in the session. Blocks are collapsed until you open '
      + 'one — expanding all of them renders every line of all ' + num(blockCount) + '.'
      : '. Blocks are collapsed until you open one.') }));

  const pager = () => {
    if (!paged) return null;
    const prev = el('button', {
      class: 'ghost', disabled: diffPager.page === 0,
      onclick: () => openSessionDiff(session, false, true, diffPager.page - 1),
    }, 'Previous turns');
    const next = el('button', {
      class: 'ghost', disabled: !out.next_cursor,
      onclick: () => openSessionDiff(session, false, true, diffPager.page + 1),
    }, 'Next turns');
    return el('div', { class: 'pager', 'data-testid': 'diff-pager' }, prev,
      el('span', { text: 'Turns ' + num(from) + '–' + num(to) + ' of ' + num(total) }), next);
  };
  const topPager = pager();
  if (topPager) body.appendChild(topPager);

  // One toolbar drives every block, which is what makes (a) "the session before
  // compaction" and (b) "after" single clicks rather than N expansions.
  const blocks = [];
  const bar = el('div', { class: 'difftoolbar', 'data-testid': 'session-diff-modes' },
    el('span', { text: 'All blocks:' }));
  const allButtons = [];
  for (const [m, label] of DIFF_MODES) {
    const b = el('button', {
      class: 'ghost small', 'data-mode': m, 'aria-pressed': String(m === 'git'),
      onclick: () => {
        for (const x of allButtons) x.setAttribute('aria-pressed', String(x.dataset.mode === m));
        for (const blk of blocks) { blk.open = true; blk.setMode(m); }
      },
    }, label);
    allButtons.push(b);
    bar.appendChild(b);
  }
  bar.appendChild(el('span', { class: 'spacer' }));
  bar.appendChild(el('button', {
    class: 'ghost small',
    title: 'Renders every line of all ' + num(blockCount) + ' blocks on this page',
    onclick: () => { for (const blk of blocks) blk.open = !blk.open; },
  }, 'Expand / collapse all ' + num(blockCount)));
  body.appendChild(bar);

  // Turn numbers come from the position in the FULL request list, not in the filtered
  // one: "turn 3" has to mean the third turn of the conversation, not the third turn
  // that happened to be rewritten.
  // `offset` is this page's first turn, so a turn number is the conversation's, not the
  // page's: on page 7 the first block is "turn 301", not "turn 1".
  const turnOf = new Map(reqs.map((e, i) => [e, offset + i + 1]));
  for (const e of withContent) {
    for (const c of e.content) {
      const blk = diffBlock(c, e.components, {
        prefix: 'turn ' + turnOf.get(e) + ' · ',
        open: blocks.length === 0,
      });
      blocks.push(blk);
      body.appendChild(blk);
    }
  }
  // A second pager at the bottom: the top one is 400 blocks up by the time you have read
  // the page, and scrolling back to find Next is not a feature.
  const botPager = pager();
  if (botPager) body.appendChild(botPager);
}

Object.assign(loaders, { requests: loadRequests });
