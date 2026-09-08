package hermes

import (
	"context"
	"fmt"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/runtimes"
)

const openRouterInputField = "openrouterApiKey"

func (*runtime) Descriptor() runtimes.Descriptor {
	return runtimes.Descriptor{
		ID:                    Type,
		Name:                  "Hermes",
		ContractVersion:       runtimes.ContractVersion,
		Profile:               runtimes.InteractiveProfile,
		HelmKey:               Type,
		Cancellation:          "notify_only",
		LegacyVersionsKey:     "hermesVersions",
		AuthFailureExitCode:   42,
		RequireCatalogContext: true,
		AuthModes: []runtimes.AuthMode{{
			ID:           kyberv1.AgentAuthTypeAPIKey,
			Name:         "OpenRouter API key",
			Flow:         "api-key",
			InputField:   openRouterInputField,
			SecretSuffix: "openrouter",
			Channels:     []string{"telegram", "discord", "slack"},
		}},
		Features: []runtimes.Feature{
			runtimes.SessionRestart,
			runtimes.SessionResume,
			runtimes.Compaction,
			runtimes.ModelCatalog,
			runtimes.UsageReporting,
		},
	}
}

func (*runtime) Authentication() runtimes.Authentication { return authentication{} }

type authentication struct{}

func (authentication) Validate(mode kyberv1.AgentAuthType, input runtimes.AuthInput) error {
	if mode != kyberv1.AgentAuthTypeAPIKey {
		return fmt.Errorf("authType must be api-key")
	}
	if input[openRouterInputField] == "" {
		return &runtimes.AuthValidationError{
			Field:   "secrets.runtimeAuth." + openRouterInputField,
			Message: "OpenRouter API key is required for a Hermes agent",
		}
	}
	return nil
}

func (authentication) Prepare(_ context.Context, mode kyberv1.AgentAuthType, input runtimes.AuthInput, _ runtimes.AuthOptions) ([]runtimes.Credential, error) {
	if err := (authentication{}).Validate(mode, input); err != nil {
		return nil, err
	}
	return []runtimes.Credential{{
		Suffix: "openrouter",
		Data:   map[string][]byte{"token": []byte(input[openRouterInputField])},
	}}, nil
}

func (authentication) Pending(kyberv1.AgentAuthType, map[string][]byte) bool { return false }
