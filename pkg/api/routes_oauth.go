package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/oauth"
	"github.com/matty-v/kyber/pkg/runtimes"
)

type reauthorizeRequest struct {
	OAuthCode    string `json:"oauthCode"`
	PkceVerifier string `json:"pkceVerifier"`
	State        string `json:"state"`
}

// handleReauthorize handles POST /api/v1/agents/{name}/oauth.
//
// Exchanges the provided PKCE authorization code for tokens, patches the
// agent's <name>-oauth Secret with the new credentials, and sets
// desiredPhase=Running to restart the agent. Used by the PWA's Re-authorize
// button when an agent is in NeedsAuth phase.
//
// Returns 204 on success, 400 on validation/exchange failure, 404 if agent
// not found, 502 if Anthropic exchange fails for non-grant reasons.
func (s *Server) handleReauthorize(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}

	var body reauthorizeRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.OAuthCode == "" || body.PkceVerifier == "" {
		writeJSONError(w, http.StatusBadRequest, "validation_error", "oauthCode and pkceVerifier are required")
		return
	}

	// Verify agent exists.
	agent := &kyberv1.Agent{}
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent '"+name+"' not found")
			return
		}
		slog.Error("failed to get agent for reauthorize", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get agent")
		return
	}

	// Caller-level authorization (kyber#474): re-auth resumes the agent to
	// Running (a lifecycle mutation), so it requires lifecycle:write — gated
	// before the token exchange so an unauthorized caller never triggers a
	// credential write. Permissive mode (default) allows but audit-logs.
	if !s.authorizePhase(w, r, name, kyberv1.AgentPhaseRunning) {
		return
	}

	descriptor, _ := runtimes.Describe(agent.Spec.Runtime)
	mode, _ := descriptor.Auth(agent.Spec.Secrets.AuthType)
	strategy, ok := runtimes.AuthenticationFor(agent.Spec.Runtime)
	if !ok || mode.Flow != "authorization-code" {
		writeJSONError(w, 409, "invalid_auth_mode", "authorization code flow is not supported for this agent")
		return
	}
	input := runtimes.AuthInput{mode.InputField: body.OAuthCode, "pkceVerifier": body.PkceVerifier, "pkceState": body.State}
	credentials, err := strategy.Prepare(r.Context(), agent.Spec.Secrets.AuthType, input, runtimes.AuthOptions{TokenURL: s.anthropicTokenURL()})
	if err != nil {
		if oauth.IsInvalidGrant(err) {
			writeJSONError(w, http.StatusBadRequest, "oauth_exchange_failed",
				"authorization code invalid or expired — try re-authorizing")
			return
		}
		writeJSONError(w, http.StatusBadGateway, "oauth_exchange_failed",
			fmt.Sprintf("Anthropic token exchange failed: %v", err))
		return
	}

	if len(credentials) != 1 || credentials[0].Suffix != mode.SecretSuffix {
		writeJSONError(w, 502, "oauth_exchange_failed", "runtime returned invalid credentials")
		return
	}
	// Patch the existing provider-owned credential Secret.
	sec := &corev1.Secret{}
	secKey := types.NamespacedName{Name: runtimes.CredentialName(agent.Spec.Runtime, name, agent.Spec.Secrets.AuthType), Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), secKey, sec); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found",
				"oauth secret '"+name+"-oauth' not found — agent may not have been created with OAuth")
			return
		}
		slog.Error("failed to get oauth secret for reauthorize", "name", name, "secret", name+"-oauth", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get oauth secret")
		return
	}
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	for k, v := range credentials[0].Data {
		sec.Data[k] = v
	}
	if err := s.K8sClient.Update(r.Context(), sec); err != nil {
		slog.Error("failed to update oauth secret", "name", name, "secret", name+"-oauth", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to update oauth secret")
		return
	}

	// Set desiredPhase=Running to trigger a restart with the new credentials.
	patch := client.MergeFrom(agent.DeepCopy())
	agent.Spec.DesiredPhase = kyberv1.AgentPhaseRunning
	if err := s.K8sClient.Patch(r.Context(), agent, patch); err != nil {
		slog.Error("failed to patch agent desired phase for reauthorize", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to update agent")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
