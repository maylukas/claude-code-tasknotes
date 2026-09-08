package tn

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// newParallelismTestServer builds a Server with a temp state file AND a
// temp vault (DashboardPath set so repoSettingsFor can find
// <vault>/Repos/<slug>.md), for tests exercising repo-note-driven
// parallelism behavior.
func newParallelismTestServer(t *testing.T, projects map[string]ProjectConfig, spawnFunc func(project, cwd string, env map[string]string) error) *Server {
	t.Helper()
	dir := t.TempDir()
	vaultDir := filepath.Join(dir, "vault")
	if err := os.MkdirAll(filepath.Join(vaultDir, "Repos"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := ServeConfig{Port: 0, Projects: projects, DashboardPath: filepath.Join(vaultDir, "Claude Sessions.md")}
	return newServer(filepath.Join(dir, "state.json"), cfg, spawnFunc)
}

// writeRepoNote writes <vault>/Repos/<slug>.md with the given
// max-orchestrators/max-workers frontmatter (0 = omit that key).
func writeRepoNote(t *testing.T, srv *Server, slug string, maxOrchestrators, maxWorkers int) {
	t.Helper()
	vaultDir := filepath.Dir(srv.dashboardPath)
	content := "---\ntags:\n  - repo\n"
	if maxOrchestrators > 0 {
		content += "max-orchestrators: " + strconv.Itoa(maxOrchestrators) + "\n"
	}
	if maxWorkers > 0 {
		content += "max-workers: " + strconv.Itoa(maxWorkers) + "\n"
	}
	content += "---\n\n# " + slug + "\n"
	if err := os.WriteFile(filepath.Join(vaultDir, "Repos", slug+".md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeStartableTasksAPI serves a fixed task list for every POST
// /api/tasks/query call — used by reconcileSpawnsOnce's startable-work
// query in these tests, which don't care about the exact query shape.
func fakeStartableTasksAPI(t *testing.T, tasks []Task) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{"data": map[string]any{"tasks": tasks, "total": len(tasks), "filtered": len(tasks)}}
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestReconcileSpawnsOnce_ScaleOutWhenSaturated verifies the new
// saturation trigger: one accepting agent, max-orchestrators=2,
// max-workers=1 (repo note), and a startable-task backlog (3) exceeding
// what one generation can plausibly keep up with (1*1=1) — a second
// generation must be spawned.
func TestReconcileSpawnsOnce_ScaleOutWhenSaturated(t *testing.T) {
	tasks := []Task{
		{Path: "Tasks/A.md", Title: "A", Status: "open", Tags: []string{"claude"}, Projects: []string{"myapp"}},
		{Path: "Tasks/B.md", Title: "B", Status: "open", Tags: []string{"claude"}, Projects: []string{"myapp"}},
		{Path: "Tasks/C.md", Title: "C", Status: "open", Tags: []string{"claude"}, Projects: []string{"myapp"}},
	}
	apiSrv := fakeStartableTasksAPI(t, tasks)

	var callCount int
	srv := newParallelismTestServer(t,
		map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}},
		func(project, cwd string, env map[string]string) error {
			callCount++
			return nil
		})
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	writeRepoNote(t, srv, "myapp", 2, 1)

	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp-g1"] = &Agent{Name: "orchestrator-myapp-g1", Project: "myapp", LastSeenAt: time.Now(), Accepting: true}
	srv.mu.Unlock()

	srv.reconcileSpawnsOnce()

	if callCount != 1 {
		t.Errorf("expected exactly 1 scale-out spawn, got %d calls", callCount)
	}
}

// TestReconcileSpawnsOnce_NoScaleOutWhenNotSaturated verifies the same
// setup but with a startable backlog that does NOT exceed capacity
// (max-workers=5, only 3 startable tasks) — no scale-out.
func TestReconcileSpawnsOnce_NoScaleOutWhenNotSaturated(t *testing.T) {
	tasks := []Task{
		{Path: "Tasks/A.md", Title: "A", Status: "open", Tags: []string{"claude"}, Projects: []string{"myapp"}},
		{Path: "Tasks/B.md", Title: "B", Status: "open", Tags: []string{"claude"}, Projects: []string{"myapp"}},
		{Path: "Tasks/C.md", Title: "C", Status: "open", Tags: []string{"claude"}, Projects: []string{"myapp"}},
	}
	apiSrv := fakeStartableTasksAPI(t, tasks)

	var callCount int
	srv := newParallelismTestServer(t,
		map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}},
		func(project, cwd string, env map[string]string) error {
			callCount++
			return nil
		})
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	writeRepoNote(t, srv, "myapp", 2, 5)

	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp-g1"] = &Agent{Name: "orchestrator-myapp-g1", Project: "myapp", LastSeenAt: time.Now(), Accepting: true}
	srv.mu.Unlock()

	srv.reconcileSpawnsOnce()

	if callCount != 0 {
		t.Errorf("expected no scale-out spawn when not saturated, got %d calls", callCount)
	}
}

// TestReconcileSpawnsOnce_ScaleOutCappedAtMaxOrchestrators verifies a
// project already AT max-orchestrators never scales out further, no
// matter how saturated the backlog looks.
func TestReconcileSpawnsOnce_ScaleOutCappedAtMaxOrchestrators(t *testing.T) {
	tasks := make([]Task, 20)
	for i := range tasks {
		tasks[i] = Task{Path: fmt.Sprintf("Tasks/T%d.md", i), Title: fmt.Sprintf("T%d", i), Status: "open", Tags: []string{"claude"}, Projects: []string{"myapp"}}
	}
	apiSrv := fakeStartableTasksAPI(t, tasks)

	var callCount int
	srv := newParallelismTestServer(t,
		map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}},
		func(project, cwd string, env map[string]string) error {
			callCount++
			return nil
		})
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	writeRepoNote(t, srv, "myapp", 1, 1) // max-orchestrators=1: already at cap

	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp-g1"] = &Agent{Name: "orchestrator-myapp-g1", Project: "myapp", LastSeenAt: time.Now(), Accepting: true}
	srv.mu.Unlock()

	srv.reconcileSpawnsOnce()

	if callCount != 0 {
		t.Errorf("expected no spawn beyond max-orchestrators=1, got %d calls", callCount)
	}
}

// TestReconcileSpawnsOnce_SpawnOnPendingWorkWithEmptyQueue verifies the
// live deadlock this closes: no accepting agent, NO queued message at
// all, but a startable open claude-tagged task exists — must still spawn.
func TestReconcileSpawnsOnce_SpawnOnPendingWorkWithEmptyQueue(t *testing.T) {
	tasks := []Task{
		{Path: "Tasks/A.md", Title: "A", Status: "open", Tags: []string{"claude"}, Projects: []string{"myapp"}},
	}
	apiSrv := fakeStartableTasksAPI(t, tasks)

	var callCount int
	srv := newParallelismTestServer(t,
		map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}},
		func(project, cwd string, env map[string]string) error {
			callCount++
			return nil
		})
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	// No repo note written: defaults (max-orchestrators=1) apply.
	// No agents registered, no messages queued at all.

	srv.reconcileSpawnsOnce()

	if callCount != 1 {
		t.Errorf("expected a spawn triggered by pending startable work despite an empty queue, got %d calls", callCount)
	}
}

// TestReconcileSpawnsOnce_NoSpawnWhenNothingRoutable verifies the
// baseline: no accepting agent, no queued message, no startable task —
// no spawn.
func TestReconcileSpawnsOnce_NoSpawnWhenNothingRoutable(t *testing.T) {
	apiSrv := fakeStartableTasksAPI(t, nil)

	var callCount int
	srv := newParallelismTestServer(t,
		map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}},
		func(project, cwd string, env map[string]string) error {
			callCount++
			return nil
		})
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})

	srv.reconcileSpawnsOnce()

	if callCount != 0 {
		t.Errorf("expected no spawn when nothing is routable, got %d calls", callCount)
	}
}

// TestPickAcceptingAgentForAssignmentLocked_LeastOwnedWins verifies NEW
// assignments go to the accepting agent owning the fewest tasks, and ties
// break lexicographically.
func TestPickAcceptingAgentForAssignmentLocked_LeastOwnedWins(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp-g1", "myapp")
	register(t, ts, "orchestrator-myapp-g2", "myapp")

	// g1 owns 2 tasks, g2 owns 0 — g2 should win.
	seedOwnedTask(srv, "orchestrator-myapp-g1", "Tasks/A.md", "A")
	seedOwnedTask(srv, "orchestrator-myapp-g1", "Tasks/B.md", "B")

	srv.mu.Lock()
	picked := srv.pickAcceptingAgentForAssignmentLocked("myapp")
	srv.mu.Unlock()
	if picked == nil || picked.Name != "orchestrator-myapp-g2" {
		t.Fatalf("expected orchestrator-myapp-g2 (fewest owned), got %+v", picked)
	}

	// Even up ownership: 2 vs 2 — tie breaks lexicographically (g1 < g2).
	seedOwnedTask(srv, "orchestrator-myapp-g2", "Tasks/C.md", "C")
	seedOwnedTask(srv, "orchestrator-myapp-g2", "Tasks/D.md", "D")

	srv.mu.Lock()
	pickedTie := srv.pickAcceptingAgentForAssignmentLocked("myapp")
	srv.mu.Unlock()
	if pickedTie == nil || pickedTie.Name != "orchestrator-myapp-g1" {
		t.Fatalf("expected orchestrator-myapp-g1 on a tie (lexicographic), got %+v", pickedTie)
	}
}

// TestDispatchMessage_NewAssignmentGoesToLeastOwnedAgent verifies the
// end-to-end path: a brand-new (no owner yet) task assignment routes to
// the accepting agent with the fewest owned tasks, not just the
// lexicographically-first one.
func TestDispatchMessage_NewAssignmentGoesToLeastOwnedAgent(t *testing.T) {
	srv, ts := newTestServer(t)
	// Register in an order where the lexicographically-first name (g1) is
	// the BUSY one, to prove least-owned actually overrides plain sort.
	register(t, ts, "orchestrator-myapp-g1", "myapp")
	register(t, ts, "orchestrator-myapp-g2", "myapp")
	seedOwnedTask(srv, "orchestrator-myapp-g1", "Tasks/A.md", "A")
	seedOwnedTask(srv, "orchestrator-myapp-g1", "Tasks/B.md", "B")

	msg, _, _ := srv.dispatchMessage(sendRequest{Project: "myapp", TaskPath: "Tasks/New.md", Text: "Task assigned: New (Tasks/New.md)"})
	if msg.To != "orchestrator-myapp-g2" {
		t.Errorf("expected the new assignment to go to the less-loaded agent orchestrator-myapp-g2, got %q", msg.To)
	}
}

// TestSpawnOrchestrator_RepoNoteMaxWorkersOverridesServeJSON verifies a
// repo-note max-workers takes precedence over a serve.json env entry of
// the same name.
func TestSpawnOrchestrator_RepoNoteMaxWorkersOverridesServeJSON(t *testing.T) {
	var gotEnv map[string]string
	projects := map[string]ProjectConfig{
		"myapp": {
			AutoSpawn: true, Cwd: "/repos/myapp",
			Env: map[string]EnvEntry{"TN_MAX_WORKERS": {Value: "2"}},
		},
	}
	srv := newParallelismTestServer(t, projects, func(project, cwd string, env map[string]string) error {
		gotEnv = env
		return nil
	})
	writeRepoNote(t, srv, "myapp", 0, 5)

	srv.spawnOrchestrator("myapp", "/repos/myapp")

	if gotEnv["TN_MAX_WORKERS"] != "5" {
		t.Errorf("expected repo-note max-workers (5) to override serve.json's (2), got %q", gotEnv["TN_MAX_WORKERS"])
	}
}

// TestSpawnOrchestrator_ServeJSONMaxWorkersKeptWhenNoRepoNoteOverride
// verifies serve.json's TN_MAX_WORKERS entry survives untouched when the
// repo note doesn't set max-workers at all.
func TestSpawnOrchestrator_ServeJSONMaxWorkersKeptWhenNoRepoNoteOverride(t *testing.T) {
	var gotEnv map[string]string
	projects := map[string]ProjectConfig{
		"myapp": {
			AutoSpawn: true, Cwd: "/repos/myapp",
			Env: map[string]EnvEntry{"TN_MAX_WORKERS": {Value: "2"}},
		},
	}
	srv := newParallelismTestServer(t, projects, func(project, cwd string, env map[string]string) error {
		gotEnv = env
		return nil
	})
	// No repo note written at all — defaults, no max-workers override.

	srv.spawnOrchestrator("myapp", "/repos/myapp")

	if gotEnv["TN_MAX_WORKERS"] != "2" {
		t.Errorf("expected serve.json's TN_MAX_WORKERS (2) kept, got %q", gotEnv["TN_MAX_WORKERS"])
	}
}
