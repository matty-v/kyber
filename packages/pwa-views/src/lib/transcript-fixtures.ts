// A realistic two-session transcript: an older session yesterday and a current
// session today (Telegram weather Q&A with a WebSearch tool call + thinking).
const L = (o: unknown) => JSON.stringify(o)

export const twoSessionJsonl: string = [
  // --- older session (yesterday) ---
  L({ type: 'user', sessionId: 'sess-old', timestamp: '2026-07-12T09:00:00Z', message: { content: 'remind me tomorrow' } }),
  L({ type: 'assistant', sessionId: 'sess-old', timestamp: '2026-07-12T09:00:04Z', message: { content: [{ type: 'text', text: 'Will do.' }] } }),
  // --- current session (today) ---
  L({ type: 'user', sessionId: 'sess-new', timestamp: '2026-07-13T18:42:41Z', message: { content: '<channel source="plugin:telegram:telegram" chat_id="1000000001" user="1000000001" ts="2026-07-13T18:42:41.000Z">\nWeather in Springfield today?\n</channel>' } }),
  L({ type: 'assistant', sessionId: 'sess-new', timestamp: '2026-07-13T18:42:45Z', message: { content: [{ type: 'thinking', thinking: 'I should search for current conditions.' }] } }),
  L({ type: 'assistant', sessionId: 'sess-new', timestamp: '2026-07-13T18:42:46Z', message: { content: [{ type: 'tool_use', id: 'tu_ws', name: 'WebSearch', input: { query: 'Springfield weather today' } }] } }),
  L({ type: 'user', sessionId: 'sess-new', timestamp: '2026-07-13T18:42:51Z', message: { content: [{ type: 'tool_result', tool_use_id: 'tu_ws', content: 'Sunny, 75F now, high 81F.' }] } }),
  L({ type: 'assistant', sessionId: 'sess-new', timestamp: '2026-07-13T18:42:59Z', message: { content: [{ type: 'text', text: 'It is ~75°F now in Springfield, high of 81°F, sunny.' }] } }),
].join('\n')
