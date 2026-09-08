# Lifecycle of a task

A narrative walk of one task through the bridge, from creation in Obsidian to being marked done, tying together mechanisms documented separately elsewhere.

This follows one representative path. Not every task goes through every step below (a task with no dependencies skips the unblock pass, one nobody ever comments on skips the reply loop, and so on), but the order shown is the order these mechanisms fire in when they do apply.

## 1. Creation and routing

A task becomes eligible for an agent when it carries the `claude` or `spawn` tag, or contains an `@claude:` line in its details. Which project it belongs to is resolved by `routingSlugForTask`: the task's `claude-project` custom property if set, otherwise its first linked project, both normalized by `normalizeProjectSlug` (wikilink brackets, path prefix, and a trailing `.md` stripped, then lowercased) so `[[Repos/MyApp]]` and `myapp` resolve to the same slug. `serve.json`'s project keys are matched against that slug case-insensitively.

A tag-based task is only auto-assigned while its status is `open` (`assignableStatuses` in `serve.go`, which also treats an unset/`"none"` status as open). This is a positive allowlist, not a check that merely excludes the parked statuses: a negative check would also let `in-progress` and already-`done` tasks through, and the daemon's own duplicate-assignment guard doesn't save it there, since that guard only blocks while the prior assignment is still unacked, so a later reconciler pass could re-assign a claude-tagged task that's already finished. `triage`, `needs-input`, and `review` are parked for you; moving the task to `open` is your approval to route it. An `@claude:` line, by contrast, routes regardless of status, so you can comment on or answer a parked task without reopening it.

## 2. Webhook to assignment

TaskNotes fires a webhook (`task.created`, `task.updated`, or `task.completed`, mutually exclusive, not variants of each other) to the daemon's `POST /webhooks/tasknotes`. `handleTaskNotesWebhook` decodes the payload and hands it to `routeTaskNotification`, which does the tag/status/dependency checks above and, if the task is startable, creates a `Message` addressed to a project's agent (or its logical queue name, `orchestrator-<slug>`, if none is alive yet) with text prefixed `Task assigned:`.

If a task declares `--blocked-by` on another task, assignment is skipped unless there's fetched evidence it's actually startable (`dependencies.isBlocked` false); a bare webhook payload without that field is treated conservatively as still blocked. Two background passes catch what the webhook alone would miss: a due-task scanner (every 10 minutes, plus once at startup) picks up recurrence/scheduled-date rollovers that never touch the note file and so never fire a webhook, and a webhook-miss reconciler on the same cadence re-routes anything TaskNotes' webhook delivery dropped while the daemon was briefly down.

## 3. Spawn, if nobody's listening

If no agent is alive and accepting for the resolved project, and that project has `autoSpawn: true`, `spawnOrchestrator` calls the injected spawn function to start a new **generation**: a fresh tmux session named `tn-<slug>-g<gen>` running an agent named `orchestrator-<slug>-g<gen>` (`gen` a short timestamp-derived number). `buildLaunchCommand` composes the shell line: any per-project env vars, `TN_HEADLESS=1`, `TN_AGENT_NAME=<name>`, then `claude --permission-mode auto` with a bootstrap prompt built by `buildOrchestratorPrompt`. That prompt tells the session to read `ORCHESTRATOR.md` (the operating contract injected into every daemon-spawned session) and register with the bridge under its exact assigned name and tmux session.

`--permission-mode auto` exists because an unattended session can never answer an interactive prompt: it approves safe tool calls and denies risky ones with an error the session is expected to react to by flipping the task to `needs-input`, per the contract. See [generations and drain](generations-and-drain.md) for what happens when more than one generation is alive for a project at once.

## 4. Inbox, ack, and work

The spawned session calls `tn register --name <agentName> --project <slug> --tmux-session <tmuxSession>`, then long-polls `GET /inbox --wait <seconds>` for queued messages. On delivery a message flips from `queued` to `delivered`; polling itself is the daemon's liveness signal for that agent (`aliveWindow`, 90 seconds; no separate heartbeat call exists). The session acknowledges a message with `POST /ack` (`tn ack --name N --msg ID [--response "text"]`), which is what an orchestrator does immediately on receiving a `Task assigned:` message, establishing it as that task's owner for future task-scoped routing.

For work that needs isolation or parallelism, an orchestrator can dispatch worker subagents (often in their own git worktree under `<repo>/.claude/worktrees/agent-*`) and declare them with `tn worker start --name N --task P` / `tn worker end` purely so `GET /status` can show what's actually in flight. The daemon doesn't schedule or supervise workers itself; that's Claude Code's own Task-tool mechanism. Worktrees left behind after a worker finishes are eventually cleaned up by the [worktree reaper](../how-to/worktree-reaper.md), not by anything in this lifecycle.

## 5. Note updates as the record

As the session works, it writes status back through the same structured note-body commands documented in [`docs/reference/note-layout.md`](../reference/note-layout.md): `tn note` prepends a folded history entry, `tn brief` replaces the "Current" summary block, and `tn ask` sets a "Needs you" block plus flips the task's status to `needs-input` in one call. The task note itself is the durable record of what happened, not the bridge's message log, which is transport, not history.

## 6. Needs-input and the human reply

When a session calls `tn ask`, the task moves to `needs-input` and drops out of auto-assignment (step 1) until you act. Answering happens the same way as any other comment: an `@claude:` line in the task's details, written directly in Obsidian or via the web UI's `POST /reply` (which strips a duplicated `@claude:` prefix if you typed one yourself and writes exactly one). The daemon detects that reply the same way it detects any `@claude:` line (routing it to the task's current owner if one is alive, an alive accepting agent as fallback, or the logical queue otherwise) and, because a reply to a `needs-input` or `review` task is itself the unblock signal, auto-clears the `ask` block and flips the task's status back to `in-progress` (`autoFlipToInProgress`) as part of delivering it, logging a "user replied: ..." history entry in the same write.

## 7. Review, merge, and the MR watcher

For work with an associated GitLab merge request (`customProperties.mr`), a background MR watcher polls `glab` every 5 minutes over non-archived tasks in `review`, `in-progress`, or `open`, reacting only to an **observed change** in the MR's state since the last check:

- **Merged**: a `review` or `in-progress` task auto-transitions straight to `done`, with a bridge-attributed note, then the unblock pass (below) runs immediately for anything that depended on it.
- **Closed without merge**: a `review` task reopens to `in-progress`; either way, the task's owner (or a fallback agent, or the queue) gets an informational message to go investigate.

A task's dependents don't wait for the next 10-minute scan: the moment a task transitions to `done`, an unblock pass queries for open, dependency-bearing tasks that are now startable and routes an assignment for each, suffixed `[unblocked by <completed title>]`.

## 8. Closing the task

`tn done`/`tn status <path> done` route through `POST /tasks/close` rather than writing the status directly. If the task's details carry a `DONE MEANS:` heading followed by a numbered list of criteria, the close is checked against it first: every numbered criterion needs a corresponding `Evidence N:` line somewhere in the task's details, or the close is rejected outright (nothing written, so nothing to revert) with the missing numbers listed. This is a mechanical count, not a quality check: it forces the act of writing down what justifies the close, not a verification that the evidence is actually sufficient. A task with no `DONE MEANS:` block closes immediately; this is opt-in friction, not a universal gate. A human or Obsidian-direct close bypasses this synchronous check entirely (there's no other path for it to go through) and is instead picked up informationally, later, by the same reconciler pass that catches webhook misses, never reverted, just noted.

## 9. Reflected everywhere, throughout

None of the above is visible only in the task note. Every state mutation (register, message queued/delivered/acked, a webhook-driven status change) triggers a debounced (at most once per 2 seconds) re-render of the dashboard note (`Claude Sessions.md`, if `dashboardPath` is configured) and, if `sessionPagesDir` is set, the acting agent's own session page: a merged, newest-first feed of `tn log` entries and message events. The same state backs `GET /status` (JSON, spectator-safe: it never marks a message delivered or bumps an agent's `lastSeenAt` just by being polled) and the embedded web UI at `GET /ui`, so a menu-bar client or a browser tab sees the same picture without touching Obsidian at all.
