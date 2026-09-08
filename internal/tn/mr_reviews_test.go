package tn

import (
	"strings"
	"testing"
	"time"
)

// --- MR review-comment watching ---

// TestMRDiscussionsAPIPath_EscapesNestedProjectPath verifies the discussions
// glab-api path is built with the project's slashes percent-escaped (so
// nested groups don't get parsed as extra path segments by glab/GitLab) and
// per_page=100 attached, and that an unparseable URL is rejected.
func TestMRDiscussionsAPIPath_EscapesNestedProjectPath(t *testing.T) {
	path, err := mrDiscussionsAPIPath("https://gitlab.example.com/team/subgroup/project/-/merge_requests/9")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "projects/team%2Fsubgroup%2Fproject/merge_requests/9/discussions?per_page=100"
	if path != want {
		t.Errorf("got %q, want %q", path, want)
	}

	if _, err := mrDiscussionsAPIPath("::not a url::"); err == nil {
		t.Errorf("expected an error for an unparseable MR URL")
	}
}

// TestMRResourceAPIPath_EscapesNestedProjectPath is mrDiscussionsAPIPath's
// sibling test for the MR-resource path (used to fetch the MR's author).
func TestMRResourceAPIPath_EscapesNestedProjectPath(t *testing.T) {
	path, err := mrResourceAPIPath("https://gitlab.example.com/team/subgroup/project/-/merge_requests/9")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "projects/team%2Fsubgroup%2Fproject/merge_requests/9"
	if path != want {
		t.Errorf("got %q, want %q", path, want)
	}

	if _, err := mrResourceAPIPath("::not a url::"); err == nil {
		t.Errorf("expected an error for an unparseable MR URL")
	}
}

// TestCheckMRStatesOnce_MRReviews_SeedingIsSilent verifies a task's first
// review-comment observation seeds State.MRReviews (max note ID across
// every thread, and the open-thread count) without a PUT or a message —
// same "no notification storm on rollout" contract as MRStates' own
// seeding.
func TestCheckMRStatesOnce_MRReviews_SeedingIsSilent(t *testing.T) {
	mrURL := "https://gitlab.example.com/team/project/-/merge_requests/20"
	task := reviewTaskFixture(mrURL)
	apiSrv, puts := fakeMRWatcherAPI(t, []Task{task}, nil, map[string]Task{reviewTaskPath: task})
	client := NewClient(Config{URL: apiSrv.URL})
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.tnClient = client

	srv.mrDiscussionsFunc = func(string) (mrDiscussions, error) {
		return mrDiscussions{
			MRAuthor: "agent-bot",
			Threads: [][]mrNote{
				{{ID: 1, Author: "agent-bot", Body: "opening comment", Resolvable: false}},
				{{ID: 2, Author: "reviewer1", Body: "please fix X", Resolvable: true, Resolved: false}},
			},
		}, nil
	}

	srv.checkMRStatesOnce(client, func(string) (string, error) { return "opened", nil })

	if len(*puts) != 0 {
		t.Fatalf("expected no PUT on first observation, got %+v", *puts)
	}
	srv.mu.Lock()
	rev, seen := srv.state.MRReviews[reviewTaskPath]
	srv.mu.Unlock()
	if !seen {
		t.Fatalf("expected MRReviews seeded for %s", reviewTaskPath)
	}
	if rev.LastNoteID != 2 {
		t.Errorf("expected LastNoteID=2 (max across both threads), got %d", rev.LastNoteID)
	}
	if rev.OpenThreads != 1 {
		t.Errorf("expected OpenThreads=1 (only the second thread has a resolvable, unresolved first note), got %d", rev.OpenThreads)
	}
	if !rev.LastNotifiedAt.IsZero() {
		t.Errorf("expected LastNotifiedAt to stay zero on a silent seed, got %v", rev.LastNotifiedAt)
	}
	msgs := queuedMessagesTo(srv, "orchestrator-proj-mr")
	if len(msgs) != 0 {
		t.Errorf("expected no message on the silent seed pass, got %+v", msgs)
	}
}

// TestCheckTaskMRReviews_NewReviewerNote_NotesAndMessagesOwner verifies a
// new reviewer note on a later pass produces exactly one PUT (details
// mentioning the new-note count/author/excerpt, no status key, and — for a
// needs-input task — the original ask block surviving untouched), one
// bridge message to the owning/accepting agent, an activity entry, and
// LastNoteID advanced to the new high-water mark.
func TestCheckTaskMRReviews_NewReviewerNote_NotesAndMessagesOwner(t *testing.T) {
	mrURL := "https://gitlab.example.com/team/project/-/merge_requests/21"
	path := "Tasks/NeedsInputReview.md"
	askText := "Which environment should this ship to first?"
	askDetails := renderNoteBody(noteBody{Ask: askText})
	task := Task{
		Path: path, Title: "Needs Input Review", Status: "needs-input",
		Tags: []string{"claude"}, Projects: []string{"proj-review"},
		CustomProperties: map[string]string{"mr": mrURL},
		Details:          askDetails,
	}
	apiSrv, puts := fakeMRWatcherAPI(t, []Task{task}, nil, map[string]Task{path: task})
	client := NewClient(Config{URL: apiSrv.URL})
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.tnClient = client

	srv.mrDiscussionsFunc = func(string) (mrDiscussions, error) {
		return mrDiscussions{
			MRAuthor: "agent-bot",
			Threads:  [][]mrNote{{{ID: 1, Author: "agent-bot", Body: "opening", Resolvable: false}}},
		}, nil
	}
	srv.checkMRStatesOnce(client, func(string) (string, error) { return "opened", nil })
	if len(*puts) != 0 {
		t.Fatalf("expected no PUT after the seed pass, got %+v", *puts)
	}

	srv.mrDiscussionsFunc = func(string) (mrDiscussions, error) {
		return mrDiscussions{
			MRAuthor: "agent-bot",
			Threads: [][]mrNote{
				{{ID: 1, Author: "agent-bot", Body: "opening", Resolvable: false}},
				{{ID: 2, Author: "reviewer1", Body: "please rename this variable, it's confusing", Resolvable: true, Resolved: false}},
			},
		}, nil
	}
	srv.checkMRStatesOnce(client, func(string) (string, error) { return "opened", nil })

	if len(*puts) != 1 {
		t.Fatalf("expected exactly 1 PUT for the new reviewer note, got %+v", *puts)
	}
	put := (*puts)[0]
	if _, hasStatus := put["status"]; hasStatus {
		t.Errorf("expected no status key in the PUT (task must stay in needs-input), got %+v", put)
	}
	details, _ := put["details"].(string)
	if !strings.Contains(details, "1 new") || !strings.Contains(details, "reviewer1") {
		t.Errorf("expected the note to mention the new-note count and author, got %q", details)
	}
	if !strings.Contains(details, "rename this variable") {
		t.Errorf("expected the note to include the note body excerpt, got %q", details)
	}
	if !strings.Contains(details, "1 unresolved thread") {
		t.Errorf("expected the note to mention the unresolved-thread count, got %q", details)
	}
	if !strings.Contains(details, askText) {
		t.Errorf("expected the original ask block to survive the note untouched, got %q", details)
	}

	msgs := queuedMessagesTo(srv, "orchestrator-proj-review")
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 owner-notification message, got %+v", msgs)
	}
	if !strings.Contains(msgs[0].Text, mrURL) || !strings.Contains(msgs[0].Text, "1 new") ||
		!strings.Contains(msgs[0].Text, "Address them, push, then reply/resolve on GitLab") {
		t.Errorf("unexpected message text: %q", msgs[0].Text)
	}

	srv.mu.Lock()
	rev := srv.state.MRReviews[path]
	activity := append([]*ActivityEntry(nil), srv.state.Activity...)
	srv.mu.Unlock()
	if rev.LastNoteID != 2 {
		t.Errorf("expected LastNoteID advanced to 2, got %d", rev.LastNoteID)
	}
	if rev.OpenThreads != 1 {
		t.Errorf("expected OpenThreads=1, got %d", rev.OpenThreads)
	}
	if rev.LastNotifiedAt.IsZero() {
		t.Errorf("expected LastNotifiedAt set after a real notification")
	}
	foundActivity := false
	for _, a := range activity {
		if strings.Contains(a.Text, "Review comments on") && strings.Contains(a.Text, path) && a.TaskPath == path {
			foundActivity = true
		}
	}
	if !foundActivity {
		t.Errorf("expected an activity entry for the new review comments, got %+v", activity)
	}
}

// TestCheckTaskMRReviews_AuthorNoteDoesNotTrigger verifies a note authored
// by the MR's own author (the agent acting on the user's behalf) never
// counts as new reviewer activity, even though it does still advance the
// LastNoteID high-water mark.
func TestCheckTaskMRReviews_AuthorNoteDoesNotTrigger(t *testing.T) {
	mrURL := "https://gitlab.example.com/team/project/-/merge_requests/22"
	task := reviewTaskFixture(mrURL)
	apiSrv, puts := fakeMRWatcherAPI(t, []Task{task}, nil, map[string]Task{reviewTaskPath: task})
	client := NewClient(Config{URL: apiSrv.URL})
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.tnClient = client

	srv.mrDiscussionsFunc = func(string) (mrDiscussions, error) {
		return mrDiscussions{MRAuthor: "agent-bot", Threads: [][]mrNote{{{ID: 1, Author: "agent-bot", Body: "seed", Resolvable: false}}}}, nil
	}
	srv.checkMRStatesOnce(client, func(string) (string, error) { return "opened", nil })

	srv.mrDiscussionsFunc = func(string) (mrDiscussions, error) {
		return mrDiscussions{MRAuthor: "agent-bot", Threads: [][]mrNote{
			{{ID: 1, Author: "agent-bot", Body: "seed", Resolvable: false}},
			{{ID: 2, Author: "agent-bot", Body: "a follow-up by the same author", Resolvable: false}},
		}}, nil
	}
	srv.checkMRStatesOnce(client, func(string) (string, error) { return "opened", nil })

	if len(*puts) != 0 {
		t.Errorf("expected no PUT from the MR author's own note, got %+v", *puts)
	}
	msgs := queuedMessagesTo(srv, "orchestrator-proj-mr")
	if len(msgs) != 0 {
		t.Errorf("expected no message from the MR author's own note, got %+v", msgs)
	}
	srv.mu.Lock()
	rev := srv.state.MRReviews[reviewTaskPath]
	srv.mu.Unlock()
	if rev.LastNoteID != 2 {
		t.Errorf("expected the high-water mark to still advance to 2 even though nothing counted as new, got %d", rev.LastNoteID)
	}
}

// TestCheckTaskMRReviews_ThreadResolved_ReducesCountSilently verifies a
// thread going from unresolved to resolved (with no accompanying new note —
// GitLab's "Resolve thread" button posts none) updates OpenThreads without
// a PUT/note or a message.
func TestCheckTaskMRReviews_ThreadResolved_ReducesCountSilently(t *testing.T) {
	mrURL := "https://gitlab.example.com/team/project/-/merge_requests/23"
	task := reviewTaskFixture(mrURL)
	apiSrv, puts := fakeMRWatcherAPI(t, []Task{task}, nil, map[string]Task{reviewTaskPath: task})
	client := NewClient(Config{URL: apiSrv.URL})
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.tnClient = client

	srv.mrDiscussionsFunc = func(string) (mrDiscussions, error) {
		return mrDiscussions{MRAuthor: "agent-bot", Threads: [][]mrNote{
			{{ID: 1, Author: "reviewer1", Body: "fix this", Resolvable: true, Resolved: false}},
		}}, nil
	}
	srv.checkMRStatesOnce(client, func(string) (string, error) { return "opened", nil })
	srv.mu.Lock()
	seeded := srv.state.MRReviews[reviewTaskPath]
	srv.mu.Unlock()
	if seeded.OpenThreads != 1 {
		t.Fatalf("expected seed OpenThreads=1, got %d", seeded.OpenThreads)
	}

	srv.mrDiscussionsFunc = func(string) (mrDiscussions, error) {
		return mrDiscussions{MRAuthor: "agent-bot", Threads: [][]mrNote{
			{{ID: 1, Author: "reviewer1", Body: "fix this", Resolvable: true, Resolved: true}},
		}}, nil
	}
	srv.checkMRStatesOnce(client, func(string) (string, error) { return "opened", nil })

	if len(*puts) != 0 {
		t.Errorf("expected no PUT/note from a resolved thread with no new note, got %+v", *puts)
	}
	msgs := queuedMessagesTo(srv, "orchestrator-proj-mr")
	if len(msgs) != 0 {
		t.Errorf("expected no message from a resolved thread alone, got %+v", msgs)
	}
	srv.mu.Lock()
	rev := srv.state.MRReviews[reviewTaskPath]
	srv.mu.Unlock()
	if rev.OpenThreads != 0 {
		t.Errorf("expected OpenThreads reduced to 0, got %d", rev.OpenThreads)
	}
}

// TestCheckTaskMRReviews_RepeatPassNoNewNotes_NoOp verifies a repeated pass
// observing the SAME discussions after a real notification never
// re-notes/re-messages.
func TestCheckTaskMRReviews_RepeatPassNoNewNotes_NoOp(t *testing.T) {
	mrURL := "https://gitlab.example.com/team/project/-/merge_requests/24"
	task := reviewTaskFixture(mrURL)
	apiSrv, puts := fakeMRWatcherAPI(t, []Task{task}, nil, map[string]Task{reviewTaskPath: task})
	client := NewClient(Config{URL: apiSrv.URL})
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.tnClient = client

	srv.mrDiscussionsFunc = func(string) (mrDiscussions, error) {
		return mrDiscussions{MRAuthor: "agent-bot", Threads: [][]mrNote{{{ID: 1, Author: "agent-bot", Body: "opening", Resolvable: false}}}}, nil
	}
	srv.checkMRStatesOnce(client, func(string) (string, error) { return "opened", nil })

	notifyDisc := mrDiscussions{MRAuthor: "agent-bot", Threads: [][]mrNote{
		{{ID: 1, Author: "agent-bot", Body: "opening", Resolvable: false}},
		{{ID: 2, Author: "reviewer1", Body: "please address this", Resolvable: true, Resolved: false}},
	}}
	srv.mrDiscussionsFunc = func(string) (mrDiscussions, error) { return notifyDisc, nil }
	srv.checkMRStatesOnce(client, func(string) (string, error) { return "opened", nil })
	if len(*puts) != 1 {
		t.Fatalf("expected exactly 1 PUT after the new reviewer note, got %+v", *puts)
	}

	// Repeat pass, same discussions — no new note this time.
	srv.checkMRStatesOnce(client, func(string) (string, error) { return "opened", nil })
	if len(*puts) != 1 {
		t.Errorf("expected still exactly 1 PUT after a repeat pass with no new notes, got %+v", *puts)
	}
	msgs := queuedMessagesTo(srv, "orchestrator-proj-mr")
	if len(msgs) != 1 {
		t.Errorf("expected still exactly 1 message after the repeat pass, got %+v", msgs)
	}
}

// TestCheckTaskMRReviews_NoMRComments_DiscussionsFuncNeverCalled verifies
// TN_NO_MRCOMMENTS=1 disables the discussions fetch entirely — the
// injected func is never invoked, and MRReviews stays untouched.
func TestCheckTaskMRReviews_NoMRComments_DiscussionsFuncNeverCalled(t *testing.T) {
	t.Setenv("TN_NO_MRCOMMENTS", "1")
	mrURL := "https://gitlab.example.com/team/project/-/merge_requests/25"
	task := reviewTaskFixture(mrURL)
	apiSrv, puts := fakeMRWatcherAPI(t, []Task{task}, nil, map[string]Task{reviewTaskPath: task})
	client := NewClient(Config{URL: apiSrv.URL})
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.tnClient = client

	called := false
	srv.mrDiscussionsFunc = func(string) (mrDiscussions, error) {
		called = true
		return mrDiscussions{}, nil
	}
	srv.checkMRStatesOnce(client, func(string) (string, error) { return "opened", nil })

	if called {
		t.Errorf("expected the discussions func never called while TN_NO_MRCOMMENTS=1")
	}
	if len(*puts) != 0 {
		t.Errorf("expected no PUT, got %+v", *puts)
	}
	srv.mu.Lock()
	_, seen := srv.state.MRReviews[reviewTaskPath]
	srv.mu.Unlock()
	if seen {
		t.Errorf("expected MRReviews untouched while TN_NO_MRCOMMENTS=1")
	}
}

// TestCheckTaskMRReviews_DiscussionsError_StateUnchanged verifies a failed
// discussions fetch never mutates MRReviews — a later successful pass
// picks up cleanly from the last confirmed state, same contract as
// checkTaskMRState's glab-error handling.
func TestCheckTaskMRReviews_DiscussionsError_StateUnchanged(t *testing.T) {
	mrURL := "https://gitlab.example.com/team/project/-/merge_requests/26"
	task := reviewTaskFixture(mrURL)
	apiSrv, puts := fakeMRWatcherAPI(t, []Task{task}, nil, map[string]Task{reviewTaskPath: task})
	client := NewClient(Config{URL: apiSrv.URL})
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.tnClient = client

	srv.mrDiscussionsFunc = func(string) (mrDiscussions, error) {
		return mrDiscussions{MRAuthor: "agent-bot", Threads: [][]mrNote{{{ID: 1, Author: "agent-bot", Body: "seed", Resolvable: false}}}}, nil
	}
	srv.checkMRStatesOnce(client, func(string) (string, error) { return "opened", nil })

	srv.mrDiscussionsFunc = func(string) (mrDiscussions, error) { return mrDiscussions{}, errFakeGlab }
	srv.checkMRStatesOnce(client, func(string) (string, error) { return "opened", nil })
	srv.checkMRStatesOnce(client, func(string) (string, error) { return "opened", nil }) // second failure: still no dupe warning issue, but re-assert state too

	if len(*puts) != 0 {
		t.Errorf("expected no PUT while the discussions fetch keeps failing, got %+v", *puts)
	}
	srv.mu.Lock()
	rev := srv.state.MRReviews[reviewTaskPath]
	srv.mu.Unlock()
	if rev.LastNoteID != 1 {
		t.Errorf("expected MRReviews to remain at the last confirmed state (LastNoteID=1), got %+v", rev)
	}
}

// TestStatus_MROpenThreads_OnNeedsActionAndOwnedTasks verifies
// /status.needsActionTasks[].mrOpenThreads and
// /status.projects.<slug>.agents[].ownedTasks[].mrOpenThreads both surface
// the MR watcher's last-observed open-thread count.
func TestStatus_MROpenThreads_OnNeedsActionAndOwnedTasks(t *testing.T) {
	path := "Tasks/ReviewWithThreads.md"
	task := Task{Path: path, Title: "Review With Threads", Status: "review", Projects: []string{"proj-mr"}}
	agent := Agent{Name: "orchestrator-proj-mr-g1", Project: "proj-mr", Accepting: true, LastSeenAt: time.Now()}

	snap := dashboardSnapshot{
		Agents:    []Agent{agent},
		MRReviews: map[string]mrReviewState{path: {LastNoteID: 5, OpenThreads: 3}},
	}
	ownedTaskPaths := map[string][]string{agent.Name: {path}}
	taskSummaryByPath := map[string]taskSummary{path: {Title: task.Title, Status: task.Status}}

	resp := buildStatusResponse(snap, nil, ownedTaskPaths, []Task{task}, statusTaskCounts{}, nil,
		fixedRepoSettings(defaultRepoSettings()), time.Now(), 8391, false, nil, time.Time{}, 0, "", taskSummaryByPath, nil, nil)

	found := false
	for _, nt := range resp.NeedsActionTasks {
		if nt.Path == path {
			found = true
			if nt.MROpenThreads != 3 {
				t.Errorf("expected needsActionTasks[%q].mrOpenThreads = 3, got %d", path, nt.MROpenThreads)
			}
		}
	}
	if !found {
		t.Errorf("expected %q in needsActionTasks, got %+v", path, resp.NeedsActionTasks)
	}

	proj, ok := resp.Projects["proj-mr"]
	if !ok || len(proj.Agents) != 1 {
		t.Fatalf("expected one agent for proj-mr, got %+v", resp.Projects)
	}
	ownedFound := false
	for _, ot := range proj.Agents[0].OwnedTasks {
		if ot.Path == path {
			ownedFound = true
			if ot.MROpenThreads != 3 {
				t.Errorf("expected ownedTasks[%q].mrOpenThreads = 3, got %d", path, ot.MROpenThreads)
			}
		}
	}
	if !ownedFound {
		t.Errorf("expected %q in ownedTasks, got %+v", path, proj.Agents[0].OwnedTasks)
	}
}
