package tn

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestParseMRURL covers glab -R project path / iid extraction, including
// nested groups (a common real-world GitLab shape) and non-parseable input.
func TestParseMRURL(t *testing.T) {
	cases := []struct {
		name        string
		url         string
		wantProject string
		wantIID     string
		wantOK      bool
	}{
		{"simple", "https://gitlab.example.com/team/project/-/merge_requests/42", "team/project", "42", true},
		{"nested groups", "https://gitlab.example.com/team/subgroup/deep/project/-/merge_requests/7", "team/subgroup/deep/project", "7", true},
		{"trailing slash", "https://gitlab.example.com/team/project/-/merge_requests/42/", "team/project", "42", true},
		{"not a merge request url", "https://gitlab.example.com/team/project/-/issues/42", "", "", false},
		{"no iid", "https://gitlab.example.com/team/project/-/merge_requests/", "", "", false},
		{"non-numeric iid", "https://gitlab.example.com/team/project/-/merge_requests/abc", "", "", false},
		{"not a url", "::not a url::", "", "", false},
		{"empty", "", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			project, iid, ok := parseMRURL(c.url)
			if project != c.wantProject || iid != c.wantIID || ok != c.wantOK {
				t.Errorf("parseMRURL(%q) = (%q, %q, %v), want (%q, %q, %v)", c.url, project, iid, ok, c.wantProject, c.wantIID, c.wantOK)
			}
		})
	}
}

// TestBuildMRWatchQuery_PollsAllNonCompletedTasks verifies the query shape
// widened away from the old review/in-progress/open status-OR group: no
// status filter at all, just archived is-not-checked plus a
// status.isCompleted is-not-checked condition — so a task parked in
// needs-input or triage keeps being polled instead of falling out of the
// watcher's view forever.
func TestBuildMRWatchQuery_PollsAllNonCompletedTasks(t *testing.T) {
	fn := buildMRWatchQuery()
	if fn.Type != "group" || fn.ID != "root" || fn.Conjunction != "and" {
		t.Fatalf("unexpected root shape: %+v", fn)
	}
	want := []filterNode{
		{Type: "condition", ID: "archived", Property: "archived", Operator: "is-not-checked"},
		{Type: "condition", ID: "mr-watch-not-completed", Property: "status.isCompleted", Operator: "is-not-checked"},
	}
	if len(fn.Children) != len(want) {
		t.Fatalf("expected %d conditions (no status-OR group), got %d: %+v", len(want), len(fn.Children), fn.Children)
	}
	for i, w := range want {
		if fn.Children[i].Type != w.Type || fn.Children[i].ID != w.ID || fn.Children[i].Property != w.Property || fn.Children[i].Operator != w.Operator {
			t.Errorf("condition %d: got %+v, want %+v", i, fn.Children[i], w)
		}
	}
	for _, c := range fn.Children {
		if c.Property == "status" {
			t.Errorf("expected no bare status condition/OR-group, found one: %+v", fn.Children)
		}
	}
	if fn.SortKey != "dateModified" || fn.SortDirection != "desc" {
		t.Errorf("expected sort unchanged (dateModified desc), got %q/%q", fn.SortKey, fn.SortDirection)
	}
}

// fakeMRWatcherAPI simulates the TaskNotes API surface the MR watcher
// touches: POST /api/tasks/query (routes to mrWatchTasks or
// unblockPassTasks depending on which query shape it recognizes — the
// MR-watch query always includes an "mr-watch-not-completed" condition ID,
// which the unblock-pass query never has), GET /api/tasks/:id, and PUT
// /api/tasks/:id (captured for assertions).
func fakeMRWatcherAPI(t *testing.T, mrWatchTasks, unblockPassTasks []Task, detailsByPath map[string]Task) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var puts []map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/tasks/query":
			body, _ := io.ReadAll(r.Body)
			tasks := unblockPassTasks
			if strings.Contains(string(body), "mr-watch-not-completed") {
				tasks = mrWatchTasks
			}
			resp := map[string]any{"data": map[string]any{"tasks": tasks, "total": len(tasks), "filtered": len(tasks)}}
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
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/tasks/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			body["_path"] = id
			mu.Lock()
			puts = append(puts, body)
			mu.Unlock()
			title := detailsByPath[id].Title
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"path": id, "title": title}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &puts
}

const reviewTaskPath = "Tasks/Review.md"

func reviewTaskFixture(mrURL string) Task {
	return Task{
		Path: reviewTaskPath, Title: "Review Me", Status: "review",
		Tags: []string{"claude"}, Projects: []string{"proj-mr"},
		CustomProperties: map[string]string{"mr": mrURL},
	}
}

// TestCheckMRStatesOnce_SeedsWithoutTransitioning verifies a task's first
// observation just records the state — no PUT, no note, no message —
// unless it's the one legitimate catch-up case (merged+review, covered
// separately).
func TestCheckMRStatesOnce_SeedsWithoutTransitioning(t *testing.T) {
	task := reviewTaskFixture("https://gitlab.example.com/team/project/-/merge_requests/1")
	apiSrv, puts := fakeMRWatcherAPI(t, []Task{task}, nil, map[string]Task{reviewTaskPath: task})
	client := NewClient(Config{URL: apiSrv.URL})
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})

	mrState := func(url string) (string, error) { return "opened", nil }
	srv.checkMRStatesOnce(client, mrState)

	srv.mu.Lock()
	got := srv.state.MRStates[reviewTaskPath]
	srv.mu.Unlock()
	if got != "opened" {
		t.Errorf("expected MRStates seeded to %q, got %q", "opened", got)
	}
	if len(*puts) != 0 {
		t.Errorf("expected no PUT on first observation of an opened MR, got %+v", *puts)
	}
}

// TestCheckMRStatesOnce_SeedMergedReviewIsLegitCatchUp verifies the ONE
// special case: a task's first-ever observation already shows "merged"
// while it's still in review — the user must have merged before the
// watcher existed, and that's a legitimate transition to catch up on, not
// just a seed.
func TestCheckMRStatesOnce_SeedMergedReviewIsLegitCatchUp(t *testing.T) {
	mrURL := "https://gitlab.example.com/team/project/-/merge_requests/1"
	task := reviewTaskFixture(mrURL)
	apiSrv, puts := fakeMRWatcherAPI(t, []Task{task}, nil, map[string]Task{reviewTaskPath: task})
	client := NewClient(Config{URL: apiSrv.URL})
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.tnClient = client

	mrState := func(url string) (string, error) { return "merged", nil }
	srv.checkMRStatesOnce(client, mrState)

	if len(*puts) != 1 {
		t.Fatalf("expected exactly 1 PUT (auto-done catch-up), got %+v", *puts)
	}
	if (*puts)[0]["status"] != "done" {
		t.Errorf("expected status done, got %+v", (*puts)[0])
	}
}

// TestCheckMRStatesOnce_OpenedToMerged_ReviewAutoDonesAndRunsUnblockPass
// verifies the core transition: a review task's MR going opened->merged
// auto-transitions it to done with a note, AND directly invokes the
// unblock pass (verified by a dependent task in the unblock-pass query
// response actually getting a "Task assigned:" message queued).
func TestCheckMRStatesOnce_OpenedToMerged_ReviewAutoDonesAndRunsUnblockPass(t *testing.T) {
	mrURL := "https://gitlab.example.com/team/project/-/merge_requests/1"
	task := reviewTaskFixture(mrURL)
	dependent := Task{
		Path: "Tasks/Dependent.md", Title: "Dependent Task", Status: "open",
		Tags: []string{"claude"}, Projects: []string{"proj-dep"},
	}
	apiSrv, puts := fakeMRWatcherAPI(t, []Task{task}, []Task{dependent}, map[string]Task{reviewTaskPath: task})
	client := NewClient(Config{URL: apiSrv.URL})
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.tnClient = client

	// Seed as "opened" first (no transition).
	srv.checkMRStatesOnce(client, func(string) (string, error) { return "opened", nil })
	if len(*puts) != 0 {
		t.Fatalf("expected no PUT after the opened seed, got %+v", *puts)
	}

	// Now it merges.
	srv.checkMRStatesOnce(client, func(string) (string, error) { return "merged", nil })

	if len(*puts) != 1 {
		t.Fatalf("expected exactly 1 PUT for the merge transition, got %+v", *puts)
	}
	put := (*puts)[0]
	if put["status"] != "done" {
		t.Errorf("expected status done, got %+v", put)
	}
	details, _ := put["details"].(string)
	if !strings.Contains(details, "MR merged") || !strings.Contains(details, mrURL) {
		t.Errorf("expected the note to mention the merge and MR URL, got %q", details)
	}

	msgs := queuedMessagesTo(srv, "orchestrator-proj-dep")
	if len(msgs) != 1 || !strings.HasPrefix(msgs[0].Text, "Task assigned:") {
		t.Errorf("expected the unblock pass to have routed an assignment for the dependent task, got %+v", msgs)
	}
}

// TestCheckMRStatesOnce_InProgressMerged_TransitionsToDone verifies the
// merge transition also applies to an in-progress task (not just review).
func TestCheckMRStatesOnce_InProgressMerged_TransitionsToDone(t *testing.T) {
	mrURL := "https://gitlab.example.com/team/project/-/merge_requests/9"
	const path = "Tasks/InProgress.md"
	task := Task{
		Path: path, Title: "In Progress Task", Status: "in-progress",
		Tags: []string{"claude"}, Projects: []string{"proj-ip"},
		CustomProperties: map[string]string{"mr": mrURL},
	}
	apiSrv, puts := fakeMRWatcherAPI(t, []Task{task}, nil, map[string]Task{path: task})
	client := NewClient(Config{URL: apiSrv.URL})
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.tnClient = client

	srv.checkMRStatesOnce(client, func(string) (string, error) { return "opened", nil })
	srv.checkMRStatesOnce(client, func(string) (string, error) { return "merged", nil })

	if len(*puts) != 1 || (*puts)[0]["status"] != "done" {
		t.Errorf("expected the in-progress task auto-done on merge, got %+v", *puts)
	}
}

// TestCheckMRStatesOnce_OpenedToClosed_ReviewReopensAndNotifiesOwner
// verifies a review task's MR closing without merge reopens it to
// in-progress with a note, and notifies the project's accepting agent
// (standing in for "the task's owner, fallback accepting agent" — no
// owner exists yet here, so it falls through to the accepting agent).
func TestCheckMRStatesOnce_OpenedToClosed_ReviewReopensAndNotifiesOwner(t *testing.T) {
	mrURL := "https://gitlab.example.com/team/project/-/merge_requests/3"
	task := reviewTaskFixture(mrURL)
	apiSrv, puts := fakeMRWatcherAPI(t, []Task{task}, nil, map[string]Task{reviewTaskPath: task})
	client := NewClient(Config{URL: apiSrv.URL})
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.tnClient = client
	srv.mu.Lock()
	srv.state.Agents["agent-mr"] = &Agent{Name: "agent-mr", Project: "proj-mr", LastSeenAt: time.Now(), Accepting: true}
	srv.mu.Unlock()

	srv.checkMRStatesOnce(client, func(string) (string, error) { return "opened", nil })
	srv.checkMRStatesOnce(client, func(string) (string, error) { return "closed", nil })

	if len(*puts) != 1 {
		t.Fatalf("expected exactly 1 PUT for the reopen, got %+v", *puts)
	}
	put := (*puts)[0]
	if put["status"] != "in-progress" {
		t.Errorf("expected status in-progress, got %+v", put)
	}
	details, _ := put["details"].(string)
	if !strings.Contains(details, "MR closed") || !strings.Contains(details, "reopened") {
		t.Errorf("expected the note to mention the close and reopening, got %q", details)
	}

	msgs := queuedMessagesTo(srv, "agent-mr")
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "investigate and reopen or supersede") {
		t.Errorf("expected an owner-notification message, got %+v", msgs)
	}
}

// TestCheckMRStatesOnce_RepeatPassSameState_NoDuplicateTransition verifies
// a repeated pass observing the SAME state never re-transitions or
// re-notes/re-messages.
func TestCheckMRStatesOnce_RepeatPassSameState_NoDuplicateTransition(t *testing.T) {
	mrURL := "https://gitlab.example.com/team/project/-/merge_requests/5"
	task := reviewTaskFixture(mrURL)
	apiSrv, puts := fakeMRWatcherAPI(t, []Task{task}, nil, map[string]Task{reviewTaskPath: task})
	client := NewClient(Config{URL: apiSrv.URL})
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.tnClient = client

	srv.checkMRStatesOnce(client, func(string) (string, error) { return "opened", nil })
	srv.checkMRStatesOnce(client, func(string) (string, error) { return "merged", nil })
	if len(*puts) != 1 {
		t.Fatalf("expected exactly 1 PUT after the merge transition, got %+v", *puts)
	}

	// Same state again — must not re-transition (the task is now "done" in
	// reality, but our fake GET still serves the stale review copy, which
	// is exactly why this must be guarded by observed-state-change, not by
	// re-fetching and re-checking the task's live status).
	srv.checkMRStatesOnce(client, func(string) (string, error) { return "merged", nil })
	if len(*puts) != 1 {
		t.Errorf("expected still exactly 1 PUT after a repeat pass with the same state, got %+v", *puts)
	}
}

// TestCheckMRStatesOnce_GlabErrorLeavesStateUnchanged verifies a failed
// lookup (glab error or unparseable URL) never mutates MRStates and never
// transitions anything — a later successful pass picks up cleanly from
// the last CONFIRMED state.
func TestCheckMRStatesOnce_GlabErrorLeavesStateUnchanged(t *testing.T) {
	mrURL := "https://gitlab.example.com/team/project/-/merge_requests/6"
	task := reviewTaskFixture(mrURL)
	apiSrv, puts := fakeMRWatcherAPI(t, []Task{task}, nil, map[string]Task{reviewTaskPath: task})
	client := NewClient(Config{URL: apiSrv.URL})
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.tnClient = client

	srv.checkMRStatesOnce(client, func(string) (string, error) { return "opened", nil })

	failing := func(string) (string, error) { return "", errFakeGlab }
	srv.checkMRStatesOnce(client, failing)
	srv.checkMRStatesOnce(client, failing) // second failure: still no dupe warning issue, but re-assert state too

	srv.mu.Lock()
	got := srv.state.MRStates[reviewTaskPath]
	srv.mu.Unlock()
	if got != "opened" {
		t.Errorf("expected MRStates to remain at the last confirmed state %q, got %q", "opened", got)
	}
	if len(*puts) != 0 {
		t.Errorf("expected no PUT while glab keeps failing, got %+v", *puts)
	}
}

var errFakeGlab = &fakeGlabError{}

type fakeGlabError struct{}

func (e *fakeGlabError) Error() string { return "glab: not authenticated" }

// TestCheckMRStatesOnce_NeedsInputMerged_NotesWithoutClosing and
// TestCheckMRStatesOnce_TriageMerged_NotesWithoutClosing verify a task
// parked in needs-input/triage whose MR merges gets a note, not an
// auto-close: exactly one PUT with details carrying the merge note AND the
// original ask block untouched, no "status" key in the PUT at all,
// MRStates records "merged", an activity entry is appended, and
// /status.needsActionTasks surfaces the task with mrState "merged".
func TestCheckMRStatesOnce_NeedsInputMerged_NotesWithoutClosing(t *testing.T) {
	testParkedStatusMergedNotesWithoutClosing(t, "needs-input")
}

func TestCheckMRStatesOnce_TriageMerged_NotesWithoutClosing(t *testing.T) {
	testParkedStatusMergedNotesWithoutClosing(t, "triage")
}

func testParkedStatusMergedNotesWithoutClosing(t *testing.T, status string) {
	t.Helper()
	mrURL := "https://gitlab.example.com/team/project/-/merge_requests/11"
	path := "Tasks/Parked-" + status + ".md"
	askText := "Which environment should this ship to first?"
	askDetails := renderNoteBody(noteBody{Ask: askText})
	task := Task{
		Path: path, Title: "Parked Task", Status: status,
		Tags: []string{"claude"}, Projects: []string{"proj-parked"},
		CustomProperties: map[string]string{"mr": mrURL},
		Details:          askDetails,
	}
	apiSrv, puts := fakeMRWatcherAPI(t, []Task{task}, nil, map[string]Task{path: task})
	client := NewClient(Config{URL: apiSrv.URL})
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.tnClient = client

	// Seed as "opened" first (no transition).
	srv.checkMRStatesOnce(client, func(string) (string, error) { return "opened", nil })
	if len(*puts) != 0 {
		t.Fatalf("expected no PUT after the opened seed, got %+v", *puts)
	}

	// Now it merges.
	srv.checkMRStatesOnce(client, func(string) (string, error) { return "merged", nil })

	if len(*puts) != 1 {
		t.Fatalf("expected exactly 1 PUT for the merge note, got %+v", *puts)
	}
	put := (*puts)[0]
	if _, hasStatus := put["status"]; hasStatus {
		t.Errorf("expected no status key in the PUT (task must stay in %s), got %+v", status, put)
	}
	details, _ := put["details"].(string)
	if !strings.Contains(details, "MR merged") || !strings.Contains(details, mrURL) {
		t.Errorf("expected the note to mention the merge and MR URL, got %q", details)
	}
	if !strings.Contains(details, status) {
		t.Errorf("expected the note to mention the task was left in %s, got %q", status, details)
	}
	if !strings.Contains(details, askText) {
		t.Errorf("expected the original ask block to survive the note untouched, got %q", details)
	}

	srv.mu.Lock()
	gotState := srv.state.MRStates[path]
	activity := append([]*ActivityEntry(nil), srv.state.Activity...)
	srv.mu.Unlock()
	if gotState != "merged" {
		t.Errorf("expected MRStates[%q] = merged, got %q", path, gotState)
	}
	foundActivity := false
	for _, a := range activity {
		if strings.Contains(a.Text, "MR merged") && strings.Contains(a.Text, path) && strings.Contains(a.Text, status) {
			foundActivity = true
		}
	}
	if !foundActivity {
		t.Errorf("expected an activity entry noting the merge while %s, got %+v", status, activity)
	}

	snap := dashboardSnapshot{MRStates: map[string]string{path: gotState}}
	resp := buildStatusResponse(snap, nil, nil, []Task{task}, statusTaskCounts{}, nil, fixedRepoSettings(defaultRepoSettings()), time.Now(), 8391, false, nil, time.Time{}, 0, "", nil, nil, nil)
	found := false
	for _, nt := range resp.NeedsActionTasks {
		if nt.Path == path {
			found = true
			if nt.MRState != "merged" {
				t.Errorf("expected needsActionTasks[%q].mrState = merged, got %q", path, nt.MRState)
			}
		}
	}
	if !found {
		t.Errorf("expected %q in needsActionTasks, got %+v", path, resp.NeedsActionTasks)
	}
}

// TestCheckMRStatesOnce_NeedsInputFirstSeenMerged_IsCatchUp verifies the
// catch-up rule extends to needs-input (and triage): a task's very first
// observation already shows "merged" while parked in needs-input — the MR
// must have merged before the watcher ever saw this task (these statuses
// were never polled before the query widened to every non-completed task,
// so this first-observation case is exactly the one that matters) — and
// that's still a legitimate note-worthy event, not just a silent seed.
func TestCheckMRStatesOnce_NeedsInputFirstSeenMerged_IsCatchUp(t *testing.T) {
	mrURL := "https://gitlab.example.com/team/project/-/merge_requests/12"
	const path = "Tasks/NeedsInputCatchUp.md"
	task := Task{
		Path: path, Title: "Needs Input Task", Status: "needs-input",
		Tags: []string{"claude"}, Projects: []string{"proj-parked"},
		CustomProperties: map[string]string{"mr": mrURL},
	}
	apiSrv, puts := fakeMRWatcherAPI(t, []Task{task}, nil, map[string]Task{path: task})
	client := NewClient(Config{URL: apiSrv.URL})
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.tnClient = client

	// First-ever observation already merged — no prior "opened" seed pass.
	srv.checkMRStatesOnce(client, func(string) (string, error) { return "merged", nil })

	if len(*puts) != 1 {
		t.Fatalf("expected exactly 1 PUT (note catch-up), got %+v", *puts)
	}
	put := (*puts)[0]
	if _, hasStatus := put["status"]; hasStatus {
		t.Errorf("expected no status key in the catch-up PUT, got %+v", put)
	}
	details, _ := put["details"].(string)
	if !strings.Contains(details, "MR merged") {
		t.Errorf("expected a merge note, got %q", details)
	}

	srv.mu.Lock()
	got := srv.state.MRStates[path]
	srv.mu.Unlock()
	if got != "merged" {
		t.Errorf("expected MRStates[%q] = merged, got %q", path, got)
	}
}
