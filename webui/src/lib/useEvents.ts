import { useEffect, useRef, useState } from 'react'
import { bridgeUrl } from './api'

export type ConnectionState = 'connecting' | 'open' | 'disconnected'

// useEvents subscribes to GET /events (SSE) and calls onEvent whenever the
// daemon signals a mutation happened, so the caller can re-fetch /status.
// Reconnects with backoff on failure/close — this is the primary live-
// update path; the caller's own periodic poll (if any) is the fallback
// for when the stream can never be established at all (see
// SPEC-ui-v2.md: "Polling stays as the fallback when the stream cannot be
// established").
export function useEvents(onEvent: () => void): ConnectionState {
  const [state, setState] = useState<ConnectionState>('connecting')
  const onEventRef = useRef(onEvent)
  onEventRef.current = onEvent

  useEffect(() => {
    let cancelled = false
    let es: EventSource | null = null
    let retryDelay = 1000
    let retryTimer: number | undefined

    function connect() {
      if (cancelled) return
      setState('connecting')
      es = new EventSource(bridgeUrl() + '/events')

      es.onopen = () => {
        if (cancelled) return
        setState('open')
        retryDelay = 1000 // reset backoff once a connection actually succeeds
      }

      es.onmessage = () => {
        if (cancelled) return
        onEventRef.current()
      }

      es.onerror = () => {
        if (cancelled) return
        setState('disconnected')
        es?.close()
        retryTimer = window.setTimeout(connect, retryDelay)
        retryDelay = Math.min(retryDelay * 2, 30000)
      }
    }

    connect()

    return () => {
      cancelled = true
      es?.close()
      if (retryTimer !== undefined) window.clearTimeout(retryTimer)
    }
  }, [])

  return state
}
