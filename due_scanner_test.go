package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// TestBuildDueTasksQuery verifies the FilterQuery shape: non-archived,
// non-completed, excludes the three human-attention statuses, tagged
// "claude", scheduled on-or-before today.
func TestBuildDueTasksQuery(t *testing.T) {
	fg := buildDueTasksQuery("2026-08-17")
	if fg.Type != "group" || fg.Conjunction != "and" {
		t.Fatalf("unexpected group shape: %+v", fg)
	}
	want := []FilterCondition{
		{Type: "condition", ID: "archived", Property: "archived", Operator: "is-not-checked"},
		{Type: "condition", ID: "completed", Property: "status.isCompleted", Operator: "is-not-checked"},
		{Type: "condition", ID: "not-triage", Property: "status", Operator: "is-not", Value: "triage"},
		{Type: "condition", ID: "not-needs-input", Property: "status", Operator: "is-not", Value: "needs-input"},
		{Type: "condition", ID: "not-review", Property: "status", Operator: "is-not", Value: "review"},
		{Type: "condition", ID: "not-in-progress", Property: "status", Operator: "is-not", Value: "in-progress"},
		{Type: "condition", ID: "not-blocked", Property: "dependencies.isBlocked", Operator: "is-not-checked"},
		{Type: "condition", ID: "tag-claude", Property: "tags", Operator: "contains", Value: "claude"},
		{Type: "condition", ID: "scheduled", Property: "scheduled", Operator: "is-on-or-before", Value: "2026-08-17"},
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

// newDueScannerTestServer builds a Server with a temp-dir state file, a
// no-op spawnFunc, and the given project config, without starting any
// background goroutines.
func newDueScannerTestServer(t *testing.T, projects map[string]ProjectConfig) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg := ServeConfig{Port: 0, Projects: projects}
	return newServer(filepath.Join(dir, "state.json"), cfg, func(project, cwd string, env map[string]string) error { return nil })
}

func queuedMessagesTo(srv *Server, to string) []*Message {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	var out []*Message
	for _, m := range srv.state.Messages {
		if m.To == to {
			out = append(out, m)
		}
	}
	return out
}

// TestScanDueTasksOnce_AssignsWhenDueAndRoutable verifies a due, claude
// tagged task with a routable project gets assigned, and the occurrence is
// recorded for today.
func TestScanDueTasksOnce_AssignsWhenDueAndRoutable(t *testing.T) {
	today := "2026-08-17"
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}})

	task := Task{Path: "Tasks/Recurring.md", Title: "Water the plants", Scheduled: today,
		Tags: []string{"claude"}, Projects: []string{"[[Myapp]]"}}
	srv.scanDueTasksOnce([]Task{task}, today)

	msgs := queuedMessagesTo(srv, "orchestrator-myapp")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 queued assignment, got %d", len(msgs))
	}
	want := "Task assigned: Water the plants (Tasks/Recurring.md) [scheduled 2026-08-17]"
	if msgs[0].Text != want {
		t.Errorf("got text %q, want %q", msgs[0].Text, want)
	}

	srv.mu.Lock()
	stored := srv.state.AssignedOccurrences["Tasks/Recurring.md"]
	srv.mu.Unlock()
	if stored != today {
		t.Errorf("expected AssignedOccurrences[path] = %q, got %q", today, stored)
	}
}

// TestScanDueTasksOnce_SkipsWhenAlreadyAssignedToday verifies no duplicate
// assignment is queued when the occurrence was already routed today.
func TestScanDueTasksOnce_SkipsWhenAlreadyAssignedToday(t *testing.T) {
	today := "2026-08-17"
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}})

	srv.mu.Lock()
	srv.state.AssignedOccurrences["Tasks/Recurring.md"] = today
	srv.mu.Unlock()

	task := Task{Path: "Tasks/Recurring.md", Title: "Water the plants", Scheduled: today,
		Tags: []string{"claude"}, Projects: []string{"[[Myapp]]"}}
	srv.scanDueTasksOnce([]Task{task}, today)

	if msgs := queuedMessagesTo(srv, "orchestrator-myapp"); len(msgs) != 0 {
		t.Errorf("expected no new assignment, got %d: %+v", len(msgs), msgs)
	}
}

// TestScanDueTasksOnce_AssignsAgainOnNewOccurrence verifies a task last
// assigned on a prior day gets a fresh assignment today (a new occurrence).
func TestScanDueTasksOnce_AssignsAgainOnNewOccurrence(t *testing.T) {
	today := "2026-08-17"
	yesterday := "2026-08-16"
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}})

	srv.mu.Lock()
	srv.state.AssignedOccurrences["Tasks/Recurring.md"] = yesterday
	srv.mu.Unlock()

	task := Task{Path: "Tasks/Recurring.md", Title: "Water the plants", Scheduled: today,
		Tags: []string{"claude"}, Projects: []string{"[[Myapp]]"}}
	srv.scanDueTasksOnce([]Task{task}, today)

	msgs := queuedMessagesTo(srv, "orchestrator-myapp")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 new assignment for the new occurrence, got %d", len(msgs))
	}

	srv.mu.Lock()
	stored := srv.state.AssignedOccurrences["Tasks/Recurring.md"]
	srv.mu.Unlock()
	if stored != today {
		t.Errorf("expected AssignedOccurrences[path] updated to %q, got %q", today, stored)
	}
}

// TestScanDueTasksOnce_SkipsWhenPendingAssignmentExists verifies a task
// with an already-pending (queued/delivered) "Task assigned:" message —
// e.g. one routed moments earlier by the webhook path — isn't
// double-assigned, and the occurrence isn't recorded either (so the real
// dedup key stays the pending message, not a premature stored date).
func TestScanDueTasksOnce_SkipsWhenPendingAssignmentExists(t *testing.T) {
	today := "2026-08-17"
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}})

	srv.mu.Lock()
	srv.state.Messages = append(srv.state.Messages, &Message{
		ID: "msg-existing", To: "orchestrator-myapp", TaskPath: "Tasks/Recurring.md",
		Text: "Task assigned: Water the plants (Tasks/Recurring.md)", Status: "queued", CreatedAt: time.Now(),
	})
	srv.mu.Unlock()

	task := Task{Path: "Tasks/Recurring.md", Title: "Water the plants", Scheduled: today,
		Tags: []string{"claude"}, Projects: []string{"[[Myapp]]"}}
	srv.scanDueTasksOnce([]Task{task}, today)

	msgs := queuedMessagesTo(srv, "orchestrator-myapp")
	if len(msgs) != 1 {
		t.Fatalf("expected the pre-existing message to remain the only one, got %d: %+v", len(msgs), msgs)
	}

	srv.mu.Lock()
	_, stored := srv.state.AssignedOccurrences["Tasks/Recurring.md"]
	srv.mu.Unlock()
	if stored {
		t.Error("expected no occurrence recorded when skipped due to a pending assignment")
	}
}

// TestScanDueTasksOnce_SkipsUnroutableTasks covers both unroutable cases:
// no resolvable slug at all, and a resolvable slug with neither an alive
// agent nor autoSpawn configured.
func TestScanDueTasksOnce_SkipsUnroutableTasks(t *testing.T) {
	today := "2026-08-17"
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}})

	noSlug := Task{Path: "Tasks/NoProject.md", Title: "Untagged project", Scheduled: today, Tags: []string{"claude"}}
	unconfigured := Task{Path: "Tasks/Unconfigured.md", Title: "Unconfigured", Scheduled: today,
		Tags: []string{"claude"}, Projects: []string{"[[SomeOtherRepo]]"}}

	srv.scanDueTasksOnce([]Task{noSlug, unconfigured}, today)

	srv.mu.Lock()
	total := len(srv.state.Messages)
	_, noSlugStored := srv.state.AssignedOccurrences["Tasks/NoProject.md"]
	_, unconfiguredStored := srv.state.AssignedOccurrences["Tasks/Unconfigured.md"]
	srv.mu.Unlock()

	if total != 0 {
		t.Errorf("expected no messages queued for unroutable tasks, got %d", total)
	}
	if noSlugStored || unconfiguredStored {
		t.Error("expected no occurrence recorded for unroutable tasks")
	}
}

// TestScanDueTasksOnce_RoutesToAliveAgentEvenWithoutAutoSpawn verifies a
// project with a live agent but no autoSpawn config still gets routed (the
// "unroutable" skip only applies when there's neither).
func TestScanDueTasksOnce_RoutesToAliveAgentEvenWithoutAutoSpawn(t *testing.T) {
	today := "2026-08-17"
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{}) // no autoSpawn config at all

	srv.mu.Lock()
	srv.state.Agents["claude/myapp-manual"] = &Agent{
		Name: "claude/myapp-manual", Project: "myapp", LastSeenAt: time.Now(), Accepting: true,
	}
	srv.mu.Unlock()

	task := Task{Path: "Tasks/Recurring.md", Title: "Water the plants", Scheduled: today,
		Tags: []string{"claude"}, Projects: []string{"[[Myapp]]"}}
	srv.scanDueTasksOnce([]Task{task}, today)

	if msgs := queuedMessagesTo(srv, "claude/myapp-manual"); len(msgs) != 1 {
		t.Fatalf("expected 1 assignment routed to the alive agent, got %d", len(msgs))
	}
}

// TestScanDueTasksRemote_QueryShapeAndProcessing spins up an httptest
// TaskNotes API, verifies the POST /api/tasks/query body matches
// buildDueTasksQuery's shape, and confirms the returned task is processed
// (queued to the resolved orchestrator).
func TestScanDueTasksRemote_QueryShapeAndProcessing(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody FilterGroup
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"tasks":[{"path":"Tasks/Recurring.md","title":"Water the plants","scheduled":"2026-08-17","tags":["claude"],"projects":["[[Myapp]]"]}],"total":1,"filtered":1}}`))
	}))
	defer apiSrv.Close()

	client := NewClient(Config{URL: apiSrv.URL})
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{"myapp": {AutoSpawn: true, Cwd: "/repos/myapp"}})

	srv.scanDueTasksRemote(client)

	if gotMethod != http.MethodPost || gotPath != "/api/tasks/query" {
		t.Fatalf("expected POST /api/tasks/query, got %s %s", gotMethod, gotPath)
	}
	if len(gotBody.Children) != 9 {
		t.Fatalf("expected 9 filter conditions sent, got %+v", gotBody.Children)
	}
	foundScheduled := false
	for _, c := range gotBody.Children {
		if c.Property == "scheduled" && c.Operator == "is-on-or-before" {
			foundScheduled = true
		}
	}
	if !foundScheduled {
		t.Errorf("expected a scheduled is-on-or-before condition, got %+v", gotBody.Children)
	}

	msgs := queuedMessagesTo(srv, "orchestrator-myapp")
	if len(msgs) != 1 {
		t.Fatalf("expected the fetched due task to be processed and queued, got %d messages", len(msgs))
	}
}
