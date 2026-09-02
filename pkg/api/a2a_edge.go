package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/capabilities"
	"github.com/matty-v/kyber/pkg/taskstore"
)

const (
	a2aAgentPrefix = "/a2a/v1/agents/"
	a2aCardPath    = "/.well-known/agent-card.json"
	a2aMaxBody     = 256 << 10
)

func (s *Server) handleA2A(w http.ResponseWriter, r *http.Request) {
	remainder := strings.TrimPrefix(r.URL.Path, a2aAgentPrefix)
	parts := strings.SplitN(remainder, "/", 2)
	if len(parts) != 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	agent, child := parts[0], "/"+parts[1]
	if len(utilvalidation.IsDNS1123Subdomain(agent)) != 0 {
		http.NotFound(w, r)
		return
	}
	if child == a2aCardPath {
		s.handleA2ACard(w, r, agent)
		return
	}
	if s.TaskStore == nil || !s.TasksEnabled {
		writeA2AEdgeError(w, http.StatusServiceUnavailable, a2a.ErrUnsupportedOperation, "durable task service is unavailable")
		return
	}
	if len(r.Header.Values("A2A-Version")) != 1 || r.Header.Get("A2A-Version") != string(a2a.Version) {
		writeA2AEdgeError(w, http.StatusBadRequest, a2a.ErrVersionNotSupported, "Kyber supports A2A-Version 1.0")
		return
	}
	w.Header().Set("A2A-Version", string(a2a.Version))
	if encoding := strings.TrimSpace(r.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		writeA2AEdgeError(w, http.StatusUnsupportedMediaType, a2a.ErrUnsupportedContentType, "content encoding is not supported")
		return
	}
	accept := strings.ToLower(r.Header.Get("Accept"))
	isStream := child == "/message:stream" || strings.HasSuffix(child, ":subscribe")
	if accept != "" && !strings.Contains(accept, "*/*") && !strings.Contains(accept, "application/json") && !(isStream && strings.Contains(accept, "text/event-stream")) {
		writeA2AEdgeError(w, http.StatusNotAcceptable, a2a.ErrUnsupportedContentType, "requested response media type is not supported")
		return
	}
	needsJSONBody := child == "/message:send" || child == "/message:stream" || (r.Method == http.MethodPost && strings.Contains(child, "/pushNotificationConfigs"))
	if needsJSONBody {
		mediaType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
		if mediaType != "application/json" {
			writeA2AEdgeError(w, http.StatusUnsupportedMediaType, a2a.ErrUnsupportedContentType, "Content-Type must be application/json")
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, a2aMaxBody+1))
		if err != nil {
			writeA2AEdgeError(w, http.StatusBadRequest, a2a.ErrParseError, "invalid JSON body")
			return
		}
		if len(raw) > a2aMaxBody {
			writeA2AEdgeError(w, http.StatusRequestEntityTooLarge, a2a.ErrInvalidRequest, "request body exceeds the configured limit")
			return
		}
		if err := validateA2AJSON(raw); err != nil {
			writeA2AEdgeError(w, http.StatusBadRequest, a2a.ErrParseError, "invalid JSON body")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
	}
	if isStream {
		release, retryAfter, ok := s.acquireTaskEventStream(r, agent+":"+child)
		if !ok {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
			writeA2AEdgeError(w, http.StatusTooManyRequests, a2a.ErrServerError, "task event stream capacity exceeded")
			return
		}
		defer release()
		ctx, cancel := context.WithTimeout(r.Context(), taskEventMaxAge)
		defer cancel()
		go func() {
			ticker := time.NewTicker(taskEventHeartbeat)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if !s.taskEventCallerStillAuthorized(r, agent) {
						cancel()
						return
					}
				}
			}
		}()
		r = r.WithContext(ctx)
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path, r2.URL.RawPath = child, ""
	a2asrv.NewRESTHandler(&a2aTaskHandler{server: s, agent: agent}, a2asrv.WithTransportKeepAlive(taskEventHeartbeat)).ServeHTTP(w, r2)
}

func validateA2AJSON(raw []byte) error {
	if len(raw) == 0 || !utf8.Valid(raw) {
		return errors.New("invalid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := validateA2AJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("JSON body must contain one value")
	}
	return nil
}

func validateA2AJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("JSON exceeds maximum depth")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		count := 0
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return errors.New("duplicate object key")
			}
			seen[key] = struct{}{}
			count++
			if count > 256 {
				return errors.New("object exceeds maximum fields")
			}
			if err := validateA2AJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("unterminated object")
		}
	case '[':
		count := 0
		for decoder.More() {
			count++
			if count > 256 {
				return errors.New("array exceeds maximum items")
			}
			if err := validateA2AJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("unterminated array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

func writeA2AEdgeError(w http.ResponseWriter, status int, cause error, message string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "https://a2a-protocol.org/errors/" + strings.ToLower(a2a.ErrorReason(cause)), "title": message, "status": status})
}

func (s *Server) handleA2ACard(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	caller := callerFrom(r.Context())
	if caller == nil || !caller.AgentResources.Has(s.Namespace+"/"+name) {
		http.NotFound(w, r)
		return
	}
	card, err := s.a2aCard(r, name)
	if err != nil {
		if errors.Is(err, errA2ACardNotFound) {
			http.NotFound(w, r)
		} else {
			writeA2AEdgeError(w, http.StatusServiceUnavailable, a2a.ErrInternalError, "agent card unavailable")
		}
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=10, must-revalidate")
	w.Header().Set("Content-Type", "application/json")
	encoded, err := json.Marshal(card)
	if err != nil {
		writeA2AEdgeError(w, http.StatusInternalServerError, a2a.ErrInternalError, "agent card unavailable")
		return
	}
	sum := sha256.Sum256(encoded)
	etag := fmt.Sprintf(`"sha256:%x"`, sum)
	w.Header().Set("ETag", etag)
	if strings.TrimSpace(r.Header.Get("If-None-Match")) == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(append(encoded, '\n'))
}

var errA2ACardNotFound = errors.New("A2A agent card not found")

func (s *Server) a2aCard(r *http.Request, name string) (*a2a.AgentCard, error) {
	if !s.TasksEnabled || s.TaskStore == nil {
		return nil, errors.New("durable task service unavailable")
	}
	if _, ok := s.TaskStore.(taskstore.EventStore); !ok {
		return nil, errors.New("durable task event service unavailable")
	}
	if s.K8sClient == nil {
		return nil, errors.New("Kubernetes client unavailable")
	}
	agent := &kyberv1.Agent{}
	if err := s.K8sClient.Get(r.Context(), types.NamespacedName{Namespace: s.Namespace, Name: name}, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, errA2ACardNotFound
		}
		return nil, err
	}
	if agent.Spec.PublicCapabilities == nil {
		return nil, errA2ACardNotFound
	}
	manifest, digest, err := capabilities.NormalizeAndValidate(agent.Spec.PublicCapabilities)
	if err != nil {
		return nil, err
	}
	status := agent.Status.PublicCapabilities
	if status == nil || status.ObservedGeneration != agent.Generation || status.ManifestRevision != digest {
		return nil, errors.New("capability manifest pending")
	}
	valid := meta.FindStatusCondition(status.Conditions, "Valid")
	if valid == nil || valid.Status != "True" {
		return nil, errors.New("capability manifest invalid")
	}
	available := make(map[string]bool, len(status.Capabilities))
	for _, item := range status.Capabilities {
		available[item.ID] = item.Availability == "available"
	}
	base := strings.TrimRight(s.PublicURL, "/") + a2aAgentPrefix + name + "/"
	if strings.TrimSpace(s.PublicURL) == "" {
		base = a2aAgentPrefix + name + "/"
	}
	card := &a2a.AgentCard{
		Name: manifest.Identity.DisplayName, Description: manifest.Identity.Description, DocumentationURL: manifest.Identity.DocumentationURL,
		Version: digest, SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(base, a2a.TransportProtocolHTTPJSON)},
		Capabilities:         a2a.AgentCapabilities{Streaming: true},
		SecuritySchemes:      a2a.NamedSecuritySchemes{a2a.SecuritySchemeName("bearer"): a2a.HTTPAuthSecurityScheme{Scheme: "Bearer", Description: "Kyber MAT-23 bearer principal"}},
		SecurityRequirements: a2a.SecurityRequirementsOptions{a2a.SecurityRequirements{a2a.SecuritySchemeName("bearer"): {}}},
		Skills:               []a2a.AgentSkill{}, DefaultInputModes: []string{}, DefaultOutputModes: []string{},
	}
	inputs, outputs := map[string]bool{}, map[string]bool{}
	for _, capability := range manifest.Capabilities {
		if !available[capability.ID] {
			continue
		}
		inputModes := a2aSupportedInputModes(capability.InputModes)
		if len(inputModes) == 0 {
			continue
		}
		card.Skills = append(card.Skills, a2a.AgentSkill{ID: capability.ID, Name: capability.Name, Description: capability.Description, InputModes: inputModes, OutputModes: capability.OutputModes})
		for _, mode := range inputModes {
			inputs[mode] = true
		}
		for _, mode := range capability.OutputModes {
			outputs[mode] = true
		}
	}
	for mode := range inputs {
		card.DefaultInputModes = append(card.DefaultInputModes, mode)
	}
	for mode := range outputs {
		card.DefaultOutputModes = append(card.DefaultOutputModes, mode)
	}
	sort.Strings(card.DefaultInputModes)
	sort.Strings(card.DefaultOutputModes)
	return card, nil
}

func a2aSupportedInputModes(modes []string) []string {
	out := make([]string, 0, len(modes))
	for _, mode := range modes {
		switch mode {
		case "text/plain", "text/markdown", "application/json":
			out = append(out, mode)
		}
	}
	return out
}
