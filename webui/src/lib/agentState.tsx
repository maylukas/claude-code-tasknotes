import { Badge } from '@/components/ui/badge'
import type { AgentView } from '@/lib/api'

// agentState/AgentStateBadge: shared between the agent detail page and
// the project detail page, so an agent's state reads identically no
// matter which route it's viewed from.
export function agentState(a: AgentView): { label: string; variant: 'default' | 'secondary' | 'destructive' | 'outline' } {
  // Rate-limited outranks gone: a session parked on a usage limit stops
  // polling and goes not-alive within the alive window, and "gone" would
  // hide the one fact that matters — it comes back by itself.
  if (a.rateLimited) return { label: 'usage limit' + (a.rateLimitResetsAt ? ' · resets ' + a.rateLimitResetsAt : ''), variant: 'secondary' }
  if (!a.alive) return { label: 'gone', variant: 'outline' }
  if (a.stuck) return { label: 'stuck', variant: 'destructive' }
  // 'secondary', not 'destructive': this agent may well be working fine —
  // it just has an old unacked message (often an informational one it had
  // no reason to ack). Giving it the same red severity as "stuck" is what
  // made this misreadable as "frozen" in the first place.
  if (a.unackedBacklog) return { label: 'unacked backlog', variant: 'secondary' }
  if (a.draining) return { label: 'draining', variant: 'secondary' }
  return { label: 'accepting', variant: 'default' }
}

export function AgentStateBadge({ agent }: { agent: AgentView }) {
  const { label, variant } = agentState(agent)
  return <Badge variant={variant}>{label}</Badge>
}
