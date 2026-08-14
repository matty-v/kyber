// POST /api/v1/rotate-api-key — generate a new API key, persist it to the
// kyber-api-credentials Secret, swap it into the running authenticator, and
// return the plaintext key once. (#143)
//
// In-memory rotation: the authenticator's key is mutated atomically via
// SetKey, so the new key authenticates immediately and the old key returns
// 401 on the next request. No pod restart is required — the env-var-derived
// key only matters at process start; once the process is running, we drive
// authentication off the runtime-mutable APIKeyAuthenticator.
//
// The Secret update is the persistence layer: a future restart of the
// control-plane pod will read the new key from the Secret via the existing
// KYBER_API_KEY env var, so the rotation survives pod recycle.

package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// apiKeyByteLen is the entropy size of the generated key. 32 bytes →
// 64 hex characters. Matches the `openssl rand -hex 32` helm install
// guidance documented in docs/installation.md.
const apiKeyByteLen = 32

// apiKeySecretField is the key on the Secret's data map holding the API key.
// Mirrors `key: api-key` in the helm Secret template.
const apiKeySecretField = "api-key"

// handleRotateAPIKey is wired in registerProtectedRoutes. The handler is
// gated by API-key auth like every other protected route — only an
// already-authenticated session can rotate. There is no opt-out.
func (s *Server) handleRotateAPIKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}

	if s.APIKeySecretName == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "rotation_unavailable",
			"API key rotation is not configured on this control-plane (KYBER_API_KEY_SECRET_NAME unset)")
		return
	}
	if s.auth == nil {
		// Defensive — buildTopHandler should have populated this.
		writeJSONError(w, http.StatusInternalServerError, "rotation_unavailable",
			"authenticator not initialized")
		return
	}

	// Generate a replacement browser credential before changing persistent or
	// in-memory state. This keeps a random-source failure from leaving the
	// initiating browser signed out after an otherwise successful rotation.
	_, cookieErr := r.Cookie(browserSessionCookie)
	usedBrowserSession := cookieErr == nil
	var browserCaller *Caller
	var replacementToken string
	if usedBrowserSession {
		browserCaller = callerFrom(r.Context())
		if browserCaller == nil {
			writeJSONError(w, http.StatusInternalServerError, "session_creation_failed", "authenticated caller missing")
			return
		}
		var err error
		replacementToken, err = generateBrowserSessionToken()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "session_creation_failed", "failed to refresh browser session")
			return
		}
	}

	newKey, err := generateAPIKey()
	if err != nil {
		slog.ErrorContext(r.Context(), "rotate-api-key: generate failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "key_generation_failed",
			"failed to generate new key")
		return
	}

	// Persist to the Secret first. If this fails, we leave the in-memory key
	// alone — the rotation is rolled back to the pre-rotation state implicitly.
	if err := s.patchAPIKeySecret(r.Context(), newKey); err != nil {
		slog.ErrorContext(r.Context(), "rotate-api-key: secret patch failed",
			"secret", s.APIKeySecretName, "err", err)
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusInternalServerError, "secret_not_found",
				fmt.Sprintf("Secret %q not found in namespace %q", s.APIKeySecretName, s.Namespace))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "secret_update_failed",
			"failed to update API key Secret")
		return
	}

	// Swap the in-memory key. After this returns, the OLD key returns 401
	// and the NEW key authenticates. The seed APIKey field is updated too so
	// any code that reads s.APIKey (none today, but defensive) sees the new
	// value.
	s.auth.SetKey(newKey)
	s.APIKey = newKey

	// Rotation revokes every browser session along with the old legacy key.
	// If this request itself used a browser session, immediately issue a fresh
	// session for the authenticated caller so the initiating browser stays in.
	if usedBrowserSession {
		s.auth.ReplaceBrowserSessions(replacementToken, *browserCaller)
		setBrowserSessionCookie(w, r, replacementToken)
	} else {
		s.auth.ClearBrowserSessions()
	}

	// Audit trail via k8s Event. Target the Secret since that's the object
	// that just changed; reason "ApiKeyRotated" is greppable in `kubectl get
	// events`.
	if s.Recorder != nil {
		secretRef := &corev1.Secret{}
		secretRef.Name = s.APIKeySecretName
		secretRef.Namespace = s.Namespace
		s.Recorder.Eventf(secretRef, corev1.EventTypeNormal, "ApiKeyRotated",
			"Kyber control-plane API key rotated via /api/v1/rotate-api-key (remote=%s)",
			r.RemoteAddr)
	}

	writeJSON(w, http.StatusOK, map[string]string{"apiKey": newKey})
}

// generateAPIKey returns a hex-encoded random key. The encoding matches
// `openssl rand -hex 32` from the helm install docs so operators rotating
// via this endpoint produce keys indistinguishable from manually-issued
// ones.
func generateAPIKey() (string, error) {
	buf := make([]byte, apiKeyByteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// patchAPIKeySecret merge-patches the api-key field on the configured
// Secret. The other fields on the Secret (webhook-secret, k3s-join-token,
// etc.) are left untouched.
func (s *Server) patchAPIKeySecret(ctx context.Context, newKey string) error {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: s.APIKeySecretName, Namespace: s.Namespace}
	if err := s.K8sClient.Get(ctx, key, secret); err != nil {
		return err
	}
	patch := client.MergeFrom(secret.DeepCopy())
	if secret.Data == nil {
		secret.Data = make(map[string][]byte)
	}
	secret.Data[apiKeySecretField] = []byte(newKey)
	return s.K8sClient.Patch(ctx, secret, patch)
}
