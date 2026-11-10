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
		"subscriptionHeadline(state.sub.limits, o, state.sub.win, Date.now(), compact, pageFiltered())", // a filter hides the estimate
		"'data-testid': 'saved-total-usd'", // the dollar view is still there for everyone else
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
	if n := strings.Count(html, "data-sub-label="); n != 3 {
		t.Errorf("index.html has %d Sessions headers with a token label, want 3", n)
	}
}
