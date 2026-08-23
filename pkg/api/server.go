// Package api implements the Kyber control plane HTTP API.
//
// The public API (this file) runs on :8080 and handles /api/v1/* requests from
// the PWA, CLI, and operator tooling. It is authenticated via an API key.
// Webhooks at /webhooks/* bypass the API key check.
//
// The internal API (internal.go) runs on :8082 and handles /internal/* requests
// from agent init containers and cluster-internal services. It has no authentication.
//
// These two servers are intentionally separate — different ports, different auth models,
// different audiences. Do not merge them.
package api

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/matty-v/kyber/pkg/adapters"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/configexport"
	"github.com/matty-v/kyber/pkg/contextwindowmap"
	"github.com/matty-v/kyber/pkg/githubapp"
	"github.com/matty-v/kyber/pkg/inbound"
	"github.com/matty-v/kyber/pkg/metrics"
	"github.com/matty-v/kyber/pkg/metricsstore"
	"github.com/matty-v/kyber/pkg/oauth"
	"github.com/matty-v/kyber/pkg/runtimedetect"
	"github.com/matty-v/kyber/pkg/statechangestore"
	"github.com/matty-v/kyber/pkg/tokenstore"
	"github.com/matty-v/kyber/pkg/updates"
)

// DefaultPublicPort is the default listen address for the public HTTP API.
const DefaultPublicPort = ":8080"

// Server is the public HTTP API server for the Kyber control plane.
// It is registered as a controller-runtime Runnable and starts alongside the controllers.
type Server struct {
	// K8sClient is used for all CRD reads and writes.
	K8sClient client.Client

	// ComputeSimulation is an explicitly enabled development-only scenario
	// controller. It is nil in production and for real compute providers.
	ComputeSimulation adapters.SimulationController

	// CapacityProvider supplies provider-neutral capabilities, profiles, and
	// validation to operator-facing compute APIs. Nil preserves compatibility
	// for tests and installations still using only the legacy adapter contract.
	CapacityProvider adapters.CapacityProvider

	// TokenStore persists per-agent Claude Code context-budget snapshots.
	TokenStore tokenstore.TokenStore

	// TokenAccumulator holds all-time per-agent/model token counts in Redis.
	// When non-nil, handleMetricsTokens reads the accumulator first (Tier 1)
	// before falling back to Prometheus TSDB (Tier 2).
	TokenAccumulator tokenstore.Accumulator

	// APIKey is the API key for the public API. All /api/v1/* requests must include
	// "Authorization: Bearer <APIKey>". If empty, all requests are rejected with 401.
	// Mutated by /api/v1/rotate-api-key (#143) to keep the seed value in sync with
	// the active authenticator. The authoritative key for live requests is on the
	// `auth` field below.
	APIKey string

	// auth is the live authenticator. Built lazily in buildTopHandler from APIKey.
	// The rotate-api-key handler swaps the key on this object via auth.SetKey, so
	// rotations take effect immediately without a process restart.
	auth *APIKeyAuthenticator

	// APIKeySecretName is the name of the Kubernetes Secret holding the api-key
	// the control-plane authenticates against. The rotate handler patches this
	// Secret's `api-key` field. Defaults to "" (rotation disabled — the handler
	// returns 503).
	APIKeySecretName string

	// Callers is the optional set of scoped API keys (kyber#474), parsed from the
	// `callers` JSON document on the api-key Secret. Each resolves to a Caller
	// with a bounded scope set; the legacy APIKey above remains full-scope. Empty
	// (the default) means only the legacy key is accepted — prior behavior.
	Callers []ScopedCaller

	// AuthzEnforce gates caller-level authorization on lifecycle mutations
	// (kyber#474). Default false (permissive): authz decisions are audit-logged
	// but never block, so single-key installs are unaffected. Set true
	// (KYBER_AUTHZ_ENFORCE=true) to return 403 on an authenticated-but-
	// unauthorized caller. The legacy key is full-scope, so enforcement only bites
	// once an operator issues scoped (sub-full) keys.
	AuthzEnforce bool

	// Addr is the listen address. Defaults to ":8080".
	Addr string

	// Namespace is the Kubernetes namespace for all CRD operations. Defaults to "kyber-system".
	Namespace string

	// LoggingGlobalLevel and LoggingComponentLevels are the Helm-desired
	// verbosity settings exposed read-only through /api/v1/logging/settings.
	LoggingGlobalLevel      string
	LoggingComponentLevels  map[string]string
	LoggingArchiveRetention int

	// PublicURL is the externally-reachable HTTPS URL of this Kyber instance
	// (e.g. "https://kyber.your-tailnet.ts.net"). Used to render inbound-binding
	// webhook URLs (PublicURL + /webhooks/inbound/<agent>/<binding>).
	// If empty, rendered URLs fall back to a relative path.
	PublicURL string

	// AnthropicTokenURL is the OAuth token endpoint used when exchanging
	// authorization codes and refreshing access tokens. Defaults to
	// oauth.DefaultTokenURL when empty; tests set this to the mockserver URL
	// via the ANTHROPIC_TOKEN_URL env var read in main.go.
	AnthropicTokenURL string

	// ValidRuntimes is the set of runtime identifiers that the control plane
	// has registered adapters for (e.g. {"claude-code": true}). Agent creation
	// is rejected with 400 if the requested runtime is not in this set.
	// If nil or empty, runtime validation is skipped (dev/test convenience).
	ValidRuntimes map[string]bool

	// RuntimeImages maps a registered runtime identifier to the container
	// image this install will launch its agents from (e.g.
	// {"codex": "ghcr.io/matty-v/kyber-codex:v2.6.2"}). A registered runtime
	// with an EMPTY image means the chart never pinned image.<runtime>.tag,
	// so the adapter would hand pod_builder an empty containers[0].image and
	// the agent could never start.
	//
	// Agent creation is rejected with 400 in that case. Before kyber#674 the
	// create succeeded and the agent sat with a blank status forever while the
	// controller retried an invalid pod every ~20s — the failure was visible
	// only in the control-plane log. If nil, this check is skipped
	// (dev/test convenience, matching ValidRuntimes).
	RuntimeImages map[string]string

	// Clientset is the typed Kubernetes clientset used for log streaming (CoreV1.Pods.GetLogs)
	// and exec (CoreV1.RESTClient). controller-runtime's client.Client does not expose streaming
	// log reads, so a separate clientset is required.
	// Optional — log/exec endpoints return 503 when nil.
	Clientset kubernetes.Interface

	// ArchiveReader backs the durable, absolute-window log surface
	// (GET /api/v1/agents/{name}/logs?source=archive). It reads agent stdout
	// shipped off-cluster (GCS via node ADC, or S3/MinIO via Secret creds) by the
	// log-shipper DaemonSet (kyber#431, #437). Optional — when nil, source=archive
	// returns 503; the kubelet live-tail (source=kubelet, the default) is
	// unaffected.
	ArchiveReader                 ArchiveReader
	PlatformArchiveReader         PlatformArchiveReader
	PlatformArchiveDisabledReason string

	// ArchiveDisabledReason, when ArchiveReader is nil, names the config that is
	// missing/invalid so the 503 is self-diagnosing (kyber#437). It MUST name
	// config *keys* only (e.g. "KYBER_LOG_ARCHIVE_BUCKET unset"), never their
	// values — credentials must not leak into a response body. Empty string
	// yields the generic "archive log surface not configured" message.
	ArchiveDisabledReason string

	// TranscriptReader backs the Claude Code session-transcript surface
	// (GET /api/v1/agents/{name}/logs?source=transcript, kyber#446). It is the
	// SAME ArchiveReader machinery as ArchiveReader, constructed against the same
	// bucket but rooted at the "transcripts/" object prefix instead of "agents/",
	// so the transcript lane (shipped by the transcript-tailer sidecar) and the
	// boot-stdout archive lane never intermix. Optional — when nil,
	// source=transcript returns 503; source=archive and the kubelet live-tail are
	// unaffected.
	//
	// SECURITY: this surface exposes the agent's FULL session content (every user
	// prompt, assistant turn, and tool result — which may contain secrets), so it
	// is gated behind the exact same protected mux + agent-existence check as
	// source=archive: branching happens after the auth boundary, so authz is
	// identical and no broader.
	TranscriptReader ArchiveReader

	// TranscriptDisabledReason mirrors ArchiveDisabledReason for the transcript
	// surface: when TranscriptReader is nil, it names the missing config key
	// (never a value) so the 503 is self-diagnosing.
	TranscriptDisabledReason string

	// MaxConcurrentReads bounds simultaneous in-flight windowed log reads
	// (source=archive|transcript) through serveWindowedLines (kyber#463). The
	// single-read streaming caps (kyber#456) bound one read; this bounds the
	// AGGREGATE so N concurrent large reads can't collectively exhaust the
	// control-plane (memory) or starve its CPU (which would time out the liveness
	// probe and SIGKILL the whole process — taking /version + the agents list
	// down with it). Over-cap reads get an immediate 429 + Retry-After, never a
	// partial/crashed stream. 0 or negative → defaultMaxConcurrentReads. Set from
	// KYBER_MAX_CONCURRENT_READS. The gate wraps ONLY the read handlers, so other
	// endpoints and the :8081 probe listener are never throttled.
	MaxConcurrentReads int
	MaxExportBytes     int64

	// readSlots is the counting semaphore enforcing MaxConcurrentReads, lazily
	// initialized (readSlotsOnce) so a struct-literal Server (tests + main) works
	// without a constructor.
	readSlots       chan struct{}
	readSlotsOnce   sync.Once
	exportSlots     chan struct{}
	exportSlotsOnce sync.Once

	// RestConfig is the rest.Config used to build SPDY executors for exec proxy.
	// Optional — exec endpoints return 503 when nil.
	RestConfig *rest.Config

	// EventBus is the shared in-process event bus for the WebSocket events endpoint.
	// Populated by startEventInformers during Server.Start.
	// Optional — events endpoint returns 503 when nil.
	EventBus *EventBus

	// InformerCache is the controller-runtime cache used to register informers for the
	// events endpoint. When set, Start() calls startEventInformers automatically.
	InformerCache cache.Cache

	// Recorder emits Kubernetes Events for API-initiated bulk actions that
	// operators want an audit trail for (e.g. machine-level restart-agents).
	// Optional — handlers that use it must nil-check and skip event emission
	// when unset. Wired from mgr.GetEventRecorderFor("kyber-api") in main;
	// tests leave it nil or pass a record.FakeRecorder.
	Recorder record.EventRecorder

	// RestartSessionCommands maps runtime identifier → argv for the in-pod
	// session-restart command (#128). nil/absent = 501 Not Implemented for
	// that runtime. Kept here (rather than reaching into the controllers'
	// adapter registry) so pkg/api stays free of the runtime-adapter
	// interface dependency. Wired from main.go by iterating runtimes.All()
	// and pulling RestartSessionCommand() off each adapter (kyber#250).
	RestartSessionCommands map[string][]string

	// CompactSessionCommands maps runtime identifier → argv for the in-pod
	// session-compaction command. nil/absent = 501 Not Implemented for that
	// runtime. Populated the same way as RestartSessionCommands (main.go
	// iterating runtimes.All()), and kept a separate map rather than a
	// richer per-runtime struct so adding the next in-pod action stays a
	// one-line change on both sides.
	CompactSessionCommands map[string][]string

	// ComputeProvider identifies which ComputeAdapter backs this control plane.
	// Populated from the KYBER_COMPUTE_PROVIDER env var at startup. Exposed
	// to the PWA via GET /api/v1/config so the UI can render provider-
	// conditional forms (e.g., Create Machine shows different fields on
	// mock vs gce). Empty string when unset.
	ComputeProvider string

	// BuildSHA is the short Git SHA of the control-plane build, injected via
	// -ldflags "-X main.BuildSHA=…" in images/control-plane/Dockerfile. Empty
	// on local dev builds (make build / go run). Surfaced through
	// GET /api/v1/version for the PWA Diagnostics card.
	BuildSHA string

	// BuildDate is the RFC3339 build timestamp, injected the same way as
	// BuildSHA. Empty on local dev builds.
	BuildDate string

	// ChartVersion is the user-facing release version. Build-injected into the
	// control-plane image via -ldflags "-X main.Version=…" (mirroring BuildSHA),
	// so it rides with the code and converges atomically on image rollout
	// (kyber#482). Falls back to the chart-rendered /etc/kyber/chart-version
	// file on dev/test builds where no ldflag was passed; empty when neither is
	// present. Resolved by cmd/control-plane resolveDisplayVersion at boot.
	// (Field name unchanged for contract stability.)
	ChartVersion string

	// ClusterName is the user-facing logical name (e.g. "kyber-gcp",
	// "kyber-dev"). Surfaced via /api/v1/cluster-info. Sourced from the
	// KYBER_CLUSTER_NAME env var, threaded through cmd/control-plane/main.go.
	// Empty string is acceptable — the PWA header renders blank in that case.
	ClusterName string

	// AllowedOrigins is the CORS allowlist for cross-origin browser clients
	// (holocron in Phase C). Sourced from the KYBER_CORS_ALLOWED_ORIGINS env
	// var (comma-separated), threaded through cmd/control-plane/main.go. Empty
	// means CORS is disabled — same-origin clients only.
	AllowedOrigins []string

	// Substrate identifies the Kubernetes namespace the control-plane is
	// managing (e.g. "kyber-prod", "kyber-preview-pr-12"). Sourced
	// from KYBER_NAMESPACE at boot in main.go. Exposed via /api/v1/version
	// so the PWA Diagnostics card can tell operators which instance a tab
	// is pointed at.
	Substrate string

	// GCEVMTypeCatalog is the set of allowed GCE machine types and their
	// declared capacities. Populated from KYBER_GCE_VM_TYPE_CATALOG (a JSON
	// array rendered by the Helm chart from compute.gce.vmTypeCatalog).
	// When nil, createMachine falls back to DefaultGCEVMTypeCatalog().
	GCEVMTypeCatalog map[string]kyberv1.MachineCapacity

	// InboundDeduper guards against accidental duplicate POSTs to the
	// /webhooks/inbound/* endpoints. Backed by Redis in production
	// (RedisDeduper) and by an in-memory map in dev/test
	// (NewMemoryDeduper). When nil, dedup is skipped — the receiver
	// fails open so a missing dependency doesn't black-hole traffic.
	InboundDeduper inbound.Deduper

	// InboundRateLimiter enforces per-(agent,binding) rate limits on
	// /webhooks/inbound/* requests. When nil, rate limiting is skipped
	// (dev/test convenience).
	InboundRateLimiter *inbound.RateLimiter

	// InboundQueue holds in-flight inbound dispatches, one bounded
	// channel per agent. The queue's handler closure (wired in main.go
	// after the Server is constructed) execs into the agent pod via
	// kyber-job-dispatch --stdin. When nil, /webhooks/inbound/* returns
	// 503 — the receiver refuses to accept work it cannot dispatch.
	InboundQueue *inbound.Queue

	// InboundEventAggregator batches high-volume Events on the inbound
	// path (rate-limited, queue-full) so a flood produces 1 Event per
	// minute, not N. Optional — when nil, aggregated Events are skipped
	// silently and per-occurrence Events still fire via Recorder.
	InboundEventAggregator *InboundEventAggregator

	// InboundEnvelopeCache stores rendered envelopes for inbound-prompt
	// replay (kyber#208 Phase 3). Backed by Redis in production with a
	// 7-day TTL; in-memory fallback in dev/test. Optional — when nil, the
	// receiver simply skips the cache write and the replay endpoint
	// returns 503 because there's nothing to replay against.
	InboundEnvelopeCache inbound.EnvelopeCache

	// GithubAppClient drives the /api/v1/github/repos and .../exists
	// endpoints that power the Create Agent identity-repo dropdown +
	// collision check (#134). When nil (App not configured), both
	// endpoints return 503.
	GithubAppClient *githubapp.Client

	// IdentityRepoOwner is the GitHub user/org under which auto-created
	// identity repos are placed (e.g. "matty-v"). Surfaced to the PWA via
	// GET /api/v1/config so the wizard knows what owner to prefix.
	IdentityRepoOwner string

	// githubReposCache caches the /installation/repositories result for a
	// short TTL — the dropdown re-opens repeatedly during a wizard session
	// and the upstream call is paginated.
	githubReposCacheMu sync.Mutex
	githubReposCache   []githubapp.Repository
	githubReposCacheAt time.Time

	// MetricsConfig holds settings for the /api/v1/metrics/* endpoints.
	// Zero value is safe — time-series panels return empty results when
	// PrometheusURL is unset; CRD-backed panels (summary, last-active) always work.
	MetricsConfig metrics.Config

	// MetricsStore holds the Redis-backed (or memory) time-series store for
	// agent activity metrics. When non-nil, Activity and WorkingTime panel
	// handlers read from this store first (Tier 1) before falling back to
	// Prometheus TSDB (Tier 2).
	MetricsStore metricsstore.MetricsStore

	// NodeStore holds the Redis-backed (or memory) store for latest node
	// resource samples. When non-nil, the Nodes panel handler reads from
	// this store first (Tier 1) before falling back to Prometheus TSDB.
	NodeStore metricsstore.NodeStore

	// StateChangeAccumulator holds accumulated per-agent state-transition
	// counts. When non-nil, the State Changes panel handler reads from
	// this accumulator first (Tier 1) before falling back to Prometheus.
	StateChangeAccumulator statechangestore.Accumulator

	// ConfigExporter renders the values file that recreates this cluster, with
	// secrets removed — the infra-as-code artifact for an install that has no
	// deploy repo. Nil makes GET /api/v1/config/export return 503.
	//
	// Read-only: it lists Helm's own release Secrets (narrowed by the
	// owner=helm label so unrelated credentials never enter the process) and
	// decodes the stored values. The redaction rules live in
	// pkg/configexport, covered by a test that fails when a new
	// credential-looking chart value appears unclassified.
	ConfigExporter *configexport.Reader

	// FleetDefaultsConfigMapName is the name of the ConfigMap that holds
	// the cluster-wide fleet-default values consumed by the agent
	// reconciler (defaultModel today; defaultRuntimeVersion plumbed for
	// PR-C). When empty, the PWA settings panel's Fleet Defaults card
	// renders as "not configured" and writes return 503 — the chart sets
	// `controlPlane.fleetDefaults.configMapName` so this is unset only in
	// dev/test installs. See kyber#376 / PR-B of #374.
	FleetDefaultsConfigMapName string

	// UpdateChecker answers GET /api/v1/updates: what this cluster runs, what
	// release is available, and whether Kyber may install it here. Nil (the
	// default in tests and on installs with update checking disabled) makes
	// every /api/v1/updates route return 503.
	//
	// It only ever READS — from the release feed and from the cluster.
	// Installing an update is the UpdateApplier's job.
	UpdateChecker *updates.Checker

	// UpdateApplier starts a self-upgrade: POST /api/v1/updates/apply creates
	// a Job that pulls the target chart, applies its CRDs, runs the Helm
	// upgrade and rolls back if the result does not verify. Nil (the default
	// in tests, and on installs where selfUpgrade is not enabled in the chart)
	// makes the apply route return 503 and the status report
	// applySupported:false.
	UpdateApplier *updates.Applier

	// UpdateStore persists the operator's update policy. Separate from the
	// checker because PUT writes through it directly, and because the policy
	// deliberately lives in a control-plane-owned ConfigMap the Helm chart
	// does not template — the settings governing upgrades must not be
	// re-rendered by an upgrade.
	UpdateStore *updates.Store

	// FleetDefaultsInvalidator clears the reconciler-side cache after a
	// successful PUT /api/v1/fleet-defaults so the operator sees their
	// own edit take effect on the next reconcile rather than waiting up
	// to fleetdefaults.ResolveCacheTTL. Optional — nil means writes
	// still persist; the cache just expires on its own.
	FleetDefaultsInvalidator interface{ Invalidate() }

	// ContextWindows resolves a model's context-window size from the
	// operator-editable kyber-model-context-windows ConfigMap — the single
	// source of truth shared with the detection poller (#396). The token-usage
	// GET handler resolves the limit server-side at serve-time through it, so an
	// operator's ConfigMap correction changes the budget % for already-running
	// agents on their next read with no pod restart. Optional — nil falls back
	// to the 200K floor + contextWindowKnown=false (LookupNormalized is nil-safe).
	ContextWindows *contextwindowmap.Resolver

	// RuntimeDetectCache is the shared cache that the runtimedetect.Poller
	// goroutine writes to and that GET /api/v1/available reads from. nil
	// disables /available — the handler returns the empty contract so the
	// PWA renders "detection unavailable" rather than 5xx. (kyber#375)
	RuntimeDetectCache runtimedetect.Cache

	// Snapshots resolves a model's auto-detected context window from the
	// detection snapshot (the same RuntimeDetectCache /available reads),
	// reusing the hot-path-safe runtimedetect.SnapshotResolver (30s TTL + 2s
	// bounded read) built in kyber#493. The serve-time token-budget gauge
	// (kyber#500) resolves the window through it — between the operator
	// override ConfigMap and the in-Go tokenreport floor — so the gauge agrees
	// with /available and the pod adapter instead of flooring auto-detected
	// models. Optional/nil-safe — nil falls through to the ConfigMap→floor
	// path (today's behavior). Assigned in cmd/control-plane/main.go from
	// RuntimeDetectCache.
	Snapshots SnapshotWindowResolver

	// AnthropicKeySecretName is the Kubernetes Secret in this server's
	// Namespace whose `api-key` field holds the operator-supplied
	// Anthropic API key used by the detection poller. Patched by
	// PUT /api/v1/settings/anthropic-key. Empty disables the write
	// endpoint (returns 503). (kyber#375)
	AnthropicKeySecretName string

	server *http.Server
}

// defaultMaxConcurrentReads is the in-flight cap for windowed log reads when
// MaxConcurrentReads is unset/non-positive (kyber#463). Conservative and safe
// regardless of control-plane size: Boba Fett measured 3 concurrent large reads
// OK and 6 crashing the CP in production (256Mi req / 500m cpu), so 2 leaves margin.
// Operators with more CPU headroom can raise it via KYBER_MAX_CONCURRENT_READS.
const defaultMaxConcurrentReads = 2

// initReadSlots lazily builds the read semaphore exactly once, sizing it to a
// floored MaxConcurrentReads (a 0/negative config can never disable the gate).
func (s *Server) initReadSlots() {
	s.readSlotsOnce.Do(func() {
		k := s.MaxConcurrentReads
		if k < 1 {
			k = defaultMaxConcurrentReads
		}
		s.readSlots = make(chan struct{}, k)
	})
}

// tryAcquireReadSlot non-blocking-acquires one windowed-read slot. On success it
// returns a release func (idempotent via defer) and true; at capacity it returns
// nil, false so the caller can return 429 backpressure WITHOUT having written any
// body. The slot must be released on every return path (including a client
// disconnect mid-stream), so callers `defer release()` immediately after acquire.
func (s *Server) tryAcquireReadSlot() (release func(), ok bool) {
	s.initReadSlots()
	select {
	case s.readSlots <- struct{}{}:
		return func() { <-s.readSlots }, true
	default:
		return nil, false
	}
}

func (s *Server) tryAcquireExportSlot() (release func(), ok bool) {
	s.exportSlotsOnce.Do(func() { s.exportSlots = make(chan struct{}, 1) })
	select {
	case s.exportSlots <- struct{}{}:
		return func() { <-s.exportSlots }, true
	default:
		return nil, false
	}
}

// Start begins listening. It blocks until the context is cancelled or the server errors.
// Intended to be wrapped in manager.RunnableFunc and added to the controller-runtime manager.
func (s *Server) Start(ctx context.Context) error {
	s.applyDefaults()

	// Initialise the event bus and register informers if a cache is provided.
	if s.EventBus == nil {
		s.EventBus = NewEventBus()
	}
	if s.InformerCache != nil {
		if err := s.startEventInformers(ctx, s.InformerCache); err != nil {
			// Non-fatal: events endpoint will return 503, but logs/exec still work.
			_ = err
		}
	}

	top := s.buildTopHandler()

	s.server = &http.Server{
		Addr:         s.Addr,
		Handler:      top,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	// Graceful shutdown when the context is cancelled.
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.server.Shutdown(shutdownCtx)
	}()

	if err := s.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// applyDefaults fills in zero-value fields with their defaults.
func (s *Server) applyDefaults() {
	if s.Addr == "" {
		s.Addr = DefaultPublicPort
	}
	if s.Namespace == "" {
		s.Namespace = "kyber-system"
	}
}

// anthropicTokenURL returns the configured token endpoint, falling back to
// the real Anthropic endpoint when empty.
func (s *Server) anthropicTokenURL() string {
	if s.AnthropicTokenURL != "" {
		return s.AnthropicTokenURL
	}
	return oauth.DefaultTokenURL
}

// buildTopHandler assembles the full request handler tree:
//
//   - /healthz and /readyz are public — no API key required (kubelet probes hit these).
//   - /webhooks/* bypass API key auth and use their own secret-header validation.
//   - All other routes require a valid Bearer API key.
//
// Middleware chain (outermost first): recover → request-id → logging → (auth for protected routes).
// Auth is applied only to the protected sub-mux, not to public or webhook routes.
//
// NOTE: /api/v1/backups routes (spec lines 65-67) are deferred to a later task.
// They were not included in the C1 plan.
func (s *Server) buildTopHandler() http.Handler {
	// Propagate the allowlist to the WebSocket upgrader. Same list governs
	// both REST/SSE (via corsMiddleware) and WS (via the upgrader's
	// CheckOrigin). Called inside buildTopHandler so the test path
	// (BuildHandler -> buildTopHandler) also wires it.
	setWSAllowedOrigins(s.AllowedOrigins)

	// Protected routes: API key auth applied inside this sub-mux.
	protectedMux := http.NewServeMux()
	// Initialize the runtime-mutable authenticator on the Server. Stash it
	// so the rotate-api-key handler (#143) can swap the accepted key in-place
	// after updating the Secret, without a pod restart.
	if s.auth == nil {
		s.auth = NewAPIKeyAuthenticator(s.APIKey, s.Callers...)
	}
	s.registerProtectedRoutes(protectedMux)
	protected := authMiddleware(s.auth, protectedMux)

	// Webhook routes: bypass API key auth, use their own secret-header validation.
	webhookMux := http.NewServeMux()
	s.registerWebhookRoutes(webhookMux)

	// PWA static file server — served at "/" without auth.
	// The embedded pwa_dist/ directory is populated by `make pwa-build`.
	// If only the placeholder index.html is present (dev / CI go-only builds) it
	// still serves something rather than 404-ing. API routes take priority because
	// they are registered on the protected sub-mux which is matched first via the
	// /api/ prefix in the top-level mux below.
	pwaFS, err := fs.Sub(pwaAssets, "pwa_dist")
	if err != nil {
		// Should never happen — the embed directive always includes pwa_dist/.
		panic("embed: failed to sub pwa_dist: " + err.Error())
	}
	pwaServer := http.FileServer(http.FS(pwaFS))

	// SPA handler: serve static assets from the embedded FS; fall back to
	// index.html for any path not present so React Router can handle it
	// client-side.
	//
	// Note: io/fs.FS.Open requires paths WITHOUT a leading slash ("path must
	// not start with /") — stripping it is required. Earlier versions of this
	// function passed r.URL.Path directly, which made every Open call fail
	// and caused every asset request (including /assets/*.js and .css) to
	// fall through to index.html. The browser then tried to execute HTML as
	// JavaScript and the React app never mounted — the symptom was a blank
	// white page. See docs/installation.md § Troubleshooting.
	// buildArtifactExts are the extensions the PWA build emits under
	// content-hashed names. A request for one of these that does not exist is
	// a stale client, never a client-side route — React Router paths are
	// extensionless.
	buildArtifactExts := []string{".js", ".mjs", ".css", ".map", ".wasm"}
	isBuildArtifactPath := func(p string) bool {
		for _, ext := range buildArtifactExts {
			if strings.HasSuffix(p, ext) {
				return true
			}
		}
		return false
	}

	// serveIndex writes index.html with caching disabled.
	//
	// index.html is the version pointer for the whole app: it names the
	// hashed bundle to load. A cached copy — in the browser, in a CDN, in a
	// corporate proxy — pins that client to whatever build was current when
	// it was stored, and no amount of reloading dislodges it because the
	// reload is answered from the cache. The bundles it points at are
	// content-hashed and safe to cache forever; this one file must not be.
	serveIndex := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, must-revalidate")
		http.ServeFileFS(w, r, pwaFS, "index.html")
	}

	spaHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqPath := strings.TrimPrefix(r.URL.Path, "/")
		if reqPath == "" {
			reqPath = "index.html"
		}
		// The service worker and its registration shim are version pointers
		// too — a stale sw.js is a worker that can never be replaced.
		if reqPath == "index.html" || reqPath == "sw.js" || reqPath == "registerSW.js" {
			w.Header().Set("Cache-Control", "no-store, must-revalidate")
		}
		f, ferr := pwaFS.Open(reqPath)
		if ferr != nil {
			// A miss on a build artifact means the CLIENT is stale — it is
			// running an index.html from a previous build and asking for a
			// file that build produced. Answer 404.
			//
			// Falling back to index.html here is actively harmful: the
			// browser receives HTML with a 200 and a JavaScript content
			// type, which either throws a syntax error or — worse, and what
			// happened on the dev instance — lets a CDN cache the response
			// so a stale client keeps working long after the deploy. A 404
			// is what the service worker and the browser both know how to
			// recover from.
			//
			// Keyed on the extension rather than on the assets/ directory:
			// vite-plugin-pwa emits hashed JS at the dist ROOT too
			// (workbox-<hash>.js, since inlineWorkboxRuntime defaults to
			// false), and a stale service worker's
			// importScripts('/workbox-OLDHASH.js') hits exactly the failure
			// above. Client-side routes never carry these extensions.
			if isBuildArtifactPath(reqPath) {
				http.NotFound(w, r)
				return
			}
			// Anything else — a deep link like /agents/bob — is a real
			// client-side route. Serve index.html so React Router handles it.
			serveIndex(w, r)
			return
		}
		_ = f.Close()
		pwaServer.ServeHTTP(w, r)
	})

	// Top-level mux: dispatch to public, webhook, API, or PWA.
	// /healthz and /readyz are registered directly here — no auth at this level.
	top := http.NewServeMux()
	top.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	top.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// Public — cluster-info exposes only non-sensitive metadata (display name,
	// chart version, capability list). Mounting outside the auth wall avoids a
	// first-install deadlock: a fresh deploy with no API key set must still
	// resolve cluster-info so EmbeddedClusterProvider can land the user on the
	// Settings page to enter a key. Auth-required endpoints stay inside
	// registerProtectedRoutes().
	top.HandleFunc("/api/v1/cluster-info", s.handleClusterInfo)
	// Browser-only API-key exchange. The handler authenticates the bearer key
	// itself, then stores only an opaque session token in an HttpOnly cookie.
	top.HandleFunc("/api/v1/browser-session", s.handleBrowserSession)
	top.Handle("/webhooks/", webhookMux)
	// Protected API routes — auth required.
	top.Handle("/api/", protected)
	// PWA static assets and SPA fallback — no auth required.
	top.Handle("/", spaHandler)

	// Apply recover+requestID+logging once at the root so every request — public,
	// webhook, and protected alike — gets a request-ID header and is logged.
	// CORS wraps outside the inner chain so headers are set even on panic-recovered
	// 500s, and so BuildHandler() (test path) gets the same middleware as Start().
	return corsMiddleware(s.AllowedOrigins)(
		recoverMiddleware(requestIDMiddleware(loggingMiddleware(top))),
	)
}

// registerProtectedRoutes registers all /api/v1/* routes on mux.
// These routes require API key authentication (applied by the caller).
func (s *Server) registerProtectedRoutes(mux *http.ServeMux) {
	if s.ComputeSimulation != nil {
		mux.HandleFunc("/api/v1/dev/compute/instances", s.handleComputeSimulation)
		mux.HandleFunc("/api/v1/dev/compute/scenarios", s.handleComputeSimulation)
	}
	// Machines.
	mux.HandleFunc("/api/v1/machines", s.handleMachines)
	mux.HandleFunc("/api/v1/machines/", s.handleMachines)
	mux.HandleFunc("/api/v1/machine-candidates", s.handleMachineCandidates)
	mux.HandleFunc("/api/v1/machines/preflight", s.handleMachinePreflight)

	// Agents.
	mux.HandleFunc("/api/v1/agents", s.handleAgents)
	mux.HandleFunc("/api/v1/agents/", s.handleAgents)

	// Fleet.
	mux.HandleFunc("/api/v1/fleet", s.handleFleet)
	mux.HandleFunc("/api/v1/fleet/", s.handleFleet)

	// Events — WebSocket event stream (deferred to C2).
	mux.HandleFunc("/api/v1/events", s.handleEvents)

	// Public config.
	mux.HandleFunc("/api/v1/config", s.handleConfig)
	mux.HandleFunc("/api/v1/logging/settings", s.handleLoggingSettings)
	mux.HandleFunc("/api/v1/logging/targets", s.handleLoggingTargets)
	mux.HandleFunc("/api/v1/logging/logs", s.handleLoggingLogs)
	mux.HandleFunc("/api/v1/logging/export", s.handleLoggingExport)

	// Fleet defaults — GET/PUT the kyber-fleet-defaults ConfigMap so the
	// PWA Settings panel can read + edit defaultModel and
	// defaultRuntimeVersion without a chart redeploy (kyber#376 / PR-B).
	mux.HandleFunc("/api/v1/fleet-defaults", s.handleFleetDefaults)

	// Build + chart version + substrate (Diagnostics card in the PWA Settings page).
	mux.HandleFunc("/api/v1/version", s.handleVersion)

	// Update checking (read-only in this build — no apply path).
	mux.HandleFunc("/api/v1/updates", s.handleUpdates)
	mux.HandleFunc("/api/v1/updates/", s.handleUpdates)

	// Export the values file that recreates this cluster (secrets removed).
	mux.HandleFunc("/api/v1/config/export", s.handleConfigExport)

	// Inbound debug (kyber#208 Phase 3) — operator-facing dry-run of the
	// dispatcher pipeline against a synthetic payload + binding spec. No
	// HMAC, no dedup, no queue — just the trace.
	mux.HandleFunc("/api/v1/inbound-debug", s.handleInboundDebug)

	// Rotate API key — generates a fresh key, persists it to the Secret,
	// and swaps it on the live authenticator so the new key works immediately
	// and the old key returns 401. See #143.
	mux.HandleFunc("/api/v1/rotate-api-key", s.handleRotateAPIKey)

	// GitHub repo list + collision check (#134).
	mux.HandleFunc("/api/v1/github/repos", s.handleGitHubRepos)
	mux.HandleFunc("/api/v1/github/repos/", s.handleGitHubReposPath)

	// Metrics tab — cluster observability dashboard (#328).
	mux.HandleFunc("/api/v1/metrics/", s.handleMetrics)

	// Runtime detection (kyber#375 / PR-A of #374). /available surfaces
	// Claude Code versions + Claude models the detection poller has seen.
	// /settings/anthropic-key is the operator-facing write endpoint that
	// stores the API key in the kyber-anthropic-key Secret.
	mux.HandleFunc("/api/v1/available", s.handleAvailable)
	mux.HandleFunc("/api/v1/settings/anthropic-key", s.handleAnthropicKeySetting)
}

// registerWebhookRoutes registers all /webhooks/* routes on mux.
// These routes bypass API key auth — they use their own secret header validation.
func (s *Server) registerWebhookRoutes(mux *http.ServeMux) {
	// Inbound prompts (kyber#208 Phase 1). HMAC auth happens inside the
	// handler against the per-binding Secret; the top-level webhook mux
	// only ensures the API-key middleware is bypassed.
	mux.HandleFunc("/webhooks/inbound/", s.handleInbound)
}

// BuildHandler assembles and returns the full HTTP handler without starting a listener.
// This is used by tests that create an httptest.Server directly.
func (s *Server) BuildHandler() http.Handler {
	s.applyDefaults()
	return s.buildTopHandler()
}
