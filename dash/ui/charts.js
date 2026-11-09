// Charts for the Overview's "Detailed diagnostics". Hand-drawn SVG like the rest of the page (no chart
// library, nothing from a CDN), built around four rules:
//   - one idea per chart, and the title says it;
//   - the series are labelled on the plot itself, not in a legend you have to decode;
//   - hovering any point shows the exact figures for that time;
//   - the data is binned to a readable number of marks (at most 48) rather than drawn point by point.
// Motion is a short grow-in and stops under prefers-reduced-motion (see .plot rules in style.css).
// Depends on app.js globals: el, clear, svgEl, usd, num, compact, ms, dur, emptyState, SERIES.
'use strict';

const CGCharts = (() => {
  const MAX_BINS = 48;

  /** rebin collapses buckets to at most n bins. `sum` fields are added, `mean` fields are request-weighted. */
  function rebin(buckets, sum, mean, n = MAX_BINS) {
    if (!buckets.length) return [];
    const per = Math.max(1, Math.ceil(buckets.length / n));
    const out = [];
    for (let i = 0; i < buckets.length; i += per) {
      const grp = buckets.slice(i, i + per);
      const bin = { ts: grp[0].ts, end: grp[grp.length - 1].ts, requests: 0 };
      for (const k of sum) bin[k] = grp.reduce((s, b) => s + (b[k] || 0), 0);
      const w = grp.reduce((s, b) => s + (b.requests || 0), 0);
      bin.requests = grp.reduce((s, b) => s + (b.requests || 0), 0);
      for (const k of mean) bin[k] = w ? grp.reduce((s, b) => s + (b[k] || 0) * (b.requests || 0), 0) / w : 0;
      out.push(bin);
    }
    return out;
  }

  /** The label for a time on the x axis: a date for long windows, a clock time for short ones. */
  function axisLabel(ts, spanMs) {
    const d = new Date(ts);
    if (spanMs <= 2 * 86400000) return d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
    return d.toLocaleDateString([], { month: 'short', day: 'numeric' });
  }
  const tipTime = (ts) => new Date(ts).toLocaleString([], { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });

  function niceTicks(lo, hi, count = 3) {
    if (hi === lo) return [lo];
    const raw = (hi - lo) / count;
    const mag = Math.pow(10, Math.floor(Math.log10(raw)));
    const step = [1, 2, 2.5, 5, 10].map((m) => m * mag).find((s) => s >= raw) || raw;
    const out = [];
    for (let t = Math.ceil(lo / step) * step; t <= hi + step * 1e-9; t += step) out.push(+t.toFixed(10));
    return out;
  }

  /** A plot frame: an svg of the host's real width, an HTML tooltip, and helpers to place marks. */
  function frame(host, h, label) {
    clear(host);
    host.classList.add('plot');
    const w = Math.max(300, Math.round(host.clientWidth || 640));
    const svg = svgEl('svg', { viewBox: `0 0 ${w} ${h}`, role: 'img', 'aria-label': label, class: 'plot-svg' });
    const tip = el('div', { class: 'plot-tip', role: 'status', hidden: 'hidden' });
    host.append(svg, tip);
    return { svg, tip, w, h };
  }
  const text = (svg, attrs, str) => { const t = svgEl('text', attrs); t.textContent = str; svg.appendChild(t); return t; };

  /** Hover: a vertical rule, and a tooltip listing `rows(i)` for the nearest bin. */
  function hover(f, xs, bins, rows, pad) {
    const rule = svgEl('line', { class: 'plot-rule', y1: pad.t, y2: f.h - pad.b, visibility: 'hidden' });
    f.svg.appendChild(rule);
    const hit = svgEl('rect', { x: pad.l, y: pad.t, width: f.w - pad.l - pad.r, height: f.h - pad.t - pad.b, fill: 'transparent' });
    f.svg.appendChild(hit);
    const show = (ev) => {
      const r = f.svg.getBoundingClientRect();
      const x = ((ev.clientX - r.left) / r.width) * f.w;
      let best = 0;
      for (let i = 1; i < xs.length; i++) if (Math.abs(xs[i] - x) < Math.abs(xs[best] - x)) best = i;
      rule.setAttribute('x1', xs[best]); rule.setAttribute('x2', xs[best]); rule.setAttribute('visibility', 'visible');
      f.tip.hidden = false;
      clear(f.tip);
      f.tip.appendChild(el('strong', { text: tipTime(bins[best].ts) }));
      for (const [k, v, color] of rows(best)) {
        f.tip.appendChild(el('div', {}, color ? el('i', { class: 'swatch' }) : null, k + ' ', el('b', { text: v })));
        if (color) f.tip.lastChild.firstChild.style.background = color;
      }
      const tw = f.tip.offsetWidth;
      const left = (xs[best] / f.w) * r.width;
      f.tip.style.left = Math.max(0, Math.min(r.width - tw, left + 12)) + 'px';
    };
    hit.addEventListener('mousemove', show);
    hit.addEventListener('mouseleave', () => { f.tip.hidden = true; rule.setAttribute('visibility', 'hidden'); });
  }

  /** Cumulative net saving over time: one line, zero marked, the latest value written at its end. */
  function netSaving(host, buckets) {
    const bins = rebin(buckets, ['baseline_cost_usd', 'cost_usd', 'cg_llm_cost_usd'], []);
    let acc = 0;
    const vals = bins.map((b) => (acc += b.baseline_cost_usd - b.cost_usd - b.cg_llm_cost_usd));
    if (!bins.length || bins.every((b) => !b.baseline_cost_usd && !b.cost_usd)) {
      return emptyState(host, 'No priced requests yet', 'This needs provider usage data and a known model price.');
    }
    const pad = { l: 52, r: 76, t: 14, b: 24 };
    const f = frame(host, 220, 'Cumulative net saving over time, in dollars');
    const lo = Math.min(0, ...vals), hi = Math.max(0, ...vals) || 1;
    const x = (i) => pad.l + (bins.length === 1 ? 0 : (i / (bins.length - 1)) * (f.w - pad.l - pad.r));
    const y = (v) => pad.t + (1 - (v - lo) / (hi - lo)) * (f.h - pad.t - pad.b);
    for (const t of niceTicks(lo, hi)) {
      f.svg.appendChild(svgEl('line', { class: t === 0 ? 'plot-zero' : 'plot-grid', x1: pad.l, x2: f.w - pad.r, y1: y(t), y2: y(t) }));
      text(f.svg, { class: 'plot-axis', x: pad.l - 8, y: y(t) + 4, 'text-anchor': 'end' }, usd(t));
    }
    const color = acc >= 0 ? 'var(--good)' : 'var(--bad)';
    const pts = vals.map((v, i) => `${x(i)},${y(v)}`);
    const area = svgEl('path', { class: 'plot-area', d: `M${x(0)},${y(0)} L${pts.join(' L')} L${x(vals.length - 1)},${y(0)} Z`, fill: color });
    const line = svgEl('path', { class: 'plot-line', d: 'M' + pts.join(' L'), stroke: color, pathLength: 1 });
    f.svg.append(area, line);
    const last = vals.length - 1;
    f.svg.appendChild(svgEl('circle', { cx: x(last), cy: y(vals[last]), r: 3.5, fill: color }));
    text(f.svg, { class: 'plot-end', x: x(last) + 8, y: y(vals[last]) + 4, fill: color }, usd(vals[last]));
    const span = bins[last].ts - bins[0].ts;
    [0, Math.floor(last / 2), last].forEach((i, n) => text(f.svg, { class: 'plot-axis', x: x(i), y: f.h - 6, 'text-anchor': n === 0 ? 'start' : n === 2 ? 'end' : 'middle' }, axisLabel(bins[i].ts, span)));
    hover(f, vals.map((_, i) => x(i)), bins, (i) => [['Net saving so far', usd(vals[i]), color]], pad);
  }

  /** Columns of one measure, optionally with a darker part (`part`) at the bottom. */
  function columns(host, buckets, opts) {
    const bins = rebin(buckets, opts.sum, []);
    if (!bins.length || bins.every((b) => !b[opts.key])) return emptyState(host, opts.empty || 'Nothing in this window', '');
    const pad = { l: 52, r: 12, t: 14, b: 24 };
    const f = frame(host, opts.h || 180, opts.label);
    const hi = Math.max(...bins.map((b) => b[opts.key])) || 1;
    const bw = (f.w - pad.l - pad.r) / bins.length;
    const y = (v) => f.h - pad.b - (v / hi) * (f.h - pad.t - pad.b);
    for (const t of niceTicks(0, hi)) {
      f.svg.appendChild(svgEl('line', { class: 'plot-grid', x1: pad.l, x2: f.w - pad.r, y1: y(t), y2: y(t) }));
      text(f.svg, { class: 'plot-axis', x: pad.l - 8, y: y(t) + 4, 'text-anchor': 'end' }, opts.fmt(t));
    }
    bins.forEach((b, i) => {
      const x0 = pad.l + i * bw + Math.min(1.5, bw * 0.12);
      const wd = Math.max(1, bw - Math.min(3, bw * 0.24));
      const top = y(b[opts.key]);
      f.svg.appendChild(svgEl('rect', { class: 'plot-col', x: x0, y: top, width: wd, height: f.h - pad.b - top, fill: opts.color }));
      if (opts.part && b[opts.part]) {
        const pt = y(b[opts.part]);
        f.svg.appendChild(svgEl('rect', { class: 'plot-col plot-col-part', x: x0, y: pt, width: wd, height: f.h - pad.b - pt, fill: opts.partColor }));
      }
    });
    const span = bins[bins.length - 1].ts - bins[0].ts;
    [0, Math.floor((bins.length - 1) / 2), bins.length - 1].forEach((i, n) => text(f.svg, { class: 'plot-axis', x: pad.l + i * bw + bw / 2, y: f.h - 6, 'text-anchor': n === 0 ? 'start' : n === 2 ? 'end' : 'middle' }, axisLabel(bins[i].ts, span)));
    hover(f, bins.map((_, i) => pad.l + i * bw + bw / 2), bins, opts.rows(bins), pad);
  }

  /** Cache reads, writes and new input as a share of input tokens: 100% stacked columns. */
  function cacheShare(host, buckets) {
    const bins = rebin(buckets, ['cache_read', 'cache_write', 'fresh_input'], []);
    const tot = (b) => b.cache_read + b.cache_write + b.fresh_input;
    if (!bins.length || bins.every((b) => !tot(b))) return emptyState(host, 'No usage data in this window', 'Provider token counts are needed.');
    const pad = { l: 40, r: 96, t: 14, b: 24 };
    const f = frame(host, 200, 'Share of input tokens read from the cache, written to it, or sent fresh');
    const bw = (f.w - pad.l - pad.r) / bins.length;
    const y = (share) => pad.t + (1 - share) * (f.h - pad.t - pad.b);
    for (const t of [0, 0.5, 1]) {
      f.svg.appendChild(svgEl('line', { class: 'plot-grid', x1: pad.l, x2: f.w - pad.r, y1: y(t), y2: y(t) }));
      text(f.svg, { class: 'plot-axis', x: pad.l - 8, y: y(t) + 4, 'text-anchor': 'end' }, Math.round(t * 100) + '%');
    }
    const parts = [['cache_read', SERIES[0], 'Read from cache'], ['cache_write', SERIES[1], 'Written to cache'], ['fresh_input', SERIES[2], 'Sent fresh']];
    bins.forEach((b, i) => {
      let acc = 0;
      for (const [k, color] of parts) {
        const s = tot(b) ? b[k] / tot(b) : 0;
        f.svg.appendChild(svgEl('rect', { class: 'plot-col', x: pad.l + i * bw + 0.75, y: y(acc + s), width: Math.max(1, bw - 1.5), height: Math.max(0, y(acc) - y(acc + s)), fill: color }));
        acc += s;
      }
    });
    // Labels on the plot, level with where each part sits in the last column.
    const lastB = bins[bins.length - 1];
    let acc = 0;
    const spots = [];
    for (const [k, color, name] of parts) {
      const s = tot(lastB) ? lastB[k] / tot(lastB) : 0;
      spots.push([y(acc + s / 2) + 4, color, name]);
      acc += s;
    }
    // Labels sit level with their part, pushed apart so thin parts do not overprint each other.
    spots.sort((p, q) => p[0] - q[0]);
    for (let i = 1; i < spots.length; i++) spots[i][0] = Math.max(spots[i][0], spots[i - 1][0] + 15);
    for (const [yy, color, name] of spots) text(f.svg, { class: 'plot-end', x: f.w - pad.r + 8, y: yy, fill: color }, name);
    const span = lastB.ts - bins[0].ts;
    [0, Math.floor((bins.length - 1) / 2), bins.length - 1].forEach((i, n) => text(f.svg, { class: 'plot-axis', x: pad.l + i * bw + bw / 2, y: f.h - 6, 'text-anchor': n === 0 ? 'start' : n === 2 ? 'end' : 'middle' }, axisLabel(bins[i].ts, span)));
    hover(f, bins.map((_, i) => pad.l + i * bw + bw / 2), bins, (i) => parts.map(([k, color, name]) => [name, tot(bins[i]) ? (100 * bins[i][k] / tot(bins[i])).toFixed(1) + '%' : '—', color]), pad);
  }

  /** A single line on its own scale, with its range written beside it. Used for latency, where the two
   *  measures differ by a factor of a thousand and cannot share an axis. */
  function trend(host, buckets, opts) {
    const bins = rebin(buckets, [], [opts.key]);
    const vals = bins.map((b) => b[opts.key]);
    if (!bins.length || vals.every((v) => !v)) return emptyState(host, opts.empty || 'No latency recorded', '');
    const pad = { l: 56, r: 100, t: 10, b: 20 };
    const f = frame(host, 96, opts.label);
    const lo = 0, hi = Math.max(...vals) || 1;
    const x = (i) => pad.l + (bins.length === 1 ? 0 : (i / (bins.length - 1)) * (f.w - pad.l - pad.r));
    const y = (v) => pad.t + (1 - (v - lo) / (hi - lo)) * (f.h - pad.t - pad.b);
    f.svg.appendChild(svgEl('line', { class: 'plot-grid', x1: pad.l, x2: f.w - pad.r, y1: y(0), y2: y(0) }));
    f.svg.appendChild(svgEl('line', { class: 'plot-grid', x1: pad.l, x2: f.w - pad.r, y1: y(hi), y2: y(hi) }));
    text(f.svg, { class: 'plot-axis', x: pad.l - 8, y: y(hi) + 4, 'text-anchor': 'end' }, opts.fmt(hi));
    text(f.svg, { class: 'plot-axis', x: pad.l - 8, y: y(0) + 4, 'text-anchor': 'end' }, opts.fmt(0));
    f.svg.appendChild(svgEl('path', { class: 'plot-line', d: 'M' + vals.map((v, i) => `${x(i)},${y(v)}`).join(' L'), stroke: opts.color, pathLength: 1 }));
    const mean = bins.reduce((s, b) => s + b[opts.key] * b.requests, 0) / Math.max(1, bins.reduce((s, b) => s + b.requests, 0));
    text(f.svg, { class: 'plot-end', x: f.w - pad.r + 8, y: pad.t + 12, fill: opts.color }, 'average');
    text(f.svg, { class: 'plot-end plot-big', x: f.w - pad.r + 8, y: pad.t + 32, fill: opts.color }, opts.fmt(mean));
    hover(f, vals.map((_, i) => x(i)), bins, (i) => [[opts.name, opts.fmt(vals[i]), opts.color]], pad);
  }

  /** Money saved and money added, as bars either side of one axis, each with its figure. Saved goes right in
   *  green; cost added goes left in red. Rows whose figure is an upper bound are outlined, not filled. */
  function savingsBars(host, o) {
    clear(host);
    const w = Object.fromEntries((o.waterfall || []).map((s) => [s.key, s]));
    if (!w.baseline || !o.baseline_cost_usd) return emptyState(host, 'No priced requests yet', 'This needs provider usage data and a known model price.');
    // delta_usd is negative for a saving. flip so that saved is positive.
    const rows = [
      ['Your preset', -(w.compaction ? w.compaction.delta_usd : 0), false],
      ['Our own model calls', -(w.cg_llm ? w.cg_llm.delta_usd : 0), false],
      ['Cache strategy pings', -(w.keepalive_ping ? w.keepalive_ping.delta_usd : 0), false],
      ['Cache strategy: rewrites avoided', -(w.keepalive_saved ? w.keepalive_saved.delta_usd : 0), true],
      // Net of the pings' own cost, so a single bar. Absent on a server from before the Ping button.
      ['Manual pings', -(w.manual_pings ? w.manual_pings.delta_usd : 0), true],
      ['Tools and skills filter', -(w.decl_filter_saved ? w.decl_filter_saved.delta_usd : 0), false],
    ];
    const max = Math.max(...rows.map((r) => Math.abs(r[1])), 0.01);
    host.appendChild(el('p', { class: 'plot-lead' },
      'Without context-guru this window would have cost ', el('b', { text: usd(o.baseline_cost_usd) }),
      '. You were billed ', el('b', { text: usd(o.cost_usd + o.cg_llm_cost_usd) }), '.'));
    const grid = el('div', { class: 'diverge', role: 'list' });
    for (const [name, v, bound] of rows) {
      const side = v >= 0 ? 'saved' : 'cost';
      const bar = el('i', { class: 'dv-bar ' + side + (bound ? ' bound' : '') });
      bar.style.width = (Math.abs(v) / max) * 100 + '%';
      grid.appendChild(el('div', { class: 'dv-row', role: 'listitem' },
        el('span', { class: 'dv-name', text: name }),
        el('span', { class: 'dv-cost' }, side === 'cost' ? bar : null),
        el('span', { class: 'dv-saved' }, side === 'saved' ? bar : null),
        el('span', { class: 'dv-val ' + side, text: (v >= 0 ? '+' : '−') + usd(Math.abs(v)) + (bound ? ' at most' : '') })));
    }
    host.append(el('div', { class: 'dv-head', 'aria-hidden': 'true' }, el('span'), el('span', { text: 'cost added' }), el('span', { text: 'money saved' }), el('span')), grid);
  }

  return { netSaving, columns, cacheShare, trend, savingsBars };
})();
