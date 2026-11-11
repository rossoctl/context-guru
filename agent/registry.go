package agent

import (
	"errors"
	"fmt"
	"sync"
)

// Registry maps a route path to the agent that owns it. It is safe for concurrent use.
type Registry struct {
	mu     sync.RWMutex
	byPath map[string]Agent
}

func NewRegistry() *Registry { return &Registry{byPath: map[string]Agent{}} }

// Register adds a, with every path it claims. A path that is empty, repeated, or claimed by an
// agent already registered is refused, and then a claims no path at all.
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
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range paths {
		if old, ok := r.byPath[p]; ok {
			return fmt.Errorf("agent: path %s is claimed by %T and %s", p, old, name)
		}
	}
	for _, p := range paths {
		r.byPath[p] = a
	}
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

// ForPath looks path up in the default registry.
func ForPath(path string) Agent { return defaultRegistry.ForPath(path) }

// resetDefault empties the default registry, for tests. It clears the map under the lock and
// keeps the variable: a swap would race with any ForPath or Register still running.
func resetDefault() {
	defaultRegistry.mu.Lock()
	defer defaultRegistry.mu.Unlock()
	defaultRegistry.byPath = map[string]Agent{}
}
