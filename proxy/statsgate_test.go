package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// /stats is a PROCESS-WIDE aggregate over every tenant, gated by default on the caller
// being on this host. The default predicate used to test only that the transport peer
// was loopback, which silently stops meaning anything once a reverse proxy terminates
// TLS in front: nginx connects from 127.0.0.1, so EVERY remote caller satisfied it.
// Observed live on the deployed service — an unauthenticated GET /stats over TLS
// returned the whole rollup.
//
// The distinguishing signal is the forwarded headers the front end adds. A remote
// caller cannot forge a loopback RemoteAddr, so it can never reach the loopback branch
// on its own; a loopback peer that DOES set them is the front end relaying someone
// else. Each header gets its own case because a deployment behind Apache, Envoy or a
// k8s ingress may set a different subset than nginx does, and any one of them is
// already proof the request was relayed.
func TestStatsGateRejectsRelayedLoopbackRequests(t *testing.T) {
	h := &Handler{}

	for _, tc := range []struct {
		name    string
		peer    string
		headers map[string]string
		want    bool
	}{
		// The two cases that must keep working, or this fix breaks operations: local
		// ops on the box, and the Prometheus job that scrapes 127.0.0.1:4000 direct.
		// Neither goes through a proxy, so neither sets a forwarded header.
		{"local ops, no forwarded headers", "127.0.0.1:5555", nil, true},
		{"local ops over IPv6 loopback", "[::1]:5555", nil, true},

		// Relayed through a same-host front end. Before the fix every one of these
		// returned true and served the aggregate to the network.
		{"relayed by nginx", "127.0.0.1:5555", map[string]string{"X-Forwarded-For": "9.1.2.3"}, false},
		{"relayed, X-Real-IP only", "127.0.0.1:5555", map[string]string{"X-Real-IP": "9.1.2.3"}, false},
		{"relayed, proto only", "127.0.0.1:5555", map[string]string{"X-Forwarded-Proto": "https"}, false},
		{"relayed, host only", "127.0.0.1:5555", map[string]string{"X-Forwarded-Host": "cg.ibm.com"}, false},

		// A remote peer was never trusted and still is not, headers or no headers.
		// The second case is the important one: a caller that strips its own
		// forwarded headers must not thereby look local.
		{"remote peer", "9.1.2.3:5555", nil, false},
		{"remote peer with no headers", "9.1.2.3:5555", map[string]string{}, false},

		// A forged XFF from a remote peer is irrelevant — the branch is unreachable.
		{"remote peer forging loopback XFF", "9.1.2.3:5555", map[string]string{"X-Forwarded-For": "127.0.0.1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/stats", nil)
			r.RemoteAddr = tc.peer
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if got := h.statsTrusted(r); got != tc.want {
				t.Errorf("statsTrusted(peer=%s, headers=%v) = %v, want %v",
					tc.peer, tc.headers, got, tc.want)
			}
		})
	}
}

// An explicit host-supplied predicate still wins outright: a deployment that knows how
// to identify its own managers must not have that decision second-guessed by the
// header heuristic, in either direction.
func TestStatsGateHonoursExplicitPredicate(t *testing.T) {
	for _, want := range []bool{true, false} {
		h := &Handler{opts: Options{StatsTrusted: func(*http.Request) bool { return want }}}
		r := httptest.NewRequest(http.MethodGet, "/stats", nil)
		// Deliberately the shape the default predicate would REJECT, so a true
		// result can only have come from the override.
		r.RemoteAddr = "127.0.0.1:5555"
		r.Header.Set("X-Forwarded-For", "9.1.2.3")
		if got := h.statsTrusted(r); got != want {
			t.Errorf("explicit predicate returning %v was overridden, got %v", want, got)
		}
	}
}
