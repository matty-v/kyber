package capabilities

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

const (
	SchemaV1Alpha1 = "v1alpha1"
	MaxPublicBytes = 8 << 10
)

var (
	idPattern      = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)
	versionPattern = regexp.MustCompile(`^[0-9A-Za-z](?:[0-9A-Za-z.+_-]{0,63})?$`)
	allowedModes   = map[string]bool{
		"text/plain": true, "text/markdown": true, "application/json": true,
		"application/octet-stream": true, "image/png": true, "image/jpeg": true,
		"image/webp": true, "audio/mpeg": true, "audio/wav": true, "audio/ogg": true,
	}
	knownFeatures = map[string]bool{
		"durable": true, "progress": true, "typed-results": true, "files": true,
		"cancellation": true, "multi-turn": true, "authorization-request": true,
		"event-replay": true,
	}
)

// PublicManifest is the allowlisted stable contract. It intentionally has no
// evidence, runtime, model, namespace, pod, path, prompt, or tool fields.
type PublicManifest struct {
	SchemaVersion string             `json:"schemaVersion"`
	Identity      PublicIdentity     `json:"identity"`
	Capabilities  []PublicCapability `json:"capabilities"`
}

type PublicIdentity struct {
	DisplayName      string `json:"displayName"`
	Description      string `json:"description"`
	DocumentationURL string `json:"documentationUrl,omitempty"`
}

type PublicCapability struct {
	ID           string   `json:"id"`
	Version      string   `json:"version"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	InputModes   []string `json:"inputModes"`
	OutputModes  []string `json:"outputModes"`
	TaskFeatures []string `json:"taskFeatures,omitempty"`
}

func NormalizeAndValidate(in *kyberv1.AgentPublicCapabilities) (PublicManifest, string, error) {
	if in == nil {
		return PublicManifest{}, "", fmt.Errorf("manifest is not declared")
	}
	if in.SchemaVersion != SchemaV1Alpha1 {
		return PublicManifest{}, "", fmt.Errorf("unsupported schemaVersion %q", in.SchemaVersion)
	}
	identity := in.Identity
	identity.DisplayName = strings.TrimSpace(identity.DisplayName)
	identity.Description = strings.TrimSpace(identity.Description)
	identity.DocumentationURL = strings.TrimSpace(identity.DocumentationURL)
	if identity.DisplayName == "" || len(identity.DisplayName) > 128 || invalidPublicText(identity.DisplayName) {
		return PublicManifest{}, "", fmt.Errorf("identity.displayName must be non-empty bounded plain text")
	}
	if identity.Description == "" || len(identity.Description) > 1024 || invalidPublicText(identity.Description) {
		return PublicManifest{}, "", fmt.Errorf("identity.description must be non-empty bounded plain text")
	}
	if identity.DocumentationURL != "" {
		if err := validatePublicURL(identity.DocumentationURL); err != nil {
			return PublicManifest{}, "", fmt.Errorf("identity.documentationUrl: %w", err)
		}
	}
	if len(in.Capabilities) > 50 {
		return PublicManifest{}, "", fmt.Errorf("capabilities exceeds 50 entries")
	}
	out := PublicManifest{SchemaVersion: SchemaV1Alpha1, Identity: PublicIdentity{DisplayName: identity.DisplayName, Description: identity.Description, DocumentationURL: identity.DocumentationURL}, Capabilities: make([]PublicCapability, 0, len(in.Capabilities))}
	seen := make(map[string]struct{}, len(in.Capabilities))
	for i, declared := range in.Capabilities {
		if err := validateEvidence(declared.Evidence); err != nil {
			return PublicManifest{}, "", fmt.Errorf("capabilities[%d].evidence: %w", i, err)
		}
		capability, err := normalizeCapability(declared)
		if err != nil {
			return PublicManifest{}, "", fmt.Errorf("capabilities[%d]: %w", i, err)
		}
		if _, ok := seen[capability.ID]; ok {
			return PublicManifest{}, "", fmt.Errorf("duplicate capability id %q", capability.ID)
		}
		seen[capability.ID] = struct{}{}
		out.Capabilities = append(out.Capabilities, capability)
	}
	sort.Slice(out.Capabilities, func(i, j int) bool { return out.Capabilities[i].ID < out.Capabilities[j].ID })
	encoded, err := json.Marshal(out)
	if err != nil {
		return PublicManifest{}, "", err
	}
	if len(encoded) > MaxPublicBytes {
		return PublicManifest{}, "", fmt.Errorf("normalized public payload exceeds %d bytes", MaxPublicBytes)
	}
	sum := sha256.Sum256(encoded)
	return out, "sha256:" + hex.EncodeToString(sum[:]), nil
}

func validateEvidence(evidence *kyberv1.AgentPublicCapabilityEvidence) error {
	if evidence == nil {
		return nil
	}
	sets := []struct {
		name   string
		values []string
		max    int
	}{
		{"requiredSkills", evidence.RequiredSkills, 16},
		{"requiredConnectors", evidence.RequiredConnectors, 16},
		{"requiredPlatformFeatures", evidence.RequiredPlatformFeatures, 16},
		{"runtimeAdapters", evidence.RuntimeAdapters, 8},
	}
	for _, set := range sets {
		if len(set.values) > set.max {
			return fmt.Errorf("%s exceeds %d entries", set.name, set.max)
		}
		seen := map[string]struct{}{}
		for _, value := range set.values {
			if !idPattern.MatchString(value) {
				return fmt.Errorf("%s value %q must be a lowercase slug", set.name, value)
			}
			if _, ok := seen[value]; ok {
				return fmt.Errorf("%s contains duplicate %q", set.name, value)
			}
			seen[value] = struct{}{}
		}
	}
	for _, feature := range evidence.RequiredPlatformFeatures {
		if !knownFeatures[feature] {
			return fmt.Errorf("requiredPlatformFeatures contains unknown feature %q", feature)
		}
	}
	for _, runtime := range evidence.RuntimeAdapters {
		if runtime != "claude-code" && runtime != "codex" {
			return fmt.Errorf("runtimeAdapters contains unsupported runtime %q", runtime)
		}
	}
	return nil
}

func normalizeCapability(in kyberv1.AgentPublicCapability) (PublicCapability, error) {
	in.ID = strings.TrimSpace(in.ID)
	if !idPattern.MatchString(in.ID) {
		return PublicCapability{}, fmt.Errorf("id %q is not a lowercase slug", in.ID)
	}
	in.Version = strings.TrimSpace(in.Version)
	if !versionPattern.MatchString(in.Version) {
		return PublicCapability{}, fmt.Errorf("version %q is invalid", in.Version)
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Description = strings.TrimSpace(in.Description)
	if in.Name == "" || len(in.Name) > 128 || invalidPublicText(in.Name) {
		return PublicCapability{}, fmt.Errorf("name must be non-empty bounded plain text")
	}
	if in.Description == "" || len(in.Description) > 1024 || invalidPublicText(in.Description) {
		return PublicCapability{}, fmt.Errorf("description must be non-empty bounded plain text")
	}
	inputs, err := normalizeModes(in.InputModes)
	if err != nil {
		return PublicCapability{}, fmt.Errorf("inputModes: %w", err)
	}
	outputs, err := normalizeModes(in.OutputModes)
	if err != nil {
		return PublicCapability{}, fmt.Errorf("outputModes: %w", err)
	}
	features, err := normalizeFeatures(in.TaskFeatures)
	if err != nil {
		return PublicCapability{}, fmt.Errorf("taskFeatures: %w", err)
	}
	return PublicCapability{ID: in.ID, Version: in.Version, Name: in.Name, Description: in.Description, InputModes: inputs, OutputModes: outputs, TaskFeatures: features}, nil
}

func normalizeModes(values []string) ([]string, error) {
	if len(values) == 0 || len(values) > 16 {
		return nil, fmt.Errorf("must contain 1 to 16 values")
	}
	return normalizeRegistry(values, func(value string) bool { return allowedModes[value] }, "unsupported MIME mode")
}

func normalizeFeatures(values []string) ([]string, error) {
	if len(values) > 16 {
		return nil, fmt.Errorf("must contain at most 16 values")
	}
	return normalizeRegistry(values, func(value string) bool { return knownFeatures[value] }, "unsupported feature")
}

func normalizeRegistry(values []string, allowed func(string) bool, label string) ([]string, error) {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, raw := range values {
		value := strings.ToLower(strings.TrimSpace(raw))
		if !allowed(value) {
			return nil, fmt.Errorf("%s %q", label, raw)
		}
		if _, ok := seen[value]; ok {
			return nil, fmt.Errorf("duplicate value %q", value)
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out, nil
}

func invalidPublicText(value string) bool {
	if strings.ContainsAny(value, "<>`") {
		return true
	}
	for _, r := range value {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return true
		}
	}
	lower := strings.ToLower(value)
	return strings.Contains(lower, "-----begin ") || strings.Contains(lower, "file://") || strings.Contains(lower, "/var/run/")
}

func validatePublicURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("must be an absolute HTTPS URL without credentials or fragment")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return fmt.Errorf("internal host is not allowed")
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast()) {
		return fmt.Errorf("private address is not allowed")
	}
	return nil
}
