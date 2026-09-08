package main

import (
	"net/http"
	"testing"
	"time"
)

// TestHandleWorkerStart_DeclaresANewWorker verifies POST /workers/start
// records an entry for the (name, taskPath) pair.
func TestHandleWorkerStart_DeclaresANewWorker(t *testing.T) {
	srv, ts := newTestServer(t)

	resp := mustPost(t, ts.URL+"/workers/start", map[string]any{"name": "orchestrator-myapp-g1", "taskPath": "Tasks/A.md"})
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	entries := srv.state.Workers["orchestrator-myapp-g1"]
	if len(entries) != 1 || entries[0].TaskPath != "Tasks/A.md" {
		t.Errorf("expected 1 worker entry for Tasks/A.md, got %+v", entries)
	}
}

// TestHandleWorkerStart_RepeatIsRefreshNotDuplicate verifies declaring the
// same (name, taskPath) twice refreshes StartedAt rather than appending a
// second entry.
func TestHandleWorkerStart_RepeatIsRefreshNotDuplicate(t *testing.T) {
	srv, ts := newTestServer(t)

	resp1 := mustPost(t, ts.URL+"/workers/start", map[string]any{"name": "g1", "taskPath": "Tasks/A.md"})
	resp1.Body.Close()

	srv.mu.Lock()
	firstStarted := srv.state.Workers["g1"][0].StartedAt
	srv.mu.Unlock()

	time.Sleep(5 * time.Millisecond)

	resp2 := mustPost(t, ts.URL+"/workers/start", map[string]any{"name": "g1", "taskPath": "Tasks/A.md"})
	resp2.Body.Close()

	srv.mu.Lock()
	defer srv.mu.Unlock()
	entries := srv.state.Workers["g1"]
	if len(entries) != 1 {
		t.Fatalf("expected still exactly 1 entry after a repeat start, got %+v", entries)
	}
	if !entries[0].StartedAt.After(firstStarted) {
		t.Errorf("expected StartedAt refreshed on the repeat, got %v (was %v)", entries[0].StartedAt, firstStarted)
	}
}

// TestHandleWorkerEnd_RemovesOnlyTheMatchingEntry verifies POST
// /workers/end removes exactly the (name, taskPath) entry, leaving any
// other declared workers for the same agent untouched.
func TestHandleWorkerEnd_RemovesOnlyTheMatchingEntry(t *testing.T) {
	srv, ts := newTestServer(t)

	for _, path := range []string{"Tasks/A.md", "Tasks/B.md"} {
		resp := mustPost(t, ts.URL+"/workers/start", map[string]any{"name": "g1", "taskPath": path})
		resp.Body.Close()
	}

	resp := mustPost(t, ts.URL+"/workers/end", map[string]any{"name": "g1", "taskPath": "Tasks/A.md"})
	resp.Body.Close()

	srv.mu.Lock()
	defer srv.mu.Unlock()
	entries := srv.state.Workers["g1"]
	if len(entries) != 1 || entries[0].TaskPath != "Tasks/B.md" {
		t.Errorf("expected only Tasks/B.md left, got %+v", entries)
	}
}

// TestHandleWorkerStart_MissingFieldsRejected verifies both name and
// taskPath are required.
func TestHandleWorkerStart_MissingFieldsRejected(t *testing.T) {
	_, ts := newTestServer(t)

	resp := mustPost(t, ts.URL+"/workers/start", map[string]any{"name": "g1"})
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("expected 400 with no taskPath, got %d", resp.StatusCode)
	}
}

// TestStatusAgent_OwnedTasksAndWorkersSurfaceEndToEnd is the full-stack
// version: register an agent, seed ownership via an acked "Task
// assigned:" message, declare a worker via the real HTTP endpoint, seed a
// fake TaskNotes query response so the task-summary cache has titles, and
// confirm GET /status's agent entry carries both ownedTasks (by title) and
// workers.
func TestStatusAgent_OwnedTasksAndWorkersSurfaceEndToEnd(t *testing.T) {
	tasks := []Task{
		{Path: "Tasks/A.md", Title: "Fix the thing", Status: "in-progress", Projects: []string{"myapp"}},
	}
	apiSrv := fakeStartableTasksAPI(t, tasks)

	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	register(t, ts, "orchestrator-myapp-g1", "myapp")
	seedOwnedTask(srv, "orchestrator-myapp-g1", "Tasks/A.md", "Fix the thing")

	workerResp := mustPost(t, ts.URL+"/workers/start", map[string]any{"name": "orchestrator-myapp-g1", "taskPath": "Tasks/A.md"})
	workerResp.Body.Close()

	statusResp, err := http.Get(ts.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Projects map[string]struct {
			Agents []statusAgent `json:"agents"`
		} `json:"projects"`
	}
	decodeInto(t, statusResp, &status)

	myapp, ok := status.Projects["myapp"]
	if !ok || len(myapp.Agents) != 1 {
		t.Fatalf("expected 1 agent under myapp, got %+v", status.Projects)
	}
	a := myapp.Agents[0]

	if len(a.OwnedTasks) != 1 || a.OwnedTasks[0].Title != "Fix the thing" || a.OwnedTasks[0].Status != "in-progress" {
		t.Errorf("expected ownedTasks[0] to carry the real title/status, got %+v", a.OwnedTasks)
	}
	if len(a.Workers) != 1 || a.Workers[0].Path != "Tasks/A.md" || a.Workers[0].Title != "Fix the thing" {
		t.Errorf("expected workers[0] to carry the same title, got %+v", a.Workers)
	}
}

// TestStatusAgent_WorkersHiddenOnceOwningAgentGoesStale verifies the
// worker-staleness fix: a declared worker is hidden from /status once its
// owning agent's LastSeenAt falls outside aliveWindow — a dead
// orchestrator's workers must not linger in the UI forever just because
// nothing ever called `tn worker end` for them (it can't, if it's dead).
// The underlying state.Workers entry is untouched (display-layer only,
// not deleted) — verified directly, not just inferred from the JSON
// omitting it.
func TestStatusAgent_WorkersHiddenOnceOwningAgentGoesStale(t *testing.T) {
	tasks := []Task{
		{Path: "Tasks/A.md", Title: "Fix the thing", Status: "in-progress", Projects: []string{"myapp"}},
	}
	apiSrv := fakeStartableTasksAPI(t, tasks)

	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	register(t, ts, "orchestrator-myapp-g1", "myapp")

	workerResp := mustPost(t, ts.URL+"/workers/start", map[string]any{"name": "orchestrator-myapp-g1", "taskPath": "Tasks/A.md"})
	workerResp.Body.Close()

	// Kill the orchestrator without a matching `tn worker end` — exactly
	// the scenario a dead agent can't clean up after itself for.
	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp-g1"].LastSeenAt = time.Now().Add(-2 * aliveWindow)
	stillThere := len(srv.state.Workers["orchestrator-myapp-g1"])
	srv.mu.Unlock()
	if stillThere != 1 {
		t.Fatalf("test setup broken: expected the raw worker entry still present before checking visibility, got %d", stillThere)
	}

	statusResp, err := http.Get(ts.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Projects map[string]struct {
			Agents []statusAgent `json:"agents"`
		} `json:"projects"`
	}
	decodeInto(t, statusResp, &status)

	myapp, ok := status.Projects["myapp"]
	if !ok || len(myapp.Agents) != 1 {
		t.Fatalf("expected 1 agent under myapp, got %+v", status.Projects)
	}
	a := myapp.Agents[0]
	if a.Alive {
		t.Fatalf("test setup broken: expected the agent to be reported not-alive after backdating LastSeenAt")
	}
	if len(a.Workers) != 0 {
		t.Errorf("expected workers hidden once the owning agent is stale, got %+v", a.Workers)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.state.Workers["orchestrator-myapp-g1"]) != 1 {
		t.Errorf("expected the underlying worker entry NOT deleted, only hidden from display, got %+v", srv.state.Workers)
	}
}

// TestStatusAgent_OwnedTasksHiddenOnceAgentGoesStale_HistoricalOwnedShown
// verifies the ownership-pile-up fix: ownedTaskPathsLocked is pure
// message-history with no expiry (the latest acked "Task assigned:"
// recipient owns a task forever, even if that agent died days ago), so a
// dead generation must not be shown as still carrying live work.
// OwnedInProgress/OwnedTasks must be empty for a non-alive agent, and the
// same count must reappear as HistoricalOwned instead — not silently
// dropped, and not deleted from the underlying message history.
func TestStatusAgent_OwnedTasksHiddenOnceAgentGoesStale_HistoricalOwnedShown(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp-g1", "myapp")
	seedOwnedTask(srv, "orchestrator-myapp-g1", "Tasks/A.md", "Fix the thing")
	seedOwnedTask(srv, "orchestrator-myapp-g1", "Tasks/B.md", "Fix another thing")

	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp-g1"].LastSeenAt = time.Now().Add(-2 * aliveWindow)
	srv.mu.Unlock()

	statusResp, err := http.Get(ts.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Projects map[string]struct {
			Agents []statusAgent `json:"agents"`
		} `json:"projects"`
	}
	decodeInto(t, statusResp, &status)

	myapp, ok := status.Projects["myapp"]
	if !ok || len(myapp.Agents) != 1 {
		t.Fatalf("expected 1 agent under myapp, got %+v", status.Projects)
	}
	a := myapp.Agents[0]
	if a.Alive {
		t.Fatalf("test setup broken: expected agent reported not-alive after backdating LastSeenAt")
	}
	if a.OwnedInProgress != 0 {
		t.Errorf("expected OwnedInProgress 0 for a dead agent, got %d", a.OwnedInProgress)
	}
	if len(a.OwnedTasks) != 0 {
		t.Errorf("expected OwnedTasks empty for a dead agent, got %+v", a.OwnedTasks)
	}
	if a.HistoricalOwned != 2 {
		t.Errorf("expected HistoricalOwned 2 (not dropped, just relabeled), got %d", a.HistoricalOwned)
	}
}

// TestStatusAgent_HistoricalOwnedIsZeroForALiveAgent verifies the new
// field doesn't leak a redundant/confusing value for the normal (alive)
// case — HistoricalOwned is meaningful ONLY for a dead agent.
func TestStatusAgent_HistoricalOwnedIsZeroForALiveAgent(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp-g1", "myapp")
	seedOwnedTask(srv, "orchestrator-myapp-g1", "Tasks/A.md", "Fix the thing")

	statusResp, err := http.Get(ts.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Projects map[string]struct {
			Agents []statusAgent `json:"agents"`
		} `json:"projects"`
	}
	decodeInto(t, statusResp, &status)

	a := status.Projects["myapp"].Agents[0]
	if !a.Alive {
		t.Fatalf("test setup broken: expected agent reported alive")
	}
	if a.OwnedInProgress != 1 {
		t.Errorf("expected OwnedInProgress 1 for a live agent, got %d", a.OwnedInProgress)
	}
	if a.HistoricalOwned != 0 {
		t.Errorf("expected HistoricalOwned 0 for a live agent, got %d", a.HistoricalOwned)
	}
}

// --- Test-verification-agent hygiene (zz- prefix, POST /agents/purge) ---

// TestStatusProject_ExcludesTestVerificationAgentPrefix verifies a
// zz--prefixed agent (registered only to exercise an endpoint against
// the real daemon — see testVerificationAgentPrefix) never appears in
// /status's per-project agent list, live or not, alongside a normal
// agent that DOES appear.
func TestStatusProject_ExcludesTestVerificationAgentPrefix(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp-g1", "myapp")
	register(t, ts, "zz-worker-verify", "myapp")

	resp, err := http.Get(ts.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Projects map[string]struct {
			Agents []statusAgent `json:"agents"`
		} `json:"projects"`
	}
	decodeInto(t, resp, &status)

	myapp := status.Projects["myapp"]
	if len(myapp.Agents) != 1 || myapp.Agents[0].Name != "orchestrator-myapp-g1" {
		t.Errorf("expected only the real agent in the list, got %+v", myapp.Agents)
	}
	srv.mu.Lock()
	_, stillRegistered := srv.state.Agents["zz-worker-verify"]
	srv.mu.Unlock()
	if !stillRegistered {
		t.Error("expected the zz- agent still IN the registry (filtered from display, not deleted) until explicitly purged")
	}
}

// TestHandlePurgeAgent_RemovesAgentAndItsWorkers verifies POST
// /agents/purge actually deletes the record (unlike deregister/retire,
// which only tombstone it) — for cleaning up pure registry pollution
// like a leftover verification-only registration.
func TestHandlePurgeAgent_RemovesAgentAndItsWorkers(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "zz-worker-verify", "myapp")
	srv.mu.Lock()
	srv.state.Workers["zz-worker-verify"] = []workerEntry{{TaskPath: "Tasks/A.md", StartedAt: time.Now()}}
	srv.mu.Unlock()

	resp := mustPost(t, ts.URL+"/agents/purge", map[string]any{"name": "zz-worker-verify"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	srv.mu.Lock()
	_, agentStillThere := srv.state.Agents["zz-worker-verify"]
	_, workersStillThere := srv.state.Workers["zz-worker-verify"]
	srv.mu.Unlock()
	if agentStillThere {
		t.Error("expected the agent record actually deleted, not just tombstoned")
	}
	if workersStillThere {
		t.Error("expected its declared workers deleted too")
	}
}

// TestHandlePurgeAgent_RemovesPermissionDenials verifies a purged agent's
// PermissionDenied telemetry goes away too — PermissionDenials is keyed
// by a composite "agentName|toolName|CommandSummary" string, not agent
// name directly, so this needs its own scan-and-delete rather than
// falling out of the Workers-style single map delete for free. Also
// checks an UNRELATED agent's entry survives the purge.
func TestHandlePurgeAgent_RemovesPermissionDenials(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "zz-denial-verify", "myapp")
	register(t, ts, "orchestrator-myapp", "myapp")
	srv.mu.Lock()
	srv.state.PermissionDenials["zz-denial-verify|Bash|glab mr"] = &permissionDenialRecord{AgentName: "zz-denial-verify", ToolName: "Bash", CommandSummary: "glab mr", Count: 1}
	srv.state.PermissionDenials["orchestrator-myapp|Bash|git push"] = &permissionDenialRecord{AgentName: "orchestrator-myapp", ToolName: "Bash", CommandSummary: "git push", Count: 1}
	srv.mu.Unlock()

	resp := mustPost(t, ts.URL+"/agents/purge", map[string]any{"name": "zz-denial-verify"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if _, ok := srv.state.PermissionDenials["zz-denial-verify|Bash|glab mr"]; ok {
		t.Error("expected the purged agent's PermissionDenials entry removed")
	}
	if _, ok := srv.state.PermissionDenials["orchestrator-myapp|Bash|git push"]; !ok {
		t.Error("expected an unrelated agent's PermissionDenials entry to survive the purge")
	}
}

// TestHandlePurgeAgent_UnknownName404s verifies purging a name that was
// never registered (or already purged) 404s rather than silently
// succeeding.
func TestHandlePurgeAgent_UnknownName404s(t *testing.T) {
	_, ts := newTestServer(t)
	resp := mustPost(t, ts.URL+"/agents/purge", map[string]any{"name": "never-existed"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for an unknown name, got %d", resp.StatusCode)
	}
}
