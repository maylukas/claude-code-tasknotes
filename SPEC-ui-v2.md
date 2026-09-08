# Spec: UI v2 — the daemon owns the app, Obsidian embeds it

Supersedes the "plugin renders everything itself" shape in SPEC-obsidian-plugin.md.
User's steer: incorporate the UI into the existing daemon rather than maintaining a
second implementation inside Obsidian.

## Shape

- **The daemon serves the whole UI.** `GET /ui` becomes a real single-page app, not a
  template with a poll loop. Assets are embedded in the Go binary with `embed` (still
  stdlib only) so there is nothing to install and nothing to keep in sync.
- **The Obsidian plugin becomes a thin wrapper**: an `ItemView` containing an
  `<iframe src="http://localhost:8391/ui?host=obsidian">` and nothing else. It stops
  being a second renderer. Roughly fifty lines.
- **A `postMessage` bridge** gives the embedded app the one thing an iframe cannot do:
  open a vault note. The app posts `{type:"open-task", path}`; the plugin calls
  `app.workspace.openLinkText(path, "", false)`. When `host` is not `obsidian`, the app
  falls back to an `obsidian://open?...` link. Same code, both hosts.

Consequences worth stating: the app survives Obsidian restarts, opens on a second
monitor as a normal browser window, and can use any layout it likes — while still
living inside Obsidian for day-to-day use.

## Live updates: SSE, not polling

Add `GET /events` (text/event-stream). The daemon already knows when something changes —
every mutation goes through `s.mu` and already triggers dashboard renders. Push a small
event on each change (`{"type":"status"}` is enough; the client re-fetches `/status`),
plus a heartbeat every 30s so proxies and sleeping laptops do not silently drop the
connection. Client reconnects with backoff. Polling stays as the fallback when the
stream cannot be established.

This also removes the UI from the TaskNotes polling-pressure problem already documented
in SPEC-serve.md: one push beats N clients on timers.

## What the app shows (v2 scope)

1. **Needs you** — as today, but grouped and filterable, each row opening the task.
2. **Orchestrators, with what they are actually doing** — this is the new capability the
   user asked for. Per live agent: state, context %, owned in-progress tasks BY TITLE
   (not just a count), and its current workers. Requires daemon work: `/status` agents
   gain `ownedTasks: [{path,title,status}]`, and worker visibility needs agents to
   report — extend `tn log` usage or add `tn worker start|end --name N --task P` so the
   orchestrator declares its workers and the daemon can show a live tree.
3. **Feed with reply** — merged, newest-first: messages, acks, activity entries, MR
   state changes, Jira requests, sleep events. Each row that belongs to a task links to
   it. A reply box posts to a new `POST /reply {taskPath, text}` which appends an
   `@claude:` line to that task's reply block, so it routes through the existing
   webhook path with no new delivery mechanism. That is the feature the user asked for
   by name: reply from the UI instead of editing the note.
4. **Projects** — counts, integration branch, spawn-pause toggle, drain/retire.

## Constraints

- **Stack, specified by the user: React + TanStack Router + shadcn/ui**, TypeScript,
  built with Vite. Not plain ES modules — this is meant to grow (per-orchestrator views,
  timelines, a reply feed), and hand-rolled DOM does not survive that.
- **Go stays stdlib-only.** The built bundle is committed and embedded via `embed`, so
  the daemon gains no dependencies and still ships as one signed binary. `npm`/`pnpm`
  lives only in `webui/`, never in the daemon's build path.
- **The build must be a deliberate step, not part of `go build`.** `webui/` has its own
  `pnpm build` that emits into `webui/dist/`, which is what `embed` picks up. Document
  it in CLAUDE.md next to the codesign step, and note that forgetting to rebuild the
  bundle is the obvious footgun — the daemon will happily serve a stale UI.
- TanStack Router gives real routes (`/`, `/project/$slug`, `/agent/$name`,
  `/task/$path`), so deep links work in both hosts and the Obsidian iframe can be told
  to open a specific view rather than always the dashboard.
- CSS must work in both hosts: respect `prefers-color-scheme`, and when `host=obsidian`
  inherit Obsidian's CSS variables (`--background-primary`, `--text-normal`, etc.) so it
  does not look pasted in.
- The markdown dashboard and session pages KEEP WORKING throughout. They are the
  fallback when Obsidian and the browser are both closed, and they are what the tray
  links to.

## Verification

- No flicker and no scroll reset under a live stream, same acceptance test as v1.
- The iframe opens a note in Obsidian via the bridge, and the same build opens an
  `obsidian://` link when run standalone in a browser.
- SSE: a change made via `tn` appears in the UI without a poll; killing the daemon shows
  the disconnected state; restarting it reconnects without a page reload.
- Reply: a reply typed in the UI lands as an `@claude:` line on the task and produces a
  routed message to the owning agent — verified end to end, not just an HTTP 200.
