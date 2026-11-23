// Observe native state without intercepting tool.call/tool.run: those hooks
// can disable the native background handoff they are trying to observe.
const HELPER = '/usr/local/bin/kyber-claude-work'
let main = false
let mainBoundarySeen = false
let covered = false
let eventGap = false
let lastWorkedAt
let timer
let dirty = false
let worker
const turns = new Map()
const backgrounds = new Map()

async function invoke($, mode, body) {
  const env = await $.process.hookEnv()
  const result = await $.process.run([HELPER, mode], {
    env: { ...env, KYBER_POD_UID: await $.env.get('KYBER_POD_UID') ?? '',
      HOME: await $.env.get('HOME') ?? '/home/kyber',
      CLAUDE_CONFIG_DIR: await $.env.get('CLAUDE_CONFIG_DIR')
        ?? `${await $.env.get('HOME') ?? '/home/kyber'}/.claude`,
      KYBER_CLAUDE_WORK_ROOT: await $.env.get('KYBER_CLAUDE_WORK_ROOT') ?? '/tmp/kyber-native-activity/claude' },
    stdin: JSON.stringify({ ...body, nativePID: env.CLAUDE_PID }),
    timeoutMs: 2000,
  })
  if (result.exitCode !== 0) throw new Error('native observer helper unavailable')
}

function publish($) {
  dirty = true
  if (worker) return worker
  worker = (async () => {
    while (dirty) {
      dirty = false
      try {
        // Acquisition time, not helper/HTTP delivery time, owns the lease.
        const observedAt = new Date(await $.clock.now()).toISOString()
        const agents = await $.agent.list()
        for (const a of agents) {
          if (['completed', 'failed', 'killed'].includes(a.status)) backgrounds.delete(a.id)
        }
        const current = agents.map(a => ({ id: a.id, kind: a.kind, status: a.status,
          background: backgrounds.get(a.id) }))
        const unknownOwnership = current.some(a => ['running', 'pending'].includes(a.status)
          && typeof a.background !== 'boolean')
        const paneGap = agents.some(a => a.teammateId && a.status === 'running'
          && !Array.from(turns.values()).includes(a.id))
        const ambiguousInitialTurn = !mainBoundarySeen && main
          && agents.some(a => a.status === 'running' || a.status === 'pending')
        await invoke($, 'publish', { observedAt, lastWorkedAt, main,
          agents: current, complete: covered && !paneGap && !ambiguousInitialTurn && !unknownOwnership, eventGap })
        eventGap = false
      } catch {
        // A failed native read is a fresh incomplete observation, never a
        // replay of the last successful counts with a new timestamp.
        try {
          await invoke($, 'publish', { observedAt: new Date(await $.clock.now()).toISOString(),
            main: false, agents: [], complete: false, eventGap })
        } catch { /* A dead helper leaves the previous native sample to expire. */ }
      }
    }
  })().finally(() => { worker = undefined })
  return worker
}

export function register(on) {
  on('session.start', ($, e, next) => {
    timer?.cancel()
    timer = $.clock.every(5000, () => publish($))
    void publish($)
    return next(e)
  })
  on('agent.spawn', async ($, e, next) => {
    // next resolves at native start, not at completion. Only keep the opaque
    // identity and native scheduling flag; never retain a delegated prompt.
    const result = await next(e)
    if (result.agentId) {
      if (backgrounds.size >= 1024) eventGap = true
      else if (typeof e.background === 'boolean') backgrounds.set(result.agentId, e.background)
      else eventGap = true
      void publish($)
    }
    return result
  })
  on('turn.start', ($, e, next) => {
    covered = true
    if (typeof e.turnId !== 'string' || !e.turnId || turns.size >= 1024) eventGap = true
    else turns.set(e.turnId, e.agentId ?? 'main')
    if (!e.agentId) { mainBoundarySeen = true; main = true }
    lastWorkedAt = new Date().toISOString()
    void publish($)
    return next(e)
  })
  on('turn.complete', ($, e, next) => {
    turns.delete(e.turnId)
    if (!e.agentId) { mainBoundarySeen = true; main = false }
    lastWorkedAt = new Date().toISOString()
    void publish($)
    return next(e)
  })
  on('ui.render', { component: 'AbovePrompt' }, ($, e, next) => {
    // This is a native boolean, including on observer load/reload, not a
    // terminal-text scrape. Pass the original drawing through unchanged.
    covered = true
    if (!mainBoundarySeen) main = e.props.isWorking
    return next(e)
  })
  on('session.append', { door: 'tool-result' }, async ($, e, next) => {
    const ids = e.message.content
      .filter(block => block.type === 'tool_result' && typeof block.tool_use_id === 'string')
      .map(block => block.tool_use_id)
    if (ids.length) {
      try { await invoke($, 'returned', { ids }) } catch { eventGap = true }
    }
    return next(e)
  })
  on('session.end', async ($, e, next) => {
    timer?.cancel()
    dirty = false
    main = false
    turns.clear()
    try {
      await worker
      await invoke($, 'publish', { observedAt: new Date(await $.clock.now()).toISOString(),
        lastWorkedAt, main: false, agents: [], complete: covered, eventGap, closed: true })
    } catch { /* Native exit must not wait on a failed observer. */ }
    return next(e)
  })
}
