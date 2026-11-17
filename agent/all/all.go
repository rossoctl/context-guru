// Package all blank-imports every built-in agent so their init() registrations run. A host (the
// proxy, a sidecar plugin) imports this package for its side effects, then asks the agent package
// for the agents. Import it for effect:
//
//	import _ "github.com/rossoctl/context-guru/agent/all"
package all

import (
	_ "github.com/rossoctl/context-guru/agent/bob"
)
