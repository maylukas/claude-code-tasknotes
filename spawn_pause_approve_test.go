package main

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

// TestReconcileSpawnsOnce_PausedSkipsSpawn verifies a reconciler pass with
// genuinely routable work does NOT call spawnFunc while SpawnPaused is
// true, and DOES call it once unpaused — same candidate set either way.
func TestReconcileSpawnsOnce_PausedSkipsSpawn(t *testing.T) {
	var callCount int
	srv := newReconcilerTestServer(t,
		map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}},
		func(project, cwd string, env map[string]string) error {
			callCount++
			return nil
		})
	seedQueuedOrchestratorMessage(srv, "orchestrator-myapp")

	srv.mu.Lock()
	srv.state.SpawnPaused = true
	srv.mu.Unlock()

	srv.reconcileSpawnsOnce()
	if callCount != 0 {
		t.Fatalf("expected no spawn while paused, got %d calls", callCount)
	}

	srv.mu.Lock()
	srv.state.SpawnPaused = false
	srv.mu.Unlock()

	srv.reconcileSpawnsOnce()
	if callCount != 1 {
		t.Errorf("expected exactly 1 spawn once unpaused, got %d calls", callCount)
	}
}

// TestSpawnOrchestrator_PausedSkipsSpawn verifies the direct
// dispatchMessage/spawnOrchestrator path (not just the reconciler) is
// also blocked while paused.
func TestSpawnOrchestrator_PausedSkipsSpawn(t *testing.T) {
	var callCount int
	srv := newReconcilerTestServer(t,
		map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}},
		func(project, cwd string, env map[string]string) error {
			callCount++
			return nil
		})
	srv.mu.Lock()
	srv.state.SpawnPaused = true
	srv.mu.Unlock()

	srv.spawnOrchestrator("myapp", "/repos/myapp")

	if callCount != 0 {
		t.Errorf("expected spawnOrchestrator itself to skip while paused, got %d calls", callCount)
	}
}

// TestHandleSpawnPause_PersistsAcrossStateReload verifies POST
// /spawn/pause is durable — a fresh loadState from the same path reflects
// it, not just the in-memory Server.
func TestHandleSpawnPause_PersistsAcrossStateReload(t *testing.T) {
	srv, ts := newTestServer(t)

	resp := mustPost(t, ts.URL+"/spawn/pause", map[string]any{"paused": true})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	reloaded := loadState(srv.statePath)
	if !reloaded.SpawnPaused {
		t.Error("expected SpawnPaused=true to persist across a state reload")
	}

	// GET reports the current value too.
	getResp, err := http.Get(ts.URL + "/spawn/pause")
	if err != nil {
		t.Fatal(err)
	}
	defer getResp.Body.Close()
	var out struct {
		Paused bool `json:"paused"`
	}
	decodeInto(t, getResp, &out)
	if !out.Paused {
		t.Error("expected GET /spawn/pause to report paused=true")
	}

	// Unpause and confirm it persists too.
	resp2 := mustPost(t, ts.URL+"/spawn/pause", map[string]any{"paused": false})
	resp2.Body.Close()
	if reloaded2 := loadState(srv.statePath); reloaded2.SpawnPaused {
		t.Error("expected SpawnPaused=false to persist after unpausing")
	}
}

// TestHandleApprove_SendsRightKey verifies a successful approve call
// sends the correct key for the requested option via injected
// capture/sendKeys funcs (never real tmux), and marks it sent in the
// response.
func TestHandleApprove_SendsRightKey(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp-g1", "myapp")

	srv.mu.Lock()
	srv.stuck.byName["orchestrator-myapp-g1"] = &stuckSessionState{Stuck: true}
	srv.stuckCapture = func(session string) (string, error) {
		return "Do you want to proceed?\n❯ 1. Yes\n2. Yes, and don't ask again this session\n3. No", nil
	}
	var sentSession, sentKey string
	srv.stuckSendKeys = func(session, keys string) error {
		sentSession, sentKey = session, keys
		return nil
	}
	srv.mu.Unlock()

	resp := mustPost(t, ts.URL+"/agents/approve", map[string]any{"name": "orchestrator-myapp-g1", "option": "always"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out struct {
		Sent   bool   `json:"sent"`
		Reason string `json:"reason"`
	}
	decodeInto(t, resp, &out)
	if !out.Sent {
		t.Fatalf("expected sent=true, got %+v", out)
	}
	if sentSession != "tn-myapp-g1" || sentKey != "2" {
		t.Errorf("expected key 2 (the parsed don't-ask option) sent to tn-myapp-g1, got session=%q key=%q", sentSession, sentKey)
	}
}

// TestHandleApprove_409WhenNotStuck verifies approving an agent with no
// currently tracked stuck prompt is refused (409) without ever touching
// the injected capture/sendKeys funcs.
func TestHandleApprove_409WhenNotStuck(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp-g1", "myapp")

	var captureCalls int
	srv.mu.Lock()
	srv.stuckCapture = func(session string) (string, error) {
		captureCalls++
		return "", nil
	}
	srv.mu.Unlock()

	resp := mustPost(t, ts.URL+"/agents/approve", map[string]any{"name": "orchestrator-myapp-g1", "option": "once"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
	if captureCalls != 0 {
		t.Errorf("expected no tmux capture attempted when not stuck, got %d calls", captureCalls)
	}
}

// TestHandleApprove_DeregisteredAgent409 verifies approving a
// deregistered agent (a death path already marked it gone) is rejected
// outright, without ever consulting the possibly-stale stuck snapshot or
// attempting a tmux capture — sending an approval keystroke into an
// abandoned pane (deliberately never killed, see
// checkStuckSessionsOnce's doc comment) would act on a session nobody is
// waiting on.
func TestHandleApprove_DeregisteredAgent409(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp-g1", "myapp")
	deregisteredAt := time.Now()
	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp-g1"].DeregisteredAt = &deregisteredAt
	srv.stuck.mu.Lock()
	srv.stuck.byName["orchestrator-myapp-g1"] = &stuckSessionState{Stuck: true, Excerpt: "stale prompt"}
	srv.stuck.mu.Unlock()
	var captureCalls int
	srv.stuckCapture = func(session string) (string, error) {
		captureCalls++
		return "", nil
	}
	srv.mu.Unlock()

	resp := mustPost(t, ts.URL+"/agents/approve", map[string]any{"name": "orchestrator-myapp-g1", "option": "once"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for a deregistered agent even with a stale Stuck=true, got %d", resp.StatusCode)
	}
	if captureCalls != 0 {
		t.Errorf("expected no tmux capture attempted for a deregistered agent, got %d calls", captureCalls)
	}
}

// TestHandleApprove_UnknownAgent404 verifies approving a never-registered
// agent name fails.
func TestHandleApprove_UnknownAgent404(t *testing.T) {
	_, ts := newTestServer(t)
	resp := mustPost(t, ts.URL+"/agents/approve", map[string]any{"name": "ghost", "option": "once"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

// TestHandleApprove_NoSendWhenOptionGoneOnRecapture verifies the
// re-capture safety: the dialog-time state said stuck, but the FRESH
// capture no longer offers the requested option (e.g. the prompt shrank
// to a 2-option Yes/No between the client's poll and this call) — no key
// sent, sent=false with a reason.
func TestHandleApprove_NoSendWhenOptionGoneOnRecapture(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp-g1", "myapp")

	srv.mu.Lock()
	srv.stuck.byName["orchestrator-myapp-g1"] = &stuckSessionState{Stuck: true}
	srv.stuckCapture = func(session string) (string, error) {
		return "Do you want to proceed?\n❯ 1. Yes\n2. No", nil // no "always" option
	}
	var sendCalls int
	srv.stuckSendKeys = func(session, keys string) error {
		sendCalls++
		return nil
	}
	srv.mu.Unlock()

	resp := mustPost(t, ts.URL+"/agents/approve", map[string]any{"name": "orchestrator-myapp-g1", "option": "always"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 (a resolved-but-unsent request, not an error), got %d", resp.StatusCode)
	}
	var out struct {
		Sent   bool   `json:"sent"`
		Reason string `json:"reason"`
	}
	decodeInto(t, resp, &out)
	if out.Sent {
		t.Error("expected sent=false when the option is gone on re-capture")
	}
	if out.Reason == "" {
		t.Error("expected a non-empty reason")
	}
	if sendCalls != 0 {
		t.Errorf("expected no keys sent, got %d calls", sendCalls)
	}
}

// TestHandleApprove_ClearsStuckWhenPromptAlreadyGone: the user answers the
// prompt in the pane (or it resolves) and THEN clicks approve in the tray.
// The approval is correctly not sent — and the stuck state must clear on
// the spot, not at the next detector tick (live: 95s of stale ⛔ after an
// approval, 2026-09-04).
func TestHandleApprove_ClearsStuckWhenPromptAlreadyGone(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp-g1", "myapp")

	srv.mu.Lock()
	srv.stuck.byName["orchestrator-myapp-g1"] = &stuckSessionState{Stuck: true, StuckSince: time.Now()}
	srv.stuckCapture = func(session string) (string, error) {
		return "⏺ Ran 1 shell command\n❯ \n", nil // prompt gone, session idle
	}
	srv.stuckSendKeys = func(session, keys string) error { t.Errorf("no keys must be sent, got %q", keys); return nil }
	srv.mu.Unlock()

	resp := mustPost(t, ts.URL+"/agents/approve", map[string]any{"name": "orchestrator-myapp-g1", "option": "once"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out struct {
		Sent bool `json:"sent"`
	}
	decodeInto(t, resp, &out)
	if out.Sent {
		t.Error("expected sent=false")
	}
	if info := srv.stuck.snapshot()["orchestrator-myapp-g1"]; info.Stuck {
		t.Error("stuck state must be cleared immediately by the approve path, not left for the next tick")
	}
}

// TestHandleApprove_RescansAfterSend: after a successfully sent approval
// the pane is re-scanned shortly after, so a consumed prompt clears
// without waiting for the periodic tick.
func TestHandleApprove_RescansAfterSend(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp-g1", "myapp")
	done := make(chan struct{})
	var mu sync.Mutex
	pane := "Do you want to proceed?\n❯ 1. Yes\n2. No"
	srv.mu.Lock()
	srv.approveRescanDelay = 0
	srv.approveRescanDone = func() { close(done) }
	srv.stuck.byName["orchestrator-myapp-g1"] = &stuckSessionState{Stuck: true, StuckSince: time.Now()}
	srv.stuckCapture = func(session string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		return pane, nil
	}
	srv.stuckSendKeys = func(session, keys string) error {
		mu.Lock()
		pane = "⏺ proceeding\n❯ \n" // Claude consumed the key
		mu.Unlock()
		return nil
	}
	srv.mu.Unlock()

	resp := mustPost(t, ts.URL+"/agents/approve", map[string]any{"name": "orchestrator-myapp-g1", "option": "once"})
	resp.Body.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("post-send rescan never completed")
	}
	if info := srv.stuck.snapshot()["orchestrator-myapp-g1"]; info.Stuck {
		t.Error("stuck state should clear via the post-send rescan")
	}
}

// TestHandleApprove_InvalidOption400 verifies an unrecognized option
// string is rejected.
func TestHandleApprove_InvalidOption400(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp-g1", "myapp")
	srv.mu.Lock()
	srv.stuck.byName["orchestrator-myapp-g1"] = &stuckSessionState{Stuck: true}
	srv.mu.Unlock()

	resp := mustPost(t, ts.URL+"/agents/approve", map[string]any{"name": "orchestrator-myapp-g1", "option": "maybe"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for an unrecognized option, got %d", resp.StatusCode)
	}
}
