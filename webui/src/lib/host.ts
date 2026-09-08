// host.ts: the one thing that differs between "embedded in Obsidian" and
// "standalone browser tab" — opening a vault note. Everything else in the
// app is identical code for both hosts (SPEC-ui-v2.md: "Same code, both
// hosts").

export function isObsidianHost(): boolean {
  return new URLSearchParams(window.location.search).get('host') === 'obsidian'
}

// openTask asks the host to open a task note. Inside Obsidian's iframe,
// the plugin wrapper listens for this postMessage and calls
// app.workspace.openLinkText — an internal navigation, not a URL handoff.
// Standalone in a browser, there is no plugin listening, so this falls
// back to an obsidian:// deep link instead (best-effort: requires
// Obsidian to be running and the URL handler enabled, same as any other
// obsidian:// link).
export function openTask(path: string, vaultName?: string): void {
  if (isObsidianHost()) {
    window.parent.postMessage({ type: 'open-task', path }, '*')
    return
  }
  if (!vaultName) {
    console.warn('openTask: no vaultName known for the obsidian:// fallback; path was', path)
    return
  }
  const withoutExt = path.endsWith('.md') ? path.slice(0, -3) : path
  const url =
    'obsidian://open?vault=' + encodeURIComponent(vaultName) + '&file=' + encodeURIComponent(withoutExt)
  window.open(url, '_self')
}

// openTaskUI asks the host to open a task in TASKNOTES' OWN edit UI
// (its modal), not the raw markdown note. Inside Obsidian, the plugin
// wrapper tries app.plugins.plugins.tasknotes.openTaskEditModal and has
// its own fallback chain (TaskNotes missing/disabled/throws ->
// openLinkText) — see obsidian-plugin/main.js. A standalone browser host
// has no Obsidian plugin to ask, so there is no modal to open there; it
// falls back straight to the same obsidian:// link openTask uses, same
// as today, rather than a dead click.
export function openTaskUI(path: string, vaultName?: string): void {
  if (isObsidianHost()) {
    window.parent.postMessage({ type: 'open-task-ui', path }, '*')
    return
  }
  openTask(path, vaultName)
}

// handleTaskClick centralizes the modifier-click convention used by every
// clickable task row: plain click opens TaskNotes' own edit UI (falls
// back safely if TaskNotes is missing/disabled), Cmd/Ctrl-click always
// opens the raw markdown note — still needed for editing the reply
// block/history, which TaskNotes' modal doesn't expose.
export function handleTaskClick(e: { metaKey: boolean; ctrlKey: boolean }, path: string, vaultName?: string): void {
  if (e.metaKey || e.ctrlKey) {
    openTask(path, vaultName)
  } else {
    openTaskUI(path, vaultName)
  }
}
