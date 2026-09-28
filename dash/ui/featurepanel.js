// The feature/predictor inspection panel: every feature kvcache/predictor's registry knows
// about (including the ones this schema genuinely cannot build, shown rather than hidden), a
// correlation view over a sibling job's own output, and every registered predictor's
// calibration and net dollars against BOTH the never-ping and the ping-everyone baseline.
//
// One appended file, self-mounting exactly the way keepalivepage.js documents at its own top:
// the tab, the section and the loader registration all happen here. Every helper it uses is
// app.js's (el, clear, $, num, usd, pct, api, tile, tileGroup, emptyState, loadingState,
// errorState, aborted, mountTab) — no second design system, and no stylesheet of its own.
//
// HONESTY IS ON THE DATA, NOT JUST THE LABEL: every number the server sends here carries its
// own `honesty` (observed/replayed/estimated) beside it — see dash/featurepanel.go's own
// top-of-file comment — and fpHonestyPill renders exactly that tag rather than a caption this
// file made up.
'use strict';

// ── mount ──────────────────────────────────────────────────────────────────
const fpView = mountTab({
  group: 'behaviour', after: 'kapage', view: 'featurepanel', label: 'Features & predictors',
});

const fp = { data: null };

/** fpHonestyPill renders one FPHonesty tag, in the same pill style every other tab already
 *  uses for a reachability/quality flag. */
function fpHonestyPill(honesty) {
  if (!honesty) return null;
  const cls = honesty === 'observed' ? 'good' : honesty === 'replayed' ? 'neutral'
    : honesty === 'hindsight' ? 'bad' : 'warn';
  return el('span', { class: 'pill ' + cls }, honesty);
}

/** fpRange renders a low/high interval beside a point estimate, or nothing when the server
 *  did not report one — never a fabricated range. */
function fpRange(low, high, ranged, fmt) {
  if (!ranged) return null;
  return el('span', { class: 'section-note' }, ' [' + fmt(low) + ', ' + fmt(high) + ']');
}

function fpRenderShell() {
  clear(fpView);
  fpView.appendChild(el('div', { class: 'section' },
    el('h2', {}, 'Features & predictors'),
    el('span', { class: 'section-note' },
      'Every signal a keep-alive predictor could use, what a sibling sweep found it '
      + 'correlates with, and what each registered predictor would decide, calibrate to, and '
      + 'net — against both never-ping and ping-everyone.')));
  fpView.appendChild(el('div', { id: 'fp-features' }));
  fpView.appendChild(el('div', { id: 'fp-correlations' }));
  fpView.appendChild(el('div', { id: 'fp-predictors' }));
}
fpRenderShell();

// ── the feature browser ─────────────────────────────────────────────────────
function fpRenderFeatures(features) {
  const host = clear($('#fp-features'));
  host.appendChild(el('div', { class: 'section' }, el('h3', {}, 'Feature browser')));
  if (!features || !features.length) {
    emptyState(host, 'No features registered', 'The predictor feature registry is empty.');
    return;
  }
  const table = el('table', { class: 'tbl', 'data-testid': 'fp-feature-table' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Feature'), el('th', {}, 'Availability'), el('th', {}, 'Value/status'),
      el('th', {}, 'Source'), el('th', {}, 'Missing behaviour'), el('th', {}, 'Privacy'),
      el('th', {}, 'Explanation'))));
  const body = el('tbody');
  table.appendChild(body);
  features.forEach((f) => {
    body.appendChild(el('tr', { class: f.buildable ? '' : 'kap-missing-feature' },
      el('td', {}, f.id),
      el('td', {}, el('span', {
        class: 'pill ' + (f.availability === 'live' ? 'good'
          : f.availability === 'offline_derivable' ? 'neutral' : 'warn'),
      }, f.availability.replace(/_/g, ' '))),
      el('td', {}, f.buildable
        ? (f.example ? el('span', {}, f.example, ' ', fpHonestyPill(f.example_honesty)) : '—')
        : el('span', { class: 'pill bad', title: f.absent_reason }, 'absent')),
      el('td', {}, [f.source_table, f.source_column].filter(Boolean).join('.') || '—'),
      el('td', {}, f.missing || '—'),
      el('td', {}, f.privacy),
      el('td', {}, f.buildable ? f.description : (f.description + ' — ' + (f.absent_reason || '')))));
  });
  host.appendChild(table);
}

// ── the correlation view ─────────────────────────────────────────────────────
function fpRenderCorrelations(corr) {
  const host = clear($('#fp-correlations'));
  host.appendChild(el('div', { class: 'section' }, el('h3', {}, 'Correlation view'),
    el('span', { class: 'section-note' },
      'Each feature against the time bucket of the next request, and against each '
      + 'strategy’s outcome.')));
  if (!corr || !corr.computed) {
    emptyState(host, 'Not yet computed', (corr && corr.note) || 'The correlation sweep has not run yet.');
    return;
  }
  if (!corr.features || !corr.features.length) {
    emptyState(host, 'Nothing reported', 'The sweep produced no feature rows.');
    return;
  }
  const table = el('table', { class: 'tbl', 'data-testid': 'fp-correlation-table' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Feature'), el('th', { class: 'num' }, 'vs next bucket'),
      el('th', {}, 'vs strategy outcome'))));
  const body = el('tbody');
  table.appendChild(body);
  corr.features.forEach((f) => {
    body.appendChild(el('tr', {},
      el('td', {}, f.feature),
      el('td', { class: 'num' }, f.vs_next_bucket.toFixed(3),
        fpRange(f.low, f.high, f.ranged, (v) => v.toFixed(3))),
      el('td', {}, (f.vs_strategy_outcome || [])
        .map((s) => s.strategy + ': ' + s.correlation.toFixed(3)).join('; ') || '—')));
  });
  host.appendChild(table);
  if (corr.generated_at) {
    host.appendChild(el('p', { class: 'hint' }, 'Sweep generated at ' + corr.generated_at + '.'));
  }
}

// ── the predictor comparison panel ──────────────────────────────────────────

/** fpCalibrationRow renders one calibration bin: mean predicted, observed rate with its
 *  Wilson interval, and the sample size the estimate rests on. */
function fpCalibrationRow(b) {
  return el('tr', {},
    el('td', {}, b.horizon), el('td', { class: 'num' }, b.bin_low.toFixed(2) + '–' + b.bin_high.toFixed(2)),
    el('td', { class: 'num' }, num(b.n)),
    el('td', { class: 'num' }, pct(b.mean_predicted * 100)),
    el('td', { class: 'num' }, pct(b.observed_rate * 100),
      fpRange(b.observed_low * 100, b.observed_high * 100, b.ranged, (v) => v.toFixed(1) + '%')),
    el('td', {}, fpHonestyPill(b.honesty)));
}

/** fpNetDollarsCell renders one predictor's net dollars against one baseline — the range
 *  this comparison's real uncertainty lives in is the OTHER baseline's own cell, not a
 *  fabricated interval on this one: see this file's own note beside the two columns. */
function fpNetDollarsCell(nd) {
  if (!nd) return el('td', { class: 'num' }, '—');
  return el('td', { class: 'num' }, nd.known ? usd(nd.net_usd) + ' (' + pct(nd.net_pct) + ')' : 'not priced',
    ' ', fpHonestyPill(nd.honesty));
}

function fpRenderPredictors(predictors) {
  const host = clear($('#fp-predictors'));
  host.appendChild(el('div', { class: 'section' }, el('h3', {}, 'Predictor comparison'),
    el('span', { class: 'section-note' },
      'Net dollars against BOTH baselines: never-ping (fixed-5m) and ping-everyone '
      + '(keepalive-5m). Pinging every eligible row with no feature already beats never-ping '
      + 'by 5.7%, so never-ping alone flatters every predictor by that margin.')));
  if (!predictors || !predictors.length) {
    emptyState(host, 'No predictors registered', 'The predictor registry is empty.');
    return;
  }
  predictors.forEach((p) => {
    const wrap = el('div', { class: 'section' });
    wrap.appendChild(el('h4', {}, p.id + '@' + p.version,
      ' ', p.validated ? el('span', { class: 'pill good' }, 'validated')
        : el('span', { class: 'pill warn', title: 'Visible for comparison, not armed for real traffic.' }, 'unvalidated'),
      !p.enforceable ? el('span', { class: 'pill bad' }, 'not enforceable') : null));
    if (p.description) wrap.appendChild(el('p', { class: 'section-note' }, p.description));

    const netTable = el('table', { class: 'tbl' },
      el('thead', {}, el('tr', {}, el('th', {}, ''), el('th', {}, 'vs never-ping (fixed-5m)'),
        el('th', {}, 'vs ping-everyone (keepalive-5m)'))),
      el('tbody', {}, el('tr', {},
        el('td', {}, 'Net dollars'), fpNetDollarsCell(p.vs_never_ping), fpNetDollarsCell(p.vs_ping_everyone))));
    wrap.appendChild(netTable);

    if (p.calibration && p.calibration.length) {
      const calTable = el('table', { class: 'tbl' },
        el('thead', {}, el('tr', {}, el('th', {}, 'Horizon'), el('th', {}, 'Predicted bin'),
          el('th', { class: 'num' }, 'n'), el('th', { class: 'num' }, 'Mean predicted'),
          el('th', { class: 'num' }, 'Observed'), el('th', {}, ''))));
      const cb = el('tbody');
      calTable.appendChild(cb);
      p.calibration.forEach((b) => cb.appendChild(fpCalibrationRow(b)));
      wrap.appendChild(calTable);
    } else {
      wrap.appendChild(el('p', { class: 'hint' }, 'No calibration data — no non-censored decision point had a usable prediction from this predictor in scope.'));
    }
    host.appendChild(wrap);
  });
}

// ── loading ────────────────────────────────────────────────────────────────
async function loadFeaturePanel() {
  loadingState($('#fp-features'), 4);
  try {
    fp.data = await api('featurepanel');
  } catch (err) {
    if (aborted(err)) return;
    errorState($('#fp-features'), 'Could not load the feature panel', err);
    return;
  }
  fpRenderFeatures(fp.data.features);
  fpRenderCorrelations(fp.data.correlations);
  fpRenderPredictors(fp.data.predictors);
}

// Registered here, so mounting this whole view is one line in the shared page.
Object.assign(loaders, { featurepanel: loadFeaturePanel });
