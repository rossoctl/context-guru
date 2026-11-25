// Package pi holds Pi's mark. Pi calls the model through a provider SDK, so its User-Agent is the
// SDK's. The Pi plugin sets ClientHeader on each routed request instead. agent/client calls Is.
package pi

import (
	"net/http"
	"strings"
)

// ClientHeader is the header the Pi plugin sets to Value. A host never forwards it upstream (the
// proxy drops every x-context-guru-* header).
const ClientHeader = "x-context-guru-client"

// Value is what the Pi plugin sends in ClientHeader.
const Value = "pi"

// Is reports whether h carries Pi's mark.
func Is(h http.Header) bool {
	return strings.EqualFold(strings.TrimSpace(h.Get(ClientHeader)), Value)
}
