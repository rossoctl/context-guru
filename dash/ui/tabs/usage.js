// context-guru dashboard: the Usage tab.
// Moved out of app.js (#1089). app.js loads first and holds the shared DOM helpers,
// formatting, state and router that this file uses. See the file list in index.html.
'use strict';

/** DAY_MS is dash.DayMs: per-day bars are the shared time series at a day-wide bucket. */
const DAY_MS = 86400000;

/** Labels for the breakdown dimensions the server offers. Unknown keys fall back to the
 *  raw name, so a dimension added server-side appears in the picker without a UI change. */
const DIM_LABELS = {
  model: 'model', provider: 'provider', agent: 'agent', preset: 'preset', mode: 'mode',
  reasoning_effort: 'reasoning effort', thinking_mode: 'thinking mode',
  stop_reason: 'stop reason', tool_choice: 'tool choice',
  cache_miss_reason: 'cache outcome', cache_breakpoints: 'cache_control breakpoints',
  cache_bp_location: 'breakpoint placement', cache_ttl: 'cache lifetime asked for',
  stream: 'streaming',
};

/** dailyColumns: one column per UTC day (spent), the period total in one sentence above. */
function dailyColumns(host, totalHost, buckets) {
  clear(host);
  if (!buckets.length) { totalHost.textContent = ''; emptyState(host, 'No days in this window', 'Send traffic through the proxy, or widen the time range.'); return; }
  if (noMoneyChart(host)) { totalHost.textContent = ''; return; }
  const spent = (b) => b.cost_usd + b.cg_llm_cost_usd;
  const sum = (f) => buckets.reduce((n, b) => n + f(b), 0);
  const priced = buckets.some((b) => !(b.baseline_cost_usd === 0 && b.cost_usd === 0));
  totalHost.textContent = priced
    ? 'You spent ' + usd(sum(spent)) + ' over ' + num(buckets.length) + ' days and saved ' + usd(sum((b) => b.saved_usd)) + ' against the same work without context-guru.'
    : 'No priced days in this window, so spend and savings are unknown.';
  const max = Math.max(...buckets.map(spent), Number.MIN_VALUE);
  const cols = el('div', { class: 'daycols' }, el('span', { class: 'daycols-max', text: usd(max) }));
  for (const b of buckets) {
    const day = new Date(b.ts).toISOString().slice(0, 10);
    cols.appendChild(el('span', {
      class: 'daycol', tabindex: '0',
      title: day + ': spent ' + usd(spent(b)) + ', saved ' + usd(b.saved_usd)
        + ((b.defer_net_usd || 0) > 0 ? ', tool deferral saved ' + usd(b.defer_net_usd) : '') + ', ' + num(b.requests) + ' requests',
    }, el('i', { style: 'height:' + Math.max(2, (spent(b) / max) * 100) + '%;background:' + SERIES_SPENT }),
    // Tool deferral's net saving that day, its own series beside the spend. Only on a day that deferred.
    (b.defer_net_usd || 0) > 0
      ? el('i', { class: 'daycol-defer', style: 'height:' + Math.max(2, (b.defer_net_usd / max) * 100) + '%;background:' + SERIES_SAVED })
      : null));
  }
  host.appendChild(cols);
  if (buckets.some((b) => (b.defer_net_usd || 0) > 0)) {
    host.appendChild(el('div', { class: 'daycols-key' }, el('i', { style: 'background:' + SERIES_SPENT }), 'spent ',
      el('i', { style: 'background:' + SERIES_SAVED }), 'tool deferral saved'));
  }
  host.appendChild(el('div', { class: 'daycols-axis' },
    el('span', { text: new Date(buckets[0].ts).toISOString().slice(0, 10) }),
    el('span', { text: 'tallest day ' + usd(max) }),
    el('span', { text: new Date(buckets[buckets.length - 1].ts).toISOString().slice(0, 10) })));
}

async function loadUsage() {
  const dayHost = $('#chart-daily');
  const breakHost = $('#chart-breakdown');
  const body = clear($('#breakdown-body'));
  loadingRows(body, 10);
  try {
    // Per-day bars need no endpoint of their own: the series is bucketed in SQL from the
    // raw timestamp, so a day-wide bucket is a query parameter.
    //
    // /api/stats comes along for the cache panel. It is the same aggregate Overview already
    // fetches and it honours the same filters, so asking for it here keeps the cache detail
    // consistent with the rest of the page instead of inventing a second endpoint.
    const [{ buckets }, bd, o] = await Promise.all([
      api('series', { bucket: DAY_MS }),
      api('breakdown', { dim: state.dim }),
      api('stats'),
    ]);
    renderCacheDetail(o);
    dailyColumns(dayHost, $('#daily-total'), buckets || []);

    syncDimPicker(bd.dimensions || []);
    $('#breakdown-key-head').textContent = DIM_LABELS[bd.dim] || bd.dim;
    const groups = bd.groups || [];
    clear(body);
    if (!groups.length) {
      tableMessage(body, 10, 'Nothing to break down', 'No requests match the current filters.');
      emptyState(breakHost, 'Nothing to break down', 'No requests match the current filters.');
      return;
    }
    // One stacked bar of spend by group (top 4, the tail folded), then the table.
    const ranked = groups.filter((g) => g.incomplete_rows < g.requests && g.spent_usd > 0)
      .sort((x, y) => y.spent_usd - x.spent_usd);
    const rows = ranked.slice(0, 4).map((g) => ({ label: g.key || '(not set)', value: g.spent_usd }));
    const tail = ranked.slice(4).reduce((n, g) => n + g.spent_usd, 0);
    if (tail > 0) rows.push({ label: 'other', value: tail, color: 'var(--fg-faint)' });
    stackedShare(breakHost, rows, { format: usd, empty: 'No priced spend', emptyDetail: 'Every group here is unpriced.' });
    for (const g of groups) {
      const unpriced = g.incomplete_rows >= g.requests;
      body.appendChild(el('tr', {},
        el('td', { text: g.key || '(not set)' }),
        el('td', { class: 'num', text: num(g.requests) }),
        el('td', { class: 'num' + (unpriced ? ' na' : ''), text: unpriced ? 'unknown' : usd(g.spent_usd) }),
        el('td', { class: 'num' + (unpriced ? ' na' : ''), text: unpriced ? 'unknown' : usd(g.saved_usd) }),
        el('td', { class: 'num more', text: compact(g.tokens_before) }),
        el('td', { class: 'num more', text: compact(g.tokens_after) }),
        el('td', { class: 'num more', text: compact(g.saved_unique) }),
        el('td', { class: 'num more' + (unpriced ? ' na' : ''), text: unpriced ? 'unknown' : usd(g.total_saved_usd) }),
        el('td', { class: 'num more', text: num(g.incomplete_rows) })));
    }
  } catch (err) {
    if (aborted(err)) return;
    clear(body);
    tableMessage(body, 10, 'Could not load the breakdown', err.message);
    emptyState(breakHost, 'Could not load the breakdown', err.message, { error: true });
    emptyState(dayHost, 'Could not load daily usage', err.message, { error: true });
  }
}


/** syncDimPicker fills the dimension picker from what the SERVER says it can group by,
 *  so the options can never name a dimension the query would reject. */
function syncDimPicker(dims) {
  const sel = $('#f-dim');
  if (sel.options.length !== dims.length) {
    clear(sel);
    for (const d of dims) sel.appendChild(el('option', { value: d }, DIM_LABELS[d] || d));
  }
  sel.value = state.dim;
}

Object.assign(loaders, { usage: loadUsage });
