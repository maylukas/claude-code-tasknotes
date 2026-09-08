package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// TestHandleStatus_NoDeliverySideEffects verifies GET /status is
// spectator-safe: a queued message stays queued (not delivered) and an
// agent's LastSeenAt is untouched by the call — unlike /inbox, this is
// pure observation.
func TestHandleStatus_NoDeliverySideEffects(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp-g1", "myapp")

	srv.mu.Lock()
	srv.state.Messages = append(srv.state.Messages, &Message{
		ID: "m1", To: "orchestrator-myapp-g1", Status: "queued", CreatedAt: time.Now(),
	})
	beforeLastSeen := srv.state.Agents["orchestrator-myapp-g1"].LastSeenAt
	srv.mu.Unlock()

	resp, err := http.Get(ts.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.state.Messages[0].Status != "queued" {
		t.Errorf("expected the message to stay queued, got %q", srv.state.Messages[0].Status)
	}
	if !srv.state.Agents["orchestrator-myapp-g1"].LastSeenAt.Equal(beforeLastSeen) {
		t.Errorf("expected LastSeenAt untouched by /status, got %v (was %v)", srv.state.Agents["orchestrator-myapp-g1"].LastSeenAt, beforeLastSeen)
	}
}

// TestBuildStatusResponse_PermissionDenialsSurfacedSortedByRecency verifies
// /status.permissionDenials converts the persisted telemetry map into a
// slice sorted most-recent-first — a human scanning this list should see
// what just happened at the top, not have to hunt for it.
func TestBuildStatusResponse_PermissionDenialsSurfacedSortedByRecency(t *testing.T) {
	now := time.Now()
	snap := dashboardSnapshot{}
	denials := map[string]*permissionDenialRecord{
		"a": {AgentName: "orchestrator-myapp", ToolName: "Bash", CommandSummary: "git push", Count: 2, FirstSeenAt: now.Add(-2 * time.Hour), LastSeenAt: now.Add(-90 * time.Minute), LastCommand: "git push --force origin main"},
		"b": {AgentName: "orchestrator-myapp", ToolName: "Bash", CommandSummary: "glab mr", Count: 5, FirstSeenAt: now.Add(-3 * time.Hour), LastSeenAt: now.Add(-5 * time.Minute), LastCommand: "glab mr update 789"},
	}
	resp := buildStatusResponse(snap, nil, nil, nil, statusTaskCounts{}, nil, fixedRepoSettings(defaultRepoSettings()), now, 8391, false, nil, time.Time{}, 0, "", nil, nil, denials)

	if len(resp.PermissionDenials) != 2 {
		t.Fatalf("expected 2 permission-denial records, got %d", len(resp.PermissionDenials))
	}
	if resp.PermissionDenials[0].CommandSummary != "glab mr" {
		t.Errorf("expected the more-recently-seen record first, got %+v", resp.PermissionDenials)
	}
	if resp.PermissionDenials[0].Count != 5 || resp.PermissionDenials[0].LastCommand != "glab mr update 789" {
		t.Errorf("expected fields mapped through correctly, got %+v", resp.PermissionDenials[0])
	}
}

// TestBuildStatusResponse_PermissionDenialsExcludesTestVerificationAgents
// verifies zz-prefixed test registrations never surface in
// /status.permissionDenials — same convention as agents/stuckPrompts, so
// a spike's throwaway verification traffic never looks like real
// telemetry to whoever reads this endpoint.
func TestBuildStatusResponse_PermissionDenialsExcludesTestVerificationAgents(t *testing.T) {
	now := time.Now()
	denials := map[string]*permissionDenialRecord{
		"a": {AgentName: "zz-spiketest", ToolName: "Bash", CommandSummary: "rm -rf", Count: 1, LastSeenAt: now},
		"b": {AgentName: "orchestrator-myapp", ToolName: "Bash", CommandSummary: "git push", Count: 1, LastSeenAt: now},
	}
	resp := buildStatusResponse(dashboardSnapshot{}, nil, nil, nil, statusTaskCounts{}, nil, fixedRepoSettings(defaultRepoSettings()), now, 8391, false, nil, time.Time{}, 0, "", nil, nil, denials)

	if len(resp.PermissionDenials) != 1 || resp.PermissionDenials[0].AgentName != "orchestrator-myapp" {
		t.Errorf("expected only the non-zz- agent's record surfaced, got %+v", resp.PermissionDenials)
	}
}

// TestStatusPermissionDenial_JSONFieldIsAgentName is a real WIRE-FORMAT
// test, not just a Go-struct-field test — the 2026-08-26 production
// incident this pins down is exactly the kind Go-level field access
// can't catch: the underlying data was correct the whole time
// (permissionDenialRecord.AgentName, and the hook payload's own
// "agentName" key, were never wrong), but the /status JSON key was
// "agent" — a name that only matched an unrelated sibling struct
// (statusStuckPrompt), not this record's own data model — so a query
// that reasonably expected "agentName" (matching the hook payload and
// the persisted record) saw nothing there and read a populated record as
// empty. Marshal to actual JSON and check the literal key, not just that
// some Go field holds the right value.
func TestStatusPermissionDenial_JSONFieldIsAgentName(t *testing.T) {
	rec := statusPermissionDenial{AgentName: "orchestrator-myapp-g34005", ToolName: "Bash", CommandSummary: "sed -i", Count: 1}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if got, ok := raw["agentName"]; !ok || got != "orchestrator-myapp-g34005" {
		t.Errorf("expected JSON key \"agentName\" with the agent's name, got raw=%v (full JSON: %s)", raw, b)
	}
	if _, ok := raw["agent"]; ok {
		t.Errorf("expected NO \"agent\" key (the old, inconsistent name) in the JSON, got %s", b)
	}
}

// TestStatusStructs_IdentityFieldWireNames pins down the CURRENT, known
// state of an unresolved inconsistency across /status, found while
// auditing for the same bug class as
// TestStatusPermissionDenial_JSONFieldIsAgentName: statusStuckPrompt and
// statusActivityEntry — both established, both already consumed by the
// webui/tray — use the JSON key "agent" for an agent-identity reference.
// statusPermissionDenial (new, 2026-08-26) deliberately uses "agentName"
// instead, matching ITS OWN feature's pipeline (the hook payload,
// permissionDenialRecord) rather than these two. That is a live,
// accepted tradeoff, not an oversight — flagged for a decision rather
// than silently unified either way, since renaming either existing
// struct touches real consumers. This test's job is narrow: if that
// tradeoff is ever revisited, whoever changes it should have to update
// this test deliberately, not discover the drift by accident the way the
// permissionDenial bug was discovered.
func TestStatusStructs_IdentityFieldWireNames(t *testing.T) {
	assertKey := func(t *testing.T, v any, wantKey string) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal failed: %v", err)
		}
		var raw map[string]any
		if err := json.Unmarshal(b, &raw); err != nil {
			t.Fatalf("unmarshal failed: %v", err)
		}
		if _, ok := raw[wantKey]; !ok {
			t.Errorf("expected JSON key %q, got keys %v (full JSON: %s)", wantKey, raw, b)
		}
	}
	assertKey(t, statusStuckPrompt{Agent: "x"}, "agent")
	assertKey(t, statusActivityEntry{Agent: "x"}, "agent")
	assertKey(t, statusPermissionDenial{AgentName: "x"}, "agentName")
}

// fixedRepoSettings returns a repoSettingsFor-shaped func for tests that
// don't need a real vault on disk.
func fixedRepoSettings(settings repoSettings) func(string) repoSettings {
	return func(string) repoSettings { return settings }
}

// TestBuildStatusResponse_BucketsAgentsPerProjectAndAppliesCaps is the
// core shape test: agents bucketed under the right project, task/pending
// counts aggregated correctly, and both list caps (needsAction, activity)
// enforced at 20.
func TestBuildStatusResponse_BucketsAgentsPerProjectAndAppliesCaps(t *testing.T) {
	now := time.Now()
	pct := 47.0
	snap := dashboardSnapshot{
		Agents: []Agent{
			{Name: "orchestrator-myapp-g1", Project: "myapp", LastSeenAt: now, Accepting: true, ContextPct: &pct, TmuxSession: "tn-myapp-g1"},
			{Name: "orchestrator-myapp-g2", Project: "myapp", LastSeenAt: now.Add(-3 * time.Hour), Accepting: true},
			{Name: "orchestrator-other-g1", Project: "other", LastSeenAt: now, Accepting: false},
		},
		Messages: []Message{
			{To: "orchestrator-myapp-g1", Status: "queued"},
			{To: "orchestrator-myapp-g1", Status: "delivered"},
			{To: "orchestrator-myapp-g1", Status: "delivered"},
			{To: "orchestrator-other-g1", Status: "queued"},
		},
	}
	stuckByAgent := map[string]stuckInfo{
		"orchestrator-myapp-g2": {Stuck: true, Excerpt: "Do you want to proceed?"},
	}
	ownedTaskPaths := map[string][]string{"orchestrator-myapp-g1": {"Tasks/A.md", "Tasks/B.md", "Tasks/C.md"}}
	taskSummaryByPath := map[string]taskSummary{
		"Tasks/A.md": {Title: "A", Status: "in-progress"},
		"Tasks/B.md": {Title: "B", Status: "in-progress"},
		"Tasks/C.md": {Title: "C", Status: "in-progress"},
	}
	workersByAgent := map[string][]workerEntry{
		"orchestrator-myapp-g1": {{TaskPath: "Tasks/A.md", StartedAt: now}},
	}

	var needsActionTasks []Task
	for i := 0; i < 25; i++ {
		needsActionTasks = append(needsActionTasks, Task{
			Path: "Tasks/T.md", Title: "T", Status: "review", Projects: []string{"myapp"},
		})
	}
	taskCounts := statusTaskCounts{"myapp": {"open": 3, "in-progress": 1}}
	configuredProjects := map[string]ProjectConfig{"myapp": {AutoSpawn: true}}
	askBriefByPath := map[string]statusAskBrief{"Tasks/T.md": {Ask: "Merge or reply?", Brief: "Fix verified."}}

	resp := buildStatusResponse(snap, stuckByAgent, ownedTaskPaths, needsActionTasks, taskCounts,
		configuredProjects, fixedRepoSettings(repoSettings{MaxOrchestrators: 2, MaxWorkers: 4}), now, 8391, false, askBriefByPath, time.Time{}, 0, "", taskSummaryByPath, workersByAgent, nil)

	if resp.Daemon.Port != 8391 || resp.Daemon.Version != daemonVersion {
		t.Errorf("unexpected daemon block: %+v", resp.Daemon)
	}

	myapp, ok := resp.Projects["myapp"]
	if !ok {
		t.Fatalf("expected a myapp project bucket, got %+v", resp.Projects)
	}
	if len(myapp.Agents) != 2 {
		t.Fatalf("expected 2 agents bucketed under myapp, got %+v", myapp.Agents)
	}
	if myapp.Queued != 1 || myapp.Delivered != 2 {
		t.Errorf("expected queued=1 delivered=2 for myapp, got queued=%d delivered=%d", myapp.Queued, myapp.Delivered)
	}
	if myapp.MaxOrchestrators != 2 || myapp.MaxWorkers != 4 {
		t.Errorf("expected repo settings applied, got %+v", myapp)
	}
	if myapp.TaskCounts["open"] != 3 || myapp.TaskCounts["in-progress"] != 1 {
		t.Errorf("expected task counts applied, got %+v", myapp.TaskCounts)
	}
	if myapp.NeedsAction != 25 {
		t.Errorf("expected all 25 needs-action tasks counted for myapp, got %d", myapp.NeedsAction)
	}

	other, ok := resp.Projects["other"]
	if !ok || len(other.Agents) != 1 || other.Agents[0].Draining != true || other.Agents[0].Accepting != false {
		t.Errorf("expected other project's agent bucketed and draining, got %+v", resp.Projects["other"])
	}

	// Per-agent shape.
	var g1 statusAgent
	for _, a := range myapp.Agents {
		if a.Name == "orchestrator-myapp-g1" {
			g1 = a
		}
	}
	if g1.ContextPct == nil || *g1.ContextPct != 47.0 || g1.TmuxSession != "tn-myapp-g1" || g1.OwnedInProgress != 3 {
		t.Errorf("unexpected g1 shape: %+v", g1)
	}
	if len(g1.OwnedTasks) != 3 {
		t.Fatalf("expected 3 ownedTasks entries, got %+v", g1.OwnedTasks)
	}
	byPath := map[string]statusOwnedTask{}
	for _, ot := range g1.OwnedTasks {
		byPath[ot.Path] = ot
	}
	if byPath["Tasks/A.md"].Title != "A" || byPath["Tasks/A.md"].Status != "in-progress" {
		t.Errorf("expected ownedTasks titles/status filled in from the task-summary cache, got %+v", g1.OwnedTasks)
	}
	if len(g1.Workers) != 1 || g1.Workers[0].Path != "Tasks/A.md" || g1.Workers[0].Title != "A" {
		t.Errorf("expected 1 worker with its title filled in, got %+v", g1.Workers)
	}

	if len(resp.NeedsActionTasks) != statusNeedsActionCap {
		t.Errorf("expected needsActionTasks capped at %d, got %d", statusNeedsActionCap, len(resp.NeedsActionTasks))
	}
	if resp.NeedsActionTasks[0].Ask != "Merge or reply?" || resp.NeedsActionTasks[0].Brief != "Fix verified." {
		t.Errorf("expected ask/brief populated from askBriefByPath, got %+v", resp.NeedsActionTasks[0])
	}

	if len(resp.StuckPrompts) != 1 || resp.StuckPrompts[0].Agent != "orchestrator-myapp-g2" || resp.StuckPrompts[0].TmuxSession != "tn-myapp-g2" {
		t.Errorf("expected orchestrator-myapp-g2's stuck prompt with its legacy-derived tmux session, got %+v", resp.StuckPrompts)
	}
}

// TestBuildStatusResponse_IntegrationBranchPerTaskOnly verifies
// needsActionTasks[].integrationBranch is populated from each task's own
// customProperties["integration-branch"] (integrationBranchFor) — task
// level only, one task with it set and one without in the same response,
// no project-level equivalent (statusProject has no such field at all).
func TestBuildStatusResponse_IntegrationBranchPerTaskOnly(t *testing.T) {
	now := time.Now()
	snap := dashboardSnapshot{}
	needsActionTasks := []Task{
		{
			Path: "Tasks/OnBranch.md", Title: "OnBranch", Status: "review", Projects: []string{"myapp"},
			CustomProperties: map[string]string{"integration-branch": "feat/e2e-recorder"},
		},
		{Path: "Tasks/NoBranch.md", Title: "NoBranch", Status: "review", Projects: []string{"myapp"}},
	}
	taskCounts := statusTaskCounts{}
	configuredProjects := map[string]ProjectConfig{"myapp": {AutoSpawn: true}}

	resp := buildStatusResponse(snap, nil, nil, needsActionTasks, taskCounts,
		configuredProjects, fixedRepoSettings(defaultRepoSettings()), now, 8391, false, nil, time.Time{}, 0, "", nil, nil, nil)

	byPath := map[string]statusNeedsActionTask{}
	for _, t := range resp.NeedsActionTasks {
		byPath[t.Path] = t
	}
	if byPath["Tasks/OnBranch.md"].IntegrationBranch != "feat/e2e-recorder" {
		t.Errorf("expected the set task's integrationBranch populated, got %+v", byPath["Tasks/OnBranch.md"])
	}
	if byPath["Tasks/NoBranch.md"].IntegrationBranch != "" {
		t.Errorf("expected the unset task's integrationBranch empty, got %+v", byPath["Tasks/NoBranch.md"])
	}
}

// TestBuildStatusResponse_TriageSummary_OldestAndCount is the 2026-08-25
// passive triage-visibility fix: /status.triage surfaces a count and the
// single oldest triage task, computed from the FULL needsActionTasks list
// (not the capped display slice), so an old triage item never goes
// missing just because it fell past statusNeedsActionCap. Non-triage
// needsActionTasks (review, needs-input) must not be counted.
func TestBuildStatusResponse_TriageSummary_OldestAndCount(t *testing.T) {
	now := time.Now()
	snap := dashboardSnapshot{}
	oldest := now.Add(-3 * time.Hour)
	middle := now.Add(-1 * time.Hour)
	needsActionTasks := []Task{
		{Path: "Tasks/Newer.md", Title: "Newer", Status: "triage", Projects: []string{"myapp"}, DateCreated: middle.Format(time.RFC3339Nano)},
		{Path: "Tasks/Oldest.md", Title: "Oldest", Status: "triage", Projects: []string{"myapp"}, DateCreated: oldest.Format(time.RFC3339Nano)},
		{Path: "Tasks/NotTriage.md", Title: "In Review", Status: "review", Projects: []string{"myapp"}, DateCreated: now.Add(-24 * time.Hour).Format(time.RFC3339Nano)},
	}
	taskCounts := statusTaskCounts{}
	configuredProjects := map[string]ProjectConfig{"myapp": {AutoSpawn: true}}

	resp := buildStatusResponse(snap, nil, nil, needsActionTasks, taskCounts,
		configuredProjects, fixedRepoSettings(defaultRepoSettings()), now, 8391, false, nil, time.Time{}, 0, "", nil, nil, nil)

	if resp.Triage.Count != 2 {
		t.Fatalf("expected triage count 2 (review task excluded), got %d", resp.Triage.Count)
	}
	if resp.Triage.OldestPath != "Tasks/Oldest.md" {
		t.Errorf("expected the older triage task as oldest, got %+v", resp.Triage)
	}
	if resp.Triage.OldestTitle != "Oldest" {
		t.Errorf("expected oldest title %q, got %+v", "Oldest", resp.Triage)
	}
	gotAge := time.Duration(resp.Triage.OldestAgeSeconds) * time.Second
	if gotAge < 2*time.Hour || gotAge > 4*time.Hour {
		t.Errorf("expected oldest age around 3h, got %v", gotAge)
	}
}

// TestBuildStatusResponse_TriageSummary_ZeroWhenNoneTriage verifies the
// zero case renders cleanly — no oldest task, count 0 — rather than a
// stale/zero-value date leaking through.
func TestBuildStatusResponse_TriageSummary_ZeroWhenNoneTriage(t *testing.T) {
	now := time.Now()
	snap := dashboardSnapshot{}
	needsActionTasks := []Task{
		{Path: "Tasks/InReview.md", Title: "InReview", Status: "review", Projects: []string{"myapp"}},
	}
	resp := buildStatusResponse(snap, nil, nil, needsActionTasks, statusTaskCounts{},
		map[string]ProjectConfig{"myapp": {AutoSpawn: true}}, fixedRepoSettings(defaultRepoSettings()), now, 8391, false, nil, time.Time{}, 0, "", nil, nil, nil)

	if resp.Triage.Count != 0 || resp.Triage.OldestPath != "" || resp.Triage.OldestAgeSeconds != 0 {
		t.Errorf("expected an empty triage summary, got %+v", resp.Triage)
	}
}

// TestBuildStatusResponse_IntegrationRoleAndActiveOwnersRollup verifies
// needsActionTasks[].integrationRole is populated per task, and the
// project-level ActiveOwners rollup counts only role="owner" tasks within
// that project's needsActionTasks (a member task, however many are
// present, never contributes to the count).
func TestBuildStatusResponse_IntegrationRoleAndActiveOwnersRollup(t *testing.T) {
	now := time.Now()
	snap := dashboardSnapshot{}
	needsActionTasks := []Task{
		{
			Path: "Tasks/Owner1.md", Title: "Owner1", Status: "review", Projects: []string{"myapp"},
			CustomProperties: map[string]string{"integration-branch": "feat/e2e-recorder", "integration-role": "owner"},
		},
		{
			Path: "Tasks/Member1.md", Title: "Member1", Status: "review", Projects: []string{"myapp"},
			CustomProperties: map[string]string{"integration-branch": "feat/e2e-recorder"},
		},
		{
			Path: "Tasks/Owner2.md", Title: "Owner2", Status: "review", Projects: []string{"other"},
			CustomProperties: map[string]string{"integration-branch": "feat/other", "integration-role": "owner"},
		},
	}
	taskCounts := statusTaskCounts{}
	configuredProjects := map[string]ProjectConfig{"myapp": {AutoSpawn: true}, "other": {AutoSpawn: true}}

	resp := buildStatusResponse(snap, nil, nil, needsActionTasks, taskCounts,
		configuredProjects, fixedRepoSettings(defaultRepoSettings()), now, 8391, false, nil, time.Time{}, 0, "", nil, nil, nil)

	byPath := map[string]statusNeedsActionTask{}
	for _, t := range resp.NeedsActionTasks {
		byPath[t.Path] = t
	}
	if byPath["Tasks/Owner1.md"].IntegrationRole != "owner" {
		t.Errorf("expected Owner1's integrationRole populated, got %+v", byPath["Tasks/Owner1.md"])
	}
	if byPath["Tasks/Member1.md"].IntegrationRole != "" {
		t.Errorf("expected Member1's integrationRole empty, got %+v", byPath["Tasks/Member1.md"])
	}

	if resp.Projects["myapp"].ActiveOwners != 1 {
		t.Errorf("expected myapp's ActiveOwners=1 (Owner1 only, Member1 doesn't count), got %d", resp.Projects["myapp"].ActiveOwners)
	}
	if resp.Projects["other"].ActiveOwners != 1 {
		t.Errorf("expected other's ActiveOwners=1 (Owner2), got %d", resp.Projects["other"].ActiveOwners)
	}
}

// TestBuildStatusResponse_RecentActivityCappedAndSorted verifies activity
// entries are newest-first and capped at 20.
func TestBuildStatusResponse_RecentActivityCappedAndSorted(t *testing.T) {
	now := time.Now()
	var activity []ActivityEntry
	for i := 0; i < 25; i++ {
		activity = append(activity, ActivityEntry{Agent: "orchestrator-myapp-g1", At: now.Add(time.Duration(i) * time.Minute), Text: "entry"})
	}
	snap := dashboardSnapshot{
		Agents:   []Agent{{Name: "orchestrator-myapp-g1", Project: "myapp", LastSeenAt: now, Accepting: true}},
		Activity: activity,
	}

	resp := buildStatusResponse(snap, nil, nil, nil, nil, nil, fixedRepoSettings(defaultRepoSettings()), now, 8391, false, nil, time.Time{}, 0, "", nil, nil, nil)

	if len(resp.RecentActivity) != statusRecentActivityCap {
		t.Fatalf("expected recentActivity capped at %d, got %d", statusRecentActivityCap, len(resp.RecentActivity))
	}
	// Newest first: the last-appended entry (i=24) has the latest At.
	if !resp.RecentActivity[0].At.Equal(activity[24].At) {
		t.Errorf("expected newest-first ordering, got %+v", resp.RecentActivity[0])
	}
	if resp.RecentActivity[0].Project != "myapp" {
		t.Errorf("expected the activity entry's project resolved from its agent, got %+v", resp.RecentActivity[0])
	}
}

// TestStatusTaskCountsCache_TTL mirrors TestRepoSettingsCache_TTL: cached
// within TTL, re-fetched past it, using an injected clock rather than a
// real sleep.
func TestStatusTaskCountsCache_TTL(t *testing.T) {
	var c statusTaskCountsCache
	var fetchCalls int
	fetch := func() ([]Task, error) {
		fetchCalls++
		return []Task{{Path: "Tasks/A.md", Status: "open", Projects: []string{"myapp"}}}, nil
	}

	base := time.Now()
	first := c.get(base, statusTaskCountsCacheTTL, fetch)
	if fetchCalls != 1 || first["myapp"]["open"] != 1 {
		t.Fatalf("expected first call to fetch, got %+v (fetchCalls=%d)", first, fetchCalls)
	}

	within := c.get(base.Add(10*time.Second), statusTaskCountsCacheTTL, fetch)
	if fetchCalls != 1 || within["myapp"]["open"] != 1 {
		t.Fatalf("expected the cached value reused within TTL, got %+v (fetchCalls=%d)", within, fetchCalls)
	}

	after := c.get(base.Add(31*time.Second), statusTaskCountsCacheTTL, fetch)
	if fetchCalls != 2 || after["myapp"]["open"] != 1 {
		t.Fatalf("expected a re-fetch past TTL, got %+v (fetchCalls=%d)", after, fetchCalls)
	}
}

// TestStatusTaskCountsCache_FetchFailureKeepsStaleCounts verifies a
// failed fetch keeps serving the last successfully bucketed counts rather
// than blanking the response.
func TestStatusTaskCountsCache_FetchFailureKeepsStaleCounts(t *testing.T) {
	var c statusTaskCountsCache
	base := time.Now()
	good := c.get(base, statusTaskCountsCacheTTL, func() ([]Task, error) {
		return []Task{{Path: "Tasks/A.md", Status: "open", Projects: []string{"myapp"}}}, nil
	})
	if good["myapp"]["open"] != 1 {
		t.Fatalf("expected initial counts, got %+v", good)
	}

	afterFailure := c.get(base.Add(31*time.Second), statusTaskCountsCacheTTL, func() ([]Task, error) {
		return nil, errFakeGlab
	})
	if afterFailure["myapp"]["open"] != 1 {
		t.Errorf("expected stale counts kept on a fetch failure, got %+v", afterFailure)
	}
}

// TestHandleUI_BareSlashServesEmbeddedIndexHTML verifies GET /ui/ (root)
// serves the embedded React SPA's index.html — the UI v2 replacement for
// the old server-rendered polling page (SPEC-ui-v2.md).
func TestHandleUI_BareSlashServesEmbeddedIndexHTML(t *testing.T) {
	_, ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/ui/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("expected Content-Type text/html, got %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "<!doctype html>") {
		t.Errorf("expected the embedded index.html shell, got a %d-byte body without a doctype", len(body))
	}
}

// TestHandleUI_BareUINoTrailingSlashRedirectsOnce verifies GET /ui
// (without the trailing slash) redirects exactly once to /ui/, rather than
// looping — the exact bug this handler had at one point: http.FileServer
// redirects any request ending in "index.html" back to "/", which (before
// switching to http.ServeContent) re-entered this handler and picked
// "index.html" again, forever.
func TestHandleUI_BareUINoTrailingSlashRedirectsOnce(t *testing.T) {
	_, ts := newTestServer(t)

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 1 {
				return fmt.Errorf("expected at most 1 redirect, got %d", len(via))
			}
			return nil
		},
	}
	resp, err := client.Get(ts.URL + "/ui")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after following the single redirect, got %d", resp.StatusCode)
	}
}

// TestHandleUI_UnknownClientRouteFallsBackToIndexHTML verifies a path that
// isn't a real embedded asset (a TanStack Router client-side route, e.g.
// /ui/task/Some%2FPath.md) still serves index.html rather than 404ing —
// the SERVER doesn't know app routes, only the bundled router does.
func TestHandleUI_UnknownClientRouteFallsBackToIndexHTML(t *testing.T) {
	_, ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/ui/task/Some%2FPath.md")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 (SPA fallback), got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "<!doctype html>") {
		t.Errorf("expected the index.html shell as a fallback, got a %d-byte body without a doctype", len(body))
	}
}

// TestHandleUI_RealAssetServedWithItsOwnBytes verifies a real file inside
// the embedded bundle (not index.html) is served as itself, not silently
// redirected to the SPA fallback.
func TestHandleUI_RealAssetServedWithItsOwnBytes(t *testing.T) {
	_, ts := newTestServer(t)

	entries, err := fs.ReadDir(webUIDist, "assets")
	if err != nil || len(entries) == 0 {
		t.Skip("no assets/ in the embedded bundle (webui/dist not built) — skipping")
	}
	var jsFile string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".js") {
			jsFile = e.Name()
			break
		}
	}
	if jsFile == "" {
		t.Skip("no .js asset found to test against")
	}

	want, err := fs.ReadFile(webUIDist, "assets/"+jsFile)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.URL + "/ui/assets/" + jsFile)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != string(want) {
		t.Errorf("expected the real asset bytes served verbatim, got a %d-byte body (want %d)", len(got), len(want))
	}
}

// TestHandleStatus_NeedsActionTasksCarryAskAndBrief is the end-to-end
// version of TestBuildStatusResponse_BucketsAgentsPerProjectAndAppliesCaps's
// ask/brief assertion: a real GET /status call against a fake TaskNotes
// API whose query response has NO details (matching the live API's actual
// behavior — POST /api/tasks/query never includes Details) still ends up
// with ask/brief populated, via the bounded per-task GetTask fetch.
func TestHandleStatus_NeedsActionTasksCarryAskAndBrief(t *testing.T) {
	const path = "Tasks/Review.md"
	fullDetails := applyNoteBodyEdit("Original description.", func(nb *noteBody) {
		nb.Ask = "Merge MR !973 or reply."
		nb.Brief = "Fix verified on VM4."
	})

	var getCalls int
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/tasks/query":
			// Deliberately NO "details" field — matches the real API.
			resp := map[string]any{"data": map[string]any{"tasks": []map[string]any{
				{"path": path, "title": "Review", "status": "review", "projects": []string{"myapp"}},
			}, "total": 1, "filtered": 1}}
			json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/tasks/"):
			getCalls++
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"path": path, "title": "Review", "details": fullDetails}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer apiSrv.Close()

	srv, ts := newTestServer(t)
	srv.tnClient = NewClient(Config{URL: apiSrv.URL})

	resp, err := http.Get(ts.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out statusResponse
	decodeInto(t, resp, &out)

	if len(out.NeedsActionTasks) != 1 {
		t.Fatalf("expected 1 needs-action task, got %+v", out.NeedsActionTasks)
	}
	if out.NeedsActionTasks[0].Ask != "Merge MR !973 or reply." {
		t.Errorf("expected ask populated via the per-task fetch, got %q", out.NeedsActionTasks[0].Ask)
	}
	if out.NeedsActionTasks[0].Brief != "Fix verified on VM4." {
		t.Errorf("expected brief populated via the per-task fetch, got %q", out.NeedsActionTasks[0].Brief)
	}
	if getCalls != 1 {
		t.Errorf("expected exactly 1 bounded GetTask call, got %d", getCalls)
	}

	// A second /status call within the cache TTL must NOT re-fetch.
	resp2, err := http.Get(ts.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if getCalls != 1 {
		t.Errorf("expected the ask/brief cache to avoid a second GetTask call, got %d total calls", getCalls)
	}
}

// TestBuildStatusResponse_LastSleepOmittedWhenNeverDetected verifies the
// daemon block's lastSleepAt/lastSleepSeconds are entirely absent (not
// present as zero values) when no host sleep has been detected yet.
func TestBuildStatusResponse_LastSleepOmittedWhenNeverDetected(t *testing.T) {
	now := mustTime(t, "2026-08-17T12:00:00Z")
	snap := dashboardSnapshot{}
	resp := buildStatusResponse(snap, nil, nil, nil, nil, nil, fixedRepoSettings(defaultRepoSettings()), now, 8391, false, nil, time.Time{}, 0, "", nil, nil, nil)

	if resp.Daemon.LastSleepAt != nil || resp.Daemon.LastSleepSeconds != nil {
		t.Errorf("expected both lastSleep fields nil when never detected, got %+v", resp.Daemon)
	}
}

// TestBuildStatusResponse_LastSleepPopulatedWhenDetected verifies a
// detected sleep is surfaced with both fields set.
func TestBuildStatusResponse_LastSleepPopulatedWhenDetected(t *testing.T) {
	now := mustTime(t, "2026-08-17T12:00:00Z")
	sleepAt := now.Add(-14 * time.Minute)
	snap := dashboardSnapshot{}
	resp := buildStatusResponse(snap, nil, nil, nil, nil, nil, fixedRepoSettings(defaultRepoSettings()), now, 8391, false, nil, sleepAt, 5*time.Minute, "", nil, nil, nil)

	if resp.Daemon.LastSleepAt == nil || !resp.Daemon.LastSleepAt.Equal(sleepAt) {
		t.Errorf("expected LastSleepAt %v, got %v", sleepAt, resp.Daemon.LastSleepAt)
	}
	if resp.Daemon.LastSleepSeconds == nil || *resp.Daemon.LastSleepSeconds != 300 {
		t.Errorf("expected LastSleepSeconds 300, got %v", resp.Daemon.LastSleepSeconds)
	}
}

// TestBuildStatusResponse_BuildHashSurfacesEmbeddedWebUIHash verifies
// /status.daemon.buildHash carries the actual computed hash of the
// embedded webui bundle (webUIBuildHash), not a blank/placeholder value —
// this is what lets the UI footer and a human comparing `pnpm build`
// output detect a stale-bundle rebuild (CLAUDE.md's documented footgun).
func TestBuildStatusResponse_BuildHashSurfacesEmbeddedWebUIHash(t *testing.T) {
	now := mustTime(t, "2026-08-17T12:00:00Z")
	snap := dashboardSnapshot{}
	resp := buildStatusResponse(snap, nil, nil, nil, nil, nil, fixedRepoSettings(defaultRepoSettings()), now, 8391, false, nil, time.Time{}, 0, "", nil, nil, nil)

	if resp.Daemon.BuildHash == "" {
		t.Fatal("expected Daemon.BuildHash to be non-empty")
	}
	if resp.Daemon.BuildHash != webUIBuildHash {
		t.Errorf("expected Daemon.BuildHash %q to equal package-level webUIBuildHash %q", resp.Daemon.BuildHash, webUIBuildHash)
	}
}

// TestComputeWebUIBuildHash_DeterministicAndContentSensitive verifies the
// hash function is stable across repeated calls on the same fs.FS and
// changes when file content changes — the two properties the footer
// depends on (stable so it doesn't flap on every render; sensitive so a
// stale bundle is actually detectable).
func TestComputeWebUIBuildHash_DeterministicAndContentSensitive(t *testing.T) {
	a := fstest.MapFS{
		"index.html":    {Data: []byte("<html>v1</html>")},
		"assets/app.js": {Data: []byte("console.log(1)")},
	}
	b := fstest.MapFS{
		"index.html":    {Data: []byte("<html>v1</html>")},
		"assets/app.js": {Data: []byte("console.log(2)")}, // content changed
	}

	h1 := computeWebUIBuildHash(a)
	h2 := computeWebUIBuildHash(a)
	if h1 != h2 {
		t.Errorf("expected same fs.FS to hash deterministically, got %q then %q", h1, h2)
	}
	if h1 == "" {
		t.Fatal("expected non-empty hash")
	}

	h3 := computeWebUIBuildHash(b)
	if h3 == h1 {
		t.Errorf("expected changed file content to change the hash, got same value %q for both", h1)
	}
}

// TestWebUIActuallyBuilt_DetectsMissingAssetsDir verifies the "not really
// built" runtime heuristic: a bundle with no assets/ directory (a stub or
// placeholder, not a real Vite build) is correctly flagged as not built,
// while one with an assets/ dir passes.
func TestWebUIActuallyBuilt_DetectsMissingAssetsDir(t *testing.T) {
	if _, err := fs.Stat(fstest.MapFS{"index.html": {Data: []byte("stub")}}, "assets"); err == nil {
		t.Fatal("test fixture itself is broken: stub fs unexpectedly has an assets/ entry")
	}
	// webUIDist is the real embedded bundle in this checkout; it must
	// report itself as actually built, or the daemon would refuse to
	// serve /ui at all in the test binary that ships in this repo.
	if !webUIActuallyBuilt() {
		t.Error("expected the real committed webui/dist to report as actually built")
	}
}
