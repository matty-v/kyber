package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// RestartMachineAgentsResponse is the JSON body returned by
// POST /api/v1/machines/{id}/restart-agents. Shape locks to issue #127
// refinement D6; future rolling-restart variants can add a `mode` field
// without breaking compatibility.
type RestartMachineAgentsResponse struct {
	Restarted []string                      `json:"restarted"`
	Skipped   []RestartMachineAgentsSkipped `json:"skipped,omitempty"`
	Count     int                           `json:"count"`
}

// RestartMachineAgentsSkipped describes an agent not restarted and why.
// Reason is the agent's current phase (e.g. "Stopped", "Draining").
type RestartMachineAgentsSkipped struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// CreateMachineRequest is the JSON body for POST /api/v1/machines.
type CreateMachineRequest struct {
	Name     string `json:"name"`
	Provider string `json:"provider"` // "gce", "fake", "static", or compatibility "mock"

	// Managed-provider fields (required for gce/fake; rejected for static/mock).
	Profile           string `json:"profile,omitempty"`
	Location          string `json:"location,omitempty"`
	Interruptible     *bool  `json:"interruptible,omitempty"`
	AvailabilityClass string `json:"availabilityClass,omitempty"`
	ManagementMode    string `json:"managementMode,omitempty"`
	// Deprecated compatibility aliases retained for older clients.
	MachineType string `json:"machineType,omitempty"`
	DiskSizeGb  int32  `json:"diskSizeGb,omitempty"`
	Spot        bool   `json:"spot,omitempty"`
	Zone        string `json:"zone,omitempty"`

	// Existing-node capacity (optional for static/mock; rejected for gce/fake).
	Capacity *CreateMachineCapacity `json:"capacity,omitempty"`
}

// CreateMachineCapacity mirrors kyberv1.MachineCapacity in string form for
// the wire API — parsed into resource.Quantity on receive. EphemeralStorage
// added in #129 PR-C; optional for backward compat with older PWA clients.
type CreateMachineCapacity struct {
	CPU              string `json:"cpu"`
	Memory           string `json:"memory"`
	EphemeralStorage string `json:"ephemeralStorage,omitempty"`
}

// MachineResponse is the JSON representation of a Machine returned by the API.
type MachineResponse struct {
	ID        string                `json:"id"`
	Phase     kyberv1.MachinePhase  `json:"phase"`
	Spec      machineSpecResponse   `json:"spec"`
	Status    machineStatusResponse `json:"status,omitempty"`
	CreatedAt string                `json:"createdAt"`
}

type machineSpecResponse struct {
	Provider          string                           `json:"provider"`
	Capacity          *machineCapacityResponse         `json:"capacity,omitempty"`
	MachineType       string                           `json:"machineType,omitempty"`
	DiskSizeGb        int32                            `json:"diskSizeGb,omitempty"`
	Spot              bool                             `json:"spot,omitempty"`
	Zone              string                           `json:"zone,omitempty"`
	Profile           string                           `json:"profile,omitempty"`
	AvailabilityClass kyberv1.MachineAvailabilityClass `json:"availabilityClass,omitempty"`
	Location          string                           `json:"location,omitempty"`
	ManagementMode    kyberv1.MachineManagementMode    `json:"managementMode,omitempty"`
}

type machineCapacityResponse struct {
	CPU              string `json:"cpu"`
	Memory           string `json:"memory"`
	EphemeralStorage string `json:"ephemeralStorage,omitempty"`
}

type machineAllocatableResponse struct {
	CPU              string `json:"cpu,omitempty"`
	Memory           string `json:"memory,omitempty"`
	EphemeralStorage string `json:"ephemeralStorage,omitempty"`
}

type machineStatusResponse struct {
	Phase           kyberv1.MachinePhase            `json:"phase,omitempty"`
	Message         string                          `json:"message,omitempty"`
	InstanceId      string                          `json:"instanceId,omitempty"`
	ProviderRef     string                          `json:"providerRef,omitempty"`
	Availability    kyberv1.MachineAvailability     `json:"availability,omitempty"`
	ResolvedProfile *kyberv1.ResolvedMachineProfile `json:"resolvedProfile,omitempty"`
	ExternalIP      string                          `json:"externalIP,omitempty"`
	InternalIP      string                          `json:"internalIP,omitempty"`
	NodeName        string                          `json:"nodeName,omitempty"`
	AgentCount      int32                           `json:"agentCount,omitempty"`
	Allocatable     *machineAllocatableResponse     `json:"allocatable,omitempty"`
	// Three new capacity fields shipped by #140. Mirror the renamed
	// Status.{Observed,Assignable,Available}Capacity. PWA #142 reads these
	// directly; `allocatable` above is preserved as the legacy alias of
	// ObservedCapacity for backward compat during the migration.
	ObservedCapacity   *machineCapacityResponse `json:"observedCapacity,omitempty"`
	AssignableCapacity *machineCapacityResponse `json:"assignableCapacity,omitempty"`
	AvailableCapacity  *machineCapacityResponse `json:"availableCapacity,omitempty"`
	AvailableCPU       string                   `json:"availableCPU,omitempty"`
	AvailableMemory    string                   `json:"availableMemory,omitempty"`
}

// MachineListResponse wraps a list of machines.
type MachineListResponse struct {
	Items []MachineResponse `json:"items"`
}

// machineToResponse converts a Machine CRD to the API response shape.
func machineToResponse(m *kyberv1.Machine) MachineResponse {
	status := machineStatusResponse{
		Phase:           m.Status.Phase,
		Message:         m.Status.Message,
		InstanceId:      m.Status.InstanceId,
		ProviderRef:     m.Status.ProviderRef,
		Availability:    m.Status.Availability,
		ResolvedProfile: m.Status.ResolvedProfile,
		ExternalIP:      m.Status.ExternalIP,
		InternalIP:      m.Status.InternalIP,
		NodeName:        m.Status.NodeName,
		AgentCount:      m.Status.AgentCount,
	}
	// quantityOrEmpty returns "" when q is the zero value, otherwise q.String().
	// Important: resource.Quantity{}.String() emits "0", so naive String() leaks
	// "0" onto the wire even with omitempty (the field is non-empty after the
	// json marshal). The empty string keeps PWA tier-1 fallback honest —
	// "0" would be truthy and mask a missing-field state.
	quantityOrEmpty := func(q resource.Quantity) string {
		if q.IsZero() {
			return ""
		}
		return q.String()
	}
	if m.Status.ObservedCapacity != nil {
		status.Allocatable = &machineAllocatableResponse{
			CPU:              m.Status.ObservedCapacity.CPU.String(),
			Memory:           m.Status.ObservedCapacity.Memory.String(),
			EphemeralStorage: quantityOrEmpty(m.Status.ObservedCapacity.EphemeralStorage),
		}
		status.ObservedCapacity = &machineCapacityResponse{
			CPU:              m.Status.ObservedCapacity.CPU.String(),
			Memory:           m.Status.ObservedCapacity.Memory.String(),
			EphemeralStorage: quantityOrEmpty(m.Status.ObservedCapacity.EphemeralStorage),
		}
	}
	if m.Status.AssignableCapacity != nil {
		status.AssignableCapacity = &machineCapacityResponse{
			CPU:              m.Status.AssignableCapacity.CPU.String(),
			Memory:           m.Status.AssignableCapacity.Memory.String(),
			EphemeralStorage: quantityOrEmpty(m.Status.AssignableCapacity.EphemeralStorage),
		}
	}
	if m.Status.AvailableCapacity != nil {
		status.AvailableCapacity = &machineCapacityResponse{
			CPU:              m.Status.AvailableCapacity.CPU.String(),
			Memory:           m.Status.AvailableCapacity.Memory.String(),
			EphemeralStorage: quantityOrEmpty(m.Status.AvailableCapacity.EphemeralStorage),
		}
	}
	specResp := machineSpecResponse{
		Provider:          string(m.Spec.Provider),
		MachineType:       m.Spec.MachineType,
		DiskSizeGb:        m.Spec.DiskSizeGb,
		Spot:              m.Spec.Spot,
		Zone:              m.Spec.Zone,
		Profile:           m.Spec.Profile,
		AvailabilityClass: m.Spec.AvailabilityClass,
		Location:          m.Spec.Location,
		ManagementMode:    m.Spec.ManagementMode,
	}
	if specResp.Profile == "" {
		specResp.Profile = m.Spec.MachineType
	}
	if specResp.Location == "" {
		specResp.Location = m.Spec.Zone
	}
	if specResp.AvailabilityClass == "" && (m.Spec.MachineType != "" || m.Spec.Profile != "") {
		specResp.AvailabilityClass = kyberv1.MachineAvailabilityReliable
		if m.Spec.Spot {
			specResp.AvailabilityClass = kyberv1.MachineAvailabilityCostOptimized
		}
	}
	if specResp.ManagementMode == "" {
		if m.Spec.Provider == kyberv1.MachineProviderStatic || m.Spec.Provider == kyberv1.MachineProviderMock {
			specResp.ManagementMode = kyberv1.MachineManagementExternal
		} else {
			specResp.ManagementMode = kyberv1.MachineManagementManaged
		}
	}
	if !m.Spec.Capacity.CPU.IsZero() || !m.Spec.Capacity.Memory.IsZero() || !m.Spec.Capacity.EphemeralStorage.IsZero() {
		specResp.Capacity = &machineCapacityResponse{
			CPU:              m.Spec.Capacity.CPU.String(),
			Memory:           m.Spec.Capacity.Memory.String(),
			EphemeralStorage: quantityOrEmpty(m.Spec.Capacity.EphemeralStorage),
		}
	}
	return MachineResponse{
		ID:        m.Name,
		Phase:     m.Status.Phase,
		Spec:      specResp,
		Status:    status,
		CreatedAt: m.CreationTimestamp.UTC().Format(time.RFC3339),
	}
}

// handleMachines dispatches /api/v1/machines and /api/v1/machines/{name}[/action].
func (s *Server) handleMachines(w http.ResponseWriter, r *http.Request) {
	suffix, _ := trimPrefix(r.URL.Path, "/api/v1/machines")
	suffix = trimLeadingSlash(suffix)

	if suffix == "" {
		// Collection endpoints.
		switch r.Method {
		case http.MethodPost:
			s.createMachine(w, r)
		case http.MethodGet:
			s.listMachines(w, r)
		default:
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		}
		return
	}

	name, action, ok := splitAction(suffix)
	if !ok || !isValidName(name) {
		writeJSONError(w, http.StatusBadRequest, "invalid_name", "invalid machine name")
		return
	}

	if action == "" {
		switch r.Method {
		case http.MethodGet:
			s.getMachine(w, r, name)
		case http.MethodDelete:
			s.deleteMachine(w, r, name)
		default:
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		}
		return
	}

	// C2: logs and exec do their own method checking.
	switch action {
	case "logs":
		s.handleMachineLogs(w, r, name)
		return
	case "exec":
		s.handleMachineExec(w, r, name)
		return
	}

	// All other sub-action endpoints only accept POST.
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}

	switch action {
	case "start":
		s.setMachineDesiredPhase(w, r, name, kyberv1.MachinePhaseRunning)
	case "stop":
		s.setMachineDesiredPhase(w, r, name, kyberv1.MachinePhaseStopped)
	case "reboot":
		// Reboot: stop then start via the controller. We model this by setting
		// desiredPhase=Running (the controller stops and restarts the VM). For a
		// more explicit signal a future controller can interpret a dedicated field.
		s.setMachineDesiredPhase(w, r, name, kyberv1.MachinePhaseRunning)
	case "restart-agents":
		s.restartMachineAgents(w, r, name)
	default:
		writeJSONError(w, http.StatusNotFound, "not_found", "unknown action")
	}
}

func (s *Server) createMachine(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB
	var req CreateMachineRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds 1MB")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}

	if req.Name == "" {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "name is required", "name")
		return
	}
	if !isValidName(req.Name) {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"name must be lowercase alphanumeric + hyphens, 1-63 chars", "name")
		return
	}
	if s.ComputeProvider != "" && !providerMatchesInstall(req.Provider, s.ComputeProvider) {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			fmt.Sprintf("provider %q does not match this installation's compute provider %q", req.Provider, s.ComputeProvider),
			"provider")
		return
	}

	spec := kyberv1.MachineSpec{
		Provider:     kyberv1.MachineProvider(req.Provider),
		DesiredPhase: kyberv1.MachinePhaseRunning,
	}

	switch kyberv1.MachineProvider(req.Provider) {
	case kyberv1.MachineProviderGCE, kyberv1.MachineProviderGKE, kyberv1.MachineProviderFake:
		provider := req.Provider
		externalGKE := provider == string(kyberv1.MachineProviderGKE) && req.ManagementMode == string(kyberv1.MachineManagementExternal)
		profile := req.Profile
		if profile == "" {
			profile = req.MachineType
		}
		if req.Profile != "" && req.MachineType != "" && req.Profile != req.MachineType {
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "profile conflicts with deprecated machineType", "profile")
			return
		}
		location := req.Location
		if location == "" {
			location = req.Zone
		}
		if req.Location != "" && req.Zone != "" && req.Location != req.Zone {
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "location conflicts with deprecated zone", "location")
			return
		}
		interruptible := req.Spot
		if req.Interruptible != nil {
			interruptible = *req.Interruptible
		}
		availabilityClass := kyberv1.MachineAvailabilityReliable
		if interruptible {
			availabilityClass = kyberv1.MachineAvailabilityCostOptimized
		}
		if req.AvailabilityClass != "" {
			availabilityClass = kyberv1.MachineAvailabilityClass(req.AvailabilityClass)
			if availabilityClass != kyberv1.MachineAvailabilityReliable && availabilityClass != kyberv1.MachineAvailabilityCostOptimized {
				writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "availabilityClass must be reliable or costOptimized", "availabilityClass")
				return
			}
			classInterruptible := availabilityClass == kyberv1.MachineAvailabilityCostOptimized
			if (req.Interruptible != nil || req.Spot) && classInterruptible != interruptible {
				writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "availabilityClass conflicts with deprecated interruptible/spot intent", "availabilityClass")
				return
			}
			interruptible = classInterruptible
		}
		if req.ManagementMode != "" && req.ManagementMode != string(kyberv1.MachineManagementManaged) && !externalGKE {
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "managed providers require managementMode=Managed", "managementMode")
			return
		}
		if req.Capacity != nil {
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
				fmt.Sprintf("provider=%s: capacity is derived from machineType; do not send it", provider), "capacity")
			return
		}
		if externalGKE {
			if profile != "" || req.DiskSizeGb != 0 || req.Interruptible != nil || req.Spot || req.AvailabilityClass != "" {
				writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "external GKE capacity does not accept profile, disk, or availability settings", "managementMode")
				return
			}
			spec.Location = location
			spec.Zone = location
			spec.ManagementMode = kyberv1.MachineManagementExternal
			break
		}
		if profile == "" {
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
				fmt.Sprintf("provider=%s requires profile", provider), "profile")
			return
		}
		if provider != string(kyberv1.MachineProviderGKE) && req.DiskSizeGb < 10 {
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
				fmt.Sprintf("provider=%s requires diskSizeGb >= 10", provider), "diskSizeGb")
			return
		}
		if location == "" {
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
				fmt.Sprintf("provider=%s requires location", provider), "location")
			return
		}
		catalog := activeMachineCatalog(s.GCEVMTypeCatalog)
		profileCapacity, ok := catalog[profile]
		validProfiles := make([]string, 0, len(catalog))
		for candidate := range catalog {
			validProfiles = append(validProfiles, candidate)
		}
		if provider == string(kyberv1.MachineProviderGKE) && s.CapacityProvider != nil {
			providerProfiles, profileErr := s.CapacityProvider.Profiles(r.Context())
			if profileErr != nil {
				writeJSONError(w, http.StatusServiceUnavailable, "COMPUTE_UNAVAILABLE", "compute profiles unavailable")
				return
			}
			ok = false
			validProfiles = validProfiles[:0]
			for _, candidate := range providerProfiles {
				validProfiles = append(validProfiles, candidate.ID)
				if candidate.ID != profile {
					continue
				}
				cpu, cpuErr := resource.ParseQuantity(candidate.CPU)
				memory, memoryErr := resource.ParseQuantity(candidate.Memory)
				if cpuErr != nil || memoryErr != nil {
					writeJSONError(w, http.StatusInternalServerError, "COMPUTE_PROFILE_INVALID", "configured compute profile capacity is invalid")
					return
				}
				profileCapacity = kyberv1.MachineCapacity{CPU: cpu, Memory: memory}
				ok = true
				break
			}
		}
		if !ok {
			sort.Strings(validProfiles)
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
				fmt.Sprintf("unknown profile %q; valid profiles: %s", profile, strings.Join(validProfiles, ", ")),
				"profile")
			return
		}
		spec.MachineType = profile
		spec.Profile = profile
		spec.DiskSizeGb = req.DiskSizeGb
		spec.Spot = interruptible
		spec.AvailabilityClass = availabilityClass
		spec.Zone = location
		spec.Location = location
		spec.ManagementMode = kyberv1.MachineManagementManaged
		spec.Capacity = profileCapacity

	case kyberv1.MachineProviderMock, kyberv1.MachineProviderStatic:
		provider := req.Provider
		if req.AvailabilityClass != "" {
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "external providers do not accept availabilityClass", "availabilityClass")
			return
		}
		if req.ManagementMode != "" && req.ManagementMode != string(kyberv1.MachineManagementExternal) {
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "static providers require managementMode=External", "managementMode")
			return
		}
		spec.ManagementMode = kyberv1.MachineManagementExternal
		// Reject any GCE-only field — emit one error per offending field.
		if req.Profile != "" || req.Location != "" || req.Interruptible != nil || req.MachineType != "" {
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
				fmt.Sprintf("provider=%s: machineType/diskSizeGb/spot/zone must be absent", provider), "machineType")
			return
		}
		if req.DiskSizeGb != 0 {
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
				fmt.Sprintf("provider=%s: machineType/diskSizeGb/spot/zone must be absent", provider), "diskSizeGb")
			return
		}
		if req.Spot {
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
				fmt.Sprintf("provider=%s: machineType/diskSizeGb/spot/zone must be absent", provider), "spot")
			return
		}
		if req.Zone != "" {
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
				fmt.Sprintf("provider=%s: machineType/diskSizeGb/spot/zone must be absent", provider), "zone")
			return
		}
		var existing kyberv1.MachineList
		if err := s.K8sClient.List(r.Context(), &existing,
			client.InNamespace(s.Namespace)); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal_error",
				"list machines: "+err.Error())
			return
		}

		// The first static Machine retains the standalone single-node fallback.
		// Every additional Machine must have its own explicitly labelled Ready
		// node; otherwise two logical Machines could silently attach to the same
		// node and double-count its capacity.
		var selectedNode *corev1.Node
		if hasStaticMachine(existing.Items) {
			var nodeErr error
			selectedNode, nodeErr = pickReadyNodeForMachine(r.Context(), s.K8sClient, req.Name)
			if nodeErr != nil {
				writeJSONError(w, http.StatusConflict, "conflict", nodeErr.Error())
				return
			}
		}

		// Auto-fill capacity from the selected Ready node when omitted
		// (kyber#240). On standalone the operator shouldn't have to translate
		// `node.status.allocatable` into a number — the API does that math.
		// The controller's ComputeAssignable then subtracts the platform
		// reservation to derive status.assignableCapacity, and ComputeAvailable
		// subtracts agent requests on top.
		var cpuQ, memQ, diskQ resource.Quantity
		var err error
		if req.Capacity == nil {
			node := selectedNode
			var nodeErr error
			if node == nil {
				node, nodeErr = pickStandaloneNode(r.Context(), s.K8sClient)
			}
			if nodeErr != nil {
				writeJSONError(w, http.StatusInternalServerError, "internal_error",
					"auto-detect node capacity: "+nodeErr.Error())
				return
			}
			cpuQ = node.Status.Allocatable[corev1.ResourceCPU]
			memQ = node.Status.Allocatable[corev1.ResourceMemory]
			diskQ = node.Status.Allocatable[corev1.ResourceEphemeralStorage]
		} else {
			cpuQ, err = resource.ParseQuantity(req.Capacity.CPU)
			if err != nil {
				writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
					"capacity.cpu: "+err.Error(), "capacity.cpu")
				return
			}
			memQ, err = resource.ParseQuantity(req.Capacity.Memory)
			if err != nil {
				writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
					"capacity.memory: "+err.Error(), "capacity.memory")
				return
			}
			// EphemeralStorage is optional for backward compat with older PWAs that
			// don't send it. The mock-form picker (#129 PR-C) does send it; an
			// older client just gets a zero-disk Machine and disk-aware UX
			// degrades gracefully.
			if req.Capacity.EphemeralStorage != "" {
				diskQ, err = resource.ParseQuantity(req.Capacity.EphemeralStorage)
				if err != nil {
					writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
						"capacity.ephemeralStorage: "+err.Error(), "capacity.ephemeralStorage")
					return
				}
			}
		}
		spec.Capacity = kyberv1.MachineCapacity{CPU: cpuQ, Memory: memQ, EphemeralStorage: diskQ}

	default:
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			fmt.Sprintf("unknown provider %q (must be gce, static, fake, or mock)", req.Provider), "provider")
		return
	}
	if spec.Provider == kyberv1.MachineProviderFake {
		var existing kyberv1.MachineList
		if err := s.K8sClient.List(r.Context(), &existing, client.InNamespace(s.Namespace)); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "list machines: "+err.Error())
			return
		}
		for i := range existing.Items {
			if existing.Items[i].Spec.Provider == kyberv1.MachineProviderFake {
				writeJSONError(w, http.StatusConflict, "conflict",
					fmt.Sprintf("only one Machine is allowed when provider=fake (found %q)", existing.Items[i].Name))
				return
			}
		}
	}

	machine := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: s.Namespace,
		},
		Spec: spec,
	}

	if err := s.K8sClient.Create(r.Context(), machine); err != nil {
		if k8serrors.IsAlreadyExists(err) {
			writeJSONError(w, http.StatusConflict, "conflict", "machine '"+req.Name+"' already exists")
			return
		}
		slog.Error("failed to create machine", "machine", req.Name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to create machine: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, machineToResponse(machine))
}

func providerMatchesInstall(requested, configured string) bool {
	if requested == configured {
		return true
	}
	return (requested == string(kyberv1.MachineProviderMock) && configured == string(kyberv1.MachineProviderStatic)) ||
		(requested == string(kyberv1.MachineProviderStatic) && configured == string(kyberv1.MachineProviderMock))
}

// enrichMachineResponse adds live available resource data to a MachineResponse.
// If the node isn't ready or the query fails, the fields are left empty.
func (s *Server) enrichMachineResponse(ctx context.Context, resp *MachineResponse, nodeName string) {
	if nodeName == "" {
		return
	}
	avail, err := nodeAvailableResources(ctx, s.K8sClient, nodeName)
	if err != nil {
		return
	}
	resp.Status.AvailableCPU = avail.CPU.String()
	resp.Status.AvailableMemory = avail.Memory.String()
}

func (s *Server) listMachines(w http.ResponseWriter, r *http.Request) {
	list := &kyberv1.MachineList{}
	if err := s.K8sClient.List(r.Context(), list, client.InNamespace(s.Namespace)); err != nil {
		slog.Error("failed to list machines", "namespace", s.Namespace, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to list machines")
		return
	}

	items := make([]MachineResponse, 0, len(list.Items))
	for i := range list.Items {
		resp := machineToResponse(&list.Items[i])
		s.enrichMachineResponse(r.Context(), &resp, list.Items[i].Status.NodeName)
		items = append(items, resp)
	}
	// Sort by ID so consumers see a stable order across refetches (#263).
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	writeJSON(w, http.StatusOK, MachineListResponse{Items: items})
}

func (s *Server) getMachine(w http.ResponseWriter, r *http.Request, name string) {
	machine := &kyberv1.Machine{}
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, machine); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "machine '"+name+"' not found")
			return
		}
		slog.Error("failed to get machine", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get machine")
		return
	}
	resp := machineToResponse(machine)
	s.enrichMachineResponse(r.Context(), &resp, machine.Status.NodeName)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) deleteMachine(w http.ResponseWriter, r *http.Request, name string) {
	// kyber#565 — same two interlocks as the agent DELETE path, enforced before
	// any k8s read/mutation: an always-on ?confirm=<name> safety check, then the
	// #474 lifecycle:admin caller gate. The existing "refuse if agents attached"
	// guard below is unchanged and runs after these pass.
	if r.URL.Query().Get("confirm") != name {
		writeJSONError(w, http.StatusBadRequest, "confirmation_required",
			"delete requires ?confirm=<name> matching the machine name")
		return
	}
	if !s.authorizeAction(w, r, name, "machine-delete", ScopeLifecycleAdmin) {
		return
	}

	machine := &kyberv1.Machine{}
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, machine); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "machine '"+name+"' not found")
			return
		}
		slog.Error("failed to get machine for deletion", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get machine")
		return
	}

	// Refuse to delete a machine that still has agents attached (spec §243).
	var agentList kyberv1.AgentList
	if err := s.K8sClient.List(r.Context(), &agentList, client.InNamespace(s.Namespace)); err != nil {
		slog.Error("failed to list agents for machine deletion check", "machine", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to list agents")
		return
	}
	var attached []string
	for i := range agentList.Items {
		if agentList.Items[i].Spec.Machine == name {
			attached = append(attached, agentList.Items[i].Name)
		}
	}
	if len(attached) > 0 {
		writeJSONError(w, http.StatusUnprocessableEntity, "machine_has_agents",
			fmt.Sprintf("machine has %d attached agent(s): %s", len(attached), strings.Join(attached, ", ")))
		return
	}

	if err := s.K8sClient.Delete(r.Context(), machine); err != nil {
		if k8serrors.IsNotFound(err) {
			// NotFound is benign here — concurrent delete; treat as success.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		slog.Error("failed to delete machine", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to delete machine")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setMachineDesiredPhase(w http.ResponseWriter, r *http.Request, name string, phase kyberv1.MachinePhase) {
	machine := &kyberv1.Machine{}
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, machine); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "machine '"+name+"' not found")
			return
		}
		slog.Error("failed to get machine", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get machine")
		return
	}

	patch := client.MergeFrom(machine.DeepCopy())
	machine.Spec.DesiredPhase = phase
	if err := s.K8sClient.Patch(r.Context(), machine, patch); err != nil {
		slog.Error("failed to patch machine desired phase", "name", name, "phase", phase, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to update machine")
		return
	}

	writeJSON(w, http.StatusOK, machineToResponse(machine))
}

// trimLeadingSlash removes a single leading slash from s, if present.
func trimLeadingSlash(s string) string {
	if len(s) > 0 && s[0] == '/' {
		return s[1:]
	}
	return s
}

// restartAgentsEligiblePhases lists agent phases that participate in a
// machine-level "restart all agents" action. Anything not in this set is
// skipped with its current phase reported as the reason. Rationale (issue
// #127): restart anything the operator would expect to be active; respect
// explicit pauses (Stopped), don't interfere with in-flight infra
// transitions (Draining/WaitingForMachine), and skip tombstones (Deleted).
// Restarting is idempotent so it's safe to re-issue.
var restartAgentsEligiblePhases = map[kyberv1.AgentPhase]bool{
	kyberv1.AgentPhaseCreating:   true,
	kyberv1.AgentPhaseStarting:   true,
	kyberv1.AgentPhaseRunning:    true,
	kyberv1.AgentPhaseRestarting: true,
	kyberv1.AgentPhaseFailed:     true,
	kyberv1.AgentPhaseNeedsAuth:  true,
}

// restartMachineAgents patches spec.desiredPhase=Restarting on every eligible
// Agent scheduled to the given machine. Ineligible agents (Stopped,
// Draining, WaitingForMachine, etc.) are reported in the Skipped list with
// their current phase as the reason.
//
// Concurrency note: client.MergeFrom patches are idempotent — two simultaneous
// operators hitting this endpoint each write desiredPhase=Restarting, the
// controller's state machine sees the first transition and dedupes the second.
// The underlying CRD has no guard against out-of-band kubectl patches during
// this operation; that's acceptable per issue #127 refinement.
func (s *Server) restartMachineAgents(w http.ResponseWriter, r *http.Request, name string) {
	ctx := r.Context()

	machine := &kyberv1.Machine{}
	machineKey := types.NamespacedName{Name: name, Namespace: s.Namespace}
	if err := s.K8sClient.Get(ctx, machineKey, machine); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "machine '"+name+"' not found")
			return
		}
		slog.Error("failed to get machine", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get machine")
		return
	}

	var agentList kyberv1.AgentList
	if err := s.K8sClient.List(ctx, &agentList, client.InNamespace(s.Namespace)); err != nil {
		slog.Error("failed to list agents for machine restart", "machine", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to list agents")
		return
	}

	resp := RestartMachineAgentsResponse{
		Restarted: []string{},
	}
	for i := range agentList.Items {
		agent := &agentList.Items[i]
		if agent.Spec.Machine != name {
			continue
		}
		if !restartAgentsEligiblePhases[agent.Status.Phase] {
			reason := string(agent.Status.Phase)
			if reason == "" {
				reason = "Unknown"
			}
			resp.Skipped = append(resp.Skipped, RestartMachineAgentsSkipped{
				Name:   agent.Name,
				Reason: reason,
			})
			continue
		}
		patch := client.MergeFrom(agent.DeepCopy())
		agent.Spec.DesiredPhase = kyberv1.AgentPhaseRestarting
		if err := s.K8sClient.Patch(ctx, agent, patch); err != nil {
			slog.Error("failed to patch agent desiredPhase for machine restart",
				"machine", name, "agent", agent.Name, "error", err)
			writeJSONError(w, http.StatusInternalServerError, "internal_error",
				"failed to restart agent '"+agent.Name+"': "+err.Error())
			return
		}
		resp.Restarted = append(resp.Restarted, agent.Name)
	}
	resp.Count = len(resp.Restarted)

	if s.Recorder != nil {
		skippedSummary := ""
		if len(resp.Skipped) > 0 {
			parts := make([]string, 0, len(resp.Skipped))
			for _, sk := range resp.Skipped {
				parts = append(parts, sk.Name+": "+sk.Reason)
			}
			skippedSummary = fmt.Sprintf("; skipped %d (%s)", len(resp.Skipped), strings.Join(parts, ", "))
		}
		s.Recorder.Eventf(machine, corev1.EventTypeNormal, "MachineAgentsRestarted",
			"Triggered restart of %d agent(s) (%s)%s",
			resp.Count, strings.Join(resp.Restarted, ", "), skippedSummary)
	}

	writeJSON(w, http.StatusOK, resp)
}

// pickStandaloneNode returns the first Ready node in the cluster — used by
// POST /api/v1/machines (provider=mock) to auto-fill spec.capacity from
// node.status.allocatable when the request omits capacity. Standalone is
// expected to be a single-node cluster, so "first Ready" is unambiguous.
// Errors when no unassigned nodes exist or none are Ready (caller surfaces 500).
func pickStandaloneNode(ctx context.Context, c client.Client) (*corev1.Node, error) {
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Labels["kyber.io/machine"] != "" {
			continue
		}
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				return n, nil
			}
		}
	}
	if len(nodes.Items) == 0 {
		return nil, fmt.Errorf("no nodes in cluster")
	}
	return nil, fmt.Errorf("no Ready nodes (have %d nodes total)", len(nodes.Items))
}

func hasStaticMachine(machines []kyberv1.Machine) bool {
	for i := range machines {
		if machines[i].Spec.Provider == kyberv1.MachineProviderMock ||
			machines[i].Spec.Provider == kyberv1.MachineProviderStatic {
			return true
		}
	}
	return false
}

func pickReadyNodeForMachine(ctx context.Context, c client.Client, machineName string) (*corev1.Node, error) {
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes, client.MatchingLabels{"kyber.io/machine": machineName}); err != nil {
		return nil, fmt.Errorf("list nodes for Machine %q: %w", machineName, err)
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				return n, nil
			}
		}
	}
	return nil, fmt.Errorf("additional static Machine %q requires a distinct Ready node labelled kyber.io/machine=%s", machineName, machineName)
}
