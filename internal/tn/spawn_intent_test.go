package tn

import (
	"net/http/httptest"
	"testing"
	"time"
)

// TestSpawnIntent_BothPathsDecideSameWindow_OnlyOneSpawn is the direct
// regression guard for the live incident: two independent spawn paths
// (dispatchMessage's on-demand spawn, then reconcileSpawnsOnce's periodic
// pass) both observing zero accepting agents for the same project within
// the same short window must produce exactly ONE spawnFunc call, not two —
// the real incident had orchestrator-myapp-g21766 and g21777 both spawn
// ~11s apart because neither path knew the other had already decided to.
func TestSpawnIntent_BothPathsDecideSameWindow_OnlyOneSpawn(t *testing.T) {
	var callCount int
	srv := newReconcilerTestServer(t,
		map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}},
		func(project, cwd string, env map[string]string) error {
			callCount++
			return nil
		})
	// Also routable via the reconciler's own path (a queued orchestrator
	// message), so BOTH paths independently have a reason to spawn.
	seedQueuedOrchestratorMessage(srv, "orchestrator-myapp")

	// Path 1: an on-demand dispatch decides to spawn (no accepting agent
	// exists yet) — this is the FIRST decision, exactly like the webhook-
	// triggered "starting orchestrator for project myapp" log line.
	srv.dispatchMessage(sendRequest{Project: "myapp", Text: "Task assigned: X (Tasks/X.md)"})

	// Path 2: the reconciler's periodic pass runs moments later — in the
	// real incident, 11 seconds later, well before the first spawn had
	// finished registering. Its own independent decision must see the
	// first path's still-pending intent and skip.
	srv.reconcileSpawnsOnce()

	if callCount != 1 {
		t.Fatalf("expected exactly 1 spawnFunc call across both paths in the same window, got %d", callCount)
	}
}

// TestSpawnIntent_ExpiryAllowsLaterLegitimateSpawn verifies a spawn intent
// older than spawnIntentTTL is treated as stale (e.g. the earlier spawn
// silently failed) rather than permanently blocking further spawns for
// that project.
func TestSpawnIntent_ExpiryAllowsLaterLegitimateSpawn(t *testing.T) {
	var callCount int
	srv := newReconcilerTestServer(t,
		map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}},
		func(project, cwd string, env map[string]string) error {
			callCount++
			return nil
		})
	seedQueuedOrchestratorMessage(srv, "orchestrator-myapp")

	srv.mu.Lock()
	srv.state.PendingSpawns["myapp"] = time.Now().Add(-spawnIntentTTL - time.Minute)
	srv.mu.Unlock()

	srv.reconcileSpawnsOnce()

	if callCount != 1 {
		t.Fatalf("expected the reconciler to spawn once the stale intent expired, got %d calls", callCount)
	}
}

// TestSpawnIntent_RegistrationClearsIntentImmediately verifies a fresh
// registration for the project clears its recorded spawn intent right
// away, rather than leaving it to expire on its own after spawnIntentTTL.
func TestSpawnIntent_RegistrationClearsIntentImmediately(t *testing.T) {
	srv, ts := newTestServer(t)

	srv.mu.Lock()
	srv.state.PendingSpawns["proj-a"] = time.Now()
	srv.mu.Unlock()

	register(t, ts, "orchestrator-proj-a-g1", "proj-a")

	srv.mu.Lock()
	_, stillPending := srv.state.PendingSpawns["proj-a"]
	srv.mu.Unlock()

	if stillPending {
		t.Error("expected registration to clear the project's spawn intent immediately")
	}
}

// TestSpawnIntent_RegistrationForDifferentProjectDoesNotClearIntent
// verifies clearSpawnIntentLocked is scoped to the registering agent's OWN
// project — a registration for project B must not clear project A's
// still-pending intent.
func TestSpawnIntent_RegistrationForDifferentProjectDoesNotClearIntent(t *testing.T) {
	srv, ts := newTestServer(t)

	srv.mu.Lock()
	srv.state.PendingSpawns["proj-a"] = time.Now()
	srv.mu.Unlock()

	register(t, ts, "orchestrator-proj-b-g1", "proj-b")

	srv.mu.Lock()
	_, stillPending := srv.state.PendingSpawns["proj-a"]
	srv.mu.Unlock()

	if !stillPending {
		t.Error("expected a different project's registration to leave proj-a's intent untouched")
	}
}

// TestSpawnIntent_MaxOrchestratorsTwoAllowsTwoSequentialSpawns verifies
// the fix caps correctly rather than hard-limiting every project to one
// spawn ever: with max-orchestrators=2 (repo note), a first spawn decision
// (zero accepting agents) followed by that agent registering (clearing its
// intent) followed by a genuine scale-out decision (one accepting agent,
// saturated backlog, still under the cap of 2) must both go through —
// two total spawnFunc calls, not one.
func TestSpawnIntent_MaxOrchestratorsTwoAllowsTwoSequentialSpawns(t *testing.T) {
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
	writeRepoNote(t, srv, "myapp", 2, 1) // max-orchestrators=2, max-workers=1

	seedQueuedOrchestratorMessage(srv, "orchestrator-myapp")

	// First decision: zero accepting agents, a queued message — spawns
	// generation 1.
	srv.reconcileSpawnsOnce()
	if callCount != 1 {
		t.Fatalf("expected 1 spawn for the initial generation, got %d", callCount)
	}

	// Generation 1 registers, clearing its intent.
	ts := httptest.NewServer(newMux(srv))
	defer ts.Close()
	register(t, ts, "orchestrator-myapp-g1", "myapp")

	// Second decision: one accepting agent, backlog saturated relative to
	// max-workers=1 (3 startable > 1*1) and still under max-orchestrators=2
	// — a legitimate scale-out, must NOT be suppressed by the intent fix.
	srv.reconcileSpawnsOnce()
	if callCount != 2 {
		t.Fatalf("expected max-orchestrators=2 to allow a second spawn, got %d total calls", callCount)
	}
}
