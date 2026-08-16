package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/tenant"
)

// offloadTenancy is a hosted proxy whose tenants run a real offloading pipeline, so
// GET /expand can be exercised end to end: a chat request stashes an original and
// records its owner, then the HTTP half of the reversibility invariant has to hand it
// back — to that tenant and no other.
func offloadTenancy(t *testing.T) (*httptest.Server, *tenant.Registry) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"ok","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("TEST_UPSTREAM_KEY", "not-a-real-key")

	const cfgDoc = "pipeline: [collapse]\ncomponents:\n  collapse: {max_tokens: 20, head_lines: 2, tail_lines: 2}\nmode: sync\n"
	reg, err := tenant.Open("", tenant.Options{
		DefaultConfig: cfgDoc, DefaultUpOpenAI: "up", DefaultUpAnthropic: "up",
		Validate: config.Validate,
	})
	if err != nil {
		t.Fatalf("tenant.Open: %v", err)
	}
	t.Cleanup(func() { reg.Close() })

	agg := metrics.NewAggregator()
	build := func(doc []byte, e components.Emitter) (BuiltConfig, error) {
		cfg, err := config.LoadBytes(doc)
		if err != nil {
			return BuiltConfig{}, err
		}
		pipe, err := cfg.Build(e)
		if err != nil {
			return BuiltConfig{}, err
		}
		return BuiltConfig{Pipe: pipe, Store: store.NewMemory(store.Options{}), Preset: "test"}, nil
	}
	h := New(components.NewPipeline(nil, nil), store.NewMemory(store.Options{}), agg, Options{
		Tenants: NewTenantSource(reg, agg, build, 0),
		Upstreams: map[string]Upstream{"up": {
			Dialect: "openai", BaseURL: upstream.URL, KeyEnv: "TEST_UPSTREAM_KEY",
		}},
	})
	srv := httptest.NewServer(h.Mux())
	t.Cleanup(srv.Close)
	return srv, reg
}

// The angle brackets come back JSON-escaped, so match the id itself.
var markerRe = regexp.MustCompile(`cg:([0-9a-f]{16})`)

// chatAndMarker sends one offloadable request and returns the marker id it minted.
func chatAndMarker(t *testing.T, srv *httptest.Server, token, sess string) string {
	t.Helper()
	var log strings.Builder
	for i := 0; i < 40; i++ {
		log.WriteString("log content line with enough words to matter here\n")
	}
	body := `{"model":"gpt-x","messages":[{"role":"tool","content":` +
		strings.ReplaceAll(`"`+log.String()+`"`, "\n", `\n`) + `}]}`
	// /compact runs the same pipeline against the same tenant store as a chat request
	// (so the stash and its owner record are written identically) and returns the
	// rewritten body, which is where the marker can be read from.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/compact", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-context-guru-session", sess)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/compact = %d %s", resp.StatusCode, out)
	}
	m := markerRe.FindStringSubmatch(string(out))
	if m == nil {
		t.Fatalf("no expand marker was minted: %s", out)
	}
	return m[1]
}

func getExpand(t *testing.T, srv *httptest.Server, token, sess, id string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/expand?id="+id, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if sess != "" {
		req.Header.Set("x-context-guru-session", sess)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// The owner of a stash must be able to expand it. Regression: the handler compared the
// RAW session header against owner keys recorded under the TENANT-SCOPED session, so
// hosted /expand 404'd for everyone — fail-closed, but the HTTP half of reversibility
// was dead.
func TestHostedExpandOwnSessionAndCrossTenant(t *testing.T) {
	srv, reg := offloadTenancy(t)
	_, tokA, err := reg.Register("laptop", "a@ibm.com")
	if err != nil {
		t.Fatal(err)
	}
	_, tokB, err := reg.Register("laptop", "b@ibm.com")
	if err != nil {
		t.Fatal(err)
	}

	id := chatAndMarker(t, srv, tokA, "sess-1")

	if code, body := getExpand(t, srv, tokA, "sess-1", id); code != http.StatusOK {
		t.Fatalf("a tenant cannot expand its own marker: %d %s", code, body)
	} else if !strings.Contains(body, "log content line") {
		t.Fatalf("/expand returned something other than the original: %q", body)
	}
	// Another tenant, same id: resolved against ITS store, and not owned either way.
	if code, _ := getExpand(t, srv, tokB, "sess-1", id); code != http.StatusNotFound {
		t.Errorf("another tenant expanded a stash it does not own: %d", code)
	}
	// The colon attack. ':' is inside the id charset and the scoped key is
	// "<tenant>:<session>", so a client can put a whole composite key in the header.
	// It must not forge the victim's namespace: scoping PREFIXES, so B asking for
	// "<A>:sess-1" resolves to "B:<A>:sess-1" — and against B's store, not A's.
	tenA, err := reg.Resolve(tokA)
	if err != nil {
		t.Fatal(err)
	}
	for _, forged := range []string{
		tenA.ID + ":sess-1",       // A's exact scoped key
		":" + tenA.ID + ":sess-1", // ...with a leading separator
		tenA.ID + ":sess-1:",      // ...and a trailing one
	} {
		if code, _ := getExpand(t, srv, tokB, forged, id); code != http.StatusNotFound {
			t.Errorf("session %q forged another tenant's namespace: %d", forged, code)
		}
	}
	// A tenant naming its OWN id as the session is not a shortcut either.
	if code, _ := getExpand(t, srv, tokB, tenA.ID, id); code != http.StatusNotFound {
		t.Errorf("a tenant id used as a session id expanded another tenant's stash: %d", code)
	}
	// Same tenant, a different session it never stashed under.
	if code, _ := getExpand(t, srv, tokA, "other-session", id); code != http.StatusNotFound {
		t.Errorf("a session expanded a stash it does not own: %d", code)
	}
	// No session at all stays a miss rather than matching an empty-content key.
	if code, _ := getExpand(t, srv, tokA, "", id); code != http.StatusNotFound {
		t.Errorf("/expand with no session = %d, want 404", code)
	}
}
