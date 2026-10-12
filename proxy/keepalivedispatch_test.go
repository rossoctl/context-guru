package proxy

import (
	"net/http"
	"testing"

	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/keepalive/keepalivetest"
)

func TestHTTPDispatcherMeetsTheContract(t *testing.T) {
	keepalivetest.RunDispatcherContract(t, func(t *testing.T, upstreamURL string) keepalivetest.Harness {
		d := newHTTPDispatcher(http.DefaultClient)
		return keepalivetest.Harness{D: d, Hold: func(k keepalive.Key, hdr http.Header) {
			r, _ := http.NewRequest(http.MethodPost, "http://proxy.local/v1/messages", nil)
			r.Header = hdr
			d.hold(k, upstream{base: upstreamURL, path: "/v1/messages"}, r)
		}}
	})
}
