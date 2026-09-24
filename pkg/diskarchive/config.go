package diskarchive

import "encoding/json"

// ConfigVersion is the current version of Config. Version 0 (absent) is the
// original untyped map, whose keys Config still decodes.
const ConfigVersion = 2

// Config is the agent's non-secret configuration, recorded so an import can
// rebuild the agent and not only its disk. Secret values never appear:
// user secrets are listed by key, and Secret references keep only their names.
type Config struct {
	Version             int             `json:"version,omitempty"`
	Runtime             string          `json:"runtime,omitempty"`
	RuntimeVersion      string          `json:"runtimeVersion,omitempty"`
	Model               string          `json:"model,omitempty"`
	Machine             string          `json:"machine,omitempty"`
	Resources           ConfigResources `json:"resources"`
	StartupPrompt       string          `json:"startupPrompt,omitempty"`
	SessionResume       bool            `json:"sessionResume,omitempty"`
	RequestReplyEnabled bool            `json:"requestReplyEnabled,omitempty"`
	IdentityRepo        string          `json:"identityRepo,omitempty"`
	AuthType            string          `json:"authType,omitempty"`
	Channels            map[string]bool `json:"channels,omitempty"`
	Jobs                []ConfigJob     `json:"jobs,omitempty"`
	// InboundBindings names the bindings; InboundBindingSpecs carries their
	// definitions. Archives before version 2 have names only.
	InboundBindings []string `json:"inboundBindings,omitempty"`

	SoulDescription     string          `json:"soulDescription,omitempty"`
	Profile             *ConfigProfile  `json:"profile,omitempty"`
	PublicCapabilities  json.RawMessage `json:"publicCapabilities,omitempty"`
	A2APeers            json.RawMessage `json:"a2aPeers,omitempty"`
	InboundBindingSpecs json.RawMessage `json:"inboundBindingSpecs,omitempty"`
	UserSecrets         []ConfigSecret  `json:"userSecrets,omitempty"`
}

// ConfigResources is the agent's resource request as quantity strings.
type ConfigResources struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	Disk   string `json:"disk,omitempty"`
}

// ConfigJob is one scheduled job.
type ConfigJob struct {
	Name              string `json:"name"`
	Schedule          string `json:"schedule"`
	Prompt            string `json:"prompt"`
	Exclusive         bool   `json:"exclusive,omitempty"`
	ClearContextAfter bool   `json:"clearContextAfter,omitempty"`
	Paused            bool   `json:"paused,omitempty"`
}

// ConfigProfile is the operator-facing profile. The avatar image travels with
// it: it is small (the API caps it at 1 MiB) and not secret.
type ConfigProfile struct {
	Alias             string `json:"alias,omitempty"`
	Description       string `json:"description,omitempty"`
	AvatarContentType string `json:"avatarContentType,omitempty"`
	Avatar            []byte `json:"avatar,omitempty"`
	// HasAvatar is set on API views, which leave the image bytes out.
	HasAvatar bool `json:"hasAvatar,omitempty"`
}

// ConfigSecret describes one user secret without its value.
type ConfigSecret struct {
	Key          string `json:"key"`
	Kind         string `json:"kind"`
	Size         int    `json:"size"`
	SHA256Prefix string `json:"sha256Prefix,omitempty"`
}
