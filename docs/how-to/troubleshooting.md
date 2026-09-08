# Troubleshooting

Symptom-first lookup for common problems, followed by short sections with more detail.

## Quick reference

| Symptom | Check | Fix |
|---|---|---|
| `tn` commands fail with "TaskNotes API not reachable" | Is Obsidian open? Is the HTTP API enabled in TaskNotes plugin settings? | Enable it; confirm the port matches `~/.config/tn/config.json`. See [Set up TaskNotes for tn](set-up-tasknotes.md). |
| Task changes in Obsidian don't trigger orchestrator activity | Is `tn serve` running? Did the webhook register? | Check the log for `serve: registered webhook ...`; see [Webhook not firing](#webhook-not-firing). |
| A `claude`-tagged task never spawns an orchestrator | `autoSpawn` for that project? `cwd` set? Is spawning paused? | See [Orchestrator not spawning](#orchestrator-not-spawning). |
| Dashboard/web UI shows a stuck prompt, but the pane looks clear when you check | The 2-minute detector may be mid-cycle, or the state hasn't cleared yet | Wait for the next scan, or approve the prompt (a stale approval attempt is detected safely; see below). |
| An agent shows as "gone" that you know is still working | Has it polled its inbox in the last 90 seconds? | Inbox polling *is* the daemon's only liveness signal, so a hung or crashed process reads as gone. Check the session directly (`tmux attach`). |
| `GET /ui` looks wrong after editing `webui/src/` | Did you run `pnpm build` before `go build`? | See [Stale web UI after rebuild](#stale-web-ui-after-rebuild). |
| A flood of permission-approval dialogs right after a rebuild | Is the binary code-signed with a stable identity? | See [Prompt storms after rebuild](#prompt-storms-after-rebuild). |
| The daemon feels sluggish overnight or after the machine was idle | Check for "left background QoS tier" in the log | See [Daemon slow overnight](#daemon-slow-overnight). |
| An orchestrator pane is parked, doing nothing | Look for a claude.ai usage-limit message in the pane | See [Usage limit parked sessions](#usage-limit-parked-sessions) and [Credential profiles](credential-profiles.md). |
| A leftover test agent clutters `/status`/the web UI | Was it registered with a `zz-` prefix? | See [Cleaning up test agents](#cleaning-up-test-agents). |

## Bridge unreachable

Every `tn` client command that talks to the daemon (`register`, `send`, `agents`, `creds`, `worktrees`, etc.) resolves the bridge URL from `TN_BRIDGE_URL`, else `~/.config/tn/serve.json`'s port, else `http://localhost:8391`. If the daemon isn't running or listening on a different port, these fail with a connection-refused error. Confirm the daemon is up:

```bash
curl -s localhost:8391/health
```

If that fails, start (or restart) the daemon; see [Run tn serve as a service](run-as-a-service.md).

## Webhook not firing

The daemon self-registers its webhook URL with TaskNotes at startup, and retries every 60 seconds until it succeeds. This is expected and non-fatal if TaskNotes was briefly unreachable at daemon startup. Confirm registration:

```text
serve: registered webhook http://localhost:8391/webhooks/tasknotes (events=[task.created task.updated task.completed])
```

If instead you see repeated `TaskNotes API not reachable for webhook self-registration, will keep retrying`, TaskNotes itself isn't reachable, the same underlying cause as [Bridge unreachable](#bridge-unreachable) but in the other direction (`tn serve` to TaskNotes, not `tn` to `tn serve`).

If the webhook is registered but events still don't seem to route (a task change in Obsidian produces no daemon activity), the most common cause is the TaskNotes plugin needing a reload after a version change. The daemon deletes and recreates its own webhook registration automatically if it detects a stale/incomplete event list, but a broken plugin-side webhook delivery mechanism itself needs a plugin reload to fix. Even with webhooks fully broken, the [webhook-miss reconciler](../explanation/lifecycle-of-a-task.md) catches up within about 10 minutes, so a routing gap that resolves itself within that window without any action is expected, not a bug.

## Orchestrator not spawning

Check, in order:

1. **`autoSpawn`** is `true` for the project in `serve.json`.
2. **`cwd`** is set and exists.
3. **Spawning isn't paused**: `curl localhost:8391/status | grep spawnPaused`, then `tn resume` if it's paused.
4. **The task actually routes to this project**: see [Add a project](add-a-project.md) step 4 for the routing-slug rules, and confirm with `tn repo <slug>`.
5. **The task is tagged correctly**: `claude`/`spawn` tag, or an `@claude:` line, and (for the tag-based path) status is `open` (or unset). This is a positive allowlist, so `in-progress` and other custom statuses are not auto-assigned either, not just `triage`/`needs-input`/`review`.
6. **tmux and the `claude` binary resolve.** The daemon looks for `tmux` at `/opt/homebrew/bin/tmux`, `/usr/local/bin/tmux`, then `/usr/bin/tmux` (falling back to a bare `tmux` lookup on `PATH` only if none of those exist). Under launchd, `PATH` has no Homebrew directories, so if `tmux` lives somewhere else entirely, spawning fails silently from the daemon's perspective. Similarly, it looks for `claude` at `~/.local/bin/claude`, falling back to a bare `claude` on `PATH`.
7. **The reconciler will retry**: it runs every 30 seconds and re-attempts spawning for any project with queued or startable work but no live accepting orchestrator, so a transient failure (tmux briefly unavailable, for instance) self-heals within that window.

Check the daemon log for `serve: spawn:` lines; they explain exactly which branch of the spawn logic fired (existing session reused, stale session killed and respawned, fresh spawn, or an error from `tmux new-session`/`tmux send-keys`).

## Stuck prompt shown but pane clear

The daemon's periodic pane-scraping check runs every 2 minutes; a prompt can resolve itself (a fast auto-response, or a human who was already watching) in the gap between a scan detecting it and you checking. Approving a stuck prompt, whether via the web UI, the Obsidian plugin, or the native dialog, always re-captures the pane fresh and re-verifies the prompt is still showing before sending a keystroke, so a stale approval attempt on an already-cleared prompt is detected safely and reported back rather than sending an approval into empty air.

If installed, the [Claude Code permission hook](install-claude-hook.md) shortens this gap considerably by reporting a pending prompt the moment Claude Code itself reports one, rather than waiting for the next 2-minute scan.

## "Gone" agents and the 90-second window

An agent is considered alive only if it has polled its inbox (`GET /inbox`) within the last 90 seconds: polling *is* the daemon's heartbeat, and there is no separate liveness ping. A crashed, hung, or network-partitioned agent process reads as "gone" the moment that window elapses, with no other signal available. If you believe an agent is actually still working despite showing as gone, check its tmux session directly (`tmux attach -t <session>`); the daemon's view is only ever as good as the inbox-poll signal.

## Stale web UI after rebuild

See [Web UI development](web-ui-development.md#checking-for-staleness) for the full explanation. Short version: `go build` embeds whatever is currently on disk in `webui/dist/`, with no check against `webui/src/`. Compare `/status.daemon.buildHash` (also shown in the UI's own footer) against a fresh `pnpm build` to confirm.

## Prompt storms after rebuild

macOS's TCC subsystem keys its Apple-Events approval (needed for the daemon's native dialogs) on the binary's code identity. An unsigned, or ad-hoc-signed, rebuild changes identity on every build, which can retrigger the "would like to access data from other apps" approval prompt repeatedly. Sign with a stable identity to avoid this; see [Run tn serve as a service](run-as-a-service.md#4-code-sign-the-binary).

## Daemon slow overnight

Under launchd, a process (and everything it spawns, including `git` and `tmux`) runs in the background QoS tier by default, where disk I/O is throttled hard and timers can be coalesced or deferred while the machine is idle. This can silently degrade both background sweeps (like the worktree reaper) and the liveness-detection timers the daemon depends on. The daemon lifts itself out of this tier once at startup. Confirm it worked:

```text
serve: left background QoS tier (taskpolicy -B -p 12345)
```

If this line is missing from the log, `/usr/sbin/taskpolicy` wasn't found or refused the call. The daemon logs a warning and continues, but background work will run at throttled I/O priority for the life of that process.

## Usage limit parked sessions

An orchestrator pane parked on a claude.ai usage-limit message (`You've hit your session limit · resets ...`) is detected the same way a stuck prompt is. With `autoSwap` enabled, the daemon swaps credentials automatically and nudges the pane to resume; see [Credential profiles](credential-profiles.md). Without `autoSwap`, the session simply waits out the reset time on its own; `/status` and the dashboard both surface a rate-limited state so this is visible rather than looking like a stuck or dead session.

## Cleaning up test agents

If you register a throwaway agent against the real running daemon (to verify a fix live rather than against a test harness), prefix its name with `zz-`. `/status` excludes anything under that prefix from every project's agent list, so it never shows up in the web UI or dashboard looking like a real generation. It's still stored underneath in the daemon's state, though, so remove it when done:

```bash
curl -X POST localhost:8391/agents/purge -d '{"name":"zz-your-test-agent"}'
```

Unlike a normal drain/retire or `/agents/deregister` (which only tombstone a record, kept forever, same as any real generation), `/agents/purge` actually deletes it. This is also the general fix for a leftover test registration that never used the `zz-` prefix in the first place.

## Where logs are

- Running under launchd per [Run tn serve as a service](run-as-a-service.md): wherever `StandardOutPath`/`StandardErrorPath` in your plist point. The guide's example uses `~/Library/Logs/tn-serve.log`.
- Running in a foreground terminal: directly in that terminal's stdout.

## See also

- [Install the Claude Code permission hook](install-claude-hook.md)
- [Worktree reaper](worktree-reaper.md)
- [Credential profiles](credential-profiles.md)
- [Explanation: security model](../explanation/security-model.md)
