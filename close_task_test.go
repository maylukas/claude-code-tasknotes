package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeCloseTaskAPI serves GET/PUT /api/tasks/:id against an in-memory
// single task, tracking call counts so tests can assert whether a PUT
// (the status write) happened at all — the whole point of the synchronous
// check is that a rejection means TaskNotes was never touched.
type fakeCloseTaskAPI struct {
	task     Task
	getCalls int
	putCalls int
	lastPUT  map[string]any
}

func newFakeCloseTaskAPI(t *testing.T, task Task) (*fakeCloseTaskAPI, *httptest.Server) {
	t.Helper()
	f := &fakeCloseTaskAPI{task: task}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/api/tasks/"))
		if !strings.HasPrefix(r.URL.Path, "/api/tasks/") || id != f.task.Path {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
			f.getCalls++
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"data": f.task})
		case http.MethodPut:
			f.putCalls++
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			f.lastPUT = body
			if details, ok := body["details"].(string); ok {
				f.task.Details = details
			}
			if status, ok := body["status"].(string); ok {
				f.task.Status = status
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"data": f.task})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// --- handleCloseTask (the synchronous check) ---

func TestHandleCloseTask_NoDoneMeansBlock_ClosesImmediately(t *testing.T) {
	fake, apiSrv := newFakeCloseTaskAPI(t, Task{Path: "Tasks/Plain.md", Title: "Plain", Status: "in-progress", Details: "Just a plain task."})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})

	resp := mustPost(t, ts.URL+"/tasks/close", map[string]any{"path": "Tasks/Plain.md"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for a task with no DONE MEANS block, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if fake.putCalls != 1 {
		t.Errorf("expected exactly 1 status write, got %d", fake.putCalls)
	}
	if fake.task.Status != "done" {
		t.Errorf("expected status done, got %q", fake.task.Status)
	}
}

func TestHandleCloseTask_CriteriaMet_Closes(t *testing.T) {
	details := "DONE MEANS:\n1. MR merged\n\nEvidence 1: MR !1154 merged"
	fake, apiSrv := newFakeCloseTaskAPI(t, Task{Path: "Tasks/Met.md", Title: "Met", Status: "review", Details: details})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})

	resp := mustPost(t, ts.URL+"/tasks/close", map[string]any{"path": "Tasks/Met.md"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 when criteria are met, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if fake.putCalls != 1 || fake.task.Status != "done" {
		t.Errorf("expected the status write to go through, got putCalls=%d status=%q", fake.putCalls, fake.task.Status)
	}
}

// TestHandleCloseTask_CriteriaUnmet_RejectsWithoutTouchingTaskNotes is the
// core of the synchronous half: a rejection must happen BEFORE any write,
// so there is nothing to revert and no flicker.
func TestHandleCloseTask_CriteriaUnmet_RejectsWithoutTouchingTaskNotes(t *testing.T) {
	details := "DONE MEANS:\n1. MR merged\n2. Tests pass\n\nEvidence 1: MR !1154 merged"
	fake, apiSrv := newFakeCloseTaskAPI(t, Task{Path: "Tasks/Unmet.md", Title: "Unmet", Status: "review", Details: details})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})

	resp := mustPost(t, ts.URL+"/tasks/close", map[string]any{"path": "Tasks/Unmet.md"})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 when criteria are unmet, got %d", resp.StatusCode)
	}
	var out struct {
		MissingEvidence []int `json:"missingEvidence"`
		RequiredCount   int   `json:"requiredCount"`
	}
	decodeInto(t, resp, &out)
	if out.RequiredCount != 2 || len(out.MissingEvidence) != 1 || out.MissingEvidence[0] != 2 {
		t.Errorf("expected requiredCount=2, missing=[2], got %+v", out)
	}
	if fake.putCalls != 0 {
		t.Errorf("expected ZERO status writes on rejection, got %d", fake.putCalls)
	}
	if fake.task.Status != "review" {
		t.Errorf("expected status unchanged at 'review', got %q", fake.task.Status)
	}
}

// --- closeTaskChecked (main.go's CLI-side routing + fallback) ---

func TestCloseTaskChecked_RoutesThroughBridgeWhenReachable(t *testing.T) {
	details := "DONE MEANS:\n1. MR merged\n\nEvidence 1: done"
	fake, apiSrv := newFakeCloseTaskAPI(t, Task{Path: "Tasks/Bridge.md", Title: "Bridge", Status: "review", Details: details})
	srv, bridgeTs := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	t.Setenv("TN_BRIDGE_URL", bridgeTs.URL)

	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("fallback direct-to-TaskNotes client must not be used when the bridge is reachable")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failSrv.Close()
	directClient := newTestClient(failSrv, "")

	if err := closeTaskChecked(directClient, "Tasks/Bridge.md"); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if fake.putCalls != 1 || fake.task.Status != "done" {
		t.Errorf("expected the bridge-mediated write to land, got putCalls=%d status=%q", fake.putCalls, fake.task.Status)
	}
}

// TestCloseTaskChecked_RejectionSurfacedNotFallback verifies the fallback-
// safety rule mirrored from createTaskIdempotent: a 422 (criteria checked
// and genuinely unmet) must be surfaced as an error, NEVER retried via the
// direct-to-TaskNotes fallback — falling back would defeat the entire
// synchronous check.
func TestCloseTaskChecked_RejectionSurfacedNotFallback(t *testing.T) {
	details := "DONE MEANS:\n1. MR merged\n\n(no evidence yet)"
	fake, apiSrv := newFakeCloseTaskAPI(t, Task{Path: "Tasks/Rejected.md", Title: "Rejected", Status: "review", Details: details})
	srv, bridgeTs := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	t.Setenv("TN_BRIDGE_URL", bridgeTs.URL)

	var fallbackCalled bool
	directSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer directSrv.Close()
	directClient := newTestClient(directSrv, "")

	err := closeTaskChecked(directClient, "Tasks/Rejected.md")
	if err == nil {
		t.Fatal("expected an error surfacing the rejection")
	}
	if fallbackCalled {
		t.Error("expected NO fallback to the direct client on a 422 rejection")
	}
	if fake.putCalls != 0 {
		t.Errorf("expected zero status writes, got %d", fake.putCalls)
	}
}

// TestCloseTaskChecked_BridgeUnreachable_FallsBackWithVisibleMarker is
// team-lead's 2026-08-25 refinement: when the bridge is down (connection
// refused), the close is ALLOWED (never blocked just because the daemon is
// restarting) but must leave a visible marker saying the check did not
// run — an unchecked close must never look identical to a checked one.
func TestCloseTaskChecked_BridgeUnreachable_FallsBackWithVisibleMarker(t *testing.T) {
	details := "DONE MEANS:\n1. MR merged\n\n(no evidence — bridge will be down for this test)"
	fake, apiSrv := newFakeCloseTaskAPI(t, Task{Path: "Tasks/Fallback.md", Title: "Fallback", Status: "review", Details: details})
	t.Setenv("TN_BRIDGE_URL", "http://127.0.0.1:1") // nothing listens here — connection refused
	directClient := NewClient(Config{URL: apiSrv.URL})

	if err := closeTaskChecked(directClient, "Tasks/Fallback.md"); err != nil {
		t.Fatalf("expected the fallback close to succeed (bridge down must never BLOCK a close), got %v", err)
	}
	if fake.task.Status != "done" {
		t.Errorf("expected status done via the fallback path, got %q", fake.task.Status)
	}
	if !strings.Contains(fake.task.Details, "closed without a DONE MEANS criteria check: bridge unreachable") {
		t.Errorf("expected a visible bypass marker in the task's details, got:\n%s", fake.task.Details)
	}
}

// --- noteDoneMeansGapIfUnmet (the webhook/reconciler inform-never-revert half) ---

func TestNoteDoneMeansGapIfUnmet_UnmetCriteria_AddsNoteOnce(t *testing.T) {
	details := "DONE MEANS:\n1. MR merged\n\n(no evidence — this task was closed some other way)"
	fake, apiSrv := newFakeCloseTaskAPI(t, Task{Path: "Tasks/Human.md", Title: "Human", Status: "done", Details: details})
	srv, _ := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})

	task := webhookTask{Path: "Tasks/Human.md", Title: "Human", Status: "done", Details: details, DetailsKnown: true}
	// Two observations of the same still-done task, simulating a webhook
	// event followed by a reconciler pass — the note must fire only once.
	srv.routeTaskNotification(task)
	srv.routeTaskNotification(task)

	if fake.putCalls != 1 {
		t.Fatalf("expected exactly 1 note write across 2 observations, got %d", fake.putCalls)
	}
	if !strings.Contains(fake.task.Details, "DONE MEANS check") || !strings.Contains(fake.task.Details, "NOT reverted") {
		t.Errorf("expected an informational note mentioning the check and 'not reverted', got:\n%s", fake.task.Details)
	}
	if fake.task.Status != "done" {
		t.Errorf("expected status to remain done — this path must NEVER revert, got %q", fake.task.Status)
	}
}

func TestNoteDoneMeansGapIfUnmet_CriteriaMet_NoNoteAdded(t *testing.T) {
	details := "DONE MEANS:\n1. MR merged\n\nEvidence 1: MR !1154 merged"
	fake, apiSrv := newFakeCloseTaskAPI(t, Task{Path: "Tasks/Satisfied.md", Title: "Satisfied", Status: "done", Details: details})
	srv, _ := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})

	task := webhookTask{Path: "Tasks/Satisfied.md", Title: "Satisfied", Status: "done", Details: details, DetailsKnown: true}
	srv.routeTaskNotification(task)

	if fake.putCalls != 0 {
		t.Errorf("expected no note when criteria are already satisfied (e.g. a checked close), got %d writes", fake.putCalls)
	}
}

// TestNoteDoneMeansGapIfUnmet_DetailsUnknown_SkipsCheck verifies the
// absence-of-signal guard: a partial webhook payload (DetailsKnown=false)
// must never be read as "no DONE MEANS block, nothing to check" — the
// real task has unmet criteria, but this observation can't see them, so
// it must do nothing rather than wrongly conclude there's no gap.
func TestNoteDoneMeansGapIfUnmet_DetailsUnknown_SkipsCheck(t *testing.T) {
	realDetails := "DONE MEANS:\n1. MR merged\n\n(unmet)"
	fake, apiSrv := newFakeCloseTaskAPI(t, Task{Path: "Tasks/Partial.md", Title: "Partial", Status: "done", Details: realDetails})
	srv, _ := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})

	task := webhookTask{Path: "Tasks/Partial.md", Title: "Partial", Status: "done", Details: "", DetailsKnown: false}
	srv.routeTaskNotification(task)

	if fake.putCalls != 0 {
		t.Errorf("expected no write from a DetailsKnown=false observation, got %d", fake.putCalls)
	}
}

// TestNoteDoneMeansGapIfUnmet_ReopenThenReclose_NotesAgain verifies the
// self-healing dedup marker: leaving "done" clears DoneMeansNoted, so a
// genuine reopen-then-reclose is checked fresh rather than permanently
// silenced by the first note.
func TestNoteDoneMeansGapIfUnmet_ReopenThenReclose_NotesAgain(t *testing.T) {
	details := "DONE MEANS:\n1. MR merged\n\n(still unmet)"
	fake, apiSrv := newFakeCloseTaskAPI(t, Task{Path: "Tasks/Cycle.md", Title: "Cycle", Status: "done", Details: details})
	srv, _ := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})

	doneTask := webhookTask{Path: "Tasks/Cycle.md", Title: "Cycle", Status: "done", Details: details, DetailsKnown: true}
	srv.routeTaskNotification(doneTask)
	if fake.putCalls != 1 {
		t.Fatalf("expected 1 note after the first done observation, got %d", fake.putCalls)
	}

	srv.mu.Lock()
	_, stillMarked := srv.state.DoneMeansNoted["Tasks/Cycle.md"]
	srv.mu.Unlock()
	if !stillMarked {
		t.Fatal("expected the dedup marker set after the first note")
	}

	// Reopen: status leaves "done" — marker must clear.
	reopened := webhookTask{Path: "Tasks/Cycle.md", Title: "Cycle", Status: "in-progress", Details: details, DetailsKnown: true}
	srv.routeTaskNotification(reopened)

	srv.mu.Lock()
	_, stillMarkedAfterReopen := srv.state.DoneMeansNoted["Tasks/Cycle.md"]
	srv.mu.Unlock()
	if stillMarkedAfterReopen {
		t.Error("expected the dedup marker cleared after the task left done")
	}

	// The underlying details still show unmet criteria (fake.task.Details
	// was overwritten by the note write above) — reset it to simulate a
	// fresh close attempt with criteria still unmet, then re-close.
	fake.task.Details = details
	srv.routeTaskNotification(doneTask)

	if fake.putCalls != 2 {
		t.Errorf("expected a second note after the reopen-then-reclose cycle, got %d total writes", fake.putCalls)
	}
}
