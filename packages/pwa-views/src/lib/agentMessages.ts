export function agentActionConfirmMessage(action: string, agentName: string): string {
  if (action === 'delete') {
    return (
      `This will permanently delete agent "${agentName}" and wipe its ` +
      `persistent disk, running session, and Kyber-managed secrets (OAuth, ` +
      `Telegram, Anthropic). The agent's GitHub identity repo is NOT deleted. ` +
      `This cannot be undone.`
    )
  }
  if (action === 'restart-session') {
    // #128: a context reset, not a pod roll. Callers who want a full reset
    // reach for "Restart pod" (the renamed /restart action) instead.
    return (
      `Reset the in-flight session for "${agentName}"? The pod stays up; the ` +
      `conversation / context is lost. Memory and identity files on disk are ` +
      `not affected.`
    )
  }
  if (action === 'restart') {
    return (
      `Restart the pod for "${agentName}"? This rolls the container (fresh ` +
      `image, full init path, session brief + resume). Use "Restart session" ` +
      `for a lighter-weight context reset that keeps the pod up.`
    )
  }
  if (action === 'force-needs-auth') {
    // #395: wedged-agent recovery. Tears down a running pod and parks the
    // agent until an operator re-authorizes — hence the confirm gate.
    return (
      `Force "${agentName}" into re-authorization? This deletes its running ` +
      `pod and drops the agent to NeedsAuth — it stays parked until you ` +
      `re-authorize it (via the Re-authorize flow), which rebuilds the pod ` +
      `with fresh credentials. Use this to recover a wedged agent. Memory and ` +
      `identity files on disk are not affected.`
    )
  }
  return `This will ${action} agent "${agentName}".`
}
