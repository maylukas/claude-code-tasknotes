import { createContext, useCallback, useContext, useEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { fetchStatus, type StatusResponse } from './api'
import { useEvents, type ConnectionState } from './useEvents'

interface StatusContextValue {
  status: StatusResponse | null
  connection: ConnectionState
  error: string | null
  // refetch: an immediate manual re-fetch, for callers that just POSTed a
  // mutation (drain/retire/spawn-pause) and want the UI to reflect it
  // right away rather than waiting on the SSE round trip (server ->
  // triggerRenders -> /events -> this same refetch) to come back around.
  refetch: () => void
}

const StatusContext = createContext<StatusContextValue | null>(null)

// FALLBACK_POLL_MS only applies while the SSE stream is NOT open — once
// connected, live push is the only refresh path, which is the whole point
// of SPEC-ui-v2.md's SSE requirement (removing N-clients-on-timers load
// from TaskNotes). This is deliberately not the primary path.
const FALLBACK_POLL_MS = 15000

// DISCONNECT_ERROR_THRESHOLD_MS: the daemon self-restarts within ~10s on
// every rebuild (CLAUDE.md), which drops every SSE connection and can
// cause the fallback poll to land mid-restart. Per team-lead: "do not
// treat a disconnect as an error state in the UI until a reconnect
// attempt has actually failed... or the view will flash 'bridge
// unreachable' every time either of us ships." So a lone failed fetch
// never surfaces the banner — only sustained failure past a normal
// restart's duration does, with margin for the fast SSE reconnect
// backoff (useEvents.ts: 1s, 2s, 4s...) to have already recovered it.
const DISCONNECT_ERROR_THRESHOLD_MS = 20000

export function StatusProvider({ children }: { children: ReactNode }) {
  const [status, setStatus] = useState<StatusResponse | null>(null)
  const [error, setError] = useState<string | null>(null)
  const firstFailureAtRef = useRef<number | null>(null)

  const refetch = useCallback(() => {
    fetchStatus()
      .then((s) => {
        firstFailureAtRef.current = null
        setStatus(s)
        setError(null)
      })
      .catch((e) => {
        const now = Date.now()
        if (firstFailureAtRef.current === null) firstFailureAtRef.current = now
        if (now - firstFailureAtRef.current >= DISCONNECT_ERROR_THRESHOLD_MS) {
          setError(e instanceof Error ? e.message : String(e))
        }
      })
  }, [])

  const connection = useEvents(refetch)
  const connectionRef = useRef(connection)
  connectionRef.current = connection

  // Initial load, independent of SSE (the stream only tells us about
  // CHANGES; the very first paint needs an explicit fetch regardless).
  useEffect(() => {
    refetch()
  }, [refetch])

  // Fallback poll — only while not connected. Cleared the moment the
  // stream opens; recreated if it drops.
  useEffect(() => {
    if (connection === 'open') return
    const id = window.setInterval(refetch, FALLBACK_POLL_MS)
    return () => window.clearInterval(id)
  }, [connection, refetch])

  return <StatusContext.Provider value={{ status, connection, error, refetch }}>{children}</StatusContext.Provider>
}

export function useStatus(): StatusContextValue {
  const ctx = useContext(StatusContext)
  if (!ctx) throw new Error('useStatus must be used within a StatusProvider')
  return ctx
}
