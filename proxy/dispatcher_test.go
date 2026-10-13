package proxy

import (
	"net/http"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/dispatch/dispatchtest"
	"github.com/rossoctl/context-guru/keepalive"
)

func TestProxyDispatcherMeetsTheContract(t *testing.T) {
	dispatchtest.Contract(t, func(t *testing.T, upstreamURL string) dispatchtest.Harness {
		d := NewDispatcher(nil)
		return dispatchtest.Harness{D: d, Hold: func(h http.Header) dispatch.Ref {
			return d.hold(keepalive.Key{}, upstream{base: upstreamURL, path: "/v1/messages"}, h)
		}}
	})
}

// Every path that drops a keep-alive entry releases its credential; Shutdown drops them all. After
// it, the proxy must hold no caller credential at all.
func TestShutdownReleasesEveryHeldRef(t *testing.T) {
	d := NewDispatcher(nil)
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	k := keepalive.New(keepalive.Options{Dispatcher: d, Now: func() time.Time { return now },
		Go: func(f func()) { f() }})
	hdr := http.Header{"Authorization": {"Bearer sk-caller"}}
	up := upstream{base: "http://up", path: "/v1/messages"}
	for i, s := range []string{"s1", "s1", "s2", "s2"} { // a session's FIRST request is not tracked
		key := keepalive.Key{Tenant: "t1", Session: s}
		k.Record(keepalive.Record{Key: key, StartedAt: now.Add(time.Duration(i) * time.Second),
			Body: []byte(kaBody), Provider: bschemas.Anthropic, Endpoint: up.base, Route: up.path,
			Status: http.StatusOK, Usage: keepalive.Usage{CacheRead: 48576}, UsageOK: true,
			Policy: kaPolicy(), Header: hdr,
			Hold: func() dispatch.Ref { return d.hold(key, up, hdr) }})
	}
	if n := d.heldCount(); n != 2 {
		t.Fatalf("held %d credentials for two tracked sessions, want 2", n)
	}
	k.Shutdown()
	if n := d.heldCount(); n != 0 {
		t.Fatalf("after Shutdown the proxy still holds %d credentials", n)
	}
}

// The keeper parses a ping's usage itself (keepalive.PingUsage). It must read a ping response
// exactly as the proxy's own parser reads it, or a ping's cost and its write-vs-read guard would
// change with the host.
func TestPingUsageMatchesResponseUsage(t *testing.T) {
	for name, body := range map[string]string{
		"anthropic read": `{"stop_reason":"max_tokens","usage":{"input_tokens":3,"cache_read_input_tokens":48576,` +
			`"cache_creation_input_tokens":0,"output_tokens":1}}`,
		"anthropic write 1h": `{"stop_reason":"max_tokens","usage":{"input_tokens":3,"cache_read_input_tokens":0,` +
			`"cache_creation_input_tokens":900,"cache_creation":{"ephemeral_1h_input_tokens":900},"output_tokens":1}}`,
		"responses": `{"usage":{"input_tokens":48003,"input_tokens_details":{"cached_tokens":48000},` +
			`"output_tokens":16}}`,
	} {
		t.Run(name, func(t *testing.T) {
			want, ok := responseUsage("application/json", []byte(body))
			if !ok {
				t.Fatal("responseUsage did not parse the fixture")
			}
			got := keepalive.PingUsage([]byte(body))
			if got.FreshInput != want.FreshInput || got.CacheRead != want.CacheRead ||
				got.CacheWrite != want.CacheWrite || got.CacheWrite1h != want.CacheWrite1h ||
				got.Output != want.Output {
				t.Fatalf("PingUsage = %+v, responseUsage = %+v", got, want)
			}
		})
	}
}
