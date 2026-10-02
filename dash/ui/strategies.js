// Manager-controlled keep-alive strategies: a durable rule that runs above every tenant's own
// account switch and below a per-session override — see proxy/keepalivestrategy.go for the
// resolution chain and the 2026-09-28 keep-alive predictor study (PHASE2.md) for why this page
// composes the gates it does.
//
// One appended file, self-mounted the same way campaigns.js is (manager-only, no local-ok
// exemption, fetched by app.js's maybeLoadManagerScript the one time a hosted manager signs
// in) — but UNLIKE campaigns.js, this view's tab/panel come from index.html's own
// tpl-view-strategies template rather than mountTab(): that template, and the reveal of it,
// predate this split and are left exactly as they were. This file only fills what already
// exists (#strategy-form, #strategies-list, #strategies-count, #ka-tenant-econ). Every helper
// used here is app.js's own (el, clear, api, ctl, usd, num, when, tile, tileGroup, openDrawer,
// emptyState, errorState, loadingState, fieldError, $, $$) and KA_CALC_MAX_K, which stays
// defined in app.js because the plain Keep-alive tab's own calculator reads it too.
'use strict';

const strategyForm = {
  editingID: '', // '' = creating a new one
  windows: [],   // the windows accumulated for the form currently open
  tenants: [],   // the roster, for the account picker; loaded once
  // tenantCaps is the per-tenant cap-with-off-switch editor's own state: tenant id ->
  // {maxPings: number|null, off: bool}. This is the mechanism the 2026-08-25 predictor study
  // ranked above every flat cap and every predictor, including a well-calibrated one — and
  // the one thing the form could not express before this file existed (see
  // tenant.TenantCap/proxy's TenantCapFor). Independent of Target: an all-target strategy can
  // still turn itself off for one tenant.
  tenantCaps: {},
  // predictors is the server's own catalog (GET /api/keepalive/predictors), loaded once —
  // replaces a hardcoded mirror of proxy's knownPredictorIDs, so a predictor added on the
  // server appears here with no matching front-end edit, and an unvalidated research one
  // (kind:"trained", selectable_for_enforcement:false) is shown disabled rather than omitted:
  // visible for comparison, refused at save time by validPredictorRef regardless.
  predictors: [],
  // forecastGen guards refreshStrategyForecast's own async fetch the same way campaigns.js's
  // *Gen counters do: bumped at the start of every debounced call, checked again once the fetch
  // resolves, so a slow response to an earlier edit can never overwrite what a later edit is
  // now showing.
  forecastGen: 0,
  // previewGen is the same guard for runStrategyPreview — see its own comment.
  previewGen: 0,
};

const STRATEGY_DAYS = [
  [0, 'Sun'], [1, 'Mon'], [2, 'Tue'], [3, 'Wed'], [4, 'Thu'], [5, 'Fri'], [6, 'Sat'],
];

function dayLabel(days) {
  if (!days || !days.length) return 'every day';
  return [...days].sort((a, b) => a - b).map((d) => STRATEGY_DAYS[d][1]).join(',');
}
function windowLabel(w) {
  return `${dayLabel(w.days)} ${w.start}–${w.end} ${w.tz || 'Asia/Jerusalem'}`;
}

/**
 * loadKeepAliveMaxPingsByTenant renders the per-tenant max_pings economics table — see
 * PHASE2.md's finding P2-4: a single pooled curve harms roughly a third of tenants, some of
 * them at every setting, so this is per-tenant by construction, never averaged into one line.
 * The server already sorts worst-first (off-recommended, then lowest optimal net); nothing
 * here re-sorts or computes a dollar figure of its own.
 */
async function loadKeepAliveMaxPingsByTenant() {
  const host = clear($('#ka-tenant-econ'));
  loadingState(host, 3);
  let out;
  try {
    out = await ctl('/api/keepalive/strategies/max-pings-by-tenant');
  } catch (e) {
    clear(host);
    errorState(host, 'Could not compute the per-tenant max_pings economics', e);
    return;
  }
  clear(host);
  const rows = out.tenants || [];
  if (out.note) host.appendChild(el('p', { class: 'note' }, out.note));
  if (!rows.length) {
    emptyState(host, 'No tenant has keep-alive-relevant history in this deployment yet', '');
    return;
  }
  const tbl = el('table', { class: 'grid', 'data-testid': 'ka-tenant-econ-table' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Tenant'), el('th', { class: 'num' }, 'Decision points'),
      el('th', {}, 'Current'), el('th', {}, 'Modelled state'),
      el('th', { class: 'num' }, 'Optimal max_pings'), el('th', { class: 'num' }, 'Optimal net'))));
  const body = el('tbody');
  for (const t of rows) {
    const current = t.current_max_pings
      ? `${t.current_max_pings} (${t.current_strategy_id})` : 'none (account default)';
    let state, stateClass = '';
    if (!t.priced) {
      state = 'not priced';
    } else if (t.off_recommended) {
      state = 'keep-alive does not pay for this tenant at any setting — consider OFF';
      stateClass = 'bad-text';
    } else if (t.current_max_pings && t.current_max_pings === t.optimal_max_pings) {
      state = 'already at its own modelled optimum';
      stateClass = 'good-text';
    } else if (t.current_max_pings) {
      state = t.optimal_max_pings > t.current_max_pings
        ? 'this tenant’s own history says raise the cap' : 'this tenant’s own history '
          + 'says lower the cap';
    } else {
      state = 'no strategy currently matches this tenant';
    }
    body.appendChild(el('tr', { 'data-testid': 'ka-tenant-econ-' + t.tenant_id },
      el('td', {}, el('code', { class: 'clip' }, t.tenant_id)),
      el('td', { class: 'num' },
        num(t.decision_points) + (t.thin_data ? ' (thin — too little to trust)' : '')),
      el('td', {}, current),
      el('td', { class: stateClass }, state),
      el('td', { class: 'num' }, t.priced ? num(t.optimal_max_pings) : '—'),
      el('td', { class: 'num ' + (t.priced && t.optimal_net_usd < 0 ? 'bad-text' : '') },
        t.priced ? usd(t.optimal_net_usd) : '—')));
  }
  tbl.appendChild(body);
  host.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, tbl));
}

async function loadStrategies() {
  loadKeepAliveMaxPingsByTenant(); // independent read; its own try/catch, awaited separately
  const form = $('#strategy-form');
  if (!form.dataset.built) {
    try {
      strategyForm.tenants = (await ctl('/api/tenants')).tenants || [];
    } catch (_) { strategyForm.tenants = []; /* the picker still works for "every account" */ }
    try {
      strategyForm.predictors = (await ctl('/api/keepalive/predictors')).predictors || [];
    } catch (_) { strategyForm.predictors = []; /* the dropdown still works with just "None" */ }
    buildStrategyForm(form);
    form.dataset.built = '1';
  }
  const host = clear($('#strategies-list'));
  loadingState(host);
  try {
    const out = await ctl('/api/keepalive/strategies');
    const rows = out.strategies || [];
    $('#strategies-count').textContent = `${rows.length} strateg${rows.length === 1 ? 'y' : 'ies'}`;
    renderStrategiesList(clear(host), rows);
  } catch (e) {
    clear(host);
    errorState(host, 'Could not list strategies', e);
  }
}

/** buildStrategyForm draws a fresh create form. editStrategy repaints it pre-filled. */
function buildStrategyForm(form) {
  clear(form);
  strategyForm.editingID = '';
  strategyForm.windows = [];
  strategyForm.tenantCaps = {};
  $('#strategy-form-title').textContent = 'New strategy';

  const name = el('input', { type: 'text', id: 'sf-name', maxlength: '64', required: 'required' });
  const idle = el('input', { type: 'number', id: 'sf-idle', value: '280', min: '1' });
  const pings = el('input', { type: 'number', id: 'sf-pings', value: '1', min: '1' });
  const prefix = el('input', { type: 'number', id: 'sf-prefix', value: '20000', min: '0' });
  const usdCap = el('input', { type: 'number', id: 'sf-usd', value: '0', min: '0', step: '0.01' });
  const usdPerTenant = el('input', {
    type: 'number', id: 'sf-usd-tenant', value: '0', min: '0', step: '0.01',
    'data-testid': 'sf-usd-tenant',
  });
  const models = el('input', {
    type: 'text', id: 'sf-models', placeholder: 'e.g. aws/claude-sonnet-5, aws/claude-opus-5',
    'data-testid': 'sf-models',
  });
  const active = el('input', { type: 'checkbox', id: 'sf-active', checked: 'checked' });

  // Mode: shadow (the default for anything new) matches and is fully visible everywhere, but
  // never turns the keep-alive on, changes a ping schedule, or promotes the head-TTL tier —
  // see tenant.ModeShadow. Opt in to Enforce once a shadow strategy's own Stats drawer (once
  // it has some) and this form's Preview panel below look right.
  const mode = el('select', { id: 'sf-mode', 'data-testid': 'sf-mode' },
    el('option', { value: 'shadow' }, 'Shadow — matches, visible, never spends or pings (default)'),
    el('option', { value: 'enforce' }, 'Enforce — actually runs on live traffic'));
  const modeWarning = el('p', { class: 'hint', 'data-testid': 'sf-mode-warning' });
  const syncModeWarning = () => {
    modeWarning.textContent = mode.value === 'enforce'
      ? 'This strategy will change live traffic and live spend as soon as it is saved.'
      : 'Nothing below can spend money or send a ping while this stays in shadow.';
    modeWarning.className = 'hint' + (mode.value === 'enforce' ? ' bad-text' : '');
  };
  mode.addEventListener('change', syncModeWarning);

  // Predictor gate: optional, on top of the windows below. "" means no gate at all — every
  // strategy created before this field existed, and every strategy that leaves it unset,
  // behaves exactly as before. Options come from the server's own catalog (loadStrategies),
  // so a research-only entry (kind:"trained") is shown for comparison but disabled — it is
  // never selectable here, and validPredictorRef refuses it server-side regardless of what a
  // crafted request sends.
  const predictor = el('select', { id: 'sf-predictor', 'data-testid': 'sf-predictor' },
    el('option', { value: '' }, 'None — windows only (default)'),
    ...strategyForm.predictors.filter((p) => p.id !== '').map((p) => el('option', {
      value: p.id, disabled: p.selectable_for_enforcement ? null : 'disabled',
    }, p.description + (p.selectable_for_enforcement ? '' : ' — comparison only, not selectable'))));
  const predictorThreshold = el('input', {
    type: 'number', id: 'sf-predictor-threshold', value: '0.5', min: '0', max: '1', step: '0.01',
    disabled: 'disabled',
  });
  predictor.addEventListener('change', () => { predictorThreshold.disabled = !predictor.value; });

  const targetAll = el('input', { type: 'radio', name: 'sf-target-mode', value: 'all', checked: 'checked' });
  const targetList = el('input', { type: 'radio', name: 'sf-target-mode', value: 'list' });
  const targetIDs = el('select', {
    id: 'sf-target-ids', 'data-testid': 'sf-target-ids', multiple: 'multiple', size: '5', disabled: 'disabled',
  }, ...strategyForm.tenants.map((t) => el('option', { value: t.id }, t.label ? `${t.email} · ${t.label}` : t.email)));
  const syncTargetDisabled = () => { targetIDs.disabled = !targetList.checked; };
  targetAll.addEventListener('change', syncTargetDisabled);
  targetList.addEventListener('change', syncTargetDisabled);

  // ── per-tenant cap + off-switch ────────────────────────────────────────
  // The mechanism the study ranked above every flat cap and every predictor. Independent of
  // Target above: an ALL-target strategy can still be turned off for one tenant losing money
  // on it, without needing a second, list-target strategy just to carve that tenant out.
  const capsList = el('ul', { id: 'sf-tenant-caps-list', 'data-testid': 'sf-tenant-caps-list' });
  const paintCaps = () => {
    clear(capsList);
    for (const [id, c] of Object.entries(strategyForm.tenantCaps)) {
      const label = c.off ? `${id}: off` : `${id}: max_pings ${c.maxPings}`;
      capsList.appendChild(el('li', { 'data-testid': 'sf-tenant-cap-' + id }, label + ' ',
        el('button', {
          type: 'button', class: 'ghost small',
          onclick: () => { delete strategyForm.tenantCaps[id]; paintCaps(); },
        }, 'Remove')));
    }
  };
  const capTenant = el('select', { 'data-testid': 'sf-cap-tenant' },
    el('option', { value: '' }, 'Pick an account…'),
    ...strategyForm.tenants.map((t) => el('option', { value: t.id }, t.label ? `${t.email} · ${t.label}` : t.email)));
  const capMaxPings = el('input', {
    type: 'number', min: '0', placeholder: 'max_pings override', 'data-testid': 'sf-cap-maxpings',
  });
  const capOff = el('input', { type: 'checkbox', 'data-testid': 'sf-cap-off' });
  const addCap = el('button', {
    type: 'button', class: 'ghost small', 'data-testid': 'sf-cap-add',
    onclick: () => {
      if (!capTenant.value) return;
      if (!capOff.checked && capMaxPings.value === '') return;
      strategyForm.tenantCaps[capTenant.value] = capOff.checked
        ? { off: true }
        : { maxPings: Math.max(0, Number(capMaxPings.value) || 0) };
      capTenant.value = '';
      capMaxPings.value = '';
      capOff.checked = false;
      paintCaps();
    },
  }, 'Add override');
  const tenantCapsField = el('fieldset', { class: 'field' },
    el('legend', {}, 'Per-tenant overrides (cap or off) — beats Target for the tenants named here'),
    el('p', { class: 'note' },
      'Off excludes that one tenant from this strategy entirely, even under "Every account". ' +
      'A max_pings override replaces the strategy’s own Max pings for that tenant only — a ' +
      'lower ceiling where its own history says a flat cap loses money there, or 0 to the ' +
      'same effect as Off.'),
    el('div', { class: 'row-actions' }, capTenant, capMaxPings,
      el('label', {}, capOff, ' Off'), addCap),
    capsList);

  // refreshStrategyForecast: a MODELLED preview of this strategy's own economics, reusing the
  // Keep-alive tab's own calculator route (GET /api/keepalive/calc) rather than a second
  // simulator — see PHASE2.md's P2-1c/P2-1d finding that every active strategy today is set to
  // max_pings 1 or 2, well short of the modelled optimum, with no visible way to see that while
  // editing one. Informational only: it never writes anything and the optimum it names is never
  // applied for the caller — a human reads it and decides.
  let sfForecastDeb = null;
  const refreshStrategyForecast = () => {
    clearTimeout(sfForecastDeb);
    sfForecastDeb = setTimeout(async () => {
      const gen = ++strategyForm.forecastGen;
      const x = Math.max(1, Number(idle.value) || 280);
      const k = Math.max(1, Number(pings.value) || 1);
      let tenantQ = '', tenantNote = '';
      if (targetList.checked) {
        const picked = Array.from(targetIDs.selectedOptions).map((o) => o.value);
        if (picked.length === 0) {
          clear(forecast).appendChild(el('span', {}, 'Pick at least one account (or switch to '
            + '"Every account") to preview this setting’s modelled net.'));
          return;
        }
        tenantQ = '&tenant=' + encodeURIComponent(picked[0]);
        if (picked.length > 1) {
          tenantNote = ` Previewing on ${picked[0]} only — the first of ${picked.length} `
            + 'picked accounts; a combined preview across several accounts is not built yet.';
        }
      }
      if (k > KA_CALC_MAX_K) {
        clear(forecast).appendChild(el('span', {},
          `This preview only replays max_pings up to ${KA_CALC_MAX_K} — net falls off `
          + 'sharply well before then (see the Keep-alive tab’s own ladder), so a setting '
          + 'this high is not worth previewing.'));
        return;
      }
      clear(forecast).appendChild(el('span', { class: 'muted' }, 'Computing…'));
      let out;
      try {
        out = await ctl(`/api/keepalive/calc?x=${x}&k=${k}${tenantQ}`);
      } catch (e) {
        if (gen !== strategyForm.forecastGen) return;
        errorState(clear(forecast), 'Could not compute the modelled net', e);
        return;
      }
      if (gen !== strategyForm.forecastGen) return; // a newer edit already asked again
      clear(forecast);
      if (!out.priced || !out.rows || !out.rows.length) {
        forecast.appendChild(el('span', {},
          'No priced model on this scope’s own history to preview a modelled net against.'));
        return;
      }
      const current = out.rows.find((r) => r.max_pings === k);
      const optimal = out.rows.find((r) => r.optimal);
      const p = el('p', {}, el('strong', {
        class: current && current.net_usd < 0 ? 'bad-text' : '',
      }, `Modelled net at max_pings=${k}: `
        + (current ? usd(current.net_usd) : 'not enough history to price')));
      if (optimal && current && optimal.max_pings !== k) {
        p.appendChild(el('span', {}, ` · this scope’s modelled optimum is `
          + `max_pings=${optimal.max_pings} (${usd(optimal.net_usd)}) — shown for `
          + 'reference, not applied for you.'));
      }
      forecast.appendChild(p);
      // The specific case PHASE2.md's P2-4/P2-5 findings exist to warn about: a flat cap
      // applied everywhere harms roughly a third of tenants, some of them AT EVERY max_pings —
      // for those, no rung on this ladder is the fix, because none of them turns a profit. The
      // "optimal" row is still just the least-bad one when that happens, and saying only
      // "optimal" there would read as good news that is not there.
      if (optimal && optimal.net_usd <= 0) {
        forecast.appendChild(el('p', { class: 'hint bad-text' },
          `This scope’s own history has no max_pings setting that modelled net-positive — `
          + `even ${optimal.max_pings} (its least-bad rung) is ${usd(optimal.net_usd)}. `
          + 'Consider leaving keep-alive off for this scope rather than tuning it further.'));
      }
      forecast.appendChild(el('p', { class: 'hint' },
        'MODELLED: a replay of this scope’s own past idle gaps, assuming every ping refreshes '
        + 'the cache successfully — a per-scope replay, not this deployment’s pooled estimate, '
        + 'so the optimum here can differ from a service-wide figure quoted elsewhere. Realized '
        + 'keep-alive net has historically run well below a comparable modelled estimate on '
        + 'this deployment, so trust the SHAPE (which max_pings is better, or whether none is) '
        + 'far more than the absolute dollars.' + tenantNote));
    }, 300);
  };
  idle.addEventListener('input', refreshStrategyForecast);
  pings.addEventListener('input', refreshStrategyForecast);
  targetAll.addEventListener('change', refreshStrategyForecast);
  targetList.addEventListener('change', refreshStrategyForecast);
  targetIDs.addEventListener('change', refreshStrategyForecast);

  const dayBoxes = STRATEGY_DAYS.map(([v, label]) => el('label', { class: 'comp' },
    el('input', { type: 'checkbox', value: String(v), 'data-testid': 'sf-day-' + v }), ' ' + label));
  const winStart = el('input', { type: 'time', value: '09:00', 'data-testid': 'sf-window-start' });
  const winEnd = el('input', { type: 'time', value: '18:00', 'data-testid': 'sf-window-end' });
  const winTZ = el('input', { type: 'text', value: 'Asia/Jerusalem', 'data-testid': 'sf-window-tz' });
  const winList = el('ul', { id: 'sf-windows-list', 'data-testid': 'sf-windows-list' });
  const windowsField = el('fieldset', { class: 'field' },
    el('legend', {}, 'Windows (at least one; each is checked in its own timezone)'));

  const paintWindows = () => {
    clear(winList);
    strategyForm.windows.forEach((w, i) => {
      winList.appendChild(el('li', {}, windowLabel(w) + ' ',
        el('button', {
          type: 'button', class: 'ghost small', 'data-testid': 'sf-window-remove-' + i,
          onclick: () => { strategyForm.windows.splice(i, 1); paintWindows(); },
        }, 'Remove')));
    });
  };

  const addWindow = el('button', {
    type: 'button', class: 'ghost small', 'data-testid': 'sf-window-add',
    onclick: () => {
      const days = dayBoxes
        .map((box, i) => (box.querySelector('input').checked ? i : -1))
        .filter((i) => i >= 0);
      if (!winStart.value || !winEnd.value) {
        fieldError(windowsField, 'Give this window a start and an end.');
        return;
      }
      fieldError(windowsField, '');
      strategyForm.windows.push({
        days, start: winStart.value, end: winEnd.value, tz: winTZ.value.trim() || 'Asia/Jerusalem',
      });
      // Days are per-window, not sticky across additions — a manager building "9-12
      // weekdays" and "14-18 weekends" would otherwise have the second Add silently
      // reuse the first window's days.
      for (const box of dayBoxes) box.querySelector('input').checked = false;
      paintWindows();
    },
  }, 'Add window');

  windowsField.appendChild(el('div', { class: 'comp-grid' }, ...dayBoxes));
  windowsField.appendChild(el('label', {}, 'Start ', winStart));
  windowsField.appendChild(el('label', {}, 'End ', winEnd));
  windowsField.appendChild(el('label', {}, 'Timezone ', winTZ));
  windowsField.appendChild(addWindow);
  windowsField.appendChild(winList);
  windowsField.appendChild(el('p', { class: 'field-error', role: 'alert', hidden: true }));

  const preview = el('div', { id: 'sf-preview', 'data-testid': 'sf-preview' });
  const previewBtn = el('button', {
    type: 'button', class: 'ghost', 'data-testid': 'sf-preview-run',
    onclick: () => runStrategyPreview(preview, {
      idle, pings, usdPerTenant, models, targetAll, targetList, targetIDs,
    }),
  }, 'Preview against history');

  const status = el('p', { class: 'field-error', role: 'alert', hidden: true, 'data-testid': 'sf-status' });
  const submit = el('button', { type: 'submit', class: 'primary', 'data-testid': 'sf-submit' }, 'Create strategy');
  const cancel = el('button', {
    type: 'button', class: 'ghost', hidden: true, 'data-testid': 'sf-cancel',
    onclick: () => buildStrategyForm(form),
  }, 'Cancel edit');

  form.appendChild(el('div', { class: 'field' }, el('label', { for: 'sf-name' }, 'Name'), name));
  form.appendChild(el('div', { class: 'field' },
    el('label', { for: 'sf-mode' }, 'Mode'), mode, modeWarning));
  form.appendChild(el('div', { class: 'field' }, el('label', { for: 'sf-idle' }, 'Idle seconds'), idle));
  form.appendChild(el('div', { class: 'field' }, el('label', { for: 'sf-pings' }, 'Max pings'), pings));
  const forecast = el('div', { id: 'sf-forecast', class: 'note', 'data-testid': 'sf-forecast' });
  form.appendChild(forecast);
  form.appendChild(el('div', { class: 'field' },
    el('label', { for: 'sf-prefix' }, 'Min prefix tokens'), prefix));
  form.appendChild(el('div', { class: 'field' },
    el('label', { for: 'sf-usd' }, 'Max $/ping (0 = default)'), usdCap));
  form.appendChild(el('div', { class: 'field' },
    el('label', { for: 'sf-usd-tenant' }, 'Per-tenant $ budget (0 = none, advisory — see Preview)'),
    usdPerTenant));
  form.appendChild(el('div', { class: 'field' },
    el('label', { for: 'sf-models' }, 'Compatible models (comma-separated, blank = any; advisory)'),
    models));
  form.appendChild(el('div', { class: 'field' }, el('label', {}, active, ' Active')));
  form.appendChild(el('fieldset', { class: 'field' },
    el('legend', {}, 'Predictor gate (optional, in addition to the windows below)'),
    el('label', { for: 'sf-predictor' }, 'Predictor'), predictor,
    el('label', { for: 'sf-predictor-threshold' }, 'Minimum probability'), predictorThreshold));
  form.appendChild(el('fieldset', { class: 'field' },
    el('legend', {}, 'Target'),
    el('label', {}, targetAll, ' Every account'),
    el('label', {}, targetList, ' Pick accounts'),
    el('label', {}, 'Accounts (used only with "Pick accounts")', targetIDs)));
  form.appendChild(tenantCapsField);
  form.appendChild(windowsField);
  form.appendChild(el('div', { class: 'field' }, previewBtn, preview));
  form.appendChild(el('div', { class: 'actions' }, submit, cancel, status));
  refreshStrategyForecast(); // paint a preview for the defaults, not just after the first edit
  syncModeWarning();

  // editStrategy calls this fresh build and then overwrites the fields — simpler than a
  // second code path that patches an existing DOM tree field by field.
  form._fill = (s) => {
    strategyForm.editingID = s.id;
    strategyForm.windows = (s.windows || []).map((w) => ({ ...w }));
    strategyForm.tenantCaps = {};
    for (const [id, c] of Object.entries(s.tenant_caps || {})) {
      strategyForm.tenantCaps[id] = c.off ? { off: true } : { maxPings: c.max_pings || 0 };
    }
    $('#strategy-form-title').textContent = 'Edit: ' + s.name;
    name.value = s.name;
    mode.value = s.mode || 'shadow';
    idle.value = String(s.idle_seconds);
    pings.value = String(s.max_pings);
    prefix.value = String(s.min_prefix_tokens);
    usdCap.value = String(s.max_usd_per_ping);
    usdPerTenant.value = String(s.max_usd_per_tenant || 0);
    models.value = (s.models || []).join(', ');
    active.checked = !!s.active;
    predictor.value = s.predictor_id || '';
    predictorThreshold.value = String(s.predictor_threshold || 0.5);
    predictorThreshold.disabled = !predictor.value;
    if (s.target && s.target.mode === 'list') {
      targetList.checked = true;
      for (const o of targetIDs.options) o.selected = (s.target.tenant_ids || []).includes(o.value);
    } else {
      targetAll.checked = true;
    }
    syncTargetDisabled();
    syncModeWarning();
    paintWindows();
    paintCaps();
    refreshStrategyForecast(); // the preview must match the strategy actually loaded, not the defaults
    submit.textContent = 'Save changes';
    cancel.hidden = false;
  };

  form.onsubmit = async (ev) => {
    ev.preventDefault();
    status.hidden = true;
    if (strategyForm.windows.length === 0) {
      fieldError(windowsField, 'Add at least one window; a strategy with none can never fire.');
      return;
    }
    fieldError(windowsField, '');
    const tenantCapsOut = {};
    for (const [id, c] of Object.entries(strategyForm.tenantCaps)) {
      tenantCapsOut[id] = c.off ? { off: true } : { max_pings: c.maxPings };
    }
    const body = {
      name: name.value.trim(),
      mode: mode.value,
      idle_seconds: Number(idle.value) || 0,
      max_pings: Number(pings.value) || 0,
      min_prefix_tokens: Number(prefix.value) || 0,
      max_usd_per_ping: Number(usdCap.value) || 0,
      max_usd_per_tenant: Number(usdPerTenant.value) || 0,
      models: models.value.split(',').map((m) => m.trim()).filter(Boolean),
      active: active.checked,
      predictor_id: predictor.value,
      predictor_threshold: predictor.value ? (Number(predictorThreshold.value) || 0) : 0,
      windows: strategyForm.windows,
      tenant_caps: tenantCapsOut,
      target: targetList.checked
        ? { mode: 'list', tenant_ids: Array.from(targetIDs.selectedOptions).map((o) => o.value) }
        : { mode: 'all' },
    };
    submit.disabled = true;
    try {
      const editing = strategyForm.editingID;
      const path = editing ? '/api/keepalive/strategies/' + editing : '/api/keepalive/strategies';
      await ctl(path, { method: editing ? 'PATCH' : 'POST', body: JSON.stringify(body) });
      buildStrategyForm(form);
      loadStrategies();
    } catch (e) {
      status.textContent = e.message;
      status.hidden = false;
      submit.disabled = false;
    }
  };
}

/**
 * runStrategyPreview calls GET /api/keepalive/strategies/preview with exactly what the form
 * would submit, and renders the per-tenant bracket — see dash/strategypreview.go. Read-only:
 * this never creates or changes a strategy, only replays history against the candidate
 * currently on screen. previewGen guards it the same way refreshStrategyForecast's own gen
 * does, so a slow response to an earlier click can never overwrite a later one.
 */
async function runStrategyPreview(host, f) {
  const gen = ++strategyForm.previewGen;
  let tenantIDs;
  if (f.targetList.checked) {
    tenantIDs = Array.from(f.targetIDs.selectedOptions).map((o) => o.value);
    if (!tenantIDs.length) {
      clear(host).appendChild(el('p', { class: 'note' }, 'Pick at least one account to preview.'));
      return;
    }
  } else {
    tenantIDs = strategyForm.tenants.map((t) => t.id);
    if (!tenantIDs.length) {
      clear(host).appendChild(el('p', { class: 'note' },
        'No accounts on this deployment’s roster to preview against.'));
      return;
    }
  }
  const tenantCaps = {};
  for (const [id, c] of Object.entries(strategyForm.tenantCaps)) {
    tenantCaps[id] = c.off ? { off: true } : { max_pings: c.maxPings };
  }
  const candidate = {
    idle_seconds: Math.max(1, Number(f.idle.value) || 280),
    max_pings: Math.max(1, Number(f.pings.value) || 1),
    max_usd_per_tenant: Number(f.usdPerTenant.value) || 0,
    models: f.models.value.split(',').map((m) => m.trim()).filter(Boolean),
    tenant_ids: tenantIDs,
    tenant_caps: tenantCaps,
  };
  clear(host);
  loadingState(host, 2);
  let out;
  try {
    out = await api('keepalive/strategies/preview?candidate=' + encodeURIComponent(JSON.stringify(candidate)));
  } catch (e) {
    if (gen !== strategyForm.previewGen) return;
    errorState(clear(host), 'Could not preview this configuration', e);
    return;
  }
  if (gen !== strategyForm.previewGen) return;
  clear(host);
  for (const c of out.caveats || []) host.appendChild(el('p', { class: 'hint' }, c));
  host.appendChild(tileGroup(null, null, [
    tile('sp-ping', 'Total ping $ (upper bound)', usd(out.total_ping_usd)),
    tile('sp-net-low', 'Total net (conservative)', usd(out.total_net_usd_low), null,
      out.total_net_usd_low < 0 ? 'bad' : 'good'),
    tile('sp-net-high', 'Total net (generous)', usd(out.total_net_usd_high), null,
      out.total_net_usd_high < 0 ? 'bad' : 'good'),
    tile('sp-harmed', 'Tenants harmed either way', num((out.harmed_tenants || []).length)),
  ]));
  const cohorts = out.cohorts || [];
  if (!cohorts.length) {
    emptyState(host, 'No cohorts to preview', '');
    return;
  }
  const tbl = el('table', { class: 'grid compact', 'data-testid': 'sp-cohorts-table' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Tenant'), el('th', {}, 'State'), el('th', { class: 'num' }, 'Requests'),
      el('th', { class: 'num' }, 'Pings'), el('th', { class: 'num' }, 'Net (low..high)'),
      el('th', {}, ''))));
  const body = el('tbody');
  for (const c of cohorts) {
    let state = c.off ? 'off' : (c.priced ? '' : 'no priced history');
    body.appendChild(el('tr', { 'data-testid': 'sp-cohort-' + c.tenant_id },
      el('td', {}, el('code', { class: 'clip' }, c.tenant_id)),
      el('td', {}, state,
        c.thin_data ? el('span', { class: 'pill neutral' }, 'thin') : null,
        c.over_tenant_budget ? el('span', { class: 'pill missing' }, 'over budget') : null),
      el('td', { class: 'num' }, num(c.requests)),
      el('td', { class: 'num' }, c.off ? '—' : num(c.pings || 0)),
      el('td', { class: 'num ' + (c.harmed ? 'bad-text' : '') },
        c.priced ? `${usd(c.net_usd_low)} .. ${usd(c.net_usd_high)}` : '—'),
      el('td', {}, c.harmed ? el('span', { class: 'pill missing' }, 'harmed either way') : null)));
  }
  tbl.appendChild(body);
  host.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, tbl));
}

function editStrategy(s) {
  const form = $('#strategy-form');
  buildStrategyForm(form);
  form._fill(s);
  form.scrollIntoView({ block: 'start', behavior: 'smooth' });
}

async function toggleStrategyActive(s) {
  try {
    await ctl('/api/keepalive/strategies/' + s.id, {
      method: 'PATCH', body: JSON.stringify({ active: !s.active }),
    });
    loadStrategies();
  } catch (e) { alert(e.message); }
}

async function deleteStrategy(s) {
  if (!confirm(`Delete "${s.name}"? Anything it already pinged is not un-pinged; it just ` +
    'stops matching new requests.')) return;
  try {
    await ctl('/api/keepalive/strategies/' + s.id, { method: 'DELETE' });
    loadStrategies();
  } catch (e) { alert(e.message); }
}

/** openStrategyLedger shows one strategy's per-tenant economics, in the shared drawer. */
async function openStrategyLedger(s) {
  const body = openDrawer('Strategy: ' + s.name, null);
  loadingState(body, 2);
  try {
    const led = await api('keepalive/strategies/' + s.id + '/ledger');
    clear(body);
    body.appendChild(tileGroup(null, null, [
      tile('sl-pings', 'Pings', num(led.pings)),
      tile('sl-ping-usd', 'Ping cost', usd(led.ping_usd)),
      tile('sl-saved', 'Saved', usd(led.saved_usd)),
      tile('sl-net', 'Net', usd(led.net_usd), null, led.net_usd < 0 ? 'bad' : 'good'),
    ]));
    body.appendChild(el('p', { class: 'note' },
      'Saved is only the credit THIS strategy’s own pings earned — a request rescued by a ' +
      'different strategy, or by account config or a session override with no strategy at ' +
      'all, is not counted here. This ledger is ALL TIME — unlike Overview or the Keep-Alive ' +
      'tab, it ignores whatever date range the dashboard is set to, so a lower or higher ' +
      'number here than those pages show is not a discrepancy.'));
    // Pings > 0 with Saved stuck at exactly $0 is not a broken calculation — it is what a
    // strategy looks like before its FIRST real rescue under the current attribution code
    // (2026-08-30). A ping only earns a Saved credit once the real request it protected
    // actually resumes after the idle gap; every ping this strategy has sent so far either
    // predates that code (so the row it rescued was written before this column existed to
    // carry the strategy id at all) or has not yet been followed by such a resumption. Saved
    // populates the next time this strategy is in a matching window AND a session it pinged
    // comes back — there is nothing to fix here by waiting longer on this page.
    if (led.pings > 0 && led.saved_usd === 0) {
      body.appendChild(el('p', { class: 'note' },
        'Saved reads $0 with real pings above: none of them has yet been followed by the ' +
        'real request it protected actually resuming — that is the moment a ping turns into ' +
        'a credit, not the moment it is sent. This is expected for a strategy whose pings are ' +
        'all recent or predate 2026-08-30’s per-strategy attribution; it is not a stuck ' +
        'calculation, and it will move the next time this strategy pings a session that then comes back.'));
    }
    if (!led.tenants || !led.tenants.length) {
      emptyState(body, 'No pings under this strategy yet', '');
      return;
    }
    const tbl = el('table', { class: 'grid' },
      el('thead', {}, el('tr', {},
        el('th', {}, 'Account'), el('th', { class: 'num' }, 'Pings'),
        el('th', { class: 'num' }, 'Ping cost'), el('th', { class: 'num' }, 'Saved'),
        el('th', { class: 'num' }, 'Net'))));
    const tbody = el('tbody');
    for (const r of led.tenants) {
      tbody.appendChild(el('tr', {},
        el('td', {}, el('code', { class: 'clip' }, r.tenant_id)),
        el('td', { class: 'num' }, num(r.pings)),
        el('td', { class: 'num' }, usd(r.ping_usd)),
        el('td', { class: 'num' }, usd(r.saved_usd)),
        el('td', { class: 'num ' + (r.net_usd < 0 ? 'bad-text' : 'good-text') }, usd(r.net_usd))));
    }
    tbl.appendChild(tbody);
    body.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, tbl));
  } catch (e) {
    clear(body);
    errorState(body, 'Could not read this strategy’s ledger', e);
  }
}

function renderStrategiesList(host, rows) {
  if (!rows.length) {
    emptyState(host, 'No strategies yet', 'Create one above.');
    return;
  }
  host.appendChild(el('p', { class: 'note' },
    'Each strategy’s “Stats” drawer shows pings, cost, and Saved — all exact and additive ' +
    'across strategies, no double-counting. They will not sum to the Overview or ' +
    'Keep-Alive tab’s total, though: a credit whose ping matched no strategy (plain ' +
    'account config or a session override) belongs to none of these rows and only shows ' +
    'up in the account-wide total.'));
  const tbl = el('table', { class: 'grid' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Name'), el('th', {}, 'Windows'), el('th', {}, 'Target'),
      el('th', {}, 'Idle / pings'), el('th', {}, 'State'),
      el('th', {}, el('span', { class: 'vh' }, 'Row actions')))));
  const body = el('tbody');
  for (const s of rows) {
    const overrideCount = Object.keys(s.tenant_caps || {}).length;
    body.appendChild(el('tr', { class: s.active ? '' : 'revoked' },
      el('td', {}, s.name),
      el('td', {}, (s.windows || []).map(windowLabel).join('; ') || '—',
        s.predictor_id
          ? el('div', { class: 'muted small' }, 'gated: ' + s.predictor_id
            + ' ≥ ' + s.predictor_threshold)
          : null,
        overrideCount
          ? el('div', { class: 'muted small' }, `${overrideCount} tenant override(s)`)
          : null),
      el('td', {}, s.target && s.target.mode === 'list'
        ? `${(s.target.tenant_ids || []).length} account(s)` : 'every account'),
      el('td', {}, `${s.idle_seconds}s / ${s.max_pings}`),
      el('td', {},
        el('span', { class: 'pill ' + (s.active ? 'complete' : 'partial') }, s.active ? 'active' : 'paused'),
        el('div', { class: 'muted small' },
          el('span', { class: 'pill ' + (s.enforcing ? 'complete' : 'neutral') },
            s.enforcing ? 'enforcing' : 'shadow')),
        s.in_window ? el('div', { class: 'muted small' }, 'in a matching window right now') : null),
      el('td', {}, el('div', { class: 'row-actions' },
        el('button', { class: 'ghost small', onclick: () => toggleStrategyActive(s) },
          s.active ? 'Pause' : 'Resume'),
        el('button', { class: 'ghost small', onclick: () => editStrategy(s) }, 'Edit'),
        el('button', { class: 'ghost small', onclick: () => openStrategyLedger(s) }, 'Stats'),
        el('button', { class: 'ghost small', onclick: () => deleteStrategy(s) }, 'Delete')))));
  }
  tbl.appendChild(body);
  host.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, tbl));
}

// ── wiring ─────────────────────────────────────────────────────────────────
Object.assign(loaders, { strategies: loadStrategies });
UNFILTERED_VIEWS.add('strategies');
