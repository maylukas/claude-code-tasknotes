# Spec: `tn serve` bridge daemon

Extend this Go CLI (stdlib only, Go 1.26) with a local bridge daemon plus agent/messaging subcommands. Read api.go, main.go, main_test.go first and match style.

## Purpose

Coordination layer between the Obsidian TaskNotes plugin (HTTP API on :8390) and Claude Code agent sessions working on the user's own machine. The daemon keeps a registry of agent sessions per project, a message queue consumed via inbox long-polling, and reacts to TaskNotes webhooks by routing task assignments and messages to the right agent session. If no live orchestrator session exists for a project, it can start one in a detached tmux session (user-configured opt-in per project).

## New files

- `serve.go` — daemon: HTTP server, persistent state, webhook handling, orchestrator startup
- `bridge.go` — client helpers for new subcommands
- `serve_test.go`

Keep existing commands working; extend usage text.

## Daemon: `tn serve [--port 8391]`

- Listen on 127.0.0.1:<port>; default 8391, env `TN_BRIDGE_PORT` overrides default.
- State at `~/.local/state/tn/state.json` (create dirs; atomic write tmp+rename; sync.Mutex; load on start).
- Optional config `~/.config/tn/serve.json`:
  `{"port":8391,"projects":{"<slug>":{"autoSpawn":true,"cwd":"/path/to/repo"}}}` — missing file = empty config; autoSpawn defaults false.

### State model

- `agents`: map[name]{name, project, sessionId?, registeredAt, lastSeenAt, deregisteredAt?, accepting, tmuxSession?, contextPct?, contextPctAt?}
  — see SPEC-generations.md: `accepting` resets true on every register, false via `tn
  drain` or daemon auto-drain; `tmuxSession` is the generation's reported tmux session
  (falls back to `tn-<slug>` derivation for legacy agents that never reported one);
  `contextPct` is the last-reported context-window usage %, fed by the statusline
  wrapper via `POST /agents/context` — nil until reported.
- `messages`: []{id, to, project, taskPath?, text, from?, status: queued|delivered|acked, createdAt, deliveredAt?, ackedAt?, response?}
- `taskDetailsHash`: map[taskPath]sha256hex (whole-details short-circuit)
- `routedClaudeLines`: map[taskPath][]sha256hex (line-level @claude: dedup, see webhook handling)
- `mrStates`: map[taskPath]string, last-observed GitLab MR state ("opened"/"merged"/"closed") — see MR watcher below.
- Alive = lastSeenAt within 90s.

### HTTP API (JSON, localhost only, no auth)

- `POST /agents/register` {name, project, sessionId?, tmuxSession?} → {agent}; same name
  re-registers/revives, and every register (fresh or repeat) sets `accepting: true`.
  Also re-targets anything still queued to the logical name `orchestrator-<slug>` onto
  this agent (see SPEC-generations.md).
- `POST /agents/drain` {name} → sets `accepting: false`, idempotent.
- `POST /agents/context` {name, usedPct} → stores `contextPct`/`contextPctAt`; crossing
  `drainContextPct` (40) while accepting auto-drains the agent once and queues it an
  informational message telling it to `tn retire` once done (see SPEC-generations.md).
- `POST /agents/retire` {name} → 404 unknown agent; 409 {error:"pending messages"} ONLY if
  the agent is still `accepting` (not draining) AND has an unacked queued/delivered
  message — a live agent retiring with unread mail is a mistake worth blocking. A DRAINING
  agent's pending messages are instead REASSIGNED (same id/taskPath/from/text, status reset
  to `queued`, `deliveredAt` cleared — a re-delivery, not a new message) to the project's
  current accepting agent, or the logical `orchestrator-<slug>` queue name if none — logged
  as `serve: retiring <name>, reassigned N unacked message(s) to <target>` — and retirement
  proceeds (200). Fixes a real deadlock: a drained generation that correctly stops acting on
  stale FYIs could never retire under the old any-pending→409 rule, and those messages were
  addressed to its own concrete agent name, which register's logical-queue re-targeting
  never reaches. Deliberately does NOT check whether a reassigned message's task is already
  done/closed first (would need a TaskNotes API call per message inside a handler that holds
  `s.mu` for one synchronous mutation) — everything is reassigned, the receiving agent
  triages. Once deregistered, ~2s later the agent's tmux session is killed (injectable,
  tests never touch tmux) and the retirement is logged/recorded — daemon-mediated
  self-termination, replacing the old deregister+`tmux kill-session` pair an orchestrator
  used to run on itself.
- `POST /agents/deregister` {name}
- `GET  /agents` → {agents:[…, alive bool, accepting bool, tmuxSession?, contextPct?]}
- `GET  /inbox?name=N&wait=S` — long-poll: queued messages for N → mark delivered, return {messages}; else block up to S seconds (default 25, max 55) for new ones; may return empty list. Every call bumps lastSeenAt (poll = heartbeat). Use sync.Cond or broadcast channel, no busy loop.
- `POST /ack` {name, msgId, response?} → status acked, store response.
- `POST /messages` {to?, project?, text, taskPath?, from?} — resolution: `to` → that agent;
  else `taskPath` set and it already has a live owner (latest acked "Task assigned:" To for
  that path) → the owner, even if draining; else `project` → alive ACCEPTING agent
  registered for it; none → start orchestrator (below, only if autoSpawn) and queue to the
  logical name `orchestrator-<project>`; no autoSpawn → 202 {queued:true, warning:"no live
  agent"} queued the same way. See SPEC-generations.md for the full routing model.
- `GET  /messages?id=X` or `?to=N&status=S`
- `POST /webhooks/tasknotes` — see below.
- `GET  /health` → {ok, agents, queued, uptime, version}
- `GET  /status` → one aggregate JSON for the tray-app foundation (see the
  "Status endpoint" section below) — SPECTATOR-SAFE, never marks a message
  delivered or bumps LastSeenAt.
- `GET  /ui` → self-contained HTML status page, no server-side per-request
  data; polls `/status` client-side every 5s.
- `POST /spawn/pause` {paused} / `GET /spawn/pause` → menu-bar manual spawn
  kill-switch, `state.SpawnPaused` (persisted). While true, `spawnOrchestrator`
  and `reconcileSpawnsOnce`'s batch check both skip actually spawning and log;
  routing/drain/retire/MR-watcher/stuck-dialogs are unaffected. Surfaced as
  `daemon.spawnPaused` in `/status` and a banner in `/ui`. CLI: `tn pause` /
  `tn resume`.
- `POST /agents/approve` {name, option: "once"|"always"|"deny"} → server-side
  equivalent of clicking the stuck-prompt dialog, reusing
  `sendApprovalIfStillStuck`/`resolveStuckPromptMapping` (now returning
  `(sent bool, reason string)` so both the dialog callback and this endpoint
  share one implementation). 404 unknown agent, 400 bad option, 409 if the
  agent has no currently tracked stuck prompt (checked before touching tmux);
  200 `{sent, reason}` otherwise — `sent:false` with a reason is a normal
  response (prompt cleared, or the requested option no longer offered), not
  an error.

### Orchestrator startup

See SPEC-generations.md for the full generation model (naming, drain, routing). Summary:

- Each spawn is a new generation: agent name `orchestrator-<P>-g<gen>`, tmux session
  `tn-<P>-g<gen>` (`gen := time.Now().Unix() mod 100000`).
- If `tmux has-session -t <session>` fails: `tmux new-session -d -s <session> -c <cwd or
  $HOME>` then send the claude CLI launch line with an initial prompt into that session.
  claude binary: `$HOME/.local/bin/claude` if present, else `claude`. If the session
  already exists AND no agent is alive for it, its panes are checked for a live claude
  process before treating it as stale-and-killable (see the stale-tmux-respawn note in
  SPEC-generations.md) — never killed while a claude process is still running.
- Initial prompt: see ORCHESTRATOR.md (source of truth for the contract) — must carry the
  generation's agent name and tmux session literally so the session registers as exactly
  what the daemon is expecting.
- Only when projects[P].autoSpawn true, and only when no alive ACCEPTING agent exists for
  the project (a draining-only generation still triggers a fresh spawn). Log each start.
- Implement behind an injectable `spawnFunc func(project, cwd string, env map[string]string) error` field so tests never touch tmux.

### Parallelism (multiple orchestrators)

Per-repo config read live from `<vaultDir>/Repos/<slug>.md` frontmatter (`repoSettingsFor`,
`repoSettingsCacheTTL` = 60s, `vaultDir` = `filepath.Dir(dashboardPath)`; no dashboard
configured → `defaultRepoSettings()`): `max-orchestrators` (int, default 1) and
`max-workers` (int, default 0/unset). Parsing (`parseRepoSettings`) is deliberately not a
real YAML parser — just `key: value` lines between `---` fences, tolerant of a missing
file, missing keys, or junk/non-positive values (all fall back to defaults).

- `max-workers`, when set, is injected as `TN_MAX_WORKERS` in `spawnOrchestrator`'s env,
  overriding a serve.json `env` entry of the same name (repo-note settings are fetched
  and applied AFTER `injectedEnv`, so the map write wins).
- `reconcileSpawnsOnce` spawns a new generation for an autoSpawn project when EITHER: no
  alive accepting agent exists AND there's routable work (a queued message to
  `orchestrator-<slug>` OR a startable open claude-tagged task — the latter closes a live
  deadlock: a draining orchestrator plus an idle-but-nonempty open queue no longer waits
  for an external event); OR the accepting count is below `max-orchestrators` AND the
  startable-task count exceeds `acceptingCount * effectiveMaxWorkers` (unset falls back to
  `defaultMaxWorkers` = 3, matching ORCHESTRATOR.md's documented `$TN_MAX_WORKERS`
  default) — scale out. Startable-task counts come from ONE global query
  (`buildStartableOpenTasksQuery` — open, non-archived, claude-tagged, not blocked),
  bucketed per project client-side via `routingSlugForTask` (TaskNotes has no queryable
  routing-slug property), same pattern as the due-task scanner/MR watcher. A query failure
  or nil `tnClient` degrades both new triggers to "no startable work known", falling back
  to the original queued-message-only behavior rather than blocking the pass.
- NEW-assignment routing (`resolveTargetLocked`'s accepting-agent fallback — task-scoped
  owner routing is unaffected) picks the accepting agent owning the FEWEST tasks
  (`pickAcceptingAgentForAssignmentLocked`, `ownedTaskPathsLocked`-derived count — a cheap
  in-memory count under `s.mu`, not a live TaskNotes query), tie-broken lexicographically.
  With `max-orchestrators == 1` there's only ever one candidate, so this is unobservable
  from the default single-generation behavior.
- **Spawn-decision race fix.** Two independent paths can decide to spawn: `resolveTargetLocked`
  (on-demand, from a message routed with no accepting agent) and `reconcileSpawnsOnce`
  (periodic). Both used to count only LIVE accepting agents, so if both observed zero
  accepting agents within the same short window (a real spawn is slow — tmux + claude
  startup + registration, seconds wide) they'd both decide to spawn, exceeding
  `max-orchestrators`. Fixed via `state.PendingSpawns map[string]time.Time`
  (project slug → decided-at) and a shared helper, `effectiveAcceptingCountLocked(slug)`:
  live accepting count + 1 if a recent (`spawnIntentTTL` = 3min) pending intent exists for
  that slug — BOTH paths call this same helper instead of counting independently. The
  intent is recorded (`recordSpawnIntentLocked`) at DECISION time, under `s.mu`, never at
  the (unlocked, slow) moment the actual spawn runs — recording later would reopen the same
  window. Cleared (`clearSpawnIntentLocked`) the moment an agent registers for that project.
  Keyed by slug alone (one timestamp, not a counter), so it represents at most "1 pending" —
  correct for the default `max-orchestrators == 1` case this was fixing; a project with a
  higher cap racing TWO simultaneous decisions for the same slug within the TTL window could
  still slip a third spawn through. Accepted tradeoff, not fixed further (not something hit
  in practice, and the simple version is what's actually running).

## TaskNotes webhook handling

`POST /webhooks/tasknotes`: parse {event, data} leniently (task at data.task or data; fields path, tags, projects, details, title, status). For task.created / task.updated:

Routing slug resolution order: TaskNotes `projects` values are wikilinks to project notes (e.g. `[[Myapp/Subprojects/E2E Tests]]`), not plain slugs, so the routing slug is resolved as (1) the task's `claude-project` custom property if non-empty (whether TaskNotes puts it directly on the task or nested under `customProperties`, which itself may live at `data.task.customProperties` or `data.customProperties`), else (2) the first `projects[]` entry — both normalized the same way: strip `[[` `]]`, strip any path prefix (keep only the text after the last `/`), strip a trailing `.md`, lowercase. serve.json `projects` config keys are also matched case-insensitively at lookup, and orchestrator/tmux naming (`orchestrator-<slug>-g<gen>` / `tn-<slug>-g<gen>`, see SPEC-generations.md) always uses the lowercased slug, so casing differences between how a task and the config spell a project never cause a mismatch.

1. Tag `spawn` or `claude` present → route message {project: resolved routing slug, taskPath, text: "Task assigned: <title> (<path>)"} via the same resolution as POST /messages. Deduped: skipped if a not-yet-acked "Task assigned:" message already exists for the same taskPath (TaskNotes fires task.created and task.updated for one edit). Also skipped when the task's status is triage / needs-input / review (human-attention statuses — see humanAttentionStatuses): those are parked for the user, and moving the task to `open` is their approval, which fires task.updated and routes it normally then.
2. Line-level dedup: extract lines matching `(?m)^(?:>[ \t]*)*@claude\b(?:[ \t]*[:,\-—][ \t]*|[ \t]+)(.+)$` — an optional repeated Obsidian blockquote prefix (`>`, possibly nested), then EITHER an explicit separator (colon/comma/dash/em-dash, optionally padded) OR required bare whitespace with no punctuation; the separator/whitespace alternation is deliberate, not "both optional" (see below) — from `extractClaudeLines`, which FIRST strips the history block out of the details string entirely (via `extractBlock(details, "history")`) before matching, then routes only the ones not already in `routedClaudeLines[path]` (sha256 hex of each routed line's trimmed text), then stores the full current set — so a line no longer present drops out and can be asked again if re-added. `taskDetailsHash[path]` (sha256 of the whole details blob) is kept as a cheap short-circuit: unchanged details + an already-migrated task (has a `routedClaudeLines` entry) skips extraction entirely. Every `tn note` append changes the details hash, so hash-level "route all lines on any change" would re-deliver every previously-routed line on every unrelated edit — line-level dedup replaced that. Migration: a task with a `taskDetailsHash` entry but no `routedClaudeLines` entry (predates this scheme) has its current lines seeded as already-routed without dispatching, so upgrading doesn't fire every pre-existing line at once. Unlike assignment routing, @claude-line routing stays active in every status — users comment on parked tasks (e.g. answering a needs-input question) without reopening them.

   **Two regressions found and fixed after the reply-block feature shipped (SPEC-note-layout.md addendum), both live incidents:**
   - The quote-prefix support is REQUIRED (not cosmetic): the reply block instructs the user to write `@claude: ...` inside a callout, where `wrapCallout` `> `-prefixes every line — without it, a reply typed exactly where instructed never routes.
   - The separator MUST be a mandatory alternation, never optional-on-its-own: with `[:,\-—]?` (optional), a line that's exactly `@claude:` with NOTHING after it — the reply block's own unfilled placeholder, see `defaultReplyBody` — can satisfy the capture group's minimum-one-char requirement by declining to consume the colon and capturing the colon itself as "content," so the empty placeholder spuriously self-routes the instant `tn ask` writes it, before any user reply.
   - The history-block exclusion is REQUIRED (not cosmetic either, found live AFTER the above two fixes shipped): `wrapCallout` applies the identical `> ` prefix to the history block as to the reply block, so without excluding history, a user's original `@claude: ...` line — once swept into a permanent history entry, which happens naturally over a task's lifecycle — would match FOREVER on every future observation regardless of `routedClaudeLines` dedup succeeding elsewhere. Verified live: the same already-actioned reply text was re-dispatched as a "new" message four times over roughly 30 minutes on a real task, each one re-triggering the needs-input/review auto-flip and silently reverting its status — the same board-unreliability symptom the reconciler fix (above) addressed, via a completely different root cause. `extractClaudeLines` now strips the history block before matching, so only the live portion of the body (ask, reply, links, brief, description) is ever routable.

   **A third, more far-reaching regression, found live after the above two**: `diffClaudeLinesLocked` trusted every `details` value at face value, including observations that carry NO real details at all — POST `/api/tasks/query` never returns `details` (confirmed against the live API), and a webhook payload for a partial/status-only update (e.g. `tn status`/`tn done`, which PUT only the changed field) can likewise omit the `details` key entirely from its JSON. `json.Unmarshal` alone can't distinguish "field absent" from "field present and empty" — both leave the Go string `""`. Treating `details == ""` as "content genuinely changed to nothing" REPLACED `routedClaudeLines[path]` with an empty set, wiping dedup memory; the next confirmed-real observation of the SAME unchanged `@claude:` line then saw "changed, nothing previously routed" and re-dispatched it, re-triggering the auto-flip. Confirmed live across 20+ tasks (`taskDetailsHash[path] == sha256("")`), redispatching at whatever cadence real vs. details-omitting observations happened to alternate (the reconciler's real `GetTask` fetches vs. webhooks from ordinary status-only updates elsewhere in the vault — NOT the due-task scanner, which never calls this routing path at all despite an earlier, incorrect attribution).

   Fix: `webhookTask` gained `DetailsKnown bool` (`json:"-"`, never settable from a payload directly) — true only for a confirmed-current, complete read of the body: `taskToWebhookTask` (the reconciler's `GetTask`-sourced conversion, its one caller) always sets it true; `extractWebhookTask` sets it via a new `hasJSONKey(raw, "details")` helper (parses into `map[string]json.RawMessage` and checks key presence, not value emptiness) against whichever JSON object actually became the task. `diffClaudeLinesLocked` gained a `detailsKnown bool` parameter and an early-exit guard: `details == "" && !detailsKnown && (hashKnown || hasRoutedSet)` — skip entirely, touching NEITHER ledger, when an unconfirmed-empty observation would otherwise wipe state we already trust. A task's first-ever observation happening to be unconfirmed-empty is still safe to fall through (nothing to wipe yet); a CONFIRMED-empty observation (`detailsKnown=true`, e.g. a real fetch showing the description is now genuinely blank) still updates state normally — the guard only protects against UNCONFIRMED emptiness, never overrides a real one. The guard sits before the migration-seed branch too, so a stored hash that's itself a product of the poisoning (`sha256("")`) can't "legitimately" re-seed an empty routed set from another unconfirmed-empty observation.

Respond 200 fast; routing synchronous, but never block on orchestrator startup output.

## Webhook self-registration

On start and every 60s until success: using the existing TaskNotes client/config, GET /api/webhooks; if our URL `http://localhost:<port>/webhooks/tasknotes` absent, POST /api/webhooks {url, events:["task.created","task.updated"]}. TaskNotes API down = fine: log once, keep retrying quietly, never crash.

## Due-task scanner

TaskNotes recurrence/scheduled-date rollover never touches the note file, so webhooks don't fire when an occurrence comes due. `tn serve` queries the TaskNotes API every 10 minutes (plus once ~30s after startup) for open, `claude`-tagged tasks scheduled on or before today and NOT in a human-attention status (triage / needs-input / review — same approval semantics as webhook routing: the user must move the task to open first), resolves each one's routing slug the same way webhook routing does, and routes an assignment — deduped per calendar day via `assignedOccurrences` (taskPath → last-assigned date) in state.json, and skipped if a "Task assigned:" message is already pending for that path. Unroutable tasks (no slug, or a slug with neither a live agent nor autoSpawn configured) are skipped rather than queued. TaskNotes API down = log once, retry next tick, never crash. Disabled via `TN_NO_SCANNER=1`.

## Webhook-miss reconciler

TaskNotes doesn't retry webhook deliveries, so an event fired while the daemon was unavailable (e.g. mid self-restart) is lost forever otherwise. On the same 10-minute cadence as the due-task scanner (same `TN_NO_SCANNER=1` guard), `tn serve` queries for tasks modified since a watermark (`state.lastReconciledAt`, minus a 15-minute buffer; falls back to 24h before now on fresh state) and re-runs each one through the exact same routing pipeline the webhook handler uses (`routeTaskNotification`) — the assignment branch (with its human-attention status guard) and the `@claude:` line diff both apply identically. The query never includes task details (confirmed against the live API), so each candidate's details are fetched individually via `GET /api/tasks/:id`, capped at `webhookMissTaskCap` (200, raised from 50 — see below) tasks per pass (logged if capped). The watermark only advances to the pass's start time when the query succeeded AND every candidate's detail fetch succeeded; any failure leaves it unchanged so the next pass retries the same window — this also means the first pass after a long outage automatically covers the whole gap. Reprocessing is safe: `routeTaskNotification`'s own assignment dedup and line-level `@claude:` dedup make it a no-op for anything already routed.

`webhookMissTaskCap` was raised from 50 to 200 after chronic (not one-off) overflow: 71-78 candidates observed per pass, repeatedly. With ascending sort, a truncated pass always drops the newest candidates permanently for that day — `buildRecentlyModifiedQuery`'s `sinceDate` is DAY-granularity only (a bare `"YYYY-MM-DD"`, not a timestamp), so the query returns the identical full same-day candidate set on every pass regardless of how far a watermark has crept within that day; a "resume where the last pass left off" rotation therefore can't shrink the query results intra-day without either true API pagination (the `FilterGroup` shape has no offset/limit field) or a separately-persisted client-side slice cursor — meaningfully more state and edge-case surface than the problem warrants. Raising the cap was chosen instead: TaskNotes returns the full candidate list in ONE query response regardless of cap (no pagination cost either way), and each extra `GetTask` is cheap. 200 gives ~2.5x headroom over the worst observed volume; the existing truncation log line is the "pass still incomplete" signal if real volume ever exceeds even that.

`buildRecentlyModifiedQuery` sorts `dateModified` ASCENDING (oldest-modified first), not the more obvious descending — deliberately, to prevent starvation under a cap-truncated pass. A real burst produced 71 candidates against the 50 cap in one pass; with descending sort the dropped tail is always the OLDEST candidates, and since the query window only ever advances forward as the watermark moves, a candidate dropped that way has no guarantee of surviving into a later pass before it ages out of `dateModified is-on-or-after sinceDate` entirely — a task whose webhook delivery was genuinely missed could go unprocessed forever, silently. With ascending sort the dropped tail is the NEWEST candidates instead, which have more runway before the watermark catches up and reliably get processed on a later pass — truncation under sustained volume delays freshly-modified tasks by a pass or two (tolerable; the live webhook already routes them promptly) rather than silently starving old ones. Chosen over raising the cap because a higher cap only raises the volume threshold at which the same starvation would recur; ascending sort removes the failure mode regardless of volume.

## Task dependencies

`--blocked-by PATH-OR-[[WIKILINK]]` is repeatable on `tn create`/`tn update` (replaces the whole array on update, like `--tag`) → body `blockedBy: [{"uid": <value verbatim>, "reltype": "FINISHTOSTART"}, ...]`. `tn list --startable` adds `dependencies.isBlocked is-not-checked`.

Both query properties verified live against the real API: `dependencies.isBlocked is-not-checked` and `blockedBy is-not-empty`/`is-empty` correctly discriminate (confirmed by creating a real blocker/blocked task pair and checking each query's filtered count before/after). The plain top-level `isBlocked` property is broken — a query for `isBlocked is-not-checked` matched 0/29 real tasks even though every one of them has `isBlocked: false` — so it's never used.

`webhookTask` gained `BlockedBy []TaskDependency` and `IsBlocked *bool` (nil = no evidence either way, e.g. a bare webhook payload; non-nil = a live fetch's confirmed value — `taskToWebhookTask` always sets it, since `GetTask`/`QueryTasksRaw` always return a real `isBlocked`). Assignment routing in `routeTaskNotification` additionally skips when `isBlockedByUnresolvedDependency(t)`: `len(BlockedBy) > 0 && (IsBlocked == nil || *IsBlocked)` — pragmatically conservative when there's no fetched evidence. `@claude:` line routing is unaffected. `buildDueTasksQuery` gained the same `dependencies.isBlocked is-not-checked` condition.

Unblock pass: when `routeTaskNotification` sees `t.Status == "done"` (webhook or reconciler path — both funnel through the same function), `runUnblockPass` queries `buildUnblockedDependentsQuery()` (open, non-archived, claude-tagged, `blockedBy is-not-empty`, `dependencies.isBlocked is-not-checked`) and routes an assignment for each result via `routeUnblockedDependent`, text suffixed `[unblocked by <completed title>]`, skipping a candidate that's in a human-attention status, already `in-progress`, or already has a pending "Task assigned:" message (`hasPendingAssignmentLocked` — reused as-is, since the suffix doesn't change the prefix). Capped at `unblockPassTaskCap` (50), logged if capped; a query failure logs and returns, no retry (the due-task scanner and webhook-miss reconciler both independently re-cover the same tasks on their own cadence).

## MR watcher

Every `mrWatchInterval` (5 minutes, plus once ~45s after startup, `TN_NO_MRWATCH=1`
disables), `tn serve` queries non-archived tasks with status `review`/`in-progress`/`open`
(`buildMRWatchQuery`, same status-OR-group shape as `buildNeedsActionQuery` since
`FilterQuery` has no "in" operator) and, client-side, filters to those with a non-empty
`customProperties.mr`. For each, `mrStateFunc` (real impl `glabMRState`, injectable so
tests never exec `glab`) parses the URL as `https://<host>/<group/.../project>/-/merge_requests/<iid>`
(`parseMRURL` — nested GitLab groups preserved whole) and shells out to
`glab mr view <iid> -R <group/.../project> -F json` (binary resolved by `resolveGlabBin`) (env `HOME` set
explicitly, same launchd-environment gotcha as tmux's absolute-path resolution) to read
`.state` (`opened`/`merged`/`closed`).

State is tracked per task path in `state.MRStates` (persisted) and acted on only when it
CHANGES from the last-recorded value (`checkTaskMRState`) — a task's first-ever
observation just seeds the map without transitioning, except when it's already `merged`
while the task is still `review` (a legitimate catch-up: the user merged before the
watcher ever saw it). On an observed transition:

- **merged**: a `review` or `in-progress` task only — `bridgeTransitionTask` fetches the
  task, appends a `bridge`-attributed note (same format as `tn note`'s `AddNote`), and
  PUTs both the new details and `status: "done"` in one call; then `runUnblockPass` is
  invoked directly (we already know the status change succeeded, no need to wait for a
  webhook/reconciler pass to notice it independently). An `open` task whose MR happens to
  be merged is left alone — its state is still recorded, nothing to transition.
- **closed** (without merge): a `review` task only reopens to `in-progress` with a note
  (the note literally says "reopened", which would mislead for a task that was never in
  review) — regardless of whether it reopened, the task's owner (via `dispatchMessage`'s
  existing owner-first resolution — `taskOwnerLocked`, fallback alive accepting agent,
  fallback logical queue) gets an informational message to investigate.
- **opened**: recorded only, no action.

A failed lookup (unparseable URL or a `glab` error) never mutates `MRStates` — a later
successful pass just resumes from the last confirmed state — and is logged once per task
path per outage (`mrWatchWarnedOnce`, cleared on the next success). The dashboard's
Needs-your-action review rows append the known state after the MR link
(`— [MR](url) (opened)`) via `dashboardSnapshot.MRStates`, sourced from
`snapshotLocked()`'s copy of `state.MRStates`.

## Integration branch

A workstream's target branch (e.g. a consolidated MR that per-testcase fixes should stack
onto, instead of a scatter of MRs opened against the default branch) needs to survive
orchestrator retirement — a fresh generation must be able to learn it without reading the
board. `integrationBranchFor(t Task)` / `integrationBranchForWebhookTask(t webhookTask)`
resolve it as the task's OWN `customProperties["integration-branch"]`, or `""` —
**deliberately no repo-note or other fallback source anywhere in the resolution path**.
An earlier draft of this spec added a `Repos/<slug>.md` frontmatter fallback (mirroring
`max-orchestrators`/`max-workers`); that was wrong and was removed before shipping — the
user's model is that `development` is the default target for everything, and only tasks
descended from a specific parent (e.g. an e2e workstream task) should target something
else. A repo-wide default would have wrongly captured every unrelated task in the repo.

`tn create --title T --from <parent-task-path>` copies `integration-branch` AND
`claude-project` from the parent task's own `customProperties` onto the new task —
explicit `--claude-project`/`--integration-branch` flags on the same `create` call still
win over whatever `--from` would have inherited. A missing/unreadable parent (`GetTask`
fails) is a hard error: no task is created. This is the actual inheritance mechanism —
mechanical and explicit, never inferred from "From [[X]]"-style text in a task's details.

When a resolved branch is non-empty, every "Task assigned:" message
(`appendIntegrationBranchSuffix`, applied in `routeTaskNotification` — covering both the
webhook and reconciler entry points — plus `routeUnblockedDependent` and
`processDueTask`) gets a suffix appended. `tn get` prints an `integration-branch: <b>`
line when set; `/status`'s `needsActionTasks[]` gains an `integrationBranch` field
(task-level only — no per-project equivalent, since there's no project-level source);
`/ui`'s needs-action list and the dashboard note's Needs-your-action section both show
it per task; the SwiftBar tray shows it as a line under each needs-your-action task row.

### Owner vs member (`integration-role`)

A workstream distinguishes who stacks changes onto the integration branch from who
merges it out. `integrationRoleFor(t Task)` / `integrationRoleForWebhookTask(t
webhookTask)` resolve `customProperties["integration-role"]` — `"owner"` or `""`
(unset = member) — task-level only, same no-fallback rule as `integrationBranchFor`.
The OWNER holds the single consolidated MR from the integration branch into the
default branch and must be `blockedBy` every member task; a member's own MR targets
the integration branch itself. `tn create --title T --integration-role owner` sets it;
`tn create --from` copies `integration-branch`/`claude-project` but **deliberately
never** `integration-role` — a child task must never silently inherit ownership of its
parent's consolidated MR (explicit test: `TestCmdCreate_FromDoesNotCopyIntegrationRole`).

The assignment-message suffix is role-aware (`appendIntegrationBranchSuffix(text,
branch, role)`): a member gets the plain `[integration branch: <b>]` suffix, the owner
gets `[integration OWNER of <b> — consolidated MR to development]` instead, so a fresh
generation (the orchestrator that assigned it may have retired) knows immediately
whether to stack a change onto the branch or finalize it. In `routeUnblockedDependent`
specifically — the one place the "last member just completed" moment is actually known
— an owner's assignment text additionally gets an `"All members complete — "` prefix.
No new blocking logic was written for owner tasks: the EXISTING dependency machinery
(`isBlockedByUnresolvedDependency` holds an owner back while any member is
incomplete; `buildUnblockedDependentsQuery`/`runUnblockPass` assigns it once, exactly
like any other now-unblocked dependent, when the last one finishes) already gives this
for free — `TestOwnerWorkflow_BlockedWhileMembersIncomplete_ThenAssignedOnce` asserts
the full blocked-then-assigned-once cycle end-to-end.

`tn get` prints an `integration-role: <r>` line when set. `/status`'s
`needsActionTasks[]` gains `integrationRole`; `statusProject` gains `activeOwners` — a
best-effort rollup counting that project's `needsActionTasks` with role owner (NOT an
exhaustive count of every owner task in the project: an owner still blocked on its
members, not yet in a needs-action status, isn't counted — see the field's doc comment
in serve.go). `/ui`'s project header shows "N workstream(s) awaiting merge" when
non-zero, and an owner's needs-action row shows "OWNER of `<branch>`" instead of the
plain branch code; the dashboard note's Needs-your-action lines show "**OWNER** of
`<branch>`" the same way; the SwiftBar tray shows a "★ OWNER of <branch>" line per
owner task and a "★ N workstream(s) awaiting merge" line under each project row.

## Jira transitions

**Constraint that shapes the whole design: the daemon cannot call Jira.** `jira` is an
HTTP MCP server (`https://jira-mcp.example.com/mcp`) with no local
credentials, and there's no `jira`/`acli` CLI installed on this machine — only AGENTS
have the MCP tools. So the daemon is the TRIGGER and an agent is the EXECUTOR: `tn
serve` never makes an HTTP call to Jira itself, it only emits a bridge message asking an
agent to do so.

Two customProperties fields (already registered in the TaskNotes plugin): `jira`
(comma-separated issue keys, e.g. `PROJ-101,PROJ-102` — Bug/Story, normal transition
policy) and `jira-nc` (a single nonconformity issue key — a REGULATED RECORD that is
**never** auto-transitioned, comment-only; your nonconformity procedure governs its closure). `--jira
K[,K2]`/`--jira-nc K` on `tn create`/`tn update` set them, merged into the same
customProperties helper as `--mr`/`--integration-branch`/`--integration-role`; shown in
`tn get`, `/status`'s `needsActionTasks[].jira`/`.jiraNc`, `/ui`, and the tray.

`checkJiraTransition(t webhookTask)` runs on EVERY task observation — called from the
very end of `routeTaskNotification`, the same shared choke point covering both the
webhook handler and the reconciler. It tracks `state.LastKnownStatus[path]` purely to
detect a GENUINE status change (a task's first-ever observation only seeds this map,
firing no request — matches every other migration-style dedup map in this file, so
enabling this feature doesn't flood every existing jira-tagged task with a request the
moment it ships; a repeated observation of the SAME status is also a no-op). On a
genuine change into `in-progress`, `review`, or `done` (`jiraStatusesOfInterest` — any
other status, e.g. `open`/`triage`, produces no request at all), for each key in `jira`
it dispatches one bridge message per key:

> Jira: transition PROJ-101 to In Progress because task Tasks/X.md moved to in-progress.
> Use mcp__jira__jira_transitions to list, pick the transition whose toStatus category
> matches "indeterminate" AND whose meaning genuinely matches reaching In Progress — never
> a transition that merely shares the category, such as a rejection/withdrawal ("Won't
> do", "Zurückgewiesen") when the target is a done category, or a "Blocked"-style status
> when the target is indeterminate. If no transition satisfies BOTH the category and that
> meaning (the issue may be several workflow states behind the work), do not pick the
> nearest same-category option and do not force a multi-hop jump — refuse and tn ask on
> the task naming the desired category instead.

and for `jira-nc` (if set), a DIFFERENT, comment-only message instead — never a
transition instruction:

> Jira NC: add a comment to NC-1 noting task Tasks/X.md reached review (MR: <url> if the
> task has one) — do NOT change its status; it's a regulated record and closure follows
> your nonconformity procedure.

The message names a Jira status CATEGORY (`indeterminate` for in-progress/review,
`done` for done — Jira's own category set has no distinct "review" category, so review's
hint explicitly tells the agent to look for a named "In Review" transition first and fall
back to In Progress), never a specific transition or status name: those are localized
(this Jira instance answers in German, e.g. "Zu erledigen"/"Fertig") and per-project
workflow, resolvable only by an agent calling `mcp__jira__jira_transitions`. The
daemon also does NOT attempt multi-hop workflow logic: a real observed case has an issue
in "To be approved" with only two transitions available ("approve" → Ready for
Development, "Won't do") while its fix is already in an open MR — several states behind
the work. The message's own fallback line ("if none matches... tn ask... rather than
guessing") is the documented way this is meant to be handled — by the agent, not the
daemon.

Dedup: `state.JiraRequests["<path>|<key>|<target>"]` (target is the status category for
a transition request, or the literal `"nc-comment"` for a jira-nc request) → RFC3339
timestamp of the last request, checked/set individually per key so a multi-key `jira`
value produces one independent request per key. The whole path's entries are cleared
the moment ITS status changes to anything else (not just re-observed) — a later return
to the same category is a new episode and requests again.

**Routing: accepting agent, never the draining task owner.** Both `checkJiraTransition`
dispatches set `sendRequest.SkipOwnerRouting: true`, which makes `resolveTargetLocked`
skip its normal task-owner-wins-even-while-draining rule and fall straight to picking an
alive ACCEPTING agent (or queuing to the logical name if none). Every other task-scoped
dispatch (a routed `@claude:` reply, an assignment) still prefers the owner even while
draining — that's correct there, since it's a follow-up on work the owner is already
doing. A Jira request is different: the task has already REACHED its target status, so
there's no continuity with the owner to preserve, and a draining agent is alive but not
picking up new work — routing there left a real request sitting `delivered`/unacked
during the !1067 live test (`orchestrator-myapp-g57080`, draining, never touched it).
`TestJiraTransition_RoutesToAcceptingAgentNotDrainingOwner` asserts both directions in
one test (Jira → accepting agent, same-task `@claude:` reply → still the draining owner)
so the split can't quietly collapse back to "everything task-scoped goes to the owner".

**The rejection/blocked trap.** Matching purely on `toStatus.category` is unsafe: a
rejection/withdrawal transition ("Won't do" / "Zurückgewiesen") shares the `done`
category with genuine completion, and a "Blocked"-style status can share `indeterminate`
with real in-progress work — caught live when an orchestrator working PROJ-101 flagged
that literal category-matching per this spec's original wording could mark a shipped,
merged fix as rejected. Both request templates above now say explicitly: match the
category AND confirm the transition's target genuinely means what was asked, name the
known reject-style transitions as examples, and if no transition satisfies both, refuse
and `tn ask` rather than picking the nearest same-category option. Mirrored in the vault
at `Reference/Jira workflows.md` under "Gates", generalized beyond the PROJ-101 case that
first surfaced it.

## Self-restart durability

`watchBinaryForSelfRestart`'s ticker goroutine used to call `os.Exit(0)` directly the
moment it saw the on-disk binary change, with nothing stopping that from firing WHILE
another goroutine was mid-mutation — e.g. an assignment marker set in memory a moment
before `saveLocked` would have persisted it. `saveLocked`'s `atomicWriteFile` (tmp+rename,
fully synchronous) was never at risk of writing a torn/partial `state.json`; the actual
gap was purely in exit TIMING relative to in-memory mutation. Fix: `exitForSelfRestart(exit
func(code int))` acquires `s.mu` — the SAME mutex every state-mutating handler holds for
its own atomic Lock/mutate/`saveLocked`/Unlock cycle — before calling `exit`, and never
unlocks (the process is exiting). The watcher can therefore never interrupt a half-applied
mutation: it simply blocks until whichever handler currently holds the lock finishes.
`exit` is injectable (the real caller passes `os.Exit`) so `TestExitForSelfRestart_WaitsForInFlightMutation`
can verify the mutex-wait behavior directly — hold `s.mu` to simulate an in-flight mutation,
confirm exit does not fire while held, release it, confirm exit proceeds immediately —
without the test process actually terminating. Deliberately scoped small per this fix's
own directive: no WAL, no journal: a shared mutex fully closes the actual gap.

## Status endpoint (tray-app foundation)

`GET /status` (`handleStatus` → pure `buildStatusResponse`, directly unit-testable
with no lock/network): `{daemon:{uptime,version,pid,port}, projects:{<slug>:{agents:[{name,
alive,accepting,draining,stuck,unackedBacklog,rateLimited,rateLimitResetsAt?,rateLimitSince?,
contextPct?,tmuxSession?,lastSeenAt,ownedInProgress}],
queued,delivered,taskCounts:{<status>:N},needsAction,maxOrchestrators,maxWorkers,
activeOwners?}}, needsActionTasks:[{title,path,status,project?,mr?,integrationBranch?,
integrationRole?}] (cap 20), stuckPrompts:[{agent,
tmuxSession?,excerpt}], recentActivity:[{agent,project?,at,text}] (cap 20, newest first)}`.
`daemonStartedAt`/`daemonVersion` (currently `"0.1.0"`, bump on notable changes) are
package-level. `ownedInProgress` is `len(ownedTaskPathsLocked(name))` — a cheap
in-memory, message-history-derived count, deliberately NOT verified against each
task's live status (that would mean a per-agent live query on every poll).

SPECTATOR-SAFE by construction: only reads `snapshotLocked()`/`stuck.snapshot()` plus
two TaskNotes queries shared across every project (never per-project/per-request),
each independently cached — `needsActionCache` (reused as-is) and a new 30s
`statusTaskCountsCache` (`buildStatusTaskCountsQuery` — every non-archived task, no
status filter, capped at `statusTaskCountsCap`=500, bucketed by `routingSlugForTask`
+ status client-side, same "one global query, bucket locally" pattern as the
due-task scanner/MR watcher/reconciler; a fetch failure keeps serving the last
successfully bucketed counts rather than blanking the response). Every configured
(serve.json) project is seeded into the response even with zero agents, so an
idle-but-configured project still shows up.

`GET /ui` (`handleUI`): a `const uiTemplateSrc` HTML/CSS/JS string parsed once via
`html/template` (`template.Must`) and executed with nil data — the page carries no
per-request server-side data (it's a static shell that polls `/status` client-side
via `fetch` every 5s and renders from that JSON), but still goes through
`html/template` rather than a raw string write, for auto-escaping if per-request
data is ever added later. Dark-mode via `@media (prefers-color-scheme: dark)`
overriding CSS custom properties. `obsidian://open?vault=<urlencoded "Obsidian
Vault">&file=<urlencoded path with .md stripped>` links for needs-your-action tasks.

## Stuck session detection

Headless orchestrators can silently wedge on an interactive permission prompt (auto-mode's "3 consecutive actions were blocked... Do you want to proceed?") — a Monitor-based inbox heartbeat looks alive throughout, since it's just idle, not actually blocked. An eager pass runs once ~15s after daemon startup (covers a prompt appearing around a binary self-restart), then every `stuckCheckInterval` (2 minutes, `TN_NO_STUCKCHECK=1` disables detection entirely — see `TN_NO_DIALOG` below for a narrower option), for each alive agent with a KNOWN tmux session — any name, not just `orchestrator-<slug>`-prefixed ones; a reported `TmuxSession` (generation agents) wins, falling back to `tn-<slug>` derivation for legacy agents (see SPEC-generations.md): `tmux capture-pane -t <session> -p` (read-only), then `detectLiveStuckPrompt` scans the last `stuckExcerptLines` (~15) lines for BOTH a marker ("Do you want to proceed?", "requires confirmation", "Esc to cancel") AND at least one numbered menu option (`parseStuckPromptOptions`) — **both required, in the same narrow window**. `stuck` = both found. Independently, `unackedBacklog` = not stuck AND the oldest delivered-but-unacked, non-Informational message to that agent has been waiting over 30 minutes (renamed from `stalled` 2026-08-25 — see the naming-lesson note below the two-sample design rule; a message the daemon itself marks `Informational`, e.g. a pure FYI nothing was asked of the recipient, is excluded from this computation entirely, since an agent never acking something that asked nothing of it is not evidence of anything). Precedence for display: rate-limited > gone > stuck > unackedBacklog > draining > alive (`sessionStatusDisplay` — see the usage-limit section below for why rate-limited outranks even gone).

**Scrollback false-positive fix.** Originally scanned the last ~40 lines for a bare marker substring, with no options requirement — Claude's pane prints an already-answered prompt verbatim as history, so a marker anywhere in that wider scan routinely re-matched a RESOLVED prompt, not a live one (live incident: this fired a fresh osascript dialog every ~2min for sessions working fine, and `/status.stuckPrompts` read 0 at the same moment a dialog was showing — a symptom of the detected-state flapping true/false between passes as scrollback moved the stale marker in and out of the scan window). Narrowing to one `stuckExcerptLines` window (shared with the excerpt shown on the session page) AND requiring an option line in that SAME window closes it: a prompt that's scrolled away has, by definition, had its option list scroll away with it, so it can never register live again. This also directly reconciles the dialog/status disagreement — `st.Stuck` is set (feeding `/status.stuckPrompts` via `stuckTracker.snapshot()`) in the SAME locked section, before the dialog is ever shown, so a dialog firing for an agent guarantees that agent is already present as stuck in the same pass.

Detection state (`stuckTracker`) is entirely in-memory, under its own mutex independent of the main state lock — the approval dialog can block up to 120s and must never hold up the rest of the bridge. On a fresh transition into stuck (or every `stuckDialogRepromptInterval`, 15 minutes, while it stays stuck), `tn serve` shows a macOS `display dialog` via `/usr/bin/osascript`: title `Claude orchestrator stuck: <name>`, text = the matched marker line plus the tmux attach hint, buttons `{"Dismiss", "Allow once", "Always allow"}`, default `Dismiss`, giving up after 120s (a timeout returns no button, treated the same as Dismiss). On "Allow once"/"Always allow": re-capture the pane and verify a marker is *still* present — the user may have already resolved it by attaching directly — before sending `1`/`2` respectively via `tmux send-keys`; a cleared prompt sends nothing. Every actual send is logged distinctly (`serve: user approved stuck prompt for %s (option %s)`) and appended as a bridge activity entry for that agent (so it shows on the session page).

**Episode signature fix.** "Is this a new prompt episode (reprompt immediately) or the same one still showing (stay rate-limited)?" used to hash the raw captured excerpt text. A working agent's pane changes on nearly every detector tick — elapsed-time counters, spinners, streaming tool output — so the raw-text hash changed almost every pass even though the SAME prompt with the SAME options was still showing, making nearly every tick look like a new episode and firing a fresh dialog every ~2 minutes regardless of `stuckDialogRepromptInterval`. This *was* the actual mechanism behind the live prompt storm, confirmed by an A/B/A test (detector off → 0 prompts in 5min; detector on, tray off → a prompt within ~2min). Fixed via `stuckEpisodeSignature(markerLine, opts)`: hashes the marker line plus every parsed numbered option's number+text, NOT the raw excerpt — a repainted pane with identical options is the same episode; a genuine change in the marker or the option set is a new one.

**`TN_NO_DIALOG=1`** suppresses ONLY the osascript `display dialog` call (`dialogFuncForEnv` swaps in a no-op `dialogSuppressed` that returns `("", nil)`, the same "no selection" outcome a timeout/Dismiss already produces) — detection, `/status.stuckPrompts`, the dashboard flag, and the session-page excerpt all keep working exactly as normal, since `st.Stuck`/`st.Excerpt`/the reprompt bookkeeping are all set in `updateStuckState` BEFORE `dialog(...)` is ever called. This is the daemon's ONLY Apple Events dependency; the SwiftBar tray's Attach / Allow once / Deny actions (`POST /agents/approve`) cover the identical response surface without it, which is what makes the dialog redundant rather than load-bearing. Defaults to the real dialog when unset (opt-in suppression, matching every other `TN_NO_*` flag's convention); the LaunchAgent plist sets `TN_NO_DIALOG=1` in `EnvironmentVariables` by default on this machine — editing the plist alone does NOT take effect on a daemon self-restart, launchd caches a job's environment at load time, so it needs an actual `launchctl` unload/load (or reboot) to pick up.

**Usage-limit ("rate limited") wait.** The same pane capture also feeds `detectRateLimitWait`, a state INDEPENDENT of `stuck` (neither suppresses detection of the other; a pane can carry both). It scans a wider window than the permission-prompt detector — `rateLimitScanLines` (30) vs `stuckExcerptLines` (15), because a usage-limit block is several lines tall and gets pushed down by whatever the session printed just before it stopped — for any of four case-insensitive substrings: `hit your session limit`, `hit your limit`, `usage limit reached`, `continuing automatically at`. Widening is safe here in a way it explicitly is not for the permission prompt (see the scrollback false-positive fix above): there is nothing to falsely re-answer, the dialog is informational and fires at most once per episode, and `usage limit has reset` — checked BEFORE the markers — ends an episode whose banner is still sitting in the window. An episode also ends when the markers simply scroll out. Both transitions are logged (`serve: <agent> usage-limit wait detected (resets <raw>)` / `... usage-limit wait cleared`) rather than pushed into the activity feed: an episode is self-resolving and typically minutes long. The reset time is kept as RAW pane text (`2:30pm (Europe/Berlin)`, or `2:30pm` from the `continuing automatically at` fallback) — whatever follows `resets `/`continuing automatically at ` up to end-of-line or a ` ·` separator — never parsed into a `time.Time`: the pane prints a bare local clock time whose date and DST treatment are Claude's, and re-deriving an absolute instant would invent precision the source doesn't have. It is display text and the episode's identity, so an empty reset string is a valid limited result.

State is `RateLimited`/`RateLimitResetsAt`/`RateLimitSince` on `stuckSessionState`, surfaced as `rateLimited`/`rateLimitResetsAt`/`rateLimitSince` on each `/status` agent entry, as `⏳ usage limit (resets <raw>)` in the dashboard Sessions table, and as a `## Waiting: claude.ai usage limit` block on the session page. It outranks every other status INCLUDING gone, which nothing else does: a parked session stops polling its inbox by definition, so it goes not-alive within `aliveWindow` every time, and reporting that as `⚫ gone` would hide the one fact that explains it — it comes back by itself, nobody needs to investigate or respawn it. For the same reason it outranks `unackedBacklog`, which is how "suppress the backlog alarm while rate-limited" is implemented at the only place that alarm exists; `/status` keeps reporting `unackedBacklog` as a field, since the backlog is real, just expected. Exactly one osascript dialog per episode (identity = agent + raw reset string, so a moved reset time is a new episode; no reprompt interval, since there is nothing to approve and nagging about a self-resolving wait is how an alert channel gets trained into noise), title `Usage limit — <agent>`, single `OK` button, through the same injected `dialogFunc` — so `TN_NO_DIALOG=1` suppresses it exactly like the stuck dialog, with detection and reporting unaffected.

Pane capture, the dialog, and send-keys are all behind injectable function types (`captureFunc`, `dialogFunc`, `sendKeysFunc`) so tests never touch tmux or osascript.

Surfaced on the dashboard's Sessions table Status column and on each agent's session page (same precedence); a stuck session's page also gets a `## Stuck: permission prompt` section with the last 15 captured lines in a fenced code block.

**Approve re-scan (0.6.5).** `POST /agents/approve` re-scans the agent's pane right after acting: synchronously when the approval was NOT sent (the prompt was already gone — the pane is past it, so the tray's own post-action refresh must see it cleared), and ~3s later in the background when it was sent (Claude needs a moment to consume the key). Live incident 2026-09-04: a tray approval hit "cleared before send", and ⛔ stayed on the tray for 95 more seconds until the next `stuckCheckInterval` tick while the attached pane showed nothing to answer. The re-scan uses `dialogSuppressed` — this path never opens a dialog.

## Credential profiles & auto-swap (2026-09-03, daemon 0.6.0)

Claude Code keeps its claude.ai OAuth credentials in ONE macOS Keychain item (service `Claude Code-credentials`, account = macOS username, secret = an opaque JSON blob with `claudeAiOauth.{accessToken,refreshToken,...}`), read through `/usr/bin/security`. Every Claude session on the machine reads that item, so replacing its contents switches all of them on their next API call — verified live 2026-09-02: after a manual `/login`, every orchestrator pane printed "signed-in claude.ai account or organization changed on this machine" and carried on under the new account.

`creds.go` builds on that. A **profile** is a saved copy of the blob under a label; profiles are themselves Keychain items (service `tn-credential-profiles`, account = label) so no secret is ever written to disk. `~/.local/state/tn/creds.json` (0600) holds only bookkeeping: `active`, and per profile `savedAt`/`lastUsedAt`/`lastLimitedAt`/`limitedUntil`/`email` (email is read — never written — from `~/.claude.json` at save time, for display). `keychainStore` is an interface; tests inject a map-backed fake and must never construct `securityCLIKeychain`. `newServer` leaves `Server.creds` nil — only `cmdServe` wires the real store — so no test path can reach `security` by accident; the endpoints answer 503 while nil and auto-swap is skipped.

**Which profile is live** is resolved against the Keychain, not believed from metadata (`resolveActiveLocked`): the live blob's refresh token (then access token) is matched against every stored profile, falling back to the recorded `active` label. A manual `/login` behind tn's back is therefore reflected by `tn creds list`, and `tn creds save <label>` after a `/login` is the way to capture a new account.

**Swap order** (`credsStore.Swap`) is the load-bearing part: (1) read the live blob; (2) resolve which profile it belongs to; (3) write the live blob BACK into that profile — Claude rotates refresh tokens, and without the write-back the next swap back would land on a token Claude already retired, a dead profile indistinguishable from a live one until the first API call; (4) read the target profile; (5) overwrite the Claude item; (6) record `active`/`lastUsedAt`. Same-label swaps are a 409/`errCredsSameProfile`; removing the active profile is refused (the Keychain would still hold its tokens). `~/.claude.json` is deliberately never modified — running sessions rewrite it wholesale, so a concurrent edit is a corruption race, and nothing on the API path reads it; only `/login`'s displayed email lags.

**Auto-swap** (serve.json `"credentials": {"autoSwap": true, "minSwapInterval": "10m"}`, default off) hooks the usage-limit episode start in `updateStuckState` (`autoSwapCredentials`), BEFORE the dialog so parked panes are moving again while the dialog is still up. Per episode: mark the active profile limited until the pane's reset text parses (`parseResetTime`: `2:30pm (Europe/Berlin)`, `2:30pm`, `15:04`, `3pm`; tz from the parenthesised zone, date = next such wall-clock instant; unparseable → `credsLimitFallbackCooldown` 5h, Claude's session window); skip if the last swap is younger than `minSwapInterval` (several orchestrators share one account and park together — one swap serves all, and a swap that didn't help must not chain into another); pick the oldest-used eligible profile (`Eligible`: stored, not active, not cooling down); swap; then **nudge every currently rate-limited pane**, not just the trigger's: `Escape` (cancels "continuing automatically at …"; harmless on an idle prompt) then the continuation text `credsNudgeText`, mirroring the wording Claude itself prints when a limit resets. `sendKeysReal` appends Enter to each send, so Escape+Enter is an empty submit. The dialog becomes "Usage limit — credentials swapped" with from/to and the nudge count; when no swap happened (off, interval, no eligible profile, error) the 0.5.6 dialog is shown with an `Auto-swap: <reason>` line appended. Every failure degrades to "no swap + note" — nothing here can take the detector down.

Endpoints: `GET /creds` → `{autoSwap, minSwapInterval, active, profiles:[{label,email,active,stored,savedAt,lastUsedAt,lastLimitedAt,limitedUntil}], lastSwap:{from,to,trigger,at}|null}` — spectator-safe, never a token. `POST /creds/swap {"label"}` → manual swap + the same pane nudge (`{from,to,nudged:[agent]}`; 404 unknown label, 409 already active, 503 no store). CLI: `tn creds save|list|use|rm` — `use` goes through the daemon so panes are nudged, and swaps directly (saying panes were NOT nudged) when the daemon is down.

Caveats, stated once here and in the README: a swap switches EVERY Claude session on the machine including interactive ones and disconnects Remote Control; the `security add-generic-password -w <secret>` argument is briefly visible in `ps` (Claude Code writes the item the same way; there is no stdin form); and rotating accounts to get past usage limits may conflict with Anthropic's usage policy — the feature is opt-in for that reason.

## Worktree reaper (2026-09-03, daemon 0.6.1)

Every `isolation:"worktree"` worker gets `<cwd>/.claude/worktrees/agent-<id>`; the harness removes it only when UNCHANGED at exit, and every real worker commits, so nothing is ever removed (census 2026-09-03, myapp: 103 agent worktrees — 7 locked, 28 dirty, 28 with unpushed commits, 8 merged, 11 already-deleted directories). ORCHESTRATOR.md's "tear down on task done" is not happening either. `reaper.go` is the daemon-side answer.

**Scope and rules.** Only entries under `.claude/worktrees/` are considered — never the main worktree, never a session's scratchpad worktree. A worktree is removed only when nothing can be lost: not locked by a live process (the harness lock reason is `claude agent <name> (pid N …)`; a dead pid is a stale lock and gets `git worktree unlock`, a live or unparseable holder is skipped); directory mtime older than `minAge` (2h) so a worker between creation and first commit is never raced; `git status --porcelain --untracked-files=normal` empty; and HEAD present on some remote branch (`git branch -r --contains HEAD`) — merged or not, because a pushed branch survives its worktree and an UNPUSHED commit is the only thing a worktree can uniquely hold. The local branch is deleted only when merged into the default branch (`origin/HEAD`'s target, e.g. `origin/development`; fallback origin/main|master), always with `git branch -d`, never `-D`; `git worktree remove` is never `--force`. Entries whose directory is already gone are counted as `pruned` and cleaned with `git worktree prune` (loses nothing) — but git's prune skips LOCKED entries, so a gone-directory entry whose lock holder pid is dead is unlocked first, and one whose holder is alive (or unparseable) is reported as `locked` rather than counted as pruned forever (0.6.4; live: three such entries). Everything else — locked, young, dirty, unpushed, error — is counted and reported, not acted on; the report is as much the product as the removal.

**Config** (serve.json): `"worktrees": {"reap": false, "dryRun": true, "minAge": "2h", "interval": "30m", "maxRemovePerPass": 20}` — those are the defaults: off, and dry-run when on. Turning `dryRun` off is a deliberate edit after reading a few passes in the log. `TN_NO_REAPER=1` disables the periodic sweep. Passes are serialised (a second call while one runs gets 409) and capped at `maxRemovePerPass` removals.

**Surfaces.** Log: one summary line per project per pass (with wall time) plus one line per removed/removable/error entry. `/status.worktrees` = `{enabled, dryRun, running, lastRunAt, projects:{slug:{scanned, removed, removable, pruned, locked, young, dirty, unpushed, skipped, errors, durationSeconds, defaultBranch, at, error}}}` — counts only. `POST /worktrees/reap {"project"?, "apply"?}` starts one pass **asynchronously** (202 `{started, dryRun}`; 409 if one is running) and `GET /worktrees[?project=]` returns the last report per project with full per-worktree entries (verdict/branch/age/note/seconds) plus `running`; `apply` defaults to false and is honoured even when the periodic reaper is off, so `tn worktrees [--project P] [--apply] [--json]` (POST, then poll GET every 2s) works as a manual tool without enabling the background sweep. Tests use real temporary git repositories (bare origin + clone + worktrees in every state), not stubbed porcelain.

**Time bounds and the background-QoS finding (2026-09-04, daemon 0.6.2).** The first live daemon pass over 104 worktrees took FIVE HOURS while the identical pass from a test binary in a shell took 32s. Cause: a launchd agent and everything it spawns run in macOS's background tier (PRIO_DARWIN_BG), where disk I/O is throttled hard — `git status` on an 8k-file worktree: 0.23s foreground, 21–43s under `taskpolicy -b`. There is no per-child escape (`taskpolicy -B` only takes `-p <pid>`; `-c` only clamps down), so `cmdServe` now calls `leaveBackgroundTier()` — `taskpolicy -B -p <own pid>` — once at startup, and children inherit. This is not only about git: the same night the daemon's 2-minute orphan check ran 9 times in 9 hours (background-tier timers are coalesced while the machine idles), which had silently disabled the liveness detection the daemon exists for, with `lastSleepAt` unset because it was throttling, not sleep. Belt and braces on top: every git query has a 3-minute timeout (`git worktree remove` gets 20 minutes — it deletes the tree itself, and a fresh monorepo worktree carries a full node_modules; two removals were killed at 3 minutes on the first apply pass, 0.6.3), each project pass has a 30-minute budget after which remaining worktrees are counted as `skipped` (picked up next pass), and a pass that is still running when the next tick fires is simply not overlapped.

## Design rule: never act on a single negative sample

Three independently-built subsystems — stuck detection (above), orphan/death detection (below), and the stuck-approval dialog's own "still present?" recheck before sending a keystroke — were each fixed by the same realization after each produced a real false positive first: **a single missed signal is not evidence, it's the absence of one.** An agent that hasn't polled in 90s might be doing real synchronous work, not dead. A pane that doesn't match a stuck marker this tick might have just repainted, not cleared. Silence is not proof of anything by itself; only a CONFIRMED, repeated, or otherwise corroborated absence is. Concretely this has meant: requiring the SAME signature on two consecutive ticks (stuck detector's episode signature, orphan detector's `deadConfirmTicks=2`) before acting, or re-verifying before an irreversible action rather than trusting a stale read (the approval dialog's re-capture-before-send). The next mechanism that decides "X is gone / X is done / X is safe" from one observation should start from this rule rather than rediscover it after its own incident.

**A field's NAME is part of its contract, not a cosmetic detail.** `unackedBacklog` (this section, above) was called `stalled` until 2026-08-25. The computation was never wrong — a message can genuinely sit delivered-but-unacked past the threshold — but the NAME asserted something the computation didn't measure: "stalled" reads as "this agent is frozen," and a human (twice, including reporting it to the user) acted on that reading rather than on what the field actually said. The agent in question had a fresh `LastSeenAt` and an actively-running 42-minute subagent the whole time; the one unacked message was an informational resend the agent had no reason to ever ack. This is the SECOND time in one day a correctly-computed MEASUREMENT got misread as a MALFUNCTION and sent someone chasing a non-problem — the first is `deadOrUnobservedTaskOwnersLocked`/`LastSeenAt`-as-death, in Orphan/dead-agent detection just below. Same shape both times: a signal computed correctly, named or read in a way that implies a conclusion it doesn't support, then trusted as one. The fix here was two-part, not just a rename: (1) `Message.Informational` excludes a message that asked nothing of its recipient from the computation entirely — an agent not acking a pure FYI is not evidence of anything, and this codebase already had at least one such message (`"FYI, not an action item: ..."`, the dead-agent-reset notice) that had been silently eligible to cause the exact same false flag for years before anyone noticed; (2) the field itself, and every place it surfaces (Go struct/JSON/webui/dashboard text/these specs), was renamed to say what it measures. Naming the mechanism honestly is not optional polish — it is the last mile of the two-sample rule above, because the best-designed signal still fails if what a human reads off it isn't true.

## PermissionRequest hook detection (2026-08-26)

Pane-scraping (above) infers a pending modal from the last N lines of a terminal snapshot — that inference is why it needs two-tick confirmation, a persistence backstop, and still produced a real false positive (g53286). Claude Code's own `PermissionRequest` hook fires a structured, unambiguous event at the exact moment a permission decision is genuinely pending, and — confirmed empirically against a real interactive session, not just read from docs — fires WHILE the CLI is actually blocked waiting for an answer, which is precisely when the agent has no way to self-report. This section adds that as a second, faster detection channel; it does not replace pane-scraping, for a reason documented below that turned out to be permanent rather than transitional.

**Verified empirically (2026-08-26 spike), not assumed:**
- `PermissionRequest` is real, and its "report but don't decide" contract holds by construction: a real `"Do you want to proceed? 1. Yes / 2. No"` prompt was captured sitting genuinely pending in the pane after the hook had already fired and exited 0 with empty stdout — silence never approved anything.
- `$TN_AGENT_NAME` (set by `buildLaunchCommand` as a literal env-var prefix on the exact command that execs `claude` in the spawned tmux pane) reaches the hook subprocess's own environment, confirmed directly in two independent sessions. This is the entire correlation mechanism — no new state needed to map a hook POST back to which agent sent it.
- The real payload for `PermissionRequest` carries `session_id, transcript_path, cwd, prompt_id, permission_mode, effort, hook_event_name, tool_name, tool_input` — notably **no `tool_use_id`** (unlike `PreToolUse`, which has one). Any future correlation/dedup work on this event must key on `prompt_id`, not `tool_use_id`. `permission_mode` was observed as `"default"` when the CLI flag was `manual`, so the CLI-flag-to-payload-string mapping isn't 1:1 — don't branch on this field without re-verifying the mapping for `auto`, which is what production actually spawns with; the current implementation deliberately receives it but does not branch on it.

**`PermissionDenied` was reconsidered mid-design, and the reversal itself is worth recording.** It was FIRST scoped OUT — two negative empirical attempts (a human declining a live prompt, a hard `permissions.deny` rule) found nothing, and the working assumption was version skew against the installed build (2.1.246). That reasoning didn't survive contact with a canonical-docs re-check (code.claude.com/docs/en/hooks, fetched directly, not relayed): both attempts tested the WRONG triggers, not evidence the event doesn't exist. `PermissionDenied` IS real, documented, and supports `hookSpecificOutput.retry:true` — telling the model it may retry a denied call, ignored when "the classifier produced no verdict" (i.e. it does nothing for the two triggers already tested, which explains the earlier negative results perfectly). Four FURTHER attempts specifically targeting a genuine Stage 2 classifier verdict (the real production trigger: model self-refusal via `curl | sudo bash`, a `git push --force` in a harmless throwaway repo, a `glab mr update` matching the exact real-world command, and re-confirming the hard-deny-rule short-circuit) still didn't reproduce it — "usually transient" in the real error text reads as genuine non-determinism, not something forceable on demand. Decision: build it anyway, as PASSIVE TELEMETRY — no retry, no decision, exit 0 unconditionally — because a telemetry-only handler costs almost nothing either way: if it never fires in production, that's learned for free; if it fires, it's exactly the signal the user asked for. A real occurrence in the wild is better evidence than a lucky synthetic reproduction, and grinding further on a repro that resists forcing isn't a good use of time.

**THREE REFUSAL LAYERS ARE NOW KNOWN, and hooks see NONE of them — this is a permanent, explicit non-goal, not a temporary gap to be closed by a future version upgrade:**
1. **Model self-refusal** — Claude's own judgment declining to run something (the `curl | sudo bash` attempt above): no `PreToolUse` fired for the refused command at all, because the tool call was never actually attempted.
2. **`permissions.deny` rules** — a hard deny short-circuits before ANY hook fires, confirmed empirically: not even `PreToolUse` runs for it.
3. **Stage 2 classifier verdicts** — the real production trigger (`"Stage 2 classifier error - blocking based on stage 1 assessment"`), unreproduced synthetically; `PermissionDenied` telemetry (below) is how a REAL occurrence gets captured instead of waited for.

The user's own stated goal was *"reliable detection of the failed permissions to be able to resolve them reliably through interaction"* — hooks reliably detect PENDING prompts (layer that's answerable), but may detect NOTHING for a refused call, depending on which of the three layers refused it. This is what makes pane-scraping and the `seenSinceStart`-shaped proof marker PERMANENT architecture rather than transitional scaffolding (see below): nobody should later assume hook coverage is complete and quietly stop maintaining the fallback, because hook coverage of the refused case is structurally partial by design, not by omission. `retry:true` stays documented as a real capability specifically so nobody rebuilds it as an automatic response later without knowing it was deliberately left unattended: a hook that silently retries a classifier denial is a blanket allow wearing a disguise, and this project has already been steered away from blanket allows twice in one day.

**Architecture — PermissionRequest (answerable).** `POST /hooks/permission-request` (`handleHookPermissionRequest`) receives `{agentName, sessionId, promptId, toolName, permissionMode, command}` from `hooks/claude-hook.sh`, a small script the user installs into their own `~/.claude/settings.json` (never written by the daemon itself — see that script's own header comment for the exact entry and reasoning; it handles BOTH `PermissionRequest` and `PermissionDenied` by branching on `hook_event_name`, one file kept in sync rather than two). The handler responds `200` IMMEDIATELY, before doing any real work — Claude Code appears to hold the interactive prompt from rendering until a configured hook returns or times out (every hooks.json example in Claude Code's own docs sets a bounded `timeout`), so the HTTP round trip must never make a human wait on the daemon. The actual work — capture the pane fresh, re-derive `stuck` from `detectLiveStuckPrompt` (never trust the hook firing alone: the prompt may already be resolved by the time this runs), then feed into the SAME `updateStuckState` state machine pane-scraping uses — happens in a goroutine (`triggerHookConfirmedStuck`), sanity-checked by literally removing the `go` keyword during development: it deadlocked the entire test suite on a blocked HTTP call, which is exactly the failure mode being guarded against.

**Architecture — PermissionDenied (telemetry only).** `POST /hooks/permission-denied` (`handleHookPermissionDenied`) does NO session/pane interaction at all — there's nothing pending to capture or resolve, only a fact to record. Aggregates into `State.PermissionDenials` (`permissionDenialRecord`), keyed by `agentName+toolName+CommandSummary` — grouped by command SHAPE (first ~2 whitespace-separated tokens, e.g. `"git push"`, `"glab mr"`), not exact text, so the same underlying block recurring with different arguments (different MR numbers, different branches) accumulates into ONE counter with a `Count`/`FirstSeenAt`/`LastSeenAt`/`LastCommand`, rather than a fresh record every time — "glab mr update denied 4 times" is what makes this actionable; "Bash denied 4 times" isn't. Surfaced at `/status.permissionDenials`, sorted most-recent-first, same passive-visibility posture as `Triage` — it does not decide anything is wrong, it answers "what's been blocked, how often, most recently when" for whoever looks. `zz-`-prefixed test agents are excluded, same as everywhere else, and `POST /agents/purge` deletes a purged name's `PermissionDenials` entries too (a composite-key scan, since the map isn't keyed by agent name alone) — otherwise spike/verification traffic would linger in `state.json` forever even once hidden from `/status`, exactly the class of bug `/agents/purge` already exists to fix for agent records.

**Deployment scoping: settings.json entry vs. skill-frontmatter — resolved in favor of neither, in favor of a THIRD option.** The original design proposed a global `~/.claude/settings.json` entry; a later idea was scoping the hook via a skill's own frontmatter (active only for daemon-spawned sessions that invoke `skills/tasknotes`, never touching the user's global config). Checked against the docs before building on it: **hooks are documented as loaded ONLY at session start and explicitly CANNOT be hot-swapped mid-session** — "Editing hooks/hooks.json won't affect current session... Must restart Claude Code: exit and run `claude` again." No `hooks:` field was found documented in `SKILL.md`'s own YAML frontmatter (only `name`/`description`/`version`/`effort`) or in subagent frontmatter as a session-hook-injecting mechanism — invoking a skill mid-session, and "hooks cannot be supplied per invocation," are in direct tension, and this was not chased further empirically given the "cannot hot-swap" claim is specific and comes from the same hook-development reference material as everything else confirmed accurate so far. **The actual answer was already sitting in this session's own spike methodology**: a PROJECT-LOCAL `.claude/settings.json`, placed in the root of each `tn`-managed repo, achieves the identical scoping goal (daemon-spawned orchestrators only, since Claude Code reads project-local settings based on the session's own `cwd` — which for a spawned orchestrator is always that project's repo) without needing any unconfirmed mid-session mechanism, without touching the user's global config, and without inventing anything new — this is the exact same deployment shape both hook spikes in this section already used successfully. Recommended over a global entry for that reason; the global-settings.json snippet in `hooks/claude-hook.sh`'s own header comment stays as the simpler option for a user who wants coverage everywhere with one edit.

`confirmedStuck` gained a third OR-branch: `fastConfirm || backstopConfirm || (stuck && hookTriggered)` — ANDed with `stuck` from the FRESH capture, not a bare `hookTriggered`, for the same reason `sendApprovalIfStillStuck` always re-verifies before sending a keystroke rather than trusting the moment of triggering. Everything downstream (dedup by episode signature, the re-prompt interval, sticky display, the clear-transition log/activity entry, `sendApprovalIfStillStuck`'s own re-verification) is completely unchanged — the hook only changes HOW FAST `confirmedStuck` is reached, never what happens once it is.

**The `seenSinceStart`-shaped proof marker: `Agent.HookConfirmedAt`.** Set the first time (and only the first time) this agent's hook is ever observed to fire; `nil` means UNPROVEN, never "not stuck" — the same three-state discipline as `seenSinceStart`/`deadConfirmTicks` above, applied to a different absence. Deliberately persisted on the `Agent` record itself, unlike `seenSinceStart`'s non-persistence: what this proves — that THIS agent's own environment has working hook plumbing — is scoped to the agent's identity, not the daemon's process lifetime, and a daemon restart doesn't invalidate it the way it invalidates "have I personally heard from you since I came up." A new generation (new agent name) always starts unproven, correctly. Only a proven agent gets the near-instant hook path; every other agent is covered exactly as before, with pane-scraping continuously and fully active for it.

**Reliability comparison.** Every fresh confirmation (`!wasStuck && confirmedStuck`) logs which path found it — `"%s confirmed stuck via %s"` with source `hook`, `pane-fast`, or `pane-backstop`. The acceptance bar: every pane-detector confirmation should also show up as a hook confirmation, sooner, for any agent with a proven hook. Grep both log lines and compare timestamps rather than trusting either mechanism's self-report — nearly free, since both branches run unconditionally regardless of which one actually fires first.

**Live end-to-end verification (2026-08-26):** a `zz-`prefixed test agent with a real tmux pane rendering a genuine `"Do you want to proceed?"` prompt received the actual `hooks/claude-hook.sh` script's POST against the real running daemon. Log trail: proof marker set on first fire, the request logged with its real tool/prompt/mode fields, and `"zz-hooktest confirmed stuck via hook"` — the full pipeline (script → network → handler → goroutine → fresh capture → state machine) working end to end, not just each piece in isolation. `/status.stuckPrompts` correctly excluded it, same as every other `zz-`-prefixed test registration (`testVerificationAgentPrefix`) — expected, not a gap.

**First real production result (2026-08-26, minutes after deploy).** `PermissionDenied` telemetry did what two full spike rounds of synthetic reproduction couldn't: captured a genuine Stage 2 classifier denial in the wild, passively, without anyone forcing it. Within about 8 minutes, one real agent (`orchestrator-myapp-g34005`, an e2e worker) produced three distinct denied command shapes:

```
count 4, 12:07:56 -> 12:09:47: cd apps/miku-mobile && env MIKU_APPIUM_HOST=... bash e2e/scripts/run-spec-safe.sh ...
count 1: cd .../.claude/worktrees/agent-.../apps/miku-mobile
count 1: sed -i '' 's/post-medication-plan-with-on-demand/post-medication-plan-od/g' ... (a fixture rename)
```

This overturned the day's working assumption. The whole day's chase had been integration-branch pushes and `glab mr` operations as THE blocking problem — none of that appears here. The count-4 entry is the spec runner itself, the command that actually executes e2e specs against a VM, denied four times in under two minutes; the other two are a `cd` into the repo's own app directory and a `cd` into a worktree. None of these are remotely dangerous operations, which is exactly consistent with "Stage 2 classifier error ... usually transient" reading as a classifier misfire rather than a deliberate policy decision — the friction was in the CORE WORK LOOP, not the higher-risk operations everyone had been watching for. Surfaced to the user as an allowlist decision (their permission policy, not this daemon's to broaden unilaterally); the value delivered here is turning "why is e2e slow" from speculation into three named, counted, timestamped command shapes within minutes of deployment, passively — precisely what the user asked for.

**A real bug surfaced and got fixed the same day this first result landed**, worth recording for its own lesson: `statusPermissionDenial`'s JSON key was `"agent"`, matching an existing sibling struct's (`statusStuckPrompt`) convention rather than the name this feature's OWN pipeline uses everywhere else — the hook payload's `agentName` field, and `permissionDenialRecord.AgentName`. A query expecting `agentName` (the name used by everything upstream of `/status`) found nothing and read a fully-populated real record as empty — diagnosed by diffing raw `state.json` against raw `/status` output directly rather than guessing from either end, which took minutes rather than a round of speculative fixes. Fixed by matching the field to its own feature's pipeline naming rather than the unrelated sibling's. The deeper lesson: every existing test for this feature asserted the Go struct field's VALUE, never the actual JSON key on the wire — exactly the layer the bug lived in, and a class of bug invisible to value-level tests by construction. `TestStatusPermissionDenial_JSONFieldIsAgentName` (status_test.go) now marshals to real JSON and checks the literal key.

**Known, unresolved tradeoff, not a bug**: fixing `statusPermissionDenial` to use `agentName` makes it the ODD ONE OUT against the /status surface's actual established convention — `statusStuckPrompt.Agent` and `statusActivityEntry.Agent` both already use the shorter `"agent"`, and both are live, already consumed by the webui/tray. Renaming either of those to match would be a bigger, riskier change (real consumers, not a brand-new field) and hasn't been done. `TestStatusStructs_IdentityFieldWireNames` pins down this exact, current state — a future decision to unify one way or the other should have to update that test deliberately, not discover the asymmetry by accident.

**Grouping granularity, open design question, not yet acted on.** `permissionDenialRecord` groups by the first ~2 whitespace-separated tokens of the command (`commandSummary`) — coarse enough that both `cd` denials above (a repo-relative path and a worktree-absolute path) collapse into neighboring "cd"-family records but stay as two separate rows rather than merging into one, since the full key is `agentName+toolName+commandSummary` and both commands genuinely start with `cd`. Whether 2-token grouping is coarse enough to stay useful and fine enough to stay actionable as volume grows is untested at scale — one agent produced 3 shapes in under 8 minutes here; a busier day across many agents could produce a lot of rows. No design decision made yet; worth revisiting once there's more real volume to look at rather than guessing ahead of the data.

**Stuck-prompt excerpt was showing the decision, not the subject (found, then fixed, 2026-08-26).** A real defect, raised by the user directly from the tray: the excerpt/dialog showed `"Do you want to proceed? 1. Yes / 2. Yes, and don't ask again for: curl * / 3. No"` with Attach/Allow once/Deny buttons — and no indication of what command was actually being approved. Their words: "I would like to see the command I'm allowing." Root cause: `stuckExcerptLines` (15) bounds BOTH the narrow window `detectLiveStuckPrompt`/`parseStuckPromptOptions` scan for a live prompt AND what gets stored as the displayed excerpt — and the command block (a tool-use header naming the subagent, the command itself, the classifier's stated reason) routinely sits ABOVE that narrow tail, so the excerpt reliably contained the least useful half: the marker and options, never the thing they're options FOR. Manually capturing 40 lines instead of 15 against the actual reporting pane showed the whole block.

Fix, deliberately NOT just bumping `stuckExcerptLines` itself (that would reintroduce the exact false-positive risk that narrowed it from 40 to 15 in the first place — a marker substring anywhere in a wide scan can match an already-resolved prompt still sitting in scrollback, see this section's own opening paragraphs): a SEPARATE, wider constant, `stuckContextScanLines = 40`, used ONLY to build the displayed excerpt (`buildDisplayExcerpt`), and ONLY ever consulted after `detectLiveStuckPrompt` has already confirmed a live prompt on the untouched narrow window — widening it here cannot manufacture a false positive, it can only show more of an already-confirmed-real prompt's own surrounding context.

Combined with a second, independent source that turned out to already be flowing to the daemon and being silently discarded: `hooks/claude-hook.sh` already extracts `tool_input.command` and sends it in the `/hooks/permission-request` payload (the sibling `/hooks/permission-denied` handler already consumed the identical field) — `handleHookPermissionRequest` just never read it. Now threaded through `triggerHookConfirmedStuck` → `recordHookCommand` → `stuckSessionState.HookCommand`/`HookToolName`/`HookCommandSignature` (scoped to the specific prompt episode's signature, cleared the moment a DIFFERENT episode's signature is seen, so a stale command from a resolved prompt is never shown against a later, unrelated one) and prepended to the built excerpt (`"[hook] Bash: <command>\n\n<wide pane capture>"`) whenever it's available for the current episode. This is the more reliable of the two sources where it applies — structured JSON, immune to any scan-window truncation or terminal wrapping — but only ever present when the hook actually fired (a hard `permissions.deny` rule bypasses the hook entirely, and non-Bash-shaped tools carry no `tool_input.command`), so the widened pane capture stays the fallback (and the only source at all for the human-readable framing — which subagent asked, the classifier's stated reason — that a hook payload doesn't carry).

Priority order requested and delivered: command (hook-sourced when available, otherwise from the widened pane capture), which subagent asked (pane only), the reason (pane only), then the options (already shown via the dialog's own buttons, and already dropped from the text body by `stuckPromptContext`, unchanged). Regression test (`TestUpdateStuckState_ExcerptIncludesCommandBlockBeyondNarrowWindow`) reproduces the actual incident shape and — confirmed by reverting the fix and rerunning it — fails exactly as the incident did (command/attribution/reason absent) against the pre-fix single-window code.

**Live-verified against the actual reported prompt (2026-08-26, minutes after deploy)**, not a synthetic one: `orchestrator-myapp-g57739`'s genuinely-pending prompt (the one that prompted the bug report) showed, post-deploy, `"Bash command · from the svc-9023-redeploy agent"`, `"curl -s -m 3 http://localhost:9023/health ..."`, and `"Blocked by classifier"` — all three previously-missing pieces, confirmed independently by both team-lead and this agent's own `/status` poll. Since the hook had already fired for this SPECIFIC episode before the fix existed, `HookCommandSignature` had nothing cached for it — this was an honest test of the WIDENED PANE PATH alone (the fallback), not the hook path, and it supplied everything on its own. The in-memory stuck tracker resets on daemon restart (never persisted, by design — see `stuckSessionState`'s own doc comment), so the prompt needed one fresh pane-scraping detection pass after the restart before the excerpt reappeared at all; expected, not a fault.

## Orphan / dead-agent detection

**Honest history, not just mechanism.** `sweepOrphanedOwnershipDebt` was built to fix a real incident (a week-long chain of dead generations left 12 in-progress tasks stuck on one project) — but for its entire life before 2026-08-25, it never actually recovered anything real. Its one measurable effect in that period was mass-false-orphaning LIVE agents on every daemon restart (roughly ten restarts on 2026-08-25 alone, each one flagging all registered agents dead, including whichever was genuinely working), which produced at least one confirmed duplicate-MR incident when a reset raced a live orchestrator that had just dispatched a worker. The three layers and the tmux-based recovery below were built that same day in direct response. Once they landed and the mechanism was re-enabled, the first real sweep run found NOTHING to recover: `taskOwnerLocked` no longer resolved any task to the 9 agents it correctly identified as dead, because every task they used to own had already been reassigned organically through ordinary same-day work, not through any sweep. That is the right end state for a safety net — present, verified, idle — and it should not be read as "the mechanism worked as designed from the start." It did not. It was net-negative for most of its life and only became net-neutral-or-positive once the false-positive path was actually closed.

`checkOrphanedDeathsOnce` (ticks on the same cadence as the stuck detector) and `sweepOrphanedOwnershipDebt` (a one-shot pass at startup, recovering debt from agents already dead before this process began) both decide whether an agent's owned in-progress tasks should be reset to `open`. Both are gated behind `TN_ENABLE_ORPHAN_SWEEP=1` (default OFF — inverted from every other `TN_NO_*` flag, deliberately: this gates a mechanism with a *confirmed* false-positive path, not a hypothetical one) and, even when enabled, behind two further layers, added after a 2026-08-25 incident where a daemon restart flagged 11 of 11 registered agents as dead, including the only genuinely live one, because none had polled since the process came back up:

1. **Startup grace** (`Server.startedAt`, per-instance, separate from the package-level `daemonStartedAt` used for `/status` uptime): both mechanisms are completely inert for one `aliveWindow` after construction.
2. **Per-agent "seen since start"** (`Server.seenSinceStart`, non-persisted, populated by `touchAgentLocked` — poll, ack, worker start/end, log, context report — and by registration): an agent absent from it is UNKNOWN, not dead, regardless of how stale its `LastSeenAt` looks. Unknown must never authorise a destructive action; only after an agent has proven it can report in at all does its subsequent silence become evidence. Applied to `checkOrphanedDeathsOnce` and to `deadOrUnobservedTaskOwnersLocked` (the startup sweep's candidate source) identically.
3. **Two-tick confirmation** (`deadConfirmTicks=2`, `Agent.ConsecutiveDeadTicks`): `checkOrphanedDeathsOnce` only treats an agent as a candidate after two consecutive ticks of it being both known and not-alive — the design rule above, applied here specifically.

Layer 2 alone would mean `sweepOrphanedOwnershipDebt` can never recover an agent that died before the CURRENT restart and never polls again — the mechanism's original headline case (a week-long chain of dead generations). That gap is closed WITHOUT reintroducing any inference from silence: `deadTaskOwners` (the sweep's real candidate function, wrapping `deadOrUnobservedTaskOwnersLocked`) checks a never-observed agent's tmux session via `tmux has-session`, a POSITIVE, VERIFIABLE fact independent of whether this process has personally heard from the agent — sessions outlive a daemon restart. Three-state, not boolean (`tmuxSessionState`: `Exists` / `Gone` / `Unknown`), because `has-session`'s exit code alone cannot distinguish "this session doesn't exist" from "the tmux server itself isn't reachable at all" (no binary, no server, a permissions error) — only the former is evidence about a specific agent; the latter is a fact about the machine that would read identically for every agent at once, reproducing the exact mass-false-positive shape this whole mechanism exists to prevent. Recovery fires ONLY on a confirmed `Gone`: `Exists` means hung, not dead (the stuck detector's case, not this one's), and `Unknown` must never collapse into `Gone`. An agent with no resolvable tmux session at all (`tmuxSessionForAgent` returns `ok=false`) is left alone too — nothing to check, so nothing to confirm. The tmux calls themselves run OUTSIDE `s.mu` (`deadTaskOwners` acquires the lock only for the in-memory half, `deadOrUnobservedTaskOwnersLocked`, then releases it before any subprocess call — the same "no external I/O under the lock" rule as every network call elsewhere in this file). Distinct from the boolean `tmuxHasSessionFunc` used by the spawn path (`tmuxSessionExistenceFunc` here) — that one's "couldn't tell" and "not there" are safe to collapse (worst case: a redundant spawn attempt); this one's aren't, because it feeds a decision to reset another agent's owned tasks.

The disabled-path log lines (`"...DISABLED... would have processed..."`) run through the exact same gated logic as the enabled path — same grace period, same `seenSinceStart` check, same two-tick requirement for the periodic check, same tmux-confirmation path for the startup sweep — so they're a genuine dry-run prediction of what enabling would do, not a bypass of it. Two further always-on logs, independent of the enable flag, keep the whole mechanism observable rather than silently correct: `"orphan check: N agent(s) look stale but have not been observed..."` (periodic check, per-tick) and, for the startup sweep specifically, `"N never-observed agent(s) confirmed dead via tmux session absence"` / `"N never-observed agent(s) have an unresolvable tmux check"`. If a future restart logs a genuinely live, currently-polling agent in any of these, one of these layers has broken.

Existing (guard against resetting a task a live agent has since adopted or is actively working via a declared worker): `hasLiveDeclaredWorkerLocked`, checked inside `resetOrphanedInProgressTasksForAgent` on the enabled path only — the disabled-path "would have processed" counts are agent-level and do not run this per-task check, so they can slightly OVERSTATE true impact in the narrow case where a specific task would actually survive the guard. They can never wrongly name a currently-alive, actively-polling agent, which is the risk the layers above close.

**Stranded-message reassignment (2026-08-27) — the same class of gap, one hop over.** `handleRetire` reassigns an agent's pending (queued/delivered) messages on a CLEAN retire (see its own doc comment, which documents the incident that motivated it: three generations accumulating 8/16/8 stranded messages). That fix lived only inside `handleRetire` — a generation that dies WITHOUT calling it, the common case, had NO code path that ever reassigned its mail. Verified directly against production state, not theory: 31 queued/delivered messages sat permanently on dead or never-valid recipients, including seven "Task assigned:" directives up to six days old (five were done anyway via other means, two were still genuinely open, one had no corresponding task on the board at all) and 18 more addressed to `"claude/setup"`, an identity that was never a real registered agent.

Three complementary mechanisms, not one, because the failure shapes need different confirmation discipline:

- **`reassignStrandedMessagesForDeadAgent(agentName, project)`** — called from `triggerOrphanCheckIfNeeded`, right alongside `checkOrphanedTasks`, for a message addressed to a KNOWN agent record that's confirmed dead or cleanly retired THIS daemon lifetime. Deliberately reuses the EXISTING confirmed-dead signal (`deadConfirmTicks`, the startup grace period, `seenSinceStart`) rather than judging liveness independently — a second, separate liveness check here could in principle disagree with the confirmed one and reintroduce the exact false-positive risk (reassigning a LIVE agent's work) that the whole three-layer apparatus above exists to prevent. For a clean retire this is normally a no-op (handleRetire already reassigned everything inline before `triggerOrphanCheckIfNeeded` runs); it only does real work for a dirty death, which had no other path.
- **`sweepStrandedMessagesAtStartup`** — `sweepOrphanedOwnershipDebt`'s counterpart for messages instead of owned tasks, same placement (a one-shot startup pass) and same reason to exist: `reassignStrandedMessagesForDeadAgent` above only ever sees an agent THIS process personally watched transition from alive to dead — it is structurally blind to a generation that was already dead before the current restart and never polls again, which was the live incident's actual headline case (`orchestrator-myapp-g17165`, dead since 2026-08-25, holding 9 stranded messages across several restarts by the time this fix shipped). `deadMessageRecipients`/`deadOrUnobservedMessageRecipientsLocked` mirror `deadTaskOwners`/`deadOrUnobservedTaskOwnersLocked` exactly — same tmux-session-confirmed-gone positive fact for cross-restart recovery, same three-state discipline (`Gone` recovers it, `Exists` means hung not dead and is left alone, `Unknown` never collapses into `Gone`) — deliberately NOT re-derived from scratch; a genuinely different candidate SOURCE (every distinct message recipient, not just acked task owners — a stranded message is by definition never acked) feeding the identical, already-proven verification logic. Each confirmed-dead name found this way still flows through `reassignStrandedMessagesForDeadAgent`, so the archive-vs-reassign decision below applies uniformly regardless of which of the two paths found the agent.
- **`sweepUnroutableMessagesOnce`** — an independent periodic pass (same ticker as `checkOrphanedDeathsOnce`) for a message addressed to a name that was NEVER a valid agent or project queue at all (`"claude/setup"`-shaped) — no agent record exists to be alive or dead, so there's no liveness question to confirm and no reason to gate this on `deadConfirmTicks` or a tmux check. Legitimate ONLY as the logical `orchestrator-<slug>` queue for a project this daemon actually knows about (an autoSpawn config entry, or any agent — dead or alive — ever registered for it); that's normal in-flight queueing ahead of a fresh spawn (`retargetLogicalQueueLocked` drains it), not a bug, and is left untouched.

All three funnel into **`reconcileStrandedMessage`**, the shared archive-vs-reassign decision — explicitly NOT a blind bulk-reassign, per the requirement that motivated this whole fix: dumping a six-day-old "Task assigned:" directive on a live orchestrator for work that's already done would recreate the exact duplicate-work problem this project keeps paying for.

- No `TaskPath` (a reply, a note, an ad hoc send — nothing to check staleness against): always reassigns. Same default `handleRetire` already used.
- `TaskPath` set, task fetched successfully, `Status == "done"`: **archives** — `Message.Status` gets a fourth, terminal value (`"archived"`, plus `ArchivedAt`/`ArchiveReason`), never deleted, same tombstone-not-delete philosophy as `Agent.DeregisteredAt`. Every existing `Status ==` comparison in this codebase is an exact match against `"queued"`/`"delivered"`/`"acked"` — confirmed by grep before adding the fourth value — so `"archived"` is automatically excluded from every dashboard/count/delivery path with zero further changes needed.
- `TaskPath` set but the fetch fails (network blip, deleted task, anything) or the task is any status OTHER than `"done"`: reassigns. A fetch error is UNKNOWN, not "done" — the same "absence of signal is not signal of absence" discipline this file already documents elsewhere (layer 2 above; `checkOrphanedDeathsOnce`'s own doc comment) applied to a new place. An unknown state must never authorize the destructive (archiving) outcome.
- No known project to reassign to at all (an unroutable recipient with nowhere to send it): can only ever archive.

**Malformed-recipient prevention, found by direct consequence of building the sweep.** The 2 (of 31) stranded messages addressed to a bare `"orchestrator-"` (no slug after the dash) trace to an unguarded `"orchestrator-" + strings.ToLower(project)` expression reached with `project == ""` — confirmed live source: `routeTaskNotification` dispatches a "Task assigned:" message for any `claude`/`spawn`-tagged, assignable-status task via `t.routingSlug()`, which returns `""` for a task with neither a `claude-project` custom property nor any `projects[]` entry. `orchestratorQueueName(project)` is now the ONLY place that composes this shape, returns `""` instead of a malformed name for an empty project, and `resolveTargetLocked`'s final fallback (`dispatchMessage`'s underlying router) fails loudly — `400`, nothing queued, a log line naming the dropped text — rather than manufacture an address nothing will ever answer to. `handleRetire` was refactored onto the same shared `pickReassignmentTargetLocked` helper for the same reason, though in practice `Agent.Project` is validated non-empty at registration (`handleRegister` rejects `project == ""`), so that specific call site was lower-risk than `routeTaskNotification`'s.

**"Task still open" is only a proxy for "still relevant" — reassigned mail carries its own provenance.** Caught during live review of the actual 31-message backlog before deploying, not found by testing: one of g17165's two still-open task assignments ("Reinstall the app APK on tainted VMs ...203.0.113.7") would have reassigned cleanly under the decision above, since its task genuinely wasn't done — but the VM it names had been terminated the same day, unrelated to this fix. Nothing in this daemon can detect that a task's WORLD went stale while the task itself stayed open; the archive-vs-reassign check only ever asks about the task's own status, which is a necessary proxy but not a sufficient one. Rather than special-case that one message, `reconcileStrandedMessage`'s reassignment path now runs every reassigned message through `stampStrandedProvenance`, prepending `"[Reassigned after being stranded since <original send date> — the world may have changed since then...; verify this is still current before acting]"` before the original text — so ANY reassigned message, regardless of age or content, arrives visibly marked as old mail rather than looking freshly written. Idempotent against a repeat sweep re-stamping the same message (checks for the marker prefix first). This is a general answer to a general problem: the daemon cannot know when a task's premises have gone stale, so instead of trying to detect that (impossible from here), it makes the message's own age impossible to miss.

**Deliberately NOT done in this pass, scoped out rather than silently skipped:** a general "reject an explicit `to` at send time if it has never been a valid agent name" guard — the broader version of what would have caught `"claude/setup"` at CREATION time instead of relying on the retroactive sweep to clean it up afterward. `handleSendMessage` already rejects `to=="" && project==""` at the HTTP boundary, but a caller that supplies an explicit (wrong) `to` with a real project currently still gets queued unchecked — `resolveTargetLocked`'s `if to != "" { return to, ... }` does zero validation. Not fixed here because the actual origin of `"claude/setup"` as a value is unconfirmed (unlike the `"orchestrator-"` case, which has a clear, single, now-guarded source), and a blanket "unknown recipient" rejection risks false-positive-blocking any legitimate use of sending to a not-yet-registered concrete name — a pattern this investigation didn't find evidence either for or against. The retroactive sweep (`sweepUnroutableMessagesOnce`) cleans up existing damage of this shape regardless of source; a send-time guard is a separate, narrower-scoped follow-up if the "claude/setup" origin is ever pinned down.

## Host sleep detection & self-heal

A host that sleeps mid-task silently kills every agent's tmux session — and a sleep-death was previously INDISTINGUISHABLE on the board from ordinary bookkeeping drift: a task reads `in-progress`, there's no live worker, and nothing says why. That's worse than losing the work outright, since it corrupts diagnosis (an agent picking it back up can't tell "abandoned" from "actually still running somewhere" from "the host just slept"). `startSleepDetector` closes the gap by giving the cause a name and acting on it, rather than leaving a mysterious hole in the activity feed.

A background goroutine (`startSleepDetector`) records wall-clock time every `sleepCheckInterval` (30s). A sleeping Mac stops scheduling goroutines entirely, so the NEXT tick after waking simply arrives late by however long the sleep lasted — `isHostSleepGap(gap)` treats a gap over `sleepGapThreshold` (3× the interval, 90s) as a detected sleep, not scheduling jitter. On detection: logs `serve: detected host sleep of ~Ns (HH:MM -> HH:MM)`, records `lastSleepAt`/`lastSleepDuration` on the Server (in-memory only, like `stuckTracker` — a fresh daemon restart simply has nothing to report until the next real sleep) for `/status`'s daemon block (`lastSleepAt`/`lastSleepSeconds`, both `omitempty` pointers — absent entirely, not zero, when nothing's been detected yet), and runs `handleHostSleep(sleepStart)`.

`handleHostSleep` finds every agent whose `LastSeenAt` falls BEFORE `sleepStart` (its last poll happened before the host went under — it never got a chance to poll again, during the sleep or since waking) AND is now beyond `aliveWindow`. Each is treated as SLEEP-KILLED, not merely dead: `reconcileSleepKilledAgent` marks it deregistered with the specific cause, and for every in-progress task it owned (`ownedTaskPathsLocked`), appends a note and resets it to `open` via `bridgeTransitionTask` (the same path `transitionMRMerged` uses — which already attributes its note to `"bridge"`, never the dead agent, avoiding the resurrection bug `appendActivityLocked` would otherwise cause by bumping the dead agent's own `LastSeenAt`): *"Worker \<name\> was lost to a host sleep at \<time\> — not abandoned, not drifted. Reset to open for pickup."* A bridge-attributed activity entry is logged too, listing what was recovered (or noting there was nothing in-progress to recover). Idempotent: the check-and-set of `DeregisteredAt` happens under one lock acquisition, so a repeat pass for the same agent (a second sleep-detector tick, or a race with `checkOrphanedDeathsOnce`) is a full no-op — no duplicate note, no duplicate activity entry.

The environmental half (raising this machine's battery idle-sleep timeout, wrapping long agent work in `caffeinate -i`) is out of scope for the daemon — this section is only the board-honesty half.

## Idempotent task creation

`tn create` used to call TaskNotes' `POST /api/tasks` directly (`api.go`'s `Client`, 10s timeout) — a purely local HTTP call, but TaskNotes' own create handler can genuinely be slow enough to trip that timeout even though the create actually lands (confirmed by reading the installed plugin's create-task code path: the file write is followed by an AWAITED `waitForFreshTaskData` cache-readiness retry loop — up to ~2.65s worst case on its own — plus whatever contention Obsidian's single-threaded event loop is under from other concurrent vault activity; webhook DELIVERY itself is NOT the cause — `triggerWebhook` schedules the real HTTP fan-out via `setImmediate` and returns immediately, confirmed not to block the response). The reported live symptom: an agent's `tn create` timed out, retried, and created 2-3 duplicate tasks over ~100 seconds because each retry looked like a fresh, independent request.

Fix: `POST /tasks/create {idempotencyKey?, fields}` (`handleCreateTask`) — the daemon now proxies task creation instead of the CLI calling TaskNotes directly. `tn create` computes `idempotencyKey = sha256Hex(title|claude-project|details)` (stable across retries of the identical command) and calls the bridge FIRST; only on a crisp connection-refused (`*bridgeUnreachableError` — "the daemon genuinely isn't running") does it fall back to the old direct-to-TaskNotes path, with no dedup in that fallback case. Any OTHER bridge-side error (including the daemon's own TaskNotes call failing) is surfaced as-is, deliberately NOT triggering a fallback — an ambiguous failure retrying via a second, independent path would risk exactly the duplicate this exists to prevent.

Server-side: `state.CreatedTasks map[string]createdTaskRecord` (`{Path, Title, CreatedAt}`, persisted). A request with a key already present and younger than `createIdempotencyTTL` (10 minutes) returns that recorded task WITHOUT calling TaskNotes again — a repeat is indistinguishable from a first-time success, since the CLI only ever prints the returned path. No key at all (older binaries, or a caller that doesn't want dedup) skips the whole mechanism, unchanged from before.

**Ambiguous-failure gap, closed (first attempt) — then found to be wrong under a real stall.** If the daemon's own call to TaskNotes times out or the connection drops (`isAmbiguousCreateError`: not a clean `*httpError`, not `*connRefusedError` — genuinely unknown whether the create landed), `handleCreateTask` does a lookup BEFORE any retry rather than risking a second create: `findRecentDuplicateTask(title, claudeProject)` queries TaskNotes for `title is <exact>` (server-side — confirmed live to work) and filters the results client-side for a matching `claude-project` custom property AND a `DateCreated` inside `createIdempotencyTTL`.

The original version of this fix did a SINGLE, IMMEDIATE lookup: found → treat as success; not found → retry once. That is wrong, and it was proven wrong live: during a 2026-08-26 Obsidian hang (renderer blocked, confirmed via `ps aux` showing 0% CPU — genuinely stuck, not overloaded) the flush of a queued write took well over a minute. The immediate lookup ran long before that flush, found nothing, and retried — and then BOTH the original (delayed) write and the retry landed, producing `-2`-suffixed duplicate tasks (`zz-histfix-check2-2`, `zz-history-loss-recheck-2`, `zz-probe-3`, etc.). A client-side timeout tells you nothing about whether the server will eventually complete the write; querying once, immediately, cannot distinguish "not written" from "not-yet-written" — it just moves the race one layer in (CLI→daemon down to daemon→TaskNotes) instead of removing it.

**Fix, corrected: bounded backoff-requery, honest ambiguity report on exhaustion, no idempotency key (TaskNotes doesn't support one).** Three options were on the table: (1) wait and re-query with backoff before deciding, (2) make the create idempotent server-side on a caller-supplied key so a duplicate write is a no-op, (3) never auto-retry, always report the ambiguity and let a human/agent decide. Option 2 was the initial instinct — a server-side idempotency key is the only option correct regardless of timing — but confirmed directly against TaskNotes' own docs (`https://tasknotes.dev/HTTP_API/`, fetched live, not assumed): `POST /api/tasks` accepts `title, details, status, priority, due, scheduled, tags, contexts, projects, recurrence, recurrence_anchor, timeEstimate, reminders, blockedBy` and nothing else — no idempotency key, no client-supplied ID, no dedup mechanism of any kind. Option 2 is not available.

Implemented instead: 1+3 combined, via `findRecentDuplicateTaskWithBackoff` wrapping `findRecentDuplicateTask` on `createAmbiguityBackoffSchedule` (a package `var`, not `const`, so tests can shrink it) — `500ms, 1s, 2s, 4s, 8s, 8s, 8s, 8s`, ~39.5s of sleep across up to 9 lookup attempts. Combined with the ~10s the initial `CreateTask` call can take before it times out ambiguously, the whole handler can take up to ~50s, comfortably inside the 65s `BridgeClient` timeout (`bridge.go`) the CLI allows for the whole `/tasks/create` round trip. If a match appears at any point in the window, it's treated as success (existing behavior: path returned, idempotency mapping recorded, no retry). If the whole window elapses with nothing found, `handleCreateTask` does **not** fall back to a blind retry — that guess is exactly what manufactured the duplicates — it returns `409 Conflict {"error": "...", "ambiguous": true, "title": "..."}` instead, surfacing the unresolved ambiguity to the caller rather than silently guessing. `createTaskIdempotent` (`main.go`) already treats any non-2xx bridge response other than connection-refused as a surfaced error with no fallback retry, so this flows through to the CLI/agent as an honest failure, unchanged code on that side.

Regression coverage: `TestHandleCreateTask_AmbiguousFailureRaceWriteAppearsAfterDelay` reproduces the actual incident shape (lookup finds nothing on the first N queries, then finds the delayed-but-real write) and — confirmed by reverting the fix and rerunning it — fails against the old single-lookup code exactly as the incident did (2 create calls, the retried path returned instead of the original). `TestHandleCreateTask_AmbiguousFailureExhaustsBackoffReportsAmbiguityNoRetry` covers the other half: backoff fully exhausted, no blind retry, honest `409`.

This does NOT use a hidden `customProperty` marker (server-side custom-property filtering was tried and confirmed live to silently match nothing on this TaskNotes version — `customProperties.jira is PROJ-101` against a task that genuinely carries that value returned zero results, the same class of gotcha as the documented `isBlocked` one). Title+claude-project inside the TTL window is the accepted natural key instead — two genuinely distinct tasks sharing both within ~10 minutes is a case worth losing to, and it degrades to an honest-ambiguity report in that rare case (not a silent wrong answer).

**Known pressure source, not yet acted on.** The root-cause finding (an awaited cache-readiness loop inside TaskNotes' own create handler, on Obsidian's single-threaded event loop) implies our OWN polling contributes to the load that causes creates to time out in the first place: the reconciler, due-task scanner, MR watcher, needs-action cache, and status task-count cache each hit TaskNotes on independent timers, and multiple generations poll independently on top of that. If creates start timing out again, the lever is coalescing those passes behind one shared cached read — NOT raising timeouts further, which just makes failures slower to surface without addressing the actual contention.

## New subcommands (bridge client; env `TN_BRIDGE_URL`, default http://localhost:8391)

- `tn serve [--port N]`
- `tn register --name N --project P [--session ID] [--tmux-session S]`
- `tn drain --name N`
- `tn retire --name N`
- `tn inbox --name N [--wait S] [--json]` — one long-poll; default output one line per message: `id<TAB>from-or-project<TAB>taskPath-or--<TAB>text`; empty + exit 0 if none.
- `tn ack --name N --msg ID [--response "text"]`
- `tn send [--to AGENT | --project P] [--task PATH] [--from NAME] [--wait S] TEXT` — with --wait: poll GET /messages?id= every 2s up to S sec for acked, print response; exit 3 on timeout.
- `tn agents` — table: name, project, alive, lastSeen.
- Bridge unreachable → stderr `tn: bridge not reachable at <url> — start it with 'tn serve'`, exit 1.

## LaunchAgent file

Create `~/Library/LaunchAgents/com.example.tn-serve.plist`: Label com.example.tn-serve, ProgramArguments [~/bin/tn, serve], RunAtLoad true, KeepAlive true, StandardOutPath + StandardErrorPath ~/Library/Logs/tn-serve.log. Create file only — do not load it.

## Verification

- gofmt / go vet / go build clean; rebuild `~/bin/tn`.
- serve_test.go: register→alive; send→inbox long-poll delivers→ack stores response; webhook spawn-tag routes; @claude diff routes once per hash change; no-live-agent send → queued warning; startup path uses injected spawnFunc (assert called with right project/cwd, no real tmux).
- `go test ./...` all green incl. existing tests.
- Functional smoke: `tn serve --port 18787` in background; with TN_BRIDGE_URL=http://localhost:18787 run register/send/inbox/ack/agents; verify output; kill daemon.
