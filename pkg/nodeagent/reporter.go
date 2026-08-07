package nodeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// ResourceReporter periodically collects host metrics and POSTs them to the
// control plane's POST /internal/nodes/{name}/resources endpoint (kyber#343).
// This runs alongside the existing OTEL push loop on the node-agent's 30s timer.
type ResourceReporter struct {
	// ControlPlaneURL is the base URL of the control plane, e.g.
	// "http://kyber-control-plane.kyber-system:8082". Required.
	ControlPlaneURL string

	// NodeName is the name of this node as known to Kubernetes.
	// Read from the KYBER_NODE_NAME env var (set by the Helm chart via
	// the Downward API). Required.
	NodeName string

	// PodToken is optional; sent as Bearer when non-empty for forward-compat
	// with internal auth. Loaded from the pod-token Secret mount path.
	PodToken string

	// HTTPClient is optional; defaults to a 5s-timeout client.
	HTTPClient *http.Client
}

// NewResourceReporter builds a ResourceReporter from environment variables.
// Returns nil when KYBER_CONTROL_PLANE_INTERNAL_URL is not set (OTEL-only mode).
func NewResourceReporter() *ResourceReporter {
	cpURL := os.Getenv("KYBER_CONTROL_PLANE_INTERNAL_URL")
	if cpURL == "" {
		return nil
	}
	return &ResourceReporter{
		ControlPlaneURL: cpURL,
		NodeName:        os.Getenv("KYBER_NODE_NAME"),
		PodToken:        LoadPodToken(),
	}
}

// DefaultPodTokenPath is where the control-plane-signed node-agent pod-token
// Secret is mounted (kyber#566). The DaemonSet mounts the kyber-node-agent-token
// Secret here; the value authenticates the node-agent's calls to the internal
// API's machine/node routes.
const DefaultPodTokenPath = "/var/run/secrets/kyber/pod-token"

// LoadPodToken reads the mounted pod-token, trimming trailing whitespace. It
// returns "" when the token is not mounted (e.g. during the #566 migration
// before the Secret is delivered) — callers send no Bearer in that case, which
// the internal API's grace mode tolerates. The path is overridable via
// KYBER_POD_TOKEN_PATH (tests, non-default mounts).
func LoadPodToken() string {
	path := os.Getenv("KYBER_POD_TOKEN_PATH")
	if path == "" {
		path = DefaultPodTokenPath
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

type resourcePayload struct {
	CPUPercent     float64 `json:"cpuPercent"`
	MemUsedBytes   float64 `json:"memUsedBytes"`
	MemTotalBytes  float64 `json:"memTotalBytes"`
	DiskUsedBytes  float64 `json:"diskUsedBytes"`
	DiskTotalBytes float64 `json:"diskTotalBytes"`
	UpdatedAt      string  `json:"updatedAt"`
}

// Report POSTs the given Metrics snapshot to the control plane. Errors are
// logged but never returned — resource reporting must not crash the agent.
func (r *ResourceReporter) Report(ctx context.Context, m *Metrics) {
	if r == nil || r.ControlPlaneURL == "" || r.NodeName == "" {
		return
	}
	client := r.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}

	payload := resourcePayload{
		CPUPercent:     m.CPUPercent,
		MemUsedBytes:   float64(m.MemoryUsed),
		MemTotalBytes:  float64(m.MemoryTotal),
		DiskUsedBytes:  float64(m.DiskUsed),
		DiskTotalBytes: float64(m.DiskTotal),
		UpdatedAt:      m.Timestamp.UTC().Format(time.RFC3339),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[resource-reporter] marshal error: %v", err)
		return
	}

	url := fmt.Sprintf("%s/internal/nodes/%s/resources", r.ControlPlaneURL, r.NodeName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		log.Printf("[resource-reporter] build request error: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if r.PodToken != "" {
		req.Header.Set("Authorization", "Bearer "+r.PodToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[resource-reporter] post error: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		log.Printf("[resource-reporter] post status=%d", resp.StatusCode)
	}
}
