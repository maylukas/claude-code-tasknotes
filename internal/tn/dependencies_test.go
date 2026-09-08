package tn

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIsBlockedByUnresolvedDependency covers the four combinations: no
// blockers (never held back); blockers with no evidence either way (held
// back — pragmatically conservative); blockers with confirmed-still-blocked
// evidence (held back); blockers with confirmed-unblocked evidence (not
// held back).
func TestIsBlockedByUnresolvedDependency(t *testing.T) {
	trueVal, falseVal := true, false

	cases := []struct {
		name string
		task webhookTask
		want bool
	}{
		{"no blockers", webhookTask{}, false},
		{"blockers, no evidence", webhookTask{BlockedBy: []TaskDependency{{UID: "Tasks/A.md"}}}, true},
		{"blockers, confirmed still blocked", webhookTask{BlockedBy: []TaskDependency{{UID: "Tasks/A.md"}}, IsBlocked: &trueVal}, true},
		{"blockers, confirmed unblocked", webhookTask{BlockedBy: []TaskDependency{{UID: "Tasks/A.md"}}, IsBlocked: &falseVal}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isBlockedByUnresolvedDependency(c.task); got != c.want {
				t.Errorf("isBlockedByUnresolvedDependency(%+v) = %v, want %v", c.task, got, c.want)
			}
		})
	}
}

// TestIntegrationBranchFor_TaskLevelOnlyNoFallback verifies
// integrationBranchFor resolves ONLY from the task's own
// customProperties["integration-branch"] — no repo-note or other fallback
// source anywhere in the resolution path (a corrected requirement: an
// earlier draft of the spec had a repo-wide fallback, which would have
// wrongly captured every unrelated task in the repo). The function's own
// signature — func(Task) string, no repoSettings parameter — structurally
// enforces this; this test covers the two resolvable states.
func TestIntegrationBranchFor_TaskLevelOnlyNoFallback(t *testing.T) {
	cases := []struct {
		name string
		task Task
		want string
	}{
		{"unset", Task{Path: "Tasks/A.md"}, ""},
		{"empty customProperties map", Task{Path: "Tasks/A.md", CustomProperties: map[string]string{}}, ""},
		{"set", Task{Path: "Tasks/A.md", CustomProperties: map[string]string{"integration-branch": "feat/e2e-recorder"}}, "feat/e2e-recorder"},
		{"unrelated custom properties present", Task{Path: "Tasks/A.md", CustomProperties: map[string]string{"mr": "https://x", "claude-project": "myapp"}}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := integrationBranchFor(c.task); got != c.want {
				t.Errorf("integrationBranchFor(%+v) = %q, want %q", c.task, got, c.want)
			}
		})
	}
}

// TestIntegrationRoleFor_TaskLevelOnly verifies integrationRoleFor resolves
// ONLY from the task's own customProperties["integration-role"] — "owner"
// or "" (unset = member). Task-level only, same as integrationBranchFor.
func TestIntegrationRoleFor_TaskLevelOnly(t *testing.T) {
	cases := []struct {
		name string
		task Task
		want string
	}{
		{"unset", Task{Path: "Tasks/A.md"}, ""},
		{"owner", Task{Path: "Tasks/A.md", CustomProperties: map[string]string{"integration-role": "owner"}}, "owner"},
		{"unrelated custom properties present", Task{Path: "Tasks/A.md", CustomProperties: map[string]string{"integration-branch": "feat/x"}}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := integrationRoleFor(c.task); got != c.want {
				t.Errorf("integrationRoleFor(%+v) = %q, want %q", c.task, got, c.want)
			}
		})
	}
}

// TestAppendIntegrationBranchSuffix covers the unset, member, and owner
// cases — a member gets the plain "[integration branch: <b>]" suffix, the
// owner gets the role-aware "[integration OWNER of <b> — consolidated MR
// to development]" suffix instead, so a fresh generation knows immediately
// whether to stack onto the branch or finalize it.
func TestAppendIntegrationBranchSuffix(t *testing.T) {
	if got := appendIntegrationBranchSuffix("Task assigned: X (x.md)", "", ""); got != "Task assigned: X (x.md)" {
		t.Errorf("expected text unchanged when branch is empty, got %q", got)
	}
	wantMember := "Task assigned: X (x.md) [integration branch: feat/thing]"
	if got := appendIntegrationBranchSuffix("Task assigned: X (x.md)", "feat/thing", ""); got != wantMember {
		t.Errorf("member: got %q, want %q", got, wantMember)
	}
	wantOwner := "Task assigned: X (x.md) [integration OWNER of feat/thing — consolidated MR to development]"
	if got := appendIntegrationBranchSuffix("Task assigned: X (x.md)", "feat/thing", "owner"); got != wantOwner {
		t.Errorf("owner: got %q, want %q", got, wantOwner)
	}
}

// TestRouteTaskNotification_DependencyGuard verifies the assignment branch
// skips a claude-tagged task with an unresolved dependency, and routes
// normally once there's confirmed-unblocked evidence.
func TestRouteTaskNotification_DependencyGuard(t *testing.T) {
	trueVal, falseVal := true, false

	t.Run("skips when blocked with no evidence", func(t *testing.T) {
		srv, ts := newTestServer(t)
		register(t, ts, "agent-dep1", "proj-dep")
		srv.routeTaskNotification(webhookTask{
			Path: "Tasks/Dep1.md", Title: "Dep1", Status: "open", Tags: []string{"claude"},
			Projects: []string{"proj-dep"}, BlockedBy: []TaskDependency{{UID: "Tasks/Blocker.md"}},
		})
		if msgs := queuedMessagesTo(srv, "agent-dep1"); len(msgs) != 0 {
			t.Errorf("expected no assignment while blocked with no evidence, got %+v", msgs)
		}
	})

	t.Run("skips when blocked with confirmed-blocked evidence", func(t *testing.T) {
		srv, ts := newTestServer(t)
		register(t, ts, "agent-dep2", "proj-dep")
		srv.routeTaskNotification(webhookTask{
			Path: "Tasks/Dep2.md", Title: "Dep2", Status: "open", Tags: []string{"claude"},
			Projects: []string{"proj-dep"}, BlockedBy: []TaskDependency{{UID: "Tasks/Blocker.md"}}, IsBlocked: &trueVal,
		})
		if msgs := queuedMessagesTo(srv, "agent-dep2"); len(msgs) != 0 {
			t.Errorf("expected no assignment while confirmed blocked, got %+v", msgs)
		}
	})

	t.Run("routes when confirmed unblocked despite having blockedBy entries", func(t *testing.T) {
		srv, ts := newTestServer(t)
		register(t, ts, "agent-dep3", "proj-dep")
		srv.routeTaskNotification(webhookTask{
			Path: "Tasks/Dep3.md", Title: "Dep3", Status: "open", Tags: []string{"claude"},
			Projects: []string{"proj-dep"}, BlockedBy: []TaskDependency{{UID: "Tasks/Blocker.md"}}, IsBlocked: &falseVal,
		})
		if msgs := queuedMessagesTo(srv, "agent-dep3"); len(msgs) != 1 {
			t.Fatalf("expected the assignment to route once confirmed unblocked, got %+v", msgs)
		}
	})

	t.Run("routes normally with no blockedBy at all", func(t *testing.T) {
		srv, ts := newTestServer(t)
		register(t, ts, "agent-dep4", "proj-dep")
		srv.routeTaskNotification(webhookTask{
			Path: "Tasks/Dep4.md", Title: "Dep4", Status: "open", Tags: []string{"claude"},
			Projects: []string{"proj-dep"},
		})
		if msgs := queuedMessagesTo(srv, "agent-dep4"); len(msgs) != 1 {
			t.Fatalf("expected normal routing with no dependencies, got %+v", msgs)
		}
	})
}

// TestBuildUnblockedDependentsQuery verifies the unblock pass's query
// shape: open, non-archived, claude-tagged, has blockers, and TaskNotes
// itself now considers it startable.
func TestBuildUnblockedDependentsQuery(t *testing.T) {
	fg := buildUnblockedDependentsQuery()
	if fg.Type != "group" || fg.Conjunction != "and" {
		t.Fatalf("unexpected group shape: %+v", fg)
	}
	want := []FilterCondition{
		{Type: "condition", ID: "archived", Property: "archived", Operator: "is-not-checked"},
		{Type: "condition", ID: "completed", Property: "status.isCompleted", Operator: "is-not-checked"},
		{Type: "condition", ID: "tag-claude", Property: "tags", Operator: "contains", Value: "claude"},
		{Type: "condition", ID: "has-blockers", Property: "blockedBy", Operator: "is-not-empty"},
		{Type: "condition", ID: "not-blocked", Property: "dependencies.isBlocked", Operator: "is-not-checked"},
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

// fakeUnblockQueryAPI simulates POST /api/tasks/query returning a fixed set
// of candidate tasks for the unblock pass.
func fakeUnblockQueryAPI(t *testing.T, tasks []Task) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"tasks": tasks, "total": len(tasks), "filtered": len(tasks),
		}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRunUnblockPass_RoutesOnceWithSuffixAndDedups verifies a now-startable
// dependent gets exactly one assignment with the "[unblocked by ...]"
// suffix, and that running the pass again doesn't duplicate it (the
// existing pending-assignment dedup applies, since both messages share the
// "Task assigned:" prefix).
func TestRunUnblockPass_RoutesOnceWithSuffixAndDedups(t *testing.T) {
	apiSrv := fakeUnblockQueryAPI(t, []Task{
		{Path: "Tasks/Dependent.md", Title: "Dependent", Status: "open", Projects: []string{"proj-unblock"}},
	})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	register(t, ts, "agent-unblock", "proj-unblock")

	srv.runUnblockPass("Blocker Task")

	msgs := queuedMessagesTo(srv, "agent-unblock")
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 assignment, got %+v", msgs)
	}
	want := "Task assigned: Dependent (Tasks/Dependent.md) [unblocked by Blocker Task]"
	if msgs[0].Text != want {
		t.Errorf("got text %q, want %q", msgs[0].Text, want)
	}

	// A second pass (e.g. from the 10-min due-task scanner also seeing it,
	// or the completed-task webhook firing twice) must not duplicate.
	srv.runUnblockPass("Blocker Task")
	if msgs := queuedMessagesTo(srv, "agent-unblock"); len(msgs) != 1 {
		t.Errorf("expected still exactly 1 assignment after a second pass, got %d: %+v", len(msgs), msgs)
	}
}

// TestRunUnblockPass_AppendsIntegrationBranchSuffixOnlyWhenSet verifies the
// assignment text carries " [integration branch: <b>]" when the dependent
// task's own customProperties.integration-branch is set, and omits it
// entirely otherwise — this is how a fresh generation (the orchestrator
// that knew about the workstream's target branch may have since retired)
// learns the branch without reading the board.
func TestRunUnblockPass_AppendsIntegrationBranchSuffixOnlyWhenSet(t *testing.T) {
	apiSrv := fakeUnblockQueryAPI(t, []Task{
		{
			Path: "Tasks/OnBranch.md", Title: "OnBranch", Status: "open", Projects: []string{"proj-unblock"},
			CustomProperties: map[string]string{"integration-branch": "feat/e2e-recorder"},
		},
		{Path: "Tasks/NoBranch.md", Title: "NoBranch", Status: "open", Projects: []string{"proj-unblock"}},
		{
			Path: "Tasks/Owner.md", Title: "Owner", Status: "open", Projects: []string{"proj-unblock"},
			CustomProperties: map[string]string{"integration-branch": "feat/e2e-recorder", "integration-role": "owner"},
		},
	})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	register(t, ts, "agent-branch", "proj-unblock")

	srv.runUnblockPass("Blocker Task")

	msgs := queuedMessagesTo(srv, "agent-branch")
	if len(msgs) != 3 {
		t.Fatalf("expected 3 assignments, got %+v", msgs)
	}
	byPath := map[string]string{}
	for _, m := range msgs {
		byPath[m.TaskPath] = m.Text
	}
	wantOnBranch := "Task assigned: OnBranch (Tasks/OnBranch.md) [unblocked by Blocker Task] [integration branch: feat/e2e-recorder]"
	if byPath["Tasks/OnBranch.md"] != wantOnBranch {
		t.Errorf("member: got %q, want %q", byPath["Tasks/OnBranch.md"], wantOnBranch)
	}
	wantNoBranch := "Task assigned: NoBranch (Tasks/NoBranch.md) [unblocked by Blocker Task]"
	if byPath["Tasks/NoBranch.md"] != wantNoBranch {
		t.Errorf("got %q, want %q (no suffix when unset)", byPath["Tasks/NoBranch.md"], wantNoBranch)
	}
	// The owner gets the "All members complete —" prefix in ADDITION to the
	// role-aware OWNER suffix — this pass firing at all means its last
	// blockedBy member just completed, which is exactly what team-lead
	// asked to call out explicitly (routeUnblockedDependent is the one
	// place that specific moment is known).
	wantOwner := "All members complete — Task assigned: Owner (Tasks/Owner.md) [unblocked by Blocker Task] [integration OWNER of feat/e2e-recorder — consolidated MR to development]"
	if byPath["Tasks/Owner.md"] != wantOwner {
		t.Errorf("owner: got %q, want %q", byPath["Tasks/Owner.md"], wantOwner)
	}
}

// TestOwnerWorkflow_BlockedWhileMembersIncomplete_ThenAssignedOnce is the
// end-to-end assertion team-lead asked for: the EXISTING dependency
// machinery (isBlockedByUnresolvedDependency + buildUnblockedDependentsQuery)
// should already give this for free — integration-role only adds a
// message suffix, no new blocking logic was written for owner tasks
// specifically. Phase 1: an owner task with an unresolved blockedBy member
// is never assigned via the normal webhook path. Phase 2: once the member
// completes (simulated via the unblock pass query returning the
// now-unblocked owner, as TaskNotes itself would report
// dependencies.isBlocked is-not-checked at that point), it's assigned
// EXACTLY once, with the "All members complete —" prefix and the OWNER
// suffix — and a second identical pass doesn't duplicate it.
func TestOwnerWorkflow_BlockedWhileMembersIncomplete_ThenAssignedOnce(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "agent-owner", "proj-owner")

	trueVal := true
	// Phase 1: owner task observed via webhook, still blocked by its one
	// member (no confirmed-unblocked evidence).
	srv.routeTaskNotification(webhookTask{
		Path: "Tasks/OwnerTask.md", Title: "OwnerTask", Status: "open", Tags: []string{"claude"},
		Projects:         []string{"proj-owner"},
		BlockedBy:        []TaskDependency{{UID: "Tasks/Member.md"}},
		IsBlocked:        &trueVal,
		CustomProperties: map[string]string{"integration-branch": "feat/e2e-recorder", "integration-role": "owner"},
	})
	if msgs := queuedMessagesTo(srv, "agent-owner"); len(msgs) != 0 {
		t.Fatalf("expected the owner task NOT assigned while blocked, got %+v", msgs)
	}

	// Phase 2: the member completes; the unblock pass finds the owner now
	// startable.
	apiSrv := fakeUnblockQueryAPI(t, []Task{
		{
			Path: "Tasks/OwnerTask.md", Title: "OwnerTask", Status: "open", Projects: []string{"proj-owner"},
			CustomProperties: map[string]string{"integration-branch": "feat/e2e-recorder", "integration-role": "owner"},
		},
	})
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	srv.runUnblockPass("Member")

	msgs := queuedMessagesTo(srv, "agent-owner")
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 assignment once unblocked, got %d: %+v", len(msgs), msgs)
	}
	want := "All members complete — Task assigned: OwnerTask (Tasks/OwnerTask.md) [unblocked by Member] [integration OWNER of feat/e2e-recorder — consolidated MR to development]"
	if msgs[0].Text != want {
		t.Errorf("got %q, want %q", msgs[0].Text, want)
	}

	// A second identical pass must not duplicate (existing dedup applies).
	srv.runUnblockPass("Member")
	if msgs := queuedMessagesTo(srv, "agent-owner"); len(msgs) != 1 {
		t.Errorf("expected still exactly 1 assignment after a second pass, got %d: %+v", len(msgs), msgs)
	}
}

// TestRunUnblockPass_SkipsInProgressDependents verifies a candidate that's
// already in-progress (someone's already on it) isn't reassigned.
func TestRunUnblockPass_SkipsInProgressDependents(t *testing.T) {
	apiSrv := fakeUnblockQueryAPI(t, []Task{
		{Path: "Tasks/AlreadyWorking.md", Title: "AlreadyWorking", Status: "in-progress", Projects: []string{"proj-unblock"}},
	})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	register(t, ts, "agent-working", "proj-unblock")

	srv.runUnblockPass("Blocker Task")

	if msgs := queuedMessagesTo(srv, "agent-working"); len(msgs) != 0 {
		t.Errorf("expected no assignment for an already in-progress dependent, got %+v", msgs)
	}
}

// TestRunUnblockPass_SkipsHumanAttentionStatuses verifies a candidate
// parked in triage/needs-input/review isn't auto-assigned either.
func TestRunUnblockPass_SkipsHumanAttentionStatuses(t *testing.T) {
	apiSrv := fakeUnblockQueryAPI(t, []Task{
		{Path: "Tasks/Parked.md", Title: "Parked", Status: "triage", Projects: []string{"proj-unblock"}},
	})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	register(t, ts, "agent-parked2", "proj-unblock")

	srv.runUnblockPass("Blocker Task")

	if msgs := queuedMessagesTo(srv, "agent-parked2"); len(msgs) != 0 {
		t.Errorf("expected no assignment for a task parked in triage, got %+v", msgs)
	}
}

// TestRouteTaskNotification_TriggersUnblockPassOnDone verifies the
// end-to-end trigger: a task transitioning to "done" causes
// routeTaskNotification to run the unblock pass automatically.
func TestRouteTaskNotification_TriggersUnblockPassOnDone(t *testing.T) {
	apiSrv := fakeUnblockQueryAPI(t, []Task{
		{Path: "Tasks/WasBlocked.md", Title: "WasBlocked", Status: "open", Projects: []string{"proj-trigger"}},
	})
	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})
	register(t, ts, "agent-trigger", "proj-trigger")

	srv.routeTaskNotification(webhookTask{
		Path: "Tasks/Blocker.md", Title: "Finished Blocker", Status: "done",
	})

	msgs := queuedMessagesTo(srv, "agent-trigger")
	if len(msgs) != 1 {
		t.Fatalf("expected the unblock pass to trigger and route the dependent, got %+v", msgs)
	}
	if !strings.Contains(msgs[0].Text, "[unblocked by Finished Blocker]") {
		t.Errorf("expected the completed task's title in the suffix, got %q", msgs[0].Text)
	}
}
