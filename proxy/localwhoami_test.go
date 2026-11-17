package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rossoctl/context-guru/store"
)

// The local proxy's /api/whoami names the preset it runs now, so the Overview can show the live
// preset and not only the one the window's requests ran under.
func TestLocalWhoamiNamesTheRunningPreset(t *testing.T) {
	h := New(nil, store.NewMemory(store.Options{}), nil, Options{Preset: "ultrahigh"})
	out, ok := h.LocalWhoami()(httptest.NewRequest(http.MethodGet, "/api/whoami", nil)).(map[string]any)
	if !ok {
		t.Fatalf("whoami did not return an object")
	}
	if out["preset"] != "ultrahigh" || out["hosted"] != false || out["authenticated"] != false {
		t.Errorf("whoami = %v, want preset ultrahigh, not hosted, not signed in", out)
	}
}

// No preset configured: the field is absent, and the page falls back to the window's history.
func TestLocalWhoamiOmitsAnEmptyPreset(t *testing.T) {
	h := New(nil, store.NewMemory(store.Options{}), nil, Options{})
	out := h.LocalWhoami()(httptest.NewRequest(http.MethodGet, "/api/whoami", nil)).(map[string]any)
	if _, ok := out["preset"]; ok {
		t.Errorf("whoami = %v, want no preset field", out)
	}
}
