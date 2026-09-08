import { createFileRoute, Link } from '@tanstack/react-router'
import { useMemo } from 'react'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent } from '@/components/ui/card'
import { useStatus } from '@/lib/StatusContext'
import { handleTaskClick } from '@/lib/host'
import { AgentStateBadge } from '@/lib/agentState'
import { AgentControls } from '@/lib/AgentControls'

export const Route = createFileRoute('/project/$slug')({
  component: ProjectDetailPage,
})

function ProjectDetailPage() {
  const { slug } = Route.useParams()
  const { status, refetch } = useStatus()
  const project = status?.projects[slug]

  // Integration-branch tasks for this project: task-level only (no
  // project-level equivalent — see statusNeedsActionTask.IntegrationBranch
  // in serve.go), so filtered client-side from the same needsActionTasks
  // list the Needs You page already uses.
  const branchTasks = useMemo(
    () => (status?.needsActionTasks ?? []).filter((t) => t.project && plainProject(t.project) === slug && t.integrationBranch),
    [status, slug]
  )

  // Live vs. not-running agents are shown as two visually distinct
  // groups, not interleaved — a dead generation next to a live one in
  // the same flat list reads as "these are all currently running", which
  // is exactly what team-lead flagged ("gone 41%" reads like a state, not
  // like "this agent no longer exists"). Not-running agents still get
  // their own row (with HistoricalOwned, not OwnedInProgress — see
  // statusAgent's doc comment) rather than disappearing outright: a
  // generation that died mid-task is exactly the thing this view exists
  // to make visible.
  const liveAgents = useMemo(() => (project?.agents ?? []).filter((a) => a.alive), [project])
  const inactiveAgents = useMemo(() => (project?.agents ?? []).filter((a) => !a.alive), [project])

  return (
    <div className="mx-auto max-w-2xl p-4">
      <Link to="/" className="text-sm text-muted-foreground hover:text-foreground">
        ← Back
      </Link>

      {!project && status && <p className="mt-4 text-sm text-muted-foreground">No project named "{slug}".</p>}

      {project && (
        <>
          <header className="mt-2 mb-4 flex items-center gap-3">
            <h1 className="text-lg font-semibold">{slug}</h1>
            <span className="text-xs text-muted-foreground">
              max {project.maxOrchestrators} orchestrator{project.maxOrchestrators === 1 ? '' : 's'}, max {project.maxWorkers || '∞'} worker
              {project.maxWorkers === 1 ? '' : 's'}
            </span>
          </header>

          <section className="mb-6 flex flex-wrap gap-2">
            {Object.entries(project.taskCounts)
              .sort(([a], [b]) => a.localeCompare(b))
              .map(([taskStatus, count]) => (
                <Badge key={taskStatus} variant="outline">
                  {taskStatus}: {count}
                </Badge>
              ))}
            <Badge variant="secondary">queued: {project.queued}</Badge>
            <Badge variant="secondary">delivered: {project.delivered}</Badge>
            {!!project.activeOwners && (
              <Badge variant="default">
                {project.activeOwners} workstream{project.activeOwners === 1 ? '' : 's'} awaiting merge
              </Badge>
            )}
          </section>

          {branchTasks.length > 0 && (
            <section className="mb-6">
              <h2 className="mb-2 text-sm font-semibold text-muted-foreground">Integration branches</h2>
              <div className="flex flex-col gap-2">
                {branchTasks.map((t) => (
                  <Card key={t.path} className="cursor-pointer hover:bg-accent" onClick={(e) => handleTaskClick(e, t.path, status?.daemon.vaultName)}>
                    <CardContent className="flex items-center justify-between px-3 py-2 text-sm">
                      <span>{t.title}</span>
                      <span className="text-xs text-muted-foreground">
                        {t.integrationRole === 'owner' ? '★ OWNER of ' : '→ '}
                        {t.integrationBranch}
                      </span>
                    </CardContent>
                  </Card>
                ))}
              </div>
            </section>
          )}

          <section>
            <h2 className="mb-2 text-sm font-semibold text-muted-foreground">Agents ({liveAgents.length})</h2>
            <div className="flex flex-col gap-2">
              {liveAgents.map((a) => (
                <Card key={a.name}>
                  <CardContent className="px-3 py-2">
                    <div className="flex items-center justify-between gap-2">
                      <Link to="/agent/$name" params={{ name: a.name }} className="flex items-center gap-2 hover:underline">
                        <span className="font-medium">{a.name}</span>
                        <AgentStateBadge agent={a} />
                        {a.contextPct != null && (
                          <span className="text-xs text-muted-foreground">{Math.round(a.contextPct)}%</span>
                        )}
                      </Link>
                      <span className="text-xs text-muted-foreground">{a.ownedInProgress} owned</span>
                    </div>
                    <div className="mt-2">
                      <AgentControls agent={a} onChanged={refetch} />
                    </div>
                  </CardContent>
                </Card>
              ))}
              {liveAgents.length === 0 && <p className="text-sm text-muted-foreground">No live agents.</p>}
            </div>
          </section>

          {inactiveAgents.length > 0 && (
            <section className="mt-6">
              <h2 className="mb-2 text-sm font-semibold text-muted-foreground">
                Not running ({inactiveAgents.length})
              </h2>
              <div className="flex flex-col gap-2">
                {inactiveAgents.map((a) => (
                  <Card key={a.name} className="opacity-60">
                    <CardContent className="px-3 py-2">
                      <div className="flex items-center justify-between gap-2">
                        <Link to="/agent/$name" params={{ name: a.name }} className="flex items-center gap-2 hover:underline">
                          <span className="font-medium">{a.name}</span>
                          <AgentStateBadge agent={a} />
                        </Link>
                        {!!a.historicalOwned && (
                          <span className="text-xs text-muted-foreground">
                            {a.historicalOwned} historical
                          </span>
                        )}
                      </div>
                      <p className="mt-1 text-xs text-muted-foreground">
                        No longer running — last seen {new Date(a.lastSeenAt).toLocaleString()}.
                      </p>
                      <div className="mt-2">
                        <AgentControls agent={a} onChanged={refetch} />
                      </div>
                    </CardContent>
                  </Card>
                ))}
              </div>
            </section>
          )}
        </>
      )}
    </div>
  )
}

function plainProject(value: string): string {
  return value
    .trim()
    .replace(/^\[\[|\]\]$/g, '')
    .split('/')
    .pop()!
}
