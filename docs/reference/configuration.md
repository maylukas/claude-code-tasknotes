# Configuration reference

Exhaustive reference for every configuration file, environment variable, and file path `tn` and `tn serve` read or write.

## Client config

`~/.config/tn/config.json` configures the TaskNotes API connection used by task commands (`tn list`, `tn get`, `tn create`, and so on):

```json
{ "url": "http://localhost:8390", "token": "your-token" }
```

Both keys are optional; a missing file is not an error. Precedence (first match wins):

1. Environment variables `TASKNOTES_URL` / `TASKNOTES_TOKEN`.
2. `~/.config/tn/config.json` (`url` / `token` keys: only non-empty values override).
3. Default `http://localhost:8390`, no token.

Env variables are applied *after* the file, so they always win.

## Daemon config

`~/.config/tn/serve.json` configures `tn serve`. A missing file yields an empty project map (nothing auto-spawns) and the dashboard disabled; the daemon still starts and serves its API.

### Top level

| Key | Type | Default | Description |
|---|---|---|---|
| `port` | int | `8391` | Bridge daemon listen port. See [Ports](#ports) for full precedence. |
| `projects` | object, `{slug: ProjectConfig}` | `{}` | Per-project configuration. Slugs are lowercased on load and matched case-insensitively. |
| `dashboardPath` | string | `""` (disabled) | Vault path to render the dashboard note to. No code-level default, purely opt-in. |
| `sessionPagesDir` | string | if empty and `dashboardPath` is set: `<dir of dashboardPath>/Claude Sessions`; otherwise `""` (disabled) | Directory for per-session rendered pages. |
| `orchestratorDoc` | string | resolved (see the note under [Files and directories](#files-and-directories)) | Path to `ORCHESTRATOR.md`, the operating contract given to spawned orchestrator sessions. Overrides the default lookup; `TN_ORCHESTRATOR_DOC` overrides this key. |
| `credentials` | object | `{"autoSwap": false, "minSwapInterval": "10m"}` | See [Credentials](#credentials). |
| `worktrees` | object | see [Worktree reaper](#worktree-reaper) | Worktree reaper settings. |

`tn serve`'s own `--port` flag, then env `TN_BRIDGE_PORT`, then this file's `port` key, then the built-in default `8391` (see [Ports](#ports)).

### `projects.<slug>`

| Key | Type | Default | Description |
|---|---|---|---|
| `autoSpawn` | bool | `false` | Whether the daemon spawns an orchestrator automatically when a message routes to this project with no live accepting agent. |
| `cwd` | string | `""` | Absolute path to the project's working directory, used as the spawn location. |
| `env` | object, `{name: EnvEntry}` | none | Per-project environment entries injected into the spawned orchestrator's environment. See [Env entries](#env-entries). |
| `jiraStatusMap` | object, `{taskStatus: jiraStatusName}` | none | Maps a TaskNotes task status (e.g. `in-progress`, `review`, `done`) to this project's actual Jira workflow status *name* to transition to, not a status category. Configurable per project because different Jira projects and issue types have different workflow graphs: a project's "board done" status may sit several transitions before its true category-done status, reached only after a later step like acceptance testing or release. A task status with no entry in this map (including an entirely unconfigured map) falls back to a safer, less precise category-only Jira transition request. |

### Env entries

Each value in a project's `env` map may be either a plain JSON string, or an object:

```json
{
  "env": {
    "GITHUB_TOKEN": "ghp_...",
    "STAGING_URL": { "description": "set via 1Password CLI before spawning; not stored here" }
  }
}
```

- **String form** (`"GITHUB_TOKEN": "ghp_..."`) is shorthand for `{"value": "ghp_..."}`.
- **Object form** (`{"value": "...", "description": "..."}`): both fields optional.
- An entry with a **non-empty `value`** is actually injected into the spawned orchestrator's process environment.
- An entry with an **empty (or absent) `value`** is *advisory only*: it is never injected. Its `description` exists to tell a human or agent how to obtain that value at use time (for example, a keychain command to run). This is enforced in code: only entries with a non-empty value are copied into the map handed to the spawn command.

`tn env [--project P]` lists entries with an `injected`/`advisory` status column, but never prints the values themselves, even for injected entries.

### Credentials

| Key | Type | Default | Description |
|---|---|---|---|
| `autoSwap` | bool | `false` | Off by default: a swap switches *every* Claude Code session on the machine, not just the rate-limited one. |
| `minSwapInterval` | duration string (e.g. `"10m"`) | `"10m"` | Minimum time between automatic swaps. A rate-limit hit within this window of the last swap does not trigger another one; the dialog notes why. Only applied if it parses and is greater than zero; otherwise the default stands. |

When `autoSwap` is on and a usage-limit is detected on a pane: the active profile is marked limited until its reported reset time, the oldest-used eligible profile is swapped in, and every other currently rate-limited pane is nudged to resume (they share the account, so they parked together and resume together). See [../how-to/credential-profiles.md](../how-to/credential-profiles.md).

### Worktree reaper

| Key | Type | Default | Description |
|---|---|---|---|
| `reap` | bool | `false` | Master on/off for the periodic background sweep. Independently overridable off via env `TN_NO_REAPER=1`. A manual `tn worktrees --apply` (or `POST /worktrees/reap`) works regardless of this setting. |
| `dryRun` | bool | `true` | When true, the periodic sweep reports and counts but never removes anything or deletes branches. Does not affect `tn worktrees --apply`, which is always an explicit apply regardless of this setting. |
| `minAge` | duration string | `"2h"` | A worktree directory must be older than this before it's even considered for removal (this protects a worktree between creation and its first commit). Only applied if it parses and is non-negative. |
| `interval` | duration string | `"30m"` | How often the periodic sweep runs. Only applied if it parses and is positive. |
| `maxRemovePerPass` | int | `20` | Cap on removals in a single pass. Only applied if positive. |

A worktree is only considered under `<repo>/.claude/worktrees/`. It's removable only if: not locked by a live process, older than `minAge`, `git status --porcelain` clean, and its HEAD commit exists on some remote branch. A local branch is only deleted with `git branch -d` (never `-D`) when it's merged into the default branch; `git worktree remove` is never forced. See [../how-to/worktree-reaper.md](../how-to/worktree-reaper.md).

Not configurable (hardcoded): an initial 2-minute delay before the first pass after startup, a 30-minute wall-clock budget per project's pass, a 3-minute timeout per git query, and a 20-minute timeout specifically for `git worktree remove` (raised after real removes were killed mid-delete on a large worktree).

### Full annotated example

```json
{
  "port": 8391,
  "dashboardPath": "~/Documents/MyVault/Claude Sessions.md",
  "sessionPagesDir": "~/Documents/MyVault/Claude Sessions",
  "credentials": {
    "autoSwap": true,
    "minSwapInterval": "10m"
  },
  "worktrees": {
    "reap": true,
    "dryRun": false,
    "minAge": "2h",
    "interval": "30m",
    "maxRemovePerPass": 20
  },
  "projects": {
    "myapp": {
      "autoSpawn": true,
      "cwd": "/path/to/myapp",
      "env": {
        "GITHUB_TOKEN": "ghp_...",
        "STAGING_URL": { "description": "fetch via 1Password CLI before spawning" }
      },
      "jiraStatusMap": {
        "in-progress": "In Development",
        "review": "Code Review",
        "done": "Merged"
      }
    }
  }
}
```

## Repo notes

An optional per-project note at `<vault>/Repos/<slug>.md` (where `<vault>` is the directory containing `dashboardPath`; there is no separate vault-root config key, so repo notes are unreachable if `dashboardPath` is unset). Frontmatter:

```yaml
---
tags:
  - repo
repo-path: /path/to/myapp
max-orchestrators: 1
max-workers: 3
---
```

> **Note:** Only `max-orchestrators` and `max-workers` are actually read by the daemon. `tags` and `repo-path` are consumed by the Obsidian TaskNotes plugin's own autosuggestion filter (for the `claude-project` field picker). The Go code parses frontmatter with a small hand-rolled line parser that recognizes only those two keys and silently ignores everything else, including `repo-path`.

| Key | Type | Default | Description |
|---|---|---|---|
| `max-orchestrators` | int | `1` | Caps concurrently-accepting orchestrator generations for this project. A value that fails to parse as a positive integer is treated as absent (falls back to the default), but is still recorded as "set" for `tn repo`'s unset-vs-default distinction (see below). |
| `max-workers` | int | unset (`0`) | Overrides any `TN_MAX_WORKERS` env entry for this project when set to a positive integer, and feeds the reconciler's scale-out math. |

Both keys, and the whole file, may be omitted. Settings are cached for **60 seconds**: short by design, so editing the note in Obsidian takes effect without a daemon restart. `tn repo <slug>` (or `GET /repos?slug=`) reports whether each value came from the note or from a default, since the resolved number alone can't distinguish "explicitly set to 1" from "never set."

## Environment variables

### Read by `tn` / `tn serve`

| Variable | Effect |
|---|---|
| `TASKNOTES_URL` | Overrides the TaskNotes API base URL. Highest precedence. |
| `TASKNOTES_TOKEN` | Overrides the TaskNotes API bearer token. Highest precedence. |
| `TASKNOTES_AGENT` | Attribution name used by `tn note` when `--by` isn't given. Falls back to the literal `"claude"` if unset. |
| `TN_BRIDGE_URL` | Overrides the bridge base URL used by client commands (`tn agents`, `tn send`, `tn inbox`, and so on). Default `http://localhost:8391`. Has no effect on `tn serve`'s own listen port. |
| `TN_BRIDGE_PORT` | Overrides `tn serve`'s own listen port. Applied after `serve.json`'s `port` key, before the `--port` flag. |
| `TN_ORCHESTRATOR_DOC` | Path to `ORCHESTRATOR.md` for spawned orchestrator sessions. Highest precedence, above `serve.json`'s `orchestratorDoc`. |
| `TN_NO_REAPER` | `"1"` disables the periodic worktree-reaper sweep (same effect as `worktrees.reap=false`, already the default). |
| `TN_ENABLE_ORPHAN_SWEEP` | `"1"` enables the orphaned-agent-death sweep. Off by default after a confirmed false positive (a live agent was once wrongly declared dead). |
| `TN_NO_SCANNER` | `"1"` disables the due-task scanner. |
| `TN_NO_MRWATCH` | `"1"` disables the MR-state watcher. |
| `TN_NO_REPAIR` | `"1"` disables all three startup cleanup passes (poisoned-dedup-state repair, orphaned-ownership-debt sweep, stranded-message sweep). One variable gates all three. |
| `TN_NO_RECONCILER` | `"1"` disables the spawn reconciler. |
| `TN_NO_STUCKCHECK` | `"1"` disables stuck-session *detection* entirely (not just the popup; see `TN_NO_DIALOG` for that narrower knob). |
| `TN_NO_DIALOG` | `"1"` suppresses only the macOS approval popup, while detection stays active. |
| `TN_NO_SELFRESTART` | `"1"` disables the self-restart-on-binary-change watcher; set this for foreground/dev runs, tests, and smoke runs. |
| `USER` | Fallback account-name source for the macOS Keychain lookup, when `user.Current()` fails. |

> **Warning:** `TN_NO_SELFRESTART=1` should always be set when starting `tn serve` for tests or smoke runs against a real environment: without it, the daemon watches its own binary and restarts itself on change.

### Injected into spawned orchestrator sessions

These are set by the daemon into the environment of a process it spawns; they're never read back by `tn` itself, only by the spawned Claude Code session, `ORCHESTRATOR.md`, and the skill.

| Variable | Value | Description |
|---|---|---|
| `TN_HEADLESS` | `1` | Always set. Signals the daemon-spawned session to follow headless-output conventions. |
| `TN_AGENT_NAME` | the generated agent name | Always set, alongside `TN_HEADLESS`. Also what `hooks/claude-hook.sh` reads to know it should fire at all (see [http-api.md](http-api.md#post-hookspermission-request)). |
| `TN_MAX_WORKERS` | resolved worker cap | Set only when a source provides one: the project's repo-note `max-workers` (wins if present), else the project's `serve.json` `env.TN_MAX_WORKERS` entry as a fallback. |

## Files and directories

| Path | Purpose | Format |
|---|---|---|
| `~/.config/tn/config.json` | TaskNotes API client config | JSON, `{url, token}` |
| `~/.config/tn/serve.json` | Daemon config | JSON, see [Daemon config](#daemon-config) |
| `~/.local/state/tn/state.json` | Daemon state: agents, messages, workers, activity, permission denials | JSON, written atomically (temp file + rename) |
| `~/.local/state/tn/creds.json` | Credential-profile metadata: labels, active profile, last-used/last-limited times | JSON. Contains **no secrets**: only bookkeeping. Same directory as `state.json`. |
| Keychain service `Claude Code-credentials` | Claude Code's own live OAuth blob, shared by every Claude Code session on the machine | macOS Keychain item, account = local username |
| Keychain service `tn-credential-profiles` | `tn`'s saved copies of credential profiles, one item per label | macOS Keychain item, account = profile label |
| `~/.claude.json` | Read-only, for the display email only (`oauthAccount.emailAddress`) | Never written by `tn`: a running Claude Code session rewrites this file wholesale, so `tn` deliberately never touches it otherwise |
| Dashboard note (`serve.json`'s `dashboardPath`, e.g. `~/Documents/MyVault/Claude Sessions.md`) | Rendered, generated dashboard; never hand-edit | Obsidian markdown, regenerated on every render trigger |
| Session pages dir (`serve.json`'s `sessionPagesDir`) | Per-session rendered pages | Obsidian markdown |
| `<vault>/Repos/<slug>.md` | Optional per-project settings note | Obsidian markdown with YAML frontmatter, see [Repo notes](#repo-notes) |

> **Note:** `ORCHESTRATOR.md` (the operating contract read by daemon-spawned sessions) is located at startup, in this order: the `TN_ORCHESTRATOR_DOC` environment variable, `serve.json`'s `orchestratorDoc` key, `~/.config/tn/ORCHESTRATOR.md`, `ORCHESTRATOR.md` next to the `tn` binary, then `./ORCHESTRATOR.md` in the daemon's working directory. The first existing file wins; the daemon logs `serve: orchestrator contract: <path>` at startup, or a warning naming the two ways to set it when none is found. Copy the file from this repository to `~/.config/tn/` or point `orchestratorDoc` at your checkout.

> **Note:** the MR watcher (`TN_NO_MRWATCH` above) invokes the `glab` CLI, resolved from `/opt/homebrew/bin/glab`, `/usr/local/bin/glab`, `/usr/bin/glab`, then `glab` on `$PATH`.

The LaunchAgent plist (macOS service supervisor) is not written by `tn`. It's a user-created file. Logs (for example `~/Library/Logs/tn-serve.log`) exist only if something external redirects the daemon's stdout/stderr to a file. `tn serve` itself never opens or writes a log file; it only calls Go's standard `log` package, which goes to stderr by default. See [../how-to/run-as-a-service.md](../how-to/run-as-a-service.md) for a working LaunchAgent example.

## Ports

| Service | Default | Overrides, in precedence order |
|---|---|---|
| TaskNotes API | `8390` | `~/.config/tn/config.json`'s `url` key → env `TASKNOTES_URL` (client-side only; there is no server-side port here: TaskNotes itself, not `tn`, owns that listener) |
| Bridge daemon | `8391` | `tn serve --port` flag → env `TN_BRIDGE_PORT` → `serve.json`'s `port` key → default `8391` (server-side listen port) |
| Bridge client base URL | `http://localhost:8391` | env `TN_BRIDGE_URL` only: no config-file override on the client side |

The bridge daemon binds to `127.0.0.1` only, never `0.0.0.0` (see [../explanation/security-model.md](../explanation/security-model.md)).
