package runtimes

import "fmt"

// Telegram sidecar wiring shared by the controller and every runtime adapter
// (kyber#684).
//
// This lives here rather than in pkg/controllers/agent because the dependency
// only runs one way: the controller imports pkg/runtimes, so the adapters
// cannot import the controller back. Both ends of the same loopback connection
// have to agree on the port, and the first cut had it as a constant in the
// controller and a hardcoded string literal in each adapter — change the
// constant and the adapters keep pointing at a closed port, with no compile
// error and no failing test. One definition, three readers.

// TelegramMCPPort is the loopback port the kyber-mcp-telegram sidecar serves
// MCP on. Not a container port: the sidecar and the runtime share the pod's
// network namespace, so exposing it would widen the trust boundary for nothing.
//
// That shared namespace is also why the number matters. Every sidecar in an
// agent pod binds into ONE loopback space, so the 1400x range is allocated
// pod-wide, not per-container:
//
//	14002  kyber-mcp-discord   /healthz
//	14003  kyber-mcp-telegram  /healthz
//	14004  kyber-mcp-telegram  /send
//	14005  kyber-mcp-discord   /send
//	14006  kyber-mcp-telegram  /mcp      <- this one
//	14007  kyber-mcp-discord   /mcp
//
// 14005 was the first pick here and it collided with Discord's /send: an agent
// with both channels enabled would have had two containers binding the same
// port, and whichever lost would crash-loop. Check this table before adding a
// port, and add to it when you do.
const TelegramMCPPort = 14006

// TelegramMCPAddr is what the sidecar binds. Loopback-only by construction.
func TelegramMCPAddr() string { return fmt.Sprintf("127.0.0.1:%d", TelegramMCPPort) }

// TelegramMCPURL is what the runtime registers as an MCP server —
// `claude mcp add --transport http` for Claude Code, an [mcp_servers] entry in
// config.toml for Codex.
func TelegramMCPURL() string { return fmt.Sprintf("http://%s/mcp", TelegramMCPAddr()) }

// TelegramAttachmentDir is where the sidecar writes files fetched by the
// download_attachment tool, and where the agent reads them back.
//
// It is a path on the shared "persist" PVC, mounted at the SAME path in both
// containers, because the tool returns this path to the model verbatim and the
// model then reads it. Mount it anywhere else in the sidecar and the download
// succeeds into a directory the agent cannot see.
const TelegramAttachmentDir = "/persist/telegram-attachments"
