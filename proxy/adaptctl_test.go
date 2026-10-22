package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/tenant"
)

func adaptHostedFixture(t *testing.T) *hostedFixture {
	t.Helper()
	t.Setenv(envRegisterMode, "open")
	return newHostedFixtureOpts(t, "up", "anthropic", "TEST_UPSTREAM_KEY", func(o *Options) {
		r, err := dash.NewRecorder(dash.Options{DBPath: ":memory:", BatchSize: 1, FlushInterval: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close() })
		o.Dashboard = r
	})
}

// offer stands in for a report the account just fetched.
func offer(h *Handler, tenantID, doc string, cands []string, settings map[string]map[string]int) string {
	return h.API().OfferAdapt("compaction", tenantID, map[string]any{"c1": dash.AdaptCompactionChoice{
		Candidates: cands, Settings: settings, ConfigHash: dash.AdaptConfigHash(doc)}})
}

func storedForm(t *testing.T, f *hostedFixture, jar []*http.Cookie) (config.Form, string) {
	t.Helper()
	_, me := f.do(t, "GET", "/api/me", "", jar)
	tn, _ := me["tenant"].(map[string]any)
	doc, _ := tn["effective_config_yaml"].(string)
	form, err := config.ParseForm(doc)
	if err != nil || form.ParseError != "" {
		t.Fatalf("stored document does not load: %v %s\n%s", err, form.ParseError, doc)
	}
	return form, doc
}

// Hosted: a plain account applies an offered option with its cookie; only the candidate change
// is stored; a stale basis, a moved configuration, another account's offer and an unknown id
// are refused; revert restores the previous document.
func TestAdaptApplyHostedStoresOnlyTheOfferedChange(t *testing.T) {
	f := adaptHostedFixture(t)
	f.signUp(t, "boss@ibm.com", "l")
	w, verify := f.signUp(t, "user@ibm.com", "u")
	jar := w.Result().Cookies()
	id := verify["tenant"].(map[string]any)["id"].(string)

	basis := offer(f.h, id, tenant.DefaultConfigYAML, []string{"format", "searchfold", "textclean", "extract"},
		map[string]map[string]int{"extract": {"min_tokens": 800}})
	if w, _ := f.do(t, "POST", "/api/adapt/compaction/apply", `{"option":"c1","basis":"stale"}`, jar); w.Code != http.StatusConflict {
		t.Errorf("stale basis = %d %s", w.Code, w.Body)
	}
	if w, _ := f.do(t, "POST", "/api/adapt/compaction/apply", `{"option":"c9","basis":"`+basis+`"}`, jar); w.Code != http.StatusConflict {
		t.Errorf("unknown option = %d %s", w.Code, w.Body)
	}
	w, out := f.do(t, "POST", "/api/adapt/compaction/apply", `{"option":"c1","basis":"`+basis+`"}`, jar)
	if w.Code != http.StatusOK || out["ok"] != true || out["undo"].(map[string]any)["option"] != "revert" {
		t.Fatalf("apply = %d %s", w.Code, w.Body)
	}
	form, doc := storedForm(t, f, jar)
	if got := strings.Join(form.Pipeline, ","); got != "format,searchfold,textclean,extract,cachesplit,toolfilter" {
		t.Fatalf("pipeline %s", got)
	}
	if v, _ := form.Components["extract"]["min_tokens"].(int); v != 800 || strings.Contains(doc, "keepalive") {
		t.Fatalf("stored document:\n%s", doc)
	}
	// The same offer again: the configuration it was computed against is gone.
	if w, _ := f.do(t, "POST", "/api/adapt/compaction/apply", `{"option":"c1","basis":"`+basis+`"}`, jar); w.Code != http.StatusConflict {
		t.Errorf("moved configuration = %d %s", w.Code, w.Body)
	}
	// Another account's offer is not this account's to apply.
	other := offer(f.h, "someone-else", doc, []string{"format"}, nil)
	if w, _ := f.do(t, "POST", "/api/adapt/compaction/apply", `{"option":"c1","basis":"`+other+`"}`, jar); w.Code != http.StatusConflict {
		t.Errorf("foreign offer = %d %s", w.Code, w.Body)
	}
	if w, _ := f.do(t, "POST", "/api/adapt/compaction/apply", `{"option":"revert"}`, jar); w.Code != http.StatusOK {
		t.Fatalf("revert = %d %s", w.Code, w.Body)
	}
	if form, _ = storedForm(t, f, jar); strings.Join(form.Pipeline, ",") != "format,dedup,toon,cmdfilter,searchfold,textclean,extract,cachesplit,toolfilter" {
		t.Fatalf("revert left %v", form.Pipeline)
	}
	// Nothing left to undo: the latest change is the revert itself.
	if w, _ := f.do(t, "POST", "/api/adapt/compaction/apply", `{"option":"revert"}`, jar); w.Code != http.StatusConflict {
		t.Errorf("second revert = %d %s", w.Code, w.Body)
	}
	if w, _ := f.do(t, "POST", "/api/adapt/compaction/apply", `{"option":"c1"}`, jar); w.Code != http.StatusBadRequest {
		t.Errorf("apply without a basis = %d %s", w.Code, w.Body)
	}
	w, _ = f.do(t, "GET", "/api/me/audit", "", jar)
	if n := strings.Count(w.Body.String(), "config_yaml"); n < 2 {
		t.Errorf("audit has %d configuration changes, want the apply and the revert", n)
	}
	if w, _ := f.do(t, "POST", "/api/adapt/compaction/apply", `{"option":"revert"}`, nil); w.Code == http.StatusOK {
		t.Error("an unauthenticated caller changed a configuration")
	}
}

// The account's proxy token reaches /api/adapt/* — the report and the apply — for its own
// account, and nothing else on the control plane; and applies are rate-limited.
func TestAdaptTokenScope(t *testing.T) {
	f := adaptHostedFixture(t)
	f.signUp(t, "boss@ibm.com", "l")
	_, verify := f.signUp(t, "user@ibm.com", "u")
	id := verify["tenant"].(map[string]any)["id"].(string)
	tok := verify["token"].(string)
	// Wired as main wires it.
	f.h.API().SetAuth(f.h.DashAuth())
	f.h.API().SetPricer(fixedPricer{})
	f.h.API().SetAdaptCompactionConfig(f.h.DashAdaptConfig())
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("content-type", "application/json")
		r.Header.Set(TokenHeader, tok)
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, r)
		return w
	}
	basis := offer(f.h, id, tenant.DefaultConfigYAML, []string{"format", "searchfold", "textclean"}, nil)
	if w := call("POST", "/api/adapt/compaction/apply", `{"option":"c1","basis":"`+basis+`"}`); w.Code != http.StatusOK {
		t.Fatalf("token apply = %d %s", w.Code, w.Body)
	}
	entries, _ := f.reg.Audit(id, 10)
	found := false
	for _, e := range entries {
		found = found || (e.Field == "adapt" && strings.Contains(e.After, "compaction c1 via proxy token"))
	}
	if !found {
		t.Errorf("no audit row names the token apply: %+v", entries)
	}
	for _, rt := range [][2]string{{"GET", "/api/me"}, {"GET", "/api/me/audit"}, {"GET", "/api/stats"}} {
		if w := call(rt[0], rt[1], ""); w.Code == http.StatusOK {
			t.Errorf("the proxy token opened %s %s", rt[0], rt[1])
		}
	}
	if w := call("PUT", "/api/me", `{"config_yaml":"pipeline: []\n"}`); w.Code == http.StatusOK {
		t.Error("the proxy token wrote a configuration document")
	}
	// The report route accepts it.
	if w := call("GET", "/api/adapt/compaction?since=1", ""); w.Code != http.StatusOK {
		t.Errorf("token refused on the report: %d %s", w.Code, w.Body)
	}
	limited := false
	for i := 0; i < adaptApplyBurst+1; i++ {
		if w := call("POST", "/api/adapt/compaction/apply", `{"option":"revert"}`); w.Code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Error("applies were not rate-limited")
	}
}

func localAdaptHandler(t *testing.T, override string) *Handler {
	t.Helper()
	build := func(doc []byte, e components.Emitter) (BuiltConfig, error) {
		cfg, err := config.LoadBytes(doc)
		if err != nil {
			return BuiltConfig{}, err
		}
		p, err := cfg.Build(e)
		return BuiltConfig{Pipe: p, Preset: cfg.Preset}, err
	}
	base := "preset: house\n"
	built, err := build([]byte(base), metrics.NewAggregator())
	if err != nil {
		t.Fatal(err)
	}
	rec, err := dash.NewRecorder(dash.Options{DBPath: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	h := New(built.Pipe, store.NewMemory(store.Options{}), nil, Options{Dashboard: rec,
		BaseConfig: base, BuildConfig: build, AdaptOverride: override})
	t.Cleanup(h.Close)
	return h
}

// Single-tenant: a loopback apply rebuilds the RUNNING pipeline at once, persists the override,
// a restarted proxy comes back with it, revert returns to the configuration it was started with,
// and nobody off loopback may apply.
func TestAdaptApplyLocalHotReloadsAndPersists(t *testing.T) {
	override := filepath.Join(t.TempDir(), "d.db.adapt.json")
	h := localAdaptHandler(t, override)
	mux := h.Mux()
	post := func(body, addr string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/adapt/compaction/apply", strings.NewReader(body))
		if after, ok := strings.CutPrefix(addr, "relayed:"); ok {
			addr = after
			r.Header.Set("X-Forwarded-For", "203.0.113.9")
		}
		r.RemoteAddr = addr
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	basis := offer(h, "", "preset: house\n", []string{"format", "searchfold", "textclean"}, nil)
	for _, addr := range []string{"198.51.100.7:4000", "relayed:127.0.0.1:4000"} {
		if w := post(`{"option":"c1","basis":"`+basis+`"}`, addr); w.Code != http.StatusForbidden {
			t.Errorf("apply from %s = %d %s", addr, w.Code, w.Body)
		}
	}
	if w := post(`{"option":"c1","basis":"`+basis+`"}`, "127.0.0.1:4000"); w.Code != http.StatusOK {
		t.Fatalf("apply = %d %s", w.Code, w.Body)
	}
	want := "format,searchfold,textclean,cachesplit,toolfilter"
	if got := strings.Join(h.static.Load().Pipe.Names(), ","); got != want {
		t.Fatalf("running pipeline %s, want %s", got, want)
	}
	if _, err := os.Stat(override); err != nil {
		t.Fatalf("override not saved: %v", err)
	}
	if got := strings.Join(localAdaptHandler(t, override).static.Load().Pipe.Names(), ","); got != want {
		t.Fatalf("restart came back with %s", got)
	}
	if w := post(`{"option":"revert"}`, "127.0.0.1:4000"); w.Code != http.StatusOK {
		t.Fatalf("revert = %d %s", w.Code, w.Body)
	}
	if got := strings.Join(h.static.Load().Pipe.Names(), ","); !strings.Contains(got, "dedup") {
		t.Fatalf("revert left %s", got)
	}
	if _, err := os.Stat(override); !os.IsNotExist(err) {
		t.Errorf("back at the start configuration, the override should be gone: %v", err)
	}
	// A tools revert is not a compaction revert.
	basis = offer(h, "", "preset: house\n", []string{"format"}, nil)
	if w := post(`{"option":"c1","basis":"`+basis+`"}`, "127.0.0.1:4000"); w.Code != http.StatusOK {
		t.Fatalf("apply = %d %s", w.Code, w.Body)
	}
	if _, err := h.adaptRevert(adaptWho{Local: true}, "tools"); err == nil {
		t.Error("a local tools revert undid a compaction apply")
	}
}

// An override written against a different start configuration is not applied: the user changed
// their preset since, and that newer choice wins.
func TestAdaptLocalOverrideYieldsToANewerStartConfig(t *testing.T) {
	override := filepath.Join(t.TempDir(), "o.json")
	stale := `{"base":"000000000000","doc":"pipeline: [format]\n"}`
	if err := os.WriteFile(override, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(localAdaptHandler(t, override).static.Load().Pipe.Names(), ","); got == "format" {
		t.Fatal("a stale override was applied")
	}
}

// Revert undoes only an adapt apply that is still the latest change: a manager's edit made
// after it can never be rolled back through this route.
func TestAdaptRevertNeverUndoesAManagersEdit(t *testing.T) {
	f := adaptHostedFixture(t)
	bw, _ := f.signUp(t, "boss@ibm.com", "l")
	boss := bw.Result().Cookies()
	w, verify := f.signUp(t, "user@ibm.com", "u")
	jar := w.Result().Cookies()
	id := verify["tenant"].(map[string]any)["id"].(string)
	basis := offer(f.h, id, tenant.DefaultConfigYAML, []string{"format", "searchfold", "textclean"}, nil)
	if w, _ := f.do(t, "POST", "/api/adapt/compaction/apply", `{"option":"c1","basis":"`+basis+`"}`, jar); w.Code != http.StatusOK {
		t.Fatalf("apply = %d %s", w.Code, w.Body)
	}
	if w, _ := f.do(t, "PATCH", "/api/tenants/"+id, `{"config_yaml":"pipeline: [format, cachesplit]\n"}`, boss); w.Code != http.StatusOK {
		t.Fatalf("manager edit = %d %s", w.Code, w.Body)
	}
	if w, _ := f.do(t, "POST", "/api/adapt/compaction/apply", `{"option":"revert"}`, jar); w.Code != http.StatusConflict {
		t.Fatalf("revert over a manager's edit = %d %s", w.Code, w.Body)
	}
	// The account's proxy token cannot either.
	r := httptest.NewRequest("POST", "/api/adapt/compaction/apply", strings.NewReader(`{"option":"revert"}`))
	r.Header.Set(TokenHeader, verify["token"].(string))
	tw := httptest.NewRecorder()
	f.mux.ServeHTTP(tw, r)
	if tw.Code != http.StatusConflict {
		t.Fatalf("token revert over a manager's edit = %d %s", tw.Code, tw.Body)
	}
	if form, _ := storedForm(t, f, jar); strings.Join(form.Pipeline, ",") != "format,cachesplit" {
		t.Fatalf("the manager's edit was lost: %v", form.Pipeline)
	}
	// And with no adapt apply at all, there is nothing to undo.
	w2, _ := f.signUp(t, "other@ibm.com", "o")
	if w, _ := f.do(t, "POST", "/api/adapt/compaction/apply", `{"option":"revert"}`, w2.Result().Cookies()); w.Code != http.StatusConflict {
		t.Errorf("revert with no apply = %d %s", w.Code, w.Body)
	}
}

// An adapt apply a manager made on the account's behalf is the manager's: the account's token
// cannot revert it.
func TestAdaptRevertIsOnlyTheCallersOwnApply(t *testing.T) {
	f := adaptHostedFixture(t)
	bw, _ := f.signUp(t, "boss@ibm.com", "l")
	boss := bw.Result().Cookies()
	_, verify := f.signUp(t, "user@ibm.com", "u")
	id := verify["tenant"].(map[string]any)["id"].(string)
	basis := offer(f.h, id, tenant.DefaultConfigYAML, []string{"format", "searchfold", "textclean"}, nil)
	if w, _ := f.do(t, "POST", "/api/adapt/compaction/apply?tenant="+id, `{"option":"c1","basis":"`+basis+`"}`, boss); w.Code != http.StatusOK {
		t.Fatalf("manager apply = %d %s", w.Code, w.Body)
	}
	r := httptest.NewRequest("POST", "/api/adapt/compaction/apply", strings.NewReader(`{"option":"revert"}`))
	r.Header.Set(TokenHeader, verify["token"].(string))
	tw := httptest.NewRecorder()
	f.mux.ServeHTTP(tw, r)
	if tw.Code != http.StatusConflict {
		t.Fatalf("token reverted a manager's apply: %d %s", tw.Code, tw.Body)
	}
}

// Revert is per kind, and a revert is not itself revertable: from an explicit stored document,
// apply, revert, revert again -> 409; a tools revert never undoes a compaction apply.
func TestAdaptRevertIsPerKindAndOnce(t *testing.T) {
	f := adaptHostedFixture(t)
	bw, _ := f.signUp(t, "boss@ibm.com", "l")
	w, verify := f.signUp(t, "user@ibm.com", "u")
	jar := w.Result().Cookies()
	id := verify["tenant"].(map[string]any)["id"].(string)
	explicit := "pipeline: [format, dedup, searchfold, cachesplit]\n"
	if w, _ := f.do(t, "PATCH", "/api/tenants/"+id, `{"config_yaml":"pipeline: [format, dedup, searchfold, cachesplit]\n"}`, bw.Result().Cookies()); w.Code != http.StatusOK {
		t.Fatalf("manager edit = %d %s", w.Code, w.Body)
	}
	basis := offer(f.h, id, explicit, []string{"format", "searchfold"}, nil)
	if w, _ := f.do(t, "POST", "/api/adapt/compaction/apply", `{"option":"c1","basis":"`+basis+`"}`, jar); w.Code != http.StatusOK {
		t.Fatalf("apply = %d %s", w.Code, w.Body)
	}
	who, err := f.h.adaptPrincipal(withCookies(httptest.NewRequest("GET", "/", nil), jar))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.adaptRevert(who, "tools"); err == nil {
		t.Fatal("a tools revert undid a compaction apply")
	}
	if w, _ := f.do(t, "POST", "/api/adapt/compaction/apply", `{"option":"revert"}`, jar); w.Code != http.StatusOK {
		t.Fatalf("revert = %d %s", w.Code, w.Body)
	}
	if form, _ := storedForm(t, f, jar); strings.Join(form.Pipeline, ",") != "format,dedup,searchfold,cachesplit" {
		t.Fatalf("revert left %v", form.Pipeline)
	}
	if w, _ := f.do(t, "POST", "/api/adapt/compaction/apply", `{"option":"revert"}`, jar); w.Code != http.StatusConflict {
		t.Fatalf("second revert = %d %s", w.Code, w.Body)
	}
}

func withCookies(r *http.Request, cs []*http.Cookie) *http.Request {
	for _, c := range cs {
		r.AddCookie(c)
	}
	return r
}

// save-more config: switching to a shipped preset stores `preset: <name>` with no explicit
// pipeline, keeps the rest of the document, and is undone by its own kind's revert only.
func TestSaveMoreAppliesAPresetAndRevertsIt(t *testing.T) {
	f := adaptHostedFixture(t)
	bw, _ := f.signUp(t, "boss@ibm.com", "l")
	w, verify := f.signUp(t, "user@ibm.com", "u")
	jar := w.Result().Cookies()
	id := verify["tenant"].(map[string]any)["id"].(string)
	start := "pipeline: [format, dedup, cachesplit]\nmode: sync\n"
	if w, _ := f.do(t, "PATCH", "/api/tenants/"+id, `{"config_yaml":"pipeline: [format, dedup, cachesplit]\nmode: sync\n"}`, bw.Result().Cookies()); w.Code != http.StatusOK {
		t.Fatalf("manager edit = %d %s", w.Code, w.Body)
	}
	basis := f.h.API().OfferAdapt("compaction", id, map[string]any{"safe": dash.AdaptCompactionChoice{
		Preset: "safe", ConfigHash: dash.AdaptConfigHash(start)}})
	w, out := f.do(t, "POST", "/api/save-more/config/apply", `{"option":"safe","basis":"`+basis+`"}`, jar)
	if w.Code != http.StatusOK || out["ok"] != true {
		t.Fatalf("apply = %d %s", w.Code, w.Body)
	}
	form, doc := storedForm(t, f, jar)
	if want, _ := dash.AdaptPresetDoc(start, "safe"); doc != want {
		t.Fatalf("stored %q, the report priced %q", doc, want)
	}
	if !strings.Contains(doc, "preset: safe") || strings.Contains(doc, "pipeline:") || !strings.Contains(doc, "mode: sync") {
		t.Fatalf("stored document:\n%s", doc)
	}
	want, _ := config.PresetPipeline("safe")
	if strings.Join(form.Pipeline, ",") != strings.Join(want, ",") {
		t.Fatalf("pipeline %v, want the preset's %v", form.Pipeline, want)
	}
	who, err := f.h.adaptPrincipal(withCookies(httptest.NewRequest("GET", "/", nil), jar))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.adaptRevert(who, "tools"); err == nil {
		t.Error("a tools revert undid a config apply")
	}
	// Either route's revert: one report, one kind.
	if w, _ := f.do(t, "POST", "/api/adapt/compaction/apply", `{"option":"revert"}`, jar); w.Code != http.StatusOK {
		t.Fatalf("revert = %d %s", w.Code, w.Body)
	}
	if form, _ = storedForm(t, f, jar); strings.Join(form.Pipeline, ",") != "format,dedup,cachesplit" {
		t.Fatalf("revert left %v", form.Pipeline)
	}
}

// "<preset> without your overrides": applying it writes exactly the pure document the report
// priced, without the account's own block for the preset's component.
func TestSaveMoreAppliesThePresetWithoutOverrides(t *testing.T) {
	f := adaptHostedFixture(t)
	bw, _ := f.signUp(t, "boss@ibm.com", "l")
	w, verify := f.signUp(t, "user@ibm.com", "u")
	jar := w.Result().Cookies()
	id := verify["tenant"].(map[string]any)["id"].(string)
	start := "components:\n    format:\n        min_tokens: 999999\npipeline:\n    - cachesplit\n"
	body, _ := json.Marshal(map[string]string{"config_yaml": start})
	if w, _ := f.do(t, "PATCH", "/api/tenants/"+id, string(body), bw.Result().Cookies()); w.Code != http.StatusOK {
		t.Fatalf("manager edit = %d %s", w.Code, w.Body)
	}
	_, stored := storedForm(t, f, jar)
	basis := f.h.API().OfferAdapt("compaction", id, map[string]any{"safe-without-overrides": dash.AdaptCompactionChoice{
		Preset: "safe", DropOverrides: []string{"format"}, ConfigHash: dash.AdaptConfigHash(stored)}})
	if w, _ := f.do(t, "POST", "/api/adapt/compaction/apply", `{"option":"safe-without-overrides","basis":"`+basis+`"}`, jar); w.Code != http.StatusOK {
		t.Fatalf("apply = %d %s", w.Code, w.Body)
	}
	_, doc := storedForm(t, f, jar)
	want, _, _ := dash.AdaptPresetDocWith(stored, "safe", true)
	if doc != want || strings.Contains(doc, "999999") {
		t.Fatalf("stored %q, priced %q", doc, want)
	}
}

// On a single-tenant proxy there is no registry to announce a configuration change, so a local apply or
// revert has to drop the dashboard's cached reads itself: the removal list in the running pipeline just
// changed, and /api/toolfilter may otherwise answer from a body up to ten minutes old.
func TestAdaptApplyLocalDropsTheDashboardsCachedReads(t *testing.T) {
	h := localAdaptHandler(t, filepath.Join(t.TempDir(), "d.db.adapt.json"))
	mux := h.Mux()
	get := func() string {
		r := httptest.NewRequest("GET", "/api/toolfilter", nil)
		r.RemoteAddr = "127.0.0.1:4000"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w.Header().Get("X-Cache")
	}
	get() // computes and caches
	if got := get(); got != "hit" {
		t.Fatalf("second read X-Cache = %q, want hit; the test needs a warm cache to prove anything", got)
	}
	basis := offer(h, "", "preset: house\n", []string{"format", "searchfold", "textclean"}, nil)
	r := httptest.NewRequest("POST", "/api/adapt/compaction/apply", strings.NewReader(`{"option":"c1","basis":"`+basis+`"}`))
	r.RemoteAddr = "127.0.0.1:4000"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("apply = %d %s", w.Code, w.Body)
	}
	if got := get(); got == "hit" || got == "stale" {
		t.Errorf("read right after a local apply X-Cache = %q; a cached body from before the apply was served", got)
	}
}
