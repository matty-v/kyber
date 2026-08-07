package agent

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// The legacy generated action, as it exists today on every agent wired for
// Discord before the bot token moved into the sidecar.
const legacyDiscordAction = "Someone messaged you on Discord (details below). Reply conversationally.\n\n" +
	"To reply, POST to https://discord.com/api/v10/channels/CHANNEL_ID/messages " +
	"with the header Authorization: Bot $DISCORD_BOT_TOKEN."

const legacyLoopbackDiscordAction = "Someone messaged you on Discord (details below). Reply conversationally.\n\n" +
	"To send your reply, run this in your shell — replace REPLY_TEXT with your message, " +
	"CHANNEL_ID with channel_id, and MESSAGE_ID with message_id shown below:\n" +
	`curl -sS -X POST -H "Content-Type: application/json" -d '{"channel_id":"CHANNEL_ID","content":"REPLY_TEXT","message_id":"MESSAGE_ID"}' http://127.0.0.1:14005/send` + "\n\n" +
	"Keep replies short and conversational. Discord credentials stay in the sidecar; do not look for a bot token."

func discordAgent(t *testing.T, action string, mentionOnly bool, withChannel bool) *kyberv1.Agent {
	t.Helper()
	ag := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: rfdAgentName, Namespace: rfdNS},
		Spec: kyberv1.AgentSpec{
			Machine: "m", Runtime: "claude-code", Model: "sonnet",
			InboundBindings: []kyberv1.AgentInboundBinding{
				{Name: "github", Action: "do the thing"},
				{Name: DiscordInboundBindingName, Action: action},
			},
		},
	}
	if withChannel {
		ag.Spec.Channels = &kyberv1.AgentChannels{
			Discord: &kyberv1.AgentDiscordChannel{ExistingSecret: "han-discord", MentionOnly: mentionOnly},
		}
	}
	return ag
}

func reloadAgent(t *testing.T, r *AgentReconciler) *kyberv1.Agent {
	t.Helper()
	got := &kyberv1.Agent{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: rfdAgentName, Namespace: rfdNS}, got); err != nil {
		t.Fatalf("re-getting agent: %v", err)
	}
	return got
}

func discordBinding(t *testing.T, ag *kyberv1.Agent) kyberv1.AgentInboundBinding {
	t.Helper()
	for _, b := range ag.Spec.InboundBindings {
		if b.Name == DiscordInboundBindingName {
			return b
		}
	}
	t.Fatal("discord binding missing")
	return kyberv1.AgentInboundBinding{}
}

func TestMigrateLegacyDiscordAction_RewritesStaleAction(t *testing.T) {
	scheme := newResolverScheme(t)
	ag := discordAgent(t, legacyDiscordAction, false, true)
	r := &AgentReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(ag).Build(), Scheme: scheme}

	if err := r.migrateLegacyDiscordAction(context.Background(), ag); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	got := discordBinding(t, reloadAgent(t, r))
	if strings.Contains(got.Action, "DISCORD_BOT_TOKEN") {
		t.Fatalf("stale token reference survived: %q", got.Action)
	}
	if !strings.Contains(got.Action, "127.0.0.1:14005/send") {
		t.Fatalf("migrated action does not point at the sidecar endpoint: %q", got.Action)
	}
	foundAttachments := false
	for _, field := range got.Fields {
		if field.Label == "attachments" && field.JsonPath == "$.attachments" {
			foundAttachments = true
		}
	}
	if !foundAttachments {
		t.Fatal("migrated binding has no attachments field")
	}
	// The unrelated binding must be untouched.
	for _, b := range reloadAgent(t, r).Spec.InboundBindings {
		if b.Name == "github" && b.Action != "do the thing" {
			t.Fatalf("unrelated binding was rewritten: %q", b.Action)
		}
	}
}

func TestMigrateLegacyDiscordAction_CarriesMentionOnly(t *testing.T) {
	scheme := newResolverScheme(t)
	ag := discordAgent(t, legacyDiscordAction, true, true)
	r := &AgentReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(ag).Build(), Scheme: scheme}

	if err := r.migrateLegacyDiscordAction(context.Background(), ag); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if got := discordBinding(t, reloadAgent(t, r)).Action; !strings.Contains(got, "always reply") {
		t.Fatalf("mentionOnly guidance missing from migrated action: %q", got)
	}
}

// An operator's hand-tuned action is theirs. Only text that references the
// now-absent env var is provably broken, so only that is rewritten.
func TestMigrateLegacyDiscordAction_LeavesCustomActionAlone(t *testing.T) {
	scheme := newResolverScheme(t)
	const custom = "Reply in haiku by POSTing to http://127.0.0.1:14005/send"
	ag := discordAgent(t, custom, false, true)
	r := &AgentReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(ag).Build(), Scheme: scheme}

	if err := r.migrateLegacyDiscordAction(context.Background(), ag); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if got := discordBinding(t, reloadAgent(t, r)).Action; got != custom {
		t.Fatalf("custom action was rewritten: %q", got)
	}
}

// No Discord channel means the binding is inert; don't touch the spec.
func TestMigrateLegacyDiscordAction_SkipsWhenChannelDisabled(t *testing.T) {
	scheme := newResolverScheme(t)
	ag := discordAgent(t, legacyDiscordAction, false, false)
	r := &AgentReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(ag).Build(), Scheme: scheme}

	if err := r.migrateLegacyDiscordAction(context.Background(), ag); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if got := discordBinding(t, reloadAgent(t, r)).Action; got != legacyDiscordAction {
		t.Fatalf("action changed for a disabled channel: %q", got)
	}
}

// Running twice must be a no-op the second time — the migrated text no longer
// matches, so the reconciler stops writing.
func TestMigrateLegacyDiscordAction_Idempotent(t *testing.T) {
	scheme := newResolverScheme(t)
	ag := discordAgent(t, legacyDiscordAction, false, true)
	r := &AgentReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(ag).Build(), Scheme: scheme}

	ctx := context.Background()
	if err := r.migrateLegacyDiscordAction(ctx, ag); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	first := reloadAgent(t, r)
	if err := r.migrateLegacyDiscordAction(ctx, first); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	second := reloadAgent(t, r)

	if discordBinding(t, first).Action != discordBinding(t, second).Action {
		t.Fatal("second migration changed the action")
	}
	if first.ResourceVersion != second.ResourceVersion {
		t.Fatalf("second migration issued a write: rv %s -> %s", first.ResourceVersion, second.ResourceVersion)
	}
}

// Version drift in the legacy text must not defeat detection — the old default
// named v10, but any direct discord.com API call paired with the env var is
// equally broken.
func TestIsLegacyDiscordDefaultAction(t *testing.T) {
	cases := []struct {
		name   string
		action string
		want   bool
	}{
		{"generated v10 default", legacyDiscordAction, true},
		{"generated loopback-only default", legacyLoopbackDiscordAction, true},
		{"generated MCP default before attachments", "Someone messaged you on Discord (details below). Reply conversationally. Use kyber-discord. Keep replies short and conversational.", true},
		{"different api version", "POST https://discord.com/api/v9/channels/X/messages with $DISCORD_BOT_TOKEN", true},
		{"loopback endpoint", "POST http://127.0.0.1:14005/send", false},
		{"empty", "", false},
		{"token mentioned but no discord api call", "the DISCORD_BOT_TOKEN is not yours", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsLegacyDiscordDefaultAction(tc.action); got != tc.want {
				t.Fatalf("IsLegacyDiscordDefaultAction(%q) = %v, want %v", tc.action, got, tc.want)
			}
		})
	}
}
