package runtimes

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

const ContractVersion = "1.0"
const InteractiveProfile = "interactive-tmux-v1"

type Feature string

const (
	SessionRestart       Feature = "session-restart"
	SessionResume        Feature = "session-resume"
	Compaction           Feature = "compaction"
	JobTurnHooks         Feature = "job-turn-hooks"
	TaskReceipts         Feature = "task-receipts"
	TaskTools            Feature = "task-tools"
	ModelCatalog         Feature = "model-catalog"
	UsageReporting       Feature = "usage-reporting"
	RuntimeRepairFeature Feature = "runtime-repair"
)

var runtimeID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// Descriptor is trusted, code-owned integration metadata. It declares support,
// not observed readiness or provider availability. No credentials belong here.
type Descriptor struct {
	ID                    string     `json:"id"`
	Name                  string     `json:"name"`
	ContractVersion       string     `json:"contractVersion"`
	Profile               string     `json:"profile"`
	AuthModes             []AuthMode `json:"authModes"`
	Features              []Feature  `json:"features"`
	Cancellation          string     `json:"cancellation"`
	ModelPrefix           string     `json:"-"`
	LegacyCatalogKey      string     `json:"legacyCatalogKey,omitempty"`
	LegacyVersionsKey     string     `json:"legacyVersionsKey,omitempty"`
	LegacyDefaultsKey     string     `json:"-"`
	HelmKey               string     `json:"-"`
	TranscriptPath        string     `json:"-"` // relative to persisted HOME
	TranscriptExchange    string     `json:"-"` // trusted jq expression yielding one exchange
	RequireCatalogContext bool       `json:"-"`
	AuthFailureExitCode   int32      `json:"-"`
}
type AuthMode struct {
	ID                  kyberv1.AgentAuthType `json:"id"`
	Name                string                `json:"name"`
	Flow                string                `json:"flow"`                 // api-key, authorization-code, device-code
	InputField          string                `json:"inputField,omitempty"` // existing request field, no value
	AuthorizationURL    string                `json:"authorizationUrl,omitempty"`
	AuthorizationParams map[string]string     `json:"authorizationParams,omitempty"`
	SecretSuffix        string                `json:"-"`
	ReauthorizePath     string                `json:"reauthorizePath,omitempty"`
	Channels            []string              `json:"channels,omitempty"`
}

func (m AuthMode) SupportsChannel(channel string) bool {
	for _, supported := range m.Channels {
		if supported == channel {
			return true
		}
	}
	return false
}

func (d Descriptor) Supports(f Feature) bool {
	for _, v := range d.Features {
		if v == f {
			return true
		}
	}
	return false
}
func (d Descriptor) Auth(mode kyberv1.AgentAuthType) (AuthMode, bool) {
	for _, m := range d.AuthModes {
		if m.ID == mode {
			return m, true
		}
	}
	return AuthMode{}, false
}
func (d Descriptor) Validate() error {
	if !runtimeID.MatchString(d.ID) || d.Name == "" || d.ContractVersion != ContractVersion || d.Profile != InteractiveProfile {
		return fmt.Errorf("invalid runtime descriptor")
	}
	if len(d.AuthModes) == 0 {
		return fmt.Errorf("runtime %s has no auth modes", d.ID)
	}
	seen := map[kyberv1.AgentAuthType]bool{}
	for _, m := range d.AuthModes {
		if m.ID == "" || m.SecretSuffix == "" || seen[m.ID] {
			return fmt.Errorf("runtime %s has invalid auth modes", d.ID)
		}
		seen[m.ID] = true
		channels := map[string]bool{}
		for _, channel := range m.Channels {
			if channel != "telegram" && channel != "discord" && channel != "slack" {
				return fmt.Errorf("runtime %s auth mode %s has invalid channel %q", d.ID, m.ID, channel)
			}
			if channels[channel] {
				return fmt.Errorf("runtime %s auth mode %s repeats channel %q", d.ID, m.ID, channel)
			}
			channels[channel] = true
		}
	}
	return nil
}

// DescribedRuntime is separate from Runtime so old test doubles remain usable.
// Production discovery fails closed for an undescribed runtime.
type DescribedRuntime interface {
	Descriptor() Descriptor
	Authentication() Authentication
}

func Describe(id string) (Descriptor, bool) {
	rt, ok := Get(id)
	if !ok {
		return Descriptor{}, false
	}
	provider, ok := rt.(DescribedRuntime)
	if !ok {
		return Descriptor{}, false
	}
	d := provider.Descriptor()
	if d.ID != id || d.Validate() != nil {
		return Descriptor{}, false
	}
	d.AuthModes = append([]AuthMode(nil), d.AuthModes...)
	for i, m := range d.AuthModes {
		if m.AuthorizationParams != nil {
			params := map[string]string{}
			for k, v := range m.AuthorizationParams {
				params[k] = v
			}
			d.AuthModes[i].AuthorizationParams = params
		}
		d.AuthModes[i].Channels = append([]string(nil), m.Channels...)
	}
	d.Features = append([]Feature{}, d.Features...)
	return d, true
}
func Descriptors() []Descriptor {
	out := []Descriptor{}
	for _, rt := range All() {
		if d, ok := Describe(rt.Type()); ok {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// AuthInput contains only request values selected by the API compatibility
// boundary. Authentication implementations own provider formats and exchange.
type AuthInput map[string]string
type AuthOptions struct{ TokenURL string }
type Credential struct {
	Suffix string
	Data   map[string][]byte
}
type Authentication interface {
	Validate(kyberv1.AgentAuthType, AuthInput) error
	Prepare(context.Context, kyberv1.AgentAuthType, AuthInput, AuthOptions) ([]Credential, error)
	Pending(kyberv1.AgentAuthType, map[string][]byte) bool
}

func AuthenticationFor(id string) (Authentication, bool) {
	rt, ok := Get(id)
	if !ok {
		return nil, false
	}
	p, ok := rt.(DescribedRuntime)
	if !ok {
		return nil, false
	}
	auth := p.Authentication()
	return auth, auth != nil
}
func CredentialName(id, name string, mode kyberv1.AgentAuthType) string {
	d, ok := Describe(id)
	if !ok {
		return ""
	}
	m, ok := d.Auth(mode)
	if !ok {
		return ""
	}
	return name + "-" + m.SecretSuffix
}
func TranscriptRoot(id, persistHome string) string {
	d, ok := Describe(id)
	if !ok || d.TranscriptPath == "" {
		return ""
	}
	return strings.TrimRight(persistHome, "/") + "/" + d.TranscriptPath
}

// AuthValidationError carries a compatibility field path without credential data.
type AuthValidationError struct{ Field, Message string }

func (e *AuthValidationError) Error() string { return e.Message }
