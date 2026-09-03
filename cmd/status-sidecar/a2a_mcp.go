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
	"regexp"
	"strings"
	"time"
)

const maxAgentCardBytes = 256 << 10

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
		if !outboundA2APeerNamePattern.MatchString(candidate.Name) {
			return nil, fmt.Errorf("invalid outbound A2A peer name")
		}
		if _, exists := peers[candidate.Name]; exists {
			return nil, fmt.Errorf("duplicate outbound A2A peer name %q", candidate.Name)
		}
		base, err := url.Parse(candidate.URL)
		if err != nil || base.Scheme != "https" || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
			return nil, fmt.Errorf("peer %q must have an HTTPS base URL without userinfo, query, or fragment", candidate.Name)
		}
		credential, ok := os.LookupEnv(candidate.CredentialEnv)
		if !ok || strings.TrimSpace(credential) == "" {
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
			Peer string `json:"peer"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return outboundA2AError("invalid tool arguments")
	}
	peer, ok := s.peers[params.Arguments.Peer]
	if !ok {
		return outboundA2AError("peer is not configured for this agent")
	}
	if params.Name != "discover_peer" {
		return outboundA2AError("outbound A2A operation is not implemented")
	}
	discover := s.discover
	if discover == nil {
		discover = discoverOutboundA2APeer
	}
	card, err := discover(ctx, peer)
	if err != nil {
		return outboundA2AError("peer discovery failed")
	}
	return requestToolResult{
		Content:           []requestToolContent{{Type: "text", Text: "Discovered configured A2A peer " + peer.Name + "."}},
		StructuredContent: card,
	}
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
	peer := map[string]any{"type": "string", "minLength": 1, "maxLength": 128}
	taskID := map[string]any{"type": "string", "minLength": 1, "maxLength": 256}
	object := func(properties map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "additionalProperties": false, "properties": properties, "required": required}
	}
	tool := func(name, description string, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "description": description, "inputSchema": schema}
	}
	return []map[string]any{
		tool("discover_peer", "Read the bounded Agent Card for an operator-configured peer.", object(map[string]any{"peer": peer}, "peer")),
		tool("delegate_task", "Delegate a new durable task to an operator-configured peer.", object(map[string]any{"peer": peer, "skill_id": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"}, "context_id": map[string]any{"type": "string"}, "idempotency_key": map[string]any{"type": "string"}}, "peer", "skill_id", "message")),
		tool("get_task", "Get a delegated task by durable handle.", object(map[string]any{"peer": peer, "task_id": taskID, "history_length": map[string]any{"type": "integer", "minimum": 0}, "include_artifacts": map[string]any{"type": "boolean"}}, "peer", "task_id")),
		tool("list_tasks", "List this source agent's tasks for one configured peer.", object(map[string]any{"peer": peer, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}}, "peer")),
		tool("await_task", "Wait for bounded task progress using resumable A2A events.", object(map[string]any{"peer": peer, "task_id": taskID, "cursor": map[string]any{"type": "string"}, "timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 60}}, "peer", "task_id")),
		tool("continue_task", "Continue an input-required delegated task.", object(map[string]any{"peer": peer, "task_id": taskID, "message": map[string]any{"type": "string"}, "idempotency_key": map[string]any{"type": "string"}}, "peer", "task_id", "message")),
		tool("cancel_task", "Request cancellation of a delegated task.", object(map[string]any{"peer": peer, "task_id": taskID}, "peer", "task_id")),
		tool("download_artifact", "Download one authorized artifact part beneath the managed results directory.", object(map[string]any{"peer": peer, "task_id": taskID, "artifact_id": map[string]any{"type": "string"}, "part_index": map[string]any{"type": "integer", "minimum": 0}}, "peer", "task_id", "artifact_id", "part_index")),
	}
}

func (s *outboundA2AMCPServer) write(w http.ResponseWriter, response requestRPCResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}
