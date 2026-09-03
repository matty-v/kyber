package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/matty-v/kyber/pkg/taskobject"
	"golang.org/x/sys/unix"
)

const maxAgentCardBytes = 256 << 10
const maxA2AEventStreamBytes = 1 << 20
const maxA2ATaskIDBytes = 256
const maxA2AContextIDBytes = 256
const maxA2ACursorBytes = 1024
const maxA2AArtifactIDBytes = 256
const maxA2AHistoryLength = 100
const maxA2AArtifactParts = 1024
const maxA2APeerURLBytes = 2048
const maxA2ACredentialBytes = 16 << 10

var outboundA2APeerNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)

type outboundA2APeer struct {
	Name         string
	BaseURL      *url.URL
	Credential   string
	AllowPrivate bool
}

// outboundA2AMCPServer owns the runtime-facing A2A client boundary. Peer
// resolution and credentials deliberately stay behind this loopback service;
// the harness receives bounded peer names and task handles, never URLs or
// bearer values.
type outboundA2AMCPServer struct {
	peers    map[string]outboundA2APeer
	discover func(context.Context, outboundA2APeer) (any, error)
}

func (s *outboundA2AMCPServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req requestRPCRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
		s.write(w, requestRPCResponse{JSONRPC: "2.0", Error: &requestRPCError{Code: -32700, Message: "parse error"}})
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	response := requestRPCResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		response.Result = map[string]any{
			"protocolVersion": requestMCPProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "kyber-a2a", "version": "1"},
		}
	case "ping":
		response.Result = map[string]any{}
	case "tools/list":
		response.Result = map[string]any{"tools": outboundA2ATools()}
	case "tools/call":
		response.Result = s.callTool(r.Context(), req.Params)
	default:
		response.Error = &requestRPCError{Code: -32601, Message: "method not found: " + req.Method}
	}
	s.write(w, response)
}

func loadOutboundA2APeers(raw string) (map[string]outboundA2APeer, error) {
	peers := make(map[string]outboundA2APeer)
	if strings.TrimSpace(raw) == "" {
		return peers, nil
	}
	var configured []struct {
		Name          string `json:"name"`
		URL           string `json:"url"`
		CredentialEnv string `json:"credentialEnv"`
		AllowPrivate  bool   `json:"allowPrivateNetwork"`
	}
	if err := json.Unmarshal([]byte(raw), &configured); err != nil {
		return nil, fmt.Errorf("parsing KYBER_A2A_PEERS_JSON: %w", err)
	}
	if len(configured) > 16 {
		return nil, fmt.Errorf("KYBER_A2A_PEERS_JSON exceeds 16 peers")
	}
	for _, candidate := range configured {
		if len(candidate.Name) > 63 || !outboundA2APeerNamePattern.MatchString(candidate.Name) {
			return nil, fmt.Errorf("invalid outbound A2A peer name")
		}
		if _, exists := peers[candidate.Name]; exists {
			return nil, fmt.Errorf("duplicate outbound A2A peer name %q", candidate.Name)
		}
		if len(candidate.URL) > maxA2APeerURLBytes {
			return nil, fmt.Errorf("peer %q URL exceeds limit", candidate.Name)
		}
		base, err := url.Parse(candidate.URL)
		if err != nil || base.Scheme != "https" || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
			return nil, fmt.Errorf("peer %q must have an HTTPS base URL without userinfo, query, or fragment", candidate.Name)
		}
		credential, ok := os.LookupEnv(candidate.CredentialEnv)
		if !ok || strings.TrimSpace(credential) == "" || len(credential) > maxA2ACredentialBytes {
			return nil, fmt.Errorf("peer %q credential is unavailable", candidate.Name)
		}
		peers[candidate.Name] = outboundA2APeer{Name: candidate.Name, BaseURL: base, Credential: credential, AllowPrivate: candidate.AllowPrivate}
	}
	return peers, nil
}

func (s *outboundA2AMCPServer) callTool(ctx context.Context, raw json.RawMessage) requestToolResult {
	var params struct {
		Name      string `json:"name"`
		Arguments struct {
			Peer           string `json:"peer"`
			SkillID        string `json:"skill_id"`
			Message        string `json:"message"`
			ContextID      string `json:"context_id"`
			IdempotencyKey string `json:"idempotency_key"`
			TaskID         string `json:"task_id"`
			HistoryLength  *int   `json:"history_length"`
			Limit          int    `json:"limit"`
			Cursor         string `json:"cursor"`
			TimeoutSeconds int    `json:"timeout_seconds"`
			ArtifactID     string `json:"artifact_id"`
			PartIndex      int    `json:"part_index"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return outboundA2AError("invalid tool arguments")
	}
	peer, ok := s.peers[params.Arguments.Peer]
	if !ok {
		return outboundA2AError("peer is not configured for this agent")
	}
	var result any
	var err error
	switch params.Name {
	case "discover_peer":
		discover := s.discover
		if discover == nil {
			discover = discoverOutboundA2APeer
		}
		result, err = discover(ctx, peer)
	case "delegate_task":
		if err = validateOutboundA2ASkill(ctx, peer, params.Arguments.SkillID); err != nil {
			break
		}
		result, err = sendOutboundA2AMessage(ctx, peer, params.Arguments.Message, params.Arguments.ContextID, "", params.Arguments.IdempotencyKey)
	case "continue_task":
		result, err = sendOutboundA2AMessage(ctx, peer, params.Arguments.Message, "", params.Arguments.TaskID, params.Arguments.IdempotencyKey)
	case "get_task":
		result, err = getOutboundA2ATask(ctx, peer, params.Arguments.TaskID, params.Arguments.HistoryLength)
	case "list_tasks":
		result, err = listOutboundA2ATasks(ctx, peer, params.Arguments.Limit)
	case "cancel_task":
		result, err = cancelOutboundA2ATask(ctx, peer, params.Arguments.TaskID)
	case "await_task":
		result, err = awaitOutboundA2ATask(ctx, peer, params.Arguments.TaskID, params.Arguments.Cursor, params.Arguments.TimeoutSeconds)
	case "download_artifact":
		result, err = downloadOutboundA2AArtifact(ctx, peer, params.Arguments.TaskID, params.Arguments.ArtifactID, params.Arguments.PartIndex)
	default:
		return outboundA2AError("outbound A2A operation is not implemented")
	}
	if err != nil {
		return outboundA2AError("outbound A2A operation failed")
	}
	return requestToolResult{
		Content:           []requestToolContent{{Type: "text", Text: "Outbound A2A operation completed for peer " + peer.Name + "."}},
		StructuredContent: result,
	}
}

func validateOutboundA2ASkill(ctx context.Context, peer outboundA2APeer, skillID string) error {
	if skillID == "" || len(skillID) > 128 {
		return fmt.Errorf("skill ID is required and bounded")
	}
	card, err := discoverOutboundA2APeer(ctx, peer)
	if err != nil {
		return err
	}
	root, ok := card.(map[string]any)
	if !ok {
		return fmt.Errorf("Agent Card is malformed")
	}
	skills, ok := root["skills"].([]any)
	if !ok {
		return fmt.Errorf("Agent Card has no skills")
	}
	for _, raw := range skills {
		skill, ok := raw.(map[string]any)
		if ok && skill["id"] == skillID {
			return nil
		}
	}
	return fmt.Errorf("peer does not advertise requested skill")
}

func outboundA2ATransport(peer outboundA2APeer) a2aclient.Transport {
	base := &http.Transport{
		DialContext:           outboundA2ADialContext(peer.AllowPrivate),
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		DisableCompression:    true,
	}
	client := &http.Client{
		Timeout:       30 * time.Second,
		Transport:     &outboundA2AHTTPTransport{base: &outboundA2AAuthTransport{base: base, credential: peer.Credential}},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	return a2aclient.NewRESTTransport(peer.BaseURL, client)
}

type outboundA2AHTTPTransport struct {
	base http.RoundTripper
}

func (t *outboundA2AHTTPTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.ContentLength > maxAgentCardBytes {
		response.Body.Close()
		return nil, fmt.Errorf("response exceeds limit")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxAgentCardBytes+1))
	response.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if len(body) > maxAgentCardBytes {
		return nil, fmt.Errorf("response exceeds limit")
	}
	response.Body = io.NopCloser(strings.NewReader(string(body)))
	response.ContentLength = int64(len(body))
	return response, nil
}

type outboundA2AAuthTransport struct {
	base       http.RoundTripper
	credential string
}

type outboundA2AStreamTransport struct{ base http.RoundTripper }

type outboundA2ALimitedBody struct {
	io.Reader
	io.Closer
}

func (t *outboundA2AStreamTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	response.Body = &outboundA2ALimitedBody{Reader: io.LimitReader(response.Body, maxA2AEventStreamBytes), Closer: response.Body}
	return response, nil
}

func (t *outboundA2AAuthTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	request.Header = request.Header.Clone()
	request.Header.Set("Authorization", "Bearer "+t.credential)
	request.Header.Set("A2A-Version", "1.0")
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		response.Body.Close()
		return nil, fmt.Errorf("redirects are prohibited")
	}
	return response, nil
}

func sendOutboundA2AMessage(ctx context.Context, peer outboundA2APeer, text, contextID, taskID, idempotencyKey string) (any, error) {
	if strings.TrimSpace(text) == "" || len(text) > 64<<10 {
		return nil, fmt.Errorf("message is empty or exceeds limit")
	}
	message := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	if idempotencyKey != "" {
		if len(idempotencyKey) > 256 {
			return nil, fmt.Errorf("idempotency key exceeds limit")
		}
		message.ID = idempotencyKey
	}
	if len(contextID) > maxA2AContextIDBytes {
		return nil, fmt.Errorf("context ID exceeds limit")
	}
	if len(taskID) > maxA2ATaskIDBytes {
		return nil, fmt.Errorf("task ID exceeds limit")
	}
	message.ContextID = contextID
	message.TaskID = a2a.TaskID(taskID)
	request := &a2a.SendMessageRequest{Message: message, Config: &a2a.SendMessageConfig{ReturnImmediately: true}}
	result, err := outboundA2ATransport(peer).SendMessage(ctx, a2aclient.ServiceParams{}, request)
	if err != nil {
		return nil, fmt.Errorf("sending A2A message: %w", err)
	}
	return outboundA2AStructured(result)
}

func getOutboundA2ATask(ctx context.Context, peer outboundA2APeer, taskID string, historyLength *int) (any, error) {
	if taskID == "" || len(taskID) > maxA2ATaskIDBytes {
		return nil, fmt.Errorf("task ID is required and bounded")
	}
	if historyLength != nil && (*historyLength < 0 || *historyLength > maxA2AHistoryLength) {
		return nil, fmt.Errorf("history length is outside bounds")
	}
	result, err := outboundA2ATransport(peer).GetTask(ctx, a2aclient.ServiceParams{}, &a2a.GetTaskRequest{ID: a2a.TaskID(taskID), HistoryLength: historyLength})
	if err != nil {
		return nil, fmt.Errorf("getting A2A task: %w", err)
	}
	return outboundA2AStructured(result)
}

func listOutboundA2ATasks(ctx context.Context, peer outboundA2APeer, limit int) (any, error) {
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 100 {
		return nil, fmt.Errorf("list limit is outside bounds")
	}
	result, err := outboundA2ATransport(peer).ListTasks(ctx, a2aclient.ServiceParams{}, &a2a.ListTasksRequest{PageSize: limit})
	if err != nil {
		return nil, fmt.Errorf("listing A2A tasks: %w", err)
	}
	return outboundA2AStructured(result)
}

func cancelOutboundA2ATask(ctx context.Context, peer outboundA2APeer, taskID string) (any, error) {
	if taskID == "" || len(taskID) > maxA2ATaskIDBytes {
		return nil, fmt.Errorf("task ID is required and bounded")
	}
	result, err := outboundA2ATransport(peer).CancelTask(ctx, a2aclient.ServiceParams{}, &a2a.CancelTaskRequest{ID: a2a.TaskID(taskID)})
	if err != nil {
		return nil, fmt.Errorf("canceling A2A task: %w", err)
	}
	return outboundA2AStructured(result)
}

func awaitOutboundA2ATask(ctx context.Context, peer outboundA2APeer, taskID, cursor string, timeoutSeconds int) (any, error) {
	if taskID == "" || len(taskID) > maxA2ATaskIDBytes {
		return nil, fmt.Errorf("task ID is required and bounded")
	}
	if len(cursor) > maxA2ACursorBytes {
		return nil, fmt.Errorf("cursor exceeds limit")
	}
	if timeoutSeconds == 0 {
		timeoutSeconds = 30
	}
	if timeoutSeconds < 1 || timeoutSeconds > 60 {
		return nil, fmt.Errorf("timeout is outside bounds")
	}
	base := &http.Transport{
		DialContext:           outboundA2ADialContext(peer.AllowPrivate),
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		DisableCompression:    true,
	}
	client := &http.Client{
		Transport:     &outboundA2AStreamTransport{base: &outboundA2AAuthTransport{base: base, credential: peer.Credential}},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	transport := a2aclient.NewRESTTransport(peer.BaseURL, client)
	params := a2aclient.ServiceParams{}
	if cursor != "" {
		params["Last-Event-ID"] = []string{cursor}
	}
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()
	events := make([]any, 0, 16)
	for event, eventErr := range transport.SubscribeToTask(waitCtx, params, &a2a.SubscribeToTaskRequest{ID: a2a.TaskID(taskID)}) {
		if eventErr != nil {
			if waitCtx.Err() != nil {
				break
			}
			return nil, fmt.Errorf("following A2A task: %w", eventErr)
		}
		structured, err := outboundA2AStructured(event)
		if err != nil {
			return nil, err
		}
		events = append(events, structured)
		if len(events) >= 128 {
			break
		}
	}
	return map[string]any{"taskId": taskID, "events": events, "timedOut": waitCtx.Err() != nil}, nil
}

const maxOutboundArtifactBytes = 20 << 20

func downloadOutboundA2AArtifact(ctx context.Context, peer outboundA2APeer, taskID, artifactID string, partIndex int) (any, error) {
	if taskID == "" || len(taskID) > maxA2ATaskIDBytes || artifactID == "" || len(artifactID) > maxA2AArtifactIDBytes || partIndex < 0 || partIndex >= maxA2AArtifactParts {
		return nil, fmt.Errorf("task, artifact, and part index are required and bounded")
	}
	task, err := outboundA2ATransport(peer).GetTask(ctx, a2aclient.ServiceParams{}, &a2a.GetTaskRequest{ID: a2a.TaskID(taskID)})
	if err != nil {
		return nil, fmt.Errorf("getting A2A artifact task: %w", err)
	}
	var part *a2a.Part
	for _, artifact := range task.Artifacts {
		if string(artifact.ID) == artifactID && partIndex < len(artifact.Parts) {
			part = artifact.Parts[partIndex]
			break
		}
	}
	if part == nil {
		return nil, fmt.Errorf("artifact part was not found")
	}
	var content []byte
	if raw := part.Raw(); raw != nil {
		content = raw
	} else if remote := part.URL(); remote != "" {
		content, err = fetchOutboundA2AArtifact(ctx, peer, string(remote))
		if err != nil {
			return nil, err
		}
	} else {
		return nil, fmt.Errorf("artifact part is not a file")
	}
	filename := taskobject.SanitizeFilename(part.Filename)
	if filename == "" {
		filename = "artifact.bin"
	}
	path, err := writeOutboundA2AArtifact(taskID, artifactID, partIndex, filename, content)
	if err != nil {
		return nil, err
	}
	return map[string]any{"path": path, "filename": filename, "mediaType": part.MediaType, "sizeBytes": len(content)}, nil
}

func fetchOutboundA2AArtifact(ctx context.Context, peer outboundA2APeer, reference string) ([]byte, error) {
	resolved, err := peer.BaseURL.Parse(reference)
	if err != nil || resolved.Scheme != peer.BaseURL.Scheme || resolved.Host != peer.BaseURL.Host || resolved.User != nil {
		return nil, fmt.Errorf("artifact URL is outside the configured peer origin")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, resolved.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("building artifact request: %w", err)
	}
	base := &http.Transport{DialContext: outboundA2ADialContext(peer.AllowPrivate), TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second, DisableCompression: true}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &outboundA2AAuthTransport{base: base, credential: peer.Credential}, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetching artifact: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching artifact: unexpected status %d", response.StatusCode)
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, maxOutboundArtifactBytes+1))
	if err != nil || len(content) > maxOutboundArtifactBytes {
		return nil, fmt.Errorf("reading artifact failed or exceeded limit")
	}
	return content, nil
}

func writeOutboundA2AArtifact(taskID, artifactID string, partIndex int, filename string, content []byte) (string, error) {
	root, err := unix.Open("/persist", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", fmt.Errorf("opening persist root: %w", err)
	}
	defer unix.Close(root)
	if err := unix.Mkdirat(root, "a2a-results", 0o755); err != nil && err != unix.EEXIST {
		return "", fmt.Errorf("creating A2A results root: %w", err)
	}
	results, err := unix.Openat(root, "a2a-results", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", fmt.Errorf("opening A2A results root: %w", err)
	}
	defer unix.Close(results)
	dirname := taskobject.SanitizeFilename(taskID + "-" + artifactID)
	if err := unix.Mkdirat(results, dirname, 0o755); err != nil && err != unix.EEXIST {
		return "", fmt.Errorf("creating artifact directory: %w", err)
	}
	dir, err := unix.Openat(results, dirname, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", fmt.Errorf("opening artifact directory: %w", err)
	}
	defer unix.Close(dir)
	name := fmt.Sprintf("%d-%s", partIndex, filename)
	fd, err := unix.Openat(dir, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0o644)
	if err != nil {
		return "", fmt.Errorf("creating artifact file: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	if _, err := file.Write(content); err != nil {
		file.Close()
		_ = unix.Unlinkat(dir, name, 0)
		return "", fmt.Errorf("writing artifact file: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = unix.Unlinkat(dir, name, 0)
		return "", fmt.Errorf("closing artifact file: %w", err)
	}
	return filepath.Join("/persist/a2a-results", dirname, name), nil
}

func outboundA2AStructured(value any) (any, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encoding A2A result: %w", err)
	}
	var result any
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decoding A2A result: %w", err)
	}
	return result, nil
}

func discoverOutboundA2APeer(ctx context.Context, peer outboundA2APeer) (any, error) {
	cardURL := *peer.BaseURL
	cardURL.Path = strings.TrimRight(cardURL.Path, "/") + "/.well-known/agent-card.json"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, cardURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("building Agent Card request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+peer.Credential)
	request.Header.Set("A2A-Version", "1.0")
	client := &http.Client{
		Timeout:       10 * time.Second,
		Transport:     &http.Transport{DialContext: outboundA2ADialContext(peer.AllowPrivate)},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetching Agent Card: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching Agent Card: unexpected status %d", response.StatusCode)
	}
	if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "application/json") {
		return nil, fmt.Errorf("fetching Agent Card: unexpected content type")
	}
	limited := io.LimitReader(response.Body, maxAgentCardBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("reading Agent Card: %w", err)
	}
	if len(body) > maxAgentCardBytes {
		return nil, fmt.Errorf("reading Agent Card: response exceeds limit")
	}
	var card any
	if err := json.Unmarshal(body, &card); err != nil {
		return nil, fmt.Errorf("decoding Agent Card: %w", err)
	}
	return card, nil
}

func outboundA2ADialContext(allowPrivate bool) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("parsing peer address: %w", err)
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolving peer host: %w", err)
		}
		if len(addresses) == 0 {
			return nil, fmt.Errorf("peer host has no addresses")
		}
		for _, candidate := range addresses {
			ip := candidate.IP
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
				return nil, fmt.Errorf("peer host resolves to a prohibited address")
			}
			if !allowPrivate && ip.IsPrivate() {
				return nil, fmt.Errorf("peer host resolves to private address space")
			}
		}
		dialer := &net.Dialer{Timeout: 5 * time.Second}
		return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].IP.String(), port))
	}
}

func outboundA2AError(message string) requestToolResult {
	return requestToolResult{Content: []requestToolContent{{Type: "text", Text: message}}, IsError: true}
}

func outboundA2ATools() []map[string]any {
	peer := map[string]any{"type": "string", "minLength": 1, "maxLength": 63, "pattern": outboundA2APeerNamePattern.String()}
	taskID := map[string]any{"type": "string", "minLength": 1, "maxLength": maxA2ATaskIDBytes}
	message := map[string]any{"type": "string", "minLength": 1, "maxLength": 64 << 10}
	idempotencyKey := map[string]any{"type": "string", "maxLength": 256}
	object := func(properties map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "additionalProperties": false, "properties": properties, "required": required}
	}
	tool := func(name, description string, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "description": description, "inputSchema": schema}
	}
	return []map[string]any{
		tool("discover_peer", "Read the bounded Agent Card for an operator-configured peer.", object(map[string]any{"peer": peer}, "peer")),
		tool("delegate_task", "Delegate a new durable task to an operator-configured peer.", object(map[string]any{"peer": peer, "skill_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}, "message": message, "context_id": map[string]any{"type": "string", "maxLength": maxA2AContextIDBytes}, "idempotency_key": idempotencyKey}, "peer", "skill_id", "message")),
		tool("get_task", "Get a delegated task by durable handle.", object(map[string]any{"peer": peer, "task_id": taskID, "history_length": map[string]any{"type": "integer", "minimum": 0, "maximum": maxA2AHistoryLength}}, "peer", "task_id")),
		tool("list_tasks", "List this source agent's tasks for one configured peer.", object(map[string]any{"peer": peer, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}}, "peer")),
		tool("await_task", "Wait for bounded task progress using resumable A2A events.", object(map[string]any{"peer": peer, "task_id": taskID, "cursor": map[string]any{"type": "string", "maxLength": maxA2ACursorBytes}, "timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 60}}, "peer", "task_id")),
		tool("continue_task", "Continue an input-required delegated task.", object(map[string]any{"peer": peer, "task_id": taskID, "message": message, "idempotency_key": idempotencyKey}, "peer", "task_id", "message")),
		tool("cancel_task", "Request cancellation of a delegated task.", object(map[string]any{"peer": peer, "task_id": taskID}, "peer", "task_id")),
		tool("download_artifact", "Download one authorized artifact part beneath the managed results directory.", object(map[string]any{"peer": peer, "task_id": taskID, "artifact_id": map[string]any{"type": "string", "minLength": 1, "maxLength": maxA2AArtifactIDBytes}, "part_index": map[string]any{"type": "integer", "minimum": 0, "maximum": maxA2AArtifactParts - 1}}, "peer", "task_id", "artifact_id", "part_index")),
	}
}

func (s *outboundA2AMCPServer) write(w http.ResponseWriter, response requestRPCResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}
