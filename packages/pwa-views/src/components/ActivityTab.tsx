// Activity tab for AgentDetail — read-only observation surface. Just the
// structured, multi-session conversation history now (the Pod Boot Log and
// Raw terminal sub-views were removed 2026-07-21 per the Activity redesign).
// Refresh and Export live in the history header (AgentHistory).

import { AgentHistory } from './AgentHistory'

interface Props {
  agentName: string
}

export function ActivityTab({ agentName }: Props) {
  return <AgentHistory agentName={agentName} />
}
