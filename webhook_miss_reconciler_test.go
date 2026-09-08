package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestBuildRecentlyModifiedQuery verifies the FilterQuery shape: only
// archived is-not-checked and dateModified is-on-or-after — deliberately no
// status/tag filter, since @claude: routing applies regardless. Also
// verifies the sort is ASCENDING (oldest-modified first) — a regression
// guard for the starvation fix: with a descending sort, a per-pass cap
// truncation always drops the OLDEST candidates, and since the query
// window only ever advances forward, a candidate dropped this way has no
// guarantee of surviving into a later pass before aging out of the window
// entirely — i.e. a genuinely webhook-missed task could go unprocessed
// forever. Ascending sort means truncation drops the NEWEST candidates
// instead, which reliably get picked up on a later pass.
func TestBuildRecentlyModifiedQuery(t *testing.T) {
	fg := buildRecentlyModifiedQuery("2026-08-17")
	if fg.Type != "group" || fg.Conjunction != "and" {
		t.Fatalf("unexpected group shape: %+v", fg)
	}
	if fg.SortKey != "dateModified" || fg.SortDirection != "asc" {
		t.Errorf("expected ascending dateModified sort (oldest-first, so cap truncation never permanently starves an old candidate), got sortKey=%q sortDirection=%q", fg.SortKey, fg.SortDirection)
	}
	want := []FilterCondition{
		{Type: "condition", ID: "archived", Property: "archived", Operator: "is-not-checked"},
		{Type: "condition", ID: "modified", Property: "dateModified", Operator: "is-on-or-after", Value: "2026-08-17"},
	}
	if len(fg.Children) != len(want) {
		t.Fatalf("expected %d conditions, got %d: %+v", len(want), len(fg.Children), fg.Children)
	}
	for i, w := range want {
		if fg.Children[i] != w {
			t.Errorf("condition %d: got %+v, want %+v", i, fg.Children[i], w)
		}
	}
}

// TestComputeReconcileSince covers the watermark model: fresh state (zero
// watermark) falls back to a 24h lookback from now; an existing watermark
// uses a 15min buffer instead.
func TestComputeReconcileSince(t *testing.T) {
	now := mustTime(t, "2026-08-17T12:00:00Z")

	t.Run("fresh state falls back to 24h before now", func(t *testing.T) {
		since := computeReconcileSince(time.Time{}, now)
		want := now.Add(-24 * time.Hour)
		if !since.Equal(want) {
			t.Errorf("got %v, want %v", since, want)
		}
	})

	t.Run("existing watermark uses a 15min buffer", func(t *testing.T) {
		watermark := now.Add(-2 * time.Hour)
		since := computeReconcileSince(watermark, now)
		want := watermark.Add(-15 * time.Minute)
		if !since.Equal(want) {
			t.Errorf("got %v, want %v", since, want)
		}
	})
}

// TestTaskToWebhookTask verifies the field mapping used to feed a fetched
// TaskNotes API Task into the shared routeTaskNotification pipeline.
func TestTaskToWebhookTask(t *testing.T) {
	task := Task{
		Path: "Tasks/X.md", Title: "X", Status: "open",
		Tags: []string{"claude"}, Projects: []string{"[[Myapp]]"},
		Details:          "@claude: hi",
		CustomProperties: map[string]string{"claude-project": "myapp"},
	}
	wt := taskToWebhookTask(task)
	if wt.Path != task.Path || wt.Title != task.Title || wt.Status != task.Status {
		t.Errorf("unexpected mapping: %+v", wt)
	}
	if len(wt.Tags) != 1 || wt.Tags[0] != "claude" {
		t.Errorf("unexpected tags: %+v", wt.Tags)
	}
	if wt.Details != "@claude: hi" {
		t.Errorf("unexpected details: %q", wt.Details)
	}
	if wt.CustomProperties["claude-project"] != "myapp" {
		t.Errorf("unexpected customProperties: %+v", wt.CustomProperties)
	}
}

// fakeTaskNotesAPI simulates the two TaskNotes endpoints the reconciler
// needs: POST /api/tasks/query (returns queryResults — realistically
// carrying no Details, matching the live API) and GET /api/tasks/:id
// (returns the full task, with Details, from detailsByPath keyed by path;
// a path missing from detailsByPath 404s, simulating a fetch failure).
// Returns the server and a slice recording every dateModified value the
// query was called with, for assertions on the watermark window used.
func fakeTaskNotesAPI(t *testing.T, queryResults []Task, detailsByPath map[string]Task) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var sinceValues []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/tasks/query":
			var fg FilterGroup
			json.NewDecoder(r.Body).Decode(&fg)
			for _, c := range fg.Children {
				if c.Property == "dateModified" {
					mu.Lock()
					sinceValues = append(sinceValues, c.Value)
					mu.Unlock()
				}
			}
			resp := map[string]any{"data": map[string]any{
				"tasks": queryResults, "total": len(queryResults), "filtered": len(queryResults),
			}}
			json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/tasks/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
			task, ok := detailsByPath[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				json.NewEncoder(w).Encode(map[string]any{"error": "not found"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"data": task})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &sinceValues
}

// TestReconcileWebhookMisses_NewClaudeLineRoutesOnceNotAgainNextPass
// verifies a task carrying an unrouted @claude: line (details only
// available via the individual GET fallback, since the query response
// omits them) routes exactly once, and a second identical pass doesn't
// re-route it.
func TestReconcileWebhookMisses_NewClaudeLineRoutesOnceNotAgainNextPass(t *testing.T) {
	const path = "Tasks/Miss.md"
	queryResults := []Task{{Path: path, Title: "Miss", Status: "open", Projects: []string{"proj-miss"}}}
	detailsByPath := map[string]Task{
		path: {Path: path, Title: "Miss", Status: "open", Projects: []string{"proj-miss"}, Details: "@claude: please help with X"},
	}
	apiSrv, _ := fakeTaskNotesAPI(t, queryResults, detailsByPath)
	client := NewClient(Config{URL: apiSrv.URL})

	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.mu.Lock()
	srv.state.Agents["agent-miss"] = &Agent{Name: "agent-miss", Project: "proj-miss", LastSeenAt: time.Now(), Accepting: true}
	srv.mu.Unlock()

	srv.reconcileWebhookMissesRemote(client)

	msgs := queuedMessagesTo(srv, "agent-miss")
	if len(msgs) != 1 || msgs[0].Text != "please help with X" {
		t.Fatalf("expected exactly 1 routed @claude line, got %+v", msgs)
	}

	// Second, identical pass: must not re-route the same line.
	srv.reconcileWebhookMissesRemote(client)
	msgs = queuedMessagesTo(srv, "agent-miss")
	if len(msgs) != 1 {
		t.Fatalf("expected still exactly 1 message after a second identical pass, got %d: %+v", len(msgs), msgs)
	}
}

// TestReconcileWebhookMisses_AssignmentRespectsTriageGuard verifies a
// claude-tagged task parked in triage does NOT get an assignment routed via
// the scanner path, while an @claude: line in the same details still
// routes (the guard is assignment-specific).
func TestReconcileWebhookMisses_AssignmentRespectsTriageGuard(t *testing.T) {
	const path = "Tasks/Parked.md"
	queryResults := []Task{{Path: path, Title: "Parked", Status: "triage", Tags: []string{"claude"}, Projects: []string{"proj-parked"}}}
	detailsByPath := map[string]Task{
		path: {Path: path, Title: "Parked", Status: "triage", Tags: []string{"claude"}, Projects: []string{"proj-parked"},
			Details: "@claude: still fine to comment here"},
	}
	apiSrv, _ := fakeTaskNotesAPI(t, queryResults, detailsByPath)
	client := NewClient(Config{URL: apiSrv.URL})

	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.mu.Lock()
	srv.state.Agents["agent-parked"] = &Agent{Name: "agent-parked", Project: "proj-parked", LastSeenAt: time.Now(), Accepting: true}
	srv.mu.Unlock()

	srv.reconcileWebhookMissesRemote(client)

	msgs := queuedMessagesTo(srv, "agent-parked")
	if len(msgs) != 1 {
		t.Fatalf("expected only the @claude line to route (no assignment), got %d: %+v", len(msgs), msgs)
	}
	if strings.HasPrefix(msgs[0].Text, "Task assigned:") {
		t.Errorf("expected the assignment to be skipped while parked in triage, got %+v", msgs)
	}
	if msgs[0].Text != "still fine to comment here" {
		t.Errorf("expected the @claude line to route despite triage, got %+v", msgs)
	}
}

// TestReconcileWebhookMisses_SkipsInProgressOrDone is the regression test
// for the live bug: hasPendingAssignmentLocked alone stops blocking the
// moment the PRIOR assignment is acked, so this reconciler — re-scanning
// recently-modified tasks every 10 minutes AND on every daemon restart —
// was re-assigning any claude-tagged task even once it was in-progress or
// already done. assignableStatuses (a positive allowlist) must block both,
// via the exact code path that produced the live redelivery cycles.
func TestReconcileWebhookMisses_SkipsInProgressOrDone(t *testing.T) {
	inProgressPath, donePath := "Tasks/InProgress.md", "Tasks/Done.md"
	queryResults := []Task{
		{Path: inProgressPath, Title: "In Progress", Status: "in-progress", Tags: []string{"claude"}, Projects: []string{"proj-ip"}},
		{Path: donePath, Title: "Done", Status: "done", Tags: []string{"claude"}, Projects: []string{"proj-ip"}},
	}
	detailsByPath := map[string]Task{
		inProgressPath: {Path: inProgressPath, Title: "In Progress", Status: "in-progress", Tags: []string{"claude"}, Projects: []string{"proj-ip"}},
		donePath:       {Path: donePath, Title: "Done", Status: "done", Tags: []string{"claude"}, Projects: []string{"proj-ip"}},
	}
	apiSrv, _ := fakeTaskNotesAPI(t, queryResults, detailsByPath)
	client := NewClient(Config{URL: apiSrv.URL})

	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.mu.Lock()
	srv.state.Agents["agent-ip"] = &Agent{Name: "agent-ip", Project: "proj-ip", LastSeenAt: time.Now(), Accepting: true}
	srv.mu.Unlock()

	srv.reconcileWebhookMissesRemote(client)

	msgs := queuedMessagesTo(srv, "agent-ip")
	if len(msgs) != 0 {
		t.Fatalf("expected NEITHER the in-progress nor the done task re-assigned, got %+v", msgs)
	}
}

// TestReconcileWebhookMisses_FetchFailureKeepsWatermarkUnchanged covers the
// details-fetch fallback failure path with a stub fetcher (the fake
// server's GET 404s for a path missing from detailsByPath): the pass must
// not advance LastReconciledAt, so the same window is retried next time.
func TestReconcileWebhookMisses_FetchFailureKeepsWatermarkUnchanged(t *testing.T) {
	const path = "Tasks/Unfetchable.md"
	queryResults := []Task{{Path: path, Title: "Unfetchable", Status: "open"}}
	detailsByPath := map[string]Task{} // GET will 404 for this path

	apiSrv, _ := fakeTaskNotesAPI(t, queryResults, detailsByPath)
	client := NewClient(Config{URL: apiSrv.URL})

	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})

	srv.reconcileWebhookMissesRemote(client)

	srv.mu.Lock()
	watermark := srv.state.LastReconciledAt
	srv.mu.Unlock()
	if !watermark.IsZero() {
		t.Errorf("expected watermark to stay unset after a fetch failure, got %v", watermark)
	}
}

// TestReconcileWebhookMisses_WatermarkAdvancesOnlyOnSuccess verifies a
// fully successful pass (query ok, every candidate fetched) advances
// LastReconciledAt to (approximately) the pass start time.
func TestReconcileWebhookMisses_WatermarkAdvancesOnlyOnSuccess(t *testing.T) {
	const path = "Tasks/Ok.md"
	queryResults := []Task{{Path: path, Title: "Ok", Status: "open"}}
	detailsByPath := map[string]Task{path: {Path: path, Title: "Ok", Status: "open", Details: "no @claude lines here"}}

	apiSrv, _ := fakeTaskNotesAPI(t, queryResults, detailsByPath)
	client := NewClient(Config{URL: apiSrv.URL})

	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})

	before := time.Now()
	srv.reconcileWebhookMissesRemote(client)
	after := time.Now()

	srv.mu.Lock()
	watermark := srv.state.LastReconciledAt
	srv.mu.Unlock()
	if watermark.Before(before) || watermark.After(after) {
		t.Errorf("expected watermark set to the pass start time (between %v and %v), got %v", before, after, watermark)
	}
}

// TestReconcileWebhookMisses_DowntimeSimulation verifies that after a long
// gap since the last successful pass (simulating the daemon having been
// down or unable to complete a pass for hours), the next pass queries a
// wide-enough window (watermark - 15min buffer, hours in the past) to catch
// an @claude: line on a task that's "old" relative to the normal 10-min
// steady-state cadence.
func TestReconcileWebhookMisses_DowntimeSimulation(t *testing.T) {
	const path = "Tasks/OldMiss.md"
	queryResults := []Task{{Path: path, Title: "OldMiss", Status: "open", Projects: []string{"proj-old"}}}
	detailsByPath := map[string]Task{
		path: {Path: path, Title: "OldMiss", Status: "open", Projects: []string{"proj-old"}, Details: "@claude: missed during downtime"},
	}
	apiSrv, sinceValues := fakeTaskNotesAPI(t, queryResults, detailsByPath)
	client := NewClient(Config{URL: apiSrv.URL})

	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.mu.Lock()
	srv.state.Agents["agent-old"] = &Agent{Name: "agent-old", Project: "proj-old", LastSeenAt: time.Now(), Accepting: true}
	fiveHoursAgo := time.Now().Add(-5 * time.Hour)
	srv.state.LastReconciledAt = fiveHoursAgo
	srv.mu.Unlock()

	srv.reconcileWebhookMissesRemote(client)

	msgs := queuedMessagesTo(srv, "agent-old")
	if len(msgs) != 1 || msgs[0].Text != "missed during downtime" {
		t.Fatalf("expected the old-modified task's @claude line to route on the startup pass, got %+v", msgs)
	}

	// The query window should reflect ~5h15m in the past (watermark -
	// 15min buffer), not just the normal ~25min steady-state window.
	wantSince := fiveHoursAgo.Add(-15 * time.Minute).Format("2006-01-02")
	if len(*sinceValues) != 1 || (*sinceValues)[0] != wantSince {
		t.Errorf("expected the query's dateModified to be %q (day-granularity), got %+v", wantSince, *sinceValues)
	}
}

// TestReconcileWebhookMisses_OpenTaskAssignedOnceAcrossTwoPasses is the
// regression test for the live re-assignment-storm bug: state.json showed
// the same taskPath's "Task assigned:" message repeating at exactly the
// 10-minute reconciler interval. hasPendingAssignmentLocked alone only
// blocks while the prior message is unacked, and orchestrators ack
// immediately per contract, so a task that legitimately just sits in
// "open" (never touched again) was re-assigned on every single pass. The
// AssignedTasks marker (see routeTaskNotification/markAssignedLocked) must
// make assignment one-shot per open episode: two reconciler passes over
// the exact same still-open, already-acked task must produce exactly ONE
// assignment total, not two.
func TestReconcileWebhookMisses_OpenTaskAssignedOnceAcrossTwoPasses(t *testing.T) {
	const path = "Tasks/Idle.md"
	queryResults := []Task{{Path: path, Title: "Idle", Status: "open", Tags: []string{"claude"}, Projects: []string{"proj-idle"}}}
	detailsByPath := map[string]Task{
		path: {Path: path, Title: "Idle", Status: "open", Tags: []string{"claude"}, Projects: []string{"proj-idle"}},
	}
	apiSrv, _ := fakeTaskNotesAPI(t, queryResults, detailsByPath)
	client := NewClient(Config{URL: apiSrv.URL})

	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.mu.Lock()
	srv.state.Agents["agent-idle"] = &Agent{Name: "agent-idle", Project: "proj-idle", LastSeenAt: time.Now(), Accepting: true}
	srv.mu.Unlock()

	srv.reconcileWebhookMissesRemote(client)

	msgs := queuedMessagesTo(srv, "agent-idle")
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 assignment after the first pass, got %d: %+v", len(msgs), msgs)
	}

	// Simulate the contract: orchestrators ack immediately.
	srv.mu.Lock()
	now := time.Now()
	msgs[0].Status = "acked"
	msgs[0].AckedAt = &now
	srv.mu.Unlock()

	// A second, identical pass over the same still-open task — this is
	// exactly what fired every 10 minutes in the live incident.
	srv.reconcileWebhookMissesRemote(client)

	msgs = queuedMessagesTo(srv, "agent-idle")
	if len(msgs) != 1 {
		t.Fatalf("expected still exactly 1 assignment after a second pass over the same open task, got %d: %+v", len(msgs), msgs)
	}
}

// TestAssignedTasks_ReopenAfterInProgressAssignsAgainOnce verifies the
// self-healing side of the fix: a task observed leaving assignableStatuses
// (e.g. in-progress, once the agent picks it up) clears its AssignedTasks
// marker, so a later genuine reopen to "open" is assigned again — exactly
// once, not on every subsequent observation.
func TestAssignedTasks_ReopenAfterInProgressAssignsAgainOnce(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.mu.Lock()
	srv.state.Agents["agent-reopen"] = &Agent{Name: "agent-reopen", Project: "proj-reopen", LastSeenAt: time.Now(), Accepting: true}
	srv.mu.Unlock()

	task := webhookTask{Path: "Tasks/Reopen.md", Title: "Reopen", Status: "open", Tags: []string{"claude"}, Projects: []string{"proj-reopen"}}

	srv.routeTaskNotification(task)
	msgs := queuedMessagesTo(srv, "agent-reopen")
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 assignment on first observation, got %d: %+v", len(msgs), msgs)
	}

	// Ack it, then re-observe the same still-open task: must NOT reassign
	// (this is the bug being fixed).
	srv.mu.Lock()
	now := time.Now()
	msgs[0].Status = "acked"
	msgs[0].AckedAt = &now
	srv.mu.Unlock()

	srv.routeTaskNotification(task)
	if got := queuedMessagesTo(srv, "agent-reopen"); len(got) != 1 {
		t.Fatalf("expected no reassignment for a same-episode re-observation, got %d: %+v", len(got), got)
	}

	// The agent picks it up: observed in-progress. Then it genuinely
	// reopens (e.g. a manual revert) — a new episode, so it's assigned
	// again exactly once.
	inProgress := task
	inProgress.Status = "in-progress"
	srv.routeTaskNotification(inProgress)

	srv.routeTaskNotification(task) // back to open

	msgs = queuedMessagesTo(srv, "agent-reopen")
	if len(msgs) != 2 {
		t.Fatalf("expected exactly 2 total assignments after a genuine reopen, got %d: %+v", len(msgs), msgs)
	}

	// And it stays one-shot for the new episode too.
	srv.routeTaskNotification(task)
	if got := queuedMessagesTo(srv, "agent-reopen"); len(got) != 2 {
		t.Fatalf("expected still exactly 2 assignments after a further same-episode observation, got %d: %+v", len(got), got)
	}
}

// TestAssignedTasks_ScannerAndMRWatcherRespectMarker verifies the
// AssignedTasks marker's set/clear paths outside routeTaskNotification: the
// due-task scanner (processDueTask) sets it alongside its own
// AssignedOccurrences dedup, and the MR watcher (checkTaskMRState's caller)
// clears it via the same observeTaskStatusLocked path used everywhere else,
// confirmed directly against state rather than through a full HTTP/query
// round trip.
func TestAssignedTasks_ScannerAndMRWatcherRespectMarker(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{"proj-scan": {AutoSpawn: true}})
	srv.mu.Lock()
	srv.state.Agents["agent-scan"] = &Agent{Name: "agent-scan", Project: "proj-scan", LastSeenAt: time.Now(), Accepting: true}
	srv.mu.Unlock()

	task := Task{Path: "Tasks/Due.md", Title: "Due", Status: "open", Tags: []string{"claude"}, Projects: []string{"proj-scan"}}
	srv.processDueTask(task, "2026-08-19")

	srv.mu.Lock()
	_, marked := srv.state.AssignedTasks[task.Path]
	occurrence, hasOccurrence := srv.state.AssignedOccurrences[task.Path]
	srv.mu.Unlock()
	if !marked {
		t.Fatal("expected processDueTask to set the AssignedTasks marker")
	}
	// BOTH ledgers must be set by any single assignment — this is the
	// actual live-bug fix: a marker in one ledger only, with no matching
	// entry in the other, is exactly what let the scanner re-assign a
	// task the webhook/reconciler path had already handled (or vice
	// versa).
	if !hasOccurrence || occurrence != "2026-08-19" {
		t.Fatalf("expected AssignedOccurrences also set to today, got %q (present=%v)", occurrence, hasOccurrence)
	}

	// The MR watcher observing this same task, now in-progress, must clear
	// BOTH ledger entries via observeTaskStatusLocked — same mechanism
	// routeTaskNotification uses.
	srv.mu.Lock()
	cleared := srv.observeTaskStatusLocked(task.Path, "in-progress")
	if cleared {
		srv.saveLocked()
	}
	_, stillMarked := srv.state.AssignedTasks[task.Path]
	_, stillHasOccurrence := srv.state.AssignedOccurrences[task.Path]
	srv.mu.Unlock()
	if !cleared || stillMarked || stillHasOccurrence {
		t.Fatalf("expected both ledger entries cleared once observed in-progress, cleared=%v stillMarked=%v stillHasOccurrence=%v",
			cleared, stillMarked, stillHasOccurrence)
	}
}

// TestAssignedTasks_WebhookThenScannerAssignsOnce is the exact live repro
// team-lead reported: three tasks each assigned twice, second assignment
// for all three at the same scanner-pass timestamp, with the tasks NOT
// modified in between (still open the whole time — no legitimate re-arm
// episode). Root cause was two dedup ledgers that didn't talk to each
// other: markAssignedLocked only wrote AssignedTasks per call site, so a
// webhook-first assignment had no AssignedOccurrences entry, and the very
// next scanner pass — checking only AssignedOccurrences — saw nothing for
// today and re-assigned it. The first message is acked (per contract)
// before the scanner pass, matching the live sequence exactly — this is
// what proves the fix is the ledger consistency, not hasPendingAssignmentLocked.
func TestAssignedTasks_WebhookThenScannerAssignsOnce(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.mu.Lock()
	srv.state.Agents["agent-cross"] = &Agent{Name: "agent-cross", Project: "proj-cross", LastSeenAt: time.Now(), Accepting: true}
	srv.mu.Unlock()

	srv.routeTaskNotification(webhookTask{
		Path: "Tasks/Cross.md", Title: "Cross", Status: "open", Tags: []string{"claude"}, Projects: []string{"proj-cross"},
	})
	msgs := queuedMessagesTo(srv, "agent-cross")
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 assignment from the webhook path, got %+v", msgs)
	}
	srv.mu.Lock()
	now := time.Now()
	msgs[0].Status, msgs[0].AckedAt = "acked", &now
	srv.mu.Unlock()

	today := time.Now().Format("2006-01-02")
	task := Task{Path: "Tasks/Cross.md", Title: "Cross", Status: "open", Tags: []string{"claude"}, Projects: []string{"proj-cross"}}
	srv.processDueTask(task, today)

	msgs = queuedMessagesTo(srv, "agent-cross")
	if len(msgs) != 1 {
		t.Fatalf("expected still exactly 1 assignment after the scanner pass, got %d: %+v", len(msgs), msgs)
	}

	srv.mu.Lock()
	_, hasAssigned := srv.state.AssignedTasks[task.Path]
	occ, hasOccurrence := srv.state.AssignedOccurrences[task.Path]
	srv.mu.Unlock()
	if !hasAssigned || !hasOccurrence || occ != today {
		t.Errorf("expected both ledgers set from the webhook assignment, assigned=%v occurrence=%q(present=%v)", hasAssigned, occ, hasOccurrence)
	}
}

// TestAssignedTasks_ScannerThenReconcilerAssignsOnce is the mirror image:
// the scanner assigns first, then a reconciler/webhook pass over the same
// still-open task must not duplicate it.
func TestAssignedTasks_ScannerThenReconcilerAssignsOnce(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.mu.Lock()
	srv.state.Agents["agent-cross2"] = &Agent{Name: "agent-cross2", Project: "proj-cross2", LastSeenAt: time.Now(), Accepting: true}
	srv.mu.Unlock()

	today := time.Now().Format("2006-01-02")
	task := Task{Path: "Tasks/Cross2.md", Title: "Cross2", Status: "open", Tags: []string{"claude"}, Projects: []string{"proj-cross2"}}
	srv.processDueTask(task, today)
	msgs := queuedMessagesTo(srv, "agent-cross2")
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 assignment from the scanner, got %+v", msgs)
	}
	srv.mu.Lock()
	now := time.Now()
	msgs[0].Status, msgs[0].AckedAt = "acked", &now
	srv.mu.Unlock()

	srv.routeTaskNotification(webhookTask{
		Path: "Tasks/Cross2.md", Title: "Cross2", Status: "open", Tags: []string{"claude"}, Projects: []string{"proj-cross2"},
	})

	if msgs := queuedMessagesTo(srv, "agent-cross2"); len(msgs) != 1 {
		t.Fatalf("expected still exactly 1 assignment after the reconciler/webhook pass, got %d: %+v", len(msgs), msgs)
	}
}

// TestAssignedTasks_RecurringTaskAssignsAgainOnNewDay verifies the
// recurring-task behavior is preserved by the fix: a task assigned
// yesterday, still sitting in "open" (never left assignableStatuses, so
// nothing else re-armed it), gets assigned again exactly once on today's
// scanner pass — processDueTask itself must clear the stale AssignedTasks
// marker on seeing a new occurrence, not rely on observeTaskStatusLocked.
func TestAssignedTasks_RecurringTaskAssignsAgainOnNewDay(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.mu.Lock()
	srv.state.Agents["agent-recur"] = &Agent{Name: "agent-recur", Project: "proj-recur", LastSeenAt: time.Now(), Accepting: true}
	srv.mu.Unlock()

	task := Task{Path: "Tasks/Recur.md", Title: "Recur", Status: "open", Tags: []string{"claude"}, Projects: []string{"proj-recur"}}
	srv.processDueTask(task, "2026-08-18")
	msgs := queuedMessagesTo(srv, "agent-recur")
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 assignment for yesterday's occurrence, got %+v", msgs)
	}
	srv.mu.Lock()
	now := time.Now()
	msgs[0].Status, msgs[0].AckedAt = "acked", &now
	srv.mu.Unlock()

	srv.processDueTask(task, "2026-08-19")
	msgs = queuedMessagesTo(srv, "agent-recur")
	if len(msgs) != 2 {
		t.Fatalf("expected a fresh assignment for the new day's occurrence (2 total), got %d: %+v", len(msgs), msgs)
	}

	// Stays one-shot for THAT occurrence too — same-day repeat is a no-op.
	srv.processDueTask(task, "2026-08-19")
	if msgs := queuedMessagesTo(srv, "agent-recur"); len(msgs) != 2 {
		t.Errorf("expected still exactly 2 total after a repeat same-day pass, got %d: %+v", len(msgs), msgs)
	}
}

// TestAssignedTasks_ScannerAssignedThenLeavesAndReturnsOpenReassignsOnce
// covers "task leaves open then returns" across BOTH ledgers and BOTH
// dispatch paths: scanner-assigned, then observed in-progress (webhook)
// clears both ledger entries, then observed open again (webhook) assigns
// exactly once for the new episode — and a same-day scanner pass
// afterward must not pile on a third.
func TestAssignedTasks_ScannerAssignedThenLeavesAndReturnsOpenReassignsOnce(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.mu.Lock()
	srv.state.Agents["agent-cycle"] = &Agent{Name: "agent-cycle", Project: "proj-cycle", LastSeenAt: time.Now(), Accepting: true}
	srv.mu.Unlock()

	today := time.Now().Format("2006-01-02")
	task := Task{Path: "Tasks/Cycle.md", Title: "Cycle", Status: "open", Tags: []string{"claude"}, Projects: []string{"proj-cycle"}}
	srv.processDueTask(task, today)
	msgs := queuedMessagesTo(srv, "agent-cycle")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 assignment from the scanner, got %+v", msgs)
	}
	srv.mu.Lock()
	now := time.Now()
	msgs[0].Status, msgs[0].AckedAt = "acked", &now
	srv.mu.Unlock()

	srv.routeTaskNotification(webhookTask{
		Path: task.Path, Title: task.Title, Status: "in-progress", Tags: task.Tags, Projects: task.Projects,
	})
	srv.routeTaskNotification(webhookTask{
		Path: task.Path, Title: task.Title, Status: "open", Tags: task.Tags, Projects: task.Projects,
	})

	msgs = queuedMessagesTo(srv, "agent-cycle")
	if len(msgs) != 2 {
		t.Fatalf("expected exactly 2 total assignments after a genuine reopen, got %d: %+v", len(msgs), msgs)
	}

	srv.processDueTask(task, today)
	if msgs := queuedMessagesTo(srv, "agent-cycle"); len(msgs) != 2 {
		t.Errorf("expected still exactly 2 after a same-day scanner pass, got %d: %+v", len(msgs), msgs)
	}
}

// TestAssignedTasks_PersistsAcrossStateReload verifies AssignedTasks
// round-trips through loadState like the codebase's other dedup maps
// (AssignedOccurrences, MRStates) — the marker must survive a daemon
// restart, or every restart would re-open the exact re-assignment-storm
// window the fix closes.
func TestAssignedTasks_PersistsAcrossStateReload(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	st := loadState(statePath)
	st.AssignedTasks["Tasks/Persisted.md"] = time.Now().Format(time.RFC3339)
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(statePath, b); err != nil {
		t.Fatal(err)
	}

	reloaded := loadState(statePath)
	if _, ok := reloaded.AssignedTasks["Tasks/Persisted.md"]; !ok {
		t.Errorf("expected AssignedTasks to survive a state reload, got %+v", reloaded.AssignedTasks)
	}
}

// TestRepairPoisonedDedupState_ReseedsWithoutDispatching is the regression
// test for the live dedup-poisoning aftermath (SPEC-serve.md): the
// structural fix (webhookTask.DetailsKnown) stops NEW corruption but
// doesn't repair what's already stored — 20 tasks had taskDetailsHash ==
// sha256("") and 76/103 routedClaudeLines entries were empty at the time
// this was diagnosed, each primed to fire one more spurious dispatch the
// next time it's genuinely observed. A poisoned entry (stored hash ==
// sha256("")) whose LIVE details actually contain an @claude line must,
// after the pass, have its hash match the live content, the line's hash
// recorded, and ZERO messages dispatched (a repair is a seed, not a diff).
// A healthy entry (hash already matches its own content) must be left
// functionally unchanged.
func TestRepairPoisonedDedupState_ReseedsWithoutDispatching(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "agent-repair", "proj-repair")

	const poisonedPath = "Tasks/Poisoned.md"
	const healthyPath = "Tasks/Healthy.md"
	const healthyDetails = "some plain notes, no claude line here"

	srv.mu.Lock()
	srv.state.TaskDetailsHash[poisonedPath] = sha256Hex("") // the poisoning signature
	srv.state.RoutedClaudeLines[poisonedPath] = []string{}
	srv.state.TaskDetailsHash[healthyPath] = sha256Hex(healthyDetails)
	srv.state.RoutedClaudeLines[healthyPath] = []string{}
	srv.mu.Unlock()

	const liveDetails = "@claude: please check the logs"
	apiSrv, _ := fakeTaskNotesAPI(t, nil, map[string]Task{
		poisonedPath: {Path: poisonedPath, Title: "Poisoned", Details: liveDetails},
		healthyPath:  {Path: healthyPath, Title: "Healthy", Details: healthyDetails},
	})
	client := NewClient(Config{URL: apiSrv.URL})

	srv.repairPoisonedDedupState(client)

	srv.mu.Lock()
	poisonedHash := srv.state.TaskDetailsHash[poisonedPath]
	poisonedLines := srv.state.RoutedClaudeLines[poisonedPath]
	healthyHash := srv.state.TaskDetailsHash[healthyPath]
	healthyLines := srv.state.RoutedClaudeLines[healthyPath]
	srv.mu.Unlock()

	if wantHash := sha256Hex(liveDetails); poisonedHash != wantHash {
		t.Errorf("expected the poisoned entry's hash reseeded to match live content, got %q want %q", poisonedHash, wantHash)
	}
	wantLineHash := hashClaudeLine("please check the logs")
	if len(poisonedLines) != 1 || poisonedLines[0] != wantLineHash {
		t.Errorf("expected the routed-line hash recorded for the poisoned entry, got %v want [%s]", poisonedLines, wantLineHash)
	}
	if msgs := queuedMessagesTo(srv, "agent-repair"); len(msgs) != 0 {
		t.Errorf("expected ZERO dispatches from a repair pass, got %+v", msgs)
	}

	if healthyHash != sha256Hex(healthyDetails) {
		t.Errorf("expected the healthy entry's hash left unchanged, got %q", healthyHash)
	}
	if len(healthyLines) != 0 {
		t.Errorf("expected the healthy entry's routed lines left unchanged, got %v", healthyLines)
	}
}

// TestRepairPoisonedDedupState_NoCandidatesIsNoOp verifies a clean state
// (nothing poisoned) produces no fetches, no state changes, and no
// dispatches.
func TestRepairPoisonedDedupState_NoCandidatesIsNoOp(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "agent-clean", "proj-clean")

	srv.mu.Lock()
	srv.state.TaskDetailsHash["Tasks/Clean.md"] = sha256Hex("some real content")
	srv.state.RoutedClaudeLines["Tasks/Clean.md"] = []string{hashClaudeLine("an already-routed line")}
	srv.mu.Unlock()

	fetchCalled := false
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchCalled = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer apiSrv.Close()
	client := NewClient(Config{URL: apiSrv.URL})

	srv.repairPoisonedDedupState(client)

	if fetchCalled {
		t.Error("expected no GetTask call when nothing is poisoned")
	}
	if msgs := queuedMessagesTo(srv, "agent-clean"); len(msgs) != 0 {
		t.Errorf("expected zero dispatches, got %+v", msgs)
	}
}

// TestReconcileWebhookMisses_TriageAndReviewNeverAssign is the marker's
// counterpart to TestReconcileWebhookMisses_SkipsInProgressOrDone: a task
// parked in triage or review must never be assigned, and — since neither
// status is in assignableStatuses — observing either one also clears any
// stale AssignedTasks marker it happened to carry.
func TestReconcileWebhookMisses_TriageAndReviewNeverAssign(t *testing.T) {
	triagePath, reviewPath := "Tasks/Triage.md", "Tasks/Review.md"
	queryResults := []Task{
		{Path: triagePath, Title: "Triage", Status: "triage", Tags: []string{"claude"}, Projects: []string{"proj-tr"}},
		{Path: reviewPath, Title: "Review", Status: "review", Tags: []string{"claude"}, Projects: []string{"proj-tr"}},
	}
	detailsByPath := map[string]Task{
		triagePath: {Path: triagePath, Title: "Triage", Status: "triage", Tags: []string{"claude"}, Projects: []string{"proj-tr"}},
		reviewPath: {Path: reviewPath, Title: "Review", Status: "review", Tags: []string{"claude"}, Projects: []string{"proj-tr"}},
	}
	apiSrv, _ := fakeTaskNotesAPI(t, queryResults, detailsByPath)
	client := NewClient(Config{URL: apiSrv.URL})

	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.mu.Lock()
	srv.state.Agents["agent-tr"] = &Agent{Name: "agent-tr", Project: "proj-tr", LastSeenAt: time.Now(), Accepting: true}
	srv.state.AssignedTasks[triagePath] = time.Now().Format(time.RFC3339)
	srv.state.AssignedTasks[reviewPath] = time.Now().Format(time.RFC3339)
	srv.mu.Unlock()

	srv.reconcileWebhookMissesRemote(client)

	if msgs := queuedMessagesTo(srv, "agent-tr"); len(msgs) != 0 {
		t.Fatalf("expected NEITHER triage nor review task assigned, got %+v", msgs)
	}

	srv.mu.Lock()
	_, triageStillMarked := srv.state.AssignedTasks[triagePath]
	_, reviewStillMarked := srv.state.AssignedTasks[reviewPath]
	srv.mu.Unlock()
	if triageStillMarked || reviewStillMarked {
		t.Errorf("expected stale markers cleared on observation, triage=%v review=%v", triageStillMarked, reviewStillMarked)
	}
}

// TestReconcileWebhookMisses_ReassignmentStormNeverDoubleAssigns is the
// acceptance test for the "13 open tasks against 2 agents" live shape —
// a starved queue going from ~0 startable to a full backlog in one step,
// exactly what the earlier re-assignment-storm bug (AssignedTasks +
// AssignedOccurrences not talking to each other — see markAssignedLocked's
// doc comment) was fixed for, but at a scale that fix was never actually
// exercised at. Runs the SAME reconciler pass twice in a row (simulating
// two ticks with no intervening status change) and asserts every one of
// the 13 tasks gets EXACTLY ONE "Task assigned:" message total — never
// zero, never two — AND that the burst actually spreads across both
// agents rather than dogpiling one (see assignmentLoadLocked's doc
// comment for the burst-blindness bug this also covers).
func TestReconcileWebhookMisses_ReassignmentStormNeverDoubleAssigns(t *testing.T) {
	const n = 13
	var queryResults []Task
	detailsByPath := map[string]Task{}
	paths := make([]string, 0, n)
	for i := 0; i < n; i++ {
		path := fmt.Sprintf("Tasks/Storm-%02d.md", i)
		paths = append(paths, path)
		task := Task{Path: path, Title: fmt.Sprintf("Storm task %d", i), Status: "open", Tags: []string{"claude"}, Projects: []string{"myapp"}}
		queryResults = append(queryResults, task)
		detailsByPath[path] = task
	}
	apiSrv, _ := fakeTaskNotesAPI(t, queryResults, detailsByPath)
	client := NewClient(Config{URL: apiSrv.URL})

	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.mu.Lock()
	srv.state.Agents["orchestrator-myapp-g1"] = &Agent{Name: "orchestrator-myapp-g1", Project: "myapp", LastSeenAt: time.Now(), Accepting: true}
	srv.state.Agents["orchestrator-myapp-g2"] = &Agent{Name: "orchestrator-myapp-g2", Project: "myapp", LastSeenAt: time.Now(), Accepting: true}
	srv.mu.Unlock()

	srv.reconcileWebhookMissesRemote(client)
	// A second pass with nothing having changed (no acks, no status
	// changes — the realistic "next tick before anyone's caught up" case)
	// must not touch anything already assigned.
	srv.reconcileWebhookMissesRemote(client)

	srv.mu.Lock()
	assignedCount := map[string]int{}
	recipientOf := map[string]string{}
	for _, m := range srv.state.Messages {
		if !strings.HasPrefix(m.Text, "Task assigned:") {
			continue
		}
		assignedCount[m.TaskPath]++
		recipientOf[m.TaskPath] = m.To
	}
	srv.mu.Unlock()

	for _, p := range paths {
		if assignedCount[p] != 1 {
			t.Errorf("expected exactly 1 'Task assigned:' message for %s across both passes, got %d", p, assignedCount[p])
		}
	}

	// Distribution must actually be balanced now — pickAcceptingAgentFor
	// AssignmentLocked compares assignmentLoadLocked (queued+delivered+
	// acked), not ownedTaskPathsLocked (acked only), so a burst with
	// nothing acked yet no longer looks like a tie on every comparison.
	// Before this fix this was 13/0 (see the function's own doc comment);
	// still no enforced per-agent CAP tied to maxWorkers anywhere in this
	// path — that's deliberate (assignment is unbounded by design,
	// concurrency is the ORCHESTRATOR.md contract's job), so this asserts
	// balance, not a ceiling.
	byAgent := map[string]int{}
	for _, to := range recipientOf {
		byAgent[to]++
	}
	t.Logf("distribution across %d agents for %d tasks (single burst, no acks in between): %+v", 2, n, byAgent)
	for name, count := range byAgent {
		if count == n {
			t.Errorf("expected the burst spread across both agents, but %s alone got all %d — load-balancing is blind to unacked dispatches again", name, n)
		}
	}
	if len(byAgent) != 2 {
		t.Errorf("expected both agents to receive at least one task, got %+v", byAgent)
	}
}
