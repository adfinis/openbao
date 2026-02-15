package main

import (
	"context"
	"fmt"
)

// OpWeight pairs an operation name with its percentage weight.
type OpWeight struct {
	Name   string
	Weight int // percent [0, 100]
}

// Workload is the interface for pluggable workload types.
// Each workload defines its operations and how to execute them.
type Workload interface {
	// Name returns the workload identifier (e.g. "kv", "transit", "pki").
	Name() string

	// Ops returns the weighted operation list.  Weights must sum to 100.
	Ops() []OpWeight

	// Execute runs a single operation and returns an Event for recording.
	// The worker passes its context (for deadline), the operation name
	// chosen by weighted selection, and itself (for key selection, payload
	// generation, seq numbers, and client access).
	Execute(ctx context.Context, op string, w *Worker) Event
}

// WorkloadFactory creates a Workload from config.
type WorkloadFactory func(cfg *Config, clients *ClientSet) (Workload, error)

// registry holds registered workload factories.
var registry = map[string]WorkloadFactory{}

// RegisterWorkload adds a factory to the global registry.
func RegisterWorkload(name string, factory WorkloadFactory) {
	registry[name] = factory
}

// NewWorkload creates a Workload by name from the registry.
func NewWorkload(name string, cfg *Config, clients *ClientSet) (Workload, error) {
	factory, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown workload %q (registered: %v)", name, registeredNames())
	}
	return factory(cfg, clients)
}

func registeredNames() []string {
	names := make([]string, 0, len(registry))
	for k := range registry {
		names = append(names, k)
	}
	return names
}

// ClientSet holds the API clients for each configured node role.
type ClientSet struct {
	Primary    *BaoClient
	Secondary1 *BaoClient // nil if not configured
	Secondary2 *BaoClient // nil if not configured
}
