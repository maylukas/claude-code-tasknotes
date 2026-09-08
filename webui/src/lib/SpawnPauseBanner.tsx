import { useState } from 'react'
import { Button } from '@/components/ui/button'
import { setSpawnPaused } from '@/lib/api'
import { useStatus } from '@/lib/StatusContext'

// SpawnPauseBanner: the global (daemon-wide, not per-project) spawn
// kill-switch. Per team-lead: "should show its state unambiguously — a
// paused system that looks running is how we lose an afternoon" — so
// this is always visible with an explicit label in BOTH states, not just
// a toggle button that only speaks up when paused.
export function SpawnPauseBanner() {
  const { status, connection, refetch } = useStatus()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  if (!status) return null
  const paused = status.daemon.spawnPaused

  const toggle = () => {
    if (busy) return
    setBusy(true)
    setError(null)
    setSpawnPaused(!paused)
      .then(refetch)
      .catch((e) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setBusy(false))
  }

  return (
    <div
      className={
        'mb-4 flex items-center justify-between gap-3 rounded-md border px-3 py-2 text-sm ' +
        (paused ? 'border-destructive/40 bg-destructive/10 text-destructive' : 'border-border bg-muted/40 text-muted-foreground')
      }
    >
      <span className="font-medium">{paused ? '⏸ Spawning is PAUSED — no new orchestrators will start' : '▶ Spawning is active'}</span>
      <div className="flex items-center gap-2">
        {error && <span className="text-xs text-destructive">{error}</span>}
        <Button
          size="sm"
          variant={paused ? 'default' : 'outline'}
          disabled={busy || connection === 'connecting'}
          onClick={toggle}
        >
          {busy ? 'Working…' : paused ? 'Resume' : 'Pause'}
        </Button>
      </div>
    </div>
  )
}
