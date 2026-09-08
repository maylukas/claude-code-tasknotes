// Types match statusResponse in ../../serve.go exactly (buildStatusResponse) —
// verified against the live daemon's GET /status output, not guessed from the
// spec text alone.

export interface AgentView {
  name: string
  alive: boolean
  accepting: boolean
  draining: boolean
  stuck: boolean
  // unackedBacklog: the oldest delivered-but-unacked message to this agent
  // has been waiting past the threshold. This measures inbox hygiene, NOT
  // whether the agent is frozen — a demonstrably working agent can carry
  // this if it never had a reason to ack something (see Message.Informational
  // on the Go side). Renamed from "stalled" 2026-08-25: that name read as
  // "this agent is stuck" to everyone looking at it, including the humans
  // deciding whether to kill a generation over it.
  unackedBacklog: boolean
  // rateLimited: the pane is parked on a claude.ai usage limit (daemon
  // 0.5.6+). Independent of stuck; the session resumes by itself at
  // rateLimitResetsAt (raw pane text, e.g. "2:30pm (Europe/Berlin)").
  // Outranks even "gone" for display: a parked session stops polling
  // its inbox, so it would otherwise read as dead.
  rateLimited?: boolean
  rateLimitResetsAt?: string
  rateLimitSince?: string
  contextPct?: number
  tmuxSession?: string
  lastSeenAt: string
  // ownedInProgress/ownedTasks are 0/empty for a non-alive agent — see
  // historicalOwned. ownedTasks lands with Milestone 2 (SPEC-ui-v2.md #2)
  // — optional since an older daemon or an agent with none declared
  // omits it (omitempty on the Go side).
  ownedInProgress: number
  ownedTasks?: Array<{ path: string; title?: string; status?: string }>
  // historicalOwned is set ONLY for a non-alive agent: the same
  // ownedTaskPathsLocked count that WOULD be ownedInProgress if this
  // agent were alive, kept visible under a name that makes clear it's
  // not current work (message-history ownership never expires on its
  // own, so a dead generation would otherwise appear to still be
  // carrying every task it ever touched — see serve.go's statusAgent
  // doc comment).
  historicalOwned?: number
  workers?: Array<{ path: string; title?: string; status?: string; startedAt: string }>
}

export interface ProjectView {
  agents: AgentView[]
  queued: number
  delivered: number
  taskCounts: Record<string, number>
  needsAction: number
  maxOrchestrators: number
  maxWorkers: number
  activeOwners?: number
}

export interface NeedsActionTask {
  title: string
  path: string
  status: string
  project?: string
  mr?: string
  integrationBranch?: string
  integrationRole?: string
  jira?: string
  jiraNc?: string
  ask?: string
  brief?: string
}

export interface StuckPrompt {
  agent: string
  tmuxSession?: string
  excerpt: string
}

export interface ActivityEntry {
  agent: string
  project?: string
  at: string
  text: string
}

export interface DaemonInfo {
  uptime: number
  version: string
  pid: number
  port: number
  spawnPaused: boolean
  lastSleepAt?: string
  lastSleepSeconds?: number
  // vaultName: NOT in team-lead's itemized daemon-work list — added
  // because the standalone-browser host needs it for the obsidian://
  // fallback link (SPEC-ui-v2.md's "same code, both hosts" for opening a
  // task), and the daemon already computes the vault's folder path via
  // dashboardPath — see host.ts's openTask and serve.go's derivation.
  vaultName?: string
  // buildHash: short hash of the webui bundle actually embedded in the
  // running binary (computed in serve.go from the embedded bytes
  // themselves). Rendered in the footer so a stale rebuild — CLAUDE.md's
  // documented footgun where `go build` embeds whatever's in webui/dist
  // without checking it against webui/src — is visible instead of silent.
  buildHash?: string
}

// CredentialProfile/CredentialsStatus match statusCredentials in serve.go
// (daemon 0.6.0+, creds.go): label/email/cooldown only — a token never
// leaves the Keychain, so nothing here is sensitive to render.
export interface CredentialProfile {
  label: string
  email?: string
  active: boolean
  stored: boolean
  savedAt?: string
  lastUsedAt?: string
  lastLimitedAt?: string
  limitedUntil?: string
}

export interface CredentialsStatus {
  autoSwap: boolean
  minSwapInterval: string
  active: string
  profiles: CredentialProfile[]
  lastSwap?: { from: string; to: string; trigger: string; at: string } | null
  error?: string
}

export interface StatusResponse {
  daemon: DaemonInfo
  projects: Record<string, ProjectView>
  needsActionTasks: NeedsActionTask[]
  stuckPrompts: StuckPrompt[]
  recentActivity: ActivityEntry[]
  // credentials is absent on a daemon older than 0.6.0 or one with no
  // credential store wired — the UI then simply shows no switcher.
  credentials?: CredentialsStatus
}

// FeedEntry matches feedEntry in serve.go's GET /feed (SPEC-ui-v2.md #3):
// a merged, newest-first timeline of message sends, acks, and activity log
// entries. kind lets the UI pick an icon without parsing text.
export interface FeedEntry {
  at: string
  kind: 'message' | 'acked' | 'activity'
  text: string
  agent?: string
  to?: string
  taskPath?: string
}

// bridgeUrl resolves the daemon base URL. The UI is SERVED by the same
// daemon it talks to (embedded via Go's embed, mounted at /ui) — so the
// same origin is always correct, in both the standalone-browser host and
// the Obsidian iframe (the iframe's src is itself http://localhost:8391/ui,
// so window.location.origin already IS the daemon).
export function bridgeUrl(): string {
  return window.location.origin
}

export async function fetchStatus(): Promise<StatusResponse> {
  const res = await fetch(bridgeUrl() + '/status')
  if (!res.ok) throw new Error('HTTP ' + res.status)
  return res.json()
}

export async function fetchFeed(): Promise<FeedEntry[]> {
  const res = await fetch(bridgeUrl() + '/feed')
  if (!res.ok) throw new Error('HTTP ' + res.status)
  const body = (await res.json()) as { feed: FeedEntry[] }
  return body.feed
}

export async function postJson(path: string, body: unknown): Promise<void> {
  const res = await fetch(bridgeUrl() + path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  if (!res.ok) {
    let message = 'HTTP ' + res.status
    try {
      const j = await res.json()
      if (j && typeof j.error === 'string') message = j.error
    } catch {
      // body wasn't JSON — keep the HTTP-status message
    }
    throw new Error(message)
  }
}

export const approve = (name: string, option: 'once' | 'always' | 'deny') =>
  postJson('/agents/approve', { name, option })
export const drain = (name: string) => postJson('/agents/drain', { name })
export const retire = (name: string) => postJson('/agents/retire', { name })
export const setSpawnPaused = (paused: boolean) => postJson('/spawn/pause', { paused })
export const sendReply = (taskPath: string, text: string) => postJson('/reply', { taskPath, text })
// swapCredentials switches EVERY Claude session on the machine to the
// named profile and nudges parked orchestrator panes — see creds.go.
export const swapCredentials = (label: string) => postJson('/creds/swap', { label })
