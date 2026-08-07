package api

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/messagebuffer"
)

// TelegramWebhookSecretHeader is the header Telegram sends when a webhook secret is configured.
// See: https://core.telegram.org/bots/api#setwebhook
const TelegramWebhookSecretHeader = "X-Telegram-Bot-Api-Secret-Token"

// handleTelegramWebhook handles POST /webhooks/telegram/{agent-name}.
//
// Flow:
//  1. Validate the webhook secret header — fail closed (reject) when no secret is
//     configured on the server, so a default install can't accept unauthenticated
//     traffic (kyber#564).
//  2. Extract the agent name from the path.
//  3. Look up the Agent CRD.
//  4. Buffer the message in MessageBuffer.
//  5. If the agent is Suspended, patch spec.desiredPhase = Running to wake it.
//  6. Return 200 in all non-fatal cases to prevent Telegram retries.
func (s *Server) handleTelegramWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// Telegram only uses POST — return 405 for other methods.
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}

	// Validate the webhook secret. The secret is ALWAYS validated: an unset/empty
	// secret fails closed (kyber#564) — the routes reject all traffic rather than
	// treating "no secret" as "auth disabled". Both reject paths reuse the
	// drop-with-200 response so Telegram does not retry-storm.
	got := r.Header.Get(TelegramWebhookSecretHeader)
	if s.WebhookSecret == "" {
		// Fail closed: no secret configured → reject before any side effect
		// (no buffer, no patch, no wake). Log the condition, never a secret value.
		slog.Warn("telegram webhook rejected: no webhook secret configured (fail-closed, kyber#564)",
			"path", r.URL.Path,
		)
		w.WriteHeader(http.StatusOK)
		return
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.WebhookSecret)) != 1 {
		// Always return 200 to Telegram even on auth failures — avoids retry storms.
		// The mismatch is logged for debugging.
		slog.Warn("telegram webhook secret mismatch",
			"path", r.URL.Path,
			"has_header", got != "",
		)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Extract agent name from path: /webhooks/telegram/{agent-name}
	suffix, _ := trimPrefix(r.URL.Path, "/webhooks/telegram/")
	agentName := trimLeadingSlash(suffix)
	if agentName == "" {
		// Path was just /webhooks/telegram or /webhooks/telegram/ — not a valid agent.
		w.WriteHeader(http.StatusOK)
		return
	}

	// Read the body — Telegram sends the update as JSON.
	// Cap at 1 MB to prevent abuse.
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		slog.Error("telegram webhook: reading body", "agent", agentName, "error", err)
		w.WriteHeader(http.StatusOK) // Don't cause Telegram retries.
		return
	}

	// Extract enough for a useful pending message (best-effort — raw body on failure).
	text := extractTelegramText(body)
	from := extractTelegramFrom(body)

	// Look up the Agent CRD.
	agent := &kyberv1.Agent{}
	key := types.NamespacedName{Name: agentName, Namespace: s.Namespace}
	if err := s.K8sClient.Get(r.Context(), key, agent); err != nil {
		if k8serrors.IsNotFound(err) {
			slog.Warn("telegram webhook: agent not found", "agent", agentName)
		} else {
			slog.Error("telegram webhook: getting agent", "agent", agentName, "error", err)
		}
		// Always return 200 to Telegram.
		w.WriteHeader(http.StatusOK)
		return
	}

	// Buffer the message regardless of current phase.
	if s.MessageBuffer != nil {
		msg := messagebuffer.PendingMessage{
			Source:    "telegram",
			From:      from,
			Text:      text,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		}
		if pushErr := s.MessageBuffer.Push(r.Context(), agentName, msg); pushErr != nil {
			slog.Error("telegram webhook: buffering message", "agent", agentName, "error", pushErr)
			// Non-fatal — continue with wake logic.
		}
	}

	// If the agent is Suspended, wake it by patching desiredPhase = Running.
	// The controller will drain the message buffer and include pending messages in the brief.
	//
	// kyber#474: this desiredPhase=Running wake is intentionally EXEMPT from the
	// caller-level scope check applied to the API lifecycle verbs. Webhook routes
	// bypass the Bearer API-key wall entirely and are gated by their own
	// per-binding secret-header validation (X-Telegram-Bot-Api-Secret-Token),
	// so there is no API Caller in context to authorize; the binding secret IS
	// the authorization. Adding a scope check here would have nothing to check.
	if agent.Status.Phase == kyberv1.AgentPhaseSuspended {
		patch := client.MergeFrom(agent.DeepCopy())
		agent.Spec.DesiredPhase = kyberv1.AgentPhaseRunning
		if patchErr := s.K8sClient.Patch(r.Context(), agent, patch); patchErr != nil {
			slog.Error("telegram webhook: patching desiredPhase", "agent", agentName, "error", patchErr)
			// Non-fatal — message is already buffered.
		} else {
			slog.Info("telegram webhook: waking suspended agent", "agent", agentName)
		}
	}

	// Always 200 so Telegram stops retrying.
	w.WriteHeader(http.StatusOK)
}

// extractTelegramText extracts the message text from a Telegram Update JSON payload.
// Returns an empty string if the payload cannot be parsed or has no text.
func extractTelegramText(body []byte) string {
	var update struct {
		Message struct {
			Text string `json:"text"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &update); err != nil {
		return string(body) // fall back to raw body for traceability
	}
	return update.Message.Text
}

// extractTelegramFrom extracts the sender identifier from a Telegram Update JSON payload.
// Returns an empty string if the payload cannot be parsed.
func extractTelegramFrom(body []byte) string {
	var update struct {
		Message struct {
			From struct {
				ID       int64  `json:"id"`
				Username string `json:"username"`
			} `json:"from"`
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &update); err != nil {
		return ""
	}
	if update.Message.From.Username != "" {
		return "@" + update.Message.From.Username
	}
	if update.Message.From.ID != 0 {
		return fmt.Sprintf("%d", update.Message.From.ID)
	}
	if update.Message.Chat.ID != 0 {
		return fmt.Sprintf("chat:%d", update.Message.Chat.ID)
	}
	return ""
}
