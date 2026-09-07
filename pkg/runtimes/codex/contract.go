package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/matty-v/kyber/pkg/codexauth"
	"strings"
	"time"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/runtimes"
)

func (*runtime) Descriptor() runtimes.Descriptor {
	return runtimes.Descriptor{
		ID: "codex", Name: "Codex", ContractVersion: runtimes.ContractVersion, Profile: runtimes.InteractiveProfile, HelmKey: "codex", Cancellation: "notify_only", ModelPrefix: "gpt-", LegacyDefaultsKey: "codex", LegacyCatalogKey: "codexModels", LegacyVersionsKey: "codexVersions", AuthFailureExitCode: 42,
		AuthModes: []runtimes.AuthMode{
			{ID: kyberv1.AgentAuthTypeOAuth, Name: "ChatGPT subscription", Flow: "device-code", InputField: "codexAuthJson", SecretSuffix: "codex-auth", ReauthorizePath: "codex-device-auth"},
			{ID: kyberv1.AgentAuthTypeAPIKey, Name: "OpenAI API key", Flow: "api-key", InputField: "openaiApiKey", SecretSuffix: "openai"},
		},
		Features:           []runtimes.Feature{runtimes.SessionRestart, runtimes.SessionResume, runtimes.Compaction, runtimes.JobTurnHooks, runtimes.TaskReceipts, runtimes.TaskTools, runtimes.ModelCatalog, runtimes.UsageReporting, runtimes.RuntimeRepairFeature},
		TranscriptPath:     ".codex/sessions",
		TranscriptExchange: `select(.type == "event_msg") | select(.payload.type == "user_message" or .payload.type == "agent_message") | {role: (if .payload.type == "user_message" then "user" else "assistant" end), timestamp: (.timestamp // ""), content: ((.payload.message // "") | tostring)}`,
	}
}
func (*runtime) Authentication() runtimes.Authentication { return authentication{} }

type authentication struct{}

func (authentication) Prepare(_ context.Context, mode kyberv1.AgentAuthType, in runtimes.AuthInput, _ runtimes.AuthOptions) ([]runtimes.Credential, error) {
	switch mode {
	case kyberv1.AgentAuthTypeOAuth:
		doc := in["codexAuthJson"]
		if doc == "" {
			doc = "{}"
		}
		if len(doc) > 256*1024 || !json.Valid([]byte(doc)) {
			return nil, fmt.Errorf("Codex auth.json must be valid JSON no larger than 256 KiB")
		}
		return []runtimes.Credential{{Suffix: "codex-auth", Data: map[string][]byte{"auth.json": []byte(doc)}}}, nil
	case kyberv1.AgentAuthTypeAPIKey:
		key := in["openaiApiKey"]
		if key == "" {
			return nil, fmt.Errorf("OpenAI API key is required for a Codex API-key agent")
		}
		return []runtimes.Credential{{Suffix: "openai", Data: map[string][]byte{"token": []byte(key)}}}, nil
	default:
		return nil, fmt.Errorf("authType must be oauth or api-key")
	}
}
func (authentication) Pending(mode kyberv1.AgentAuthType, data map[string][]byte) bool {
	return mode == kyberv1.AgentAuthTypeOAuth && strings.TrimSpace(string(data["auth.json"])) == "{}"
}

func (authentication) Validate(mode kyberv1.AgentAuthType, in runtimes.AuthInput) error {
	switch mode {
	case kyberv1.AgentAuthTypeOAuth:
		doc := in["codexAuthJson"]
		if doc != "" && (len(doc) > 256*1024 || !json.Valid([]byte(doc))) {
			return &runtimes.AuthValidationError{Field: "secrets.codexAuthJson", Message: "Codex auth.json must be valid JSON no larger than 256 KiB"}
		}
	case kyberv1.AgentAuthTypeAPIKey:
		if in["openaiApiKey"] == "" {
			return &runtimes.AuthValidationError{Field: "secrets.openaiApiKey", Message: "OpenAI API key is required for a Codex API-key agent"}
		}
	default:
		return fmt.Errorf("authType must be oauth or api-key")
	}
	return nil
}

func (authentication) ParseDeviceAuth(pane string, startedAt, now time.Time) runtimes.AuthObservation {
	return codexauth.Parse(pane, startedAt, now)
}
