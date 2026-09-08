import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { RouterProvider, createRouter } from '@tanstack/react-router'
import { isObsidianHost } from '@/lib/host'
import { listenForObsidianTheme } from '@/lib/theme'
import { routeTree } from './routeTree.gen'
import './index.css'

// Sets the CSS hook the theme-inheritance block in index.css keys off of
// (`:root[data-host="obsidian"]`) — done here, once, before first paint,
// rather than per-component, since it never changes for the lifetime of
// the page.
document.documentElement.dataset.host = isObsidianHost() ? 'obsidian' : 'standalone'

// Receives Obsidian's actual CSS variable values from the plugin wrapper
// (postMessage — an iframe cannot inherit them any other way, see
// lib/theme.ts) and applies them before first paint, so there's no
// unthemed flash while waiting for the plugin's first message.
listenForObsidianTheme()

// basepath matches vite.config.ts's `base: '/ui/'` — the daemon always
// serves this app mounted at /ui, in both hosts.
const router = createRouter({ routeTree, basepath: '/ui' })

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <RouterProvider router={router} />
  </StrictMode>
)
