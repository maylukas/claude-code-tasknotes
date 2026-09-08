package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// --- POST /hooks/permission-request (handleHookPermissionRequest) ---

func TestHandleHookPermissionRequest_MissingAgentName_400(t *testing.T) {
	_, ts := newTestServer(t)
	resp := mustPost(t, ts.URL+"/hooks/permission-request", map[string]any{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for a missing agentName, got %d", resp.StatusCode)
	}
}

func TestHandleHookPermissionRequest_UnknownAgent_404(t *testing.T) {
	_, ts := newTestServer(t)
	resp := mustPost(t, ts.URL+"/hooks/permission-request", map[string]any{"agentName": "orchestrator-nonexistent"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for an unregistered agent, got %d", resp.StatusCode)
	}
}

// TestHandleHookPermissionRequest_UnresolvableSession_Conflict verifies an
// agent with a name that doesn't resolve to any tmux session (no reported
// TmuxSession, and a name that doesn't match the legacy orchestrator-
// derivation) is rejected rather than silently doing nothing — there's no
// pane the daemon could ever capture for it.
func TestHandleHookPermissionRequest_UnresolvableSession_Conflict(t *testing.T) {
	_, ts := newTestServer(t)
	register(t, ts, "not-an-orchestrator-name", "myapp")
	resp := mustPost(t, ts.URL+"/hooks/permission-request", map[string]any{"agentName": "not-an-orchestrator-name"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("expected 409 for an agent with no resolvable session, got %d", resp.StatusCode)
	}
}

// TestHandleHookPermissionRequest_FirstFire_SetsHookConfirmedAt verifies
// the proof marker — see Agent.HookConfirmedAt's doc comment — is set the
// first time this agent's hook is ever observed to fire.
func TestHandleHookPermissionRequest_FirstFire_SetsHookConfirmedAt(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp", "myapp")
	captureDone := make(chan struct{}, 1)
	srv.stuckCapture = func(session string) (string, error) {
		captureDone <- struct{}{}
		return "no marker here", nil
	}

	srv.mu.Lock()
	if srv.state.Agents["orchestrator-myapp"].HookConfirmedAt != nil {
		t.Fatal("expected HookConfirmedAt nil before any hook POST")
	}
	srv.mu.Unlock()

	// HookConfirmedAt itself is set synchronously inside the handler,
	// before it even responds — but the test must still wait for the
	// async triggerHookConfirmedStuck goroutine to finish before this
	// function returns and its (implicit, via t.Cleanup in newTestServer)
	// teardown runs, or the goroutine's read of srv.stuckCapture races
	// against teardown mutating server state — a race in the TEST, not in
	// production (nothing in real usage mutates stuckCapture after setup).
	resp := mustPost(t, ts.URL+"/hooks/permission-request", map[string]any{"agentName": "orchestrator-myapp"})
	resp.Body.Close()
	select {
	case <-captureDone:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the async capture to have run")
	}

	srv.mu.Lock()
	confirmedAt := srv.state.Agents["orchestrator-myapp"].HookConfirmedAt
	srv.mu.Unlock()
	if confirmedAt == nil {
		t.Fatal("expected HookConfirmedAt set after the first hook POST")
	}
}

// TestHandleHookPermissionRequest_SecondFire_DoesNotResetHookConfirmedAt
// verifies the marker records the FIRST proof, not the most recent one —
// a second (or thousandth) fire must not keep rewriting it.
func TestHandleHookPermissionRequest_SecondFire_DoesNotResetHookConfirmedAt(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp", "myapp")
	captureDone := make(chan struct{}, 2)
	srv.stuckCapture = func(session string) (string, error) {
		captureDone <- struct{}{}
		return "no marker here", nil
	}
	waitForCapture := func() {
		t.Helper()
		select {
		case <-captureDone:
		case <-time.After(2 * time.Second):
			t.Fatal("expected the async capture to have run")
		}
	}

	resp1 := mustPost(t, ts.URL+"/hooks/permission-request", map[string]any{"agentName": "orchestrator-myapp"})
	resp1.Body.Close()
	waitForCapture()
	srv.mu.Lock()
	first := *srv.state.Agents["orchestrator-myapp"].HookConfirmedAt
	srv.mu.Unlock()

	time.Sleep(5 * time.Millisecond) // ensure a second fire would produce a detectably different timestamp if wrongly overwritten
	resp2 := mustPost(t, ts.URL+"/hooks/permission-request", map[string]any{"agentName": "orchestrator-myapp"})
	resp2.Body.Close()
	waitForCapture()
	srv.mu.Lock()
	second := *srv.state.Agents["orchestrator-myapp"].HookConfirmedAt
	srv.mu.Unlock()

	if !first.Equal(second) {
		t.Errorf("expected HookConfirmedAt to stay at its first value, got first=%v second=%v", first, second)
	}
}

// TestHandleHookPermissionRequest_RespondsBeforeAsyncWorkCompletes is the
// safety-critical property: the HTTP response must return immediately,
// never waiting on the (potentially minutes-long, human-waiting) work
// triggered by a genuine pending prompt. Injects a capture function that
// blocks until the test releases it, standing in for however long the
// real async work (pane capture, possibly a dialog) might take.
func TestHandleHookPermissionRequest_RespondsBeforeAsyncWorkCompletes(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp", "myapp")

	captureStarted := make(chan struct{})
	captureBlock := make(chan struct{})
	captureReturned := make(chan struct{})
	srv.stuckCapture = func(session string) (string, error) {
		close(captureStarted)
		<-captureBlock
		defer close(captureReturned)
		return "no marker here", nil
	}

	start := time.Now()
	resp := mustPost(t, ts.URL+"/hooks/permission-request", map[string]any{"agentName": "orchestrator-myapp"})
	elapsed := time.Since(start)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("expected the HTTP response to return immediately without waiting on async work, took %v", elapsed)
	}

	select {
	case <-captureStarted:
		// good — the async work genuinely started
	case <-time.After(2 * time.Second):
		t.Fatal("expected the async capture to have started shortly after the HTTP response")
	}
	close(captureBlock) // release it so the goroutine can finish

	// Wait for the goroutine to fully return before this test function
	// does — otherwise its read of srv.stuckCapture can race against
	// whatever runs at teardown. A race in test synchronization, not in
	// production (nothing in real usage mutates stuckCapture after setup).
	select {
	case <-captureReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the async capture to have returned")
	}
}

// --- triggerHookConfirmedStuck ---

// TestTriggerHookConfirmedStuck_MarkerPresent_ConfirmsImmediately is the
// core payoff: a SINGLE hook-triggered call reaches confirmedStuck right
// away, unlike pane-scraping's own fastConfirm (needs 2 consecutive
// matching ticks) or backstopConfirm (needs stuckPersistenceBackstopTicks
// consecutive ticks).
func TestTriggerHookConfirmedStuck_MarkerPresent_ConfirmsImmediately(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.stuckCapture = func(session string) (string, error) {
		return "Do you want to proceed?\n❯ 1. Yes\n2. No", nil
	}
	defer func() { srv.stuckCapture = nil }()
	t.Setenv("TN_NO_DIALOG", "1") // avoid any real osascript call in tests

	srv.triggerHookConfirmedStuck("orchestrator-myapp", "tn-myapp", "", "")

	info := srv.stuck.snapshot()["orchestrator-myapp"]
	if !info.Stuck {
		t.Errorf("expected Stuck true after a single hook-triggered call, got %+v", info)
	}
}

// TestTriggerHookConfirmedStuck_PromptAlreadyResolved_DoesNotConfirm is
// the safety re-verification: if the prompt has already resolved by the
// time the fresh capture lands (a fast auto-response, or a human who was
// already watching), the hook firing must not manufacture a stuck state
// out of a pane that no longer shows one.
func TestTriggerHookConfirmedStuck_PromptAlreadyResolved_DoesNotConfirm(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.stuckCapture = func(session string) (string, error) {
		return "already resolved, nothing pending here", nil
	}
	defer func() { srv.stuckCapture = nil }()
	t.Setenv("TN_NO_DIALOG", "1")

	srv.triggerHookConfirmedStuck("orchestrator-myapp", "tn-myapp", "", "")

	info := srv.stuck.snapshot()["orchestrator-myapp"]
	if info.Stuck {
		t.Errorf("expected Stuck false when the fresh capture shows no marker, got %+v", info)
	}
}

// TestTriggerHookConfirmedStuck_CaptureError_NoOp verifies a failed
// capture (session gone, tmux error) is logged and gives up cleanly
// rather than treating a capture failure as evidence of anything.
func TestTriggerHookConfirmedStuck_CaptureError_NoOp(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.stuckCapture = func(session string) (string, error) {
		return "", errors.New("capture failed")
	}
	defer func() { srv.stuckCapture = nil }()
	t.Setenv("TN_NO_DIALOG", "1")

	srv.triggerHookConfirmedStuck("orchestrator-myapp", "tn-myapp", "", "")

	info := srv.stuck.snapshot()["orchestrator-myapp"]
	if info.Stuck {
		t.Errorf("expected Stuck false after a capture error, got %+v", info)
	}
}

// TestTriggerHookConfirmedStuck_CommandThreadsIntoExcerpt verifies the
// hook's own tool_input.command (already extracted by hooks/claude-hook.sh
// and already sent for the sibling PermissionDenied event, but silently
// discarded here until 2026-08-26) reaches the displayed excerpt — the fix
// for the live incident where a user could see a prompt's options but not
// the command those options were approving.
func TestTriggerHookConfirmedStuck_CommandThreadsIntoExcerpt(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.stuckCapture = func(session string) (string, error) {
		return "Do you want to proceed?\n❯ 1. Yes\n2. No", nil
	}
	defer func() { srv.stuckCapture = nil }()
	t.Setenv("TN_NO_DIALOG", "1")

	srv.triggerHookConfirmedStuck("orchestrator-myapp", "tn-myapp", "Bash", "curl -s -m 3 http://localhost:9023/health")

	info := srv.stuck.snapshot()["orchestrator-myapp"]
	if !info.Stuck {
		t.Fatalf("expected Stuck true, got %+v", info)
	}
	if !strings.Contains(info.Excerpt, "curl -s -m 3 http://localhost:9023/health") {
		t.Errorf("expected the hook-reported command in the excerpt, got:\n%s", info.Excerpt)
	}
}

// TestTriggerHookConfirmedStuck_EmptyCommandFallsBackToPaneOnly verifies a
// hook fire with no command (a non-Bash-shaped tool) doesn't break anything
// — display just falls back to the pane capture alone, no "[hook]" prefix.
func TestTriggerHookConfirmedStuck_EmptyCommandFallsBackToPaneOnly(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.stuckCapture = func(session string) (string, error) {
		return "Do you want to proceed?\n❯ 1. Yes\n2. No", nil
	}
	defer func() { srv.stuckCapture = nil }()
	t.Setenv("TN_NO_DIALOG", "1")

	srv.triggerHookConfirmedStuck("orchestrator-myapp", "tn-myapp", "Read", "")

	info := srv.stuck.snapshot()["orchestrator-myapp"]
	if !info.Stuck {
		t.Fatalf("expected Stuck true, got %+v", info)
	}
	if strings.HasPrefix(info.Excerpt, "[hook]") {
		t.Errorf("expected no hook prefix with an empty command, got:\n%s", info.Excerpt)
	}
}

// --- commandSummary ---

func TestCommandSummary(t *testing.T) {
	cases := []struct {
		command string
		want    string
	}{
		{"git push --force origin main", "git push"},
		{"glab mr update 123 --target-branch integration/main", "glab mr"},
		{"echo hello", "echo hello"},
		{"echo", "echo"},
		{"", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := commandSummary(c.command); got != c.want {
			t.Errorf("commandSummary(%q) = %q, want %q", c.command, got, c.want)
		}
	}
}

// --- POST /hooks/permission-denied (handleHookPermissionDenied) — pure
// telemetry, no decision, no retry, no session/pane interaction at all. ---

func TestHandleHookPermissionDenied_MissingAgentName_400(t *testing.T) {
	_, ts := newTestServer(t)
	resp := mustPost(t, ts.URL+"/hooks/permission-denied", map[string]any{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for a missing agentName, got %d", resp.StatusCode)
	}
}

func TestHandleHookPermissionDenied_UnknownAgent_404(t *testing.T) {
	_, ts := newTestServer(t)
	resp := mustPost(t, ts.URL+"/hooks/permission-denied", map[string]any{"agentName": "orchestrator-nonexistent"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for an unregistered agent, got %d", resp.StatusCode)
	}
}

// TestHandleHookPermissionDenied_FirstFire_CreatesRecord verifies a new
// denial creates a fresh telemetry record with Count=1.
func TestHandleHookPermissionDenied_FirstFire_CreatesRecord(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp", "myapp")

	resp := mustPost(t, ts.URL+"/hooks/permission-denied", map[string]any{
		"agentName": "orchestrator-myapp", "toolName": "Bash", "command": "glab mr update 123",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	srv.mu.Lock()
	rec := srv.state.PermissionDenials["orchestrator-myapp|Bash|glab mr"]
	srv.mu.Unlock()
	if rec == nil {
		t.Fatal("expected a telemetry record to be created")
	}
	if rec.Count != 1 || rec.LastCommand != "glab mr update 123" || rec.CommandSummary != "glab mr" {
		t.Errorf("unexpected record: %+v", rec)
	}
	if rec.FirstSeenAt.IsZero() || rec.LastSeenAt.IsZero() {
		t.Errorf("expected FirstSeenAt/LastSeenAt set, got %+v", rec)
	}
}

// TestHandleHookPermissionDenied_RepeatedDenials_AccumulateOneRecord is
// the core of why this is grouped by command SHAPE, not exact text: the
// same underlying block (glab mr update) recurring with different MR
// numbers must accumulate into ONE counter, not a fresh record each time.
func TestHandleHookPermissionDenied_RepeatedDenials_AccumulateOneRecord(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp", "myapp")

	for _, mr := range []string{"123", "456", "789"} {
		resp := mustPost(t, ts.URL+"/hooks/permission-denied", map[string]any{
			"agentName": "orchestrator-myapp", "toolName": "Bash", "command": "glab mr update " + mr,
		})
		resp.Body.Close()
	}

	srv.mu.Lock()
	n := len(srv.state.PermissionDenials)
	rec := srv.state.PermissionDenials["orchestrator-myapp|Bash|glab mr"]
	srv.mu.Unlock()

	if n != 1 {
		t.Errorf("expected exactly 1 accumulated record for the same command shape, got %d records", n)
	}
	if rec == nil || rec.Count != 3 {
		t.Errorf("expected Count=3 after 3 denials of the same shape, got %+v", rec)
	}
	if rec.LastCommand != "glab mr update 789" {
		t.Errorf("expected LastCommand to be the most recent one, got %q", rec.LastCommand)
	}
}

// TestHandleHookPermissionDenied_DifferentCommandShapes_SeparateRecords
// verifies distinct command shapes for the SAME agent don't collapse
// into one counter — "git push" and "glab mr update" are different
// problems a human would want to see separately.
func TestHandleHookPermissionDenied_DifferentCommandShapes_SeparateRecords(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp", "myapp")

	resp1 := mustPost(t, ts.URL+"/hooks/permission-denied", map[string]any{
		"agentName": "orchestrator-myapp", "toolName": "Bash", "command": "git push --force origin main",
	})
	resp1.Body.Close()
	resp2 := mustPost(t, ts.URL+"/hooks/permission-denied", map[string]any{
		"agentName": "orchestrator-myapp", "toolName": "Bash", "command": "glab mr update 123",
	})
	resp2.Body.Close()

	srv.mu.Lock()
	n := len(srv.state.PermissionDenials)
	srv.mu.Unlock()
	if n != 2 {
		t.Errorf("expected 2 separate records for 2 different command shapes, got %d", n)
	}
}

// TestHandleHookPermissionDenied_NeverInteractsWithStuckState verifies
// this handler is genuinely passive telemetry — it must never touch the
// stuck-detector state machine, unlike handleHookPermissionRequest.
func TestHandleHookPermissionDenied_NeverInteractsWithStuckState(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp", "myapp")
	captureCalled := false
	srv.stuckCapture = func(session string) (string, error) {
		captureCalled = true
		return "no marker here", nil
	}
	defer func() { srv.stuckCapture = nil }()

	resp := mustPost(t, ts.URL+"/hooks/permission-denied", map[string]any{
		"agentName": "orchestrator-myapp", "toolName": "Bash", "command": "rm -rf /",
	})
	resp.Body.Close()

	if captureCalled {
		t.Error("expected PermissionDenied telemetry to never trigger a pane capture")
	}
	info := srv.stuck.snapshot()["orchestrator-myapp"]
	if info.Stuck {
		t.Error("expected PermissionDenied telemetry to never set Stuck")
	}
}
