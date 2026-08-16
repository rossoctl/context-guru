package tenant

import (
	"errors"
	"testing"
	"time"
)

func TestWebSessionLifecycle(t *testing.T) {
	r := open(t, Options{})
	tn, tok := register(t, r, "a@ibm.com")

	got, cookie, err := r.NewWebSession(tok, 0)
	if err != nil {
		t.Fatalf("NewWebSession: %v", err)
	}
	if got.ID != tn.ID || cookie == "" {
		t.Fatalf("session for %v, cookie %q", got, cookie)
	}
	if cookie == tok {
		t.Fatal("the dashboard cookie is the proxy token; they must be separate credentials")
	}
	if back, err := r.WebSession(cookie); err != nil || back.ID != tn.ID {
		t.Fatalf("WebSession = %v, %v", back, err)
	}
	if err := r.EndWebSession(cookie); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WebSession(cookie); !errors.Is(err, ErrNoSession) {
		t.Errorf("after sign-out: %v, want ErrNoSession", err)
	}
	// Idempotent.
	if err := r.EndWebSession(cookie); err != nil {
		t.Errorf("second sign-out: %v", err)
	}
	if _, err := r.WebSession(""); !errors.Is(err, ErrNoSession) {
		t.Errorf("empty cookie: %v", err)
	}
}

func TestWebSessionRejectsBadToken(t *testing.T) {
	r := open(t, Options{})
	register(t, r, "a@ibm.com")
	if _, _, err := r.NewWebSession("cg_live_AAAAAAAAAAAAAAAAAAAAAAAAAA", 0); !errors.Is(err, ErrUnknownToken) {
		t.Errorf("signing in with an unissued token: %v", err)
	}
	if _, _, err := r.NewWebSession("garbage", 0); !errors.Is(err, ErrUnknownToken) {
		t.Errorf("signing in with junk: %v", err)
	}
}

// Expiry is enforced in the query, so a sweeper that never runs cannot extend a login.
func TestWebSessionExpiryIsEnforcedOnRead(t *testing.T) {
	r := open(t, Options{})
	_, tok := register(t, r, "a@ibm.com")
	_, cookie, err := r.NewWebSession(tok, -time.Hour) // negative => default TTL
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WebSession(cookie); err != nil {
		t.Fatalf("default TTL session should be live: %v", err)
	}
	// Force it into the past without running any sweeper.
	if _, err := r.db.Exec(`UPDATE dash_sessions SET expires_at = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WebSession(cookie); !errors.Is(err, ErrNoSession) {
		t.Errorf("an expired cookie still resolved: %v", err)
	}
	n, err := r.SweepWebSessions()
	if err != nil || n != 1 {
		t.Errorf("SweepWebSessions = %d, %v", n, err)
	}
}

// Disabling an account must close its dashboard logins too, not just block the proxy.
func TestDisablingEndsWebSessions(t *testing.T) {
	r := open(t, Options{ManagerEmail: "boss@ibm.com"})
	mgr, _ := register(t, r, "boss@ibm.com")
	a, tok := register(t, r, "a@ibm.com")
	_, cookie, err := r.NewWebSession(tok, 0)
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	if err := r.Update(mgr, a.ID, Patch{Disabled: &yes}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WebSession(cookie); err == nil {
		t.Fatal("a disabled tenant kept a live dashboard session")
	}
}
