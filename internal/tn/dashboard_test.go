package tn

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

// TestRenderDashboardMarkdown_EmptyStates verifies the empty-state copy for
// a fresh, empty snapshot.
func TestRenderDashboardMarkdown_EmptyStates(t *testing.T) {
	now := mustTime(t, "2026-08-17T12:04:31Z")
	md := renderDashboardMarkdown(dashboardSnapshot{}, now, "", nil, false, nil)

	if !strings.Contains(md, "_No sessions registered._") {
		t.Errorf("expected empty sessions state, got:\n%s", md)
	}
	if !strings.Contains(md, "_No pending messages._") {
		t.Errorf("expected empty pending state, got:\n%s", md)
	}
	if strings.Contains(md, "## Recent activity") {
		t.Errorf("expected Recent activity section to be omitted when empty, got:\n%s", md)
	}
	if !strings.Contains(md, "Updated: 2026-08-17 12:04:31") {
		t.Errorf("expected updated timestamp, got:\n%s", md)
	}
	if !strings.HasPrefix(md, "---\ngenerated: true\n---\n\n") {
		t.Errorf("expected frontmatter prefix, got:\n%s", md)
	}
}

// TestRenderDashboardMarkdown_TriageSummary verifies the passive triage
// line (count + oldest item) appears when triage tasks exist, is omitted
// when there are none, and degrades to count-only when the oldest task's
// DateCreated can't be parsed — mirrors buildStatusResponse's /status.triage
// behavior via the shared computeTriageSummary helper.
func TestRenderDashboardMarkdown_TriageSummary(t *testing.T) {
	now := mustTime(t, "2026-08-25T12:00:00Z")

	t.Run("no triage tasks omits the line", func(t *testing.T) {
		tasks := []Task{{Path: "Tasks/A.md", Title: "A", Status: "review"}}
		md := renderDashboardMarkdown(dashboardSnapshot{}, now, "", tasks, false, nil)
		if strings.Contains(md, "_Triage:") {
			t.Errorf("expected no triage line, got:\n%s", md)
		}
	})

	t.Run("triage tasks present shows count and oldest", func(t *testing.T) {
		tasks := []Task{
			{Path: "Tasks/Newer.md", Title: "Newer", Status: "triage", DateCreated: now.Add(-1 * time.Hour).Format(time.RFC3339Nano)},
			{Path: "Tasks/Oldest.md", Title: "Oldest", Status: "triage", DateCreated: now.Add(-26 * time.Hour).Format(time.RFC3339Nano)},
		}
		md := renderDashboardMarkdown(dashboardSnapshot{}, now, "", tasks, false, nil)
		if !strings.Contains(md, "_Triage: 2 waiting, oldest 1d") {
			t.Errorf("expected triage summary line with count and age, got:\n%s", md)
		}
		if !strings.Contains(md, "Tasks/Oldest") {
			t.Errorf("expected oldest task wikilink, got:\n%s", md)
		}
	})

	t.Run("unparseable DateCreated degrades to count-only", func(t *testing.T) {
		tasks := []Task{{Path: "Tasks/NoDate.md", Title: "NoDate", Status: "triage"}}
		md := renderDashboardMarkdown(dashboardSnapshot{}, now, "", tasks, false, nil)
		if !strings.Contains(md, "_Triage: 1 waiting._") {
			t.Errorf("expected count-only triage line, got:\n%s", md)
		}
	})

	t.Run("needsActionErr suppresses the line entirely", func(t *testing.T) {
		tasks := []Task{{Path: "Tasks/A.md", Title: "A", Status: "triage", DateCreated: now.Format(time.RFC3339Nano)}}
		md := renderDashboardMarkdown(dashboardSnapshot{}, now, "", tasks, true, nil)
		if strings.Contains(md, "_Triage:") {
			t.Errorf("expected no triage line when needsActionErr is set, got:\n%s", md)
		}
	})
}

// TestRenderDashboardMarkdown_Sessions verifies the alive/gone flags, queued
// counts, and tmux attach column.
func TestRenderDashboardMarkdown_Sessions(t *testing.T) {
	now := mustTime(t, "2026-08-17T12:04:31Z")

	alive := Agent{
		Name: "orchestrator-procfit", Project: "procfit",
		LastSeenAt: now.Add(-21 * time.Second),
		Accepting:  true,
	}
	gone := Agent{
		Name: "claude/foo-e2e", Project: "foo",
		LastSeenAt: now.Add(-44*time.Minute - 30*time.Second),
	}
	deregNoPending := Agent{
		Name: "claude/bar-old", Project: "bar",
		LastSeenAt:     now.Add(-2 * time.Hour),
		DeregisteredAt: timePtr(now.Add(-1 * time.Hour)),
	}
	deregWithPending := Agent{
		Name: "claude/baz-stuck", Project: "baz",
		LastSeenAt:     now.Add(-3 * time.Hour),
		DeregisteredAt: timePtr(now.Add(-2 * time.Hour)),
	}

	messages := []Message{
		{ID: "msg-1", To: "orchestrator-procfit", Status: "queued", CreatedAt: now.Add(-3 * time.Minute)},
		{ID: "msg-2", To: "orchestrator-procfit", Status: "delivered", CreatedAt: now.Add(-1 * time.Minute)},
		{ID: "msg-3", To: "orchestrator-procfit", Status: "acked", CreatedAt: now.Add(-10 * time.Minute), AckedAt: timePtr(now.Add(-9 * time.Minute))},
		{ID: "msg-4", To: "claude/baz-stuck", Status: "queued", CreatedAt: now.Add(-30 * time.Minute)},
	}

	snap := dashboardSnapshot{
		Agents:   []Agent{alive, gone, deregNoPending, deregWithPending},
		Messages: messages,
	}
	md := renderDashboardMarkdown(snap, now, "", nil, false, nil)

	if !strings.Contains(md, "| orchestrator-procfit | procfit | 🟢 alive | 12:04:10 | 2 | `tmux attach -t tn-procfit` |") {
		t.Errorf("missing/incorrect alive orchestrator row:\n%s", md)
	}
	if !strings.Contains(md, "| claude/foo-e2e | foo | ⚫ gone | 11:20:01 | 0 | — |") {
		t.Errorf("missing/incorrect gone agent row:\n%s", md)
	}
	if strings.Contains(md, "claude/bar-old") {
		t.Errorf("deregistered agent with no pending messages should be omitted:\n%s", md)
	}
	if !strings.Contains(md, "| claude/baz-stuck | baz | ⚫ gone | 09:04:31 | 1 | — |") {
		t.Errorf("deregistered agent with pending messages should still appear as gone:\n%s", md)
	}
}

// TestRenderDashboardMarkdown_DrainingGeneration verifies a generation
// agent that has run `tn drain` (Accepting: false) shows 🌀 draining, and
// that its Attach column uses its own reported TmuxSession rather than the
// legacy tn-<project> derivation (SPEC-generations.md).
func TestRenderDashboardMarkdown_DrainingGeneration(t *testing.T) {
	now := mustTime(t, "2026-08-17T12:04:31Z")

	pct := 47.0
	draining := Agent{
		Name: "orchestrator-myapp-g54321", Project: "myapp",
		LastSeenAt:  now.Add(-10 * time.Second),
		Accepting:   false,
		TmuxSession: "tn-myapp-g54321",
		ContextPct:  &pct,
	}
	snap := dashboardSnapshot{Agents: []Agent{draining}}
	md := renderDashboardMarkdown(snap, now, "", nil, false, nil)

	if !strings.Contains(md, "| orchestrator-myapp-g54321 | myapp | 🌀 draining (47%) | 12:04:21 | 0 | `tmux attach -t tn-myapp-g54321` |") {
		t.Errorf("missing/incorrect draining generation row with context pct:\n%s", md)
	}
}

// TestRenderDashboardMarkdown_PendingAndWikilinks verifies pending message
// formatting: oldest-first ordering, humanized age, truncation, and
// wikilink conversion for .md task paths.
func TestRenderDashboardMarkdown_PendingAndWikilinks(t *testing.T) {
	now := mustTime(t, "2026-08-17T12:04:31Z")

	longText := strings.Repeat("x", 150)
	messages := []Message{
		{ID: "msg-older", To: "orchestrator-procfit", Status: "queued", TaskPath: "TaskNotes/Fix login.md",
			Text: "Task assigned: Fix login", CreatedAt: now.Add(-3 * time.Minute)},
		{ID: "msg-newer", To: "claude/foo-e2e", Status: "delivered", TaskPath: "not-a-task-ref",
			Text: "@claude line text", CreatedAt: now.Add(-40 * time.Second)},
		{ID: "msg-long", To: "orchestrator-procfit", Status: "queued",
			Text: longText, CreatedAt: now.Add(-2 * time.Hour)},
	}
	snap := dashboardSnapshot{Messages: messages}
	md := renderDashboardMarkdown(snap, now, "", nil, false, nil)

	// Oldest first: msg-long (2h) before msg-older (3m) before msg-newer (40s).
	longIdx := strings.Index(md, "msg-long")
	olderIdx := strings.Index(md, "msg-older")
	newerIdx := strings.Index(md, "msg-newer")
	if !(longIdx < olderIdx && olderIdx < newerIdx) {
		t.Errorf("expected oldest-first ordering (long, older, newer), got indices %d,%d,%d:\n%s", longIdx, olderIdx, newerIdx, md)
	}

	if !strings.Contains(md, "**orchestrator-procfit** (queued, 3m): Task assigned: Fix login ([[TaskNotes/Fix login]])") {
		t.Errorf("expected humanized age + wikilink for msg-older, got:\n%s", md)
	}
	if !strings.Contains(md, "**claude/foo-e2e** (delivered, 40s): @claude line text") {
		t.Errorf("expected humanized seconds age for msg-newer, got:\n%s", md)
	}
	if strings.Contains(md, "not-a-task-ref]]") {
		t.Errorf("non-.md taskPath should not become a wikilink:\n%s", md)
	}
	if !strings.Contains(md, "**orchestrator-procfit** (queued, 2h): "+strings.Repeat("x", 120)+"…") {
		t.Errorf("expected text truncated to 120 chars with ellipsis, got:\n%s", md)
	}
}

// TestRenderDashboardMarkdown_RecentActivity verifies the acked-messages
// section: newest first, capped at 10, response truncation, and that it's
// omitted entirely when there are no acked messages (covered by the empty
// states test).
func TestRenderDashboardMarkdown_RecentActivity(t *testing.T) {
	now := mustTime(t, "2026-08-17T12:04:31Z")

	var messages []Message
	for i := 0; i < 12; i++ {
		ackedAt := now.Add(-time.Duration(i) * time.Minute)
		messages = append(messages, Message{
			ID: "msg-" + string(rune('a'+i)), To: "orchestrator-procfit", Status: "acked",
			Response: "ok " + string(rune('a'+i)), TaskPath: "Tasks/T.md",
			CreatedAt: ackedAt.Add(-time.Minute), AckedAt: timePtr(ackedAt),
		})
	}
	snap := dashboardSnapshot{Messages: messages}
	md := renderDashboardMarkdown(snap, now, "", nil, false, nil)

	if !strings.Contains(md, "## Recent activity") {
		t.Fatalf("expected Recent activity section:\n%s", md)
	}

	// Newest (msg-a, ackedAt=now) should appear before oldest-of-the-ten.
	idxA := strings.Index(md, "msg-a ")
	idxJ := strings.Index(md, "msg-j ") // 10th newest (index 9)
	if idxA == -1 || idxJ == -1 || idxA > idxJ {
		t.Errorf("expected newest-first ordering, got:\n%s", md)
	}
	// Capped at 10: the 11th and 12th oldest (msg-k, msg-l) must not appear.
	if strings.Contains(md, "msg-k ") || strings.Contains(md, "msg-l ") {
		t.Errorf("expected activity capped at 10 entries, got:\n%s", md)
	}
	if !strings.Contains(md, `✅ orchestrator-procfit acked msg-a — "ok a" ([[Tasks/T]])`) {
		t.Errorf("expected formatted activity line with wikilink, got:\n%s", md)
	}
}

func timePtr(t time.Time) *time.Time { return &t }

// TestAtomicWriteFile verifies the write-then-rename behavior: content
// lands at the target path, no stray temp file is left, and a second write
// correctly overwrites.
func TestAtomicWriteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dashboard.md")

	if err := atomicWriteFile(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "first" {
		t.Errorf("expected content %q, got %q", "first", string(b))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 file in dir (no stray tmp), got %d: %+v", len(entries), entries)
	}

	if err := atomicWriteFile(path, []byte("second")); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "second" {
		t.Errorf("expected overwritten content %q, got %q", "second", string(b))
	}
}

// TestAtomicWriteFile_CreatesDir verifies the target directory is created
// if missing.
func TestAtomicWriteFile_CreatesDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "dashboard.md")

	if err := atomicWriteFile(path, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "hi" {
		t.Fatalf("expected file to exist with content %q, got %q, err=%v", "hi", string(b), err)
	}
}

// TestDebouncerCoalescesRapidTriggers fires 5 triggers in quick succession
// and asserts they collapse into a small, bounded number of renders (not 5),
// proving the debounce coalesces rather than firing once per Trigger call.
// Uses a short interval so the test runs fast without an injectable clock.
func TestDebouncerCoalescesRapidTriggers(t *testing.T) {
	var calls int64
	d := newDebouncer(100*time.Millisecond, func() {
		atomic.AddInt64(&calls, 1)
	})
	go d.run()

	for i := 0; i < 5; i++ {
		d.Trigger()
		time.Sleep(5 * time.Millisecond)
	}

	time.Sleep(250 * time.Millisecond)

	got := atomic.LoadInt64(&calls)
	if got == 0 {
		t.Fatal("expected at least 1 render")
	}
	if got > 2 {
		t.Errorf("expected rapid triggers to coalesce into <=2 renders within the debounce window, got %d", got)
	}
}

// TestDebouncerSpacesOutRenders verifies renders are spaced at least
// interval apart even under sustained triggering.
func TestDebouncerSpacesOutRenders(t *testing.T) {
	const interval = 80 * time.Millisecond
	// times is written from the debouncer's own background goroutine (via
	// fn, called from d.run()) and read from this test's goroutine after a
	// sleep — a plain slice here is a genuine data race (caught by `go
	// test -race`): the sleep usually happens to land after the writes in
	// practice, but Go's memory model gives no such guarantee without
	// explicit synchronization. mu makes both sides safe.
	var mu sync.Mutex
	var times []time.Time
	d := newDebouncer(interval, func() {
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
	})
	go d.run()

	deadline := time.Now().Add(350 * time.Millisecond)
	for time.Now().Before(deadline) {
		d.Trigger()
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(120 * time.Millisecond)

	mu.Lock()
	got := append([]time.Time(nil), times...)
	mu.Unlock()

	if len(got) < 2 {
		t.Fatalf("expected at least 2 renders over ~350ms with an 80ms debounce, got %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		gap := got[i].Sub(got[i-1])
		if gap < interval-10*time.Millisecond {
			t.Errorf("renders %d and %d were only %v apart, want >= ~%v", i-1, i, gap, interval)
		}
	}
}

// TestHumanizeAge covers the age-bucket boundaries.
func TestHumanizeAge(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{40 * time.Second, "40s"},
		{3 * time.Minute, "3m"},
		{2 * time.Hour, "2h"},
		{25 * time.Hour, "1d"},
		{-5 * time.Second, "0s"},
	}
	for _, c := range cases {
		if got := humanizeAge(c.d); got != c.want {
			t.Errorf("humanizeAge(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// TestTruncate covers the exact-boundary and over-length cases.
func TestTruncate(t *testing.T) {
	if got := truncate("short", 120); got != "short" {
		t.Errorf("expected untouched short string, got %q", got)
	}
	exact := strings.Repeat("a", 120)
	if got := truncate(exact, 120); got != exact {
		t.Errorf("expected exact-length string untouched, got %q", got)
	}
	long := strings.Repeat("a", 121)
	if got := truncate(long, 120); got != strings.Repeat("a", 120)+"…" {
		t.Errorf("expected truncation with ellipsis, got %q", got)
	}
}

// TestTaskWikilink covers the .md-suffix rule.
func TestTaskWikilink(t *testing.T) {
	if got := taskWikilink("TaskNotes/Fix login.md"); got != "[[TaskNotes/Fix login]]" {
		t.Errorf("got %q", got)
	}
	if got := taskWikilink("not-markdown"); got != "" {
		t.Errorf("expected empty for non-.md path, got %q", got)
	}
	if got := taskWikilink(""); got != "" {
		t.Errorf("expected empty for empty path, got %q", got)
	}
}
