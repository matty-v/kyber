package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/matty-v/kyber/pkg/taskobject"
	"github.com/matty-v/kyber/pkg/taskstore"
)

var taskIDPattern = regexp.MustCompile(`^task_[a-f0-9]{32}$`)
var attemptIDPattern = regexp.MustCompile(`^attempt_[a-f0-9]{32}$`)
var updateIDPattern = regexp.MustCompile(`^update_[a-f0-9]{32}$`)
var resultIDPattern = regexp.MustCompile(`^result_[a-f0-9]{32}$`)

func (s *InternalServer) handleTaskReceiptPost(w http.ResponseWriter, r *http.Request, agent string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.taskStore == nil {
		http.Error(w, "task service unavailable", http.StatusServiceUnavailable)
		return
	}
	var receipt taskstore.Receipt
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	if d.Decode(&receipt) != nil || d.Decode(&struct{}{}) != io.EOF || !taskIDPattern.MatchString(receipt.TaskID) || !attemptIDPattern.MatchString(receipt.AttemptID) {
		http.Error(w, "invalid task receipt", http.StatusBadRequest)
		return
	}
	task, created, err := s.taskStore.AcceptReceipt(r.Context(), taskstore.AgentRef{Namespace: s.namespace, Name: agent}, receipt)
	if err != nil {
		writeInternalTaskError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"receipt": receipt, "taskVersion": task.Version})
}

func (s *InternalServer) handleTaskReceiptGet(w http.ResponseWriter, r *http.Request, agent, attempt string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.taskStore == nil {
		http.Error(w, "task service unavailable", http.StatusServiceUnavailable)
		return
	}
	if !attemptIDPattern.MatchString(attempt) {
		http.Error(w, "invalid attempt", http.StatusBadRequest)
		return
	}
	receipt, err := s.taskStore.GetReceipt(r.Context(), taskstore.AgentRef{Namespace: s.namespace, Name: agent}, attempt)
	if err != nil {
		writeInternalTaskError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(receipt)
}

func (s *InternalServer) handleTaskComplete(w http.ResponseWriter, r *http.Request, agent string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.taskStore == nil {
		http.Error(w, "task service unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		TaskID    string `json:"task_id"`
		AttemptID string `json:"attempt_id"`
		Response  string `json:"response"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, taskstore.HardMaxResponseBytes*6+2048))
	d.DisallowUnknownFields()
	if d.Decode(&body) != nil || d.Decode(&struct{}{}) != io.EOF || !taskIDPattern.MatchString(body.TaskID) || !attemptIDPattern.MatchString(body.AttemptID) || body.Response == "" {
		http.Error(w, "invalid task completion", http.StatusBadRequest)
		return
	}
	a := taskstore.AgentRef{Namespace: s.namespace, Name: agent}
	receipt, err := s.taskStore.GetReceipt(r.Context(), a, body.AttemptID)
	if err != nil || receipt.TaskID != body.TaskID {
		if err == nil {
			err = taskstore.ErrReceiptConflict
		}
		writeInternalTaskError(w, err)
		return
	}
	task, err := s.taskStore.Get(r.Context(), a, body.TaskID)
	if err != nil {
		writeInternalTaskError(w, err)
		return
	}
	if err = s.taskStore.Complete(r.Context(), a, body.TaskID, task.Version, body.Response); err != nil {
		writeInternalTaskError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *InternalServer) handleTaskProgress(w http.ResponseWriter, r *http.Request, agent string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.taskStore == nil {
		http.Error(w, "task service unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		TaskID    string `json:"task_id"`
		AttemptID string `json:"attempt_id"`
		UpdateID  string `json:"update_id"`
		Message   string `json:"message"`
		Percent   *int   `json:"percent"`
	}
	if !decodeInternalTaskBody(w, r, taskstore.HardMaxProgressBytes*6+2048, &body) ||
		!taskIDPattern.MatchString(body.TaskID) || !attemptIDPattern.MatchString(body.AttemptID) ||
		!updateIDPattern.MatchString(body.UpdateID) || strings.TrimSpace(body.Message) == "" ||
		(body.Percent != nil && (*body.Percent < 0 || *body.Percent > 100)) {
		http.Error(w, "invalid task progress", http.StatusBadRequest)
		return
	}
	a := taskstore.AgentRef{Namespace: s.namespace, Name: agent}
	if err := s.requireCurrentTaskAttempt(r, a, body.TaskID, body.AttemptID); err != nil {
		writeInternalTaskError(w, err)
		return
	}
	progress, replay, err := s.taskStore.ReportProgress(r.Context(), a, body.TaskID, body.AttemptID, taskstore.ProgressUpdate{UpdateID: body.UpdateID, Message: body.Message, Percent: body.Percent})
	if err != nil {
		writeInternalTaskError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"progress": progress})
}

func (s *InternalServer) handleTaskResult(w http.ResponseWriter, r *http.Request, agent string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.taskStore == nil {
		http.Error(w, "task service unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		TaskID      string          `json:"task_id"`
		AttemptID   string          `json:"attempt_id"`
		ResultID    string          `json:"result_id"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Kind        string          `json:"kind"`
		Text        string          `json:"text"`
		Value       json.RawMessage `json:"value"`
	}
	if !decodeInternalTaskBody(w, r, taskstore.HardMaxJSONPartBytes*6+taskstore.HardMaxTextPartBytes*6+4096, &body) ||
		!taskIDPattern.MatchString(body.TaskID) || !attemptIDPattern.MatchString(body.AttemptID) ||
		!resultIDPattern.MatchString(body.ResultID) || strings.TrimSpace(body.Name) == "" {
		http.Error(w, "invalid task result", http.StatusBadRequest)
		return
	}
	a := taskstore.AgentRef{Namespace: s.namespace, Name: agent}
	if err := s.requireCurrentTaskAttempt(r, a, body.TaskID, body.AttemptID); err != nil {
		writeInternalTaskError(w, err)
		return
	}
	part := taskstore.ResultPart{ID: body.ResultID + "_0"}
	switch body.Kind {
	case string(taskstore.PartText):
		part.Kind, part.Text = taskstore.PartText, body.Text
	case string(taskstore.PartJSON):
		canonical, err := canonicalTaskJSON(body.Value)
		if err != nil {
			http.Error(w, "invalid JSON result", http.StatusBadRequest)
			return
		}
		part.Kind, part.JSON = taskstore.PartJSON, canonical
	default:
		http.Error(w, "invalid task result kind", http.StatusBadRequest)
		return
	}
	result := taskstore.Result{ID: body.ResultID, Name: body.Name, Description: body.Description, Parts: []taskstore.ResultPart{part}}
	stored, replay, err := s.taskStore.PublishResult(r.Context(), a, body.TaskID, body.AttemptID, result)
	if err != nil {
		writeInternalTaskError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"result": stored})
}

func (s *InternalServer) handleTaskFile(w http.ResponseWriter, r *http.Request, agent string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.taskStore == nil || s.taskObjectStore == nil {
		http.Error(w, "task file service unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.ContentLength < 0 || r.ContentLength > taskstore.HardMaxFileBytes {
		http.Error(w, "file exceeds the platform hard limit", http.StatusRequestEntityTooLarge)
		return
	}
	encoded := r.Header.Get("X-Kyber-Task-Metadata")
	if len(encoded) == 0 || len(encoded) > 16<<10 {
		http.Error(w, "invalid task file metadata", http.StatusBadRequest)
		return
	}
	metadataJSON, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		http.Error(w, "invalid task file metadata", http.StatusBadRequest)
		return
	}
	var metadata struct {
		TaskID            string `json:"task_id"`
		AttemptID         string `json:"attempt_id"`
		ResultID          string `json:"result_id"`
		Name              string `json:"name"`
		Description       string `json:"description"`
		Filename          string `json:"filename"`
		DeclaredMediaType string `json:"declared_media_type"`
		SniffedMediaType  string `json:"sniffed_media_type"`
	}
	d := json.NewDecoder(bytes.NewReader(metadataJSON))
	d.DisallowUnknownFields()
	if d.Decode(&metadata) != nil || d.Decode(&struct{}{}) != io.EOF ||
		!taskIDPattern.MatchString(metadata.TaskID) || !attemptIDPattern.MatchString(metadata.AttemptID) ||
		!resultIDPattern.MatchString(metadata.ResultID) || strings.TrimSpace(metadata.Name) == "" ||
		len([]byte(metadata.Name)) > taskstore.HardMaxResultNameBytes ||
		len([]byte(metadata.Description)) > taskstore.HardMaxDescriptionBytes ||
		len([]byte(metadata.Filename)) > taskstore.HardMaxFilenameBytes {
		http.Error(w, "invalid task file metadata", http.StatusBadRequest)
		return
	}
	a := taskstore.AgentRef{Namespace: s.namespace, Name: agent}
	if err := s.requireCurrentTaskAttempt(r, a, metadata.TaskID, metadata.AttemptID); err != nil {
		writeInternalTaskError(w, err)
		return
	}
	reader := bufio.NewReader(io.LimitReader(r.Body, r.ContentLength+1))
	serverSniffed, err := taskobject.SniffMediaType(reader)
	if err != nil {
		http.Error(w, "invalid task file", http.StatusBadRequest)
		return
	}
	mediaType := reconcileTaskMediaType(metadata.DeclaredMediaType, metadata.SniffedMediaType, serverSniffed)
	objectID, err := newTaskObjectID(s.namespace, agent, metadata.TaskID, metadata.ResultID)
	if err != nil {
		http.Error(w, "task file service unavailable", http.StatusServiceUnavailable)
		return
	}
	filename := taskobject.SanitizeFilename(metadata.Filename)
	if err := s.taskStore.PrepareFileUpload(r.Context(), a, metadata.TaskID, metadata.AttemptID, taskstore.PendingFile{
		ObjectID: objectID, ResultID: metadata.ResultID, Name: metadata.Name,
		Filename: filename, MediaType: mediaType, SizeBytes: r.ContentLength,
	}); err != nil {
		writeInternalTaskError(w, err)
		return
	}
	hash := sha256.New()
	if err := s.taskObjectStore.Put(r.Context(), objectID, io.TeeReader(reader, hash), r.ContentLength, taskobject.PutOptions{Filename: filename, ContentType: mediaType}); err != nil {
		_ = s.taskStore.AbandonFileUpload(context.WithoutCancel(r.Context()), objectID)
		http.Error(w, "task file storage unavailable", http.StatusServiceUnavailable)
		return
	}
	file := &taskstore.FileMetadata{ObjectID: objectID, Filename: filename, MediaType: mediaType, SHA256: hex.EncodeToString(hash.Sum(nil)), SizeBytes: r.ContentLength, ScanStatus: "not_configured"}
	result := taskstore.Result{ID: metadata.ResultID, Name: metadata.Name, Description: metadata.Description, Parts: []taskstore.ResultPart{{ID: metadata.ResultID + "_0", Kind: taskstore.PartFile, File: file}}}
	stored, replay, err := s.taskStore.PublishResult(r.Context(), a, metadata.TaskID, metadata.AttemptID, result)
	if err != nil {
		// Commit can fail ambiguously after PostgreSQL accepted the result. Read
		// back before abandoning so a referenced object is never deleted.
		if task, getErr := s.taskStore.Get(context.WithoutCancel(r.Context()), a, metadata.TaskID); getErr == nil {
			for i := range task.Results {
				if task.Results[i].ID == metadata.ResultID && resultUsesObject(task.Results[i], objectID) {
					stored = &task.Results[i]
					err = nil
					break
				}
			}
			if err != nil {
				_ = s.taskStore.AbandonFileUpload(context.WithoutCancel(r.Context()), objectID)
			}
		}
	}
	if err != nil {
		writeInternalTaskError(w, err)
		return
	}
	if replay {
		_ = s.taskStore.AbandonFileUpload(context.WithoutCancel(r.Context()), objectID)
	}
	w.Header().Set("Content-Type", "application/json")
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"result": stored})
}

func resultUsesObject(result taskstore.Result, objectID string) bool {
	for i := range result.Parts {
		if result.Parts[i].File != nil && result.Parts[i].File.ObjectID == objectID {
			return true
		}
	}
	return false
}

func newTaskObjectID(namespace, agent, taskID, resultID string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "task-results/" + namespace + "/" + agent + "/" + taskID + "/" + resultID + "/object_" + hex.EncodeToString(b[:]), nil
}

func reconcileTaskMediaType(declared, sidecarSniffed, serverSniffed string) string {
	declared = canonicalTaskMediaType(declared)
	sidecarSniffed = canonicalTaskMediaType(sidecarSniffed)
	serverSniffed = canonicalTaskMediaType(serverSniffed)
	if serverSniffed == "" {
		serverSniffed = "application/octet-stream"
	}
	if sidecarSniffed != "" && sidecarSniffed != serverSniffed {
		return "application/octet-stream"
	}
	if declared == "" || declared == "application/octet-stream" {
		return serverSniffed
	}
	if declared == serverSniffed || serverSniffed == "application/octet-stream" {
		return declared
	}
	return "application/octet-stream"
}

func canonicalTaskMediaType(value string) string {
	value = strings.ToLower(strings.TrimSpace(strings.SplitN(value, ";", 2)[0]))
	switch value {
	case "audio/wav", "audio/x-wav", "audio/vnd.wave":
		// net/http.DetectContentType reports RIFF/WAVE content as audio/wave.
		return "audio/wave"
	default:
		return value
	}
}

func decodeInternalTaskBody(w http.ResponseWriter, r *http.Request, limit int, dst any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, int64(limit)))
	d.DisallowUnknownFields()
	return d.Decode(dst) == nil && d.Decode(&struct{}{}) == io.EOF
}

func (s *InternalServer) requireCurrentTaskAttempt(r *http.Request, a taskstore.AgentRef, taskID, attemptID string) error {
	receipt, err := s.taskStore.GetReceipt(r.Context(), a, attemptID)
	if err != nil {
		return err
	}
	if receipt.TaskID != taskID {
		return taskstore.ErrInvalidAttempt
	}
	return nil
}

func canonicalTaskJSON(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || len(raw) > taskstore.HardMaxJSONPartBytes {
		return nil, taskstore.ErrResultTooLarge
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := decodeUniqueJSONValue(dec, &value, 0); err != nil {
		return nil, err
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return nil, taskstore.ErrInvalid
	}
	return json.Marshal(value)
}

func decodeUniqueJSONValue(dec *json.Decoder, dst *any, depth int) error {
	if depth > 64 {
		return taskstore.ErrInvalid
	}
	token, err := dec.Token()
	if err != nil {
		return err
	}
	switch token := token.(type) {
	case json.Delim:
		switch token {
		case '{':
			object := map[string]any{}
			for dec.More() {
				keyToken, err := dec.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return taskstore.ErrInvalid
				}
				if _, exists := object[key]; exists {
					return taskstore.ErrInvalid
				}
				var child any
				if err := decodeUniqueJSONValue(dec, &child, depth+1); err != nil {
					return err
				}
				object[key] = child
			}
			if end, err := dec.Token(); err != nil || end != json.Delim('}') {
				return taskstore.ErrInvalid
			}
			*dst = object
		case '[':
			var array []any
			for dec.More() {
				var child any
				if err := decodeUniqueJSONValue(dec, &child, depth+1); err != nil {
					return err
				}
				array = append(array, child)
			}
			if end, err := dec.Token(); err != nil || end != json.Delim(']') {
				return taskstore.ErrInvalid
			}
			*dst = array
		default:
			return taskstore.ErrInvalid
		}
	default:
		*dst = token
	}
	return nil
}

func writeInternalTaskError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, taskstore.ErrInvalid):
		http.Error(w, "invalid task operation", http.StatusBadRequest)
	case errors.Is(err, taskstore.ErrNotFound):
		http.Error(w, "task or receipt not found", http.StatusNotFound)
	case errors.Is(err, taskstore.ErrConflict), errors.Is(err, taskstore.ErrReceiptConflict), errors.Is(err, taskstore.ErrInvalidAttempt), errors.Is(err, taskstore.ErrUpdateConflict), errors.Is(err, taskstore.ErrResultConflict):
		http.Error(w, "task operation conflicts with current state", http.StatusConflict)
	case errors.Is(err, taskstore.ErrResponseTooLarge), errors.Is(err, taskstore.ErrProgressTooLarge), errors.Is(err, taskstore.ErrResultTooLarge):
		http.Error(w, "response too large", http.StatusRequestEntityTooLarge)
	case errors.Is(err, taskstore.ErrUpdateLimit), errors.Is(err, taskstore.ErrResultLimit):
		http.Error(w, "task mutation limit reached", http.StatusTooManyRequests)
	default:
		http.Error(w, "task store unavailable", http.StatusServiceUnavailable)
	}
}
