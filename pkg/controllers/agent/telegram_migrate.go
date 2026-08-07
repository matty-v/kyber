package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// Telegram convergence migration (kyber#684).
//
// Before the convergence, Telegram was provisioned two different ways depending
// on the runtime. A Codex agent got the full inbound rail: an HMAC secret, an
// allowed-user list, and a signed inbound binding pointing at the sidecar. A
// Claude Code agent got a Secret containing ONLY the bot token, because the
// in-process plugin did its own polling, its own allowlisting (from
// ~/.claude/channels/telegram/access.json on the PVC) and its own replying. It
// never needed the platform's inbound rail at all.
//
// The convergence removes the plugin, so those agents now need the rail they
// never had. Without this migration an already-configured Claude Code agent
// would come back from its next restart with a sidecar that has no HMAC secret
// to sign with, no allowlist (so every inbound message is dropped), and no
// binding for the control plane to route to — silently deaf, with a healthy
// green pod. That is the shape of failure this platform has eaten before
// (#678/#679), and the reason it gets fixed at reconcile time rather than
// documented as an operator step.
//
// Same pattern and the same place in the reconcile loop as
// migrateLegacyDiscordAction, which exists because Discord's bot token moved
// into a sidecar and already-wired agents needed healing.

// telegramHMACRandomBytes matches the comms API's generator so a healed Secret
// is indistinguishable from a freshly-configured one.
const telegramHMACRandomBytes = 32

func generateTelegramHMACSecret() (string, error) {
	buf := make([]byte, telegramHMACRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating telegram HMAC secret: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// TelegramWiring is what the migration observed about an agent's Telegram
// setup, so the caller can raise a condition instead of leaving the operator to
// infer it from a crash-looping sidecar.
type TelegramWiring struct {
	// SecretExists is false when the agent has telegramEnabled but no
	// "<name>-telegram" Secret at all.
	SecretExists bool
	// HasAllowlist is false when nothing tells the sidecar who may message this
	// agent. The sidecar refuses to start in that state — deliberately, since
	// the alternative is a bot that answers strangers — so this is the
	// difference between a working channel and a crash-looping container.
	HasAllowlist bool
}

// migrateLegacyTelegramSecret backfills the inbound rail for an agent that had
// Telegram configured under the pre-#684 plugin path.
//
// Idempotent and additive: it only ever fills in what is absent, and returns
// immediately once an agent is fully wired, so it costs a map lookup on the
// steady-state reconcile. It never overwrites an operator's values.
//
// Best-effort by design. A failure here means Telegram stays broken; failing
// the reconcile would mean the agent stays DOWN. The channel is not worth the
// agent.
func (r *AgentReconciler) migrateLegacyTelegramSecret(ctx context.Context, agent *kyberv1.Agent) TelegramWiring {
	if !agent.Spec.Secrets.TelegramEnabled {
		return TelegramWiring{SecretExists: true, HasAllowlist: true}
	}
	logger := log.FromContext(ctx)
	secretName := agent.Name + "-telegram"

	var secret corev1.Secret
	key := types.NamespacedName{Namespace: agent.Namespace, Name: secretName}
	if err := r.Get(ctx, key, &secret); err != nil {
		if !errors.IsNotFound(err) {
			logger.Info("telegram migration: reading the agent's Secret (best-effort)",
				"agent", agent.Name, "secret", secretName, "err", err)
			// Couldn't read it — report no opinion rather than a false alarm.
			return TelegramWiring{SecretExists: true, HasAllowlist: true}
		}
		// No Secret means Telegram was never actually configured, whatever the
		// spec flag says. Nothing to migrate, and inventing a bot token is not
		// something this can do.
		return TelegramWiring{}
	}

	wiring := TelegramWiring{
		SecretExists: true,
		HasAllowlist: len(secret.Data[TelegramAllowedUserIDsKey]) > 0,
	}
	missing := map[string][]byte{}

	// The HMAC secret the sidecar signs inbound POSTs with. Generated rather
	// than requested from the operator: it is a shared secret between two
	// components we own, so there is nothing for a human to decide.
	if len(secret.Data[TelegramWebhookSecretKey]) == 0 {
		hmacSecret, err := generateTelegramHMACSecret()
		if err != nil {
			logger.Info("telegram migration: generating the HMAC secret (best-effort)", "agent", agent.Name, "err", err)
			return wiring
		}
		missing[TelegramWebhookSecretKey] = []byte(hmacSecret)
	}

	// The allowlist. This IS a human decision — it names who may talk to the
	// agent — and it previously lived on the agent's PVC in the plugin's
	// access.json, where the control plane cannot read it. The install-level
	// default carries it across. Where that is unset there is nothing to seed
	// from, so the key stays absent and the caller raises a condition naming the
	// fix; guessing an allowlist would mean guessing who is allowed to command
	// this agent, which is not a guess worth making.
	if !wiring.HasAllowlist && r.TelegramDefaultAllowedUserIDs != "" {
		missing[TelegramAllowedUserIDsKey] = []byte(r.TelegramDefaultAllowedUserIDs)
		wiring.HasAllowlist = true
	}

	if len(missing) > 0 {
		patch := client.MergeFrom(secret.DeepCopy())
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		names := make([]string, 0, len(missing))
		for k, v := range missing {
			secret.Data[k] = v
			names = append(names, k)
		}
		if err := r.Patch(ctx, &secret, patch); err != nil {
			logger.Info("telegram migration: patching the agent's Secret (best-effort)",
				"agent", agent.Name, "secret", secretName, "err", err)
			return wiring
		}
		logger.Info("telegram migration: backfilled the sidecar's Secret keys — this agent was configured "+
			"under the retired in-process plugin, which stored only the bot token",
			"agent", agent.Name, "secret", secretName, "keys", strings.Join(names, ","))
	}

	// The inbound binding. Without it the sidecar POSTs into the void: the
	// control plane has no route for this agent and the message is rejected.
	// Bindings created before attachment support also omitted the file metadata,
	// leaving the model no file_id to pass to download_attachment. Add any
	// missing canonical fields while preserving operator-customized fields and
	// action text.
	for i := range agent.Spec.InboundBindings {
		binding := &agent.Spec.InboundBindings[i]
		if binding.Name != TelegramInboundBindingName {
			continue
		}
		desired := TelegramInboundBinding(secretName, DefaultTelegramAction())
		patch := client.MergeFrom(agent.DeepCopy())
		changed := false
		if binding.Action == legacyTelegramAction() || binding.Action == attachmentTelegramAction() || binding.Action == interactiveTelegramAction() {
			binding.Action = desired.Action
			changed = true
		}
		for _, field := range desired.Fields {
			if telegramBindingHasField(binding.Fields, field) {
				continue
			}
			binding.Fields = append(binding.Fields, field)
			changed = true
		}
		if !changed {
			return wiring
		}
		if err := r.Patch(ctx, agent, patch); err != nil {
			logger.Info("telegram migration: patching attachment fields on the inbound binding (best-effort)",
				"agent", agent.Name, "err", err)
			return wiring
		}
		logger.Info("telegram migration: added attachment fields to the inbound binding",
			"agent", agent.Name, "binding", TelegramInboundBindingName)
		return wiring
	}
	patch := client.MergeFrom(agent.DeepCopy())
	agent.Spec.InboundBindings = append(agent.Spec.InboundBindings,
		TelegramInboundBinding(secretName, DefaultTelegramAction()))
	if err := r.Patch(ctx, agent, patch); err != nil {
		logger.Info("telegram migration: adding the inbound binding (best-effort)", "agent", agent.Name, "err", err)
		return wiring
	}
	logger.Info("telegram migration: added the Telegram inbound binding — the retired plugin polled in-process "+
		"and never used the platform's inbound rail, so this agent had none",
		"agent", agent.Name, "binding", TelegramInboundBindingName)
	return wiring
}

func telegramBindingHasField(fields []kyberv1.AgentInboundField, want kyberv1.AgentInboundField) bool {
	for _, field := range fields {
		if field.Label == want.Label && field.JsonPath == want.JsonPath {
			return true
		}
	}
	return false
}
