package api

import (
	"log/slog"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// handleCodexDeviceAuth discards a Codex subscription credential and starts a
// fresh in-pod `codex login --device-auth` attempt. The placeholder Secret is
// intentional: its new resourceVersion opens the NeedsAuth recovery gate, and
// start-codex.sh replaces {} through the interactive device flow. The existing
// Codex credential syncer then persists the resulting auth.json back here.
func (s *Server) handleCodexDeviceAuth(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}

	agent := &kyberv1.Agent{}
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent '"+name+"' not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get agent")
		return
	}
	if agent.Spec.Runtime != "codex" || agent.Spec.Secrets.AuthType != kyberv1.AgentAuthTypeOAuth {
		writeJSONErrorWithField(w, http.StatusConflict, "invalid_auth_mode",
			"device auth is available only for Codex agents using a ChatGPT subscription", "secrets.authType")
		return
	}
	if !s.authorizePhase(w, r, name, kyberv1.AgentPhaseRunning) {
		return
	}

	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{Name: name + "-codex-auth", Namespace: s.Namespace}
	err := s.K8sClient.Get(r.Context(), secretKey, secret)
	switch {
	case k8serrors.IsNotFound(err):
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretKey.Name, Namespace: secretKey.Namespace},
			Data:       map[string][]byte{"auth.json": []byte("{}")},
		}
		if err := s.K8sClient.Create(r.Context(), secret); err != nil {
			slog.Error("failed to create codex auth secret", "name", name, "error", err)
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to start Codex device auth")
			return
		}
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to read Codex auth secret")
		return
	default:
		secret.Data = map[string][]byte{"auth.json": []byte("{}")}
		if err := s.K8sClient.Update(r.Context(), secret); err != nil {
			slog.Error("failed to reset codex auth secret", "name", name, "error", err)
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to start Codex device auth")
			return
		}
	}

	// Re-arm the recovery gate BEFORE the spec patch, exactly as an explicit
	// Start does. Writing {} above is not enough on its own: on every retry the
	// Secret already holds {}, Kubernetes does not bump resourceVersion for a
	// byte-identical update, and the controller's claim still matches — so the
	// gate stays shut and this endpoint becomes a silent no-op that still
	// answers 204. See rearmRecoveryGate for the full reasoning.
	if err := s.rearmRecoveryGate(r.Context(), agent); err != nil {
		slog.Error("failed to clear recovery input", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to start Codex device auth")
		return
	}

	patch := client.MergeFrom(agent.DeepCopy())
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseRunning
	if err := s.K8sClient.Patch(r.Context(), agent, patch); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to restart agent")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
