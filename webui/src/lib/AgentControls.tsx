import { useState } from 'react'
import { Button } from '@/components/ui/button'
import { drain, retire, type AgentView } from '@/lib/api'

// AgentControls: Drain and Retire, per team-lead's explicit asymmetry —
// "Retire must confirm, naming the agent AND its owned-task count (the
// tray does this); drain must not." Retire's confirm is inline (not a
// native browser confirm()) so it behaves the same and stays visible
// whether this is rendered standalone in a browser or inside Obsidian's
// iframe, where a native dialog's behavior is less predictable.
export function AgentControls({ agent, onChanged }: { agent: AgentView; onChanged: () => void }) {
  const [confirmingRetire, setConfirmingRetire] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const doDrain = () => {
    if (busy) return
    setBusy(true)
    setError(null)
    drain(agent.name)
      .then(onChanged)
      .catch((e) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setBusy(false))
  }

  const doRetire = () => {
    if (busy) return
    setBusy(true)
    setError(null)
    retire(agent.name)
      .then(() => {
        setConfirmingRetire(false)
        onChanged()
      })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setBusy(false))
  }

  return (
    <div onClick={(e) => e.stopPropagation()}>
      <div className="flex items-center gap-2">
        <Button size="sm" variant="outline" disabled={busy || agent.draining} onClick={doDrain}>
          {agent.draining ? 'Draining' : 'Drain'}
        </Button>
        <Button size="sm" variant="destructive" disabled={busy} onClick={() => setConfirmingRetire((c) => !c)}>
          Retire
        </Button>
      </div>

      {confirmingRetire && (
        <div className="mt-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-sm">
          <p>
            Retire <span className="font-medium">{agent.name}</span>? It owns{' '}
            <span className="font-medium">{agent.ownedInProgress}</span> in-progress task
            {agent.ownedInProgress === 1 ? '' : 's'}.
          </p>
          <div className="mt-2 flex items-center gap-2">
            <Button size="sm" variant="destructive" disabled={busy} onClick={doRetire}>
              {busy ? 'Retiring…' : 'Confirm retire'}
            </Button>
            <Button size="sm" variant="outline" disabled={busy} onClick={() => setConfirmingRetire(false)}>
              Cancel
            </Button>
          </div>
        </div>
      )}

      {error && <p className="mt-1 text-xs text-destructive">{error}</p>}
    </div>
  )
}
