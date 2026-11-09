// On a Claude subscription, `home` and `insights savings` lead with the tokens saved in this session and in the
// current 5-hour window, then the 5-hour share as an estimate (#986). Anything else is today's dollar view.
package plugin

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
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
	up, err := url.Parse(cgProxyStub(t, "cachestate_warm"))
	if err != nil {
		t.Fatal(err)
	}
	rest := httputil.NewSingleHostReverseProxy(up)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/me/limits", func(w http.ResponseWriter, _ *http.Request) {
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
			w.Write([]byte(`{"saved_unique":42000,"replay_tokens":610000,"cost_usd":3.1,"total_saved_usd":0.2}`))
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
	return srv.URL
}

const subWindow = `{"saved_unique":85000,"replay_tokens":1200000,"cost_usd":12.4,"total_saved_usd":0.7}`

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
			[]string{"This session:       saved 610.0k rd / 42.0k wr", "This 5-hour window: saved 1.2M rd / 85.0k wr"}, []string{est}},
		{"small spend", subLimits, `{"saved_unique":85000,"replay_tokens":1200000,"cost_usd":0.49,"total_saved_usd":0.2}`,
			[]string{"This 5-hour window: saved 1.2M rd / 85.0k wr"}, []string{est}},
		{"nothing saved", subLimits, `{"saved_unique":0,"replay_tokens":0,"cost_usd":4,"total_saved_usd":0}`,
			[]string{"This 5-hour window: nothing saved yet"}, []string{est}},
		{"window over", strings.Replace(subLimits, subReset, "1791473000", 1), subWindow,
			[]string{"This session: saved 610.0k rd / 42.0k wr"}, []string{"5-hour window", est}},
		{"no window", `{"subscription":true}`, subWindow,
			[]string{"This session: saved 610.0k rd / 42.0k wr"}, []string{"5-hour window", est}},
		{"estimate", subLimits, subWindow, []string{"                    ≈ 3% " + est}, nil},
	} {
		base := cgSubscriptionStub(t, c.limits, c.window)
		out := runCG(t, base, nil, "home")
		if !strings.HasPrefix(out, "This session:") || !strings.Contains(out, "\nAt API prices") {
			t.Errorf("%s: want the token lead and the API-price note:\n%s", c.name, out)
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
