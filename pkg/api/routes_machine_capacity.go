package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/matty-v/kyber/pkg/adapters"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

type MachinePreflightResponse struct {
	Valid    bool                   `json:"valid"`
	Resolved MachinePreflightIntent `json:"resolved"`
	Warnings []string               `json:"warnings"`
}

type MachinePreflightIntent struct {
	Provider          string                           `json:"provider"`
	Profile           string                           `json:"profile,omitempty"`
	AvailabilityClass kyberv1.MachineAvailabilityClass `json:"availabilityClass,omitempty"`
	Location          string                           `json:"location,omitempty"`
	ManagementMode    kyberv1.MachineManagementMode    `json:"managementMode"`
	DiskSizeGB        int32                            `json:"diskSizeGb,omitempty"`
	Capacity          *machineCapacityResponse         `json:"capacity,omitempty"`
}

type MachineCandidate struct {
	ID          string                   `json:"id"`
	DisplayName string                   `json:"displayName"`
	Location    string                   `json:"location,omitempty"`
	Capacity    *machineCapacityResponse `json:"capacity,omitempty"`
}

type MachineCandidatesResponse struct {
	Items []MachineCandidate `json:"items"`
}

func (s *Server) handleMachinePreflight(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only POST is supported")
		return
	}
	if s.CapacityProvider == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "COMPUTE_UNAVAILABLE", "compute provider does not support preflight")
		return
	}
	var req CreateMachineRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if s.ComputeProvider != "" && !providerMatchesInstall(req.Provider, s.ComputeProvider) {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "provider does not match this installation", "provider")
		return
	}
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
	class := kyberv1.MachineAvailabilityReliable
	if interruptible {
		class = kyberv1.MachineAvailabilityCostOptimized
	}
	if req.AvailabilityClass != "" {
		class = kyberv1.MachineAvailabilityClass(req.AvailabilityClass)
		if class != kyberv1.MachineAvailabilityReliable && class != kyberv1.MachineAvailabilityCostOptimized {
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "availabilityClass must be reliable or costOptimized", "availabilityClass")
			return
		}
		classInterruptible := class == kyberv1.MachineAvailabilityCostOptimized
		if (req.Interruptible != nil || req.Spot) && classInterruptible != interruptible {
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "availabilityClass conflicts with deprecated interruptible/spot intent", "availabilityClass")
			return
		}
		interruptible = classInterruptible
	}
	capabilities, err := s.CapacityProvider.Capabilities(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "COMPUTE_UNAVAILABLE", "compute capabilities unavailable")
		return
	}
	mode := kyberv1.MachineManagementManaged
	if !capabilities.CanProvision {
		mode = kyberv1.MachineManagementExternal
	}
	if req.ManagementMode != "" && req.ManagementMode != string(mode) {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "managementMode is not supported by this installation", "managementMode")
		return
	}
	desired := adapters.DesiredMachine{
		Availability: adapters.DesiredOnline, Profile: profile, DiskSizeGb: int(req.DiskSizeGb),
		Interruptible: interruptible, Location: location, Managed: mode == kyberv1.MachineManagementManaged,
	}
	if err := s.CapacityProvider.Validate(r.Context(), desired); err != nil {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error(), "profile")
		return
	}
	resolved := MachinePreflightIntent{
		Provider: req.Provider, Profile: profile, AvailabilityClass: class,
		Location: location, ManagementMode: mode, DiskSizeGB: req.DiskSizeGb,
	}
	if capacity, ok := activeMachineCatalog(s.GCEVMTypeCatalog)[profile]; ok {
		resolved.Capacity = &machineCapacityResponse{CPU: capacity.CPU.String(), Memory: capacity.Memory.String()}
	}
	writeJSON(w, http.StatusOK, MachinePreflightResponse{Valid: true, Resolved: resolved, Warnings: []string{}})
}

func (s *Server) handleMachineCandidates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only GET is supported")
		return
	}
	if s.CapacityProvider == nil {
		writeJSON(w, http.StatusOK, MachineCandidatesResponse{Items: []MachineCandidate{}})
		return
	}
	capabilities, err := s.CapacityProvider.Capabilities(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "COMPUTE_UNAVAILABLE", "compute capabilities unavailable")
		return
	}
	if !capabilities.CanDiscoverExisting {
		writeJSON(w, http.StatusOK, MachineCandidatesResponse{Items: []MachineCandidate{}})
		return
	}
	var nodes corev1.NodeList
	if err := s.K8sClient.List(r.Context(), &nodes); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "listing capacity candidates failed")
		return
	}
	var machines kyberv1.MachineList
	if err := s.K8sClient.List(r.Context(), &machines, client.InNamespace(s.Namespace)); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "listing registered machines failed")
		return
	}
	claimed := make(map[string]bool, len(machines.Items))
	for _, machine := range machines.Items {
		if machine.Status.NodeName != "" {
			claimed[machine.Status.NodeName] = true
		}
	}
	items := make([]MachineCandidate, 0)
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if claimed[node.Name] || node.Labels[adapters.MachineLabelKey] != "" || !candidateNodeReady(node) || isPlatformNode(node) {
			continue
		}
		items = append(items, MachineCandidate{
			ID: base64.RawURLEncoding.EncodeToString([]byte(node.Name)), DisplayName: node.Name,
			Location: node.Labels[corev1.LabelTopologyZone],
			Capacity: &machineCapacityResponse{
				CPU: node.Status.Allocatable.Cpu().String(), Memory: node.Status.Allocatable.Memory().String(),
			},
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].DisplayName < items[j].DisplayName })
	writeJSON(w, http.StatusOK, MachineCandidatesResponse{Items: items})
}

func activeMachineCatalog(catalog map[string]kyberv1.MachineCapacity) map[string]kyberv1.MachineCapacity {
	if catalog != nil {
		return catalog
	}
	return DefaultGCEVMTypeCatalog()
}

func candidateNodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func isPlatformNode(node *corev1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Key == "kyber.io/platform" {
			return true
		}
	}
	return false
}
