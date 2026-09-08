# Design history

An index of the historical design documents in this repository, with a note on what each one was for and how current it is.

> **Note:** These are records of reasoning at the time a change was proposed or made, not living documentation. Where a spec and the running code disagree, **the code is correct**: treat every claim below as "true when written," and cross-check anything load-bearing against the source before relying on it. For up-to-date explanations of how the system behaves today, see the other pages under `docs/explanation/`.

## `SPEC-serve.md`

The largest and most central spec: the original design for `tn serve` itself, covering the daemon's state model, HTTP handlers, webhook routing, orchestrator spawning, and every background loop added since (due-task scanning, the MR watcher, Jira transition prompts, stuck-session detection via pane-scraping and later Claude Code hooks, the status endpoint and web UI foundation). It grew incrementally alongside `serve.go` rather than being written once, so later sections (the "Design rule: never act on a single negative sample" section in particular) capture hard-won lessons from real production incidents, not just up-front design. It's the closest thing to a running commentary on the daemon's evolution and is worth reading end to end if you want the *why* behind a mechanism `serve.go`'s comments only briefly gesture at.

## `SPEC-generations.md`

The design for replacing hard orchestrator rotation with overlapping generations and a drain lifecycle: agent naming, the `Accepting` flag, routing changes needed to skip draining agents for new work while still delivering task-scoped messages to a draining owner, and the reconciler behavior that spawns a new generation once only draining agents are left. Condensed and brought current in [`docs/explanation/generations-and-drain.md`](generations-and-drain.md).

## `SPEC-dashboard.md`

The original design for rendering the daemon's state into a live `Claude Sessions.md` note in the vault: what triggers a render, the debounce interval, and the sections the generated note contains (needs-your-action, sessions, pending messages, recent activity). Largely superseded in spirit by `SPEC-ui-v2.md`'s embedded web UI, but the dashboard note itself is still rendered: it's the fallback that works even when Obsidian's own plugin isn't loaded.

## `SPEC-note-layout.md`

The design for the structured, anchored task-note body (ask/brief/description/history, later extended with reply and links blocks) that replaced a flat append-only note. Includes several dated addenda documenting real bugs found after the initial design shipped (a line-anchored `@claude:` extraction bug that truncated multi-line replies, and a `tn update --details` call that was destroying managed blocks it never touched), each with the incident and the fix. See [`docs/reference/note-layout.md`](../reference/note-layout.md) for the current, authoritative format.

## `SPEC-ui-v2.md`

Supersedes `SPEC-obsidian-plugin.md`'s original shape: instead of the Obsidian plugin rendering its own view from polled JSON, the daemon serves a real single-page app at `GET /ui` with assets embedded via `go:embed`, updated live over server-sent events, and the plugin becomes a thin iframe wrapper. This is the shape actually built; see `ARCHITECTURE.md`'s section on `webui/` and `obsidian-plugin/`.

## `SPEC-obsidian-plugin.md`

The original Obsidian plugin design: a polling `ItemView` that mutated the DOM in place to avoid the dashboard note's whole-note-rewrite scroll-jump problem. Superseded by `SPEC-ui-v2.md` before this approach was built out. Read it for the reasoning behind why a plugin-owned renderer was rejected in favor of the daemon owning the UI, not as a description of the current plugin.

## `IMPLEMENTATION-PLAN-REMOTE-EXECUTION.md`

**An unimplemented proposal**, not a description of anything running today. It sketches evolving `tn serve` into an "edge daemon" that synchronizes with a remote coordinator for multi-machine work assignment, offline-first operation, and shared Obsidian state across machines: a substantially larger scope than anything currently in `serve.go`. Useful as a record of a considered direction and its design constraints (invariants, persistence model, synchronization protocol, migration plan) if that work is ever picked up, but none of it should be assumed to exist in the current daemon.

## `ORCHESTRATOR.md`

Not a design document: this is the **live operating contract** injected as the bootstrap prompt into every daemon-spawned orchestrator session (`buildOrchestratorPrompt` tells a spawned session to read it and follow it exactly). It covers headless-output conventions, message-handling discipline, generations and drain from the orchestrator's own side, worktree isolation rules, and worker-declaration requirements. Changes to it need to stay in sync with `serve.go`'s `buildOrchestratorPrompt`/`buildLaunchCommand`, their tests, and `skills/tasknotes/`; see `CLAUDE.md`'s "Conventions & gotchas."

## `CLAUDE.md`

The maintainer-and-agent-facing operating notes for this repository itself: build and test commands, the file map, runtime layout for the machine the daemon runs on, and a growing set of "Conventions & gotchas" entries, each one a short incident writeup plus the rule it produced (the web UI stale-bundle footgun, published-interface renames, daemon-deploy authorization, verifying a deploy actually took effect). It's process documentation for working on this codebase, not end-user documentation, but the incidents it records are the same ones several `docs/explanation/` pages and this file draw on.
