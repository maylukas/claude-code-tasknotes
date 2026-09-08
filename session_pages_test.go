package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestSanitizeSessionFilename covers the "/" and non-[A-Za-z0-9._ -]
// replacement rule.
func TestSanitizeSessionFilename(t *testing.T) {
	cases := []struct{ in, want string }{
		{"orchestrator-myapp", "orchestrator-myapp"},
		{"claude/foo-e2e", "claude-foo-e2e"},
		{"weird:name*here?", "weird-name-here-"},
		{"a b_c.d-e", "a b_c.d-e"}, // space, underscore(not allowed!), dot, hyphen
	}
	for _, c := range cases {
		if got := sanitizeSessionFilename(c.in); got != c.want {
			t.Errorf("sanitizeSessionFilename(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestFormatEventTimestamp covers the today-vs-not-today formatting rule.
func TestFormatEventTimestamp(t *testing.T) {
	now := mustTime(t, "2026-08-17T14:00:00Z")
	today := mustTime(t, "2026-08-17T09:30:00Z")
	yesterday := mustTime(t, "2026-08-16T09:30:00Z")

	if got := formatEventTimestamp(today, now); got != "09:30" {
		t.Errorf("today: got %q, want %q", got, "09:30")
	}
	if got := formatEventTimestamp(yesterday, now); got != "2026-08-16 09:30" {
		t.Errorf("not today: got %q, want %q", got, "2026-08-16 09:30")
	}
}

// TestBuildSessionEvents_MergedOrderingAndMapping verifies the merged feed:
// activity entries plus message events (received/acked/sent), newest first,
// with the correct emoji-prefixed formatting for each event kind.
func TestBuildSessionEvents_MergedOrderingAndMapping(t *testing.T) {
	now := mustTime(t, "2026-08-17T12:00:00Z")

	activity := []ActivityEntry{
		{Agent: "orchestrator-myapp", At: now.Add(-50 * time.Minute), Text: "picked up task X"},
		{Agent: "someone-else", At: now.Add(-10 * time.Minute), Text: "not this agent"},
	}
	messages := []Message{
		// received (queued): oldest
		{ID: "m-recv", To: "orchestrator-myapp", From: "tester", Text: "Task assigned: X (Tasks/X.md)",
			TaskPath: "Tasks/X.md", Status: "queued", CreatedAt: now.Add(-40 * time.Minute)},
		// acked: newer, response present
		{ID: "m-ack", To: "orchestrator-myapp", From: "tester", Text: "please check logs",
			TaskPath: "Tasks/Y.md", Status: "acked", CreatedAt: now.Add(-30 * time.Minute),
			AckedAt: timePtr(now.Add(-20 * time.Minute)), Response: "checked, all clear"},
		// sent BY this agent: newest
		{ID: "m-sent", To: "claude/foo-e2e", From: "orchestrator-myapp", Text: "please review",
			TaskPath: "Tasks/Z.md", Status: "queued", CreatedAt: now.Add(-5 * time.Minute)},
		// belongs to a different agent entirely: must not appear
		{ID: "m-other", To: "someone-else", From: "someone-else-too", Text: "irrelevant",
			Status: "queued", CreatedAt: now.Add(-1 * time.Minute)},
	}

	events := buildSessionEvents("orchestrator-myapp", messages, activity)

	if len(events) != 4 {
		t.Fatalf("expected 4 events for orchestrator-myapp (3 message events + 1 activity entry), got %d: %+v", len(events), events)
	}

	// Newest first: m-sent (-5m), m-ack (acked at -20m), m-recv (-40m), then
	// the activity entry (-50m) last.
	wantOrder := []string{
		"📤 sent to claude/foo-e2e: please review",
		"✅ acked: please check logs — response: checked, all clear",
		"📨 received: Task assigned: X (Tasks/X.md)",
		"📝 picked up task X",
	}
	for i, want := range wantOrder {
		if events[i].Text != want {
			t.Errorf("event[%d] = %q, want %q", i, events[i].Text, want)
		}
	}

	if events[2].Wikilink != "[[Tasks/X]]" {
		t.Errorf("expected received event wikilink, got %q", events[2].Wikilink)
	}
	if events[1].Wikilink != "[[Tasks/Y]]" {
		t.Errorf("expected acked event wikilink, got %q", events[1].Wikilink)
	}
	if events[0].Wikilink != "[[Tasks/Z]]" {
		t.Errorf("expected sent event wikilink, got %q", events[0].Wikilink)
	}
}

// TestBuildSessionEvents_IncludesActivityEntry verifies a plain `tn log`
// activity entry appears in the merged feed with its own prefix.
func TestBuildSessionEvents_IncludesActivityEntry(t *testing.T) {
	activity := []ActivityEntry{
		{Agent: "orchestrator-myapp", At: time.Now(), Text: "subagent spawned"},
	}
	events := buildSessionEvents("orchestrator-myapp", nil, activity)
	if len(events) != 1 || events[0].Text != "📝 subagent spawned" {
		t.Fatalf("expected 1 activity event, got %+v", events)
	}
}

// TestBuildSessionEvents_CapAt200 verifies the merged feed is capped at 200
// entries, keeping the newest.
func TestBuildSessionEvents_CapAt200(t *testing.T) {
	now := time.Now()
	var activity []ActivityEntry
	for i := 0; i < 250; i++ {
		activity = append(activity, ActivityEntry{
			Agent: "orchestrator-myapp",
			At:    now.Add(-time.Duration(i) * time.Minute),
			Text:  "entry",
		})
	}
	events := buildSessionEvents("orchestrator-myapp", nil, activity)
	if len(events) != 200 {
		t.Fatalf("expected cap of 200, got %d", len(events))
	}
	// Newest (i=0, At=now) must be kept; the 201st-oldest (i=200) must be dropped.
	if !events[0].At.Equal(now) {
		t.Errorf("expected newest entry first, got At=%v", events[0].At)
	}
}

// TestSessionPageCandidates verifies the alive-OR-has-messages-OR-activity
// inclusion rule.
func TestSessionPageCandidates(t *testing.T) {
	now := mustTime(t, "2026-08-17T12:00:00Z")

	alive := Agent{Name: "alive-agent", LastSeenAt: now.Add(-10 * time.Second)}
	goneWithMsg := Agent{Name: "gone-with-msg", LastSeenAt: now.Add(-2 * time.Hour)}
	goneWithActivity := Agent{Name: "gone-with-activity", LastSeenAt: now.Add(-2 * time.Hour)}
	goneNothing := Agent{Name: "gone-nothing", LastSeenAt: now.Add(-2 * time.Hour)}

	messages := []Message{
		{To: "gone-with-msg", From: "someone", Status: "queued", CreatedAt: now},
	}
	activity := []ActivityEntry{
		{Agent: "gone-with-activity", At: now, Text: "x"},
	}

	got := sessionPageCandidates([]Agent{alive, goneWithMsg, goneWithActivity, goneNothing}, messages, activity, now)

	names := map[string]bool{}
	for _, a := range got {
		names[a.Name] = true
	}
	for _, want := range []string{"alive-agent", "gone-with-msg", "gone-with-activity"} {
		if !names[want] {
			t.Errorf("expected %q to be a candidate, got %+v", want, got)
		}
	}
	if names["gone-nothing"] {
		t.Errorf("expected gone-nothing to be excluded, got %+v", got)
	}
}

// TestRenderSessionPageMarkdown_Header verifies the header fields, alive
// status, and the orchestrator-only tmux attach line.
func TestRenderSessionPageMarkdown_Header(t *testing.T) {
	now := mustTime(t, "2026-08-17T12:00:00Z")
	registeredAt := now.Add(-2 * time.Hour)
	lastSeenAt := now.Add(-10 * time.Second)

	orchestrator := Agent{
		Name: "orchestrator-myapp", Project: "myapp",
		RegisteredAt: registeredAt, LastSeenAt: lastSeenAt, Accepting: true,
	}
	md := renderSessionPageMarkdown(orchestrator, true, nil, now, stuckInfo{})

	if !strings.HasPrefix(md, "---\ngenerated: true\n---\n\n# orchestrator-myapp\n\n") {
		t.Errorf("unexpected header prefix:\n%s", md)
	}
	if !strings.Contains(md, "- Project: myapp\n") {
		t.Errorf("expected project line:\n%s", md)
	}
	if !strings.Contains(md, "- Status: 🟢 alive\n") {
		t.Errorf("expected alive status:\n%s", md)
	}
	if !strings.Contains(md, "- Attach: `tmux attach -t tn-myapp`\n") {
		t.Errorf("expected tmux attach line for orchestrator, got:\n%s", md)
	}
	if !strings.Contains(md, "_No activity yet._") {
		t.Errorf("expected empty-activity state, got:\n%s", md)
	}

	// A non-orchestrator agent gets no attach line.
	plain := Agent{Name: "claude/foo-e2e", Project: "foo", RegisteredAt: registeredAt, LastSeenAt: lastSeenAt}
	md2 := renderSessionPageMarkdown(plain, false, nil, now, stuckInfo{})
	if strings.Contains(md2, "tmux attach") {
		t.Errorf("expected no attach line for non-orchestrator agent, got:\n%s", md2)
	}
	if !strings.Contains(md2, "- Status: ⚫ gone\n") {
		t.Errorf("expected gone status, got:\n%s", md2)
	}
}

// TestRenderSessionPageMarkdown_ContextPct verifies a known ContextPct
// report is shown next to the status line.
func TestRenderSessionPageMarkdown_ContextPct(t *testing.T) {
	now := mustTime(t, "2026-08-17T12:00:00Z")
	pct := 47.0
	agent := Agent{
		Name: "orchestrator-myapp-g1", Project: "myapp",
		LastSeenAt: now.Add(-10 * time.Second), Accepting: false, ContextPct: &pct,
	}
	md := renderSessionPageMarkdown(agent, true, nil, now, stuckInfo{})
	if !strings.Contains(md, "- Status: 🌀 draining (47%)\n") {
		t.Errorf("expected status line with context pct, got:\n%s", md)
	}
}

// TestRenderSessionPagesWritesFiles is an end-to-end (no HTTP, no
// debouncer) check that renderSessionPages() actually lands one sanitized
// file per candidate agent on disk, and skips agents that aren't candidates.
func TestRenderSessionPagesWritesFiles(t *testing.T) {
	dir := t.TempDir()
	sessionPagesDir := dir + "/Claude Sessions"
	cfg := ServeConfig{Port: 0, Projects: map[string]ProjectConfig{}, SessionPagesDir: sessionPagesDir}
	srv := newServer(dir+"/state.json", cfg, func(project, cwd string, env map[string]string) error { return nil })

	srv.mu.Lock()
	srv.state.Agents["claude/foo-e2e"] = &Agent{
		Name: "claude/foo-e2e", Project: "foo", LastSeenAt: time.Now(),
	}
	srv.state.Agents["gone-nothing"] = &Agent{
		Name: "gone-nothing", Project: "bar", LastSeenAt: time.Now().Add(-2 * time.Hour),
	}
	srv.appendActivityLocked("claude/foo-e2e", "did a thing", "")
	srv.mu.Unlock()

	srv.renderSessionPages()

	pagePath := sessionPagesDir + "/claude-foo-e2e.md"
	b, err := os.ReadFile(pagePath)
	if err != nil {
		t.Fatalf("expected session page at %s: %v", pagePath, err)
	}
	if !strings.Contains(string(b), "# claude/foo-e2e") {
		t.Errorf("expected header for claude/foo-e2e, got:\n%s", string(b))
	}
	if !strings.Contains(string(b), "📝 did a thing") {
		t.Errorf("expected activity entry, got:\n%s", string(b))
	}

	if _, err := os.Stat(sessionPagesDir + "/gone-nothing.md"); !os.IsNotExist(err) {
		t.Errorf("expected no page for a non-candidate agent, stat err=%v", err)
	}
}

// TestPostActivityStoresEntryAndBumpsLastSeen verifies POST /activity
// appends an entry and bumps the agent's lastSeenAt.
func TestPostActivityStoresEntryAndBumpsLastSeen(t *testing.T) {
	_, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp", "myapp")

	resp := mustPost(t, ts.URL+"/activity", map[string]any{"name": "orchestrator-myapp", "text": "picked up task X"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out struct {
		Activity ActivityEntry `json:"activity"`
	}
	decodeInto(t, resp, &out)
	if out.Activity.Agent != "orchestrator-myapp" || out.Activity.Text != "picked up task X" {
		t.Errorf("unexpected activity entry: %+v", out.Activity)
	}

	agentsResp, err := http.Get(ts.URL + "/agents")
	if err != nil {
		t.Fatal(err)
	}
	var agentsOut struct {
		Agents []agentView `json:"agents"`
	}
	decodeInto(t, agentsResp, &agentsOut)
	if len(agentsOut.Agents) != 1 || !agentsOut.Agents[0].Alive {
		t.Fatalf("expected lastSeenAt bumped (agent still alive), got %+v", agentsOut.Agents)
	}
}

// TestActivityCapAt500 verifies stored activity is capped at 500 entries,
// dropping the oldest.
func TestActivityCapAt500(t *testing.T) {
	srv, _ := newTestServer(t)

	srv.mu.Lock()
	for i := 0; i < 500; i++ {
		srv.appendActivityLocked("agent-x", "entry", "")
	}
	srv.mu.Unlock()

	srv.mu.Lock()
	before := len(srv.state.Activity)
	srv.mu.Unlock()
	if before != 500 {
		t.Fatalf("expected 500 entries before the capping append, got %d", before)
	}

	srv.mu.Lock()
	srv.appendActivityLocked("agent-x", "the 501st entry", "")
	after := len(srv.state.Activity)
	last := srv.state.Activity[len(srv.state.Activity)-1].Text
	srv.mu.Unlock()

	if after != 500 {
		t.Errorf("expected cap held at 500, got %d", after)
	}
	if last != "the 501st entry" {
		t.Errorf("expected newest entry retained, got %q", last)
	}
}

// TestDashboardSessionNamesWikilinkedWhenPagesEnabled verifies the Sessions
// table links session names to their page when sessionPagesDir is set, and
// falls back to a plain name when it's empty.
func TestDashboardSessionNamesWikilinkedWhenPagesEnabled(t *testing.T) {
	now := mustTime(t, "2026-08-17T12:00:00Z")
	snap := dashboardSnapshot{
		Agents: []Agent{{Name: "orchestrator-myapp", Project: "myapp", LastSeenAt: now.Add(-5 * time.Second)}},
	}

	mdEnabled := renderDashboardMarkdown(snap, now, "/Users/someone/Documents/MyVault/Claude Sessions", nil, false, nil)
	if !strings.Contains(mdEnabled, "| [[Claude Sessions/orchestrator-myapp|orchestrator-myapp]] | myapp |") {
		t.Errorf("expected wikilinked session name, got:\n%s", mdEnabled)
	}

	mdDisabled := renderDashboardMarkdown(snap, now, "", nil, false, nil)
	if !strings.Contains(mdDisabled, "| orchestrator-myapp | myapp |") {
		t.Errorf("expected plain session name when pages disabled, got:\n%s", mdDisabled)
	}
	if strings.Contains(mdDisabled, "[[") {
		t.Errorf("expected no wikilinks when pages disabled, got:\n%s", mdDisabled)
	}
}

// TestCmdLogWiring is a light end-to-end check that `tn log` reaches
// POST /activity with the right body (store-level: verified via the HTTP
// server rather than exec'ing the built binary).
func TestCmdLogWiring(t *testing.T) {
	_, ts := newTestServer(t)
	register(t, ts, "orchestrator-myapp", "myapp")

	b := NewBridgeClient(ts.URL)
	body, err := b.LogActivity("orchestrator-myapp", "phase transition: implementation -> review")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Activity ActivityEntry `json:"activity"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Activity.Text != "phase transition: implementation -> review" {
		t.Errorf("unexpected stored text: %q", out.Activity.Text)
	}
}
