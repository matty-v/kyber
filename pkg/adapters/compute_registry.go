package adapters

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// ProviderConfig carries provider-scoped configuration into an adapter
// factory. Keys are interpreted only by the selected provider.
type ProviderConfig map[string]string

// ComputeFactory constructs one configured compute adapter.
type ComputeFactory func(context.Context, ProviderConfig) (ComputeAdapter, error)

var (
	computeFactoriesMu sync.RWMutex
	computeFactories   = map[string]ComputeFactory{}
)

// RegisterComputeProvider registers a provider factory. Duplicate or invalid
// registration is a programmer error and panics during process startup.
func RegisterComputeProvider(providerType string, factory ComputeFactory) {
	if providerType == "" {
		panic("register compute provider: empty type")
	}
	if factory == nil {
		panic("register compute provider " + providerType + ": nil factory")
	}

	computeFactoriesMu.Lock()
	defer computeFactoriesMu.Unlock()
	if _, exists := computeFactories[providerType]; exists {
		panic("register compute provider " + providerType + ": duplicate type")
	}
	computeFactories[providerType] = factory
}

// NewComputeAdapter constructs the selected provider. It never substitutes a
// different provider when configuration or initialization fails.
func NewComputeAdapter(ctx context.Context, providerType string, config ProviderConfig) (ComputeAdapter, error) {
	computeFactoriesMu.RLock()
	factory, ok := computeFactories[providerType]
	computeFactoriesMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("compute provider %q is not registered (available: %v)", providerType, RegisteredComputeProviders())
	}

	adapter, err := factory(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("initializing compute provider %q: %w", providerType, err)
	}
	if adapter == nil {
		return nil, fmt.Errorf("initializing compute provider %q: factory returned nil adapter", providerType)
	}
	if adapter.Type() != providerType {
		return nil, fmt.Errorf("initializing compute provider %q: adapter reports type %q", providerType, adapter.Type())
	}
	return adapter, nil
}

// RegisteredComputeProviders returns provider types in stable order.
func RegisteredComputeProviders() []string {
	computeFactoriesMu.RLock()
	defer computeFactoriesMu.RUnlock()
	out := make([]string, 0, len(computeFactories))
	for providerType := range computeFactories {
		out = append(out, providerType)
	}
	sort.Strings(out)
	return out
}
