package dash

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The dashboard's assets are ~705 KB of JS and ~91 KB of CSS. They were served raw to
// every client on every cold load; over a WAN that is the whole story of the load time.
// What this guards is not the compression but the NEGOTIATION: a client that did not ask
// for gzip must not be handed a gzip body, and the two representations must not share one
// validator, or a cache revalidating with the wrong one serves the wrong body.
func TestUIAssetsNegotiateGzip(t *testing.T) {
	a, _ := newTestAPI(t, Options{})
	m := http.NewServeMux()
	a.Mount(m)

	get := func(ae string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/dashboard/app.js", nil)
		if ae != "" {
			req.Header.Set("Accept-Encoding", ae)
		}
		w := httptest.NewRecorder()
		m.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("Accept-Encoding %q -> %d", ae, w.Code)
		}
		return w
	}

	raw := get("")
	if enc := raw.Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("a client that offered no encoding got Content-Encoding %q", enc)
	}

	gz := get("gzip, deflate, br")
	if enc := gz.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q; want gzip", enc)
	}
	if gz.Body.Len() >= raw.Body.Len() {
		t.Errorf("gzip body is %d bytes against %d raw — no compression happened", gz.Body.Len(), raw.Body.Len())
	}
	// Compressed or not, it has to be the same script.
	zr, err := gzip.NewReader(gz.Body)
	if err != nil {
		t.Fatalf("gzip body does not decompress: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("reading the gzip body: %v", err)
	}
	if string(got) != raw.Body.String() {
		t.Errorf("the decompressed body differs from the raw one (%d vs %d bytes)", len(got), raw.Body.Len())
	}

	// Two representations, two validators. With one shared ETag a shared cache holding the
	// gzip copy can answer a raw client's revalidation with 304 and hand it the compressed
	// bytes, which is a broken page rather than a slow one.
	if a, b := raw.Header().Get("ETag"), gz.Header().Get("ETag"); a == "" || a == b {
		t.Errorf("raw and gzip share the ETag %q — a cache cannot tell the representations apart", a)
	}
	for _, w := range []*httptest.ResponseRecorder{raw, gz} {
		if v := w.Header().Get("Vary"); v != "Accept-Encoding" {
			t.Errorf("Vary = %q; want Accept-Encoding, or a cache keys both representations together", v)
		}
	}

	// An explicit refusal is a refusal. `gzip;q=0` means "not gzip", not "gzip is fine".
	if enc := get("gzip;q=0, identity").Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("gzip;q=0 was served Content-Encoding %q", enc)
	}
}
