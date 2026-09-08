# Set up TaskNotes for tn

Configure the Obsidian TaskNotes plugin itself so `tn` and `tn serve` can talk to it, route tasks correctly, and use the optional integrations.

Most of this repo's other docs assume "install TaskNotes and enable its HTTP API" is the whole story. It isn't: routing, the note-layout fields, and a few optional features all depend on TaskNotes-side configuration that's easy to miss. This page covers that configuration once, so the rest of the docs can link back to it instead of repeating it.

## 1. Install TaskNotes

Install [TaskNotes](https://github.com/callumalpass/tasknotes) as an Obsidian community plugin, on desktop. No minimum version is pinned anywhere in this codebase; this repo is developed and tested against TaskNotes 4.12.x. TaskNotes' HTTP API has existed since TaskNotes 3.19, so any reasonably current install should work, but the newer the better. The HTTP API (and therefore everything `tn` does) is desktop-only; TaskNotes' mobile app doesn't expose it.

## 2. Enable the HTTP API

In TaskNotes' settings, open the **HTTP API** tab and turn it on. You'll set a port there.

TaskNotes' own default port is `8080`, but `tn`'s default (`resolveConfig` in `internal/tn/api.go`) is `8390`. Pick one of two approaches:

- Set TaskNotes' HTTP API port to `8390` so `tn`'s built-in default just works, or
- Leave TaskNotes on whatever port you prefer and put that port in `~/.config/tn/config.json`'s `url` field instead (see [Reference: configuration](../reference/configuration.md)).

TaskNotes also lets you set an authentication token for the API. It's optional on TaskNotes' side, but recommended: `tn` always sends it as `Authorization: Bearer <token>` when one is configured (`internal/tn/api.go`'s `request()`), and without a token the API accepts unauthenticated requests from anything on your machine.

Restart Obsidian after enabling the HTTP API for the setting to take effect.

Test it directly, then through `tn`:

```bash
curl http://localhost:8390/api/health
tn health
```

Both should return a JSON health response. If `curl` fails, the HTTP API setting or port is the problem, not `tn`.

## 3. Configure custom statuses

`tn`'s routing, gating, and status displays all key off specific status **values**, not display labels. Under TaskNotes' **Settings > Statuses**, make sure these values exist:

| Status | What it means to tn |
|---|---|
| `open` (or unset/`none`) | Assignable: a `claude`/`spawn`-tagged task is only auto-assigned while its status is `open`, unset, or `none`. This is a positive allowlist, not just an exclusion of the parked statuses below. Recommended as the default status for new tasks. |
| `triage` | Parked for you. Appears in `/status`'s needs-your-action list and its dedicated triage summary; not auto-assigned. |
| `in-progress` | Normal in-flight work. Not auto-(re)assigned via the tag path (an `@claude:` line still routes regardless of status). |
| `needs-input` | Parked for you. Set by `tn ask` alongside the task's "Needs you" note block; cleared automatically the moment you reply with an `@claude:` line. |
| `review` | Work awaiting merge. The MR watcher checks every non-completed task (not just this status) and can transition a `review` task to `done` when the linked MR merges, or back to `in-progress` if the MR closes without merging. A task parked in `needs-input`/`triage` whose MR merges or closes is only noted, never auto-transitioned. |
| `done` | Terminal. Set by `tn done` (routed through the bridge's optional DONE MEANS evidence gate) or the daemon's `POST /tasks/close`. Mark this status **completed** in TaskNotes' own status settings (verified live against TaskNotes 4.12.3): this is what makes TaskNotes' own completion/archiving behavior, and its `task.completed` webhook event, fire correctly. |

Every value above is case-sensitive and must match exactly what the code checks for; a status named "Done" instead of `done` (TaskNotes lets you set a separate display label, so this is easy to get wrong) won't be recognized.

## 4. Add user fields

Under **Settings > User fields**, add a custom field for each key below. The **key** must match exactly (this is what ends up in a task's `customProperties`); the display name is free text and doesn't need to match anything. Use type **text** for all of them.

| Key | Used by | Required? |
|---|---|---|
| `claude-project` | Bridge routing (which project a task belongs to) | **Required** for the bridge to route any task at all |
| `mr` | MR watcher (GitLab merge-request or GitHub pull-request URL) | Optional |
| `integration-branch` | Workstream target branch | Optional |
| `integration-role` | Workstream ownership (`owner` holds the consolidated MR) | Optional |
| `jira` | Jira transition requests | Optional, see [Jira integration](jira-integration.md) |
| `jira-nc` | Jira comment-only requests (regulated record) | Optional, see [Jira integration](jira-integration.md) |

Only `claude-project` is required to use the bridge at all. Everything else is an optional feature you can add later, if and when you need it. `tn create`/`tn update` write all six of these into a task's `customProperties`.

## 5. Tags

- The `claude` or `spawn` tag on a task routes it to an agent. Either name works interchangeably.
- The `repo` tag on repo notes (`Repos/<slug>.md`) is a convention used by [Add a project](add-a-project.md), not a TaskNotes setting you must change. Optionally, TaskNotes' project-field autosuggestion can be filtered to require a tag (**Settings > project field**); if you set that up, use `repo` so repo notes show up in the picker.
- TaskNotes' own task-identifying tag (the one that marks something as a task at all, `task` by default) stays whatever you already have configured. `tn` doesn't depend on it or read it.

## 6. Webhooks

Nothing to configure by hand here. On startup, `tn serve` registers its own webhook subscription with TaskNotes (`POST /api/webhooks`, watching `task.created`, `task.updated`, and `task.completed`), retrying every 60 seconds until it succeeds, and replaces a stale registration automatically if it's missing an event the current daemon version needs.

TaskNotes generates a secret for the webhook and can sign its payloads, but `tn` does not verify that signature; see [Explanation: security model](../explanation/security-model.md) for the full picture of what this daemon does and doesn't authenticate. To see the current registration yourself: `curl` TaskNotes' own `GET /api/webhooks` (with your token, if you set one), or watch the daemon log for a line starting with `serve: registered webhook`.

## 7. Dependencies (optional)

`tn create --blocked-by` writes TaskNotes' native `blockedBy` field (one `{"uid", "reltype": "FINISHTOSTART"}` entry per value), and `tn list --startable` filters on TaskNotes' `dependencies.isBlocked` query property. No TaskNotes setting is required beyond having a version of TaskNotes that supports task dependencies at all.

## 8. Optional: the Obsidian plugin

The daemon's own web UI (`GET /ui`) can also be embedded directly in Obsidian's sidebar. This isn't required, it's a convenience on top of everything above; see [Obsidian plugin](obsidian-plugin.md).

## Verify

1. `tn health` returns a JSON health response (confirms the HTTP API and your `config.json` are both correct).
2. Create a task exercising the fields you just configured:
   ```bash
   tn create --title "Set up check" --claude-project myapp --tag claude
   ```
3. Open the resulting note in Obsidian and confirm `claude-project` (under whatever display name you gave it) shows up in the note's frontmatter with the value `myapp`.
4. With `tn serve` running, check its log for a `serve: registered webhook` line, confirming the whole chain (HTTP API, token, webhook registration) works end to end.

> **Caution:** TaskNotes stores its own settings, including the HTTP API configuration, custom statuses, and user fields, in `<vault>/.obsidian/plugins/tasknotes/data.json`. Don't hand-edit this file while Obsidian is running: Obsidian only loads it at plugin start, so a running instance can silently overwrite a manual edit, and any change still needs a plugin reload (or an Obsidian restart) to take effect either way. Make these changes through TaskNotes' own Settings UI instead.

## See also

- [Your first bridged task](../tutorials/first-task.md)
- [Add a project](add-a-project.md)
- [Jira integration](jira-integration.md)
- [Reference: configuration](../reference/configuration.md)
- [Explanation: security model](../explanation/security-model.md)
