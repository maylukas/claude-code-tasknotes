package tn

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- incremental MR-watcher polling (ChangedSince/MRCursors) ---

// fakeIncrementalHost is a codeHost test double that also implements
// incrementalCodeHost and projectPathHost, so this suite can drive
// checkMRStatesOnceCore's grouping/fallback logic directly (via
// srv.codeHostFunc, the same injection point checkMRStatesOnceReal itself
// uses) without execing glab — the ~30 legacy MR-watcher tests inject at
// the mrStateFunc level via checkMRStatesOnce instead, which is exactly the
// non-incremental path this suite is NOT exercising.
type fakeIncrementalHost struct {
	name    string
	project string

	changeState  func(mrURL string) (string, error)
	changedSince func(projectPath string, since time.Time) (map[string]string, time.Time, error)
	discussions  func(mrURL string) (mrDiscussions, error)

	mu                sync.Mutex
	changeStateCalls  []string
	changedSinceCalls []time.Time
	discussionsCalls  []string
}

func (h *fakeIncrementalHost) Name() string        { return h.name }
func (h *fakeIncrementalHost) Matches(string) bool { return true }

func (h *fakeIncrementalHost) ChangeState(mrURL string) (string, error) {
	h.mu.Lock()
	h.changeStateCalls = append(h.changeStateCalls, mrURL)
	h.mu.Unlock()
	if h.changeState == nil {
		return "", fmt.Errorf("fakeIncrementalHost: ChangeState not configured")
	}
	return h.changeState(mrURL)
}

func (h *fakeIncrementalHost) Discussions(mrURL string) (mrDiscussions, error) {
	h.mu.Lock()
	h.discussionsCalls = append(h.discussionsCalls, mrURL)
	h.mu.Unlock()
	if h.discussions == nil {
		return mrDiscussions{}, nil
	}
	return h.discussions(mrURL)
}

func (h *fakeIncrementalHost) ChangedSince(projectPath string, since time.Time) (map[string]string, time.Time, error) {
	h.mu.Lock()
	h.changedSinceCalls = append(h.changedSinceCalls, since)
	h.mu.Unlock()
	if h.changedSince == nil {
		return map[string]string{}, time.Time{}, nil
	}
	return h.changedSince(projectPath, since)
}

// projectPathOf satisfies projectPathHost — always returns the fixed
// project this fake was constructed with, regardless of changeURL, since
// these tests only ever exercise a single project per fake instance.
func (h *fakeIncrementalHost) projectPathOf(string) (string, bool) {
	return h.project, true
}

func incrementalTaskFixture(path, title, status, projectSlug, mrURL string) Task {
	return Task{
		Path: path, Title: title, Status: status,
		Tags: []string{"claude"}, Projects: []string{projectSlug},
		CustomProperties: map[string]string{"mr": mrURL},
	}
}

// TestIncremental_OneChangedTwoUnchanged_NoChangeStateForUnchanged covers
// scenario (a): three tasks in one project, one changed — exactly one
// ChangedSince call, zero ChangeState calls for the two already-seen
// unchanged tasks, and the one transition from the listed state applied.
func TestIncremental_OneChangedTwoUnchanged_NoChangeStateForUnchanged(t *testing.T) {
	proj := "team/incr-project-a"
	url1 := "https://gitlab.example.com/" + proj + "/-/merge_requests/1"
	url2 := "https://gitlab.example.com/" + proj + "/-/merge_requests/2"
	url3 := "https://gitlab.example.com/" + proj + "/-/merge_requests/3"
	path1, path2, path3 := "Tasks/Incr1.md", "Tasks/Incr2.md", "Tasks/Incr3.md"

	task1 := incrementalTaskFixture(path1, "Incr 1", "review", "proj-incr", url1)
	task2 := incrementalTaskFixture(path2, "Incr 2", "review", "proj-incr", url2)
	task3 := incrementalTaskFixture(path3, "Incr 3", "review", "proj-incr", url3)

	apiSrv, puts := fakeMRWatcherAPI(t, []Task{task1, task2, task3}, nil,
		map[string]Task{path1: task1, path2: task2, path3: task3})
	client := NewClient(Config{URL: apiSrv.URL})
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.tnClient = client

	srv.mu.Lock()
	srv.state.MRStates[path1] = "opened"
	srv.state.MRStates[path2] = "opened"
	srv.state.MRStates[path3] = "opened"
	srv.mu.Unlock()

	fake := &fakeIncrementalHost{name: "gitlab", project: proj}
	fake.changedSince = func(p string, since time.Time) (map[string]string, time.Time, error) {
		return map[string]string{url1: "merged"}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), nil
	}
	srv.codeHostFunc = func(changeURL, override string) (codeHost, error) { return fake, nil }

	srv.checkMRStatesOnceReal(client)

	fake.mu.Lock()
	nChangedSince := len(fake.changedSinceCalls)
	nChangeState := len(fake.changeStateCalls)
	fake.mu.Unlock()
	if nChangedSince != 1 {
		t.Errorf("expected exactly 1 ChangedSince call, got %d", nChangedSince)
	}
	if nChangeState != 0 {
		t.Errorf("expected 0 ChangeState calls (both unchanged tasks already seen), got %d: %v", nChangeState, fake.changeStateCalls)
	}
	if len(*puts) != 1 {
		t.Fatalf("expected exactly 1 PUT for task1's merge transition, got %+v", *puts)
	}
	if (*puts)[0]["_path"] != path1 || (*puts)[0]["status"] != "done" {
		t.Errorf("expected the PUT to auto-done task1 from the listed state, got %+v", (*puts)[0])
	}
}

// TestIncremental_UnseenTaskPolledIndividuallyEvenIfNotInChanges covers
// scenario (b): a task never seen before is polled individually via
// ChangeState even though the listing did not include it.
func TestIncremental_UnseenTaskPolledIndividuallyEvenIfNotInChanges(t *testing.T) {
	proj := "team/incr-project-b"
	url1 := "https://gitlab.example.com/" + proj + "/-/merge_requests/10"
	path1 := "Tasks/IncrUnseen.md"
	task1 := incrementalTaskFixture(path1, "Incr Unseen", "review", "proj-incr", url1)

	apiSrv, puts := fakeMRWatcherAPI(t, []Task{task1}, nil, map[string]Task{path1: task1})
	client := NewClient(Config{URL: apiSrv.URL})
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.tnClient = client
	// No MRStates seed — first observation.

	fake := &fakeIncrementalHost{name: "gitlab", project: proj}
	fake.changedSince = func(p string, since time.Time) (map[string]string, time.Time, error) {
		return map[string]string{}, time.Time{}, nil // listing doesn't include this MR at all
	}
	fake.changeState = func(string) (string, error) { return "opened", nil }
	srv.codeHostFunc = func(changeURL, override string) (codeHost, error) { return fake, nil }

	srv.checkMRStatesOnceReal(client)

	fake.mu.Lock()
	nChangeState := len(fake.changeStateCalls)
	fake.mu.Unlock()
	if nChangeState != 1 {
		t.Errorf("expected exactly 1 individual ChangeState call for the never-seen task, got %d", nChangeState)
	}
	srv.mu.Lock()
	got := srv.state.MRStates[path1]
	srv.mu.Unlock()
	if got != "opened" {
		t.Errorf("expected MRStates seeded to opened, got %q", got)
	}
	if len(*puts) != 0 {
		t.Errorf("expected no PUT on the silent seed, got %+v", *puts)
	}
}

// TestIncremental_ChangedSinceError_FallsBackPerTask covers scenario (c): a
// ChangedSince failure falls the whole group back to per-task ChangeState
// polling this pass, and never advances the cursor.
func TestIncremental_ChangedSinceError_FallsBackPerTask(t *testing.T) {
	proj := "team/incr-project-c"
	url1 := "https://gitlab.example.com/" + proj + "/-/merge_requests/20"
	path1 := "Tasks/IncrErr.md"
	task1 := incrementalTaskFixture(path1, "Incr Err", "review", "proj-incr", url1)

	apiSrv, puts := fakeMRWatcherAPI(t, []Task{task1}, nil, map[string]Task{path1: task1})
	client := NewClient(Config{URL: apiSrv.URL})
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.tnClient = client
	srv.mu.Lock()
	srv.state.MRStates[path1] = "opened"
	srv.mu.Unlock()

	fake := &fakeIncrementalHost{name: "gitlab", project: proj}
	fake.changedSince = func(p string, since time.Time) (map[string]string, time.Time, error) {
		return nil, time.Time{}, errFakeGlab
	}
	fake.changeState = func(string) (string, error) { return "merged", nil }
	srv.codeHostFunc = func(changeURL, override string) (codeHost, error) { return fake, nil }

	srv.checkMRStatesOnceReal(client)

	fake.mu.Lock()
	nChangeState := len(fake.changeStateCalls)
	fake.mu.Unlock()
	if nChangeState != 1 {
		t.Errorf("expected the fallback to poll the task individually via ChangeState, got %d calls", nChangeState)
	}
	if len(*puts) != 1 || (*puts)[0]["status"] != "done" {
		t.Errorf("expected the fallback to still apply the merge transition, got %+v", *puts)
	}
	srv.mu.Lock()
	cursor := srv.state.MRCursors["gitlab|"+proj]
	srv.mu.Unlock()
	if !cursor.IsZero() {
		t.Errorf("expected the cursor to remain unset after a ChangedSince error, got %v", cursor)
	}
}

// TestIncremental_CursorAdvancesAndIsPassedToNextCall covers scenario (d):
// the cursor advances to ChangedSince's reported newest, and the NEXT
// pass's ChangedSince call receives that stored cursor verbatim as since.
func TestIncremental_CursorAdvancesAndIsPassedToNextCall(t *testing.T) {
	proj := "team/incr-project-d"
	url1 := "https://gitlab.example.com/" + proj + "/-/merge_requests/30"
	path1 := "Tasks/IncrCursor.md"
	task1 := incrementalTaskFixture(path1, "Incr Cursor", "review", "proj-incr", url1)

	apiSrv, _ := fakeMRWatcherAPI(t, []Task{task1}, nil, map[string]Task{path1: task1})
	client := NewClient(Config{URL: apiSrv.URL})
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.tnClient = client
	srv.mu.Lock()
	srv.state.MRStates[path1] = "opened"
	srv.mu.Unlock()

	newest := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	fake := &fakeIncrementalHost{name: "gitlab", project: proj}
	fake.changedSince = func(p string, since time.Time) (map[string]string, time.Time, error) {
		return map[string]string{}, newest, nil
	}
	srv.codeHostFunc = func(changeURL, override string) (codeHost, error) { return fake, nil }

	srv.checkMRStatesOnceReal(client)
	srv.checkMRStatesOnceReal(client)

	fake.mu.Lock()
	calls := append([]time.Time(nil), fake.changedSinceCalls...)
	fake.mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("expected 2 ChangedSince calls, got %d", len(calls))
	}
	if !calls[0].IsZero() {
		t.Errorf("expected the first call's since to be zero (no cursor yet), got %v", calls[0])
	}
	if !calls[1].Equal(newest) {
		t.Errorf("expected the second call's since to equal the advanced cursor %v, got %v", newest, calls[1])
	}

	srv.mu.Lock()
	cursor := srv.state.MRCursors["gitlab|"+proj]
	srv.mu.Unlock()
	if !cursor.Equal(newest) {
		t.Errorf("expected the stored cursor to equal newest %v, got %v", newest, cursor)
	}
}

// TestIncremental_DiscussionsFetchedOnlyForChangedAndOnRefreshPass covers
// scenario (e): discussions are fetched only for changed/first-seen open
// tasks on an ordinary pass, and for every open task in the group
// (including ones the listing found unchanged) on the periodic
// mrReviewRefreshEvery-th refresh pass.
func TestIncremental_DiscussionsFetchedOnlyForChangedAndOnRefreshPass(t *testing.T) {
	proj := "team/incr-project-e"
	urlA := "https://gitlab.example.com/" + proj + "/-/merge_requests/40"
	urlB := "https://gitlab.example.com/" + proj + "/-/merge_requests/41"
	pathA, pathB := "Tasks/IncrDiscA.md", "Tasks/IncrDiscB.md"
	taskA := incrementalTaskFixture(pathA, "A", "review", "proj-incr", urlA)
	taskB := incrementalTaskFixture(pathB, "B", "review", "proj-incr", urlB)

	apiSrv, _ := fakeMRWatcherAPI(t, []Task{taskA, taskB}, nil, map[string]Task{pathA: taskA, pathB: taskB})
	client := NewClient(Config{URL: apiSrv.URL})
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.tnClient = client
	srv.mu.Lock()
	srv.state.MRStates[pathA] = "opened"
	srv.state.MRStates[pathB] = "opened"
	srv.mu.Unlock()

	fake := &fakeIncrementalHost{name: "gitlab", project: proj}
	fake.discussions = func(string) (mrDiscussions, error) { return mrDiscussions{}, nil }
	srv.codeHostFunc = func(changeURL, override string) (codeHost, error) { return fake, nil }

	// Pass 1: only task A appears "changed" (stays opened); B is unchanged.
	fake.changedSince = func(p string, since time.Time) (map[string]string, time.Time, error) {
		return map[string]string{urlA: "opened"}, time.Now(), nil
	}
	srv.checkMRStatesOnceReal(client)

	fake.mu.Lock()
	discCalls := append([]string(nil), fake.discussionsCalls...)
	fake.mu.Unlock()
	if len(discCalls) != 1 || discCalls[0] != urlA {
		t.Fatalf("expected discussions fetched only for the changed open task A on pass 1, got %v", discCalls)
	}

	// Passes 2-5: nothing changes at all.
	fake.changedSince = func(p string, since time.Time) (map[string]string, time.Time, error) {
		return map[string]string{}, time.Time{}, nil
	}
	for i := 0; i < 4; i++ {
		srv.checkMRStatesOnceReal(client)
	}
	fake.mu.Lock()
	discCalls = append([]string(nil), fake.discussionsCalls...)
	fake.mu.Unlock()
	if len(discCalls) != 1 {
		t.Fatalf("expected still only 1 discussions call through pass 5, got %d: %v", len(discCalls), discCalls)
	}

	// Pass 6: periodic refresh — task B (unchanged but open) also gets discussions.
	srv.checkMRStatesOnceReal(client)
	fake.mu.Lock()
	discCalls = append([]string(nil), fake.discussionsCalls...)
	fake.mu.Unlock()
	foundB := false
	for _, u := range discCalls {
		if u == urlB {
			foundB = true
		}
	}
	if !foundB {
		t.Errorf("expected the periodic refresh pass (6th) to fetch discussions for task B too, got %v", discCalls)
	}
}

// TestIncremental_EnvDisable_NoChangedSinceCall covers scenario (f):
// TN_NO_MRINCREMENTAL=1 forces the old per-task path — ChangedSince is
// never called at all.
func TestIncremental_EnvDisable_NoChangedSinceCall(t *testing.T) {
	t.Setenv("TN_NO_MRINCREMENTAL", "1")
	proj := "team/incr-project-f"
	url1 := "https://gitlab.example.com/" + proj + "/-/merge_requests/50"
	path1 := "Tasks/IncrDisabled.md"
	task1 := incrementalTaskFixture(path1, "Incr Disabled", "review", "proj-incr", url1)

	apiSrv, _ := fakeMRWatcherAPI(t, []Task{task1}, nil, map[string]Task{path1: task1})
	client := NewClient(Config{URL: apiSrv.URL})
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.tnClient = client
	srv.mu.Lock()
	srv.state.MRStates[path1] = "opened"
	srv.mu.Unlock()

	fake := &fakeIncrementalHost{name: "gitlab", project: proj}
	fake.changeState = func(string) (string, error) { return "opened", nil }
	srv.codeHostFunc = func(changeURL, override string) (codeHost, error) { return fake, nil }

	srv.checkMRStatesOnceReal(client)

	fake.mu.Lock()
	nChangedSince := len(fake.changedSinceCalls)
	nChangeState := len(fake.changeStateCalls)
	fake.mu.Unlock()
	if nChangedSince != 0 {
		t.Errorf("expected TN_NO_MRINCREMENTAL=1 to skip ChangedSince entirely, got %d calls", nChangedSince)
	}
	if nChangeState != 1 {
		t.Errorf("expected the per-task path to still poll via ChangeState, got %d calls", nChangeState)
	}
}

// TestGitlabChangedSince_ParsesPagesAndLockedState covers scenario (g):
// gitlabHost.ChangedSince parses a paginated canned response (via an
// injected runner — no real glab call), normalizes "locked" to "opened",
// and reports the max updated_at across every page as newest.
func TestGitlabChangedSince_ParsesPagesAndLockedState(t *testing.T) {
	type raw struct {
		WebURL    string `json:"web_url"`
		State     string `json:"state"`
		UpdatedAt string `json:"updated_at"`
	}
	page1Items := make([]raw, glabMRListPerPage)
	for i := range page1Items {
		page1Items[i] = raw{
			WebURL:    fmt.Sprintf("https://gitlab.example.com/team/project/-/merge_requests/%d", i+1),
			State:     "opened",
			UpdatedAt: "2026-01-01T00:00:00Z",
		}
	}
	page1Items[len(page1Items)-1].State = "locked"
	page1Items[len(page1Items)-1].UpdatedAt = "2026-01-02T00:00:00Z"
	page1, err := json.Marshal(page1Items)
	if err != nil {
		t.Fatalf("marshal page1: %v", err)
	}

	page2Items := []raw{
		{WebURL: "https://gitlab.example.com/team/project/-/merge_requests/999", State: "merged", UpdatedAt: "2026-01-03T00:00:00Z"},
	}
	page2, err := json.Marshal(page2Items)
	if err != nil {
		t.Fatalf("marshal page2: %v", err)
	}

	var calledPaths []string
	runner := func(args ...string) ([]byte, error) {
		if len(args) != 2 || args[0] != "api" {
			t.Fatalf("unexpected glab args: %v", args)
		}
		calledPaths = append(calledPaths, args[1])
		if strings.Contains(args[1], "page=2") {
			return page2, nil
		}
		return page1, nil
	}

	h := gitlabHost{runner: runner}
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	changes, newest, err := h.ChangedSince("team/project", since)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(calledPaths) != 2 {
		t.Fatalf("expected exactly 2 api calls (2 pages — page1 full, page2 short), got %d: %v", len(calledPaths), calledPaths)
	}
	if !strings.Contains(calledPaths[0], "updated_after=2025-12-31T23%3A59%3A59Z") {
		t.Errorf("expected the first page's updated_after to be since minus 1 second, got %q", calledPaths[0])
	}
	if len(changes) != glabMRListPerPage+1 {
		t.Errorf("expected %d total changes across both pages, got %d", glabMRListPerPage+1, len(changes))
	}
	lockedURL := page1Items[len(page1Items)-1].WebURL
	if changes[lockedURL] != "opened" {
		t.Errorf("expected a locked MR to be normalized to opened, got %q", changes[lockedURL])
	}
	wantNewest := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	if !newest.Equal(wantNewest) {
		t.Errorf("expected newest = %v (max updated_at across both pages), got %v", wantNewest, newest)
	}
}
