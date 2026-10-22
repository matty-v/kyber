package api

import (
	"net/http"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type agentResourceChoice struct {
	Value  string `json:"value"`
	Reason string `json:"reason,omitempty"`
}

type agentResourceOptions struct {
	Machine         string                `json:"machine"`
	Provider        string                `json:"provider"`
	CurrentCPU      string                `json:"currentCPU"`
	CurrentMemory   string                `json:"currentMemory"`
	AvailableCPU    string                `json:"availableCPU"`
	AvailableMemory string                `json:"availableMemory"`
	MinimumCPU      string                `json:"minimumCPU"`
	MinimumMemory   string                `json:"minimumMemory"`
	CPUStep         string                `json:"cpuStep"`
	MemoryStep      string                `json:"memoryStep"`
	CPU             []agentResourceChoice `json:"cpu"`
	Memory          []agentResourceChoice `json:"memory"`
}

var agentCPUPresets = []string{"100m", "250m", "500m", "750m", "1", "1500m", "2", "4", "8", "16", "32"}
var agentMemoryPresets = []string{"128Mi", "256Mi", "512Mi", "768Mi", "1Gi", "1536Mi", "2Gi", "3Gi", "4Gi", "8Gi", "16Gi", "32Gi", "64Gi", "128Gi"}

func resourceChoices(values []string, available resource.Quantity, other resource.Quantity, cpuDimension bool, policy *agentAdmissionPolicy, current *kyberv1.Agent) []agentResourceChoice {
	choices := make([]agentResourceChoice, 0, len(values))
	for _, value := range values {
		choice := agentResourceChoice{Value: value}
		quantity := resource.MustParse(value)
		if quantity.Cmp(available) > 0 {
			choice.Reason = "Does not fit on this machine"
		} else if cpuDimension {
			if err := policy.check(quantity, other, current); err != nil {
				choice.Reason = err.Error()
			}
		} else if err := policy.check(other, quantity, current); err != nil {
			choice.Reason = err.Error()
		}
		choices = append(choices, choice)
	}
	return choices
}

func (s *Server) getAgentResourceOptions(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	reader := s.APIReader
	if reader == nil {
		reader = s.K8sClient
	}
	agent := &kyberv1.Agent{}
	if err := reader.Get(r.Context(), types.NamespacedName{Name: name, Namespace: s.Namespace}, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent not found")
		} else {
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to read agent")
		}
		return
	}
	s.writeAgentResourceOptions(w, r, agent.Spec.Machine, agent)
}

func (s *Server) getMachineAgentResourceOptions(w http.ResponseWriter, r *http.Request, name string) {
	s.writeAgentResourceOptions(w, r, name, nil)
}

func (s *Server) writeAgentResourceOptions(w http.ResponseWriter, r *http.Request, machineName string, agent *kyberv1.Agent) {
	reader := s.APIReader
	if reader == nil {
		reader = s.K8sClient
	}
	machine := &kyberv1.Machine{}
	if err := reader.Get(r.Context(), types.NamespacedName{Name: machineName, Namespace: s.Namespace}, machine); err != nil {
		writeJSONError(w, http.StatusConflict, "INSUFFICIENT_CAPACITY", "machine capacity unavailable")
		return
	}
	var agents kyberv1.AgentList
	if err := reader.List(r.Context(), &agents, client.InNamespace(s.Namespace)); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to read capacity")
		return
	}
	excludedName := ""
	currentCPU, currentMemory := "", ""
	if agent != nil {
		excludedName = agent.Name
		currentCPU = agent.Spec.Resources.CPU.String()
		currentMemory = agent.Spec.Resources.Memory.String()
	}
	available, err := agentResourceHeadroom(r.Context(), reader, machine, agents.Items, excludedName)
	if err != nil {
		if agent == nil {
			writeJSONError(w, http.StatusConflict, "INSUFFICIENT_CAPACITY", err.Error())
			return
		}
		// A stopped/offline Agent may still shrink its limits. Offer no increase
		// while the node's current headroom cannot be verified.
		available = MachineAvailable{CPU: agent.Spec.Resources.CPU.DeepCopy(), Memory: agent.Spec.Resources.Memory.DeepCopy()}
	}
	policy, err := loadAgentAdmissionPolicy(r.Context(), reader, s.Namespace)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	pairedCPU, pairedMemory := resource.MustParse("1"), resource.MustParse("2Gi")
	if agent != nil {
		pairedCPU, pairedMemory = agent.Spec.Resources.CPU.DeepCopy(), agent.Spec.Resources.Memory.DeepCopy()
	}
	writeJSON(w, http.StatusOK, agentResourceOptions{
		Machine: machine.Name, Provider: string(machine.Spec.Provider),
		CurrentCPU: currentCPU, CurrentMemory: currentMemory,
		AvailableCPU: available.CPU.String(), AvailableMemory: available.Memory.String(),
		MinimumCPU: setResourcesMinCPU.String(), MinimumMemory: setResourcesMinMemory.String(),
		CPUStep: "50m", MemoryStep: "128Mi",
		CPU:    resourceChoices(agentCPUPresets, available.CPU, pairedMemory, true, policy, agent),
		Memory: resourceChoices(agentMemoryPresets, available.Memory, pairedCPU, false, policy, agent),
	})
}
