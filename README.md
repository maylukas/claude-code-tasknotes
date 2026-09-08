# claude-code-tasknotes

A CLI (`tn`) and local bridge daemon that turn Obsidian TaskNotes into a task board for Claude Code agent sessions.

<!-- badges: build, release, license -->

This repository (Go module `tasknotes-cli`) builds a single binary, `tn`. Used alone, it's a CLI wrapper around the [TaskNotes](https://github.com/callumalpass/tasknotes) Obsidian plugin's HTTP API for creating, listing, and updating tasks. Run as `tn serve`, it also becomes a local daemon that watches your task board, spawns and coordinates Claude Code agent sessions to work through it, and reports status back into Obsidian. It's for people who already track work in TaskNotes and want Claude Code sessions to pick up tasks, report progress, and hand back questions without leaving that workflow.

## Table of contents

- [Why](#why)
- [How it works](#how-it-works)
- [Requirements](#requirements)
- [Install](#install)
- [Quickstart](#quickstart)
- [Features](#features)
- [Documentation](#documentation)
- [Status and versioning](#status-and-versioning)
- [Contributing](#contributing)
- [Security](#security)
- [License](#license)

## Why

- **TaskNotes is already the task board.** Rather than build a new tracker for agent work, `tn` reads and writes the same task notes you already use in Obsidian.
- **Claude Code sessions need somewhere to report progress and ask questions.** A task note becomes the shared surface: an agent's current status, its history, and anything it needs from you all live in one place you can read and edit like any other note.
- **Long-running agent work outlives a single chat.** `tn serve` keeps a registry of agent sessions and a message queue, so a session can be recreated, replaced, or handed off without losing state.
- **Assigning work should not require babysitting a terminal.** Tag a task, and the daemon can route it to a running agent, or start one in tmux if none is running.
- **You should be able to see what's happening from Obsidian.** A generated dashboard note, per-session pages, and an embedded web UI all reflect live daemon state without extra tooling.

## How it works

```text
 Obsidian ─────────────────────────────────────────────────────────────────┐
   │  TaskNotes plugin (HTTP API on :8390)                                 │
   │      ▲                     │ webhooks (task created/updated/completed)│
   │      │ CRUD                ▼                                         │
   │   tn (CLI) ───────────► tn serve (bridge daemon, :8391) ◄──────────┐  │
   │                              │                                     │  │
   │                              │ spawns / routes messages            │  │
   │                              ▼                                     │  │
   │                       tmux: Claude Code orchestrator session(s)    │  │
   │                       (register, inbox long-poll, ack, send)  ─────┘  │
   │                              │                                        │
   │                              ▼                                        │
   │        dashboard note + per-session pages, written back into vault    │
   └──────────────────────────────────────────────────────────────────────┘

 Also served by tn serve:
   GET /ui    self-contained web UI (SSE live updates)
   GET /status  JSON status for a menu-bar/tray client
   obsidian-plugin/  thin iframe wrapper embedding /ui inside Obsidian
```

`tn` talks to the TaskNotes HTTP API for everyday task CRUD: creating tasks, listing them, updating status, and writing structured note content. `tn serve` adds a second role: it receives TaskNotes webhooks, keeps a registry of Claude Code agent sessions per project, and routes messages to them through a long-polled inbox. When a task is tagged for an agent and no session is running, the daemon can start one in a detached tmux session running Claude Code with a fixed operating contract. As sessions work, they register, poll for messages, acknowledge them, and write status back to the task note; the daemon mirrors all of this into a dashboard note, optional per-session pages, and its own web UI.

A few things follow from that shape, worth knowing up front:

- **Routing is per task, not per repo.** A task's `claude-project` custom property (or its first linked project) resolves to a project slug, which the daemon matches against `serve.json`'s configured projects, case-insensitively.
- **Sessions are generations, not a single long-lived process.** Each spawn gets a name like `orchestrator-<slug>-g<gen>` and its own tmux session; a session that's used up enough of its context window drains (stops taking new work) while a fresh generation takes over, so there's no hard cutover. See [generations and drain](docs/explanation/generations-and-drain.md).
- **The daemon never talks to an LLM API itself.** It only starts a `claude` process in tmux and exchanges plain messages with it over its own local HTTP API; all reasoning happens inside the spawned Claude Code session.

## Requirements

- **Go 1.26+** to build (standard library only, no external Go dependencies).
- **Obsidian** with the [TaskNotes](https://github.com/callumalpass/tasknotes) plugin, HTTP API enabled. See [`docs/how-to/set-up-tasknotes.md`](docs/how-to/set-up-tasknotes.md) for the full TaskNotes-side configuration (statuses, user fields, tags) `tn` depends on.
- **tmux** and the **Claude Code CLI** if you want `tn serve`'s orchestrator-spawning features.
- **macOS** for the daemon's dialog prompts, Keychain-backed credential profiles, and launchd supervision. The daemon core is portable Go and runs on Linux, but those macOS-specific features (`osascript` dialogs, `/usr/bin/security`, `codesign`, launchd) are unavailable there.
- **Node.js + pnpm**, only if you're changing the embedded web UI (`webui/`).

## Install

Build from source:

```bash
git clone https://github.com/maylukas/claude-code-tasknotes.git
cd claude-code-tasknotes
go build -o ~/bin/tn .
```

Make sure `~/bin` is on your `PATH`. The module path in `go.mod` (`tasknotes-cli`) has no host prefix, so `go install` from a remote import path isn't available until the module is published under one; build from a local clone instead.

> **Note:** On macOS, sign the binary with your own identity so the daemon's Apple Events permission dialogs don't re-prompt on every rebuild:
> ```bash
> codesign -s "Apple Development: Your Name (TEAMID)" -f --identifier com.example.tn ~/bin/tn
> ```
> See [`docs/how-to/run-as-a-service.md`](docs/how-to/run-as-a-service.md) for running `tn serve` under launchd.

## Quickstart

1. In Obsidian, enable TaskNotes' HTTP API (TaskNotes plugin settings) and note the port and token, if any.
2. Write `~/.config/tn/config.json`:
   ```json
   { "url": "http://localhost:8390", "token": "your-token" }
   ```
3. Confirm the CLI can reach it:
   ```bash
   tn health
   ```
4. Create and list a task:
   ```bash
   tn create --title "Write release notes" --priority high --tag docs
   tn list
   ```

Next: run the bridge daemon and route a task to a Claude Code session. See [`docs/tutorials/first-task.md`](docs/tutorials/first-task.md).

## Configuration files at a glance

| File | Purpose |
|---|---|
| `~/.config/tn/config.json` | TaskNotes API URL/token for the CLI (env `TASKNOTES_URL`/`TASKNOTES_TOKEN` override) |
| `~/.config/tn/serve.json` | Daemon config: port, per-project `autoSpawn`/`cwd`/`env`, dashboard/session-page paths, credential and worktree-reaper settings |
| `~/.local/state/tn/state.json` | Daemon state: agent registry, message queue, routing dedup markers |
| `~/.local/state/tn/creds.json` | Non-secret bookkeeping for saved credential profiles (secrets live in the Keychain, never here) |

Full key-by-key reference: [`docs/reference/configuration.md`](docs/reference/configuration.md).

## Features

- Full task CRUD against the TaskNotes HTTP API, including dependencies, Jira linkage, and MR tracking: [`docs/reference/cli.md`](docs/reference/cli.md)
- A structured, foldable task-note body (ask / brief / description / history) instead of a flat log: [`docs/reference/note-layout.md`](docs/reference/note-layout.md)
- A local bridge daemon (`tn serve`) with an agent registry, message bus, and webhook routing: [`docs/reference/http-api.md`](docs/reference/http-api.md)
- Automatic orchestrator spawning in tmux, with generation-based rolling replacement instead of hard rotation: [`docs/explanation/generations-and-drain.md`](docs/explanation/generations-and-drain.md)
- A live dashboard note and per-session pages rendered back into your vault: [`docs/how-to/add-a-project.md`](docs/how-to/add-a-project.md)
- An embedded web UI and `/status` JSON endpoint for a tray/menu-bar client: [`docs/reference/http-api.md`](docs/reference/http-api.md)
- Stuck-prompt detection (pane scraping plus Claude Code hooks) with macOS approval dialogs: [`docs/how-to/install-claude-hook.md`](docs/how-to/install-claude-hook.md)
- Keychain-backed claude.ai credential profiles with optional auto-swap on usage limits: [`docs/how-to/credential-profiles.md`](docs/how-to/credential-profiles.md)
- A worktree reaper that cleans up finished agent worktrees safely: [`docs/how-to/worktree-reaper.md`](docs/how-to/worktree-reaper.md)
- Optional Jira status-transition prompts for tasks carrying Jira keys: [`docs/how-to/jira-integration.md`](docs/how-to/jira-integration.md)

## A worked example

Tagging a task routes it once the daemon is running and a project is configured:

```bash
# one-time: tell the daemon about a project (create the file, or add the
# "myapp" entry under "projects" if serve.json already exists)
cat > ~/.config/tn/serve.json <<'JSON'
{ "projects": { "myapp": { "autoSpawn": true, "cwd": "/path/to/myapp" } } }
JSON
# the daemon reads its config at startup: restart tn serve

# tag a task for that project
tn create --title "Fix flaky login test" --tag claude --claude-project myapp

# the daemon routes it to a running orchestrator, or spawns one in tmux;
# watch it show up
tn agents
tn list --project myapp
```

From there the session works the task, updates its status and note as it goes, and either finishes it or asks you something with `needs-input`, visible in the generated dashboard note, the session's own page, or `GET /ui`.

## Documentation

| Section | Where |
|---|---|
| Tutorial: your first routed task | [`docs/tutorials/first-task.md`](docs/tutorials/first-task.md) |
| How-to guides (add a project, run as a service, install hooks, credential profiles, worktree reaper, Obsidian plugin, web UI dev, Jira, troubleshooting) | [`docs/how-to/`](docs/how-to/) |
| Reference: CLI | [`docs/reference/cli.md`](docs/reference/cli.md) |
| Reference: HTTP API | [`docs/reference/http-api.md`](docs/reference/http-api.md) |
| Reference: configuration | [`docs/reference/configuration.md`](docs/reference/configuration.md) |
| Reference: note layout | [`docs/reference/note-layout.md`](docs/reference/note-layout.md) |
| Explanation: lifecycle of a task | [`docs/explanation/lifecycle-of-a-task.md`](docs/explanation/lifecycle-of-a-task.md) |
| Explanation: generations and drain | [`docs/explanation/generations-and-drain.md`](docs/explanation/generations-and-drain.md) |
| Explanation: security model | [`docs/explanation/security-model.md`](docs/explanation/security-model.md) |
| Explanation: design history | [`docs/explanation/design-history.md`](docs/explanation/design-history.md) |
| Architecture overview (codemap) | [`ARCHITECTURE.md`](ARCHITECTURE.md) |
| Full documentation index | [`docs/README.md`](docs/README.md) |

## Status and versioning

This project is pre-1.0 and under active development; expect breaking changes between releases. The daemon reports its own build via a `daemonVersion` constant, surfaced at `/health`, `/status`, and the web UI's footer, bumped on notable changes to either endpoint's shape or the daemon's observable behavior. Once tagged releases begin, this project intends to follow [SemVer](https://semver.org/). See [`CHANGELOG.md`](CHANGELOG.md) for what's changed.

## Contributing

Contributions are welcome. See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the development workflow, test requirements, and code style, and [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md) for how we expect people to treat each other here.

## Security

See [`SECURITY.md`](SECURITY.md) for how to report a vulnerability. Note that the daemon's local HTTP API has no authentication by design; see [`docs/explanation/security-model.md`](docs/explanation/security-model.md) before running it anywhere but a single-user machine.

## License

MIT, see [`LICENSE`](LICENSE).
