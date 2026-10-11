package tokenreport

import "time"

// UsageEventRequest carries source evidence and a durable source position.
// AgentUID is an expected lifetime, checked against the authenticated Agent;
// the control plane always takes the stored UID from Kubernetes.
type UsageEventRequest struct {
	AgentUID         string         `json:"agentUID"`
	RouteFingerprint string         `json:"routeFingerprint"`
	Evidence         UsageEvidence  `json:"evidence"`
	Cursor           UsageAckCursor `json:"cursor"`
}

type UsageGapRequest struct {
	AgentUID         string         `json:"agentUID"`
	RouteFingerprint string         `json:"routeFingerprint"`
	Runtime          string         `json:"runtime"`
	SourceIdentity   string         `json:"sourceIdentity"`
	Reason           string         `json:"reason"`
	DetectedAt       time.Time      `json:"detectedAt"`
	Cursor           UsageAckCursor `json:"cursor"`
}
