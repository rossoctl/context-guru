package agent

import (
	"net/http"
	"strings"
)

// HeaderRequestKind is our own header. A client plugin (OpenCode, Pi) sets it on the client's own
// compaction request, with the value RequestKindCompaction. A host strips every x-context-guru-*
// header before it forwards a request.
const (
	HeaderRequestKind     = "x-context-guru-request-kind"
	RequestKindCompaction = "compaction"
)

// PluginMarksCompaction reports whether a context-guru client plugin marked this request as the
// client's own compaction. It reads one header and no request text. A nil h gives false.
func PluginMarksCompaction(h http.Header) bool {
	return h != nil && strings.EqualFold(strings.TrimSpace(h.Get(HeaderRequestKind)), RequestKindCompaction)
}
