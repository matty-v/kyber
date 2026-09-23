package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/archivejob"
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
}

func viewArchiveJob(j *archivejob.Job) archiveJobView {
	v := archiveJobView{
		ID: j.ID, Kind: j.Kind, Agent: j.Agent, State: j.State, Step: j.Step,
		Message: j.Message, Error: j.Error, BytesDone: j.BytesDone,
		CreatedAt: j.CreatedAt, StartedAt: j.StartedAt, CompletedAt: j.CompletedAt, ExpiresAt: j.ExpiresAt,
		Cancelable:   !j.State.Terminal() && !j.CancelRequested,
		Downloadable: j.Kind == archivejob.KindExport && j.State == archivejob.StateCompleted,
		RequestedBy:  j.RequestedBy,
		Summary:      j.Summary,
	}
	if j.State == archivejob.StateCompleted {
		v.SizeBytes = j.PlainSize
	}
	if j.CancelRequested && !j.State.Terminal() {
		v.Message = "Canceling"
	}
	return v
}

// handleAgentExports serves /api/v1/agents/{name}/exports[/{id}[/cancel|/download-link]].
func (s *Server) handleAgentExports(w http.ResponseWriter, r *http.Request, name, rest string) {
	if !s.requireArchiveAccess(w, r, name) {
		return
	}
	a := s.Archives
	if ok, reason := a.Available(); !ok {
		writeJSONError(w, http.StatusServiceUnavailable, "archives_unavailable", reason)
		return
	}
	id, action, _ := strings.Cut(rest, "/")
	switch {
	case id == "" && r.Method == http.MethodPost:
		caller := "unknown"
		if c := callerFrom(r.Context()); c != nil {
			caller = c.Name
		}
		j, err := a.StartExport(r.Context(), name, caller)
		switch {
		case k8serrors.IsNotFound(err):
			writeJSONError(w, http.StatusNotFound, "not_found", "agent not found")
		case errors.Is(err, errArchivePhase), errors.Is(err, errArchiveHeld):
			writeJSONError(w, http.StatusConflict, "invalid_phase", err.Error())
		case errors.Is(err, archivejob.ErrActiveJob):
			writeJSONError(w, http.StatusConflict, "export_in_progress", "this agent already has an export in progress")
		case errors.Is(err, archivejob.ErrConcurrency):
			w.Header().Set("Retry-After", "60")
			writeJSONError(w, http.StatusTooManyRequests, "archive_capacity", "the installation is already running its maximum number of archive jobs")
		case err != nil:
			slog.Error("starting disk export failed", "agent", name, "error", err)
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to start the export")
		default:
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
		case action == "cancel" && r.Method == http.MethodPost:
			if j.State.Terminal() {
				writeJSONError(w, http.StatusConflict, "export_finished", "the export has already finished")
				return
			}
			if err := a.RequestCancel(r.Context(), j); err != nil {
				writeJSONError(w, http.StatusConflict, "export_changed", "the export changed; retry")
				return
			}
			writeJSON(w, http.StatusAccepted, viewArchiveJob(j))
		case action == "download-link" && r.Method == http.MethodPost:
			if j.State != archivejob.StateCompleted {
				writeJSONError(w, http.StatusConflict, "not_downloadable", "only a completed export can be downloaded")
				return
			}
			exp := a.now().Add(a.limits().LinkTTL)
			writeJSON(w, http.StatusOK, map[string]any{
				"url":       "/api/v1/archive-downloads/" + a.signDownload(j, exp),
				"expiresAt": exp,
				"filename":  archiveFilename(j),
			})
		default:
			writeJSONError(w, http.StatusNotFound, "not_found", "unknown export action")
		}
	}
}

// requireArchiveAccess requires archives:admin and that the caller's agent
// resources include this agent ("*" for installation-wide callers such as
// the legacy key). A disk archive can hold credentials, so a scoped key
// never reaches an agent it was not explicitly granted.
func (s *Server) requireArchiveAccess(w http.ResponseWriter, r *http.Request, name string) bool {
	if !s.requireScope(w, r, name, "disk-archive", ScopeArchivesAdmin) {
		return false
	}
	caller := callerFrom(r.Context())
	if caller == nil || !caller.AgentResources.Has(s.Namespace+"/"+name) {
		writeJSONError(w, http.StatusNotFound, "not_found", "agent not found")
		return false
	}
	return true
}

func archiveFilename(j *archivejob.Job) string {
	ts := j.CreatedAt.UTC().Format("20060102-150405")
	return fmt.Sprintf("%s-disk-%s.zip", j.Agent, ts)
}

// Download tokens are job ID, expiry and an HMAC over both under a key
// derived from the signing key. They are bearer credentials for one job's
// bytes for a few minutes, which is what lets a browser download a large
// archive with a plain navigation instead of buffering it in memory.
func (a *ArchiveService) downloadMAC(id string, exp time.Time) []byte {
	m := hmac.New(sha256.New, a.SigningKey)
	m.Write([]byte("kyber archive download v1\x00"))
	m.Write([]byte(id))
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(exp.Unix()))
	m.Write(b[:])
	return m.Sum(nil)
}

func (a *ArchiveService) signDownload(j *archivejob.Job, exp time.Time) string {
	payload := j.ID + "." + strconv.FormatInt(exp.Unix(), 10)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(a.downloadMAC(j.ID, exp))
}

func (a *ArchiveService) parseDownload(token string) (string, bool) {
	p, sig, ok := strings.Cut(token, ".")
	if !ok {
		return "", false
	}
	raw, err1 := base64.RawURLEncoding.DecodeString(p)
	mac, err2 := base64.RawURLEncoding.DecodeString(sig)
	if err1 != nil || err2 != nil {
		return "", false
	}
	id, expRaw, ok := strings.Cut(string(raw), ".")
	unix, err := strconv.ParseInt(expRaw, 10, 64)
	if !ok || err != nil {
		return "", false
	}
	exp := time.Unix(unix, 0)
	if !hmac.Equal(mac, a.downloadMAC(id, exp)) || !a.now().Before(exp) {
		return "", false
	}
	return id, true
}

// archiveDownloadPrefix is outside the authenticated API: the path token is
// the credential. loggingMiddleware redacts it.
const archiveDownloadPrefix = "/api/v1/archive-downloads/"

// handleArchiveDownload streams a completed export as a plain ZIP.
func (s *Server) handleArchiveDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	a := s.Archives
	if ok, _ := a.Available(); !ok {
		writeJSONError(w, http.StatusServiceUnavailable, "archives_unavailable", "disk archives are unavailable")
		return
	}
	id, ok := a.parseDownload(strings.TrimPrefix(r.URL.Path, archiveDownloadPrefix))
	if !ok {
		writeJSONError(w, http.StatusForbidden, "link_invalid", "this download link is invalid or has expired")
		return
	}
	j, err := a.Jobs.Get(r.Context(), id)
	if err != nil || j.Kind != archivejob.KindExport || j.State != archivejob.StateCompleted {
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
