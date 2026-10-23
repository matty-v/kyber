package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/matty-v/kyber/pkg/securityaudit"
)

// The continuation is not an authorization token: every page requires current
// administrator authentication, and the installation namespace is server-owned.
// Binding it to the normalized filter prevents accidentally mixing page sets.
type securityEventCursor struct {
	Version int    `json:"version"`
	Filter  string `json:"filter"`
	Max     int64  `json:"max"`
	Before  int64  `json:"before"`
}

type securityEventsResponse struct {
	ObservedAt time.Time           `json:"observedAt"`
	Items      []securityEventView `json:"items"`
	NextCursor string              `json:"nextCursor,omitempty"`
	Snapshot   string              `json:"snapshot"`
	Coverage   string              `json:"coverage"`
	Warnings   []string            `json:"warnings"`
}

// Explicit projection: adding a field to internal storage must not silently
// expose it through the API. Wide counters are decimal strings for lossless
// JavaScript clients, including imported uint64 credential generations.
type securityEventView struct {
	ID                  string    `json:"id"`
	OperationID         string    `json:"operationId"`
	ObservedAt          time.Time `json:"observedAt"`
	StoredAt            time.Time `json:"storedAt"`
	Sequence            string    `json:"sequence"`
	Namespace           string    `json:"namespace"`
	PrincipalID         string    `json:"principalId"`
	CredentialID        string    `json:"credentialId"`
	Generation          string    `json:"credentialGeneration"`
	CredentialAuthority string    `json:"credentialAuthority,omitempty"`
	RequestID           string    `json:"requestId"`
	Method              string    `json:"method,omitempty"`
	Action              string    `json:"action"`
	ResourceKind        string    `json:"resourceKind"`
	ResourceName        string    `json:"resourceName"`
	Phase               string    `json:"phase"`
	Outcome             string    `json:"outcome"`
	LogComponent        string    `json:"logComponent,omitempty"`
	LogWorkload         string    `json:"logWorkload,omitempty"`
	Container           string    `json:"container,omitempty"`
}

func securityHistoryView(r securityaudit.Record) securityEventView {
	return securityEventView{ID: r.ID, OperationID: r.OperationID, ObservedAt: r.ObservedAt, StoredAt: r.StoredAt,
		Sequence: strconv.FormatInt(r.Sequence, 10), Namespace: r.Namespace, PrincipalID: r.PrincipalID, CredentialID: r.CredentialID,
		Generation: strconv.FormatUint(r.Generation, 10), CredentialAuthority: r.CredentialAuthority, RequestID: r.RequestID, Method: r.Method,
		Action: r.Action, ResourceKind: r.ResourceKind, ResourceName: r.ResourceName, Phase: r.Phase, Outcome: r.Outcome,
		LogComponent: r.LogComponent, LogWorkload: r.LogWorkload, Container: r.Container}
}

func securityEventFilter(q url.Values, namespace string) (securityaudit.Filter, *securityaudit.Cursor, error) {
	f := securityaudit.Filter{Namespace: namespace, Limit: 50}
	allowed := map[string]bool{"since": true, "until": true, "principalId": true, "credentialId": true, "action": true, "resourceKind": true, "resourceName": true, "limit": true, "cursor": true, "includeHistoryViews": true}
	for k, values := range q {
		if !allowed[k] || len(values) != 1 || values[0] == "" {
			return f, nil, securityaudit.ErrInvalid
		}
	}
	for key, target := range map[string]*time.Time{"since": &f.Since, "until": &f.Until} {
		if value := q.Get(key); value != "" {
			parsed, err := time.Parse(time.RFC3339Nano, value)
			if err != nil || parsed.Year() < 2000 || parsed.Year() > 9999 {
				return f, nil, securityaudit.ErrInvalid
			}
			*target = parsed.UTC()
		}
	}
	if value := q.Get("limit"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil {
			return f, nil, securityaudit.ErrInvalid
		}
		f.Limit = n
	}
	f.PrincipalID, f.CredentialID, f.Action = q.Get("principalId"), q.Get("credentialId"), q.Get("action")
	f.ResourceKind, f.ResourceName = q.Get("resourceKind"), q.Get("resourceName")
	switch q.Get("includeHistoryViews") {
	case "", "true":
	case "false":
		f.ExcludeRoutineAuditReads = true
	default:
		return f, nil, securityaudit.ErrInvalid
	}
	if err := f.Validate(); err != nil {
		return f, nil, err
	}
	value := q.Get("cursor")
	if value == "" {
		return f, nil, nil
	}
	if len(value) > 1024 {
		return f, nil, securityaudit.ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return f, nil, securityaudit.ErrInvalid
	}
	var cursor securityEventCursor
	if json.Unmarshal(raw, &cursor) != nil || cursor.Version != 1 || cursor.Filter != securityFilterBinding(f) || cursor.Max < 1 || cursor.Before < 1 || cursor.Before > cursor.Max {
		return f, nil, securityaudit.ErrInvalid
	}
	return f, &securityaudit.Cursor{MaxSequence: cursor.Max, BeforeSequence: cursor.Before}, nil
}

func securityFilterBinding(f securityaudit.Filter) string {
	raw, _ := json.Marshal(f)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func (s *Server) handleSecurityEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.requireInstallationAdmin(w, r, "read-security-history") {
		return
	}
	if r.Method != http.MethodGet {
		writeJSONError(w, 405, "method_not_allowed", "GET required")
		return
	}
	if len(r.URL.RawQuery) > 8192 {
		writeJSONError(w, 400, "invalid_filter", "invalid security history filters")
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeJSONError(w, 400, "invalid_filter", "invalid security history filters")
		return
	}
	f, cursor, err := securityEventFilter(q, s.Namespace)
	if err != nil {
		writeJSONError(w, 400, "invalid_filter", "invalid security history filters")
		return
	}
	if !s.auditSensitiveResourceDecision(w, r, "", Scope("audit:read"), "installation", true) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), securityAuditTimeout)
	defer cancel()
	page, err := s.SecurityAudit.Query(ctx, f, cursor)
	if err != nil {
		// Database text can contain sensitive driver/configuration details.
		if errors.Is(err, securityaudit.ErrInvalid) {
			writeJSONError(w, 400, "invalid_filter", "invalid security history filters")
		} else {
			writeJSONError(w, 503, "security_audit_unavailable", "security history is unavailable")
		}
		return
	}
	items := make([]securityEventView, 0, len(page.Items))
	for _, record := range page.Items {
		items = append(items, securityHistoryView(record))
	}
	response := securityEventsResponse{ObservedAt: time.Now().UTC(), Items: items, Snapshot: strconv.FormatInt(page.Snapshot, 10), Coverage: "partial-sensitive-access", Warnings: []string{
		"History currently covers selected sensitive access and restricted-route denials; it is not a complete authentication or credential lifecycle history.",
		"Retention and collection gaps can omit earlier events. No recorded use does not mean never used.",
		"Shared administrator credentials identify the credential, not the person or application using it. Configuration-reported generations are not a complete key rotation ledger.",
	}}
	if page.Next != nil {
		raw, _ := json.Marshal(securityEventCursor{Version: 1, Filter: securityFilterBinding(f), Max: page.Next.MaxSequence, Before: page.Next.BeforeSequence})
		response.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	writeJSON(w, http.StatusOK, response)
}
