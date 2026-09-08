# Install the Claude Code permission hook

Wire up Claude Code's `PermissionRequest`/`PermissionDenied` hooks so the daemon learns about a stuck interactive prompt immediately, instead of waiting for its periodic pane-scraping check.

## What the hook does

`hooks/claude-hook.sh` is a single script that handles both the `PermissionRequest` and `PermissionDenied` Claude Code hook events (it reads `hook_event_name` from its own stdin and routes accordingly). It is **telemetry only**:

- It never approves, denies, or answers a prompt itself.
- It exits `0` with empty stdout unconditionally, regardless of what happens internally. Claude Code's documented contract is that hook silence is never approval, and this script must never be the thing standing between a real permission prompt and the human who needs to see it, whether the daemon is up, down, slow, or the script itself fails.
- On `PermissionRequest`, it POSTs to the daemon's `/hooks/permission-request` endpoint, which triggers the same stuck-detection state machine the periodic pane-scraping check uses, just faster, since it fires the moment Claude Code reports a prompt is genuinely pending rather than waiting for the next scheduled scrape.
- On `PermissionDenied`, it POSTs to `/hooks/permission-denied`: pure telemetry, no retry, no decision. It just records that a denial happened (and what command triggered it) for later diagnosis.

A hard `permissions.deny` rule in Claude Code short-circuits before any hook fires at all. This is a real, permanent gap in what the hook can see, not a temporary limitation. Pane-scraping remains the only signal for that case.

## The TN_AGENT_NAME gate

The script only does anything when the `TN_AGENT_NAME` environment variable is set:

```bash
if [ -z "${TN_AGENT_NAME:-}" ]; then
  exit 0
fi
```

`TN_AGENT_NAME` is set by the daemon (`buildLaunchCommand` in `serve.go`) as a literal environment-variable prefix on the exact shell command it types into a spawned orchestrator's tmux pane. An ordinary interactive Claude Code session run directly by a human has no `TN_AGENT_NAME` at all, so this script is a silent no-op for it. **This is what makes a global install (below) safe.**

## Install

Add both hook entries to your `~/.claude/settings.json`, pointing at an **absolute path** to `hooks/claude-hook.sh` in your checkout of this repo:

```json
{
  "hooks": {
    "PermissionRequest": [
      { "hooks": [
        { "type": "command", "command": "bash /absolute/path/to/tasknotes-cli/hooks/claude-hook.sh", "timeout": 3 }
      ] }
    ],
    "PermissionDenied": [
      { "hooks": [
        { "type": "command", "command": "bash /absolute/path/to/tasknotes-cli/hooks/claude-hook.sh", "timeout": 3 }
      ] }
    ]
  }
}
```

Keep the timeout short (3 seconds in the example above). Claude Code appears to wait for a configured hook to return, or time out, before rendering the actual interactive prompt to the human. A slow or hung daemon must never add noticeable latency to a real session.

Two installation scopes:

- **Global** (`~/.claude/settings.json`): every Claude Code session on the machine runs this script on every `PermissionRequest`/`PermissionDenied` event, but the `TN_AGENT_NAME` gate above makes it a no-op for anything not spawned by the daemon. Safe, but only as safe as that guard staying in place; see the warning below.
- **Project-local** (`.claude/settings.json` in the root of each daemon-managed repo): the hook only ever activates in sessions that already have their working directory in a daemon-managed repo. Recommended for a fresh install where you don't specifically want global coverage.

> **Warning:** the `TN_AGENT_NAME` guard is the one thing that makes a global install
> safe. Any change to the script that makes it act (a network call, a side effect,
> anything beyond an immediate exit) when `TN_AGENT_NAME` is unset would affect every
> Claude Code session on the machine, not just daemon-spawned ones. If you're modifying
> the script, keep this check first, unconditional, and unweakened.

## Verify

Wait for a daemon-spawned orchestrator to hit a real permission prompt (or trigger one deliberately in a test session), then check the daemon log:

```bash
tail -f ~/Library/Logs/tn-serve.log   # or wherever tn serve's stdout goes
```

Expect a line like:

```text
serve: hook: permission request pending for orchestrator-myapp-g43210 (tool=Bash, command="git push", prompt=..., mode=default)
```

The first time a given agent's hook successfully reaches the daemon, you'll also see:

```text
serve: hook: <agent-name>'s PermissionRequest hook confirmed working for the first time
```

If the hook fires but the daemon returns 404 (`agent not found`) or 409 (`no resolvable tmux session for agent`), the agent hasn't registered with the bridge yet, or its tmux session name can't be resolved. Check `tn agents` for its registration state.

## Fallback: pane-scrape detection

If the hook isn't installed (or `TN_AGENT_NAME` isn't set, or the daemon simply never receives the POST, say from a slow network or wrong bridge URL), the daemon's periodic pane-scraping check still runs on its own schedule and reaches the same stuck-detection outcome, just with more latency. The hook is a speed optimization on top of a detection mechanism that already works without it; installing it is recommended but not required. See [Troubleshooting](troubleshooting.md) for the pane-scrape cadence and stuck-prompt symptoms.

## See also

- [Troubleshooting](troubleshooting.md)
- [Explanation: security model](../explanation/security-model.md)
