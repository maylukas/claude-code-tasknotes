# tasknotes-cli

Go CLI + bridge daemon connecting the Obsidian TaskNotes plugin to Claude Code agent
sessions. Single binary `tn`: task CRUD against the TaskNotes HTTP API, plus a local
message bridge (`tn serve`) with agent registry, inbox long-polling, TaskNotes
webhooks, tmux orchestrator spawning, and a dashboard note rendered into the vault.

## Build / test

```bash
gofmt -l . && go vet ./... && go test ./... && go test -race ./...   # must all be clean before handing off
go build -o ~/bin/tn ./cmd/tn && codesign -s "Apple Development: Your Name (TEAMID)" -f --identifier com.example.tn ~/bin/tn
# ALWAYS sign after building: the daemon shows macOS dialogs via osascript, and TCC
# keys its Apple-Events approval on the code identity — an unsigned rebuild changes
# identity every time and re-triggers "tn would like to access data from other apps"
# prompt storms. Signing keeps one user approval valid forever.
# UNRELIABLE, DO NOT TRUST: replacing the binary on disk does NOT reliably restart
# the running daemon. It sometimes does (confirmed live) and sometimes does not
# (also confirmed live, same day — the old process kept serving for 86+ minutes
# after a fresh, signed binary landed on disk). ALWAYS verify explicitly after a
# deploy: curl localhost:8391/status and check daemonVersion + uptime against what
# you just built and bumped — a version that hasn't changed, or an uptime that
# predates your build, means the fix is compiled but NOT running. If it hasn't
# picked up: `launchctl kickstart -k gui/$UID/com.example.tn-serve`. See the
# Conventions & gotchas entry below for the live incident this rule comes from.
# -race is mandatory, not optional: it caught two real races on first use (a shared
# *Message JSON-encoded outside its lock while a concurrent inbox poll mutated it —
# genuine production concurrency, not a test artifact — and an unsynchronized slice in
# a debouncer test). Without it, this class of bug stays latent indefinitely.
```

**The web UI (`webui/`) has a SEPARATE, DELIBERATE build step — it is NOT part of
`go build`:**

```bash
cd webui && pnpm build   # emits webui/dist/, which webui/embed.go embeds via `//go:embed all:dist`
cd .. && go build -o ~/bin/tn ./cmd/tn   # picks up whatever is currently in webui/dist/ — see below
```

- **The footgun**: `go build` embeds whatever bytes are on disk in `webui/dist/` at
  that moment — it does NOT know or check whether `webui/`'s TypeScript/React source
  has changed since the bundle was last built. Edit a `.tsx` file, skip `pnpm build`,
  run `go build`, and the daemon will happily compile, sign, and serve a STALE UI with
  zero warning. There is no version check tying the two together. If `GET /ui` looks
  wrong after a webui/ change, this is the first thing to check.
  - Mitigated, not eliminated: `/status.daemon.buildHash` is a hash of the actually-
    embedded bundle bytes (`internal/tn/ui.go`'s `computeWebUIBuildHash`), rendered in the UI's
    own footer. Compare it against a fresh `pnpm build` if you suspect staleness — a
    mismatch confirms it instantly. If `webui/dist/` has *some* files but isn't a real
    Vite build (no `assets/` dir), `GET /ui` serves a loud "run pnpm build" page instead
    of attempting to serve a broken bundle. Neither catches a genuinely EMPTY
    `webui/dist/` (e.g. someone deletes its contents) — that's a Go `//go:embed`
    COMPILE-time failure ("no matching files"), not a runtime one, and deliberately left
    that way: it's already a clear, immediate, self-explanatory error rather than a
    silent one, so there's nothing to engineer around here — just re-run `pnpm build`.
- `webui/dist/` is committed (not gitignored) precisely so a fresh checkout can
  `go build` immediately without needing Node/pnpm at all — `pnpm build` is only
  required when you've actually changed something under `webui/src/`.
- Go itself stays stdlib-only: `embed` is stdlib, and the daemon's own build/run path
  never invokes `npm`/`pnpm`. `webui/`'s own toolchain (React, TanStack Router,
  shadcn/ui, TypeScript, Vite) never becomes a Go module dependency.

- Go 1.26, **stdlib only** — no external dependencies, keep it that way.
- Tests: httptest + injected `spawnFunc` — tests must never touch tmux or the real
  vault/state. Functional smoke runs use explicit high ports (18xxx) and a scratch
  `$HOME`/dashboardPath, never the real config or vault.
- Set `TN_NO_SELFRESTART=1` when starting the real daemon in tests/smoke runs.

## File map

| File | Contents |
|---|---|
| `cmd/tn/main.go` | Entrypoint (package `main`): calls `tn.Run` |
| `internal/tn/api.go` | TaskNotes API client (config resolve, task CRUD, query, webhooks list/create) |
| `internal/tn/main.go` | CLI dispatch + usage text (package `tn`) |
| `internal/tn/serve.go` | Daemon state model, `Server` type, config resolution, entrypoint (`newMux`, `cmdServe`) |
| `internal/tn/{handlers,orphans,stranded,sleep,workers,feed,status,ui,webhook,due_scanner,mr_watcher,webhook_reconciler,spawn,spawn_reconciler,debounce,stuck,needs_action,dashboard,session_pages,selfrestart,sse,repo_settings}.go` | The rest of the daemon, split by topic (HTTP handlers, webhook routing, spawn/reconcile, stuck-session detection, dashboard rendering, self-restart, etc.) — see `ARCHITECTURE.md`'s code map for the one-line purpose of each |
| `internal/tn/bridge.go` | Bridge client (register/inbox/ack/send/agents subcommands) |
| `internal/tn/note_layout.go` | Structured task-note body layout (ask/brief/description/history blocks, migration) — see `docs/design/SPEC-note-layout.md` |
| `internal/tn/reaper.go` | Worktree reaper: removes finished `.claude/worktrees/agent-*` worktrees when clean + pushed + unlocked + old enough (never `--force`, branch `-d` only when merged), `/status.worktrees`, `POST /worktrees/reap`, `tn worktrees` — see `docs/design/SPEC-serve.md` "Worktree reaper". Tests build real temp git repos |
| `internal/tn/creds.go` | claude.ai credential profiles: Keychain abstraction (`keychainStore`, real impl shells out to `/usr/bin/security`), profile save/list/use/rm, usage-limit auto-swap + parked-pane nudge, `GET /creds` / `POST /creds/swap`, `tn creds` CLI — see `docs/design/SPEC-serve.md` "Credential profiles & auto-swap". Tests inject a fake keychain; never call `security` |
| `internal/tn/*_test.go` | See conventions above |
| `webui/` | React + TanStack Router + shadcn/ui SPA (TypeScript, Vite) — `GET /ui`'s content. Own toolchain, own build step (`pnpm build`), never a Go dependency. `webui/embed.go` (package `webui`) exports `Dist embed.FS` (`//go:embed all:dist`), imported by `internal/tn/ui.go` — see Build/test above |
| `obsidian-plugin/` | Thin Obsidian plugin: an iframe pointed at the daemon's own `/ui`, plus a postMessage bridge for opening vault notes and syncing Obsidian's theme into the iframe — see `docs/design/SPEC-ui-v2.md` |
| `docs/design/SPEC-serve.md`, `docs/design/SPEC-dashboard.md`, `docs/design/SPEC-generations.md`, `docs/design/SPEC-note-layout.md`, `docs/design/SPEC-ui-v2.md`, `docs/design/SPEC-obsidian-plugin.md`, `docs/design/IMPLEMENTATION-PLAN-REMOTE-EXECUTION.md` | Implementation specs (history; code is source of truth) |
| `ORCHESTRATOR.md` | Operating contract read by daemon-spawned headless sessions |
| `skills/tasknotes/` | Claude skill, exposed via this repo being a plugin |
| `.claude-plugin/` | Plugin + local-marketplace manifests (marketplace `tasknotes-local`, user-scope install) |
| `hooks/claude-hook.sh` | Claude Code `PermissionRequest`/`PermissionDenied` hook script — one script for both events, user-installed into their own `~/.claude/settings.json` (never written by the daemon), POSTs to `POST /hooks/permission-request` or `/hooks/permission-denied` — see `docs/design/SPEC-serve.md`'s "PermissionRequest hook detection" section |

## Runtime layout (this machine)

- Binary `~/bin/tn`; TaskNotes API `http://localhost:8390` (token in
  `~/.config/tn/config.json`); bridge `http://localhost:8391`.
- Daemon config `~/.config/tn/serve.json` (dashboardPath, per-project autoSpawn/cwd);
  state `~/.local/state/tn/state.json`; logs `~/Library/Logs/tn-serve.log`.
- Credential profiles: secrets live in the Keychain (Claude's own item is service
  `Claude Code-credentials`; tn's saved profiles are service `tn-credential-profiles`,
  account = label); non-secret metadata `~/.local/state/tn/creds.json`. serve.json
  `credentials.autoSwap` (default off) enables automatic swapping on usage limits —
  a swap switches EVERY Claude session on this machine.
- LaunchAgent `~/Library/LaunchAgents/com.example.tn-serve.plist` (KeepAlive) runs the
  daemon; restart manually with `launchctl kickstart -k gui/$UID/com.example.tn-serve`.
  launchd puts the daemon in macOS's background QoS tier (throttled I/O, coalesced timers
  — 20× slower git, 2-min tickers firing hourly overnight); `cmdServe` lifts itself out
  with `taskpolicy -B -p <pid>` at startup (`leaveBackgroundTier`, reaper.go). Check the
  log for `left background QoS tier` after a restart.
- Dashboard note: `~/Documents/MyVault/Claude Sessions.md` (generated — never edit).
- Tray-app foundation: `GET /status` (JSON, spectator-safe — never marks messages
  delivered or bumps LastSeenAt) and `GET /ui` (self-contained HTML status page,
  polls `/status` every 5s). `daemonVersion` (serve.go, currently `"0.7.0"`) is
  surfaced by both plus `/health` — bump it whenever you make a notable change to
  either endpoint's shape or the daemon's observable behavior generally.

## Adding a new project (repo) to the bridge

1. **serve.json**: add under `projects` in `~/.config/tn/serve.json`:
   ```json
   "<slug>": { "autoSpawn": true, "cwd": "/absolute/path/to/repo" }
   ```
   Slug lowercase; each spawn becomes a new generation `orchestrator-<slug>-g<gen>`, tmux
   session `tn-<slug>-g<gen>` (see SPEC-generations.md), and the config key is matched
   case-insensitively.
2. **Reload the daemon**: `touch ~/bin/tn` — the self-restart watcher picks it up within
   ~10s (config is only read at startup). Confirm via `~/Library/Logs/tn-serve.log`.
3. **Repo note (for the Obsidian picker)**: create
   `~/Documents/MyVault/Repos/<slug>.md` — the `claude-project` user field's
   autosuggestion filter (required tag `repo`) offers it as a dropdown entry; the
   wikilink normalizes to the slug (path + brackets + case stripped), so the filename
   must equal the slug. Template:
   ```markdown
   ---
   tags:
     - repo
   repo-path: /absolute/path/to/repo
   max-orchestrators: 1
   max-workers: 3
   ---

   # <slug>

   <one-line description>.
   Slug: `<slug>` → orchestrator generations `orchestrator-<slug>-g<gen>`, tmux `tn-<slug>-g<gen>`.
   ```
   `max-orchestrators`/`max-workers` are optional (both keys, and the whole file, can be
   omitted — defaults are 1 and unset respectively) and read live with a 60s cache
   (`repoSettingsFor`), so editing them in Obsidian takes effect without a daemon
   restart. `max-orchestrators` caps concurrently-accepting generations for this
   project; `max-workers`, when set, overrides any serve.json env entry of the same
   name and feeds the reconciler's scale-out math.
4. **Route a task to it**: set the task's `claude-project` field to the slug (or repo
   note link), tag it `claude`/`spawn` or write an `@claude:` line → daemon spawns the
   orchestrator (`--permission-mode auto`) if none is alive. Routing fallback when the
   field is absent: first `projects[]` entry, normalized.
5. Spawn troubleshooting: reconciler retries stranded queues every 30s; spawn errors in
   the daemon log; binaries are resolved absolutely (launchd PATH has no Homebrew).

## Conventions & gotchas

- Task id in URLs = vault-relative path, `url.PathEscape`d in full (slashes too).
- List/query responses nest under `data.tasks`; single get under `data` — parse defensively.
- `PUT /api/tasks/:id` is partial; tags/projects/contexts arrays REPLACE on update.
- Bridge liveness = inbox poll within 90s (`aliveWindow`); poll IS the heartbeat.
- Message statuses: queued → delivered (on inbox poll) → acked. Dashboard "Queued"
  column counts queued+delivered.
- All state/dashboard writes go through `atomicWriteFile` (tmp+rename) — keep it that way;
  Obsidian reads the dashboard file live.
- Spawned sessions get `TN_HEADLESS=1` and a prompt pointing at `ORCHESTRATOR.md`.
  Keep prompt text, `buildOrchestratorPrompt`/`buildLaunchCommand` tests, ORCHESTRATOR.md,
  and the skill consistent when changing the protocol.
- Never edit the TaskNotes plugin's `data.json` while assuming it took effect — Obsidian
  loads it at plugin start; changes need a plugin reload and can be overwritten by a
  running Obsidian.
- Changing default ports/paths: sweep code, usage text, README, skill, ORCHESTRATOR.md
  together (defaults live in `resolveConfig`/`resolveServeConfig`/`resolveBridgeURL`).
- **Registering a test/verification agent against the REAL running daemon** (proving a
  fix works live, not against a throwaway `httptest` server): name it with a `zz-`
  prefix (`testVerificationAgentPrefix` in serve.go). `/status` excludes anything under
  that prefix from every project's agent list, so it never shows up in the webui/tray
  looking like a real generation — but it's still registered in `state.Agents`
  underneath, so purge it when you're done: `curl -X POST localhost:8391/agents/purge
  -d '{"name":"zz-..."}'`. Unlike `/agents/deregister` or a normal retire (which only
  tombstone — `DeregisteredAt` set, the record kept forever, same as a real generation),
  `/agents/purge` actually deletes the record. Live incident that established this: a
  leftover `zz-worker-verify` test registration, plus two other ad-hoc test agents that
  predated the convention, sat in the Projects view indefinitely looking like retired
  generations — `/agents/purge` is also the general fix for a name that never used the
  prefix at all.
- **Warn before a live verification produces a USER-VISIBLE artifact** — a real
  permission prompt, a desktop notification, a tray alert, anything that shows up
  outside your own tool output. Tell whoever's coordinating BEFORE you run it, not in
  the report afterward. Live incident (2026-08-26): a `zz-`prefixed test agent's tmux
  pane was set up to show a genuine "Do you want to proceed?" prompt as part of
  verifying the PermissionRequest hook end to end — correct methodology, real signal —
  but the user saw the prompt land, asked what it was, and got answered before the
  verification's own report had even arrived, based on a guess about which of two
  recent spikes caused it. No harm this time, but a prompt appearing from an
  unexplained name is exactly the kind of thing that erodes trust in alerting that's
  been deliberately built to be trustworthy. The `zz-` prefix and `/agents/purge`
  handle cleanup; they don't cover the window between "this is now visible to a human"
  and "here's what it was."
- **The moment you tell a user to reference a path, that path is a published interface,
  not an internal detail — renaming it is a breaking change.** Live incident
  (2026-08-26): `hooks/permission-request.sh`'s own header comment told users to put an
  absolute path to that file into their `~/.claude/settings.json`. The user did, via
  team-lead. This agent then renamed the file to `hooks/claude-hook.sh` (a genuinely
  better name, once the script grew to handle two hook events) without checking whether
  anyone had actually installed the old path first. The user's hook pointed at a file
  that no longer existed and would have failed SILENTLY — no error, no crash, just a
  hook that never runs, a daemon that receives nothing, and the stuck detector quietly
  falling back to pane-scraping with nobody aware anything had regressed. That is the
  exact "absent thing is indistinguishable from working thing" failure shape this whole
  project has spent two days eliminating elsewhere — caught here only because team-lead
  happened to check. **Before renaming or moving anything a human may have installed
  (a script referenced by absolute path in its own install instructions, a config file,
  a binary), grep for its current path in the place a human would have put it** —
  `~/.claude/settings.json` for a hook script, `~/.config/tn/` for daemon config, etc.
  — the same few seconds it takes to grep is what the fork-isolation and
  user-visible-artifact rules above already ask for elsewhere. The currently-installed
  path for this specific script is `hooks/claude-hook.sh`; if it moves again, update
  the user's `~/.claude/settings.json` (or confirm with whoever coordinates that it's
  already been updated) in the SAME change, not after.
- **Before `go build -o ~/bin/tn .` (or anything else that replaces or restarts the
  live daemon), confirm you're authorized for THAT deploy in THAT task — read the
  task's own brief BEFORE building, not after.** The running daemon is shared
  infrastructure: every orchestrator and worker on this machine depends on it, and
  replacing the binary on disk MAY force an immediate restart via the LaunchAgent's
  KeepAlive whether or not you meant to trigger one — but this is NOT reliable (see
  the correction two paragraphs below: the same day this rule was written, a second
  deploy left the old process running for 86+ minutes with the new binary already on
  disk). Either way, there is no "build without deploying" GUARANTEE on this machine
  short of not touching `~/bin/tn` at all — treat every build as a potential deploy,
  whether or not it turns out to have actually restarted anything. Live incident
  (2026-08-26): a task's brief explicitly said "the worker will NOT rebuild or restart
  the live tn daemon... the swap is a deploy decision for you, not part of the fix" —
  the worker built and codesigned anyway, read the brief only afterward, and the
  daemon restarted mid-task with 16 live agents connected. No agents were actually lost
  that time, but the outcome being fine doesn't make the sequence right: the second
  instance in one day (after the hook-path rename above) of taking an action whose
  blast radius extends beyond your own workspace before checking who else is affected.
  Disclosing it plainly afterward is right and expected — but disclosure is the
  backstop, not the control. The control is reading the task brief for explicit deploy
  constraints BEFORE the build, every time, not just when something reminds you to.
  **The check is not just "am I authorized to deploy" — it's "who is depending on this
  binary RIGHT NOW."** A follow-up fix the same day (a 409 error-message wording
  improvement, functionally optional — the doc-only half of the same change already
  covered the actual risk with no deploy needed at all) was deliberately held rather
  than shipped immediately, because the live answer at that moment was "two
  orchestrators mid-sweep on work the user is waiting on, each with an active
  long-poll a restart would interrupt." Authorization and current dependents are two
  different questions — a green light from earlier in the day doesn't mean nobody's
  depending on the binary staying up right now. When a change doesn't need to go out
  immediately, split it at the deploy boundary (doc/config changes that don't need a
  binary swap ship instantly; the part that needs `go build`/`codesign` waits and gets
  batched with the next change that actually requires a restart) rather than bundling
  an optional deploy with an urgent one just because they landed in the same session.
- **"I built and signed it" and "the fix is live" are two different claims — verify
  the RUNNING version, never the file's mtime.** Live incident (2026-08-26, same day
  as the two entries above): after an authorized deploy, `~/bin/tn` had a fresh mtime
  and the build/codesign steps had reported success, but `/status.daemon.version` still
  read the OLD version with an uptime that predated the build — the new binary was on
  disk and the OLD process was still serving it unchanged. This directly contradicts
  what the entry above states about `codesign -f` forcing a restart: sometimes it does
  (confirmed live, an earlier deploy the same day), sometimes it doesn't (confirmed
  live, this one) — the mechanism is not reliable enough to trust either way. This is
  the exact "a change that exists but is not in effect, indistinguishable from one that
  is" failure shape the whole project keeps tripping on, just relocated to the deploy
  step itself. **After every deploy: `curl localhost:8391/status` and check
  `daemon.version` against what you just bumped `daemonVersion` to, and `daemon.uptime`
  against how long ago you actually ran the build** — not "did the command exit 0",
  which tells you the binary compiled and got signed, nothing about whether it's
  running. If the version/uptime don't match, the daemon didn't pick it up:
  `launchctl kickstart -k gui/$UID/com.example.tn-serve` to force it. This is also
  why the version-bump discipline (`daemonVersion`, bumped on every notable change —
  see "Runtime layout" above) is load-bearing beyond its own stated purpose: without a
  version that actually changes on every deploy, there would be no way to distinguish
  a stale running binary from a fresh one at all, and a "verified" fix could be
  reported working when it was never actually running.
- **When a fix has a deliberate settling delay (a startup grace period, a background
  sweep that waits before acting, anything async), send an interim status line the
  moment you deploy — don't just go quiet until it's fully settled.** Live incident
  (2026-08-27): after an authorized deploy of a fix with a ~90s startup-grace wait
  (`sweepStrandedMessagesAtStartup`, deliberately reusing the same wait
  `sweepOrphanedOwnershipDebt` already uses, for the same false-positive-avoidance
  reason), the coordinator checked results mid-wait, saw the headline case still
  unresolved, and reported it as a bug — spending real effort on two wrong hypotheses
  before the mechanism finished on its own and the numbers matched exactly. Nothing
  was actually wrong; the report just arrived before the described delay had elapsed,
  because no one had said "there is a described delay." One line — "deployed, sweep
  waits out 90s by design, will report settled numbers" — would have prevented the
  whole detour. This is the same "absence of signal is not signal of absence" theme
  this project keeps hitting, just at the level of human-to-human status updates
  instead of agent liveness: an unreported in-flight wait reads identically to nothing
  having happened at all. State the delay up front, not just the eventual result.
