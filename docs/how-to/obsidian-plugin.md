# Obsidian plugin

Install the TN Dashboard sidebar view, a thin wrapper around the daemon's own web UI.

> **Desktop only.** The plugin manifest sets `isDesktopOnly: true`.

## What it is

The generated dashboard note is rewritten wholesale on every render, so Obsidian re-parses it and your scroll position jumps: workable as a glance-and-leave summary, but not something you can comfortably keep open while reading. The `obsidian-plugin/` directory in this repo is a small, hand-written CommonJS Obsidian plugin (`main.js`, `manifest.json`, `styles.css`, no build step, no bundler, no `node_modules`) that does exactly two things: show the daemon's own `GET /ui` page in an `<iframe>` inside a sidebar view, and give that iframe the one thing it can't do on its own, open a vault note, via `postMessage`. The live-updating behavior itself (polling, no-flicker DOM updates) belongs entirely to the embedded page, covered in [Web UI development](web-ui-development.md); the plugin does not poll anything itself.

The generated markdown dashboard and per-session pages are unaffected by installing this plugin. They keep being written exactly as before, and remain the fallback for when Obsidian is closed.

## Install

The plugin has no build step: install by symlinking the source directory straight into your vault's plugins folder:

```bash
ln -s /absolute/path/to/tasknotes-cli/obsidian-plugin \
      "/path/to/your/vault/.obsidian/plugins/tn-dashboard"
```

Then in Obsidian: **Settings > Community plugins**, and enable "TN Dashboard" (you may need to disable Safe Mode / restricted mode first, since this isn't installed via the plugin marketplace). A change to `main.js` in your checkout takes effect after **Reload app without saving** (or the community Hot Reload plugin); nothing to compile.

## Requirements

- The daemon (`tn serve`) must be running and reachable at the bridge URL configured in the plugin's settings tab (default `http://localhost:8391`) for the embedded page to load and function.
- Desktop Obsidian only.

## Open the view

Use the ribbon icon it adds, or run the command **TN Dashboard: open** from the command palette. It opens in the right sidebar and sets the iframe's `src` to `<bridge URL>/ui?host=obsidian`.

## How note-open works

The embedded page is sandboxed inside the iframe and can't call `app.workspace.openLinkText` itself, so it posts a message instead; the plugin's single `window.addEventListener("message", ...)` handler is what actually performs the navigation. Two message shapes matter:

- `{type: "open-task", path}`: the plugin calls `app.workspace.openLinkText(path, "", false)` directly, genuine internal Obsidian navigation, not an `obsidian://` deep link.
- `{type: "open-task-ui", path}`: the plugin first tries to open the task in TaskNotes' own edit modal. It looks up the TaskNotes plugin instance, calls its documented `api.tasks.get(path)` accessor (falling back to a couple of undocumented, best-effort accessors only if that one is unavailable), and if it gets back a task, calls TaskNotes' `openTaskEditModal`. If any of that fails, returns nothing, or TaskNotes isn't installed, it falls back to the same `openLinkText` call as the plain `open-task` message, so this path never dead-ends.

A third message, `{type: "request-theme"}`, is a load-order race guard: if the embedded page's own message listener attaches after the iframe's `load` event (and the plugin's first theme post) already fired, it can ask for the theme again.

## Theme sync

On iframe load, and again on every Obsidian appearance change, the plugin collects a fixed list of Obsidian CSS custom properties (background/text/accent colors, border colors, the interface font) from `getComputedStyle(document.body)` and posts them to the iframe as `{type: "obsidian-theme", vars: {...}}`. The embedded page remaps its own theme tokens onto these (see `webui/src/lib/theme.ts`), so it matches your current Obsidian theme instead of rendering with its own defaults inside the iframe.

## Settings

The plugin's only setting is a **Bridge URL** field (default `http://localhost:8391`), used to build the iframe's `src`.

## Limitations

No task editing, no rendered note content, and no functionality beyond the iframe embed and the postMessage bridge described above. Everything else, the needs-you list, stuck-prompt actions, per-project agent state, drain/retire controls, is entirely the embedded page's own doing; see [Web UI development](web-ui-development.md) for what it shows and how it updates.

## See also

- [Web UI development](web-ui-development.md): the `/ui` page this plugin embeds.
- [Troubleshooting](troubleshooting.md)
