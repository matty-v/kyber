package api

import (
	"encoding/json"
	"net/http"
)

// ErrorDetail is the inner object of an error response body.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

// ErrorResponse is the JSON body returned for all error responses.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// writeJSONError writes a JSON error response with the given HTTP status, error code, and message.
func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	writeJSONErrorWithField(w, status, code, message, "")
}

// writeJSONErrorWithField writes a JSON error response including an optional field name.
func writeJSONErrorWithField(w http.ResponseWriter, status int, code, message, field string) {
	resp := ErrorResponse{
		Error: ErrorDetail{
			Code:    code,
			Message: message,
			Field:   field,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

// writeJSON writes a JSON response with the given HTTP status and value.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
