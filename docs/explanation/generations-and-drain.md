# Generations and drain

Why orchestrator sessions are replaced the way they are, and what that means when you're watching the dashboard or `tn agents`.

## The problem with hard rotation

A Claude Code session has a finite context window. Left running indefinitely, an orchestrator eventually runs low on it mid-task, in the middle of owning work nobody else knows about. The naive fix (kill it and start a replacement at some threshold) creates a gap: whatever it was mid-way through has no owner until the new session registers, catches up, and re-derives what's still outstanding.

`tn serve` avoids the gap by never doing a hard cutover. Instead, an orchestrator that's used up enough of its budget stops accepting *new* work but keeps finishing what it already owns, while a fresh session takes over anything new. Rolling replacement, not a restart.

## Generations

Each spawn of an orchestrator for a project is a new **generation**: agent name `orchestrator-<slug>-g<gen>`, tmux session `tn-<slug>-g<gen>`, where `gen` is a short timestamp-derived number. The bootstrap prompt carries both identifiers literally, so the session registers under the exact identity the daemon is already expecting for it (`tn register --name <agentName> --project <slug> --tmux-session <tmuxSession>`).

An `Agent` record carries an `Accepting` flag, true from registration (a re-register resets it to true) until the agent stops taking new assignments. It can go false two ways:

- **Voluntarily**: the session calls `tn drain --name N` itself (idempotent), typically when its own context-window indicator crosses a threshold it's told to watch for in `ORCHESTRATOR.md`.
- **Automatically**: the user's statusline wrapper reports context usage on every render via `POST /agents/context {name, usedPct}`; once a still-accepting agent's reported usage crosses `drainContextPct` (40%), the daemon flips it to draining itself and queues it a one-shot informational message telling it to `tn retire` once its remaining work is settled.

A draining agent keeps polling and acking normally, and a task-scoped `@claude:` reply still routes to its owner even while draining; it just receives no *new* task assignments. Once a draining generation has finished everything it owns, `tn retire --name N` is its last command: the daemon confirms no pending messages remain, marks it deregistered, and kills its tmux session a couple of seconds later (deliberately after the retire call's own HTTP response goes out, not before).

A project with only draining generations counts as having no live agent for spawn purposes, so a fresh accepting generation starts alongside the old one with no gap; it's expected and normal for two generations to overlap for hours. `tn agents` / `GET /agents` show each agent's `accepting` flag, its reported `tmuxSession`, and `contextPct` when known; the dashboard and session pages render this as a status precedence of rate-limited > gone > stuck > unacked backlog > draining > alive (`sessionStatusDisplay` in `serve.go`). Rate-limited deliberately outranks even gone: a session parked on a usage limit stops polling its inbox, so by definition it reads as not-alive within the liveness window, and showing that as plain "gone" would hide the one fact that actually explains it, that it's expected to resume on its own once the limit resets (see `SPEC-serve.md`'s usage-limit handling).

## Reassignment, not silent loss

A generation that dies without cleanly retiring (a crash, a killed pane) can leave messages addressed to it. This isn't handled by an independent periodic sweep; it's triggered only once an agent's death has been *confirmed*, via `reassignStrandedMessagesForDeadAgent`, called from the same orphan-check path that confirms the death in the first place (see the two-sample rule below: there's no separate liveness judgment made here). A pending message whose referenced task is already done is archived with a reason recorded, rather than redelivered as if new: dumping a stale `Task assigned:` directive on a live orchestrator for work that's already finished would recreate real duplicate-work incidents this mechanism exists specifically to prevent. Everything else gets reassigned to whatever's now the right recipient for that project.

## Scale-out: more than one accepting generation

By default a project has at most one *accepting* generation at a time; overlap during drain still happens, but the reconciler won't scale out beyond one accepting session. This cap is tunable per repo, live, via that repo's vault note (`Repos/<slug>.md` frontmatter, read with a 60-second cache so an edit in Obsidian takes effect without a daemon restart):

```yaml
---
tags:
  - repo
repo-path: /path/to/repo
max-orchestrators: 2   # cap on concurrently accepting generations (default 1)
max-workers: 4         # TN_MAX_WORKERS override for spawned sessions (default unset)
---
```

With `max-orchestrators` above 1, a project below its cap gets an additional generation spawned once its startable (open, non-archived, `claude`-tagged, unblocked) task backlog exceeds `acceptingCount * effective max-workers` (i.e. the existing accepting generations look saturated; `max-workers` unset falls back to a default of 3, matching `ORCHESTRATOR.md`'s documented `$TN_MAX_WORKERS` default). The reconciler also spawns on startable work alone even with an empty message queue, closing a deadlock that used to be possible when a draining orchestrator left an idle-but-nonempty open queue with nothing to nudge it. With more than one accepting agent for a project, a new assignment goes to whichever owns the fewest tasks (ties broken lexicographically) rather than always the lexicographically-first agent: basic load balancing across generations, not true scheduling.

## Design rule: never act on a single negative sample

Every liveness or drain-adjacent mechanism in this codebase converges on the same rule, arrived at independently three times before it was written down once: **a single missed signal is not evidence of absence, it's the absence of a signal.** An agent that hasn't polled in 90 seconds might be doing real synchronous work, not dead. A tmux pane that doesn't match a stuck-prompt marker this tick might have just repainted, not cleared. Concretely, this means requiring the same signature on two consecutive checks before declaring something dead or stuck, or re-verifying immediately before an irreversible action rather than trusting a stale read (the stuck-approval dialog re-captures the pane right before it sends a keystroke). The corollary that came out of the same incidents: a field's name is part of its contract. A correctly-computed measurement misnamed to imply a conclusion it doesn't support gets misread and acted on as if it were that conclusion, which is exactly as costly as computing it wrong in the first place.

## Related

- [`docs/explanation/lifecycle-of-a-task.md`](lifecycle-of-a-task.md): where spawning and message routing fit into a task's full path.
- `SPEC-generations.md`: the original design spec this explanation is condensed from.
- `SPEC-serve.md`'s "Design rule: never act on a single negative sample" section: the fuller incident history behind the rule above.
