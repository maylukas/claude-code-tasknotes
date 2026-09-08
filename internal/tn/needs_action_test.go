package tn

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestBuildNeedsActionQuery verifies the nested OR-group shape: an AND root
// with archived is-not-checked plus an OR group of the three statuses.
func TestBuildNeedsActionQuery(t *testing.T) {
	q := buildNeedsActionQuery()
	if q.Type != "group" || q.Conjunction != "and" {
		t.Fatalf("unexpected root shape: %+v", q)
	}
	if len(q.Children) != 2 {
		t.Fatalf("expected 2 root children (archived + status-or group), got %d: %+v", len(q.Children), q.Children)
	}
	archived := q.Children[0]
	if archived.Type != "condition" || archived.Property != "archived" || archived.Operator != "is-not-checked" {
		t.Errorf("unexpected first condition: %+v", archived)
	}

	orGroup := q.Children[1]
	if orGroup.Type != "group" || orGroup.Conjunction != "or" {
		t.Fatalf("expected nested OR group, got %+v", orGroup)
	}
	wantStatuses := []string{"triage", "needs-input", "review"}
	if len(orGroup.Children) != len(wantStatuses) {
		t.Fatalf("expected %d status conditions, got %d: %+v", len(wantStatuses), len(orGroup.Children), orGroup.Children)
	}
	for i, want := range wantStatuses {
		c := orGroup.Children[i]
		if c.Type != "condition" || c.Property != "status" || c.Operator != "is" || c.Value != want {
			t.Errorf("status condition %d = %+v, want status=%q", i, c, want)
		}
	}

	if q.SortKey != "dateModified" || q.SortDirection != "desc" {
		t.Errorf("expected dateModified desc sort, got sortKey=%q sortDirection=%q", q.SortKey, q.SortDirection)
	}
}

// TestRenderNeedsActionSection_GroupingAndWikilinks verifies tasks are
// grouped under the right emoji subheader, in group order (triage, then
// needs-input, then review), each line wikilinked with its first project
// appended.
func TestRenderNeedsActionSection_GroupingAndWikilinks(t *testing.T) {
	tasks := []Task{
		{Path: "Tasks/Review me.md", Title: "Review me", Status: "review", Projects: []string{"[[Myapp]]"}},
		{Path: "Tasks/Triage me.md", Title: "Triage me", Status: "triage"},
		{Path: "Tasks/Answer me.md", Title: "Answer me", Status: "needs-input", Projects: []string{"[[E2E Tests]]", "[[Ignored Second]]"}},
		{Path: "Tasks/Not relevant.md", Title: "Not relevant", Status: "in-progress"},
	}
	section := renderNeedsActionSection(tasks, false, nil, nil)

	if !strings.HasPrefix(section, "## Needs your action\n\n") {
		t.Fatalf("expected section header prefix, got:\n%s", section)
	}
	if strings.Contains(section, "Not relevant") {
		t.Errorf("expected non-matching status to be excluded, got:\n%s", section)
	}

	// Group order: triage, then needs-input, then review.
	triageIdx := strings.Index(section, "🔴 Triage")
	needsInputIdx := strings.Index(section, "🟠 Needs input")
	reviewIdx := strings.Index(section, "🟣 Review")
	if triageIdx == -1 || needsInputIdx == -1 || reviewIdx == -1 {
		t.Fatalf("expected all three group headers present, got:\n%s", section)
	}
	if !(triageIdx < needsInputIdx && needsInputIdx < reviewIdx) {
		t.Errorf("expected group order triage < needs-input < review, got indices %d,%d,%d:\n%s", triageIdx, needsInputIdx, reviewIdx, section)
	}

	if !strings.Contains(section, "- [[Tasks/Triage me]] — Triage me\n") {
		t.Errorf("expected triage line with no project suffix, got:\n%s", section)
	}
	if !strings.Contains(section, "- [[Tasks/Answer me]] — Answer me ([[E2E Tests]])\n") {
		t.Errorf("expected needs-input line with only the first project, got:\n%s", section)
	}
	if !strings.Contains(section, "- [[Tasks/Review me]] — Review me ([[Myapp]])\n") {
		t.Errorf("expected review line with its project, got:\n%s", section)
	}
}

// TestRenderNeedsActionSection_MrLink verifies a task with
// customProperties.mr set gets a trailing " — [MR](<url>)" markdown link
// appended, in every group (not just review), and that a task without the
// property omits it entirely.
func TestRenderNeedsActionSection_MrLink(t *testing.T) {
	tasks := []Task{
		{Path: "Tasks/Review me.md", Title: "Review me", Status: "review", CustomProperties: map[string]string{"mr": "https://example.com/mr/1"}},
		{Path: "Tasks/Triage me.md", Title: "Triage me", Status: "triage", CustomProperties: map[string]string{"mr": "https://example.com/mr/2"}},
		{Path: "Tasks/No mr.md", Title: "No mr", Status: "needs-input"},
	}
	section := renderNeedsActionSection(tasks, false, nil, nil)

	if !strings.Contains(section, "- [[Tasks/Review me]] — Review me — [MR](https://example.com/mr/1)\n") {
		t.Errorf("expected review line with MR link, got:\n%s", section)
	}
	if !strings.Contains(section, "- [[Tasks/Triage me]] — Triage me — [MR](https://example.com/mr/2)\n") {
		t.Errorf("expected triage line with MR link too, got:\n%s", section)
	}
	if !strings.Contains(section, "- [[Tasks/No mr]] — No mr\n") {
		t.Errorf("expected no-mr line unchanged (no trailing link), got:\n%s", section)
	}
	if strings.Contains(section, "No mr — [MR]") {
		t.Errorf("expected no MR link when the property isn't set, got:\n%s", section)
	}
}

// TestRenderNeedsActionSection_MrState verifies a known MR state (from the
// MR watcher's mrStates map) is appended after the link, and omitted when
// the state isn't known yet (e.g. before the watcher's first pass).
func TestRenderNeedsActionSection_MrState(t *testing.T) {
	tasks := []Task{
		{Path: "Tasks/Review me.md", Title: "Review me", Status: "review", CustomProperties: map[string]string{"mr": "https://example.com/mr/1"}},
		{Path: "Tasks/Unknown state.md", Title: "Unknown state", Status: "review", CustomProperties: map[string]string{"mr": "https://example.com/mr/2"}},
	}
	mrStates := map[string]string{"Tasks/Review me.md": "opened"}
	section := renderNeedsActionSection(tasks, false, mrStates, nil)

	if !strings.Contains(section, "- [[Tasks/Review me]] — Review me — [MR](https://example.com/mr/1) (opened)\n") {
		t.Errorf("expected the MR link followed by its known state, got:\n%s", section)
	}
	if !strings.Contains(section, "- [[Tasks/Unknown state]] — Unknown state — [MR](https://example.com/mr/2)\n") {
		t.Errorf("expected the MR link WITHOUT a state suffix when unknown, got:\n%s", section)
	}
}

// TestRenderNeedsActionSection_MrOpenThreads verifies a nonzero open-thread
// count is appended after the MR state annotation, and omitted entirely
// when the count is zero or unknown.
func TestRenderNeedsActionSection_MrOpenThreads(t *testing.T) {
	tasks := []Task{
		{Path: "Tasks/Threads.md", Title: "Has threads", Status: "review", CustomProperties: map[string]string{"mr": "https://example.com/mr/1"}},
		{Path: "Tasks/NoThreads.md", Title: "No threads", Status: "review", CustomProperties: map[string]string{"mr": "https://example.com/mr/2"}},
	}
	mrStates := map[string]string{"Tasks/Threads.md": "opened", "Tasks/NoThreads.md": "opened"}
	mrOpenThreads := map[string]int{"Tasks/Threads.md": 2, "Tasks/NoThreads.md": 0}
	section := renderNeedsActionSection(tasks, false, mrStates, mrOpenThreads)

	if !strings.Contains(section, "- [[Tasks/Threads]] — Has threads — [MR](https://example.com/mr/1) (opened), 2 unresolved thread(s)\n") {
		t.Errorf("expected the open-thread count appended after the state, got:\n%s", section)
	}
	if !strings.Contains(section, "- [[Tasks/NoThreads]] — No threads — [MR](https://example.com/mr/2) (opened)\n") {
		t.Errorf("expected no thread-count suffix when the count is zero, got:\n%s", section)
	}
}

// TestRenderNeedsActionSection_EmptyState verifies the "nothing needs you"
// fallback when there are no matching tasks.
func TestRenderNeedsActionSection_EmptyState(t *testing.T) {
	section := renderNeedsActionSection(nil, false, nil, nil)
	if !strings.Contains(section, "_Nothing needs you._") {
		t.Errorf("expected empty-state copy, got:\n%s", section)
	}
	if strings.Contains(section, "🔴") || strings.Contains(section, "🟠") || strings.Contains(section, "🟣") {
		t.Errorf("expected no group headers when empty, got:\n%s", section)
	}
}

// TestRenderNeedsActionSection_APIUnreachable verifies the fallback line
// when the TaskNotes API couldn't be reached, and that it takes priority
// over any (stale/irrelevant) tasks slice passed alongside it.
func TestRenderNeedsActionSection_APIUnreachable(t *testing.T) {
	section := renderNeedsActionSection([]Task{{Path: "x.md", Title: "x", Status: "triage"}}, true, nil, nil)
	if !strings.Contains(section, "_TaskNotes API unreachable._") {
		t.Errorf("expected unreachable fallback, got:\n%s", section)
	}
	if strings.Contains(section, "🔴") {
		t.Errorf("expected no task groups rendered when API is unreachable, got:\n%s", section)
	}
}

// TestNeedsActionCache_ReusesWithinTTL verifies a second get() call within
// the TTL returns the cached result without invoking fetch again.
func TestNeedsActionCache_ReusesWithinTTL(t *testing.T) {
	var c needsActionCache
	var calls int
	fetch := func() ([]Task, error) {
		calls++
		return []Task{{Path: "x.md", Title: "x"}}, nil
	}

	base := time.Now()
	tasks1, err1 := c.get(base, 30*time.Second, fetch)
	if calls != 1 {
		t.Fatalf("expected fetch called once on first get, got %d calls", calls)
	}
	if err1 || len(tasks1) != 1 {
		t.Fatalf("unexpected first result: tasks=%+v err=%v", tasks1, err1)
	}

	tasks2, err2 := c.get(base.Add(10*time.Second), 30*time.Second, fetch)
	if calls != 1 {
		t.Errorf("expected fetch NOT called again within TTL, got %d calls", calls)
	}
	if err2 || len(tasks2) != 1 {
		t.Errorf("unexpected cached result: tasks=%+v err=%v", tasks2, err2)
	}
}

// TestNeedsActionCache_RefreshesAfterTTL verifies a get() call after the
// TTL has elapsed invokes fetch again.
func TestNeedsActionCache_RefreshesAfterTTL(t *testing.T) {
	var c needsActionCache
	var calls int
	fetch := func() ([]Task, error) {
		calls++
		return []Task{{Path: "x.md", Title: "x"}}, nil
	}

	base := time.Now()
	c.get(base, 30*time.Second, fetch)
	c.get(base.Add(31*time.Second), 30*time.Second, fetch)

	if calls != 2 {
		t.Errorf("expected fetch called again after TTL elapsed, got %d calls", calls)
	}
}

// TestNeedsActionCache_CachesFailureToo verifies a failed fetch is also
// cached for the TTL (so a down API isn't hammered on every render either),
// and that fetchedAt still advances (a subsequent call after the TTL
// retries).
func TestNeedsActionCache_CachesFailureToo(t *testing.T) {
	var c needsActionCache
	var calls int
	fetch := func() ([]Task, error) {
		calls++
		return nil, errors.New("connection refused")
	}

	base := time.Now()
	tasks1, err1 := c.get(base, 30*time.Second, fetch)
	if !err1 || tasks1 != nil {
		t.Fatalf("expected a failed first fetch to report apiErr=true, got tasks=%+v err=%v", tasks1, err1)
	}

	_, err2 := c.get(base.Add(5*time.Second), 30*time.Second, fetch)
	if calls != 1 {
		t.Errorf("expected the failure to be cached (no retry within TTL), got %d calls", calls)
	}
	if !err2 {
		t.Errorf("expected cached apiErr=true, got false")
	}

	_, err3 := c.get(base.Add(31*time.Second), 30*time.Second, fetch)
	if calls != 2 {
		t.Errorf("expected a retry after TTL elapsed, got %d calls", calls)
	}
	if !err3 {
		t.Errorf("expected apiErr=true again after the retry also fails")
	}
}
