import { createFileRoute, Link } from '@tanstack/react-router'
import { useMemo } from 'react'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent } from '@/components/ui/card'
import { useStatus } from '@/lib/StatusContext'
import { openTask, handleTaskClick } from '@/lib/host'
import { SpawnPauseBanner } from '@/lib/SpawnPauseBanner'
import { CredentialsBanner } from '@/lib/CredentialsBanner'
import type { NeedsActionTask, ProjectView } from '@/lib/api'

export const Route = createFileRoute('/')({
  component: NeedsYouPage,
})

// STATUS_GLYPH/STATUS_ORDER mirror tray-render.py exactly — same
// convention v1's obsidian-plugin/main.js used, carried over here.
const STATUS_GLYPH: Record<string, string> = { triage: '🔴', 'needs-input': '🟠', review: '🟣' }
const STATUS_ORDER = ['triage', 'needs-input', 'review']

function plainProject(value?: string): string {
  if (!value) return ''
  return value
    .trim()
    .replace(/^\[\[|\]\]$/g, '')
    .split('/')
    .pop()!
}

function groupByStatus(tasks: NeedsActionTask[]): NeedsActionTask[] {
  const grouped: NeedsActionTask[] = []
  for (const st of STATUS_ORDER) grouped.push(...tasks.filter((t) => t.status === st))
  grouped.push(...tasks.filter((t) => !STATUS_ORDER.includes(t.status)))
  return grouped
}

function NeedsYouPage() {
  const { status, connection, error } = useStatus()
  const tasks = useMemo(() => groupByStatus(status?.needsActionTasks ?? []), [status])

  return (
    <div className="mx-auto max-w-2xl p-4">
      <header className="mb-4 flex items-center justify-between">
        <div className="flex items-center gap-3">
          <h1 className="text-lg font-semibold">Needs you</h1>
          <Link to="/feed" className="text-sm text-muted-foreground hover:text-foreground">
            Feed →
          </Link>
        </div>
        <ConnectionBadge connection={connection} />
      </header>

      {error && (
        <div className="mb-4 rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">
          ⚠︎ Bridge unreachable: {error}
        </div>
      )}

      {status && tasks.length === 0 && (
        <p className="text-sm text-muted-foreground">Nothing needs you right now.</p>
      )}

      <div className="flex flex-col gap-2">
        {tasks.map((t) => (
          <TaskRow key={t.path} task={t} vaultName={status?.daemon.vaultName} />
        ))}
      </div>

      {status && (
        <div className="mt-8">
          <SpawnPauseBanner />
          <CredentialsBanner />
        </div>
      )}

      {status && Object.keys(status.projects).length > 0 && (
        <>
          <h1 className="mb-4 text-lg font-semibold">Projects</h1>
          <div className="flex flex-col gap-4">
            {Object.entries(status.projects)
              .filter(([, p]) => p.agents.length > 0 || p.taskCounts.open || p.taskCounts['in-progress'])
              .sort(([a], [b]) => a.localeCompare(b))
              .map(([slug, p]) => (
                <ProjectCard key={slug} slug={slug} project={p} />
              ))}
          </div>
        </>
      )}
    </div>
  )
}

function ProjectCard({ slug, project }: { slug: string; project: ProjectView }) {
  const open = project.taskCounts.open ?? 0
  const inProgress = project.taskCounts['in-progress'] ?? 0
  // Only LIVE agents get a row here — a project can accumulate many dead
  // generations over time (see statusAgent.HistoricalOwned's doc comment:
  // ownership tracking never expires), and listing them as ordinary rows
  // next to a live agent reads as if they're all still running. This is
  // the at-a-glance view; dead generations are still reachable, grouped
  // and clearly labeled, on the project detail page.
  // A rate-limited agent stays in the live list even once its inbox
  // poll lapses: it is parked, not gone (see agentState).
  const liveAgents = project.agents.filter((a) => a.alive || a.rateLimited)
  const retiredCount = project.agents.length - liveAgents.length

  return (
    <Card>
      <CardContent className="px-4 py-3">
        <div className="flex items-center justify-between">
          <Link to="/project/$slug" params={{ slug }} className="font-medium hover:underline">
            {slug}
          </Link>
          <div className="flex items-center gap-2 text-xs text-muted-foreground">
            {!!open && <span>{open} open</span>}
            {!!inProgress && <span>{inProgress} in progress</span>}
            {!!project.activeOwners && <span>{project.activeOwners} awaiting merge</span>}
          </div>
        </div>
        <div className="mt-1 flex flex-col gap-1">
          {liveAgents.map((a) => (
            <Link
              key={a.name}
              to="/agent/$name"
              params={{ name: a.name }}
              className="flex items-center gap-2 rounded px-2 py-1 text-sm hover:bg-accent"
            >
              <span
                className={
                  'h-2 w-2 shrink-0 rounded-full ' +
                  (a.rateLimited
                    ? 'bg-orange-500'
                    : a.stuck
                    ? 'bg-destructive'
                    : a.unackedBacklog
                      ? 'bg-yellow-500'
                      : a.draining
                        ? 'bg-violet-500'
                        : 'bg-green-500')
                }
              />
              <span>{a.name}</span>
              <span className="text-muted-foreground">{a.ownedInProgress} owned</span>
            </Link>
          ))}
          {liveAgents.length === 0 && <p className="text-sm text-muted-foreground">No live agents</p>}
          {retiredCount > 0 && (
            <Link
              to="/project/$slug"
              params={{ slug }}
              className="px-2 py-1 text-xs text-muted-foreground hover:underline"
            >
              + {retiredCount} retired generation{retiredCount === 1 ? '' : 's'} (not running) — view
            </Link>
          )}
        </div>
      </CardContent>
    </Card>
  )
}

function ConnectionBadge({ connection }: { connection: string }) {
  const label = connection === 'open' ? 'live' : connection === 'connecting' ? 'connecting…' : 'reconnecting…'
  const variant = connection === 'open' ? 'secondary' : 'outline'
  return <Badge variant={variant}>{label}</Badge>
}

function TaskRow({ task, vaultName }: { task: NeedsActionTask; vaultName?: string }) {
  const glyph = STATUS_GLYPH[task.status] ?? '•'
  const askOrBrief = (task.ask || task.brief || '').split('\n')[0]

  return (
    <Card
      className="cursor-pointer transition-colors hover:bg-accent"
      onClick={(e) => handleTaskClick(e, task.path, vaultName)}
    >
      <CardContent className="px-4 py-3">
        <div className="flex items-start justify-between gap-2">
          <div className="font-medium">
            {glyph} {task.title}
          </div>
          <div className="flex shrink-0 items-center gap-2">
            {task.project && (
              <span className="text-xs text-muted-foreground">{plainProject(task.project)}</span>
            )}
            <button
              type="button"
              title="Open note (Cmd/Ctrl-click also works)"
              className="text-xs text-muted-foreground underline decoration-dotted hover:text-foreground"
              onClick={(e) => {
                e.stopPropagation()
                openTask(task.path, vaultName)
              }}
            >
              note
            </button>
          </div>
        </div>
        {askOrBrief && <p className="mt-1 truncate text-sm text-muted-foreground">{askOrBrief}</p>}
        <TaskLinks task={task} />
      </CardContent>
    </Card>
  )
}

function TaskLinks({ task }: { task: NeedsActionTask }) {
  const parts: React.ReactNode[] = []
  if (task.mr) {
    parts.push(
      <a
        key="mr"
        href={task.mr}
        target="_blank"
        rel="noreferrer"
        className="text-primary underline"
        onClick={(e) => e.stopPropagation()}
      >
        MR
      </a>
    )
  }
  if (task.integrationBranch) {
    parts.push(
      <span key="branch">
        {task.integrationRole === 'owner' ? '★ OWNER of ' + task.integrationBranch : '→ ' + task.integrationBranch}
      </span>
    )
  }
  if (task.jiraNc) parts.push(<span key="jira-nc">Jira NC {task.jiraNc} (comment-only)</span>)
  else if (task.jira) parts.push(<span key="jira">Jira {task.jira}</span>)

  if (parts.length === 0) return null
  return (
    <div className="mt-1 flex flex-wrap gap-x-2 text-xs text-muted-foreground">
      {parts.map((p, i) => (
        <span key={i} className="flex items-center gap-2">
          {i > 0 && <span className="text-muted-foreground/50">·</span>}
          {p}
        </span>
      ))}
    </div>
  )
}
