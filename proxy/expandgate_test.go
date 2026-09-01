// The expand-tool advertising gate.
package proxy_test

import (
	"encoding/json"
	"fmt"
	"github.com/tidwall/gjson"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestExpandToolIsAdvertisedOnlyWhereMarkersCanExist is the test that should have existed when the `cache`
// preset shipped, and did not.
//
// The preset's promise — repeated in `config/config.go`, `docs/reference/presets.md`,
// `docs/how-to/choose-a-preset.md`, `docs/how-to/install-plugin.md` and the install skill — is
// that nothing is added to the user's requests. It was false: `expand.Inject` under `auto` gated
// only on "the request declares tools" and "the store persists", so a cachesplit-only pipeline
// forwarded `context_guru_expand` to the provider. Measured before the fix:
//
//	tools SENT by client    : [Read Bash]
//	tools FORWARDED upstream: [Read Bash context_guru_expand]
//
// That is not cosmetic. A pipeline with no Offload mints no markers, so EVERY expand call against
// it must fail — on a real session with marker-shaped text in a file, the model called it
// unprompted and got "[expand: original for id ... is no longer available]", costing a round trip
// and a step of the user's turn.
//
// TestCachePresetIsCachesplitAlone could not catch this: it asserts the presets MAP, while the
// injection happens in proxy.go after apply returns. This one reads the bytes that leave the
// process, which is the only place the claim is actually true or false.
func TestExpandToolIsAdvertisedOnlyWhereMarkersCanExist(t *testing.T) {
	for _, c := range []struct {
		name     string
		pipeline string
		wantAdd  bool
	}{
		// A cache-only pipeline: no offloader, so no marker can ever exist.
		{"cachesplit only", "pipeline: [cachesplit]\n", false},
		// `safe` is a shipped preset with no Offload — lossless folds and the cache split only —
		// and it was affected too.
		{"safe", "pipeline: [format, textclean, searchfold, cachesplit]\n", false},
		// `mcp` looks similar and is NOT affected, which is the distinction the gate has to get
		// right: `smartcrush` implements components.Offload (components/offload/smartcrush.go),
		// so that pipeline really can mint a marker and really does need the tool. I had this
		// case the wrong way round at first and the test caught it — which is the argument for
		// asking the interface rather than keeping a hand-written list of "lossy" component names.
		{"mcp (smartcrush IS an Offload)", "pipeline: [format, textclean, smartcrush, cachesplit]\n", true},
		// `off` is the A/B control arm. A control that carries an extra tool declaration is not
		// a control — and this was broken too.
		{"off (the A/B control)", "pipeline: []\n", false},
		// The feature must still work where markers can exist, or the fix has traded one silent
		// defect for another: an offloader whose output nothing can expand.
		{"a pipeline with an offloader", "pipeline: [linecap, cachesplit]\n", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var forwarded []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				forwarded, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"type":"message","usage":{"input_tokens":1}}`)
			}))
			defer upstream.Close()

			h, _ := buildHandler(t, c.pipeline, upstream.URL)
			srv := httptest.NewServer(h.Mux())
			defer srv.Close()

			// Tools declared, which is what `auto` keys on, plus a long tool output so the
			// offloader case has something to act on.
			body := []byte(`{"model":"claude-sonnet-5","max_tokens":64,` +
				`"tools":[{"name":"Read","input_schema":{"type":"object"}},` +
				`{"name":"Bash","input_schema":{"type":"object"}}],` +
				`"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"a",` +
				`"content":` + jsonStr(strings.Repeat("output line that is long enough to offload\n", 400)) +
				`}]}]}`)
			if !json.Valid(body) {
				t.Fatalf("fixture is not valid JSON")
			}

			resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json",
				strings.NewReader(string(body)))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if len(forwarded) == 0 {
				t.Fatal("upstream received nothing")
			}

			var sent, got []string
			for _, v := range gjson.GetBytes(body, "tools").Array() {
				sent = append(sent, v.Get("name").String())
			}
			for _, v := range gjson.GetBytes(forwarded, "tools").Array() {
				got = append(got, v.Get("name").String())
			}
			has := false
			for _, n := range got {
				if n == "context_guru_expand" {
					has = true
				}
			}
			if has != c.wantAdd {
				if c.wantAdd {
					t.Fatalf("pipeline %q mints markers but no longer advertises the expand tool, "+
						"so a model cannot recover what it offloaded: sent %v, forwarded %v",
						c.pipeline, sent, got)
				}
				t.Fatalf("pipeline %q has no offloader, so it mints no markers and every expand "+
					"call against it must fail — but it advertises the tool anyway: sent %v, "+
					"forwarded %v", c.pipeline, sent, got)
			}
			// The client's own tools must keep their exact identity and order either way: the
			// tools array has to be byte-stable across a session or every turn is a full
			// prefix-cache miss.
			for i, n := range sent {
				if got[i] != n {
					t.Errorf("client tool %d changed: %q -> %q (full: %v)", i, n, got[i], got)
				}
			}
		})
	}
}
