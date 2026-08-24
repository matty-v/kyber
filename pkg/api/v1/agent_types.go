package v1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentPhase represents the current lifecycle phase of an Agent.
type AgentPhase string

const (
	// AgentPhaseCreating means the agent is being provisioned (PV, identity repo, pod).
	AgentPhaseCreating AgentPhase = "Creating"
	// AgentPhaseStarting means the agent pod exists but is not yet Ready.
	AgentPhaseStarting AgentPhase = "Starting"
	// AgentPhaseRunning means the agent pod is Running and its readiness probe passes.
	AgentPhaseRunning AgentPhase = "Running"
	// AgentPhaseStopping means the agent is being gracefully shut down.
	AgentPhaseStopping AgentPhase = "Stopping"
	// AgentPhaseStopped means the agent pod is not running and the PV is preserved.
	AgentPhaseStopped AgentPhase = "Stopped"
	// AgentPhaseRestarting means the agent pod is being replaced (state captured, new pod starting).
	AgentPhaseRestarting AgentPhase = "Restarting"
	// AgentPhaseFailed means the agent has exceeded its restart retries or encountered an unrecoverable error.
	AgentPhaseFailed AgentPhase = "Failed"
	// AgentPhaseDeleted means the agent has been fully deleted including cleanup of PV and identity.
	AgentPhaseDeleted AgentPhase = "Deleted"
	// AgentPhaseDraining means the agent is being gracefully drained before its machine is preempted.
	AgentPhaseDraining AgentPhase = "Draining"
	// AgentPhaseWaitingForMachine means the agent is waiting for a replacement machine after preemption.
	AgentPhaseWaitingForMachine AgentPhase = "WaitingForMachine"
	// AgentPhaseNeedsAuth means the agent's stored refresh token is invalid and
	// requires the user to re-run the OAuth flow via the PWA's Re-authorize action.
	AgentPhaseNeedsAuth AgentPhase = "NeedsAuth"
	// AgentPhaseMemoryExhausted means the agent container was killed by the
	// kernel OOM killer (kubelet tagged the termination as OOMKilled). No
	// auto-restart fires — a crash-loop on a too-small memory limit would
	// just hide the underlying problem. Operator action (typically: bump
	// spec.resources.memory, then trigger Restart) returns the agent to
	// Starting. Pattern mirrors NeedsAuth: human-required, no retry.
	AgentPhaseMemoryExhausted AgentPhase = "MemoryExhausted"
)

// Condition Type constants for Agent.status.conditions.
const (
	// AgentConditionSidecarOutOfDate is set on Agent.status.conditions
	// when a Running pod's kyber-status-sidecar container is on a
	// different image digest than the controller's current
	// StatusSidecarImage. The condition fires when an operator (or
	// ArgoCD Image Updater) bumps the sidecar pin but existing agent
	// pods haven't been recreated to pick up the new sidecar — those
	// pods continue running an outdated sidecar until manually rolled.
	//
	// The PWA's dirty / restart-required surface (kyber#157 PR-A)
	// treats this condition as a reason to flag the agent. Recovery
	// is a pod recreate. See kyber#299.
	AgentConditionSidecarOutOfDate = "SidecarOutOfDate"

	// AgentConditionModelUnresolved is retained for API compatibility with
	// older control planes. Current controllers clear it because an empty
	// resolved model now explicitly means "use the harness default."
	AgentConditionModelUnresolved = "ModelUnresolved"

	// AgentConditionRuntimeImageMissing is set True when the agent's runtime
	// is registered but this cluster has no container image configured for
	// it — e.g. spec.runtime: codex on an install that never set
	// image.codex.tag, so KYBER_CODEX_RUNTIME_IMAGE is unset and the adapter
	// returns "" (pkg/runtimes/codex/adapter.go).
	//
	// Without this condition the failure is INVISIBLE: pod_builder stamps the
	// empty image onto containers[0], the API server rejects the pod with
	// `spec.containers[0].image: Required value`, executeAction returns that
	// error, and Reconcile bails before updatePhase ever runs — leaving the
	// agent with a completely blank status (no phase, no message) while the
	// controller retries every ~20s forever. The only evidence is a
	// control-plane log line. See kyber#674 (found in production bringing
	// up the first prod Codex agent).
	//
	// The reconciler refuses to build a pod in this state rather than
	// attempting an invalid create. Recovery is an operator action on the
	// install, not on the agent: pin the runtime's image (image.<runtime>.tag
	// in the Helm values) and the condition clears on the next reconcile.
	AgentConditionRuntimeImageMissing = "RuntimeImageMissing"

	// AgentConditionTelegramUnavailable is set True when the agent has
	// spec.secrets.telegramEnabled but the install has not pinned
	// image.telegramSidecar.tag, so no Telegram sidecar can be injected.
	//
	// This became operator-visible rather than silent in kyber#684. The sidecar
	// is now the ONLY Telegram implementation — the native Claude Code plugin
	// was retired and the runtime container no longer receives the bot token —
	// so an unpinned sidecar image means the agent has no Telegram at all.
	// Before #684 that same install would silently fall back to the plugin, so
	// without this condition the convergence would look like a working agent
	// that had quietly stopped answering its operator.
	//
	// Deliberately NOT fatal to pod construction, unlike RuntimeImageMissing:
	// Telegram is one channel, not the agent's reason to exist, and killing the
	// pod would turn a degraded channel into a dead agent. Recovery is an
	// operator action on the install: pin image.telegramSidecar.tag.
	AgentConditionTelegramUnavailable = "TelegramUnavailable"

	// AgentConditionRuntimeVersionMismatch is set True when the agent
	// pod's reported InstalledVersion (what the agent actually runs)
	// differs from its RequestedVersion (what spec.runtimeVersion or the
	// fleet default asked for) — usually because the boot-time
	// `npm install` failed and start-claude.sh fell back to the
	// baked-in version. The condition Message names both versions so an
	// operator sees the mismatch without grepping pod logs. Recovery:
	// fix whatever made the install fail (registry outage, bad version
	// string), then restart the agent so the boot path re-attempts the
	// install. Cleared (= False) on the next report cycle in which
	// installed == requested. See kyber#379 / PR-E of #374.
	AgentConditionRuntimeVersionMismatch = "RuntimeVersionMismatch"

	// AgentConditionModelUnsupported is set True when the agent's
	// pre-flight model probe (`claude --model <resolved> --print 'ping'`
	// with a hard timeout) reported the resolved model as unsupported by
	// the installed Claude Code version. Surfaces what was previously a
	// silent failure (R2-D2 incident class — agent boots, model rejected
	// inside the detached tmux session, no exit code observable to the
	// platform). Recovery: apply a newer CC version
	// (`spec.runtimeVersion` per-agent or `defaultRuntimeVersion`
	// fleet-wide) that supports the model. See kyber#379.
	AgentConditionModelUnsupported = "ModelUnsupported"
)

// AgentAuthType is the authentication method for Claude Code.
// +kubebuilder:validation:Enum=oauth;api-key
type AgentAuthType string

const (
	AgentAuthTypeOAuth  AgentAuthType = "oauth"
	AgentAuthTypeAPIKey AgentAuthType = "api-key"
)

// AgentResources specifies compute resource requests for an agent pod.
type AgentResources struct {
	// CPU is the CPU request and limit (e.g., "1", "500m").
	CPU resource.Quantity `json:"cpu"`
	// Memory is the memory request and limit (e.g., "2Gi", "512Mi").
	Memory resource.Quantity `json:"memory"`
	// Disk is the size of the agent's persistent volume (e.g., "50Gi").
	Disk resource.Quantity `json:"disk"`
}

// AgentIdentity holds the agent's identity configuration written to its PV on first boot.
type AgentIdentity struct {
	// SoulDescription is a short narrative used to scaffold the agent's SOUL.md on first boot.
	SoulDescription string `json:"soulDescription,omitempty"`
}

// AgentIdentityRepo configures a GitHub identity repo for the agent. When set,
// the control plane mints a short-lived installation token via the Kyber
// GitHub App and exposes it to the pod so start-claude.sh can clone (or pull)
// the repo into the agent's home dir. The token is refreshed continuously so
// long-running pods can push memory updates.
type AgentIdentityRepo struct {
	// Repo is the "owner/name" slug of an existing GitHub repo that the Kyber
	// GitHub App has access to (e.g. "matty-v/chewie-agent"). When empty, the
	// agent runs without a mounted identity repo.
	// +optional
	Repo string `json:"repo,omitempty"`

	// Template is the "owner/repo" slug of a GitHub template repository. When
	// set and Repo is empty, the controller creates a new private repo from this
	// template under the configured default owner (KYBER_IDENTITY_REPO_OWNER),
	// substitutes {{ .AgentName }} and {{ .Description }} in all text files,
	// and sets Repo to the resulting "owner/repo". Empty means no auto-create;
	// the operator must supply Repo directly.
	// +optional
	Template string `json:"template,omitempty"`
}

// AgentIdentityRepoPhase tracks the mint/refresh state of the per-agent
// GitHub Secret.
type AgentIdentityRepoPhase string

const (
	// AgentIdentityRepoPhasePending means no token has been minted yet.
	AgentIdentityRepoPhasePending AgentIdentityRepoPhase = "Pending"
	// AgentIdentityRepoPhaseReady means a valid token is live in the Secret.
	AgentIdentityRepoPhaseReady AgentIdentityRepoPhase = "Ready"
	// AgentIdentityRepoPhaseFailed means the last mint attempt failed; see
	// Message for the reason.
	AgentIdentityRepoPhaseFailed AgentIdentityRepoPhase = "Failed"
)

// AgentRuntimeStatus reports the version of the agent runtime harness
// (claude-code today, codex/other in the future) that is actually installed
// in the running pod. Populated by the in-pod start script on boot via a
// POST to /internal/agents/{name}/runtime-version. Resets on pod restart.
type AgentRuntimeStatus struct {
	// InstalledVersion is the version string reported by the runtime's
	// `--version` command at pod start (e.g. "2.1.119"). Empty when the pod
	// has not yet reported or the version could not be determined (in which
	// case the reporter sends the literal string "unknown").
	// +optional
	InstalledVersion string `json:"installedVersion,omitempty"`
	// InstalledAt is when the controller last received a runtime-version
	// report for this agent. Useful for detecting stale reports after a
	// pending upgrade.
	// +optional
	InstalledAt *metav1.Time `json:"installedAt,omitempty"`
	// RequestedVersion is what spec.runtimeVersion (or the fleet default
	// from PR-B) asked the agent to run. The runtime-image roll injects
	// this so the reconciler can compare against InstalledVersion and
	// raise AgentConditionRuntimeVersionMismatch when they disagree.
	// Empty when the agent never overrode the baked-in default (existing
	// behavior pre-PR-C). See kyber#379.
	// +optional
	RequestedVersion string `json:"requestedVersion,omitempty"`
	// RequestedSatisfied is true when the boot-time `npm install` ran
	// successfully (or was skipped because the requested version matched
	// the baked-in pin); false when the install failed and the pod fell
	// back to the baked-in version. Nil when the report came from an
	// older sidecar image that doesn't populate the field (the runtime-
	// image roll is staggered — see kyber#379 § Sidecar coordination).
	// +optional
	RequestedSatisfied *bool `json:"requestedSatisfied,omitempty"`
	// ModelSupported is true when the pre-flight probe
	// (`claude --model <resolved> --print 'ping'` with a hard timeout)
	// succeeded; false when it failed in a way attributable to an
	// unknown/unsupported model. Nil when the report came from an older
	// sidecar image that doesn't run the probe. The reconciler raises
	// AgentConditionModelUnsupported on False; a timeout that isn't
	// clearly attributable to model rejection reports nil rather than
	// false so a flaky network blip doesn't flip the badge.
	// +optional
	ModelSupported *bool `json:"modelSupported,omitempty"`
	// ModelProbeMessage carries the pre-flight probe's diagnostic output
	// (sanitized, truncated) when the probe did not succeed — the CLI's
	// actual rejection or error text. Empty on success or when the pod
	// runs an older image that reports only the legacy boolean. Lets the
	// ModelUnsupported condition (and the PWA via blockedReason) show
	// WHY the model was rejected, and lets an inconclusive probe surface
	// as Unknown instead of silently reporting nothing.
	// +optional
	ModelProbeMessage string `json:"modelProbeMessage,omitempty"`
}

// AgentIdentityRepoStatus reports the observed state of the agent's identity
// repo credential. Written by the reconciler after each mint attempt.
type AgentIdentityRepoStatus struct {
	// Phase is the observed state of the identity repo token.
	// +optional
	Phase AgentIdentityRepoPhase `json:"phase,omitempty"`
	// Repo mirrors spec.identityRepo.repo so status readers don't need to
	// cross-reference the spec.
	// +optional
	Repo string `json:"repo,omitempty"`
	// TokenExpiresAt is the expiry timestamp reported by GitHub for the most
	// recently minted token. Reconciler uses this to schedule refresh.
	// +optional
	TokenExpiresAt *metav1.Time `json:"tokenExpiresAt,omitempty"`
	// LastMinted is when the reconciler last successfully wrote a fresh token
	// into the agent's github Secret.
	// +optional
	LastMinted *metav1.Time `json:"lastMinted,omitempty"`
	// Message is a human-readable error when Phase=Failed.
	// +optional
	Message string `json:"message,omitempty"`
}

// AgentJob declares a recurring prompt that should be delivered into the
// agent's tmux session on a cron schedule. Rendered by the controller into a
// per-agent ConfigMap that the pod mounts and the in-pod dispatcher reads.
type AgentJob struct {
	// Name is a human-readable label for the job. Must be unique within the
	// Agent and safe to use as a filename (used for per-job lock files under
	// /persist/var/lock/).
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Schedule is a standard 5-field cron expression (minute hour dom month dow).
	// Interpreted in the pod's configured TZ.
	// +kubebuilder:validation:MinLength=9
	// +kubebuilder:validation:MaxLength=200
	Schedule string `json:"schedule"`

	// Prompt is the text delivered into the agent's tmux session when the job
	// fires. Multi-line prompts are supported; newlines are preserved.
	// +kubebuilder:validation:MinLength=1
	Prompt string `json:"prompt"`

	// Exclusive, when true, causes the dispatcher to skip a fire if a previous
	// run of the same job is still holding the per-job flock. Default false
	// lets concurrent fires overlap.
	// +optional
	Exclusive bool `json:"exclusive,omitempty"`
}

// AgentJobOutcome reports the terminal state of a single job dispatch attempt.
// +kubebuilder:validation:Enum=success;failed;skipped
type AgentJobOutcome string

const (
	// AgentJobOutcomeSuccess means the dispatcher sent the prompt into tmux
	// without error. It does NOT mean the agent fully processed the prompt —
	// tmux delivery is the only thing the dispatcher can verify.
	AgentJobOutcomeSuccess AgentJobOutcome = "success"
	// AgentJobOutcomeFailed means the dispatcher could not deliver the prompt
	// (tmux session missing, send-keys non-zero, etc.). See Error for detail.
	AgentJobOutcomeFailed AgentJobOutcome = "failed"
	// AgentJobOutcomeSkipped means the dispatcher chose not to fire because a
	// lock was held (exclusive job overlap or session-restart in progress).
	AgentJobOutcomeSkipped AgentJobOutcome = "skipped"
)

// AgentJobRun is one entry in the status.jobs ring buffer. The controller
// caps the buffer at 50 entries per job name.
type AgentJobRun struct {
	// JobName matches an AgentJob.Name at the time the run was recorded.
	// Entries are not removed when the underlying job is renamed or deleted,
	// so a reader may see names that no longer resolve in spec.jobs.
	JobName string `json:"jobName"`

	// StartedAt is when the dispatcher began handling the fire.
	StartedAt *metav1.Time `json:"startedAt"`

	// FinishedAt is when the dispatcher finished (success, failure, or skip).
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// Outcome is the terminal state of this run.
	Outcome AgentJobOutcome `json:"outcome"`

	// Error is a short human-readable reason when Outcome is "failed" or
	// "skipped". Empty on success.
	// +optional
	Error string `json:"error,omitempty"`
}

// AgentInboundBinding declares a generic HMAC-authenticated inbound prompt source.
// External systems POST signed payloads to /webhooks/inbound/<agent>/<binding>; the
// platform extracts fields per this binding and renders a structured envelope into
// the agent's session via kyber-job-dispatch.
type AgentInboundBinding struct {
	// Name identifies the binding within the agent. Forms part of the URL.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// ExistingSecret is the name of a Kubernetes Secret holding the binding's
	// HMAC secret. The Secret must contain a key named `webhook-secret`. Same
	// convention as Telegram secrets.
	// +kubebuilder:validation:MinLength=1
	ExistingSecret string `json:"existingSecret"`

	// SignatureHeader is the request header carrying the HMAC. Operator-defined
	// (e.g. "X-Hub-Signature-256" for GitHub).
	// +kubebuilder:validation:MinLength=1
	SignatureHeader string `json:"signatureHeader"`

	// SignaturePrefix is stripped from the header value before hex-decoding.
	// Empty string is valid (header is bare hex). Common: "sha256=".
	// +optional
	SignaturePrefix string `json:"signaturePrefix,omitempty"`

	// EventHeader names the request header that carries the event identifier.
	// Optional — if empty, every authenticated request is treated as event "".
	// +optional
	EventHeader string `json:"eventHeader,omitempty"`

	// EventPath is an optional JSONPath into the request body whose extracted
	// value is appended to the EventHeader value, separated by a dot. Lets the
	// operator build composite event identifiers like "pull_request.opened"
	// from the GitHub X-GitHub-Event header ("pull_request") plus body.action
	// ("opened").
	// +optional
	EventPath string `json:"eventPath,omitempty"`

	// MatchEvents is a list of event identifiers this binding accepts. If
	// empty, every event passes. Otherwise the resolved event identifier must
	// appear in this list.
	// +optional
	MatchEvents []string `json:"matchEvents,omitempty"`

	// Filters narrow further within matched events. ALL filters must pass.
	// +optional
	Filters []AgentInboundFilter `json:"filters,omitempty"`

	// Fields define the data block of the envelope rendered into the agent's
	// prompt. Order is preserved in rendering.
	// +optional
	Fields []AgentInboundField `json:"fields,omitempty"`

	// Action is the operator's static instruction text rendered into the
	// envelope. V1 has no templating — preserves the data/instruction trust
	// boundary.
	// +kubebuilder:validation:MinLength=1
	Action string `json:"action"`

	// Limits applies per-binding rate limiting. Defaults: maxPerMinute=10.
	// +optional
	Limits *AgentInboundLimits `json:"limits,omitempty"`
}

// AgentInboundFilter is a single conditional clause evaluated against the
// request body. At least one of In or Equals should be set; if both are set
// both must pass (logical AND). A filter with neither In nor Equals is a
// no-op pass — admission does not reject this misconfiguration today; the
// dispatcher just lets the filter through.
type AgentInboundFilter struct {
	// JsonPath into the request body.
	// +kubebuilder:validation:MinLength=1
	JsonPath string `json:"jsonPath"`

	// In matches if the extracted value (string-coerced) appears in this list.
	// +optional
	In []string `json:"in,omitempty"`

	// Equals matches if the extracted value (string-coerced) equals this string.
	// +optional
	Equals string `json:"equals,omitempty"`
}

// AgentInboundField extracts a value from the request body for inclusion in
// the rendered envelope's data block.
type AgentInboundField struct {
	// Label names this field in the envelope. Operator-friendly identifier.
	// +kubebuilder:validation:MinLength=1
	Label string `json:"label"`

	// JsonPath into the request body.
	// +kubebuilder:validation:MinLength=1
	JsonPath string `json:"jsonPath"`

	// Truncate caps the rendered field length to this many runes (UTF-8
	// codepoints, not bytes). Zero means no truncation.
	// +optional
	Truncate int `json:"truncate,omitempty"`

	// Prefix is prepended to the rendered field. Useful for marking URLs as such.
	// +optional
	Prefix string `json:"prefix,omitempty"`
}

// AgentInboundLimits configures per-binding rate limiting.
type AgentInboundLimits struct {
	// MaxPerMinute is the per-binding token-bucket rate. Default 10.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=600
	// +optional
	MaxPerMinute int `json:"maxPerMinute,omitempty"`
}

// AgentInboundRun is a recorded delivery of an inbound prompt — appended to
// status as a ring buffer (cap MaxInboundRunsPerBinding per binding).
type AgentInboundRun struct {
	// BindingName matches an AgentInboundBinding.Name at the time the run was
	// recorded. Entries are not removed when the underlying binding is renamed
	// or deleted, so a reader may see names that no longer resolve in
	// spec.inboundBindings.
	BindingName string `json:"bindingName"`

	// RequestID is a server-assigned identifier for the inbound request,
	// useful for correlating with logs.
	RequestID string `json:"requestId"`

	// StartedAt is when the inbound handler began processing the request.
	StartedAt *metav1.Time `json:"startedAt"`

	// FinishedAt is when the handler finished (dispatched or dropped).
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// Outcome is the terminal state of this run: "dispatched" or "dropped".
	Outcome string `json:"outcome"`

	// DropReason explains why a "dropped" run was rejected. One of:
	// sig-mismatch | missing-secret | unmatched-event | filter-rejected |
	// rate-limited | queue-full | dedup. Empty when Outcome is "dispatched".
	// +optional
	DropReason string `json:"dropReason,omitempty"`

	// Error is a short human-readable error string when processing failed
	// for an unexpected reason. Empty on the normal dispatched/dropped paths.
	// +optional
	Error string `json:"error,omitempty"`
}

// AgentSecrets holds references to secrets in GCP Secret Manager for this agent.
type AgentSecrets struct {
	// TelegramEnabled indicates whether this agent has a Telegram bot configured.
	// When true, the runtime adapter injects the TELEGRAM_BOT_TOKEN env var from
	// the k8s secret named "<agent-name>-telegram" (key "token"). Operators are
	// responsible for creating the secret before setting this flag.
	// +optional
	TelegramEnabled bool `json:"telegramEnabled,omitempty"`
	// DiscordEnabled indicates whether this agent has a Discord webhook configured
	// (kyber#132 Phase 1). When true, the runtime adapter injects the
	// KYBER_DISCORD_WEBHOOK env var from the k8s secret named "<agent-name>-discord"
	// (key "webhook-url"). Outbound-only: the agent can curl the webhook URL to
	// post messages into a Discord channel; bidirectional comms wait on
	// kyber#138 (per-channel MCP sidecars) for Phase 2.
	// +optional
	DiscordEnabled bool `json:"discordEnabled,omitempty"`
	// AnthropicKey is the Secret Manager path to the agent's Anthropic API key.
	// +optional
	AnthropicKey string `json:"anthropicKey,omitempty"`
	// AuthType specifies the Claude authentication method: "oauth" or "api-key".
	AuthType AgentAuthType `json:"authType"`
}

// AgentSpec defines the desired state of an Agent.
type AgentSpec struct {
	// Machine is the name of the Machine CRD this agent should run on.
	Machine string `json:"machine"`

	// Runtime identifies the RuntimeAdapter type (e.g., "claude-code").
	Runtime string `json:"runtime"`

	// StartupPrompt is delivered as the initial user turn whenever a new
	// runtime harness session starts, and — when SessionResume is also set —
	// as the first turn of a resumed session, so an agent interrupted
	// mid-task acts on its restored context instead of idling. Empty
	// preserves the runtime's normal interactive startup behavior.
	// +optional
	// +kubebuilder:validation:MaxLength=32768
	StartupPrompt string `json:"startupPrompt,omitempty"`

	// SessionResume, when true, makes the runtime resume its previous
	// harness session after an unexpected session end — pod recreate,
	// machine preemption, or an in-pod crash (kyber#118). If StartupPrompt
	// is also set, resume launches deliver it into the resumed session as
	// the trigger to continue interrupted work. An intentional
	// restart-session always starts a fresh session regardless of this
	// flag: intentional restart is the operator's context-clearing tool.
	// +optional
	SessionResume bool `json:"sessionResume,omitempty"`

	// Resources specifies the compute resources requested for the agent pod.
	Resources AgentResources `json:"resources"`

	// DesiredPhase is written by the API module to signal a lifecycle intent.
	// Valid values: Running, Stopped, Restarting, NeedsAuth.
	// The controller reads this and drives state transitions.
	//
	// NeedsAuth is the operator-forced re-auth intent (#395), written by
	// POST /agents/{name}/force-needs-auth. It MUST stay in this enum: the
	// setter and the controller gate (classifyEvent's EventDesiredNeedsAuth
	// arm) have both handled it since #395, but it was missing here, so the
	// API server rejected every write and the endpoint returned a blanket
	// 500 — the action could never once have worked. Unit tests missed it
	// because they build the Agent in memory and call classifyEvent directly,
	// never writing through an API server that would apply this schema; see
	// TestDesiredPhaseEnum_AcceptsEveryAPISettablePhase, which closes that gap.
	// +kubebuilder:validation:Enum=Running;Stopped;Restarting;NeedsAuth
	// +optional
	DesiredPhase AgentPhase `json:"desiredPhase,omitempty"`

	// Identity holds configuration for agent identity scaffolding on first boot.
	// +optional
	Identity AgentIdentity `json:"identity,omitempty"`

	// IdentityRepo points at a GitHub repo used as the agent's mutable
	// identity/memory store. When spec.identityRepo.repo is set, the
	// controller mints a short-lived installation token via the Kyber GitHub
	// App and exposes it to the pod (see `images/claude-code/start-claude.sh`
	// for the clone/credential-helper wiring).
	// +optional
	IdentityRepo AgentIdentityRepo `json:"identityRepo,omitempty"`

	// Secrets holds references to GCP Secret Manager paths for this agent.
	Secrets AgentSecrets `json:"secrets"`

	// Model is the LLM model identifier to use (e.g., "claude-sonnet-4").
	// Optional: when empty, the controller resolves the fleet-default
	// model (kyber-fleet-defaults ConfigMap key `defaultModel`, seeded by
	// the chart's `controlPlane.fleetDefaults.defaultModel` value and
	// writable via the PWA Settings panel — kyber#376 / PR-B of #374).
	// If both spec.model and the fleet default are empty, the runtime harness
	// selects its own default model.
	// +optional
	Model string `json:"model,omitempty"`

	// RuntimeVersion is the requested Claude Code CLI version for this
	// agent. Optional: when empty, the controller resolves the fleet
	// default (kyber-fleet-defaults ConfigMap key `defaultRuntimeVersion`,
	// seeded by chart value `controlPlane.fleetDefaults.defaultRuntimeVersion`
	// which itself defaults to the build-time `runtime.claudeCode.version`
	// baked into the kyber-claude-code image). On boot, start-claude.sh
	// validates the resolved value against the charset pattern, then runs
	// `npm install -g @anthropic-ai/claude-code@<version>` iff it differs
	// from the baked-in default. Install failure is non-fatal — the pod
	// continues on the baked-in version and PR-E surfaces the mismatch via
	// the runtime report. The kubebuilder Pattern + MaxLength validate
	// server-side; start-claude.sh re-validates pre-shell-interpolation as
	// defense in depth (kyber#377 / PR-C of #374).
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9A-Za-z.\-]+$`
	// +kubebuilder:validation:MaxLength=64
	RuntimeVersion string `json:"runtimeVersion,omitempty"`

	// Jobs declares recurring scheduled prompts for this agent. Each entry is
	// rendered as a line in a per-agent crontab file (ConfigMap-delivered),
	// fired by the pod's cron daemon, and dispatched into the agent's tmux
	// session by the in-pod kyber-job-dispatch helper.
	// +optional
	Jobs []AgentJob `json:"jobs,omitempty"`

	// InboundBindings declares generic HMAC-authenticated inbound prompt
	// sources for this agent. Each binding exposes a public endpoint at
	// /api/v1/inbound/<agent>/<binding>; verified payloads are extracted and
	// dispatched into the agent's tmux session via kyber-job-dispatch.
	// +optional
	InboundBindings []AgentInboundBinding `json:"inboundBindings,omitempty"`

	// Channels declares bidirectional comms channels for this agent, each
	// backed by a Kyber-managed sidecar container that holds the channel
	// connection and bridges inbound messages onto an inbound binding
	// (kyber#138, #646). Distinct from Secrets.{Telegram,Discord}Enabled,
	// which are the older runtime-coupled / outbound-only paths.
	// +optional
	Channels *AgentChannels `json:"channels,omitempty"`
}

// AgentChannels groups the per-channel bidirectional configs for an agent.
type AgentChannels struct {
	// Discord enables two-way Discord via the kyber-mcp-discord sidecar
	// (kyber#646). Nil = disabled.
	// +optional
	Discord *AgentDiscordChannel `json:"discord,omitempty"`
}

// AgentDiscordChannel configures the two-way Discord sidecar for an agent.
type AgentDiscordChannel struct {
	// ExistingSecret names a Secret with the channel credentials.
	// Required keys: "bot-token" (Discord bot token) and "webhook-secret"
	// (the HMAC secret, which must match the agent's "discord" inbound
	// binding). Optional keys: "guild-id", "channel-id", "allowed-user-ids"
	// (csv) — the allowlist; an empty allowlist is fail-closed (no inbound
	// accepted).
	//
	// Nothing renders the companion "discord" inbound binding from this field.
	// PUT /api/v1/agents/{name}/comms/discord (kyber#664) writes the Secret,
	// the binding, and this field together with a shared HMAC secret; setting
	// this field alone leaves the binding missing and inbound silently dropped.
	ExistingSecret string `json:"existingSecret"`

	// MentionOnly restricts inbound to messages that address the bot directly:
	// an @-mention of the bot user, or a reply to one of the bot's own messages.
	// Default (false) forwards every allowlisted message in the channel, which
	// is right for a dedicated agent channel and wrong for a shared one where
	// humans also talk to each other. Orthogonal to the allowlist — this narrows
	// WHICH messages count, not WHO may send them.
	// +optional
	MentionOnly bool `json:"mentionOnly,omitempty"`
}

// AgentStatus defines the observed state of an Agent.
type AgentStatus struct {
	// Phase is the current lifecycle phase of the agent.
	Phase AgentPhase `json:"phase,omitempty"`

	// PodName is the name of the currently running agent pod.
	// +optional
	PodName string `json:"podName,omitempty"`

	// PodIP is the IP address of the currently running agent pod.
	// +optional
	PodIP string `json:"podIP,omitempty"`

	// NodeName is the k8s node the agent pod is scheduled on.
	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// StartTime is the time the current pod became Running.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// LastTransition is the time of the most recent phase transition.
	// +optional
	LastTransition *metav1.Time `json:"lastTransition,omitempty"`

	// CurrentModel is the model currently in use by the agent.
	// Updated when a model change takes effect.
	// +optional
	CurrentModel string `json:"currentModel,omitempty"`

	// RestartCount is the number of consecutive restart attempts since last stable Running.
	// Reset after the agent has been Running for the retry reset threshold.
	// +optional
	RestartCount int32 `json:"restartCount,omitempty"`

	// RecoveryInput identifies the operator-supplied input that the most
	// recent recovery attempt out of a human-required phase consumed — the
	// credential Secret's resourceVersion for NeedsAuth, the memory limit for
	// MemoryExhausted.
	//
	// Human-required phases cannot use spec.desiredPhase=Running as their
	// retry trigger: it is permanently true for every agent, so there is no
	// edge and the agent rebuilds its pod forever (kyber#684 — measured at 515
	// pod creations in 53 minutes on an agent with a dead credential). Re-entry
	// is gated on this value changing, so it happens once per genuinely new
	// input instead of once per reconcile. Empty means "no attempt recorded",
	// which permits exactly one attempt.
	// +optional
	RecoveryInput string `json:"recoveryInput,omitempty"`

	// Message is a human-readable description of the current state or last error.
	// +optional
	Message string `json:"message,omitempty"`

	// IdentityRepo reports the mint/refresh state of the agent's GitHub
	// identity-repo credential, when spec.identityRepo.repo is set.
	// +optional
	IdentityRepo AgentIdentityRepoStatus `json:"identityRepo,omitempty"`

	// Runtime reports the version of the agent-runtime harness (claude-code,
	// codex, etc.) actually installed in the running pod. Populated by the
	// in-pod start script via /internal/agents/{name}/runtime-version on
	// each pod start.
	// +optional
	Runtime AgentRuntimeStatus `json:"runtime,omitempty"`

	// Jobs is the bounded ring buffer of recent job dispatch runs. Capped at
	// 50 entries per spec.jobs[].name by the controller; older entries roll
	// off as new ones are appended. Ordered oldest-first.
	// +optional
	Jobs []AgentJobRun `json:"jobs,omitempty"`

	// InboundRuns is the bounded ring buffer of recent inbound prompt
	// deliveries. Capped at MaxInboundRunsPerBinding entries per
	// spec.inboundBindings[].name by the controller; older entries roll off as
	// new ones are appended. Ordered oldest-first.
	// +optional
	InboundRuns []AgentInboundRun `json:"inboundRuns,omitempty"`

	// Scheduling reports a stuck-Pending pod's most recent scheduling /
	// kubelet failure (FailedScheduling, FailedMount, ImagePullBackOff,
	// ErrImagePull). Populated by the controller after a 30s grace window
	// and cleared when the pod reaches Running. Drives the PWA banner that
	// surfaces "why is this agent stuck?" without forcing the operator
	// into kubectl. See kyber#210.
	// +optional
	Scheduling *AgentSchedulingStatus `json:"scheduling,omitempty"`

	// Activity carries runtime-agnostic activity signals from the
	// kyber-status-sidecar (kyber#247 epic; foundation in kyber#248).
	// Phase A populates LastHeartbeatAt only; Phase B (kyber#249) fills in
	// State (working/idle) and LastActivityAt.
	// +optional
	Activity *ActivityStatus `json:"activity,omitempty"`

	// Conditions holds standard k8s conditions for this Agent.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the spec.metadata.generation that was last
	// applied to a running pod. The reconciler stamps this when it
	// successfully creates or recreates the pod, so spec changes that DO
	// trigger a roll update it; spec changes that DON'T (resources today,
	// per kyber#149; runtime/adapter fields where the pod isn't recreated)
	// leave it lagging. The PWA derives a "restart required" badge from
	// metadata.generation > status.observedGeneration. See kyber#157.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastKernelOOMKillAt is the wall-clock timestamp of the most recent
	// kernel OOM kill observed inside this agent's pod-level cgroup, as
	// reported by kyber-status-sidecar reading the recursive memory.events
	// file (kyber#285). Closes the gap left by #272 V1: kubelet only tags
	// containerStatuses[*].State.Terminated.Reason="OOMKilled" when the
	// kernel's OOM-killer victim is the container's PID 1. When PID 1 is
	// a shepherd like dbus-run-session and the killer picks a child
	// (claude-code), kubelet records reason="Error" and the OOM is
	// invisible at the K8s API level. The cgroup counter is kernel-
	// authoritative and does not depend on which process was the victim.
	//
	// Populated by the sidecar when the cgroup's oom_kill counter
	// increments. Compared by the agent controller against the agent
	// container's StartedAt to attribute the kill to the current life,
	// not a previous one. See classifyEvent for the routing logic.
	// +optional
	LastKernelOOMKillAt *metav1.Time `json:"lastKernelOOMKillAt,omitempty"`
}

// AgentSchedulingStatus mirrors a Pod-level scheduling/kubelet failure up to
// the Agent CR. Populated by the controller via populateSchedulingStatus
// when a pod has been Pending past schedulingGracePeriod with a matching
// event reason. Cleared when the pod reaches Running.
type AgentSchedulingStatus struct {
	// Category classifies the failure into one of a small enum so the PWA
	// can render category-specific remediation copy. Derived from the
	// event reason + message via classifySchedulingCategory.
	// +kubebuilder:validation:Enum=Capacity;Placement;Image;Storage;Other
	Category string `json:"category"`

	// LastError is the verbatim event message (e.g.
	// "0/1 nodes are available: 1 Insufficient memory."). The PWA renders
	// this in monospace so operators have a search string for kubectl.
	LastError string `json:"lastError,omitempty"`

	// FirstObservedAt is when the controller first wrote this scheduling
	// status entry. Preserved across reconciles, including when the
	// category changes (so operators read the true total stuckness
	// duration). Cleared only when the pod reaches Running.
	// +optional
	FirstObservedAt *metav1.Time `json:"firstObservedAt,omitempty"`
}

// ActivityStatus carries runtime-agnostic activity signals from the
// kyber-status-sidecar. Phase A (kyber#248) populates LastHeartbeatAt
// only — proves the sidecar is alive and connected. Phase B (kyber#249)
// adds State + LastActivityAt from the runtime-specific Probe (e.g.
// claudecode.Probe tails the JSONL transcript to detect mid-turn vs idle).
type ActivityStatus struct {
	// LastHeartbeatAt is the timestamp of the most recent heartbeat event
	// the control plane received from the agent's status sidecar. Updated
	// on every heartbeat (currently 5s cadence). Treat absence (or a
	// timestamp older than ~30s) as "sidecar is unreachable" — the agent
	// itself may still be running, but the observability surface is dark.
	// +optional
	LastHeartbeatAt *metav1.Time `json:"lastHeartbeatAt,omitempty"`

	// State is the runtime-derived activity state. Populated in Phase B.
	// One of: "working", "idle", "unknown" (default when probe hasn't
	// reported). Empty string here matches the omitempty contract — the
	// PWA renders "no signal" rather than a stale dot.
	// +kubebuilder:validation:Enum=working;idle;unknown
	// +optional
	State string `json:"state,omitempty"`

	// LastActivityAt is the timestamp of the most recent observed activity
	// (turn start / tool invocation / etc — runtime-specific). Populated
	// in Phase B. Used by the PWA to render "active 2m ago".
	// +optional
	LastActivityAt *metav1.Time `json:"lastActivityAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ag
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Machine",type=string,JSONPath=`.spec.machine`
// +kubebuilder:printcolumn:name="Runtime",type=string,JSONPath=`.spec.runtime`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.model`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Agent is the schema for the agents API.
// An Agent CRD represents a single AI agent instance managed by the Kyber platform.
type Agent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentSpec   `json:"spec"`
	Status AgentStatus `json:"status,omitempty"`
}

// SpecChangedSinceLastPod reports whether the operator has edited the Agent
// spec since the reconciler last rolled it into a pod: metadata.generation is
// ahead of the status.observedGeneration that createPod stamps.
//
// Two callers depend on this predicate and they have to agree, which is why it
// lives on the type rather than being spelled out in each package. The PWA
// renders it as the "restart required" badge (kyber#157). The agent reconciler
// reads it as a one-shot claim on the Failed → Starting edge, where it is the
// difference between an operator override and an unbounded restart loop.
//
// Generation 0 is the never-reconciled baseline: an agent whose pod has never
// been built has nothing to compare against, so it reports false rather than
// treating generation=1 as an edit.
//
// Known looseness: observedGeneration also lags for spec edits that never roll
// a pod (spec.resources, per kyber#149), so an edit made while the agent was
// healthy can still read as "changed" the first time it is consulted later.
// Both callers are fine with that — the badge is meant to over-report, and the
// reconciler claims the generation before acting, so it is bounded to a single
// cycle rather than repeating.
func (a *Agent) SpecChangedSinceLastPod() bool {
	return a.Status.ObservedGeneration > 0 &&
		a.Generation > a.Status.ObservedGeneration
}

// +kubebuilder:object:root=true

// AgentList contains a list of Agent.
type AgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Agent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Agent{}, &AgentList{})
}
