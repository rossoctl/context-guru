// context-guru dashboard: the Feedback tab.
// Moved out of app.js (#1089). app.js loads first and holds the shared DOM helpers,
// formatting, state and router that this file uses. See the file list in index.html.
'use strict';

// ── feedback ───────────────────────────────────────────────────────────────
//
// One view, two audiences. Everybody gets the form; the manager additionally gets the
// aggregate and every answer, which are the three [data-manager] cards in index.html —
// so the entitlement is declared in the markup and applied by applyAccount(), exactly
// like the Tenants tab, rather than by a second check here that could disagree with it.
//
// The server is the authority on all of it: it re-checks the 50-character rule, it
// refuses a rating it did not ask for, and it answers 403 to a plain account that asks
// to read. What this file does is make the rules visible before the round trip.

/** STAR_WORDS name each step, so a rating is never just a count of shapes. */
const STAR_WORDS = ['bad', 'poor', 'okay', 'good', 'excellent'];

/**
 * The questions and the agent selector come from the SERVER, keys AND wording:
 * tenant.FeedbackQuestions and tenant.FeedbackAgents. A key invented here would be
 * refused with a 422, and wording invented here would label a row with a different
 * question from the one the manager's email reports — so neither is written down twice.
 *
 * Filled from /api/me for the form, and from /api/feedback for the manager's view, which
 * is also what lets that view label a key it is only reading.
 */
const feedbackForm = { questions: [], agents: [] };

const labelOf = (list, key) => (list.find((x) => x.key === key) || {}).label || key;
/** dimLabel prints a question key the way the form asked it. */
const dimLabel = (key) => labelOf(feedbackForm.questions, key);
const agentLabel = (key) => (key ? labelOf(feedbackForm.agents, key) : 'not stated');

/**
 * meaningfulLen counts the characters a reader would see, collapsing every run of
 * whitespace to one — the same rule tenant.meaningfulLen applies server-side.
 *
 * Both halves count it the same way on purpose: a counter that says 62 next to a server
 * that says "not 50 yet" is a form the user cannot satisfy, and 50 spaces must not be a
 * way past a mandatory field.
 */
function meaningfulLen(s) { return (s || '').trim().split(/\s+/).filter(Boolean).join(' ').length; }

/** starSVG draws one star. currentColor, so CSS decides filled or empty. */
function starSVG() {
  const svg = svgEl('svg', { viewBox: '0 0 20 20', 'aria-hidden': 'true', class: 'star-icon' });
  svg.appendChild(svgEl('path', {
    d: 'M10 1.6l2.6 5.3 5.8.8-4.2 4.1 1 5.8-5.2-2.8-5.2 2.8 1-5.8L1.6 7.7l5.8-.8z',
    fill: 'currentColor',
  }));
  return svg;
}

/**
 * starField builds one question: a fieldset, five radios, five star labels.
 *
 * Native radios, deliberately. They come with keyboard support (arrows move within the
 * group, Tab leaves it), a name/value pair the browser submits, screen-reader semantics
 * and a focus ring — all of which a div-with-click-handlers has to reimplement and
 * usually gets wrong. The stars are the LABELS; the .on class is paint over the radio's
 * state, never the state itself.
 */
function starField(key, question, help) {
  const name = 'fb-' + key;
  const read = el('span', { class: 'stars-read', 'aria-live': 'polite' });
  const row = el('div', { class: 'stars', role: 'radiogroup', 'aria-label': question });
  const paint = () => {
    const chosen = row.querySelector('input:checked');
    const v = chosen ? Number(chosen.value) : 0;
    $$('.star', row).forEach((s, i) => s.classList.toggle('on', i < v));
    read.textContent = v ? `${v} of 5 — ${STAR_WORDS[v - 1]}` : '';
  };
  for (let v = 1; v <= 5; v++) {
    const id = name + '-' + v;
    row.appendChild(el('input', {
      type: 'radio', name, id, value: String(v), class: 'star-input',
      'data-testid': 'star-' + key + '-' + v, onchange: paint,
    }));
    // The label's accessible name is the WORD, not the position: "4 — good" is a rating,
    // "star 4" is a coordinate.
    row.appendChild(el('label', { for: id, class: 'star', title: `${v} — ${STAR_WORDS[v - 1]}` },
      el('span', { class: 'vh', text: `${v} of 5 — ${STAR_WORDS[v - 1]}` }), starSVG()));
  }
  return el('fieldset', { class: 'stars-field', 'data-dim': key },
    el('legend', {}, question),
    help ? el('p', { class: 'hint', text: help }) : null,
    el('div', { class: 'stars-line' }, row, read),
    el('p', { class: 'field-error', role: 'alert', hidden: true, 'data-testid': 'err-' + key }));
}

/** Show or clear the error under one field. role=alert, so it is announced. */
function fieldError(node, msg) {
  const p = node.querySelector('.field-error');
  if (!p) return;
  p.textContent = msg || '';
  p.hidden = !msg;
}

/**
 * loadFeedback draws the form, and — for a manager — the aggregate below it.
 *
 * The questions and the two agents come from /api/me, so this file never guesses at a key
 * the server validates or at wording the server's email prints.
 */
async function loadFeedback() {
  const form = $('#feedback-form');
  if (!form.dataset.built) {
    try {
      const me = await ctl('/api/me');
      feedbackForm.questions = me.feedback_questions || [];
      feedbackForm.agents = me.feedback_agents || [];
    } catch (e) {
      // No questions means no form: drawing an empty one would collect nothing the server
      // would accept. The loader must not reject, so this is reported in place.
      errorState(clear(form), 'Could not load the feedback form', e);
      return;
    }
    buildFeedbackForm(form);
    form.dataset.built = '1';
  }
  if (isManager()) loadFeedbackAdmin();
}

function buildFeedbackForm(form) {
  clear(form);

  // Which agent first: the same seven questions follow either way, and the answer is
  // stored so the manager can read Claude Code and Bob apart. A native select, so the
  // keyboard, the screen reader and the mobile picker all come for free.
  const agent = el('select', { id: 'fb-agent', 'data-testid': 'fb-agent' },
    el('option', { value: '' }, 'Choose one…'),
    ...feedbackForm.agents.map((a) => el('option', { value: a.key }, a.label)));
  const agentField = el('div', { class: 'field' },
    el('label', { for: 'fb-agent' }, 'Which agent is this about? (required)'),
    agent,
    el('p', { class: 'field-error', role: 'alert', hidden: true, 'data-testid': 'err-agent' }));
  form.appendChild(agentField);

  for (const q of feedbackForm.questions) form.appendChild(starField(q.key, q.label));

  const comment = el('textarea', {
    id: 'fb-comment', rows: '6', maxlength: '4000', required: 'required',
    'aria-describedby': 'fb-comment-count', 'data-testid': 'fb-comment',
    placeholder: 'How it feels, what to add or improve, any bugs.',
  });
  // ONE element carries both the live count and the validation message for this field.
  //
  // It is deliberately not a separate error paragraph that appears, and this is a bug
  // fixed rather than a preference: an element that materialises on blur moves the submit
  // button DOWN between mousedown and mouseup, so the browser sees no click on it and the
  // first press of "Send feedback" does nothing at all. Found in the browser, not in a
  // test — the count line is always present, so nothing below it can move.
  //
  // aria-live so the count and the message are both announced as they change, and
  // aria-describedby on the textarea so a screen reader reads the requirement on focus.
  const count = el('p', {
    class: 'hint', id: 'fb-comment-count', 'data-testid': 'fb-count', 'aria-live': 'polite',
  });
  const commentField = el('div', { class: 'field' },
    el('label', { for: 'fb-comment' }, 'General feeling, things to add or improve, bugs (required)'),
    el('p', { class: 'hint' }, 'At least 50 characters of real text; whitespace does not count.'),
    comment, count);
  form.appendChild(commentField);

  // tally is the field's whole validation surface: it runs on every keystroke, so the
  // requirement is never a surprise at submit time. `demand` is the submit-time voice.
  const tally = (demand) => {
    const n = meaningfulLen(comment.value);
    count.textContent = n >= 50 ? `${n} characters — long enough.`
      : demand ? `Please write at least 50 characters of real text; this is ${n}.`
        : `${n} of 50 characters.`;
    count.classList.toggle('short', n < 50);
    return n;
  };
  comment.addEventListener('input', () => tally(false));
  tally(false);

  const status = el('p', { class: 'field-error', role: 'alert', hidden: true, 'data-testid': 'fb-error' });
  const submit = el('button', { type: 'submit', class: 'primary', 'data-testid': 'fb-submit' },
    'Send feedback');
  form.appendChild(el('div', { class: 'actions' }, submit, status));

  // Assigned rather than added: "Send more feedback" rebuilds this form on the same
  // element, and a second addEventListener there would post the next submission twice.
  form.onsubmit = async (ev) => {
    ev.preventDefault();
    status.hidden = true;
    const scores = {};
    let firstBad = null;
    if (!agent.value) {
      fieldError(agentField, 'Please say which agent this is about.');
      firstBad = agentField;
    } else {
      fieldError(agentField, '');
    }
    for (const fs of $$('.stars-field', form)) {
      const chosen = fs.querySelector('input:checked');
      if (!chosen) {
        fieldError(fs, 'Please give this a rating.');
        if (!firstBad) firstBad = fs;
        continue;
      }
      fieldError(fs, '');
      scores[fs.dataset.dim] = Number(chosen.value);
    }
    if (tally(true) < 50 && !firstBad) firstBad = commentField;
    if (firstBad) {
      // Move to the first problem rather than reporting all of them at the bottom.
      const focusable = firstBad.querySelector('input,textarea,select');
      if (focusable) focusable.focus();
      firstBad.scrollIntoView({ block: 'center', behavior: 'smooth' });
      return;
    }

    submit.disabled = true;
    submit.textContent = 'Sending…';
    try {
      await ctl('/api/feedback', {
        method: 'POST',
        body: JSON.stringify({ agent: agent.value, scores, comment: comment.value }),
      });
      feedbackThanks(form);
      if (isManager()) loadFeedbackAdmin();
    } catch (e) {
      // The server's message names the rule it enforced; passing it through beats a
      // generic "something went wrong" that says less than the thing it replaced.
      status.textContent = e.message;
      status.hidden = false;
      submit.disabled = false;
      submit.textContent = 'Send feedback';
    }
  };
}

/** After a successful submit the form is replaced by what happened, and one way back. */
function feedbackThanks(form) {
  clear(form);
  form.dataset.built = '';
  form.appendChild(el('div', { class: 'banner ok', 'data-testid': 'fb-thanks', role: 'status' },
    'Thank you — that is stored here and on its way to the manager by email.'));
  form.appendChild(el('div', { class: 'actions' },
    el('button', {
      type: 'button', class: 'ghost', 'data-testid': 'fb-again',
      onclick: () => loadFeedback(),
    }, 'Send more feedback')));
}

// ── the manager's aggregate ────────────────────────────────────────────────

/**
 * distBars is a five-bucket histogram of one question's answers, one hue.
 *
 * One measure (how many people) across five ordered buckets is ONE colour: five colours
 * would imply five different things are being plotted. aria-hidden because the same
 * numbers are in the five columns beside it — the picture is for scanning, the columns
 * are the accessible reading.
 */
function distBars(dist) {
  const max = Math.max(...dist, 1);
  return el('div', { class: 'dist', 'aria-hidden': 'true' },
    ...dist.map((n, i) => el('span', {
      class: 'dist-col', title: `${i + 1}★: ${n}`,
    }, el('i', { style: 'height:' + Math.max(2, Math.round((n / max) * 100)) + '%' }))));
}

/** meanBar is a 0–5 track with the value beside it, so the number is never colour-only. */
function meanBar(mean) {
  return el('div', { class: 'mean-cell' },
    el('div', { class: 'bar-track' },
      el('div', { class: 'bar-fill', style: 'width:' + (mean / 5) * 100 + '%' })),
    el('span', { class: 'mean-val', text: mean.toFixed(2) }));
}

/**
 * npsBar renders the recommend question as its three states.
 *
 * Status colours, not series colours: promoter / passive / detractor is a judgement
 * about a state, which is exactly what --good / --warn / --bad are reserved for. Each
 * segment is also labelled in words below, because a status must never be colour alone.
 */
function npsBar(host, nps) {
  clear(host);
  // nps.n, not nps.N: the wire is JSON, and Go's tag lowercases it. This read the
  // uppercase field at first, so the panel said "nobody has answered" beside a table
  // reporting eight answers.
  if (!nps || !nps.n) {
    emptyState(host, 'Nobody has answered the recommend question yet', '');
    return;
  }
  const parts = [
    ['promoters', 'Promoters (5★)', nps.promoters, 'good'],
    ['passives', 'Passives (4★)', nps.passives, 'warn'],
    ['detractors', 'Detractors (1–3★)', nps.detractors, 'bad'],
  ];
  host.appendChild(el('div', { class: 'nps-head' },
    el('span', { class: 'nps-score', text: (nps.score > 0 ? '+' : '') + nps.score.toFixed(0) }),
    el('span', { class: 'muted small', text: `NPS from ${nps.n} answer${nps.n === 1 ? '' : 's'}` })));
  host.appendChild(el('div', { class: 'nps-bar' }, ...parts
    .filter(([, , n]) => n > 0)
    .map(([key, , n, cls]) => el('div', {
      class: 'nps-seg ' + cls, style: 'flex:' + n, 'data-testid': 'nps-' + key,
    }))));
  host.appendChild(el('div', { class: 'nps-legend' }, ...parts.map(([, label, n, cls]) =>
    el('span', {}, el('i', { class: 'sw ' + cls }), `${label}: ${n}`))));
}

/**
 * starText renders a stored rating as filled and empty stars plus its number.
 *
 * The empty half is U+2606 WHITE STAR (an outline), not a recoloured U+2605. Both halves were
 * the same filled glyph separated only by colour, which fails WCAG 1.4.1 / DESIGN §2.5 for a
 * colour-blind reader even after the unfilled colour was fixed — and at the old 1.32:1 it was
 * invisible outright, so the SCALE of the rating was unreadable.
 */
function starText(v) {
  return el('span', { class: 'star-read', title: `${v} of 5` },
    el('span', { class: 'on', text: '★'.repeat(v) }),
    el('span', { class: 'off', text: '☆'.repeat(5 - v) }),
    el('span', { class: 'vh', text: ` ${v} of 5` }));
}

/**
 * loadFeedbackAdmin fills the manager's three areas.
 *
 * Carries the tenant filter when one is set, so drilling in from the Tenants roster
 * narrows this view too. The server answers 403 to anyone who is not a manager whatever
 * this sends, so the parameter is a filter and never a permission.
 */
async function loadFeedbackAdmin() {
  const body = $('#feedback-dims-body');
  const answers = $('#feedback-answers');
  loadingRows(body, 9);
  loadingState(answers);
  try {
    const q = state.filter.tenant ? '?tenant=' + encodeURIComponent(state.filter.tenant) : '';
    const out = await ctl('/api/feedback' + q);
    // The wording for a stored key comes with the data, so this view labels a question the
    // way it was asked even before anybody has opened the form.
    feedbackForm.questions = out.questions || feedbackForm.questions;
    feedbackForm.agents = out.agents || feedbackForm.agents;
    const sum = out.summary || {};
    renderFeedbackTiles(sum);
    renderFeedbackAgents(sum);
    $('#feedback-count').textContent = `${sum.n || 0} submission${sum.n === 1 ? '' : 's'}` +
      (state.filter.tenant ? ' from the selected account' : '');

    clear(body);
    const dims = sum.dimensions || [];
    if (!dims.length) {
      tableMessage(body, 9, 'No feedback yet',
        'The form above is what fills this in. Nothing is seeded.');
    }
    for (const d of dims) {
      body.appendChild(el('tr', { 'data-testid': 'dim-' + d.dimension },
        el('td', {}, dimLabel(d.dimension)),
        el('td', {}, meanBar(d.mean)),
        el('td', {}, distBars(d.dist)),
        ...d.dist.map((n) => el('td', { class: 'num', text: String(n) })),
        el('td', { class: 'num', text: String(d.n) })));
    }

    npsBar($('#feedback-nps'), sum.nps);
    lineChart($('#chart-feedback'), [{
      name: 'Mean overall', color: SERIES[0], area: true,
      points: (sum.trend || []).map((p) => [p.day, p.mean]),
    }], { yFmt: (v) => v.toFixed(1), yMax: 5, label: 'mean overall stars per day' });

    renderFeedbackAnswers(answers, out.submissions || []);
  } catch (e) {
    clear(body);
    tableMessage(body, 9, 'Could not read the feedback', String(e.message || e), { error: true });
    errorState(answers, 'Could not read the feedback', e);
  }
}

function renderFeedbackTiles(sum) {
  const host = clear($('#feedback-tiles'));
  const overall = (sum.dimensions || []).find((d) => d.dimension === 'overall');
  const nps = sum.nps || {};
  host.appendChild(tileGroup(null, null, [
    tile('fb-count', 'Submissions', num(sum.n || 0), 'stars plus written answers'),
    tile('fb-overall', 'Mean overall', overall ? overall.mean.toFixed(2) + ' / 5' : '—',
      overall ? `${overall.n} answered` : 'nobody has answered yet',
      overall ? (overall.mean >= 4 ? 'good' : overall.mean < 3 ? 'bad' : '') : ''),
    tile('fb-nps', 'NPS', nps.n ? (nps.score > 0 ? '+' : '') + nps.score.toFixed(0) : '—',
      nps.n ? `${nps.promoters} promoters, ${nps.detractors} detractors` : 'no answers yet',
      nps.n ? (nps.score > 0 ? 'good' : nps.score < 0 ? 'bad' : '') : ''),
    // A relay that stopped working is otherwise only visible in the server log.
    tile('fb-unmailed', 'Not emailed', num(sum.unmailed || 0),
      sum.unmailed ? 'stored here, never left the relay' : 'every copy was delivered',
      sum.unmailed ? 'bad' : ''),
  ], 'headline'));
}

/**
 * renderFeedbackAgents is the reason the form asks which agent it is about: the same
 * headline numbers, read per agent, so "compaction is fine" and "compaction is not fine"
 * do not average each other away.
 *
 * The declared agents first, then anything else stored (rows from before the selector
 * existed carry no agent at all), so the order does not move as the numbers do.
 */
function renderFeedbackAgents(sum) {
  const body = clear($('#feedback-agents-body'));
  const by = sum.by_agent || {};
  const keys = feedbackForm.agents.map((a) => a.key).filter((k) => by[k])
    .concat(Object.keys(by).filter((k) => !feedbackForm.agents.some((a) => a.key === k)).sort());
  if (!keys.length) {
    tableMessage(body, 4, 'No feedback yet', 'The form above is what fills this in.');
    return;
  }
  for (const k of keys) {
    const s = by[k];
    const overall = (s.dimensions || []).find((d) => d.dimension === 'overall');
    const nps = s.nps || {};
    body.appendChild(el('tr', { 'data-testid': 'agent-' + (k || 'none') },
      el('td', {}, agentLabel(k)),
      el('td', { class: 'num', text: String(s.n || 0) }),
      el('td', {}, overall ? meanBar(overall.mean) : el('span', { class: 'muted', text: '—' })),
      el('td', {
        class: 'num',
        text: nps.n ? (nps.score > 0 ? '+' : '') + nps.score.toFixed(0) : '—',
      })));
  }
}

/**
 * renderFeedbackAnswers lists every submission verbatim.
 *
 * Every string here was typed by a user, so every one of them lands through el() and
 * textContent. Nothing on this page concatenates markup — el() throws on raw html, which
 * is what makes that a property of the page rather than a habit.
 */
function renderFeedbackAnswers(host, rows) {
  clear(host);
  if (!rows.length) {
    emptyState(host, 'No written feedback yet',
      'The form above is the only thing that writes here.');
    return;
  }
  for (const fb of rows) {
    const scores = fb.scores || {};
    // Asked-order, not alphabetical: the chips read like the form somebody filled in.
    const chips = feedbackForm.questions.filter((q) => scores[q.key])
      .map((q) => el('span', { class: 'score-chip' },
        el('span', { class: 'score-dim', text: q.label }), starText(scores[q.key])));
    host.appendChild(el('article', { class: 'answer', 'data-testid': 'answer-' + fb.id },
      el('header', { class: 'answer-head' },
        el('strong', { text: fb.email || 'unknown account' }),
        fb.label ? el('span', { class: 'muted small', text: fb.label }) : null,
        el('span', { class: 'pill', 'data-testid': 'answer-agent-' + fb.id },
          agentLabel(fb.agent)),
        el('span', { class: 'muted small', text: when(fb.created_at) }),
        fb.mailed_at
          ? el('span', { class: 'pill complete' }, 'emailed')
          : el('span', { class: 'pill missing' }, 'not emailed')),
      el('div', { class: 'score-chips' }, ...chips),
      el('div', { class: 'answer-block' },
        el('h4', {}, 'Comment'), el('p', { text: fb.comment }))));
  }
}

// Registered here rather than in the shared Object.assign above, so this whole feature
// is one appended block and the view table above stays untouched.
Object.assign(loaders, { feedback: loadFeedback });
