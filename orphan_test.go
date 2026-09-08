package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestStartupGraceRemaining covers the startup-grace duration math
// directly and deterministically (same reasoning as TestIsHostSleepGap) —
// no actual waiting on a real aliveWindow.
func TestStartupGraceRemaining(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name      string
		startedAt time.Time
		wantZero  bool
	}{
		{"just started", now, false},
		{"halfway through the window", now.Add(-aliveWindow / 2), false},
		{"just under the window", now.Add(-aliveWindow + time.Second), false},
		{"exactly at the window", now.Add(-aliveWindow), true},
		{"well past the window", now.Add(-2 * aliveWindow), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := startupGraceRemaining(c.startedAt, now)
			if (got == 0) != c.wantZero {
				t.Errorf("startupGraceRemaining(%v, now) = %v, want zero=%v", c.startedAt, got, c.wantZero)
			}
			if got < 0 {
				t.Errorf("startupGraceRemaining must never be negative, got %v", got)
			}
		})
	}
}

// fakeOrphanTaskAPI serves GetTask lookups for the orphaned-task safety
// net's per-path status checks.
func fakeOrphanTaskAPI(t *testing.T, tasksByPath map[string]Task) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
		task, ok := tasksByPath[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"error": "not found"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": task})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeOrphanResetAPI is fakeOrphanTaskAPI's mutable sibling: GET returns
// the current in-memory state (so a task genuinely reflects a prior
// reset), and PUT actually applies the partial update (status/details)
// and records the call — needed to verify the orphan-RESET path
// end-to-end (status really becomes "open", the note really lands),
// not just that a message got queued.
func fakeOrphanResetAPI(t *testing.T, tasksByPath map[string]Task) (*httptest.Server, *[]struct {
	Path string
	Body map[string]any
}) {
	t.Helper()
	var mu sync.Mutex
	tasks := map[string]Task{}
	for k, v := range tasksByPath {
		tasks[k] = v
	}
	var calls []struct {
		Path string
		Body map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
		switch r.Method {
		case http.MethodGet:
			mu.Lock()
			task, ok := tasks[id]
			mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				json.NewEncoder(w).Encode(map[string]any{"error": "not found"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"data": task})
		case http.MethodPut:
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			task := tasks[id]
			if status, ok := body["status"].(string); ok {
				task.Status = status
			}
			if details, ok := body["details"].(string); ok {
				task.Details = details
			}
			tasks[id] = task
			calls = append(calls, struct {
				Path string
				Body map[string]any
			}{Path: id, Body: body})
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"data": task})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// seedOwnedTask appends an acked "Task assigned:" message so
// taskOwnerLocked resolves path's owner to ownerName.
func seedOwnedTask(srv *Server, ownerName, path, title string) {
	ackedAt := time.Now().Add(-time.Minute)
	srv.mu.Lock()
	srv.state.Messages = append(srv.state.Messages, &Message{
		ID: "assign-" + path, To: ownerName, TaskPath: path,
		Text: "Task assigned: " + title + " (" + path + ")", Status: "acked",
		CreatedAt: ackedAt, AckedAt: &ackedAt,
	})
	srv.mu.Unlock()
}

// TestHandleRetire_OrphanedInProgressTaskNotifiesAcceptingAgent verifies a
// clean retire with an owned in-progress task queues exactly one
// informational message, listing the task, to another accepting agent for
// the same project.
func TestHandleRetire_OrphanedInProgressTaskNotifiesAcceptingAgent(t *testing.T) {
	const path = "Tasks/Foo.md"
	apiSrv := fakeOrphanTaskAPI(t, map[string]Task{
		path: {Path: path, Title: "Fix the thing", Status: "in-progress"},
	})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})

	register(t, ts, "orchestrator-myapp-g1", "myapp")
	register(t, ts, "orchestrator-myapp-g2", "myapp")
	seedOwnedTask(srv, "orchestrator-myapp-g1", path, "Fix the thing")

	resp := mustPost(t, ts.URL+"/agents/retire", map[string]any{"name": "orchestrator-myapp-g1"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	msgs := queuedMessagesTo(srv, "orchestrator-myapp-g2")
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 orphan-notification message, got %+v", msgs)
	}
	text := msgs[0].Text
	if !strings.Contains(text, "orchestrator-myapp-g1") || !strings.Contains(text, "Fix the thing") || !strings.Contains(text, path) {
		t.Errorf("expected the message to name the dead agent and list the task, got %q", text)
	}
	// The reset already happened by the time this message is read — it's
	// context, not an action item (see checkOrphanedTasks's doc comment:
	// the original ask-someone-else-to-adopt-it design is what let a week
	// of orphaned tasks pile up undetected).
	if !strings.Contains(text, "FYI") || !strings.Contains(text, "not an action item") || !strings.Contains(text, "already reset to open") {
		t.Errorf("expected the FYI-not-action-item framing, got %q", text)
	}
}

// TestHandleRetire_NoInProgressOwnedTasks_NoMessage verifies an owned task
// that isn't (or is no longer) in-progress produces no orphan message.
func TestHandleRetire_NoInProgressOwnedTasks_NoMessage(t *testing.T) {
	const path = "Tasks/Foo.md"
	apiSrv := fakeOrphanTaskAPI(t, map[string]Task{
		path: {Path: path, Title: "Fix the thing", Status: "review"},
	})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})

	register(t, ts, "orchestrator-myapp-g1", "myapp")
	register(t, ts, "orchestrator-myapp-g2", "myapp")
	seedOwnedTask(srv, "orchestrator-myapp-g1", path, "Fix the thing")

	resp := mustPost(t, ts.URL+"/agents/retire", map[string]any{"name": "orchestrator-myapp-g1"})
	resp.Body.Close()

	if msgs := queuedMessagesTo(srv, "orchestrator-myapp-g2"); len(msgs) != 0 {
		t.Errorf("expected no orphan message for a task that isn't in-progress, got %+v", msgs)
	}
}

// TestHandleRetire_NoOwnedTasks_NoMessage verifies an agent retiring with
// nothing owned at all produces no message and no API calls.
func TestHandleRetire_NoOwnedTasks_NoMessage(t *testing.T) {
	apiSrv := fakeOrphanTaskAPI(t, map[string]Task{})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})

	register(t, ts, "orchestrator-myapp-g1", "myapp")
	register(t, ts, "orchestrator-myapp-g2", "myapp")

	resp := mustPost(t, ts.URL+"/agents/retire", map[string]any{"name": "orchestrator-myapp-g1"})
	resp.Body.Close()

	if msgs := queuedMessagesTo(srv, "orchestrator-myapp-g2"); len(msgs) != 0 {
		t.Errorf("expected no orphan message when nothing is owned, got %+v", msgs)
	}
}

// TestHandleRetire_RepeatRetireDoesNotDuplicateOrphanMessage verifies
// retiring the same (already-retired) agent name again doesn't re-fire the
// orphan check — OrphanHandledAt dedups per lifecycle.
func TestHandleRetire_RepeatRetireDoesNotDuplicateOrphanMessage(t *testing.T) {
	const path = "Tasks/Foo.md"
	apiSrv := fakeOrphanTaskAPI(t, map[string]Task{
		path: {Path: path, Title: "Fix the thing", Status: "in-progress"},
	})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})

	register(t, ts, "orchestrator-myapp-g1", "myapp")
	register(t, ts, "orchestrator-myapp-g2", "myapp")
	seedOwnedTask(srv, "orchestrator-myapp-g1", path, "Fix the thing")

	resp1 := mustPost(t, ts.URL+"/agents/retire", map[string]any{"name": "orchestrator-myapp-g1"})
	resp1.Body.Close()
	resp2 := mustPost(t, ts.URL+"/agents/retire", map[string]any{"name": "orchestrator-myapp-g1"})
	resp2.Body.Close()

	if msgs := queuedMessagesTo(srv, "orchestrator-myapp-g2"); len(msgs) != 1 {
		t.Errorf("expected exactly 1 orphan message despite retiring twice, got %+v", msgs)
	}
}

// TestCheckOrphanedDeathsOnce_DiedWithoutRetiring verifies the OTHER
// trigger: an agent that goes stale (alive window elapsed) without ever
// being deregistered/retired gets caught by the periodic liveness check
// and its owned in-progress work gets flagged the same way.
func TestCheckOrphanedDeathsOnce_DiedWithoutRetiring(t *testing.T) {
	t.Setenv("TN_ENABLE_ORPHAN_SWEEP", "1") // gated off by default; this test exercises the enabled path
	const path = "Tasks/Foo.md"
	apiSrv := fakeOrphanTaskAPI(t, map[string]Task{
		path: {Path: path, Title: "Fix the thing", Status: "in-progress"},
	})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	srv.startedAt = time.Now().Add(-2 * aliveWindow) // clear of the startup grace period

	register(t, ts, "orchestrator-myapp-g1", "myapp")
	register(t, ts, "orchestrator-myapp-g2", "myapp")
	seedOwnedTask(srv, "orchestrator-myapp-g1", path, "Fix the thing")

	// Simulate g1 having gone stale (crashed without deregistering) by
	// backdating its LastSeenAt well past aliveWindow.
	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp-g1"].LastSeenAt = time.Now().Add(-5 * time.Minute)
	srv.mu.Unlock()

	// First pass: one miss is not enough (deadConfirmTicks=2) — no
	// message yet, but the streak must be recorded.
	srv.checkOrphanedDeathsOnce()
	if msgs := queuedMessagesTo(srv, "orchestrator-myapp-g2"); len(msgs) != 0 {
		t.Fatalf("expected no message after a single missed tick, got %+v", msgs)
	}
	srv.mu.Lock()
	streak := srv.state.Agents["orchestrator-myapp-g1"].ConsecutiveDeadTicks
	srv.mu.Unlock()
	if streak != 1 {
		t.Fatalf("expected ConsecutiveDeadTicks=1 after one missed tick, got %d", streak)
	}

	// Second consecutive pass: now it fires.
	srv.checkOrphanedDeathsOnce()
	msgs := queuedMessagesTo(srv, "orchestrator-myapp-g2")
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "orchestrator-myapp-g1") {
		t.Fatalf("expected 1 orphan message naming the dead agent after the second consecutive miss, got %+v", msgs)
	}

	// A repeat pass must not duplicate it.
	srv.checkOrphanedDeathsOnce()
	if msgs := queuedMessagesTo(srv, "orchestrator-myapp-g2"); len(msgs) != 1 {
		t.Errorf("expected still exactly 1 message after a repeat pass, got %+v", msgs)
	}
}

// TestCheckOrphanedDeathsOnce_AliveAgentUntouched verifies a currently
// alive agent is never flagged.
func TestCheckOrphanedDeathsOnce_AliveAgentUntouched(t *testing.T) {
	apiSrv := fakeOrphanTaskAPI(t, map[string]Task{})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	srv.startedAt = time.Now().Add(-2 * aliveWindow) // clear of the startup grace period
	register(t, ts, "orchestrator-myapp-g1", "myapp")

	srv.checkOrphanedDeathsOnce()

	srv.mu.Lock()
	handled := srv.state.Agents["orchestrator-myapp-g1"].OrphanHandledAt
	srv.mu.Unlock()
	if !handled.IsZero() {
		t.Errorf("expected an alive agent's OrphanHandledAt to stay unset, got %v", handled)
	}
}

// TestCheckOrphanedDeathsOnce_StartupGracePeriod_NoActionEvenAfterTwoTicks
// verifies fix 1: within aliveWindow of Server construction, the detector
// must be a complete no-op — not just gated at the enabled-flag level, but
// not even accumulating ConsecutiveDeadTicks — regardless of how many
// times it's called or how stale an agent's LastSeenAt looks. A
// LastSeenAt predating THIS process's own start is absence of signal, not
// evidence of death (see the 2026-08-25 incident: every agent looks dead
// immediately after a restart).
func TestCheckOrphanedDeathsOnce_StartupGracePeriod_NoActionEvenAfterTwoTicks(t *testing.T) {
	t.Setenv("TN_ENABLE_ORPHAN_SWEEP", "1")
	const path = "Tasks/Foo.md"
	apiSrv := fakeOrphanTaskAPI(t, map[string]Task{
		path: {Path: path, Title: "Fix the thing", Status: "in-progress"},
	})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	// srv.startedAt defaults to "now" (newServer sets it at construction)
	// — still inside the grace period.

	register(t, ts, "orchestrator-myapp-g1", "myapp")
	register(t, ts, "orchestrator-myapp-g2", "myapp")
	seedOwnedTask(srv, "orchestrator-myapp-g1", path, "Fix the thing")

	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp-g1"].LastSeenAt = time.Now().Add(-5 * time.Minute)
	srv.mu.Unlock()

	// Two calls — enough to satisfy deadConfirmTicks if the grace period
	// weren't gating first.
	srv.checkOrphanedDeathsOnce()
	srv.checkOrphanedDeathsOnce()

	if msgs := queuedMessagesTo(srv, "orchestrator-myapp-g2"); len(msgs) != 0 {
		t.Errorf("expected no orphan message during the startup grace period, got %+v", msgs)
	}
	srv.mu.Lock()
	streak := srv.state.Agents["orchestrator-myapp-g1"].ConsecutiveDeadTicks
	srv.mu.Unlock()
	if streak != 0 {
		t.Errorf("expected ConsecutiveDeadTicks to stay 0 during the grace period, got %d", streak)
	}
}

// TestCheckOrphanedDeathsOnce_NeverObservedThisLifetime_TreatedAsUnknownNotDead
// is the per-agent half of fix 1, added after independently verifying the
// 2026-08-25 incident was 11-of-11, not a partial misfire: the global
// startup-uptime timer alone is not enough, because an agent that simply
// never gets a chance to poll (host under load, slow tmux attach, or
// genuinely dead from before this restart) looks IDENTICAL to a dead one
// once the timer alone has elapsed. An agent absent from seenSinceStart —
// loaded from persisted state at startup, like every real agent record
// is, but never actually observed acting THIS process's lifetime — must
// stay excluded no matter how stale its LastSeenAt looks or how many
// ticks pass, until it proves it can report in at all.
func TestCheckOrphanedDeathsOnce_NeverObservedThisLifetime_TreatedAsUnknownNotDead(t *testing.T) {
	t.Setenv("TN_ENABLE_ORPHAN_SWEEP", "1")
	const path = "Tasks/Foo.md"
	apiSrv := fakeOrphanTaskAPI(t, map[string]Task{
		path: {Path: path, Title: "Fix the thing", Status: "in-progress"},
	})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	srv.startedAt = time.Now().Add(-2 * aliveWindow) // clear of the global grace period

	register(t, ts, "orchestrator-myapp-g2", "myapp") // the accepting peer that would receive the orphan message

	// Simulate an agent record present from BEFORE this restart (loaded
	// from state.json at startup) that has NOT yet been observed this
	// lifetime — deliberately inserted directly, bypassing register(),
	// so seenSinceStart never gets set for it.
	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp-g1"] = &Agent{
		Name: "orchestrator-myapp-g1", Project: "myapp",
		RegisteredAt: time.Now().Add(-24 * time.Hour),
		LastSeenAt:   time.Now().Add(-24 * time.Hour), // stale by any measure
	}
	srv.mu.Unlock()
	seedOwnedTask(srv, "orchestrator-myapp-g1", path, "Fix the thing")

	// Many ticks — enough to satisfy deadConfirmTicks many times over —
	// must still produce nothing while the agent stays unobserved.
	for i := 0; i < 5; i++ {
		srv.checkOrphanedDeathsOnce()
	}
	if msgs := queuedMessagesTo(srv, "orchestrator-myapp-g2"); len(msgs) != 0 {
		t.Fatalf("expected no orphan message for a never-observed agent, got %+v", msgs)
	}
	srv.mu.Lock()
	streak := srv.state.Agents["orchestrator-myapp-g1"].ConsecutiveDeadTicks
	srv.mu.Unlock()
	if streak != 0 {
		t.Fatalf("expected ConsecutiveDeadTicks to stay 0 for a never-observed agent, got %d", streak)
	}

	// Once it IS observed (any authenticated action — here, a poll),
	// normal two-tick tracking must start applying to it going forward.
	// LastSeenAt is fresh from the poll, so it now reads alive — no
	// death-tracking fires immediately, which is correct: it just
	// stopped being unknown, it didn't retroactively become dead.
	inboxResp, err := http.Get(ts.URL + "/inbox?name=orchestrator-myapp-g1&wait=0")
	if err != nil {
		t.Fatalf("inbox poll: %v", err)
	}
	inboxResp.Body.Close()
	srv.checkOrphanedDeathsOnce()
	if msgs := queuedMessagesTo(srv, "orchestrator-myapp-g2"); len(msgs) != 0 {
		t.Errorf("expected still no orphan message right after the agent's first observed poll, got %+v", msgs)
	}
}

// fakeTmuxSessionExistence returns a tmuxSessionExistenceFunc driven by
// an explicit per-session map — sessions absent from the map answer
// tmuxSessionUnknown, matching "the check itself couldn't resolve it"
// rather than silently defaulting to a specific state.
func fakeTmuxSessionExistence(states map[string]tmuxSessionState) tmuxSessionExistenceFunc {
	return func(session string) tmuxSessionState {
		if st, ok := states[session]; ok {
			return st
		}
		return tmuxSessionUnknown
	}
}

// seedUnobservedAgent registers an Agent record directly (bypassing
// register(), so seenSinceStart is never set for it) with the given tmux
// session, simulating an agent loaded from persisted state at startup
// that this process's lifetime has never actually heard from.
func seedUnobservedAgent(srv *Server, name, tmuxSession string) {
	srv.mu.Lock()
	srv.state.Agents[name] = &Agent{
		Name: name, Project: "myapp", TmuxSession: tmuxSession,
		RegisteredAt: time.Now().Add(-24 * time.Hour),
		LastSeenAt:   time.Now().Add(-24 * time.Hour),
	}
	srv.mu.Unlock()
}

// TestDeadTaskOwners_NeverObservedThisLifetime covers all four tmux-check
// outcomes for an owner never seen this process's lifetime — the
// cross-restart recovery path added after the 2026-08-25 incident, and
// the three-state discipline that keeps it from reintroducing the exact
// failure mode it fixes.
func TestDeadTaskOwners_NeverObservedThisLifetime(t *testing.T) {
	cases := []struct {
		name     string
		state    tmuxSessionState // omit the session from the fake map entirely for "no entry" cases
		noEntry  bool
		wantDead bool
	}{
		{name: "tmux confirms session gone -> recovered as dead", state: tmuxSessionGone, wantDead: true},
		{name: "tmux confirms session exists -> hung, not dead, excluded", state: tmuxSessionExists, wantDead: false},
		{name: "tmux check unresolvable -> unknown, never dead", noEntry: true, wantDead: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := newTestServer(t)
			const path = "Tasks/Foo.md"
			const session = "tn-myapp-g1"
			seedUnobservedAgent(srv, "orchestrator-myapp-g1", session)
			seedOwnedTask(srv, "orchestrator-myapp-g1", path, "Fix the thing")

			states := map[string]tmuxSessionState{}
			if !c.noEntry {
				states[session] = c.state
			}
			names := srv.deadTaskOwners(time.Now(), fakeTmuxSessionExistence(states))

			got := len(names) == 1 && names[0] == "orchestrator-myapp-g1"
			if got != c.wantDead {
				t.Errorf("wantDead=%v, got names=%+v", c.wantDead, names)
			}
		})
	}
}

// TestDeadTaskOwners_NoResolvableTmuxSession_Excluded covers an
// unobserved owner with no way to even ask tmux (empty TmuxSession and a
// name that doesn't fall back to a derivable one) — nothing to check, so
// nothing to confirm; must stay excluded rather than guessed at either
// way.
func TestDeadTaskOwners_NoResolvableTmuxSession_Excluded(t *testing.T) {
	srv, _ := newTestServer(t)
	const path = "Tasks/Foo.md"
	seedUnobservedAgent(srv, "not-orchestrator-prefixed", "") // tmuxSessionForAgent returns ok=false for this
	seedOwnedTask(srv, "not-orchestrator-prefixed", path, "Fix the thing")

	names := srv.deadTaskOwners(time.Now(), fakeTmuxSessionExistence(nil))
	if len(names) != 0 {
		t.Errorf("expected an agent with no resolvable tmux session to be excluded, got %+v", names)
	}
}

// TestCheckOrphanedDeathsOnce_DisabledByDefault_LeavesTaskAlone verifies
// the TN_ENABLE_ORPHAN_SWEEP gate: with it unset (the default), a
// dead-looking agent's owned in-progress task must NOT be reset and
// OrphanHandledAt must NOT be set, even after enough consecutive misses to
// satisfy deadConfirmTicks. This is the incident fix: liveness-based death
// detection defaults OFF until the false-positive path (single-sample
// LastSeenAt, no startup grace, no restart tolerance) is hardened.
func TestCheckOrphanedDeathsOnce_DisabledByDefault_LeavesTaskAlone(t *testing.T) {
	const path = "Tasks/Foo.md"
	apiSrv := fakeOrphanTaskAPI(t, map[string]Task{
		path: {Path: path, Title: "Fix the thing", Status: "in-progress"},
	})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	srv.startedAt = time.Now().Add(-2 * aliveWindow) // clear of the startup grace period

	register(t, ts, "orchestrator-myapp-g1", "myapp")
	register(t, ts, "orchestrator-myapp-g2", "myapp")
	seedOwnedTask(srv, "orchestrator-myapp-g1", path, "Fix the thing")

	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp-g1"].LastSeenAt = time.Now().Add(-5 * time.Minute)
	srv.mu.Unlock()

	// Two consecutive misses — enough to satisfy deadConfirmTicks — but
	// the flag being unset must still block any action.
	srv.checkOrphanedDeathsOnce()
	srv.checkOrphanedDeathsOnce()

	if msgs := queuedMessagesTo(srv, "orchestrator-myapp-g2"); len(msgs) != 0 {
		t.Errorf("expected no orphan message while TN_ENABLE_ORPHAN_SWEEP is unset, got %+v", msgs)
	}
	srv.mu.Lock()
	handled := srv.state.Agents["orchestrator-myapp-g1"].OrphanHandledAt
	srv.mu.Unlock()
	if !handled.IsZero() {
		t.Errorf("expected OrphanHandledAt to stay unset while the sweep is disabled, got %v", handled)
	}
}

// --- Orphan RESET (primary mechanism, not just a message — see
// checkOrphanedTasks/resetOrphanedInProgressTasksForAgent's doc comments) ---

// TestResetOrphanedInProgressTasksForAgent_DeadOwnerNoLiveWorker_ResetsWithNote
// is the core case: a dead agent's owned, still-in-progress, unadopted
// task is reset to open with a "not abandoned, not drifted" note.
func TestResetOrphanedInProgressTasksForAgent_DeadOwnerNoLiveWorker_ResetsWithNote(t *testing.T) {
	const path = "Tasks/Foo.md"
	apiSrv, calls := fakeOrphanResetAPI(t, map[string]Task{
		path: {Path: path, Title: "Fix the thing", Status: "in-progress", Details: "Original description."},
	})
	srv, _ := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	seedOwnedTask(srv, "orchestrator-myapp-g1", path, "Fix the thing")

	reset, skipped := srv.resetOrphanedInProgressTasksForAgent("orchestrator-myapp-g1")

	if len(reset) != 1 || !strings.Contains(reset[0], "Fix the thing") || !strings.Contains(reset[0], path) {
		t.Fatalf("expected 1 reset entry naming the task, got reset=%+v skipped=%+v", reset, skipped)
	}
	if len(skipped) != 0 {
		t.Errorf("expected nothing skipped, got %+v", skipped)
	}

	if len(*calls) != 1 {
		t.Fatalf("expected exactly 1 PUT, got %d: %+v", len(*calls), *calls)
	}
	if (*calls)[0].Body["status"] != "open" {
		t.Errorf("expected status:open in the PUT body, got %+v", (*calls)[0].Body)
	}
	details, _ := (*calls)[0].Body["details"].(string)
	if !strings.Contains(details, "orchestrator-myapp-g1") || !strings.Contains(details, "Not abandoned, not drifted") {
		t.Errorf("expected the note naming the dead agent in the same 'not abandoned, not drifted' voice as sleep-death recovery, got:\n%s", details)
	}
	if !strings.Contains(details, "Original description.") {
		t.Errorf("expected the original description preserved, got:\n%s", details)
	}
}

// TestResetOrphanedInProgressTasksForAgent_SurfacesKnownInFlightPointers
// verifies the note carries whatever the daemon already knows about
// in-flight work — MR, integration branch, Jira, and the dead agent's
// own last logged activity entry — so the next picker isn't blind to
// work that might already be on a branch.
func TestResetOrphanedInProgressTasksForAgent_SurfacesKnownInFlightPointers(t *testing.T) {
	const path = "Tasks/Foo.md"
	apiSrv, calls := fakeOrphanResetAPI(t, map[string]Task{
		path: {
			Path: path, Title: "Fix the thing", Status: "in-progress",
			CustomProperties: map[string]string{
				"mr":                 "https://gitlab.example.com/myapp/merge_requests/42",
				"integration-branch": "feat/big-migration",
				"jira":               "CX-123",
			},
		},
	})
	srv, _ := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	seedOwnedTask(srv, "orchestrator-myapp-g1", path, "Fix the thing")
	srv.mu.Lock()
	srv.appendActivityLocked("orchestrator-myapp-g1", "pushed WIP commit, about to run the migration script", "")
	srv.mu.Unlock()

	reset, _ := srv.resetOrphanedInProgressTasksForAgent("orchestrator-myapp-g1")
	if len(reset) != 1 {
		t.Fatalf("expected 1 reset, got %+v", reset)
	}

	details, _ := (*calls)[0].Body["details"].(string)
	for _, want := range []string{
		"https://gitlab.example.com/myapp/merge_requests/42",
		"feat/big-migration",
		"CX-123",
		"pushed WIP commit, about to run the migration script",
		"uncommitted or unpushed work",
		"unverified",
	} {
		if !strings.Contains(details, want) {
			t.Errorf("expected the note to contain %q, got:\n%s", want, details)
		}
	}
}

// TestResetOrphanedInProgressTasksForAgent_NoKnownPointers_StillGetsCaveat
// verifies a task with nothing extra known about it still gets the plain
// reset note with the unverified-uncommitted-work caveat, and doesn't
// claim a "Known pointers:" section it has nothing to put in.
func TestResetOrphanedInProgressTasksForAgent_NoKnownPointers_StillGetsCaveat(t *testing.T) {
	const path = "Tasks/Foo.md"
	apiSrv, calls := fakeOrphanResetAPI(t, map[string]Task{
		path: {Path: path, Title: "Fix the thing", Status: "in-progress"},
	})
	srv, _ := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	seedOwnedTask(srv, "orchestrator-myapp-g1", path, "Fix the thing")

	reset, _ := srv.resetOrphanedInProgressTasksForAgent("orchestrator-myapp-g1")
	if len(reset) != 1 {
		t.Fatalf("expected 1 reset, got %+v", reset)
	}

	details, _ := (*calls)[0].Body["details"].(string)
	if strings.Contains(details, "Known pointers:") {
		t.Errorf("expected no 'Known pointers:' section when nothing is known, got:\n%s", details)
	}
	if !strings.Contains(details, "uncommitted or unpushed work") || !strings.Contains(details, "unverified") {
		t.Errorf("expected the unverified-uncommitted-work caveat regardless, got:\n%s", details)
	}
}

// TestResetOrphanedInProgressTasksForAgent_LiveAdopter_Untouched verifies
// the first guard: if a LIVE agent has since been formally assigned the
// task (a fresh "Task assigned:" message — real adoption, not just an
// FYI notice), the dead agent's original ownership no longer resolves to
// it at all, so it's never even a reset candidate.
func TestResetOrphanedInProgressTasksForAgent_LiveAdopter_Untouched(t *testing.T) {
	const path = "Tasks/Foo.md"
	apiSrv, calls := fakeOrphanResetAPI(t, map[string]Task{
		path: {Path: path, Title: "Fix the thing", Status: "in-progress"},
	})
	srv, _ := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	seedOwnedTask(srv, "orchestrator-myapp-g1", path, "Fix the thing")
	// A later, real assignment to a different (live) agent — genuine
	// adoption, taskOwnerLocked now resolves to this one instead.
	seedOwnedTask(srv, "orchestrator-myapp-g2", path, "Fix the thing")

	reset, skipped := srv.resetOrphanedInProgressTasksForAgent("orchestrator-myapp-g1")

	if len(reset) != 0 || len(skipped) != 0 {
		t.Errorf("expected no candidates at all for the original dead owner once reassigned, got reset=%+v skipped=%+v", reset, skipped)
	}
	if len(*calls) != 0 {
		t.Errorf("expected no PUT — adopted work must be left alone, got %+v", *calls)
	}
}

// TestResetOrphanedInProgressTasksForAgent_LiveDeclaredWorker_Untouched
// verifies the second guard: a live agent has DECLARED a worker
// (tn worker start) against the task, even though ownership per message
// history hasn't formally transferred — resetting it out from under an
// active worker would be worse than the bug this fixes.
func TestResetOrphanedInProgressTasksForAgent_LiveDeclaredWorker_Untouched(t *testing.T) {
	const path = "Tasks/Foo.md"
	apiSrv, calls := fakeOrphanResetAPI(t, map[string]Task{
		path: {Path: path, Title: "Fix the thing", Status: "in-progress"},
	})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	seedOwnedTask(srv, "orchestrator-myapp-g1", path, "Fix the thing")
	register(t, ts, "orchestrator-myapp-g2", "myapp") // the live agent declaring the worker
	srv.mu.Lock()
	srv.state.Workers["orchestrator-myapp-g2"] = []workerEntry{{TaskPath: path, StartedAt: time.Now()}}
	srv.mu.Unlock()

	reset, skipped := srv.resetOrphanedInProgressTasksForAgent("orchestrator-myapp-g1")

	if len(reset) != 0 {
		t.Errorf("expected nothing reset while a live worker is declared, got %+v", reset)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "Fix the thing") {
		t.Errorf("expected 1 skipped entry naming the task, got %+v", skipped)
	}
	if len(*calls) != 0 {
		t.Errorf("expected no PUT — actively-worked task must be left alone, got %+v", *calls)
	}
}

// TestCheckOrphanedTasks_RepeatPassIsNoOp verifies calling the reset
// logic twice for the same dead agent only resets once — the second pass
// finds the task already "open" (not "in-progress" anymore) and does
// nothing further, same idempotence discipline as the other one-shot
// migration/repair passes in this file.
func TestCheckOrphanedTasks_RepeatPassIsNoOp(t *testing.T) {
	const path = "Tasks/Foo.md"
	apiSrv, calls := fakeOrphanResetAPI(t, map[string]Task{
		path: {Path: path, Title: "Fix the thing", Status: "in-progress"},
	})
	srv, _ := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	seedOwnedTask(srv, "orchestrator-myapp-g1", path, "Fix the thing")

	reset1, _ := srv.resetOrphanedInProgressTasksForAgent("orchestrator-myapp-g1")
	if len(reset1) != 1 {
		t.Fatalf("pass 1: expected 1 reset, got %+v", reset1)
	}
	reset2, skipped2 := srv.resetOrphanedInProgressTasksForAgent("orchestrator-myapp-g1")
	if len(reset2) != 0 || len(skipped2) != 0 {
		t.Errorf("pass 2: expected a no-op (task already open, not in-progress), got reset=%+v skipped=%+v", reset2, skipped2)
	}
	if len(*calls) != 1 {
		t.Errorf("expected exactly 1 PUT total across both passes, got %d: %+v", len(*calls), *calls)
	}
}

// TestSweepOrphanedOwnershipDebt_ResetsBacklogAndLogsCount is the
// retroactive-sweep acceptance test: several dead generations, each
// owning in-progress tasks from before this fix existed, get swept in
// one startup pass — including one task correctly left alone because a
// live agent has since adopted it, proving the sweep uses the exact same
// guard as the forward path rather than a cruder "reset everything"
// pass.
func TestSweepOrphanedOwnershipDebt_ResetsBacklogAndLogsCount(t *testing.T) {
	t.Setenv("TN_ENABLE_ORPHAN_SWEEP", "1") // gated off by default; this test exercises the enabled path
	const pathA = "Tasks/A.md"
	const pathB = "Tasks/B.md"
	const pathC = "Tasks/C.md" // adopted by a live agent — must survive the sweep
	apiSrv, calls := fakeOrphanResetAPI(t, map[string]Task{
		pathA: {Path: pathA, Title: "Task A", Status: "in-progress"},
		pathB: {Path: pathB, Title: "Task B", Status: "in-progress"},
		pathC: {Path: pathC, Title: "Task C", Status: "in-progress"},
	})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	srv.startedAt = time.Now().Add(-2 * aliveWindow) // clear of the startup grace period (no sleep)

	// Three generations, crashed without retiring (registered, then went
	// stale — same shape as a real dead generation: Agent records are
	// never pruned, so deadTaskOwnersLocked's "registered but not alive"
	// check is what actually distinguishes dead from live, not absence
	// from the registry).
	register(t, ts, "orchestrator-myapp-g1", "myapp")
	register(t, ts, "orchestrator-myapp-g2", "myapp")
	register(t, ts, "orchestrator-myapp-g3", "myapp")
	seedOwnedTask(srv, "orchestrator-myapp-g1", pathA, "Task A")
	seedOwnedTask(srv, "orchestrator-myapp-g2", pathB, "Task B")
	seedOwnedTask(srv, "orchestrator-myapp-g3", pathC, "Task C")
	// pathC was genuinely adopted by a live agent afterward.
	register(t, ts, "orchestrator-myapp-g4", "myapp")
	seedOwnedTask(srv, "orchestrator-myapp-g4", pathC, "Task C")

	srv.mu.Lock()
	stale := time.Now().Add(-5 * time.Minute)
	srv.state.Agents["orchestrator-myapp-g1"].LastSeenAt = stale
	srv.state.Agents["orchestrator-myapp-g2"].LastSeenAt = stale
	srv.state.Agents["orchestrator-myapp-g3"].LastSeenAt = stale
	srv.mu.Unlock()

	srv.sweepOrphanedOwnershipDebt(srv.tnClient, fakeTmuxSessionExistence(nil))

	if len(*calls) != 2 {
		t.Fatalf("expected exactly 2 PUTs (A and B, not the adopted C), got %d: %+v", len(*calls), *calls)
	}
	resetPaths := map[string]bool{}
	for _, c := range *calls {
		resetPaths[c.Path] = true
		if c.Body["status"] != "open" {
			t.Errorf("expected status:open for %s, got %+v", c.Path, c.Body)
		}
	}
	if !resetPaths[pathA] || !resetPaths[pathB] {
		t.Errorf("expected both A and B reset, got %+v", *calls)
	}
	if resetPaths[pathC] {
		t.Errorf("expected C (adopted by a live agent) left alone, got %+v", *calls)
	}

	// A repeat sweep (e.g. after a restart) must be a no-op: both reset
	// tasks are no longer "in-progress", so their dead owners drop out of
	// the candidate list entirely.
	srv.sweepOrphanedOwnershipDebt(srv.tnClient, fakeTmuxSessionExistence(nil))
	if len(*calls) != 2 {
		t.Errorf("expected a repeat sweep to add no further PUTs, got %d total: %+v", len(*calls), *calls)
	}
}

// TestSweepOrphanedOwnershipDebt_DisabledByDefault_LeavesBacklogAlone is
// the startup-sweep half of the TN_ENABLE_ORPHAN_SWEEP gate: the exact
// scenario from TestSweepOrphanedOwnershipDebt_ResetsBacklogAndLogsCount
// must produce zero PUTs when the flag is unset. This is the specific
// window the incident traced to a restart: on every daemon restart every
// agent's LastSeenAt predates the restart, so this startup pass would
// otherwise see them all as dead task owners before any of them get a
// chance to poll again.
func TestSweepOrphanedOwnershipDebt_DisabledByDefault_LeavesBacklogAlone(t *testing.T) {
	const pathA = "Tasks/A.md"
	apiSrv, calls := fakeOrphanResetAPI(t, map[string]Task{
		pathA: {Path: pathA, Title: "Task A", Status: "in-progress"},
	})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})

	register(t, ts, "orchestrator-myapp-g1", "myapp")
	seedOwnedTask(srv, "orchestrator-myapp-g1", pathA, "Task A")

	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp-g1"].LastSeenAt = time.Now().Add(-5 * time.Minute)
	srv.mu.Unlock()

	srv.sweepOrphanedOwnershipDebt(srv.tnClient, fakeTmuxSessionExistence(nil))

	if len(*calls) != 0 {
		t.Errorf("expected zero PUTs while TN_ENABLE_ORPHAN_SWEEP is unset, got %d: %+v", len(*calls), *calls)
	}
}
