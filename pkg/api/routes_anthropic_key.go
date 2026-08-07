// Package api / /api/v1/settings/anthropic-key — operator-facing write
// endpoint for the Anthropic API key used by the detection poller
// (kyber#375 PR-A).
//
// Patterned on /api/v1/rotate-api-key (#143): the API patches the
// `api-key` field of the configured K8s Secret. The poller reads the
// Secret on each cycle (via the file mount), so a rotation propagates on
// the next poll without a control-plane restart.
//
// GET returns only a `configured: bool` — never the key itself, even to
// authenticated callers. Write-only from the UI is an explicit AC
// (kyber#375).

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// anthropicKeySecretField is the field name on the Secret holding the key.
// Mirrors `api-key: ...` in the helm template — same convention as the
// rotate-api-key handler.
const anthropicKeySecretField = "api-key"

// maxAnthropicKeyBytes guards the write endpoint against pathological
// payloads. Anthropic keys are short (~108 chars in practice); 2 KiB is
// generous headroom.
const maxAnthropicKeyBytes = 2 * 1024

// anthropicKeyPutRequest is the body of PUT /api/v1/settings/anthropic-key.
type anthropicKeyPutRequest struct {
	Key string `json:"key"`
}

// anthropicKeyStatusResponse is what GET returns. configured=true when the
// Secret holds a non-empty value. The key itself is never echoed.
type anthropicKeyStatusResponse struct {
	Configured bool `json:"configured"`
}

func (s *Server) handleAnthropicKeySetting(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.getAnthropicKey(w, r)
	case http.MethodPut:
		s.putAnthropicKey(w, r)
	case http.MethodDelete:
		s.deleteAnthropicKey(w, r)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET, PUT, DELETE only")
	}
}

func (s *Server) getAnthropicKey(w http.ResponseWriter, r *http.Request) {
	if s.AnthropicKeySecretName == "" {
		writeJSON(w, http.StatusOK, anthropicKeyStatusResponse{Configured: false})
		return
	}
	if s.K8sClient == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "kube_client_unavailable",
			"no kubernetes client configured")
		return
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: s.AnthropicKeySecretName, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, secret); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSON(w, http.StatusOK, anthropicKeyStatusResponse{Configured: false})
			return
		}
		slog.ErrorContext(r.Context(), "anthropic-key: secret get failed",
			"secret", s.AnthropicKeySecretName, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "secret_read_failed",
			"failed to read anthropic-key Secret")
		return
	}
	val := secret.Data[anthropicKeySecretField]
	writeJSON(w, http.StatusOK, anthropicKeyStatusResponse{Configured: len(val) > 0})
}

func (s *Server) putAnthropicKey(w http.ResponseWriter, r *http.Request) {
	if s.AnthropicKeySecretName == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "anthropic_key_not_configured",
			"detection poller's Anthropic key Secret is not configured on this control-plane")
		return
	}
	if s.K8sClient == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "kube_client_unavailable",
			"no kubernetes client configured")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxAnthropicKeyBytes+1))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "failed to read request body")
		return
	}
	if len(body) > maxAnthropicKeyBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "request_too_large",
			fmt.Sprintf("body exceeds %d bytes", maxAnthropicKeyBytes))
		return
	}
	var req anthropicKeyPutRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_json", "request body must be JSON {\"key\": \"...\"}")
		return
	}
	if req.Key == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_key", "key must be non-empty (use DELETE to clear)")
		return
	}

	if err := s.patchAnthropicKeySecret(r.Context(), req.Key); err != nil {
		// Deliberately do NOT include req.Key or err.Error() in the
		// response — the key is sensitive and k8s errors may echo the
		// request. Log the underlying error server-side.
		slog.ErrorContext(r.Context(), "anthropic-key: secret patch failed",
			"secret", s.AnthropicKeySecretName, "err", err)
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusInternalServerError, "secret_not_found",
				fmt.Sprintf("Secret %q not found in namespace %q", s.AnthropicKeySecretName, s.Namespace))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "secret_update_failed",
			"failed to update anthropic-key Secret")
		return
	}

	if s.Recorder != nil {
		secretRef := &corev1.Secret{}
		secretRef.Name = s.AnthropicKeySecretName
		secretRef.Namespace = s.Namespace
		s.Recorder.Eventf(secretRef, corev1.EventTypeNormal, "AnthropicKeyUpdated",
			"Anthropic API key updated via /api/v1/settings/anthropic-key (remote=%s)",
			r.RemoteAddr)
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteAnthropicKey(w http.ResponseWriter, r *http.Request) {
	if s.AnthropicKeySecretName == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "anthropic_key_not_configured",
			"detection poller's Anthropic key Secret is not configured on this control-plane")
		return
	}
	if s.K8sClient == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "kube_client_unavailable",
			"no kubernetes client configured")
		return
	}
	if err := s.clearAnthropicKeySecret(r.Context()); err != nil {
		slog.ErrorContext(r.Context(), "anthropic-key: secret clear failed",
			"secret", s.AnthropicKeySecretName, "err", err)
		if k8serrors.IsNotFound(err) {
			// Already gone — operator's DELETE goal is achieved.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "secret_update_failed",
			"failed to clear anthropic-key Secret")
		return
	}
	if s.Recorder != nil {
		secretRef := &corev1.Secret{}
		secretRef.Name = s.AnthropicKeySecretName
		secretRef.Namespace = s.Namespace
		s.Recorder.Eventf(secretRef, corev1.EventTypeNormal, "AnthropicKeyCleared",
			"Anthropic API key cleared via /api/v1/settings/anthropic-key (remote=%s)",
			r.RemoteAddr)
	}
	w.WriteHeader(http.StatusNoContent)
}

// patchAnthropicKeySecret writes the given key into the Secret's `api-key`
// field, leaving any other fields untouched. Mirrors patchAPIKeySecret.
func (s *Server) patchAnthropicKeySecret(ctx context.Context, newKey string) error {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: s.AnthropicKeySecretName, Namespace: s.Namespace}
	if err := s.K8sClient.Get(ctx, key, secret); err != nil {
		return err
	}
	patch := client.MergeFrom(secret.DeepCopy())
	if secret.Data == nil {
		secret.Data = make(map[string][]byte)
	}
	secret.Data[anthropicKeySecretField] = []byte(newKey)
	return s.K8sClient.Patch(ctx, secret, patch)
}

// clearAnthropicKeySecret blanks out the `api-key` field on the Secret.
// Other fields are left alone so a future tenant of the same Secret isn't
// accidentally clobbered.
func (s *Server) clearAnthropicKeySecret(ctx context.Context) error {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: s.AnthropicKeySecretName, Namespace: s.Namespace}
	if err := s.K8sClient.Get(ctx, key, secret); err != nil {
		return err
	}
	patch := client.MergeFrom(secret.DeepCopy())
	if secret.Data == nil {
		secret.Data = make(map[string][]byte)
	}
	secret.Data[anthropicKeySecretField] = []byte("")
	return s.K8sClient.Patch(ctx, secret, patch)
}
