import { useStatus } from '@/lib/StatusContext'

// BuildHashFooter renders the daemon's reported version and webui build
// hash in a small, unobtrusive footer on every route. This is what turns
// the stale-bundle footgun documented in CLAUDE.md (`go build` embeds
// whatever's on disk in webui/dist with no check against webui/src) from
// something you have to remember into something you can just look at:
// compare this hash against what `pnpm build` just produced, and a stale
// running daemon is immediately obvious instead of silently serving old UI.
export function BuildHashFooter() {
  const { status } = useStatus()
  const daemon = status?.daemon
  if (!daemon) return null

  return (
    <footer className="border-t px-4 py-2 text-center text-xs text-muted-foreground">
      tn {daemon.version}
      {daemon.buildHash && <> · build {daemon.buildHash}</>}
    </footer>
  )
}
