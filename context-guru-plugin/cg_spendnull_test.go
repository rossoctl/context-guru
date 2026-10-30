package plugin

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// #781: an older proxy sends the spend lists as null when they are empty. cg.py must read a null
// list as empty, so insights details and insights cache details exit 0 instead of crashing.
func TestCGInsightsDetailsReadsNullListsAsEmpty(t *testing.T) {
	spend := `{"window":{"since":0,"until":0},"requests":3,"priced_requests":3,"unpriced_requests":0,` +
		`"usd":{"total":1.5,"input":0.5,"output":0.5,"cache_read":0.25,"cache_write":0.25},"stored_cost_usd":1.5,` +
		`"rewrites":{"ttl_expiry":{"n":1,"tokens":40000,"usd":0.4}},` +
		`"idle_gaps":null,"top_rewrites":null,"models":null,"sessions":null,"daily":null,"session_days":null,"live":null,` +
		`"rates":{},"ledger":{"gross_saved_usd":0,"cg_llm_usd":0,"ping_usd":0,"ping_saved_usd":0}}`
	mux := http.NewServeMux()
	mux.HandleFunc("/api/spend/explain", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(spend)) }) //nolint:errcheck
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{}`)) })                   //nolint:errcheck
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	base := "http://127.0.0.1:" + fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
	for _, args := range [][]string{{"insights", "details"}, {"insights", "cache", "details"}} {
		out, code := runCGCode(t, base, []string{"XDG_CACHE_HOME=" + t.TempDir()}, args...)
		if code != 0 || strings.Contains(out, "Traceback") {
			t.Errorf("cg.py %v: exit %d, want 0\n%s", args, code, out)
		}
	}
}
