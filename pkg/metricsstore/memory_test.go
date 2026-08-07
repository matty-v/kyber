package metricsstore_test

import (
	"context"
	"testing"

	"github.com/matty-v/kyber/pkg/metricsstore"
)

func TestMemoryMetricsStore_AddAndRangeQuery(t *testing.T) {
	ctx := context.Background()
	store := metricsstore.NewMemoryMetricsStore()

	if err := store.AddPoint(ctx, "ts:activity:ns:han:working", 100, 0.5); err != nil {
		t.Fatalf("AddPoint: %v", err)
	}
	if err := store.AddPoint(ctx, "ts:activity:ns:han:working", 200, 0.8); err != nil {
		t.Fatalf("AddPoint: %v", err)
	}
	if err := store.AddPoint(ctx, "ts:activity:ns:han:working", 300, 0.2); err != nil {
		t.Fatalf("AddPoint: %v", err)
	}

	pts, err := store.RangeQuery(ctx, "ts:activity:ns:han:working", 100, 200)
	if err != nil {
		t.Fatalf("RangeQuery: %v", err)
	}
	if len(pts) != 2 {
		t.Fatalf("want 2 points, got %d", len(pts))
	}
	if pts[0].Timestamp != 100 || pts[0].Value != 0.5 {
		t.Errorf("pts[0] = %+v, want {100, 0.5}", pts[0])
	}
	if pts[1].Timestamp != 200 || pts[1].Value != 0.8 {
		t.Errorf("pts[1] = %+v, want {200, 0.8}", pts[1])
	}
}

func TestMemoryMetricsStore_RangeQueryEmpty(t *testing.T) {
	ctx := context.Background()
	store := metricsstore.NewMemoryMetricsStore()
	pts, err := store.RangeQuery(ctx, "ts:activity:ns:han:working", 0, 999)
	if err != nil {
		t.Fatalf("RangeQuery: %v", err)
	}
	if len(pts) != 0 {
		t.Errorf("want 0 points, got %d", len(pts))
	}
}

func TestMemoryMetricsStore_EvictBefore(t *testing.T) {
	ctx := context.Background()
	store := metricsstore.NewMemoryMetricsStore()

	_ = store.AddPoint(ctx, "ts:activity:ns:han:working", 100, 1.0)
	_ = store.AddPoint(ctx, "ts:activity:ns:han:working", 200, 2.0)
	_ = store.AddPoint(ctx, "ts:activity:ns:han:working", 300, 3.0)

	if err := store.EvictBefore(ctx, 200); err != nil {
		t.Fatalf("EvictBefore: %v", err)
	}

	pts, _ := store.RangeQuery(ctx, "ts:activity:ns:han:working", 0, 9999)
	if len(pts) != 2 {
		t.Fatalf("want 2 points after eviction, got %d", len(pts))
	}
	if pts[0].Timestamp != 200 {
		t.Errorf("expected first remaining point at ts=200, got %d", pts[0].Timestamp)
	}
}

func TestMemoryNodeStore_PutAndGetAll(t *testing.T) {
	ctx := context.Background()
	store := metricsstore.NewMemoryNodeStore()

	sample := metricsstore.NodeSample{
		CPUPercent:     42.5,
		MemUsedBytes:   1 << 30,
		MemTotalBytes:  8 << 30,
		DiskUsedBytes:  5 << 30,
		DiskTotalBytes: 100 << 30,
		UpdatedAt:      "2026-05-25T00:00:00Z",
	}
	if err := store.PutNode(ctx, "kyber-system", "node-1", sample); err != nil {
		t.Fatalf("PutNode: %v", err)
	}

	samples, err := store.GetAllNodes(ctx, "kyber-system")
	if err != nil {
		t.Fatalf("GetAllNodes: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("want 1 sample, got %d", len(samples))
	}
	got := samples[0]
	if got.Node != "node-1" {
		t.Errorf("Node = %q, want %q", got.Node, "node-1")
	}
	if got.CPUPercent != 42.5 {
		t.Errorf("CPUPercent = %f, want 42.5", got.CPUPercent)
	}
	if got.MemUsedBytes != float64(1<<30) {
		t.Errorf("MemUsedBytes = %f, want %d", got.MemUsedBytes, 1<<30)
	}
}

func TestMemoryNodeStore_GetAllNodesEmpty(t *testing.T) {
	ctx := context.Background()
	store := metricsstore.NewMemoryNodeStore()
	samples, err := store.GetAllNodes(ctx, "kyber-system")
	if err != nil {
		t.Fatalf("GetAllNodes: %v", err)
	}
	if len(samples) != 0 {
		t.Errorf("want 0 samples, got %d", len(samples))
	}
}

func TestMemoryNodeStore_OverwritesLatest(t *testing.T) {
	ctx := context.Background()
	store := metricsstore.NewMemoryNodeStore()

	_ = store.PutNode(ctx, "ns", "node-1", metricsstore.NodeSample{CPUPercent: 10.0})
	_ = store.PutNode(ctx, "ns", "node-1", metricsstore.NodeSample{CPUPercent: 99.0})

	samples, _ := store.GetAllNodes(ctx, "ns")
	if len(samples) != 1 {
		t.Fatalf("want 1 sample after overwrite, got %d", len(samples))
	}
	if samples[0].CPUPercent != 99.0 {
		t.Errorf("CPUPercent = %f, want 99.0 (latest)", samples[0].CPUPercent)
	}
}
