package proxy

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/tenant"
)

// The keep-alive's write routes at the HTTP level, plus Bug 2's stale-page refusal — which is
// here rather than in config because the stored document is what it compares against.

// setDoc stores a configuration document for one tenant, as a manager would.
func setDoc(t *testing.T, f *mgrFixture, id, doc string) {
	t.Helper()
	tn, err := f.reg.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Update(tn, id, tenant.Patch{ConfigYAML: &doc}); err != nil {
		t.Fatalf("storing the document: %v", err)
	}
}

// Bug 2's second rule. Preserving the unmodelled stops a save DROPPING a component the client
// could not see; it cannot stop one ADDING a component back, because from the server's side an
// addition is indistinguishable from a deliberate one. So the page states the pipeline it
// rendered from and a mismatch is a 409 — and the stored document must be byte-identical after.
func TestASaveFromAStalePageIsRefused(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, id := f.signUpJar(t, "boss@ibm.com")
	const stored = "pipeline:\n  - format\n  - linecap\n  - extract\nmode: sync\n"
	setDoc(t, f, id, stored)
	before, err := f.reg.Get(id)
	if err != nil {
		t.Fatal(err)
	}

	// A page that rendered from a DIFFERENT pipeline: this is the stale render that put `toon`
	// back and took `linecap` out.
	body := `{"config":{"pipeline":["format","toon"],` +
		`"pipeline_known":["format","toon","extract"],` +
		`"pipeline_base":["format","extract"],` +
		`"components":{},"cache":{"keepalive":false,"head_ttl_1h":false}}}`
	w, _ := f.do(t, http.MethodPut, "/api/me", body, mgrJar)
	if w.Code != http.StatusConflict {
		t.Fatalf("a save from a stale page = %d, want 409: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "reload") {
		t.Errorf("the refusal does not tell the reader what to do: %s", w.Body)
	}
	after, err := f.reg.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.ConfigYAML != before.ConfigYAML {
		t.Errorf("a refused save changed the stored document:\n%q\n->\n%q",
			before.ConfigYAML, after.ConfigYAML)
	}

	// The SAME body with a base that matches is accepted, and `extract` survives because the
	// client did not claim to have drawn it... it did claim it, so it is removed. `linecap` was
	// NOT claimed, so it survives at its own index. Both halves of the rule in one save.
	ok := `{"config":{"pipeline":["format","toon"],` +
		`"pipeline_known":["format","toon","extract"],` +
		`"pipeline_base":["format","linecap","extract"],` +
		`"components":{},"cache":{"keepalive":false,"head_ttl_1h":false}}}`
	w, _ = f.do(t, http.MethodPut, "/api/me", ok, mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("a save whose base matches = %d, want 200: %s", w.Code, w.Body)
	}
	saved, err := f.reg.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	got, err := config.ParseForm(saved.ConfigYAML)
	if err != nil {
		t.Fatal(err)
	}
	if !hasName(got.Pipeline, "linecap") {
		t.Errorf("linecap was dropped by a client that did not render it: %v\n%s",
			got.Pipeline, saved.ConfigYAML)
	}
	if hasName(got.Pipeline, "extract") {
		t.Errorf("a DECLARED removal did not remove extract: %v", got.Pipeline)
	}
	if !hasName(got.Pipeline, "toon") {
		t.Errorf("the save did not add toon: %v", got.Pipeline)
	}
	// A nil base is still accepted — an older cached bundle does not send one, and refusing
	// every save from one would break the settings page for as long as that bundle is cached.
	nobase := `{"config":{"pipeline":["format"],"pipeline_known":["format"],` +
		`"components":{},"cache":{"keepalive":false,"head_ttl_1h":false}}}`
	if w, _ := f.do(t, http.MethodPut, "/api/me", nobase, mgrJar); w.Code != http.StatusOK {
		t.Errorf("a save with no pipeline_base = %d, want 200 (old bundle): %s", w.Code, w.Body)
	}
}

// hasName reports membership in a component list. Named for what it holds rather than
// `contains`, which this package's limits_test already uses for a substring test.
func hasName(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// Arming answers with the two numbers the caller is authorizing: how long their credential may
// be held, and what this can cost at worst. Arming without showing that is the thing this whole
// feature is trying not to be.
func TestArmingStatesTheHoldAndTheWorstCaseSpend(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	until := time.Now().Add(2 * time.Hour).UnixMilli()
	body := fmt.Sprintf(`{"session":"s-1","idle_seconds":280,"max_pings":2,"until":%d}`, until)
	w, out := f.do(t, http.MethodPost, "/api/me/keepalive/sessions", body, mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("arm = %d: %s", w.Code, w.Body)
	}
	// (K+1) x X = 3 x 280 s = 14 minutes, which is what the Settings copy promises at the
	// shipped defaults — an override changes that number, so it must be stated.
	if got, _ := out["hold_minutes"].(float64); got != 14 {
		t.Errorf("hold_minutes = %v, want 14 ((K+1) x X)", out["hold_minutes"])
	}
	if _, ok := out["worst_case_pings"]; !ok {
		t.Error("the answer does not say how many pings this can send at worst")
	}
	if got, _ := out["durable"].(bool); got {
		t.Error("the answer claims the override is durable; it is lost on restart")
	}
	// An unpriced model must say so rather than produce a figure. This fixture has no price
	// list, so `priced` is the honest answer.
	if _, ok := out["priced"]; !ok {
		t.Error("the answer does not state whether it could be priced")
	}
	if got, _ := out["priced"].(bool); !got {
		if _, has := out["worst_case_usd"]; has {
			t.Error("priced=false but a dollar figure was returned anyway")
		}
	}
}

// The body's MaxUSDPerPing is ignored, and the answer reports the value actually in force. A
// silently dropped field is worse than a refused one.
func TestArmingIgnoresAPostedPerPingBudget(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	until := time.Now().Add(time.Hour).UnixMilli()
	body := fmt.Sprintf(
		`{"session":"s-1","idle_seconds":280,"max_pings":2,"until":%d,"max_usd_per_ping":99}`, until)
	w, out := f.do(t, http.MethodPost, "/api/me/keepalive/sessions", body, mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("arm = %d: %s", w.Code, w.Body)
	}
	if got, has := out["max_usd_per_ping"].(float64); has && got == 99 {
		t.Error("a posted per-ping budget of $99 was accepted; the guard is the account's own")
	}
}

// Every arm and every disarm leaves a DURABLE audit row with the resolved parameters. That row is
// the durable half of this feature: the live policy does not survive a restart, but who
// authorized what spend, with which parameters, until when, does.
func TestArmingAndDisarmingAreAudited(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, id := f.signUpJar(t, "boss@ibm.com")
	until := time.Now().Add(time.Hour)
	body := fmt.Sprintf(`{"session":"s-1","idle_seconds":280,"max_pings":2,"until":%d}`,
		until.UnixMilli())
	if w, _ := f.do(t, http.MethodPost, "/api/me/keepalive/sessions", body, mgrJar); w.Code != 200 {
		t.Fatalf("arm = %d: %s", w.Code, w.Body)
	}
	if w, _ := f.do(t, http.MethodDelete, "/api/me/keepalive/sessions/s-1", "", mgrJar); w.Code != 200 {
		t.Fatalf("disarm = %d: %s", w.Code, w.Body)
	}
	entries, err := f.reg.Audit(id, 100)
	if err != nil {
		t.Fatal(err)
	}
	var armed, disarmed bool
	for _, e := range entries {
		if e.Field != "keepalive_session" {
			continue
		}
		if strings.Contains(e.After, "armed") && strings.Contains(e.After, "idle=280s") &&
			strings.Contains(e.After, "pings=2") && strings.Contains(e.After, "hold<=14min") {
			armed = true
		}
		if e.After == "disarmed" {
			disarmed = true
		}
	}
	if !armed {
		t.Errorf("no audit row carries the arm and its resolved parameters: %+v", entries)
	}
	if !disarmed {
		t.Errorf("no audit row carries the disarm: %+v", entries)
	}
}

// The bounds are refusals with reasons at the HTTP layer too, never silent clamps: a clamp on a
// spend authorization tells the person they got what they asked for when they did not.
func TestArmingRefusesEveryBoundWithAReason(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	ok := time.Now().Add(time.Hour).UnixMilli()
	for name, body := range map[string]string{
		"no session":   fmt.Sprintf(`{"session":"","idle_seconds":280,"max_pings":2,"until":%d}`, ok),
		"idle too low": fmt.Sprintf(`{"session":"s","idle_seconds":10,"max_pings":2,"until":%d}`, ok),
		"idle past the TTL": fmt.Sprintf(
			`{"session":"s","idle_seconds":295,"max_pings":2,"until":%d}`, ok),
		"too many pings": fmt.Sprintf(`{"session":"s","idle_seconds":280,"max_pings":50,"until":%d}`, ok),
		"expiry in the past": fmt.Sprintf(`{"session":"s","idle_seconds":280,"max_pings":2,"until":%d}`,
			time.Now().Add(-time.Hour).UnixMilli()),
		"expiry beyond 12h": fmt.Sprintf(`{"session":"s","idle_seconds":280,"max_pings":2,"until":%d}`,
			time.Now().Add(20*time.Hour).UnixMilli()),
		"negative prefix floor": fmt.Sprintf(
			`{"session":"s","idle_seconds":280,"max_pings":2,"min_prefix_tokens":-1,"until":%d}`, ok),
		"hold beyond an hour": fmt.Sprintf(
			`{"session":"s","idle_seconds":290,"max_pings":11,"until":%d}`, ok),
	} {
		w, out := f.do(t, http.MethodPost, "/api/me/keepalive/sessions", body, mgrJar)
		if name == "hold beyond an hour" {
			// (11+1) x 290 = 58 min, inside the ceiling: this one is ACCEPTED, and it is here to
			// prove the ceiling is not simply refusing everything.
			if w.Code != http.StatusOK {
				t.Errorf("%s: 58 minutes of hold = %d, want 200: %s", name, w.Code, w.Body)
			}
			continue
		}
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400: %s", name, w.Code, w.Body)
			continue
		}
		if msg, _ := out["error"].(string); msg == "" || msg == "invalid" {
			t.Errorf("%s was refused without a reason: %s", name, w.Body)
		}
	}
}

// A plain account may NOT arm — v1 keeps the manager gate, because arming spends a credential —
// but may switch the whole thing off and may disarm. Consent withdrawal must never be harder
// than consent, and the account box is drawn disabled for a non-manager.
func TestConsentWithdrawalIsOpenToAnyPrincipal(t *testing.T) {
	f := newMgrFixture(t)
	_, _ = f.signUpJar(t, "boss@ibm.com")
	userJar, userID := f.signUpJar(t, "a@ibm.com")
	setDoc(t, f, userID, "pipeline:\n  - format\ncache:\n  keepalive: true\n")

	until := time.Now().Add(time.Hour).UnixMilli()
	body := fmt.Sprintf(`{"session":"s","idle_seconds":280,"max_pings":2,"until":%d}`, until)
	if w, _ := f.do(t, http.MethodPost, "/api/me/keepalive/sessions", body, userJar); w.Code != http.StatusForbidden {
		t.Errorf("a plain account armed a session: %d %s", w.Code, w.Body)
	}
	// Off, by the person whose key is being spent.
	w, _ := f.do(t, http.MethodDelete, "/api/me/keepalive", "", userJar)
	if w.Code != http.StatusOK {
		t.Fatalf("a plain account could not switch the keep-alive off: %d %s", w.Code, w.Body)
	}
	tn, err := f.reg.Get(userID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := config.ParseForm(tn.ConfigYAML)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cache == nil || got.Cache.KeepAlive {
		t.Errorf("the stored consent was not withdrawn:\n%s", tn.ConfigYAML)
	}
	// The pipeline is untouched: an off switch must not be a pipeline edit.
	if !hasName(got.Pipeline, "format") {
		t.Errorf("switching the keep-alive off changed the pipeline: %v", got.Pipeline)
	}
	// And it is audited as a consent change.
	entries, err := f.reg.Audit(userID, 100)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range entries {
		if e.Field == "keepalive" && e.After == "false" {
			found = true
		}
	}
	if !found {
		t.Errorf("switching the keep-alive off was not audited: %+v", entries)
	}
	// Disarming is open too.
	if w, _ := f.do(t, http.MethodDelete, "/api/me/keepalive/sessions/s", "", userJar); w.Code != http.StatusOK {
		t.Errorf("a plain account could not disarm: %d %s", w.Code, w.Body)
	}
}

// Arming is rate-limited, because arming is a spend authorization.
func TestArmingIsRateLimited(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	until := time.Now().Add(time.Hour).UnixMilli()
	var limited bool
	for i := 0; i <= overrideArmsPerHour; i++ {
		body := fmt.Sprintf(`{"session":"s-%d","idle_seconds":280,"max_pings":2,"until":%d}`, i, until)
		w, _ := f.do(t, http.MethodPost, "/api/me/keepalive/sessions", body, mgrJar)
		if w.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Errorf("more than %d arms an hour were accepted", overrideArmsPerHour)
	}
}
