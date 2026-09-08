# Your first bridged task

Walk through creating a TaskNotes task that spawns a Claude Code orchestrator, watch it run, and answer it back.

## Prerequisites

- macOS, with [Obsidian](https://obsidian.md) and the [TaskNotes](https://github.com/callumalpass/tasknotes) plugin installed on a vault.
- TaskNotes' HTTP API enabled, with an access token generated (TaskNotes plugin settings). Note the port it listens on (default `8390`). See [Set up TaskNotes for tn](../how-to/set-up-tasknotes.md) for the full TaskNotes-side configuration this tutorial assumes.
- [tmux](https://github.com/tmux/tmux) installed.
- The [Claude Code](https://claude.com/claude-code) CLI installed and logged in (run `claude` once interactively and complete `/login`).
- Go 1.26 to build `tn` from source.

## 1. Build the binary

```bash
git clone <this-repo-url> tasknotes-cli
cd tasknotes-cli
go build -o ~/bin/tn ./cmd/tn
```

Make sure `~/bin` is on your `PATH`. Confirm the binary runs:

```bash
tn help
```

Expected output: the full usage synopsis, starting with `tn - CLI for the Obsidian TaskNotes HTTP API`.

> **macOS note:** if you plan to run `tn serve` as a background service later, see
> [Run tn serve as a service](../how-to/run-as-a-service.md) for code signing:
> not required for this tutorial's foreground run.

## 2. Configure the TaskNotes API client

Create `~/.config/tn/config.json` with the URL and token from TaskNotes' HTTP API settings:

```json
{
  "url": "http://localhost:8390",
  "token": "your-tasknotes-api-token"
}
```

Verify the CLI can reach TaskNotes:

```bash
tn health
```

Expected output: a JSON blob from the TaskNotes API (exact shape depends on your TaskNotes version). If instead you see `tn: TaskNotes API not reachable at http://localhost:8390 — enable HTTP API in TaskNotes settings`, double check the plugin setting and the port.

## 3. Configure the bridge daemon

Create `~/.config/tn/serve.json` with one project. Replace `/path/to/myapp` with any directory on disk you're comfortable spawning a Claude Code session in (it doesn't need to be a real project for this tutorial, just a git repo or any writable directory), and replace `/path/to/vault` with the absolute path to the Obsidian vault where TaskNotes is enabled:

```json
{
  "projects": {
    "myapp": {
      "autoSpawn": true,
      "cwd": "/path/to/myapp"
    }
  },
  "dashboardPath": "/path/to/vault/Claude Sessions.md"
}
```

`dashboardPath` does two things this tutorial relies on: it makes the daemon render a live dashboard note into your vault (step 8), and it's how the daemon locates your vault at all to read the repo note in the next step. Without it set, the repo note below is never read.

## 4. Start the daemon in the foreground

Run it directly in a terminal so you can watch the startup log (setting `TN_NO_SELFRESTART=1` is optional here; it only matters if you later rebuild the binary while this instance is running, so it doesn't exit and expect launchd to restart it):

```bash
TN_NO_SELFRESTART=1 tn serve
```

Expected output includes lines like:

```text
2026/09/02 10:00:00 serve: left background QoS tier (taskpolicy -B -p 12345)
2026/09/02 10:00:01 serve: registered webhook http://localhost:8391/webhooks/tasknotes (events=[task.created task.updated task.completed])
```

The webhook registration line confirms TaskNotes accepted the daemon's callback URL. If instead you see repeated `TaskNotes API not reachable for webhook self-registration, will keep retrying`, the daemon can't reach TaskNotes. Check that Obsidian is open and the HTTP API is enabled.

Leave this terminal running for the rest of the tutorial.

## 5. Create a repo note

With `dashboardPath` set, the daemon knows where your vault is and reads per-project parallelism settings (and the TaskNotes project-field autosuggestion relies on this too) from a note at `Repos/<slug>.md` inside it. Create `Repos/myapp.md`:

```markdown
---
tags:
  - repo
repo-path: /path/to/myapp
max-orchestrators: 1
max-workers: 3
---

# myapp

Tutorial project.
Slug: `myapp` → orchestrator generations `orchestrator-myapp-g<gen>`, tmux `tn-myapp-g<gen>`.
```

## 6. Create a task

In a second terminal:

```bash
tn create --title "Say hello" --claude-project myapp --tag claude
```

Expected output: the bare path of the newly created task, e.g. `TaskNotes/Say hello.md`.

The `--claude-project myapp` flag sets the routing slug directly; the `claude` tag marks it as a task the bridge should act on.

## 7. Watch the orchestrator spawn

Back in the `tn serve` terminal, within a few seconds you should see something like:

```text
2026/09/02 10:05:12 serve: spawn: tmux session tn-myapp-g43210 exists and an agent is alive for it; no-op
```

(the first spawn logs differently, a `tmux new-session`/`tmux send-keys` sequence, but a follow-up spawn attempt logs the no-op line above). Confirm the tmux session exists:

```bash
tmux ls
```

Expected output includes a line like `tn-myapp-g43210: 1 windows...`. The generation number (`g43210` here) is derived from a timestamp, so yours will differ.

You can attach to watch the orchestrator work:

```bash
tmux attach -t tn-myapp-g43210
```

Detach with `Ctrl-b d` without killing the session.

## 8. Open the dashboard

Since you set `dashboardPath` in step 3 (see the full key list in [Add a project](../how-to/add-a-project.md)), a `Claude Sessions.md` note now appears in your vault. Alternatively, with the daemon running, open:

```
http://localhost:8391/ui
```

in a browser. Expected: a status page showing the `myapp` project, its registered agent, and the task moving through statuses (`open` → `in-progress` → likely `review` or `done`, depending on what the orchestrator did with a plain "Say hello" task).

## 9. Watch the task move

```bash
tn get "TaskNotes/Say hello.md"
```

(adjust the path to whatever `tn create` returned). Expected output shows the current `status:`, and once the orchestrator has acted, an `ask:` or `brief:` line if it left you a question or a status update.

## 10. Answer a question

If the orchestrator parks the task on `needs-input` with an `ask:` line, reply directly on the task:

```bash
tn ask "TaskNotes/Say hello.md" --clear
tn note "TaskNotes/Say hello.md" "@claude: please continue" --by you
```

Or, in Obsidian, open the task note and write a line starting with `@claude:` under the "Your reply" callout the CLI maintains. The daemon picks it up on the next webhook or reconciler pass and clears the ask block automatically.

## 11. Finish

Once you're done experimenting, stop the daemon with `Ctrl-C` in its terminal, and retire the orchestrator generation cleanly from inside its tmux session (or just `tmux kill-session -t tn-myapp-g<gen>` for a tutorial cleanup; a real deployment lets the orchestrator call `tn retire` itself).

## Where to go next

- [Add a project](../how-to/add-a-project.md): wire up a real repository.
- [Run tn serve as a service](../how-to/run-as-a-service.md): keep the daemon running under launchd instead of a foreground terminal.
- [Install the Claude Code permission hook](../how-to/install-claude-hook.md): faster stuck-prompt detection than pane scraping alone.
- [Troubleshooting](../how-to/troubleshooting.md): symptom-to-fix table if something above didn't behave as described.
- [Reference: note layout](../reference/note-layout.md): the full anchored-block format behind `tn note`/`tn brief`/`tn ask`.
- [Explanation: lifecycle of a task](../explanation/lifecycle-of-a-task.md): the full routing and status-flip story.
