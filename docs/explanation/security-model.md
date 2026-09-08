# Security model

An honest description of what `tn` and `tn serve` trust, what they don't, and what that means for how you run them. See [`SECURITY.md`](../../SECURITY.md) to report a vulnerability.

## The threat model, stated plainly

Both the TaskNotes plugin's HTTP API (`:8390`) and the bridge daemon (`:8391`, `tn serve`) bind to `127.0.0.1` and accept requests with **no authentication of any kind**. This is a deliberate simplicity choice for a single-user, single-machine tool, not an oversight, but it means **any local process running as your user can call any endpoint**, full stop. There is no token, no origin check, no per-caller identity. A malicious or compromised local process (a browser extension with local-network access, another user's process if the machine is shared, a dependency in an unrelated project you run on the same machine) has the same access to both APIs that `tn` itself does.

If that's a problem for your environment, don't run either service on a machine you don't fully trust, and don't expose either port beyond localhost. There's no configuration flag that changes the bind address away from loopback; that would need a code change.

## What the sensitive endpoints actually do

Some of the daemon's endpoints have real, physical-world side effects once you consider that anyone on the machine can call them:

| Endpoint | What it does | Why it matters |
|---|---|---|
| `POST /creds/swap` | Replaces the shared macOS Keychain item every Claude Code session on the machine reads its OAuth credentials from | Switches **every** running Claude session, interactive ones included, to a different claude.ai account, and disconnects Remote Control, on a single unauthenticated call |
| `POST /tasks/close` | Marks a task done (after checking any `DONE MEANS:` evidence gate) | Writes to your TaskNotes vault on behalf of whatever called it, no attribution beyond whatever the message text says |
| `POST /agents/approve` | Sends `1`/`2` (or nothing, for deny) as literal keystrokes into a specific tmux pane | This is the same action as a human clicking a permission dialog: a caller can approve or deny whatever an orchestrator is currently blocked on |
| `POST /hooks/permission-request`, `POST /hooks/permission-denied` | Accept structured telemetry from Claude Code's own hook system | Unauthenticated by the same design as everything else: anything on the machine can post fabricated hook events claiming to be a given agent name |

None of these are behind any additional check beyond "the daemon is reachable." Treat the daemon's port as equivalent in sensitivity to a shell running as your user, because in practice it grants comparable capability.

## Secrets handling

- **TaskNotes API token** lives in `~/.config/tn/config.json` in plain text (or as the `TASKNOTES_TOKEN` environment variable). Anything that can read your home directory can read it.
- **claude.ai credentials** live in the macOS login Keychain, one shared item every Claude Code session on the machine reads. `tn creds save <label>` copies that item's contents into a second, labeled Keychain item (service `tn-credential-profiles`); the secret itself never touches disk; only non-secret bookkeeping (which label is active, last-used/last-rate-limited timestamps) lives in `~/.local/state/tn/creds.json`. Writing a Keychain item (`security add-generic-password -U -w <secret>`) passes the secret as a command-line argument, which is **briefly visible in `ps` output to other processes running as your user**: this is exactly how Claude Code itself writes the same item, not a shortcut unique to `tn`.
- **`GET /projects/env`** (the daemon's listing of configured per-project environment variables) never returns a value, only whether an entry is `injected` or `advisory`, and its description. Injected values themselves are read from `serve.json` only at the moment the daemon builds a spawn command; they don't appear in bridge responses, dashboard renders, or session pages.

## The permission model of spawned sessions

Daemon-spawned orchestrator sessions run with `claude --permission-mode auto`: an unattended session can never answer an interactive prompt, so `auto` approves calls Claude Code judges safe and denies risky ones with an error the session is expected to react to (flipping the task to `needs-input` per `ORCHESTRATOR.md`, rather than the daemon making that call). The daemon's own hooks (`hooks/claude-hook.sh`, installed by you into `~/.claude/settings.json` or a project-local `.claude/settings.json`, never written by the daemon itself) are **telemetry only**: `POST /hooks/permission-denied` records a denial for visibility and decides nothing; `POST /hooks/permission-request` speeds up detection of a genuinely pending prompt but still routes to the same human-facing dialog (or `POST /agents/approve`) that pane-scraping alone would have shown eventually. A hook response is never treated as an approval: Claude Code's own documented contract is that hook silence approves nothing, and this project doesn't override that anywhere.

Three distinct refusal layers exist upstream of the daemon entirely (Claude's own model-level refusal, a hard `permissions.deny` rule, and a Stage-2 classifier verdict), and hooks see none of the first two. This is a structural limitation of Claude Code's own hook coverage, not something `tn` chooses to skip; `PermissionDenied` telemetry is how a real classifier-driven denial gets captured passively when it happens, not a mechanism for catching every possible refusal.

## On a shared machine

Don't. Every point above assumes a single trusted user on the machine. If you need multi-user isolation, put the daemon behind something that enforces it at the OS or network layer. This project doesn't implement authentication, authorization, or per-user isolation anywhere, and retrofitting it isn't a configuration change.

## Reporting

See [`SECURITY.md`](../../SECURITY.md) for how to report a vulnerability.
