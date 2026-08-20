package adapters

// compile-time interface assertion: GCEAdapter must implement ComputeAdapter.
// This is the only test in this file — no live GCE tests run in CI.
// Real GCE integration tests require a GCP project with ADC configured.
var _ ComputeAdapter = (*GCEAdapter)(nil)
var _ CapacityProvider = (*GCEAdapter)(nil)
