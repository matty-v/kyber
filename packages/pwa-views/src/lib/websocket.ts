// WebSocket client that maintains a single shared connection to the events endpoint.
// Reconnects with exponential backoff on drop. Broadcasts events to subscribers.

import type { KyberEvent } from './types'

type EventHandler = (event: KyberEvent) => void

// Factory function that opens a WebSocket to the events endpoint.
// Provided by the consumer (useWebSocket) so the client doesn't pull
// cluster credentials out of globals.
export type EventStreamFactory = () => WebSocket

class EventBusClient {
  private ws: WebSocket | null = null
  private handlers = new Set<EventHandler>()
  private reconnectDelay = 1000
  private maxDelay = 30000
  private stopped = false
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null
  private factory: EventStreamFactory | null = null

  subscribe(handler: EventHandler, factory: EventStreamFactory): () => void {
    this.factory = factory
    this.stopped = false // clear the latch so re-subscribe reconnects after a previous disconnect
    this.handlers.add(handler)
    if (this.handlers.size === 1) {
      this.connect()
    }
    return () => {
      this.handlers.delete(handler)
      if (this.handlers.size === 0) {
        this.disconnect()
      }
    }
  }

  private connect() {
    if (this.stopped) return
    if (!this.factory) return
    try {
      this.ws = this.factory()
    } catch {
      this.scheduleReconnect()
      return
    }

    this.ws.onmessage = (e: MessageEvent<string>) => {
      try {
        const event = JSON.parse(e.data) as KyberEvent
        this.dispatch(event)
      } catch {
        // ignore malformed frames
      }
    }

    this.ws.onopen = () => {
      this.reconnectDelay = 1000
    }

    this.ws.onclose = () => {
      if (!this.stopped) {
        this.scheduleReconnect()
      }
    }

    this.ws.onerror = () => {
      // onclose fires right after onerror — let onclose handle reconnect
    }
  }

  private disconnect() {
    this.stopped = true
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer)
      this.reconnectTimer = null
    }
    if (this.ws) {
      this.ws.onclose = null
      this.ws.close()
      this.ws = null
    }
  }

  private scheduleReconnect() {
    const delay = this.reconnectDelay
    this.reconnectDelay = Math.min(this.reconnectDelay * 2, this.maxDelay)
    this.reconnectTimer = setTimeout(() => {
      if (!this.stopped && this.handlers.size > 0) {
        this.connect()
      }
    }, delay)
  }

  private dispatch(event: KyberEvent) {
    for (const handler of this.handlers) {
      try {
        handler(event)
      } catch {
        // don't let one bad handler break others
      }
    }
  }

  reset() {
    this.stopped = false
    if (this.handlers.size > 0) {
      this.connect()
    }
  }
}

// Singleton — one shared connection for the whole app.
export const eventBus = new EventBusClient()
