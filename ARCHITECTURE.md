# Architecture

A codemap for `tasknotes-cli`: what lives where, and why, so you can find the right ~50 lines instead of reading 10,000.

## Bird's-eye view

Everything ships as one Go binary, `tn`. A thin `cmd/tn` entrypoint (package `main`) calls into `internal/tn` (package `tn`), which holds all the logic; stdlib only, no third-party Go modules. The binary does two unrelated-looking jobs from the same codebase:

1. **CLI**: a thin wrapper around the TaskNotes Obsidian plugin's HTTP API (`internal/tn/api.go`), invoked as `tn <command>`.
2. **Daemon**: `tn serve` runs an HTTP server on `127.0.0.1:8391` that bridges TaskNotes webhooks to Claude Code agent sessions running in tmux, plus a handful of background loops (spawn reconciliation, stuck-session detection, due-task scanning, a merge/pull-request watcher, and more).

These two jobs share the `Task`/`Client` types in `api.go` and the structured note-body logic in `note_layout.go`, but otherwise `main.go`'s CLI dispatch and the daemon's HTTP handlers are independent code paths. Two satellite subtrees, `webui/` (a React SPA embedded into the daemon at `GET /ui` via `webui/embed.go`) and `obsidian-plugin/` (a thin Obsidian panel that iframes it), have their own toolchains and are never Go dependencies of `internal/tn` — the daemon imports the small `tasknotes-cli/webui` package and serves its exported `webui.Dist` (an `embed.FS`).

## Repository layout

```text
cmd/tn/main.go        entrypoint (package main): calls tn.Run and nothing else
internal/tn/          package tn — all CLI and daemon logic (see code map below)
webui/                React + TanStack Router + shadcn/ui SPA, served at GET /ui, own pnpm build step
  embed.go            package webui: `Dist embed.FS` (`//go:embed all:dist`), imported by internal/tn
obsidian-plugin/       Thin ItemView: an iframe pointed at the daemon's own /ui, plus a postMessage bridge
hooks/                 Claude Code PermissionRequest/PermissionDenied hook script
skills/                Claude skill, exposed via this repo being a plugin
docs/                  Diátaxis documentation; docs/design/ holds the historical SPEC-*.md / IMPLEMENTATION-PLAN-*.md records
```

## Code map: `internal/tn`

| File | Contents |
|---|---|
| `main.go` | `usage` text (the authoritative CLI synopsis), `Run()`'s command dispatch table, and one `cmd*` function per subcommand |
| `api.go` | `Task`/`Client` types, `resolveConfig` (env/config.json/defaults), task CRUD (`CreateTask`, `UpdateTask`, `GetTask`, `QueryTasks`), webhook list/create |
| `bridge.go` | `resolveBridgeURL`, the bridge HTTP client, and the CLI-side implementation of `register`/`drain`/`retire`/`inbox`/`ack`/`send`/`agents`/`log`/`env`/`repo` |
| `note_layout.go` | `noteBody` parse/render/migrate (pure functions, no I/O), `checkDoneMeans` (the DONE MEANS evidence gate) |
| `creds.go` | `keychainStore` interface + macOS `security`-backed implementation, `credsStore` (profile capture/list/use), `handleCredsList`/`handleCredsSwap`, auto-swap on a detected usage limit |
| `reaper.go` | `worktreeReaper`: eligibility rules (unlocked, aged past `minAge`, clean, HEAD pushed), `tn worktrees` CLI, background sweep |

### The daemon, split by topic

The daemon used to be one large `serve.go` (see [design history](docs/explanation/design-history.md) for why it was written that way); it's now split into topical files, still all in package `tn` alongside the files above:

| File | What it does |
|---|---|
| `serve.go` | State model (`State`, `Agent`, `Message`, `ServeConfig`, `ProjectConfig` — see [data structures](#key-data-structures) below), the `Server` type, config resolution, and the entrypoint (`newMux`, `cmdServe`) |
| `repo_settings.go` | `repoSettingsFor`: live 60s-cached read of a project's `max-orchestrators`/`max-workers` from its repo note |
| `sse.go` | Server-sent events (`GET /events`) for the web UI |
| `handlers.go` | Core agent-registry and messaging HTTP handlers: register/drain/retire/context/approve, inbox/ack/messages |
| `orphans.go` | Orphaned-task safety net: detects a dead agent's in-progress tasks and resets them to open |
| `stranded.go` | Stranded-message sweep: reassigns or archives messages addressed to a confirmed-dead agent |
| `sleep.go` | Host sleep detection and self-heal: recovers liveness bookkeeping after the machine sleeps |
| `workers.go` | Worker visibility (`tn worker start|end`): declares/retracts a worker subtask for `/status` |
| `feed.go` | Session activity feed and the human-reply endpoint (`GET /feed`, `POST /reply`) |
| `status.go` | `GET /status` aggregate JSON (tray-app foundation) |
| `ui.go` | `GET /ui`: serves the embedded `webui.Dist` bundle |
| `webhook.go` | TaskNotes webhook handling (`handleTaskNotesWebhook`, routing, `@claude:` line extraction) and the daemon's own webhook self-registration with TaskNotes on startup |
| `due_scanner.go` | Due-task scanner: picks up recurring/scheduled tasks that came due without a webhook |
| `mr_watcher.go` | MR/PR watcher orchestration: polls the resolved code-host provider for state on tasks carrying an `mr` URL |
| `codehost.go` | `codeHost` provider interface + `resolveCodeHost` (URL-shape detection, per-project override) |
| `codehost_gitlab.go` | GitLab provider (`gitlabHost`, via `glab`) |
| `codehost_github.go` | GitHub provider (`githubHost`, via `gh`) |
| `webhook_reconciler.go` | Webhook-miss reconciler: re-routes tasks modified while the daemon was briefly down |
| `spawn.go` | Orchestrator startup: builds the launch command, starts/reuses a tmux session |
| `spawn_reconciler.go` | Spawn reconciler: retries failed spawns, scales out additional generations |
| `debounce.go` | Dashboard-render coalescing |
| `stuck.go` | Stuck session detection: pane-scraping + hook-based permission-prompt detection, macOS dialogs |
| `needs_action.go` | Needs-your-action dashboard section: live TaskNotes query for triage/needs-input/review tasks |
| `dashboard.go` | Dashboard rendering: `renderDashboardMarkdown`, writes `Claude Sessions.md` |
| `session_pages.go` | Per-session activity pages: one page per agent under `sessionPagesDir` |
| `selfrestart.go` | Self-restart on binary change: exits when `~/bin/tn`'s bytes change, so launchd restarts it |

Plus one `*_test.go` file per file above (see [testing conventions](#testing-conventions) below).

## Key data structures

| Type | Persisted at | Purpose |
|---|---|---|
| `State` | `~/.local/state/tn/state.json` | The daemon's entire in-memory model: `Agents`, `Messages`, dedup markers (`TaskDetailsHash`, `RoutedClaudeLines`, `AssignedOccurrences`), `LastReconciledAt`, `SpawnPaused` |
| `Agent` | inside `State.Agents` | One registered session: name, project, `Accepting` (drain flag), `TmuxSession`, `ContextPct`, liveness timestamps |
| `Message` | inside `State.Messages` | One inbox item: `To`/`From`/`Text`/`Status` (`queued`→`delivered`→`acked`, or a terminal `archived`), optional `TaskPath` |
| `ServeConfig` / `ProjectConfig` | `~/.config/tn/serve.json` | Daemon config: port, dashboard/session-page paths, credential and worktree-reaper settings; per-project `autoSpawn`, `cwd`, `env`, `jiraStatusMap` |
| `credsStore` state | `~/.local/state/tn/creds.json` (secrets in Keychain) | Non-secret bookkeeping for saved claude.ai credential profiles |

## Background loops

Each runs as its own goroutine, started from `cmdServe`:

| Loop | Interval | Purpose |
|---|---|---|
| Spawn reconciler | 30s (`reconcilerInterval`) | Retries stranded spawns, scales out generations under `max-orchestrators` |
| Stuck session detector | 2m (`stuckCheckInterval`) | tmux pane-scrape for permission prompts; re-prompts a stuck dialog every 15m (`stuckDialogRepromptInterval`) |
| Sleep detector | 30s (`sleepCheckInterval`) | Detects the host having slept and re-baselines liveness checks |
| Due-task scanner / webhook-miss reconciler | 10m (`dueScannerInterval`), shared cadence | Picks up recurring tasks and re-routes tasks TaskNotes' webhook missed |
| MR watcher | 5m (`mrWatchInterval`) | Polls the resolved code-host provider (`glab` or `gh`) for merge/pull-request state transitions |
| Worktree reaper sweep | 30m default (`worktreeReaperDefaultInterval`), configurable | Removes eligible finished worktrees |
| Dashboard render debounce | 2s (`dashboardDebounceInterval`) | Coalesces rapid state mutations into one render |
| SSE heartbeat | 30s (`sseHeartbeatInterval`) | Keeps `/events` connections alive |
| Binary self-restart watcher | 10s (`binaryWatchInterval`) | Exits the process when `~/bin/tn`'s bytes change, so launchd (`KeepAlive`) relaunches the new build |

Agent liveness itself isn't a loop: it's derived from `aliveWindow` (90s) against each agent's `LastSeenAt`, bumped on every inbox poll. The poll *is* the heartbeat.

## Invariants and deliberate absences

- **Stdlib only.** No third-party Go modules, in the daemon or the CLI. `embed` (for `webui/dist`) is the only non-trivial stdlib feature relied on.
- **No authentication on the HTTP API.** Both TaskNotes' API and the daemon's bind to `127.0.0.1` and trust any local process. See [security model](docs/explanation/security-model.md).
- **Tests never touch tmux, the real vault, or the Keychain.** Every side effect that would is injected as a function value and faked in tests: `spawnFunc`, `captureFunc`, `dialogFunc`, `sendKeysFunc`, `keychainStore`, `gitRunFunc` (and the `tmuxHasSessionFunc`/`tmuxListPanesFunc`/`tmuxKillSessionFunc`/`tmuxNewSessionFunc` family). Functional smoke runs use explicit high ports (18xxx) and a scratch `$HOME`/dashboard path.
- **All state and dashboard writes are atomic**, via `atomicWriteFile` (write to a temp file, then rename). Obsidian and other readers must never see a partially-written file.
- **`webui/dist/` is committed, not gitignored.** A fresh checkout can `go build` immediately without Node/pnpm; `pnpm build` is only needed after touching `webui/src/`. `go build` embeds whatever bytes are currently on disk in `webui/dist/` with no check against `webui/src/`; see the "footgun" note in `CLAUDE.md`'s build/test section if the served UI ever looks stale.
- **External binaries are resolved, never assumed**: `resolveTmuxBin`, `resolveGlabBin`, and `resolveGitBin` (`internal/tn/serve.go`, `internal/tn/reaper.go`) try the usual Homebrew and system locations before falling back to `$PATH`, because launchd's environment has no Homebrew on `PATH`. The operating contract `ORCHESTRATOR.md` is located by `resolveOrchestratorDoc` (env `TN_ORCHESTRATOR_DOC`, `serve.json` `orchestratorDoc`, `~/.config/tn/`, next to the binary, cwd) and logged at startup.

## `webui/` and `obsidian-plugin/`

`webui/` is a React + TypeScript SPA built with Vite, using TanStack Router and shadcn/ui, that renders `GET /ui`'s content: the dashboard, session views, and status page a human actually looks at. It has its own `package.json`/`pnpm-lock.yaml` and is never a Go module dependency of `internal/tn`; `pnpm build` emits `webui/dist/`, which `webui/embed.go` (package `webui`, `//go:embed all:dist`) exposes as `Dist embed.FS`, imported and served by `internal/tn/ui.go`.

`obsidian-plugin/` is deliberately small: an `ItemView` containing an `<iframe src="http://localhost:8391/ui?host=obsidian">` and a `postMessage` bridge for the one thing an iframe can't do on its own, opening a vault note or syncing Obsidian's active theme into the embedded page. It supersedes an earlier design (see `docs/design/SPEC-obsidian-plugin.md`) where the plugin rendered its own DOM directly from polled JSON; that approach is documented for history but the daemon-owned-UI shape in `docs/design/SPEC-ui-v2.md` is what's actually built.

## Data flow: one webhook to one routed message

A concrete trace through the layers above, for a task tagged `claude` that just moved to `open`:

1. TaskNotes fires a `task.updated` webhook to `POST /webhooks/tasknotes` (registered on startup by the webhook self-registration region).
2. `handleTaskNotesWebhook` decodes it and calls `routeTaskNotification`, which resolves a routing slug from the task's `claude-project` custom property (or its first linked project) via `routingSlugForTask`/`normalizeProjectSlug`.
3. If the task carries the `claude`/`spawn` tag, is in an assignable status, and isn't blocked by an unresolved dependency, the daemon looks for an alive, accepting agent for that slug. If one exists, it gets a new `Message` with `Status: "queued"`. If none exists (and the project has `autoSpawn: true`), `spawnOrchestrator` calls the injected `spawnFunc`, which starts a tmux session running `buildLaunchCommand`'s `claude --permission-mode auto <prompt>`.
4. The spawned session runs `tn register --name orchestrator-<slug>-g<gen> --project <slug> --tmux-session tn-<slug>-g<gen>`, then long-polls `GET /inbox`. On its next poll the queued message flips to `delivered`.
5. The session works the task, writes structured note content back through `tn note`/`tn brief`/`tn ask`, and calls `POST /ack` when done with the message, flipping it to `acked` and (if it included a `response`) recording that text.
6. Every one of these mutations calls `triggerRenders`, which debounces (`dashboardDebounceInterval`) into a `renderDashboardMarkdown` pass and, if `sessionPagesDir` is set, an updated per-agent page, so the vault reflects state within a couple of seconds without the human doing anything.

See `docs/explanation/lifecycle-of-a-task.md` for the full walk, including the `needs-input`/reply loop and the MR-watcher-driven close.

## Testing conventions

Every `*_test.go` file (there's one per major subsystem: `serve_test.go`, `stuck_detector_test.go`, `creds_test.go`, `reaper_test.go`, and so on, all in `internal/tn/` alongside the code they test) builds a `Server`/`Client` via `httptest` and the same injected-function seams described above, never a real daemon, tmux session, or vault. `go test -race ./...` is not optional: the project's own history records two real concurrency bugs (a `*Message` JSON-encoded outside its lock while a concurrent inbox poll mutated it, and an unsynchronized slice in a debouncer test) that only `-race` caught, on first use. A change to the daemon isn't done until `gofmt -l .`, `go vet ./...`, `go test ./...`, and `go test -race ./...` are all clean; see `CLAUDE.md`'s build/test section for the exact commands and the deploy caveats around actually restarting the running daemon.

## Where to change what

- **Adding a CLI command**: `internal/tn/main.go` (usage text + `Run()`'s switch + a `cmd*` function), calling into `api.go` or `bridge.go` as appropriate.
- **Adding a TaskNotes API call**: `internal/tn/api.go`'s `Client` methods.
- **Changing the task-note body format**: `internal/tn/note_layout.go`; keep it pure (no I/O), and update `docs/design/SPEC-note-layout.md` and `docs/reference/note-layout.md` alongside it.
- **Adding a daemon HTTP endpoint**: a handler in `internal/tn/handlers.go` (or the relevant topical file) plus a route in `newMux` (`internal/tn/serve.go`); update `docs/reference/http-api.md`.
- **Changing the orchestrator spawn/prompt protocol**: `buildOrchestratorPrompt`/`buildLaunchCommand` in `internal/tn/spawn.go`, and keep `ORCHESTRATOR.md`, their tests, and `skills/tasknotes/` consistent; see `CLAUDE.md`'s "Conventions & gotchas."
- **Changing a background loop's behavior**: find it via the region table above; most have a `TN_NO_*` env var to disable them for tests/dev (see `docs/reference/configuration.md`).
- **Changing the web UI**: `webui/src/`, then `pnpm build` before `go build` picks up the change (see `CLAUDE.md`'s build/test section for the footgun this creates if skipped).
- **Changing the Obsidian panel**: `obsidian-plugin/`, symlinked into a vault's plugins directory per `docs/design/SPEC-obsidian-plugin.md`.

## Glossary

- **Orchestrator**: a Claude Code session the daemon spawned in tmux to work a project's task queue, running under `ORCHESTRATOR.md`'s operating contract.
- **Generation**: one spawn of an orchestrator, named `orchestrator-<slug>-g<gen>` with tmux session `tn-<slug>-g<gen>`. Generations replace each other on a rolling basis, not a hard restart.
- **Draining**: a generation that has stopped accepting new task assignments (voluntarily via `tn drain`, or auto-drained past `drainContextPct`) but keeps finishing what it already owns.
- **Worker**: a subtask an orchestrator dispatches (via Claude Code's own Task tool), often in an isolated git worktree under `<repo>/.claude/worktrees/`; declared to the daemon via `tn worker start|end` purely for `/status` visibility, not scheduled by the daemon itself.
- **Routing slug**: the normalized project identifier (`normalizeProjectSlug`) used to match a task to a `serve.json` project entry, an agent name, and a tmux session.

## Further reading

The `SPEC-*.md` files and `IMPLEMENTATION-PLAN-REMOTE-EXECUTION.md` in `docs/design/` are historical design records, not living documentation: they capture the reasoning behind a change at the time it was made, and the code is the source of truth where they've since drifted. `docs/explanation/design-history.md` indexes all of them with a summary of what each covers. For narrative explanations of how the system behaves today, see `docs/explanation/lifecycle-of-a-task.md`, `docs/explanation/generations-and-drain.md`, and `docs/explanation/security-model.md`.
