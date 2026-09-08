# Add a project

Wire up a new repository so the bridge daemon can route tasks to it and spawn orchestrator sessions.

## Prerequisites

- `tn serve` already configured and reachable (see the [first bridged task tutorial](../tutorials/first-task.md)).
- A local checkout of the repository you want to add.

## 1. Add a serve.json entry

Edit `~/.config/tn/serve.json` and add an entry under `projects`:

```json
{
  "projects": {
    "myapp": {
      "autoSpawn": true,
      "cwd": "/absolute/path/to/myapp"
    }
  }
}
```

The slug (`myapp` here) is matched case-insensitively against whatever routing slug a task resolves to (see step 4). Each spawn for this project becomes a new generation, agent name `orchestrator-myapp-g<gen>`, tmux session `tn-myapp-g<gen>`, where `gen` is a timestamp-derived number.

Other optional keys on a project entry:

| Key | Meaning |
|---|---|
| `autoSpawn` | Whether the daemon may spawn an orchestrator for this project at all. |
| `cwd` | Working directory the orchestrator's tmux session starts in. |
| `env` | Per-project environment variables injected into the spawn command; see the doc comment on `EnvEntry` in `internal/tn/serve.go` for the injected-vs-advisory distinction. |
| `jiraStatusMap` | Optional Jira workflow status names per TaskNotes status; see [Jira integration](jira-integration.md). |

## 2. Reload the daemon

`serve.json` is only read at startup. Two ways to pick up the change:

- Restart `tn serve` directly (`Ctrl-C` then re-run it, or `launchctl kickstart -k gui/$UID/com.example.tn-serve` if you're running it as a service; see [Run tn serve as a service](run-as-a-service.md)).
- If the self-restart watcher is enabled (the default; it's off only when `TN_NO_SELFRESTART=1` is set), touch the binary: `touch ~/bin/tn`. The watcher (`watchBinaryForSelfRestart` in `internal/tn/selfrestart.go`) polls the binary's mtime/size every 10 seconds and exits the process the moment it changes, so a launchd `KeepAlive` supervisor brings it back up within about 10 seconds, re-reading `serve.json` on the new process's startup. A stat error only counts as "changed" after several consecutive failures, so a `go build -o` in-progress rename doesn't trigger a spurious restart.

Either way, confirm the reload happened by tailing the log (`~/Library/Logs/tn-serve.log` if you followed the service how-to, or the terminal it's running in otherwise) for a fresh startup sequence.

## 3. Create the repo note

The daemon reads per-project parallelism settings from a note in your vault at `Repos/<slug>.md`. The filename (minus `.md`) must equal the slug exactly (wikilinks used elsewhere are normalized to match it: brackets, path prefix, and `.md` all stripped, then lowercased).

```markdown
---
tags:
  - repo
repo-path: /absolute/path/to/myapp
max-orchestrators: 1
max-workers: 3
---

# myapp

One-line description of the repo.
Slug: `myapp` → orchestrator generations `orchestrator-myapp-g<gen>`, tmux `tn-myapp-g<gen>`.
```

`max-orchestrators` and `max-workers` are both optional, and the whole file is optional (defaults: 1 accepting generation at a time, no `TN_MAX_WORKERS` override). They're read live with a 60-second cache, so editing them in Obsidian takes effect without a daemon restart; no `touch ~/bin/tn` needed for this step. `max-workers`, when set, overrides any `env` entry of the same name in `serve.json` and feeds the reconciler's scale-out math for `max-orchestrators > 1`.

The `repo` tag is required for TaskNotes' own `claude-project` field autosuggestion to offer this note as a dropdown entry when editing a task in Obsidian. That filtering behavior lives in the TaskNotes plugin itself, not in this daemon.

## 4. Route a task to the project

A task's routing slug resolves in this order:

1. The task's `claude-project` custom property, if set (`tn create`/`tn update --claude-project SLUG`), itself often a wikilink, e.g. `[[Repos/myapp]]`, which is normalized the same way as the repo note's filename.
2. Otherwise, the first entry in the task's `projects[]` array.

Either way, mark the task so the bridge picks it up at all: tag it `claude` or `spawn`, or write a line starting with `@claude:` in its details.

```bash
tn create --title "Fix the thing" --claude-project myapp --tag claude
```

A `claude`/`spawn`-tagged task is only auto-assigned while its status is `open` (or unset, TaskNotes' `""`/`none`). This is a positive allowlist, not just an exclusion of `triage`/`needs-input`/`review`: a task sitting in `in-progress`, or any other custom status, is not auto-assigned either. `triage`, `needs-input`, and `review` are parked for you, and moving the task to `open` is your approval to route it. An `@claude:` line routes in any status, so you can comment on a parked task without reopening it.

## 5. Verify

Check the resolved settings the daemon actually sees:

```bash
tn repo myapp
```

Expected output:

```text
slug:            myapp
repoPath:        /absolute/path/to/myapp
autoSpawn:       true
maxOrchestrators: 1 (set by repo note)
maxWorkers:      3 (set by repo note)
```

`(set by repo note)` and `(unset, default)` distinguish a value the repo note actually configured from one that's just the built-in default; a project with no repo note at all, or one that doesn't set these keys, shows `(unset, default)` instead.

This returns 404 (surfaced by `tn` as an HTTP error) if the slug isn't a configured project in `serve.json` at all; that's a signal the reload in step 2 didn't happen or the slug doesn't match.

Check the full daemon-level view, including any live agents for the project:

```bash
curl http://localhost:8391/status | python3 -m json.tool
```

Look for `myapp` under the `projects` key.

## See also

- [Run tn serve as a service](run-as-a-service.md)
- [Troubleshooting](troubleshooting.md): "orchestrator not spawning" if the task doesn't route as expected.
- [Reference: configuration](../reference/configuration.md)
- [Explanation: lifecycle of a task](../explanation/lifecycle-of-a-task.md)
