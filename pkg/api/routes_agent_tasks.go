package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/taskobject"
	"github.com/matty-v/kyber/pkg/taskstore"
)

const maxAgentTaskBodyBytes = 6*(taskstore.HardMaxPromptBytes+taskstore.HardMaxCorrelationBytes) + 2048

var agentTaskIDPattern = regexp.MustCompile(`^task_[a-f0-9]{32}$`)

type createAgentTaskInput struct {
	Prompt      string     `json:"prompt"`
	Correlation string     `json:"correlation,omitempty"`
	DeadlineAt  *time.Time `json:"deadlineAt,omitempty"`
}

type agentTaskResponse struct {
	ID          string                    `json:"id"`
	Agent       string                    `json:"agent"`
	State       taskstore.State           `json:"state"`
	FailureCode taskstore.FailureCode     `json:"failureCode,omitempty"`
	Error       string                    `json:"error,omitempty"`
	Correlation string                    `json:"correlation,omitempty"`
	Response    string                    `json:"response,omitempty"`
	Version     int64                     `json:"version"`
	CreatedAt   time.Time                 `json:"createdAt"`
	UpdatedAt   time.Time                 `json:"updatedAt"`
	DeadlineAt  time.Time                 `json:"deadlineAt"`
	RetainUntil time.Time                 `json:"retainUntil"`
	CompletedAt *time.Time                `json:"completedAt,omitempty"`
	Progress    *taskstore.Progress       `json:"progress,omitempty"`
	Results     []agentTaskResultResponse `json:"results,omitempty"`
}

type agentTaskResultResponse struct {
	ID          string                  `json:"id"`
	Name        string                  `json:"name"`
	Description string                  `json:"description,omitempty"`
	Parts       []agentTaskPartResponse `json:"parts,omitempty"`
	PartKinds   []taskstore.PartKind    `json:"partKinds,omitempty"`
	PartCount   int                     `json:"partCount"`
	TotalBytes  int64                   `json:"totalBytes"`
	CreatedAt   time.Time               `json:"createdAt"`
}

type agentTaskPartResponse struct {
	Kind  taskstore.PartKind     `json:"kind"`
	Value any                    `json:"value,omitempty"`
	File  *agentTaskFileResponse `json:"file,omitempty"`
}

type agentTaskFileResponse struct {
	Filename     string `json:"filename"`
	MediaType    string `json:"mediaType"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
	ScanStatus   string `json:"scanStatus"`
	DownloadPath string `json:"downloadPath"`
}

type agentTaskListResponse struct {
	Tasks      []agentTaskResponse `json:"tasks"`
	NextCursor string              `json:"nextCursor,omitempty"`
}

func (s *Server) handleAgentTasks(w http.ResponseWriter, r *http.Request, agentName, subpath string) {
	subpath = strings.Trim(subpath, "/")
	if subpath == "" {
		switch r.Method {
		case http.MethodPost:
			if !s.requireScope(w, r, agentName, "task-submit", ScopeRequestsWrite) {
				return
			}
			s.createAgentTask(w, r, agentName)
		case http.MethodGet:
			if !s.requireScope(w, r, agentName, "task-list", ScopeRequestsRead) {
				return
			}
			s.listAgentTasks(w, r, agentName)
		default:
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		}
		return
	}
	parts := strings.Split(subpath, "/")
	if len(parts) == 7 && agentTaskIDPattern.MatchString(parts[0]) && parts[1] == "results" && resultIDPattern.MatchString(parts[2]) && parts[3] == "parts" && parts[5] == "content" && parts[6] == "" {
		// Retained for compatibility with a trailing slash normalized below.
		parts = parts[:6]
	}
	if len(parts) == 6 && agentTaskIDPattern.MatchString(parts[0]) && parts[1] == "results" && resultIDPattern.MatchString(parts[2]) && parts[3] == "parts" && parts[5] == "content" {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		if !s.requireScope(w, r, agentName, "task-result-read", ScopeRequestsRead) {
			return
		}
		ordinal, err := strconv.Atoi(parts[4])
		if err != nil || ordinal < 0 {
			writeJSONError(w, http.StatusNotFound, "not_found", "task result not found")
			return
		}
		s.downloadAgentTaskPart(w, r, agentName, parts[0], parts[2], ordinal)
		return
	}
	if len(parts) != 1 || !agentTaskIDPattern.MatchString(subpath) {
		writeJSONError(w, http.StatusNotFound, "not_found", "task not found")
		return
	}
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	if !s.requireScope(w, r, agentName, "task-read", ScopeRequestsRead) {
		return
	}
	s.getAgentTask(w, r, agentName, subpath)
}

func (s *Server) downloadAgentTaskPart(w http.ResponseWriter, r *http.Request, agentName, taskID, resultID string, ordinal int) {
	if !s.requireTaskStore(w) || s.TaskObjectStore == nil {
		if s.TaskObjectStore == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "task_file_service_unavailable", "task file service is unavailable")
		}
		return
	}
	task, err := s.TaskStore.Get(r.Context(), taskstore.AgentRef{Namespace: s.Namespace, Name: agentName}, taskID)
	if err != nil {
		if errors.Is(err, taskstore.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "not_found", "task result not found")
		} else {
			writeJSONError(w, http.StatusServiceUnavailable, "task_store_error", "failed to read task")
		}
		return
	}
	var file *taskstore.FileMetadata
	for _, result := range task.Results {
		if result.ID == resultID && ordinal < len(result.Parts) && result.Parts[ordinal].Kind == taskstore.PartFile {
			file = result.Parts[ordinal].File
			break
		}
	}
	if file == nil {
		writeJSONError(w, http.StatusNotFound, "not_found", "task result not found")
		return
	}
	byteRange, partial, err := parseTaskByteRange(r.Header.Get("Range"), file.SizeBytes)
	if err != nil {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(file.SizeBytes, 10))
		writeJSONError(w, http.StatusRequestedRangeNotSatisfiable, "invalid_range", "requested byte range is not satisfiable")
		return
	}
	object, err := s.TaskObjectStore.Open(r.Context(), file.ObjectID, byteRange)
	if err != nil {
		if errors.Is(err, taskobject.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "not_found", "task result content not found")
		} else {
			writeJSONError(w, http.StatusServiceUnavailable, "task_file_service_unavailable", "task file service is unavailable")
		}
		return
	}
	defer object.Body.Close()
	w.Header().Set("Content-Type", file.MediaType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+taskobject.SanitizeFilename(file.Filename)+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(object.Size, 10))
	if partial {
		end := byteRange.Offset + object.Size - 1
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(byteRange.Offset, 10)+"-"+strconv.FormatInt(end, 10)+"/"+strconv.FormatInt(file.SizeBytes, 10))
		w.WriteHeader(http.StatusPartialContent)
	}
	_, _ = io.Copy(w, object.Body)
}

func parseTaskByteRange(header string, size int64) (*taskobject.ByteRange, bool, error) {
	if header == "" {
		return nil, false, nil
	}
	if size <= 0 || !strings.HasPrefix(header, "bytes=") || strings.Contains(header, ",") {
		return nil, false, taskstore.ErrInvalid
	}
	fields := strings.SplitN(strings.TrimPrefix(header, "bytes="), "-", 2)
	if len(fields) != 2 {
		return nil, false, taskstore.ErrInvalid
	}
	if fields[0] == "" {
		suffix, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || suffix <= 0 {
			return nil, false, taskstore.ErrInvalid
		}
		if suffix > size {
			suffix = size
		}
		return &taskobject.ByteRange{Offset: size - suffix, Length: suffix}, true, nil
	}
	start, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return nil, false, taskstore.ErrInvalid
	}
	end := size - 1
	if fields[1] != "" {
		end, err = strconv.ParseInt(fields[1], 10, 64)
		if err != nil || end < start {
			return nil, false, taskstore.ErrInvalid
		}
		if end >= size {
			end = size - 1
		}
	}
	return &taskobject.ByteRange{Offset: start, Length: end - start + 1}, true, nil
}

func (s *Server) requireTaskStore(w http.ResponseWriter) bool {
	if s.TaskStore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "task_service_unavailable", "durable tasks require PostgreSQL")
		return false
	}
	return true
}
func (s *Server) ensureTaskAgent(w http.ResponseWriter, r *http.Request, name string) bool {
	a := &kyberv1.Agent{}
	if err := s.K8sClient.Get(r.Context(), types.NamespacedName{Namespace: s.Namespace, Name: name}, a); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent not found")
		} else {
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get agent")
		}
		return false
	}
	if !a.Spec.RequestReplyEnabled {
		writeJSONError(w, http.StatusForbidden, "agent_tasks_disabled", "durable tasks are disabled for this agent")
		return false
	}
	return true
}

func (s *Server) createAgentTask(w http.ResponseWriter, r *http.Request, agentName string) {
	if !s.TasksEnabled {
		writeJSONError(w, http.StatusServiceUnavailable, "task_creation_disabled", "durable task creation is disabled")
		return
	}
	if !s.requireTaskStore(w) || !s.ensureTaskAgent(w, r, agentName) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAgentTaskBodyBytes)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	var in createAgentTaskInput
	if err := d.Decode(&in); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "task body exceeds the configured limit")
		} else {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		}
		return
	}
	if err := d.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "task body must contain one JSON object")
		return
	}
	caller := callerFrom(r.Context())
	if caller == nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	hash := taskCreateHash(in)
	for attempts := 0; attempts < 3; attempts++ {
		id, err := newAgentTaskID()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to create task")
			return
		}
		p := taskstore.CreateParams{ID: id, Agent: taskstore.AgentRef{Namespace: s.Namespace, Name: agentName}, CreatedBy: caller.Name, Prompt: in.Prompt, Correlation: in.Correlation, IdempotencyKey: key, RequestHash: hash}
		if in.DeadlineAt != nil {
			p.DeadlineAt = *in.DeadlineAt
		}
		result, err := s.TaskStore.Create(r.Context(), p)
		if errors.Is(err, taskstore.ErrConflict) {
			continue
		}
		if err != nil {
			writeTaskStoreError(w, err)
			return
		}
		location := "/api/v1/agents/" + agentName + "/tasks/" + result.Task.ID
		w.Header().Set("Location", location)
		if result.Replay {
			w.Header().Set("Idempotent-Replay", "true")
		}
		writeJSON(w, http.StatusAccepted, taskResponse(result.Task, true))
		return
	}
	writeJSONError(w, http.StatusServiceUnavailable, "task_store_error", "failed to allocate task ID")
}

func taskCreateHash(in createAgentTaskInput) string {
	deadline := ""
	if in.DeadlineAt != nil {
		deadline = in.DeadlineAt.UTC().Format(time.RFC3339Nano)
	}
	b, _ := json.Marshal([]string{in.Prompt, in.Correlation, deadline})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func newAgentTaskID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "task_" + hex.EncodeToString(b[:]), nil
}

func (s *Server) getAgentTask(w http.ResponseWriter, r *http.Request, agentName, id string) {
	if !s.requireTaskStore(w) {
		return
	}
	t, err := s.TaskStore.Get(r.Context(), taskstore.AgentRef{Namespace: s.Namespace, Name: agentName}, id)
	if errors.Is(err, taskstore.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, "not_found", "task not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "task_store_error", "failed to read task")
		return
	}
	writeJSON(w, http.StatusOK, taskResponse(t, true))
}

func (s *Server) listAgentTasks(w http.ResponseWriter, r *http.Request, agentName string) {
	if !s.requireTaskStore(w) {
		return
	}
	limit := 0
	var err error
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_limit", "limit must be an integer")
			return
		}
	}
	state := taskstore.State(r.URL.Query().Get("state"))
	if state != "" && state != taskstore.StateQueued && state != taskstore.StateDispatched && state != taskstore.StateCompleted && state != taskstore.StateFailed {
		writeJSONError(w, http.StatusBadRequest, "invalid_state", "unknown task state")
		return
	}
	page, err := s.TaskStore.List(r.Context(), taskstore.ListParams{Agent: taskstore.AgentRef{Namespace: s.Namespace, Name: agentName}, State: state, Limit: limit, Cursor: r.URL.Query().Get("cursor")})
	if err != nil {
		if errors.Is(err, taskstore.ErrInvalidCursor) || errors.Is(err, taskstore.ErrInvalid) {
			writeJSONError(w, http.StatusBadRequest, "invalid_page", "invalid task page parameters")
		} else {
			writeJSONError(w, http.StatusServiceUnavailable, "task_store_error", "failed to list tasks")
		}
		return
	}
	out := agentTaskListResponse{Tasks: make([]agentTaskResponse, 0, len(page.Tasks)), NextCursor: page.NextCursor}
	for _, t := range page.Tasks {
		out.Tasks = append(out.Tasks, taskResponse(t, false))
	}
	writeJSON(w, http.StatusOK, out)
}

func taskResponse(t *taskstore.Task, includeResult bool) agentTaskResponse {
	out := agentTaskResponse{ID: t.ID, Agent: t.AgentName, State: t.State, FailureCode: t.FailureCode, Correlation: t.Correlation, Version: t.Version, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt, DeadlineAt: t.DeadlineAt, RetainUntil: t.RetainUntil, CompletedAt: t.CompletedAt, Progress: t.Progress}
	if includeResult && t.State == taskstore.StateCompleted {
		out.Response = t.Response
	}
	if t.State == taskstore.StateFailed {
		switch t.FailureCode {
		case taskstore.FailureAgentUnavailable:
			out.Error = "agent was unavailable before the task deadline"
		case taskstore.FailureDelivery:
			out.Error = "task delivery failed"
		case taskstore.FailureDeliveryUnknown:
			out.Error = "task delivery could not be confirmed after an interruption"
		case taskstore.FailureDeadline:
			out.Error = "task deadline exceeded"
		default:
			out.Error = "task failed"
		}
	}
	for _, result := range t.Results {
		r := agentTaskResultResponse{ID: result.ID, Name: result.Name, PartCount: len(result.Parts), CreatedAt: result.CreatedAt}
		if includeResult {
			r.Description = result.Description
			r.Parts = make([]agentTaskPartResponse, 0, len(result.Parts))
		}
		for ordinal, part := range result.Parts {
			r.PartKinds = append(r.PartKinds, part.Kind)
			p := agentTaskPartResponse{Kind: part.Kind}
			switch part.Kind {
			case taskstore.PartText:
				r.TotalBytes += int64(len([]byte(part.Text)))
				p.Value = part.Text
			case taskstore.PartJSON:
				r.TotalBytes += int64(len(part.JSON))
				p.Value = json.RawMessage(part.JSON)
			case taskstore.PartFile:
				if part.File != nil {
					r.TotalBytes += part.File.SizeBytes
					p.File = &agentTaskFileResponse{Filename: part.File.Filename, MediaType: part.File.MediaType, Size: part.File.SizeBytes, SHA256: part.File.SHA256, ScanStatus: part.File.ScanStatus, DownloadPath: "/api/v1/agents/" + t.AgentName + "/tasks/" + t.ID + "/results/" + result.ID + "/parts/" + strconv.Itoa(ordinal) + "/content"}
				}
			}
			if includeResult {
				r.Parts = append(r.Parts, p)
			}
		}
		if includeResult {
			r.PartKinds = nil
		}
		out.Results = append(out.Results, r)
	}
	return out
}

func writeTaskStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, taskstore.ErrInvalid):
		writeJSONError(w, http.StatusBadRequest, "invalid_task", "prompt and a future deadline are required")
	case errors.Is(err, taskstore.ErrPromptTooLarge):
		writeJSONError(w, http.StatusRequestEntityTooLarge, "prompt_too_large", "prompt exceeds the configured limit")
	case errors.Is(err, taskstore.ErrCorrelationTooLarge):
		writeJSONError(w, http.StatusRequestEntityTooLarge, "correlation_too_large", "correlation exceeds the configured limit")
	case errors.Is(err, taskstore.ErrIdempotencyTooLarge):
		writeJSONError(w, http.StatusBadRequest, "idempotency_key_too_large", "Idempotency-Key exceeds 128 bytes")
	case errors.Is(err, taskstore.ErrIdempotencyConflict):
		writeJSONError(w, http.StatusConflict, "idempotency_conflict", "Idempotency-Key was already used with different input")
	case errors.Is(err, taskstore.ErrOutstandingLimit):
		w.Header().Set("Retry-After", "1")
		writeJSONError(w, http.StatusTooManyRequests, "too_many_tasks", "agent has too many outstanding tasks")
	case errors.Is(err, taskstore.ErrCapacity):
		w.Header().Set("Retry-After", "60")
		writeJSONError(w, http.StatusServiceUnavailable, "task_capacity_exhausted", "retained task capacity is exhausted")
	default:
		writeJSONError(w, http.StatusServiceUnavailable, "task_store_error", "failed to create task")
	}
}
