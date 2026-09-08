# tn webui

React + TanStack Router + shadcn/ui single-page app. This is the content served at
`GET /ui` by the `tn serve` bridge daemon, which embeds the built `dist/` output
directly into the Go binary via `go:embed` (see `serve.go`) — the daemon never runs
Node or a JS toolchain itself.

```bash
pnpm install
pnpm dev      # Vite dev server with HMR, talks to a running tn serve instance
pnpm build    # emits dist/, which serve.go embeds via `//go:embed all:webui/dist`
pnpm lint
```

`dist/` is committed on purpose so a fresh checkout can `go build` the Go binary
without Node or pnpm installed. Run `pnpm build` and commit the result whenever you
change anything under `src/` — the Go build has no way to know the source changed and
will silently keep embedding whatever is already in `dist/`.

See `../docs/how-to/web-ui-development.md` for the full workflow, including how the
daemon's build-hash check works and how to spot a stale `GET /ui` bundle.
