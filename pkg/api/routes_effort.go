package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/fleetdefaults"
	"github.com/matty-v/kyber/pkg/reasoningeffort"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type effortOptionsResponse struct {
	reasoningeffort.Options
	Runtime        string `json:"runtime"`
	RuntimeVersion string `json:"runtimeVersion"`
	Model          string `json:"model"`
}

// effortTuple resolves desired defaults, never an old installed observation.
// A missing/unavailable default does not manufacture a compatible version.
func (s *Server) effortTuple(ctx context.Context, agent *kyberv1.Agent) (string, string, error) {
	model, version := agent.Spec.Model, agent.Spec.RuntimeVersion
	if agent.Spec.Runtime != "claude-code" && agent.Spec.Runtime != "codex" {
		return model, version, nil
	}
	if model != "" && version != "" {
		return model, version, nil
	}
	if s.FleetDefaultsConfigMapName == "" {
		return model, version, nil
	}
	var cm corev1.ConfigMap
	if err := s.K8sClient.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.FleetDefaultsConfigMapName}, &cm); err != nil {
		if k8serrors.IsNotFound(err) {
			return model, version, nil
		}
		return "", "", err
	}
	modelKey, versionKey := fleetdefaults.KeyDefaultModel, fleetdefaults.KeyDefaultRuntimeVersion
	if agent.Spec.Runtime == "codex" {
		modelKey, versionKey = fleetdefaults.KeyCodexDefaultModel, fleetdefaults.KeyCodexDefaultRuntimeVersion
	}
	if model == "" {
		model = cm.Data[modelKey]
	}
	if version == "" {
		version = cm.Data[versionKey]
	}
	return model, version, nil
}

func (s *Server) validateAgentEffort(w http.ResponseWriter, r *http.Request, agent *kyberv1.Agent, custom bool) bool {
	if agent.Spec.Effort == "" {
		return true
	}
	model, version, err := s.effortTuple(r.Context(), agent)
	if err != nil {
		writeJSONError(w, 503, "effort_support_unavailable", "effort compatibility is unavailable; clear effort or retry")
		return false
	}
	if !reasoningeffort.ImageSupported(agent.Spec.Runtime, s.RuntimeImages[agent.Spec.Runtime], s.BuildSHA, agent.Status.EffortRuntimeImage) {
		writeJSONErrorWithField(w, 400, "VALIDATION_ERROR", "runtime image effort integration is unverified; update runtime images or clear effort", "effort")
		return false
	}
	if err = reasoningeffort.Validate(agent.Spec.Runtime, version, model, custom, agent.Spec.Effort); err != nil {
		writeJSONErrorWithField(w, 400, "VALIDATION_ERROR", err.Error(), "effort")
		return false
	}
	return true
}

func (s *Server) writeEffortOptions(w http.ResponseWriter, r *http.Request, agent *kyberv1.Agent, custom bool) {
	model, version, err := s.effortTuple(r.Context(), agent)
	if err != nil {
		writeJSONError(w, 503, "effort_support_unavailable", "effort compatibility is unavailable")
		return
	}
	options := reasoningeffort.Lookup(agent.Spec.Runtime, version, model, custom)
	if options.State == "supported" && !reasoningeffort.ImageSupported(agent.Spec.Runtime, s.RuntimeImages[agent.Spec.Runtime], s.BuildSHA, agent.Status.EffortRuntimeImage) {
		options = reasoningeffort.Options{State: "unknown", Levels: []string{}, Reason: "Update the runtime image to enable effort."}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, effortOptionsResponse{Options: options, Runtime: agent.Spec.Runtime, RuntimeVersion: version, Model: model})
}

func (s *Server) getCreateEffortOptions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, 405, "method_not_allowed", "only GET is supported")
		return
	}
	q := r.URL.Query()
	if len(q.Get("runtime")) > 64 || len(q.Get("model")) > 128 || len(q.Get("runtimeVersion")) > 64 {
		writeJSONError(w, 400, "bad_request", "invalid effort lookup")
		return
	}
	a := &kyberv1.Agent{Spec: kyberv1.AgentSpec{Runtime: q.Get("runtime"), Model: q.Get("model"), RuntimeVersion: q.Get("runtimeVersion")}}
	s.writeEffortOptions(w, r, a, q.Get("customEndpoint") == "true")
}

func (s *Server) getAgentEffortOptions(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		writeJSONError(w, 405, "method_not_allowed", "only GET is supported")
		return
	}
	var agent kyberv1.Agent
	if err := s.K8sClient.Get(r.Context(), types.NamespacedName{Namespace: s.Namespace, Name: name}, &agent); err != nil {
		writeJSONError(w, 404, "not_found", "agent unavailable")
		return
	}
	s.writeEffortOptions(w, r, &agent, agent.Spec.Inference != nil)
}

func (s *Server) setAgentEffort(w http.ResponseWriter, r *http.Request, name string) {
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	var req struct {
		Effort *string `json:"effort"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Effort == nil {
		writeJSONErrorWithField(w, 400, "VALIDATION_ERROR", "effort must be an explicit string; use empty to reset", "effort")
		return
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		writeJSONError(w, 400, "bad_request", "expected one JSON object")
		return
	}
	if !s.requireManagementAudit(w, r, "lifecycle:write", "agent", name, ScopeLifecycleWrite, false) {
		return
	}
	var agent kyberv1.Agent
	if err := s.K8sClient.Get(r.Context(), types.NamespacedName{Namespace: s.Namespace, Name: name}, &agent); err != nil {
		writeJSONError(w, 404, "not_found", "agent unavailable")
		return
	}
	if agent.DeletionTimestamp != nil {
		writeJSONError(w, 409, "conflict", "agent is being deleted")
		return
	}
	if agent.Spec.Effort == *req.Effort {
		completeManagementMutation(r, "lifecycle:write")
		writeJSON(w, 200, agentToResponse(&agent))
		return
	}
	proposed := agent.DeepCopy()
	proposed.Spec.Effort = *req.Effort
	if !s.validateAgentEffort(w, r, proposed, proposed.Spec.Inference != nil) {
		return
	}
	patch := client.MergeFromWithOptions(agent.DeepCopy(), client.MergeFromWithOptimisticLock{})
	agent.Spec.Effort = *req.Effort
	if agent.Status.Phase == kyberv1.AgentPhaseFailed {
		if agent.Annotations == nil {
			agent.Annotations = map[string]string{}
		}
		// This optimistic patch changes spec exactly once. A later independent
		// spec edit advances past this marker and retains normal repair behavior.
		agent.Annotations[kyberv1.AnnotationPassiveConfigGeneration] = strconv.FormatInt(agent.Generation+1, 10)
	}
	// An observed Running phase can still have a Stop/NeedsAuth intent queued.
	// A configuration edit must not revive the Agent or override that request.
	runnableIntent := agent.Spec.DesiredPhase == "" || agent.Spec.DesiredPhase == kyberv1.AgentPhaseRunning || agent.Spec.DesiredPhase == kyberv1.AgentPhaseRestarting
	if !archiveHeld(&agent) && runnableIntent {
		switch agent.Status.Phase {
		case kyberv1.AgentPhaseRunning, kyberv1.AgentPhaseStarting, kyberv1.AgentPhaseRestarting:
			agent.Spec.DesiredPhase = kyberv1.AgentPhaseRestarting
		}
	}
	beginManagementMutation(r)
	if err := s.K8sClient.Patch(r.Context(), &agent, patch); err != nil {
		if k8serrors.IsConflict(err) {
			writeJSONError(w, 409, "conflict", "agent changed; refresh before applying effort")
			return
		}
		writeJSONError(w, 500, "internal_error", "could not update effort")
		return
	}
	completeManagementMutation(r, "lifecycle:write")
	writeJSON(w, 200, agentToResponse(&agent))
}
