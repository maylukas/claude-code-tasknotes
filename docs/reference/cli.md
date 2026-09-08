# CLI reference

Exhaustive reference for the `tn` command-line tool, covering every subcommand, flag, and exit code.

`tn` is a single binary with two roles: a client for the TaskNotes HTTP API (task CRUD) and a client for the `tn serve` bridge daemon (agent registry, messaging, spawn control). Which role a command uses is not always obvious from its name; see the notes on each command below.

## Configuration

`tn` resolves the TaskNotes API connection in this order (first match wins):

1. Environment variables `TASKNOTES_URL` and `TASKNOTES_TOKEN`.
2. `~/.config/tn/config.json`:
   ```json
   { "url": "http://localhost:8390", "token": "your-token" }
   ```
3. Defaults: `http://localhost:8390`, no token.

Bridge commands (everything from `tn serve` down) resolve the daemon's base URL separately:

1. Environment variable `TN_BRIDGE_URL`.
2. Default `http://localhost:8391`.

`tn serve`'s own listen port follows a different precedence; see [configuration.md](configuration.md#ports).

See [configuration.md](configuration.md) for the full config file schema, and [../explanation/security-model.md](../explanation/security-model.md) for why neither endpoint requires authentication by default.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | Success. |
| `1` | Default failure: a non-2xx HTTP response, a connection failure (TaskNotes API or bridge unreachable), a usage error, or any other error. The message is printed to stderr prefixed `tn:`. |
| `3` | `tn send --wait` timed out waiting for an ack. Only this one command produces a non-1 failure exit code. |

`tn help` (or no arguments, or `-h`/`--help`) prints the built-in usage text and exits `0`.

## `--json`

Most read commands (`list`, `get`, `agents`, `env`, `repo`, `inbox`, `msg`) accept `--json` to print the raw API response instead of a formatted table. `health`, `stats`, `register`, `drain`, `retire`, `pause`, and `resume` always print raw JSON: there is no non-JSON mode for them.

---

## Task commands

These talk to the TaskNotes HTTP API (`internal/tn/api.go`), mostly via a GET-then-PUT round trip so that structured note blocks (history, ask, brief, links) are preserved on write. See [note-layout.md](note-layout.md) for the full body-block format.

### `tn health`

**Synopsis:** `tn health`

**Description:** Calls `GET /api/health` on the TaskNotes API and prints the raw JSON response. Useful to confirm the TaskNotes plugin's HTTP API is enabled and reachable before running anything else.

**Options:** none.

**Example:**
```bash
$ tn health
{
  "status": "ok"
}
```

### `tn list`

**Synopsis:** `tn list [--status v] [--project v] [--tag v] [--context v] [--all] [--startable] [--json]`

**Description:** Queries TaskNotes for tasks matching the given filters (`POST /api/tasks/query`) and prints one line per task: path, status, priority, due date, title. By default, archived and completed tasks are excluded.

**Options:**

| Flag | Type | Default | Description |
|---|---|---|---|
| `--status` | string | `""` | Filter to an exact status value. |
| `--project` | string | `""` | Filter to tasks whose `projects` array contains this value. |
| `--tag` | string | `""` | Filter to tasks whose `tags` array contains this value. |
| `--context` | string | `""` | Filter to tasks whose `contexts` array contains this value. |
| `--all` | bool | `false` | Include archived and completed tasks (normally excluded). |
| `--startable` | bool | `false` | Exclude tasks blocked by an incomplete dependency (`dependencies.isBlocked is-not-checked`). |
| `--json` | bool | `false` | Print the raw task array as JSON instead of a table. |

**Example:**
```bash
$ tn list --project myapp --status in-progress
tasks/fix-login-bug.md	in-progress	high	2026-09-10	Fix login bug
```

**Notes:** A plain top-level `isBlocked` property does not work as a filter against the TaskNotes API. `--startable` uses `dependencies.isBlocked` instead, verified against a live instance.

### `tn get <path>`

**Synopsis:** `tn get <path> [--json] [--history N]`

**Description:** Fetches one task (`GET /api/tasks/:id`) and prints its fields, structured note blocks (ask/reply/links/brief, if present), description, and recent history entries.

**Options:**

| Flag | Type | Default | Description |
|---|---|---|---|
| `--json` | bool | `false` | Print the raw task JSON. |
| `--history` | int | `5` | Number of most-recent history entries to print. `0` prints all of them. |

Flags may appear after `<path>` on the command line.

**Example:**
```bash
$ tn get "tasks/fix-login-bug.md"
title: Fix login bug
status: in-progress
priority: high
due: 2026-09-10
...

Login form throws a 500 on empty password.

history (2 of 7):

2026-09-02 10:14 (worker-1): reproduced locally, root cause is a nil check
...
… (5 more, see the note in Obsidian)
```

**Notes:** History is printed by default, not just under `--json`. An agent that writes a note and reads the task back needs to actually see it landed, since a note that silently failed to write is otherwise indistinguishable from one that succeeded.

### `tn create`

**Synopsis:**
```
tn create --title T [--details D] [--status S] [--priority P] [--due YYYY-MM-DD]
          [--scheduled YYYY-MM-DD] [--project P]... [--tag T]... [--context C]...
          [--claude-project SLUG] [--mr URL] [--integration-branch B]
          [--integration-role owner] [--jira K[,K2]] [--jira-nc K]
          [--from PARENT-PATH] [--blocked-by PATH]...
```

**Description:** Creates a new task. Routes through the bridge daemon's `POST /tasks/create` when it's reachable, so a client-side retry after a timeout is deduplicated by an idempotency key derived from title + `claude-project` + details; falls back to calling the TaskNotes API directly (no dedup) only when the bridge is unreachable.

**Options:**

| Flag | Type | Default | Description |
|---|---|---|---|
| `--title` | string | *(required)* | Task title. |
| `--details` | string | `""` | Task details / description. |
| `--status` | string | `""` | Task status. |
| `--priority` | string | `""` | Task priority. |
| `--due` | string | `""` | Due date, `YYYY-MM-DD`. |
| `--scheduled` | string | `""` | Scheduled date, `YYYY-MM-DD`. |
| `--project` | string, repeatable | none | Project(s) to attach. |
| `--tag` | string, repeatable | none | Tag(s) to attach. |
| `--context` | string, repeatable | none | Context(s) to attach. |
| `--claude-project` | string | `""` | Bridge routing slug: sets `customProperties.claude-project`. |
| `--mr` | string | `""` | Merge/pull request URL: sets `customProperties.mr` and seeds the links block with an "MR" entry. |
| `--integration-branch` | string | `""` | Target branch for this workstream: sets `customProperties.integration-branch`. |
| `--integration-role` | string | `""` | `owner` marks this task as holding the consolidated MR for a workstream. Never inherited via `--from`. |
| `--jira` | string | `""` | Comma-separated Jira issue key(s); normal transition policy applies. |
| `--jira-nc` | string | `""` | A Jira issue that must never be auto-transitioned by the bridge (comment-only, e.g. a regulated nonconformity record). |
| `--from` | string | `""` | Parent task path. Inherits `claude-project` and `integration-branch` from the parent (only for flags not given explicitly on this command). |
| `--blocked-by` | string, repeatable | none | Path or `[[wikilink]]` of a task this one depends on. |

**Example:**
```bash
$ tn create --title "Fix login bug" --project myapp --claude-project myapp --priority high
tasks/fix-login-bug.md
```

**Notes:**
- `--integration-role owner` is deliberately never inherited via `--from`: a child task must never silently inherit ownership of its parent's consolidated MR.
- `--from` fails loudly (no task created) if the parent task can't be fetched, rather than silently creating an un-inherited task.
- The idempotency key is derived only from title, `claude-project`, and details: a retry with the exact same values dedupes; a retry with any of those changed creates a new task.

### `tn update <path>`

**Synopsis:**
```
tn update <path> [--title T] [--details D] [--status S] [--priority P]
          [--due YYYY-MM-DD] [--scheduled YYYY-MM-DD]
          [--project P]... [--tag T]... [--context C]... [--claude-project SLUG]
          [--mr URL] [--integration-branch B] [--integration-role owner]
          [--jira K[,K2]] [--jira-nc K] [--blocked-by PATH]...
          [--replace-history]
```

**Description:** Partial update: only the flags explicitly passed are sent (`PUT /api/tasks/:id`). Flags may appear after `<path>`.

**Options:** same flags as `tn create` (minus `--title`'s required-ness and `--from`), plus:

| Flag | Type | Default | Description |
|---|---|---|---|
| `--replace-history` | bool | `false` | Required to let `--details` replace the *entire* details field, including the managed history/ask/reply/links/brief blocks. Without it, `--details` only replaces the free-form description, leaving those blocks untouched. |

**Example:**
```bash
$ tn update tasks/fix-login-bug.md --status review --tag reviewed
```

> **Warning:** `--project`, `--tag`, `--context`, and `--blocked-by` **replace** the entire array on the task; they do not merge with the existing values. Pass every value you want to keep.

**Notes:**
- `--details` without `--replace-history` performs a GET-then-PUT that only rewrites the Description block, preserving history/ask/reply/links/brief. This closed a real incident where a task's entire multi-hundred-entry history was destroyed by a naive wholesale-replace write.
- `--replace-history` prints a warning stating exactly how many history entries the write is about to remove.
- Even without `--replace-history`, an update is refused (no write performed) if the existing body can't be safely parsed or migrated AND looks like it has history content. The error message names the exact recovery options.
- `--mr` seeds/updates the links block's "MR" entry on top of whatever `--details` produced (or a fresh fetch, if `--details` wasn't given).

### `tn status <path> <value>`

**Synopsis:** `tn status <path> <value>`

**Description:** Sets the task's status field directly. If `value` is `"done"`, this is equivalent to `tn done <path>` (routed through the bridge's DONE MEANS check instead of a raw write).

**Example:**
```bash
$ tn status tasks/fix-login-bug.md review
```

### `tn done <path>`

**Synopsis:** `tn done <path>`

**Description:** Sets status to `"done"`. Routes through the bridge daemon's `POST /tasks/close` (not a direct write): if the task has a "DONE MEANS" criteria block, the daemon checks it against Evidence lines already in the task's details *before* writing; a failing check means the status write never happens at all. Falls back to a direct write only if the bridge is genuinely unreachable, and in that case appends a history note saying the check did not run (never silently skips it).

**Status codes surfaced to the user:**
- A rejection (criteria unmet) prints the daemon's explanation and exits `1`; the status is *not* changed.
- A successful close exits `0` silently.

**Example:**
```bash
$ tn done tasks/fix-login-bug.md
tn: close rejected — DONE MEANS criteria unmet: missing evidence for "tests pass"
```

### `tn note <path> <text>`

**Synopsis:** `tn note <path> <text> [--by NAME]`

**Description:** Prepends a new entry to the task's folded history block (GET-then-PUT). `--by` may appear before or after the positional arguments.

**Options:**

| Flag | Type | Default | Description |
|---|---|---|---|
| `--by` | string | `$TASKNOTES_AGENT`, else `"claude"` | Attribution name for the history entry. |

**Example:**
```bash
$ tn note tasks/fix-login-bug.md "Reproduced locally, root cause is a nil check" --by worker-1
```

**Notes:** `<text>` is validated: it must be non-empty after trimming and must not itself look like a flag (start with `--`). This catches a real incident where a missing text argument caused `"--by"` itself to be stored as the note body.

### `tn brief <path> <text>`

**Synopsis:** `tn brief <path> <text>`

**Description:** Replaces the task's "Current" (brief) block outright, creating it if absent.

**Example:**
```bash
$ tn brief tasks/fix-login-bug.md "Waiting on a code review from teammate."
```

### `tn ask <path> <text>` / `tn ask --clear <path>`

**Synopsis:**
```
tn ask <path> <text>
tn ask --clear <path>
```

**Description:** Sets the "Needs you" block and status to `needs-input` in one write. If the task has no reply block yet, one is seeded with a default template, but an existing reply block is never overwritten (the user may already be mid-reply). `--clear` removes the ask block only; it leaves the reply block and status untouched.

**Options:**

| Flag | Type | Default | Description |
|---|---|---|---|
| `--clear` | bool | `false` | Remove the ask block instead of setting one. |

**Example:**
```bash
$ tn ask tasks/deploy-plan.md "Which environment should this ship to first?"
$ tn ask --clear tasks/deploy-plan.md
```

### `tn link <path> <url>`

**Synopsis:** `tn link <path> <url> [--label L]`

**Description:** Appends a URL to the task's links block, creating the block if absent. A second call for the same URL updates its label in place rather than adding a duplicate.

**Options:**

| Flag | Type | Default | Description |
|---|---|---|---|
| `--label` | string | `""` | Display label for the link. May appear before or after the positional arguments. |

**Example:**
```bash
$ tn link tasks/fix-login-bug.md https://github.com/example/myapp/pull/42 --label "PR #42"
```

### `tn stats`

**Synopsis:** `tn stats`

**Description:** Calls `GET /api/stats` and prints the raw JSON response.

---

## Bridge commands

These talk to the `tn serve` daemon (`internal/tn/bridge.go`), not the TaskNotes API directly. Most fail with a clear "bridge not reachable" message if the daemon isn't running.

### `tn serve`

**Synopsis:** `tn serve [--port N]`

**Description:** Runs the bridge daemon in the foreground: agent registry, inbox message bus, TaskNotes webhook receiver, tmux orchestrator spawning, dashboard/session-page rendering, stuck-prompt detection, MR watcher, due-task scanner, reconciler, credential auto-swap, worktree reaper, and the embedded web UI. See [../reference/http-api.md](http-api.md) for everything it exposes and [configuration.md](configuration.md) for `serve.json`.

**Options:**

| Flag | Type | Default | Description |
|---|---|---|---|
| `--port` | int | `8391` (see precedence in configuration.md) | Listen port. Highest-precedence override. |

### `tn register`

**Synopsis:** `tn register --name N --project P [--session ID] [--tmux-session S]`

**Description:** Registers (or revives) an agent session with the daemon. Prints the raw JSON `{"agent": ...}` response.

**Options:**

| Flag | Type | Default | Description |
|---|---|---|---|
| `--name` | string | *(required)* | Agent name: the registry key. |
| `--project` | string | *(required)* | Project slug this agent serves. |
| `--session` | string | `""` | Claude Code session ID. |
| `--tmux-session` | string | `""` | tmux session name the agent is running in. |

### `tn drain`

**Synopsis:** `tn drain --name N`

**Description:** Marks the agent as no longer accepting new task assignments. Idempotent. The agent should keep answering task-scoped messages and acking until it retires. See [../explanation/generations-and-drain.md](../explanation/generations-and-drain.md).

### `tn retire`

**Synopsis:** `tn retire --name N`

**Description:** An agent's final command: the daemon verifies no pending (unacked) messages remain, marks it deregistered, and kills its tmux session shortly after responding. Fails if pending messages remain: ack everything first.

**Notes:** Refuses (non-zero exit, message printed) if the agent still has queued or delivered messages it hasn't acked.

### `tn pause` / `tn resume`

**Synopsis:**
```
tn pause
tn resume
```

**Description:** `tn pause` stops the daemon from spawning any new orchestrator generations, project-wide, until `tn resume` is run. Everything else (routing, drain, retire, MR watcher, stuck-prompt dialogs) keeps working normally.

### `tn inbox`

**Synopsis:** `tn inbox --name N [--wait S] [--json]`

**Description:** Long-polls the daemon for queued messages addressed to `name`. Each line is `id`, `from`-or-`project`, `taskPath` (or `-`), and a *summarized* text (long text is cut and marked with a pointer to `tn msg <id>` for the full text).

**Options:**

| Flag | Type | Default | Description |
|---|---|---|---|
| `--name` | string | *(required)* | Agent name to poll for. |
| `--wait` | int | `25` (server default; max `55`) | Long-poll wait budget, seconds. |
| `--json` | bool | `false` | Print the raw JSON response (full, untruncated text). |

**Example:**
```bash
$ tn inbox --name orchestrator-myapp-g3 --wait 30
msg-042	team-lead	tasks/fix-login-bug.md	Can you also check the signup flow for the same…(210 more chars, run `tn msg msg-042`)
```

**Notes:** Message text is summarized to 120 runes per line by design: `tn inbox` is consumed through a persistent long-poll loop where each line is a *notification*, not a data channel; an unbounded line risks silent mid-sentence truncation upstream. Use `tn msg <id>` for the full text.

### `tn msg <id>`

**Synopsis:** `tn msg [--json] ID`

**Description:** Prints one message's full, untruncated fields (the companion to `tn inbox`'s summary line).

**Options:**

| Flag | Type | Default | Description |
|---|---|---|---|
| `--json` | bool | `false` | Print raw JSON. |

**Example:**
```bash
$ tn msg msg-042
id: msg-042
from: team-lead
to: orchestrator-myapp-g3
taskPath: tasks/fix-login-bug.md
status: delivered
text: Can you also check the signup flow for the same issue?
```

### `tn ack`

**Synopsis:** `tn ack --name N --msg ID [--response "text"]`

**Description:** Acknowledges a message, optionally with a response. If the message is task-scoped and a response is given, it's also appended to the task's history (deduplicated against an identical `tn note`).

**Options:**

| Flag | Type | Default | Description |
|---|---|---|---|
| `--name` | string | *(required)* | Acking agent's name. |
| `--msg` | string | *(required)* | Message ID. |
| `--response` | string | `""` | Response text. |

### `tn worker start` / `tn worker end`

**Synopsis:** `tn worker start|end --name N --task P`

**Description:** An orchestrator declaring (or retracting) a worker subtask, purely for `/status` visibility; the daemon has no other way to see a worker subagent. `start` is idempotent: a repeat call for the same `(name, task)` just refreshes the start time.

**Options:**

| Flag | Type | Default | Description |
|---|---|---|---|
| `--name` | string | *(required)* | Orchestrator's own agent name. |
| `--task` | string | *(required)* | Worker's task path. |

### `tn send`

**Synopsis:** `tn send [--to AGENT | --project P] [--task PATH] [--from NAME] [--wait S] TEXT`

**Description:** Sends a message via the bridge. Exactly one of `--to` or `--project` should generally be given: `--to` routes directly to a named agent; `--project` lets the daemon pick a live accepting agent (or the task's current owner, or queue/auto-spawn if none is live).

**Options:**

| Flag | Type | Default | Description |
|---|---|---|---|
| `--to` | string | `""` | Target agent name. |
| `--project` | string | `""` | Target project slug (routing fallback if `--to` is empty). |
| `--task` | string | `""` | Associates the message with a task path. |
| `--from` | string | `""` | Sender name. |
| `--wait` | int | `0` (don't wait) | Poll for an ack up to this many seconds. |

**Example:**
```bash
$ tn send --project myapp --task tasks/fix-login-bug.md "please prioritize this"
msg-043
$ tn send --to orchestrator-myapp-g3 --wait 30 "status?"
msg-044
please prioritize this, already on it
```

**Notes:** With `--wait > 0` and no ack within the deadline, `tn send` exits with code `3` and the message "timed out waiting for ack" (the only command with a non-1, non-0 exit code).

### `tn agents`

**Synopsis:** `tn agents [--json]`

**Description:** Lists registered agents: name, project, alive (boolean), an accepting/draining indicator (with context-usage percentage if reported), and last-seen time.

### `tn log`

**Synopsis:** `tn log --name N "text"`

**Description:** Appends a free-text entry to the named agent's activity log (visible in the dashboard/feed).

### `tn env`

**Synopsis:** `tn env [--project P] [--json]`

**Description:** Lists configured per-project environment entries: project, name, `injected`/`advisory` status, and description. Values are never printed; see [configuration.md](configuration.md#env-entries).

**Options:**

| Flag | Type | Default | Description |
|---|---|---|---|
| `--project` | string | `""` (all projects) | Limit to one project slug. |
| `--json` | bool | `false` | Print raw JSON. |

### `tn repo <slug>`

**Synopsis:** `tn repo <slug> [--json]`

**Description:** Prints the daemon's resolved view of a project's repo-note settings (`max-orchestrators`, `max-workers`) plus its configured `cwd`/`autoSpawn`, surfacing state the daemon already holds rather than requiring an agent to parse vault frontmatter itself (which it may not even have filesystem access to, under TCC). Distinguishes an explicitly-set value from a fallback/default via `*Set` fields.

**Example:**
```bash
$ tn repo myapp
slug:            myapp
repoPath:        /path/to/myapp
autoSpawn:       true
maxOrchestrators: 1 (unset, default)
maxWorkers:      3 (set by repo note)
```

---

## Maintenance commands

### `tn creds`

**Synopsis:** `tn creds save|list|use|rm [label]`

**Description:** Manages claude.ai credential profiles, backed by the macOS Keychain. Lets you park multiple accounts and swap between them (e.g. when one hits a usage limit).

| Subcommand | Description |
|---|---|
| `tn creds save <label>` | Captures the *current* `/login` session's credentials as a new profile named `<label>`. That profile becomes active. |
| `tn creds list` | Lists saved profiles: label, email, rate-limit cooldown ("limited until"), and last-used time. `*` marks the currently active profile. |
| `tn creds use <label>` | Swaps every Claude Code session on the machine to `<label>`. Prefers the running daemon (`POST /creds/swap`, which also nudges any parked rate-limited panes); falls back to a direct Keychain swap if the daemon isn't running (panes are then *not* nudged). |
| `tn creds rm <label>` | Removes a saved profile. |

**Example:**
```bash
$ tn creds save work
saved profile "work" (you@example.com) — now the active profile
$ tn creds use personal
swapped credentials work → personal via daemon; 2 parked pane(s) nudged
note: every Claude session on this machine now uses this account (Remote Control sessions disconnect).
```

> **Warning:** Swapping credentials affects *every* Claude Code session on the machine at once: there is one shared Keychain item, not one per session.

**Notes:** macOS-only (Keychain via `/usr/bin/security`). See [configuration.md](configuration.md#credentials) for the `credentials.autoSwap` daemon setting that does this automatically on a detected usage-limit hit.

### `tn worktrees`

**Synopsis:** `tn worktrees [--project P] [--apply] [--json]`

**Description:** Reports (and, with `--apply`, removes) finished agent worktrees under `<repo>/.claude/worktrees` via the daemon's worktree reaper. Without `--apply`, this is always a dry run; actual removal requires the flag explicitly, regardless of the daemon's own background `worktrees.dryRun` setting.

**Options:**

| Flag | Type | Default | Description |
|---|---|---|---|
| `--project`, `-p` | string | `""` (all configured projects) | Limit to one project slug. |
| `--apply` | bool | `false` | Actually remove worktrees classified as removable, instead of only reporting. |
| `--json` | bool | `false` | Print the raw JSON report. |

**Example:**
```bash
$ tn worktrees --project myapp --apply
myapp (APPLY) — /path/to/myapp
  scanned 8 in 12s (14:03): removed 3, removable 0, pruned 1, locked 0, young 2, dirty 1, unpushed 1, skipped 0, errors 0 (default main)
  removed     wt-worker-3                 fix-signup-flow                              6h merged  merged into main
```

**Notes:** The CLI POSTs `/worktrees/reap` to start a pass, then polls `GET /worktrees` every 2 seconds until it's no longer running (a full pass over a large repo can take minutes). If a pass is already running, it prints a notice and waits rather than failing. See [../how-to/worktree-reaper.md](../how-to/worktree-reaper.md).
