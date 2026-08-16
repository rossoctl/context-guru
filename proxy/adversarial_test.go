package proxy

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The registration mode is switched on in TWO places — the control plane enforces it
// and the startup banner reports it — so the value must be resolved by one function.
// Before this, the banner read CG_REGISTER raw: "Open" enforced as OPEN while the log
// said self-registration was off.
//
// The same table pins fail-closed for everything that is not exactly open/invite.
func TestRegisterModeNormalisesAndFailsClosed(t *testing.T) {
	for raw, want := range map[string]string{
		"open": "open", "Open": "open", "OPEN": "open", " open ": "open",
		"invite": "invite", "Invite": "invite",
		"closed": "closed", "": "closed", "1": "closed", "true": "closed",
		"opne": "closed", "open-ish": "closed", "yes": "closed",
	} {
		t.Setenv(envRegisterMode, raw)
		if got := RegisterMode(); got != want {
			t.Errorf("RegisterMode() with CG_REGISTER=%q = %q, want %q", raw, got, want)
		}
	}
}

// And the enforcement agrees with that resolution: a mode the banner reports as open
// must actually register, and one it reports as closed must actually refuse.
func TestRegistrationEnforcementMatchesReportedMode(t *testing.T) {
	for i, raw := range []string{"OPEN", " open", "Open", "1", "true", "opne", ""} {
		t.Setenv(envRegisterMode, raw)
		f := newHostedFixture(t, "up", "openai")
		mode := RegisterMode()
		// A fresh address per case, so the rate limit never decides the outcome.
		code := registerVia(t, f, "a@ibm.com", "", "203.0.113."+strconv.Itoa(i+1)+":9999")
		switch mode {
		case "open":
			if code != http.StatusCreated {
				t.Errorf("CG_REGISTER=%q reports %q but register = %d, want 201", raw, mode, code)
			}
		default:
			if code != http.StatusForbidden {
				t.Errorf("CG_REGISTER=%q reports %q but register = %d, want 403", raw, mode, code)
			}
		}
	}
}

// The banner in cmd/context-guru-proxy must not read CG_REGISTER itself — that is
// exactly how it came to report "off" for a value this package accepts. Source
// inspection because there is no way to call into package main; same approach as the
// METRICS_TOKEN check in security_test.go, and the thing a reviewer would grep for.
func TestStartupBannerUsesTheSharedRegisterResolver(t *testing.T) {
	src, err := os.ReadFile("../cmd/context-guru-proxy/main.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if !strings.Contains(body, "proxy.RegisterMode()") {
		t.Error("main.go no longer reports the mode via proxy.RegisterMode()")
	}
	// The flag help text may name the variable; a READ of it is the problem.
	for _, bad := range []string{`Getenv("CG_REGISTER")`, `envOr("CG_REGISTER"`} {
		if strings.Contains(body, bad) {
			t.Errorf("main.go reads CG_REGISTER directly (%s); the banner can disagree with "+
				"the control plane's normalisation again", bad)
		}
	}
}

// X-Forwarded-For is a forgeable header, and the bucket key is the one thing an
// attacker would love to control. Trusted from a loopback peer (our own nginx, which
// appends the address it saw) and from nobody else.
func TestRegistrationBucketTrustsForwardedForOnlyFromLoopback(t *testing.T) {
	req := func(remote, xff string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/register", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}
	// A remote caller cannot buy a bucket with a header, however it dresses it up.
	for _, xff := range []string{"1.2.3.4", "1.2.3.4, 5.6.7.8", "not-an-ip", "127.0.0.1"} {
		if got := registrantIP(req("198.51.100.9:4444", xff)); got != "198.51.100.9" {
			t.Errorf("a remote client set its own bucket via XFF %q: got %q", xff, got)
		}
	}
	// Behind our own nginx: the LAST element is the address nginx saw, so two clients
	// get two buckets instead of sharing the loopback one.
	if got := registrantIP(req("127.0.0.1:5555", "9.9.9.9, 203.0.113.7")); got != "203.0.113.7" {
		t.Errorf("behind a loopback proxy the client address was not used: got %q", got)
	}
	if got := registrantIP(req("127.0.0.1:5555", "")); got != "127.0.0.1" {
		t.Errorf("a loopback peer with no XFF should key on itself, got %q", got)
	}
	if got := registrantIP(req("127.0.0.1:5555", "garbage")); got != "127.0.0.1" {
		t.Errorf("an unparseable XFF should fall back to the peer, got %q", got)
	}
}

// Per-ADDRESS limiting is free to bypass on IPv6: the smallest allocation anyone gets
// is a /64. The bucket is therefore the prefix, not the address.
func TestRegistrationBucketIsPerIPv6Prefix(t *testing.T) {
	if a, b := regBucket("2001:db8::1"), regBucket("2001:db8::dead:beef"); a != b {
		t.Errorf("two addresses in one /64 got separate buckets: %q vs %q", a, b)
	}
	if a, b := regBucket("2001:db8::1"), regBucket("2001:db8:0:1::1"); a == b {
		t.Errorf("two different /64s share a bucket: %q", a)
	}
	for _, v4 := range []string{"203.0.113.7", "127.0.0.1", "not-an-ip"} {
		if got := regBucket(v4); got != v4 {
			t.Errorf("regBucket(%q) = %q, want it unchanged", v4, got)
		}
	}

	// End to end: rotating the host part of one /64 does not buy fresh budget.
	t.Setenv(envRegisterMode, "open")
	f := newHostedFixture(t, "up", "openai")
	limited := false
	for i := 0; i < registrationsPerMinute+3; i++ {
		addr := "[2001:db8::" + strconv.Itoa(i+1) + "]:5555"
		if registerVia(t, f, "u"+strconv.Itoa(i)+"@ibm.com", "", addr) == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Error("rotating addresses inside one IPv6 /64 bypassed the registration limit")
	}
}
