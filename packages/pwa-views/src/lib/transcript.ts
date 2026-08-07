/** A single rendered turn in the conversation. */
export type Turn =
  | { kind: 'user'; ts: string; text: string; channel?: ChannelInfo }
  | { kind: 'assistant'; ts: string; text: string }
  | { kind: 'thinking'; ts: string; text: string }
  | { kind: 'tool'; ts: string; name: string; input: unknown; result?: string; isError?: boolean }
  // A subagent (behind-the-scenes) invocation: a contiguous run of the agent's
  // sidechain work, grouped into one collapsible block with its own nested turns.
  | { kind: 'subagent'; ts: string; name: string; turns: Turn[] }

/** Parsed Telegram (or other) channel envelope metadata. */
export interface ChannelInfo {
  source: string
  chatId?: string
  user?: string
}

/** One Claude Code session (= one JSONL file / sessionId). */
export interface Session {
  id: string
  startedAt: string
  endedAt: string
  turns: Turn[]
  firstUserText: string
  lastAssistantText: string
}

// Raw JSONL line shape (only the fields we read).
interface RawLine {
  type?: string
  sessionId?: string
  timestamp?: string
  isSidechain?: boolean
  // Claude Code subagent attribution (present on some sidechain records) — used
  // to name the subagent block when available.
  attributionAgent?: string
  attributionSkill?: string
  message?: { content?: unknown }
  payload?: {
    session_id?: string
    type?: string
    message?: string
    phase?: string
  }
}

interface RawBlock {
  type?: string
  text?: string
  thinking?: string
  id?: string
  name?: string
  input?: unknown
  tool_use_id?: string
  content?: unknown
  is_error?: boolean
}

const CHANNEL_RE = /^<channel\s+([^>]*)>\n?([\s\S]*?)\n?<\/channel>\s*$/
const ATTR_RE = /(\w+)="([^"]*)"/g

// If `text` is a channel envelope (<channel source=...>body</channel>), return
// the parsed metadata + clean body; otherwise null.
export function parseChannelEnvelope(text: string): { channel: ChannelInfo; text: string } | null {
  const m = CHANNEL_RE.exec(text.trim())
  if (!m) return null
  const attrs: Record<string, string> = {}
  for (const a of m[1].matchAll(ATTR_RE)) attrs[a[1]] = a[2]
  if (!attrs.source) return null
  return {
    channel: { source: attrs.source, chatId: attrs.chat_id, user: attrs.user },
    text: m[2].trim(),
  }
}

function toolResultText(content: unknown): string {
  if (typeof content === 'string') return content
  if (Array.isArray(content)) {
    return (content as RawBlock[])
      .map((b) => (typeof b.text === 'string' ? b.text : typeof b.content === 'string' ? b.content : ''))
      .filter(Boolean)
      .join('\n')
  }
  return content == null ? '' : JSON.stringify(content)
}

// extractTurns walks one line's message content into ordered turns. CONSECUTIVE
// `text` blocks in one message are one logical message → coalesced into a single
// turn joined by "\n". `thinking` and `tool_use`/`tool_result` are their own
// turns and flush any buffered text first so ordering is preserved. Tool results
// arrive in a LATER line, so the shared `pendingTools` index back-fills the
// earlier tool turn by tool_use_id (a tool_result-only line yields no new turn
// but still mutates its pending tool turn).
function extractTurns(
  line: RawLine,
  ts: string,
  pendingTools: Map<string, Extract<Turn, { kind: 'tool' }>>,
): Turn[] {
  const turns: Turn[] = []
  const pushUser = (raw: string) => {
    const env = parseChannelEnvelope(raw)
    turns.push(env ? { kind: 'user', ts, text: env.text, channel: env.channel } : { kind: 'user', ts, text: raw })
  }
  const content = line.message?.content
  if (typeof content === 'string') {
    if (content.trim() !== '') pushUser(content)
  } else if (Array.isArray(content)) {
    let textParts: string[] = []
    const flushText = () => {
      const joined = textParts.join('\n')
      textParts = []
      if (joined.trim() === '') return
      if (line.type === 'user') pushUser(joined)
      else turns.push({ kind: 'assistant', ts, text: joined })
    }
    for (const block of content as RawBlock[]) {
      if (block.type === 'text' && typeof block.text === 'string') {
        textParts.push(block.text)
      } else if (block.type === 'thinking' && (block.thinking ?? '').trim() !== '') {
        flushText()
        turns.push({ kind: 'thinking', ts, text: block.thinking as string })
      } else if (block.type === 'tool_use') {
        flushText()
        const t: Turn = { kind: 'tool', ts, name: block.name ?? 'tool', input: block.input }
        turns.push(t)
        if (block.id) pendingTools.set(block.id, t as Extract<Turn, { kind: 'tool' }>)
      } else if (block.type === 'tool_result' && block.tool_use_id) {
        flushText()
        const t = pendingTools.get(block.tool_use_id)
        if (t) {
          t.result = toolResultText(block.content)
          t.isError = block.is_error === true
          pendingTools.delete(block.tool_use_id)
        }
        // A tool_result with no matching tool_use is dropped (noise).
      }
    }
    flushText()
  }
  return turns
}

// subagentName picks a human label for a sidechain block: the skill, else the
// agent, else a generic fallback.
function subagentName(line: RawLine): string {
  return (line.attributionSkill || line.attributionAgent || '').trim() || 'subagent'
}

export function parseTranscript(jsonl: string): Session[] {
  const sessions = new Map<string, Session>()
  const pendingTools = new Map<string, Extract<Turn, { kind: 'tool' }>>()
  // The subagent block currently accumulating for each session. A contiguous run
  // of sidechain lines feeds one block; the next main-session line closes it so a
  // later sidechain run starts a fresh block.
  const openSub = new Map<string, Extract<Turn, { kind: 'subagent' }>>()
  // Codex rollout records carry the session id on session_meta and omit it
  // from subsequent event_msg lines. Archive output preserves file order, so
  // retain the current id until the next session_meta boundary.
  let codexSessionId = ''

  const getSession = (sid: string, ts: string): Session => {
    let s = sessions.get(sid)
    if (!s) {
      s = { id: sid, startedAt: ts, endedAt: ts, turns: [], firstUserText: '', lastAssistantText: '' }
      sessions.set(sid, s)
    }
    return s
  }
  const extend = (s: Session, ts: string) => {
    if (ts && (!s.startedAt || ts < s.startedAt)) s.startedAt = ts
    if (ts && ts > s.endedAt) s.endedAt = ts
  }

  for (const rawLine of jsonl.split('\n')) {
    const trimmed = rawLine.trim()
    if (!trimmed) continue
    let line: RawLine
    try {
      line = JSON.parse(trimmed) as RawLine
    } catch {
      continue // torn/partial line — skip, mirroring the session-saver's tolerant decode
    }
    if (line.type === 'session_meta' && line.payload?.session_id) {
      codexSessionId = line.payload.session_id
      continue
    }
    if (line.type === 'event_msg' && codexSessionId) {
      const ts = line.timestamp ?? ''
      const kind = line.payload?.type
      const text = line.payload?.message
      if ((kind === 'user_message' || kind === 'agent_message') && text?.trim()) {
        const s = getSession(codexSessionId, ts)
        const turn: Turn = kind === 'user_message'
          ? { kind: 'user', ts, text }
          : { kind: 'assistant', ts, text }
        s.turns.push(turn)
        extend(s, ts)
        if (turn.kind === 'user' && !s.firstUserText) s.firstUserText = turn.text
        if (turn.kind === 'assistant') s.lastAssistantText = turn.text
      }
      continue
    }
    if (line.type !== 'user' && line.type !== 'assistant') continue
    const sid = line.sessionId ?? 'unknown'
    const ts = line.timestamp ?? ''
    const turns = extractTurns(line, ts, pendingTools)

    if (line.isSidechain === true) {
      // Behind-the-scenes subagent work: group the contiguous run into one
      // collapsible block instead of dropping it. A tool_result-only line adds no
      // turn but was already back-filled above; still extend the session window.
      if (turns.length === 0) {
        const existing = sessions.get(sid)
        if (existing) extend(existing, ts)
        continue
      }
      const s = getSession(sid, ts)
      let block = openSub.get(sid)
      if (!block) {
        block = { kind: 'subagent', ts, name: subagentName(line), turns: [] }
        s.turns.push(block)
        openSub.set(sid, block)
      } else if (block.name === 'subagent' && subagentName(line) !== 'subagent') {
        // Upgrade a generically-named run to the first real attribution we see.
        block.name = subagentName(line)
      }
      for (const t of turns) block.turns.push(t)
      extend(s, ts)
      continue
    }

    // Main-session line — close any open subagent block for this session so the
    // next sidechain run is a separate block.
    openSub.delete(sid)
    if (turns.length === 0) {
      const existing = sessions.get(sid)
      if (existing && ts && ts > existing.endedAt) existing.endedAt = ts
      continue
    }
    const s = getSession(sid, ts)
    for (const t of turns) {
      s.turns.push(t)
      extend(s, ts)
      if (t.kind === 'user' && !s.firstUserText) s.firstUserText = t.text
      if (t.kind === 'assistant') s.lastAssistantText = t.text
    }
  }

  return [...sessions.values()].sort((a, b) => (a.startedAt < b.startedAt ? 1 : a.startedAt > b.startedAt ? -1 : 0))
}

// transcriptToText renders parsed sessions to a plain-text export (the Export
// .txt button). Pure + dependency-free so it is unit-testable.
export function transcriptToText(sessions: Session[]): string {
  const time = (ts: string) => {
    if (!ts) return ''
    const d = new Date(ts)
    return Number.isNaN(d.getTime()) ? '' : d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' })
  }
  const stampOf = (ts: string) => {
    const t = time(ts)
    return t ? `[${t}] ` : ''
  }
  const out: string[] = []
  const renderTurn = (t: Turn, indent: string) => {
    const s = indent + stampOf(t.ts)
    switch (t.kind) {
      case 'user':
        out.push(`${s}You${t.channel ? ` (via ${t.channel.source})` : ''}: ${t.text}`)
        break
      case 'assistant':
        out.push(`${s}Assistant: ${t.text}`)
        break
      case 'thinking':
        out.push(`${s}[thinking] ${t.text}`)
        break
      case 'tool':
        out.push(`${s}[tool] ${t.name}${t.isError ? ' (error)' : ''}`)
        if (t.result) out.push(`${indent}    -> ${t.result.split('\n')[0]}`)
        break
      case 'subagent':
        out.push(`${s}[subagent] ${t.name} (${t.turns.length} ${t.turns.length === 1 ? 'step' : 'steps'})`)
        for (const inner of t.turns) renderTurn(inner, indent + '    ')
        break
    }
  }
  const day = (ts: string) => (ts && !Number.isNaN(new Date(ts).getTime()) ? new Date(ts).toLocaleString() : 'unknown')
  for (const sess of sessions) {
    out.push('='.repeat(60))
    out.push(`SESSION  ${day(sess.startedAt)}  ->  ${day(sess.endedAt)}   (${sess.turns.length} turns)`)
    out.push('='.repeat(60))
    for (const t of sess.turns) renderTurn(t, '')
    out.push('')
  }
  return out.join('\n')
}
