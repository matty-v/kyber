package adapters

import (
	"context"
	"strings"
	"testing"
)

func TestNewComputeAdapterMock(t *testing.T) {
	adapter, err := NewComputeAdapter(context.Background(), "mock", nil)
	if err != nil {
		t.Fatalf("NewComputeAdapter: %v", err)
	}
	if adapter.Type() != "mock" {
		t.Errorf("Type() = %q, want mock", adapter.Type())
	}
}

func TestNewComputeAdapterUnknown(t *testing.T) {
	_, err := NewComputeAdapter(context.Background(), "missing", nil)
	if err == nil {
		t.Fatal("NewComputeAdapter: expected error")
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("error = %q, want registered-provider context", err)
	}
}

func TestRegisteredComputeProviders(t *testing.T) {
	got := RegisteredComputeProviders()
	want := []string{"eks", "fake", "gce", "gke", "mock", "static"}
	if len(got) != len(want) {
		t.Fatalf("RegisteredComputeProviders() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("RegisteredComputeProviders() = %v, want %v", got, want)
			break
		}
	}
}
