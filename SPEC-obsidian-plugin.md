# Spec: TN Dashboard — an Obsidian plugin

Replaces the generated `Claude Sessions.md` note as the primary live view. The note is
rewritten wholesale every render, so Obsidian re-parses it and the reader's scroll
position jumps — unusable while actually reading it. A plugin polls the daemon directly
and mutates the DOM in place, so nothing scrolls or flickers.

**The markdown dashboard and session pages STAY exactly as they are for now.** They are
the fallback while the plugin proves itself, and they work when Obsidian is closed.
Do not remove or change the daemon's rendering of them in this work.

## Placement and install

- Source lives in this repo: `obsidian-plugin/` (main.js, manifest.json, styles.css).
- Installed by symlinking that directory to
  `~/Documents/MyVault/.obsidian/plugins/tn-dashboard`, the same pattern the
  repo already uses for the Claude skill. No build step, no bundler, no node_modules:
  hand-written CommonJS `main.js` against Obsidian's API, so a change is live after
  "Reload app without saving" (or the hot-reload plugin) with nothing to compile.
- `manifest.json`: id `tn-dashboard`, name "TN Dashboard", minAppVersion 1.8.0,
  isDesktopOnly true.

## The view

An `ItemView` (type `tn-dashboard`) that opens in the right sidebar, plus a ribbon icon
and a command "TN Dashboard: open". Polls `GET http://localhost:8391/status` every 5s
while the view is visible and STOPS polling when it is hidden or the workspace leaf is
detached — no background traffic when nobody is looking.

Sections, in this order (matching what the menu-bar app taught us about priority):

1. **Needs you** — `needsActionTasks`, grouped by status with the same glyphs the tray
   uses (🔴 triage, 🟠 needs-input, 🟣 review). Each row is clickable and opens that task
   in the main pane via `app.workspace.openLinkText(path, "", false)` — an internal
   navigation, NOT an `obsidian://` URL. Show the `ask` or `brief` text underneath in
   muted type, and an MR link and integration branch when present.
2. **Stuck prompts** — agent name, the captured excerpt in a `<pre>`, and buttons
   Attach / Allow once / Deny. Allow and Deny POST to `/agents/approve`; Attach cannot
   run tmux from inside Obsidian, so it copies `tmux attach -t <session>` to the
   clipboard and shows a Notice saying so.
3. **Projects** — per project: task counts by status, then a row per live agent with
   state badge (accepting / draining / stuck / unacked backlog), context %, owned in-progress
   count, and Drain / Retire buttons (`/agents/drain`, `/agents/retire`). Retire asks
   for confirmation naming the agent and its owned-task count; Drain does not.
4. **Footer** — daemon version, uptime, port, spawn-paused banner with a Pause/Resume
   toggle (`/spawn/pause`), and a red banner when the fetch fails, with the same
   "bridge unreachable" wording the tray uses.

## The requirement that actually matters: no flicker

Never rebuild the container's innerHTML on refresh. On each poll, reconcile:

- Keep a map of row key → HTMLElement (agent name, task path, project slug).
- Update text and class on existing nodes; create nodes only for genuinely new keys;
  remove only nodes whose key has disappeared.
- Never touch a node whose rendered content is unchanged — that is what preserves both
  scroll position and any text selection the reader has made.
- Verify explicitly: scroll the view down, let it poll several times, confirm the scroll
  offset does not move and a selected range survives.

## Non-goals for v1

No task editing, no note rendering, no settings tab beyond a bridge URL field, and no
attempt to replace the tray. This view answers "what needs me, what is running, what is
stuck" and offers the same four actions the tray already offers.

## Verification

- Manual: open the view, confirm all four sections render against the live daemon;
  confirm the no-flicker test above; confirm each button's effect via `/status` or
  `tn agents` afterwards; confirm the view stops polling when hidden (daemon log or a
  temporary console counter).
- Confirm the markdown dashboard is still being written and still correct — this work
  must not disturb it.
