package runtimes

import "fmt"

// SlackMCPPort is reserved for the loopback Slack Socket Mode sidecar.
const SlackMCPPort = 14008

func SlackMCPAddr() string { return fmt.Sprintf("127.0.0.1:%d", SlackMCPPort) }
func SlackMCPURL() string  { return fmt.Sprintf("http://%s/mcp", SlackMCPAddr()) }

const SlackAttachmentDir = "/persist/slack-attachments"
