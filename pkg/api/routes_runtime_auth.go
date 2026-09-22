package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/runtimes"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The canonical auth route dispatches by registered flow. Legacy /oauth and
// /codex-device-auth remain aliases with their original request/response shapes.
func (s *Server) handleRuntimeReauthorize(w http.ResponseWriter, r *http.Request, name string) {
	agent := &kyberv1.Agent{}
	if err := s.K8sClient.Get(r.Context(), types.NamespacedName{Name: name, Namespace: s.Namespace}, agent); err != nil {
		writeJSONError(w, 404, "not_found", "agent not found")
		return
	}
	descriptor, _ := runtimes.Describe(agent.Spec.Runtime)
	mode, _ := descriptor.Auth(agent.Spec.Secrets.AuthType)
	switch mode.Flow {
	case "device-code":
		s.handleCodexDeviceAuth(w, r, name)
	case "authorization-code":
		s.handleReauthorize(w, r, name)
	case "api-key":
		s.handleAPIKeyReauthorize(w, r, name, agent, mode)
	default:
		writeJSONError(w, 409, "invalid_auth_mode", "interactive authentication is not supported for this agent")
	}
}

// handleAPIKeyReauthorize writes the target harness's own Secret while the
// agent is waiting for authorization, including first use after a switch.
func (s *Server) handleAPIKeyReauthorize(w http.ResponseWriter, r *http.Request, name string, agent *kyberv1.Agent, mode runtimes.AuthMode) {
	if !s.authorizePhase(w, r, name, kyberv1.AgentPhaseRunning) {
		return
	}
	if agent.Status.Phase != kyberv1.AgentPhaseNeedsAuth {
		writeJSONError(w, http.StatusConflict, "invalid_phase", "API-key authorization requires NeedsAuth")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var body struct {
		APIKey string `json:"apiKey"`
	}
	if dec.Decode(&body) != nil || dec.Decode(&struct{}{}) != io.EOF || body.APIKey == "" || len(body.APIKey) > 8192 {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "apiKey is required and must be at most 8192 bytes", "apiKey")
		return
	}
	strategy, ok := runtimes.AuthenticationFor(agent.Spec.Runtime)
	if !ok || mode.InputField == "" {
		writeJSONError(w, http.StatusNotImplemented, "auth_unsupported", "runtime authentication unavailable")
		return
	}
	credentials, err := strategy.Prepare(r.Context(), agent.Spec.Secrets.AuthType, runtimes.AuthInput{mode.InputField: body.APIKey}, runtimes.AuthOptions{})
	if err != nil || len(credentials) != 1 || credentials[0].Suffix != mode.SecretSuffix {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid API key credential for this runtime", "apiKey")
		return
	}
	secretKey := types.NamespacedName{Name: runtimes.CredentialName(agent.Spec.Runtime, name, agent.Spec.Secrets.AuthType), Namespace: s.Namespace}
	secret := &corev1.Secret{}
	if err := s.K8sClient.Get(r.Context(), secretKey, secret); k8serrors.IsNotFound(err) {
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretKey.Name, Namespace: secretKey.Namespace}, Data: credentials[0].Data}
		if err := s.K8sClient.Create(r.Context(), secret); err != nil {
			slog.Error("failed to create runtime credential", "agent", name, "runtime", agent.Spec.Runtime, "error", err)
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to store runtime credential")
			return
		}
	} else if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to read runtime credential")
		return
	} else {
		secret.Data = credentials[0].Data
		if err := s.K8sClient.Update(r.Context(), secret); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to update runtime credential")
			return
		}
	}
	if err := s.rearmRecoveryGate(r.Context(), agent); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to rearm authorization recovery")
		return
	}
	current := &kyberv1.Agent{}
	if err := s.K8sClient.Get(r.Context(), types.NamespacedName{Name: name, Namespace: s.Namespace}, current); err != nil ||
		current.UID != agent.UID || current.Spec.Runtime != agent.Spec.Runtime || current.Spec.Secrets.AuthType != agent.Spec.Secrets.AuthType ||
		current.Status.Phase != kyberv1.AgentPhaseNeedsAuth ||
		current.Spec.DesiredPhase != kyberv1.AgentPhaseNeedsAuth && current.Spec.DesiredPhase != kyberv1.AgentPhaseRunning {
		writeJSONError(w, http.StatusConflict, "agent_changed", "agent changed during authorization; retry")
		return
	}
	before := current.DeepCopy()
	current.Spec.DesiredPhase = kyberv1.AgentPhaseRunning
	if err := s.K8sClient.Patch(r.Context(), current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		writeJSONError(w, http.StatusConflict, "agent_changed", "agent changed during authorization; retry")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
