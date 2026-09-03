package runtimes

import "fmt"

// StatusSidecarForwarderPort is the pod-loopback listener shared by runtime
// reporters and the platform request/reply MCP tool.
const StatusSidecarForwarderPort = 8091

// RequestMCPURL is registered by both supported runtimes. The listener is
// loopback-only and the status sidecar owns the control-plane credential.
func RequestMCPURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/mcp", StatusSidecarForwarderPort)
}

// A2AMCPURL is the distinct loopback endpoint for outbound A2A tools.
func A2AMCPURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/a2a/mcp", StatusSidecarForwarderPort)
}
