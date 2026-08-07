package runtimes

import "fmt"

// DiscordMCPPort is the loopback port served by kyber-mcp-discord. Channel
// sidecars and runtime containers share one pod network namespace, so this is
// a pod-wide allocation; keep it distinct from the 14002-14006 channel ports.
const DiscordMCPPort = 14007

// DiscordMCPAddr is what the Discord sidecar binds inside the agent pod.
func DiscordMCPAddr() string { return fmt.Sprintf("127.0.0.1:%d", DiscordMCPPort) }

// DiscordMCPURL is registered with each supported runtime as an HTTP MCP
// server. The listener remains loopback-only, so the pod is the auth boundary.
func DiscordMCPURL() string { return fmt.Sprintf("http://%s/mcp", DiscordMCPAddr()) }

// DiscordAttachmentDir is shared between the sidecar that downloads inbound
// attachments and the runtime that reads them.
const DiscordAttachmentDir = "/persist/discord-attachments"
