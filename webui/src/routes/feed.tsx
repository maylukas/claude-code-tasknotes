import { createFileRoute, Link } from '@tanstack/react-router'
import { useEffect, useState, useCallback } from 'react'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { fetchFeed, sendReply, type FeedEntry } from '@/lib/api'
import { useEvents } from '@/lib/useEvents'
import { handleTaskClick } from '@/lib/host'
import { useStatus } from '@/lib/StatusContext'

export const Route = createFileRoute('/feed')({
  component: FeedPage,
})

const KIND_GLYPH: Record<FeedEntry['kind'], string> = {
  message: '📤',
  acked: '✅',
  activity: '📝',
}

// taskLabel turns a vault-relative path into a short display label —
// feed rows carry only a path (see serve.go's feedEntry), no title, so
// this is the same honest fallback agent.$name.tsx already uses for rows
// without one.
function taskLabel(path: string): string {
  const base = path.split('/').pop() ?? path
  return base.endsWith('.md') ? base.slice(0, -3) : base
}

function FeedPage() {
  const { status } = useStatus()
  const [feed, setFeed] = useState<FeedEntry[] | null>(null)
  const [replyTarget, setReplyTarget] = useState<string | null>(null)

  const refetch = useCallback(() => {
    fetchFeed()
      .then(setFeed)
      .catch((e) => console.warn('feed: refetch failed', e))
  }, [])

  useEffect(() => {
    refetch()
  }, [refetch])

  const connection = useEvents(refetch)

  return (
    <div className="mx-auto max-w-2xl p-4">
      <header className="mb-4 flex items-center justify-between">
        <div className="flex items-center gap-3">
          <Link to="/" className="text-sm text-muted-foreground hover:text-foreground">
            ← Needs you
          </Link>
          <h1 className="text-lg font-semibold">Feed</h1>
        </div>
        <ConnectionBadge connection={connection} />
      </header>

      {feed === null && <p className="text-sm text-muted-foreground">Loading…</p>}
      {feed !== null && feed.length === 0 && (
        <p className="text-sm text-muted-foreground">Nothing yet.</p>
      )}

      <div className="flex flex-col gap-2">
        {(feed ?? []).map((entry, i) => (
          <FeedRow
            key={entry.at + '-' + i}
            entry={entry}
            vaultName={status?.daemon.vaultName}
            replying={replyTarget === entry.taskPath}
            onToggleReply={() =>
              setReplyTarget((cur) => (cur === entry.taskPath ? null : entry.taskPath ?? null))
            }
            onSent={() => {
              setReplyTarget(null)
              refetch()
            }}
          />
        ))}
      </div>
    </div>
  )
}

function ConnectionBadge({ connection }: { connection: string }) {
  const label = connection === 'open' ? 'live' : connection === 'connecting' ? 'connecting…' : 'reconnecting…'
  const variant = connection === 'open' ? 'secondary' : 'outline'
  return <Badge variant={variant}>{label}</Badge>
}

function FeedRow({
  entry,
  vaultName,
  replying,
  onToggleReply,
  onSent,
}: {
  entry: FeedEntry
  vaultName?: string
  replying: boolean
  onToggleReply: () => void
  onSent: () => void
}) {
  const glyph = KIND_GLYPH[entry.kind] ?? '•'
  const when = new Date(entry.at).toLocaleString()

  const who =
    entry.kind === 'message'
      ? `${entry.agent ?? '?'} → ${entry.to ?? '?'}`
      : (entry.agent ?? '')

  return (
    <Card>
      <CardContent className="px-4 py-3">
        <div className="flex items-start justify-between gap-2">
          <div className="min-w-0 flex-1">
            <div className="flex items-center gap-2 text-xs text-muted-foreground">
              <span>{glyph}</span>
              {who && <span>{who}</span>}
              <span>{when}</span>
            </div>
            <p className="mt-1 text-sm">{entry.text}</p>
            {entry.taskPath && (
              <button
                type="button"
                className="mt-1 text-xs text-primary underline decoration-dotted"
                onClick={(e) => handleTaskClick(e, entry.taskPath!, vaultName)}
              >
                {taskLabel(entry.taskPath)}
              </button>
            )}
          </div>
          {entry.taskPath && (
            <Button variant="outline" size="sm" onClick={onToggleReply}>
              {replying ? 'Cancel' : 'Reply'}
            </Button>
          )}
        </div>
        {replying && entry.taskPath && <ReplyBox taskPath={entry.taskPath} onSent={onSent} />}
      </CardContent>
    </Card>
  )
}

function ReplyBox({ taskPath, onSent }: { taskPath: string; onSent: () => void }) {
  const [text, setText] = useState('')
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const submit = () => {
    const trimmed = text.trim()
    if (!trimmed || sending) return
    setSending(true)
    setError(null)
    sendReply(taskPath, trimmed)
      .then(() => {
        setText('')
        onSent()
      })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setSending(false))
  }

  return (
    <div className="mt-3 flex flex-col gap-2 border-t pt-3">
      <textarea
        className="min-h-16 w-full resize-y rounded-md border border-input bg-background px-2 py-1.5 text-sm outline-none focus-visible:ring-3 focus-visible:ring-ring/50"
        id={`reply-${taskPath}`}
        name="reply"
        aria-label={`Reply on ${taskLabel(taskPath)}`}
        placeholder={`Reply on ${taskLabel(taskPath)}…`}
        value={text}
        disabled={sending}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) submit()
        }}
        autoFocus
      />
      <div className="flex items-center justify-between gap-2">
        <span className="text-xs text-muted-foreground">Cmd/Ctrl-Enter to send</span>
        <div className="flex items-center gap-2">
          {error && <span className="text-xs text-destructive">{error}</span>}
          <Button size="sm" disabled={sending || !text.trim()} onClick={submit}>
            {sending ? 'Sending…' : 'Send'}
          </Button>
        </div>
      </div>
    </div>
  )
}
