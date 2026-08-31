// Package api implements the Kyber control plane HTTP API.
//
// B2 adds the internal API subtree (/internal/*) used by agent init containers.
// The public API routes (/api/v1/*) are deferred to C1.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/briefstore"
	"github.com/matty-v/kyber/pkg/githubapp"
	"github.com/matty-v/kyber/pkg/metricsstore"
	"github.com/matty-v/kyber/pkg/modelprobe"
	"github.com/matty-v/kyber/pkg/requeststore"
	"github.com/matty-v/kyber/pkg/runtimedetect"
	"github.com/matty-v/kyber/pkg/skillstore"
	"github.com/matty-v/kyber/pkg/statechangestore"
	"github.com/matty-v/kyber/pkg/taskstore"
	"github.com/matty-v/kyber/pkg/telemetry"
	"github.com/matty-v/kyber/pkg/tokenreport"
	"github.com/matty-v/kyber/pkg/tokenstore"
)

// MaxJobRunsPerJob caps the number of AgentJobRun entries retained on
// Agent.status.jobs[] for any single job name. The ring buffer drops the
// oldest entry for that job when a new event pushes the count over this
// limit. 50 balances history depth (useful for debugging flaky jobs)
// against CR size (one entry ~200B, 50×jobs×agents stays well under the
// 1.5MB etcd object-size budget).
//
// The per-name cap only bounds status.jobs when the set of job NAMES is
// small (the scheduled crons: compact-memory, digest, recover-stalls, …).
// It does nothing for high-cardinality names — see MaxJobRunsTotal and the
// inbound- exclusion in handleJobEvent, added after the Lando CR-bloat
// outage where 13.5k unique inbound-<requestID> entries pushed the Agent
// CR past etcd's 2MB write limit and wedged the controller (kyber#622).
const MaxJobRunsPerJob = 50

// MaxJobRunsTotal is a defence-in-depth global cap on the TOTAL length of
// Agent.status.jobs[], enforced across all job names after the per-name
// trim. With the inbound- exclusion in handleJobEvent the array is already
// bounded by (#scheduled-job-names × MaxJobRunsPerJob) — a few hundred at
// most — so this ceiling is a runaway backstop, not a normal-path limit:
// no legitimate scheduled-job workload approaches it, but it guarantees the
// CR can never again grow unbounded from this field regardless of what job
// names appear. 500 entries × ~200B ≈ 100KB, comfortably under the 2MB CR
// budget with all other status fields.
const MaxJobRunsTotal = 500

// MaxInboundRunsPerBinding caps the number of AgentInboundRun entries
// retained on Agent.status.inboundRuns[] for any single binding name. Same
// per-binding ring-buffer semantics as MaxJobRunsPerJob.
const MaxInboundRunsPerBinding = 50

// DefaultInternalPort is the default listen address for the internal HTTP API.
// It must differ from the controller-runtime metrics port (:8080) and health probe port (:8081).
const DefaultInternalPort = ":8082"

// PreemptionHandler is called when a node agent reports imminent preemption.
type PreemptionHandler func(machineName, instanceId string)

// InternalServer serves the cluster-internal API endpoints used by pods and init containers.
//
// Callers are authenticated and authorized per-identity when an authenticator is
// wired via WithInternalAuth (kyber#566): every request must present a
// control-plane-signed pod-token, and each handler enforces act-on-self-only —
// an agent may act only on its own resources. Without WithInternalAuth the
// server is unauthenticated (pre-#566 behavior; used by tests and the first
// phase of the migration). See internal_auth.go.
type InternalServer struct {
	store             briefstore.BriefStore
	server            *http.Server
	preemptionHandler PreemptionHandler
	k8sClient         client.Client // for rotation endpoint's secret patch
	namespace         string        // e.g. "kyber-system"

	// identityRepoTokenMinter, when non-nil, lets the
	// GET /internal/agents/{name}/identity-repo-token endpoint mint a
	// short-lived GitHub token scoped to the calling agent's own identity
	// repo. Nil on installs without a Kyber App wired — the endpoint then
	// returns 503 and the agent's identity-repo git fails loudly (there is no
	// PAT fallback for the identity repo; identity-repo management is simply
	// disabled on such installs).
	identityRepoTokenMinter IdentityRepoTokenMinter

	// internalAuth, when non-nil, gates every internal route on a valid
	// per-identity pod-token (kyber#566). internalAuthGrace selects the
	// one-release accept-and-log-unauthenticated migration posture. See
	// WithInternalAuth.
	internalAuth      InternalAuthenticator
	internalAuthGrace bool

	// internalAuthFailClosed, when true with internalAuth == nil, makes every
	// internal route refuse to serve (503) — the fail-closed posture for a
	// production deploy whose signing key is missing (kyber#566 revision). It
	// is distinct from internalAuth == nil && !internalAuthFailClosed, which is
	// the intentional unauthenticated back-compat mode (tests / pre-#566). See
	// WithInternalAuthFailClosed.
	internalAuthFailClosed bool
	tokenStore             tokenstore.TokenStore
	tokenAccumulator       tokenstore.Accumulator
	metricsStore           metricsstore.MetricsStore
	nodeStore              metricsstore.NodeStore
	stateChangeAccum       statechangestore.Accumulator
	runtimeDetectCache     runtimedetect.Cache
	skillStore             skillstore.Store
	requestStore           requeststore.Store
	taskStore              taskstore.DispatchStore
	agentMetrics           AgentMetricsProvider

	// snapshotMu and snapshotPrior track the last cumulative activity_state_seconds
	// per (agent, state) so handleStatusSnapshot can store incremental delta seconds
	// rather than raw running totals. The sidecar sends cumulative totals; the stored
	// series uses delta-per-interval semantics so the PWA can sum points over a window
	// to get total seconds (equivalent to Prometheus increase() on the counter).
	snapshotMu    sync.Mutex
	snapshotPrior map[string]map[string]float64 // agentName → state → prior cumulative

	// jobEventMu serializes status.jobs[] writes per agent name. The status
	// subresource atomically replaces list fields, so two parallel POSTs
	// against the same agent would otherwise lose an append (both read the
	// same baseline, each appends locally, last writer wins). Keyed by
	// agent name; coarse-but-correct — ~1 fire/min per job in practice,
	// contention is negligible.
	jobEventMu   map[string]*sync.Mutex
	jobEventMuMu sync.Mutex
}

// InternalServerOption is a functional option for NewInternalServer.
type InternalServerOption func(*InternalServer)

// WithPreemptionHandler registers a callback invoked when a node agent reports
// imminent preemption via POST /internal/machines/{name}/preemption-notice.
func WithPreemptionHandler(h PreemptionHandler) InternalServerOption {
	return func(s *InternalServer) {
		s.preemptionHandler = h
	}
}

// WithTokenStore wires a TokenStore so the POST /internal/agents/{name}/token-usage
// endpoint can persist snapshots.
func WithTokenStore(ts tokenstore.TokenStore) InternalServerOption {
	return func(s *InternalServer) {
		s.tokenStore = ts
	}
}

// WithTokenAccumulator wires an Accumulator so the POST /internal/agents/{name}/token-usage
// endpoint increments persistent per-agent/model token counts for the Tier 1 metrics panel.
func WithTokenAccumulator(acc tokenstore.Accumulator) InternalServerOption {
	return func(s *InternalServer) {
		s.tokenAccumulator = acc
	}
}

// WithMetricsStore wires a MetricsStore so the POST /internal/agents/{name}/status
// endpoint can persist activity time-series.
func WithMetricsStore(ms metricsstore.MetricsStore) InternalServerOption {
	return func(s *InternalServer) {
		s.metricsStore = ms
	}
}

// WithNodeStore wires a NodeStore so the POST /internal/nodes/{name}/resources
// endpoint can persist node resource samples.
func WithNodeStore(ns metricsstore.NodeStore) InternalServerOption {
	return func(s *InternalServer) {
		s.nodeStore = ns
	}
}

// WithStateChangeAccumulator wires a state-change Accumulator so the
// POST /internal/agents/{name}/status endpoint can record state transitions.
func WithStateChangeAccumulator(acc statechangestore.Accumulator) InternalServerOption {
	return func(s *InternalServer) {
		s.stateChangeAccum = acc
	}
}

// WithSkillStore wires a skillstore so POST /internal/agents/{name}/skills can
// persist the skill report an agent scanned from its own filesystem. Omit it
// and the endpoint 503s; the in-pod reporter logs that and gives up for this
// boot rather than retrying, since a missing store is a deployment fact, not a
// transient one.
func WithSkillStore(st skillstore.Store) InternalServerOption {
	return func(s *InternalServer) {
		s.skillStore = st
	}
}

// WithRequestStore wires the bounded request/reply store so an authenticated
// agent pod can complete only its own dispatched requests.
func WithRequestStore(st requeststore.Store) InternalServerOption {
	return func(s *InternalServer) {
		s.requestStore = st
	}
}

func WithTaskStore(st taskstore.DispatchStore) InternalServerOption {
	return func(s *InternalServer) { s.taskStore = st }
}

// WithKubeClient wires a controller-runtime client and namespace into the InternalServer
// so the OAuth rotation endpoint can read and patch Secrets.
func WithKubeClient(c client.Client, namespace string) InternalServerOption {
	return func(s *InternalServer) {
		s.k8sClient = c
		s.namespace = namespace
	}
}

// WithAgentMetricsProvider wires live Kubernetes container metrics into
// resource status events. The sidecar still owns persistent-volume sampling.
func WithAgentMetricsProvider(provider AgentMetricsProvider) InternalServerOption {
	return func(s *InternalServer) { s.agentMetrics = provider }
}

// IdentityRepoTokenMinter mints a short-lived GitHub App installation token
// narrowed to specific repositories and permissions. Satisfied directly by
// *githubapp.Client (MintScopedToken). Declared as an interface so the
// internal server can be tested without a real GitHub App.
type IdentityRepoTokenMinter interface {
	MintScopedToken(ctx context.Context, repositories []string, permissions map[string]string) (*githubapp.InstallationToken, error)
}

// WithIdentityRepoTokenMinter wires a scoped-token minter so the
// GET /internal/agents/{name}/identity-repo-token endpoint can hand an agent a
// short-lived token scoped to its own identity repo (kyber#508 Stage 3/4).
// Omit it on installs without a Kyber App: the endpoint returns 503 and the
// agent's identity-repo git fails loudly (no PAT fallback — identity-repo
// management is disabled on such installs).
func WithIdentityRepoTokenMinter(m IdentityRepoTokenMinter) InternalServerOption {
	return func(s *InternalServer) {
		s.identityRepoTokenMinter = m
	}
}

// SetRuntimeDetectCache wires the shared catalog cache after construction.
// main constructs the internal server before optional runtime detection.
func (s *InternalServer) SetRuntimeDetectCache(cache runtimedetect.Cache) {
	s.runtimeDetectCache = cache
}

// NewInternalServer constructs an InternalServer backed by the given BriefStore.
// Call Start(addr) to begin listening.
func NewInternalServer(store briefstore.BriefStore, opts ...InternalServerOption) *InternalServer {
	s := &InternalServer{store: store, jobEventMu: map[string]*sync.Mutex{}}
	for _, opt := range opts {
		opt(s)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/agents/", s.handleAgentRoutes)
	mux.HandleFunc("/internal/machines/", s.handleMachineRoutes)
	mux.HandleFunc("/internal/nodes/", s.handleNodeRoutes)
	s.server = &http.Server{
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	return s
}

// Handler returns the HTTP handler for the internal API.
// Exposed for use with httptest.NewServer in tests and manager.Add integration.
func (s *InternalServer) Handler() http.Handler {
	return s.server.Handler
}

// Start begins listening on addr. It blocks until the server stops.
// Intended to be called in a goroutine or via manager.Add(RunnableFunc(...)).
func (s *InternalServer) Start(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.server.Addr = addr

	// Shut down when context is cancelled.
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.server.Shutdown(shutdownCtx)
	}()

	if err := s.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// handleMachineRoutes handles routes under /internal/machines/.
// Currently handles: POST /internal/machines/{name}/preemption-notice
func (s *InternalServer) handleMachineRoutes(w http.ResponseWriter, r *http.Request) {
	// Path: /internal/machines/{name}/preemption-notice
	suffix := strings.TrimPrefix(r.URL.Path, "/internal/machines/")
	parts := strings.SplitN(suffix, "/", 2)
	if len(parts) != 2 || parts[1] != "preemption-notice" || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	// Machine routes are node-agent-only (kyber#566): an agent identity must
	// not be able to spoof a preemption notice for a machine.
	if !s.authorizeNodeAgent(w, r) {
		return
	}
	machineName := parts[0]

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Timestamp  string `json:"timestamp"`
		InstanceId string `json:"instanceId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if s.preemptionHandler != nil {
		s.preemptionHandler(machineName, req.InstanceId)
	}

	w.WriteHeader(http.StatusOK)
}

// handleAgentRoutes is the dispatcher for all routes under /internal/agents/.
// It parses the agent name and sub-path then delegates to the appropriate handler.
func (s *InternalServer) handleAgentRoutes(w http.ResponseWriter, r *http.Request) {
	suffix := strings.TrimPrefix(r.URL.Path, "/internal/agents/")
	parts := strings.SplitN(suffix, "/", 2)
	if len(parts) != 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	agentName := parts[0]
	// Reject names that exceed the Kubernetes DNS subdomain limit (253 chars).
	if len(agentName) > 253 {
		http.Error(w, "invalid agent name", http.StatusBadRequest)
		return
	}
	// Per-agent authz (kyber#566): the caller may act only on its own agent
	// resources. Enforced before any handler so it covers every agent route.
	if !s.authorizeAgentSelf(w, r, agentName) {
		return
	}
	if strings.HasPrefix(parts[1], "task-receipts/") {
		s.handleTaskReceiptGet(w, r, agentName, strings.TrimPrefix(parts[1], "task-receipts/"))
		return
	}
	switch parts[1] {
	case "session-brief":
		s.handleSessionBrief(w, r, agentName)
	case "refresh-token":
		s.handleOAuthRotation(w, r, agentName)
	case "codex-auth":
		s.handleCodexAuthRotation(w, r, agentName)
	case "token-usage":
		s.handleTokenUsagePost(w, r, agentName)
	case "job-events":
		s.handleJobEvent(w, r, agentName)
	case "runtime-version":
		s.handleRuntimeVersion(w, r, agentName)
	case "runtime-catalog":
		s.handleRuntimeCatalog(w, r, agentName)
	case "skills":
		s.handleSkillsReport(w, r, agentName)
	case "request-reply":
		s.handleRequestReply(w, r, agentName)
	case "task-complete":
		s.handleTaskComplete(w, r, agentName)
	case "task-receipts":
		s.handleTaskReceiptPost(w, r, agentName)
	case "status-event":
		s.handleStatusEvent(w, r, agentName)
	case "status":
		s.handleStatusSnapshot(w, r, agentName)
	case "identity-repo-token":
		s.handleIdentityRepoToken(w, r, agentName)
	default:
		http.NotFound(w, r)
	}
}

// handleRuntimeCatalog accepts the picker-visible model catalog discovered by
// an authenticated runtime. Codex obtains this through app-server model/list,
// so the result reflects the agent owner's ChatGPT subscription.
func (s *InternalServer) handleRuntimeCatalog(w http.ResponseWriter, r *http.Request, agentName string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.runtimeDetectCache == nil {
		http.Error(w, "not configured", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Runtime string                `json:"runtime"`
		Models  []runtimedetect.Model `json:"models"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
	if err := dec.Decode(&body); err != nil || (body.Runtime != "codex" && body.Runtime != "claude-code") || len(body.Models) == 0 || len(body.Models) > 100 {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	models := make([]runtimedetect.Model, 0, len(body.Models))
	const maxReportedContextWindow = int64(10_000_000)
	for _, model := range body.Models {
		model.ID = strings.TrimSpace(model.ID)
		model.DisplayName = strings.TrimSpace(model.DisplayName)
		if model.ID == "" || len(model.ID) > 128 || len(model.DisplayName) > 256 {
			http.Error(w, "invalid model", http.StatusBadRequest)
			return
		}
		if model.DisplayName == "" {
			model.DisplayName = model.ID
		}
		if model.ContextWindowKnown && (model.ContextWindow <= 0 || model.ContextWindow > maxReportedContextWindow) {
			http.Error(w, "missing or invalid context window", http.StatusBadRequest)
			return
		}
		if body.Runtime == "claude-code" && !model.ContextWindowKnown {
			http.Error(w, "missing authoritative context window", http.StatusBadRequest)
			return
		}
		models = append(models, model)
	}
	if catalogs, ok := s.runtimeDetectCache.(runtimedetect.AgentCatalogCache); ok {
		if err := catalogs.PutAgentModels(r.Context(), agentName, models); err != nil {
			writeJSONError(w, http.StatusServiceUnavailable, "catalog_unavailable", "runtime catalog storage is unavailable")
			return
		}
	} else {
		writeJSONError(w, http.StatusServiceUnavailable, "catalog_unavailable", "runtime catalog storage does not support agent catalogs")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleIdentityRepoToken handles GET /internal/agents/{name}/identity-repo-token.
// It mints a short-lived GitHub App installation token scoped to the agent's
// OWN identity repo (contents:write) and returns it as JSON. Authentication and
// same-agent authorization already ran in handleAgentRoutes, so agentName is
// the caller's verified pod-token identity — an agent cannot mint another
// agent's token (kyber#508 Stage 3/4).
//
// The token is a secret: the response is marked no-store and the token value is
// never logged. On any failure the agent's git credential helper emits no
// credential (there is NO PAT fallback for the identity repo — a broken App flow
// must surface loudly, not be masked), so this endpoint returns plain status
// codes and leaks no GitHub error detail to the client.
func (s *InternalServer) handleIdentityRepoToken(w http.ResponseWriter, r *http.Request, agentName string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.k8sClient == nil || s.identityRepoTokenMinter == nil {
		// Not wired (install without a Kyber App): the agent's identity-repo git
		// fails loudly (no PAT fallback). 503 signals "feature not configured".
		http.Error(w, "identity-repo-token not configured", http.StatusServiceUnavailable)
		return
	}

	// Resolve the agent's own identity repo slug from its CR spec.
	agent := &kyberv1.Agent{}
	if err := s.k8sClient.Get(r.Context(), types.NamespacedName{Name: agentName, Namespace: s.namespace}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "agent lookup failed", http.StatusInternalServerError)
		return
	}
	slug := agent.Spec.IdentityRepo.Repo
	if slug == "" {
		http.Error(w, "no identity repo configured", http.StatusNotFound)
		return
	}
	parts := strings.SplitN(slug, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		http.Error(w, "invalid identity repo slug", http.StatusInternalServerError)
		return
	}
	// GitHub's access_tokens `repositories` field scopes by BARE repo name within
	// the App installation's account, so we pass parts[1] only. This trusts the
	// identity-repo owner (parts[0]) to be that same account — which holds because
	// the identity-repo owner is the install-configured account the App is
	// installed on (KYBER_IDENTITY_REPO_OWNER). A mismatched owner fails closed:
	// the agent's clone URL uses the full slug, so a token minted against the
	// wrong account simply won't authenticate.
	repoName := parts[1]

	tok, err := s.identityRepoTokenMinter.MintScopedToken(
		r.Context(), []string{repoName}, map[string]string{"contents": "write"})
	if err != nil {
		// Log server-side (no token value); return a generic error so GitHub
		// detail (which can name the app/repo) never reaches the client.
		slog.Error("identity-repo-token mint failed", "agent", agentName, "err", err)
		http.Error(w, "mint failed", http.StatusBadGateway)
		return
	}
	if tok == nil {
		// Defensive: a well-behaved minter returns an error rather than (nil, nil),
		// but never dereference a nil token at the trust boundary.
		slog.Error("identity-repo-token minter returned nil token", "agent", agentName)
		http.Error(w, "mint failed", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		Repo      string `json:"repo"`
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}{
		Repo:      slug,
		Token:     tok.Token,
		ExpiresAt: tok.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// handleSessionBrief handles GET /internal/agents/{name}/session-brief.
// It returns the stored JSON brief for the named agent, 404 if not found, 500 on store error.
func (s *InternalServer) handleSessionBrief(w http.ResponseWriter, r *http.Request, agentName string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	brief, err := s.store.Get(r.Context(), agentName)
	if err != nil {
		if errors.Is(err, briefstore.ErrBriefNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(brief); err != nil {
		// Response headers already sent; nothing useful we can do.
		return
	}
}

// handleOAuthRotation handles POST /internal/agents/{name}/refresh-token.
// It reads the agent's <name>-oauth Secret and updates whichever credential
// fields are present in the body (access_token, refresh_token, expires_at).
// Fields not present in the body are preserved unchanged.
//
// Returns 204 on success, 400 on bad body, 503 when not configured, 500 on k8s errors.
func (s *InternalServer) handleOAuthRotation(w http.ResponseWriter, r *http.Request, agent string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.k8sClient == nil {
		http.Error(w, "not configured", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    int64  `json:"expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.AccessToken == "" && body.RefreshToken == "" && body.ExpiresAt == 0 {
		http.Error(w, "body must contain at least one of access_token, refresh_token, expires_at", http.StatusBadRequest)
		return
	}

	sec := &corev1.Secret{}
	key := types.NamespacedName{Name: agent + "-oauth", Namespace: s.namespace}
	if err := s.k8sClient.Get(r.Context(), key, sec); err != nil {
		http.Error(w, "secret lookup failed", http.StatusInternalServerError)
		return
	}
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	if body.AccessToken != "" {
		sec.Data["access_token"] = []byte(body.AccessToken)
	}
	if body.RefreshToken != "" {
		sec.Data["refresh_token"] = []byte(body.RefreshToken)
	}
	if body.ExpiresAt != 0 {
		sec.Data["expires_at"] = []byte(strconv.FormatInt(body.ExpiresAt, 10))
	}
	if err := s.k8sClient.Update(r.Context(), sec); err != nil {
		http.Error(w, "secret update failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCodexAuthRotation handles POST /internal/agents/{name}/codex-auth.
// It replaces the auth.json key of the agent's <name>-codex-auth Secret with
// the document supplied by the in-pod syncer.
//
// This is the Codex counterpart of handleOAuthRotation, and it is what keeps a
// Codex agent alive across reboots (kyber#681). ChatGPT refresh tokens are
// single use: the first refresh after boot burns the token the Secret holds, so
// unless the rotated document lands back here the stored credential is dead
// rather than merely stale.
//
// The body is the whole opaque document, not parsed fields — Codex owns that
// format. We validate that it is JSON and within the same ceiling the create
// path enforces, then store it verbatim.
//
// Returns 204 on success, 400 on bad body, 503 when not configured,
// 500 on k8s errors.
func (s *InternalServer) handleCodexAuthRotation(w http.ResponseWriter, r *http.Request, agent string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.k8sClient == nil {
		http.Error(w, "not configured", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		AuthJSON string `json:"auth_json"`
	}
	// Bound the read: the create path caps codexAuthJson at 256KiB, and the
	// JSON envelope adds a little overhead on top of that.
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 512<<10))
	if err := dec.Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.AuthJSON == "" {
		http.Error(w, "body must contain auth_json", http.StatusBadRequest)
		return
	}
	if len(body.AuthJSON) > 256*1024 || !json.Valid([]byte(body.AuthJSON)) {
		http.Error(w, "auth_json must be valid JSON under 256KiB", http.StatusBadRequest)
		return
	}

	sec := &corev1.Secret{}
	key := types.NamespacedName{Name: agent + "-codex-auth", Namespace: s.namespace}
	if err := s.k8sClient.Get(r.Context(), key, sec); err != nil {
		http.Error(w, "secret lookup failed", http.StatusInternalServerError)
		return
	}
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	sec.Data["auth.json"] = []byte(body.AuthJSON)
	if err := s.k8sClient.Update(r.Context(), sec); err != nil {
		http.Error(w, "secret update failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTokenUsagePost handles POST /internal/agents/{name}/token-usage.
// It persists the provided Snapshot under agentName in the TokenStore and
// emits per-token-type increments to the kyber_agent_tokens_total OTel counter.
//
// Returns 200 on success, 400 on malformed body, 503 when not configured.
func (s *InternalServer) handleTokenUsagePost(w http.ResponseWriter, r *http.Request, agent string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.tokenStore == nil {
		http.Error(w, "not configured", http.StatusServiceUnavailable)
		return
	}
	// Read prev before storing so we can compute per-report deltas for OTel and
	// the Redis accumulator (the snapshot carries cumulative session totals).
	prev, _ := s.tokenStore.Get(r.Context(), agent)
	var snap tokenreport.Snapshot
	if err := json.NewDecoder(r.Body).Decode(&snap); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := s.tokenStore.Put(r.Context(), agent, &snap); err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	// A blank spec.model means "let the harness choose." The first finalized
	// assistant transcript is therefore the authoritative observation of the
	// concrete model actually running. Persist it in status so it survives the
	// token store's short TTL and is visible in agent views and session briefs.
	if snap.Model != "" && s.k8sClient != nil {
		key := types.NamespacedName{Name: agent, Namespace: s.namespace}
		current := &kyberv1.Agent{}
		if err := s.k8sClient.Get(r.Context(), key, current); err == nil && current.Status.CurrentModel != snap.Model {
			patch := client.MergeFrom(current.DeepCopy())
			current.Status.CurrentModel = snap.Model
			_ = s.k8sClient.Status().Patch(r.Context(), current, patch)
		}
	}
	delta := computeTokenDelta(prev, &snap)
	if telemetry.AgentTokensTotal != nil {
		emitTokenCounterDelta(r.Context(), agent, snap.Model, delta)
	}
	if s.tokenAccumulator != nil {
		_ = s.tokenAccumulator.IncrBy(r.Context(), s.namespace, agent, snap.Model, delta)
	}
	// Record the per-type delta to the windowed token time series so
	// GET /api/v1/metrics/tokens can honor start/end (kyber#428). The
	// accumulator above keeps the all-time totals; this is the windowed
	// counterpart, mirroring the activity write at internal_metrics_snapshot.go.
	if s.metricsStore != nil && !delta.IsZero() {
		s.addTokenUsagePoints(r.Context(), agent, snap.Model, delta, time.Now().Unix())
	}
	w.WriteHeader(http.StatusOK)
}

// addTokenUsagePoints writes the non-zero components of delta to their
// per-(agent,model,type) Redis token time series at timestamp ts. Zero
// components are skipped to avoid storing empty points. Best-effort: write
// errors are swallowed (same contract as the activity snapshot write).
func (s *InternalServer) addTokenUsagePoints(ctx context.Context, agent, model string, delta tokenstore.TokenDelta, ts int64) {
	add := func(tokenType string, val int64) {
		if val <= 0 {
			return
		}
		key := metricsstore.TokenUsageKey(s.namespace, agent, model, tokenType)
		_ = s.metricsStore.AddPoint(ctx, key, ts, float64(val))
	}
	add("input", delta.Input)
	add("cache_creation", delta.CacheCreation)
	add("cache_read", delta.CacheRead)
	add("output", delta.Output)
}

// computeTokenDelta returns the per-token-type increment from prev to snap.
//
// All four token types are quasi-cumulative counters and flow through the
// same safeDelta logic: input/cache_creation/cache_read carry the latest
// message's context-window numbers, and output is the reporter-accumulated
// total (since reporter start for Claude Code, since rollout-session start
// for Codex — see tokenreport.Tokens.Output). prev may be nil (first report,
// or the token store's 5-minute-TTL latest-snapshot entry expired) or carry
// a different model (session/model change); in both cases snap's values are
// the full increment — a pre-existing limitation shared identically by all
// four types (a prev-miss re-adds the current counter value once). A
// negative diff (session restart, counter rolled back) uses snap's value as
// the increment, undercounting only the gap. safeDelta never returns a
// negative value, so a malformed negative count in a reporter POST is
// clamped to 0 rather than decrementing the accumulators.
func computeTokenDelta(prev, snap *tokenreport.Snapshot) tokenstore.TokenDelta {
	var prevInput, prevCache, prevCacheRead, prevOutput int64
	if prev != nil && prev.Model == snap.Model {
		prevInput = prev.Tokens.Input
		prevCache = prev.Tokens.CacheCreation
		prevCacheRead = prev.Tokens.CacheRead
		prevOutput = prev.Tokens.Output
	}
	safeDelta := func(newVal, oldVal int64) int64 {
		d := newVal - oldVal
		if d > 0 {
			return d
		}
		if d < 0 && newVal > 0 {
			return newVal // session reset
		}
		return 0
	}
	return tokenstore.TokenDelta{
		Input:         safeDelta(snap.Tokens.Input, prevInput),
		CacheCreation: safeDelta(snap.Tokens.CacheCreation, prevCache),
		CacheRead:     safeDelta(snap.Tokens.CacheRead, prevCacheRead),
		Output:        safeDelta(snap.Tokens.Output, prevOutput),
	}
}

// emitTokenCounterDelta adds per-token-type increments to AgentTokensTotal.
func emitTokenCounterDelta(ctx context.Context, agentName, model string, delta tokenstore.TokenDelta) {
	add := func(tokenType string, val int64) {
		if val <= 0 {
			return
		}
		telemetry.AgentTokensTotal.Add(ctx, val,
			metric.WithAttributes(
				attribute.String("agent", agentName),
				attribute.String("model", model),
				attribute.String("token_type", tokenType),
			))
	}
	add("input", delta.Input)
	add("cache_creation", delta.CacheCreation)
	add("cache_read", delta.CacheRead)
	add("output", delta.Output)
}

// handleJobEvent handles POST /internal/agents/{name}/job-events (#135).
// The per-pod kyber-job-dispatch helper POSTs one of these after every job
// fire (success, failed, or skipped). The handler appends an AgentJobRun to
// Agent.status.jobs[] and enforces the per-job ring-buffer cap of
// MaxJobRunsPerJob entries.
//
// Returns 204 on success, 400 on malformed body, 404 if the Agent doesn't
// exist, 503 when not configured, 500 on k8s errors.
func (s *InternalServer) handleJobEvent(w http.ResponseWriter, r *http.Request, agentName string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.k8sClient == nil {
		http.Error(w, "not configured", http.StatusServiceUnavailable)
		return
	}

	var body struct {
		JobName    string `json:"jobName"`
		StartedAt  string `json:"startedAt"`
		FinishedAt string `json:"finishedAt"`
		Outcome    string `json:"outcome"`
		Error      string `json:"error"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.JobName == "" {
		http.Error(w, "jobName is required", http.StatusBadRequest)
		return
	}
	// Inbound dispatches (job name "inbound-<requestID>") are recorded in
	// status.inboundRuns[] by the inbound receiver (recordInboundRun →
	// appendInboundRunCapped, capped per binding). The kyber-job-dispatch
	// helper POSTs a job-event for every fire including these, but recording
	// them in status.jobs[] too is both redundant AND unbounded: each has a
	// unique requestID, so the per-name MaxJobRunsPerJob cap never triggers
	// and the array grows without limit. That is exactly what pushed Lando's
	// Agent CR past etcd's 2MB write limit and wedged the controller
	// (kyber#622). Acknowledge the POST (the dispatcher is fire-and-forget
	// and doesn't act on the status) but do not append.
	if strings.HasPrefix(body.JobName, "inbound-") {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch kyberv1.AgentJobOutcome(body.Outcome) {
	case kyberv1.AgentJobOutcomeSuccess,
		kyberv1.AgentJobOutcomeFailed,
		kyberv1.AgentJobOutcomeSkipped:
	default:
		http.Error(w, "outcome must be one of success|failed|skipped", http.StatusBadRequest)
		return
	}

	startedAt, err := parseJobEventTime(body.StartedAt)
	if err != nil {
		http.Error(w, "startedAt: "+err.Error(), http.StatusBadRequest)
		return
	}
	var finishedAt *metav1.Time
	if body.FinishedAt != "" {
		fa, err := parseJobEventTime(body.FinishedAt)
		if err != nil {
			http.Error(w, "finishedAt: "+err.Error(), http.StatusBadRequest)
			return
		}
		finishedAt = fa
	}

	run := kyberv1.AgentJobRun{
		JobName:    body.JobName,
		StartedAt:  startedAt,
		FinishedAt: finishedAt,
		Outcome:    kyberv1.AgentJobOutcome(body.Outcome),
		Error:      body.Error,
	}

	key := types.NamespacedName{Name: agentName, Namespace: s.namespace}

	// Serialize the read-modify-write per agent. A JSON merge patch against
	// the status subresource replaces the jobs list atomically; without the
	// lock two parallel POSTs for the same agent would both read the same
	// baseline, each append locally, and the second PATCH would drop the
	// first's append on the floor. Chose an in-process mutex over
	// MergeFromWithOptimisticLock + RetryOnConflict because the fake client
	// used in tests doesn't emit 409s on parallel patches, which would mask
	// regressions in CI.
	mu := s.jobEventMutex(agentName)
	mu.Lock()
	defer mu.Unlock()

	agent := &kyberv1.Agent{}
	if err := s.k8sClient.Get(r.Context(), key, agent); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, "agent lookup failed", http.StatusNotFound)
			return
		}
		http.Error(w, "agent lookup failed", http.StatusInternalServerError)
		return
	}
	patch := client.MergeFrom(agent.DeepCopy())
	agent.Status.Jobs = appendJobRunCapped(agent.Status.Jobs, run, MaxJobRunsPerJob)
	if err := s.k8sClient.Status().Patch(r.Context(), agent, patch); err != nil {
		http.Error(w, "status patch failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRuntimeVersion handles POST /internal/agents/{name}/runtime-version
// (kyber#175; extended for kyber#379 / PR-E).
//
// start-claude.sh POSTs once per pod boot so Agent.status.runtime reflects
// what's actually running, not just what the image was built with. Resets
// on every pod start.
//
// Body shape is **dual** during the runtime-image roll (PR-E §Sidecar
// coordination): the handler accepts both
//
//	{"version": "2.1.119", "reportedAt": "..."}                  // pre-PR-E
//	{"version":"...", "reportedAt":"...",
//	 "requestedVersion":"...", "requestedSatisfied":true|false,
//	 "modelSupported":true|false}                                // PR-E
//
// Older sidecar images that don't yet emit the PR-E fields must still
// report cleanly — otherwise the staggered roll would temporarily drop
// status updates from the un-upgraded half of the fleet. Do NOT tighten
// the schema to require the new fields until all sidecars are guaranteed
// to be on a post-PR-E image. The reconciler treats absent fields as
// "unknown" (nil) and leaves the previous status value untouched on
// absent-but-present-before, while explicit non-nil values overwrite.
//
// Returns 204 on success, 400 on malformed body, 404 if the Agent doesn't
// exist, 503 when not configured, 500 on k8s errors.
func (s *InternalServer) handleRuntimeVersion(w http.ResponseWriter, r *http.Request, agentName string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.k8sClient == nil {
		http.Error(w, "not configured", http.StatusServiceUnavailable)
		return
	}

	// Pointer fields distinguish "absent from the body" (nil) from
	// "reported as false" (non-nil pointer to false). Crucial because
	// the staggered sidecar roll means an absent field is "old sidecar,
	// don't know yet" — NOT "model unsupported." Flipping the badge on
	// the wrong signal would be exactly the silent-failure mode PR-E
	// closes.
	var body struct {
		Version            string  `json:"version"`
		ReportedAt         string  `json:"reportedAt"`
		RequestedVersion   *string `json:"requestedVersion,omitempty"`
		RequestedSatisfied *bool   `json:"requestedSatisfied,omitempty"`
		ModelSupported     *bool   `json:"modelSupported,omitempty"`
		// Raw probe outcome (newer start scripts). When ModelProbeExit is
		// present the server classifies it via pkg/modelprobe and it takes
		// precedence over the legacy ModelSupported bool — classification
		// lives in Go where it is unit-tested, not in a container-image
		// grep heuristic that failed open (canary regression 2026-08-22).
		ModelProbeExit   *int   `json:"modelProbeExit,omitempty"`
		ModelProbeOutput string `json:"modelProbeOutput,omitempty"`
		Runtime          string `json:"runtime,omitempty"`
		Usable           *bool  `json:"usable,omitempty"`
		ProbeMessage     string `json:"probeMessage,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.Version == "" {
		http.Error(w, "version is required", http.StatusBadRequest)
		return
	}
	// Cap defensively — the CRD has no max length and a hostile reporter
	// shouldn't be able to stuff arbitrarily large data into etcd via
	// this endpoint.
	if len(body.Version) > 128 {
		http.Error(w, "version too long", http.StatusBadRequest)
		return
	}
	if body.RequestedVersion != nil && len(*body.RequestedVersion) > 128 {
		http.Error(w, "requestedVersion too long", http.StatusBadRequest)
		return
	}
	if len(body.Runtime) > 64 || len(body.ProbeMessage) > 512 {
		http.Error(w, "runtime probe diagnostic too long", http.StatusBadRequest)
		return
	}

	reportedAt := time.Now().UTC()
	if body.ReportedAt != "" {
		if t, err := time.Parse(time.RFC3339, body.ReportedAt); err == nil {
			reportedAt = t
		}
	}

	key := types.NamespacedName{Name: agentName, Namespace: s.namespace}
	agent := &kyberv1.Agent{}
	if err := s.k8sClient.Get(r.Context(), key, agent); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, "agent not found", http.StatusNotFound)
			return
		}
		http.Error(w, "agent lookup failed", http.StatusInternalServerError)
		return
	}

	patch := client.MergeFrom(agent.DeepCopy())
	agent.Status.Runtime.InstalledVersion = body.Version
	mt := metav1.NewTime(reportedAt)
	agent.Status.Runtime.InstalledAt = &mt
	if body.Runtime != "" {
		agent.Status.Runtime.Runtime = body.Runtime
	}
	if body.Usable != nil {
		usable := *body.Usable
		agent.Status.Runtime.Usable = &usable
		agent.Status.Runtime.ProbeMessage = body.ProbeMessage
		condition := metav1.Condition{
			Type:               kyberv1.AgentConditionRuntimeUnusable,
			ObservedGeneration: agent.Generation,
			LastTransitionTime: metav1.Now(),
		}
		if usable {
			condition.Status = metav1.ConditionFalse
			condition.Reason = "ProbeSucceeded"
			condition.Message = "Runtime executable and version probe succeeded."
		} else {
			condition.Status = metav1.ConditionTrue
			condition.Reason = "ProbeFailed"
			condition.Message = body.ProbeMessage
		}
		meta.SetStatusCondition(&agent.Status.Conditions, condition)
	}
	// PR-E extensions — only overwrite when the field was present in the
	// request. An old sidecar that doesn't send these leaves whatever
	// value was last written by the post-PR-E sidecar in place; if the
	// agent's pod was recently re-rolled onto a PR-E sidecar, we don't
	// want the next stale-style report to wipe the populated fields.
	if body.RequestedVersion != nil {
		agent.Status.Runtime.RequestedVersion = *body.RequestedVersion
	}
	if body.RequestedSatisfied != nil {
		v := *body.RequestedSatisfied
		agent.Status.Runtime.RequestedSatisfied = &v
	}
	switch {
	case body.ModelProbeExit != nil:
		// Cap defensively, same rationale as the version fields.
		output := body.ModelProbeOutput
		if len(output) > 512 {
			output = output[:512]
		}
		switch modelprobe.Classify(*body.ModelProbeExit, output) {
		case modelprobe.OutcomeSupported:
			v := true
			agent.Status.Runtime.ModelSupported = &v
			agent.Status.Runtime.ModelProbeMessage = ""
		case modelprobe.OutcomeUnsupported:
			v := false
			agent.Status.Runtime.ModelSupported = &v
			agent.Status.Runtime.ModelProbeMessage = output
		case modelprobe.OutcomeInconclusive:
			// Not attributable to the model — but not silence either:
			// keep the diagnostic so the reconciler can raise the
			// condition as Unknown instead of removing it.
			agent.Status.Runtime.ModelSupported = nil
			agent.Status.Runtime.ModelProbeMessage = output
		}
	case body.ModelSupported != nil:
		// Legacy reporter (pre raw-probe image): boolean only.
		v := *body.ModelSupported
		agent.Status.Runtime.ModelSupported = &v
	}
	if err := s.k8sClient.Status().Patch(r.Context(), agent, patch); err != nil {
		http.Error(w, "status patch failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// jobEventMutex returns the per-agent mutex that gates status.jobs writes.
// Mutexes are allocated lazily and live for the server's lifetime — a few
// dozen bytes per agent is cheap, and reclaiming them would require an
// Agent-delete hook that isn't wired into this package.
func (s *InternalServer) jobEventMutex(agentName string) *sync.Mutex {
	s.jobEventMuMu.Lock()
	defer s.jobEventMuMu.Unlock()
	mu, ok := s.jobEventMu[agentName]
	if !ok {
		mu = &sync.Mutex{}
		s.jobEventMu[agentName] = mu
	}
	return mu
}

// parseJobEventTime accepts either RFC3339 or an int64 Unix millisecond
// timestamp. The dispatcher sends RFC3339 (`date -Iseconds`); the run-now
// endpoint may send millis for parity with the refresh-token endpoint.
func parseJobEventTime(s string) (*metav1.Time, error) {
	if s == "" {
		return nil, errors.New("empty timestamp")
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		mt := metav1.NewTime(t)
		return &mt, nil
	}
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		mt := metav1.NewTime(time.UnixMilli(ms))
		return &mt, nil
	}
	return nil, errors.New("must be RFC3339 or Unix millis")
}

// appendJobRunCapped appends run to the ring buffer and drops the oldest
// entry FOR THE SAME job name when the count for that name exceeds cap.
// Entries for other job names are never evicted by this call.
//
// Order is preserved (oldest-first). Callers that need per-job latest
// should scan from the tail.
func appendJobRunCapped(existing []kyberv1.AgentJobRun, run kyberv1.AgentJobRun, cap int) []kyberv1.AgentJobRun {
	out := append([]kyberv1.AgentJobRun(nil), existing...)
	out = append(out, run)
	// Count entries with the same name — if over cap, drop the oldest
	// matching entry. We only need to drop one because callers append one
	// at a time.
	count := 0
	for _, e := range out {
		if e.JobName == run.JobName {
			count++
		}
	}
	if count <= cap {
		return trimJobRunsTotal(out)
	}
	for i, e := range out {
		if e.JobName == run.JobName {
			return trimJobRunsTotal(append(out[:i], out[i+1:]...))
		}
	}
	return trimJobRunsTotal(out) // unreachable if count > cap, but defensive
}

// trimJobRunsTotal enforces the global MaxJobRunsTotal backstop by dropping
// the oldest entries (front of the slice — callers append to the back) until
// the total length is within the cap. This is a runaway guard on top of the
// per-name cap: it only bites if the set of distinct job names ever grows
// large enough to defeat appendJobRunCapped's per-name trim, so the CR can
// never again grow unbounded from status.jobs[] (kyber#622).
func trimJobRunsTotal(runs []kyberv1.AgentJobRun) []kyberv1.AgentJobRun {
	if len(runs) <= MaxJobRunsTotal {
		return runs
	}
	return runs[len(runs)-MaxJobRunsTotal:]
}
