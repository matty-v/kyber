package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/controllers/agent"
)

// commsHarness drives the /comms sub-tree against a fake cluster.
type commsHarness struct {
	handler http.Handler
	k8s     client.Client
}

func buildCommsHarness(t *testing.T, objs ...runtime.Object) *commsHarness {
	t.Helper()
	scheme := mustNewScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&kyberv1.Agent{}).
		Build()
	srv := &api.Server{
		K8sClient: fakeClient,
		APIKey:    testAPIKey,
		Namespace: "kyber-system",
		PublicURL: "https://kyber-test.example",
	}
	return &commsHarness{handler: srv.BuildHandler(), k8s: fakeClient}
}

func (h *commsHarness) do(t *testing.T, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, authedRequest(t, method, target, body))
	return rr
}

func (h *commsHarness) agent(t *testing.T, name string) *kyberv1.Agent {
	t.Helper()
	a := &kyberv1.Agent{}
	if err := h.k8s.Get(context.Background(),
		types.NamespacedName{Name: name, Namespace: "kyber-system"}, a); err != nil {
		t.Fatalf("get agent %s: %v", name, err)
	}
	return a
}

func (h *commsHarness) secret(t *testing.T, name string) (*corev1.Secret, error) {
	t.Helper()
	s := &corev1.Secret{}
	err := h.k8s.Get(context.Background(),
		types.NamespacedName{Name: name, Namespace: "kyber-system"}, s)
	return s, err
}

// commsAgent is an OAuth agent with no comms configured.
func commsAgent(name string) *kyberv1.Agent {
	return bareAgent(name)
}

// decodeChannel pulls a single channel response out of a recorder.
func decodeChannel(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rr.Body.String())
	}
	return got
}

// validDiscordPut is the minimum body that should succeed.
func validDiscordPut() map[string]any {
	return map[string]any{
		"botToken":       "bot-token-abc",
		"guildIds":       []string{"234567890123456789"},
		"channelIds":     []string{"345678901234567890"},
		"allowedUserIds": []string{"123456789012345678"},
		"mentionOnly":    true,
	}
}

// --- listing --------------------------------------------------------------

// TestComms_List_UnconfiguredAgent: both channels report not-configured rather
// than 404, so the PWA can render the tab for a fresh agent.
func TestComms_List_UnconfiguredAgent(t *testing.T) {
	h := buildCommsHarness(t, commsAgent("dave"))
	rr := h.do(t, http.MethodGet, "/api/v1/agents/dave/comms", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Channels []map[string]any `json:"channels"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Channels) != 2 {
		t.Fatalf("want 2 channels, got %d: %s", len(got.Channels), rr.Body.String())
	}
	seen := map[string]bool{}
	for _, c := range got.Channels {
		seen[c["channel"].(string)] = true
		if c["configured"] != false {
			t.Errorf("channel %v: want configured=false, got %v", c["channel"], c["configured"])
		}
	}
	if !seen["telegram"] || !seen["discord"] {
		t.Errorf("want telegram+discord, got %v", seen)
	}
}

// TestComms_UnknownChannel_404: only the two supported channels route. Guards
// against a typo silently creating garbage config.
func TestComms_UnknownChannel_404(t *testing.T) {
	h := buildCommsHarness(t, commsAgent("dave"))
	rr := h.do(t, http.MethodPut, "/api/v1/agents/dave/comms/slack", map[string]any{})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestComms_UnknownAgent_404 for both list and single-channel reads.
func TestComms_UnknownAgent_404(t *testing.T) {
	h := buildCommsHarness(t)
	for _, target := range []string{
		"/api/v1/agents/ghost/comms",
		"/api/v1/agents/ghost/comms/discord",
	} {
		if rr := h.do(t, http.MethodGet, target, nil); rr.Code != http.StatusNotFound {
			t.Errorf("%s: want 404, got %d", target, rr.Code)
		}
	}
}

// --- Discord: the one-call wiring ----------------------------------------

// TestComms_PutDiscord_WiresEverythingInOneCall is the core of #664: a single
// PUT must produce the Secret, the matching inbound binding, and the channel
// spec — with the HMAC secret shared automatically, which is the step the
// manual flow made a human do by hand.
func TestComms_PutDiscord_WiresEverythingInOneCall(t *testing.T) {
	h := buildCommsHarness(t, commsAgent("barf"))

	rr := h.do(t, http.MethodPut, "/api/v1/agents/barf/comms/discord", validDiscordPut())
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	sec, err := h.secret(t, "barf-discord")
	if err != nil {
		t.Fatalf("secret barf-discord not created: %v", err)
	}
	if got := string(sec.Data["bot-token"]); got != "bot-token-abc" {
		t.Errorf("bot-token = %q, want %q", got, "bot-token-abc")
	}
	hmacSecret := string(sec.Data["webhook-secret"])
	if len(hmacSecret) != 64 {
		t.Errorf("webhook-secret = %q, want 64 hex chars", hmacSecret)
	}
	if got := string(sec.Data["allowed-user-ids"]); got != "123456789012345678" {
		t.Errorf("allowed-user-ids = %q", got)
	}

	ag := h.agent(t, "barf")
	if ag.Spec.Channels == nil || ag.Spec.Channels.Discord == nil {
		t.Fatal("spec.channels.discord not set")
	}
	if ag.Spec.Channels.Discord.ExistingSecret != "barf-discord" {
		t.Errorf("existingSecret = %q", ag.Spec.Channels.Discord.ExistingSecret)
	}
	if ag.Annotations[agent.DiscordConfigRevisionAnnotation] == "" {
		t.Error("Discord update did not stamp a convergence revision")
	}
	if !ag.Spec.Channels.Discord.MentionOnly {
		t.Error("mentionOnly not carried through")
	}

	var binding *kyberv1.AgentInboundBinding
	for i := range ag.Spec.InboundBindings {
		if ag.Spec.InboundBindings[i].Name == "discord" {
			binding = &ag.Spec.InboundBindings[i]
		}
	}
	if binding == nil {
		t.Fatal("discord inbound binding not created")
	}

	// The whole point: the binding and the sidecar read the SAME Secret, so the
	// HMAC secret cannot be mismatched by a human copying it.
	if binding.ExistingSecret != "barf-discord" {
		t.Errorf("binding.existingSecret = %q, want the shared discord Secret", binding.ExistingSecret)
	}
	if binding.SignatureHeader != "X-Kyber-Signature-256" || binding.SignaturePrefix != "sha256=" {
		t.Errorf("signature config = %q/%q, must match the sidecar's",
			binding.SignatureHeader, binding.SignaturePrefix)
	}
	// The agent cannot reply without channel_id, so its absence is a real bug.
	wantPaths := map[string]string{
		"from": "$.user", "channel_id": "$.channel_id", "message_id": "$.message_id",
		"message": "$.content", "attachments": "$.attachments", "thread_id": "$.thread_id",
		"thread_name": "$.thread_name", "parent_channel_id": "$.parent_channel_id",
		"referenced_message": "$.referenced_message", "recent_context": "$.recent_context",
	}
	if len(binding.Fields) != len(wantPaths) {
		t.Fatalf("want %d fields, got %d", len(wantPaths), len(binding.Fields))
	}
	for _, f := range binding.Fields {
		if wantPaths[f.Label] != f.JsonPath {
			t.Errorf("field %q jsonPath = %q, want %q", f.Label, f.JsonPath, wantPaths[f.Label])
		}
	}
	if strings.Contains(binding.Action, "DISCORD_BOT_TOKEN") ||
		!strings.Contains(binding.Action, "http://127.0.0.1:14005/send") ||
		!strings.Contains(binding.Action, "kyber-discord") ||
		!strings.Contains(binding.Action, `"message_id":"MESSAGE_ID"`) {
		t.Errorf("default action must tell the agent how to reply, got:\n%s", binding.Action)
	}
	if !strings.Contains(binding.Action, "@-mention") {
		t.Error("mentionOnly=true should tell the agent every message is meant for it")
	}
}

// TestComms_PutDiscord_ResponseNeverLeaksSecrets: tokens go in, never come out.
func TestComms_PutDiscord_ResponseNeverLeaksSecrets(t *testing.T) {
	h := buildCommsHarness(t, commsAgent("barf"))
	h.do(t, http.MethodPut, "/api/v1/agents/barf/comms/discord", validDiscordPut())

	sec, err := h.secret(t, "barf-discord")
	if err != nil {
		t.Fatalf("secret: %v", err)
	}
	hmacSecret := string(sec.Data["webhook-secret"])

	for _, target := range []string{
		"/api/v1/agents/barf/comms",
		"/api/v1/agents/barf/comms/discord",
	} {
		body := h.do(t, http.MethodGet, target, nil).Body.String()
		if strings.Contains(body, "bot-token-abc") {
			t.Errorf("%s leaked the bot token: %s", target, body)
		}
		if strings.Contains(body, hmacSecret) {
			t.Errorf("%s leaked the HMAC secret: %s", target, body)
		}
	}

	got := decodeChannel(t, h.do(t, http.MethodGet, "/api/v1/agents/barf/comms/discord", nil))
	if got["botTokenSet"] != true {
		t.Errorf("botTokenSet should report presence, got %v", got["botTokenSet"])
	}
}

// TestComms_PutDiscord_EmptyAllowlistRejected: an empty allowlist is
// fail-closed in the sidecar, so accepting it would produce an agent that looks
// healthy and can hear nobody.
func TestComms_PutDiscord_EmptyAllowlistRejected(t *testing.T) {
	h := buildCommsHarness(t, commsAgent("barf"))
	body := validDiscordPut()
	delete(body, "allowedUserIds")

	rr := h.do(t, http.MethodPut, "/api/v1/agents/barf/comms/discord", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "allowedUserIds") {
		t.Errorf("error should name the field, got %s", rr.Body.String())
	}
	if _, err := h.secret(t, "barf-discord"); err == nil {
		t.Error("rejected request must not create a Secret")
	}
}

// TestComms_PutDiscord_RejectsNonSnowflakeIDs catches the common paste error —
// a channel name or URL instead of an ID — at the edge instead of at runtime.
func TestComms_PutDiscord_RejectsNonSnowflakeIDs(t *testing.T) {
	for _, tc := range []struct{ field, value string }{
		{"channelIds", "#dev-bots"},
		{"allowedUserIds", "matt"},
		{"guildIds", "https://discord.com/channels/123"},
	} {
		t.Run(tc.field+"="+tc.value, func(t *testing.T) {
			h := buildCommsHarness(t, commsAgent("barf"))
			body := validDiscordPut()
			body[tc.field] = []string{tc.value}
			rr := h.do(t, http.MethodPut, "/api/v1/agents/barf/comms/discord", body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), tc.field) {
				t.Errorf("error should name %q, got %s", tc.field, rr.Body.String())
			}
		})
	}
}

// TestComms_PutDiscord_UpdateKeepsHMACSecret: toggling a setting must not
// rotate the secret out from under a running sidecar.
func TestComms_PutDiscord_UpdateKeepsHMACSecret(t *testing.T) {
	h := buildCommsHarness(t, commsAgent("barf"))
	h.do(t, http.MethodPut, "/api/v1/agents/barf/comms/discord", validDiscordPut())
	sec, _ := h.secret(t, "barf-discord")
	first := string(sec.Data["webhook-secret"])

	// Update with no botToken at all — the stored one must survive too.
	body := validDiscordPut()
	delete(body, "botToken")
	body["mentionOnly"] = false
	if rr := h.do(t, http.MethodPut, "/api/v1/agents/barf/comms/discord", body); rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	sec, _ = h.secret(t, "barf-discord")
	if got := string(sec.Data["webhook-secret"]); got != first {
		t.Errorf("HMAC secret rotated on update: %q -> %q", first, got)
	}
	if got := string(sec.Data["bot-token"]); got != "bot-token-abc" {
		t.Errorf("bot token lost on update: %q", got)
	}
	if ag := h.agent(t, "barf"); ag.Spec.Channels.Discord.MentionOnly {
		t.Error("mentionOnly=false not applied")
	}
}

// TestComms_PutDiscord_PreservesOperatorAction: the binding's instruction text
// is where an agent's Discord persona lives; a settings change must not
// overwrite it with the generic default.
func TestComms_PutDiscord_PreservesOperatorAction(t *testing.T) {
	h := buildCommsHarness(t, commsAgent("barf"))
	body := validDiscordPut()
	body["action"] = "Reply as Barf, a grumpy space mercenary."
	h.do(t, http.MethodPut, "/api/v1/agents/barf/comms/discord", body)

	update := validDiscordPut()
	update["mentionOnly"] = false
	h.do(t, http.MethodPut, "/api/v1/agents/barf/comms/discord", update)

	ag := h.agent(t, "barf")
	for _, b := range ag.Spec.InboundBindings {
		if b.Name == "discord" && b.Action != "Reply as Barf, a grumpy space mercenary." {
			t.Errorf("custom action overwritten: %q", b.Action)
		}
	}
}

func TestComms_PutDiscord_MigratesLegacyTokenAction(t *testing.T) {
	h := buildCommsHarness(t, commsAgent("barf"))
	body := validDiscordPut()
	body["action"] = `Reply with DISCORD_BOT_TOKEN through https://discord.com/api/v10/channels/CHANNEL_ID/messages`
	h.do(t, http.MethodPut, "/api/v1/agents/barf/comms/discord", body)

	update := validDiscordPut()
	delete(update, "botToken")
	h.do(t, http.MethodPut, "/api/v1/agents/barf/comms/discord", update)

	ag := h.agent(t, "barf")
	for _, b := range ag.Spec.InboundBindings {
		if b.Name != "discord" {
			continue
		}
		if strings.Contains(b.Action, "DISCORD_BOT_TOKEN") || !strings.Contains(b.Action, "127.0.0.1:14005/send") {
			t.Errorf("legacy action was not migrated: %q", b.Action)
		}
		return
	}
	t.Fatal("discord binding missing")
}

// TestComms_PutDiscord_RequiresBotTokenWhenUnset
func TestComms_PutDiscord_RequiresBotTokenWhenUnset(t *testing.T) {
	h := buildCommsHarness(t, commsAgent("barf"))
	body := validDiscordPut()
	delete(body, "botToken")

	rr := h.do(t, http.MethodPut, "/api/v1/agents/barf/comms/discord", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "botToken") {
		t.Errorf("error should name botToken, got %s", rr.Body.String())
	}
}

// TestComms_PutDiscord_LeavesLegacyWebhookAlone: the outbound-only webhook
// shares the <agent>-discord Secret and a team's whole Discord setup
// rides it. A two-way PUT must not clobber it.
func TestComms_PutDiscord_LeavesLegacyWebhookAlone(t *testing.T) {
	legacy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "barf-discord", Namespace: "kyber-system"},
		Data:       map[string][]byte{"webhook-url": []byte("https://discord.com/api/webhooks/legacy")},
	}
	h := buildCommsHarness(t, commsAgent("barf"), legacy)

	if rr := h.do(t, http.MethodPut, "/api/v1/agents/barf/comms/discord", validDiscordPut()); rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	sec, err := h.secret(t, "barf-discord")
	if err != nil {
		t.Fatalf("secret: %v", err)
	}
	if got := string(sec.Data["webhook-url"]); got != "https://discord.com/api/webhooks/legacy" {
		t.Errorf("legacy webhook-url clobbered: %q", got)
	}
	if len(sec.Data["bot-token"]) == 0 {
		t.Error("two-way keys not written alongside the legacy key")
	}
}

// TestComms_DeleteDiscord_UnwiresButKeepsLegacyKey
func TestComms_DeleteDiscord_UnwiresButKeepsLegacyKey(t *testing.T) {
	legacy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "barf-discord", Namespace: "kyber-system"},
		Data:       map[string][]byte{"webhook-url": []byte("https://discord.com/api/webhooks/legacy")},
	}
	h := buildCommsHarness(t, commsAgent("barf"), legacy)
	h.do(t, http.MethodPut, "/api/v1/agents/barf/comms/discord", validDiscordPut())

	if rr := h.do(t, http.MethodDelete, "/api/v1/agents/barf/comms/discord", nil); rr.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", rr.Code, rr.Body.String())
	}

	ag := h.agent(t, "barf")
	if ag.Spec.Channels != nil && ag.Spec.Channels.Discord != nil {
		t.Error("spec.channels.discord still set after delete")
	}
	for _, b := range ag.Spec.InboundBindings {
		if b.Name == "discord" {
			t.Error("discord inbound binding still present after delete")
		}
	}

	sec, err := h.secret(t, "barf-discord")
	if err != nil {
		t.Fatalf("Secret should survive — it still holds the legacy webhook: %v", err)
	}
	if len(sec.Data["bot-token"]) != 0 || len(sec.Data["webhook-secret"]) != 0 {
		t.Error("two-way credentials not removed on delete")
	}
	if got := string(sec.Data["webhook-url"]); got != "https://discord.com/api/webhooks/legacy" {
		t.Errorf("legacy webhook-url removed by two-way delete: %q", got)
	}
}

// TestComms_DeleteDiscord_RemovesSecretWhenNothingLeft
func TestComms_DeleteDiscord_RemovesSecretWhenNothingLeft(t *testing.T) {
	h := buildCommsHarness(t, commsAgent("barf"))
	h.do(t, http.MethodPut, "/api/v1/agents/barf/comms/discord", validDiscordPut())
	h.do(t, http.MethodDelete, "/api/v1/agents/barf/comms/discord", nil)

	if _, err := h.secret(t, "barf-discord"); err == nil {
		t.Error("Secret should be deleted once no keys remain")
	}
}

// TestComms_DeleteDiscord_NotConfigured_404
func TestComms_DeleteDiscord_NotConfigured_404(t *testing.T) {
	h := buildCommsHarness(t, commsAgent("barf"))
	if rr := h.do(t, http.MethodDelete, "/api/v1/agents/barf/comms/discord", nil); rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// --- Telegram -------------------------------------------------------------

// TestComms_PutTelegram_EnablesOnExistingAgent is the gap this closes for
// Telegram: before #664 the flag could only ever be set at agent creation.
func TestComms_PutTelegram_EnablesOnExistingAgent(t *testing.T) {
	h := buildCommsHarness(t, commsAgent("dave"))

	// allowedUserIds is required for EVERY runtime since kyber#684. It used to
	// be Codex-only: a Claude Code agent got a token-only Secret because its
	// in-process plugin did its own allowlisting. That plugin is gone.
	rr := h.do(t, http.MethodPut, "/api/v1/agents/dave/comms/telegram",
		map[string]any{"botToken": "123:abc", "allowedUserIds": []string{"1000000001"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	if ag := h.agent(t, "dave"); !ag.Spec.Secrets.TelegramEnabled {
		t.Error("telegramEnabled not set")
	}
	sec, err := h.secret(t, "dave-telegram")
	if err != nil {
		t.Fatalf("dave-telegram not created: %v", err)
	}
	if got := string(sec.Data["token"]); got != "123:abc" {
		t.Errorf("token = %q", got)
	}
	if strings.Contains(rr.Body.String(), "123:abc") {
		t.Errorf("response leaked the bot token: %s", rr.Body.String())
	}
}

// TestComms_PutTelegram_RotatesToken: a second PUT replaces the stored token,
// which is the leaked-credential path the API had no answer for.
func TestComms_PutTelegram_RotatesToken(t *testing.T) {
	h := buildCommsHarness(t, commsAgent("dave"))
	h.do(t, http.MethodPut, "/api/v1/agents/dave/comms/telegram",
		map[string]any{"botToken": "old", "allowedUserIds": []string{"1000000001"}})
	h.do(t, http.MethodPut, "/api/v1/agents/dave/comms/telegram",
		map[string]any{"botToken": "new", "allowedUserIds": []string{"1000000001"}})

	sec, _ := h.secret(t, "dave-telegram")
	if got := string(sec.Data["token"]); got != "new" {
		t.Errorf("token = %q, want the rotated value", got)
	}
}

// TestComms_PutTelegram_ReenableWithoutToken: re-enabling after a disable
// shouldn't demand the operator dig the token out again — but only when one is
// actually stored.
func TestComms_PutTelegram_ReenableWithoutToken(t *testing.T) {
	h := buildCommsHarness(t, commsAgent("dave"))
	h.do(t, http.MethodPut, "/api/v1/agents/dave/comms/telegram",
		map[string]any{"botToken": "123:abc", "allowedUserIds": []string{"1000000001"}})

	// Flag off, token still stored (simulates a spec edit rather than DELETE).
	ag := h.agent(t, "dave")
	patch := client.MergeFrom(ag.DeepCopy())
	ag.Spec.Secrets.TelegramEnabled = false
	if err := h.k8s.Patch(context.Background(), ag, patch); err != nil {
		t.Fatalf("patch: %v", err)
	}

	if rr := h.do(t, http.MethodPut, "/api/v1/agents/dave/comms/telegram",
		map[string]any{"allowedUserIds": []string{"1000000001"}}); rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if ag := h.agent(t, "dave"); !ag.Spec.Secrets.TelegramEnabled {
		t.Error("telegram not re-enabled")
	}
}

// TestComms_PutTelegram_RequiresTokenWhenNoneStored
func TestComms_PutTelegram_RequiresTokenWhenNoneStored(t *testing.T) {
	h := buildCommsHarness(t, commsAgent("dave"))
	rr := h.do(t, http.MethodPut, "/api/v1/agents/dave/comms/telegram", map[string]any{})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "botToken") {
		t.Errorf("error should name botToken, got %s", rr.Body.String())
	}
}

// TestComms_PutTelegram_RejectsAPIKeyAgent: the same rule createAgent enforces.
// Both paths share validateTelegramAuth so they cannot drift.
func TestComms_PutTelegram_RejectsAPIKeyAgent(t *testing.T) {
	ag := commsAgent("apikey-agent")
	ag.Spec.Secrets.AuthType = kyberv1.AgentAuthTypeAPIKey
	h := buildCommsHarness(t, ag)

	rr := h.do(t, http.MethodPut, "/api/v1/agents/apikey-agent/comms/telegram",
		map[string]any{"botToken": "123:abc"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "OAuth") {
		t.Errorf("error should explain the OAuth requirement, got %s", rr.Body.String())
	}
	if _, err := h.secret(t, "apikey-agent-telegram"); err == nil {
		t.Error("rejected request must not store a token")
	}
}

// TestComms_DeleteTelegram_ClearsFlagAndToken: disabling should not leave a
// live bot token sitting in the cluster.
func TestComms_DeleteTelegram_ClearsFlagAndToken(t *testing.T) {
	h := buildCommsHarness(t, commsAgent("dave"))
	h.do(t, http.MethodPut, "/api/v1/agents/dave/comms/telegram", map[string]any{"botToken": "123:abc"})

	if rr := h.do(t, http.MethodDelete, "/api/v1/agents/dave/comms/telegram", nil); rr.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", rr.Code, rr.Body.String())
	}
	if ag := h.agent(t, "dave"); ag.Spec.Secrets.TelegramEnabled {
		t.Error("telegramEnabled still set")
	}
	if _, err := h.secret(t, "dave-telegram"); err == nil {
		t.Error("bot token should be deleted with the channel")
	}
}

// --- pod staleness --------------------------------------------------------

// TestComms_PodRestartRequired_ReflectsRunningPod: the config only takes effect
// on a new pod, so the API has to tell the operator when the live pod is behind.
func TestComms_PodRestartRequired_ReflectsRunningPod(t *testing.T) {
	t.Run("pod without the sidecar is stale once discord is configured", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "agent-barf", Namespace: "kyber-system"},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "runtime"}}},
		}
		h := buildCommsHarness(t, commsAgent("barf"), pod)
		h.do(t, http.MethodPut, "/api/v1/agents/barf/comms/discord", validDiscordPut())

		got := decodeChannel(t, h.do(t, http.MethodGet, "/api/v1/agents/barf/comms/discord", nil))
		if got["podRestartRequired"] != true {
			t.Errorf("want podRestartRequired=true, got %v", got["podRestartRequired"])
		}
	})

	t.Run("pod carrying the sidecar is stale after configuration changes", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "agent-barf", Namespace: "kyber-system"},
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "agent", Image: "runtime"},
				{Name: "kyber-mcp-discord", Image: "sidecar"},
			}},
		}
		h := buildCommsHarness(t, commsAgent("barf"), pod)
		h.do(t, http.MethodPut, "/api/v1/agents/barf/comms/discord", validDiscordPut())

		got := decodeChannel(t, h.do(t, http.MethodGet, "/api/v1/agents/barf/comms/discord", nil))
		if got["podRestartRequired"] != true {
			t.Errorf("want podRestartRequired=true, got %v", got["podRestartRequired"])
		}
	})

	t.Run("no pod means nothing to be stale", func(t *testing.T) {
		h := buildCommsHarness(t, commsAgent("barf"))
		got := decodeChannel(t, h.do(t, http.MethodGet, "/api/v1/agents/barf/comms/discord", nil))
		if got["podRestartRequired"] != false {
			t.Errorf("want podRestartRequired=false, got %v", got["podRestartRequired"])
		}
	})
}

// TestComms_PutAlwaysReportsRestartRequired: a write just changed the spec, so
// whatever the pod has is stale by definition.
func TestComms_PutAlwaysReportsRestartRequired(t *testing.T) {
	h := buildCommsHarness(t, commsAgent("barf"))
	got := decodeChannel(t, h.do(t, http.MethodPut, "/api/v1/agents/barf/comms/discord", validDiscordPut()))
	if got["podRestartRequired"] != true {
		t.Errorf("want podRestartRequired=true after PUT, got %v", got["podRestartRequired"])
	}
}

// --- method + body handling ----------------------------------------------

func TestComms_MethodNotAllowed(t *testing.T) {
	h := buildCommsHarness(t, commsAgent("dave"))
	if rr := h.do(t, http.MethodPost, "/api/v1/agents/dave/comms/telegram", map[string]any{}); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST on a channel: want 405, got %d", rr.Code)
	}
	if rr := h.do(t, http.MethodDelete, "/api/v1/agents/dave/comms", nil); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE on the collection: want 405, got %d", rr.Code)
	}
}

// TestComms_RejectsUnknownFields catches a client sending a misspelled field
// and silently getting the default instead of what it meant.
func TestComms_RejectsUnknownFields(t *testing.T) {
	h := buildCommsHarness(t, commsAgent("barf"))
	body := validDiscordPut()
	body["mention_only"] = true // snake_case typo of mentionOnly

	if rr := h.do(t, http.MethodPut, "/api/v1/agents/barf/comms/discord", body); rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
}
