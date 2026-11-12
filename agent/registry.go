package agent

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// Registry maps a route path to the agent that owns it. It is safe for concurrent use.
type Registry struct {
	mu     sync.RWMutex
	byPath map[string]Agent
	all    []Agent // each registered agent once, in Register order
	// catchAll is the one Gateway whose GatewaySpec.CatchAll is true, or nil.
	catchAll Agent
	// gateways holds each registered Gateway's Name: the host keys an upstream by it.
	gateways map[string]bool
}

func NewRegistry() *Registry {
	return &Registry{byPath: map[string]Agent{}, gateways: map[string]bool{}}
}

// Register adds a, with every path it claims. A path that is empty, repeated, or claimed by an
// agent already registered is refused, and then a claims no path at all. A Gateway with an empty
// Name, a second Gateway with the same Name, and a second CatchAll Gateway, are refused the same way.
func (r *Registry) Register(a Agent) error {
	if a == nil {
		return errors.New("agent: Register with nil agent")
	}
	// The agent's own methods run before the lock: one that called back into the registry would
	// otherwise deadlock. %T names an agent whose Name is empty.
	paths, name := a.Paths(), fmt.Sprintf("%q (%T)", a.Name(), a)
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		if p == "" {
			return fmt.Errorf("agent: %s claims an empty path", name)
		}
		if seen[p] {
			return fmt.Errorf("agent: %s claims path %s twice", name, p)
		}
		seen[p] = true
	}
	var catchAll, isGateway bool
	gwName := a.Name()
	if g, ok := a.(Gateway); ok {
		isGateway = true
		// The host keys a Gateway's upstream by its Name.
		if gwName == "" {
			return fmt.Errorf("agent: %s is a Gateway with an empty Name", name)
		}
		catchAll = g.Gateway().CatchAll
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range paths {
		if old, ok := r.byPath[p]; ok {
			return fmt.Errorf("agent: path %s is claimed by %T and %s", p, old, name)
		}
	}
	if isGateway && r.gateways[gwName] {
		return fmt.Errorf("agent: two Gateways are named %q; the host keys an upstream by the Name", gwName)
	}
	if catchAll && r.catchAll != nil {
		return fmt.Errorf("agent: the catch-all is claimed by %T and %s", r.catchAll, name)
	}
	for _, p := range paths {
		r.byPath[p] = a
	}
	if catchAll {
		r.catchAll = a
	}
	if isGateway {
		r.gateways[gwName] = true
	}
	r.all = append(r.all, a)
	return nil
}

// ForPath is the agent that owns path, or Base when no agent claims it.
func (r *Registry) ForPath(path string) Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if a, ok := r.byPath[path]; ok {
		return a
	}
	return Base{}
}

// All is every registered agent, each once, sorted by Name. The slice is the caller's own copy.
func (r *Registry) All() []Agent {
	r.mu.RLock()
	out := slices.Clone(r.all)
	r.mu.RUnlock()
	// Sorted after the lock: Name is the agent's own method (see Register).
	slices.SortStableFunc(out, func(a, b Agent) int { return strings.Compare(a.Name(), b.Name()) })
	return out
}

// defaultRegistry is where each agent package registers from its init(), as components do. A
// host then needs only a blank import of the agent's package. Tests that need isolation use
// NewRegistry.
var defaultRegistry = NewRegistry()

// Register adds a to the default registry. It panics on a refusal: that is a wiring error in an
// init(), found at start-up (the components.Register pattern).
func Register(a Agent) {
	if err := defaultRegistry.Register(a); err != nil {
		panic(err)
	}
}

// All is every agent in the default registry. See (*Registry).All.
func All() []Agent { return defaultRegistry.All() }

// ForPath looks path up in the default registry.
func ForPath(path string) Agent { return defaultRegistry.ForPath(path) }

// resetDefault empties the default registry, for tests. It clears the registry under the lock and
// keeps the variable: a swap would race with any ForPath or Register still running.
func resetDefault() {
	defaultRegistry.mu.Lock()
	defer defaultRegistry.mu.Unlock()
	defaultRegistry.byPath = map[string]Agent{}
	defaultRegistry.all, defaultRegistry.catchAll, defaultRegistry.gateways = nil, nil, map[string]bool{}
}
