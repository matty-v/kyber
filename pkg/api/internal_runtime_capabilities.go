package api

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/runtimes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (s *InternalServer) handleRuntimeCapabilities(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if s.k8sClient == nil {
		http.Error(w, "not configured", 503)
		return
	}
	var body struct {
		Runtime          string          `json:"runtime"`
		ContractVersion  string          `json:"contractVersion"`
		InstalledVersion string          `json:"installedVersion"`
		PodUID           string          `json:"podUID"`
		AgentGeneration  int64           `json:"agentGeneration"`
		Features         map[string]bool `json:"features"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if dec.Decode(&body) != nil || dec.Decode(&struct{}{}) != io.EOF || len(body.Features) > 16 || body.InstalledVersion == "" || len(body.InstalledVersion) > 128 || body.AgentGeneration < 0 || body.PodUID == "" || body.ContractVersion != runtimes.ContractVersion {
		http.Error(w, "invalid capabilities", 400)
		return
	}
	descriptor, ok := runtimes.Describe(body.Runtime)
	if !ok {
		http.Error(w, "unregistered runtime", 400)
		return
	}
	for feature := range body.Features {
		if !descriptor.Supports(runtimes.Feature(feature)) {
			http.Error(w, "undeclared feature", 400)
			return
		}
	}
	agent := &kyberv1.Agent{}
	if s.k8sClient.Get(r.Context(), types.NamespacedName{Namespace: s.namespace, Name: name}, agent) != nil {
		http.Error(w, "agent unavailable", 404)
		return
	}
	if agent.Spec.Runtime != body.Runtime || body.AgentGeneration > agent.Generation {
		http.Error(w, "stale runtime report", 409)
		return
	}
	pod := &corev1.Pod{}
	if agent.Status.PodName == "" || s.k8sClient.Get(r.Context(), types.NamespacedName{Namespace: s.namespace, Name: agent.Status.PodName}, pod) != nil || string(pod.UID) != body.PodUID {
		http.Error(w, "stale pod report", 409)
		return
	}
	before := agent.DeepCopy()
	agent.Status.Runtime.Capabilities = &kyberv1.RuntimeCapabilitiesObservation{ContractVersion: body.ContractVersion, InstalledVersion: body.InstalledVersion, PodUID: body.PodUID, AgentGeneration: body.AgentGeneration, ObservedAt: metav1.NewTime(time.Now()), Features: body.Features}
	if s.k8sClient.Status().Patch(r.Context(), agent, client.MergeFrom(before)) != nil {
		http.Error(w, "capability update unavailable", 503)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
