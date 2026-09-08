# Offline-first remote execution and shared Obsidian state

> **Status:** proposal, not implemented. Nothing in the code implements this document.

Status: implementation plan  
Scope: `tasknotes-cli`, its local bridge protocol, remote worker support, and the
TaskNotes/Obsidian integration needed to display synchronized progress  
Primary constraint: migrate the currently running Mac without interrupting active
agent work or losing bridge, task, or progress state

## 1. Decision summary

Evolve `tn serve` into an **edge daemon** that continues to listen on
`127.0.0.1:8391`. Existing CLI processes, orchestrators, tmux sessions, the TaskNotes
plugin webhook, and the launchd service keep using that local endpoint. The edge gains
a durable event journal and can synchronize with one remote coordinator when a network
connection is available.

Run the coordinator as the authority for global routing, worker discovery, task
leases, and cross-machine event ordering. It must not become a hard dependency for
local work: an edge can continue already-owned work, record progress, update its local
TaskNotes API, and render Obsidian pages while disconnected.

Run the same `tn` binary as an edge on every execution machine. Remote agents still
talk to a localhost bridge; they do not need inbound network access. The edge maintains
an outbound authenticated connection to the coordinator and spawns local tmux sessions
for work assigned to that machine.

Use two synchronization channels with separate responsibilities:

1. The `tn` event protocol synchronizes operational state: messages, progress,
   semantic task mutations, leases, acknowledgements, and worker health.
2. Obsidian Sync synchronizes human-readable Markdown: task notes, immutable progress
   event notes, machine/session pages, and coordinator-generated dashboards.

The event protocol is the correctness mechanism. Obsidian Sync is the presentation and
human-editing mechanism; it must never be used for lease ownership, inbox delivery, or
message acknowledgement.

The current machine remains the preferred **vault writer** throughout the migration.
It keeps the only active TaskNotes webhook, due-task scanner, MR watcher, and task-note
mutation loop until a later, explicit leadership cutover. Active agents are never
moved. Existing queued or delivered messages are never redelivered remotely.

For decomposed code work, use a coordinator-adjacent Git exchange remote. Component
workers publish immutable commit artifacts; one leased integration worker combines
them, publishes pinned checkpoints for downstream tasks, and owns the single canonical
MR/PR. Workers consume exact SHAs from input manifests, never silent updates from a
moving sibling branch.

## 2. Non-negotiable invariants

- `tn` commands used by existing sessions continue to work against
  `http://localhost:8391` throughout the compatibility window.
- A transient coordinator, internet, Obsidian Sync, or TaskNotes API outage cannot
  erase an accepted event or task mutation.
- Every cross-machine mutation is idempotent. Retrying after an ambiguous timeout is
  safe.
- At most one worker holds the global lease for a newly dispatched task.
- At most one integration worker may update a task graph's integration branch.
- Every dependent task attempt records an immutable input manifest of exact commit
  SHAs; dependency branches never update underneath active work.
- Work already owned by a legacy local agent is grandfathered to that agent and is
  never reassigned while it remains alive.
- A machine without internet can continue work it already owns and show its local
  progress in Obsidian.
- A disconnected machine cannot silently claim unrestricted new global work.
- Shared infrastructure mutations require a current online lease and fencing token;
  an expired offline worker cannot continue them.
- No two machines continuously rewrite the same generated Markdown file.
- Notes/progress are append-only by event identity. Status and editable fields use
  semantic conflict handling, not file modification time or blind last-write-wins.
- Existing `state.json`, task notes, dashboards, session pages, and task details are
  preserved until a separately approved contraction phase.
- Secrets remain machine-local. The coordinator distributes requirements and secret
  names, never secret values.

## 3. Current system and migration boundary

The current daemon combines five roles in one process:

- localhost HTTP bridge and agent inbox;
- agent/message registry in `~/.local/state/tn/state.json`;
- TaskNotes API client and webhook receiver;
- local tmux orchestrator launcher and stuck-session controller;
- Obsidian dashboard/session-page renderer.

The current runtime boundary is unusually helpful for migration: agents already call a
localhost bridge rather than talking directly to each other. We will preserve that
boundary and add synchronization behind it.

Current state maps to the target model as follows:

| Current state | Target owner during migration | Final owner |
|---|---|---|
| `agents` | Existing edge | Edge registration mirrored to coordinator |
| queued/delivered `messages` | Existing edge until acked | Coordinator plus destination-edge delivery record |
| `taskDetailsHash` / `routedClaudeLines` | Existing vault-writer edge | Vault-writer edge, checkpointed centrally |
| `assignedOccurrences` | Existing vault-writer edge | Elected vault writer |
| `lastReconciledAt` | Existing vault-writer edge | Elected vault writer |
| `mrStates` | Existing vault-writer edge | Elected vault writer |
| `spawnPaused` | Existing edge, mirrored | Coordinator policy with local emergency override |
| activity | Existing edge, then replicated | Coordinator event log and Obsidian projection |
| dashboard/session pages | Existing edge | Machine-owned pages plus coordinator-owned global page |

The first release must retain the current JSON reader/writer and current endpoints.
New behavior is disabled unless explicitly configured.

## 4. Target architecture

```text
                              Obsidian Sync
                       +-------------------------+
                       |                         |
+----------------------+---+             +-------+----------------+
| Existing Mac             |             | Headless vault replica |
|                           |             | (optional, coordinator)|
| Obsidian + TaskNotes API  |             | ob sync --continuous   |
|           ^               |             | global projection only |
|           | localhost     |             +------------------------+
| tn edge :8391             |
| - legacy API facade       | outbound TLS
| - journal/outbox          +------------------+
| - vault-writer leader     |                  |
| - local tmux worker       |          +-------v------------------+
+---------------------------+          | Coordinator              |
                                       | - global event log       |
+---------------------------+          | - task/worker leases     |
| Remote execution machine  | outbound | - routing               |
|                           +---------->+ - replica/read model     |
| tn edge :8391             |          | - sync cursors          |
| - replica task backend    |          +--------------------------+
| - journal/outbox          |
| - local tmux worker       |
+---------------------------+
```

All worker connections are outbound. The coordinator does not SSH into machines and
does not need a public endpoint on a laptop. A private network such as Tailscale is the
preferred first deployment, but the protocol must also be safe over ordinary HTTPS.

### 4.1 Process roles

One binary exposes three roles:

- `tn serve`: edge daemon and backward-compatible localhost bridge. On the existing
  Mac this also starts in-process local worker and vault-writer components.
- `tn coordinator serve`: central router, lease authority, event store, and replica
  API. This runs on an always-on host.
- `tn sync status|flush|doctor`: operator commands for inspecting outboxes, cursors,
  coordinator reachability, vault-writer leadership, and conflicts.

A future split into separate binaries is unnecessary unless deployment or security
boundaries demand it.

### 4.2 Authority boundaries

| Concern | Authority |
|---|---|
| Human task description, priority, due date | Task note via TaskNotes API |
| Append-only agent notes/progress | Event identity; projected into Obsidian |
| Requested task status | Semantic task-mutation event |
| Materialized task status | Current TaskNotes task after mutation reconciliation |
| New-task assignment | Coordinator lease |
| Already-running legacy assignment | Existing edge until agent ends |
| Agent/tmux process state | Owning edge |
| Global worker availability | Coordinator view of edge heartbeats |
| Local offline availability | Edge cache |
| Dashboard Markdown | Exactly one declared owner per output path |

## 5. Persistence model

### 5.1 Preserve the standard-library-only constraint

Do not introduce SQLite in the first implementation. This repository explicitly uses
the Go standard library only. Implement an append-only, checksummed journal behind a
storage interface; revisit SQLite or PostgreSQL through a separate architecture
decision if scale or operational experience justifies changing that constraint.

Proposed local layout:

```text
~/.local/state/tn/
  state.json                       # existing state, retained during migration
  v2/
    identity.json                  # stable machine ID and creation metadata
    snapshot.json                  # compacted event-derived state
    journal/
      0000000000000001.jsonl       # append-only local events
    outbox/
      coordinator.json             # ack cursor + unsent local sequence range
    inbox/
      coordinator.json             # last applied remote cursor
    conflicts/
      <conflict-id>.json           # unresolved semantic conflicts
```

Journal requirements:

- One JSON event per line, followed by `fsync` before acknowledging the mutation.
- Each record includes a version, payload length, and checksum so torn tail records
  can be detected and truncated to the last valid boundary on startup.
- Sequence numbers are monotonic per machine and allocated under one lock.
- Compaction writes a complete temporary snapshot, fsyncs it, renames it atomically,
  and only then marks old journal segments eligible for archival.
- A segment cannot be removed until the coordinator has acknowledged every contained
  event and the snapshot that includes it has passed a restart/replay test.
- No automatic deletion during the migration compatibility window.

The coordinator initially uses the same journal abstraction on a backed-up local disk.
The coordinator is a single writer, so serialization under one process lock is
sufficient for the intended personal deployment. The `EventStore` interface must not
expose file-specific behavior, leaving room for a transactional database later.

### 5.2 Event envelope

```json
{
  "version": 1,
  "id": "evt_<machine-id>_<sequence>_<random>",
  "originMachine": "macbook-lukas",
  "originSequence": 1842,
  "occurredAt": "2026-08-19T14:32:10.123Z",
  "recordedAt": "2026-08-19T14:32:10.130Z",
  "kind": "task.progress",
  "project": "tasknotes-cli",
  "taskId": "task_...",
  "taskPath": "TaskNotes/Fix login.md",
  "actor": "orchestrator-tasknotes-cli-g123",
  "correlationId": "msg_...",
  "causationId": "lease_...",
  "payload": {},
  "checksum": "sha256:..."
}
```

`id` is globally unique without coordination. Time is informational, never used as the
sole ordering or conflict-resolution mechanism. Coordinator cursors provide global
ingestion order; `originSequence` detects gaps in a machine stream.

Initial event kinds:

- `agent.registered`, `agent.heartbeat`, `agent.draining`, `agent.retired`;
- `message.queued`, `message.delivered`, `message.acked`;
- `task.snapshot.observed`, `task.assignment.requested`, `task.progress`;
- `task.note.requested`, `task.status.requested`, `task.fields.requested`;
- `task.mutation.applied`, `task.mutation.conflicted`;
- `task.graph.created`, `task.graph.changed`, `task.input.available`;
- `artifact.local`, `artifact.published`, `artifact.superseded`,
  `artifact.integrated`, `artifact.upstreamed`;
- `integration.started`, `integration.checkpoint`, `integration.conflicted`,
  `integration.verified`;
- `lease.granted`, `lease.renewed`, `lease.released`, `lease.expired`;
- `worker.capabilities`, `worker.online`, `worker.offline`;
- `sync.conflict.detected`, `sync.conflict.resolved`.

### 5.3 Stable task identity

Current bridge ownership is keyed by vault-relative path, which breaks when a task is
renamed. Introduce a `tn-id` custom property while remaining path-compatible.

- New tasks created through a v2 CLI receive a random `tn-id` immediately.
- Existing tasks continue to work by path.
- The vault writer lazily assigns `tn-id` when it first observes a task participating
  in cross-machine execution.
- A preflight command lists missing IDs before writing anything.
- During backfill, store `(tn-id, current path, previous paths[])` in the coordinator
  read model.
- Event envelopes carry both ID and last-known path throughout the compatibility
  window.
- If two offline machines assign different IDs to the same path, the vault writer
  keeps the ID materialized in the task and records the other as an alias. It does not
  discard either event stream.

Do not bulk-rewrite every task during the initial rollout. Backfill active/canary tasks
first, then perform a bounded, observable batch migration after sync is proven.

## 6. Synchronization protocol

### 6.1 Handshake

Every edge has a stable machine ID generated once and stored in `identity.json`. On
connection it sends:

```json
{
  "protocolMin": 1,
  "protocolMax": 1,
  "machineId": "macbook-lukas",
  "daemonVersion": "0.2.0",
  "lastRemoteCursor": 9311,
  "nextOriginSequence": 1842,
  "capabilities": ["vault-writer", "tmux", "macos", "project:tasknotes-cli"]
}
```

The coordinator selects a mutually supported protocol or rejects with a clear upgrade
requirement. Old standalone daemons never connect and remain fully functional.

### 6.2 HTTP endpoints

Use versioned JSON endpoints:

```text
POST /v1/sync/handshake
POST /v1/sync/push              # idempotent batch of origin events
GET  /v1/sync/pull?after=C&wait=55
POST /v1/sync/ack               # highest durably applied coordinator cursor

POST /v1/leases/acquire
POST /v1/leases/renew
POST /v1/leases/release
GET  /v1/work/pull?machine=M&wait=55

GET  /v1/tasks/:id              # coordinator replica plus pending overlay
POST /v1/tasks/:id/mutations    # semantic note/status/field request
GET  /v1/task-graphs/:root       # dependency graph + pinned artifact inputs
POST /v1/artifacts/publish       # seal a pushed Git commit as immutable input
POST /v1/integrations/checkpoint # publish an integration base for dependents
GET  /v1/status
```

Push behavior:

- Batch size and body size are bounded.
- Replayed event IDs return success with their existing coordinator cursor.
- An origin sequence gap rejects only the suffix after the gap and reports the first
  missing sequence.
- A response is successful only after the coordinator journal is durable.
- Edges retain events until the acknowledged cursor is durable locally.

Pull behavior:

- Long-polling follows the existing inbox pattern.
- The edge journals the remote event before advancing its cursor.
- Replaying a cursor range is safe.
- Applying an event to a read model or Markdown projection is idempotent.

### 6.3 Connectivity state

Expose four explicit states:

- `standalone`: remote sync not configured;
- `connected`: coordinator reachable and cursors current;
- `degraded`: coordinator reachable but a gap, conflict, or vault-writer problem exists;
- `offline`: coordinator unreachable; local journal/outbox remain active.

Never log a connectivity failure on every retry. Use exponential backoff with jitter,
reset immediately after a successful exchange, and surface the current state through
`tn sync status`, `/status`, `/ui`, and the Obsidian dashboard.

## 7. Task mutation and conflict semantics

Remote agents must keep using the same commands (`tn get`, `tn note`, `tn status`,
`tn update`) even when no Obsidian desktop exists on their machine. Add a task backend
interface beneath the CLI:

- `direct`: existing TaskNotes HTTP API; default on the current Mac;
- `replica`: local edge replica plus coordinator synchronization; used by remote
  orchestrators;
- `auto`: direct when a healthy local TaskNotes API is configured, otherwise replica.

The remote edge stores the last synchronized task snapshot plus pending mutations. A
successful remote command means “durably accepted into the mutation journal,” not
necessarily “already written into the Markdown task note.” CLI output should say when
materialization is pending, while retaining exit code 0 so existing orchestrator
control flow does not break.

Use semantic mutations rather than shipping replacement Markdown:

```json
{
  "mutationId": "mut_...",
  "taskId": "task_...",
  "baseRevision": "sha256:...",
  "operation": "append-note",
  "payload": {"by": "agent-name", "text": "Tests pass."}
}
```

Rules:

- `append-note`: apply once, detected by a hidden event marker in the TaskNotes history
  block. A changed base revision does not conflict.
- `set-status`: validate against the TaskNotes workflow. If the current status changed
  independently, apply only when the requested transition remains valid; otherwise
  create a conflict.
- `set-field`: compare the named field against its value at `baseRevision`. Apply if it
  is unchanged; otherwise create a conflict.
- tags/projects/contexts: introduce semantic `add`/`remove` operations for remote use.
  Preserve legacy replace-array behavior only for direct local calls.
- `set-details`: never auto-merge arbitrary concurrent Markdown. Preserve both versions
  and request human reconciliation.

The vault writer is the only component that converts accepted mutations into
TaskNotes API calls. It fetches the latest task, applies one semantic mutation, writes
through the existing API, fetches again, and emits `task.mutation.applied` with the
observed revision. Failed or ambiguous writes retry using the mutation marker and live
task contents, so they cannot duplicate a note.

## 8. Work assignment, leases, and offline behavior

### 8.1 Worker capability advertisement

Each edge reports capabilities without secrets:

```json
{
  "machineId": "home-server",
  "projects": ["tasknotes-cli", "myapp"],
  "platform": "linux/arm64",
  "labels": ["docker", "always-on"],
  "maxOrchestrators": 2,
  "maxWorkers": 6,
  "executionLanes": ["repo", "staging-myapp"],
  "secretNames": ["GITLAB_TOKEN"]
}
```

Project configuration remains machine-local because checkout paths and available
secrets differ per machine. Coordinator policy may require capabilities, but must not
send environment values.

### 8.2 Lease model

A lease contains:

- lease ID and monotonically increasing fencing token;
- task ID/path and project;
- machine ID and agent generation;
- issued, renewed, and expiry times;
- execution lane and offline policy;
- previous owner, when inherited from legacy state.

Default policies:

- `connected-only`: shared infrastructure, deployments, production/staging mutations;
- `offline-owned`: repo work may continue offline after acquiring a lease online;
- `offline-reserved`: a bounded list of tasks may be prefetched and reserved for a
  particular machine before expected disconnection;
- `offline-optimistic`: disabled by default and never permitted for shared side effects.

An offline-owned lease has a generous grace period and remains visible as “connection
unknown.” The coordinator does not immediately duplicate repo work just because the
edge disconnects. Reassignment requires expiry plus a safety delay and a recorded
operator/agent handoff. A fenced execution lane rejects side-effect actions from an
older token after reassignment.

### 8.3 Existing active work

At migration bootstrap:

- Every alive current agent is imported as a legacy local worker session.
- Every task inferred as owned by that agent gets a grandfathered lease.
- Grandfathered leases are informational and do not expire while the local edge still
  reports the agent alive.
- Existing queued and delivered messages remain deliverable only by the local edge.
- The coordinator mirrors their state but marks them `legacyLocalDelivery=true`.
- When acknowledged, the edge emits the acknowledgement; the coordinator never sends
  the original message again.
- When the owning agent drains/retires/dies, normal coordinator leasing may take over.

This is the central no-duplicate-execution guarantee during rollout.

## 9. Dependent work and Git integration

Large tasks need a second coordination layer beyond “task A is done before task B
starts.” A component may be code-ready before it is independently mergeable, several
parallel components may need to be tested together, and a downstream task may require
the exact commits produced by upstream tasks. Model this explicitly rather than asking
workers to discover and merge one another's moving branches.

### 9.1 Task graph

A decomposed task creates a versioned DAG with one integration root:

```json
{
  "graphId": "graph_...",
  "version": 3,
  "rootTaskId": "task_large_feature",
  "integrationTaskId": "task_integrate_large_feature",
  "repository": "tasknotes-cli",
  "upstreamBase": "origin/main",
  "baseSha": "abc123...",
  "nodes": [
    {
      "taskId": "task_api",
      "deliveryMode": "component",
      "inputs": []
    },
    {
      "taskId": "task_cli",
      "deliveryMode": "component",
      "inputs": [
        {"taskId": "task_api", "readiness": "integration-checkpoint"}
      ]
    }
  ]
}
```

Supported dependency semantics:

- `finish-to-start`: existing TaskNotes `blockedBy`; the predecessor must be `done`.
- `artifact-to-start`: the predecessor must have a verified, globally published
  artifact, but need not be merged upstream.
- `integration-checkpoint`: the predecessor's artifact must have been incorporated by
  the integration owner into a verified checkpoint. The dependent starts from that
  exact checkpoint SHA.
- `artifact-to-integrate`: both tasks may execute independently, but the integration
  task cannot complete until the required artifact exists.
- `external`: a non-code dependency such as approval, data, or environment readiness;
  no Git artifact is implied.

Keep existing `blockedBy` behavior unchanged. Add the richer dependency graph as
coordinator state and TaskNotes custom properties. An old daemon sees ordinary tasks
and remains conservative; only a coordinator-aware scheduler interprets artifact
readiness.

Graph mutations use compare-and-swap on `version`. Once any node has started, changing
its base or dependency set creates a new node attempt or an explicit amendment; it
must not silently alter an active worker's inputs.

### 9.2 Git exchange remote

Run a per-repository bare Git **exchange remote** on or adjacent to the coordinator.
Call the remote `tn-exchange`. It is an intermediate object/ref store, not the canonical
product origin and not the final review surface.

The coordinator process itself remains a control plane and does not edit source code.
A worker with the `integration:<repo>` capability owns integration worktrees. The bare
remote may live on the same always-on host as the coordinator, but it is a separate
service and storage boundary.

Preferred transport for the first deployment:

- Git smart HTTP behind TLS on the private overlay network;
- repository- and machine-scoped credentials;
- system `git-http-backend` or an existing small Git service rather than implementing
  the Git wire protocol inside `tn`;
- restricted `git-shell` over SSH is an acceptable fallback, but never expose a
  general remote shell.

Each configured project declares both remotes locally:

```text
origin       canonical GitLab/GitHub repository
tn-exchange  coordinator-adjacent intermediate repository
```

The exchange stores:

```text
refs/heads/tn/work/<task-id>/<attempt-id>       mutable, one task attempt owner
refs/tn/artifacts/<task-id>/<attempt-id>/<rev>  immutable, coordinator sealed
refs/heads/tn/integrate/<root-task-id>          mutable, one integration lease owner
refs/tn/checkpoints/<root-task-id>/<seq>        immutable, coordinator sealed
```

Workers may push only their leased work ref. After verification, a worker calls
`POST /v1/artifacts/publish` with its work ref and commit SHA. The coordinator verifies
that the object exists, belongs to the task attempt, has the expected base ancestry,
and includes required evidence. It then creates the immutable artifact ref and emits
`artifact.published`. Workers never create or rewrite sealed refs themselves.

Only the current integration lease may update the integration work ref. Published
artifact and checkpoint refs are append-only. Non-fast-forward work-ref updates require
an explicit force-with-lease operation tied to the current attempt; blind force pushes
are rejected. A pre-receive hook or credential-to-ref policy enforces ownership.

The exchange remote is not a backup substitute. Retain every artifact until it is
reachable from the canonical upstream MR/branch and that upstream state has been
verified. Back up the bare repository during the compatibility window; disable object
pruning for unexpired task graphs.

### 9.3 Artifact descriptor and pinned input manifest

A branch name is not sufficient evidence because it moves. Every published artifact
contains an immutable descriptor:

```json
{
  "artifactId": "artifact_...",
  "taskId": "task_api",
  "attemptId": "attempt_2",
  "repository": "tasknotes-cli",
  "ref": "refs/tn/artifacts/task_api/attempt_2/1",
  "commitSha": "def456...",
  "baseSha": "abc123...",
  "treeSha": "789abc...",
  "createdBy": "home-server/orchestrator-tasknotes-cli-g7",
  "leaseFencingToken": 41,
  "verification": {
    "commands": ["go test ./... -count=1"],
    "result": "passed",
    "evidenceEventIds": ["evt_..."]
  }
}
```

Every task attempt starts with a coordinator-issued input manifest:

```json
{
  "taskId": "task_cli",
  "attemptId": "attempt_1",
  "repository": "tasknotes-cli",
  "startSha": "checkpoint789...",
  "graphVersion": 3,
  "inputs": [
    {"taskId": "task_api", "artifactId": "artifact_...", "commitSha": "def456..."}
  ]
}
```

The manifest is stored in coordinator/event state and noted on the task. It need not be
committed into the product repository. The worker fetches all exact SHAs before editing
and records them in its first progress event. This makes builds reproducible and makes
it possible to explain which version of a dependency a task used.

### 9.4 Delegation workflow

The root orchestrator performs decomposition as one durable graph operation:

1. Pin the canonical upstream base SHA.
2. Create the child TaskNotes tasks and one integration task.
3. Record graph nodes, dependency kinds, expected outputs, repository, and integration
   policy.
4. Verify that the graph is acyclic and every referenced task exists.
5. Publish `task.graph.created`; only then expose nodes to the scheduler.

Execution then proceeds as follows:

1. Scheduler leases independent root nodes to capable workers.
2. Each worker creates a dedicated worktree from its pinned `startSha`.
3. The worker commits locally and can continue doing so offline.
4. When connected, it pushes its work ref to `tn-exchange`.
5. After relevant tests pass, it publishes an immutable artifact descriptor.
6. The integration worker fetches that exact artifact and merges it into the single
   integration branch in graph/topological order.
7. After the integration branch passes the graph's checkpoint verification, the
   integration worker publishes an immutable checkpoint.
8. Tasks waiting on `integration-checkpoint` receive manifests pinned to that checkpoint
   and may now start.
9. When all required nodes are integrated, the integration worker runs full-system
   verification and pushes the integration branch to canonical `origin`.
10. The integration task creates the one canonical MR/PR and links all component tasks
    and artifact SHAs.

Parallel siblings never merge each other's branches directly. Linear dependents do not
start from a moving predecessor branch; they start from an integration checkpoint.
This gives one place to resolve cross-component conflicts and prevents every worker
from inventing a different combined base.

### 9.5 Pulling updates during active work

Default dependency policy is **snapshot**: a running attempt stays pinned to its input
manifest. A newly published upstream artifact produces an `artifact.superseded` or
`task.input.available` notification but is not merged automatically into an active
worktree.

Support three explicit policies:

- `snapshot`: keep the original inputs; the integrator handles later convergence.
- `follow-compatible`: notify the worker and allow `tn deps sync` at a worker-chosen
  safe point after a clean worktree check and preview of incoming commits.
- `restart-on-change`: cancel/drain the current attempt and create a new attempt from a
  new checkpoint. Use when generated APIs or schemas make the old base invalid.

`tn deps status` shows the manifest, available replacements, and whether the integration
branch has moved. `tn deps sync` performs a fetch and either fast-forward/rebase/merge as
declared by project policy, records before/after SHAs, reruns required tests, and emits
an event. It must never run implicitly in the background while an agent has uncommitted
changes.

If multiple prerequisites are updated, the integration owner first produces one new
checkpoint. Downstream workers consume that composite checkpoint rather than merging a
set of branches independently.

### 9.6 Integration conflicts and amendments

The integration worker is the single writer for conflict resolution on the integration
branch. On conflict it:

- records `integration.conflicted` with artifact IDs and conflicted paths;
- leaves the integration task `in-progress` or `needs-input` as appropriate;
- resolves mechanical cross-component conflicts itself when ownership is clear;
- requests an amendment from the responsible component task when resolution requires
  domain changes;
- never rewrites a sealed artifact.

An amendment creates a new attempt/revision and a new immutable artifact. The old
artifact remains available and is marked superseded, not deleted. The integration
worker updates its branch with the new artifact using a recorded strategy, republishes
the checkpoint, and reruns affected plus full integration tests.

### 9.7 Task status and review semantics

Add `delivery-mode` and artifact metadata as TaskNotes custom properties:

```text
delivery-mode: standalone | component | integration
integration-root: <task-id>
integration-task: <task-id>
artifact-state: local | published | integrated | upstreamed
artifact-id: <artifact-id>
artifact-sha: <commit-sha>
```

Rules:

- `standalone` retains today's requirement: a code task enters `review` only with its
  own MR/PR.
- `component` may enter `review` with a verified exchange artifact instead of an MR.
  Its completion note contains the immutable ref/SHA, verification, integration task,
  and input manifest.
- Component review rows are grouped under their integration root and do not create a
  separate human-action notification unless conflicted or rejected.
- `integration` owns the canonical MR/PR and full-system evidence.
- Merging the integration MR marks the integration/root task done and then marks each
  incorporated component done with a note identifying the upstream merge SHA.
- Closing the integration MR without merge reopens the integration task. Component
  artifacts remain published/integrated and are not discarded.
- A component that was never incorporated is not completed merely because the root MR
  merged; it becomes a graph reconciliation error requiring attention.

Update `ORCHESTRATOR.md`, the TaskNotes skill, dashboard queries, MR watcher, and
completion validation together when these modes are implemented. Until then, keep all
tasks on the existing standalone MR rule.

### 9.8 Offline and failure behavior

- A disconnected worker may commit indefinitely to its local task branch if its lease
  permits offline-owned work.
- It records `artifact.local`, but this does not unblock work on other machines.
- Only an artifact confirmed present in `tn-exchange` becomes `published` and globally
  usable.
- If a dependent task is deliberately co-located, a local-only handoff may be enabled
  as an optimization, but the coordinator must keep it on the same machine and mark
  both tasks non-migratable until publication. Disable this initially.
- If the exchange remote is unavailable, execution may continue locally; artifact
  publication, cross-machine dependents, and integration wait visibly.
- If canonical origin is unavailable, integration can continue against the pinned base
  and exchange remote. Final upstream push/MR waits without losing artifacts.
- If the integration worker disappears, its lease expires through the normal safety
  delay. A replacement reconstructs the worktree from the integration ref, checkpoint
  manifest, and immutable artifacts before making a new commit.

## 10. Obsidian synchronization and projection

### 10.1 File synchronization

Use Obsidian Sync for the shared vault. Every interactive device retains a local copy
and therefore remains readable/editable offline. Configure a unique device name on each
device. If remote progress must enter the vault while every desktop is offline, run the
official Obsidian Headless client beside the coordinator against a dedicated local
vault directory.

Prefer a dedicated automation vault, or limit the remote/headless replica to the
TaskNotes folders. Remote execution workers do not need vault credentials; they send
events to the coordinator.

### 10.2 Conflict-safe layout

Do not make all edges render the current shared `Claude Sessions.md`. Introduce
single-writer and immutable paths:

```text
TaskNotes Runtime/
  Events/
    <task-id>/
      <event-id>.md                  # immutable, globally unique
  Machines/
    macbook-lukas.md                 # written only by that machine
    home-server.md                   # written only by that machine
  Sessions/
    <machine-id>/
      <agent-name>.md                # written only by owning edge
  Global/
    Dashboard.md                     # coordinator materializer only
  Conflicts/
    <conflict-id>.md                 # immutable until a resolution event
```

Every immutable progress file includes `event-id`, `task-id`, origin, actor, time,
kind, and text in frontmatter/body. Its path is a pure function of the event ID. If the
same event arrives through both Obsidian Sync and the `tn` event protocol, the expected
path and content hash match; no duplicate is produced. A mismatching existing file is a
reported integrity conflict and is never overwritten automatically.

The existing dashboard path remains owned by the current Mac during migration. The new
global dashboard is introduced at a different path. Only after the user has accepted
the new view may the old path become a link or redirect; do not overwrite it during
canary rollout.

### 10.3 Task view

Extend the TaskNotes Obsidian plugin with a progress view keyed by `tn-id`. It reads
immutable event notes and presents a merged timeline inside the task view. The task
note itself need not be rewritten for every progress update.

Required UI:

- merged local/remote progress ordered by coordinator cursor when available and by
  origin sequence otherwise;
- machine and agent attribution;
- “local”, “synced”, “pending upload”, and “remote cached” markers;
- vault-sync and coordinator-sync freshness shown separately;
- queued mutation indicator when remote progress is visible but the main TaskNotes
  status has not yet been materialized;
- explicit conflict cards with resolution actions;
- current lease owner and whether it is connected, offline within grace, draining, or
  expired.
- parent/integration task, graph progress, artifact readiness, pinned input manifest,
  and whether a newer dependency artifact is available;
- integration branch/checkpoint state and grouped component review progress.

Until the plugin view exists, generate a Markdown progress index per task or project.
This is an interim projection, not a reason to append concurrently to task files.

### 10.4 Loop prevention

There are two paths by which an event can arrive, so projections must carry identity:

- event Markdown contains `tn-event-id` and content checksum;
- notes materialized into a task history block contain
  `<!-- tn-event:<id> -->`;
- the vault watcher ignores coordinator-generated dashboards and machine/session
  projection files as task-input changes;
- imported event files are registered by ID before any projection is attempted;
- TaskNotes webhooks caused by a materialized mutation reference the mutation/event ID
  and update its result instead of creating a new logical mutation.

## 11. Configuration and security

Expand `serve.json` additively:

```json
{
  "port": 8391,
  "mode": "standalone",
  "machineId": "macbook-lukas",
  "coordinator": {
    "enabled": false,
    "url": "https://tn.example.internal",
    "tokenFile": "/path/to/mode-0600/token",
    "shadow": true
  },
  "gitExchange": {
    "enabled": false,
    "urlTemplate": "https://git.tn.internal/{project}.git",
    "credentialFile": "/path/to/mode-0600/git-token"
  },
  "vault": {
    "writerEligible": true,
    "runtimeDir": "/path/to/vault/TaskNotes Runtime",
    "materializeEvents": false
  },
  "projects": {
    "tasknotes-cli": {
      "autoSpawn": true,
      "cwd": "/path/to/repo",
      "capabilities": ["repo"],
      "offlinePolicy": "offline-owned",
      "integration": {
        "enabled": false,
        "updatePolicy": "snapshot",
        "checkpointTests": ["go test ./... -count=1"]
      }
    }
  }
}
```

Compatibility rules:

- Missing `mode` means `standalone`.
- Missing coordinator configuration performs no network activity.
- Existing project objects parse unchanged.
- Existing environment injection remains machine-local.
- `TN_BRIDGE_URL` and port defaults do not change.
- Config fields unknown to an old binary are harmless because Go JSON unmarshalling
  ignores them.

Security requirements:

- TLS for all coordinator traffic; prefer a private overlay network first.
- Per-machine revocable tokens with narrow scopes, stored outside `serve.json` in a
  mode-0600 file or OS credential store.
- Separate short-lived/scoped Git credentials; task workers may update only their work
  refs, while integration credentials may update only their leased integration ref.
- Coordinator stores token hashes, not tokens.
- Machine identity and token identity must match.
- Request size limits, timeouts, replay protection through event IDs, and rate limits.
- Worker allowlists per project and execution lane.
- No remote shell endpoint. Coordinator assigns typed work; the local edge performs
  known spawn operations from its own configuration.
- Never synchronize secret values into events, logs, task notes, dashboards, or vault
  files.

## 12. Implementation workstreams

### Workstream A: storage and event core

Add:

- `event.go`: envelope, kinds, validation, ID generation, checksum;
- `event_store.go`: `EventStore` and snapshot interfaces;
- `journal_store.go`: append, replay, recovery, compaction, cursor persistence;
- `read_model.go`: deterministic state reduction from events;
- `migration_v2.go`: state.json bootstrap and compatibility snapshot.

Refactor existing mutations so a successful legacy mutation can additionally emit an
event. In the first release JSON state remains primary; journal dual-write failures are
visible but do not change legacy behavior while shadow mode is off.

### Workstream B: edge synchronization

Add:

- `edge_sync.go`: handshake, push/pull loops, retry/backoff, cursor management;
- `edge_status.go`: sync health, outbox size/age, conflicts;
- bridge endpoints and CLI commands for `tn sync status|flush|doctor`;
- configuration parsing with all new fields default-disabled.

Keep sync loops independent of inbox condition variables so a slow network cannot
block localhost handlers.

### Workstream C: coordinator

Add:

- `coordinator.go`: versioned API and authentication middleware;
- `coordinator_store.go`: global event ingestion and cursors;
- `lease.go`: grant/renew/release/expiry and fencing tokens;
- `scheduler.go`: project/capability/lane-aware worker selection;
- `replica.go`: task snapshot plus pending mutation overlay;
- `coordinator_status.go`: operator and health views.

The coordinator must be able to run with all dispatch disabled, allowing safe shadow
replication before it controls any work.

### Workstream D: replica task backend

Add a `TaskBackend` interface beneath current CLI commands. The direct implementation
wraps the existing `Client`; the replica implementation calls the local edge. Keep
output compatible and add a single concise “pending vault sync” line only when relevant.

Update spawned remote orchestrator environments to select the replica backend. Do not
change existing local agent environments during migration.

### Workstream E: remote worker and routing

Generalize current `spawnFunc` into a worker executor interface while keeping the local
tmux implementation. Namespace coordinator-visible agent identity by machine while
retaining the current local display name:

```text
global: <machine-id>/<agent-name>
local:  <agent-name>
```

Add capability advertisement, work long-polling, lease heartbeats, legacy ownership
import, and fenced lane checks. Stuck-prompt approval remains edge-local; coordinator
status may link or proxy a typed approval request but never arbitrary keystrokes.

### Workstream F: vault writer and Markdown projections

Add:

- semantic mutation applicator using the existing TaskNotes API client;
- vault-writer leadership lease;
- immutable event note renderer;
- machine/session/global dashboard renderers with strict path ownership;
- projection index and content-hash verification;
- Obsidian freshness/conflict sections.

The existing renderers remain unchanged until new output paths have been reviewed.

### Workstream G: TaskNotes plugin UI

This repository does not contain the TaskNotes Obsidian plugin, so treat the plugin
work as a separately versioned dependency:

- index `TaskNotes Runtime/Events` by `task-id`;
- render progress/lease/sync state in task views;
- provide conflict-resolution actions that emit resolution events;
- show pending-vault-materialization state;
- add settings for runtime folder and coordinator status source.

The CLI implementation remains usable before this workstream lands through generated
Markdown indexes and dashboards.

### Workstream H: dependency graph and Git exchange

Add:

- `task_graph.go`: DAG validation, graph versions, dependency readiness;
- `artifact.go`: descriptors, evidence, publication, supersession;
- `integration.go`: integration lease, checkpoint manifests, completion propagation;
- `git_exchange.go`: object/ref verification and restricted ref policy;
- `deps.go`: `tn deps status|sync` and input-manifest handling;
- coordinator pre-receive authorization adapter or deployment hooks;
- integration-aware dashboard, MR watcher, and TaskNotes mutation logic.

Do not combine this workstream with initial transport/journal work. The exchange remote
is enabled per project only after ordinary remote standalone tasks work end to end.

## 13. Zero-interruption migration plan

Follow expand → shadow → canary → migrate → verify → contract. Every phase has an
explicit stop gate. Do not continue automatically after a failed gate.

### Phase 0: inventory and recovery baseline

No runtime behavior changes.

1. Record the running binary version/hash, launchd definition, `serve.json` shape,
   TaskNotes API health, current agents, queued/delivered messages, and dashboard paths.
2. Copy `state.json` to a timestamped, read-only backup while the daemon is quiescent
   under its existing state lock or through a new snapshot endpoint. Do not copy it in
   the middle of a write.
3. Export a TaskNotes task inventory and record counts by status/project.
4. Confirm the webhook-miss reconciler watermark and that the daemon log has no ongoing
   TaskNotes outage.
5. Document how to restore the current binary/config/state and kickstart launchd.

Gate:

- Existing tests pass.
- Backup can be loaded by the current binary in a scratch home directory.
- A scratch smoke test can register/send/inbox/ack without touching the real vault.

Rollback: none needed; this phase is read-only apart from backups.

### Phase 1: compatibility release with dormant v2 code

Ship storage interfaces, event types, new config parsing, and status fields, all disabled
by default. `mode` remains `standalone`; no coordinator connection, journal, task ID
write, or new Markdown projection occurs.

Deployment impact:

- Building the signed binary causes the current launchd daemon to restart, but tmux
  orchestrators continue running independently.
- Inbox monitors reconnect using their existing retry loop.
- Persisted message state reloads from the unchanged `state.json`.
- Missed TaskNotes webhooks are covered by the existing reconciler.

Gate:

- Before and after restart, compare agents, accepting/draining state, pending messages,
  `assignedOccurrences`, `MRStates`, and `spawnPaused`.
- Confirm no agent generation changed and no duplicate assignment was emitted.
- Run at least one normal local task through review with v2 disabled.

Rollback:

- Restore the prior signed binary. Config and state remain readable because no v2
  fields or files are required.

### Phase 2: local journal shadow dual-write

Enable `journal.shadow=true` only on the existing Mac. The current in-memory/JSON path
remains authoritative. For each mutation, append an equivalent v2 event after the
legacy state mutation. Do not read behavior from the journal yet.

Add a comparator that reconstructs the v2 read model and reports differences without
changing live state. Expected differences must be explained; silent divergence is a
gate failure.

Gate:

- Seven consecutive days or an agreed representative workload with zero unexplained
  divergence.
- Kill/restart tests at every journal write boundary recover to the last valid record.
- Journal outage/full-disk simulation leaves legacy service behavior intact and raises
  a visible degraded warning.

Rollback:

- Disable shadow journaling and restart. Keep `v2/` for analysis; do not delete it.

### Phase 3: coordinator in receive-only shadow mode

Deploy the coordinator with dispatch, leasing, vault writing, and remote spawning
disabled. Connect the existing edge and replicate events/snapshots outbound.

Bootstrap rules:

- Import current agents/messages with machine namespace.
- Mark queued/delivered messages as legacy-local-delivery.
- Import inferred task ownership as grandfathered leases.
- Import dedup maps and watermarks for observation only.
- Never send an imported message back to the edge.

Gate:

- Coordinator and edge counts/cursors converge after restart and after an artificial
  network partition.
- Duplicate event batches produce no duplicate read-model entries.
- No coordinator request can spawn, assign, update TaskNotes, or render into the vault.

Rollback:

- Set `coordinator.enabled=false`. The existing daemon remains fully authoritative;
  coordinator data is an unused mirror.

### Phase 4: new Obsidian projections at non-conflicting paths

Enable immutable event rendering on the existing Mac under `TaskNotes Runtime/`, while
retaining the current `Claude Sessions.md` and session directory unchanged. Then add
the coordinator-owned global dashboard under `TaskNotes Runtime/Global/Dashboard.md`.

If Obsidian Headless is used, first attach it to a dedicated scratch/test vault, then
the automation vault. It is the only writer of `Global/`; the Mac edge is the only
writer of `Machines/macbook-lukas.md` and its machine session directory.

Gate:

- Local offline events appear immediately in Obsidian.
- Remote/coordinator test events appear after sync.
- Delivering one event through both channels creates one rendered event.
- Concurrent editing of an unrelated task note produces no generated-file conflict.
- Existing dashboards continue to work.

Rollback:

- Disable new materialization. New files are additive and can remain; do not remove
  them during rollback. Existing dashboards were never replaced.

### Phase 5: remote worker observe-only

Install the compatibility binary on one remote machine. Configure project checkout and
capabilities locally, but leave `acceptWork=false`. Verify heartbeats, repo visibility,
secret-name reporting, task replica reads, and status display.

Gate:

- Disconnect/reconnect produces correct online/offline state without message loss.
- The remote worker cannot acquire a lease or spawn while observe-only.
- No vault credentials or project secret values exist on the coordinator unless
  explicitly required by that host.

Rollback: stop the remote edge or revoke its token. Local work is unaffected.

### Phase 6: isolated canary remote execution

Create a dedicated canary project or explicitly targeted canary task with no shared
infrastructure side effects. Enable one remote worker and coordinator leases only for
that canary scope.

Do not make the coordinator the general assignment authority yet. The existing Mac
continues all normal routing and scanners.

Canary sequence:

1. Coordinator grants a lease to the remote edge.
2. Remote edge spawns an orchestrator that uses the replica task backend.
3. `tn get`, notes, progress, status, and ack operations are journaled remotely.
4. Progress event Markdown is materialized centrally and becomes visible in Obsidian.
5. The Mac vault writer applies semantic mutations to the TaskNotes API.
6. The resulting TaskNotes webhook is correlated and does not create a second mutation.
7. The task reaches `review`; its MR/evidence is visible from Obsidian.

Gate:

- Repeat successfully while online, with the Mac offline temporarily, with the remote
  worker offline temporarily, and with the coordinator restarted.
- No duplicated note, assignment, branch, or orchestrator generation.
- Pending status is clearly visible while the Mac vault writer is disconnected.
- Reconnection materializes every accepted mutation exactly once.

Rollback:

- Disable canary dispatch.
- Allow the already-leased remote task to finish or explicitly drain it; do not
  reassign it automatically.
- Existing local routing never changed.

### Phase 6a: dependent-work and integration canary

After standalone remote execution passes, enable the exchange remote for one disposable
repository/project only. Create a graph with two parallel component tasks, one dependent
task requiring an integration checkpoint, and one integration task.

Canary requirements:

1. Components execute on different machines from the same pinned upstream base.
2. Both publish immutable artifacts to `tn-exchange`.
3. One integration worker merges them and publishes a checkpoint.
4. The dependent task starts from that exact checkpoint on a third attempt/worker.
5. Publish an amended component artifact while the dependent task is active and verify
   snapshot policy: notification appears, but no automatic worktree mutation occurs.
6. Integration owner incorporates the amendment, runs full tests, and opens one
   canonical MR.
7. Merge propagation completes exactly the integrated graph nodes.

Gate:

- Every worker can reconstruct its input from recorded SHAs.
- Exchange hook rejects a worker writing another task's work ref, any sealed artifact,
  or the integration branch without the integration lease.
- Exchange outage buffers local work and visibly blocks publication without losing it.
- Coordinator restart reconstructs graph/artifact/integration state.
- One deliberate merge conflict produces an actionable integration conflict and a
  successful amendment flow.
- Existing standalone projects and their MR watcher behavior are unchanged.

Rollback:

- Disable graph dispatch and new artifact publication for the canary project.
- Preserve exchange refs and let the current integration lease finish or drain.
- Push the verified integration branch to canonical origin before decommissioning any
  canary storage. Do not rewrite component tasks into standalone tasks automatically.

### Phase 7: coordinator leasing for new work

Enable coordinator assignment project by project. The local `:8391` bridge remains the
facade for every current local agent. New assignments receive coordinator leases;
grandfathered legacy ownership remains local until naturally drained.

Singleton transition:

- Existing Mac retains vault-writer leadership.
- Existing Mac retains TaskNotes webhook, due scanner, unblock pass, MR watcher, and
  webhook-miss reconciler.
- Other edges do not register TaskNotes webhooks or run these singleton jobs.
- Coordinator routing decisions enter the Mac/remote edge through event sync.

Gate per project:

- Backlog assignment and load balancing match configured limits.
- A disconnected edge receives no new unrestricted task.
- An offline-owned task continues without being duplicated.
- Lease expiry/reassignment requires the safety delay and produces an Obsidian-visible
  handoff record.
- Existing agents can drain/retire normally.

Rollback per project:

- Disable coordinator dispatch for that project.
- Preserve current leases until completion/drain.
- Route subsequent new work locally. Do not revoke active remote work by surprise.

### Phase 8: offline-first local authority

Switch the edge event journal from shadow to primary for newly accepted operations,
with `state.json` maintained as a compatibility snapshot after each committed event
batch. Existing handlers now acknowledge only after durable journal append.

Gate:

- Full state reconstructs from journal plus snapshot in scratch runs.
- Compatibility `state.json` matches required old-binary fields.
- Downgrading to the previous compatible binary preserves agents/messages/dedup maps.
- Disk-full and torn-write tests fail closed without acknowledging lost work.

Rollback:

- Stop new remote assignment, flush the compatibility snapshot, and restore the prior
  binary. Never roll back using a stale pre-migration `state.json` without reconciling
  events created after it.

### Phase 9: optional vault-writer failover

Only after multi-machine execution is stable, make vault-writer leadership transferable
to another interactive machine. Obsidian Headless alone is not sufficient to apply
TaskNotes API mutations unless direct Markdown mutation is separately designed and
verified; prefer a machine running Obsidian and the TaskNotes API.

Leadership uses a coordinator lease with a fencing token. A new writer starts only
after the old writer has stopped singleton jobs or its lease has expired plus a safety
delay. The first action is reconciliation, not mutation replay.

Gate:

- Planned handoff and crash failover both apply queued mutations exactly once.
- Exactly one webhook/scanner/MR-watcher set is active.
- Old writer cannot apply mutations after losing its fencing token.

Rollback: elect the existing Mac again after the same fenced handoff procedure.

### Phase 10: contraction, separately approved

Do not enter this phase as part of initial implementation.

Possible later cleanup:

- stop writing legacy `state.json` after the downgrade window closes;
- archive old journal segments whose coordinator acknowledgement and snapshot recovery
  have been verified;
- replace the legacy dashboard with a link to the accepted global dashboard;
- backfill `tn-id` to inactive tasks in bounded batches;
- remove legacy path-only routing after every active task has a stable ID;
- remove protocol-v1 compatibility only after every edge is upgraded.

Each deletion or irreversible format change requires a fresh backup, explicit approval,
and a tested restore path.

## 14. Verification strategy

### 14.1 Unit tests

- event validation, identity, checksum, and origin sequencing;
- journal append/replay, corrupt-tail recovery, compaction, and cursor durability;
- idempotent reducer behavior under duplicate and reordered delivery;
- state.json import/export and downgrade compatibility;
- task-ID aliasing and rename handling;
- semantic note/status/field merge rules;
- lease acquisition, renew, expiry, fencing, and grandfathering;
- scheduler capability/lane filtering;
- graph cycle/version/readiness validation;
- artifact ancestry, evidence, sealing, and supersession;
- integration checkpoint composition and completion propagation;
- ref authorization and stale integration fencing tokens;
- projection path ownership and event content-hash checks;
- configuration defaults proving all new behavior is disabled.

### 14.2 Integration tests

Build an in-process harness with one coordinator, two edges, fake TaskNotes APIs, fake
workers, and temporary vaults. Test:

- online normal flow;
- edge offline during progress and reconnection;
- coordinator restart after accepting an event but before responding;
- response loss followed by push retry;
- cursor replay and origin gap recovery;
- simultaneous human and remote status changes;
- existing delivered legacy message during bootstrap;
- alive legacy owner while coordinator sees new backlog;
- Obsidian event arriving before protocol event and vice versa;
- vault writer unavailable, then recovery;
- worker death and safe lease handoff;
- old and new binaries operating concurrently.
- parallel component artifacts merged in deterministic graph order;
- dependent task pinned to a composite checkpoint;
- artifact amendment during dependent execution under each update policy;
- integration conflict, worker replacement, and worktree reconstruction;
- exchange available while canonical origin is unavailable, then upstream recovery;
- unauthorized and stale-lease pushes rejected by exchange hooks.

Use controllable clocks; no test should sleep for production lease durations.

### 14.3 Crash and partition tests

Terminate processes at each durable boundary:

- before journal append;
- after append but before HTTP response;
- after coordinator ingest but before edge acknowledgement persistence;
- after TaskNotes PUT but before `task.mutation.applied`;
- during snapshot rename;
- during projection file rename.
- after Git object push but before artifact publication;
- after artifact ref sealing but before publication response;
- after integration merge but before checkpoint publication.

For each boundary, restart and prove either the operation was never acknowledged or it
is present exactly once.

### 14.4 Real-system canary checks

- Signed build, `gofmt`, `go vet`, and `go test ./... -count=1` clean.
- Scratch-home local bridge smoke test on an 18xxx port.
- Scratch-vault projection test; never use the real vault for automated tests.
- Coordinator backup/restore rehearsal.
- One disposable repository canary producing a real branch/MR.
- Deliberate Wi-Fi disconnect while local work continues and remains visible in
  Obsidian.
- Reconnect and compare coordinator event count, edge outbox, TaskNotes history, and
  projected event files.
- Run the Phase 6a graph through one integration MR and verify every component SHA is
  reachable from the upstream merge commit.

## 15. Observability and operator controls

Extend `/status`, `/ui`, and the Obsidian dashboard with:

- machine ID and role;
- coordinator state and last successful push/pull;
- local and remote cursors;
- outbox count, bytes, and oldest-event age;
- vault writer and fencing token age;
- worker capabilities and last heartbeat;
- leases grouped by active/offline-grace/expired/grandfathered;
- pending task mutations and oldest age;
- projection backlog and last Obsidian materialization;
- unresolved conflicts;
- vault-sync freshness supplied by the headless/local sync integration when available.
- task-graph readiness and blocked reason per node;
- local/published/integrated/upstreamed artifact counts;
- exchange remote health, storage, and oldest non-upstreamed artifact age;
- integration lease, branch head, latest checkpoint, and conflict state;
- tasks with newer dependency artifacts than their pinned manifest.

Operator controls:

- pause new global dispatch without stopping sync;
- pause one project or worker;
- drain a worker while allowing owned work to finish;
- reserve tasks for offline work;
- force an outbox flush;
- export a diagnostic bundle without secrets;
- resolve/reject a task mutation conflict;
- revoke a machine token;
- transfer vault-writer leadership through a fenced handoff.
- retry artifact publication, reassign an integration lease, and freeze a task graph;
- inspect an input manifest and request an explicit dependency refresh.

Alerts should be state transitions, not repeated log spam. Minimum warnings:

- outbox oldest age above threshold;
- origin sequence gap;
- vault writer unavailable with pending mutations;
- lease expired while agent still reports activity;
- projection hash conflict;
- journal recovery truncated a corrupt tail;
- coordinator backup failure.
- exchange artifact not reachable from its declared ref;
- integration checkpoint verification failure;
- canonical MR missing one or more required component artifacts.

## 16. Failure behavior matrix

| Failure | Required behavior |
|---|---|
| Laptop loses internet | Existing local work continues; local Obsidian updates; events buffer |
| Coordinator unavailable | Edges enter offline mode; owned work continues; no unrestricted new global claims |
| Obsidian desktop closed | Events and task mutations remain durable; materialization waits |
| TaskNotes API unavailable | Progress event files still render; semantic mutations queue |
| Obsidian Sync unavailable | Each vault remains locally usable; generated files queue in its normal sync path |
| Remote worker dies | Lease enters suspect/expiry flow; no immediate duplicate execution |
| Vault writer dies | Pending mutations wait; another writer requires fenced election |
| Duplicate event delivery | No-op with existing cursor/result returned |
| Concurrent task field edits | Conflict note/card; neither value silently discarded |
| Same immutable event file differs | Integrity conflict; preserve both bytes for inspection |
| Disk full before append | Command fails; operation is not acknowledged |
| Disk full after durable append | Operation is recoverable and retried from journal |
| Exchange remote unavailable | Local commits continue; publication and cross-machine dependents wait |
| Canonical origin unavailable | Exchange integration continues from pinned base; final push/MR waits |
| Artifact pushed but publish response lost | Retry returns the same sealed artifact by idempotency key |
| Component publishes an amendment | Existing consumers stay pinned; notification and explicit policy decide refresh |
| Integration worker dies | Replacement reconstructs from integration ref, manifest, and sealed artifacts |
| Stale worker pushes integration ref | Exchange rejects stale fencing token/credential |
| Required component absent from final merge | MR completion propagation stops and raises graph reconciliation conflict |

## 17. Acceptance criteria

The implementation is ready for general project-by-project rollout when all of the
following are true:

- The existing machine can run disconnected for a representative work session, show
  progress in Obsidian, and synchronize without duplicates afterward.
- A remote worker can complete a canary task using only its local edge and coordinator
  connection; no Obsidian desktop or vault credential is present on that worker.
- Remote progress becomes visible in the shared vault through immutable event files.
- A remote task can request notes/status while the vault writer is offline, and every
  mutation materializes exactly once when it returns.
- Current agents survive an edge binary restart and are never duplicated during
  bootstrap or cutover.
- Old standalone configuration retains current behavior with no network calls.
- Every migration phase has been rolled back successfully in a scratch environment.
- Status views clearly distinguish coordinator connectivity, Obsidian/vault freshness,
  pending local upload, and pending TaskNotes materialization.
- No automatic contraction or deletion is enabled.
- A decomposed canary can execute components on different machines, publish immutable
  artifacts, create a composite checkpoint, and start a dependent from exact SHAs.
- One integration owner can absorb component updates and produce a single canonical MR
  whose merge commit contains every required artifact.
- Workers never consume silent moving-branch updates; every dependency refresh is
  pinned, recorded, and verified.
- Exchange artifacts survive coordinator/worker restarts and are retained until
  confirmed reachable from canonical upstream.

## 18. Recommended delivery slices

Keep changes reviewable and independently releasable:

1. Event types, journal, replay, and compatibility snapshot.
2. Dormant config/status surface with no behavior change.
3. Shadow dual-write and read-model comparator.
4. Coordinator receive-only ingestion and edge sync.
5. Immutable Obsidian event projection and machine-owned pages.
6. Stable task IDs and semantic mutation journal.
7. Replica task backend for remote `tn` commands.
8. Worker capability advertisement and observe-only mode.
9. Lease/scheduler implementation plus legacy ownership import.
10. Isolated remote execution canary.
11. Task graph, immutable artifact model, and exchange remote in observe-only mode.
12. Integration worker, checkpoints, and dependent-work canary.
13. Project-scoped coordinator dispatch.
14. Journal-primary cutover and downgrade verification.
15. TaskNotes plugin progress/conflict/integration UI.
16. Optional fenced vault-writer failover.

Slices 1–5 are additive foundations. Slice 10 is the first point at which remote code
execution is allowed. Slice 12 is the first point at which dependent work is integrated
through the exchange remote. Slice 13 is the first point at which normal work may be
routed remotely. Slice 14 changes the local persistence authority and therefore
requires the strongest downgrade gate. Slice 16 is optional and should not block useful
remote execution.

## 19. Explicitly deferred decisions

- Replacing the standard-library journal with SQLite/PostgreSQL.
- Allowing optimistic offline claims by default.
- Direct headless mutation of arbitrary TaskNotes Markdown without the plugin API.
- Automatically resolving concurrent description edits.
- Synchronizing secrets through the coordinator.
- Deleting legacy state or old progress events.
- Making remote workers reachable through inbound SSH or a generic shell API.
- Allowing workers to merge arbitrary sibling branches without an integration lease.
- Treating a mutable Git branch name as a reproducible dependency input.
- Deleting exchange artifacts before upstream reachability is verified.

These are not required to deliver safe offline-first remote execution and would expand
the risk surface of the migration.
