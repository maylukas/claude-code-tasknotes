import { createFileRoute, Link } from '@tanstack/react-router'
import { Card, CardContent } from '@/components/ui/card'
import { useStatus } from '@/lib/StatusContext'
import { handleTaskClick } from '@/lib/host'
import { AgentStateBadge } from '@/lib/agentState'
import type { AgentView } from '@/lib/api'

export const Route = createFileRoute('/agent/$name')({
  component: AgentDetailPage,
})

function findAgent(status: ReturnType<typeof useStatus>['status'], name: string): AgentView | undefined {
  if (!status) return undefined
  for (const project of Object.values(status.projects)) {
    const found = project.agents.find((a) => a.name === name)
    if (found) return found
  }
  return undefined
}

function AgentDetailPage() {
  const { name } = Route.useParams()
  const { status } = useStatus()
  const agent = findAgent(status, name)

  return (
    <div className="mx-auto max-w-2xl p-4">
      <Link to="/" className="text-sm text-muted-foreground hover:text-foreground">
        ← Back
      </Link>

      {!agent && status && <p className="mt-4 text-sm text-muted-foreground">No agent named "{name}".</p>}

      {agent && (
        <>
          <header className="mt-2 mb-6 flex items-center gap-3">
            <h1 className="text-lg font-semibold">{agent.name}</h1>
            <AgentStateBadge agent={agent} />
            {agent.contextPct != null && <span className="text-sm text-muted-foreground">{Math.round(agent.contextPct)}% context</span>}
          </header>

          <section className="mb-6">
            <h2 className="mb-2 text-sm font-semibold text-muted-foreground">
              Owned tasks ({agent.ownedTasks?.length ?? agent.ownedInProgress})
            </h2>
            <div className="flex flex-col gap-2">
              {(agent.ownedTasks ?? []).map((t) => (
                <Card key={t.path} className="cursor-pointer hover:bg-accent" onClick={(e) => handleTaskClick(e, t.path, status?.daemon.vaultName)}>
                  <CardContent className="flex items-center justify-between px-3 py-2">
                    <span>{t.title || t.path}</span>
                    {t.status && (
                      <span className="text-xs text-muted-foreground">{t.status}</span>
                    )}
                  </CardContent>
                </Card>
              ))}
              {(agent.ownedTasks ?? []).length === 0 && (
                <p className="text-sm text-muted-foreground">Nothing owned right now.</p>
              )}
            </div>
          </section>

          <section>
            <h2 className="mb-2 text-sm font-semibold text-muted-foreground">
              Workers ({agent.workers?.length ?? 0})
            </h2>
            <div className="flex flex-col gap-2">
              {(agent.workers ?? []).map((w) => (
                <Card key={w.path} className="cursor-pointer hover:bg-accent" onClick={(e) => handleTaskClick(e, w.path, status?.daemon.vaultName)}>
                  <CardContent className="flex items-center justify-between px-3 py-2">
                    <span>{w.title || w.path}</span>
                    <span className="text-xs text-muted-foreground">
                      {w.status ? w.status + ' · ' : ''}
                      since {new Date(w.startedAt).toLocaleTimeString()}
                    </span>
                  </CardContent>
                </Card>
              ))}
              {(agent.workers ?? []).length === 0 && (
                <p className="text-sm text-muted-foreground">
                  No declared workers — either it isn't running any, or it hasn't called{' '}
                  <code className="text-xs">tn worker start</code> yet.
                </p>
              )}
            </div>
          </section>
        </>
      )}
    </div>
  )
}
