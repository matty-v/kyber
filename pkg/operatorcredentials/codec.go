package operatorcredentials

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"time"
)

// A distinct private type removes public transport methods. Only this codec
// combines metadata with material, and its output goes into Secret data.
type credentialData Credential
type storedCredential struct {
	Record                  credentialData `json:"record"`
	Material                []byte         `json:"material,omitempty"`
	LegacyBrowserGeneration uint64         `json:"legacyBrowserGeneration,omitempty"`
}
type storedDocument struct {
	SchemaVersion                  int                `json:"schemaVersion"`
	Namespace                      string             `json:"namespace"`
	Credentials                    []storedCredential `json:"credentials"`
	Operations                     []Operation        `json:"operations"`
	LegacyBrowserSigningKey        []byte             `json:"legacyBrowserSigningKey,omitempty"`
	LegacyBrowserSigningGeneration uint64             `json:"legacyBrowserSigningGeneration,omitempty"`
	LegacyBrowserIssuedBefore      *time.Time         `json:"legacyBrowserIssuedBefore,omitempty"`
	LegacyBrowserExpiresBefore     *time.Time         `json:"legacyBrowserExpiresBefore,omitempty"`
}

func encode(d Document) ([]byte, Document, error) {
	if len(d.Credentials) > MaxCredentials || len(d.Operations) > MaxOperations {
		return nil, Document{}, ErrCapacity
	}
	for _, c := range d.Credentials {
		if len(c.Scopes) > 64 || len(c.AgentResources) > 256 || len(c.MachineResources) > 256 || len(c.material) > 4096 {
			return nil, Document{}, ErrCapacity
		}
	}
	d = normalized(d)
	if err := validate(d); err != nil {
		return nil, Document{}, err
	}
	wire := storedDocument{LegacyBrowserSigningGeneration: d.legacyBrowserSigningGeneration, LegacyBrowserSigningKey: d.legacyBrowserSigningKey, SchemaVersion: d.SchemaVersion, Namespace: d.Namespace, Operations: d.Operations, Credentials: make([]storedCredential, 0, len(d.Credentials))}
	if !d.legacyBrowserIssuedBefore.IsZero() {
		cutoff := d.legacyBrowserIssuedBefore
		wire.LegacyBrowserIssuedBefore = &cutoff
		expiry := d.legacyBrowserExpiresBefore
		wire.LegacyBrowserExpiresBefore = &expiry
	}
	for _, c := range d.Credentials {
		wire.Credentials = append(wire.Credentials, storedCredential{Record: credentialData(c), Material: c.material, LegacyBrowserGeneration: c.legacyBrowserGeneration})
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		return nil, Document{}, ErrInvalid
	}
	if len(raw) > MaxDocumentBytes {
		return nil, Document{}, ErrCapacity
	}
	return raw, d, nil
}

func decode(raw []byte) (Document, error) {
	if len(raw) > MaxDocumentBytes {
		return Document{}, ErrCapacity
	}
	check := json.NewDecoder(bytes.NewReader(raw))
	if inspectJSON(check, 0) != nil {
		return Document{}, ErrInvalid
	}
	if _, err := check.Token(); err != io.EOF {
		return Document{}, ErrInvalid
	}
	var header struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if json.Unmarshal(raw, &header) != nil {
		return Document{}, ErrInvalid
	}
	if header.SchemaVersion != SchemaVersion {
		return Document{}, ErrUnsupported
	}
	var wire storedDocument
	parser := json.NewDecoder(bytes.NewReader(raw))
	parser.DisallowUnknownFields()
	if parser.Decode(&wire) != nil {
		return Document{}, ErrInvalid
	}
	d := Document{legacyBrowserSigningGeneration: wire.LegacyBrowserSigningGeneration, legacyBrowserSigningKey: bytes.Clone(wire.LegacyBrowserSigningKey), SchemaVersion: wire.SchemaVersion, Namespace: wire.Namespace, Operations: wire.Operations, Credentials: make([]Credential, 0, len(wire.Credentials))}
	if wire.LegacyBrowserIssuedBefore != nil {
		d.legacyBrowserIssuedBefore = *wire.LegacyBrowserIssuedBefore
	}
	if wire.LegacyBrowserExpiresBefore != nil {
		d.legacyBrowserExpiresBefore = *wire.LegacyBrowserExpiresBefore
	}
	for _, c := range wire.Credentials {
		d.Credentials = append(d.Credentials, Credential(c.Record).WithSecretMaterial(c.Material).WithLegacyBrowserGeneration(c.LegacyBrowserGeneration))
	}
	_, d, err := encode(d)
	return d, err
}

// Reject ambiguous duplicate fields (including ASCII case aliases accepted by
// encoding/json), trailing values and excessive nesting. Schema keys are ASCII;
// opaque identifier values and binary material retain their existing bytes.
func inspectJSON(parser *json.Decoder, depth int) error {
	if depth > 16 {
		return ErrInvalid
	}
	token, err := parser.Token()
	if err != nil {
		return ErrInvalid
	}
	delim, container := token.(json.Delim)
	if !container {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for parser.More() {
			key, err := parser.Token()
			if err != nil {
				return ErrInvalid
			}
			name, ok := key.(string)
			if !ok {
				return ErrInvalid
			}
			for _, r := range name {
				if r > 127 {
					return ErrInvalid
				}
			}
			name = strings.ToLower(name)
			if seen[name] {
				return ErrInvalid
			}
			seen[name] = true
			if inspectJSON(parser, depth+1) != nil {
				return ErrInvalid
			}
		}
		end, err := parser.Token()
		if err != nil || end != json.Delim('}') {
			return ErrInvalid
		}
	case '[':
		for parser.More() {
			if inspectJSON(parser, depth+1) != nil {
				return ErrInvalid
			}
		}
		end, err := parser.Token()
		if err != nil || end != json.Delim(']') {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}
