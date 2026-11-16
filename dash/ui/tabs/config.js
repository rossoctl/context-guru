// context-guru dashboard: the Config tab.
// Moved out of app.js (#1089). app.js loads first and holds the shared DOM helpers,
// formatting, state and router that this file uses. See the file list in index.html.
'use strict';

// ── config ─────────────────────────────────────────────────────────────────
function renderTree(v, key) {
  if (v === null || v === undefined) return el('div', { class: 'v', text: '—' });
  if (Array.isArray(v)) {
    return el('div', {}, el('div', { class: 'k', text: key }),
      el('div', { class: 'v', text: v.map((x) => (typeof x === 'object' ? JSON.stringify(x) : String(x))).join(', ') || '(empty)' }));
  }
  if (typeof v === 'object') {
    const box = el('details', { class: 'diff', open: key === undefined ? 'open' : null },
      el('summary', { text: key === undefined ? 'effective configuration' : key }));
    const inner = el('div', { style: 'padding:10px 12px' });
    const grid = el('div', { class: 'kv' });
    for (const [k, val] of Object.entries(v)) {
      if (val !== null && typeof val === 'object' && !Array.isArray(val)) inner.appendChild(renderTree(val, k));
      else grid.appendChild(kv(k, Array.isArray(val) ? (val.join(', ') || '(empty)') : String(val)));
    }
    inner.insertBefore(grid, inner.firstChild);
    box.appendChild(inner);
    return box;
  }
  return el('div', {}, el('div', { class: 'k', text: key }), el('div', { class: 'v', text: String(v) }));
}

/**
 * renderTenantConfig draws the account's OWN effective configuration — the document the
 * Settings page writes — above the process's.
 *
 * This is the fix for "a change made on the Settings page does not appear in the Config tab".
 * The cause was not staleness or caching: /api/config serves the configuration THIS PROXY
 * PROCESS resolved from --preset/--config at startup, and Settings writes the tenant's
 * config_yaml into a different database entirely. A tenant edit could never appear there, by
 * construction. The control plane already resolves and serves the true answer as
 * `effective_config` / `effective_config_yaml` on /api/me, and the Settings page already
 * fetches it — so this needs no new server route, only the right document on the page.
 *
 * Resolution is a CHOICE of one whole document, not a merge: an account whose config_yaml is
 * blank follows the hosted default, and an account with its own follows that one. So "which
 * values are overrides" is answered honestly at document level — this account either has its
 * own document or it does not — rather than by diffing two trees key-by-key and inventing a
 * per-key provenance the server does not actually have.
 */
function renderTenantConfig() {
  const panel = $('#config-account-panel');
  if (!panel) return;
  const t = account.tenant;
  // Only hosted deployments have a second document to disagree with. On a single-tenant
  // proxy the process config IS the answer, and a second panel saying so would be noise.
  panel.hidden = !account.hosted || !t;
  if (panel.hidden) {
    $('#config-server-title').textContent = 'Effective configuration';
    return;
  }
  // With an account document on screen, the panel below is demoted in its own title rather
  // than only in a note — a reader scanning headings has to be able to tell them apart.
  $('#config-server-title').textContent = "This proxy process's configuration";
  const host = clear($('#config-account-body'));
  const own = !!(t.config_yaml && t.config_yaml.trim());
  host.appendChild(el('div', { class: 'state' + (own ? '' : ' blocked'), 'data-testid': 'config-provenance' },
    el('div', { class: 'state-body' },
      el('strong', {}, own ? 'This account runs its own configuration'
        : 'This account follows the hosted default'),
      el('span', {}, own
        ? 'These values come from the settings you saved. They replace the shared default completely.'
        : 'You have not saved settings of your own, so these are the shared defaults. '
          + 'Saving on the Settings page replaces all of them, not just one.'),
      el('span', {}, 'The panel below is the proxy server\'s own startup settings. '
        + 'It is not what was applied to your traffic.'))));
  if (t.effective_config) {
    host.appendChild(renderTree(t.effective_config));
  } else if (t.effective_config_yaml) {
    // No parsed form from an older server: show the document verbatim rather than nothing.
    host.appendChild(el('pre', { class: 'yaml', text: t.effective_config_yaml }));
  } else {
    emptyState(host, 'No configuration reported for this account',
      'This server did not return an effective configuration. The panel below is the '
      + 'process configuration, which may not be what ran on your requests.');
  }
}

async function loadConfig() {
  renderTenantConfig();
  const host = clear($('#config-body'));
  loadingState(host, 3);
  renderLogsHelp();
  try {
    // The payload is {scope, config, description} — the same envelope /api/capture uses.
    // Rendering the envelope as the tree printed "scope: server" and "description: …" as
    // if they were configuration keys, and buried the config a level down.
    const cfg = await api('config');
    clear(host);
    // The caveat that matters stays in the layout; the server's full explanation folds.
    host.appendChild(el('p', { class: 'note', 'data-testid': 'config-scope', text:
      'Scope: ' + (cfg.scope || 'server') + ' — the values the proxy server started with'
      + (account.hosted
        ? ". These are not your account's settings; yours are in the panel above."
        : ', which on this single-account setup is also what every request used.') }));
    if (cfg.description) host.appendChild(whyBlock('Technical details: why the two can differ', cfg.description));
    const tree = cfg.config || cfg;
    if (!tree || !Object.keys(tree).length) emptyState(host, 'No settings reported', 'The proxy returned an empty configuration.');
    else host.appendChild(renderTree(tree));
  } catch (err) {
    if (aborted(err)) return;
    emptyState(host, err.status === 403 ? 'Configuration is not visible from this address'
      : 'Could not load configuration', String(err.message || err), { error: err.status !== 403 });
  }
  const chost = clear($('#capture-body'));
  // The capture counters are PROCESS-WIDE, so hosted mode serves them to managers only
  // (they leak other tenants' traffic volume). Rendering them as zeros for everyone else
  // would assert "nothing was dropped", which is a claim this page cannot make.
  if (account.hosted && !isManager()) {
    contentBlockedState(chost, 'capture_not_permitted');
    return;
  }
  try {
    const { capture: c, description } = await api('capture');
    chost.appendChild(el('div', { class: 'kv' },
      kv('Received', num(c.captured)), kv('Saved', num(c.written)),
      kv('Dropped', num(c.dropped)), kv('Save errors', num(c.errors)),
      kv(term('Waiting', 'Requests waiting to be saved / the most the queue can hold.'), c.queued + ' / ' + c.queue_cap),
      kv(term('Live viewers', 'Browsers currently receiving live updates from this proxy.'), num(c.sse_clients)),
      kv('Database', c.db_path || '(in memory — history is lost on restart)'),
      kv('Database size', compact(c.db_bytes).replace(/(\d)([KMGT]?)$/, (_, n, u) => n + ' ' + u + 'B'))));
    // A non-zero drop count is the one thing worth saying out loud here, because it means
    // every number on the Overview under-reports. The definitions fold.
    if (c.dropped > 0) {
      chost.appendChild(el('p', { class: 'note warn-text', text:
        num(c.dropped) + ' events dropped — every figure on this dashboard under-reports by ' +
        'that much. Raise the queue size or lower the traffic before drawing conclusions.' }));
    }
    if (description) chost.appendChild(whyBlock('Technical details: what these counters mean', description));
  } catch (err) {
    errorState(chost, 'Could not load capture health', err);
  }
}

Object.assign(loaders, { config: loadConfig });
