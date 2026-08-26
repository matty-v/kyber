package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/oauth"
	pkgruntimes "github.com/matty-v/kyber/pkg/runtimes"
	"github.com/matty-v/kyber/pkg/tokenreport"
)

// CreateAgentRequest is the JSON body for POST /api/v1/agents.
type CreateAgentRequest struct {
	Name          string `json:"name"`
	Machine       string `json:"machine"`
	Runtime       string `json:"runtime"`
	Model         string `json:"model"`
	StartupPrompt string `json:"startupPrompt,omitempty"`
	SessionResume bool   `json:"sessionResume,omitempty"`
	// Force skips catalog validation of the model id, same as set-model.
	Force        bool                     `json:"force,omitempty"`
	Resources    agentResourcesRequest    `json:"resources"`
	Identity     agentIdentityRequest     `json:"identity"`
	IdentityRepo agentIdentityRepoRequest `json:"identityRepo,omitempty"`
	Secrets      agentSecretsRequest      `json:"secrets"`
}

type agentResourcesRequest struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
	Disk   string `json:"disk"`
}

type agentIdentityRequest struct {
	SoulDescription string `json:"soulDescription"`
}

// agentIdentityRepoRequest carries the GitHub identity-repo config for the
// agent at creation time. When Repo is set, the controller mints a short-lived
// installation token via the Kyber GitHub App and exposes it to the pod.
// When Template is set and Repo is empty, the controller creates a new repo
// from the template and fills in Repo automatically.
type agentIdentityRepoRequest struct {
	// Repo is the "owner/name" slug of an existing GitHub repo. Mutually
	// exclusive with Template: if both are set, Repo takes precedence.
	Repo string `json:"repo,omitempty"`
	// Template is the "owner/repo" slug of a GitHub template repo. When set
	// and Repo is empty, the controller scaffolds a new repo from this template.
	Template string `json:"template,omitempty"`
}

// agentSecretsRequest carries both the CRD fields AND the actual token values.
// The CRD fields (AuthType, TelegramEnabled, DiscordEnabled) go into the
// Agent CRD spec. The token values (AnthropicAPIKey, TelegramBotToken,
// DiscordWebhookUrl, OAuthCode+PkceVerifier) are used to create k8s Secrets
// that the runtime adapter references via SecretKeyRef. This avoids requiring
// the operator to `kubectl create secret` before creating an agent from the UI.
type agentSecretsRequest struct {
	AuthType        string `json:"authType"`
	TelegramEnabled bool   `json:"telegramEnabled"`
	// DiscordEnabled flips spec.secrets.discordEnabled (kyber#132 Phase 1).
	// When true, the runtime adapter injects KYBER_DISCORD_WEBHOOK from
	// the <agent-name>-discord Secret's webhook-url key. Outbound-only
	// today; bidirectional comms wait on kyber#138.
	DiscordEnabled bool `json:"discordEnabled,omitempty"`
	// Token values — the API creates k8s Secrets from these. They are NOT stored
	// in the Agent CRD (which only has the boolean/enum flags that tell the
	// adapter which secrets to mount). After the Secret is created, the token
	// value is discarded from memory.
	AnthropicAPIKey string `json:"anthropicApiKey,omitempty"`
	// OpenAIAPIKey is used only by Codex agents created in api-key mode. It is
	// stored in <agent>-openai and injected as OPENAI_API_KEY.
	OpenAIAPIKey string `json:"openaiApiKey,omitempty"`
	// CodexAuthJSON is the opaque auth.json produced by `codex login` using a
	// ChatGPT subscription. It is stored only in <agent>-codex-auth and is never
	// returned by the API.
	CodexAuthJSON          string   `json:"codexAuthJson,omitempty"`
	TelegramBotToken       string   `json:"telegramBotToken,omitempty"`
	TelegramAllowedUserIDs []string `json:"telegramAllowedUserIds,omitempty"`
	// DiscordWebhookUrl is a Discord channel webhook (the URL Discord
	// returns from "Edit Channel → Integrations → Webhooks → Copy URL").
	// Required when DiscordEnabled is true; ignored otherwise. Stored in
	// the <agent-name>-discord Secret under key "webhook-url".
	DiscordWebhookUrl string `json:"discordWebhookUrl,omitempty"`
	// PKCE-OAuth code exchange — the only supported OAuth auth path.
	// oauthCode + pkceVerifier are required when authType="oauth".
	OAuthCode    string `json:"oauthCode,omitempty"`
	PkceVerifier string `json:"pkceVerifier,omitempty"`
	PkceState    string `json:"pkceState,omitempty"`
}

// PatchAgentRequest is the JSON body for PATCH /api/v1/agents/{name}.
type PatchAgentRequest struct {
	Model         *string                `json:"model,omitempty"`
	StartupPrompt *string                `json:"startupPrompt,omitempty"`
	SessionResume *bool                  `json:"sessionResume,omitempty"`
	Resources     *agentResourcesRequest `json:"resources,omitempty"`
	// Jobs, when non-nil, replaces spec.jobs wholesale. Empty slice clears
	// all scheduled jobs; nil leaves them untouched. Matches the common
	// PUT-list semantics — callers that want additive semantics can fetch
	// current jobs via GET, mutate, and PATCH back.
	Jobs *[]AgentJobRequest `json:"jobs,omitempty"`
}

// AgentJobRequest mirrors kyberv1.AgentJob for the API surface.
type AgentJobRequest struct {
	Name              string `json:"name"`
	Schedule          string `json:"schedule"`
	Prompt            string `json:"prompt"`
	Exclusive         bool   `json:"exclusive,omitempty"`
	ClearContextAfter bool   `json:"clearContextAfter,omitempty"`
}

// SetRuntimeVersionRequest is the JSON body for POST
// /api/v1/agents/{name}/set-runtime-version. Empty runtimeVersion clears
// spec.runtimeVersion (reverts the agent to the fleet default, per PR-B
// resolution). Non-empty values must match the CRD's charset pattern
// (^[0-9A-Za-z.\-]+$) and length cap (64). (kyber#378 PR-D)
type SetRuntimeVersionRequest struct {
	RuntimeVersion string `json:"runtimeVersion"`
}

// SetModelRequest is the JSON body for POST /api/v1/agents/{name}/set-model.
type SetModelRequest struct {
	Model string `json:"model"`
	// Force skips catalog validation of the model id — the escape hatch
	// for a model newer than the last detection poll.
	Force bool `json:"force,omitempty"`
}

// SetResourcesRequest is the JSON body for POST /api/v1/agents/{name}/set-resources.
// Both fields optional; only provided fields are patched.
type SetResourcesRequest struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// Minimum bounds enforced by setAgentResources. The issue spec locks these:
// smaller requests risk starving the Claude Code runtime + helpers.
var (
	setResourcesMinCPU    = resource.MustParse("100m")
	setResourcesMinMemory = resource.MustParse("128Mi")
)

// AgentResponse is the JSON representation of an Agent returned by the API.
type AgentResponse struct {
	ID            string                `json:"id"`
	Phase         kyberv1.AgentPhase    `json:"phase"`
	Machine       string                `json:"machine"`
	Runtime       string                `json:"runtime"`
	AuthType      kyberv1.AgentAuthType `json:"authType"`
	Model         string                `json:"model"`
	StartupPrompt string                `json:"startupPrompt,omitempty"`
	SessionResume bool                  `json:"sessionResume,omitempty"`
	// CurrentModel is the concrete model observed from the running runtime.
	// It differs from Model when spec.model is empty (harness default).
	CurrentModel string                     `json:"currentModel,omitempty"`
	Resources    agentResourcesResponse     `json:"resources"`
	IdentityRepo *agentIdentityRepoResponse `json:"identityRepo,omitempty"`
	Status       agentStatusResponse        `json:"status,omitempty"`
	Jobs         []agentJobResponse         `json:"jobs,omitempty"`
	// LastJobRuns carries at most one entry per job name — the most recent
	// run of each. Callers that want the full history need the CR directly;
	// this shape keeps the standard API response compact for the PWA's
	// "Last run" column.
	LastJobRuns []agentJobRunResponse `json:"lastJobRuns,omitempty"`
	// InboundRuns is the per-agent ring buffer of inbound-prompt outcomes
	// (capped per binding via appendInboundRunCapped). The PWA Sources tab's
	// expandable per-binding "Recent runs" panel reads this to render the
	// replay button per row. Aggregate stats live on the dedicated
	// /api/v1/agents/{name}/inbound-bindings endpoint; this duplicates the
	// raw entries so the panel doesn't need a second round-trip.
	InboundRuns []agentInboundRunResponse `json:"inboundRuns,omitempty"`
	// TokenUsage is the most-recent context-budget snapshot for this agent,
	// populated from the TokenStore when available. Omitted when the store is
	// unset, the reporter hasn't written yet, or the entry has expired. The
	// /api/v1/agents/{name}/token-usage endpoint remains the canonical source
	// for detail views; this embed exists so the list view can render a
	// Context column without an N+1 fan-out from the client.
	TokenUsage *tokenreport.Snapshot `json:"tokenUsage,omitempty"`
	// RuntimeVersion is the runtime-harness version (e.g. "2.1.119") actually
	// installed in the agent's running pod. Populated by start-claude.sh on
	// boot via /internal/agents/{name}/runtime-version. Empty when the pod
	// has not yet reported or the version could not be determined.
	RuntimeVersion *agentRuntimeVersionResponse `json:"runtimeVersion,omitempty"`
	// Scheduling surfaces a stuck-Pending pod's most recent
	// scheduling/kubelet failure (FailedScheduling, ImagePullBackOff,
	// FailedMount, etc.). Populated by the controller after a 30s grace
	// window; cleared when the pod reaches Running. The PWA banner
	// (kyber#210 PR-B) renders category-specific copy off this field.
	Scheduling *agentSchedulingStatusResponse `json:"scheduling,omitempty"`
	// Activity carries runtime-agnostic activity signals from the
	// kyber-status-sidecar (kyber#247 epic; kyber#248 foundation).
	// Phase A populates LastHeartbeatAt only — proves the sidecar is
	// alive. Phase B (kyber#249) adds State + LastActivityAt.
	Activity *agentActivityStatusResponse `json:"activity,omitempty"`
	// Dirty is true when the running pod is on an older spec generation
	// than the live Agent CR — i.e. an operator edit hasn't yet been
	// rolled into a new pod. Derived as metadata.generation >
	// status.observedGeneration; surfaces the kyber#157 PR-A
	// "restart required" signal without requiring the PWA to know about
	// generation comparisons. Subresource changes that don't bump
	// metadata.generation (cron-job ConfigMap edits, Secret rotations)
	// are NOT covered by V1; tracked as #157 follow-ups.
	Dirty bool `json:"dirty,omitempty"`
	// RuntimeVersionMismatch is true when the agent's installed Claude
	// Code version differs from what spec.runtimeVersion (or the fleet
	// default) asked for — usually a failed boot-time install fell back
	// to the baked-in pin. Drives a PWA badge on the agent detail
	// view. Derived from AgentConditionRuntimeVersionMismatch. kyber#379.
	RuntimeVersionMismatch bool `json:"runtimeVersionMismatch,omitempty"`
	// ModelUnsupported is true when the agent's pre-flight model probe
	// (start-claude.sh, PR-E) failed in a way attributable to the
	// installed Claude Code rejecting the configured model. Drives a
	// PWA badge — the remedy hint is "apply a newer CC version."
	// Derived from AgentConditionModelUnsupported. kyber#379.
	ModelUnsupported bool `json:"modelUnsupported,omitempty"`
	// RuntimeImageMissing is true when this cluster has no container image
	// configured for the agent's runtime (e.g. runtime "codex" on an install
	// that never set image.codex.tag). The agent cannot start and the fix is
	// on the INSTALL, not the agent. Derived from
	// AgentConditionRuntimeImageMissing. kyber#674.
	RuntimeImageMissing bool `json:"runtimeImageMissing,omitempty"`
	// ModelUnresolved is true when neither spec.model nor the fleet-default
	// defaultModel resolves to a model, so the reconciler refuses to build a
	// pod. The condition has existed since kyber#376 but was never surfaced
	// on the wire, which made it invisible in the PWA — the same gap kyber#674
	// fixed for RuntimeImageMissing, closed here alongside it.
	ModelUnresolved bool `json:"modelUnresolved,omitempty"`
	// BlockedReason is the remediation message of whichever blocked-before-pod
	// condition is True (RuntimeImageMissing or ModelUnresolved), rendered
	// verbatim by the PWA.
	//
	// It deliberately does NOT come from status.message: on these paths the
	// reconcile returns before updatePhase ever runs, so status.message is
	// empty — that emptiness is the whole bug. The controller computes the
	// exact remediation (which Helm value, which field) into the condition, and
	// this carries it through unchanged so the fix lives in one place instead
	// of being re-derived in TypeScript. kyber#674.
	BlockedReason string `json:"blockedReason,omitempty"`
	CreatedAt     string `json:"createdAt"`
}

// agentActivityStatusResponse mirrors AgentStatus.Activity on the wire.
// All timestamps RFC3339 strings, consistent with the other status sub-
// responses.
type agentActivityStatusResponse struct {
	LastHeartbeatAt string `json:"lastHeartbeatAt,omitempty"`
	State           string `json:"state,omitempty"`
	LastActivityAt  string `json:"lastActivityAt,omitempty"`
}

// agentSchedulingStatusResponse mirrors AgentSchedulingStatus on the wire.
// Timestamps are RFC3339 strings to keep the response shape stringly-
// uniform with the other status sub-responses.
type agentSchedulingStatusResponse struct {
	Category        string `json:"category"`
	LastError       string `json:"lastError,omitempty"`
	FirstObservedAt string `json:"firstObservedAt,omitempty"`
}

// agentRuntimeVersionResponse mirrors status.runtime. Separate struct keeps
// the response shape extensible — future fields (defaultVersion from image,
// pendingVersion on upgrade) slot in without reshuffling the parent.
type agentRuntimeVersionResponse struct {
	// InstalledVersion is the version string the pod reported.
	InstalledVersion string `json:"installedVersion"`
	// InstalledAt is RFC3339 when the controller last received a report.
	InstalledAt string `json:"installedAt,omitempty"`
	// RequestedVersion is what the controller asked the agent to run
	// (resolved spec.runtimeVersion or fleet default). Empty when the
	// agent boots on the baked-in default. kyber#379 / PR-E.
	RequestedVersion string `json:"requestedVersion,omitempty"`
	// RequestedSatisfied is the boot-time install outcome from
	// start-claude.sh: true when the requested version is running, false
	// when the install fell back to baked-in. nil when the report came
	// from a sidecar predating PR-E (staggered roll — see internal.go's
	// handleRuntimeVersion). kyber#379.
	RequestedSatisfied *bool `json:"requestedSatisfied,omitempty"`
	// ModelSupported is the pre-flight probe outcome: true when
	// `claude --model <resolved> --print` succeeded, false when it
	// failed in a way attributable to model rejection. nil when the
	// probe didn't run or the report came from an older sidecar.
	// kyber#379.
	ModelSupported *bool `json:"modelSupported,omitempty"`
	// ModelProbeMessage is the probe's diagnostic output when it did not
	// succeed (the CLI's rejection or error text, sanitized/truncated by
	// the reporter). Empty on success or on reports from older images.
	ModelProbeMessage string `json:"modelProbeMessage,omitempty"`
}

type agentJobResponse struct {
	Name              string `json:"name"`
	Schedule          string `json:"schedule"`
	Prompt            string `json:"prompt"`
	Exclusive         bool   `json:"exclusive,omitempty"`
	ClearContextAfter bool   `json:"clearContextAfter,omitempty"`
}

type agentJobRunResponse struct {
	JobName    string `json:"jobName"`
	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt,omitempty"`
	Outcome    string `json:"outcome"`
	Error      string `json:"error,omitempty"`
}

// agentInboundRunResponse mirrors kyberv1.AgentInboundRun with timestamps
// rendered as RFC3339 strings — same shape the PWA's TS type already
// declares (lib/types.ts AgentInboundRun).
type agentInboundRunResponse struct {
	BindingName string `json:"bindingName"`
	RequestID   string `json:"requestId"`
	StartedAt   string `json:"startedAt"`
	FinishedAt  string `json:"finishedAt,omitempty"`
	Outcome     string `json:"outcome"`
	DropReason  string `json:"dropReason,omitempty"`
	Error       string `json:"error,omitempty"`
}

// agentIdentityRepoResponse mirrors spec.identityRepo + status.identityRepo so
// callers can diagnose mint/refresh health without hitting the k8s API
// directly. Populated only when spec.identityRepo.repo is set.
type agentIdentityRepoResponse struct {
	// Repo is the owner/name slug from spec.identityRepo.repo.
	Repo string `json:"repo"`
	// Phase is the observed state of the per-agent github token Secret:
	// "Pending" | "Ready" | "Failed". Empty when the reconciler hasn't yet
	// written status (e.g. first reconcile in flight).
	Phase string `json:"phase,omitempty"`
	// Message is a human-readable error when Phase=Failed. Empty otherwise.
	Message string `json:"message,omitempty"`
	// TokenExpiresAt is the GitHub-reported expiry of the most recently minted
	// installation token, RFC3339. Empty until the first mint succeeds.
	TokenExpiresAt string `json:"tokenExpiresAt,omitempty"`
	// LastMinted is when the reconciler last successfully wrote a fresh token
	// into the agent's github Secret, RFC3339. Empty until the first mint
	// succeeds.
	LastMinted string `json:"lastMinted,omitempty"`
}

type agentResourcesResponse struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
	Disk   string `json:"disk"`
}

type agentStatusResponse struct {
	Phase        kyberv1.AgentPhase `json:"phase,omitempty"`
	PodName      string             `json:"podName,omitempty"`
	PodIP        string             `json:"podIP,omitempty"`
	RestartCount int32              `json:"restartCount,omitempty"`
	Message      string             `json:"message,omitempty"`
}

// AgentListResponse wraps a list of agents.
type AgentListResponse struct {
	Items []AgentResponse `json:"items"`
}

// agentToResponse converts an Agent CRD to the API response shape.
func agentToResponse(a *kyberv1.Agent) AgentResponse {
	authType := a.Spec.Secrets.AuthType
	if authType == "" {
		// Legacy Agent CRs predate the AuthType field; their established default
		// is subscription OAuth. Keep the response inside its documented enum
		// while those persisted objects roll forward.
		authType = kyberv1.AgentAuthTypeOAuth
	}
	resp := AgentResponse{
		ID:            a.Name,
		Phase:         a.Status.Phase,
		Machine:       a.Spec.Machine,
		Runtime:       a.Spec.Runtime,
		AuthType:      authType,
		Model:         a.Spec.Model,
		StartupPrompt: a.Spec.StartupPrompt,
		SessionResume: a.Spec.SessionResume,
		CurrentModel:  a.Status.CurrentModel,
		Resources: agentResourcesResponse{
			CPU:    a.Spec.Resources.CPU.String(),
			Memory: a.Spec.Resources.Memory.String(),
			Disk:   a.Spec.Resources.Disk.String(),
		},
		Status: agentStatusResponse{
			Phase:        a.Status.Phase,
			PodName:      a.Status.PodName,
			PodIP:        a.Status.PodIP,
			RestartCount: a.Status.RestartCount,
			Message:      a.Status.Message,
		},
		// Dirty: spec has advanced beyond what the running pod was built
		// from, OR the running pod's kyber-status-sidecar is on an older
		// digest than the controller's current StatusSidecarImage
		// (kyber#299). Both conditions surface the same operator action:
		// "restart the pod to pick up the change."
		//
		// Generation 0 is the never-reconciled baseline — don't flag
		// freshly-created agents whose pod hasn't been built yet (the
		// reconciler stamps ObservedGeneration on first createPod, after
		// which the comparison is meaningful).
		Dirty: a.SpecChangedSinceLastPod() ||
			meta.IsStatusConditionTrue(a.Status.Conditions, kyberv1.AgentConditionSidecarOutOfDate),
		CreatedAt: a.CreationTimestamp.UTC().Format(time.RFC3339),
	}
	if a.Status.Runtime.InstalledVersion != "" {
		rv := &agentRuntimeVersionResponse{
			InstalledVersion: a.Status.Runtime.InstalledVersion,
			RequestedVersion: a.Status.Runtime.RequestedVersion,
		}
		if a.Status.Runtime.InstalledAt != nil {
			rv.InstalledAt = a.Status.Runtime.InstalledAt.UTC().Format(time.RFC3339)
		}
		// Copy via local pointers so mutating the source later (e.g.
		// in a subsequent reconcile) doesn't leak into a cached
		// response body.
		if a.Status.Runtime.RequestedSatisfied != nil {
			v := *a.Status.Runtime.RequestedSatisfied
			rv.RequestedSatisfied = &v
		}
		if a.Status.Runtime.ModelSupported != nil {
			v := *a.Status.Runtime.ModelSupported
			rv.ModelSupported = &v
		}
		rv.ModelProbeMessage = a.Status.Runtime.ModelProbeMessage
		resp.RuntimeVersion = rv
	}
	// PR-E badges: surface the two mismatch conditions as top-level
	// booleans so the PWA renders without traversing the conditions
	// array. Computed via meta.IsStatusConditionTrue so they stay True
	// exactly when the reconciler set the condition True (and clear
	// within one reconcile cycle once the underlying signal resolves).
	resp.RuntimeVersionMismatch = meta.IsStatusConditionTrue(a.Status.Conditions, kyberv1.AgentConditionRuntimeVersionMismatch)
	resp.ModelUnsupported = meta.IsStatusConditionTrue(a.Status.Conditions, kyberv1.AgentConditionModelUnsupported)
	// kyber#674: the two "controller refused to build a pod" conditions. Both
	// leave the agent with no pod and — before this — nothing on the wire, so
	// the PWA showed a blank agent with no way to reach the cause. Surfacing
	// them here is what makes the failure visible at all.
	resp.RuntimeImageMissing = meta.IsStatusConditionTrue(a.Status.Conditions, kyberv1.AgentConditionRuntimeImageMissing)
	resp.ModelUnresolved = meta.IsStatusConditionTrue(a.Status.Conditions, kyberv1.AgentConditionModelUnresolved)
	if c := meta.FindStatusCondition(a.Status.Conditions, kyberv1.AgentConditionRuntimeImageMissing); c != nil && c.Status == metav1.ConditionTrue {
		resp.BlockedReason = c.Message
	} else if c := meta.FindStatusCondition(a.Status.Conditions, kyberv1.AgentConditionModelUnresolved); c != nil && c.Status == metav1.ConditionTrue {
		resp.BlockedReason = c.Message
	} else if c := meta.FindStatusCondition(a.Status.Conditions, kyberv1.AgentConditionModelUnsupported); c != nil && c.Status == metav1.ConditionTrue {
		// An unsupported model doesn't block the pod, but it silently
		// fails every turn — which is worse than blocked, because the
		// agent looks healthy. Surface the probe's diagnosis wherever
		// the PWA shows blockedReason (canary regression 2026-08-22).
		resp.BlockedReason = c.Message
	}
	if a.Spec.IdentityRepo.Repo != "" {
		ir := &agentIdentityRepoResponse{
			Repo:    a.Spec.IdentityRepo.Repo,
			Phase:   string(a.Status.IdentityRepo.Phase),
			Message: a.Status.IdentityRepo.Message,
		}
		if a.Status.IdentityRepo.TokenExpiresAt != nil {
			ir.TokenExpiresAt = a.Status.IdentityRepo.TokenExpiresAt.UTC().Format(time.RFC3339)
		}
		if a.Status.IdentityRepo.LastMinted != nil {
			ir.LastMinted = a.Status.IdentityRepo.LastMinted.UTC().Format(time.RFC3339)
		}
		resp.IdentityRepo = ir
	}
	for _, j := range a.Spec.Jobs {
		resp.Jobs = append(resp.Jobs, agentJobResponse{
			Name:              j.Name,
			Schedule:          j.Schedule,
			Prompt:            j.Prompt,
			Exclusive:         j.Exclusive,
			ClearContextAfter: j.ClearContextAfter,
		})
	}
	// Collapse status.jobs[] to "last run per name" for the response — full
	// history is available via kubectl if operators need it. status.jobs is
	// oldest-first (per appendJobRunCapped), so the later entry wins.
	if len(a.Status.Jobs) > 0 {
		byName := map[string]kyberv1.AgentJobRun{}
		for _, r := range a.Status.Jobs {
			byName[r.JobName] = r
		}
		names := make([]string, 0, len(byName))
		for n := range byName {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			r := byName[n]
			entry := agentJobRunResponse{
				JobName: r.JobName,
				Outcome: string(r.Outcome),
				Error:   r.Error,
			}
			if r.StartedAt != nil {
				entry.StartedAt = r.StartedAt.UTC().Format(time.RFC3339)
			}
			if r.FinishedAt != nil {
				entry.FinishedAt = r.FinishedAt.UTC().Format(time.RFC3339)
			}
			resp.LastJobRuns = append(resp.LastJobRuns, entry)
		}
	}
	// Inbound runs ring buffer — preserved as-is (per-binding cap is enforced
	// at append time in appendInboundRunCapped). Newest-first is the operator's
	// preferred order in the PWA panel; status.inboundRuns is appended-as-it-
	// happens, so we reverse here once for the client.
	if n := len(a.Status.InboundRuns); n > 0 {
		resp.InboundRuns = make([]agentInboundRunResponse, 0, n)
		for i := n - 1; i >= 0; i-- {
			r := a.Status.InboundRuns[i]
			entry := agentInboundRunResponse{
				BindingName: r.BindingName,
				RequestID:   r.RequestID,
				Outcome:     r.Outcome,
				DropReason:  r.DropReason,
				Error:       r.Error,
			}
			if r.StartedAt != nil {
				entry.StartedAt = r.StartedAt.UTC().Format(time.RFC3339)
			}
			if r.FinishedAt != nil {
				entry.FinishedAt = r.FinishedAt.UTC().Format(time.RFC3339)
			}
			resp.InboundRuns = append(resp.InboundRuns, entry)
		}
	}
	if a.Status.Scheduling != nil {
		sch := &agentSchedulingStatusResponse{
			Category:  a.Status.Scheduling.Category,
			LastError: a.Status.Scheduling.LastError,
		}
		if a.Status.Scheduling.FirstObservedAt != nil {
			sch.FirstObservedAt = a.Status.Scheduling.FirstObservedAt.UTC().Format(time.RFC3339)
		}
		resp.Scheduling = sch
	}
	if a.Status.Activity != nil {
		act := &agentActivityStatusResponse{
			State: a.Status.Activity.State,
		}
		if a.Status.Activity.LastHeartbeatAt != nil {
			act.LastHeartbeatAt = a.Status.Activity.LastHeartbeatAt.UTC().Format(time.RFC3339)
		}
		if a.Status.Activity.LastActivityAt != nil {
			act.LastActivityAt = a.Status.Activity.LastActivityAt.UTC().Format(time.RFC3339)
		}
		resp.Activity = act
	}
	return resp
}

// handleAgents dispatches /api/v1/agents and /api/v1/agents/{name}[/action].
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	suffix, _ := trimPrefix(r.URL.Path, "/api/v1/agents")
	suffix = trimLeadingSlash(suffix)

	if suffix == "" {
		switch r.Method {
		case http.MethodPost:
			s.createAgent(w, r)
		case http.MethodGet:
			s.listAgents(w, r)
		default:
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		}
		return
	}

	name, action, ok := splitAction(suffix)
	if !ok || !isValidName(name) {
		writeJSONError(w, http.StatusBadRequest, "invalid_name", "invalid agent name")
		return
	}

	if action == "" {
		switch r.Method {
		case http.MethodGet:
			s.getAgent(w, r, name)
		case http.MethodPatch:
			s.patchAgent(w, r, name)
		case http.MethodDelete:
			s.deleteAgent(w, r, name)
		default:
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		}
		return
	}

	// C2 implementations: logs streaming and exec proxy.
	switch action {
	case "logs":
		s.handleAgentLogs(w, r, name)
		return
	case "exec":
		s.handleAgentExec(w, r, name)
		return
	case "token-usage":
		s.handleTokenUsageGet(w, r, name)
		return
	case "models":
		s.handleAgentModels(w, r, name)
		return
	case "skills":
		// Read-only by design: skills are managed by talking to the
		// agent, never from the API. See routes_agent_skills.go.
		s.handleAgentSkills(w, r, name)
		return
	}

	// User-defined per-agent secrets (#75): dispatch anything under "secrets".
	if action == "secrets" || strings.HasPrefix(action, "secrets/") {
		s.handleUserSecrets(w, r, name, action)
		return
	}

	// Jobs sub-tree (#135). "jobs/{jobName}/run" is the only action for now.
	if strings.HasPrefix(action, "jobs/") {
		s.handleAgentJobAction(w, r, name, strings.TrimPrefix(action, "jobs/"))
		return
	}

	// Per-agent comms sub-tree (#664). Configures Telegram and two-way Discord
	// on an existing agent — the surface behind the PWA's Comms tab.
	if action == "comms" || strings.HasPrefix(action, "comms/") {
		s.handleAgentComms(w, r, name, strings.TrimPrefix(strings.TrimPrefix(action, "comms"), "/"))
		return
	}

	// Inbound bindings sub-tree (#208 Phase 2). Manages the
	// AgentInboundBinding entries on spec.inboundBindings plus the
	// auto-generated HMAC secret backing each binding.
	if action == "inbound-bindings" || strings.HasPrefix(action, "inbound-bindings/") {
		s.handleAgentInboundBindings(w, r, name, strings.TrimPrefix(strings.TrimPrefix(action, "inbound-bindings"), "/"))
		return
	}

	// Codex device login is the one action with both a read and a write on the
	// same path: GET reports what the in-pod flow is showing (link, code,
	// expiry) so the PWA can render it natively; POST starts a fresh flow.
	// Intercepted ahead of the POST-only guard below.
	if action == "codex-device-auth" && r.Method == http.MethodGet {
		s.handleCodexDeviceAuthStatus(w, r, name)
		return
	}

	// All other sub-actions require POST.
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}

	switch action {
	case "start":
		s.setAgentDesiredPhase(w, r, name, kyberv1.AgentPhaseRunning)
	case "stop":
		s.setAgentDesiredPhase(w, r, name, kyberv1.AgentPhaseStopped)
	case "restart":
		s.setAgentDesiredPhase(w, r, name, kyberv1.AgentPhaseRestarting)
	case "force-needs-auth":
		// Operator-forced re-auth for a wedged agent (#395). Set
		// spec.desiredPhase; the controller's classifyEvent gate
		// honors it only from recoverable phases and (for live-pod phases)
		// deletes the pod. No new handler or auth surface.
		s.setAgentDesiredPhase(w, r, name, kyberv1.AgentPhaseNeedsAuth)
	case "set-model":
		s.setAgentModel(w, r, name)
	case "set-runtime-version":
		s.setAgentRuntimeVersion(w, r, name)
	case "set-resources":
		s.setAgentResources(w, r, name)
	case "oauth":
		s.handleReauthorize(w, r, name)
	case "codex-device-auth":
		s.handleCodexDeviceAuth(w, r, name)
	case "restart-session":
		s.handleRestartSession(w, r, name)
	case "compact-session":
		s.handleCompactSession(w, r, name)
	default:
		writeJSONError(w, http.StatusNotFound, "not_found", "unknown action")
	}
}

func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB
	var req CreateAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds 1MB")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Secrets.AuthType == "" {
		req.Secrets.AuthType = string(kyberv1.AgentAuthTypeOAuth)
	}
	if utf8.RuneCountInString(req.StartupPrompt) > 32768 {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "startupPrompt must be at most 32768 characters", "startupPrompt")
		return
	}

	if req.Name == "" {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "name is required", "name")
		return
	}
	if !isValidName(req.Name) {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"name must be lowercase alphanumeric + hyphens, 1-63 chars", "name")
		return
	}
	if req.Machine == "" {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "machine is required", "machine")
		return
	}
	// Verify the referenced machine actually exists so the agent doesn't get
	// stuck with an unresolvable node affinity.
	machineObj := &kyberv1.Machine{}
	machineKey := types.NamespacedName{Name: req.Machine, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), machineKey, machineObj); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
				"machine '"+req.Machine+"' does not exist", "machine")
			return
		}
		slog.Error("failed to get machine for agent validation", "machine", req.Machine, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to verify machine")
		return
	}
	if req.Runtime == "" {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "runtime is required", "runtime")
		return
	}
	if len(s.ValidRuntimes) > 0 && !s.ValidRuntimes[req.Runtime] {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"unknown runtime '"+req.Runtime+"'", "runtime")
		return
	}
	// Same catalog check + force escape set-model applies — an unknown
	// model id would otherwise fail every turn while the agent reports
	// healthy.
	if !req.Force {
		if msg := s.validateModelValue(r.Context(), req.Runtime, req.Model, ""); msg != "" {
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", msg, "model")
			return
		}
	}
	// kyber#674: registered is not the same as usable. A runtime whose image
	// was never pinned on this install (image.<runtime>.tag empty, so the
	// adapter's image env var is absent) would produce a pod with an empty
	// containers[0].image — rejected by the API server, retried forever, and
	// leaving the agent with a completely blank status. Refuse here instead,
	// and name the value to set. Deliberately NOT phrased as "unknown
	// runtime": it IS known, it just isn't configured, and conflating the two
	// sends the operator hunting in the wrong place.
	if s.RuntimeImages != nil {
		if image, registered := s.RuntimeImages[req.Runtime]; registered && image == "" {
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
				"runtime '"+req.Runtime+"' has no container image configured on this cluster — "+
					"pin image."+pkgruntimes.HelmImageKey(req.Runtime)+".tag in the install's Helm values, "+
					"then retry. No Kyber agent can run this runtime until then.", "runtime")
			return
		}
	}
	if req.Runtime == "codex" {
		switch kyberv1.AgentAuthType(req.Secrets.AuthType) {
		case kyberv1.AgentAuthTypeOAuth:
			// Empty is the normal device-auth path. A non-empty document remains
			// accepted for backward compatibility with older API clients.
			if req.Secrets.CodexAuthJSON != "" &&
				(len(req.Secrets.CodexAuthJSON) > 256*1024 || !json.Valid([]byte(req.Secrets.CodexAuthJSON))) {
				writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
					"Codex auth.json must be valid JSON no larger than 256 KiB", "secrets.codexAuthJson")
				return
			}
		case kyberv1.AgentAuthTypeAPIKey:
			if req.Secrets.OpenAIAPIKey == "" {
				writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
					"OpenAI API key is required for a Codex API-key agent", "secrets.openaiApiKey")
				return
			}
		default:
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
				"authType must be oauth or api-key", "secrets.authType")
			return
		}
	}
	// Telegram channels require Max-subscription OAuth. API-key auth cannot
	// support channels, so reject the combination upfront. Shares its rule with
	// PUT /comms/telegram (routes_agent_comms.go) so the two entry points into
	// "enable Telegram" cannot drift apart.
	if req.Secrets.TelegramEnabled {
		if err := validateTelegramAuth(kyberv1.AgentAuthType(req.Secrets.AuthType)); err != nil {
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error(), "telegramEnabled")
			return
		}
		if err := validateTelegramAllowedUsers(req.Secrets.TelegramAllowedUserIDs); err != nil {
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error(), "secrets.telegramAllowedUserIds")
			return
		}
	}

	// Parse resource quantities.
	cpuQ, err := resource.ParseQuantity(defaultString(req.Resources.CPU, "1"))
	if err != nil {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"resources.cpu must be a valid Kubernetes quantity", "resources.cpu")
		return
	}
	memQ, err := resource.ParseQuantity(defaultString(req.Resources.Memory, "2Gi"))
	if err != nil {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"resources.memory must be a valid Kubernetes quantity", "resources.memory")
		return
	}
	diskQ, err := resource.ParseQuantity(defaultString(req.Resources.Disk, "50Gi"))
	if err != nil {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"resources.disk must be a valid Kubernetes quantity", "resources.disk")
		return
	}

	// Capacity check: fetch all agents in the namespace, then compute what's
	// still free on the target Machine.
	var agentList kyberv1.AgentList
	if err := s.K8sClient.List(r.Context(), &agentList,
		client.InNamespace(s.Namespace)); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "INTERNAL",
			"list agents for capacity check: "+err.Error())
		return
	}
	avail := machineAvailable(machineObj, agentList.Items)
	if cpuQ.Cmp(avail.CPU) > 0 || memQ.Cmp(avail.Memory) > 0 {
		writeJSONError(w, http.StatusConflict, "INSUFFICIENT_CAPACITY",
			fmt.Sprintf(
				"insufficient capacity on machine %q: available {cpu=%s, memory=%s}, requested {cpu=%s, memory=%s}",
				machineObj.Name,
				avail.CPU.String(), avail.Memory.String(),
				cpuQ.String(), memQ.String(),
			),
		)
		return
	}

	authType := kyberv1.AgentAuthType(req.Secrets.AuthType)
	if authType == "" {
		authType = kyberv1.AgentAuthTypeOAuth
	}

	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: s.Namespace,
		},
		Spec: kyberv1.AgentSpec{
			Machine:       req.Machine,
			Runtime:       req.Runtime,
			Model:         req.Model,
			StartupPrompt: req.StartupPrompt,
			SessionResume: req.SessionResume,
			Resources: kyberv1.AgentResources{
				CPU:    cpuQ,
				Memory: memQ,
				Disk:   diskQ,
			},
			Identity: kyberv1.AgentIdentity{
				SoulDescription: req.Identity.SoulDescription,
			},
			IdentityRepo: kyberv1.AgentIdentityRepo{
				Repo:     req.IdentityRepo.Repo,
				Template: req.IdentityRepo.Template,
			},
			Secrets: kyberv1.AgentSecrets{
				TelegramEnabled: req.Secrets.TelegramEnabled,
				DiscordEnabled:  req.Secrets.DiscordEnabled,
				AuthType:        authType,
			},
			DesiredPhase: kyberv1.AgentPhaseRunning,
		},
	}
	if req.Secrets.TelegramEnabled {
		agent.Spec.InboundBindings = append(agent.Spec.InboundBindings,
			telegramInboundBinding(req.Name+telegramSecretSuffix, defaultTelegramAction()))
	}

	// Validate OAuth field combinations before attempting secret creation.
	if req.Secrets.OAuthCode != "" && req.Secrets.PkceVerifier == "" {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"pkceVerifier is required when oauthCode is provided",
			"secrets.pkceVerifier")
		return
	}
	if req.Secrets.OAuthCode != "" && req.Secrets.PkceState == "" {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"pkceState is required when oauthCode is provided",
			"secrets.pkceState")
		return
	}

	// Create k8s Secrets BEFORE the Agent CRD so that a failure (e.g. invalid
	// OAuth code) leaves no orphan agent. If secrets fail, any partially-created
	// secrets are rolled back so retries get a clean slate.
	createdSecrets, err := s.createAgentSecrets(r.Context(), req)
	if err != nil {
		var conflict *secretConflictError
		if errors.As(err, &conflict) {
			writeJSONError(w, http.StatusConflict, "conflict",
				"secret '"+conflict.name+"' already exists; a prior create may have failed mid-flow — delete it and retry")
			return
		}
		if oauth.IsInvalidGrant(err) {
			writeJSONError(w, http.StatusBadRequest, "oauth_exchange_failed",
				"authorization code invalid or expired")
			return
		}
		slog.Error("failed to create agent secrets", "agent", req.Name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error",
			"failed to create agent")
		return
	}

	if err := s.K8sClient.Create(r.Context(), agent); err != nil {
		s.rollbackSecrets(r.Context(), createdSecrets)
		if k8serrors.IsAlreadyExists(err) {
			writeJSONError(w, http.StatusConflict, "conflict", "agent '"+req.Name+"' already exists")
			return
		}
		slog.Error("failed to create agent", "agent", req.Name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to create agent: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, agentToResponse(agent))
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	list := &kyberv1.AgentList{}
	if err := s.K8sClient.List(r.Context(), list, client.InNamespace(s.Namespace)); err != nil {
		slog.Error("failed to list agents", "namespace", s.Namespace, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to list agents")
		return
	}

	items := make([]AgentResponse, 0, len(list.Items))
	for i := range list.Items {
		resp := agentToResponse(&list.Items[i])
		// Hydrate token usage from the in-process store so the overview can
		// render a Context column without per-row client fetches. A missing
		// or erroring store leaves the field nil — the UI renders "—".
		//
		// #500: resolve the context-window limit server-side here too — the
		// reporter stores a raw snapshot (limit/pct=0 sentinel), so without
		// this the overview gauge has no limit to render against. Uses the
		// SAME override→snapshot precedence as the dedicated /token-usage
		// endpoint and /available, so both serve sites agree. There is no
		// built-in table or floor tier any more: an unresolvable window
		// leaves TokenUsage nil here (the UI renders "—") and 503s on
		// /token-usage, rather than showing a guessed number.
		if s.TokenStore != nil {
			if snap, err := s.TokenStore.Get(r.Context(), resp.ID); err == nil && snap != nil {
				// Resolve on a COPY — TokenStore.Get returns the shared pointer
				// (see the /token-usage handler); mutating it in place would
				// race a concurrent reader. Shallow copy is safe (value fields).
				resolved := *snap
				limit, known := s.resolveContextWindow(r.Context(), resp.ID, resolved.Model)
				if !known && resolved.ContextWindowKnown && resolved.Tokens.Limit > 0 {
					limit, known = resolved.Tokens.Limit, true
				}
				if !known {
					items = append(items, resp)
					continue
				}
				resolved.Tokens.Limit = limit
				resolved.ContextWindowKnown = known
				if limit > 0 {
					resolved.Percentage = 100.0 * float64(resolved.Tokens.Used) / float64(limit)
				} else {
					resolved.Percentage = 0
				}
				resp.TokenUsage = &resolved
			}
		}
		items = append(items, resp)
	}
	// Sort by ID so the PWA cards/rows don't reshuffle on each refetch
	// (controller-runtime's cache returns Items in unspecified order). See #263.
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	writeJSON(w, http.StatusOK, AgentListResponse{Items: items})
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request, name string) {
	agent := &kyberv1.Agent{}
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent '"+name+"' not found")
			return
		}
		slog.Error("failed to get agent", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get agent")
		return
	}
	writeJSON(w, http.StatusOK, agentToResponse(agent))
}

func (s *Server) patchAgent(w http.ResponseWriter, r *http.Request, name string) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB
	var req PatchAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds 1MB")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}

	agent := &kyberv1.Agent{}
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent '"+name+"' not found")
			return
		}
		slog.Error("failed to get agent", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get agent")
		return
	}

	patch := client.MergeFrom(agent.DeepCopy())

	if req.StartupPrompt != nil {
		if utf8.RuneCountInString(*req.StartupPrompt) > 32768 {
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "startupPrompt must be at most 32768 characters", "startupPrompt")
			return
		}
		agent.Spec.StartupPrompt = *req.StartupPrompt
	}

	if req.SessionResume != nil {
		agent.Spec.SessionResume = *req.SessionResume
	}

	if req.Model != nil {
		if *req.Model == "" {
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "model must not be empty", "model")
			return
		}
		agent.Spec.Model = *req.Model
	}

	if req.Resources != nil {
		if req.Resources.CPU != "" {
			q, err := resource.ParseQuantity(req.Resources.CPU)
			if err != nil {
				writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
					"resources.cpu must be a valid Kubernetes quantity", "resources.cpu")
				return
			}
			agent.Spec.Resources.CPU = q
		}
		if req.Resources.Memory != "" {
			q, err := resource.ParseQuantity(req.Resources.Memory)
			if err != nil {
				writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
					"resources.memory must be a valid Kubernetes quantity", "resources.memory")
				return
			}
			agent.Spec.Resources.Memory = q
		}
		if req.Resources.Disk != "" {
			q, err := resource.ParseQuantity(req.Resources.Disk)
			if err != nil {
				writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
					"resources.disk must be a valid Kubernetes quantity", "resources.disk")
				return
			}
			agent.Spec.Resources.Disk = q
		}
	}

	if req.Jobs != nil {
		jobs, err := validateJobsRequest(*req.Jobs)
		if err != nil {
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error(), "jobs")
			return
		}
		agent.Spec.Jobs = jobs
	}

	if err := s.K8sClient.Patch(r.Context(), agent, patch); err != nil {
		slog.Error("failed to patch agent", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to update agent")
		return
	}

	writeJSON(w, http.StatusOK, agentToResponse(agent))
}

// jobNameRe matches the regex enforced by the CRD validator on AgentJob.Name.
// Kept in sync with the kubebuilder:validation:Pattern in agent_types.go.
var jobNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// cronFieldRe splits a 5-field cron expression. We don't attempt full
// schedule validation here — cron's behavior is permissive and the CRD
// already bounds length. This just rejects obviously-wrong bodies (empty,
// wrong field count) so operators get a fast error at PATCH time instead
// of a silent never-fires job.
func validateJobsRequest(reqs []AgentJobRequest) ([]kyberv1.AgentJob, error) {
	out := make([]kyberv1.AgentJob, 0, len(reqs))
	seen := map[string]bool{}
	for i, j := range reqs {
		if !jobNameRe.MatchString(j.Name) {
			return nil, fmt.Errorf("jobs[%d].name: must match lowercase DNS-1123 pattern", i)
		}
		if seen[j.Name] {
			return nil, fmt.Errorf("jobs[%d].name: duplicate %q", i, j.Name)
		}
		seen[j.Name] = true
		fields := strings.Fields(j.Schedule)
		if len(fields) != 5 {
			return nil, fmt.Errorf("jobs[%d].schedule: want 5-field cron expression (min hr dom mon dow), got %d field(s)", i, len(fields))
		}
		if j.Prompt == "" {
			return nil, fmt.Errorf("jobs[%d].prompt: must not be empty", i)
		}
		out = append(out, kyberv1.AgentJob{
			Name:              j.Name,
			Schedule:          j.Schedule,
			Prompt:            j.Prompt,
			Exclusive:         j.Exclusive,
			ClearContextAfter: j.ClearContextAfter,
		})
	}
	return out, nil
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request, name string) {
	// kyber#565 — two independent interlocks in front of the destructive
	// Get-then-Delete, both enforced before any k8s mutation:
	//
	//  1. Confirmation (always-on safety): ?confirm=<name> must equal the path
	//     name. This defeats the accidental/fat-fingered DELETE and is enforced
	//     regardless of authz mode. Missing/mismatched → 400, nothing deleted.
	//  2. Authorization (the #474 caller gate): DELETE is the single most
	//     impactful verb — irreversible identity destruction with backups still
	//     open (#171) — so it requires the strictly-highest scope,
	//     lifecycle:admin, via the same authorizeAction chokepoint the lifecycle
	//     verbs use. Permissive mode (default) audit-logs a would-deny but allows
	//     through; enforce mode returns a non-leaky 403.
	if r.URL.Query().Get("confirm") != name {
		writeJSONError(w, http.StatusBadRequest, "confirmation_required",
			"delete requires ?confirm=<name> matching the agent name")
		return
	}
	if !s.authorizeAction(w, r, name, "delete", ScopeLifecycleAdmin) {
		return
	}

	agent := &kyberv1.Agent{}
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent '"+name+"' not found")
			return
		}
		slog.Error("failed to get agent for deletion", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get agent")
		return
	}
	if err := s.K8sClient.Delete(r.Context(), agent); err != nil {
		if k8serrors.IsNotFound(err) {
			// NotFound is benign here — concurrent delete; treat as success.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		slog.Error("failed to delete agent", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to delete agent")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// rearmRecoveryGate clears status.recoveryInput so the controller grants a
// NeedsAuth or MemoryExhausted agent exactly one more recovery attempt
// (kyber#684). No-op in any other phase, and when no claim is recorded.
//
// Those two phases mean "a human must supply something", so the controller
// refuses to leave them on the standing desiredPhase==Running — that value is
// permanently true, and acting on it rebuilt a dead agent's pod every ~20s
// forever. It requires the operator-supplied input to have CHANGED, which it
// tracks as the credential Secret's resourceVersion in status.recoveryInput
// (pkg/controllers/agent/reconciler.go, currentRecoveryInput).
//
// Some operator actions satisfy that naturally — /set-resources patches
// spec.resources.memory, the Claude Code re-auth flow writes a genuinely new
// <name>-oauth Secret. Others do not, and MUST call this or they are silent
// no-ops:
//
//   - A bare Start writes no operator input at all.
//   - Codex device auth writes {} into <name>-codex-auth, which is already what
//     the Secret holds on every retry after the first. Kubernetes does not bump
//     resourceVersion for a byte-identical update, so the claim still matches
//     and the gate stays shut. That was the whole of the MAT-8 defect: the API
//     answered 204, the UI showed a success toast, and nothing happened —
//     permanently, for any agent that had failed a device login once.
//
// This buys exactly one attempt, not a loop: the controller re-claims the
// current input on the very next reconcile, so an agent that fails again holds
// in NeedsAuth until a human acts again. The automatic path is untouched —
// see TestRecoveryGate_NeedsAuth_HoldsWhenCredentialUnchanged.
func (s *Server) rearmRecoveryGate(ctx context.Context, agent *kyberv1.Agent) error {
	if agent.Status.RecoveryInput == "" {
		return nil
	}
	switch agent.Status.Phase {
	case kyberv1.AgentPhaseNeedsAuth, kyberv1.AgentPhaseMemoryExhausted:
	default:
		return nil
	}
	// Patch a COPY, never the caller's object. Status().Patch decodes the
	// server's response back into whatever object it is handed
	// (controller-runtime PatchSubResource -> Into(obj)), and that response
	// carries the stored spec — so patching `agent` in place silently reverts
	// any spec edits a caller has already staged on it, and the spec patch that
	// follows computes an empty diff and writes nothing. Callers whose merge
	// base is captured after this call are unaffected either way; this keeps the
	// helper safe to call from anywhere in a handler.
	statusOnly := agent.DeepCopy()
	patch := client.MergeFrom(statusOnly.DeepCopy())
	statusOnly.Status.RecoveryInput = ""
	if err := s.K8sClient.Status().Patch(ctx, statusOnly, patch); err != nil {
		return err
	}
	agent.Status.RecoveryInput = ""
	return nil
}

func (s *Server) setAgentDesiredPhase(w http.ResponseWriter, r *http.Request, name string, phase kyberv1.AgentPhase) {
	// Caller-level authorization chokepoint (kyber#474): every lifecycle verb
	// (start/stop/restart/force-needs-auth) funnels through here, so
	// enforcing scope at the setter cannot be bypassed by hitting a route
	// directly. Audit-logs the decision; in permissive mode (default) it allows
	// but logs a would-deny.
	if !s.authorizePhase(w, r, name, phase) {
		return
	}

	agent := &kyberv1.Agent{}
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent '"+name+"' not found")
			return
		}
		slog.Error("failed to get agent", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get agent")
		return
	}

	if phase == kyberv1.AgentPhaseRunning {
		if err := s.rearmRecoveryGate(r.Context(), agent); err != nil {
			slog.Error("failed to clear recovery input", "name", name, "error", err)
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to update agent")
			return
		}
	}

	// Re-arm the retry budget on an explicit Start of a Failed agent — the
	// same shape as the NeedsAuth/MemoryExhausted block above, for the same
	// reason. The controller no longer leaves Failed on the standing
	// desiredPhase==Running (it would never reach maxRestartRetries, so a
	// crash-looping agent rebuilt its pod forever). A Start on an agent whose
	// desiredPhase is ALREADY Running writes no spec change at all, so nothing
	// downstream would notice it; clearing restartCount is what keeps the
	// button working. It buys one fresh budget of maxRestartRetries attempts,
	// not a loop: if the agent keeps crashing it lands back in Failed and holds
	// there until a human acts again.
	if phase == kyberv1.AgentPhaseRunning &&
		agent.Status.Phase == kyberv1.AgentPhaseFailed &&
		agent.Status.RestartCount > 0 {
		statusPatch := client.MergeFrom(agent.DeepCopy())
		agent.Status.RestartCount = 0
		if err := s.K8sClient.Status().Patch(r.Context(), agent, statusPatch); err != nil {
			slog.Error("failed to clear restart count", "name", name, "error", err)
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to update agent")
			return
		}
	}

	patch := client.MergeFrom(agent.DeepCopy())
	agent.Spec.DesiredPhase = phase
	if err := s.K8sClient.Patch(r.Context(), agent, patch); err != nil {
		slog.Error("failed to patch agent desired phase", "name", name, "phase", phase, "error", err)
		// A schema rejection is a bug in this server (a lifecycle verb writing a
		// phase the CRD enum does not permit), not a transient fault — surface the
		// API server's reason instead of a blanket 500. force-needs-auth spent its
		// whole life returning `500 failed to update agent` because NeedsAuth was
		// missing from the enum; that message named neither the field nor the
		// value, so the cause was invisible without reading the CRD. 400 is also
		// the honest code: retrying cannot help.
		if k8serrors.IsInvalid(err) {
			writeJSONErrorWithField(w, http.StatusBadRequest, "invalid_desired_phase", err.Error(), "desiredPhase")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to update agent")
		return
	}

	writeJSON(w, http.StatusOK, agentToResponse(agent))
}

func (s *Server) setAgentModel(w http.ResponseWriter, r *http.Request, name string) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB
	var req SetModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds 1MB")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Model == "" {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "model is required", "model")
		return
	}

	agent := &kyberv1.Agent{}
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent '"+name+"' not found")
			return
		}
		slog.Error("failed to get agent", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get agent")
		return
	}

	// An UNCHANGED model is not re-validated — re-posting the current
	// value (to trigger a roll, or from a UI resubmit) must not start
	// failing because the catalog view shifted since it was set.
	if !req.Force && req.Model != agent.Spec.Model {
		if msg := s.validateModelValue(r.Context(), agent.Spec.Runtime, req.Model, name); msg != "" {
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", msg, "model")
			return
		}
	}

	patch := client.MergeFrom(agent.DeepCopy())
	agent.Spec.Model = req.Model
	// Only roll the pod for actively-running phases; leave Stopped alone
	// so a model change doesn't start a dormant agent.
	switch agent.Status.Phase {
	case kyberv1.AgentPhaseRunning, kyberv1.AgentPhaseStarting, kyberv1.AgentPhaseRestarting:
		agent.Spec.DesiredPhase = kyberv1.AgentPhaseRestarting
	case kyberv1.AgentPhaseFailed:
		agent.Spec.DesiredPhase = kyberv1.AgentPhaseRunning
	}
	if err := s.K8sClient.Patch(r.Context(), agent, patch); err != nil {
		slog.Error("failed to patch agent model", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to update agent")
		return
	}

	writeJSON(w, http.StatusOK, agentToResponse(agent))
}

// runtimeVersionPattern mirrors the kubebuilder:validation:Pattern on
// kyberv1.AgentSpec.RuntimeVersion (PR-C). Re-checking here keeps the
// API surface honest: a malformed value gets a 400 with a clear field,
// rather than a generic CRD admission rejection downstream.
var runtimeVersionPattern = regexp.MustCompile(`^[0-9A-Za-z.\-]+$`)

// setAgentRuntimeVersion sets agent.Spec.RuntimeVersion and (when the agent
// is actively running) flips DesiredPhase to Restarting so the new value
// takes effect on the next pod boot. Empty body clears the field —
// reverts to the fleet default per PR-B resolution. (kyber#378 PR-D)
func (s *Server) setAgentRuntimeVersion(w http.ResponseWriter, r *http.Request, name string) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req SetRuntimeVersionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds 1MB")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if len(req.RuntimeVersion) > 64 {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"runtimeVersion must be 64 characters or fewer", "runtimeVersion")
		return
	}
	if req.RuntimeVersion != "" && !runtimeVersionPattern.MatchString(req.RuntimeVersion) {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			`runtimeVersion must match ^[0-9A-Za-z.\-]+$`, "runtimeVersion")
		return
	}

	agent := &kyberv1.Agent{}
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent '"+name+"' not found")
			return
		}
		slog.Error("failed to get agent", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get agent")
		return
	}

	patch := client.MergeFrom(agent.DeepCopy())
	agent.Spec.RuntimeVersion = req.RuntimeVersion
	// Mirror setAgentModel: only roll the pod for actively-running phases.
	// Stopped agents keep their disk state; the new version takes
	// effect on the next manual start.
	switch agent.Status.Phase {
	case kyberv1.AgentPhaseRunning, kyberv1.AgentPhaseStarting, kyberv1.AgentPhaseRestarting:
		agent.Spec.DesiredPhase = kyberv1.AgentPhaseRestarting
	case kyberv1.AgentPhaseFailed:
		agent.Spec.DesiredPhase = kyberv1.AgentPhaseRunning
	}
	if err := s.K8sClient.Patch(r.Context(), agent, patch); err != nil {
		slog.Error("failed to patch agent runtimeVersion", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to update agent")
		return
	}
	writeJSON(w, http.StatusOK, agentToResponse(agent))
}

func (s *Server) setAgentResources(w http.ResponseWriter, r *http.Request, name string) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req SetResourcesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds 1MB")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.CPU == "" && req.Memory == "" {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"at least one of cpu or memory must be provided", "resources")
		return
	}

	agent := &kyberv1.Agent{}
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "not_found", "agent '"+name+"' not found")
			return
		}
		slog.Error("failed to get agent", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to get agent")
		return
	}

	patch := client.MergeFrom(agent.DeepCopy())

	if req.CPU != "" {
		q, err := resource.ParseQuantity(req.CPU)
		if err != nil {
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
				"cpu must be a valid Kubernetes quantity", "cpu")
			return
		}
		agent.Spec.Resources.CPU = q
	}
	if req.Memory != "" {
		q, err := resource.ParseQuantity(req.Memory)
		if err != nil {
			writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
				"memory must be a valid Kubernetes quantity", "memory")
			return
		}
		agent.Spec.Resources.Memory = q
	}

	if req.CPU != "" && agent.Spec.Resources.CPU.Cmp(setResourcesMinCPU) < 0 {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"cpu is below the minimum of "+setResourcesMinCPU.String(), "cpu")
		return
	}
	if req.Memory != "" && agent.Spec.Resources.Memory.Cmp(setResourcesMinMemory) < 0 {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"memory is below the minimum of "+setResourcesMinMemory.String(), "memory")
		return
	}

	// Capacity check: available = machine capacity - all agents' allocations,
	// EXCLUDING this agent's own current allocation (which is about to be
	// overwritten by the patch below).
	machineObj := &kyberv1.Machine{}
	machineKey := types.NamespacedName{Name: agent.Spec.Machine, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), machineKey, machineObj); err != nil {
		if k8serrors.IsNotFound(err) {
			writeJSONError(w, http.StatusConflict, "INSUFFICIENT_CAPACITY",
				fmt.Sprintf("machine %q not found; cannot verify capacity", agent.Spec.Machine))
			return
		}
		slog.Error("failed to get machine for capacity check", "machine", agent.Spec.Machine, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to verify capacity")
		return
	}
	var agentList kyberv1.AgentList
	if err := s.K8sClient.List(r.Context(), &agentList, client.InNamespace(s.Namespace)); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "list agents for capacity check: "+err.Error())
		return
	}
	avail := MachineAvailableExcluding(machineObj, agentList.Items, agent.Name)
	if agent.Spec.Resources.CPU.Cmp(avail.CPU) > 0 || agent.Spec.Resources.Memory.Cmp(avail.Memory) > 0 {
		writeJSONError(w, http.StatusConflict, "INSUFFICIENT_CAPACITY",
			fmt.Sprintf("insufficient capacity on machine %q: available {cpu=%s, memory=%s}, requested {cpu=%s, memory=%s}",
				machineObj.Name,
				avail.CPU.String(), avail.Memory.String(),
				agent.Spec.Resources.CPU.String(), agent.Spec.Resources.Memory.String(),
			),
		)
		return
	}

	// Trigger a pod roll so the new limits land on the next pod.
	// Failed/MemoryExhausted→Running uses ResetRetry (the human-required
	// phases need explicit DesiredPhase=Running to fire their state-machine
	// recovery transitions, per kyber#272). Running→Restarting uses the
	// normal roll.
	switch agent.Status.Phase {
	case kyberv1.AgentPhaseFailed, kyberv1.AgentPhaseMemoryExhausted:
		agent.Spec.DesiredPhase = kyberv1.AgentPhaseRunning
	default:
		agent.Spec.DesiredPhase = kyberv1.AgentPhaseRestarting
	}

	// Re-arm the recovery gate (kyber#684), same reasoning as /start. A memory
	// bump changes the recorded input on its own, but a CPU-only change on a
	// MemoryExhausted agent does not — and that is still an explicit operator
	// action that must not be swallowed. Clearing here buys exactly one attempt.
	// Harmless on Failed, which is not gated.
	// The MemoryExhausted guard is this handler's own: rearmRecoveryGate also
	// covers NeedsAuth, and a NeedsAuth agent leaves here on desiredPhase
	// Restarting, which the gate deliberately does not honour.
	if agent.Status.Phase == kyberv1.AgentPhaseMemoryExhausted {
		if err := s.rearmRecoveryGate(r.Context(), agent); err != nil {
			slog.Error("failed to clear recovery input", "name", name, "error", err)
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to update agent")
			return
		}
	}

	if err := s.K8sClient.Patch(r.Context(), agent, patch); err != nil {
		slog.Error("failed to patch agent resources", "name", name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to update agent")
		return
	}

	writeJSON(w, http.StatusOK, agentToResponse(agent))
}

// defaultString returns s if non-empty, otherwise fallback.
func defaultString(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

// createAgentSecrets creates k8s Secrets for any token values provided in the
// agent creation request. The RuntimeAdapter (e.g. ClaudeCodeAdapter) references
// these secrets via valueFrom.SecretKeyRef — if the secret doesn't exist, the
// pod enters CreateContainerConfigError.
//
// Secret naming convention (matches ClaudeCodeAdapter):
//
//	<agent-name>-oauth       keys "access_token","refresh_token","expires_at" — PKCE OAuth exchange
//	<agent-name>-anthropic   key "token"  — Anthropic API key
//	<agent-name>-codex-auth  key "auth.json" — Codex ChatGPT auth ({} starts device auth)
//	<agent-name>-openai      key "token" — OpenAI API key
//	<agent-name>-telegram    key "token"  — Telegram bot token
//
// secretConflictError signals that a Secret matching the agent's naming
// convention already existed before this request — usually the residue of a
// prior create that failed mid-flow. The handler maps this to HTTP 409 rather
// than silently overwriting stored tokens.
type secretConflictError struct {
	name string
}

func (e *secretConflictError) Error() string {
	return "secret " + e.name + " already exists"
}

// rollbackSecrets best-effort deletes secrets created earlier in the request
// so a later failure doesn't leave orphans for the next retry to trip over.
// Uses a detached context so a client disconnect doesn't abandon the cleanup
// mid-delete — that's precisely the condition this function exists to prevent.
func (s *Server) rollbackSecrets(_ context.Context, names []string) {
	rbCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, name := range names {
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace}}
		if err := s.K8sClient.Delete(rbCtx, sec); err != nil && !k8serrors.IsNotFound(err) {
			slog.Warn("secret rollback failed", "secret", name, "error", err)
		}
	}
}

func (s *Server) createAgentSecrets(ctx context.Context, req CreateAgentRequest) ([]string, error) {
	type secretDef struct {
		suffix string
		value  string            // used when data is nil (single-key "token", e.g. api-key, telegram)
		data   map[string][]byte // used when non-nil (multi-key, e.g. access_token+refresh_token)
	}
	defs := []secretDef{}
	if req.Runtime == "codex" && kyberv1.AgentAuthType(req.Secrets.AuthType) == kyberv1.AgentAuthTypeOAuth {
		authJSON := req.Secrets.CodexAuthJSON
		if authJSON == "" {
			authJSON = "{}"
		}
		defs = append(defs, secretDef{suffix: "codex-auth", data: map[string][]byte{
			"auth.json": []byte(authJSON),
		}})
	}

	switch kyberv1.AgentAuthType(req.Secrets.AuthType) {
	case kyberv1.AgentAuthTypeOAuth:
		if req.Runtime != "codex" && req.Secrets.OAuthCode != "" && req.Secrets.PkceVerifier != "" {
			tok, err := oauth.NewClient(s.anthropicTokenURL()).
				ExchangeAuthorizationCode(ctx, req.Secrets.OAuthCode, req.Secrets.PkceVerifier, req.Secrets.PkceState)
			if err != nil {
				return nil, fmt.Errorf("oauth exchange: %w", err)
			}
			expiresAtMs := time.Now().UnixMilli() + int64(tok.ExpiresIn)*1000
			defs = append(defs, secretDef{
				suffix: "oauth",
				data: map[string][]byte{
					"access_token":  []byte(tok.AccessToken),
					"refresh_token": []byte(tok.RefreshToken),
					"expires_at":    []byte(strconv.FormatInt(expiresAtMs, 10)),
				},
			})
		}
	case kyberv1.AgentAuthTypeAPIKey:
		if req.Runtime == "codex" && req.Secrets.OpenAIAPIKey != "" {
			defs = append(defs, secretDef{suffix: "openai", value: req.Secrets.OpenAIAPIKey})
		} else if req.Secrets.AnthropicAPIKey != "" {
			defs = append(defs, secretDef{suffix: "anthropic", value: req.Secrets.AnthropicAPIKey})
		}
	}
	if req.Secrets.TelegramEnabled && req.Secrets.TelegramBotToken != "" {
		hmacSecret, err := generateCommsHMACSecret()
		if err != nil {
			return nil, err
		}
		defs = append(defs, secretDef{suffix: "telegram", data: map[string][]byte{
			telegramTokenKey:          []byte(req.Secrets.TelegramBotToken),
			telegramAllowedUserIDsKey: []byte(strings.Join(req.Secrets.TelegramAllowedUserIDs, ",")),
			webhookSecretKey:          []byte(hmacSecret),
		}})
	}
	// kyber#132 Phase 1 — Discord webhook for outbound notifications. The
	// adapter reads this Secret as KYBER_DISCORD_WEBHOOK; the agent shells
	// out to curl against it for one-way "build finished / deploy
	// complete" pings into a Discord channel.
	if req.Secrets.DiscordEnabled && req.Secrets.DiscordWebhookUrl != "" {
		defs = append(defs, secretDef{
			suffix: "discord",
			data:   map[string][]byte{"webhook-url": []byte(req.Secrets.DiscordWebhookUrl)},
		})
	}

	// NOTE: Telegram's public setWebhook registration is deliberately never
	// done. The runtime-neutral sidecar uses getUpdates (long-polling) from
	// inside the running agent pod; registering a public webhook would
	// disable polling for that bot.

	created := make([]string, 0, len(defs))
	for _, d := range defs {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      req.Name + "-" + d.suffix,
				Namespace: s.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "kyber-api",
					"kyber.io/agent":               req.Name,
				},
			},
			Type: corev1.SecretTypeOpaque,
		}
		if d.data != nil {
			secret.Data = d.data
		} else {
			secret.StringData = map[string]string{"token": d.value}
		}
		if err := s.K8sClient.Create(ctx, secret); err != nil {
			s.rollbackSecrets(ctx, created)
			if k8serrors.IsAlreadyExists(err) {
				// An orphan from a prior failed create. Don't silently overwrite
				// — caller must decide (delete orphan, pick new name, etc.).
				return nil, &secretConflictError{name: secret.Name}
			}
			return nil, fmt.Errorf("creating secret %s: %w", secret.Name, err)
		}
		created = append(created, secret.Name)
	}
	return created, nil
}
