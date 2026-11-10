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
		"subscriptionHeadline(state.sub.limits, o, state.sub.win, Date.now(), compact)",
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
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.js no longer contains %q", want)
		}
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
		"'data-testid': 'ka-split-tokens'",
		"tile('ka-net', 'Cache strategy net, measured', ran ? usd(o.net_usd)", // the dollar view stays for everyone else
		"o.tokens_unmeasured_requests > 0",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.js no longer contains %q", want)
		}
	}
}
