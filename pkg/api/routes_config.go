package api

import (
	"context"
	"net/http"
	"sort"

	"github.com/matty-v/kyber/pkg/adapters"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/runtimedetect"
	"github.com/matty-v/kyber/pkg/tokenreport"
)

// ConfigResponse is the read-only public config surface consumed by the PWA.
// Start minimal — expose only what the UI needs for provider-conditional
// rendering. Add fields here when a consumer requires them; do not add for
// speculative future needs.
type ConfigResponse struct {
	Compute  ConfigCompute       `json:"compute"`
	Models   []tokenreport.Model `json:"models"`
	Identity ConfigIdentity      `json:"identity"`
	// PublicURL is the externally-reachable HTTPS URL of this Kyber
	// instance — used by the PWA to render webhook URLs for inbound
	// bindings (kyber#208). Empty/omitted when the server has no
	// PublicURL configured (dev/test scenarios), in which case the PWA
	// falls back to displaying just the webhook path.
	PublicURL string `json:"publicUrl,omitempty"`
}

// ConfigIdentity carries identity-repo settings the wizard needs to
// construct the create-new-from-template flow's full repo name and call
// the collision-check endpoint with the right owner. Empty when the
// control plane was started without KYBER_IDENTITY_REPO_OWNER (auto-create
// disabled).
type ConfigIdentity struct {
	// RepoOwner is the GitHub user/org under which auto-created identity
	// repos are placed (e.g. "matty-v"). Empty string when not configured.
	RepoOwner string `json:"repoOwner"`
}

// ConfigCompute reports which ComputeAdapter the control plane is configured
// to use. Values: "gce", "static", "fake", "mock". Empty string if the server was started
// without KYBER_COMPUTE_PROVIDER (dev/test scenarios).
type ConfigCompute struct {
	// Provider is the configured ComputeAdapter type.
	Provider string `json:"provider"`
	// GCEVMTypes is the catalog of GCE machine types the control plane will
	// accept on POST /api/v1/machines, sourced from compute.gce.vmTypeCatalog
	// in the chart (or the built-in default). Surfaced here so the PWA's
	// machine-type picker stays in sync with the server-side validator —
	// without this, operators get "unknown VM type" 400s on types the picker
	// shows. Sorted alphabetically by type for stable rendering.
	// Omitted when the provider does not use managed-instance inputs.
	GCEVMTypes []ConfigVMType `json:"gceVMTypes,omitempty"`
	// Managed describes provider-neutral inputs for providers that create and
	// control instances. gceVMTypes remains above for older PWA clients.
	Managed *ConfigManagedCompute `json:"managed,omitempty"`
}

type ConfigManagedCompute struct {
	Profiles              []ConfigVMType             `json:"profiles"`
	Locations             []string                   `json:"locations"`
	DiskSizesGB           []int32                    `json:"diskSizesGb"`
	SupportsInterruptible bool                       `json:"supportsInterruptible"`
	Capabilities          *ConfigComputeCapabilities `json:"capabilities,omitempty"`
}

type ConfigComputeCapabilities struct {
	CanProvision          bool                  `json:"canProvision"`
	CanDiscoverExisting   bool                  `json:"canDiscoverExisting"`
	SuspendMode           adapters.SuspendMode  `json:"suspendMode"`
	DeletionMode          adapters.DeletionMode `json:"deletionMode"`
	SupportsReliable      bool                  `json:"supportsReliable"`
	SupportsInterruptible bool                  `json:"supportsInterruptible"`
	SupportsLocations     bool                  `json:"supportsLocations"`
}

// ConfigVMType is one entry in the GCE machine-type catalog as exposed to
// the PWA. CPU is a string vCPU count (e.g. "2"); Memory is a Kubernetes
// resource.Quantity string (e.g. "4Gi", "3750Mi"). The PWA formats both
// for display.
type ConfigVMType struct {
	Type   string `json:"type"`
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

// handleConfig returns the read-only public config. GET only.
//
// Source ordering for ConfigResponse.Models (kyber#378 / PR-D of #374):
//  1. When the detection poller's cache is populated, return its model
//     list (id + context-window enriched with the operator override map).
//  2. Otherwise return tokenreport.Models() — the in-Go knownModels table
//     — so the PWA picker stays usable during a cold start before the
//     first poll completes.
//
// New PWA clients should prefer GET /api/v1/available (richer shape:
// displayName + contextWindowKnown + claudeCodeVersions). The Models
// field on /api/v1/config is retained for backward compatibility with
// clients that haven't migrated yet.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED",
			"only GET is supported on /api/v1/config")
		return
	}
	resp := ConfigResponse{
		Compute:   ConfigCompute{Provider: s.ComputeProvider},
		Models:    modelsForConfig(r.Context(), s.RuntimeDetectCache),
		Identity:  ConfigIdentity{RepoOwner: s.IdentityRepoOwner},
		PublicURL: s.PublicURL,
	}
	if s.ComputeProvider == "gce" || s.ComputeProvider == "fake" {
		profiles := activeGCEVMTypes(s.GCEVMTypeCatalog)
		resp.Compute.GCEVMTypes = profiles
		locations := []string{"local-a"}
		if s.ComputeProvider == "gce" {
			locations = []string{
				"us-central1-a", "us-central1-b", "us-central1-c",
				"us-east1-b", "us-east1-c", "us-west1-a", "us-west1-b",
				"europe-west1-b", "europe-west1-c", "asia-east1-a",
			}
		}
		resp.Compute.Managed = &ConfigManagedCompute{
			Profiles: profiles, Locations: locations,
			DiskSizesGB: []int32{20, 50, 100, 200, 500}, SupportsInterruptible: true,
		}
	}
	if s.CapacityProvider != nil {
		capabilities, err := s.CapacityProvider.Capabilities(r.Context())
		if err != nil {
			writeJSONError(w, http.StatusServiceUnavailable, "COMPUTE_UNAVAILABLE", "compute capabilities unavailable")
			return
		}
		if resp.Compute.Managed == nil {
			resp.Compute.Managed = &ConfigManagedCompute{
				Profiles: []ConfigVMType{}, Locations: []string{}, DiskSizesGB: []int32{},
			}
		}
		resp.Compute.Managed.SupportsInterruptible = capabilities.SupportsInterruptible
		resp.Compute.Managed.Capabilities = &ConfigComputeCapabilities{
			CanProvision: capabilities.CanProvision, CanDiscoverExisting: capabilities.CanDiscoverExisting,
			SuspendMode: capabilities.SuspendMode, DeletionMode: capabilities.DeletionMode,
			SupportsReliable: capabilities.SupportsReliable, SupportsInterruptible: capabilities.SupportsInterruptible,
			SupportsLocations: capabilities.SupportsLocations,
		}
		providerProfiles, err := s.CapacityProvider.Profiles(r.Context())
		if err != nil {
			writeJSONError(w, http.StatusServiceUnavailable, "COMPUTE_UNAVAILABLE", "compute profiles unavailable")
			return
		}
		if len(providerProfiles) > 0 {
			resp.Compute.Managed.Profiles = configProfiles(providerProfiles)
		}
		if locationsProvider, ok := s.CapacityProvider.(adapters.CapacityLocations); ok {
			locations, err := locationsProvider.Locations(r.Context())
			if err != nil {
				writeJSONError(w, http.StatusServiceUnavailable, "COMPUTE_UNAVAILABLE", "compute locations unavailable")
				return
			}
			resp.Compute.Managed.Locations = locations
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func configProfiles(profiles []adapters.Profile) []ConfigVMType {
	out := make([]ConfigVMType, 0, len(profiles))
	for _, profile := range profiles {
		out = append(out, ConfigVMType{Type: profile.ID, CPU: profile.CPU, Memory: profile.Memory})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// modelsForConfig returns the model list for /api/v1/config respecting
// the PR-D source ordering: detected (cache) > fallback (knownModels).
// Returns []tokenreport.Model so the existing ConfigResponse.Models
// contract is unchanged for backward-compat clients.
func modelsForConfig(ctx context.Context, cache runtimedetect.Cache) []tokenreport.Model {
	if cache == nil {
		return tokenreport.Models()
	}
	snap, err := cache.Get(ctx)
	if err != nil || snap == nil || len(snap.Models) == 0 {
		return tokenreport.Models()
	}
	out := make([]tokenreport.Model, 0, len(snap.Models))
	for _, m := range snap.Models {
		out = append(out, tokenreport.Model{
			ID:            m.ID,
			ContextWindow: m.ContextWindow,
		})
	}
	return out
}

// activeGCEVMTypes returns the configured catalog (or the built-in default
// when nil) as a sorted slice ready for JSON serialization. Sorting at the
// edge keeps the PWA dropdown order stable across pod restarts even though
// the underlying map is unordered.
func activeGCEVMTypes(catalog map[string]kyberv1.MachineCapacity) []ConfigVMType {
	src := catalog
	if src == nil {
		src = DefaultGCEVMTypeCatalog()
	}
	out := make([]ConfigVMType, 0, len(src))
	for name, cap := range src {
		out = append(out, ConfigVMType{
			Type:   name,
			CPU:    cap.CPU.String(),
			Memory: cap.Memory.String(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}
