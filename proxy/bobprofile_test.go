package proxy_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/proxy"
	"github.com/rossoctl/context-guru/store"
)

// ideProfile is the profile shape Bob IDE 2.3.0 reads (seen live 2026-11-02).
// Both instances carry a region_domain, so the IDE would rewrite its host for either.
const ideProfile = `{"user_id":"u@example.com","instances":[` +
	`{"user_id":"u@example.com","instance_id":"i1","instance_name":"Main","role":"admin",` +
	`"plan_id":"p1","plan_name":"Pro","is_internal":false,"refresh_at":"2026-11-02T10:00:00Z",` +
	`"expires_at":"2026-12-02T10:00:00Z","subscription_id":"s1","region":"us-east",` +
	`"region_domain":"us-east.bob.example.com","allow_overage":true,"overage_limit":50,` +
	`"addons":["a1"],"teams":[{"id":"t1","name":"Team One","budget_limit":100,"usage":12.5}]},` +
	`{"user_id":"u@example.com","instance_id":"i2","instance_name":"EU","role":"member",` +
	`"plan_id":"p2","plan_name":"Free","is_internal":true,"refresh_at":"2026-11-03T10:00:00Z",` +
	`"expires_at":"2026-12-03T10:00:00Z","subscription_id":"s2","region":"eu-de",` +
	`"region_domain":"eu-de.bob.example.com","allow_overage":false,"overage_limit":0,` +
	`"addons":[],"teams":[]}]}`

// bobUpstream is a fake Bob backend. It answers with status and body on every path. It
// compresses with gzip when the request accepts gzip, and records each Accept-Encoding.
type bobUpstream struct {
	status int
	body   string

	mu    sync.Mutex
	sawAE []string // Accept-Encoding per request, "" when absent
}

func (b *bobUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ae := r.Header.Get("Accept-Encoding")
	b.mu.Lock()
	b.sawAE = append(b.sawAE, ae)
	b.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if strings.Contains(ae, "gzip") {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		zw.Write([]byte(b.body))
		zw.Close()
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(b.status)
		w.Write(buf.Bytes())
		return
	}
	w.WriteHeader(b.status)
	w.Write([]byte(b.body))
}

func (b *bobUpstream) acceptEncodings() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.sawAE)
}

// bobGateway starts a proxy with only BobUpstream set, in front of up.
func bobGateway(t *testing.T, up *bobUpstream) *httptest.Server {
	t.Helper()
	us := httptest.NewServer(up)
	t.Cleanup(us.Close)
	cfg, err := config.LoadBytes([]byte("pipeline: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	pipe, err := cfg.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	h := proxy.New(pipe, store.NewMemory(store.Options{}), nil, proxy.Options{BobUpstream: us.URL})
	srv := httptest.NewServer(h.Mux())
	t.Cleanup(srv.Close)
	return srv
}

// bobGet sends a GET as the IDE does. The client transport does not decompress, so the
// test sees the bytes the proxy really sent.
func bobGet(t *testing.T, url, acceptEncoding string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "IBM Bob/2.3.0")
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	tr := &http.Transport{DisableCompression: true}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

// assertIDEProfileWithoutRegion checks that body is ideProfile with region and
// region_domain removed from each instance, and nothing else changed.
func assertIDEProfileWithoutRegion(t *testing.T, body []byte) {
	t.Helper()
	var got, want map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("client body does not parse as JSON: %v (%q)", err, body)
	}
	if err := json.Unmarshal([]byte(ideProfile), &want); err != nil {
		t.Fatal(err)
	}
	for _, it := range want["instances"].([]any) {
		delete(it.(map[string]any), "region")
		delete(it.(map[string]any), "region_domain")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("profile body:\n got %v\nwant %v", got, want)
	}
	if insts, _ := got["instances"].([]any); len(insts) != 2 {
		t.Fatalf("instances = %d; want 2", len(insts))
	}
}

// Only GET /admin/v1/profile is rewritten. Other paths and methods get a profile-shaped
// body with region fields back byte-for-byte, so a wider match cannot go unseen.
func TestBobRegionStripOnlyOnProfileGet(t *testing.T) {
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/admin/v1/teams/t1"},
		{http.MethodPost, "/admin/v1/profile"},
		{http.MethodGet, "/admin/v1/profile/x"},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			up := &bobUpstream{status: http.StatusOK, body: ideProfile}
			srv := bobGateway(t, up)
			req, err := http.NewRequest(c.method, srv.URL+c.path, nil)
			if err != nil {
				t.Fatal(err)
			}

			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}

			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d; want 200", resp.StatusCode)
			}
			if string(body) != ideProfile {
				t.Errorf("body was changed:\n got %s\nwant %s", body, ideProfile)
			}
		})
	}
}

// The IDE replaces its base URL host with "api." + region_domain. The proxy removes the
// two region fields through the real catch-all route, and keeps every other field.
func TestBobProfileRouteStripsRegionFromIDEProfile(t *testing.T) {
	up := &bobUpstream{status: http.StatusOK, body: ideProfile}
	srv := bobGateway(t, up)

	resp, body := bobGet(t, srv.URL+"/admin/v1/profile", "")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	assertIDEProfileWithoutRegion(t, body)
	if got, want := resp.ContentLength, int64(len(body)); got != want {
		t.Errorf("Content-Length = %d; body is %d bytes", got, want)
	}
}

// The IDE sends Accept-Encoding: gzip, deflate, br. The proxy must read the profile, so it
// does not pass the client's value on. Go's transport then asks for gzip by itself and
// unpacks the reply, so the upstream sees "gzip" (or nothing), never the client's br.
// The client must get plain JSON with no region fields.
func TestBobProfileRouteDropsClientAcceptEncoding(t *testing.T) {
	up := &bobUpstream{status: http.StatusOK, body: ideProfile}
	srv := bobGateway(t, up)

	resp, body := bobGet(t, srv.URL+"/admin/v1/profile", "gzip, deflate, br")

	if got := up.acceptEncodings(); len(got) != 1 || (got[0] != "" && got[0] != "gzip") {
		t.Errorf("upstream Accept-Encoding = %q; want one request with none or the transport's gzip", got)
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Errorf("client Content-Encoding = %q; want none (body is plain JSON)", ce)
	}
	assertIDEProfileWithoutRegion(t, body)
}

// Other Bob control-plane paths are verbatim: the client's Accept-Encoding reaches the
// upstream, and the compressed reply reaches the client as it was sent.
func TestBobControlPlanePassesAcceptEncodingThrough(t *testing.T) {
	const team = `{"id":"t1","region_domain":"us-east.bob.example.com"}`
	up := &bobUpstream{status: http.StatusOK, body: team}
	srv := bobGateway(t, up)

	resp, body := bobGet(t, srv.URL+"/admin/v1/teams/t1", "gzip, deflate, br")

	if got := up.acceptEncodings(); len(got) != 1 || got[0] != "gzip, deflate, br" {
		t.Fatalf("upstream Accept-Encoding = %q; want the client's value", got)
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("client Content-Encoding = %q; want gzip passed through", ce)
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("client body is not gzip: %v", err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != team {
		t.Errorf("body = %q; want %q unchanged", plain, team)
	}
}

// A profile reply that is not 200 is forwarded as it is: status and body.
func TestBobProfileRouteForwardsNon200Unchanged(t *testing.T) {
	const denied = `{"error":"unauthorized","instances":[{"region_domain":"us-east.bob.example.com"}]}`
	up := &bobUpstream{status: http.StatusUnauthorized, body: denied}
	srv := bobGateway(t, up)

	resp, body := bobGet(t, srv.URL+"/admin/v1/profile", "")

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401", resp.StatusCode)
	}
	if string(body) != denied {
		t.Errorf("body = %q; want %q", body, denied)
	}
}

// A 200 profile reply that is not a profile document is forwarded as it is (fail open).
func TestBobProfileRouteForwardsUnknownBodyUnchanged(t *testing.T) {
	for _, raw := range []string{`{"x":1}`, `not json`, `{"instances":"nope"}`} {
		t.Run(raw, func(t *testing.T) {
			up := &bobUpstream{status: http.StatusOK, body: raw}
			srv := bobGateway(t, up)

			resp, body := bobGet(t, srv.URL+"/admin/v1/profile", "")

			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d; want 200", resp.StatusCode)
			}
			if string(body) != raw {
				t.Errorf("body = %q; want %q unchanged", body, raw)
			}
		})
	}
}
