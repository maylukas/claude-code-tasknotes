# Jira integration

Let the daemon prompt an agent to keep a linked Jira issue's status and comments in sync with a task's TaskNotes status, without ever giving the daemon Jira credentials of its own.

## The two fields

`tn create`/`tn update` accept two custom properties:

- `--jira K[,K2]`: one or more comma-separated Jira issue keys, under normal transition policy. Use this for ordinary Bug/Story-type issues the daemon should ask an agent to transition as the task progresses.
- `--jira-nc K`: a single Jira issue key treated as a regulated record. The daemon will only ever ask an agent to **comment** on it, never to transition its status. Closure of a nonconformity-style record is meant to stay a deliberate human/agent decision, not something the bridge nudges along automatically.

A task can carry either, both, or neither.

## What triggers a request

The daemon has no Jira credentials of its own; there is no HTTP client to a Jira instance anywhere in this codebase. It can only *ask*: whenever it observes a task carrying `jira` and/or `jira-nc` make a **genuine status change** into `in-progress`, `review`, or `done` (a task's first-ever observation only seeds its last-known status, so it never fires a request on daemon startup or when the feature is first enabled, and repeated observations of the same status are no-ops), it emits one bridge message per issue key asking an agent to act:

- For `jira` keys: a transition request.
- For `jira-nc`: a comment-only request, explicitly worded to say the status must not change and pointing at the MR URL if the task has one.

Each request is a genuine bridge message routed to the task (bypassing normal owner routing, so it reaches whichever agent is best positioned to act even if the task's usual owner is draining). It names a Jira status **category** to aim for (`indeterminate` for `in-progress`/`review`, `done` for `done`), never a specific transition or status name, since those are per-project workflow details only an agent with actual Jira access can resolve (via a transitions-listing tool call). The request text explicitly warns against picking a same-category status that isn't the right one (a rejection/withdrawal state sharing the "done" category, for example) and tells the agent to refuse and ask on the task rather than force a mismatched transition.

Deduplication is one-shot per `(task, issue key, target category)`: once a request has been sent for that combination, it won't be sent again until the task's status changes to something else first (which re-arms it for the next genuine transition into a status of interest).

## Per-project named status mapping

If you know a specific project's Jira workflow well enough to name exact status strings, configure `jiraStatusMap` on that project in `serve.json`:

```json
{
  "projects": {
    "myapp": {
      "autoSpawn": true,
      "cwd": "/path/to/myapp",
      "jiraStatusMap": {
        "in-progress": "In Development",
        "review": "Code Review",
        "done": "Merged"
      }
    }
  }
}
```

When a mapping exists for the status the task just moved to, the request names that exact target status instead of just a category, and explicitly tells the agent multi-hop transitions are expected (a Jira project's real "done"-category status may be several workflow states past what this task status actually means) while still listing categories of status to never land on. A task status with no entry in the map (or no map configured at all) falls back to the safer, less precise category-only request.

## What the agent needs

The daemon only sends a plain-text bridge message. Acting on it, actually calling Jira, is the receiving agent's job, using its own Jira MCP server or CLI tooling (not anything bundled in this repository). The request text names the tool call it expects (a transitions-listing tool, so the agent can pick the right one rather than guessing).

## See also

- [Reference: note layout](../reference/note-layout.md): where the daemon's own status-change notes land on the task.
- [Explanation: lifecycle of a task](../explanation/lifecycle-of-a-task.md)
