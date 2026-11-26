// context-guru dashboard: the Cache strategies tab (manager).
// Moved out of app.js (#1089). app.js loads first and holds the shared DOM helpers,
// formatting, state and router that this file uses. See the file list in index.html.
'use strict';

// ── strategies (manager) ─────────────────────────────────────────────────────
//
// Manager-controlled keep-alive strategies: a durable rule that runs above every
// tenant's own account switch and below a per-session override — see
// docs/superpowers/specs/2026-08-25-keepalive-strategies-design.md. The routes
// themselves (proxy/keepalivestrategy.go) are the control plane, like the tenant
// roster's, so every write here goes through ctl() rather than api().
//
// The form is built once in JS (buildStrategyForm), the same way the Feedback form is:
// the fields, the window builder and the account picker live here so they cannot drift
// from the validation the control route itself enforces.

const strategyForm = {
  editingID: '', // '' = creating a new one
  windows: [],   // the windows accumulated for the form currently open
  tenants: [],   // the roster, for the account picker; loaded once
};

const STRATEGY_DAYS = [
  [0, 'Sun'], [1, 'Mon'], [2, 'Tue'], [3, 'Wed'], [4, 'Thu'], [5, 'Fri'], [6, 'Sat'],
];

// STRATEGY_PREDICTORS mirrors proxy's knownPredictorIDs (keepalivestrategy.go) — the
// server is the actual source of truth and refuses anything not in that map, so this
// list only has to be right, not authoritative.
const STRATEGY_PREDICTORS = [
  { id: 'stop-reason-gated', label: 'Stop-reason gate — ping only on end_turn/max_tokens/refusal '
    + '(measured +1.54% vs fixed-5m, see the KV-cache page)' },
];

function dayLabel(days) {
  if (!days || !days.length) return 'every day';
  return [...days].sort((a, b) => a - b).map((d) => STRATEGY_DAYS[d][1]).join(',');
}
function windowLabel(w) {
  return `${dayLabel(w.days)} ${w.start}–${w.end} ${w.tz || 'Asia/Jerusalem'}`;
}

async function loadStrategies() {
  const form = $('#strategy-form');
  if (!form.dataset.built) {
    try {
      strategyForm.tenants = (await ctl('/api/tenants')).tenants || [];
    } catch (_) { strategyForm.tenants = []; /* the picker still works for "every account" */ }
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
  $('#strategy-form-title').textContent = 'New strategy';

  const name = el('input', { type: 'text', id: 'sf-name', maxlength: '64', required: 'required' });
  const idle = el('input', { type: 'number', id: 'sf-idle', value: '240', min: '1' });
  const pings = el('input', { type: 'number', id: 'sf-pings', value: '1', min: '1' });
  const prefix = el('input', { type: 'number', id: 'sf-prefix', value: '80000', min: '0' });
  const usdCap = el('input', { type: 'number', id: 'sf-usd', value: '0', min: '0', step: '0.01' });
  const active = el('input', { type: 'checkbox', id: 'sf-active', checked: 'checked' });

  // Predictor gate: optional, on top of the windows above. "" means no gate at all —
  // every strategy created before this field existed, and every strategy that leaves it
  // unset, behaves exactly as before. The option list is short and server-validated
  // (STRATEGY_PREDICTORS mirrors proxy's own knownPredictorIDs) rather than free text,
  // since an unregistered id is refused at save time either way.
  const predictor = el('select', { id: 'sf-predictor', 'data-testid': 'sf-predictor' },
    el('option', { value: '' }, 'None — windows only (default)'),
    ...STRATEGY_PREDICTORS.map((p) => el('option', { value: p.id }, p.label)));
  const predictorThreshold = el('input', {
    type: 'number', id: 'sf-predictor-threshold', value: '0.5', min: '0', max: '1', step: '0.01',
    disabled: 'disabled',
  });
  predictor.addEventListener('change', () => { predictorThreshold.disabled = !predictor.value; });

  const targetAll = el('input', { type: 'radio', name: 'sf-target-mode', value: 'all', checked: 'checked' });
  const targetList = el('input', { type: 'radio', name: 'sf-target-mode', value: 'list' });
  const targetIDs = el('select', {
    id: 'sf-target-ids', 'data-testid': 'sf-target-ids', multiple: 'multiple', size: '3', disabled: 'disabled',
  }, ...strategyForm.tenants.map((t) => el('option', { value: t.id }, t.label ? `${t.email} · ${t.label}` : t.email)));
  const syncTargetDisabled = () => { targetIDs.disabled = !targetList.checked; };
  targetAll.addEventListener('change', syncTargetDisabled);
  targetList.addEventListener('change', syncTargetDisabled);

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
        // A time input tops out at 23:59, and the server's window end is exclusive, so a window
        // ending 23:59 would leave that last minute of the day uncovered. Send the end-of-day
        // spelling instead.
        days, start: winStart.value, end: winEnd.value === '23:59' ? '24:00' : winEnd.value,
        tz: winTZ.value.trim() || 'Asia/Jerusalem',
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

  const status = el('p', { class: 'field-error', role: 'alert', hidden: true, 'data-testid': 'sf-status' });
  const submit = el('button', { type: 'submit', class: 'primary', 'data-testid': 'sf-submit' }, 'Create strategy');
  const cancel = el('button', {
    type: 'button', class: 'ghost', hidden: true, 'data-testid': 'sf-cancel',
    onclick: () => buildStrategyForm(form),
  }, 'Cancel edit');

  form.appendChild(el('div', { class: 'field' }, el('label', { for: 'sf-name' }, 'Name'), name));
  form.appendChild(el('div', { class: 'field' }, el('label', { for: 'sf-idle' }, term('Idle seconds', 'How long a session must sit quiet before the first ping is sent. Keep it under 300, the cache lifetime.')), idle));
  form.appendChild(el('div', { class: 'field' }, el('label', { for: 'sf-pings' }, term('Max pings', 'The most pings sent in a row for one idle spell. More pings keep the cache alive longer but cost more.')), pings));
  form.appendChild(el('div', { class: 'field' },
    el('label', { for: 'sf-prefix' }, term('Min prefix tokens', 'Only sessions whose cached prompt is at least this many tokens are pinged. Small prompts are cheap to rewrite, so pinging them rarely pays.')), prefix));
  form.appendChild(el('div', { class: 'field' },
    el('label', { for: 'sf-usd' }, 'Max $/ping (0 = no flat cap)'), usdCap, el('p', { class: 'hint' }, 'Skips a ping that would cost more than this. 0 sets no flat cap: a ping is sent when the planned pings cost less than the cache miss they protect, and the account\'s own cap, if any, stays.')));
  form.appendChild(el('div', { class: 'field' }, el('label', {}, active, ' Active')));
  form.appendChild(el('fieldset', { class: 'field' },
    el('legend', {}, term('Predictor gate', 'An extra check that only pings when the session looks likely to come back soon. Optional, and applied on top of the time windows.'), ' (optional, in addition to the windows below)'),
    el('label', { for: 'sf-predictor' }, 'Predictor'), predictor,
    el('label', { for: 'sf-predictor-threshold' }, 'Minimum probability'), predictorThreshold));
  form.appendChild(el('fieldset', { class: 'field' },
    el('legend', {}, 'Target'),
    el('label', {}, targetAll, ' Every account'),
    el('label', {}, targetList, ' Pick accounts'),
    el('label', {}, 'Accounts (used only with "Pick accounts")', targetIDs)));
  form.appendChild(windowsField);
  form.appendChild(el('div', { class: 'actions' }, submit, cancel, status));

  // editStrategy calls this fresh build and then overwrites the fields — simpler than a
  // second code path that patches an existing DOM tree field by field.
  form._fill = (s) => {
    strategyForm.editingID = s.id;
    strategyForm.windows = (s.windows || []).map((w) => ({ ...w }));
    $('#strategy-form-title').textContent = 'Edit: ' + s.name;
    name.value = s.name;
    idle.value = String(s.idle_seconds);
    pings.value = String(s.max_pings);
    prefix.value = String(s.min_prefix_tokens);
    usdCap.value = String(s.max_usd_per_ping);
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
    paintWindows();
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
    const body = {
      name: name.value.trim(),
      idle_seconds: Number(idle.value) || 0,
      max_pings: Number(pings.value) || 0,
      min_prefix_tokens: Number(prefix.value) || 0,
      max_usd_per_ping: Number(usdCap.value) || 0,
      active: active.checked,
      predictor_id: predictor.value,
      predictor_threshold: predictor.value ? (Number(predictorThreshold.value) || 0) : 0,
      windows: strategyForm.windows,
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
      'all, is not counted here. This ledger is ALL TIME — unlike Overview or the Cache strategy ' +
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
  host.appendChild(el('p', { class: 'hint' },
    'One row per rule. Open “Stats” on a row to see what it spent and saved. ' +
    'These do not add up to the Overview or Cache strategy totals, because pings sent under plain account ' +
    'settings or a per-session override belong to no rule.'));
  const tbl = el('table', { class: 'grid' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Name'), el('th', {}, 'Windows'), el('th', {}, 'Target'),
      el('th', {}, 'Idle / pings'), el('th', {}, 'State'),
      el('th', {}, el('span', { class: 'vh' }, 'Row actions')))));
  const body = el('tbody');
  for (const s of rows) {
    body.appendChild(el('tr', { class: s.active ? '' : 'revoked' },
      el('td', {}, s.name),
      el('td', {}, (s.windows || []).map(windowLabel).join('; ') || '—',
        s.predictor_id
          ? el('div', { class: 'muted small' }, 'gated: ' + s.predictor_id
            + ' ≥ ' + s.predictor_threshold)
          : null),
      el('td', {}, s.target && s.target.mode === 'list'
        ? `${(s.target.tenant_ids || []).length} account(s)` : 'every account'),
      el('td', {}, `${s.idle_seconds}s / ${s.max_pings}`),
      el('td', {},
        el('span', { class: 'pill ' + (s.active ? 'complete' : 'partial') }, s.active ? 'active' : 'paused'),
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

Object.assign(loaders, { strategies: loadStrategies });
UNFILTERED_VIEWS.add('strategies');
