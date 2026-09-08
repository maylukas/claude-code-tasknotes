package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestParseRepoSettings covers frontmatter parsing directly: defaults,
// both keys present, junk values, and content with no frontmatter fences
// at all.
func TestParseRepoSettings(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    repoSettings
	}{
		{"empty content", "", repoSettings{MaxOrchestrators: 1, MaxWorkers: 0}},
		{"no frontmatter fences", "# Just a heading\nmax-orchestrators: 3\n", repoSettings{MaxOrchestrators: 1, MaxWorkers: 0}},
		{"both keys set", "---\ntags:\n  - repo\nmax-orchestrators: 3\nmax-workers: 5\n---\n\n# myapp\n", repoSettings{MaxOrchestrators: 3, MaxWorkers: 5, MaxOrchestratorsPresent: true, MaxWorkersPresent: true}},
		{"only max-orchestrators", "---\nmax-orchestrators: 2\n---\n", repoSettings{MaxOrchestrators: 2, MaxWorkers: 0, MaxOrchestratorsPresent: true}},
		{"only max-workers", "---\nmax-workers: 4\n---\n", repoSettings{MaxOrchestrators: 1, MaxWorkers: 4, MaxWorkersPresent: true}},
		// The key WAS present (that's the whole point of tracking it —
		// "junk value" and "genuinely absent" must be distinguishable),
		// even though the junk value itself is ignored and the resolved
		// count stays at its default.
		{"junk value ignored, default kept", "---\nmax-orchestrators: banana\n---\n", repoSettings{MaxOrchestrators: 1, MaxWorkers: 0, MaxOrchestratorsPresent: true}},
		{"zero and negative ignored", "---\nmax-orchestrators: 0\nmax-workers: -1\n---\n", repoSettings{MaxOrchestrators: 1, MaxWorkers: 0, MaxOrchestratorsPresent: true, MaxWorkersPresent: true}},
		{"unrelated keys ignored", "---\ntags:\n  - repo\nrepo-path: /some/path\n---\n", repoSettings{MaxOrchestrators: 1, MaxWorkers: 0}},
		{"content after closing fence ignored", "---\nmax-orchestrators: 2\n---\nmax-workers: 9\n", repoSettings{MaxOrchestrators: 2, MaxWorkers: 0, MaxOrchestratorsPresent: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseRepoSettings(c.content)
			if got != c.want {
				t.Errorf("parseRepoSettings(%q) = %+v, want %+v", c.content, got, c.want)
			}
		})
	}
}

// TestReadRepoSettings_MissingFile verifies a missing repo note yields
// plain defaults rather than an error.
func TestReadRepoSettings_MissingFile(t *testing.T) {
	got := readRepoSettings(t.TempDir(), "no-such-repo")
	want := defaultRepoSettings()
	if got != want {
		t.Errorf("got %+v, want defaults %+v", got, want)
	}
}

// TestReadRepoSettings_RealFile verifies the vaultDir/Repos/<slug>.md path
// construction and a real on-disk read+parse round trip.
func TestReadRepoSettings_RealFile(t *testing.T) {
	vaultDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vaultDir, "Repos"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\ntags:\n  - repo\nmax-orchestrators: 2\nmax-workers: 4\n---\n\n# myapp\n"
	if err := os.WriteFile(filepath.Join(vaultDir, "Repos", "myapp.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got := readRepoSettings(vaultDir, "myapp")
	want := repoSettings{MaxOrchestrators: 2, MaxWorkers: 4, MaxOrchestratorsPresent: true, MaxWorkersPresent: true}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestRepoSettingsCache_TTL verifies the cache reuses a value within TTL
// (fetch called once) and re-fetches once TTL has elapsed, using an
// injected clock rather than a real sleep.
func TestRepoSettingsCache_TTL(t *testing.T) {
	c := newRepoSettingsCache()
	var fetchCalls int
	fetch := func() repoSettings {
		fetchCalls++
		return repoSettings{MaxOrchestrators: fetchCalls, MaxWorkers: 0}
	}

	base := time.Now()
	first := c.get("myapp", base, repoSettingsCacheTTL, fetch)
	if first.MaxOrchestrators != 1 || fetchCalls != 1 {
		t.Fatalf("expected first call to fetch, got %+v (fetchCalls=%d)", first, fetchCalls)
	}

	// Within TTL: cached, no re-fetch.
	within := c.get("myapp", base.Add(30*time.Second), repoSettingsCacheTTL, fetch)
	if within.MaxOrchestrators != 1 || fetchCalls != 1 {
		t.Fatalf("expected the cached value reused within TTL, got %+v (fetchCalls=%d)", within, fetchCalls)
	}

	// Past TTL: re-fetches.
	after := c.get("myapp", base.Add(61*time.Second), repoSettingsCacheTTL, fetch)
	if after.MaxOrchestrators != 2 || fetchCalls != 2 {
		t.Fatalf("expected a re-fetch past TTL, got %+v (fetchCalls=%d)", after, fetchCalls)
	}
}

// TestRepoSettingsFor_NoDashboardReturnsDefaults verifies a server with no
// DashboardPath configured (no vault to read from) returns plain defaults
// rather than erroring or panicking.
func TestRepoSettingsFor_NoDashboardReturnsDefaults(t *testing.T) {
	srv, _ := newTestServer(t) // newTestServer's cfg has no DashboardPath
	got := srv.repoSettingsFor("myapp")
	if got != defaultRepoSettings() {
		t.Errorf("expected defaults with no dashboard configured, got %+v", got)
	}
}

// --- GET /repos (tn repo) ---

func doRepoSettingsRequest(srv *Server, slug string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/repos?slug="+slug, nil)
	w := httptest.NewRecorder()
	srv.handleRepoSettings(w, req)
	return w
}

// TestHandleRepoSettings_UnknownSlug404 verifies a slug that isn't a
// configured project 404s rather than returning misleading defaults —
// repoSettingsFor alone would silently return defaults for it, which
// would misrepresent an unconfigured project as "configured, just with
// nothing set."
func TestHandleRepoSettings_UnknownSlug404(t *testing.T) {
	srv := newParallelismTestServer(t, map[string]ProjectConfig{"myapp": {Cwd: "/repos/myapp", AutoSpawn: true}}, nil)
	w := doRepoSettingsRequest(srv, "not-a-real-project")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// TestHandleRepoSettings_NoRepoNote_ReportsUnsetNotZero is the exact
// distinction the endpoint exists for: a configured project with no repo
// note at all must report maxOrchestratorsSet/maxWorkersSet false — not
// just resolve to the same defaults a "present but invalid" repo note
// would, which is the ambiguity that cost the live TN_MAX_WORKERS trace
// its certainty.
func TestHandleRepoSettings_NoRepoNote_ReportsUnsetNotZero(t *testing.T) {
	srv := newParallelismTestServer(t, map[string]ProjectConfig{
		"myapp": {Cwd: "/repos/myapp", AutoSpawn: true, Env: map[string]EnvEntry{"TN_MAX_WORKERS": {Value: "4"}}},
	}, nil)

	w := doRepoSettingsRequest(srv, "myapp")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got repoSettingsView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Slug != "myapp" || got.RepoPath != "/repos/myapp" || !got.AutoSpawn {
		t.Errorf("expected identity fields to reflect the real project config, got %+v", got)
	}
	if got.MaxOrchestratorsSet || got.MaxWorkersSet {
		t.Errorf("expected both *Set false with no repo note at all, got %+v", got)
	}
	if got.MaxOrchestrators != 1 {
		t.Errorf("expected the plain default 1, got %d", got.MaxOrchestrators)
	}
	if got.MaxWorkers != 0 {
		t.Errorf("expected maxWorkers 0 (repo note doesn't set it — the field is about the REPO NOTE'S value, not the serve.json fallback), got %d", got.MaxWorkers)
	}
	if got.MaxWorkersServeJSONFallback != "4" {
		t.Errorf("expected the serve.json fallback surfaced separately as %q, got %q", "4", got.MaxWorkersServeJSONFallback)
	}
}

// TestHandleRepoSettings_RepoNoteSet_ReportsSetTrue verifies the other
// side: a repo note that DOES set max-workers reports it resolved AND
// flagged Set=true.
func TestHandleRepoSettings_RepoNoteSet_ReportsSetTrue(t *testing.T) {
	srv := newParallelismTestServer(t, map[string]ProjectConfig{"myapp": {Cwd: "/repos/myapp", AutoSpawn: true}}, nil)
	writeRepoNote(t, srv, "myapp", 2, 5)

	w := doRepoSettingsRequest(srv, "myapp")
	var got repoSettingsView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.MaxOrchestrators != 2 || !got.MaxOrchestratorsSet {
		t.Errorf("expected maxOrchestrators=2 set=true, got %+v", got)
	}
	if got.MaxWorkers != 5 || !got.MaxWorkersSet {
		t.Errorf("expected maxWorkers=5 set=true, got %+v", got)
	}
}
