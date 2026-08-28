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
  if (action === 'compact-session') {
    // The non-destructive sibling of restart-session: the conversation is
    // summarized rather than discarded, so the confirm should not borrow
    // restart's context-loss warning.
    return (
      `Compact the session for "${agentName}"? The agent summarizes its ` +
      `conversation so far and keeps working with a smaller context. Nothing ` +
      `is discarded outright and the pod stays up. Compaction runs on the ` +
      `agent's side and may take a minute.`
    )
  }
  if (action === 'restart') {
    return (
      `Restart the pod for "${agentName}"? This rolls the container (fresh ` +
      `image, full init path, session brief + resume). Use "Restart session" ` +
      `for a lighter-weight context reset that keeps the pod up.`
    )
  }
  if (action === 'retry-startup') {
    // kyber#26: the NeedsAuth "try again with what you have" control. Worded
    // for a pod that is usually already gone (the agent exited on a credential
    // failure), so it promises a rebuild rather than a roll — and points at the
    // Re-authorize panel as the other half of the choice, since the operator
    // has to decide whether the credential is actually bad.
    return (
      `Rebuild the pod for "${agentName}" using the credentials it already ` +
      `has? Use this when you have already supplied a working credential and ` +
      `the agent is still parked. If the credential is genuinely bad, use the ` +
      `Re-authorize panel instead — this will just land back in NeedsAuth. ` +
      `Memory and identity files on disk are not affected.`
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
  if (action === 'repair-runtime') {
    return (
      `Repair the runtime harness for "${agentName}"? Kyber will mount its ` +
      `persistent disk in a short-lived maintenance pod, reinstall and verify ` +
      `the configured harness version, then restart the agent. Credentials, ` +
      `memory, identity files, and conversation state are preserved. If repair ` +
      `fails, the agent remains parked in BrokenRuntime with its diagnostic.`
    )
  }
  return `This will ${action} agent "${agentName}".`
}
