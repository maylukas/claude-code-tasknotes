package tn

import (
	"bytes"
	"log"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Fakes for spawn-evidence tests: no test in this file ever touches a
// real tmux server. ---

// fakeSpawnSnapshot is a controllable spawnSnapshotFunc: tests populate
// entries directly (mutated between reconciler ticks to simulate the world
// changing), or set err to make the snapshot fail.
type fakeSpawnSnapshot struct {
	mu      sync.Mutex
	entries map[string]spawnSnapshotEntry
	err     error
}

func newFakeSpawnSnapshot() *fakeSpawnSnapshot {
	return &fakeSpawnSnapshot{entries: map[string]spawnSnapshotEntry{}}
}

func (f *fakeSpawnSnapshot) set(session string, ev spawnSnapshotEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[session] = ev
}

func (f *fakeSpawnSnapshot) fn(sessions []string) (map[string]spawnSnapshotEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]spawnSnapshotEntry, len(sessions))
	for _, s := range sessions {
		out[s] = f.entries[s] // zero value (Exists:false) for an untracked/never-set session
	}
	return out, nil
}

// fakeKillRecorder is a tmuxKillSessionFunc that just records what it was
// asked to kill.
type fakeKillRecorder struct {
	mu     sync.Mutex
	killed []string
}

func (f *fakeKillRecorder) fn(session string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, session)
	return nil
}

func (f *fakeKillRecorder) sessions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.killed...)
}

// spawnedIdentity is one recorded spawnFunc call's identity — captured by
// the tests below instead of guessing newGenerationIdentity's time-based
// output.
type spawnedIdentity struct {
	agentName, tmuxSession string
}

// newSpawnEvidenceTestServer builds a Server (no background goroutines —
// same convention as newReconcilerTestServer) wired with a fakeSpawnSnapshot
// and fakeKillRecorder, and a spawnFunc that records every call's identity
// and never touches real tmux.
func newSpawnEvidenceTestServer(t *testing.T, projects map[string]ProjectConfig) (srv *Server, snap *fakeSpawnSnapshot, kill *fakeKillRecorder, calls *[]spawnedIdentity) {
	t.Helper()
	snap = newFakeSpawnSnapshot()
	kill = &fakeKillRecorder{}
	var mu sync.Mutex
	var recorded []spawnedIdentity
	srv = newReconcilerTestServer(t, projects, func(project, cwd string, env map[string]string, agentName, tmuxSession string) error {
		mu.Lock()
		recorded = append(recorded, spawnedIdentity{agentName, tmuxSession})
		mu.Unlock()
		return nil
	})
	srv.spawnSnapshotFunc = snap.fn
	srv.spawnKillFunc = kill.fn
	return srv, snap, kill, &recorded
}

// backdateSpawnedGeneration rewrites the tracked entry matching tmuxSession
// under slug to look age old, as of now — the test suite's stand-in for a
// clock seam (there isn't one; time.Now() is used directly throughout this
// package, same as the existing PendingSpawns tests in
// spawn_intent_test.go backdating state.PendingSpawns directly).
func backdateSpawnedGeneration(t *testing.T, srv *Server, slug, tmuxSession string, age time.Duration) {
	t.Helper()
	srv.mu.Lock()
	defer srv.mu.Unlock()
	entries := srv.state.SpawnedGenerations[slug]
	for i := range entries {
		if entries[i].TmuxSession == tmuxSession {
			entries[i].SpawnedAt = time.Now().Add(-age)
			return
		}
	}
	t.Fatalf("backdateSpawnedGeneration: no tracked entry for slug=%q tmuxSession=%q", slug, tmuxSession)
}

// registerWithTmux is register()'s sibling that also reports a tmux
// session, for tests that need to exercise clearSpawnedGenerationLocked's
// tmux-session-based match (rather than falling back to agent name).
func registerWithTmux(t *testing.T, ts *httptest.Server, name, project, tmuxSession string) {
	t.Helper()
	resp := mustPost(t, ts.URL+"/agents/register", map[string]any{"name": name, "project": project, "tmuxSession": tmuxSession})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("register %s: expected 200, got %d", name, resp.StatusCode)
	}
}

// captureLog redirects the standard logger's output for the duration of
// the test, restoring it on cleanup, and returns the buffer to inspect.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })
	return &buf
}

// TestSpawnEvidence_AliveUnregistered_RespawnsAtDeadline_ThenSuspends is
// the direct regression guard for the 232-orchestrator incident: a
// generation that comes up (tmux session alive, claude process running)
// but never reaches `tn register` must NOT be respawned on a fixed clock
// (the old spawnIntentTTL bug) — it should stay counted as outstanding for
// as long as it's genuinely alive, only get killed/retried once
// spawnRegisterDeadline is exceeded, and the whole slug must SUSPEND after
// maxConsecutiveSpawnFailures such cycles rather than looping forever.
func TestSpawnEvidence_AliveUnregistered_RespawnsAtDeadline_ThenSuspends(t *testing.T) {
	logBuf := captureLog(t)

	srv, snap, kill, calls := newSpawnEvidenceTestServer(t,
		map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}})
	seedQueuedOrchestratorMessage(srv, "orchestrator-myapp")

	latest := func() spawnedIdentity {
		c := *calls
		return c[len(c)-1]
	}
	markAliveNeverRegistered := func(session string) {
		snap.set(session, spawnSnapshotEntry{Exists: true, LiveClaude: true})
	}

	// Tick 1: nothing tracked yet, queued work, no accepting agent — spawns
	// generation 1.
	srv.reconcileSpawnsOnce()
	if len(*calls) != 1 {
		t.Fatalf("expected 1 spawn for generation 1, got %d", len(*calls))
	}
	gen1 := latest()
	markAliveNeverRegistered(gen1.tmuxSession)

	// Ticks well within spawnRegisterDeadline (10min), alive but never
	// registered: must NOT respawn, and must NOT kill.
	for _, age := range []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute, 9 * time.Minute} {
		backdateSpawnedGeneration(t, srv, "myapp", gen1.tmuxSession, age)
		srv.reconcileSpawnsOnce()
		if len(*calls) != 1 {
			t.Fatalf("age %s: expected still exactly 1 spawn (alive-unregistered within deadline), got %d", age, len(*calls))
		}
	}
	if len(kill.sessions()) != 0 {
		t.Fatalf("expected no kills before the deadline, got %v", kill.sessions())
	}

	// Past the deadline: generation 1 gets killed AND a fresh generation 2
	// spawns in the same tick (the reconciler's evidence pass and decision
	// pass share one lock scope).
	backdateSpawnedGeneration(t, srv, "myapp", gen1.tmuxSession, spawnRegisterDeadline+time.Minute)
	srv.reconcileSpawnsOnce()
	if len(*calls) != 2 {
		t.Fatalf("expected generation 2 to spawn once generation 1's deadline passed, got %d total spawns", len(*calls))
	}
	if got := kill.sessions(); len(got) != 1 || got[0] != gen1.tmuxSession {
		t.Fatalf("expected generation 1's session %q killed, got %v", gen1.tmuxSession, got)
	}
	srv.mu.Lock()
	failures := srv.state.SpawnFailures["myapp"]
	srv.mu.Unlock()
	if failures != 1 {
		t.Fatalf("expected 1 consecutive spawn failure recorded, got %d", failures)
	}

	// Generation 2 repeats the exact same failure mode.
	gen2 := latest()
	markAliveNeverRegistered(gen2.tmuxSession)
	backdateSpawnedGeneration(t, srv, "myapp", gen2.tmuxSession, spawnRegisterDeadline+time.Minute)
	srv.reconcileSpawnsOnce()
	if len(*calls) != 3 {
		t.Fatalf("expected generation 3 to spawn once generation 2's deadline passed, got %d total spawns", len(*calls))
	}

	// Generation 3 also repeats it — this is the 3rd consecutive failure,
	// hitting maxConsecutiveSpawnFailures: it must be killed, but NO
	// generation 4 may spawn.
	gen3 := latest()
	markAliveNeverRegistered(gen3.tmuxSession)
	backdateSpawnedGeneration(t, srv, "myapp", gen3.tmuxSession, spawnRegisterDeadline+time.Minute)
	srv.reconcileSpawnsOnce()
	if len(*calls) != 3 {
		t.Fatalf("expected NO 4th spawn once maxConsecutiveSpawnFailures was reached, got %d total spawns", len(*calls))
	}
	srv.mu.Lock()
	failures = srv.state.SpawnFailures["myapp"]
	srv.mu.Unlock()
	if failures != maxConsecutiveSpawnFailures {
		t.Fatalf("expected SpawnFailures == %d, got %d", maxConsecutiveSpawnFailures, failures)
	}
	if got := kill.sessions(); len(got) != 3 {
		t.Fatalf("expected exactly 3 kills total (one per failed generation), got %v", got)
	}

	// Further ticks: still suspended, zero further spawns, and the
	// "SUSPENDED" log line must be throttled (appears once, not once per
	// tick) despite multiple ticks with routable work.
	srv.reconcileSpawnsOnce()
	srv.reconcileSpawnsOnce()
	if len(*calls) != 3 {
		t.Fatalf("expected the slug to stay suspended (0 further spawns), got %d total spawns", len(*calls))
	}
	suspendedLines := strings.Count(logBuf.String(), "spawn SUSPENDED for myapp")
	if suspendedLines != 1 {
		t.Errorf("expected exactly 1 throttled \"spawn SUSPENDED\" log line across 3 suspended ticks, got %d", suspendedLines)
	}
}

// TestSpawnEvidence_FreshEntryWithinGrace_BlocksSpawn_NeverKilled verifies
// the spawnStartupGrace window: a generation younger than 60s is always
// treated as Pending, even if its pane currently shows no live claude
// process (a bare shell is completely normal during claude's own startup)
// — it must block a second spawn AND must not be killed.
func TestSpawnEvidence_FreshEntryWithinGrace_BlocksSpawn_NeverKilled(t *testing.T) {
	srv, snap, kill, calls := newSpawnEvidenceTestServer(t,
		map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}})
	seedQueuedOrchestratorMessage(srv, "orchestrator-myapp")

	srv.reconcileSpawnsOnce()
	if len(*calls) != 1 {
		t.Fatalf("expected 1 spawn, got %d", len(*calls))
	}
	gen1 := (*calls)[0]
	// No age backdating — the entry is still fresh (<60s old). Report the
	// session as existing with a bare-shell pane (no live claude).
	snap.set(gen1.tmuxSession, spawnSnapshotEntry{Exists: true, LiveClaude: false})

	srv.reconcileSpawnsOnce()
	if len(*calls) != 1 {
		t.Fatalf("expected the fresh entry's startup grace to block a second spawn, got %d total spawns", len(*calls))
	}
	if got := kill.sessions(); len(got) != 0 {
		t.Fatalf("expected the fresh entry NOT to be killed during its startup grace, got %v", got)
	}
	srv.mu.Lock()
	_, stillTracked := indexOfSpawnedGeneration(srv.state.SpawnedGenerations["myapp"], gen1.tmuxSession)
	srv.mu.Unlock()
	if !stillTracked {
		t.Error("expected generation 1's entry to still be tracked (Pending) during its startup grace")
	}
}

// TestSpawnEvidence_SemverPaneCommand_PendingNotKilled directly guards the
// versioned-symlink pane-detection bug fixed in paneHasLiveClaude/
// claudePaneVersionPattern (spawn.go): tmux reports a claude binary
// resolved through ~/.local/bin/claude's versioned-symlink layout as a
// bare version string (e.g. "2.1.266"), not "claude"/"node" — confirmed
// live. A healthy orchestrator whose pane reports exactly that shape must
// be treated as alive and stay Pending well within spawnRegisterDeadline,
// not killed as "no claude process running" by the very mechanism meant
// to protect a slow-to-register-but-healthy generation.
func TestSpawnEvidence_SemverPaneCommand_PendingNotKilled(t *testing.T) {
	if !paneHasLiveClaude([]string{"2.1.266"}) {
		t.Fatal("sanity check failed: paneHasLiveClaude must report a bare semver pane as live claude (see TestPaneHasLiveClaude)")
	}

	srv, snap, kill, calls := newSpawnEvidenceTestServer(t,
		map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}})
	seedQueuedOrchestratorMessage(srv, "orchestrator-myapp")

	srv.reconcileSpawnsOnce()
	if len(*calls) != 1 {
		t.Fatalf("expected 1 spawn, got %d", len(*calls))
	}
	gen1 := (*calls)[0]

	// What realSpawnSnapshot would actually compute for a pane reporting
	// "2.1.266": Exists true, LiveClaude true (via paneHasLiveClaude).
	snap.set(gen1.tmuxSession, spawnSnapshotEntry{Exists: true, LiveClaude: true})
	backdateSpawnedGeneration(t, srv, "myapp", gen1.tmuxSession, 5*time.Minute)

	srv.reconcileSpawnsOnce()

	if len(*calls) != 1 {
		t.Fatalf("expected the healthy-but-slow-to-register generation to stay Pending at 5min (well within spawnRegisterDeadline), got %d total spawns", len(*calls))
	}
	if got := kill.sessions(); len(got) != 0 {
		t.Fatalf("expected no kill for a generation with a live semver-reporting pane, got %v", got)
	}
	srv.mu.Lock()
	tracked := srv.state.SpawnedGenerations["myapp"]
	srv.mu.Unlock()
	if len(tracked) != 1 || !tracked[0].Pending {
		t.Errorf("expected the entry to remain tracked and Pending, got %+v", tracked)
	}
}

// indexOfSpawnedGeneration is a small test-only lookup helper.
func indexOfSpawnedGeneration(entries []SpawnedGeneration, tmuxSession string) (int, bool) {
	for i, g := range entries {
		if g.TmuxSession == tmuxSession {
			return i, true
		}
	}
	return -1, false
}

// TestSpawnEvidence_DeadPanePastGrace_KilledAndRespawnAllowed verifies a
// generation whose session exists but has no claude process running,
// once past spawnStartupGrace, is killed, counted as a failure, and a
// fresh generation is allowed to spawn (below the cap).
func TestSpawnEvidence_DeadPanePastGrace_KilledAndRespawnAllowed(t *testing.T) {
	srv, snap, kill, calls := newSpawnEvidenceTestServer(t,
		map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}})
	seedQueuedOrchestratorMessage(srv, "orchestrator-myapp")

	srv.reconcileSpawnsOnce()
	gen1 := (*calls)[0]
	snap.set(gen1.tmuxSession, spawnSnapshotEntry{Exists: true, LiveClaude: false})
	backdateSpawnedGeneration(t, srv, "myapp", gen1.tmuxSession, 90*time.Second)

	srv.reconcileSpawnsOnce()

	if got := kill.sessions(); len(got) != 1 || got[0] != gen1.tmuxSession {
		t.Fatalf("expected generation 1 killed, got %v", got)
	}
	srv.mu.Lock()
	failures := srv.state.SpawnFailures["myapp"]
	tracked := srv.state.SpawnedGenerations["myapp"]
	srv.mu.Unlock()
	if failures != 1 {
		t.Fatalf("expected 1 failure recorded, got %d", failures)
	}
	// Assert by COUNT + freshness, not by "generation 1's tmux session is
	// no longer tracked": newGenerationIdentity is time-based (whole
	// seconds), so a same-tick respawn can legitimately reuse the exact
	// same name gen1 had — the old (killed, backdated) entry is gone and a
	// brand new one takes its place under that name, which is correct
	// behavior, not a bug. What must hold is: exactly one entry remains,
	// and it's fresh (the new generation), not the 90s-old one that was
	// just resolved as a failure.
	if len(tracked) != 1 {
		t.Fatalf("expected exactly 1 tracked entry after kill+respawn, got %d (%+v)", len(tracked), tracked)
	}
	if age := time.Since(tracked[0].SpawnedAt); age > 5*time.Second {
		t.Errorf("expected the remaining tracked entry to be the FRESH generation, got one %s old", age)
	}
	if len(*calls) != 2 {
		t.Fatalf("expected a fresh generation 2 to spawn in the same tick, got %d total spawns", len(*calls))
	}
}

// TestSpawnEvidence_MissingSession_DroppedAndCounted verifies a tracked
// entry whose tmux session no longer exists at all (never showed up in the
// snapshot) is dropped and counted as a failure, same as a dead pane.
func TestSpawnEvidence_MissingSession_DroppedAndCounted(t *testing.T) {
	srv, _, kill, calls := newSpawnEvidenceTestServer(t,
		map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}})
	seedQueuedOrchestratorMessage(srv, "orchestrator-myapp")

	srv.reconcileSpawnsOnce()
	gen1 := (*calls)[0]
	// Deliberately never call snap.set for gen1's session — it stays
	// absent from the fake's entries map, which fn() reports as the zero
	// value (Exists: false), i.e. "missing".
	backdateSpawnedGeneration(t, srv, "myapp", gen1.tmuxSession, 90*time.Second)

	srv.reconcileSpawnsOnce()

	srv.mu.Lock()
	failures := srv.state.SpawnFailures["myapp"]
	tracked := srv.state.SpawnedGenerations["myapp"]
	srv.mu.Unlock()
	if failures != 1 {
		t.Fatalf("expected 1 failure recorded for a missing session, got %d", failures)
	}
	// See the same-named-respawn note in
	// TestSpawnEvidence_DeadPanePastGrace_KilledAndRespawnAllowed — assert
	// by count + freshness, not by name, since newGenerationIdentity can
	// legitimately reuse gen1's exact name for the same-tick respawn.
	if len(tracked) != 1 {
		t.Fatalf("expected exactly 1 tracked entry after the missing session was dropped and a fresh one spawned, got %d (%+v)", len(tracked), tracked)
	}
	if age := time.Since(tracked[0].SpawnedAt); age > 5*time.Second {
		t.Errorf("expected the remaining tracked entry to be the FRESH generation, got one %s old", age)
	}
	if len(*calls) != 2 {
		t.Fatalf("expected a fresh generation to spawn in the same tick once the missing session was dropped, got %d total spawns", len(*calls))
	}
	_ = kill // a missing session's "kill" is a harmless no-op; not asserted here
}

// TestSpawnEvidence_RegistrationClearsEntryAndFailures verifies a
// registration matching a tracked generation's tmux session removes that
// entry and resets the project's consecutive-failure counter — and that a
// subsequently, genuinely needed spawn (the registered agent draining,
// with queued work) still works afterward.
func TestSpawnEvidence_RegistrationClearsEntryAndFailures(t *testing.T) {
	srv, _, _, calls := newSpawnEvidenceTestServer(t,
		map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}})
	seedQueuedOrchestratorMessage(srv, "orchestrator-myapp")

	// Simulate two prior consecutive failures (still under the cap).
	srv.mu.Lock()
	srv.state.SpawnFailures["myapp"] = 2
	srv.mu.Unlock()

	srv.reconcileSpawnsOnce()
	if len(*calls) != 1 {
		t.Fatalf("expected 1 spawn (2 failures is still under the cap), got %d", len(*calls))
	}
	gen1 := (*calls)[0]

	ts := httptest.NewServer(newMux(srv))
	defer ts.Close()
	registerWithTmux(t, ts, gen1.agentName, "myapp", gen1.tmuxSession)

	srv.mu.Lock()
	failures := srv.state.SpawnFailures["myapp"]
	_, stillTracked := indexOfSpawnedGeneration(srv.state.SpawnedGenerations["myapp"], gen1.tmuxSession)
	srv.mu.Unlock()
	if failures != 0 {
		t.Fatalf("expected registration to reset SpawnFailures to 0, got %d", failures)
	}
	if stillTracked {
		t.Error("expected registration to remove the matching tracked entry")
	}

	// Now drain the registered agent — a fresh generation must be allowed
	// to spawn, proving the reset didn't leave spawning wedged some other
	// way. Registration re-targeted the original queued message from the
	// logical orchestrator-myapp queue to gen1's concrete name
	// (retargetLogicalQueueLocked), so a fresh message addressed to the
	// logical name is what actually gives the reconciler a reason to spawn
	// again here — same trigger as
	// TestReconcileSpawnsOnce_SpawnsWhenOnlyDrainingAgentAlive.
	drainResp := mustPost(t, ts.URL+"/agents/drain", map[string]any{"name": gen1.agentName})
	drainResp.Body.Close()
	seedQueuedOrchestratorMessage(srv, "orchestrator-myapp")

	srv.reconcileSpawnsOnce()
	if len(*calls) != 2 {
		t.Fatalf("expected a legitimate post-reset spawn (draining-only) to succeed, got %d total spawns", len(*calls))
	}
}

// TestSpawnEvidence_SnapshotError_NoSpawnNoKill_EntriesRetained verifies a
// failing tmux snapshot is handled conservatively: every tracked entry
// stays exactly as it was (Pending), nothing is killed, and no new spawn
// is attempted for its slug.
func TestSpawnEvidence_SnapshotError_NoSpawnNoKill_EntriesRetained(t *testing.T) {
	srv, snap, kill, calls := newSpawnEvidenceTestServer(t,
		map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}})
	seedQueuedOrchestratorMessage(srv, "orchestrator-myapp")

	srv.reconcileSpawnsOnce()
	gen1 := (*calls)[0]
	backdateSpawnedGeneration(t, srv, "myapp", gen1.tmuxSession, 5*time.Minute)

	snap.mu.Lock()
	snap.err = errFakeTmuxSnapshot
	snap.mu.Unlock()

	srv.reconcileSpawnsOnce()

	if len(*calls) != 1 {
		t.Fatalf("expected no new spawn while the snapshot is failing, got %d total spawns", len(*calls))
	}
	if got := kill.sessions(); len(got) != 0 {
		t.Fatalf("expected no kills while the snapshot is failing, got %v", got)
	}
	srv.mu.Lock()
	failures := srv.state.SpawnFailures["myapp"]
	_, stillTracked := indexOfSpawnedGeneration(srv.state.SpawnedGenerations["myapp"], gen1.tmuxSession)
	srv.mu.Unlock()
	if failures != 0 {
		t.Fatalf("expected no failure counted from a snapshot error, got %d", failures)
	}
	if !stillTracked {
		t.Error("expected the entry to remain tracked across a snapshot error")
	}
}

// errFakeTmuxSnapshot is a stand-in tmux-snapshot failure, distinct from
// any real tmux error text so it can never accidentally match the
// "no server running"-style zero-sessions heuristic in realSpawnSnapshot
// (not exercised here, since this test injects the fake directly, but kept
// distinct on principle).
var errFakeTmuxSnapshot = &fakeSnapshotError{"simulated tmux failure"}

type fakeSnapshotError struct{ msg string }

func (e *fakeSnapshotError) Error() string { return e.msg }

// TestReconcileSpawnsOnce_IdleTickDoesNotWriteState verifies a genuinely
// idle reconciler pass (no tracked generations to evaluate, no queued
// work, nothing decided) never calls saveLocked — an idle daemon must not
// rewrite state.json every reconcilerInterval (30s) tick for nothing.
func TestReconcileSpawnsOnce_IdleTickDoesNotWriteState(t *testing.T) {
	srv, _, _, calls := newSpawnEvidenceTestServer(t,
		map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}})
	// Deliberately nothing queued and no tracked generations — a pass with
	// nothing whatsoever for the reconciler to do.

	srv.reconcileSpawnsOnce()

	if len(*calls) != 0 {
		t.Fatalf("expected no spawn on a genuinely idle tick, got %d", len(*calls))
	}
	if _, err := os.Stat(srv.statePath); !os.IsNotExist(err) {
		t.Errorf("expected state.json to never be written by an idle tick, got stat err=%v", err)
	}
}

// TestHandleSpawnPause_UnpauseClearsSpawnFailures verifies POST
// /spawn/pause {"paused":false} clears every project's consecutive-failure
// counter, per handleSpawnPause's documented reset path.
func TestHandleSpawnPause_UnpauseClearsSpawnFailures(t *testing.T) {
	srv, ts := newTestServer(t)

	srv.mu.Lock()
	srv.state.SpawnFailures["myapp"] = 3
	srv.state.SpawnFailures["otherapp"] = 1
	srv.mu.Unlock()

	resp := mustPost(t, ts.URL+"/spawn/pause", map[string]any{"paused": false})
	resp.Body.Close()

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.state.SpawnFailures) != 0 {
		t.Errorf("expected un-pausing to clear every project's SpawnFailures, got %+v", srv.state.SpawnFailures)
	}
}

// TestLoadState_OldStateJSONWithoutSpawnEvidenceFields verifies a
// pre-migration state.json (written before SpawnedGenerations/
// SpawnFailures existed) loads with both as non-nil empty maps rather than
// nil — nil-map safety for every read/write site that assumes they're
// initialized.
func TestLoadState_OldStateJSONWithoutSpawnEvidenceFields(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/state.json"
	oldJSON := `{"agents":{},"messages":[],"pendingSpawns":{"myapp":"2026-01-01T00:00:00Z"}}`
	if err := atomicWriteFile(path, []byte(oldJSON)); err != nil {
		t.Fatal(err)
	}

	st := loadState(path)

	if st.SpawnedGenerations == nil {
		t.Error("expected SpawnedGenerations to be a non-nil empty map after loading old state JSON")
	}
	if st.SpawnFailures == nil {
		t.Error("expected SpawnFailures to be a non-nil empty map after loading old state JSON")
	}
	if len(st.SpawnedGenerations) != 0 || len(st.SpawnFailures) != 0 {
		t.Errorf("expected both maps empty, got %+v / %+v", st.SpawnedGenerations, st.SpawnFailures)
	}
}

// TestDispatchMessage_RespectsPendingSpawnedGeneration verifies the
// on-demand path (resolveTargetLocked, via dispatchMessage) doesn't spawn a
// second generation while a first one is still tracked as pending — the
// same suppression the reconciler gets from effectiveAcceptingCountLocked,
// exercised via the OTHER spawn path this time.
func TestDispatchMessage_RespectsPendingSpawnedGeneration(t *testing.T) {
	var callCount int
	srv := newReconcilerTestServer(t,
		map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}},
		func(project, cwd string, env map[string]string, agentName, tmuxSession string) error {
			callCount++
			return nil
		})

	srv.dispatchMessage(sendRequest{Project: "myapp", Text: "Task assigned: X (Tasks/X.md)"})
	if callCount != 1 {
		t.Fatalf("expected the first dispatch to spawn once, got %d calls", callCount)
	}

	// A second dispatch moments later (generation 1 hasn't registered yet)
	// must NOT spawn again.
	srv.dispatchMessage(sendRequest{Project: "myapp", Text: "Task assigned: Y (Tasks/Y.md)"})
	if callCount != 1 {
		t.Fatalf("expected the second dispatch to be suppressed by the still-pending generation, got %d calls", callCount)
	}
}

// TestDispatchMessage_RespectsSpawnFailureCap verifies the on-demand path
// refuses to spawn once a slug has hit maxConsecutiveSpawnFailures, same as
// the reconciler.
func TestDispatchMessage_RespectsSpawnFailureCap(t *testing.T) {
	var callCount int
	srv := newReconcilerTestServer(t,
		map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}},
		func(project, cwd string, env map[string]string, agentName, tmuxSession string) error {
			callCount++
			return nil
		})
	srv.mu.Lock()
	srv.state.SpawnFailures["myapp"] = maxConsecutiveSpawnFailures
	srv.mu.Unlock()

	_, _, extra := srv.dispatchMessage(sendRequest{Project: "myapp", Text: "Task assigned: X (Tasks/X.md)"})

	if callCount != 0 {
		t.Fatalf("expected the on-demand path to refuse spawning a suspended slug, got %d calls", callCount)
	}
	if w, _ := extra["warning"].(string); !strings.Contains(w, "suspended") {
		t.Errorf("expected a suspension warning in the response, got extra=%+v", extra)
	}
}
