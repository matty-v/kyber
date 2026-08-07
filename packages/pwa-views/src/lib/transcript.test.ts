import { describe, it, expect } from 'vitest'
import { parseTranscript, transcriptToText } from './transcript'

const line = (o: unknown) => JSON.stringify(o)

describe('parseTranscript — user/assistant text', () => {
  it('parses a string user message and an array assistant text block into one session', () => {
    const jsonl = [
      line({ type: 'user', sessionId: 's1', timestamp: '2026-07-13T18:00:00Z', message: { content: 'hello there' } }),
      line({
        type: 'assistant',
        sessionId: 's1',
        timestamp: '2026-07-13T18:00:05Z',
        message: { content: [{ type: 'text', text: 'hi!' }] },
      }),
    ].join('\n')

    const sessions = parseTranscript(jsonl)
    expect(sessions).toHaveLength(1)
    expect(sessions[0].id).toBe('s1')
    expect(sessions[0].turns).toEqual([
      { kind: 'user', ts: '2026-07-13T18:00:00Z', text: 'hello there' },
      { kind: 'assistant', ts: '2026-07-13T18:00:05Z', text: 'hi!' },
    ])
    expect(sessions[0].startedAt).toBe('2026-07-13T18:00:00Z')
    expect(sessions[0].endedAt).toBe('2026-07-13T18:00:05Z')
  })

  it('joins multiple text blocks in one assistant message with newlines', () => {
    const jsonl = line({
      type: 'assistant',
      sessionId: 's1',
      timestamp: '2026-07-13T18:00:05Z',
      message: { content: [{ type: 'text', text: 'line one' }, { type: 'text', text: 'line two' }] },
    })
    const sessions = parseTranscript(jsonl)
    expect(sessions[0].turns[0]).toEqual({ kind: 'assistant', ts: '2026-07-13T18:00:05Z', text: 'line one\nline two' })
  })

  it('returns [] for empty input', () => {
    expect(parseTranscript('')).toEqual([])
  })
})

describe('parseTranscript — Codex rollout JSONL', () => {
  it('groups event messages under the preceding session metadata', () => {
    const jsonl = [
      line({ timestamp: '2026-08-03T23:35:00Z', type: 'session_meta', payload: { session_id: 'codex-1' } }),
      line({ timestamp: '2026-08-03T23:36:00Z', type: 'event_msg', payload: { type: 'user_message', message: 'hello from Telegram' } }),
      line({ timestamp: '2026-08-03T23:36:02Z', type: 'event_msg', payload: { type: 'agent_message', message: 'hello back', phase: 'final_answer' } }),
      line({ timestamp: '2026-08-03T23:36:03Z', type: 'event_msg', payload: { type: 'token_count' } }),
    ].join('\n')

    const sessions = parseTranscript(jsonl)
    expect(sessions).toHaveLength(1)
    expect(sessions[0]).toMatchObject({
      id: 'codex-1',
      firstUserText: 'hello from Telegram',
      lastAssistantText: 'hello back',
      turns: [
        { kind: 'user', ts: '2026-08-03T23:36:00Z', text: 'hello from Telegram' },
        { kind: 'assistant', ts: '2026-08-03T23:36:02Z', text: 'hello back' },
      ],
    })
  })
})

describe('parseTranscript — thinking + tools', () => {
  const line = (o: unknown) => JSON.stringify(o)

  it('emits a thinking turn for a thinking block', () => {
    const jsonl = line({
      type: 'assistant',
      sessionId: 's1',
      timestamp: '2026-07-13T18:00:01Z',
      message: { content: [{ type: 'thinking', thinking: 'let me think' }, { type: 'text', text: 'answer' }] },
    })
    const [s] = parseTranscript(jsonl)
    expect(s.turns).toEqual([
      { kind: 'thinking', ts: '2026-07-13T18:00:01Z', text: 'let me think' },
      { kind: 'assistant', ts: '2026-07-13T18:00:01Z', text: 'answer' },
    ])
  })

  it('pairs a tool_use with its later tool_result by id', () => {
    const jsonl = [
      line({
        type: 'assistant',
        sessionId: 's1',
        timestamp: '2026-07-13T18:00:02Z',
        message: { content: [{ type: 'tool_use', id: 'tu_1', name: 'WebSearch', input: { query: 'weather' } }] },
      }),
      line({
        type: 'user',
        sessionId: 's1',
        timestamp: '2026-07-13T18:00:07Z',
        message: { content: [{ type: 'tool_result', tool_use_id: 'tu_1', content: 'sunny, 75F' }] },
      }),
    ].join('\n')
    const [s] = parseTranscript(jsonl)
    expect(s.turns).toEqual([
      { kind: 'tool', ts: '2026-07-13T18:00:02Z', name: 'WebSearch', input: { query: 'weather' }, result: 'sunny, 75F', isError: false },
    ])
  })

  it('marks an errored tool_result and stringifies array tool_result content', () => {
    const jsonl = [
      line({ type: 'assistant', sessionId: 's1', timestamp: '2026-07-13T18:00:02Z', message: { content: [{ type: 'tool_use', id: 'tu_2', name: 'Bash', input: {} }] } }),
      line({ type: 'user', sessionId: 's1', timestamp: '2026-07-13T18:00:03Z', message: { content: [{ type: 'tool_result', tool_use_id: 'tu_2', is_error: true, content: [{ type: 'text', text: 'boom' }] }] } }),
    ].join('\n')
    const [s] = parseTranscript(jsonl)
    expect(s.turns[0]).toMatchObject({ kind: 'tool', name: 'Bash', result: 'boom', isError: true })
  })
})

describe('parseTranscript — channel envelope + filtering', () => {
  const line = (o: unknown) => JSON.stringify(o)

  it('parses a Telegram channel envelope into channel info + clean text', () => {
    const body =
      '<channel source="plugin:telegram:telegram" chat_id="1000000001" message_id="12" user="1000000001" user_id="1000000001" ts="2026-07-13T18:42:41.000Z">\n' +
      "hello! What's the weather like in Springfield today?\n</channel>"
    const jsonl = line({ type: 'user', sessionId: 's1', timestamp: '2026-07-13T18:42:41Z', message: { content: body } })
    const [s] = parseTranscript(jsonl)
    expect(s.turns[0]).toEqual({
      kind: 'user',
      ts: '2026-07-13T18:42:41Z',
      text: "hello! What's the weather like in Springfield today?",
      channel: { source: 'plugin:telegram:telegram', chatId: '1000000001', user: '1000000001' },
    })
  })

  it('drops non-conversation record types', () => {
    const jsonl = [
      line({ type: 'permission-mode', sessionId: 's1', timestamp: '2026-07-13T18:00:00Z' }),
      line({ type: 'queue-operation', sessionId: 's1', timestamp: '2026-07-13T18:00:00Z' }),
      line({ type: 'assistant', sessionId: 's1', timestamp: '2026-07-13T18:00:01Z', message: { content: [{ type: 'text', text: 'real reply' }] } }),
    ].join('\n')
    const [s] = parseTranscript(jsonl)
    expect(s.turns).toEqual([{ kind: 'assistant', ts: '2026-07-13T18:00:01Z', text: 'real reply' }])
  })

  it('skips torn/partial JSON lines without throwing', () => {
    const jsonl = [
      '{"type":"user","sessionId":"s1","timestamp":"2026-07-13T18:00:00Z","message":{"content":"ok"}}',
      '{"type":"assistant","sessionId":"s1","timesta', // torn trailing line
    ].join('\n')
    const [s] = parseTranscript(jsonl)
    expect(s.turns).toEqual([{ kind: 'user', ts: '2026-07-13T18:00:00Z', text: 'ok' }])
  })
})

describe('parseTranscript — multi-session ordering', () => {
  const line = (o: unknown) => JSON.stringify(o)

  it('groups by sessionId and returns sessions newest-first', () => {
    const jsonl = [
      line({ type: 'user', sessionId: 'old', timestamp: '2026-07-11T10:00:00Z', message: { content: 'first session' } }),
      line({ type: 'user', sessionId: 'new', timestamp: '2026-07-13T10:00:00Z', message: { content: 'second session' } }),
    ].join('\n')
    const sessions = parseTranscript(jsonl)
    expect(sessions.map((s) => s.id)).toEqual(['new', 'old'])
  })
})

describe('parseTranscript — hardening', () => {
  const line = (o: unknown) => JSON.stringify(o)

  it('produces no turns and no session for an empty content array', () => {
    const jsonl = line({ type: 'assistant', sessionId: 's1', timestamp: '2026-07-13T18:00:00Z', message: { content: [] } })
    expect(parseTranscript(jsonl)).toEqual([])
  })

  it('drops an orphan tool_result without throwing', () => {
    const jsonl = line({
      type: 'user',
      sessionId: 's1',
      timestamp: '2026-07-13T18:00:00Z',
      message: { content: [{ type: 'tool_result', tool_use_id: 'nope', content: 'stray' }] },
    })
    expect(parseTranscript(jsonl)).toEqual([])
  })

  it('yields an unpaired tool turn for a tool_use with no id', () => {
    const jsonl = line({
      type: 'assistant',
      sessionId: 's1',
      timestamp: '2026-07-13T18:00:00Z',
      message: { content: [{ type: 'tool_use', name: 'Bash', input: { cmd: 'ls' } }] },
    })
    const [s] = parseTranscript(jsonl)
    expect(s.turns).toEqual([{ kind: 'tool', ts: '2026-07-13T18:00:00Z', name: 'Bash', input: { cmd: 'ls' } }])
  })

  it('extends endedAt to a trailing lone tool_result timestamp', () => {
    const jsonl = [
      line({ type: 'assistant', sessionId: 's1', timestamp: '2026-07-13T18:00:00Z', message: { content: [{ type: 'tool_use', id: 'tu_x', name: 'Bash', input: {} }] } }),
      line({ type: 'user', sessionId: 's1', timestamp: '2026-07-13T18:05:00Z', message: { content: [{ type: 'tool_result', tool_use_id: 'tu_x', content: 'done' }] } }),
    ].join('\n')
    const [s] = parseTranscript(jsonl)
    expect(s.endedAt).toBe('2026-07-13T18:05:00Z')
  })
})

describe('parseTranscript — subagent (sidechain) grouping', () => {
  const line = (o: unknown) => JSON.stringify(o)

  it('groups a contiguous sidechain run into one subagent turn, named from attribution', () => {
    const jsonl = [
      line({ type: 'user', sessionId: 's1', timestamp: '2026-07-13T18:00:00Z', message: { content: 'do research' } }),
      line({ type: 'assistant', sessionId: 's1', isSidechain: true, attributionAgent: 'workflow-subagent', attributionSkill: 'deep-research', timestamp: '2026-07-13T18:00:01Z', message: { content: [{ type: 'text', text: 'reading a file' }] } }),
      line({ type: 'assistant', sessionId: 's1', isSidechain: true, timestamp: '2026-07-13T18:00:02Z', message: { content: [{ type: 'tool_use', id: 'tu_1', name: 'Read', input: { path: 'x.md' } }] } }),
      line({ type: 'user', sessionId: 's1', isSidechain: true, timestamp: '2026-07-13T18:00:03Z', message: { content: [{ type: 'tool_result', tool_use_id: 'tu_1', content: 'file body' }] } }),
      line({ type: 'assistant', sessionId: 's1', timestamp: '2026-07-13T18:00:04Z', message: { content: [{ type: 'text', text: 'done researching' }] } }),
    ].join('\n')
    const [s] = parseTranscript(jsonl)
    // one user turn, then ONE subagent block (contiguous run), then the reply
    expect(s.turns.map((t) => t.kind)).toEqual(['user', 'subagent', 'assistant'])
    const sub = s.turns[1]
    expect(sub.kind === 'subagent' && sub.name).toBe('deep-research')
    // nested: the subagent's text turn + its tool (back-filled with the result)
    expect(sub.kind === 'subagent' && sub.turns.map((t) => t.kind)).toEqual(['assistant', 'tool'])
    const nestedTool = sub.kind === 'subagent' && sub.turns[1]
    expect(nestedTool && nestedTool.kind === 'tool' && nestedTool.result).toBe('file body')
    // the main conversation preview is unaffected by the hidden subagent work
    expect(s.firstUserText).toBe('do research')
    expect(s.lastAssistantText).toBe('done researching')
  })

  it('starts a fresh subagent block after a main-session line interrupts', () => {
    const jsonl = [
      line({ type: 'assistant', sessionId: 's1', isSidechain: true, timestamp: '2026-07-13T18:00:00Z', message: { content: [{ type: 'text', text: 'run 1' }] } }),
      line({ type: 'assistant', sessionId: 's1', timestamp: '2026-07-13T18:00:01Z', message: { content: [{ type: 'text', text: 'main' }] } }),
      line({ type: 'assistant', sessionId: 's1', isSidechain: true, timestamp: '2026-07-13T18:00:02Z', message: { content: [{ type: 'text', text: 'run 2' }] } }),
    ].join('\n')
    const [s] = parseTranscript(jsonl)
    expect(s.turns.map((t) => t.kind)).toEqual(['subagent', 'assistant', 'subagent'])
  })

  it('falls back to the generic name when no attribution is present', () => {
    const jsonl = line({ type: 'assistant', sessionId: 's1', isSidechain: true, timestamp: '2026-07-13T18:00:00Z', message: { content: [{ type: 'text', text: 'anon' }] } })
    const [s] = parseTranscript(jsonl)
    expect(s.turns[0].kind === 'subagent' && s.turns[0].name).toBe('subagent')
  })
})

describe('transcriptToText — export', () => {
  const line = (o: unknown) => JSON.stringify(o)

  it('renders sessions, turns, and nested subagent work as plain text', () => {
    const jsonl = [
      line({ type: 'user', sessionId: 's1', timestamp: '2026-07-13T18:00:00Z', message: { content: 'hi' } }),
      line({ type: 'assistant', sessionId: 's1', isSidechain: true, attributionSkill: 'deep-research', timestamp: '2026-07-13T18:00:01Z', message: { content: [{ type: 'text', text: 'inner step' }] } }),
      line({ type: 'assistant', sessionId: 's1', timestamp: '2026-07-13T18:00:02Z', message: { content: [{ type: 'text', text: 'answer' }] } }),
    ].join('\n')
    const txt = transcriptToText(parseTranscript(jsonl))
    expect(txt).toContain('SESSION')
    expect(txt).toContain('You')
    expect(txt).toContain('Assistant: answer')
    expect(txt).toContain('[subagent] deep-research')
    expect(txt).toContain('inner step')
  })

  it('returns a header-only string for no sessions', () => {
    expect(transcriptToText([])).toBe('')
  })
})
