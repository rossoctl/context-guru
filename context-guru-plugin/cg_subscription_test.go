// On a Claude subscription, `home` and `insights savings` show everything in tokens, by tier (`tokens` in /api/stats,
// #998): this session, the current 5-hour window with the 5-hour share as an estimate, then the period, and in
// `insights savings` each feature. No dollars (#986). Anything else is today's dollar view.
package plugin

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
)

// fixedNow is 1791473400 s. The window resets 2 hours later, so it started 3 hours before.
const (
	subReset       = "1791480600"
	subWindowStart = "1791462600000"
	subLimits      = `{"subscription":true,"at_ms":1791473000000,` +
		`"five_hour":{"utilization":0.54,"reset_s":` + subReset + `,"status":"allowed"},` +
		`"seven_day":{"utilization":0.08,"reset_s":1791900000,"status":"allowed"}}`
)

// cgSubscriptionStub is the warm cg stub plus GET /api/me/limits (an answer, or a status code when limits is
// "404" or "500") and the lean /api/stats reads of the session and of the 5-hour window. window is that read's body.
func cgSubscriptionStub(t *testing.T, limits, window string) string {
	t.Helper()
	base, _ := cgSubscriptionStubCounted(t, limits, window)
	return base
}

// cgSubscriptionStubCounted is cgSubscriptionStub, and also counts the GET /api/me/limits reads.
func cgSubscriptionStubCounted(t *testing.T, limits, window string) (string, *atomic.Int32) {
	t.Helper()
	var reads atomic.Int32
	up, err := url.Parse(cgProxyStub(t, "cachestate_warm"))
	if err != nil {
		t.Fatal(err)
	}
	rest := httputil.NewSingleHostReverseProxy(up)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/me/limits", func(w http.ResponseWriter, _ *http.Request) {
		reads.Add(1)
		switch limits {
		case "404":
			http.NotFound(w, nil)
		case "500":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.Write([]byte(limits))
		}
	})
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case q.Get("lean") == "1" && q.Get("session") == "session-A":
			w.Write([]byte(`{"tokens":{"saved":{"input":0,"cache_read":610000,"cache_write_5m":42000,"cache_write_1h":0},` +
				`"caused":{"input":0,"cache_read":0,"cache_write_5m":0,"cache_write_1h":0}},"cost_usd":3.1,"total_saved_usd":0.2}`))
		case q.Get("lean") == "1" && q.Get("since") == subWindowStart:
			w.Write([]byte(window))
		case q.Get("lean") == "1":
			t.Errorf("unexpected lean stats read %s", r.URL.RawQuery)
			http.NotFound(w, r)
		default:
			rest.ServeHTTP(w, r)
		}
	})
	mux.Handle("/", rest)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL, &reads
}

// subWindowTokens is a lean /api/stats answer for the 5-hour window, with the given dollars for the estimate.
func subWindowTokens(cost, saved string) string {
	return `{"tokens":{"saved":{"input":0,"cache_read":1200000,"cache_write_5m":85000,"cache_write_1h":0},` +
		`"caused":{"input":0,"cache_read":9000,"cache_write_5m":0,"cache_write_1h":0}},` +
		`"tokens_unmeasured_requests":0,"cost_usd":` + cost + `,"total_saved_usd":` + saved + `}`
}

var subWindow = subWindowTokens("12.4", "0.7")

const (
	subSessionLine = "This session:       saved 42.0k wr · 610.0k rd"
	subWindowLine  = "This 5-hour window: saved 85.0k wr · 1.2M rd · cost 9.0k rd"
)

func TestCGSubscriptionGoldens(t *testing.T) {
	base := cgSubscriptionStub(t, subLimits, subWindow)
	for name, args := range map[string][]string{
		"home-subscription":             {"home"},
		"insights-savings-subscription": {"insights", "savings"},
	} {
		t.Run(name, func(t *testing.T) { cgGolden(t, name, runCG(t, base, nil, args...)) })
	}
}

// With no subscription signal the screens are exactly today's goldens.
func TestCGSubscriptionAbsentKeepsTheDollarView(t *testing.T) {
	for _, limits := range []string{`{}`, "404", "500", `not json`, `{"subscription":false}`} {
		base := cgSubscriptionStub(t, limits, subWindow)
		for golden, args := range map[string][]string{"home": {"home"}, "insights-savings": {"insights", "savings"}} {
			if got, want := runCG(t, base, nil, args...), string(cgRead(t, "cg", golden+".golden")); got != want {
				t.Errorf("limits %s: %s changed.\n--- got\n%s\n--- want\n%s", limits, golden, got, want)
			}
		}
	}
}

func TestCGSubscriptionEstimateNeedsEnoughData(t *testing.T) {
	const est = "of the 5-hour limit (estimate at API-price weights)"
	for _, c := range []struct {
		name, limits, window string
		lines, notLines      []string
	}{
		{"low use", strings.Replace(subLimits, "0.54", "0.01", 1), subWindow,
			[]string{subSessionLine, subWindowLine}, []string{est}},
		{"small spend", subLimits, subWindowTokens("0.49", "0.2"), []string{subWindowLine}, []string{est}},
		{"nothing saved", subLimits, `{"tokens":{"saved":{},"caused":{}},"cost_usd":4,"total_saved_usd":0}`,
			[]string{"This 5-hour window: nothing saved yet"}, []string{est}},
		{"window over", strings.Replace(subLimits, subReset, "1791473000", 1), subWindow,
			[]string{"This session: saved 42.0k wr · 610.0k rd"}, []string{"5-hour window", est}},
		{"no window", `{"subscription":true}`, subWindow,
			[]string{"This session: saved 42.0k wr · 610.0k rd"}, []string{"5-hour window", est}},
		{"estimate", subLimits, subWindow, []string{"                    ≈ 3% " + est}, nil},
		// The provider stops a user at 100%, so the share saved is at most 100 - used (here 55).
		{"over the cap", strings.Replace(subLimits, "0.54", "0.45", 1), subWindowTokens("15", "45"),
			[]string{"                    ≈ without context-guru you would have hit the 5-hour limit (estimate at API-price weights)"}, []string{est}},
		{"at the cap", strings.Replace(subLimits, "0.54", "0.45", 1), subWindowTokens("15", "18.4"),
			[]string{"                    ≈ without context-guru you would have hit the 5-hour limit (estimate at API-price weights)"}, []string{est}},
		{"just below the cap", strings.Replace(subLimits, "0.54", "0.45", 1), subWindowTokens("15", "18"),
			[]string{"                    ≈ 54% " + est}, []string{"would have hit"}},
	} {
		base := cgSubscriptionStub(t, c.limits, c.window)
		out := runCG(t, base, nil, "home")
		if !strings.HasPrefix(out, "This session:") || strings.Contains(out, "$") {
			t.Errorf("%s: want the token lead and no dollars:\n%s", c.name, out)
		}
		for _, l := range c.lines {
			if !strings.Contains(out, l+"\n") {
				t.Errorf("%s: want line %q in:\n%s", c.name, l, out)
			}
		}
		for _, s := range c.notLines {
			if strings.Contains(out, s) {
				t.Errorf("%s: want no %q in:\n%s", c.name, s, out)
			}
		}
	}
}

// A subscription with no session id and no open window still shows the period in tokens, with no dollars.
func TestCGSubscriptionNoSessionNoWindow(t *testing.T) {
	base := cgSubscriptionStub(t, `{"subscription":true}`, subWindow)
	for _, args := range [][]string{{"home"}, {"insights", "savings"}} {
		got := runCG(t, base, []string{"CLAUDE_CODE_SESSION_ID="}, args...)
		if !regexp.MustCompile(`(?m)^7 days: +saved 214\.0k wr · 1\.6M rd · 12\.0k in · cost 3\.0k wr · 96\.0k rd$`).MatchString(got) ||
			strings.Contains(got, "$") || strings.Contains(got, "5-hour") {
			t.Errorf("%v: want the period in tokens, no dollars, no window:\n%s", args, got)
		}
	}
}

// The tiers are never added: each shows on its own, in the order wr, wr1h, rd, in; a tier at 0 is left out; the
// cost part shows only when a tier was paid. Requests with no token figure say "not measured", not "saved 0".
func TestCGSubscriptionTokenLines(t *testing.T) {
	for _, c := range []struct{ name, window, want string }{
		{"all tiers", `{"tokens":{"saved":{"input":7,"cache_read":3000,"cache_write_5m":2000,"cache_write_1h":1500000},` +
			`"caused":{"input":0,"cache_read":0,"cache_write_5m":0,"cache_write_1h":0}}}`,
			"This 5-hour window: saved 2.0k wr · 1.5M wr1h · 3.0k rd · 7 in"},
		{"cost only", `{"tokens":{"saved":{},"caused":{"cache_read":40000}}}`, "This 5-hour window: cost 40.0k rd"},
		{"no tokens field", `{"cost_usd":4}`, "This 5-hour window: nothing saved yet"},
	} {
		base := cgSubscriptionStub(t, subLimits, c.window)
		if out := runCG(t, base, nil, "home"); !strings.Contains(out, c.want+"\n") {
			t.Errorf("%s: want line %q in:\n%s", c.name, c.want, out)
		}
	}
	base := cgSubscriptionStub(t, subLimits, subWindow)
	if out := runCG(t, base, nil, "home"); !strings.Contains(out, "\n                    3 requests not measured in tokens\n") {
		t.Errorf("want the not-measured note under the 7 days line:\n%s", out)
	}
}

// `insights` draws three screens in one run; it reads the limits once.
func TestCGSubscriptionReadsLimitsOncePerRun(t *testing.T) {
	base, reads := cgSubscriptionStubCounted(t, subLimits, subWindow)
	out := runCG(t, base, nil, "insights")
	if !strings.Contains(out, subWindowLine) {
		t.Errorf("want the token lead in the Savings section:\n%s", out)
	}
	if n := reads.Load(); n != 1 {
		t.Errorf("GET /api/me/limits read %d times in one run, want 1", n)
	}
}
