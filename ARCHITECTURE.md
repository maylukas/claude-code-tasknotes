# Architecture

A codemap for `tasknotes-cli`: what lives where, and why, so you can find the right ~50 lines instead of reading 10,000.

## Bird's-eye view

Everything ships as one Go binary, `tn`, built from a flat package `main` at the repo root (no internal packages, stdlib only). The binary does two unrelated-looking jobs from the same codebase:

1. **CLI**: a thin wrapper around the TaskNotes Obsidian plugin's HTTP API (`api.go`), invoked as `tn <command>`.
2. **Daemon**: `tn serve` runs an HTTP server on `127.0.0.1:8391` that bridges TaskNotes webhooks to Claude Code agent sessions running in tmux, plus a handful of background loops (spawn reconciliation, stuck-session detection, due-task scanning, an MR watcher, and more).

These two jobs share the `Task`/`Client` types in `api.go` and the structured note-body logic in `note_layout.go`, but otherwise `main.go`'s CLI dispatch and `serve.go`'s HTTP handlers are independent code paths. Two satellite subtrees, `webui/` (a React SPA embedded into the daemon at `GET /ui`) and `obsidian-plugin/` (a thin Obsidian panel that iframes it), have their own toolchains and are never Go dependencies.

## Single-binary layout

```text
main.go          CLI dispatch (flag parsing, command table, usage text)
api.go           TaskNotes HTTP API client: config resolve, task CRUD, query, webhooks
bridge.go        Bridge client: register/inbox/ack/send/agents subcommands (talks to serve.go)
note_layout.go   Structured task-note body (ask/brief/description/history/reply/links blocks)
serve.go         The daemon: state, HTTP handlers, webhook routing, spawn, dashboard, background loops
creds.go         Credential-profile capture/list/swap (Keychain-backed)
reaper.go        Worktree reaper: safe cleanup of finished agent git worktrees
webui/           React + TanStack Router + shadcn/ui SPA, served at GET /ui, own pnpm build step
obsidian-plugin/ Thin ItemView: an iframe pointed at the daemon's own /ui, plus a postMessage bridge
```

## Code map: what's in each file

| File | Lines | Contents |
|---|---|---|
| `main.go` | ~860 | `usage` text (the authoritative CLI synopsis), `run()`'s command dispatch table, and one `cmd*` function per subcommand |
| `api.go` | ~530 | `Task`/`Client` types, `resolveConfig` (env/config.json/defaults), task CRUD (`CreateTask`, `UpdateTask`, `GetTask`, `QueryTasks`), webhook list/create |
| `bridge.go` | ~750 | `resolveBridgeURL`, the bridge HTTP client, and the CLI-side implementation of `register`/`drain`/`retire`/`inbox`/`ack`/`send`/`agents`/`log`/`env`/`repo` |
| `note_layout.go` | ~550 | `noteBody` parse/render/migrate (pure functions, no I/O), `checkDoneMeans` (the DONE MEANS evidence gate) |
| `creds.go` | ~1030 | `keychainStore` interface + macOS `security`-backed implementation, `credsStore` (profile capture/list/use), `handleCredsList`/`handleCredsSwap`, auto-swap on a detected usage limit |
| `reaper.go` | ~890 | `worktreeReaper`: eligibility rules (unlocked, aged past `minAge`, clean, HEAD pushed), `tn worktrees` CLI, background sweep |
| `serve.go` | ~9930 | The daemon: see the region breakdown below |

### `serve.go` region breakdown

`serve.go` is one file by deliberate choice (see [design history](docs/explanation/design-history.md)), but it's organized into commented regions. Approximate line numbers as of this writing; search for the marker comment rather than trusting the number if the file has moved on:

| Region | Around line | What it does |
|---|---|---|
| State model (`State`, `Agent`, `Message`, `ServeConfig`, `ProjectConfig`) | 40-560 | Persisted structs, see [data structures](#key-data-structures) below |
| Repo-note parallelism settings | 413 | `repoSettingsFor`: live 60s-cached read of a project's `max-orchestrators`/`max-workers` |
| SSE (`GET /events`) | 766 | Server-sent events for the web UI |
| HTTP handlers (register/drain/retire/context/approve, inbox/ack/messages) | 1473+ | Core agent-registry and messaging endpoints |
| Orphaned-task safety net | 1836 | Detects a dead agent's in-progress tasks and resets them to open |
| Stranded-message sweep | 2179 | Reassigns or archives messages addressed to a confirmed-dead agent |
| Host sleep detection & self-heal | 2486 | Recovers liveness bookkeeping after the machine sleeps |
| Worker visibility (`tn worker start|end`) | 2995 | Declares/retracts a worker subtask for `/status` |
| Feed + reply (`GET /feed`, `POST /reply`) | 3080 | Session activity feed and the human-reply endpoint |
| Status endpoint (tray-app foundation) | 4028, handler at 4759 | `GET /status` aggregate JSON |
| UI v2: embedded React SPA | 4840 | `GET /ui` serves `webui/dist` via `go:embed` |
| TaskNotes webhook handling | 4980 | `handleTaskNotesWebhook`, routing, `@claude:` line extraction |
| Webhook self-registration | 6021 | Registers the daemon's own webhook URL with TaskNotes on startup |
| Due-task scanner | 6107, `startDueTaskScanner` at 6255 | Picks up recurring/scheduled tasks that came due without a webhook |
| MR watcher | 6364, `startMRWatcher` at 6469 | Polls `glab` for merge request state on tasks carrying an MR URL |
| Webhook-miss reconciler | 6640 | Re-routes tasks modified while the daemon was briefly down |
| Orchestrator startup | 7274, `spawnOrchestratorSession` at 7393 | Builds the launch command, starts/reuses a tmux session |
| Spawn reconciler | 7484, `startSpawnReconciler` at 7497 | Retries failed spawns, scales out additional generations |
| Debounce | 7662 | Dashboard-render coalescing |
| Stuck session detection | 7713, `startStuckDetector` at 8395 | Pane-scraping + hook-based permission-prompt detection, macOS dialogs |
| Needs-your-action (dashboard section) | 8979 | Live TaskNotes query for triage/needs-input/review tasks |
| Dashboard rendering | 9120, `renderDashboardMarkdown` at 9400 | Renders `Claude Sessions.md` |
| Per-session activity pages | 9469 | One page per agent under `sessionPagesDir` |
| Self-restart on binary change | 9670, `watchBinaryForSelfRestart` at 9706 | Exits when `~/bin/tn`'s bytes change, so launchd restarts it |
| Config / entrypoint | 9765, `newMux` at 9841 | `resolveServeConfig`, the full route table, `cmdServe` |

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
| MR watcher | 5m (`mrWatchInterval`) | Polls `glab` for merge request state transitions |
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
- **External binaries are resolved, never assumed**: `resolveTmuxBin`, `resolveGlabBin`, and `resolveGitBin` (serve.go, reaper.go) try the usual Homebrew and system locations before falling back to `$PATH`, because launchd's environment has no Homebrew on `PATH`. The operating contract `ORCHESTRATOR.md` is located by `resolveOrchestratorDoc` (env `TN_ORCHESTRATOR_DOC`, `serve.json` `orchestratorDoc`, `~/.config/tn/`, next to the binary, cwd) and logged at startup.

## `webui/` and `obsidian-plugin/`

`webui/` is a React + TypeScript SPA built with Vite, using TanStack Router and shadcn/ui, that renders `GET /ui`'s content: the dashboard, session views, and status page a human actually looks at. It has its own `package.json`/`pnpm-lock.yaml` and is never a Go module dependency; `pnpm build` emits `webui/dist/`, which `serve.go` embeds wholesale via `//go:embed all:webui/dist`.

`obsidian-plugin/` is deliberately small: an `ItemView` containing an `<iframe src="http://localhost:8391/ui?host=obsidian">` and a `postMessage` bridge for the one thing an iframe can't do on its own, opening a vault note or syncing Obsidian's active theme into the embedded page. It supersedes an earlier design (see `SPEC-obsidian-plugin.md`) where the plugin rendered its own DOM directly from polled JSON; that approach is documented for history but the daemon-owned-UI shape in `SPEC-ui-v2.md` is what's actually built.

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

Every `*_test.go` file (there's one per major subsystem: `serve_test.go`, `stuck_detector_test.go`, `creds_test.go`, `reaper_test.go`, and so on, all at the repo root alongside the code they test, no `internal/` split) builds a `Server`/`Client` via `httptest` and the same injected-function seams described above, never a real daemon, tmux session, or vault. `go test -race ./...` is not optional: the project's own history records two real concurrency bugs (a `*Message` JSON-encoded outside its lock while a concurrent inbox poll mutated it, and an unsynchronized slice in a debouncer test) that only `-race` caught, on first use. A change to `serve.go` is not done until `gofmt -l .`, `go vet ./...`, `go test ./...`, and `go test -race ./...` are all clean; see `CLAUDE.md`'s build/test section for the exact commands and the deploy caveats around actually restarting the running daemon.

## Where to change what

- **Adding a CLI command**: `main.go` (usage text + `run()`'s switch + a `cmd*` function), calling into `api.go` or `bridge.go` as appropriate.
- **Adding a TaskNotes API call**: `api.go`'s `Client` methods.
- **Changing the task-note body format**: `note_layout.go`; keep it pure (no I/O), and update `SPEC-note-layout.md` and `docs/reference/note-layout.md` alongside it.
- **Adding a daemon HTTP endpoint**: a handler in `serve.go` plus a route in `newMux`; update `docs/reference/http-api.md`.
- **Changing the orchestrator spawn/prompt protocol**: `buildOrchestratorPrompt`/`buildLaunchCommand` in `serve.go`, and keep `ORCHESTRATOR.md`, their tests, and `skills/tasknotes/` consistent; see `CLAUDE.md`'s "Conventions & gotchas."
- **Changing a background loop's behavior**: find it via the region table above; most have a `TN_NO_*` env var to disable them for tests/dev (see `docs/reference/configuration.md`).
- **Changing the web UI**: `webui/src/`, then `pnpm build` before `go build` picks up the change (see `CLAUDE.md`'s build/test section for the footgun this creates if skipped).
- **Changing the Obsidian panel**: `obsidian-plugin/`, symlinked into a vault's plugins directory per `SPEC-obsidian-plugin.md`.

## Glossary

- **Orchestrator**: a Claude Code session the daemon spawned in tmux to work a project's task queue, running under `ORCHESTRATOR.md`'s operating contract.
- **Generation**: one spawn of an orchestrator, named `orchestrator-<slug>-g<gen>` with tmux session `tn-<slug>-g<gen>`. Generations replace each other on a rolling basis, not a hard restart.
- **Draining**: a generation that has stopped accepting new task assignments (voluntarily via `tn drain`, or auto-drained past `drainContextPct`) but keeps finishing what it already owns.
- **Worker**: a subtask an orchestrator dispatches (via Claude Code's own Task tool), often in an isolated git worktree under `<repo>/.claude/worktrees/`; declared to the daemon via `tn worker start|end` purely for `/status` visibility, not scheduled by the daemon itself.
- **Routing slug**: the normalized project identifier (`normalizeProjectSlug`) used to match a task to a `serve.json` project entry, an agent name, and a tmux session.

## Further reading

The `SPEC-*.md` files and `IMPLEMENTATION-PLAN-REMOTE-EXECUTION.md` at the repo root are historical design records, not living documentation: they capture the reasoning behind a change at the time it was made, and the code is the source of truth where they've since drifted. `docs/explanation/design-history.md` indexes all of them with a summary of what each covers. For narrative explanations of how the system behaves today, see `docs/explanation/lifecycle-of-a-task.md`, `docs/explanation/generations-and-drain.md`, and `docs/explanation/security-model.md`.
