// Package webui embeds the built web UI bundle (dist/, emitted by `pnpm
// build` and committed on purpose so a fresh checkout can `go build`
// immediately without Node/pnpm) so the daemon can serve it from a single
// binary. See CLAUDE.md's build section for the stale-bundle footgun: `go
// build` embeds whatever is on disk in dist/ at that moment, with no check
// against webui/src/.
package webui

import "embed"

//go:embed all:dist
var Dist embed.FS
