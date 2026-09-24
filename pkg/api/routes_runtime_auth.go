package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/runtimes"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
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
	if agent.Spec.Inference != nil {
		writeJSONError(w, http.StatusConflict, "custom_inference", "agent uses a custom inference credential; update that Secret and retry startup")
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
		secret = newAgentCredentialSecret(secretKey, agent, credentials[0].Data)
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
	s.startAfterAuthorization(w, r, types.NamespacedName{Name: name, Namespace: s.Namespace}, func(current *kyberv1.Agent) bool {
		if current.UID != agent.UID || current.Spec.Runtime != agent.Spec.Runtime || current.Spec.Secrets.AuthType != agent.Spec.Secrets.AuthType {
			return false
		}
		// An agent whose intent is already Running may have left NeedsAuth on
		// the new credential before this read; that is success, not a change.
		return current.Spec.DesiredPhase == kyberv1.AgentPhaseRunning ||
			current.Spec.DesiredPhase == kyberv1.AgentPhaseNeedsAuth && current.Status.Phase == kyberv1.AgentPhaseNeedsAuth
	})
}

// startAfterAuthBackoff spans a few seconds: the conflicting write is usually
// one the (cache-backed) client has not seen yet, and a retry only succeeds
// once the cache catches up.
var startAfterAuthBackoff = wait.Backoff{Steps: 10, Duration: 50 * time.Millisecond, Factor: 1.5, Jitter: 0.1}

// startAfterAuthorization requests desiredPhase=Running once a new credential
// is stored. Storing the Secret wakes the controller, whose status writes bump
// the Agent's resourceVersion and make an optimistic patch conflict within
// milliseconds, so conflicts are retried. The credential is already saved and
// a one-time authorization code cannot be replayed, so this never answers
// 500: success is 204, and a changed or held agent is a 409 saying so.
func (s *Server) startAfterAuthorization(w http.ResponseWriter, r *http.Request, key types.NamespacedName, unchanged func(*kyberv1.Agent) bool) {
	var status int
	var code, message string
	err := retry.RetryOnConflict(startAfterAuthBackoff, func() error {
		status, code, message = 0, "", ""
		current := &kyberv1.Agent{}
		if err := s.K8sClient.Get(r.Context(), key, current); err != nil {
			return err
		}
		if !unchanged(current) {
			status, code, message = http.StatusConflict, "agent_changed", "credential saved, but the agent changed during authorization; start it when ready"
			return nil
		}
		if current.Annotations[kyberv1.AnnotationArchiveHold] != "" {
			status, code, message = http.StatusConflict, "archive_in_progress", "credential saved; start the agent once the disk archive job on its volume finishes"
			return nil
		}
		// The new Secret already reopens NeedsAuth's recovery gate, so an
		// agent whose intent is already Running needs no spec write.
		if current.Spec.DesiredPhase == kyberv1.AgentPhaseRunning {
			return nil
		}
		patch := client.MergeFromWithOptions(current.DeepCopy(), client.MergeFromWithOptimisticLock{})
		current.Spec.DesiredPhase = kyberv1.AgentPhaseRunning
		return s.K8sClient.Patch(r.Context(), current, patch)
	})
	if err != nil {
		slog.Error("credential saved but the agent could not be set to start", "agent", key.Name, "error", err)
		writeJSONError(w, http.StatusConflict, "start_pending", "credential saved, but the agent could not be started; start it again")
		return
	}
	if status != 0 {
		writeJSONError(w, status, code, message)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// The agent deletion finalizer finds credentials by this label. First-time
// authorization after a harness switch must use the same metadata as agent
// creation, or the new runtime's Secret survives agent deletion.
func newAgentCredentialSecret(key types.NamespacedName, agent *kyberv1.Agent, data map[string][]byte) *corev1.Secret {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: key.Name, Namespace: key.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kyber-api",
				"kyber.io/agent":               agent.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
	if agent.UID != "" {
		secret.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(agent, kyberv1.GroupVersion.WithKind("Agent"))}
	}
	return secret
}
