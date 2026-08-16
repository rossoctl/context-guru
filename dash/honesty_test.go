package dash

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestContentComponentsRoundTrip: exact diff attribution is only useful if it survives
// the store. The UI reads it from the request payload, so it has to come back out in the
// order it went in.
func TestContentComponentsRoundTrip(t *testing.T) {
	db := openTestDB(t)
	e := mkEvent(time.Now().UnixMilli(), "s1", "m", 1000, 800)
	e.Content = []ContentRow{
		{Path: "messages.2", Before: "long", After: "<<cg:abc>>", Components: []string{"mask", "collapse"}},
		{Path: "messages.3", Before: "x", After: "y"}, // no attribution recorded
	}
	if err := db.insertBatch([]*Event{e}); err != nil {
		t.Fatal(err)
	}
	got, err := db.Request(e.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Content) != 2 {
		t.Fatalf("content rows = %d, want 2", len(got.Content))
	}
	if want := []string{"mask", "collapse"}; !reflect.DeepEqual(got.Content[0].Components, want) {
		t.Errorf("attribution did not survive the store: %v, want %v", got.Content[0].Components, want)
	}
	if got.Content[1].Components != nil {
		t.Errorf("an unattributed row must come back empty, not [\"\"]: %v", got.Content[1].Components)
	}
}

// TestArchiveReportsConfiguredRemoteAndReachability: with cold storage configured but
// the boot probe failed, the host leaves Options.Remote nil. The payload used to report
// an empty remote name while listing archived rows, so the UI said "cold storage is not
// configured on this deployment" directly above two archived sessions. "Not configured"
// and "configured but unreachable right now" are different facts.
func TestArchiveReportsConfiguredRemoteAndReachability(t *testing.T) {
	// Configured, probe failed: a name, no Remote.
	a, rec := newTestAPI(t, Options{RemoteName: "box:context-guru"})
	seed(t, rec, mkEvent(time.Now().UnixMilli(), "s-cold", "m", 100, 90))
	if err := rec.DB().markArchived(
		coldCandidate{SessionID: "s-cold", LastTS: 1, Requests: 1},
		ArchiveFull, "archive/_single/2026/08/s-cold.full.jsonl.gz", 10, "box:context-guru"); err != nil {
		t.Fatal(err)
	}
	w, body := get(t, a, "/api/archive", "127.0.0.1:5000")
	if w.Code != http.StatusOK {
		t.Fatalf("/api/archive = %d", w.Code)
	}
	if got := body["remote"]; got != "box:context-guru" {
		t.Errorf("remote = %v, want the CONFIGURED name — an empty name reads as 'not configured' "+
			"while the list below it shows archived sessions", got)
	}
	if got := body["reachable"]; got != false {
		t.Errorf("reachable = %v, want false: the boot probe failed", got)
	}
	if rows, _ := body["archived"].([]any); len(rows) != 1 {
		t.Fatalf("expected the archive index to still be listed, got %v", body["archived"])
	}

	// Nothing configured at all: both facts empty/false, which is the OTHER answer.
	a2, _ := newTestAPI(t, Options{})
	_, body2 := get(t, a2, "/api/archive", "127.0.0.1:5000")
	if body2["remote"] != "" || body2["reachable"] != false {
		t.Errorf("with no cold storage configured, want remote=\"\" reachable=false, got %v / %v",
			body2["remote"], body2["reachable"])
	}

	// Configured AND reachable.
	a3, _ := newTestAPI(t, Options{Remote: newMemRemote(), RemoteName: "box:context-guru"})
	_, body3 := get(t, a3, "/api/archive", "127.0.0.1:5000")
	if body3["remote"] != "box:context-guru" || body3["reachable"] != true {
		t.Errorf("with cold storage up, want remote set and reachable=true, got %v / %v",
			body3["remote"], body3["reachable"])
	}
}

// TestCaptureDescriptionMatchesThePayload: the counters are zeroed for a non-manager
// (they are process-wide), so the paragraph explaining captured/written/dropped was
// describing data that is not in the response — which reads as a broken deployment.
func TestCaptureDescriptionMatchesThePayload(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	seed(t, rec, mkEvent(time.Now().UnixMilli(), "s1", "m", 100, 90))

	var manager bool
	a.SetAuth(func(*http.Request) (Principal, bool) {
		return Principal{TenantID: "acme", Manager: manager}, true
	})

	_, body := get(t, a, "/api/capture", "127.0.0.1:5000")
	desc, _ := body["description"].(string)
	for _, word := range []string{"Captured", "written", "dropped"} {
		if strings.Contains(desc, word) {
			t.Errorf("non-manager description still explains %q, a counter zeroed out of this payload: %q",
				word, desc)
		}
	}
	if desc == "" {
		t.Error("non-manager got no description at all; it should describe what IS in the payload")
	}

	manager = true
	_, mbody := get(t, a, "/api/capture", "127.0.0.1:5000")
	if !strings.Contains(mbody["description"].(string), "dropped") {
		t.Errorf("a manager DOES see the counters, so the full description must stay: %q", mbody["description"])
	}
}

// hostedCaptureAPI is a hosted API for one tenant, with both halves of the capture
// decision under the test's control: the operator's process-wide flag and that tenant's
// own consent. The pairing is the whole point — the reported state has to be the AND of
// them, and the reported cause has to name whichever one is off.
func hostedCaptureAPI(t *testing.T, operator, tenantConsent bool) (*API, *Recorder) {
	t.Helper()
	a, rec := newTestAPI(t, Options{CaptureContent: operator, ContentCap: 4096})
	a.SetAuth(func(*http.Request) (Principal, bool) {
		return Principal{TenantID: "acme"}, true
	})
	a.SetTenantCapture(func(id string) bool { return id == "acme" && tenantConsent })
	return a, rec
}

// TestCapturedReportsTheEffectiveDecisionAndWhoCanChangeIt.
//
// `content_captured` used to be the PROCESS-GLOBAL Options.CaptureContent, but the real
// decision is proxy.captureContentFor: the operator's gate AND that tenant's consent.
// Reporting the flag alone is wrong in both directions on a hosted service — it claims
// "captured" to an account that never consented, and when the OPERATOR's gate is the one
// that is off it still renders as the tenant's own setting, so the dashboard told users
// to go and enable something they had already enabled. That was the live symptom.
func TestCapturedReportsTheEffectiveDecisionAndWhoCanChangeIt(t *testing.T) {
	for _, tc := range []struct {
		name             string
		operator, tenant bool
		wantCaptured     bool
		wantBlockedBy    string
	}{
		{"both on", true, true, true, ""},
		// The one the process flag got backwards: capture is on service-wide, this
		// account never opted in, and the flag said its content was being captured.
		{"tenant has not consented", true, false, false, CaptureBlockedByTenant},
		// The live defect: the operator's gate is off, so no message about the tenant's
		// own setting can be true — they cannot fix this and must be told who can.
		{"operator gate off", false, true, false, CaptureBlockedByOperator},
		// Both off: the operator's gate is the outer one, so it is the honest answer —
		// turning on their consent would still capture nothing.
		{"both off", false, false, false, CaptureBlockedByOperator},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, rec := hostedCaptureAPI(t, tc.operator, tc.tenant)
			e := mkEvent(time.Now().UnixMilli(), "sess-1", "m", 1000, 1000)
			e.TenantID = "acme"
			seed(t, rec, e)

			for _, path := range []string{
				"/api/sessions/sess-1/transcript",
				"/api/requests/" + itoa(e.ID),
			} {
				_, body := get(t, a, path, "127.0.0.1:5000")
				if body["content_captured"] != tc.wantCaptured {
					t.Errorf("%s: content_captured = %v, want %v (operator=%v tenant=%v)",
						path, body["content_captured"], tc.wantCaptured, tc.operator, tc.tenant)
				}
				if got, _ := body["capture_blocked_by"].(string); got != tc.wantBlockedBy {
					t.Errorf("%s: capture_blocked_by = %q, want %q — the UI cannot say which "+
						"party has to act without it", path, got, tc.wantBlockedBy)
				}
			}
		})
	}
}

// Single-tenant mode has no consent layer, so the operator's flag IS the effective
// decision — and nothing may be blamed on a tenant that does not exist.
func TestSingleTenantCaptureStateIsJustTheOperatorFlag(t *testing.T) {
	for _, operator := range []bool{true, false} {
		a, rec := newTestAPI(t, Options{CaptureContent: operator, ContentCap: 4096})
		e := mkEvent(time.Now().UnixMilli(), "sess-local", "m", 1000, 1000)
		seed(t, rec, e)
		_, body := get(t, a, "/api/sessions/sess-local/transcript", "127.0.0.1:5000")
		if body["content_captured"] != operator {
			t.Errorf("single-tenant content_captured = %v, want %v", body["content_captured"], operator)
		}
		want := ""
		if !operator {
			want = CaptureBlockedByOperator
		}
		if got, _ := body["capture_blocked_by"].(string); got != want {
			t.Errorf("single-tenant capture_blocked_by = %q, want %q: there is no tenant here "+
				"to hold responsible", got, want)
		}
	}
}

// An unknown session id must carry a state like every other answer this route gives.
// Nine states and one code path answering with a bare error means every client needs a
// state machine plus a special case — and the drawer is becoming linkable, so this is
// about to be reachable from a stale bookmark rather than unreachable.
func TestUnknownSessionStillCarriesAState(t *testing.T) {
	a, _ := newTestAPI(t, Options{CaptureContent: true})
	w, body := get(t, a, "/api/sessions/no-such-session/transcript", "127.0.0.1:5000")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: it really is absent", w.Code)
	}
	if body["state"] != TranscriptUnknownSession {
		t.Errorf("state = %v, want %q", body["state"], TranscriptUnknownSession)
	}
	if body["error"] == nil {
		t.Error("the human-readable error is gone; the UI still shows it")
	}
}

// TestConfigSaysWhoseConfigurationItIs.
//
// Observed live: this route reported preset "codesmart" with extract_llm in the pipeline
// while all 26 requests that day ran preset "custom" and extract_llm never ran once —
// because the tenant followed its own configuration. The page is read as "what compacts
// my traffic", so the payload has to say whose configuration it is. Per-tenant config
// stays with the control plane's /api/me: one source of truth per question.
func TestConfigSaysWhoseConfigurationItIs(t *testing.T) {
	opts := Options{Effective: map[string]any{"preset": "codesmart"}}

	// Single-tenant: it IS every request's configuration, and says so.
	a, _ := newTestAPI(t, opts)
	_, body := get(t, a, "/api/config", "127.0.0.1:5000")
	desc, _ := body["description"].(string)
	if desc == "" {
		t.Error("no description: the page cannot say what configuration this is")
	}
	if body["scope"] != "server" {
		t.Errorf("scope = %v, want \"server\"", body["scope"])
	}
	cfg, _ := body["config"].(map[string]any)
	if cfg["preset"] != "codesmart" {
		t.Fatalf("the configuration itself is missing from the payload: %v", body)
	}

	// Hosted: it is the server DEFAULT, which a tenant may not be running at all. The
	// description has to say so and point at where the caller's own answer lives.
	a2, _ := newTestAPI(t, opts)
	a2.SetAuth(func(*http.Request) (Principal, bool) {
		return Principal{TenantID: "acme", Manager: true}, true
	})
	_, hbody := get(t, a2, "/api/config", "127.0.0.1:5000")
	hdesc, _ := hbody["description"].(string)
	for _, want := range []string{"NOT necessarily", "own configuration"} {
		if !strings.Contains(hdesc, want) {
			t.Errorf("hosted description does not mention %q, so nothing on the page keeps a "+
				"reader from taking the server default for their own: %q", want, hdesc)
		}
	}
}
