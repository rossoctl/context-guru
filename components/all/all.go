// Package all blank-imports every built-in component so their init()
// registrations run. Binaries (context-guru-proxy, a sidecar plugin)
// import this package for its side effects, then build a pipeline by name from
// config. Import it for effect:
//
//	import _ "github.com/rossoctl/context-guru/components/all"
package all

import (
	_ "github.com/rossoctl/context-guru/components/offload"
	_ "github.com/rossoctl/context-guru/components/reformat"
)
