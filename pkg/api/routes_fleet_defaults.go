package api

import (
	"encoding/json"
	"errors"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/matty-v/kyber/pkg/fleetdefaults"
)

const fleetDefaultsAuthoredAnnotation = "kyber.io/fleet-defaults-authored"

// FleetDefaultsResponse is the JSON contract for GET /api/v1/fleet-defaults
// and the request body for PUT /api/v1/fleet-defaults.
//
// Empty strings are valid (treated as "not configured" — the reconciler
// then leaves spec.model unresolved unless the agent sets its own). The
// PWA renders empty values as placeholder hints in the input fields.
type FleetDefaultsResponse struct {
	DefaultModel               string `json:"defaultModel"`
	DefaultRuntimeVersion      string `json:"defaultRuntimeVersion"`
	CodexDefaultModel          string `json:"codexDefaultModel"`
	CodexDefaultRuntimeVersion string `json:"codexDefaultRuntimeVersion"`
}

// handleFleetDefaults serves both GET (read the kyber-fleet-defaults
// ConfigMap) and PUT (PATCH it with the request body's keys). PUT also
// invalidates the reconciler-side cache so the writer sees their own
// edit on the next reconcile without waiting for ResolveCacheTTL.
//
// kyber#376 / PR-B of #374.
func (s *Server) handleFleetDefaults(w http.ResponseWriter, r *http.Request) {
	if s.FleetDefaultsConfigMapName == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "FLEET_DEFAULTS_NOT_CONFIGURED",
			"Fleet defaults ConfigMap name not configured on this control plane.")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleFleetDefaultsGet(w, r)
	case http.MethodPut:
		s.handleFleetDefaultsPut(w, r)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED",
			"only GET and PUT are supported on /api/v1/fleet-defaults")
	}
}

func (s *Server) handleFleetDefaultsGet(w http.ResponseWriter, r *http.Request) {
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: s.Namespace, Name: s.FleetDefaultsConfigMapName}
	if err := s.K8sClient.Get(r.Context(), key, cm); err != nil {
		if apierrors.IsNotFound(err) {
			// The ConfigMap may not exist yet on a fresh chart install —
			// return an empty response so the PWA renders blank inputs
			// rather than a scary 404 banner.
			writeJSON(w, http.StatusOK, FleetDefaultsResponse{})
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "FLEET_DEFAULTS_READ_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, FleetDefaultsResponse{
		DefaultModel:               cm.Data[fleetdefaults.KeyDefaultModel],
		DefaultRuntimeVersion:      cm.Data[fleetdefaults.KeyDefaultRuntimeVersion],
		CodexDefaultModel:          cm.Data[fleetdefaults.KeyCodexDefaultModel],
		CodexDefaultRuntimeVersion: cm.Data[fleetdefaults.KeyCodexDefaultRuntimeVersion],
	})
}

func (s *Server) handleFleetDefaultsPut(w http.ResponseWriter, r *http.Request) {
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeJSONError(w, http.StatusBadRequest, "INVALID_BODY",
			"request body is not valid JSON: "+err.Error())
		return
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "INVALID_BODY", "request body is not valid JSON")
		return
	}
	var req FleetDefaultsResponse
	if err := json.Unmarshal(encoded, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "INVALID_BODY", "request body has invalid fields: "+err.Error())
		return
	}
	_, codexModelSet := raw["codexDefaultModel"]
	_, codexVersionSet := raw["codexDefaultRuntimeVersion"]
	// Build the desired ConfigMap state. Create-or-update: if it doesn't
	// exist (fresh install, never seeded), we create it now so the PWA's
	// first edit lands cleanly.
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        s.FleetDefaultsConfigMapName,
			Namespace:   s.Namespace,
			Annotations: map[string]string{fleetDefaultsAuthoredAnnotation: "true"},
		},
		Data: map[string]string{
			fleetdefaults.KeyDefaultModel:               req.DefaultModel,
			fleetdefaults.KeyDefaultRuntimeVersion:      req.DefaultRuntimeVersion,
			fleetdefaults.KeyCodexDefaultModel:          req.CodexDefaultModel,
			fleetdefaults.KeyCodexDefaultRuntimeVersion: req.CodexDefaultRuntimeVersion,
		},
	}

	existing := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: s.Namespace, Name: s.FleetDefaultsConfigMapName}
	err = s.K8sClient.Get(r.Context(), key, existing)
	switch {
	case err == nil:
		// Preserve labels/annotations the chart owns; just patch Data.
		before := existing.DeepCopy()
		if existing.Data == nil {
			existing.Data = map[string]string{}
		}
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		existing.Annotations[fleetDefaultsAuthoredAnnotation] = "true"
		existing.Data[fleetdefaults.KeyDefaultModel] = req.DefaultModel
		existing.Data[fleetdefaults.KeyDefaultRuntimeVersion] = req.DefaultRuntimeVersion
		if codexModelSet {
			existing.Data[fleetdefaults.KeyCodexDefaultModel] = req.CodexDefaultModel
		} else {
			req.CodexDefaultModel = existing.Data[fleetdefaults.KeyCodexDefaultModel]
		}
		if codexVersionSet {
			existing.Data[fleetdefaults.KeyCodexDefaultRuntimeVersion] = req.CodexDefaultRuntimeVersion
		} else {
			req.CodexDefaultRuntimeVersion = existing.Data[fleetdefaults.KeyCodexDefaultRuntimeVersion]
		}
		if patchErr := s.K8sClient.Patch(r.Context(), existing, client.MergeFrom(before)); patchErr != nil {
			writeJSONError(w, http.StatusInternalServerError, "FLEET_DEFAULTS_WRITE_ERROR", patchErr.Error())
			return
		}
	case apierrors.IsNotFound(err):
		if createErr := s.K8sClient.Create(r.Context(), desired); createErr != nil {
			// Race: another writer created the CM between Get and Create.
			// Fall through to a retry-patch on the freshly-created object.
			if !apierrors.IsAlreadyExists(createErr) {
				writeJSONError(w, http.StatusInternalServerError, "FLEET_DEFAULTS_WRITE_ERROR", createErr.Error())
				return
			}
			// Re-fetch + patch to converge on the request body.
			if getErr := s.K8sClient.Get(r.Context(), key, existing); getErr != nil {
				writeJSONError(w, http.StatusInternalServerError, "FLEET_DEFAULTS_WRITE_ERROR", getErr.Error())
				return
			}
			before := existing.DeepCopy()
			if existing.Data == nil {
				existing.Data = map[string]string{}
			}
			if existing.Annotations == nil {
				existing.Annotations = map[string]string{}
			}
			existing.Annotations[fleetDefaultsAuthoredAnnotation] = "true"
			existing.Data[fleetdefaults.KeyDefaultModel] = req.DefaultModel
			existing.Data[fleetdefaults.KeyDefaultRuntimeVersion] = req.DefaultRuntimeVersion
			if codexModelSet {
				existing.Data[fleetdefaults.KeyCodexDefaultModel] = req.CodexDefaultModel
			} else {
				req.CodexDefaultModel = existing.Data[fleetdefaults.KeyCodexDefaultModel]
			}
			if codexVersionSet {
				existing.Data[fleetdefaults.KeyCodexDefaultRuntimeVersion] = req.CodexDefaultRuntimeVersion
			} else {
				req.CodexDefaultRuntimeVersion = existing.Data[fleetdefaults.KeyCodexDefaultRuntimeVersion]
			}
			if patchErr := s.K8sClient.Patch(r.Context(), existing, client.MergeFrom(before)); patchErr != nil {
				writeJSONError(w, http.StatusInternalServerError, "FLEET_DEFAULTS_WRITE_ERROR", patchErr.Error())
				return
			}
		}
	default:
		var statusErr *apierrors.StatusError
		_ = errors.As(err, &statusErr)
		writeJSONError(w, http.StatusInternalServerError, "FLEET_DEFAULTS_READ_ERROR", err.Error())
		return
	}

	// Drop the reconciler-side cache so the next reconcile re-fetches.
	// Otherwise an operator who edits + immediately restarts an agent
	// would still see the old default for up to ResolveCacheTTL.
	if s.FleetDefaultsInvalidator != nil {
		s.FleetDefaultsInvalidator.Invalidate()
	}

	writeJSON(w, http.StatusOK, req)
}
