package dispatchtest

import (
	"net/http"
	"testing"

	"github.com/rossoctl/context-guru/dispatch"
)

func TestHTTPHostMeetsTheContract(t *testing.T) {
	Contract(t, func(t *testing.T, upstreamURL string) Harness {
		h := NewHTTPHost()
		return Harness{D: h, Hold: func(hdr http.Header) dispatch.Ref { return h.Pin(upstreamURL, hdr) }}
	})
}
