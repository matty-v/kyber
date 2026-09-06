package runtimes

import "time"

type AuthState string

const (
	AuthStarting AuthState = "starting"
	AuthReady    AuthState = "ready"
	AuthExpired  AuthState = "expired"
	AuthFailed   AuthState = "failed"
	AuthAbsent   AuthState = "absent"
)

// AuthObservation is safe operator-facing device-login information. Credential
// tokens and raw provider output must never be included.
type AuthObservation struct {
	State           AuthState `json:"state"`
	VerificationURL string    `json:"verificationUrl,omitempty"`
	UserCode        string    `json:"userCode,omitempty"`
	Detail          string    `json:"detail,omitempty"`
	ExpiresAt       time.Time `json:"expiresAt,omitzero"`
}

// DeviceAuthReader is optional. The tmux profile captures the auth session;
// each integration interprets its own CLI's output.
type DeviceAuthReader interface {
	ParseDeviceAuth(pane string, startedAt, now time.Time) AuthObservation
}
