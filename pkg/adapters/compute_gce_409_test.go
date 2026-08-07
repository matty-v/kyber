package adapters

// Unit tests for GCEAdapter.CreateInstance 409-handling logic.
// These tests use a fake gceInstancesClient — no network calls.

import (
	"context"
	"fmt"
	"testing"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
)

// fakeOp is a no-op gceOperation that always succeeds.
type fakeOp struct{ err error }

func (f *fakeOp) Wait(_ context.Context, _ ...gax.CallOption) error { return f.err }

// fakeGCEClient is a scriptable fake for gceInstancesClient.
// Insert, Delete, and Get calls are driven by queues; other methods are stubs.
type fakeGCEClient struct {
	// insertResponses drives successive Insert calls (consumed in order).
	insertResponses []insertResult
	insertCallCount int

	// getResponses drives successive Get calls (consumed in order).
	getResponses []getResult
	getCallCount int

	// deleteResponses drives successive Delete calls (consumed in order).
	deleteResponses []deleteResult
	deleteCallCount int
}

type insertResult struct {
	op  gceOperation
	err error
}
type getResult struct {
	inst *computepb.Instance
	err  error
}
type deleteResult struct {
	op  gceOperation
	err error
}

func (f *fakeGCEClient) Insert(_ context.Context, _ *computepb.InsertInstanceRequest) (gceOperation, error) {
	if f.insertCallCount >= len(f.insertResponses) {
		return nil, fmt.Errorf("unexpected Insert call #%d", f.insertCallCount+1)
	}
	r := f.insertResponses[f.insertCallCount]
	f.insertCallCount++
	return r.op, r.err
}

func (f *fakeGCEClient) Get(_ context.Context, _ *computepb.GetInstanceRequest) (*computepb.Instance, error) {
	if f.getCallCount >= len(f.getResponses) {
		return nil, fmt.Errorf("unexpected Get call #%d", f.getCallCount+1)
	}
	r := f.getResponses[f.getCallCount]
	f.getCallCount++
	return r.inst, r.err
}

func (f *fakeGCEClient) Delete(_ context.Context, _ *computepb.DeleteInstanceRequest) (gceOperation, error) {
	if f.deleteCallCount >= len(f.deleteResponses) {
		return nil, fmt.Errorf("unexpected Delete call #%d", f.deleteCallCount+1)
	}
	r := f.deleteResponses[f.deleteCallCount]
	f.deleteCallCount++
	return r.op, r.err
}

// Stub methods not exercised by these tests.
func (f *fakeGCEClient) Start(_ context.Context, _ *computepb.StartInstanceRequest) (gceOperation, error) {
	return &fakeOp{}, nil
}
func (f *fakeGCEClient) Stop(_ context.Context, _ *computepb.StopInstanceRequest) (gceOperation, error) {
	return &fakeOp{}, nil
}
func (f *fakeGCEClient) AggregatedList(_ context.Context, _ *computepb.AggregatedListInstancesRequest) gceAggregatedListIterator {
	return &emptyIterator{}
}
func (f *fakeGCEClient) Close() error { return nil }

// emptyIterator returns iterator.Done immediately.
type emptyIterator struct{}

func (e *emptyIterator) Next() (compute.InstancesScopedListPair, error) {
	return compute.InstancesScopedListPair{}, iterator.Done
}

// err409 returns a *googleapi.Error with Code 409 — what the real GCE client
// returns when an instance already exists.
func err409() error {
	return &googleapi.Error{Code: 409, Message: "already exists"}
}

// TestCreateInstance_AdoptsRunningInstance verifies that when Insert returns 409
// and the existing instance is RUNNING, CreateInstance returns the existing ID.
func TestCreateInstance_AdoptsRunningInstance(t *testing.T) {
	existingID := uint64(123456789)
	fake := &fakeGCEClient{
		insertResponses: []insertResult{
			{op: nil, err: err409()},
		},
		getResponses: []getResult{
			{inst: &computepb.Instance{
				Id:     proto.Uint64(existingID),
				Status: proto.String("RUNNING"),
			}},
		},
	}

	adapter := &GCEAdapter{
		ProjectID: "test-project",
		client:    fake,
	}

	id, err := adapter.CreateInstance(context.Background(), MachineSpec{
		Name:    "test-machine",
		Zone:    "us-central1-a",
		Spot:    true,
		DiskSizeGb: 50,
		MachineType: "n2-standard-4",
	})
	if err != nil {
		t.Fatalf("CreateInstance: unexpected error: %v", err)
	}

	want := fmt.Sprintf("%d", existingID)
	if id != want {
		t.Errorf("instance ID: got %q, want %q", id, want)
	}
	if fake.insertCallCount != 1 {
		t.Errorf("Insert call count: got %d, want 1", fake.insertCallCount)
	}
	if fake.getCallCount != 1 {
		t.Errorf("Get call count: got %d, want 1", fake.getCallCount)
	}
	if fake.deleteCallCount != 0 {
		t.Errorf("Delete call count: got %d, want 0 (no delete for RUNNING)", fake.deleteCallCount)
	}
}

// TestCreateInstance_AdoptsStagingInstance verifies adoption also works for STAGING status.
func TestCreateInstance_AdoptsStagingInstance(t *testing.T) {
	existingID := uint64(987654321)
	fake := &fakeGCEClient{
		insertResponses: []insertResult{
			{op: nil, err: err409()},
		},
		getResponses: []getResult{
			{inst: &computepb.Instance{
				Id:     proto.Uint64(existingID),
				Status: proto.String("STAGING"),
			}},
		},
	}

	adapter := &GCEAdapter{ProjectID: "test-project", client: fake}
	id, err := adapter.CreateInstance(context.Background(), MachineSpec{
		Name: "staging-machine", Zone: "us-central1-a", DiskSizeGb: 50, MachineType: "n2-standard-4",
	})
	if err != nil {
		t.Fatalf("CreateInstance: unexpected error: %v", err)
	}
	if id != fmt.Sprintf("%d", existingID) {
		t.Errorf("instance ID: got %q, want %d", id, existingID)
	}
}

// TestCreateInstance_DeletesTerminatedAndRetries verifies that when Insert returns 409
// and the existing instance is TERMINATED, we delete it and retry Insert.
func TestCreateInstance_DeletesTerminatedAndRetries(t *testing.T) {
	newID := uint64(555000111)
	fake := &fakeGCEClient{
		insertResponses: []insertResult{
			// First Insert: 409.
			{op: nil, err: err409()},
			// Second Insert (after delete): success.
			{op: &fakeOp{}, err: nil},
		},
		getResponses: []getResult{
			// Get after first Insert: TERMINATED.
			{inst: &computepb.Instance{
				Id:     proto.Uint64(77),
				Status: proto.String("TERMINATED"),
			}},
			// Get after second Insert (fetching the new instance ID).
			{inst: &computepb.Instance{Id: proto.Uint64(newID)}},
		},
		deleteResponses: []deleteResult{
			{op: &fakeOp{}, err: nil},
		},
	}

	adapter := &GCEAdapter{ProjectID: "test-project", client: fake}
	id, err := adapter.CreateInstance(context.Background(), MachineSpec{
		Name: "terminated-machine", Zone: "us-central1-a", DiskSizeGb: 50, MachineType: "n2-standard-4",
	})
	if err != nil {
		t.Fatalf("CreateInstance: unexpected error: %v", err)
	}

	want := fmt.Sprintf("%d", newID)
	if id != want {
		t.Errorf("instance ID: got %q, want %q", id, want)
	}
	if fake.insertCallCount != 2 {
		t.Errorf("Insert call count: got %d, want 2", fake.insertCallCount)
	}
	if fake.deleteCallCount != 1 {
		t.Errorf("Delete call count: got %d, want 1", fake.deleteCallCount)
	}
	if fake.getCallCount != 2 {
		t.Errorf("Get call count: got %d, want 2 (one after 409, one for new ID)", fake.getCallCount)
	}
}

// TestCreateInstance_DeletesStoppedAndRetries verifies the same delete-and-retry path
// for STOPPED status (not just TERMINATED).
func TestCreateInstance_DeletesStoppedAndRetries(t *testing.T) {
	newID := uint64(666777888)
	fake := &fakeGCEClient{
		insertResponses: []insertResult{
			{op: nil, err: err409()},
			{op: &fakeOp{}, err: nil},
		},
		getResponses: []getResult{
			{inst: &computepb.Instance{
				Id:     proto.Uint64(42),
				Status: proto.String("STOPPED"),
			}},
			{inst: &computepb.Instance{Id: proto.Uint64(newID)}},
		},
		deleteResponses: []deleteResult{
			{op: &fakeOp{}, err: nil},
		},
	}

	adapter := &GCEAdapter{ProjectID: "test-project", client: fake}
	id, err := adapter.CreateInstance(context.Background(), MachineSpec{
		Name: "stopped-machine", Zone: "us-central1-a", DiskSizeGb: 50, MachineType: "n2-standard-4",
	})
	if err != nil {
		t.Fatalf("CreateInstance: unexpected error: %v", err)
	}
	if id != fmt.Sprintf("%d", newID) {
		t.Errorf("instance ID: got %q, want %d", id, newID)
	}
}

// TestCreateInstance_AmbiguousStatusReturnsError verifies that a 409 where the
// existing instance is in REPAIRING returns an error with a clear message.
func TestCreateInstance_AmbiguousStatusReturnsError(t *testing.T) {
	fake := &fakeGCEClient{
		insertResponses: []insertResult{
			{op: nil, err: err409()},
		},
		getResponses: []getResult{
			{inst: &computepb.Instance{
				Id:     proto.Uint64(999),
				Status: proto.String("REPAIRING"),
			}},
		},
	}

	adapter := &GCEAdapter{ProjectID: "test-project", client: fake}
	_, err := adapter.CreateInstance(context.Background(), MachineSpec{
		Name: "repairing-machine", Zone: "us-central1-a", DiskSizeGb: 50, MachineType: "n2-standard-4",
	})
	if err == nil {
		t.Fatal("expected error for REPAIRING instance, got nil")
	}
	if !contains(err.Error(), "ambiguous status") {
		t.Errorf("error message should mention 'ambiguous status', got: %v", err)
	}
	if !contains(err.Error(), "REPAIRING") {
		t.Errorf("error message should include the status 'REPAIRING', got: %v", err)
	}
}

// TestCreateInstance_NonAlreadyExistsErrorPropagated verifies that non-409 Insert
// errors are propagated directly without any retry logic.
func TestCreateInstance_NonAlreadyExistsErrorPropagated(t *testing.T) {
	fake := &fakeGCEClient{
		insertResponses: []insertResult{
			{op: nil, err: &googleapi.Error{Code: 403, Message: "permission denied"}},
		},
	}

	adapter := &GCEAdapter{ProjectID: "test-project", client: fake}
	_, err := adapter.CreateInstance(context.Background(), MachineSpec{
		Name: "no-permission", Zone: "us-central1-a", DiskSizeGb: 50, MachineType: "n2-standard-4",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if fake.insertCallCount != 1 {
		t.Errorf("Insert call count: got %d, want 1 (no retry on non-409)", fake.insertCallCount)
	}
	if fake.getCallCount != 0 {
		t.Errorf("Get should not be called on non-409 error, got %d calls", fake.getCallCount)
	}
}

// TestCreateInstance_HappyPath verifies the normal (no 409) path still works.
func TestCreateInstance_HappyPath(t *testing.T) {
	newID := uint64(111222333)
	fake := &fakeGCEClient{
		insertResponses: []insertResult{
			{op: &fakeOp{}, err: nil},
		},
		getResponses: []getResult{
			{inst: &computepb.Instance{Id: proto.Uint64(newID)}},
		},
	}

	adapter := &GCEAdapter{ProjectID: "test-project", client: fake}
	id, err := adapter.CreateInstance(context.Background(), MachineSpec{
		Name: "new-machine", Zone: "us-central1-a", DiskSizeGb: 50, MachineType: "n2-standard-4",
	})
	if err != nil {
		t.Fatalf("CreateInstance: unexpected error: %v", err)
	}
	if id != fmt.Sprintf("%d", newID) {
		t.Errorf("instance ID: got %q, want %d", id, newID)
	}
	if fake.insertCallCount != 1 {
		t.Errorf("Insert call count: got %d, want 1", fake.insertCallCount)
	}
	if fake.deleteCallCount != 0 {
		t.Errorf("Delete should not be called on success, got %d calls", fake.deleteCallCount)
	}
}

// TestIsAlreadyExistsError verifies the helper recognises 409 errors.
func TestIsAlreadyExistsError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "googleapi 409",
			err:  &googleapi.Error{Code: 409, Message: "already exists"},
			want: true,
		},
		{
			name: "googleapi 403",
			err:  &googleapi.Error{Code: 403, Message: "forbidden"},
			want: false,
		},
		{
			name: "wrapped 409",
			err:  fmt.Errorf("outer: %w", &googleapi.Error{Code: 409, Message: "already exists"}),
			want: true,
		},
		{
			name: "plain string with already exists",
			err:  fmt.Errorf("googleapi: Error 409: already exists"),
			want: true,
		},
		{
			name: "unrelated error",
			err:  fmt.Errorf("connection refused"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isAlreadyExistsError(tt.err)
			if got != tt.want {
				t.Errorf("isAlreadyExistsError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// contains is a helper for checking error message substrings without importing strings in tests.
func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
