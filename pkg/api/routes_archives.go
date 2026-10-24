package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/google/uuid"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/archivejob"
	"github.com/matty-v/kyber/pkg/securityaudit"
)

// archiveJobView is the public projection of a job. Keys, token hashes and
// object locations never leave the control plane.
type archiveJobView struct {
	ID           string              `json:"id"`
	Kind         archivejob.Kind     `json:"kind"`
	Agent        string              `json:"agent"`
	State        archivejob.State    `json:"state"`
	Step         archivejob.Step     `json:"step,omitempty"`
	Message      string              `json:"message,omitempty"`
	Error        string              `json:"error,omitempty"`
	BytesDone    int64               `json:"bytesDone"`
	SizeBytes    int64               `json:"sizeBytes,omitempty"`
	CreatedAt    time.Time           `json:"createdAt"`
	StartedAt    *time.Time          `json:"startedAt,omitempty"`
	CompletedAt  *time.Time          `json:"completedAt,omitempty"`
	ExpiresAt    *time.Time          `json:"expiresAt,omitempty"`
	Cancelable   bool                `json:"cancelable"`
	Downloadable bool                `json:"downloadable"`
	RequestedBy  string              `json:"requestedBy,omitempty"`
	Summary      *archivejob.Summary `json:"summary,omitempty"`
	// Import-only.
	SourceID string   `json:"sourceId,omitempty"`
	Restored bool     `json:"restored,omitempty"`
	Skipped  []string `json:"skipped,omitempty"`
	Cutover  []string `json:"cutover,omitempty"`
	// Upload-in-parts only.
	PartSize      int64 `json:"partSize,omitempty"`
	PartCount     int   `json:"partCount,omitempty"`
	PartsReceived []int `json:"partsReceived,omitempty"`
}

func viewArchiveJob(j *archivejob.Job) archiveJobView {
	v := archiveJobView{
		ID: j.ID, Kind: j.Kind, Agent: j.Agent, State: j.State, Step: j.Step,
		Message: j.Message, Error: j.Error, BytesDone: j.BytesDone,
		CreatedAt: j.CreatedAt, StartedAt: j.StartedAt, CompletedAt: j.CompletedAt, ExpiresAt: j.ExpiresAt,
		Cancelable:   !j.State.Terminal() && !j.CancelRequested,
		Downloadable: j.Kind == archivejob.KindExport && j.State == archivejob.StateCompleted,
		RequestedBy:  j.RequestedBy,
		Summary:      summaryView(j.Summary),
		SourceID:     j.SourceJobID,
		Restored:     j.Restored,
		Skipped:      j.Skip,
		Cutover:      j.Cutover,
	}
	if j.State == archivejob.StateCompleted && j.Kind != archivejob.KindImport {
		v.SizeBytes = j.PlainSize
	}
	if j.Kind == archivejob.KindImport {
		v.Cancelable = v.Cancelable && !j.Restored
	}
	if j.PartSize > 0 {
		v.PartSize, v.PartCount, v.PartsReceived = j.PartSize, uploadPartCount(j.UploadSize, j.PartSize), j.PartsReceived
	}
	if j.CancelRequested && !j.State.Terminal() {
		v.Message = "Canceling"
	}
	return v
}

// summaryView leaves the avatar image out of API views: it travels in the
// archive, but every job listing would otherwise repeat up to a megabyte.
func summaryView(s *archivejob.Summary) *archivejob.Summary {
	if s == nil || s.Source.Config == nil || s.Source.Config.Profile == nil || len(s.Source.Config.Profile.Avatar) == 0 {
		return s
	}
	cp, cfg, prof := *s, *s.Source.Config, *s.Source.Config.Profile
	prof.Avatar, prof.HasAvatar = nil, true
	cfg.Profile = &prof
	cp.Source.Config = &cfg
	return &cp
}

// handleAgentExports serves /api/v1/agents/{name}/exports[/{id}[/cancel|/download-link]].
func (s *Server) handleAgentExports(w http.ResponseWriter, r *http.Request, name, rest string) {
	id, action, _ := strings.Cut(rest, "/")
	auditAction := "operator:unclassified"
	switch {
	case id == "" && action == "" && r.Method == http.MethodPost:
		auditAction = "archives:export"
	case action == "" && r.Method == http.MethodGet:
		auditAction = "archives:read"
	case id != "" && action == "" && r.Method == http.MethodDelete:
		auditAction = "archives:delete"
	case id != "" && action == "cancel" && r.Method == http.MethodPost:
		auditAction = "archives:cancel"
	case id != "" && action == "download-link" && r.Method == http.MethodPost:
		auditAction = "archives:link"
	}
	if !s.requireArchiveAccess(w, r, name, auditAction) {
		return
	}
	a := s.Archives
	if ok, reason := a.Available(); !ok {
		writeJSONError(w, http.StatusServiceUnavailable, "archives_unavailable", reason)
		return
	}
	switch {
	case id == "" && r.Method == http.MethodPost:
		if !s.requireManagementAudit(w, r, "archives:export", "agent", name, ScopeArchivesAdmin, false) {
			return
		}
		caller := "unknown"
		if c := callerFrom(r.Context()); c != nil {
			caller = c.Name
		}
		beginManagementMutation(r)
		j, err := a.StartExport(r.Context(), name, caller)
		switch {
		case k8serrors.IsNotFound(err):
			markSensitiveOutcome(r, "failure")
			writeJSONError(w, http.StatusNotFound, "not_found", "agent not found")
		case errors.Is(err, errArchivePhase), errors.Is(err, errArchiveHeld):
			markSensitiveOutcome(r, "failure")
			writeJSONError(w, http.StatusConflict, "invalid_phase", err.Error())
		case errors.Is(err, archivejob.ErrActiveJob):
			markSensitiveOutcome(r, "failure")
			writeJSONError(w, http.StatusConflict, "export_in_progress", "this agent already has an export in progress")
		case errors.Is(err, archivejob.ErrConcurrency):
			markSensitiveOutcome(r, "failure")
			w.Header().Set("Retry-After", "60")
			writeJSONError(w, http.StatusTooManyRequests, "archive_capacity", "the installation is already running its maximum number of archive jobs")
		case err != nil:
			slog.Error("starting disk export failed", "agent", name, "error", err)
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to start the export")
		default:
			completeManagementMutation(r, "archives:export")
			writeJSON(w, http.StatusAccepted, viewArchiveJob(j))
		}
	case id == "" && r.Method == http.MethodGet:
		jobs, err := a.Jobs.List(r.Context(), archivejob.KindExport, name, 20)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to list exports")
			return
		}
		out := make([]archiveJobView, 0, len(jobs))
		for _, j := range jobs {
			out = append(out, viewArchiveJob(j))
		}
		writeJSON(w, http.StatusOK, map[string]any{"exports": out})
	default:
		j, err := a.Jobs.Get(r.Context(), id)
		if err != nil || j.Kind != archivejob.KindExport || j.Agent != name {
			writeJSONError(w, http.StatusNotFound, "not_found", "export not found")
			return
		}
		switch {
		case action == "" && r.Method == http.MethodGet:
			writeJSON(w, http.StatusOK, viewArchiveJob(j))
		case action == "" && r.Method == http.MethodDelete:
			s.deleteArchive(w, r, j)
		case action == "cancel" && r.Method == http.MethodPost:
			if j.State.Terminal() {
				writeJSONError(w, http.StatusConflict, "export_finished", "the export has already finished")
				return
			}
			if !s.requireManagementAudit(w, r, "archives:cancel", "agent", name, ScopeArchivesAdmin, false) {
				return
			}
			beginManagementMutation(r)
			if err := a.RequestCancel(r.Context(), j); err != nil {
				writeJSONError(w, http.StatusConflict, "export_changed", "the export changed; retry")
				return
			}
			completeManagementMutation(r, "archives:cancel")
			writeJSON(w, http.StatusAccepted, viewArchiveJob(j))
		case action == "download-link" && r.Method == http.MethodPost:
			if j.State != archivejob.StateCompleted {
				writeJSONError(w, http.StatusConflict, "not_downloadable", "only a completed export can be downloaded")
				return
			}
			if !s.requireManagementAudit(w, r, "archives:link", "agent", name, ScopeArchivesAdmin, false) {
				return
			}
			exp := a.now().Add(a.limits().LinkTTL)
			token, err := s.signCurrentArchiveDownload(r, j, exp)
			if err != nil {
				if errors.Is(err, errOperatorAuthorityUnavailable) {
					writeOperatorAuthenticationUnavailable(w)
				} else {
					writeJSONError(w, http.StatusForbidden, "download_access_changed", "archive access changed; request a new download link")
				}
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"url":       "/api/v1/archive-downloads/" + token,
				"expiresAt": exp,
				"filename":  archiveFilename(j),
			})
		default:
			writeJSONError(w, http.StatusNotFound, "not_found", "unknown export action")
		}
	}
}

// requireArchiveAccess requires archives:admin and that the caller's agent
// resources include this agent ("*" for installation administrators). A disk archive can hold credentials, so a scoped key
// never reaches an agent it was not explicitly granted.
func (s *Server) requireArchiveAccess(w http.ResponseWriter, r *http.Request, name, action string) bool {
	if !s.requireScope(w, r, name, "disk-archive", ScopeArchivesAdmin) {
		s.auditOperatorDenial(w, r, action, "agent", name)
		return false
	}
	caller := callerFrom(r.Context())
	if caller == nil || !caller.AgentResources.Has(s.Namespace+"/"+name) {
		s.auditOperatorDenial(w, r, action, "agent", name)
		writeJSONError(w, http.StatusNotFound, "not_found", "agent not found")
		return false
	}
	return true
}

func archiveFilename(j *archivejob.Job) string {
	ts := j.CreatedAt.UTC().Format("20060102-150405")
	return fmt.Sprintf("%s-disk-%s.zip", j.Agent, ts)
}

// archiveDownloadPrefix is outside the authenticated API: the path token is
// the credential. loggingMiddleware redacts it.
const archiveDownloadPrefix = "/api/v1/archive-downloads/"

// handleArchiveDownload streams a completed export as a plain ZIP.
func (s *Server) handleArchiveDownload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	a := s.Archives
	if ok, _ := a.Available(); !ok {
		writeJSONError(w, http.StatusServiceUnavailable, "archives_unavailable", "disk archives are unavailable")
		return
	}
	claims, ok := s.parseCurrentArchiveDownload(strings.TrimPrefix(r.URL.Path, archiveDownloadPrefix))
	if !ok {
		s.auditOperatorDenial(w, r, "archives:download", "installation", "")
		writeJSONError(w, http.StatusForbidden, "link_invalid", "this download link is invalid or has expired")
		return
	}
	caller, err := s.resolveArchiveDownload(r.Context(), claims)
	if err != nil {
		if errors.Is(err, errOperatorAuthorityUnavailable) {
			writeOperatorAuthenticationUnavailable(w)
		} else {
			s.auditArchiveCapabilityDenial(w, r, claims)
			writeJSONError(w, http.StatusForbidden, "link_invalid", "this download link is no longer authorized; request a new link")
		}
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), callerCtxKey{}, caller))
	handler := s.liveOperatorRequestsWithAccess(s.auditOperatorOutcomes(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.streamArchiveDownload(w, r, claims)
	})), func(r *http.Request) (*Caller, error) { return s.resolveArchiveDownload(r.Context(), claims) }, func(_ *http.Request, c *Caller) bool {
		return c.Scopes.Has(ScopeArchivesAdmin) && c.AgentResources.Has(claims.Namespace+"/"+claims.Agent)
	})
	handler.ServeHTTP(w, r)
}

func (s *Server) streamArchiveDownload(w http.ResponseWriter, r *http.Request, claims archiveDownloadClaims) {
	if !s.requireManagementAudit(w, r, "archives:download", "agent", claims.Agent, ScopeArchivesAdmin, false) {
		return
	}
	a := s.Archives
	j, err := a.Jobs.Get(r.Context(), claims.Job)
	if err != nil || j.Kind != archivejob.KindExport || j.State != archivejob.StateCompleted || j.Agent != claims.Agent {
		writeJSONError(w, http.StatusNotFound, "not_found", "this export is no longer available")
		return
	}
	src, err := a.openArchive(r.Context(), j)
	if err != nil {
		slog.Error("opening export for download failed", "job", j.ID, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "the archive could not be read")
		return
	}
	// The whole archive may take far longer than the server's write timeout.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, archiveFilename(j)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Length", strconv.FormatInt(src.Size(), 10))
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, io.NewSectionReader(src, 0, src.Size())); err != nil {
		slog.Warn("export download interrupted", "job", j.ID, "error", err)
	}
}

// archiveHeld reports whether a disk export or restore owns the agent's
// volume (MAT-87/MAT-88).
func archiveHeld(agent *kyberv1.Agent) bool {
	return agent.Annotations[kyberv1.AnnotationArchiveHold] != ""
}

// rejectArchiveHeld answers 409 when a disk export or restore owns the
// agent's volume (MAT-87/MAT-88). Every verb that would start the agent or
// write its volume calls it; Stop does not, because it is the kill switch and
// never touches the disk.
func rejectArchiveHeld(w http.ResponseWriter, agent *kyberv1.Agent) bool {
	if agent.Annotations[kyberv1.AnnotationArchiveHold] == "" {
		return false
	}
	writeJSONError(w, http.StatusConflict, "archive_in_progress",
		"a disk archive job is using this agent's volume; cancel it or wait for it to finish")
	return true
}

// redactArchiveDownloadPath keeps download tokens out of request logs.
func redactArchiveDownloadPath(p string) string {
	if strings.HasPrefix(p, archiveDownloadPrefix) {
		return archiveDownloadPrefix + "[redacted]"
	}
	return p
}

// Attribution is from a verified installation signature, not an authenticated
// current role. It is used only for a denial record and never enters caller
// context or grants API access. Unknown/invalid links stay unattributed.
func (s *Server) auditArchiveCapabilityDenial(w http.ResponseWriter, r *http.Request, claims archiveDownloadClaims) {
	if s.SecurityAudit == nil {
		return
	}
	e := securityaudit.Event{ID: uuid.NewString(), OperationID: uuid.NewString(), ObservedAt: time.Now().UTC(), Namespace: s.Namespace, RequestID: w.Header().Get(RequestIDHeader), Action: "archives:download", ResourceKind: "agent", ResourceName: claims.Agent, Phase: "authorization", Outcome: "deny", Method: auditMethod(r.Method), CredentialAuthority: "configuration", PrincipalID: claims.Principal, CredentialID: claims.Credential, Generation: claims.Generation}
	if e.RequestID == "" {
		e.RequestID = uuid.NewString()
	}
	if claims.Authority == "registry" {
		e.CredentialAuthority = "registry"
	}
	if e.Validate() != nil {
		e.PrincipalID, e.CredentialID, e.Generation = "", "", 0
	}
	ctx, cancel := context.WithTimeout(r.Context(), securityAuditTimeout)
	defer cancel()
	if _, err := s.SecurityAudit.Append(ctx, e); err != nil {
		slog.Error("security audit archive denial unavailable", "operation_id", e.OperationID, "action", e.Action)
	}
}
