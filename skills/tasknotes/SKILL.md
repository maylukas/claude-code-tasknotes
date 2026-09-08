---
name: tasknotes
description: Manage agent work tasks across sessions via Obsidian TaskNotes (tn CLI). Use when starting multi-session work, picking up prior work, tracking progress/blockers, handing off between sessions, or when the user says "track this in tasknotes", "check the task board", "what's open", or asks to create/update/complete agent tasks.
---

# TaskNotes agent task management

Tasks live as Markdown notes in the user's Obsidian vault, managed through the
TaskNotes plugin's HTTP API via the `tn` CLI (source: `/path/to/tasknotes-cli`).

Headless daemon-spawned orchestrator sessions do NOT use this skill — they follow
`/path/to/tasknotes-cli/ORCHESTRATOR.md` per their bootstrap prompt.
This skill is for interactive sessions: answer the user in chat as normal; tasks are
for cross-session tracking, not your output channel.

## Config

- API: `http://localhost:8390` by default; override with `TASKNOTES_URL`.
  Token via `TASKNOTES_TOKEN` or `~/.config/tn/config.json`.
- Set `TASKNOTES_AGENT` (or pass `--by`) so notes are attributed to this agent/session,
  e.g. `TASKNOTES_AGENT="claude/processfit-e2e"`.
- If `tn` reports the API unreachable: ask the user to enable it in
  Obsidian → Settings → TaskNotes → HTTP API (and reload the plugin). Do not retry in a loop.

## Conventions

- **project** = repo or initiative slug (e.g. `processfit`, `hcm-vpn`). Always set it.
- **tag `agent`** marks agent-managed tasks. Always set it on tasks you create.
- Statuses: `open` → `in-progress` → (`needs-input` ⇄ `in-progress`) → `review` → `done`;
  plus `triage` = parked for a human decision; moving `triage` → `open` = approval
  (routes/starts auto-handling for `claude`-tagged tasks). Human-attention statuses:
  `triage`/`needs-input`/`review`.
  `needs-input` = waiting on the user's answer (pair with a `**Question:**` note);
  `review` = work finished and self-verified, human verification pending — the user
  normally moves `review` → `done`. Externally blocked: tag `blocked` + note why.
- `tn update --tag` REPLACES the whole tag array — repeat all tags the task should keep
  (check current ones with `tn get`).
- Task id = vault-relative path as printed by `tn list`/`tn create`. Pass it verbatim
  (quoted — paths contain spaces).

## Commands

```bash
tn list --project <slug> --tag agent      # open work for this project (excludes done/archived)
tn list --project <slug> --tag agent --startable   # same, minus tasks blocked by an incomplete dependency
tn list --project <slug> --all --json     # everything incl. done, machine-readable
tn get "<path>"                           # full task incl. details/notes
tn create --title "..." --claude-project <slug> --project "[[<slug>]]" --tag agent [--details "..."] [--priority high] [--due YYYY-MM-DD] [--blocked-by "<path-or-[[wikilink]]>"]
tn status "<path>" in-progress            # transition
tn done "<path>"                          # complete
tn note "<path>" "text"                   # append to the collapsed History block
tn brief "<path>" "text"                  # REPLACE the Current block (what's true now)
tn ask "<path>" "question"                # Needs-you block + status needs-input
tn update "<path>" --priority high --tag agent --tag blocked   # partial update
tn stats                                  # board summary
```

## Session workflow

1. **Start**: `tn list --project <slug> --tag agent` to load open work. If resuming a task,
   `tn get` it — the details contain the previous session's notes and next steps.
2. **Pick up**: `tn status "<path>" in-progress`, then `tn note` with a one-liner of the plan.
3. **During work**: create tasks for genuinely new, non-trivial work you discover
   (not sub-steps of the current task). Note decisions, findings, and blockers as they happen —
   notes are the cross-session memory.
4. **Waiting on the user**: `tn note` the question, `tn status "<path>" needs-input`.
   Externally blocked: tag `blocked` + note what's needed from whom.
5. **End / handoff**: `tn note` with state, what's verified, and the concrete next step.
   Finished work goes to `review` (user verifies → `done`). Exception: when the task
   belongs to a workstream with an `integration-branch`, verified member work is merged
   into that branch and set `done` directly — the user reviews the consolidated branch,
   not each piece. Escalate to the user only for genuine questions (`tn ask`).

Notes are for durable cross-session state, not a command log. One good handoff note
beats ten progress pings — and keep `tn brief` current, since that is the line the user
actually reads; History is the forensic record behind it.

## Parallel workers on one repo

Spawn each worker with the Agent tool's `isolation: "worktree"` — the harness creates and
locks its worktree before it starts. Workers must NEVER call EnterWorktree themselves or
touch a peer's worktree: concurrent EnterWorktree calls race and git writes land in the
wrong checkout. Worktrees are ephemeral, so a worker commits and pushes before returning.

## Bridge messaging (optional for interactive sessions)

A local daemon (`tn serve`, `http://localhost:8391`, override `TN_BRIDGE_URL`) relays
messages between sessions and from the user's Obsidian (`@claude:` lines in task
details, tag `spawn`/`claude` to assign). Only engage when this session should receive
such messages:

```bash
tn register --name claude/<slug>-<purpose> --project <slug>
tn send [--to AGENT | --project SLUG] [--task "<path>"] [--wait 60] "text"
tn agents                                  # who is alive
tn ack --name <name> --msg <id> --response "..."
```

To receive while working, arm one persistent Monitor (never poll via /loop):
`while true; do tn inbox --name <name> --wait 55 || sleep 5; done` — each event line is
`id<TAB>from<TAB>taskPath<TAB>text`; ack what you act on. Bridge unreachable →
skip messaging silently, task workflow still works.

`tn log --name <name> "..."` at milestones (picked up, spawned a subagent, phase
transition, blocker) — the user reads these on your session page in the vault.

`tn env --project <slug>` lists that project's configured env entries (name +
injected/advisory + description, never a value) — injected ones are already in
`$NAME`; advisory ones tell you how to fetch the material yourself. Never write a
secret's value into a note/log/message/task.
