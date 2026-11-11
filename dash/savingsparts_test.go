package dash

import (
	"os/exec"
	"strings"
	"testing"
)

// TestSavingsPartsAddUp runs savings.test.mjs: the Overview's parts must sum to total_saved_usd.
func TestSavingsPartsAddUp(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH, so savings.test.mjs did not run; run `node --test dash/savings.test.mjs`")
	}
	if out, err := exec.Command(node, "--test", "savings.test.mjs").CombinedOutput(); err != nil {
		t.Fatalf("node --test savings.test.mjs failed: %v\n%s", err, out)
	}
}

// The Keep-alive (Cache strategy) ledger is shared between the Overview and its tab for a few seconds, and
// that share is keyed by the whole query, so a change of tenant, window or filter cannot get the old ledger.
func TestSharedKeepAliveLedgerIsKeyedByTheQuery(t *testing.T) {
	src, err := uiFS.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, want := range []string{"const key = qs({});", "state.kaLedgerKey !== key", "state.kaLedgerKey = key;"} {
		if !strings.Contains(s, want) {
			t.Errorf("keepAliveLedger no longer contains %q; a change of scope could show the previous scope's ledger", want)
		}
	}
}

// On a Claude subscription the Overview shows every saving in tokens (/api/stats `tokens`, #998), and
// the 5-hour share as an estimate, from GET /api/me/limits (#985).
// The decision itself is tested in savings.test.mjs (subscriptionHeadline). This checks the wiring:
// the page asks for the limits, the headline uses the decision, and the dollar view stays for the
// rest (`{}`, a failed call, a window that is over).
func TestOverviewSubscriptionHeadlineIsWired(t *testing.T) {
	app := stripComments(readUI(t, "ui/app.js"))
	for _, want := range []string{
		"ctl('/api/me/limits')",
		"ctl('/api/stats?lean=1&since=' + fiveHourStartMs(f))",
		"subscriptionHeadline(state.sub.limits, o, state.sub.win, Date.now(), compact, pageFiltered(), state.sub.fit)", // a filter hides the estimate
		"ctl('/api/me/fivehour-weights?since=' + fiveHourStartMs(f))",                                                  // the learned weights (#1047)
		"'data-testid': 'saved-total-usd'",                                                                             // the dollar view is still there for everyone else
		"loadOverviewLimits();",
		"if (!readsLimits(account && account.tenant)) { done(null); return; }", // a manager keeps dollars
		"if (seq !== state.subSeq) return;",                                    // an older answer is dropped
		"if (isSubscription(limits, Date.now())) {",                            // the auth kind, not only a live window
		"const ft = sub ? featureTokens(o) : null;",                            // the cards read tokens.by_feature
		"saving('saved-compaction-usd', parts.compaction, ft && ft.compaction", // a card shows tokens on a subscription
		"o.tokens_unmeasured_requests",                                         // the "not measured" note
		"'data-testid': 'saved-total-pings'",                                   // the pings: a line of their own
		"'data-testid': 'saved-cache-pings'",                                   // on the Cache card too
		"tokenFigure(flow, o, compact).text",                                   // one rule for zero against not measured
		"subRows.length ? el('ul'",                                             // no empty list
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.js no longer contains %q", want)
		}
	}
}

// On a Claude subscription the Sessions tab shows tokens in its three dollar columns (#1023). The
// cells are tested in savings.test.mjs (sessionTokenCells). This checks the wiring: the tab asks for
// the limits, a manager keeps dollars, each dollar cell has its token form, and the headers change.
func TestSessionsSubscriptionTokensAreWired(t *testing.T) {
	app := stripComments(readUI(t, "ui/app.js"))
	for _, want := range []string{
		"if (!readsLimits(account && account.tenant)) return Promise.resolve(false);", // a manager keeps dollars
		"() => { if (state.limitsP === p) state.limitsP = null; });",                  // a failed answer is not kept
		"return meLimits(60000).then((l) => isSubscription(l, Date.now()), () => false);",
		"const limits = await meLimits(0);", // the Overview shares the request, and always asks fresh
		"sessionHeaders(sub);",
		"paint(!!state.sessSub);", // the table paints without waiting for the limits
		"subP.then((sub) => { if (seq === state.sessSeq && sub !== state.sessSub) paint(sub); });",
		"usdOrNA(0, false, 'no turn in this session reported the provider usage a cost needs')", // n/a, not 0
		"sub ? sessionTokenCost(s) :",
		"sub ? sessionTokenSaved(s) :",
		"sub ? sessionTokenKeepAlive(s) :",
		"'data-testid': 'session-pings-tok', text: PING_LABEL + ': ' + c.pings", // the pings: a line of their own
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.js no longer contains %q", want)
		}
	}
	html := readUI(t, "ui/index.html")
	// The Sessions table only: the Cache strategy tab has token labels of its own (#1048).
	start := strings.Index(html, `data-testid="sessions-table"`)
	if start < 0 {
		t.Fatal("index.html has no Sessions table")
	}
	html = html[start:]
	html = html[:strings.Index(html, "</table>")]
	if n := strings.Count(html, "data-sub-label="); n != 3 {
		t.Errorf("index.html has %d Sessions headers with a token label, want 3", n)
	}
}

// On a Claude subscription the Cache strategy tab shows the measured figures in tokens (#1022). The
// lines themselves are tested in savings.test.mjs (kaTokenLines). This checks the wiring: the tab
// asks for the limits, a manager keeps dollars, the token tiles replace the dollar ones, and the
// forecast keeps dollars with its API-price label.
func TestKeepAliveTabSubscriptionTokensAreWired(t *testing.T) {
	app := stripComments(readUI(t, "ui/app.js"))
	for _, want := range []string{
		"if (!readsLimits(account && account.tenant)) return false;", // a manager keeps dollars
		"try { return isSubscription(await ctl('/api/me/limits'), Date.now()); } catch (e) { return false; }",
		"const [l, sub] = await Promise.all([ledger, subP]);",
		"const tk = kaState.sub ? kaTokenLines(o, compact) : null;",
		"tk ? tile('ka-net-tok'",
		"'data-testid': 'ka-measured-tokens'",
		"'data-testid': 'ka-forecast-weights'",
		"const fv = forecastView(recIn, kaState.sub), rec = fv.rec;", // the learned forecast on a subscription (#1047)
		"const fv = forecastView(raw, sub), rec = fv.rec;",           // the detail panel shows the same forecast (#1047)
		"'data-testid': 'ka-rec-weights' }, fv.label",                // with the same weight label
		"loadKARecommend(subP);",
		"'data-testid': 'ka-split-tokens'",
		"tile('ka-net', 'Cache strategy net, measured', ran ? usd(o.net_usd)", // the dollar view stays for everyone else
		"o.tokens_unmeasured_requests > 0",
		"el('strong', {}, tk.measuredText)", // an absence never reads as a figure (#1042)
		"tk ? tk.avoidedText : 'up to ' + usd(o.saved_usd)",
		"'. ' + tk.rescuesText",
		"tk && o.pings ? 'Ahead or behind at API-price weights · ' : ''", // no prefix beside "No pings yet"
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.js no longer contains %q", want)
		}
	}
}

// On a Claude subscription the Cache strategy tab's tables show tokens (#1048). The cells are tested
// in savings.test.mjs (kaSessionTokenCells, bandTokens, tierSeries). This checks the wiring: each
// dollar cell has its token form, the pings keep a line or a column of their own, the price
// what-ifs keep dollars with their API-price label, and the headers change.
func TestKeepAliveTablesSubscriptionTokensAreWired(t *testing.T) {
	app := stripComments(readUI(t, "ui/app.js"))
	for _, want := range []string{
		"subLabels($('#ka-downside-panel'), sub);",
		"subLabels(panel, kaState.sub);",                           // the downside headers follow the units before the sessions land
		"So far in this window${tk.measured ? ' (measured)' : ''}", // "(measured)" only on a measured rescue
		"subLabels($('#ka-live-panel'), sub);",
		"subLabels($('#ka-behaviour-panel'), sub);",
		"const c = sub ? kaSessionTokenCells(s, compact) : null;",
		"c ? el('td', { class: 'num', 'data-testid': 'ka-expiry-tok' }, c.expiry || '—')",
		"c ? el('td', { class: 'num', 'data-testid': 'ka-pings-tok' }, c.pings || '—')", // the pings: a column of their own
		"c ? el('td', { class: 'num', 'data-testid': 'ka-rescue-tok' }, c.ran ? c.rescue : '—')",
		"'data-testid': 'ka-net-weights'",                // no net in tokens: ahead or behind at API-price weights
		"el('td', { class: 'num' }, usd(s.expiry_usd)),", // the dollar view stays for everyone else
		"sub ? kaLiveTokens(r) :",
		"'data-testid': 'ka-live-pings-tok', text: PING_LABEL + ': ' + c.pings", // the pings: a line of their own
		"${sub ? ' at API-price weights' : ''}",
		"'cost a lapse would charge, at API-price weights.",
		"tierSeries(b.daily).map(",
		"const cost = (g) => (sub ? bandTokens(g, compact) : usd(g.usd));",
		"kaTable('#ka-gap-bands', 'Idle gap', b.gap_bands, sub);",
		"'data-testid': 'ka-calc-weights'", // the ladder is a price what-if: dollars, labelled
		"(sub ? 'Net of the names list.' : 'Net of the names list and the searches.')",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.js no longer contains %q", want)
		}
	}
	html := readUI(t, "ui/index.html")
	for id, n := range map[string]int{"ka-downside-panel": 5, "ka-live-panel": 4, "ka-behaviour-panel": 2} {
		start := strings.Index(html, `id="`+id+`"`)
		if start < 0 {
			t.Fatalf("index.html has no #%s", id)
		}
		// The panel runs to the next panel's heading: a panel holds one h2, and nested panels hold h3s.
		h2 := strings.Index(html[start:], "<h2")
		end := strings.Index(html[start+h2+1:], "<h2")
		if h2 < 0 || end < 0 {
			t.Fatalf("#%s: no heading after it", id)
		}
		if got := strings.Count(html[start:start+h2+1+end], "data-sub-label="); got != n {
			t.Errorf("#%s has %d token labels, want %d", id, got, n)
		}
	}
}
