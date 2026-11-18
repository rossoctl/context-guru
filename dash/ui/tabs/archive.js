// context-guru dashboard: the Archive tab.
// Moved out of app.js (#1089). app.js loads first and holds the shared DOM helpers,
// formatting, state and router that this file uses. See the file list in index.html.
'use strict';

// ── archive ────────────────────────────────────────────────────────────────
async function loadArchive() {
  const host = clear($('#archive-list'));
  loadingState(host);
  try {
    const out = await ctl('/api/archive');
    const rows = out.archived || [];
    // Rows carry the remote they were written to. Saying "not configured" while listing
    // archived sessions is a contradiction the reader has to resolve; naming the remote
    // the objects are actually in, and that it is not set up here, is the true version.
    const rowRemote = rows.length ? rows[0].remote : '';
    $('#archive-remote').textContent = out.remote ? 'stored in ' + out.remote
      : rowRemote ? 'stored in ' + rowRemote + ' — not configured on this deployment now'
        : 'archive storage is not configured on this deployment';
    clear(host);
    if (!rows.length) {
      emptyState(host, 'Nothing archived yet',
        out.remote
          ? 'Sessions move here once they have been idle long enough.'
          : 'Without cold storage, old sessions are deleted rather than archived.');
      return;
    }
    const tbl = el('table', { class: 'grid' },
      el('thead', {}, el('tr', {},
        el('th', {}, 'Session'), el('th', {}, 'Requests'), el('th', {}, 'Active between'),
        el('th', {}, 'Archived on'), el('th', {}, 'Size'), el('th', {}, 'What was moved'), el('th', {}, el('span', { class: 'vh' }, 'Row actions')))));
    const body = el('tbody');
    for (const a of rows) {
      const kind = a.full_path ? 'whole session' : 'transcripts only';
      body.appendChild(el('tr', {},
        el('td', {}, el('code', { class: 'clip' }, a.session_id)),
        el('td', {}, num(a.requests)),
        el('td', {}, `${when(a.first_ts)} → ${when(a.last_ts)}`),
        el('td', {}, when(a.archived_at)),
        el('td', {}, compact((a.full_bytes || 0) + (a.content_bytes || 0)) + 'B'),
        el('td', {}, kind),
        el('td', {}, el('button', {
          class: 'ghost small', 'data-testid': 'open-archive',
          // Opens the drawer on LOCAL data only: the metrics, plus the explicit
          // "fetch transcript" affordance. Clicking a row must never be the thing
          // that starts an rclone round trip.
          onclick: () => openSessionDiff(a.session_id),
        }, 'Open'))));
    }
    tbl.appendChild(body);
    host.appendChild(el('div', { class: 'tblwrap', tabindex: '0' }, tbl));
  } catch (e) {
    clear(host);
    errorState(host, 'Could not list the archive', e);
  }
}

// Opening an archived session goes through openSessionDiff, the same drawer the
// Sessions view uses. There used to be a second reader here that fetched immediately
// and reported both failure modes through alert(); one renderer means the cold /
// unreachable / never-archived states are rendered identically wherever they are
// reached, and a modal alert is not a state a user can read a session id out of.

Object.assign(loaders, { archive: loadArchive });
