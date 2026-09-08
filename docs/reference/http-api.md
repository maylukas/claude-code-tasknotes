# HTTP API reference

Exhaustive reference for the `tn serve` bridge daemon's HTTP API.

- **Base URL:** `http://localhost:8391` (see [configuration.md](configuration.md#ports) for how to change the port). The daemon binds to `127.0.0.1` only.
- **Format:** JSON request and response bodies (`Content-Type: application/json`), except where noted.
- **Authentication:** none. See [../explanation/security-model.md](../explanation/security-model.md) for why, and what that implies about who else on the machine can reach this API.
- **Live updates:** `GET /events` is a Server-Sent Events stream for anything that wants to react to daemon state changes without polling.

## Error convention

There is no single uniform error-body shape across the API. This is a real, observed inconsistency, documented here rather than papered over:

- Most validation failures and "not found" responses use Go's standard `http.Error`, which writes a **plain-text** body (`Content-Type: text/plain; charset=utf-8`), just the message, not JSON.
- Successful responses, and a handful of structured "soft failure" responses, use a local `writeJSON` helper: `Content-Type: application/json`, with a JSON-encoded body.
- A few 409 Conflict responses carry structured JSON specifically because the caller needs more than a message: for example `POST /agents/retire`'s pending-messages conflict returns `{"error": "pending messages"}`, and `POST /agents/approve`'s conflicts return `{"sent": false, "reason": "..."}"`. These are the exceptions, not the rule; most 4xx bodies from this API are plain text.

When in doubt, check the endpoint's own **Status codes** section below rather than assuming a shape.

A wrong HTTP method on any registered path returns Go's built-in `405 Method Not Allowed` (the mux is registered with explicit `"METHOD /path"` patterns). This isn't repeated per-endpoint below.

---

## Agents

### `POST /agents/register`

Registers or revives an agent session.

**Request:**

| Field | Type | Required | Description |
|---|---|---|---|
| `name` | string | yes | Agent name: the registry key. |
| `project` | string | yes | Project slug this agent serves. |
| `sessionId` | string | no | Claude Code session ID. Only overwrites the stored value if non-empty. |
| `tmuxSession` | string | no | tmux session name. Only overwrites the stored value if non-empty. |

**Response:** `200`: `{"agent": {...}}` (full `Agent` record: name, project, sessionId, registeredAt, lastSeenAt, deregisteredAt, accepting, tmuxSession, contextPct, contextPctAt).

**Status codes:**
| Code | Condition |
|---|---|
| `400` | Invalid JSON body, or `name`/`project` empty. |
| `200` | Registered or revived. |

**Notes:** Always sets `lastSeenAt=now`, clears `deregisteredAt`, and sets `accepting=true`: re-registering always resumes accepting; only `tn drain` turns it back off. Clears any pending spawn-intent for the project. Re-targets any messages queued to the logical fallback name `orchestrator-<project>` onto this concrete agent name, so a fresh generation inherits stranded work left by a previous one.

### `POST /agents/drain`

Marks an agent as no longer accepting new task assignments. Idempotent.

**Request:** `{"name": "..."}`: `name` required.

**Response:** `200`: `{"agent": {...}}`.

**Status codes:** `400` invalid JSON or empty `name` · `404` agent not found · `200` drained.

**Notes:** Only flips `accepting=false`. The agent keeps polling, acking, and finishing owned work: draining is a "stop assigning new" signal, not a shutdown.

### `POST /agents/context`

Reports an agent's context-window usage percentage. Intended caller: a statusline wrapper on every render, not the Claude Code permission hook.

**Request:**

| Field | Type | Required | Description |
|---|---|---|---|
| `name` | string | yes | Agent name. |
| `usedPct` | float64 | no | Context usage, 0–100. |

**Response:** `200`: `{"agent": {...}}`.

**Status codes:** `400` invalid JSON or empty `name` · `404` agent not found · `200` recorded.

**Notes:** Counts as a liveness signal (bumps `lastSeenAt`), same as an inbox poll. Auto-drain: if the agent is currently accepting and `usedPct >= 40`, the daemon flips it to draining, logs an activity entry, and dispatches a message telling it to finish owned work and run `tn retire`. Fires once per crossing.

### `POST /agents/retire`

An agent's final, daemon-mediated self-termination command.

**Request:** `{"name": "..."}`: `name` required.

**Response:** `200`: `{"agent": {...}}` (returned before the tmux session is actually killed).

**Status codes:**
| Code | Condition |
|---|---|
| `400` | Invalid JSON or empty `name`. |
| `404` | Agent not found. |
| `409` | JSON `{"error": "pending messages"}`: the agent still has queued/delivered (unacked) messages **and** is still accepting (hasn't drained first). |
| `200` | Retire accepted. |

**Notes:** If the agent already drained (not accepting) but still has pending messages, retire proceeds anyway: those messages are reassigned to another target for the same project and requeued rather than left stranded. Two seconds after responding, the daemon kills the agent's tmux session (the delay avoids killing the session mid-HTTP-response). Also triggers the orphaned-in-progress-task safety check for that agent.

### `POST /agents/deregister`

A lighter tombstone than retire: no message reassignment, no tmux kill, no orphan check.

**Request:** `{"name": "..."}`.

**Response:** `200`: `{"agent": {...}}`.

**Status codes:** `400` invalid JSON · `404` agent not found · `200` deregistered.

**Notes:** Only sets `deregisteredAt=now`.

### `POST /agents/purge`

Permanently deletes an agent record, unlike retire/deregister, which only tombstone.

**Request:** `{"name": "..."}`.

**Response:** `200`: `{"purged": "<name>"}` (a different shape than the other agent-lifecycle endpoints, since the record no longer exists).

**Status codes:** `400` invalid JSON · `404` agent not found · `200` purged.

**Notes:** Deletes the agent record, its worker entries, and any permission-denial records attributed to it. Intended for cleaning up test/verification agent registrations (see the `zz-` naming convention this project uses internally) or other pure registry pollution, not part of normal agent lifecycle.

### `GET /agents`

Lists every registered agent, unfiltered, sorted by name.

**Request:** none.

**Response:** `200`:
```json
{
  "agents": [
    {
      "name": "orchestrator-myapp-g3",
      "project": "myapp",
      "sessionId": "...",
      "registeredAt": "2026-09-02T10:00:00Z",
      "lastSeenAt": "2026-09-02T10:05:00Z",
      "deregisteredAt": null,
      "alive": true,
      "accepting": true,
      "tmuxSession": "tn-myapp-g3",
      "contextPct": 12.5
    }
  ]
}
```

**Notes:** `alive` reflects whether the agent has been seen (inbox poll or other liveness signal) within the last 90 seconds.

### `POST /spawn/pause` and `GET /spawn/pause`

One handler serves both methods.

**Request (POST only):** `{"paused": true}`: `paused` optional, `false` if omitted.

**Response:** `200`: `{"paused": <bool>}` for both methods.

**Status codes:** `400` invalid JSON on POST · `200` otherwise.

**Notes:** A global (not per-project) kill switch: while paused, every spawn path skips actually spawning; everything else (routing, drain, retire) keeps working. `GET` is read-only with no side effects.

### `POST /agents/approve`

Server-side equivalent of clicking the stuck-prompt approval action, used by the tray/menu-bar client, not by `hooks/claude-hook.sh`.

**Request:**

| Field | Type | Required | Description |
|---|---|---|---|
| `name` | string | yes | Agent name. |
| `option` | string | yes | One of `"once"`, `"always"`, `"deny"`. |

**Response:** `200`: `{"sent": <bool>, "reason": "<string>"}`.

**Status codes:**
| Code | Condition |
|---|---|
| `400` | Invalid JSON, empty `name`, or `option` not one of the three allowed values. |
| `404` | Agent not found. |
| `409` | JSON `{"sent": false, "reason": "agent is deregistered"}` or `{"sent": false, "reason": "agent has no current stuck prompt"}`. |
| `200` | Handled: `sent` may still be `false` (e.g. the prompt cleared itself between detection and send) with a `reason` explaining why. |

**Notes:** Re-captures the tmux pane fresh before sending, to avoid acting on a stale "stuck" state a human may have already resolved manually. Never re-opens the daemon's own popup regardless of outcome.

### `POST /hooks/permission-request`

Receives Claude Code's `PermissionRequest` hook event, POSTed by `hooks/claude-hook.sh` (installed by the user into their own `~/.claude/settings.json`; only fires when `$TN_AGENT_NAME` is set in the session's environment, which scopes it to daemon-spawned sessions).

**Request:**

| Field | Type | Required | Description |
|---|---|---|---|
| `agentName` | string | yes | From `$TN_AGENT_NAME`. |
| `sessionId` | string | no | Claude Code's `session_id`. |
| `promptId` | string | no | Claude Code's `prompt_id`. |
| `toolName` | string | no | Claude Code's `tool_name`. |
| `permissionMode` | string | no | Claude Code's `permission_mode`. |
| `command` | string | no | `tool_input.command`, extracted by the hook script only for Bash-shaped tool calls. |

**Response:** `200`: `{"received": true}`.

**Status codes:** `400` invalid JSON or empty `agentName` · `404` agent not found · `409` plain text `"no resolvable tmux session for agent"` · `200` received.

**Notes:** Detection only: this endpoint never makes an allow/deny decision. It responds immediately (before any slow work) so the hook's own short client-side timeout never delays the real interactive prompt from rendering. After responding, it asynchronously re-captures the pane and feeds the same stuck-detection state machine the periodic pane-scraping loop uses, just faster. A hard `permissions.deny` rule short-circuits before Claude Code fires any hook at all, so this endpoint structurally cannot see that class of block; pane-scraping remains the only signal for it.

### `POST /hooks/permission-denied`

Receives Claude Code's `PermissionDenied` hook event, POSTed by the same `hooks/claude-hook.sh` script.

**Request:** same fields as `/hooks/permission-request`.

**Response:** `200`: `{"received": true}`.

**Status codes:** `400` invalid JSON or empty `agentName` · `404` agent not found · `200` received.

**Notes:** Pure telemetry: never a retry, never a decision (the script does not send retry signals even though the underlying hook event could support one; this is a deliberate product decision). Builds a "command summary" (first two whitespace-separated tokens, e.g. `"git push"`) as a human-actionable grouping key, and aggregates a count/first-seen/last-seen record per `(agent, tool, command summary)` in daemon state.

---

## Messages

Messages move through a fixed lifecycle: `queued` → `delivered` (on the recipient's next `GET /inbox` poll) → `acked` (via `POST /ack`). A fourth state, `archived`, is set only internally (never via an endpoint) when a pending message addressed to a confirmed-dead agent references an already-completed task.

**Message shape** (used throughout this section):
```json
{
  "id": "msg-042",
  "to": "orchestrator-myapp-g3",
  "project": "myapp",
  "taskPath": "tasks/fix-login-bug.md",
  "text": "please prioritize this",
  "from": "team-lead",
  "status": "delivered",
  "createdAt": "2026-09-02T10:00:00Z",
  "deliveredAt": "2026-09-02T10:00:05Z",
  "ackedAt": null,
  "response": "",
  "archivedAt": null,
  "archiveReason": ""
}
```

### `GET /inbox`

Long-polls for messages queued to one agent.

**Query parameters:**

| Param | Required | Description |
|---|---|---|
| `name` | yes | Agent name to poll for. |
| `wait` | no | Long-poll wait budget, seconds. Default `25`. Clamped to a max of `55`; non-numeric values are silently ignored (default kept, no error). |

**Response:** `200`: `{"messages": [...]}`, always, even if the wait timed out with nothing found (an empty array is not distinguishable in status code from "genuinely nothing queued").

**Status codes:** `400` missing/empty `name` · `200` otherwise, always.

**Notes:** This is the only place a message transitions `queued → delivered`. Also serves as the agent's liveness heartbeat: every call, hit or miss, bumps `lastSeenAt`.

### `POST /ack`

Acknowledges a message.

**Request:**

| Field | Type | Required | Description |
|---|---|---|---|
| `name` | string | no | Acking agent's name: bumps its liveness, and is attributed as the answerer if a response is noted onto the task. |
| `msgId` | string | yes | Message ID being acked. |
| `response` | string | no | Free-text reply. |

**Response:** `200`: `{"message": {...full acked Message...}}`.

**Status codes:** `400` invalid JSON or empty `msgId` · `404` no message with that ID · `200` acked.

**Notes:** If `response` is non-empty and the message has a `taskPath`, the response is also appended to the task's history in TaskNotes (attributed `"<name> (answering)"`), skipped silently if it's already present (avoids duplicating a response an agent also wrote via `tn note`). A TaskNotes-side write failure here is logged only; it never turns the ack itself into a non-200.

### `POST /messages`

Sends a message. This is the routing entry point used by `tn send`.

**Request:**

| Field | Type | Required | Description |
|---|---|---|---|
| `to` | string | one of `to`/`project` | Explicit recipient agent name: wins outright over all other routing if set. |
| `project` | string | one of `to`/`project` | Project slug to route by when `to` is empty. |
| `text` | string | yes | Message body. |
| `taskPath` | string | no | Associates the message with a task; also drives owner-routing (see below). |
| `from` | string | no | Sender name. |

**Response:**
```json
{
  "message": { "...": "..." },
  "queued": true,
  "warning": "no live agent"
}
```
`queued` and `warning` are only present when the message was routed to the logical fallback queue rather than a live agent.

**Status codes:**
| Code | Condition |
|---|---|
| `400` | Invalid JSON, empty `text`, or both `to` and `project` empty. Also (only reachable internally, not from this endpoint given the earlier check) `{"error": "cannot route message: ..."}` if no route can be built at all. |
| `202` | Message queued to the logical `orchestrator-<project>` fallback because no live accepting agent exists right now for that project. |
| `200` | Routed directly: to an explicit `to`, a task's current owner, or a live accepting agent. |

**Routing logic** (in order):

1. `to`, if non-empty, wins outright.
2. Otherwise, if `taskPath` is set: route to that task's current **owner** (whoever received the latest *acked* "Task assigned:" message for that path) if that agent is alive, even while draining. This keeps task-scoped follow-ups reaching whoever is actually doing the work.
3. Otherwise, pick a live, accepting agent for `project`, load-balanced across ties by fewest currently-dispatched tasks (so multiple concurrent generations under `max-orchestrators > 1` share load), tie-broken by name.
4. Otherwise, fall back to the logical queue name `orchestrator-<lowercased project>`. If the project's config has `autoSpawn` true and there's no live accepting agent (and no already-in-flight spawn intent), a new orchestrator is spawned; if `autoSpawn` is false, the response carries `"warning": "no live agent"` and nothing is spawned.
5. If `to`, `project`, and any task owner are all unresolvable, there's nowhere to route at all; this case is guarded against by the initial `to`/`project` validation for any external caller.

### `GET /messages`

Reads one message by ID, or a filtered list.

**Query parameters:**

| Param | Required | Description |
|---|---|---|
| `id` | no | Return exactly this message (ignores `to`/`status` if set). |
| `to` | no | Filter by recipient. |
| `status` | no | Filter by exact status (`queued`/`delivered`/`acked`/`archived`). |

**Response:** with `id`: `{"message": {...}}`. Without: `{"messages": [...]}` (always an array, possibly empty, never null).

**Status codes:** `404` `id` given but not found (plain text) · `200` otherwise.

**Notes:** No pagination: returns the full unbounded match. Read-only; does not touch agent liveness.

---

## Workers

Purely a bookkeeping surface for `/status` visibility: the daemon has no other way to see a worker subagent an orchestrator has spawned.

### `POST /workers/start`

**Request:** `{"name": "...", "taskPath": "..."}`: both required.

**Response:** `200`: `{"ok": true}`.

**Status codes:** `400` invalid JSON or either field empty · `200` recorded.

**Notes:** Idempotent: a second `start` for the same `(name, taskPath)` just refreshes the recorded start time rather than duplicating. Also counts as a liveness signal for the orchestrator.

### `POST /workers/end`

**Request:** `{"name": "...", "taskPath": "..."}`: both required.

**Response:** `200`: `{"ok": true}`.

**Status codes:** `400` invalid JSON or either field empty · `200` retracted (silent no-op if nothing matched).

---

## Activity and messaging surfaces

### `POST /activity`

Appends a free-text line to an agent's activity log (`tn log`'s underlying call).

**Request:** `{"name": "...", "text": "..."}`: both required.

**Response:** `200`: `{"activity": {"agent": "...", "at": "...", "text": "..."}}`.

**Status codes:** `400` invalid JSON or either field empty · `200` appended.

**Notes:** Also a liveness signal. The log is capped at 500 entries; oldest are dropped once exceeded.

### `GET /feed`

A merged, newest-first activity timeline combining messages and activity-log entries.

**Request:** none.

**Response:** `200`: `{"feed": [...]}`, each entry:
```json
{"at": "...", "kind": "message", "text": "...", "agent": "...", "to": "...", "taskPath": "..."}
```
`kind` is one of `"message"`, `"acked"`, or `"activity"`. Message/response text is truncated to 200 characters in feed entries only (the underlying stored record is untouched). Capped to the 100 most recent entries.

**Status codes:** `200` always: no request parameters to validate.

### `POST /reply`

Writes a human's reply into a task's structured "Reply" block, the counterpart to editing that block directly in Obsidian.

**Request:** `{"taskPath": "...", "text": "..."}`: both required after trimming.

**Response:** `200`: `{"ok": true}`.

**Status codes:**
| Code | Condition |
|---|---|
| `400` | Invalid JSON, empty `taskPath`/`text`, or text is empty after stripping a redundant `@claude:` prefix. |
| `503` | No TaskNotes client configured on this daemon instance. |
| `502` | Fetching or updating the task in TaskNotes failed. |
| `200` | Written. |

**Notes:** The text is always written with exactly one `@claude: ` prefix (an existing one in the submitted text is stripped first, to avoid doubling it). This endpoint does not itself dispatch a bridge message; writing the `@claude:` line triggers TaskNotes' own update webhook, which the daemon's existing webhook handler detects and routes normally. See [../explanation/lifecycle-of-a-task.md](../explanation/lifecycle-of-a-task.md).

---

## Tasks

### `POST /tasks/create`

Proxies to TaskNotes' own `POST /api/tasks`, with idempotency: the mechanism behind `tn create`.

**Request:**

| Field | Type | Required | Description |
|---|---|---|---|
| `fields` | object | yes | The task fields to create, same shape as TaskNotes' own create body (title, details, status, customProperties, and so on). |
| `idempotencyKey` | string | no | A caller-derived key (`tn create` hashes title + `claude-project` + details). A repeat call with the same non-empty key within the idempotency TTL returns the original task instead of creating a second one. |

**Response:**
- `{"task": {...}}`: created (or, on a dedup hit, the original task, with `"duplicate": true` added).
- On a genuinely ambiguous upstream failure (see Notes): `{"error": "...", "ambiguous": true, "title": "..."}`.

**Status codes:**
| Code | Condition |
|---|---|
| `400` | Invalid JSON body. |
| `503` | No TaskNotes client configured on this daemon instance. |
| `200` | Created, or a dedup hit (idempotency key or ambiguity-recovery match): check for `"duplicate": true` / `"ambiguous": true` in the body to tell which. |
| `409` | The underlying TaskNotes call failed in a way that leaves it **genuinely unknown** whether the create landed (a timeout or dropped connection, not a clean HTTP error), and a bounded backoff re-query (roughly 40 seconds, several attempts) found no matching task either. The response explicitly instructs the caller not to blindly retry: check whether the task now exists first. |
| `502` | The underlying TaskNotes call failed with a clean, unambiguous error (for example a validation failure); nothing was created. |

**Notes:** A clean error (TaskNotes flatly rejected the request, or refused the connection) is surfaced immediately as `502` with no retry. Only a genuinely ambiguous failure (timeout/dropped connection, where the write might have actually landed) triggers the backoff-and-requery dance before answering: this distinction exists because blindly retrying an ambiguous create risks creating a duplicate task.

### `POST /tasks/close`

The synchronous half of the "DONE MEANS" mechanism, the underlying call for `tn done` / `tn status <path> done`. This is the **only** place a DONE MEANS criteria check runs synchronously before a status write; a close applied directly in Obsidian, or any other path to "done," is checked only informationally, afterward, and is never reverted.

**Request:** `{"path": "..."}`: required.

**Response:** `{"task": {...updated task...}}` on success.

**Status codes:**
| Code | Condition |
|---|---|
| `400` | Invalid JSON, or `path` empty. |
| `503` | No TaskNotes client configured. |
| `502` | Fetching or updating the task in TaskNotes failed. |
| `422` | The task has a "DONE MEANS" criteria block and it's unmet. Body: `{"error": "DONE MEANS criteria unmet", "missingEvidence": [...], "requiredCount": N}`. **The status is not changed**; nothing was written. |
| `200` | Closed: either the task had no DONE MEANS block at all (closes immediately, no friction), or its criteria were satisfied. |

---

## Credentials

### `GET /creds`

Lists credential profiles and the daemon's credential-swap configuration. Spectator-safe: no side effects.

**Request:** none.

**Response:** `200`:
```json
{
  "autoSwap": true,
  "minSwapInterval": "10m0s",
  "active": "work",
  "profiles": [
    {
      "label": "work",
      "email": "you@example.com",
      "active": true,
      "stored": true,
      "savedAt": "2026-08-01T09:00:00Z",
      "lastUsedAt": "2026-09-02T10:00:00Z",
      "lastLimitedAt": null,
      "limitedUntil": null
    }
  ],
  "lastSwap": {"from": "personal", "to": "work", "trigger": "manual", "at": "2026-09-02T10:00:00Z"}
}
```
`stored: false` means a profile has metadata but its Keychain item is missing (an orphaned record). `active` is resolved live by matching the live Keychain credential blob against each stored profile, not just a recorded label. `lastSwap` is `null` if no swap has happened yet during this daemon process's lifetime (in-memory only, not persisted). Never includes secret values.

**Status codes:** `200` normal · `503` plain text `"credential store unavailable"` if no credential store is wired for this daemon instance · `500` if reading the credential metadata fails.

### `POST /creds/swap`

Swaps the active claude.ai credentials, the daemon-side half of `tn creds use`.

**Request:** `{"label": "..."}`: required.

**Response:** `200`: `{"from": "personal", "to": "work", "nudged": ["orchestrator-myapp-g3"]}`. `nudged` lists agent names whose parked, rate-limited tmux panes were resumed after the swap; always an array.

**Status codes:**
| Code | Condition |
|---|---|
| `503` | No credential store wired. |
| `400` | Invalid JSON or empty `label`. |
| `404` | Target label unknown, including a profile whose metadata exists but whose Keychain item is missing. |
| `409` | Target label is already the active profile. |
| `500` | Any other failure (Keychain read/write, account lookup, metadata save). |
| `200` | Swapped. |

**Notes:** Because Claude Code stores its OAuth blob in one Keychain item shared by every session on the machine, a swap affects *every* running Claude Code session at once, not just one. Before overwriting the live item, the daemon writes the live blob back into the *previously* active profile first: this captures any token rotation Claude Code did since the last swap, so swapping back later doesn't land on a dead token.

---

## Worktrees

### `POST /worktrees/reap`

Starts a worktree-reaper pass. Deliberately **asynchronous**: a full pass over a large repository can take minutes, and a synchronous version once stranded its own CLI caller on an HTTP timeout while the pass kept running server-side.

**Request:**

| Field | Type | Required | Description |
|---|---|---|---|
| `project` | string | no | Limit the pass to one project slug. Omitted = all configured projects. |
| `apply` | bool | no | `true` actually removes worktrees; default (`false`/omitted) is always a dry run, regardless of the daemon's own background `worktrees.dryRun` config. |

**Response:** `202`: `{"started": true, "dryRun": true}`.

**Status codes:**
| Code | Condition |
|---|---|
| `503` | No worktree reaper wired for this daemon instance. |
| `400` | Body present but fails to decode (an empty body is tolerated). |
| `404` | `project` given but not a configured project slug. |
| `409` | A reap pass is already running (single in-flight pass). `tn worktrees` specifically handles this code by waiting and polling rather than failing. |
| `202` | Pass started. |

### `GET /worktrees`

Reads the latest reap report(s), and whether a pass is currently running.

**Query parameters:** `project` (optional, filter to one project's report; if given and no report exists yet, `reports` is `{}`).

**Response:** `200`:
```json
{
  "running": false,
  "lastRunAt": "2026-09-02T10:00:00Z",
  "reports": {
    "myapp": {
      "project": "myapp",
      "cwd": "/path/to/myapp",
      "at": "2026-09-02T10:00:00Z",
      "dryRun": false,
      "defaultBranch": "main",
      "scanned": 8, "removed": 3, "removable": 0, "pruned": 1, "locked": 0,
      "young": 2, "dirty": 1, "unpushed": 1, "skipped": 0, "errors": 0,
      "error": "", "durationSeconds": 12.4,
      "entries": [
        {"path": "...", "branch": "fix-signup-flow", "verdict": "removed", "merged": true, "staleLock": false, "ageHours": 6.0, "note": "merged into main", "seconds": 0.8}
      ]
    }
  }
}
```

**Status codes:** `200` always (missing report just yields `reports: {}`) · `503` no worktree reaper wired.

**Notes:** `running` is the field `tn worktrees` polls after starting a pass, every 2 seconds, until it flips to `false`. `verdict` is one of `removed`, `removable`, `pruned`, `locked`, `young`, `dirty`, `unpushed`, `error`, `skipped`, `outside` (not under `.claude/worktrees` at all, never considered).

---

## Projects and repos

### `GET /projects/env`

Lists configured per-project environment entries, never their values (see [configuration.md](configuration.md#env-entries)).

**Query parameters:** `project` (optional, omit for every configured project).

**Response:** `200`: `{"env": [{"project": "myapp", "name": "GITHUB_TOKEN", "description": "", "injected": true}, ...]}`. Always an array. `injected` is `true` iff the entry has a non-empty value (and would actually be injected into a spawn).

**Status codes:** `200` always: an unknown `project` value yields an empty `env` array rather than a 404.

### `GET /repos`

The daemon's resolved view of a project's repo-note settings plus its actual configured `cwd`/`autoSpawn`, the underlying call for `tn repo`.

**Query parameters:** `slug`: required.

**Response:** `200`:
```json
{
  "slug": "myapp",
  "repoPath": "/path/to/myapp",
  "autoSpawn": true,
  "maxOrchestrators": 1,
  "maxWorkers": 3,
  "maxOrchestratorsSet": false,
  "maxWorkersSet": true,
  "maxWorkersServeJsonFallback": ""
}
```
`maxOrchestratorsSet`/`maxWorkersSet` are `true` only if the repo note's frontmatter actually contained that key (even if the value was invalid and got dropped back to the default): this distinguishes "never configured" from "resolved value happens to equal the default." `maxWorkersServeJsonFallback` reports the project's `env.TN_MAX_WORKERS` value from `serve.json`, if any: what `maxWorkers` actually falls back to when the repo note doesn't set it.

**Status codes:** `400` empty `slug` · `404` `slug` doesn't match any configured project · `200` resolved.

---

## Observability

### `GET /health`

Minimal daemon liveness check.

**Request:** none.

**Response:** `200`:
```json
{ "ok": true, "agents": 3, "queued": 1, "uptime": 4213.7, "version": "0.6.5" }
```
`agents` is the total number of registered agent records (any state); `queued` is the count of messages currently in `queued` status across the whole daemon; `uptime` is seconds since the process started; `version` is `daemonVersion`.

### `GET /status`

The full daemon state snapshot: the source for the tray app, the embedded web UI, and any external status tooling. Spectator-safe: it never marks a message delivered and never bumps `lastSeenAt`, so polling it has no side effects on the systems it's reporting on.

**Request:** none.

**Response:** `200`, a `statusResponse`:

```json
{
  "daemon": {
    "uptime": 4213.7,
    "version": "0.6.5",
    "pid": 4821,
    "port": 8391,
    "spawnPaused": false,
    "lastSleepAt": null,
    "lastSleepSeconds": null,
    "vaultName": "MyVault",
    "buildHash": "a1b2c3d4"
  },
  "projects": {
    "myapp": {
      "agents": [
        {
          "name": "orchestrator-myapp-g3",
          "alive": true,
          "accepting": true,
          "draining": false,
          "stuck": false,
          "unackedBacklog": false,
          "rateLimited": false,
          "rateLimitResetsAt": "",
          "rateLimitSince": null,
          "contextPct": 12.5,
          "tmuxSession": "tn-myapp-g3",
          "lastSeenAt": "2026-09-02T10:05:00Z",
          "ownedInProgress": 2,
          "ownedTasks": [{"path": "tasks/fix-login-bug.md", "title": "Fix login bug", "status": "in-progress"}],
          "historicalOwned": 0,
          "workers": [{"path": "tasks/write-tests.md", "title": "Write tests", "status": "in-progress", "startedAt": "2026-09-02T10:01:00Z"}]
        }
      ],
      "queued": 0,
      "delivered": 1,
      "taskCounts": {"open": 3, "in-progress": 2, "review": 1, "done": 40},
      "needsAction": 1,
      "maxOrchestrators": 1,
      "maxWorkers": 3,
      "activeOwners": 0
    }
  },
  "needsActionTasks": [
    {
      "title": "Fix login bug",
      "path": "tasks/fix-login-bug.md",
      "status": "needs-input",
      "project": "myapp",
      "mr": "",
      "integrationBranch": "",
      "integrationRole": "",
      "jira": "",
      "jiraNc": "",
      "ask": "Which environment should this ship to first?",
      "brief": "",
      "mrState": ""
    }
  ],
  "stuckPrompts": [{"agent": "orchestrator-myapp-g3", "tmuxSession": "tn-myapp-g3", "excerpt": "Do you want to proceed? ..."}],
  "recentActivity": [{"agent": "orchestrator-myapp-g3", "project": "myapp", "at": "2026-09-02T10:04:00Z", "text": "started work on tasks/fix-login-bug.md"}],
  "triage": {"count": 0, "oldestTitle": "", "oldestPath": "", "oldestAgeSeconds": 0},
  "permissionDenials": [
    {"agentName": "orchestrator-myapp-g3", "toolName": "Bash", "commandSummary": "git push", "count": 4, "firstSeenAt": "...", "lastSeenAt": "...", "lastCommand": "git push --force origin main"}
  ],
  "credentials": {"autoSwap": true, "minSwapInterval": "10m0s", "active": "work", "profiles": [{"...": "..."}], "lastSwap": null},
  "worktrees": {"...": "see GET /worktrees' report shape, counts only"}
}
```

`projects` is keyed by project slug. `needsActionTasks` and `recentActivity` are each capped to the 20 most recent entries. `credentials` and `worktrees` are omitted entirely (not `null`) when no credential store / worktree reaper is wired into this daemon instance (for example, in a stripped-down test build).

Key fields worth calling out explicitly:

| Field | Meaning |
|---|---|
| `projects.<slug>.agents[].rateLimited` | The agent's pane is currently parked on a claude.ai usage-limit message. Reported *alongside*, never instead of, `unackedBacklog`; a backlog while rate-limited is expected, not alarming, but still real. |
| `projects.<slug>.agents[].stuck` | The agent's pane is showing a permission/approval prompt awaiting a human (or auto-approval) decision. |
| `projects.<slug>.agents[].unackedBacklog` | Boolean, not a count: whether this agent has any unacked (queued or delivered) messages. |
| `projects.<slug>.agents[].draining` | `accepting == false`: not taking new assignments, finishing existing work. |
| `projects.<slug>.agents[].ownedTasks` / `ownedInProgress` | Tasks this agent currently holds, derived from message history (latest acked "Task assigned:" message per path), not a live TaskNotes status check. Empty/zero once the agent is no longer alive; see `historicalOwned` below. |
| `projects.<slug>.agents[].historicalOwned` | Set only for a non-alive agent: the same ownership count, kept visible under a name that makes clear it's not current work (so a generation retired days ago doesn't read as "currently carrying" tasks it touched once). |
| `projects.<slug>.agents[].workers` | Worker subtasks this agent has declared via `POST /workers/start` and not yet ended. |
| `needsActionTasks[].mrState` | The MR watcher's last-observed GitLab MR state for this task's `mr` — `"opened"`/`"merged"`/`"closed"`, or `""` if the task has no `mr` set or hasn't been observed yet. The watcher polls every non-completed task, not just `review`, so a `needs-input`/`triage` task whose MR already merged shows `mrState: "merged"` here (and gets a note on the task) instead of being silently ignored — it's never auto-closed, since the status may be parked for a reason unrelated to the MR. |

**Notes:** Spectator-safe by construction: building this response never marks a message delivered and never bumps any agent's `lastSeenAt`. Agent names under the internal test-verification prefix (`zz-`) are excluded from every project's agent list here, even though they remain in the underlying registry, so ad hoc verification agents never appear as real generations in the UI. `stuckPrompts` is not capped: it lists every agent currently tracked as stuck, across all projects, however many that is. `triage` is a deliberately passive signal (count and the single longest-waiting task), not an alert, added after triage tasks sat unpicked-up for hours while nothing surfaced that fact to a human; it does not decide anything is wrong, only how long the oldest one has been waiting. `permissionDenials` is the same kind of passive telemetry, not a decision.

### `GET /events`

Server-Sent Events stream of daemon state-change notifications.

**Request:** none.

**Response:** `200`, `Content-Type: text/event-stream`. On every daemon mutation (registration, message, spawn, and so on, anything that also triggers a dashboard/webui re-render), every connected subscriber receives:
```
data: {"type":"status"}

```
The event carries no data beyond its type: the client's whole job on receiving one is to re-fetch `GET /status` in full; there's no delta to apply, so several mutations coalescing into one client refresh is harmless. Every **30 seconds** of inactivity, a comment-only heartbeat frame is sent instead, to keep proxies and sleeping laptops from silently dropping an idle connection:
```
: heartbeat

```
The connection stays open until the client disconnects (request context cancellation).

**Notes:** Used by the embedded web UI to update live without polling `/status` on a timer. If two mutations happen close together and a subscriber hasn't consumed the first notification yet, the second is dropped rather than queued (never a problem in practice since the client always re-fetches the full, current `/status` rather than trying to apply anything incrementally).

### `GET /ui/`

Serves the embedded web UI (a built React SPA, embedded into the binary via `go:embed`).

**Request:** none: this is a static file server rooted at `/ui/`.

**Response:** `200` with the SPA's HTML/JS/CSS on a successful build. If `webui/dist/` exists but wasn't actually produced by a real Vite build (no `assets/` directory), this serves an explanatory "run `pnpm build`" page instead of attempting to serve a broken bundle. See [../how-to/web-ui-development.md](../how-to/web-ui-development.md) and the build-staleness caveat in the project's own build documentation: `GET /status`'s `daemon.buildHash` field is the fast way to confirm which bundle is actually embedded in a running binary.

---

## Webhooks

### `POST /webhooks/tasknotes`

Receives webhook deliveries from the TaskNotes Obsidian plugin. On startup, the daemon registers this URL with TaskNotes itself (retrying every 60 seconds until it succeeds), subscribed to exactly three events:

- `task.created`
- `task.updated`
- `task.completed`

`task.completed` is **not** a special case of `task.updated`; TaskNotes fires them as mutually exclusive alternatives: a status transition into a completed status fires only `task.completed`, never `task.updated`. Without subscribing to it explicitly, every transition into "done" would be invisible to this webhook path, deterministically, 100% of the time for that one direction, not a flaky drop. If the daemon finds an existing registration for its own URL with an incomplete event list (for example, one made by an older daemon version before `task.completed` was added here), it deletes and re-creates it, since TaskNotes has no update-in-place endpoint for webhook subscriptions.

**Request:** TaskNotes' own webhook payload: `{"event": "task.created" | "task.updated" | "task.completed" | ..., "data": {...task fields...}}`. Only these three event names are acted on; any other event type (if TaskNotes ever sends one to this URL) is silently ignored.

**Response:** always `200 OK` with an empty body, including when the body fails to read or fails to parse as JSON: this endpoint never reports a failure back to TaskNotes' webhook delivery mechanism.

**Notes:** This is the daemon's only inbound signal from TaskNotes itself (as opposed to what `tn` commands push via the client in `internal/tn/api.go`). It's how the daemon notices: a new or updated task tagged `claude`/`spawn` that should be assigned to an orchestrator, an `@claude:` reply line added to a task (whether via `POST /reply` or a human editing the task directly in Obsidian), and status changes generally, so it can trigger routing decisions and dashboard/webui re-renders. Task assignment only fires once per "open episode" for a given task path: a webhook double-fire or a later reconciler pass re-observing the same still-open task does not re-assign or re-queue it.
