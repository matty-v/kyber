package api

import (
	"encoding/json"
	"errors"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/skillstore"
)

// SelfProfileResources is the requested compute allocation safe to show to
// the agent itself.
type SelfProfileResources struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
	Disk   string `json:"disk"`
}

// SelfProfileSkill is the public subset of one reported skill.
type SelfProfileSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// SelfProfile is the deliberately narrow view of an Agent CR that an agent
// may read about itself. Never add placement, network, credential, channel,
// binding, or raw status fields here.
type SelfProfile struct {
	Name             string               `json:"name"`
	Runtime          string               `json:"runtime"`
	Model            string               `json:"model,omitempty"`
	Phase            kyberv1.AgentPhase   `json:"phase,omitempty"`
	Resources        SelfProfileResources `json:"resources"`
	InstalledVersion string               `json:"installedVersion,omitempty"`
	SkillsReported   bool                 `json:"skillsReported"`
	Skills           []SelfProfileSkill   `json:"skills"`
}

func (s *InternalServer) handleSelfProfile(w http.ResponseWriter, r *http.Request, agentName string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.k8sClient == nil {
		http.Error(w, "self profile is unavailable", http.StatusServiceUnavailable)
		return
	}

	agent := &kyberv1.Agent{}
	if err := s.k8sClient.Get(r.Context(), types.NamespacedName{Name: agentName, Namespace: s.namespace}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "self profile is unavailable", http.StatusServiceUnavailable)
		return
	}

	model := agent.Status.CurrentModel
	if model == "" {
		model = agent.Spec.Model
	}
	profile := SelfProfile{
		Name:    agent.Name,
		Runtime: agent.Spec.Runtime,
		Model:   model,
		Phase:   agent.Status.Phase,
		Resources: SelfProfileResources{
			CPU:    agent.Spec.Resources.CPU.String(),
			Memory: agent.Spec.Resources.Memory.String(),
			Disk:   agent.Spec.Resources.Disk.String(),
		},
		InstalledVersion: agent.Status.Runtime.InstalledVersion,
		Skills:           []SelfProfileSkill{},
	}
	if s.skillStore != nil {
		report, err := s.skillStore.Get(r.Context(), agentName)
		if err == nil {
			profile.SkillsReported = true
			for _, skill := range report.Skills {
				profile.Skills = append(profile.Skills, SelfProfileSkill{
					Name: skill.Name, Description: skill.Description,
				})
			}
		} else if !errors.Is(err, skillstore.ErrNotFound) {
			http.Error(w, "self profile is unavailable", http.StatusServiceUnavailable)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(profile)
}
