package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/controllers/agent"
	"github.com/matty-v/kyber/pkg/usersecrets"
)

// userSecretsMetadataAnnotation is the annotation key on each user-secrets
// Secret holding a JSON map of per-entry metadata. Keyed by the user-facing
// (uppercase) key; the entry includes sha256 hex prefix and RFC3339 timestamps.
// Kind and size are not stored here — kind is implied by which Secret the
// annotation lives on, and size is derived from the Data entry length.
const userSecretsMetadataAnnotation = "kyber.io/user-secrets-metadata"

// userSecretSha256PrefixLen is the number of hex characters of the sha256
// digest retained in per-entry metadata for display. Full digest would leak
// no value beyond confirmation that two entries share content, which the
// prefix already covers without bloating the list payload.
const userSecretSha256PrefixLen = 12

// maxUserSecretsMultipartBytes bounds the in-memory buffer for multipart PUT
// uploads. 1 MiB is well above the per-entry (64 KiB) and aggregate (256 KiB)
// limits defined in #75 — we still reject anything over those at the validation
// layer, but need some headroom for the multipart envelope itself.
const maxUserSecretsMultipartBytes = 1 << 20

// userSecretKind identifies which of the two backing Secrets an entry lives in.
type userSecretKind string

const (
	userSecretKindKV   userSecretKind = "kv"
	userSecretKindFile userSecretKind = "file"
)

// userSecretEntryMeta is the per-entry metadata persisted in the Secret's
// annotations and returned by the list endpoint.
type userSecretEntryMeta struct {
	Sha256Prefix string `json:"sha256Prefix"`
	CreatedAt    string `json:"createdAt"`
	UpdatedAt    string `json:"updatedAt"`
}

// userSecretListItem is a single entry in the GET /secrets list response.
type userSecretListItem struct {
	Key          string `json:"key"`
	Kind         string `json:"kind"`
	Size         int    `json:"size"`
	Sha256Prefix string `json:"sha256Prefix"`
	CreatedAt    string `json:"createdAt"`
	UpdatedAt    string `json:"updatedAt"`
}

// userSecretListResponse is the shape returned by GET /api/v1/agents/{name}/secrets.
type userSecretListResponse struct {
	Items []userSecretListItem `json:"items"`
}

// putUserSecretKVBody is the JSON body accepted by PUT /secrets/{key} with
// Content-Type application/json.
type putUserSecretKVBody struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// handleUserSecrets dispatches /api/v1/agents/{name}/secrets{/key} to the
// appropriate kind-aware handler. Called from handleAgents when the action
// starts with "secrets".
func (s *Server) handleUserSecrets(w http.ResponseWriter, r *http.Request, agentName, action string) {
	// action is "secrets" (list) or "secrets/{key}" (single entry).
	rest := strings.TrimPrefix(action, "secrets")
	rest = strings.TrimPrefix(rest, "/")

	if rest == "" {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		s.listUserSecrets(w, r, agentName)
		return
	}

	// rest is "{key}" (no further path segments allowed).
	if strings.Contains(rest, "/") {
		writeJSONError(w, http.StatusNotFound, "not_found", "unknown secrets path")
		return
	}
	key := rest

	switch r.Method {
	case http.MethodPut:
		s.putUserSecret(w, r, agentName, key)
	case http.MethodGet:
		s.getUserSecret(w, r, agentName, key)
	case http.MethodDelete:
		s.deleteUserSecret(w, r, agentName, key)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

// listUserSecrets handles GET /api/v1/agents/{name}/secrets.
// Returns all entries across both kv and file Secrets, sorted by key.
// Never returns values — only metadata.
func (s *Server) listUserSecrets(w http.ResponseWriter, r *http.Request, agentName string) {
	if _, err := s.getAgentOr404(r, w, agentName); err != nil {
		return
	}

	items := []userSecretListItem{}
	for _, kind := range []userSecretKind{userSecretKindKV, userSecretKindFile} {
		sec, err := s.getUserSecretsSecret(r, agentName, kind)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to read user-secrets Secret")
			return
		}
		if sec == nil {
			// Shell Secret not yet created (e.g. reconciler hasn't run yet). Treat as empty.
			continue
		}
		meta := readUserSecretMetadata(sec)
		for key, dataKey := range enumerateUserSecretKeys(sec, kind) {
			m := meta[key]
			items = append(items, userSecretListItem{
				Key:          key,
				Kind:         string(kind),
				Size:         len(sec.Data[dataKey]),
				Sha256Prefix: m.Sha256Prefix,
				CreatedAt:    m.CreatedAt,
				UpdatedAt:    m.UpdatedAt,
			})
		}
	}

	sort.Slice(items, func(i, j int) bool { return items[i].Key < items[j].Key })
	writeJSON(w, http.StatusOK, userSecretListResponse{Items: items})
}

// getUserSecret handles GET /api/v1/agents/{name}/secrets/{key}.
// Returns text/plain for kv, application/octet-stream for file.
func (s *Server) getUserSecret(w http.ResponseWriter, r *http.Request, agentName, key string) {
	if err := usersecrets.ValidateKey(key); err != nil {
		writeUserSecretValidationError(w, "key", err)
		return
	}
	if _, err := s.getAgentOr404(r, w, agentName); err != nil {
		return
	}

	// Try kv first, then file. At most one will have the entry (PUT enforces uniqueness).
	for _, kind := range []userSecretKind{userSecretKindKV, userSecretKindFile} {
		sec, err := s.getUserSecretsSecret(r, agentName, kind)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to read user-secrets Secret")
			return
		}
		if sec == nil {
			continue
		}
		dataKey := userSecretDataKey(key, kind)
		value, ok := sec.Data[dataKey]
		if !ok {
			continue
		}
		writeUserSecretValue(w, key, kind, value)
		return
	}

	writeJSONError(w, http.StatusNotFound, "not_found", "secret key not set")
}

// putUserSecret handles PUT /api/v1/agents/{name}/secrets/{key}. Content-Type
// selects kind: application/json → kv, multipart/form-data → file. On success
// the entry replaces any existing entry (including one in the other kind).
// Creating a new entry never rolls the agent; replacing an env-projected entry
// or changing an entry's kind rolls a live agent so stale env state is removed.
func (s *Server) putUserSecret(w http.ResponseWriter, r *http.Request, agentName, key string) {
	if err := usersecrets.ValidateKey(key); err != nil {
		writeUserSecretValidationError(w, "key", err)
		return
	}

	agent, err := s.getAgentOr404(r, w, agentName)
	if err != nil {
		return
	}

	kind, value, err := parseUserSecretPutBody(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "validation_error", err.Error())
		return
	}

	if err := usersecrets.ValidateEntrySize(len(value)); err != nil {
		writeUserSecretValidationError(w, "value", err)
		return
	}

	updatedExisting, err := s.userSecretEntryExists(r, agentName, key, kind)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to inspect prior secret entry")
		return
	}

	// Aggregate check BEFORE any mutation — computeUserSecretsAggregate already
	// excludes the same-key entry in both kinds, so this reflects the post-write
	// total without needing the cross-kind removal to have happened yet.
	// Reversing this order risks dropping the old entry and then 400-ing on
	// the aggregate — a data-loss path on cross-kind PUT overflow.
	aggregate, err := s.computeUserSecretsAggregate(r, agentName, key, kind, len(value))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to compute aggregate size")
		return
	}
	if err := usersecrets.ValidateAggregate(aggregate); err != nil {
		writeUserSecretValidationError(w, "value", err)
		return
	}

	// Remove any existing entry with this key in the other kind — one key,
	// one kind. Same-kind replacement is handled by the write below overwriting.
	otherKind := otherUserSecretKind(kind)
	removedOther, err := s.tryRemoveUserSecretEntry(r, agentName, key, otherKind)
	if err != nil {
		slog.Error("failed to clear user-secret from other kind", "agent", agentName, "key", key, "kind", otherKind, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to clear prior secret entry")
		return
	}

	if err := s.writeUserSecretEntry(r, agentName, key, kind, value); err != nil {
		slog.Error("failed to write user-secret", "agent", agentName, "key", key, "kind", kind, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to write user-secret")
		return
	}

	// A brand-new secret never interrupts a running agent. File-mode secrets
	// appear live through the kubelet-updated bind mount; a new kv-mode entry is
	// boot-time env and becomes visible on the next natural pod start. Replacing
	// an existing kv value still rolls so the old env value does not linger, and
	// changing kinds rolls in either direction to add or remove the env entry.
	if removedOther || (kind == userSecretKindKV && updatedExisting) {
		if err := s.rollAgentForUserSecret(r, agent); err != nil {
			slog.Error("failed to roll agent after user-secret write", "agent", agentName, "error", err)
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to restart agent")
			return
		}
	}

	slog.Info("user-secret set", "agent", agentName, "key", key, "kind", kind, "size", len(value))
	w.WriteHeader(http.StatusNoContent)
}

// deleteUserSecret handles DELETE /api/v1/agents/{name}/secrets/{key}.
// Looks in both kv and file; removes whichever holds the key. Rolls the agent
// on success.
func (s *Server) deleteUserSecret(w http.ResponseWriter, r *http.Request, agentName, key string) {
	if err := usersecrets.ValidateKey(key); err != nil {
		writeUserSecretValidationError(w, "key", err)
		return
	}

	agent, err := s.getAgentOr404(r, w, agentName)
	if err != nil {
		return
	}

	removed := false
	for _, kind := range []userSecretKind{userSecretKindKV, userSecretKindFile} {
		ok, err := s.tryRemoveUserSecretEntry(r, agentName, key, kind)
		if err != nil {
			slog.Error("failed to delete user-secret", "agent", agentName, "key", key, "kind", kind, "error", err)
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to delete user-secret")
			return
		}
		if ok {
			removed = true
		}
	}
	if !removed {
		writeJSONError(w, http.StatusNotFound, "not_found", "secret key not set")
		return
	}

	if err := s.rollAgentForUserSecret(r, agent); err != nil {
		slog.Error("failed to roll agent after user-secret delete", "agent", agentName, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to restart agent")
		return
	}

	slog.Info("user-secret deleted", "agent", agentName, "key", key)
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ---

// getAgentOr404 fetches the Agent CR; writes a 404 or 500 and returns the
// error if the lookup fails. Callers that already wrote a response must not
// write again.
func (s *Server) getAgentOr404(r *http.Request, w http.ResponseWriter, name string) (*kyberv1.Agent, error) {
	agent := &kyberv1.Agent{}
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent '"+name+"' not found")
			return nil, err
		}
		slog.Error("failed to get agent", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get agent")
		return nil, err
	}
	return agent, nil
}

// getUserSecretsSecret fetches one of the two user-secrets Secrets for the
// agent. Returns (nil, nil) when the Secret is missing — the caller treats
// that as an empty collection. Any other error is returned as-is.
func (s *Server) getUserSecretsSecret(r *http.Request, agentName string, kind userSecretKind) (*corev1.Secret, error) {
	name := userSecretsSecretName(agentName, kind)
	sec := &corev1.Secret{}
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, sec); err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return sec, nil
}

func (s *Server) userSecretEntryExists(r *http.Request, agentName, key string, kind userSecretKind) (bool, error) {
	sec, err := s.getUserSecretsSecret(r, agentName, kind)
	if err != nil || sec == nil {
		return false, err
	}
	_, ok := sec.Data[userSecretDataKey(key, kind)]
	return ok, nil
}

// writeUserSecretEntry upserts an entry into the kind's Secret. It creates the
// Secret if it's missing (defensive — the reconciler already does this), then
// patches Data + the metadata annotation.
func (s *Server) writeUserSecretEntry(r *http.Request, agentName, key string, kind userSecretKind, value []byte) error {
	sec, err := s.getOrCreateUserSecretsSecret(r, agentName, kind)
	if err != nil {
		return err
	}

	before := sec.DeepCopy()
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	dataKey := userSecretDataKey(key, kind)
	sec.Data[dataKey] = value

	meta := readUserSecretMetadata(sec)
	now := time.Now().UTC().Format(time.RFC3339)
	entry := meta[key]
	if entry.CreatedAt == "" {
		entry.CreatedAt = now
	}
	entry.UpdatedAt = now
	entry.Sha256Prefix = sha256HexPrefix(value, userSecretSha256PrefixLen)
	meta[key] = entry
	if err := writeUserSecretMetadata(sec, meta); err != nil {
		return fmt.Errorf("encoding metadata: %w", err)
	}

	return s.K8sClient.Patch(r.Context(), sec, client.MergeFrom(before))
}

// tryRemoveUserSecretEntry removes an entry from the kind's Secret if present.
// Returns (true, nil) when the entry was present and removed, (false, nil)
// when the entry (or the Secret itself) was not present, and (_, err) on any
// non-not-found client error.
func (s *Server) tryRemoveUserSecretEntry(r *http.Request, agentName, key string, kind userSecretKind) (bool, error) {
	sec, err := s.getUserSecretsSecret(r, agentName, kind)
	if err != nil {
		return false, err
	}
	if sec == nil {
		return false, nil
	}
	dataKey := userSecretDataKey(key, kind)
	if _, ok := sec.Data[dataKey]; !ok {
		return false, nil
	}

	before := sec.DeepCopy()
	delete(sec.Data, dataKey)
	meta := readUserSecretMetadata(sec)
	delete(meta, key)
	if err := writeUserSecretMetadata(sec, meta); err != nil {
		return false, fmt.Errorf("encoding metadata: %w", err)
	}
	if err := s.K8sClient.Patch(r.Context(), sec, client.MergeFrom(before)); err != nil {
		return false, err
	}
	return true, nil
}

// removeUserSecretEntry is tryRemoveUserSecretEntry with the "not present"
// case flattened into nil — used from PUT where the caller doesn't care
// whether anything was actually removed.
func (s *Server) removeUserSecretEntry(r *http.Request, agentName, key string, kind userSecretKind) error {
	_, err := s.tryRemoveUserSecretEntry(r, agentName, key, kind)
	return err
}

// computeUserSecretsAggregate returns the total byte size across both Secrets
// if the proposed write of newValueSize to (key, kind) were applied, including
// the effect of removing any prior entry for key in either kind. The caller
// passes this to usersecrets.ValidateAggregate for the 256 KiB check.
func (s *Server) computeUserSecretsAggregate(r *http.Request, agentName, key string, kind userSecretKind, newValueSize int) (int, error) {
	total := newValueSize
	for _, k := range []userSecretKind{userSecretKindKV, userSecretKindFile} {
		sec, err := s.getUserSecretsSecret(r, agentName, k)
		if err != nil {
			return 0, err
		}
		if sec == nil {
			continue
		}
		for dk, v := range sec.Data {
			// Skip the entry we're about to overwrite in its own kind, and any
			// existing entry in the other kind for the same logical key (it
			// will be removed before the write).
			if k == kind && dk == userSecretDataKey(key, kind) {
				continue
			}
			if k != kind && dk == userSecretDataKey(key, k) {
				continue
			}
			total += len(v)
		}
	}
	return total, nil
}

// getOrCreateUserSecretsSecret fetches the kind's Secret, creating it if it
// doesn't exist. The reconciler eagerly creates both Secrets, so this is only
// a defensive path for the narrow window between Agent creation and the
// first reconcile — or for tests that skip the reconciler.
func (s *Server) getOrCreateUserSecretsSecret(r *http.Request, agentName string, kind userSecretKind) (*corev1.Secret, error) {
	name := userSecretsSecretName(agentName, kind)
	sec, err := s.getUserSecretsSecret(r, agentName, kind)
	if err != nil {
		return nil, err
	}
	if sec != nil {
		return sec, nil
	}
	newSec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kyber-api",
				"kyber.io/agent":               agentName,
				"kyber.io/secret-kind":         "user-secrets",
			},
		},
		Type: corev1.SecretTypeOpaque,
	}
	if err := s.K8sClient.Create(r.Context(), newSec); err != nil && !k8serrors.IsAlreadyExists(err) {
		return nil, err
	}
	return s.getUserSecretsSecret(r, agentName, kind)
}

// rollAgentForUserSecret rolls a live agent's pod so a changed user-secret is
// re-projected. kv-mode user-secrets are injected as envFrom at pod boot
// (pod_builder.go) — static env — so a Running agent must have its pod
// recreated to pick up a new value.
//
// kyber#515: the prior implementation set DesiredPhase=Running for every agent,
// "to match the oauth re-auth flow." But oauth re-auth runs on a NeedsAuth
// agent, where DesiredPhase=Running is a real recovery transition; here the
// agent is already Running, so the same set is a no-op merge-patch and the pod
// was never recreated — the new value stayed stale until an unrelated restart.
//
// Drive the roll the same way setModel/setResources do (routes_agents.go): a
// live agent goes to Restarting, which the state machine honors by capturing
// session state, deleting the pod, and recreating it with fresh env. Dormant
// phases (Stopped/Failed/NeedsAuth/MemoryExhausted, or an unset phase)
// are left untouched — there is no live pod to roll and a secret update must not
// change an agent's lifecycle (no starting a Stopped agent, no force-recovering a
// Failed/NeedsAuth one). The value is already written to the Secret and lands
// via envFrom on the next start. Leaving DesiredPhase untouched also means no
// spurious roll on unrelated reconciles.
func (s *Server) rollAgentForUserSecret(r *http.Request, agent *kyberv1.Agent) error {
	switch agent.Status.Phase {
	case kyberv1.AgentPhaseRunning, kyberv1.AgentPhaseStarting, kyberv1.AgentPhaseRestarting:
		patch := client.MergeFrom(agent.DeepCopy())
		agent.Spec.DesiredPhase = kyberv1.AgentPhaseRestarting
		return s.K8sClient.Patch(r.Context(), agent, patch)
	default:
		return nil
	}
}

// parseUserSecretPutBody reads the PUT request body and returns the kind and
// value. Dispatches on Content-Type: application/json → kv (JSON body),
// multipart/form-data → file (with a "file" form field).
func parseUserSecretPutBody(r *http.Request) (userSecretKind, []byte, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, maxUserSecretsMultipartBytes)
	ct := r.Header.Get("Content-Type")
	mediaType, params, _ := mime.ParseMediaType(ct)

	switch {
	case mediaType == "application/json":
		var body putUserSecretKVBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return "", nil, errors.New("invalid JSON body")
		}
		if body.Kind != "" && body.Kind != string(userSecretKindKV) {
			return "", nil, fmt.Errorf("kind: must be %q for JSON body, got %q", userSecretKindKV, body.Kind)
		}
		return userSecretKindKV, []byte(body.Value), nil

	case mediaType == "multipart/form-data":
		if err := r.ParseMultipartForm(maxUserSecretsMultipartBytes); err != nil {
			return "", nil, errors.New("invalid multipart body")
		}
		kindField := r.FormValue("kind")
		if kindField != "" && kindField != string(userSecretKindFile) {
			return "", nil, fmt.Errorf("kind: must be %q for multipart body, got %q", userSecretKindFile, kindField)
		}
		_ = params // mime params are discarded — boundary is handled by ParseMultipartForm
		fileHeaders := r.MultipartForm.File["file"]
		if len(fileHeaders) == 0 {
			return "", nil, errors.New("multipart body: missing 'file' field")
		}
		f, err := fileHeaders[0].Open()
		if err != nil {
			return "", nil, errors.New("multipart body: could not open 'file' field")
		}
		defer f.Close()
		value, err := io.ReadAll(f)
		if err != nil {
			return "", nil, errors.New("multipart body: could not read 'file' field")
		}
		return userSecretKindFile, value, nil

	default:
		return "", nil, fmt.Errorf("unsupported Content-Type %q (want application/json or multipart/form-data)", mediaType)
	}
}

// writeUserSecretValue writes the readback response in the correct shape for
// the kind: text/plain for kv, application/octet-stream with a filename for
// file.
func writeUserSecretValue(w http.ResponseWriter, key string, kind userSecretKind, value []byte) {
	switch kind {
	case userSecretKindKV:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(value)
	case userSecretKindFile:
		filename := strings.ToLower(key) + ".bin"
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(value)
	}
}

// writeUserSecretValidationError maps a usersecrets package error to the
// API's standard 400 envelope with a stable code.
func writeUserSecretValidationError(w http.ResponseWriter, field string, err error) {
	code := "validation_error"
	switch {
	case errors.Is(err, usersecrets.ErrKeyEmpty), errors.Is(err, usersecrets.ErrKeyTooLong), errors.Is(err, usersecrets.ErrKeyBadGrammar):
		code = "invalid_key"
	case errors.Is(err, usersecrets.ErrKeyReservedPrefix):
		code = "reserved_prefix"
	case errors.Is(err, usersecrets.ErrValueTooLarge):
		code = "value_too_large"
	case errors.Is(err, usersecrets.ErrAggregateTooLarge):
		code = "aggregate_too_large"
	}
	writeJSONErrorWithField(w, http.StatusBadRequest, code, err.Error(), field)
}

// userSecretsSecretName returns the k8s Secret name holding entries of the
// given kind for the agent. Delegates to the controller package which owns
// the naming convention.
func userSecretsSecretName(agentName string, kind userSecretKind) string {
	if kind == userSecretKindKV {
		return agent.UserSecretKVName(agentName)
	}
	return agent.UserSecretFilesName(agentName)
}

// userSecretDataKey returns the map key used inside the Secret.Data for the
// given user-facing key. kv keeps the raw uppercase form (projected verbatim
// as env vars); file lowercases and suffixes with .bin so the mount path
// matches #75's spec (/user-secrets/{key.lower()}.bin).
func userSecretDataKey(key string, kind userSecretKind) string {
	if kind == userSecretKindKV {
		return key
	}
	return strings.ToLower(key) + ".bin"
}

// enumerateUserSecretKeys yields user-facing keys and their corresponding
// Secret.Data map keys for the given Secret.
func enumerateUserSecretKeys(sec *corev1.Secret, kind userSecretKind) map[string]string {
	out := map[string]string{}
	for dk := range sec.Data {
		userKey := userKeyFromDataKey(dk, kind)
		if userKey == "" {
			continue
		}
		out[userKey] = dk
	}
	return out
}

// userKeyFromDataKey inverts userSecretDataKey. Returns "" if dk doesn't
// match the expected shape for the kind — defensive against hand-edited
// Secrets.
func userKeyFromDataKey(dk string, kind userSecretKind) string {
	if kind == userSecretKindKV {
		return dk
	}
	if !strings.HasSuffix(dk, ".bin") {
		return ""
	}
	return strings.ToUpper(strings.TrimSuffix(dk, ".bin"))
}

// readUserSecretMetadata decodes the per-entry metadata annotation. Returns
// an empty map when the annotation is missing or malformed — malformed
// metadata should never cause reads to fail, only to lose display data.
func readUserSecretMetadata(sec *corev1.Secret) map[string]userSecretEntryMeta {
	out := map[string]userSecretEntryMeta{}
	if sec.Annotations == nil {
		return out
	}
	raw := sec.Annotations[userSecretsMetadataAnnotation]
	if raw == "" {
		return out
	}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

// writeUserSecretMetadata encodes meta back into the Secret's annotations,
// removing the annotation entirely when meta is empty to keep the object clean.
func writeUserSecretMetadata(sec *corev1.Secret, meta map[string]userSecretEntryMeta) error {
	if sec.Annotations == nil {
		sec.Annotations = map[string]string{}
	}
	if len(meta) == 0 {
		delete(sec.Annotations, userSecretsMetadataAnnotation)
		return nil
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	sec.Annotations[userSecretsMetadataAnnotation] = string(b)
	return nil
}

// otherUserSecretKind returns the opposite kind.
func otherUserSecretKind(k userSecretKind) userSecretKind {
	if k == userSecretKindKV {
		return userSecretKindFile
	}
	return userSecretKindKV
}

// sha256HexPrefix returns the first n hex chars of sha256(value).
func sha256HexPrefix(value []byte, n int) string {
	sum := sha256.Sum256(value)
	h := hex.EncodeToString(sum[:])
	if n > len(h) {
		return h
	}
	return h[:n]
}
