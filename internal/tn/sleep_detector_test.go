package tn

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestIsHostSleepGap covers the gap-threshold decision directly and
// deterministically — "no gap -> no action" and "a real gap -> action" —
// without waiting on a real ticker.
func TestIsHostSleepGap(t *testing.T) {
	cases := []struct {
		name string
		gap  time.Duration
		want bool
	}{
		{"normal tick, no gap", 30 * time.Second, false},
		{"slightly late tick, not a sleep", 35 * time.Second, false},
		{"exactly at threshold, not over it", sleepGapThreshold, false},
		{"just over threshold", sleepGapThreshold + time.Second, true},
		{"long sleep", 20 * time.Minute, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isHostSleepGap(c.gap); got != c.want {
				t.Errorf("isHostSleepGap(%v) = %v, want %v", c.gap, got, c.want)
			}
		})
	}
}

// fakeSleepRecoveryAPI serves GET/PUT /api/tasks/:id against a single
// in-memory task, recording every PUT body so tests can assert the note
// text and status change.
type fakeSleepRecoveryAPI struct {
	status  string
	details string
	title   string
	puts    []map[string]any
}

func newFakeSleepRecoveryAPI(t *testing.T, path, title, status string) (*fakeSleepRecoveryAPI, *httptest.Server) {
	t.Helper()
	f := &fakeSleepRecoveryAPI{status: status, title: title}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"path": path, "title": f.title, "status": f.status, "details": f.details,
			}})
		case http.MethodPut:
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			f.puts = append(f.puts, body)
			if d, ok := body["details"].(string); ok {
				f.details = d
			}
			if s, ok := body["status"].(string); ok {
				f.status = s
			}
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"path": path, "title": f.title, "status": f.status, "details": f.details,
			}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// TestHandleHostSleep_AgentLastSeenBeforeSleepIsTreatedAsSleepKilled
// verifies the core trigger: an agent whose LastSeenAt falls before the
// sleep window and is now beyond aliveWindow gets deregistered by the
// reconciliation pass — this is "a simulated clock gap triggers the pass."
func TestHandleHostSleep_AgentLastSeenBeforeSleepIsTreatedAsSleepKilled(t *testing.T) {
	srv, _ := newTestServer(t)
	sleepStart := time.Now().Add(-10 * time.Minute)

	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp-g1"] = &Agent{
		Name: "orchestrator-myapp-g1", Project: "myapp",
		LastSeenAt: sleepStart.Add(-time.Minute), // last polled BEFORE the sleep started
	}
	srv.mu.Unlock()

	srv.handleHostSleep(sleepStart)

	srv.mu.Lock()
	a := srv.state.Agents["orchestrator-myapp-g1"]
	srv.mu.Unlock()
	if a.DeregisteredAt == nil {
		t.Error("expected the agent deregistered as sleep-killed")
	}
}

// TestHandleHostSleep_AgentAliveAcrossGapUntouched verifies an agent that
// polled again AFTER the sleep window (LastSeenAt after sleepStart, still
// within aliveWindow) is left completely alone — it survived the sleep and
// is genuinely alive, not sleep-killed.
func TestHandleHostSleep_AgentAliveAcrossGapUntouched(t *testing.T) {
	srv, _ := newTestServer(t)
	sleepStart := time.Now().Add(-10 * time.Minute)

	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp-g1"] = &Agent{
		Name: "orchestrator-myapp-g1", Project: "myapp",
		LastSeenAt: time.Now(), // polled again just now — alive across the gap
	}
	srv.mu.Unlock()

	srv.handleHostSleep(sleepStart)

	srv.mu.Lock()
	a := srv.state.Agents["orchestrator-myapp-g1"]
	srv.mu.Unlock()
	if a.DeregisteredAt != nil {
		t.Error("expected an agent alive across the sleep gap left untouched")
	}
}

// TestReconcileSleepKilledAgent_InProgressTaskResetToOpenWithNote verifies
// the self-heal: the owned in-progress task gets a bridge-attributed note
// naming the sleep and is reset to open — the direct end-to-end effect the
// user actually sees on the board.
func TestReconcileSleepKilledAgent_InProgressTaskResetToOpenWithNote(t *testing.T) {
	const path = "Tasks/Interrupted.md"
	fake, apiSrv := newFakeSleepRecoveryAPI(t, path, "Interrupted", "in-progress")

	srv, _ := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp-g1"] = &Agent{Name: "orchestrator-myapp-g1", Project: "myapp"}
	srv.mu.Unlock()
	seedOwnedTask(srv, "orchestrator-myapp-g1", path, "Interrupted")

	sleepStart := time.Now().Add(-10 * time.Minute)
	srv.reconcileSleepKilledAgent(Agent{Name: "orchestrator-myapp-g1", Project: "myapp"}, sleepStart)

	if len(fake.puts) != 1 {
		t.Fatalf("expected exactly 1 PUT to reset the task, got %d", len(fake.puts))
	}
	if fake.puts[0]["status"] != "open" {
		t.Errorf("expected status reset to open, got %+v", fake.puts[0]["status"])
	}
	details, _ := fake.puts[0]["details"].(string)
	if !strings.Contains(details, "host sleep") {
		t.Errorf("expected the note to mention the host sleep, got %q", details)
	}
	if !strings.Contains(details, "orchestrator-myapp-g1") {
		t.Errorf("expected the note to name the lost agent, got %q", details)
	}
	if !strings.Contains(details, "not abandoned") {
		t.Errorf("expected the note to distinguish sleep-death from abandonment, got %q", details)
	}

	srv.mu.Lock()
	a := srv.state.Agents["orchestrator-myapp-g1"]
	srv.mu.Unlock()
	if a.DeregisteredAt == nil {
		t.Error("expected the agent deregistered")
	}
}

// TestReconcileSleepKilledAgent_Idempotent verifies calling it twice for
// the same agent/sleep only acts once. The task-reset PUT is naturally
// deduped by the task's own status (it's no longer "in-progress" after the
// first call), so that alone can't tell a real idempotency guard from no
// guard at all — the load-bearing check here is the bridge activity log:
// without the DeregisteredAt check-and-set, a second call would still find
// zero in-progress tasks and append a SECOND, redundant "sleep-killed (no
// in-progress tasks owned)" entry every time it re-ran.
func TestReconcileSleepKilledAgent_Idempotent(t *testing.T) {
	const path = "Tasks/Interrupted.md"
	fake, apiSrv := newFakeSleepRecoveryAPI(t, path, "Interrupted", "in-progress")

	srv, _ := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp-g1"] = &Agent{Name: "orchestrator-myapp-g1", Project: "myapp"}
	srv.mu.Unlock()
	seedOwnedTask(srv, "orchestrator-myapp-g1", path, "Interrupted")

	sleepStart := time.Now().Add(-10 * time.Minute)
	agentSnapshot := Agent{Name: "orchestrator-myapp-g1", Project: "myapp"}
	srv.reconcileSleepKilledAgent(agentSnapshot, sleepStart)
	srv.reconcileSleepKilledAgent(agentSnapshot, sleepStart)

	if len(fake.puts) != 1 {
		t.Errorf("expected exactly 1 PUT across two calls (idempotent), got %d", len(fake.puts))
	}
	srv.mu.Lock()
	activityCount := len(srv.state.Activity)
	srv.mu.Unlock()
	if activityCount != 1 {
		t.Errorf("expected exactly 1 bridge activity entry across two calls, got %d (second call should be a full no-op)", activityCount)
	}
}

// TestHandleHostSleep_NoMatchingAgentsNoAction verifies a sleep detection
// with no agent meeting the sleep-killed criteria (e.g. no agents
// registered at all) does nothing and doesn't panic.
func TestHandleHostSleep_NoMatchingAgentsNoAction(t *testing.T) {
	srv, _ := newTestServer(t)
	sleepStart := time.Now().Add(-10 * time.Minute)
	srv.handleHostSleep(sleepStart) // no agents at all — must be a clean no-op
}
