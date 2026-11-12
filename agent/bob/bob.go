// Package bob is the agent for IBM Bob: the Bob IDE and Bob Shell. Its init() registers it in the
// agent package's default registry, so a host needs only a blank import:
//
//	import _ "github.com/rossoctl/context-guru/agent/bob"
//
// This is a library package. It never imports a host. imports_test.go enforces that.
package bob

import (
	"net/http"

	"github.com/rossoctl/context-guru/agent"
)

// ModelPath is Bob's model route. It is the only route Bob owns.
const ModelPath = "/inference/v1/chat/completions"

// Bob overrides only where it differs from the neutral agent.
type Bob struct{ agent.Base }

// A typo in an override would otherwise compile and fall through to Base.
var _ agent.Agent = Bob{}

func init() { agent.Register(Bob{}) }

// Name is the label the dashboard already gives Bob.
func (Bob) Name() string { return "bob" }

func (Bob) Paths() []string { return []string{ModelPath} }

// SessionID is the Bob IDE's task id. The IDE sends it only in the X-Task-Id header, not in the
// body. Bob Shell sends metadata.taskId in the body, which the host reads before this.
func (Bob) SessionID(h http.Header) string { return h.Get("X-Task-Id") }

// ModelTarget sends the host's own calls to Bob's model path: Bob serves chat on no other path.
// Bob takes the key as `Authorization: Apikey <key>`, the way the Bob client sends it.
func (Bob) ModelTarget() agent.ModelTarget {
	return agent.ModelTarget{Path: ModelPath, AuthScheme: "Apikey"}
}
