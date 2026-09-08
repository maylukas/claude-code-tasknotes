import path from 'node:path'
import { tanstackRouter } from '@tanstack/router-plugin/vite'
import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// https://vite.dev/config/
export default defineConfig({
  // tanstackRouter must run before react() — it generates routeTree.gen.ts
  // from src/routes/ that the app's root import depends on.
  plugins: [tanstackRouter({ target: 'react', autoCodeSplitting: true }), react(), tailwindcss()],
  resolve: {
    alias: {
      '@': path.resolve(import.meta.dirname, './src'),
    },
  },
  // Emits into dist/, which Go's embed picks up (see ../serve.go). Never
  // hashed-into-a-CDN paths that assume a domain root — the daemon serves
  // this from /ui, both standalone and inside the Obsidian iframe.
  base: '/ui/',
  build: {
    outDir: 'dist',
  },
})
