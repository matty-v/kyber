package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/inbound"
)

// inboundDebugBodyLimit caps the debug request body. 1 MiB matches the
// receiver's inboundBodyLimit so an operator can paste any payload the
// receiver itself would accept.
const inboundDebugBodyLimit = 1 << 20

// InboundDebugRequest is the JSON body for POST /api/v1/inbound-debug. The
// binding spec is the same shape as AgentInboundBinding on the CRD; we
// don't require it to exist on a real agent — the debugger is a pure
// dispatcher dry-run. Headers and Payload are what the upstream provider
// would have sent.
type InboundDebugRequest struct {
	Binding kyberv1.AgentInboundBinding `json:"binding"`
	Payload json.RawMessage             `json:"payload"`
	// Headers is a flat map of the request headers the dispatcher should
	// see. Operators only care about EventHeader / SignatureHeader, but
	// any keys are accepted and forwarded as a single-value http.Header.
	Headers map[string]string `json:"headers,omitempty"`
	// Agent is plumbed into the rendered envelope (the "agent=" segment
	// of the envelope's banner line) for operator readability. Defaults
	// to "debug-agent" when unset.
	Agent string `json:"agent,omitempty"`
}

// InboundDebugResponse is the JSON body returned by POST /api/v1/inbound-debug.
// Mirrors inbound.DecisionDebug at the wire level so a future change to the
// trace shape only needs to update the inbound package.
type InboundDebugResponse struct {
	Match         bool                  `json:"match"`
	DropReason    string                `json:"dropReason,omitempty"`
	ResolvedEvent string                `json:"resolvedEvent"`
	FilterResults []inbound.FilterResult `json:"filterResults"`
	FieldResults  []inbound.FieldResult  `json:"fieldResults"`
	Envelope      string                `json:"envelope,omitempty"`
}

// handleInboundDebug serves POST /api/v1/inbound-debug — operator tooling
// that runs the dispatcher pipeline on a synthetic payload + binding spec
// and returns a per-step trace. No HMAC verification, no dedup, no queue
// — purely the in-memory dispatcher logic.
//
// Validation here mirrors the create-binding validators (name +
// signatureHeader + action are required) so an operator's first paste
// produces the same shape of error the management API would.
func (s *Server) handleInboundDebug(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, inboundDebugBodyLimit)
	var req InboundDebugRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
				"request body exceeds 1 MiB")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}

	// Field-level validation — same shape used by createInboundBinding so
	// an operator who pastes a half-formed spec sees a familiar error.
	if !bindingNameRe.MatchString(req.Binding.Name) {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"binding.name must be lowercase alphanumeric + hyphens, 1-63 chars (DNS-1123)",
			"binding.name")
		return
	}
	if req.Binding.SignatureHeader == "" {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"binding.signatureHeader is required", "binding.signatureHeader")
		return
	}
	if req.Binding.Action == "" {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"binding.action is required", "binding.action")
		return
	}

	// Validate the payload is real JSON. The receiver's evalJSONPath
	// silently coerces a non-JSON body to an empty doc — fine in
	// production but a footgun in the debugger where the operator wants
	// to know their paste was malformed.
	if len(req.Payload) == 0 {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"payload is required", "payload")
		return
	}
	var probe interface{}
	if err := json.Unmarshal(req.Payload, &probe); err != nil {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"payload is not valid JSON: "+err.Error(), "payload")
		return
	}

	// Build an http.Header from the flat map the operator supplied.
	headers := http.Header{}
	for k, v := range req.Headers {
		headers.Set(k, v)
	}

	agent := req.Agent
	if agent == "" {
		agent = "debug-agent"
	}
	requestID := "debug-" + uuid.New().String()

	dbg := inbound.DecideDebug([]byte(req.Payload), headers, requestID, agent, req.Binding)

	resp := InboundDebugResponse{
		Match:         dbg.Match,
		DropReason:    dbg.DropReason,
		ResolvedEvent: dbg.ResolvedEvent,
		FilterResults: dbg.FilterResults,
		FieldResults:  dbg.FieldResults,
		Envelope:      dbg.Envelope,
	}
	writeJSON(w, http.StatusOK, resp)
}
