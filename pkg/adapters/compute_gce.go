package adapters

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/proto"
)

// gceOperation abstracts the compute.Operation returned by GCE API calls.
// This allows unit tests to inject fake operations without network calls.
// The signature matches *compute.Operation.Wait which takes variadic gax.CallOption.
type gceOperation interface {
	Wait(ctx context.Context, opts ...gax.CallOption) error
}

// gceInstancesClient abstracts the subset of compute.InstancesClient methods
// used by GCEAdapter. Extracted to allow injection of a fake in unit tests.
type gceInstancesClient interface {
	Insert(ctx context.Context, req *computepb.InsertInstanceRequest) (gceOperation, error)
	Delete(ctx context.Context, req *computepb.DeleteInstanceRequest) (gceOperation, error)
	Get(ctx context.Context, req *computepb.GetInstanceRequest) (*computepb.Instance, error)
	Start(ctx context.Context, req *computepb.StartInstanceRequest) (gceOperation, error)
	Stop(ctx context.Context, req *computepb.StopInstanceRequest) (gceOperation, error)
	AggregatedList(ctx context.Context, req *computepb.AggregatedListInstancesRequest) gceAggregatedListIterator
	Close() error
}

// gceAggregatedListIterator abstracts the iterator returned by AggregatedList.
type gceAggregatedListIterator interface {
	Next() (compute.InstancesScopedListPair, error)
}

// realGCEClient wraps *compute.InstancesClient to implement gceInstancesClient.
// The real GCE client methods return *compute.Operation; we wrap them to satisfy
// the gceOperation interface.
type realGCEClient struct {
	c *compute.InstancesClient
}

func (r *realGCEClient) Insert(ctx context.Context, req *computepb.InsertInstanceRequest) (gceOperation, error) {
	return r.c.Insert(ctx, req)
}
func (r *realGCEClient) Delete(ctx context.Context, req *computepb.DeleteInstanceRequest) (gceOperation, error) {
	return r.c.Delete(ctx, req)
}
func (r *realGCEClient) Get(ctx context.Context, req *computepb.GetInstanceRequest) (*computepb.Instance, error) {
	return r.c.Get(ctx, req)
}
func (r *realGCEClient) Start(ctx context.Context, req *computepb.StartInstanceRequest) (gceOperation, error) {
	return r.c.Start(ctx, req)
}
func (r *realGCEClient) Stop(ctx context.Context, req *computepb.StopInstanceRequest) (gceOperation, error) {
	return r.c.Stop(ctx, req)
}
func (r *realGCEClient) AggregatedList(ctx context.Context, req *computepb.AggregatedListInstancesRequest) gceAggregatedListIterator {
	return r.c.AggregatedList(ctx, req)
}
func (r *realGCEClient) Close() error {
	return r.c.Close()
}

// isAlreadyExistsError returns true if err is a GCE 409 "already exists" response.
// The compute API library returns a googleapi.Error with Code=409.
func isAlreadyExistsError(err error) bool {
	if err == nil {
		return false
	}
	var gerr *googleapi.Error
	if errors.As(err, &gerr) && gerr.Code == 409 {
		return true
	}
	// Fallback: string check in case wrapping strips the typed error.
	return strings.Contains(err.Error(), "already exists")
}

// ErrInstanceNotFound is returned by GCEAdapter when the requested instance
// ID cannot be found — either because it was never valid (wrong format, e.g.
// a stale mock-adapter ID like "mock-abc-123") or because the instance has
// already been deleted out of band. Callers that don't strictly need the
// instance to exist (DeleteInstance, for example) should treat this as a
// successful no-op.
var ErrInstanceNotFound = errors.New("GCE instance not found")

// GCEAdapter implements ComputeAdapter using the official Google Cloud Compute Engine API.
// It uses Application Default Credentials (ADC) — no credentials are hardcoded.
// Set GOOGLE_APPLICATION_CREDENTIALS or run on GCE/GKE with a service account.
//
// This is the production adapter. Use MockComputeAdapter for tests and k3d.
type GCEAdapter struct {
	// ProjectID is the GCP project where instances are managed. Required.
	ProjectID string

	// Network is the VPC network for new VMs (e.g. "kyber-small-network").
	// If empty, defaults to "default". Worker VMs MUST be on the same network
	// as the k3s server for kubelet probes and pod-to-pod communication to work.
	Network string

	// Subnet is the subnetwork within Network (e.g. "kyber-small-subnet").
	// If empty, GCE auto-selects a subnet (only works with auto-mode VPCs).
	Subnet string

	// client is the GCE instances client (real or fake for tests).
	client gceInstancesClient
}

// NewGCEAdapter creates a GCEAdapter and establishes a connection to the GCE API.
// Credentials are resolved via ADC (GOOGLE_APPLICATION_CREDENTIALS or metadata server).
// Call Close() when done.
func NewGCEAdapter(ctx context.Context, projectID string, opts ...option.ClientOption) (*GCEAdapter, error) {
	if projectID == "" {
		return nil, fmt.Errorf("GCEAdapter: projectID is required")
	}
	c, err := compute.NewInstancesRESTClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating GCE instances client: %w", err)
	}
	return &GCEAdapter{
		ProjectID: projectID,
		client:    &realGCEClient{c: c},
	}, nil
}

// Close releases the underlying GCE client connection.
func (g *GCEAdapter) Close() error {
	if g.client != nil {
		return g.client.Close()
	}
	return nil
}

// instanceGCEName returns the GCE instance name for a logical machine name.
// Prefixed with "kyber-" to avoid GCE name collisions with other resources.
func instanceGCEName(machineName string) string {
	return "kyber-" + machineName
}

// buildNetworkInterface returns a NetworkInterface for the configured VPC.
// If Network is set, uses a custom VPC + explicit subnet. Otherwise uses the
// GCP default auto-mode VPC.
func (g *GCEAdapter) buildNetworkInterface(spec MachineSpec) *computepb.NetworkInterface {
	nic := &computepb.NetworkInterface{
		AccessConfigs: []*computepb.AccessConfig{
			{
				Name:        proto.String("External NAT"),
				Type:        proto.String("ONE_TO_ONE_NAT"),
				NetworkTier: proto.String("PREMIUM"),
			},
		},
	}
	if g.Network != "" {
		nic.Network = proto.String(fmt.Sprintf("projects/%s/global/networks/%s", g.ProjectID, g.Network))
		if g.Subnet != "" {
			nic.Subnetwork = proto.String(fmt.Sprintf("projects/%s/regions/%s/subnetworks/%s",
				g.ProjectID, regionFromZone(spec.Zone), g.Subnet))
		}
	} else {
		nic.Network = proto.String("global/networks/default")
	}
	return nic
}

// regionFromZone strips the last -X from a zone like "us-central1-a" → "us-central1".
func regionFromZone(zone string) string {
	if i := strings.LastIndex(zone, "-"); i > 0 {
		return zone[:i]
	}
	return zone
}

// CreateInstance provisions a new GCE VM using the provided spec.
// Returns the GCE-assigned instance ID (numeric string from the insert operation).
func (g *GCEAdapter) CreateInstance(ctx context.Context, spec MachineSpec) (string, error) {
	gceName := instanceGCEName(spec.Name)

	labels := map[string]string{
		"kyber-machine": spec.Name,
		"managed-by":    "kyber",
	}
	for k, v := range spec.Labels {
		labels[k] = v
	}

	diskSizeGb := int64(spec.DiskSizeGb)
	scheduling := &computepb.Scheduling{
		Preemptible: proto.Bool(spec.Spot),
	}
	if spec.Spot {
		// Use SPOT provisioning model for GCE spot instances.
		// ProvisioningModel is a *string field in the REST API representation.
		spotModel := computepb.Scheduling_SPOT.String()
		scheduling.ProvisioningModel = &spotModel
	}

	// Startup script installs k3s agent and labels the k8s node.
	startupScript := buildStartupScript(spec)

	req := &computepb.InsertInstanceRequest{
		Project: g.ProjectID,
		Zone:    spec.Zone,
		InstanceResource: &computepb.Instance{
			Name:        proto.String(gceName),
			MachineType: proto.String(fmt.Sprintf("zones/%s/machineTypes/%s", spec.Zone, spec.MachineType)),
			Disks: []*computepb.AttachedDisk{
				{
					Boot:       proto.Bool(true),
					AutoDelete: proto.Bool(true),
					InitializeParams: &computepb.AttachedDiskInitializeParams{
						DiskSizeGb: proto.Int64(diskSizeGb),
						// The Ubuntu 24.04 LTS image family in projects/ubuntu-os-cloud
						// is "ubuntu-2404-lts-amd64" (not "ubuntu-2404-lts"). The short
						// name returns 404 at Insert time with no suggestion from GCE.
						SourceImage: proto.String("projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64"),
					},
				},
			},
			NetworkInterfaces: []*computepb.NetworkInterface{
				g.buildNetworkInterface(spec),
			},
			Metadata: &computepb.Metadata{
				Items: []*computepb.Items{
					{Key: proto.String("startup-script"), Value: proto.String(startupScript)},
					{Key: proto.String("k3s-server-url"), Value: proto.String(spec.ServerURL)},
					{Key: proto.String("k3s-join-token"), Value: proto.String(spec.JoinToken)},
					{Key: proto.String("kyber-machine-name"), Value: proto.String(spec.Name)},
				},
			},
			Labels:     labels,
			Scheduling: scheduling,
		},
	}

	var op gceOperation
	var insertErr error
	for attempt := 0; attempt < 2; attempt++ {
		op, insertErr = g.client.Insert(ctx, req)
		if insertErr == nil {
			break
		}
		if !isAlreadyExistsError(insertErr) {
			return "", fmt.Errorf("GCE Insert(%s/%s/%s): %w", g.ProjectID, spec.Zone, gceName, insertErr)
		}

		// 409 — inspect the existing instance to decide how to proceed.
		existing, getErr := g.client.Get(ctx, &computepb.GetInstanceRequest{
			Project:  g.ProjectID,
			Zone:     spec.Zone,
			Instance: gceName,
		})
		if getErr != nil {
			return "", fmt.Errorf("GCE Insert returned 409 but Get failed: insert=%w; get=%w", insertErr, getErr)
		}

		status := ""
		if existing.Status != nil {
			status = *existing.Status
		}

		switch status {
		case "RUNNING", "STAGING", "PROVISIONING":
			// Adopt: the Insert op likely succeeded earlier but op.Wait was interrupted.
			// Return the existing instance ID as a success.
			if existing.Id != nil {
				return fmt.Sprintf("%d", *existing.Id), nil
			}
			return "", fmt.Errorf("GCE Insert 409 with adoptable status=%s but instance has no ID", status)

		case "TERMINATED", "STOPPED", "STOPPING":
			// Delete the stale instance and retry Insert.
			delOp, delErr := g.client.Delete(ctx, &computepb.DeleteInstanceRequest{
				Project:  g.ProjectID,
				Zone:     spec.Zone,
				Instance: gceName,
			})
			if delErr != nil {
				return "", fmt.Errorf("deleting stale terminated instance %s: %w", gceName, delErr)
			}
			if waitErr := delOp.Wait(ctx); waitErr != nil {
				return "", fmt.Errorf("waiting for stale instance delete of %s: %w", gceName, waitErr)
			}
			// Loop continues to retry Insert.

		default:
			return "", fmt.Errorf("GCE Insert(%s): instance exists with ambiguous status=%q — manual intervention required", gceName, status)
		}
	}
	if insertErr != nil {
		return "", fmt.Errorf("GCE Insert(%s/%s/%s): exhausted retries after 409: %w", g.ProjectID, spec.Zone, gceName, insertErr)
	}

	if err := op.Wait(ctx); err != nil {
		return "", fmt.Errorf("waiting for GCE Insert(%s): %w", gceName, err)
	}

	// Fetch the created instance to get its numeric ID.
	inst, err := g.client.Get(ctx, &computepb.GetInstanceRequest{
		Project:  g.ProjectID,
		Zone:     spec.Zone,
		Instance: gceName,
	})
	if err != nil {
		return "", fmt.Errorf("fetching created instance %s: %w", gceName, err)
	}

	return fmt.Sprintf("%d", inst.GetId()), nil
}

// StartInstance starts a stopped GCE instance identified by its numeric instance ID.
// The instance is looked up by listing with a filter on the ID.
func (g *GCEAdapter) StartInstance(ctx context.Context, instanceID string) error {
	gceName, zone, err := g.lookupInstanceByID(ctx, instanceID)
	if err != nil {
		return err
	}

	op, err := g.client.Start(ctx, &computepb.StartInstanceRequest{
		Project:  g.ProjectID,
		Zone:     zone,
		Instance: gceName,
	})
	if err != nil {
		return fmt.Errorf("GCE Start(%s): %w", gceName, err)
	}
	if err := op.Wait(ctx); err != nil {
		return fmt.Errorf("waiting for GCE Start(%s): %w", gceName, err)
	}
	return nil
}

// StopInstance stops a running GCE instance.
func (g *GCEAdapter) StopInstance(ctx context.Context, instanceID string) error {
	gceName, zone, err := g.lookupInstanceByID(ctx, instanceID)
	if err != nil {
		return err
	}

	op, err := g.client.Stop(ctx, &computepb.StopInstanceRequest{
		Project:  g.ProjectID,
		Zone:     zone,
		Instance: gceName,
	})
	if err != nil {
		return fmt.Errorf("GCE Stop(%s): %w", gceName, err)
	}
	if err := op.Wait(ctx); err != nil {
		return fmt.Errorf("waiting for GCE Stop(%s): %w", gceName, err)
	}
	return nil
}

// DeleteInstance deletes a GCE instance and its auto-delete boot disk.
// If the instance ID is invalid or the instance is already gone, this
// returns nil — a delete on a non-existent instance is a no-op so finalizers
// can complete cleanly instead of looping on lookup errors.
func (g *GCEAdapter) DeleteInstance(ctx context.Context, instanceID string) error {
	gceName, zone, err := g.lookupInstanceByID(ctx, instanceID)
	if errors.Is(err, ErrInstanceNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	op, err := g.client.Delete(ctx, &computepb.DeleteInstanceRequest{
		Project:  g.ProjectID,
		Zone:     zone,
		Instance: gceName,
	})
	if err != nil {
		return fmt.Errorf("GCE Delete(%s): %w", gceName, err)
	}
	if err := op.Wait(ctx); err != nil {
		return fmt.Errorf("waiting for GCE Delete(%s): %w", gceName, err)
	}
	return nil
}

// GetInstanceStatus fetches the current GCE status and network info for an instance.
func (g *GCEAdapter) GetInstanceStatus(ctx context.Context, instanceID string) (InstanceStatus, error) {
	gceName, zone, err := g.lookupInstanceByID(ctx, instanceID)
	if err != nil {
		return InstanceStatus{}, err
	}

	inst, err := g.client.Get(ctx, &computepb.GetInstanceRequest{
		Project:  g.ProjectID,
		Zone:     zone,
		Instance: gceName,
	})
	if err != nil {
		return InstanceStatus{}, fmt.Errorf("GCE Get(%s): %w", gceName, err)
	}

	return parseInstanceStatus(inst, zone), nil
}

// IsPreempted returns true if the GCE instance was preempted by GCE.
// Preemption is indicated by TERMINATED status on a spot/preemptible instance.
func (g *GCEAdapter) IsPreempted(ctx context.Context, instanceID string) (bool, error) {
	gceName, zone, err := g.lookupInstanceByID(ctx, instanceID)
	if err != nil {
		return false, err
	}

	inst, err := g.client.Get(ctx, &computepb.GetInstanceRequest{
		Project:  g.ProjectID,
		Zone:     zone,
		Instance: gceName,
	})
	if err != nil {
		return false, fmt.Errorf("GCE Get(%s): %w", gceName, err)
	}

	// Preemptible instance must be TERMINATED to be considered preempted.
	status := inst.GetStatus()
	if status != "TERMINATED" {
		return false, nil
	}

	// Check if instance has preemptible scheduling.
	sched := inst.GetScheduling()
	if sched == nil {
		return false, nil
	}

	// Note: Preemptible is deprecated in favor of ProvisioningModel=SPOT but is still
	// functional. We check both to be compatible with older Spot Fleet implementations
	// and with GCE APIs that return only one field.
	isPreemptible := sched.GetPreemptible() //nolint:staticcheck // deprecated but still functional
	isSpot := sched.GetProvisioningModel() == computepb.Scheduling_SPOT.String()

	// TERMINATED + (preemptible or SPOT) → preempted.
	// Operator-stopped instances will have been stopped via StopInstance, which transitions
	// them to STOPPED, not TERMINATED. TERMINATED on a preemptible instance reliably
	// indicates GCE-initiated preemption.
	return isPreemptible || isSpot, nil
}

// lookupInstanceByID finds a GCE instance's name and zone given its numeric
// instance ID.
//
// Returns ErrInstanceNotFound when:
//   - the ID is empty or not a valid uint64 (e.g. a stale mock-adapter ID
//     like "mock-abc-123" — GCE would reject the filter with a 400 anyway,
//     so we short-circuit with a clear error);
//   - the instance doesn't exist in the project (already deleted or never
//     created in this project).
//
// Callers can use errors.Is(err, ErrInstanceNotFound) to decide whether to
// treat the miss as a successful no-op (e.g. during finalizer-driven delete)
// or propagate the error.
func (g *GCEAdapter) lookupInstanceByID(ctx context.Context, instanceID string) (name, zone string, err error) {
	if instanceID == "" {
		return "", "", fmt.Errorf("%w: empty instance ID", ErrInstanceNotFound)
	}
	// GCE instance IDs are uint64 numeric strings. Reject non-numeric IDs
	// up front so we don't send them to the AggregatedList filter, which
	// GCE answers with HTTP 400 "Invalid list filter expression."
	if _, parseErr := strconv.ParseUint(instanceID, 10, 64); parseErr != nil {
		return "", "", fmt.Errorf("%w: ID %q is not a valid GCE numeric instance ID", ErrInstanceNotFound, instanceID)
	}

	filter := fmt.Sprintf("id=%s", instanceID)
	it := g.client.AggregatedList(ctx, &computepb.AggregatedListInstancesRequest{
		Project: g.ProjectID,
		Filter:  proto.String(filter),
	})

	for {
		pair, iterErr := it.Next()
		if iterErr == iterator.Done {
			break
		}
		if iterErr != nil {
			return "", "", fmt.Errorf("listing instances in project %s: %w", g.ProjectID, iterErr)
		}
		for _, inst := range pair.Value.GetInstances() {
			if strconv.FormatUint(inst.GetId(), 10) == instanceID {
				// Zone is returned as "zones/us-central1-a" — strip the prefix.
				z := inst.GetZone()
				if idx := strings.LastIndex(z, "/"); idx >= 0 {
					z = z[idx+1:]
				}
				return inst.GetName(), z, nil
			}
		}
	}

	return "", "", fmt.Errorf("%w: instance with ID %q not found in project %s", ErrInstanceNotFound, instanceID, g.ProjectID)
}

// parseInstanceStatus converts a GCE Instance proto into an InstanceStatus.
func parseInstanceStatus(inst *computepb.Instance, zone string) InstanceStatus {
	s := InstanceStatus{
		Status: inst.GetStatus(),
		Zone:   zone,
	}

	// Parse creation timestamp.
	ts := inst.GetCreationTimestamp()
	if ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			s.CreatedAt = t
		}
	}

	// Extract IP addresses from network interfaces.
	for _, ni := range inst.GetNetworkInterfaces() {
		if s.InternalIP == "" {
			s.InternalIP = ni.GetNetworkIP()
		}
		for _, ac := range ni.GetAccessConfigs() {
			if s.ExternalIP == "" {
				s.ExternalIP = ac.GetNatIP()
			}
		}
	}

	return s
}

// buildStartupScript returns the cloud-init startup script for the VM.
// The script installs the k3s agent, joins the cluster, and registers the
// kyber.io/machine label directly via k3s-agent's --node-label flag — not
// via a post-install `kubectl label` call, because k3s agents don't have a
// kubeconfig on disk and any kubectl call on the agent node would fail with
// "dial tcp 127.0.0.1:8080: i/o timeout".
func buildStartupScript(spec MachineSpec) string {
	return fmt.Sprintf(`#!/bin/bash
set -euo pipefail

MACHINE_NAME="%s"
MACHINE_LABEL="%s=${MACHINE_NAME}"
K3S_SERVER_URL=$(curl -s -H "Metadata-Flavor: Google" \
  http://metadata.google.internal/computeMetadata/v1/instance/attributes/k3s-server-url)
K3S_TOKEN=$(curl -s -H "Metadata-Flavor: Google" \
  http://metadata.google.internal/computeMetadata/v1/instance/attributes/k3s-join-token)

# Install k3s agent and join the cluster. The --node-label flag registers the
# kyber.io/machine=<name> label at kubelet registration time, which the Machine
# Controller's getNodeForMachine uses to match Nodes back to their Machine CRDs.
curl -sfL https://get.k3s.io | \
  K3S_URL="${K3S_SERVER_URL}" \
  K3S_TOKEN="${K3S_TOKEN}" \
  INSTALL_K3S_EXEC="agent --node-label ${MACHINE_LABEL}" sh -
`,
		spec.Name,
		MachineLabelKey,
	)
}

// compile-time assertion: GCEAdapter implements ComputeAdapter.
var _ ComputeAdapter = (*GCEAdapter)(nil)
