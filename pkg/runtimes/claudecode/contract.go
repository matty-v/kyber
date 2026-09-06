package claudecode

import (
	"context"
	"fmt"
	"strconv"
	"time"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/oauth"
	"github.com/matty-v/kyber/pkg/runtimes"
)

func (*runtime) Descriptor() runtimes.Descriptor {
	return runtimes.Descriptor{
		ID: "claude-code", Name: "Claude Code", ContractVersion: runtimes.ContractVersion, Profile: runtimes.InteractiveProfile, HelmKey: "claudeCode", Cancellation: "notify_only", ModelPrefix: "claude-", LegacyDefaultsKey: "claude-code", LegacyCatalogKey: "models", LegacyVersionsKey: "claudeCodeVersions", AuthFailureExitCode: 2, RequireCatalogContext: true,
		AuthModes: []runtimes.AuthMode{
			{ID: kyberv1.AgentAuthTypeOAuth, Name: "Claude subscription", Flow: "authorization-code", InputField: "oauthCode", SecretSuffix: "oauth", ReauthorizePath: "oauth", AuthorizationURL: "https://claude.ai/oauth/authorize", AuthorizationParams: map[string]string{"code": "true", "client_id": "9d1c250a-e61b-44d9-88ed-5944d1962f5e", "response_type": "code", "redirect_uri": "https://platform.claude.com/oauth/code/callback", "scope": "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"}},
			{ID: kyberv1.AgentAuthTypeAPIKey, Name: "Anthropic API key", Flow: "api-key", InputField: "anthropicApiKey", SecretSuffix: "anthropic"},
		},
		Features:           []runtimes.Feature{runtimes.SessionRestart, runtimes.SessionResume, runtimes.Compaction, runtimes.JobTurnHooks, runtimes.TaskReceipts, runtimes.TaskTools, runtimes.ModelCatalog, runtimes.UsageReporting, runtimes.RuntimeRepairFeature},
		TranscriptPath:     ".claude/projects",
		TranscriptExchange: `select((.isSidechain // false) | not) | select(.type == "user" or .type == "assistant") | {role: .type, timestamp: (.timestamp // ""), content: (.message.content as $c | if ($c | type) == "string" then $c elif ($c | type) == "array" then ([$c[] | select(.type == "text") | .text] | join("\n")) else "" end)}`,
	}
}
func (*runtime) Authentication() runtimes.Authentication { return authentication{} }

type authentication struct{}

func (authentication) Prepare(ctx context.Context, mode kyberv1.AgentAuthType, in runtimes.AuthInput, options runtimes.AuthOptions) ([]runtimes.Credential, error) {
	switch mode {
	case kyberv1.AgentAuthTypeOAuth:
		// Preserve legacy create requests that provision credentials separately.
		if in["oauthCode"] == "" || in["pkceVerifier"] == "" {
			return nil, nil
		}
		tok, err := oauth.NewClient(options.TokenURL).ExchangeAuthorizationCode(ctx, in["oauthCode"], in["pkceVerifier"], in["pkceState"])
		if err != nil {
			return nil, fmt.Errorf("oauth exchange: %w", err)
		}
		return []runtimes.Credential{{Suffix: "oauth", Data: map[string][]byte{"access_token": []byte(tok.AccessToken), "refresh_token": []byte(tok.RefreshToken), "expires_at": []byte(strconv.FormatInt(time.Now().UnixMilli()+int64(tok.ExpiresIn)*1000, 10))}}}, nil
	case kyberv1.AgentAuthTypeAPIKey:
		if in["anthropicApiKey"] == "" {
			return nil, nil
		} // existing separately-provisioned credential path
		return []runtimes.Credential{{Suffix: "anthropic", Data: map[string][]byte{"token": []byte(in["anthropicApiKey"])}}}, nil
	default:
		return nil, fmt.Errorf("authType must be oauth or api-key")
	}
}
func (authentication) Pending(kyberv1.AgentAuthType, map[string][]byte) bool { return false }

func (authentication) Validate(mode kyberv1.AgentAuthType, input runtimes.AuthInput) error {
	if mode != kyberv1.AgentAuthTypeOAuth && mode != kyberv1.AgentAuthTypeAPIKey {
		return fmt.Errorf("authType must be oauth or api-key")
	}
	if input["oauthCode"] != "" {
		for _, field := range []string{"pkceVerifier", "pkceState"} {
			if input[field] == "" {
				return &runtimes.AuthValidationError{Field: "secrets." + field, Message: field + " is required when oauthCode is provided"}
			}
		}
	}
	return nil
}
