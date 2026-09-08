package tn

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"sort"
	"strings"

	"tasknotes-cli/webui"
)

// --- UI v2: embedded React SPA (SPEC-ui-v2.md) ---
//
// webui.Dist (webui/embed.go) embeds webui/dist — the committed,
// pre-built Vite bundle. Go stays stdlib-only: `embed` is stdlib, and the
// daemon never runs npm/pnpm itself. `webui/dist` must exist and be
// current at `go build` time — see CLAUDE.md's build section for the
// `pnpm build` step and the stale-bundle footgun (the daemon will happily
// embed and serve whatever was there at the last Go build, silently, with
// no version check against webui/'s source).

// webUIDist is webui.Dist rooted at dist/, so paths match exactly what
// Vite emitted (index.html, assets/...) — see vite.config.ts's
// base:'/ui/' and outDir:'dist'.
var webUIDist = mustSubFS(webui.Dist, "dist")

func mustSubFS(f embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		// embed.FS is compiled into the binary; a bad path here is a
		// build-time bug (dir renamed without updating the embed
		// directive), never a runtime condition — fail loudly and early.
		panic("serve: bad embed sub-path " + dir + ": " + err.Error())
	}
	return sub
}

// webUIBuildHash is a short hash of the CONTENT actually embedded in this
// binary (path+bytes of every file under webui/dist, computed once at
// program init) — surfaced via /status.daemon.buildHash and the UI's own
// footer. This is what turns the stale-bundle footgun (CLAUDE.md: `go
// build` embeds whatever's on disk in webui/dist with no check against
// webui/src/) from something you have to remember into something you can
// SEE: if the footer's hash doesn't match what `pnpm build` just produced
// moments ago, the running binary is stale, instantly obvious instead of
// silently serving old UI. Computed from the embedded bytes themselves
// (not a separate metadata file Vite would need to keep in sync) so it
// can never drift from what's actually served.
var webUIBuildHash = computeWebUIBuildHash(webUIDist)

func computeWebUIBuildHash(fsys fs.FS) string {
	var paths []string
	fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	sort.Strings(paths) // deterministic regardless of walk order
	h := sha256.New()
	for _, p := range paths {
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			continue
		}
		h.Write([]byte(p))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// webUIActuallyBuilt reports whether the embedded bundle looks like a
// REAL Vite build rather than an empty/placeholder webui/dist — Vite
// always emits an assets/ directory for this app's script-based SPA
// output, so its absence is a reliable signal that `pnpm build` was never
// actually run (a fresh checkout whose webui/dist somehow ended up with
// nothing but a hand-placed placeholder, or was partially cleaned).
// Deliberately does NOT catch a genuinely EMPTY webui/dist (zero files) —
// that's a Go `//go:embed` COMPILE-time failure ("no matching files"),
// not something a runtime check can paper over; this only covers the
// "dist has *something* but it isn't a real build" case.
func webUIActuallyBuilt() bool {
	_, err := fs.Stat(webUIDist, "assets")
	return err == nil
}

// handleUI implements GET /ui/*: serves the embedded React SPA. Any
// requested path that doesn't exist as a real file in the embedded bundle
// (a client-side TanStack Router route like /ui/task/Foo.md, or a fresh
// deep link) falls back to index.html — the SERVER doesn't know about
// app routes, only the bundled router does, so any unrecognized path is
// assumed to be one of its routes rather than a 404.
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if !webUIActuallyBuilt() {
		// Loud, not silent: a missing real build (fresh checkout with only
		// a placeholder in webui/dist, or a partially-cleaned dist/) would
		// otherwise either fail to compile (empty dir — a genuine go:embed
		// hard limit, nothing to do about that one at runtime) or silently
		// 404/serve garbage. This is the case we CAN catch: dist has files,
		// but not a real Vite build.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `<!doctype html><html><body style="font-family:system-ui;max-width:40em;margin:4em auto;line-height:1.5">`+
			`<h1>Web UI not built</h1>`+
			`<p>The embedded bundle in <code>webui/dist</code> isn't a real build (no `+
			`<code>assets/</code> directory found). Run:</p>`+
			`<pre>cd webui &amp;&amp; pnpm build</pre>`+
			`<p>then rebuild the daemon: <code>go build -o ~/bin/tn .</code></p>`+
			`</body></html>`)
		return
	}
	upath := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/ui"), "/")
	if upath == "" {
		upath = "index.html"
	}
	if _, err := fs.Stat(webUIDist, upath); err != nil {
		upath = "index.html" // SPA fallback for a client-side route
	}

	// Deliberately NOT http.FileServer here: it special-cases any request
	// whose path ends in "index.html" by 301-redirecting to the same path
	// WITHOUT that suffix — exactly what upath is set to on every SPA-
	// fallback and every bare /ui/ request, which turned into an infinite
	// redirect loop (each redirect re-entering this handler, re-triggering
	// the same fallback, redirecting again). http.ServeContent has no such
	// special-casing — it just serves bytes.
	f, err := webUIDist.Open(upath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		// Every embed.FS file implements io.ReadSeeker; this only exists
		// so a broken assumption fails loudly instead of panicking below.
		http.Error(w, "internal error: embedded UI asset is not seekable", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, upath, stat.ModTime(), rs)
}
