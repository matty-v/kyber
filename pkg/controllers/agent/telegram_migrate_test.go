package agent

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	pkgruntimes "github.com/matty-v/kyber/pkg/runtimes"
)

// kyber#684 — migrating an agent off the retired in-process Telegram plugin.
//
// This is the defect that would have taken down BOTH live Claude Code agents on
// a production cluster. Verified against the real cluster before it was written:
// dave-telegram, r2-d2-telegram and alice-telegram each held exactly one key
// ("token"), and neither dave nor r2-d2 had any spec.inboundBindings at all.
// The plugin polled, allowlisted and replied entirely in-process, so the
// platform's inbound rail was never provisioned for them.
//
// Un-gating the sidecar without this migration would have given those agents a
// container that cannot sign, cannot allowlist, and has nowhere to deliver —
// and, with the Secret keys still required, a pod that never becomes ready.

func legacyTelegramAgent(name string) *kyberv1.Agent {
	ag := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       kyberv1.AgentSpec{Runtime: "claude-code", Model: "claude-opus-5"},
	}
	ag.Spec.Secrets.TelegramEnabled = true
	return ag
}

// legacyTelegramSecret is the exact shape found in production: the bot token, and
// nothing else.
func legacyTelegramSecret(agentName string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: agentName + "-telegram", Namespace: "default"},
		Data:       map[string][]byte{TelegramTokenKey: []byte("123:abc")},
	}
}

func migrationReconciler(defaultAllowed string, objs ...client.Object) *AgentReconciler {
	b := fake.NewClientBuilder().WithScheme(buildTestScheme())
	for _, o := range objs {
		if ag, ok := o.(*kyberv1.Agent); ok {
			b = b.WithStatusSubresource(ag)
		}
		b = b.WithObjects(o)
	}
	return &AgentReconciler{
		Client:                        b.Build(),
		Scheme:                        buildTestScheme(),
		Recorder:                      record.NewFakeRecorder(16),
		AdapterRegistry:               map[string]pkgruntimes.Adapter{"claude-code": stubAdapterWithImage("kyber/cc:v1")},
		TelegramSidecarImage:          "ghcr.io/matty-v/kyber-mcp-telegram:v2.6.3",
		TelegramDefaultAllowedUserIDs: defaultAllowed,
	}
}

func fetchSecret(t *testing.T, r *AgentReconciler, name string) *corev1.Secret {
	t.Helper()
	var sec corev1.Secret
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &sec); err != nil {
		t.Fatalf("fetching %s: %v", name, err)
	}
	return &sec
}

// The whole point: a token-only Secret gains the two keys the sidecar needs.
func TestMigrateLegacyTelegram_BackfillsSecretKeys(t *testing.T) {
	ag := legacyTelegramAgent("r2-d2")
	r := migrationReconciler("1000000001", ag, legacyTelegramSecret("r2-d2"))

	wiring := r.migrateLegacyTelegramSecret(context.Background(), ag)
	if !wiring.HasAllowlist {
		t.Error("wiring reports no allowlist after seeding one")
	}

	sec := fetchSecret(t, r, "r2-d2-telegram")
	if len(sec.Data[TelegramWebhookSecretKey]) == 0 {
		t.Error("no webhook-secret — the sidecar cannot sign inbound, so every message is rejected")
	}
	if got := string(sec.Data[TelegramAllowedUserIDsKey]); got != "1000000001" {
		t.Errorf("allowed-user-ids = %q, want the install default seeded in", got)
	}
	if got := string(sec.Data[TelegramTokenKey]); got != "123:abc" {
		t.Errorf("the existing bot token was disturbed: %q", got)
	}
}

// Without a binding the sidecar POSTs into the void — the control plane has no
// route for the agent and rejects the envelope.
func TestMigrateLegacyTelegram_AddsInboundBinding(t *testing.T) {
	ag := legacyTelegramAgent("r2-d2")
	r := migrationReconciler("1000000001", ag, legacyTelegramSecret("r2-d2"))

	r.migrateLegacyTelegramSecret(context.Background(), ag)

	var binding *kyberv1.AgentInboundBinding
	for i := range ag.Spec.InboundBindings {
		if ag.Spec.InboundBindings[i].Name == TelegramInboundBindingName {
			binding = &ag.Spec.InboundBindings[i]
		}
	}
	if binding == nil {
		t.Fatal("no Telegram inbound binding — inbound messages have nowhere to go")
	}
	if binding.ExistingSecret != "r2-d2-telegram" {
		t.Errorf("binding points at %q, want the agent's Telegram Secret", binding.ExistingSecret)
	}
	if binding.SignatureHeader == "" || binding.SignaturePrefix == "" {
		t.Error("binding has no signature verification configured")
	}
}

func TestMigrateLegacyTelegram_HealsAttachmentFields(t *testing.T) {
	ag := legacyTelegramAgent("r2-d2")
	ag.Spec.InboundBindings = []kyberv1.AgentInboundBinding{{
		Name: TelegramInboundBindingName, ExistingSecret: "r2-d2-telegram",
		Action: legacyTelegramAction(),
		Fields: []kyberv1.AgentInboundField{
			{Label: "from", JsonPath: "$.user"},
			{Label: "chat_id", JsonPath: "$.chat_id"},
			{Label: "message", JsonPath: "$.content"},
		},
	}}
	sec := legacyTelegramSecret("r2-d2")
	sec.Data[TelegramAllowedUserIDsKey] = []byte("1000000001")
	sec.Data[TelegramWebhookSecretKey] = []byte("existing-hmac")
	r := migrationReconciler("", ag, sec)

	r.migrateLegacyTelegramSecret(context.Background(), ag)

	binding := ag.Spec.InboundBindings[0]
	for _, field := range TelegramInboundBinding("r2-d2-telegram", DefaultTelegramAction()).Fields {
		if !telegramBindingHasField(binding.Fields, field) {
			t.Errorf("migrated binding missing field %+v", field)
		}
	}
	if !strings.Contains(binding.Action, "download_attachment") {
		t.Errorf("legacy default action was not upgraded: %q", binding.Action)
	}
}

func TestMigrateLegacyTelegram_PreservesCustomAction(t *testing.T) {
	ag := legacyTelegramAgent("r2-d2")
	ag.Spec.InboundBindings = []kyberv1.AgentInboundBinding{{
		Name: TelegramInboundBindingName, ExistingSecret: "r2-d2-telegram", Action: "custom operator action",
	}}
	sec := legacyTelegramSecret("r2-d2")
	sec.Data[TelegramAllowedUserIDsKey] = []byte("1000000001")
	sec.Data[TelegramWebhookSecretKey] = []byte("existing-hmac")
	r := migrationReconciler("", ag, sec)

	r.migrateLegacyTelegramSecret(context.Background(), ag)

	if got := ag.Spec.InboundBindings[0].Action; got != "custom operator action" {
		t.Errorf("custom action = %q, want preserved", got)
	}
}

// Idempotence matters more than usual here: this runs on EVERY reconcile of
// every Telegram-enabled agent. A second pass must not rotate the HMAC secret
// (which would break the sidecar mid-flight) or append a duplicate binding.
func TestMigrateLegacyTelegram_IsIdempotent(t *testing.T) {
	ag := legacyTelegramAgent("r2-d2")
	r := migrationReconciler("1000000001", ag, legacyTelegramSecret("r2-d2"))

	r.migrateLegacyTelegramSecret(context.Background(), ag)
	firstSecret := string(fetchSecret(t, r, "r2-d2-telegram").Data[TelegramWebhookSecretKey])

	for i := 0; i < 3; i++ {
		r.migrateLegacyTelegramSecret(context.Background(), ag)
	}

	if got := string(fetchSecret(t, r, "r2-d2-telegram").Data[TelegramWebhookSecretKey]); got != firstSecret {
		t.Error("the HMAC secret was rotated on a later pass — that breaks a running sidecar's signing")
	}
	var count int
	for i := range ag.Spec.InboundBindings {
		if ag.Spec.InboundBindings[i].Name == TelegramInboundBindingName {
			count++
		}
	}
	if count != 1 {
		t.Errorf("found %d Telegram bindings after repeated migration, want exactly 1", count)
	}
}

// An operator's allowlist must survive. Seeding is for agents that have none.
func TestMigrateLegacyTelegram_NeverOverwritesAnExistingAllowlist(t *testing.T) {
	ag := legacyTelegramAgent("r2-d2")
	sec := legacyTelegramSecret("r2-d2")
	sec.Data[TelegramAllowedUserIDsKey] = []byte("111,222")
	r := migrationReconciler("1000000001", ag, sec)

	r.migrateLegacyTelegramSecret(context.Background(), ag)

	if got := string(fetchSecret(t, r, "r2-d2-telegram").Data[TelegramAllowedUserIDsKey]); got != "111,222" {
		t.Errorf("allowed-user-ids = %q — the install default overwrote an operator's list", got)
	}
}

// With no install default there is nothing to seed from, and guessing who may
// command an agent is not a guess worth making. The state must be reported so
// the caller can raise a condition instead of leaving a crash-looping sidecar
// as the only evidence.
func TestMigrateLegacyTelegram_ReportsMissingAllowlistRatherThanGuessing(t *testing.T) {
	ag := legacyTelegramAgent("r2-d2")
	r := migrationReconciler("", ag, legacyTelegramSecret("r2-d2"))

	wiring := r.migrateLegacyTelegramSecret(context.Background(), ag)

	if wiring.HasAllowlist {
		t.Error("reported an allowlist that does not exist")
	}
	if _, ok := fetchSecret(t, r, "r2-d2-telegram").Data[TelegramAllowedUserIDsKey]; ok {
		t.Error("wrote an allowlist with nothing to seed it from — that would be inventing who may command the agent")
	}
	// The HMAC secret has no such problem: it is shared between two components
	// we own, so it should still have been generated.
	if len(fetchSecret(t, r, "r2-d2-telegram").Data[TelegramWebhookSecretKey]) == 0 {
		t.Error("the HMAC secret needs no human input and should have been generated regardless")
	}
}

// That missing allowlist has to reach the operator. The sidecar refuses to start
// without one, so without a condition the only symptom is a crash-looping
// container with the reason buried in its logs.
func TestReconcileTelegramCondition_SurfacesAMissingAllowlist(t *testing.T) {
	ag := legacyTelegramAgent("r2-d2")
	r := migrationReconciler("", ag, legacyTelegramSecret("r2-d2"))

	wiring := r.migrateLegacyTelegramSecret(context.Background(), ag)
	r.reconcileTelegramCondition(context.Background(), ag, wiring)

	cond := storedCondition(t, r, ag)
	if cond == nil {
		t.Fatal("no condition for an agent whose Telegram sidecar cannot start")
	}
	if cond.Reason != "NoTelegramAllowlist" {
		t.Errorf("reason = %q, want NoTelegramAllowlist", cond.Reason)
	}
	if !strings.Contains(cond.Message, "telegram.defaultAllowedUserIds") ||
		!strings.Contains(cond.Message, "/comms") {
		t.Errorf("message must name both remediations verbatim, got: %q", cond.Message)
	}
}

// An agent that never enabled Telegram must not be touched — no Secret reads, no
// binding, no condition.
func TestMigrateLegacyTelegram_SkipsAgentsWithoutTelegram(t *testing.T) {
	ag := legacyTelegramAgent("dave")
	ag.Spec.Secrets.TelegramEnabled = false
	r := migrationReconciler("1000000001", ag)

	r.migrateLegacyTelegramSecret(context.Background(), ag)

	if len(ag.Spec.InboundBindings) != 0 {
		t.Errorf("added %d bindings to an agent with Telegram disabled", len(ag.Spec.InboundBindings))
	}
}

// telegramEnabled with no Secret at all means Telegram was never really
// configured. There is no bot token to invent, so the migration must not
// fabricate a half-configured channel.
func TestMigrateLegacyTelegram_NoSecretIsNotHalfConfigured(t *testing.T) {
	ag := legacyTelegramAgent("ghost")
	r := migrationReconciler("1000000001", ag)

	wiring := r.migrateLegacyTelegramSecret(context.Background(), ag)

	if wiring.SecretExists {
		t.Error("reported a Secret that does not exist")
	}
	if len(ag.Spec.InboundBindings) != 0 {
		t.Error("bound an inbound rail to a Secret that does not exist")
	}
}
