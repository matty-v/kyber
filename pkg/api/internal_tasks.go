package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"

	"github.com/matty-v/kyber/pkg/taskstore"
)

var taskIDPattern = regexp.MustCompile(`^task_[a-f0-9]{32}$`)
var attemptIDPattern = regexp.MustCompile(`^attempt_[a-f0-9]{32}$`)

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

func writeInternalTaskError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, taskstore.ErrInvalid):
		http.Error(w, "invalid task operation", http.StatusBadRequest)
	case errors.Is(err, taskstore.ErrNotFound):
		http.Error(w, "task or receipt not found", http.StatusNotFound)
	case errors.Is(err, taskstore.ErrConflict), errors.Is(err, taskstore.ErrReceiptConflict):
		http.Error(w, "task operation conflicts with current state", http.StatusConflict)
	case errors.Is(err, taskstore.ErrResponseTooLarge):
		http.Error(w, "response too large", http.StatusRequestEntityTooLarge)
	default:
		http.Error(w, "task store unavailable", http.StatusServiceUnavailable)
	}
}
