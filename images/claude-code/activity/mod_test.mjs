import test from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'

let generation = 0
async function harness() {
  const code = await readFile(new URL('./mod/hooks/register.js', import.meta.url), 'utf8')
  const module = await import(`data:text/javascript;base64,${Buffer.from(code).toString('base64')}#${generation++}`)
  const handlers = new Map()
  const sent = []
  let tick, agents = [], failRead = false
  const $ = {
    process: {
      hookEnv: async () => ({ CLAUDE_PID: '123' }),
      run: async (argv, options) => {
        sent.push({ mode: argv[1], body: JSON.parse(options.stdin) })
        return { exitCode: 0 }
      },
    },
    env: { get: async key => key === 'HOME' ? '/home/canary' : undefined },
    clock: { now: async () => Date.now(), every: (_ms, callback) => {
      tick = callback
      return { cancel() { tick = undefined } }
    } },
    agent: { list: async () => {
      if (failRead) throw new Error('native read failed')
      return agents
    } },
  }
  module.register((name, matcher, callback) => handlers.set(name, callback ?? matcher))
  const settle = () => new Promise(resolve => setImmediate(resolve))
  async function fire(name, event, response = event) {
    const handler = handlers.get(name)
    const result = await handler($, event, async _value => response)
    await settle()
    return result
  }
  await fire('session.start', {})
  await fire('ui.render', { props: { isWorking: false } })
  return { handlers, sent, fire, settle,
    setAgents(value) { agents = value },
    failRead(value) { failRead = value },
    async tick() { await tick?.(); await settle() },
    latest() { return sent.filter(frame => frame.mode === 'publish').at(-1)?.body },
  }
}

test('native handoff and cancellation pass through without tool interceptors', async () => {
  const h = await harness()
  assert.equal(h.handlers.has('tool.call'), false)
  assert.equal(h.handlers.has('tool.run'), false)
  assert.equal(h.handlers.has('classic.PreToolUse'), false)
  const start = { turnId: 'turn' }
  assert.equal(await h.fire('turn.start', start), start)
  await h.tick()
  assert.equal(h.latest().main, true)
  const end = { turnId: 'turn', isAborted: true }
  assert.equal(await h.fire('turn.complete', end), end)
  // A delegated child can keep the native spinner busy. It must not resurrect
  // the completed main turn or move background-only work into Working now.
  await h.fire('agent.spawn', { background: true, prompt: 'PRIVATE' }, { agentId: 'child' })
  h.setAgents([{ id: 'child', kind: 'local_agent', status: 'running' }])
  await h.fire('ui.render', { props: { isWorking: true } })
  await h.tick()
  assert.equal(h.latest().main, false)
  assert.equal(h.latest().complete, true)
  assert.deepEqual(h.latest().agents, [{ id: 'child', kind: 'local_agent', status: 'running', background: true }])
})

test('tool-result observation forwards only identities and preserves the row', async () => {
  const h = await harness()
  const event = { message: { content: [{ type: 'tool_result', tool_use_id: 'native-call', content: 'PRIVATE OUTPUT' }] } }
  assert.equal(await h.fire('session.append', event), event)
  const frame = h.sent.find(frame => frame.mode === 'returned')
  assert.deepEqual(frame.body, { ids: ['native-call'], nativePID: '123' })
  assert.equal(JSON.stringify(frame).includes('PRIVATE OUTPUT'), false)
})

test('a failed native read emits incomplete rather than renewing cached work', async () => {
  const h = await harness()
  await h.fire('turn.start', { turnId: 'turn' })
  await h.tick()
  assert.equal(h.latest().main, true)
  h.failRead(true)
  await h.tick()
  assert.equal(h.latest().complete, false)
  assert.equal(h.latest().main, false)
  h.failRead(false)
  await h.tick()
  assert.equal(h.latest().main, true)
  assert.equal(h.latest().complete, true)
})

test('a stale teammate roster is not a live in-process turn', async () => {
  const h = await harness()
  h.setAgents([{ id: 'pane', kind: 'in_process_teammate', teammateId: 'peer', status: 'running' }])
  await h.tick()
  assert.equal(h.latest().complete, false)
  await h.fire('agent.spawn', { background: true }, { agentId: 'pane' })
  await h.fire('turn.start', { turnId: 'child-turn', agentId: 'pane' })
  await h.tick()
  assert.equal(h.latest().complete, true)
})


test('native spawn distinguishes synchronous and background agents, unknown coverage fails closed', async () => {
  const h = await harness()
  h.setAgents([{ id: 'sync', status: 'running' }, { id: 'async', status: 'running' }])
  await h.tick()
  assert.equal(h.latest().complete, false)
  const result = { agentId: 'sync', model: 'native' }
  assert.equal(await h.fire('agent.spawn', { background: false, prompt: 'PRIVATE' }, result), result)
  await h.fire('agent.spawn', { background: true, prompt: 'PRIVATE' }, { agentId: 'async' })
  await h.tick()
  assert.equal(h.latest().complete, true)
  assert.deepEqual(h.latest().agents.map(a => a.background), [false, true])
  assert.equal(JSON.stringify(h.sent).includes('PRIVATE'), false)
})
