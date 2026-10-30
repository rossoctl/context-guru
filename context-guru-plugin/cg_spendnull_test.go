package plugin

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// #781: an older proxy sends empty lists as null (Go encodes a nil slice that way). cg.py must read
// a null list as empty, on every endpoint the insights commands read, so each command exits 0.
func TestCGInsightsReadNullListsAsEmpty(t *testing.T) {
	// top_rewrites is NOT empty here: the cache details table must also print a row (it read
	// o["tz"], which cg.py never set: a KeyError for every dashboard with a real rewrite).
	spend := `{"window":{"since":0,"until":0},"requests":3,"priced_requests":0,"unpriced_requests":3,` +
		`"usd":{"total":0,"input":0,"output":0,"cache_read":0,"cache_write":0},"stored_cost_usd":0,` +
		`"rewrites":{"ttl_expiry":{"n":1,"tokens":40000,"usd":0.4}},` +
		`"idle_gaps":null,"models":null,"sessions":null,"daily":null,"session_days":null,"live":null,` +
		`"top_rewrites":[{"ts":1793000000000,"session":"s1","model":"claude-sonnet-5-5","context":40000,"gap_s":420,"cause":"ttl_expiry","usd":0.4}],` +
		`"rates":{},"ledger":{"gross_saved_usd":0,"cg_llm_usd":0,"ping_usd":0,"ping_saved_usd":0}}`
	// The shape /api/tools sends for a window with no tool-bearing session: null lists, a truthy totals.
	tools := `{"tools":null,"servers":null,"aside":null,"models":null,"skills":{"state":"absent","unknown_sessions":0,"skills":null},` +
		`"totals":{"tools":0,"tokens":0,"unused_usd":0,"sessions":0}}`
	mux := http.NewServeMux()
	mux.HandleFunc("/api/spend/explain", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(spend)) })              //nolint:errcheck
	mux.HandleFunc("/api/tools", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(tools)) })                      //nolint:errcheck
	mux.HandleFunc("/api/tools/last-used", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"tools":null}`)) }) //nolint:errcheck
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{}`)) })                                //nolint:errcheck
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	base := "http://127.0.0.1:" + fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
	for _, args := range [][]string{
		{"insights"}, {"insights", "details"},
		{"insights", "cache"}, {"insights", "cache", "details"},
		{"insights", "tools"}, {"insights", "tools", "details"},
	} {
		out, code := runCGCode(t, base, []string{"XDG_CACHE_HOME=" + t.TempDir()}, args...)
		if code != 0 || strings.Contains(out, "Traceback") {
			t.Errorf("cg.py %v: exit %d, want 0\n%s", args, code, out)
		}
	}
}
