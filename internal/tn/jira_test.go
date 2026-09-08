package tn

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newJiraTestServer is newTestServer's shape but with caller-supplied
// per-project config, so tests can exercise a configured JiraStatusMap —
// newTestServer's empty Projects map means every jira_test.go test that
// predates this one exercises the (unaffected) fallback path automatically.
func newJiraTestServer(t *testing.T, projects map[string]ProjectConfig) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	cfg := ServeConfig{Port: 0, Projects: projects}
	srv := newServer(filepath.Join(dir, "state.json"), cfg, func(project, cwd string, env map[string]string) error { return nil })
	ts := httptest.NewServer(newMux(srv))
	t.Cleanup(ts.Close)
	return srv, ts
}

// TestSplitJiraKeys covers the comma-separated parsing: trims whitespace,
// drops empties, handles the empty-string input.
func TestSplitJiraKeys(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"single", "PROJ-101", []string{"PROJ-101"}},
		{"multi no spaces", "CX-1,CX-2", []string{"CX-1", "CX-2"}},
		{"multi with spaces", "CX-1, CX-2 , CX-3", []string{"CX-1", "CX-2", "CX-3"}},
		{"trailing comma dropped", "CX-1,", []string{"CX-1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := splitJiraKeys(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("splitJiraKeys(%q) = %v, want %v", c.in, got, c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Errorf("index %d: got %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

// TestJiraStatusesOfInterest verifies the status->category mapping: only
// in-progress/review/done produce a target; anything else (open, triage,
// needs-input) is deliberately not a Jira-workflow signal.
func TestJiraStatusesOfInterest(t *testing.T) {
	want := map[string]string{
		"in-progress": "indeterminate",
		"review":      "indeterminate",
		"done":        "done",
	}
	for status, category := range want {
		target, ok := jiraStatusesOfInterest[status]
		if !ok || target.category != category {
			t.Errorf("status %q: got %+v ok=%v, want category %q", status, target, ok, category)
		}
	}
	for _, status := range []string{"open", "triage", "needs-input", ""} {
		if _, ok := jiraStatusesOfInterest[status]; ok {
			t.Errorf("expected status %q to produce no Jira target", status)
		}
	}
}

// jiraWebhookPost posts a task.updated webhook for path/status, optionally
// carrying jira/jira-nc/mr custom properties.
func jiraWebhookPost(t *testing.T, tsURL, path, status string, customProps map[string]any) {
	t.Helper()
	task := map[string]any{
		"path": path, "title": path, "status": status, "projects": []string{"proj-jira"},
	}
	if len(customProps) > 0 {
		task["customProperties"] = customProps
	}
	payload := map[string]any{"event": "task.updated", "data": map[string]any{"task": task}}
	resp := mustPost(t, tsURL+"/webhooks/tasknotes", payload)
	resp.Body.Close()
}

// TestJiraTransition_RequestOncePerStatusChange verifies: a task's
// first-ever observation only seeds LastKnownStatus (no request — matches
// the other migration-style dedup maps, so shipping this doesn't flood
// every existing jira-tagged task the moment it's enabled); a genuine
// status change into a status of interest produces exactly one request;
// re-observing the SAME status produces no additional request.
func TestJiraTransition_RequestOncePerStatusChange(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "agent-jira", "proj-jira")
	const path = "Tasks/Jira.md"

	jiraWebhookPost(t, ts.URL, path, "open", map[string]any{"jira": "CX-100"})
	if msgs := queuedMessagesTo(srv, "agent-jira"); len(msgs) != 0 {
		t.Fatalf("expected no request on the first-ever observation (seed only), got %+v", msgs)
	}

	jiraWebhookPost(t, ts.URL, path, "in-progress", map[string]any{"jira": "CX-100"})
	msgs := queuedMessagesTo(srv, "agent-jira")
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 request after the status change, got %+v", msgs)
	}
	if !strings.Contains(msgs[0].Text, "CX-100") || !strings.Contains(msgs[0].Text, "In Progress") {
		t.Errorf("unexpected request text: %q", msgs[0].Text)
	}
	if !strings.Contains(msgs[0].Text, "mcp__jira__jira_transitions") || !strings.Contains(msgs[0].Text, "mcp__jira__jira_transition") {
		t.Errorf("expected the request to name the MCP tools to use, got %q", msgs[0].Text)
	}
	if !strings.Contains(msgs[0].Text, "tn ask") {
		t.Errorf("expected fallback guidance (tn ask) for when no matching transition exists, got %q", msgs[0].Text)
	}

	jiraWebhookPost(t, ts.URL, path, "in-progress", map[string]any{"jira": "CX-100"})
	if msgs := queuedMessagesTo(srv, "agent-jira"); len(msgs) != 1 {
		t.Errorf("expected still exactly 1 request after a same-status re-observation, got %d: %+v", len(msgs), msgs)
	}
}

// TestJiraTransition_FirstObservationAlreadyInStatusOfInterestIsSeedOnly
// is the regression guard for the "first-ever observation only seeds,
// never requests" contract: a task observed for the very FIRST time while
// ALREADY in a status of interest (e.g. a task created directly as
// in-progress, or the very first reconciler pass after this feature is
// enabled on an existing in-progress task) must not fire a request — there
// is no "previous status" to have genuinely changed FROM. Caught by the
// sanity check for this fix: removing the hadPrior guard made this exact
// case wrongly fire.
func TestJiraTransition_FirstObservationAlreadyInStatusOfInterestIsSeedOnly(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "agent-seed", "proj-jira")

	jiraWebhookPost(t, ts.URL, "Tasks/Seed.md", "in-progress", map[string]any{"jira": "CX-999"})

	if msgs := queuedMessagesTo(srv, "agent-seed"); len(msgs) != 0 {
		t.Fatalf("expected no request on a first-ever observation already in-progress (seed only), got %+v", msgs)
	}
}

// TestJiraTransition_ReArmsOnStatusChange verifies a task that leaves and
// later returns to a status of interest is requested again — the
// "re-arm when the task's status changes again" requirement.
func TestJiraTransition_ReArmsOnStatusChange(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "agent-rearm", "proj-jira")
	const path = "Tasks/Rearm.md"

	jiraWebhookPost(t, ts.URL, path, "open", map[string]any{"jira": "CX-200"})
	jiraWebhookPost(t, ts.URL, path, "in-progress", map[string]any{"jira": "CX-200"})
	if msgs := queuedMessagesTo(srv, "agent-rearm"); len(msgs) != 1 {
		t.Fatalf("expected 1 request after the first transition into in-progress, got %+v", msgs)
	}

	jiraWebhookPost(t, ts.URL, path, "needs-input", map[string]any{"jira": "CX-200"}) // leaves the status of interest
	jiraWebhookPost(t, ts.URL, path, "in-progress", map[string]any{"jira": "CX-200"}) // returns: a new episode

	if msgs := queuedMessagesTo(srv, "agent-rearm"); len(msgs) != 2 {
		t.Fatalf("expected a second request after returning to in-progress, got %d: %+v", len(msgs), msgs)
	}
}

// TestJiraTransition_NCKeyNeverTransitionsOnlyComment verifies jira-nc (a
// regulated nonconformity record) never produces a transition request —
// only a comment-only request that explicitly says not to change status,
// and includes the MR link when the task has one.
func TestJiraTransition_NCKeyNeverTransitionsOnlyComment(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "agent-nc", "proj-jira")
	const path = "Tasks/NC.md"
	props := map[string]any{"jira-nc": "NC-1", "mr": "https://gitlab.example.com/mr/1"}

	jiraWebhookPost(t, ts.URL, path, "open", props)
	jiraWebhookPost(t, ts.URL, path, "review", props)

	msgs := queuedMessagesTo(srv, "agent-nc")
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 request, got %+v", msgs)
	}
	if !strings.HasPrefix(msgs[0].Text, "Jira NC:") {
		t.Errorf("expected an NC-prefixed comment-only request, got %q", msgs[0].Text)
	}
	if strings.Contains(msgs[0].Text, "jira_transition") {
		t.Errorf("expected NO transition instruction for an NC key, got %q", msgs[0].Text)
	}
	if !strings.Contains(msgs[0].Text, "do NOT change its status") || !strings.Contains(msgs[0].Text, "nonconformity procedure") {
		t.Errorf("expected explicit no-status-change guidance and nonconformity procedure reference, got %q", msgs[0].Text)
	}
	if !strings.Contains(msgs[0].Text, "https://gitlab.example.com/mr/1") {
		t.Errorf("expected the MR link included, got %q", msgs[0].Text)
	}
}

// TestJiraTransition_MultiKeyProducesOneRequestPerKey verifies a
// comma-separated `jira` value produces one independent request per key.
func TestJiraTransition_MultiKeyProducesOneRequestPerKey(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "agent-multi", "proj-jira")
	const path = "Tasks/Multi.md"
	props := map[string]any{"jira": "CX-1, CX-2 ,CX-3"}

	jiraWebhookPost(t, ts.URL, path, "open", props)
	jiraWebhookPost(t, ts.URL, path, "done", props)

	msgs := queuedMessagesTo(srv, "agent-multi")
	if len(msgs) != 3 {
		t.Fatalf("expected exactly 3 requests (one per key), got %d: %+v", len(msgs), msgs)
	}
	seen := map[string]bool{}
	for _, m := range msgs {
		for _, key := range []string{"CX-1", "CX-2", "CX-3"} {
			if strings.Contains(m.Text, key) {
				seen[key] = true
			}
		}
	}
	for _, key := range []string{"CX-1", "CX-2", "CX-3"} {
		if !seen[key] {
			t.Errorf("expected a request mentioning %s, got %+v", key, msgs)
		}
	}
}

// TestJiraTransition_NoJiraFieldNoMessages verifies a task with neither
// jira nor jira-nc set never produces a request, regardless of how many
// times its status changes.
func TestJiraTransition_NoJiraFieldNoMessages(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "agent-none", "proj-jira")
	const path = "Tasks/None.md"

	jiraWebhookPost(t, ts.URL, path, "open", nil)
	jiraWebhookPost(t, ts.URL, path, "in-progress", nil)
	jiraWebhookPost(t, ts.URL, path, "review", nil)
	jiraWebhookPost(t, ts.URL, path, "done", nil)

	if msgs := queuedMessagesTo(srv, "agent-none"); len(msgs) != 0 {
		t.Errorf("expected zero messages with no jira/jira-nc field set, got %+v", msgs)
	}
}

// TestJiraTransition_UninterestingStatusNoRequest verifies a status change
// into something OTHER than in-progress/review/done (e.g. back to open, or
// triage) produces no request even with a jira field set.
func TestJiraTransition_UninterestingStatusNoRequest(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "agent-uninteresting", "proj-jira")
	const path = "Tasks/Uninteresting.md"
	props := map[string]any{"jira": "CX-300"}

	jiraWebhookPost(t, ts.URL, path, "open", props)
	jiraWebhookPost(t, ts.URL, path, "triage", props)

	if msgs := queuedMessagesTo(srv, "agent-uninteresting"); len(msgs) != 0 {
		t.Errorf("expected zero requests for a transition into a status with no Jira target, got %+v", msgs)
	}
}

// TestJiraTransition_RequestTextExcludesRejectionTransitions verifies the
// !1067 live-test finding: matching purely on toStatus category is unsafe
// because a rejection/withdrawal transition ("Won't do"/"Zurückgewiesen")
// shares the done category with genuine completion, and a "Blocked"-style
// status can share indeterminate with real in-progress work. The request
// text must name these traps explicitly and instruct refuse-and-tn-ask
// over picking the nearest same-category option.
func TestJiraTransition_RequestTextExcludesRejectionTransitions(t *testing.T) {
	srv, ts := newTestServer(t)
	register(t, ts, "agent-reject", "proj-jira")
	const path = "Tasks/Reject.md"
	props := map[string]any{"jira": "CX-500"}

	jiraWebhookPost(t, ts.URL, path, "open", props)
	jiraWebhookPost(t, ts.URL, path, "done", props)

	msgs := queuedMessagesTo(srv, "agent-reject")
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 request, got %+v", msgs)
	}
	text := msgs[0].Text
	for _, want := range []string{"Won't do", "Zurückgewiesen", "Blocked", "refuse", "tn ask"} {
		if !strings.Contains(text, want) {
			t.Errorf("expected request text to mention %q, got %q", want, text)
		}
	}
}

// TestJiraTransition_RoutesToAcceptingAgentNotDrainingOwner verifies the
// other !1067 live-test finding: a Jira request is a fresh action about a
// task that has already reached its target status, not a follow-up on
// work in flight, so it must route to an alive ACCEPTING agent rather than
// a draining task owner (alive, but not picking up new work — which left a
// real request sitting unacked during the live test). Asserted alongside a
// same-task @claude reply, which MUST still reach the draining owner, so
// this distinction can't silently regress.
func TestJiraTransition_RoutesToAcceptingAgentNotDrainingOwner(t *testing.T) {
	srv, ts := newTestServer(t)
	const path = "Tasks/Owned.md"

	register(t, ts, "orchestrator-myapp-g1", "myapp")
	ackedAt := time.Now().Add(-time.Minute)
	srv.mu.Lock()
	srv.state.Messages = append(srv.state.Messages, &Message{
		ID: "assign-owned", To: "orchestrator-myapp-g1", TaskPath: path,
		Text: "Task assigned: Owned (Tasks/Owned.md)", Status: "acked",
		CreatedAt: ackedAt, AckedAt: &ackedAt,
	})
	srv.mu.Unlock()
	resp := mustPost(t, ts.URL+"/agents/drain", map[string]any{"name": "orchestrator-myapp-g1"})
	resp.Body.Close()

	register(t, ts, "orchestrator-myapp-g2", "myapp")

	post := func(status, details string, customProps map[string]any) {
		task := map[string]any{
			"path": path, "title": "Owned", "status": status,
			"projects": []string{"myapp"},
		}
		if details != "" {
			task["details"] = details
		}
		if len(customProps) > 0 {
			task["customProperties"] = customProps
		}
		payload := map[string]any{"event": "task.updated", "data": map[string]any{"task": task}}
		r := mustPost(t, ts.URL+"/webhooks/tasknotes", payload)
		r.Body.Close()
	}

	// Seed observation (no request), then a genuine status change into
	// "done" fires the Jira request.
	post("review", "", map[string]any{"jira": "CX-600"})
	post("done", "", map[string]any{"jira": "CX-600"})

	for _, m := range queuedMessagesTo(srv, "orchestrator-myapp-g1") {
		if strings.HasPrefix(m.Text, "Jira:") {
			t.Fatalf("Jira request must not route to the draining owner, got %+v", m)
		}
	}
	found := false
	for _, m := range queuedMessagesTo(srv, "orchestrator-myapp-g2") {
		if strings.HasPrefix(m.Text, "Jira:") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the Jira request routed to the accepting agent orchestrator-myapp-g2")
	}

	// Same task, same status (no new Jira request — already deduped), but
	// an @claude reply: must still reach the draining owner directly.
	post("done", "@claude: still here, any update?", map[string]any{"jira": "CX-600"})
	replyFound := false
	for _, m := range queuedMessagesTo(srv, "orchestrator-myapp-g1") {
		if m.Text == "still here, any update?" {
			replyFound = true
		}
	}
	if !replyFound {
		t.Fatalf("expected the @claude reply to still reach the draining owner (ownership routing must not regress)")
	}
}

// TestJiraTransition_ConfiguredStatusMapProducesNamedTarget verifies the
// per-project jiraStatusMap correction: when configured, the request names
// a specific Jira STATUS (e.g. "Merged"), not merely a category — CX Bug's
// board-"done" status is MERGED, several hops before the category-done
// status Fertig (reached only after acceptance testing/release), so a
// category-only request was wrong even when "matched correctly".
func TestJiraTransition_ConfiguredStatusMapProducesNamedTarget(t *testing.T) {
	projects := map[string]ProjectConfig{
		"proj-jira": {JiraStatusMap: map[string]string{
			"in-progress": "In Development",
			"review":      "Code Review",
			"done":        "Merged",
		}},
	}
	cases := []struct {
		status string
		want   string
	}{
		{"in-progress", "In Development"},
		{"review", "Code Review"},
		{"done", "Merged"},
	}
	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			srv, ts := newJiraTestServer(t, projects)
			register(t, ts, "agent-mapped-"+c.status, "proj-jira")
			path := "Tasks/Mapped-" + c.status + ".md"
			props := map[string]any{"jira": "CX-700"}

			jiraWebhookPost(t, ts.URL, path, "open", props)
			jiraWebhookPost(t, ts.URL, path, c.status, props)

			msgs := queuedMessagesTo(srv, "agent-mapped-"+c.status)
			if len(msgs) != 1 {
				t.Fatalf("expected exactly 1 request, got %+v", msgs)
			}
			text := msgs[0].Text
			if !strings.Contains(text, "Target Jira status: \""+c.want+"\"") {
				t.Errorf("expected the named target status %q in the request, got %q", c.want, text)
			}
			if strings.Contains(text, "toStatus category matches") {
				t.Errorf("expected the mapped branch (not the category-only fallback wording), got %q", text)
			}
		})
	}
}

// TestJiraTransition_NoConfiguredMapFallsBackToCategoryText verifies a
// project with no jiraStatusMap entry for the task's status (including no
// map configured at all) keeps producing the category-only request — the
// mapped feature must never silently apply another project's status names.
func TestJiraTransition_NoConfiguredMapFallsBackToCategoryText(t *testing.T) {
	// No projects config at all.
	srv, ts := newJiraTestServer(t, nil)
	register(t, ts, "agent-nomap", "proj-jira")
	const path = "Tasks/NoMap.md"
	props := map[string]any{"jira": "CX-701"}

	jiraWebhookPost(t, ts.URL, path, "open", props)
	jiraWebhookPost(t, ts.URL, path, "done", props)

	msgs := queuedMessagesTo(srv, "agent-nomap")
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 request, got %+v", msgs)
	}
	text := msgs[0].Text
	if !strings.Contains(text, "toStatus category matches") {
		t.Errorf("expected the category-only fallback wording with no configured map, got %q", text)
	}
	if strings.Contains(text, "Target Jira status:") {
		t.Errorf("expected NO named-target wording with no configured map, got %q", text)
	}
}

// TestJiraTransition_ConfiguredMapWinsOverPartialMap verifies a project
// whose map covers SOME statuses but not the one this task just moved
// into falls back to category text for that status specifically — the
// per-status fallback, not an all-or-nothing per-project switch.
func TestJiraTransition_ConfiguredMapWinsOverPartialMap(t *testing.T) {
	projects := map[string]ProjectConfig{
		"proj-jira": {JiraStatusMap: map[string]string{
			"done": "Merged",
			// deliberately no "in-progress" entry
		}},
	}
	srv, ts := newJiraTestServer(t, projects)
	register(t, ts, "agent-partial", "proj-jira")
	const path = "Tasks/Partial.md"
	props := map[string]any{"jira": "CX-702"}

	jiraWebhookPost(t, ts.URL, path, "open", props)
	jiraWebhookPost(t, ts.URL, path, "in-progress", props)

	msgs := queuedMessagesTo(srv, "agent-partial")
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 request, got %+v", msgs)
	}
	if !strings.Contains(msgs[0].Text, "toStatus category matches") {
		t.Errorf("expected in-progress (unmapped) to fall back to category text, got %q", msgs[0].Text)
	}
}

// TestJiraTransition_NamedTargetTextIncludesNeverAutomateList verifies the
// mapped-branch request text names every never-automate status from the
// CX Bug graph (Reference/Jira workflows.md) — the QA/release/rejection
// states multi-hop walking must never land on.
func TestJiraTransition_NamedTargetTextIncludesNeverAutomateList(t *testing.T) {
	projects := map[string]ProjectConfig{
		"proj-jira": {JiraStatusMap: map[string]string{"done": "Merged"}},
	}
	srv, ts := newJiraTestServer(t, projects)
	register(t, ts, "agent-never", "proj-jira")
	const path = "Tasks/Never.md"
	props := map[string]any{"jira": "CX-703"}

	jiraWebhookPost(t, ts.URL, path, "open", props)
	jiraWebhookPost(t, ts.URL, path, "done", props)

	msgs := queuedMessagesTo(srv, "agent-never")
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 request, got %+v", msgs)
	}
	text := msgs[0].Text
	for _, want := range []string{
		"Acceptance Test", "In AT", "Ready for Release", "Fertig", "Retired", "Zurückgewiesen", "Duplicate",
		"multi-hop is expected", "tn ask",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("expected request text to mention %q, got %q", want, text)
		}
	}
}
