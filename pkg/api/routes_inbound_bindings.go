package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// bindingNameRe matches the regex enforced by the CRD validator on
// AgentInboundBinding.Name. Kept in sync with the kubebuilder:validation:Pattern
// in agent_types.go.
var bindingNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// inboundRotationGrace is the window during which the previous webhook secret
// remains valid after a rotate-secret call. After this window expires, the
// receiver only accepts the current secret and the previous secret is ignored
// (though it remains in the Secret's Data until the next rotation overwrites
// it). 24h matches the typical operator turnaround for "I rotated the secret;
// please update your webhook config."
const inboundRotationGrace = 24 * time.Hour

// Keys inside a binding's HMAC k8s Secret. webhookSecretKey is shared with the
// receiver in routes_inbound.go.
const (
	webhookSecretPrevKey            = "webhook-secret-prev"
	webhookSecretPrevRotatedAtKey   = "webhook-secret-prev-rotated-at"
	webhookSecretRandomBytes        = 32
)

// inboundBindingFieldRequest mirrors kyberv1.AgentInboundField.
type inboundBindingFieldRequest struct {
	Label    string `json:"label"`
	JsonPath string `json:"jsonPath"`
	Truncate int    `json:"truncate,omitempty"`
	Prefix   string `json:"prefix,omitempty"`
}

// inboundBindingFilterRequest mirrors kyberv1.AgentInboundFilter.
type inboundBindingFilterRequest struct {
	JsonPath string   `json:"jsonPath"`
	In       []string `json:"in,omitempty"`
	Equals   string   `json:"equals,omitempty"`
}

// inboundBindingLimitsRequest mirrors kyberv1.AgentInboundLimits.
type inboundBindingLimitsRequest struct {
	MaxPerMinute int `json:"maxPerMinute,omitempty"`
}

// CreateInboundBindingRequest is the JSON body for
// POST /api/v1/agents/{name}/inbound-bindings.
//
// `existingSecret` is intentionally absent — the server auto-generates a
// dedicated Secret per binding and returns the freshly-minted HMAC value
// exactly once in the response.
type CreateInboundBindingRequest struct {
	Name            string                        `json:"name"`
	SignatureHeader string                        `json:"signatureHeader"`
	SignaturePrefix string                        `json:"signaturePrefix,omitempty"`
	EventHeader     string                        `json:"eventHeader,omitempty"`
	EventPath       string                        `json:"eventPath,omitempty"`
	MatchEvents     []string                      `json:"matchEvents,omitempty"`
	Filters         []inboundBindingFilterRequest `json:"filters,omitempty"`
	Fields          []inboundBindingFieldRequest  `json:"fields,omitempty"`
	Action          string                        `json:"action"`
	Limits          *inboundBindingLimitsRequest  `json:"limits,omitempty"`
}

// UpdateInboundBindingRequest is the JSON body for
// PATCH /api/v1/agents/{name}/inbound-bindings/{binding} (#222).
//
// All fields are optional pointers — only fields the client sends are applied.
// `name` and `existingSecret` are intentionally absent because the URL path
// pins the target binding (renaming would invalidate the upstream URL the
// operator already pasted) and rotation has its own dedicated endpoint.
//
// The struct uses pointers so we can tell "client sent zero" apart from
// "client omitted the field" — necessary for clearing list fields like
// matchEvents.
type UpdateInboundBindingRequest struct {
	SignatureHeader *string                        `json:"signatureHeader,omitempty"`
	SignaturePrefix *string                        `json:"signaturePrefix,omitempty"`
	EventHeader     *string                        `json:"eventHeader,omitempty"`
	EventPath       *string                        `json:"eventPath,omitempty"`
	MatchEvents     *[]string                      `json:"matchEvents,omitempty"`
	Filters         *[]inboundBindingFilterRequest `json:"filters,omitempty"`
	Fields          *[]inboundBindingFieldRequest  `json:"fields,omitempty"`
	Action          *string                        `json:"action,omitempty"`
	Limits          *inboundBindingLimitsRequest   `json:"limits,omitempty"`
}

// updateInboundBindingRawRequest is the decode-target for PATCH bodies.
// Keeps unknown / read-only field names so the handler can reject them with
// a useful error rather than silently ignoring an operator's edit.
type updateInboundBindingRawRequest struct {
	UpdateInboundBindingRequest
	// Read-only fields. Tracked here so the handler can detect and reject
	// them with a 400 + field; the dedicated rotate-secret endpoint owns
	// the existingSecret rename, and renaming a binding requires DELETE +
	// POST so the operator updates the upstream URL explicitly.
	Name           *string `json:"name,omitempty"`
	ExistingSecret *string `json:"existingSecret,omitempty"`
}

// inboundBindingResponse is one entry in the list/create responses. Mirrors
// the AgentInboundBinding CRD shape verbatim plus a computed `url` and
// `stats` block.
type inboundBindingResponse struct {
	Name            string                        `json:"name"`
	ExistingSecret  string                        `json:"existingSecret"`
	SignatureHeader string                        `json:"signatureHeader"`
	SignaturePrefix string                        `json:"signaturePrefix,omitempty"`
	EventHeader     string                        `json:"eventHeader,omitempty"`
	EventPath       string                        `json:"eventPath,omitempty"`
	MatchEvents     []string                      `json:"matchEvents,omitempty"`
	Filters         []inboundBindingFilterRequest `json:"filters,omitempty"`
	Fields          []inboundBindingFieldRequest  `json:"fields,omitempty"`
	Action          string                        `json:"action"`
	Limits          *inboundBindingLimitsRequest  `json:"limits,omitempty"`
	// URL is the externally-reachable webhook URL. Computed from the
	// server's PublicURL — omitted when PublicURL is unset (dev/test).
	URL string `json:"url,omitempty"`
	// Stats summarises status.inboundRuns filtered to this binding.
	Stats inboundBindingStats `json:"stats"`
}

// inboundBindingStats summarises the per-binding ring buffer. `dropped`
// always carries all 7 known drop-reason keys with zero defaults so the PWA
// can render a stable badge UI without nil-checking each.
type inboundBindingStats struct {
	LastReceivedAt  string         `json:"lastReceivedAt,omitempty"`
	TotalDispatched int            `json:"totalDispatched"`
	Dropped         map[string]int `json:"dropped"`
}

// inboundBindingListResponse is the GET response.
type inboundBindingListResponse struct {
	Bindings []inboundBindingResponse `json:"bindings"`
}

// createInboundBindingResponse is the POST response. The plaintext secret is
// returned ONCE — clients must persist it before navigating away.
type createInboundBindingResponse struct {
	Binding inboundBindingResponse `json:"binding"`
	// Secret is the hex-encoded HMAC shared secret. Returned exactly once
	// at creation time and once again on rotate-secret; never readable
	// from any GET endpoint.
	Secret string `json:"secret"`
	URL    string `json:"url,omitempty"`
}

// rotateInboundBindingResponse is the POST .../rotate-secret response.
type rotateInboundBindingResponse struct {
	Secret    string `json:"secret"`
	RotatedAt string `json:"rotatedAt"`
}

// handleAgentInboundBindings dispatches the inbound-bindings sub-tree under
// /api/v1/agents/{name}. `subpath` is whatever follows `inbound-bindings/` —
// "" for the collection, "{binding}" for a single binding, or
// "{binding}/rotate-secret" for the rotation action.
func (s *Server) handleAgentInboundBindings(w http.ResponseWriter, r *http.Request, agentName, subpath string) {
	subpath = strings.TrimPrefix(subpath, "/")
	if subpath == "" {
		switch r.Method {
		case http.MethodGet:
			s.listInboundBindings(w, r, agentName)
		case http.MethodPost:
			s.createInboundBinding(w, r, agentName)
		default:
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		}
		return
	}

	parts := strings.SplitN(subpath, "/", 2)
	bindingName := parts[0]
	if !bindingNameRe.MatchString(bindingName) {
		writeJSONError(w, http.StatusBadRequest, "invalid_binding_name", "invalid binding name")
		return
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodDelete:
			s.deleteInboundBinding(w, r, agentName, bindingName)
		case http.MethodPatch:
			s.updateInboundBinding(w, r, agentName, bindingName)
		default:
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		}
		return
	}

	// "{binding}/rotate-secret" — current secret rotation (Phase 2).
	// "{binding}/replay/{requestId}" — replay a cached envelope (Phase 3).
	subParts := strings.SplitN(parts[1], "/", 2)
	switch subParts[0] {
	case "rotate-secret":
		if len(subParts) != 1 {
			writeJSONError(w, http.StatusNotFound, "not_found", "unknown inbound-bindings action")
			return
		}
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		s.rotateInboundBindingSecret(w, r, agentName, bindingName)
	case "replay":
		if len(subParts) != 2 || subParts[1] == "" {
			writeJSONError(w, http.StatusBadRequest, "bad_request",
				"replay requires a request ID: /replay/{requestId}")
			return
		}
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		s.replayInboundRun(w, r, agentName, bindingName, subParts[1])
	default:
		writeJSONError(w, http.StatusNotFound, "not_found", "unknown inbound-bindings action")
	}
}

// listInboundBindings handles GET /api/v1/agents/{name}/inbound-bindings.
func (s *Server) listInboundBindings(w http.ResponseWriter, r *http.Request, agentName string) {
	agent := &kyberv1.Agent{}
	key := types.NamespacedName{Name: agentName, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent '"+agentName+"' not found")
			return
		}
		slog.Error("inbound-bindings: failed to get agent", "name", agentName, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get agent")
		return
	}

	bindings := make([]inboundBindingResponse, 0, len(agent.Spec.InboundBindings))
	for _, b := range agent.Spec.InboundBindings {
		bindings = append(bindings, s.bindingToResponse(agentName, b, agent.Status.InboundRuns))
	}
	writeJSON(w, http.StatusOK, inboundBindingListResponse{Bindings: bindings})
}

// createInboundBinding handles POST /api/v1/agents/{name}/inbound-bindings.
//
// Flow:
//  1. Validate body.
//  2. Fetch the agent; reject if it doesn't exist or already has a binding
//     with this name.
//  3. Generate 32 random bytes; hex-encode for the response.
//  4. Create the K8s Secret holding the raw bytes.
//  5. Patch the Agent to append the binding (existingSecret =
//     <agent>-<binding>-hmac).
//  6. On Patch failure AFTER Secret creation, best-effort delete the orphan
//     Secret so a retry gets a clean slate.
func (s *Server) createInboundBinding(w http.ResponseWriter, r *http.Request, agentName string) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB
	var req CreateInboundBindingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds 1MB")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}

	// Field-level validation. The CRD validator catches these on the eventual
	// Patch too, but surfacing them at the API edge gives the PWA a usable
	// {"error":{"field":"…"}} shape rather than a generic admission error.
	if !bindingNameRe.MatchString(req.Name) {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"name must be lowercase alphanumeric + hyphens, 1-63 chars (DNS-1123)", "name")
		return
	}
	if len(req.Name) > 63 {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"name exceeds 63 characters", "name")
		return
	}
	if req.SignatureHeader == "" {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"signatureHeader is required", "signatureHeader")
		return
	}
	if req.Action == "" {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"action is required", "action")
		return
	}

	agent := &kyberv1.Agent{}
	key := types.NamespacedName{Name: agentName, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent '"+agentName+"' not found")
			return
		}
		slog.Error("inbound-bindings: failed to get agent for create", "name", agentName, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get agent")
		return
	}

	// Name collision check — give the PWA an actionable 409 rather than
	// letting the eventual Patch silently merge two entries with the same
	// name.
	for _, b := range agent.Spec.InboundBindings {
		if b.Name == req.Name {
			writeJSONError(w, http.StatusConflict, "conflict",
				"agent '"+agentName+"' already has an inbound binding named '"+req.Name+"'")
			return
		}
	}

	// Generate 32 random bytes, hex-encode, and store the HEX STRING (as
	// ASCII bytes) in the K8s Secret. The hex string IS the HMAC key —
	// upstream providers (GitHub, Stripe, etc.) treat the user-pasted
	// secret as a literal byte sequence for HMAC, so the verifier and the
	// pasted value must agree on those exact bytes. Storing raw bytes
	// would force the operator to hex-decode before pasting, which neither
	// GitHub nor any other provider supports.
	secretBytes := make([]byte, webhookSecretRandomBytes)
	if _, err := rand.Read(secretBytes); err != nil {
		slog.Error("inbound-bindings: rand.Read failed", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to generate secret")
		return
	}
	secretHex := hex.EncodeToString(secretBytes)
	secretAtRest := []byte(secretHex)

	secretName := bindingSecretName(agentName, req.Name)
	secretObj := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: s.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kyber-api",
				"kyber.io/agent":               agentName,
				"kyber.io/binding":             req.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{webhookSecretKey: secretAtRest},
	}
	if err := s.K8sClient.Create(r.Context(), secretObj); err != nil {
		if k8serrors.IsAlreadyExists(err) {
			writeJSONError(w, http.StatusConflict, "conflict",
				"secret '"+secretName+"' already exists; a prior create may have failed mid-flow — delete it and retry")
			return
		}
		slog.Error("inbound-bindings: failed to create hmac secret",
			"agent", agentName, "binding", req.Name, "secret", secretName, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to create hmac secret")
		return
	}

	// Patch the Agent to append the binding. Retry on Conflict so two
	// operators creating differently-named bindings concurrently don't
	// race — the loser of the optimistic-concurrency race re-reads the
	// current spec and re-appends. Cap retries to avoid live-locking on a
	// genuinely broken agent state.
	binding := buildBindingFromRequest(req, secretName)
	const patchRetries = 5
	var patchErr error
	for attempt := 0; attempt < patchRetries; attempt++ {
		patch := client.MergeFrom(agent.DeepCopy())
		// Re-check name collision on each retry — another caller may have
		// claimed the name in the window between our Get and Patch.
		for _, b := range agent.Spec.InboundBindings {
			if b.Name == req.Name {
				s.cleanupOrphanSecret(secretName)
				writeJSONError(w, http.StatusConflict, "conflict",
					"agent '"+agentName+"' already has an inbound binding named '"+req.Name+"'")
				return
			}
		}
		agent.Spec.InboundBindings = append(agent.Spec.InboundBindings, binding)
		patchErr = s.K8sClient.Patch(r.Context(), agent, patch)
		if patchErr == nil {
			break
		}
		if !k8serrors.IsConflict(patchErr) {
			break
		}
		// Roll back the local mutation; re-fetch and retry.
		agent.Spec.InboundBindings = agent.Spec.InboundBindings[:len(agent.Spec.InboundBindings)-1]
		if err := s.K8sClient.Get(r.Context(), key, agent); err != nil {
			patchErr = err
			break
		}
	}
	if patchErr != nil {
		s.cleanupOrphanSecret(secretName)
		slog.Error("inbound-bindings: failed to patch agent",
			"agent", agentName, "binding", req.Name, "error", patchErr)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to attach binding to agent")
		return
	}

	resp := createInboundBindingResponse{
		Binding: s.bindingToResponse(agentName, binding, agent.Status.InboundRuns),
		Secret:  secretHex,
		URL:     s.bindingURL(agentName, req.Name),
	}
	writeJSON(w, http.StatusCreated, resp)
}

// deleteInboundBinding handles DELETE /api/v1/agents/{name}/inbound-bindings/{binding}.
func (s *Server) deleteInboundBinding(w http.ResponseWriter, r *http.Request, agentName, bindingName string) {
	agent := &kyberv1.Agent{}
	key := types.NamespacedName{Name: agentName, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent '"+agentName+"' not found")
			return
		}
		slog.Error("inbound-bindings: failed to get agent for delete", "name", agentName, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get agent")
		return
	}

	idx := -1
	var existingSecret string
	for i, b := range agent.Spec.InboundBindings {
		if b.Name == bindingName {
			idx = i
			existingSecret = b.ExistingSecret
			break
		}
	}
	if idx < 0 {
		writeJSONError(w, http.StatusNotFound, "not_found", "binding not found")
		return
	}

	patch := client.MergeFrom(agent.DeepCopy())
	agent.Spec.InboundBindings = append(
		agent.Spec.InboundBindings[:idx],
		agent.Spec.InboundBindings[idx+1:]...,
	)
	if err := s.K8sClient.Patch(r.Context(), agent, patch); err != nil {
		slog.Error("inbound-bindings: failed to patch agent on delete",
			"agent", agentName, "binding", bindingName, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to remove binding")
		return
	}

	// Delete the K8s Secret. NotFound is benign — the operator may have
	// removed it manually before calling DELETE here.
	if existingSecret != "" {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: existingSecret, Namespace: s.Namespace,
		}}
		if err := s.K8sClient.Delete(r.Context(), secret); err != nil && !k8serrors.IsNotFound(err) {
			slog.Warn("inbound-bindings: failed to delete hmac secret on binding delete",
				"agent", agentName, "binding", bindingName, "secret", existingSecret, "error", err)
			// Don't fail the request — the binding is gone from the spec
			// so the receiver no longer accepts traffic. A leaked Secret
			// is operational cleanup, not a correctness problem.
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// updateInboundBinding handles
// PATCH /api/v1/agents/{name}/inbound-bindings/{binding} (#222).
//
// Replaces the matched binding's editable fields with the values from the
// request body and patches the Agent. status.inboundRuns is preserved
// because the binding's name doesn't change — the per-binding stats stay
// associated with the same identity.
func (s *Server) updateInboundBinding(w http.ResponseWriter, r *http.Request, agentName, bindingName string) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req updateInboundBindingRawRequest
	if err := dec.Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds 1MB")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}

	// Reject read-only fields with a clear field-level 400 so the PWA can
	// surface "you can't rename a binding via PATCH".
	if req.Name != nil {
		writeJSONErrorWithField(w, http.StatusBadRequest, "READ_ONLY_FIELD",
			"name is read-only; DELETE + POST to rename a binding", "name")
		return
	}
	if req.ExistingSecret != nil {
		writeJSONErrorWithField(w, http.StatusBadRequest, "READ_ONLY_FIELD",
			"existingSecret is read-only; use rotate-secret to mint a new HMAC", "existingSecret")
		return
	}

	// Field-level validation on values the client did send.
	if req.SignatureHeader != nil && *req.SignatureHeader == "" {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"signatureHeader cannot be empty", "signatureHeader")
		return
	}
	if req.Action != nil && *req.Action == "" {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"action cannot be empty", "action")
		return
	}

	agent := &kyberv1.Agent{}
	key := types.NamespacedName{Name: agentName, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent '"+agentName+"' not found")
			return
		}
		slog.Error("inbound-bindings: failed to get agent for update", "name", agentName, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get agent")
		return
	}

	idx := -1
	for i, b := range agent.Spec.InboundBindings {
		if b.Name == bindingName {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeJSONError(w, http.StatusNotFound, "not_found", "binding not found")
		return
	}

	// Patch with retry-on-Conflict — same pattern as createInboundBinding.
	const patchRetries = 5
	var updated kyberv1.AgentInboundBinding
	var patchErr error
	for attempt := 0; attempt < patchRetries; attempt++ {
		// Re-locate the binding on each retry — concurrent edits may have
		// shuffled or removed it.
		idx = -1
		for i, b := range agent.Spec.InboundBindings {
			if b.Name == bindingName {
				idx = i
				break
			}
		}
		if idx < 0 {
			writeJSONError(w, http.StatusNotFound, "not_found", "binding not found")
			return
		}
		patch := client.MergeFrom(agent.DeepCopy())
		updated = applyBindingUpdate(agent.Spec.InboundBindings[idx], req.UpdateInboundBindingRequest)
		agent.Spec.InboundBindings[idx] = updated
		patchErr = s.K8sClient.Patch(r.Context(), agent, patch)
		if patchErr == nil {
			break
		}
		if !k8serrors.IsConflict(patchErr) {
			break
		}
		// Re-fetch and retry.
		if err := s.K8sClient.Get(r.Context(), key, agent); err != nil {
			patchErr = err
			break
		}
	}
	if patchErr != nil {
		slog.Error("inbound-bindings: failed to patch agent on update",
			"agent", agentName, "binding", bindingName, "error", patchErr)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to update binding")
		return
	}

	writeJSON(w, http.StatusOK, s.bindingToResponse(agentName, updated, agent.Status.InboundRuns))
}

// applyBindingUpdate returns a copy of `cur` with each field from `req`
// applied when present. Pure transform — exposed for unit tests.
func applyBindingUpdate(cur kyberv1.AgentInboundBinding, req UpdateInboundBindingRequest) kyberv1.AgentInboundBinding {
	out := cur
	if req.SignatureHeader != nil {
		out.SignatureHeader = *req.SignatureHeader
	}
	if req.SignaturePrefix != nil {
		out.SignaturePrefix = *req.SignaturePrefix
	}
	if req.EventHeader != nil {
		out.EventHeader = *req.EventHeader
	}
	if req.EventPath != nil {
		out.EventPath = *req.EventPath
	}
	if req.MatchEvents != nil {
		out.MatchEvents = append([]string(nil), *req.MatchEvents...)
	}
	if req.Filters != nil {
		out.Filters = make([]kyberv1.AgentInboundFilter, 0, len(*req.Filters))
		for _, f := range *req.Filters {
			out.Filters = append(out.Filters, kyberv1.AgentInboundFilter{
				JsonPath: f.JsonPath,
				In:       f.In,
				Equals:   f.Equals,
			})
		}
	}
	if req.Fields != nil {
		out.Fields = make([]kyberv1.AgentInboundField, 0, len(*req.Fields))
		for _, f := range *req.Fields {
			out.Fields = append(out.Fields, kyberv1.AgentInboundField{
				Label:    f.Label,
				JsonPath: f.JsonPath,
				Truncate: f.Truncate,
				Prefix:   f.Prefix,
			})
		}
	}
	if req.Action != nil {
		out.Action = *req.Action
	}
	if req.Limits != nil {
		out.Limits = &kyberv1.AgentInboundLimits{MaxPerMinute: req.Limits.MaxPerMinute}
	}
	return out
}

// rotateInboundBindingSecret handles
// POST /api/v1/agents/{name}/inbound-bindings/{binding}/rotate-secret.
//
// Generates fresh random bytes, archives the previous current secret as
// "webhook-secret-prev" with a "webhook-secret-prev-rotated-at" timestamp,
// and writes the new bytes as "webhook-secret". The receiver honours the
// 24h grace window so an in-flight upstream cutover doesn't drop traffic
// during the rotation.
func (s *Server) rotateInboundBindingSecret(w http.ResponseWriter, r *http.Request, agentName, bindingName string) {
	agent := &kyberv1.Agent{}
	key := types.NamespacedName{Name: agentName, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent '"+agentName+"' not found")
			return
		}
		slog.Error("inbound-bindings: failed to get agent for rotate", "name", agentName, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get agent")
		return
	}

	var existingSecret string
	for _, b := range agent.Spec.InboundBindings {
		if b.Name == bindingName {
			existingSecret = b.ExistingSecret
			break
		}
	}
	if existingSecret == "" {
		writeJSONError(w, http.StatusNotFound, "not_found", "binding not found")
		return
	}

	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{Name: existingSecret, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), secretKey, secret); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found",
				"hmac secret '"+existingSecret+"' for binding '"+bindingName+"' not found")
			return
		}
		slog.Error("inbound-bindings: failed to get hmac secret for rotate",
			"agent", agentName, "binding", bindingName, "secret", existingSecret, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to read hmac secret")
		return
	}

	// Reject a second rotation that would overwrite a still-valid prev key.
	// Without this check, an operator who rotates twice within 24h kills
	// the original secret prematurely — anyone still using it loses access
	// even though they're inside what they thought was a grace window.
	if prevTimestamp := string(secret.Data[webhookSecretPrevRotatedAtKey]); prevTimestamp != "" {
		if t, err := time.Parse(time.RFC3339, prevTimestamp); err == nil {
			if time.Since(t) < inboundRotationGrace {
				writeJSONError(w, http.StatusConflict, "rotation_in_progress",
					"a previous rotation is still inside its 24h grace window; wait for it to expire before rotating again")
				return
			}
		}
	}

	newBytes := make([]byte, webhookSecretRandomBytes)
	if _, err := rand.Read(newBytes); err != nil {
		slog.Error("inbound-bindings: rand.Read failed on rotate", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to generate secret")
		return
	}
	newHex := hex.EncodeToString(newBytes)
	rotatedAt := time.Now().UTC()

	patch := client.MergeFrom(secret.DeepCopy())
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	// Same encoding contract as create: store the hex string (as ASCII
	// bytes) so the verifier and the operator-pasted upstream value match.
	prev := secret.Data[webhookSecretKey]
	secret.Data[webhookSecretKey] = []byte(newHex)
	secret.Data[webhookSecretPrevKey] = prev
	secret.Data[webhookSecretPrevRotatedAtKey] = []byte(rotatedAt.Format(time.RFC3339))

	if err := s.K8sClient.Patch(r.Context(), secret, patch); err != nil {
		slog.Error("inbound-bindings: failed to patch hmac secret on rotate",
			"agent", agentName, "binding", bindingName, "secret", existingSecret, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to rotate hmac secret")
		return
	}

	writeJSON(w, http.StatusOK, rotateInboundBindingResponse{
		Secret:    newHex,
		RotatedAt: rotatedAt.Format(time.RFC3339),
	})
}

// bindingSecretName returns the conventional Secret name "<agent>-<binding>-hmac".
func bindingSecretName(agentName, bindingName string) string {
	return agentName + "-" + bindingName + "-hmac"
}

// bindingURL returns the externally-reachable webhook URL for a binding,
// or "" when the server has no PublicURL configured (the PWA falls back to
// rendering just the path).
func (s *Server) bindingURL(agentName, bindingName string) string {
	if s.PublicURL == "" {
		return ""
	}
	return strings.TrimRight(s.PublicURL, "/") + "/webhooks/inbound/" + agentName + "/" + bindingName
}

// cleanupOrphanSecret best-effort deletes a Secret that was created earlier
// in a request but whose follow-up Patch failed. Detached context so a
// client disconnect doesn't abandon the cleanup mid-delete.
func (s *Server) cleanupOrphanSecret(secretName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: s.Namespace}}
	if err := s.K8sClient.Delete(ctx, sec); err != nil && !k8serrors.IsNotFound(err) {
		slog.Warn("inbound-bindings: orphan secret cleanup failed",
			"secret", secretName, "error", err)
	}
}

// buildBindingFromRequest converts a CreateInboundBindingRequest into the
// CRD shape, plugging in the server-assigned existingSecret name.
func buildBindingFromRequest(req CreateInboundBindingRequest, secretName string) kyberv1.AgentInboundBinding {
	out := kyberv1.AgentInboundBinding{
		Name:            req.Name,
		ExistingSecret:  secretName,
		SignatureHeader: req.SignatureHeader,
		SignaturePrefix: req.SignaturePrefix,
		EventHeader:     req.EventHeader,
		EventPath:       req.EventPath,
		MatchEvents:     req.MatchEvents,
		Action:          req.Action,
	}
	if len(req.Filters) > 0 {
		out.Filters = make([]kyberv1.AgentInboundFilter, 0, len(req.Filters))
		for _, f := range req.Filters {
			out.Filters = append(out.Filters, kyberv1.AgentInboundFilter{
				JsonPath: f.JsonPath,
				In:       f.In,
				Equals:   f.Equals,
			})
		}
	}
	if len(req.Fields) > 0 {
		out.Fields = make([]kyberv1.AgentInboundField, 0, len(req.Fields))
		for _, f := range req.Fields {
			out.Fields = append(out.Fields, kyberv1.AgentInboundField{
				Label:    f.Label,
				JsonPath: f.JsonPath,
				Truncate: f.Truncate,
				Prefix:   f.Prefix,
			})
		}
	}
	if req.Limits != nil {
		out.Limits = &kyberv1.AgentInboundLimits{MaxPerMinute: req.Limits.MaxPerMinute}
	}
	return out
}

// bindingToResponse converts a CRD binding to the API shape and folds in
// the per-binding stats computed from the Agent's status.inboundRuns ring
// buffer.
func (s *Server) bindingToResponse(agentName string, b kyberv1.AgentInboundBinding, runs []kyberv1.AgentInboundRun) inboundBindingResponse {
	resp := inboundBindingResponse{
		Name:            b.Name,
		ExistingSecret:  b.ExistingSecret,
		SignatureHeader: b.SignatureHeader,
		SignaturePrefix: b.SignaturePrefix,
		EventHeader:     b.EventHeader,
		EventPath:       b.EventPath,
		MatchEvents:     b.MatchEvents,
		Action:          b.Action,
		URL:             s.bindingURL(agentName, b.Name),
		Stats:           computeInboundBindingStats(b.Name, runs),
	}
	if len(b.Filters) > 0 {
		resp.Filters = make([]inboundBindingFilterRequest, 0, len(b.Filters))
		for _, f := range b.Filters {
			resp.Filters = append(resp.Filters, inboundBindingFilterRequest{
				JsonPath: f.JsonPath,
				In:       f.In,
				Equals:   f.Equals,
			})
		}
	}
	if len(b.Fields) > 0 {
		resp.Fields = make([]inboundBindingFieldRequest, 0, len(b.Fields))
		for _, f := range b.Fields {
			resp.Fields = append(resp.Fields, inboundBindingFieldRequest{
				Label:    f.Label,
				JsonPath: f.JsonPath,
				Truncate: f.Truncate,
				Prefix:   f.Prefix,
			})
		}
	}
	if b.Limits != nil {
		resp.Limits = &inboundBindingLimitsRequest{MaxPerMinute: b.Limits.MaxPerMinute}
	}
	return resp
}

// computeInboundBindingStats walks status.inboundRuns and produces the
// per-binding aggregate. All 7 known drop-reason keys are emitted so the PWA
// can render a stable badge UI without nil-checking each.
func computeInboundBindingStats(bindingName string, runs []kyberv1.AgentInboundRun) inboundBindingStats {
	stats := inboundBindingStats{
		Dropped: map[string]int{
			inboundDropRateLimited:    0,
			inboundDropQueueFull:      0,
			inboundDropSigMismatch:    0,
			inboundDropMissingSecret:  0,
			inboundDropUnmatchedEvent: 0,
			inboundDropFilterRejected: 0,
			inboundDropDedup:          0,
		},
	}
	var lastStarted *metav1.Time
	for i := range runs {
		r := runs[i]
		if r.BindingName != bindingName {
			continue
		}
		if r.StartedAt != nil {
			if lastStarted == nil || r.StartedAt.After(lastStarted.Time) {
				lastStarted = r.StartedAt
			}
		}
		switch r.Outcome {
		case inboundOutcomeDispatched:
			stats.TotalDispatched++
		case inboundOutcomeDropped:
			if _, ok := stats.Dropped[r.DropReason]; ok {
				stats.Dropped[r.DropReason]++
			}
		}
	}
	if lastStarted != nil {
		stats.LastReceivedAt = lastStarted.UTC().Format(time.RFC3339)
	}
	return stats
}

