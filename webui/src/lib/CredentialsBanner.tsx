import { useState } from 'react'
import { Button } from '@/components/ui/button'
import { swapCredentials, type CredentialProfile } from '@/lib/api'
import { useStatus } from '@/lib/StatusContext'

// CredentialsBanner: which claude.ai account every Claude session on this
// machine is using, and a one-click switch to any other saved profile
// (daemon 0.6.0+, creds.go). Same always-visible-both-states rule as
// SpawnPauseBanner: auto-swap ON is stated explicitly, because a swap
// changes the account under every interactive session too — that must
// never be a surprise. Hidden entirely on a daemon without the block.
export function CredentialsBanner() {
  const { status, connection, refetch } = useStatus()
  const [busy, setBusy] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  const creds = status?.credentials
  if (!creds) return null

  const activeProfile = creds.profiles.find((p) => p.active)
  const others = creds.profiles.filter((p) => !p.active)
  const parked = Object.values(status!.projects).flatMap((p) => p.agents).filter((a) => a.rateLimited)

  const swap = (label: string) => {
    if (busy) return
    setBusy(label)
    setError(null)
    swapCredentials(label)
      .then(refetch)
      .catch((e) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setBusy(null))
  }

  return (
    <div
      className={
        'mb-4 rounded-md border px-3 py-2 text-sm ' +
        (parked.length > 0 ? 'border-orange-500/40 bg-orange-500/10' : 'border-border bg-muted/40')
      }
    >
      <div className="flex items-center justify-between gap-3">
        <span className="font-medium">
          🔑 {creds.active ? creds.active : 'unsaved account'}
          {activeProfile?.email && <span className="ml-1 font-normal text-muted-foreground">({activeProfile.email})</span>}
          <span className="ml-2 text-xs font-normal text-muted-foreground">
            {creds.autoSwap ? 'auto-swap on' : 'auto-swap off'}
          </span>
        </span>
        <div className="flex flex-wrap items-center justify-end gap-2">
          {others.map((p) => (
            <Button
              key={p.label}
              size="sm"
              variant="outline"
              title="Switches EVERY Claude session on this machine and nudges parked orchestrator panes"
              disabled={busy !== null || connection === 'connecting' || !p.stored}
              onClick={() => swap(p.label)}
            >
              {busy === p.label ? 'Switching…' : 'Switch to ' + p.label}
              <ProfileNote profile={p} />
            </Button>
          ))}
          {others.length === 0 && (
            <span className="text-xs text-muted-foreground">
              no other profile — /login as another account, then <code>tn creds save &lt;label&gt;</code>
            </span>
          )}
        </div>
      </div>
      {parked.length > 0 && (
        <p className="mt-1 text-xs text-orange-700 dark:text-orange-300">
          ⏳ {parked.map((a) => a.name.split('-').pop() + (a.rateLimitResetsAt ? ' (resets ' + a.rateLimitResetsAt + ')' : '')).join(', ')} parked on a usage limit
        </p>
      )}
      {creds.lastSwap && (
        <p className="mt-1 text-xs text-muted-foreground">
          last swap {creds.lastSwap.from || '?'} → {creds.lastSwap.to} ({creds.lastSwap.trigger}, {new Date(creds.lastSwap.at).toLocaleTimeString()})
        </p>
      )}
      {creds.error && <p className="mt-1 text-xs text-destructive">{creds.error}</p>}
      {error && <p className="mt-1 text-xs text-destructive">{error}</p>}
    </div>
  )
}

function ProfileNote({ profile }: { profile: CredentialProfile }) {
  if (!profile.stored) return <span className="ml-1 text-xs opacity-70">(no Keychain item)</span>
  if (profile.limitedUntil && new Date(profile.limitedUntil) > new Date()) {
    return <span className="ml-1 text-xs opacity-70">(limited until {new Date(profile.limitedUntil).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })})</span>
  }
  return null
}
