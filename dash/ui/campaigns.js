// Strategy campaigns: bulk-create/activate keep-alive strategies from a batch of
// KV-cache suggest cells (GET /api/kvcache/suggest), and see, per tenant, per
// hour-of-day, what each cell PREDICTED against what actually happened once it went
// live — see proxy/campaign.go for the full design.
//
// One appended file, self-mounted the same way kvcache.js and tools.js are: the tab,
// the section and the loader registration all happen here, so this feature is one
// line in the shared page (index.html's one added <script> tag). Manager-only, like
// Strategies, whose form/list/drawer conventions this borrows directly
// (loadStrategies/openStrategyLedger in app.js) — a campaign is a bulk, backtested way
// to create the same keepalive_strategies rows that page edits by hand. Every helper
// used here is app.js's own (el, clear, api, ctl, usd, num, when, tile, tileGroup,
// openDrawer, emptyState, errorState, loadingState) — no arithmetic happens in this
// file; every dollar figure arrives already computed from the server.
'use strict';

// ── mount ──────────────────────────────────────────────────────────────────
// Right after Strategies: a campaign is a bulk way to create the same rows that tab
// edits by hand.
(function mountCampaignsTab() {
  const tabs = $('.tabs');
  const tab = el('button', {
    role: 'tab', class: 'tab', 'data-view': 'campaigns', 'data-testid': 'tab-campaigns',
    'data-manager': '', hidden: 'hidden', 'aria-selected': 'false',
  }, 'Campaigns');
  const after = $('.tab[data-view="strategies"]', tabs);
  tabs.insertBefore(tab, after ? after.nextSibling : null);
})();

const campView = el('section', { class: 'view', id: 'view-campaigns', hidden: 'hidden' });
$('#main').appendChild(campView);

// ── local state ────────────────────────────────────────────────────────────
// Its own object, not app.js's shared filter state: this view is in UNFILTERED_VIEWS,
// the same as Strategies, since a campaign is a standing artifact, not a
// date-ranged view of live traffic.
const camp = {
  // The last suggest payload fetched live or read from an uploaded file, awaiting a
  // name before it becomes POST /api/keepalive/campaigns' body. Always submitted as
  // source:"upload" regardless of where it came from — see createCampaignFromPending's
  // own comment for why re-fetching at commit time would be the wrong choice.
  pending: null,
  // The preview's own filters. They narrow WHAT GETS CREATED, not just what is displayed
  // — campFilteredCells feeds both the table and the create body, which is what makes
  // "import this campaign for these two accounts only" a filter rather than a separate
  // feature (see createCampaignFromPending). Every dimension here is one the payload can
  // answer from a field it already carries; nothing is inferred.
  filter: {
    users: [],      // [] = every user in the payload
    arm: '',        // '' = every best_strategy
    hideThin: false, // drop cells the suggester itself flagged insufficient_data
    minSaving: 0,   // predicted saving floor, in dollars
    hourFrom: 0,
    hourTo: 23,
  },
  // The holdout panel's two windows, in epoch ms, and its last result. Kept on this
  // object rather than read back out of the inputs at render time so a re-render never
  // silently disagrees with the numbers already on screen.
  holdout: null,
  holdoutGen: 0,
  // The campaign drawer's own per-tenant filter — the overview table can run to every
  // account this deployment has, and the owner's question of a campaign is almost always
  // about one of them.
  drawerTenant: '',
  // Bumped at the START of every drawer render (renderCampaignOverview,
  // renderCampaignTenantDrilldown) — not only when the drawer first opens — and checked
  // again once each one's fetch resolves, so a slow render that the manager has since
  // navigated away from (a different campaign, a different tenant, or back to the
  // overview — all inside the SAME open drawer) can never overwrite whatever's now
  // actually on screen.
  drawerGen: 0,
  // Bumped at the start of fetchLiveSuggestions and onUploadSuggestFile, and checked by
  // each before writing camp.pending or #camp-preview — so a slow live-fetch and a
  // faster file upload (or the reverse) started before it resolved can't stomp on
  // whichever one the manager is actually looking at. createCampaignFromPending reads
  // it too, so a create that finishes after the manager has already started a NEWER
  // preview doesn't clear that newer one out from under them.
  previewGen: 0,
  // Bumped at the start of refreshCampaignsList — same shape, for the campaigns list
  // table: whichever of two overlapping list refreshes resolves LAST otherwise wins,
  // regardless of which one was actually started last.
  listGen: 0,
};

async function loadCampaigns() {
  if (!campView.dataset.built) {
    buildCampaignsSkeleton(campView);
    campView.dataset.built = '1';
  }
  await refreshCampaignsList();
}

function buildCampaignsSkeleton(host) {
  host.appendChild(el('div', { class: 'panel' },
    el('h2', {}, 'New campaign'),
    el('p', { class: 'note' },
      'Bulk-create keep-alive strategies from a batch of KV-cache suggestions: fetch the ' +
      'live per-tenant, per-hour recommendations, or upload a JSON file in the same shape ' +
      '(GET /api/kvcache/suggest) to hand-edit one first. Only cells whose best strategy has ' +
      'a real enforcement path on this deployment are actually activated — everything else ' +
      'is still recorded, with a reason, never hidden. A fetched or uploaded payload can name ' +
      'any tenant, and creating from it targets exactly the tenants it names, regardless of ' +
      'who is uploading — there is no separate per-tenant confirmation step.'),
    el('div', { class: 'row-actions' },
      el('button', { class: 'ghost', 'data-testid': 'camp-fetch-live', onclick: fetchLiveSuggestions },
        'Fetch live suggestions'),
      el('span', { class: 'muted' }, 'or upload a file:'),
      el('input', {
        type: 'file', accept: 'application/json,.json', 'data-testid': 'camp-upload',
        onchange: onUploadSuggestFile,
      })),
    el('div', { id: 'camp-preview' })));
  buildCampHoldoutPanel(host);
  host.appendChild(el('div', { class: 'panel' },
    el('div', { class: 'card-head' }, el('h2', {}, 'Campaigns'),
      el('span', { class: 'muted', id: 'campaigns-count' })),
    el('div', { id: 'campaigns-list' })));
}

// ── train/test holdout ─────────────────────────────────────────────────────
//
// The honest answer to "show me each strategy's exact and accurate prediction per hour per
// user": a suggest cell's predicted saving is in-sample by construction (the arm is chosen
// by maximising the very figure then presented as its prediction, over the same rows), so
// there is no exact prediction to show — there is a fit, and there is what happens on rows
// the choice was not made on. This panel shows both, side by side, and calls them what
// they are. See dash/kvcacheholdout.go for why no train/test concept existed before this
// and why it is built from the live suggester rather than a second one.

/** campDefaultHoldout is two adjacent 7-day windows ending now: the split a manager almost
 * always wants, prefilled so the panel is one click rather than four date pickers. Seven
 * days because both windows must contain the same weekdays to be comparable at all — this
 * suggester reads Sunday-Thursday and buckets by hour-of-day, so a train window of five
 * days and a test window of nine would differ in which days they average over. */
function campDefaultHoldout() {
  const day = 86_400_000;
  const now = Date.now();
  return {
    trainSince: now - 14 * day, trainUntil: now - 7 * day,
    testSince: now - 7 * day, testUntil: now,
  };
}

function buildCampHoldoutPanel(host) {
  const w = campDefaultHoldout();
  // msToLocal/localToMs are app.js's own, used by the global range control — the two
  // `<input type="datetime-local">` are the platform's picker and need no date library,
  // exactly as initRange's comment says.
  const input = (id, ms) => el('input', {
    type: 'datetime-local', id, 'data-testid': id, value: msToLocal(ms),
  });
  host.appendChild(el('div', { class: 'panel' },
    el('h2', {}, 'Train / test check'),
    el('p', { class: 'note' },
      'Choose each cell’s strategy on a TRAIN window, then score that same strategy on a ' +
      'disjoint TEST window it was not chosen on. Train saving is what this page’s ' +
      'suggestions and a campaign’s frozen prediction already show; test saving is what ' +
      'survived. The gap between them is this deployment’s own overfitting estimate — not ' +
      'a correction to subtract from a prediction, and not a reason to distrust the ' +
      'mechanism, just the measurement to have before committing to one. The two windows ' +
      'must not overlap: an arm scored on rows it was chosen on is an in-sample number.'),
    el('div', { class: 'row-actions' },
      el('div', { class: 'field' }, el('label', { for: 'camp-train-from' }, 'Train from'),
        input('camp-train-from', w.trainSince)),
      el('div', { class: 'field' }, el('label', { for: 'camp-train-to' }, 'Train to'),
        input('camp-train-to', w.trainUntil)),
      el('div', { class: 'field' }, el('label', { for: 'camp-test-from' }, 'Test from'),
        input('camp-test-from', w.testSince)),
      el('div', { class: 'field' }, el('label', { for: 'camp-test-to' }, 'Test to'),
        input('camp-test-to', w.testUntil)),
      el('div', { class: 'field' }, el('label', {}, ' '),
        el('button', { 'data-testid': 'camp-holdout-run', onclick: runCampHoldout }, 'Run check'))),
    el('div', { id: 'camp-holdout' })));
}

async function runCampHoldout() {
  const gen = ++camp.holdoutGen;
  const btn = $('[data-testid="camp-holdout-run"]');
  const out = clear($('#camp-holdout'));
  const qs = {
    train_since: localToMs($('#camp-train-from').value),
    train_until: localToMs($('#camp-train-to').value),
    test_since: localToMs($('#camp-test-from').value),
    test_until: localToMs($('#camp-test-to').value),
  };
  btn.disabled = true;
  loadingState(out, 4);
  try {
    // Two full dataset replays server-side, so this is the slowest control on the page —
    // the button stays disabled for the duration rather than letting a second click queue
    // another pair of them behind the first.
    const res = await api('kvcache/suggest/holdout', qs);
    if (gen !== camp.holdoutGen) return;
    camp.holdout = res;
    renderCampHoldout(clear(out), res);
  } catch (e) {
    if (gen !== camp.holdoutGen) return;
    errorState(clear(out), 'Could not run the train/test check', e);
  } finally {
    btn.disabled = false;
  }
}

function renderCampHoldout(host, res) {
  host.appendChild(tileGroup(null, null, [
    tile('camp-ho-train', 'Train saving (in-sample)', usd(res.total_train_saving_usd)),
    tile('camp-ho-test', 'Test saving (held out)', usd(res.total_test_saving_usd), null,
      res.total_test_saving_usd < 0 ? 'bad' : 'good'),
    // Never a fabricated percentage: the server sets retention_known false where the ratio
    // is undefined (no comparable cells, or a train total of exactly zero), and the tile
    // then says so rather than showing 0%.
    tile('camp-ho-retention', 'Predicted saving retained',
      res.retention_known ? `${res.retention_pct.toFixed(1)}%` : 'not enough data'),
    tile('camp-ho-cells', 'Comparable cells', `${num(res.comparable_cells)} of ${num(res.total_cells)}`),
    tile('camp-ho-users', 'Users', num((res.users || []).length)),
  ]));
  for (const n of res.notes || []) {
    host.appendChild(el('p', { class: 'muted small' }, n));
  }
  const cells = res.cells || [];
  if (!cells.length) {
    emptyState(host, 'No cells in the train window', 'Widen the train window, or pick one with traffic in it.');
    return;
  }
  host.appendChild(el('p', { class: 'note' },
    'Click a row for every candidate strategy’s train-and-test pair in that cell — which ' +
    'answers whether the arm that won on train was anywhere near the best on test.'));
  const tbl = el('table', { class: 'grid compact', 'data-testid': 'camp-holdout-table' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Tenant'), el('th', { class: 'num' }, 'Hour (UTC)'),
      el('th', {}, 'Strategy chosen on train'),
      el('th', { class: 'num' }, 'Train n'), el('th', { class: 'num' }, 'Train $'),
      el('th', { class: 'num' }, 'Test n'), el('th', { class: 'num' }, 'Test $'))));
  const body = el('tbody');
  for (const c of cells) {
    body.appendChild(el('tr', { class: 'click', onclick: () => openCampHoldoutCell(c) },
      el('td', {}, el('code', { class: 'clip' }, c.user)),
      el('td', { class: 'num' }, String(c.hour_utc)),
      el('td', {}, c.arm),
      el('td', { class: 'num' }, num(c.train_requests),
        c.train_insufficient_data ? el('span', { class: 'pill neutral' }, 'thin') : null),
      el('td', { class: 'num' }, c.train_known ? usd(c.train_saving_usd) : '—'),
      el('td', { class: 'num' }, num(c.test_requests),
        c.test_insufficient_data ? el('span', { class: 'pill neutral' }, 'thin') : null),
      // A missing test figure renders as its own reason, never as $0.00 — "nothing was
      // measured" and "this arm saved nothing" are opposite findings.
      el('td', { class: 'num ' + (c.test_known && c.test_saving_usd < 0 ? 'bad-text' : '') },
        c.test_known
          ? usd(c.test_saving_usd)
          : el('span', { class: 'pill neutral', title: c.test_note || '' }, 'no data'))));
  }
  tbl.appendChild(body);
  host.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, tbl));
}

/** openCampHoldoutCell shows every candidate arm's train-and-test pair for one cell, so a
 * reader can see the whole comparison the winner came out of rather than only the winner. */
function openCampHoldoutCell(c) {
  const body = openDrawer(`${c.user} · ${hourUTCToLocalLabel(c.hour_utc)}`, null);
  body.appendChild(el('p', { class: 'note' },
    `${num(c.train_requests)} train request(s), ${num(c.test_requests)} test request(s). ` +
    `Chosen on train: ${c.arm}.` + (c.test_known ? '' : ` No test figure — ${c.test_note}`)));
  const tbl = el('table', { class: 'grid compact', 'data-testid': 'camp-holdout-arms-table' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Strategy'), el('th', { class: 'num' }, 'Train $'),
      el('th', { class: 'num' }, 'Test $'))));
  const tbody = el('tbody');
  for (const a of c.arms || []) {
    tbody.appendChild(el('tr', { class: a.chosen ? 'click' : '' },
      el('td', {}, a.strategy,
        a.chosen ? el('span', { class: 'pill complete' }, 'chosen') : null),
      el('td', { class: 'num' }, a.train_known ? usd(a.train_saving_usd) : '—'),
      el('td', { class: 'num ' + (a.test_known && a.test_saving_usd < 0 ? 'bad-text' : '') },
        a.test_known ? usd(a.test_saving_usd) : '—')));
  }
  tbl.appendChild(tbody);
  body.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, tbl));
}

// ── create flow: fetch or upload, preview, name, create ────────────────────

async function fetchLiveSuggestions() {
  const gen = ++camp.previewGen;
  const btn = $('[data-testid="camp-fetch-live"]');
  const preview = clear($('#camp-preview'));
  btn.disabled = true;
  loadingState(preview, 3);
  try {
    const suggest = await api('kvcache/suggest');
    if (gen !== camp.previewGen) return;
    camp.pending = suggest;
    renderSuggestPreview(clear(preview), suggest);
  } catch (e) {
    if (gen !== camp.previewGen) return;
    errorState(clear(preview), 'Could not fetch live suggestions', e);
  } finally {
    btn.disabled = false;
  }
}

function onUploadSuggestFile(ev) {
  const file = ev.target.files && ev.target.files[0];
  ev.target.value = ''; // lets the same file be chosen again after a fix
  if (!file) return;
  const gen = ++camp.previewGen;
  const preview = clear($('#camp-preview'));
  const reader = new FileReader();
  reader.onload = () => {
    if (gen !== camp.previewGen) return;
    let suggest;
    try {
      suggest = JSON.parse(reader.result);
    } catch (e) {
      errorState(clear(preview), 'That file is not valid JSON', e);
      return;
    }
    // JSON.parse("null") and JSON.parse("42") both succeed without throwing, so the
    // try/catch above alone does not guarantee an object with a .cells to read.
    if (!suggest || typeof suggest !== 'object' || Array.isArray(suggest)) {
      errorState(clear(preview), 'That file is valid JSON, but not a suggest object', '');
      return;
    }
    camp.pending = suggest;
    renderSuggestPreview(clear(preview), suggest);
  };
  reader.onerror = () => {
    if (gen !== camp.previewGen) return;
    errorState(clear(preview), 'Could not read that file', reader.error);
  };
  reader.readAsText(file);
}

/**
 * campNamedCells is every cell the SERVER would consider at all: one with a tenant id.
 * A cell without one is dropped by ctlCreateCampaign regardless (an ambiguous pool —
 * single-tenant traffic, or pre-tenancy rows), so it is excluded here before any filter
 * runs rather than counted and then silently discarded on create.
 */
function campNamedCells(suggest) {
  return (suggest.cells || []).filter((c) => c.user);
}

/**
 * campFilteredCells applies camp.filter to a payload's named cells. This is the single
 * definition of "the cells this campaign is about": the preview table renders it, the
 * count reports it, and createCampaignFromPending SUBMITS it. Keeping those three on one
 * function is what makes the filters trustworthy — a filter that changed the table but
 * not the create body would quietly enforce hours a manager had just excluded, which is
 * the worst possible failure for a control whose whole job is narrowing a bulk action.
 */
function campFilteredCells(suggest) {
  const f = camp.filter;
  return campNamedCells(suggest).filter((c) => {
    if (f.users.length && !f.users.includes(c.user)) return false;
    if (f.arm && c.best_strategy !== f.arm) return false;
    if (f.hideThin && c.insufficient_data) return false;
    // A cell whose saving could not be priced at all (valued false) has saving_usd 0 for
    // want of a rate, not because it saves nothing — so a positive floor excludes it,
    // which is the honest reading of "only cells predicted to save at least $X".
    if (f.minSaving > 0 && !(c.saving_usd >= f.minSaving)) return false;
    if (c.hour_utc < f.hourFrom || c.hour_utc > f.hourTo) return false;
    return true;
  });
}

/** campSelect builds one labelled select and wires it to a change handler — kvcache.js's
 * kvSelect, which this page cannot import (classic scripts, one shared global scope, and
 * that one is private to the KV-cache tab's own state). */
function campSelect(label, value, options, onChange, testid) {
  const sel = el('select', { 'data-testid': testid, onchange: (ev) => onChange(ev.target.value) });
  for (const [v, text] of options) {
    const opt = el('option', { value: v }, text);
    if (v === value) opt.setAttribute('selected', 'selected');
    sel.appendChild(opt);
  }
  return el('div', { class: 'field' }, el('label', {}, label), sel);
}

/**
 * campNumber builds one labelled numeric input. Native type=number, min/max/step explicit,
 * the same way every other threshold field in this dashboard is built.
 *
 * onChange receives the INPUT ELEMENT as well as the raw string, so a handler that
 * normalises a value can write the normalised one back. min/max on a number input are
 * advisory: the browser marks an out-of-range value invalid but still fires `change`
 * carrying it, so a handler that clamped only its own state left the field displaying 99
 * while the table filtered on 23 — a control describing a narrowing it is not performing,
 * which is a worse outcome than either accepting or refusing the input outright.
 */
function campNumber(label, value, attrs, onChange, testid) {
  const input = el('input', Object.assign({
    type: 'number', value: String(value), 'data-testid': testid,
    onchange: (ev) => onChange(ev.target.value, ev.target),
  }, attrs));
  return el('div', { class: 'field' }, el('label', {}, label), input);
}

/**
 * renderCampFilterBar builds the preview's filter row.
 *
 * The user picker is a native `<select multiple>`, matching the strategy form's own
 * account target (app.js's sf-target-ids) rather than inventing a chip widget — a manager
 * who already picks accounts that way for a hand-made strategy picks them the same way
 * for a bulk one. Every option carries its own cell count, so the size of what a
 * selection is about is visible before it is made.
 *
 * onChange re-renders only the table and the create row, never the bar itself: rebuilding
 * a `<select multiple>` mid-interaction would drop the selection the manager is still
 * making, and re-rendering the bar is also what would steal focus from the number input
 * someone is halfway through typing into.
 */
function renderCampFilterBar(host, suggest, onChange) {
  const named = campNamedCells(suggest);
  const cellsPerUser = {};
  const arms = {};
  for (const c of named) {
    cellsPerUser[c.user] = (cellsPerUser[c.user] || 0) + 1;
    arms[c.best_strategy] = (arms[c.best_strategy] || 0) + 1;
  }
  const users = Object.keys(cellsPerUser).sort();

  const userSel = el('select', {
    multiple: 'multiple', size: String(Math.min(6, Math.max(3, users.length))),
    'data-testid': 'camp-filter-users',
    onchange: (ev) => {
      camp.filter.users = Array.from(ev.target.selectedOptions).map((o) => o.value);
      onChange();
    },
  }, ...users.map((u) => {
    const opt = el('option', { value: u }, `${u} · ${num(cellsPerUser[u])} cell(s)`);
    if (camp.filter.users.includes(u)) opt.setAttribute('selected', 'selected');
    return opt;
  }));

  const bar = el('div', { class: 'row-actions', 'data-testid': 'camp-filters' },
    el('div', { class: 'field' },
      el('label', {}, `Users (${users.length}) — none selected means all`), userSel),
    campSelect('Best strategy', camp.filter.arm,
      [['', 'Every strategy']].concat(Object.keys(arms).sort().map(
        (a) => [a, `${a} · ${num(arms[a])}`])),
      (v) => { camp.filter.arm = v; onChange(); }, 'camp-filter-arm'),
    campNumber('Min predicted $', camp.filter.minSaving, { min: '0', step: '0.01' },
      (v, input) => {
        const n = Number(v);
        camp.filter.minSaving = Number.isFinite(n) && n > 0 ? n : 0;
        input.value = String(camp.filter.minSaving);
        onChange();
      }, 'camp-filter-minsaving'),
    campNumber('Hour from (UTC)', camp.filter.hourFrom, { min: '0', max: '23', step: '1' },
      (v, input) => {
        camp.filter.hourFrom = campClampHour(v, 0);
        input.value = String(camp.filter.hourFrom);
        onChange();
      }, 'camp-filter-hourfrom'),
    campNumber('Hour to (UTC)', camp.filter.hourTo, { min: '0', max: '23', step: '1' },
      (v, input) => {
        camp.filter.hourTo = campClampHour(v, 23);
        input.value = String(camp.filter.hourTo);
        onChange();
      }, 'camp-filter-hourto'),
    el('div', { class: 'field' }, el('label', {},
      el('input', {
        type: 'checkbox', 'data-testid': 'camp-filter-hidethin',
        checked: camp.filter.hideThin ? 'checked' : null,
        onchange: (ev) => { camp.filter.hideThin = ev.target.checked; onChange(); },
      }), ' Hide thin-data cells')),
    el('button', {
      class: 'ghost small', 'data-testid': 'camp-filter-reset',
      onclick: () => {
        camp.filter = { users: [], arm: '', hideThin: false, minSaving: 0, hourFrom: 0, hourTo: 23 };
        // The one place the bar itself IS rebuilt, because the reset's whole purpose is to
        // clear the controls' own displayed state — the selection it would otherwise
        // clobber is exactly what it is meant to clear.
        renderSuggestPreview(clear(host.parentNode), suggest);
      },
    }, 'Reset filters'));
  host.appendChild(bar);
}

/** campClampHour keeps a hand-typed hour inside [0,23]. Its callers write the clamped value
 * back into the input — see campNumber on why clamping only the state is worse than not
 * clamping at all. An out-of-range hour would also be refused by the server
 * (resolveCampaignCell bounds-checks it), but a filter that quietly matched nothing is a
 * worse way to learn that than a field that corrects itself as you leave it. */
function campClampHour(v, fallback) {
  const n = Math.trunc(Number(v));
  if (!Number.isFinite(n)) return fallback;
  return Math.min(23, Math.max(0, n));
}

/** renderSuggestPreview shows the suggest payload, filters over it, and a name field to
 * create a campaign from exactly what the filters leave. */
function renderSuggestPreview(host, suggest) {
  const cells = suggest.cells || [];
  if (!cells.length) {
    emptyState(host, 'No cells in this suggestion set', 'Nothing to campaign over.');
    return;
  }
  const named = campNamedCells(suggest);
  const unnamed = cells.length - named.length;
  host.appendChild(el('p', { class: 'note' },
    `${named.length} cell(s) across ${(suggest.users || []).length} tenant(s), baseline ` +
    `"${suggest.baseline}", weekdays ${(suggest.weekdays_included || []).join(', ') || '—'}.` +
    (unnamed ? ` ${unnamed} cell(s) with no tenant id will be excluded from the campaign.` : '') +
    ' Which of these actually activate is decided when the campaign is created — this is ' +
    'the raw recommendation, not yet resolved against what this deployment can enforce.'));
  host.appendChild(el('p', { class: 'note' },
    'Predicted saving is IN-SAMPLE: each cell’s strategy was chosen by replaying every ' +
    'candidate over that cell’s own history and keeping the best, so the figure it wins ' +
    'with measures fit, not forecast. Use the train/test panel below to see how much of it ' +
    'survives on traffic the choice was not made on before committing to a large campaign.'));

  const filterHost = el('div');
  const tableHost = el('div');
  const actionHost = el('div');
  host.appendChild(filterHost);
  host.appendChild(tableHost);
  host.appendChild(actionHost);

  const rerender = () => {
    renderCampPreviewTable(clear(tableHost), suggest);
    renderCampCreateRow(clear(actionHost), suggest, host);
  };
  renderCampFilterBar(filterHost, suggest, rerender);
  rerender();
}

/** renderCampPreviewTable renders exactly the cells camp.filter leaves. */
function renderCampPreviewTable(host, suggest) {
  const named = campNamedCells(suggest);
  const shownCells = campFilteredCells(suggest);
  if (!shownCells.length) {
    emptyState(host, 'No cells match these filters',
      `All ${named.length} cell(s) were filtered out. Widen a filter, or reset them.`);
    return;
  }
  const SHOWN = 200;
  const tbl = el('table', { class: 'grid compact', 'data-testid': 'camp-preview-table' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Tenant'), el('th', { class: 'num' }, 'Hour (UTC)'),
      el('th', { class: 'num' }, 'Requests'), el('th', {}, 'Best strategy'),
      el('th', { class: 'num' }, 'Predicted saving'),
      el('th', { class: 'num' }, 'Oracle ceiling'))));
  const body = el('tbody');
  for (const c of shownCells.slice(0, SHOWN)) {
    body.appendChild(el('tr', {},
      el('td', {}, el('code', { class: 'clip' }, c.user)),
      el('td', { class: 'num' }, String(c.hour_utc)),
      el('td', { class: 'num' }, num(c.requests)),
      el('td', {}, c.best_strategy,
        c.insufficient_data ? el('span', { class: 'pill neutral' }, 'thin data') : null),
      el('td', { class: 'num' }, usd(c.saving_usd)),
      // The exact ceiling on the same rows, already on the wire per cell — how much was
      // there to capture, beside how much this recommendation captures. Rendered as
      // unknown rather than 0 where the backtest could not price it.
      el('td', { class: 'num' },
        c.optimal_known ? usd(c.optimal_saving_usd) : el('span', { class: 'pill neutral' }, 'no rate'))));
  }
  tbl.appendChild(body);
  host.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, tbl));
  host.appendChild(el('p', { class: 'muted small' },
    shownCells.length > SHOWN
      ? `Showing the first ${SHOWN} of ${shownCells.length} matching cell(s), of ${named.length} total.`
      : `${shownCells.length} of ${named.length} cell(s) match.`));
}

/**
 * renderCampCreateRow builds the name field and the create button, labelled with how many
 * cells and accounts the current filters would actually commit.
 *
 * The count is in the BUTTON, not only in the note above it: a manager who filtered to two
 * accounts and then scrolled past the table needs the commit control itself to say what it
 * is about to do, because the filters are the only thing standing between "create" and a
 * live strategy for every account in the payload.
 */
function renderCampCreateRow(host, suggest, previewHost) {
  const shownCells = campFilteredCells(suggest);
  const users = new Set(shownCells.map((c) => c.user));
  const nameInput = el('input', {
    type: 'text', maxlength: '64', placeholder: 'Campaign name', 'data-testid': 'camp-name',
  });
  const createBtn = el('button', {
    'data-testid': 'camp-create',
    disabled: shownCells.length ? null : 'disabled',
    onclick: () => createCampaignFromPending(nameInput, createBtn, previewHost, suggest),
  }, shownCells.length
    ? `Create campaign — ${shownCells.length} cell(s), ${users.size} account(s)`
    : 'Create campaign — nothing selected');
  host.appendChild(el('div', { class: 'row-actions', style: 'margin-top:var(--sp-3)' },
    nameInput, createBtn));
}

/**
 * createCampaignFromPending always sends source:"upload" with the exact payload the
 * preview above rendered — even when that payload came from "Fetch live suggestions" —
 * rather than asking the server to re-fetch live data at commit time. Re-fetching would
 * let traffic between preview and commit change the numbers a manager just reviewed;
 * submitting exactly what was shown means "create this" always means what it says.
 *
 * createBtn is disabled synchronously, before the first await, and only re-enabled in a
 * finally — the same pattern fetchLiveSuggestions already uses — because a campaign
 * create has no idempotency/dedup on the server: two clicks (an accidental double-click,
 * or an impatient second click on a slow network) would each create a full, independent
 * set of live keep-alive strategies, not just a UI glitch.
 *
 * gen is captured (not bumped) at the start: starting a create does not itself start a
 * new preview, it commits the CURRENT one. But the manager isn't prevented from
 * starting a fresh fetch/upload while this POST is still in flight (only the Create
 * button itself is disabled) — if that happens, #camp-preview and camp.pending already
 * belong to that newer preview by the time this call resolves, and clearing/nulling
 * them here would wipe out work the manager has already moved on to, even though the
 * OLD campaign this call submitted was created successfully.
 */
async function createCampaignFromPending(nameInput, createBtn, previewHost, previewed) {
  const name = (nameInput.value || '').trim();
  if (!name) { nameInput.focus(); return; }
  const gen = camp.previewGen;
  // The payload this button was rendered against, not camp.pending: the two are normally
  // the same object, but a fetch/upload started while this create is being clicked
  // replaces camp.pending, and submitting THAT would create a campaign from cells nobody
  // reviewed. previewed is captured when the row is built, so "create this" means the
  // table above it.
  const suggest = previewed || camp.pending;
  const cells = campFilteredCells(suggest);
  if (!cells.length) return;
  createBtn.disabled = true;
  try {
    const created = await ctl('/api/keepalive/campaigns', {
      method: 'POST',
      // Only the filtered cells, with users narrowed to match — this is the whole of
      // "import a campaign for these accounts": the server takes an uploaded suggest
      // payload verbatim, so a narrowed payload IS a narrowed campaign, with no
      // per-tenant import route to build or keep in step with the unfiltered one.
      //
      // total_saving_usd is deliberately dropped rather than carried over. Nothing on the
      // server reads it, but it describes the UNFILTERED payload, and a stale aggregate
      // travelling inside a narrowed one is exactly the kind of number that gets believed
      // later by something that does start reading it.
      body: JSON.stringify({
        name,
        source: 'upload',
        suggest: Object.assign({}, suggest, {
          cells,
          users: Array.from(new Set(cells.map((c) => c.user))).sort(),
          total_saving_usd: undefined,
        }),
      }),
    });
    if (gen === camp.previewGen) {
      camp.pending = null;
      renderCreatedResult(clear(previewHost), created);
    }
    await refreshCampaignsList();
  } catch (e) {
    if (gen === camp.previewGen) errorState(previewHost, 'Could not create this campaign', e);
  } finally {
    createBtn.disabled = false;
  }
}

function renderCreatedResult(host, created) {
  const cells = created.cells || [];
  const activated = cells.filter((c) => c.activatable).length;
  const skipped = cells.length - activated;
  host.appendChild(el('div', { class: 'banner ok' },
    `Campaign "${created.name}" created — ${activated} cell(s) activated, ` +
    `${skipped} recorded but not activated.`));
  if (!skipped) return;
  const byReason = {};
  for (const c of cells) {
    if (c.activatable) continue;
    byReason[c.skip_reason] = (byReason[c.skip_reason] || 0) + 1;
  }
  const list = el('ul', { class: 'muted small' });
  for (const [reason, n] of Object.entries(byReason)) {
    list.appendChild(el('li', {}, `${n}× — ${reason}`));
  }
  host.appendChild(list);
}

// ── list ─────────────────────────────────────────────────────────────────

async function refreshCampaignsList() {
  const gen = ++camp.listGen;
  const host = clear($('#campaigns-list'));
  loadingState(host, 2);
  try {
    const out = await ctl('/api/keepalive/campaigns');
    if (gen !== camp.listGen) return;
    const rows = out.campaigns || [];
    $('#campaigns-count').textContent = `${rows.length} campaign${rows.length === 1 ? '' : 's'}`;
    renderCampaignsList(clear(host), rows);
  } catch (e) {
    if (gen !== camp.listGen) return;
    errorState(clear(host), 'Could not list campaigns', e);
  }
}

function renderCampaignsList(host, rows) {
  if (!rows.length) {
    emptyState(host, 'No campaigns yet', 'Create one above.');
    return;
  }
  const tbl = el('table', { class: 'grid', 'data-testid': 'campaigns-table' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Name'), el('th', {}, 'Source'), el('th', {}, 'Created'), el('th', {}, 'State'),
      el('th', {}, el('span', { class: 'vh' }, 'Row actions')))));
  const body = el('tbody');
  for (const c of rows) {
    body.appendChild(el('tr', { class: c.status === 'archived' ? 'revoked' : '' },
      el('td', {}, c.name),
      el('td', {}, c.source === 'suggest-live' ? 'live fetch' : 'upload'),
      el('td', {}, when(c.created_at)),
      el('td', {}, el('span', { class: 'pill ' + (c.status === 'active' ? 'complete' : 'partial') }, c.status)),
      el('td', {}, el('div', { class: 'row-actions' },
        el('button', { class: 'ghost small', onclick: () => openCampaignDrawer(c) }, 'Stats'),
        c.status === 'active'
          ? el('button', { class: 'ghost small', onclick: () => archiveCampaign(c) }, 'Archive')
          : null))));
  }
  tbl.appendChild(body);
  host.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, tbl));
}

async function archiveCampaign(c) {
  if (!confirm(`Archive "${c.name}"? Its strategies keep running — this only stops the ` +
    'campaign itself from being managed as a group.')) return;
  try {
    await ctl('/api/keepalive/campaigns/' + c.id, { method: 'DELETE' });
    await refreshCampaignsList();
  } catch (e) { alert(e.message); }
}

// ── drawer: aggregate overview, and a per-tenant drill-down in place of it ─

/** openStrategyLedger's per-entity precedent, for a whole campaign instead of one strategy. */
async function openCampaignDrawer(c) {
  const body = openDrawer('Campaign: ' + c.name, null);
  await renderCampaignOverview(body, c.id);
}

/**
 * Bumps camp.drawerGen itself, at the very start, rather than taking a generation from
 * the caller: #drawer-body is one singleton node the drawer reuses across opens AND
 * across in-drawer navigation (a tenant drill-down, "back to overview", a different
 * campaign entirely) — closing it, or navigating away from it, does not cancel an
 * in-flight fetch. A gen threaded in from the CALLER (as this used to do) is only fresh
 * at drawer-open; two navigations inside the same open drawer would share it and could
 * still race. Bumping here means every render, including a second one inside the same
 * drawer session, gets its own token.
 */
async function renderCampaignOverview(body, campaignID) {
  const gen = ++camp.drawerGen;
  clear(body);
  loadingState(body, 3);
  let detail;
  try {
    detail = await ctl('/api/keepalive/campaigns/' + campaignID);
  } catch (e) {
    if (gen !== camp.drawerGen) return;
    clear(body);
    errorState(body, 'Could not load this campaign', e);
    return;
  }
  if (gen !== camp.drawerGen) return;
  clear(body);
  body.appendChild(tileGroup(null, null, [
    tile('camp-predicted', 'Predicted saving', usd(detail.total_predicted_usd)),
    // The exact ceiling on the SAME cells Predicted sums: what a policy with perfect
    // foreknowledge of every next request would have saved, frozen at campaign-creation
    // time right alongside Predicted (see dash.KVCacheSuggestion.OptimalSavingUSD) — how
    // much headroom remains beyond what this campaign's own choice of arm captures.
    tile('camp-oracle', 'Oracle ceiling (predicted)', usd(detail.total_optimal_saving_usd)),
    tile('camp-real-saved', 'Real saved (ceiling)', usd(detail.total_real_saved_usd)),
    tile('camp-real-net', 'Real net', usd(detail.total_real_net_usd), null,
      detail.total_real_net_usd < 0 ? 'bad' : 'good'),
    tile('camp-tenants', 'Tenants', num((detail.tenants || []).length)),
  ]));
  if (detail.caveat) body.appendChild(el('p', { class: 'note' }, detail.caveat));
  const tenants = detail.tenants || [];
  if (!tenants.length) {
    emptyState(body, 'No tenants in this campaign', '');
    return;
  }
  body.appendChild(el('p', { class: 'note' }, 'Click a tenant for every hour cell’s ' +
    'historical/predicted saving alongside what actually happened.'));

  // A per-tenant filter over the rows already in hand: a campaign can target every account
  // this deployment has, and a question about one of them should not mean reading past the
  // other seventeen. Purely client-side — the payload is already here, so narrowing it
  // costs no request and cannot disagree with the totals above, which deliberately keep
  // covering the WHOLE campaign no matter what is filtered below them.
  const tenantHost = el('div');
  const renderRows = () => {
    const shown = camp.drawerTenant
      ? tenants.filter((t) => t.tenant_id === camp.drawerTenant)
      : tenants;
    clear(tenantHost);
    const tbl = el('table', { class: 'grid', 'data-testid': 'camp-tenants-table' },
      el('thead', {}, el('tr', {},
        el('th', {}, 'Tenant'), el('th', { class: 'num' }, 'Predicted'),
        el('th', { class: 'num' }, 'Oracle ceiling'),
        el('th', { class: 'num' }, 'Real ping cost'), el('th', { class: 'num' }, 'Real saved'),
        el('th', { class: 'num' }, 'Real net'))));
    const tbody = el('tbody');
    for (const t of shown) {
      tbody.appendChild(el('tr', {
        class: 'click', onclick: () => renderCampaignTenantDrilldown(body, campaignID, t.tenant_id),
      },
        el('td', {}, el('code', { class: 'clip' }, t.tenant_id)),
        el('td', { class: 'num' }, usd(t.predicted_usd)),
        el('td', { class: 'num' }, usd(t.optimal_saving_usd)),
        el('td', { class: 'num' }, usd(t.real_ping_usd)),
        el('td', { class: 'num' }, usd(t.real_saved_usd)),
        el('td', { class: 'num ' + (t.real_net_usd < 0 ? 'bad-text' : 'good-text') }, usd(t.real_net_usd))));
    }
    tbl.appendChild(tbody);
    tenantHost.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, tbl));
  };
  body.appendChild(el('div', { class: 'row-actions' },
    campSelect('Tenant', camp.drawerTenant,
      [['', `All ${tenants.length} tenant(s)`]].concat(
        tenants.map((t) => [t.tenant_id, t.tenant_id])),
      (v) => { camp.drawerTenant = v; renderRows(); }, 'camp-drawer-tenant')));
  body.appendChild(tenantHost);
  renderRows();
}

/**
 * hourUTCToLocalLabel shows both the UTC hour a window actually fires at (windows are
 * stored in UTC, matching exactly the hour suggest computed each cell's saving from —
 * see proxy/campaign.go) and today's real Asia/Jerusalem equivalent, computed from
 * TODAY'S actual date rather than a fixed one so the DST offset shown is the one
 * genuinely in effect right now, not a guess about which regime some other day is in.
 */
function hourUTCToLocalLabel(hourUTC) {
  const now = new Date();
  const atHour = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate(), hourUTC, 0, 0));
  const local = new Intl.DateTimeFormat(undefined, {
    timeZone: 'Asia/Jerusalem', hour: '2-digit', minute: '2-digit', hour12: false,
  }).format(atHour);
  return `${String(hourUTC).padStart(2, '0')}:00 UTC (${local} Jerusalem today)`;
}

/** See renderCampaignOverview's doc comment — bumps its own generation, same reason. */
async function renderCampaignTenantDrilldown(body, campaignID, tenantID) {
  const gen = ++camp.drawerGen;
  clear(body);
  loadingState(body, 3);
  let out;
  try {
    out = await ctl('/api/keepalive/campaigns/' + campaignID + '/tenants/' + encodeURIComponent(tenantID));
  } catch (e) {
    if (gen !== camp.drawerGen) return;
    clear(body);
    errorState(body, 'Could not load this tenant’s drill-down', e);
    return;
  }
  if (gen !== camp.drawerGen) return;
  clear(body);
  body.appendChild(el('button', {
    class: 'ghost small', 'data-testid': 'camp-back-to-overview',
    onclick: () => renderCampaignOverview(body, campaignID),
  }, '← Back to overview'));
  body.appendChild(el('h3', {}, 'Tenant: ' + tenantID));
  if (out.caveat) body.appendChild(el('p', { class: 'note' }, out.caveat));
  const cells = out.cells || [];
  if (!cells.length) {
    emptyState(body, 'No cells for this tenant', '');
    return;
  }
  const tbl = el('table', { class: 'grid compact', 'data-testid': 'camp-drilldown-table' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Hour'), el('th', {}, 'Best strategy'), el('th', {}, 'State'),
      el('th', { class: 'num' }, 'Historical/predicted'), el('th', { class: 'num' }, 'Real saved'),
      el('th', { class: 'num' }, 'Real net'), el('th', { class: 'num' }, '$/1k requests'),
      el('th', { class: 'num' }, '$/active day'))));
  const tbody = el('tbody');
  for (const cell of cells) {
    const hasReal = cell.real_requests > 0;
    tbody.appendChild(el('tr', {},
      el('td', {}, hourUTCToLocalLabel(cell.hour_utc)),
      el('td', {}, cell.arm),
      el('td', {},
        cell.activatable
          ? el('span', { class: 'pill complete' }, 'active')
          : el('span', { class: 'pill missing', title: cell.skip_reason || '' }, 'not activated')),
      el('td', { class: 'num' }, usd(cell.predicted_usd)),
      el('td', { class: 'num' }, hasReal ? usd(cell.real_saved_usd) : el('span', { class: 'pill neutral' }, 'no data yet')),
      el('td', { class: 'num ' + (hasReal && cell.real_net_usd < 0 ? 'bad-text' : 'good-text') },
        hasReal ? usd(cell.real_net_usd) : '—'),
      // != null, not truthy: the rate is a JSON pointer field on the server
      // (proxy/campaign.go's *float64), sent only when its denominator was nonzero —
      // but the rate ITSELF can legitimately be exactly 0 (real traffic, $0 credited
      // that hour), which a truthy check would wrongly render as "—" (no data).
      el('td', { class: 'num' },
        cell.real_saved_usd_per_1k_requests != null ? usd(cell.real_saved_usd_per_1k_requests) : '—'),
      el('td', { class: 'num' },
        cell.real_saved_usd_per_active_day != null ? usd(cell.real_saved_usd_per_active_day) : '—')));
  }
  tbl.appendChild(tbody);
  body.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, tbl));
}

// ── wiring ─────────────────────────────────────────────────────────────────
Object.assign(loaders, { campaigns: loadCampaigns });
UNFILTERED_VIEWS.add('campaigns');
