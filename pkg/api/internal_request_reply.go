package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/matty-v/kyber/pkg/requeststore"
)

const maxInternalRequestReplyBodyBytes = requeststore.HardMaxResponseBytes*6 + 1024

// handleRequestReply accepts an explicit response from the authenticated
// target agent. authorizeAgentSelf has already bound the pod identity to
// agentName before this handler runs.
func (s *InternalServer) handleRequestReply(w http.ResponseWriter, r *http.Request, agentName string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.requestStore == nil {
		http.Error(w, "request replies are unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		RequestID string `json:"request_id"`
		Response  string `json:"response"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxInternalRequestReplyBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, "request reply body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid request reply", http.StatusBadRequest)
		return
	}
	if !agentRequestIDPattern.MatchString(body.RequestID) {
		http.Error(w, "invalid request reply", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid request reply", http.StatusBadRequest)
		return
	}
	if err := s.requestStore.Complete(r.Context(), agentName, body.RequestID, body.Response); err != nil {
		switch {
		case errors.Is(err, requeststore.ErrResponseTooLarge):
			http.Error(w, "response too large", http.StatusRequestEntityTooLarge)
		case errors.Is(err, requeststore.ErrNotFound):
			http.Error(w, "request not found or expired", http.StatusNotFound)
		case errors.Is(err, requeststore.ErrConflict):
			http.Error(w, "request response conflicts with current state", http.StatusConflict)
		default:
			http.Error(w, "request store unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
