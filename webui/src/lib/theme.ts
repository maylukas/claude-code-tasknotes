// theme.ts closes a real gap in the naive version of this design: an
// <iframe> is a SEPARATE browsing context with its own independent
// Document, so CSS custom properties defined on Obsidian's OWN document
// (--background-primary, --text-normal, etc., set by whatever theme the
// user has configured) never cross the iframe boundary via ordinary CSS
// inheritance — a bare `var(--background-primary)` inside this app's own
// stylesheet resolves to nothing at all inside the iframe, silently,
// regardless of same-origin status. Only JS can carry values across that
// boundary. The plugin (obsidian-plugin/main.js) reads Obsidian's actual
// computed variable values and posts them in; this listener copies them
// onto THIS document's <html> as inline custom properties — which is what
// makes index.css's `:root[data-host="obsidian"]` block (still written as
// plain `var(--background-primary)` references) actually resolve, since
// those variables now genuinely exist in this document once this runs.
import { isObsidianHost } from './host'

export function listenForObsidianTheme(): void {
  if (!isObsidianHost()) return

  window.addEventListener('message', (event) => {
    const data = event.data
    if (!data || data.type !== 'obsidian-theme' || typeof data.vars !== 'object') return
    const root = document.documentElement
    for (const [key, value] of Object.entries(data.vars as Record<string, string>)) {
      if (typeof value === 'string' && value !== '') root.style.setProperty(key, value)
    }
  })

  // The plugin may have posted its FIRST theme snapshot before this
  // listener was attached (a real race on initial iframe load) — ask for
  // a resend rather than assuming we caught it.
  window.parent.postMessage({ type: 'request-theme' }, '*')
}
