package accrec

import (
	"github.com/rossoctl/context-guru/accwire"
	"github.com/rossoctl/context-guru/components"
)

// MetricComponents is the component allow-list for an accountant metrics sink
// (acmetrics.WithComponents): every registered component and every operator outside the registry
// (accwire.OperatorsOutsideRegistry). The sink pushes a name outside the list as component="other",
// so with this list each operator context-guru books keeps its own `component` label, and the
// central dashboard can split the net saving by it.
func MetricComponents() []string {
	return append(components.Names(), accwire.OperatorsOutsideRegistry()...)
}
