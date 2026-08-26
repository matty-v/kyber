package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/eks"
)

const (
	EKSConfigRegion       = "eks.region"
	EKSConfigCluster      = "eks.cluster"
	EKSConfigProfiles     = "eks.profiles"
	EKSConfigAllowedZones = "eks.allowedZones"
	EKSConfigNodeRoleARN  = "eks.nodeRoleArn"
)

type EKSProfile struct {
	ID                  string   `json:"id"`
	DisplayName         string   `json:"displayName"`
	CPU                 string   `json:"cpu"`
	Memory              string   `json:"memory"`
	InstanceTypes       []string `json:"instanceTypes"`
	DiskSizeGB          int32    `json:"diskSizeGb"`
	AvailabilityClasses []string `json:"availabilityClasses"`
}

type eksClient interface{}

type EKSAdapter struct {
	region, cluster, nodeRoleARN string
	allowedZones                 map[string]struct{}
	profiles                     map[string]EKSProfile
	client                       eksClient
}

func (e *EKSAdapter) providerRef(nodeGroup string) ProviderRef {
	return ProviderRef("eks://" + url.PathEscape(e.cluster) + "/" + url.PathEscape(nodeGroup))
}

func (e *EKSAdapter) parseProviderRef(ref ProviderRef) (string, error) {
	u, err := url.Parse(string(ref))
	if err != nil || u.Scheme != "eks" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid EKS provider ref")
	}
	cluster, err := url.PathUnescape(u.Host)
	if err != nil || cluster != e.cluster {
		return "", fmt.Errorf("EKS provider ref belongs to another cluster")
	}
	parts := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	if len(parts) != 1 || parts[0] == "" {
		return "", fmt.Errorf("invalid EKS provider ref path")
	}
	name, err := url.PathUnescape(parts[0])
	if err != nil || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("invalid EKS node group name")
	}
	return name, nil
}

func init() {
	RegisterComputeProvider("eks", func(ctx context.Context, cfg ProviderConfig) (ComputeAdapter, error) {
		adapter, err := parseEKSConfig(cfg)
		if err != nil {
			return nil, err
		}
		awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(adapter.region))
		if err != nil {
			return nil, fmt.Errorf("loading AWS default credential chain: %w", err)
		}
		adapter.client = eks.NewFromConfig(awsCfg)
		return adapter, nil
	})
}

func parseEKSConfig(cfg ProviderConfig) (*EKSAdapter, error) {
	required := []string{EKSConfigRegion, EKSConfigCluster, EKSConfigProfiles, EKSConfigAllowedZones, EKSConfigNodeRoleARN}
	for _, key := range required {
		if strings.TrimSpace(cfg[key]) == "" {
			return nil, fmt.Errorf("%s is required", key)
		}
	}
	var profiles []EKSProfile
	if err := json.Unmarshal([]byte(cfg[EKSConfigProfiles]), &profiles); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", EKSConfigProfiles, err)
	}
	var zones []string
	if err := json.Unmarshal([]byte(cfg[EKSConfigAllowedZones]), &zones); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", EKSConfigAllowedZones, err)
	}
	if len(profiles) == 0 || len(zones) == 0 {
		return nil, fmt.Errorf("EKS profiles and allowed zones must not be empty")
	}
	a := &EKSAdapter{region: cfg[EKSConfigRegion], cluster: cfg[EKSConfigCluster], nodeRoleARN: cfg[EKSConfigNodeRoleARN], profiles: map[string]EKSProfile{}, allowedZones: map[string]struct{}{}}
	for _, zone := range zones {
		if strings.TrimSpace(zone) == "" {
			return nil, fmt.Errorf("EKS allowed zone must not be empty")
		}
		a.allowedZones[zone] = struct{}{}
	}
	for _, profile := range profiles {
		if profile.ID == "" || len(profile.InstanceTypes) == 0 || profile.DiskSizeGB < 1 {
			return nil, fmt.Errorf("invalid EKS profile %q", profile.ID)
		}
		if _, exists := a.profiles[profile.ID]; exists {
			return nil, fmt.Errorf("duplicate EKS profile %q", profile.ID)
		}
		a.profiles[profile.ID] = profile
	}
	return a, nil
}

func (e *EKSAdapter) Type() string                       { return "eks" }
func (e *EKSAdapter) NodeAttachment() NodeAttachmentMode { return NodeAttachmentManaged }
func (e *EKSAdapter) Capabilities(context.Context) (Capabilities, error) {
	return Capabilities{CanProvision: true, SuspendMode: SuspendCapacity, DeletionMode: DeleteCapacity, SupportsReliable: true, SupportsInterruptible: false, SupportsLocations: true, ReliableFallbackMode: ReliableFallbackUnsupported}, nil
}
func (e *EKSAdapter) Profiles(context.Context) ([]Profile, error) {
	out := make([]Profile, 0, len(e.profiles))
	for _, p := range e.profiles {
		out = append(out, Profile{ID: p.ID, DisplayName: p.DisplayName, CPU: p.CPU, Memory: p.Memory, AvailabilityClasses: append([]string(nil), p.AvailabilityClasses...)})
	}
	return out, nil
}
func (e *EKSAdapter) Validate(_ context.Context, d DesiredMachine) error {
	if _, ok := e.profiles[d.Profile]; !ok {
		return fmt.Errorf("validating EKS capacity: unknown profile %q", d.Profile)
	}
	if _, ok := e.allowedZones[d.Location]; !ok {
		return fmt.Errorf("validating EKS capacity: location %q is not allowed", d.Location)
	}
	if d.Interruptible {
		return fmt.Errorf("validating EKS capacity: cost-optimized lifecycle is not enabled yet")
	}
	return nil
}
func (e *EKSAdapter) Reconcile(context.Context, MachineIdentity, DesiredMachine, ProviderRef) (CapacityObservation, error) {
	return CapacityObservation{}, fmt.Errorf("EKS reliable lifecycle is not implemented")
}
func (e *EKSAdapter) CreateInstance(context.Context, MachineSpec) (string, error) {
	return "", fmt.Errorf("EKS uses declarative capacity reconciliation")
}
func (e *EKSAdapter) StartInstance(context.Context, string) error {
	return fmt.Errorf("EKS uses declarative capacity reconciliation")
}
func (e *EKSAdapter) StopInstance(context.Context, string) error {
	return fmt.Errorf("EKS uses declarative capacity reconciliation")
}
func (e *EKSAdapter) DeleteInstance(context.Context, string) error {
	return fmt.Errorf("EKS uses declarative capacity reconciliation")
}
func (e *EKSAdapter) Observe(context.Context, string) (InstanceObservation, error) {
	return InstanceObservation{}, fmt.Errorf("EKS uses declarative capacity reconciliation")
}

var _ ComputeAdapter = (*EKSAdapter)(nil)
var _ CapacityProvider = (*EKSAdapter)(nil)
