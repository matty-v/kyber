package adapters

import (
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/proto"
)

func TestGCEInstanceState(t *testing.T) {
	tests := []struct {
		status string
		want   InstanceState
	}{
		{status: "PROVISIONING", want: InstanceStatePending},
		{status: "STAGING", want: InstanceStatePending},
		{status: "STOPPING", want: InstanceStatePending},
		{status: "RUNNING", want: InstanceStateRunning},
		{status: "STOPPED", want: InstanceStateStopped},
		{status: "SUSPENDED", want: InstanceStateStopped},
		{status: "TERMINATED", want: InstanceStateStopped},
		{status: "UNRECOGNIZED", want: InstanceStateUnknown},
	}

	for _, tc := range tests {
		t.Run(tc.status, func(t *testing.T) {
			if got := gceInstanceState(tc.status); got != tc.want {
				t.Errorf("gceInstanceState(%q) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

func TestParseInstanceObservationInterruption(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		scheduling *computepb.Scheduling
		want       InterruptionState
	}{
		{
			name:   "running spot is not interrupted",
			status: "RUNNING",
			scheduling: &computepb.Scheduling{
				ProvisioningModel: proto.String(computepb.Scheduling_SPOT.String()),
			},
			want: InterruptionNone,
		},
		{
			name:   "terminated regular instance is stopped",
			status: "TERMINATED",
			want:   InterruptionNone,
		},
		{
			name:   "terminated spot instance is preempted",
			status: "TERMINATED",
			scheduling: &computepb.Scheduling{
				ProvisioningModel: proto.String(computepb.Scheduling_SPOT.String()),
			},
			want: InterruptionPreempted,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst := &computepb.Instance{
				Status:     proto.String(tc.status),
				Scheduling: tc.scheduling,
			}
			got := parseInstanceObservation(inst, "test-location")
			if got.Interruption != tc.want {
				t.Errorf("Interruption = %q, want %q", got.Interruption, tc.want)
			}
			if got.Location != "test-location" {
				t.Errorf("Location = %q, want test-location", got.Location)
			}
		})
	}
}
