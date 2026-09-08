package tn

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// --- Needs your action (dashboard section) ---

// needsActionCacheTTL bounds how often the dashboard queries TaskNotes for
// the Needs your action section — the dashboard debounce can fire often
// (every mutation), and this avoids hammering the API on every render.
const needsActionCacheTTL = 30 * time.Second

// needsActionCache holds the last fetched Needs-your-action query result.
// Guarded by its own mutex, independent of Server.mu, since fetching can
// block on a network call and rendering already runs outside Server.mu.
type needsActionCache struct {
	mu        sync.Mutex
	fetchedAt time.Time
	tasks     []Task
	apiErr    bool
}

// get returns the cached tasks (and whether the cached fetch had failed) if
// fetched within ttl of now; otherwise it calls fetch, caches the result —
// including a failure, so a down API isn't retried on every render either —
// and returns it.
func (c *needsActionCache) get(now time.Time, ttl time.Duration, fetch func() ([]Task, error)) ([]Task, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.fetchedAt.IsZero() && now.Sub(c.fetchedAt) < ttl {
		return c.tasks, c.apiErr
	}

	tasks, err := fetch()
	c.fetchedAt = now
	if err != nil {
		c.tasks = nil
		c.apiErr = true
		return nil, true
	}
	c.tasks = tasks
	c.apiErr = false
	return tasks, false
}

// buildNeedsActionQuery builds the FilterQuery for the dashboard's Needs
// your action section: open (non-archived) tasks whose status is triage,
// needs-input, or review. FilterQuery has no "in" operator, so the three
// statuses are OR'd together in a nested group inside the AND root — a
// shape FilterGroup/FilterCondition can't express, hence filterNode.
// Requests dateModified-desc sorting; if TaskNotes doesn't support that
// sort key it's expected to fall back to its default ordering rather than
// erroring, per the same "always send a sortKey" precedent buildListQuery
// and buildDueTasksQuery already follow.
func buildNeedsActionQuery() filterNode {
	statusOr := filterNode{
		Type: "group", ID: "status-or", Conjunction: "or",
		Children: []filterNode{
			{Type: "condition", ID: "s-triage", Property: "status", Operator: "is", Value: "triage"},
			{Type: "condition", ID: "s-needs-input", Property: "status", Operator: "is", Value: "needs-input"},
			{Type: "condition", ID: "s-review", Property: "status", Operator: "is", Value: "review"},
		},
	}
	return filterNode{
		Type: "group", ID: "root", Conjunction: "and",
		Children: []filterNode{
			{Type: "condition", ID: "archived", Property: "archived", Operator: "is-not-checked"},
			statusOr,
		},
		SortKey:       "dateModified",
		SortDirection: "desc",
	}
}

// needsActionGroups defines the render order, emoji, and label for each
// status bucket in the Needs your action section.
var needsActionGroups = []struct{ status, emoji, label string }{
	{"triage", "🔴", "Triage (your decision)"},
	{"needs-input", "🟠", "Needs input (agent waiting on you)"},
	{"review", "🟣", "Review (verify & mark done)"},
}

// renderNeedsActionSection renders the "## Needs your action" dashboard
// section from a fetched task list, grouped by status with an emoji
// subheader per non-empty group. Each line links the task and, if present,
// its first project. apiErr renders a one-line unreachable fallback instead
// (the section is never omitted outright, so its position at the top of the
// note stays stable across renders).
func renderNeedsActionSection(tasks []Task, apiErr bool, mrStates map[string]string) string {
	var b strings.Builder
	b.WriteString("## Needs your action\n\n")

	if apiErr {
		b.WriteString("_TaskNotes API unreachable._\n\n")
		return b.String()
	}

	hasAny := false
	for _, g := range needsActionGroups {
		var lines []string
		for _, t := range tasks {
			if t.Status != g.status {
				continue
			}
			line := fmt.Sprintf("- %s — %s", taskWikilink(t.Path), t.Title)
			if len(t.Projects) > 0 && t.Projects[0] != "" {
				line += " (" + t.Projects[0] + ")"
			}
			if mr := t.CustomProperties["mr"]; mr != "" {
				line += fmt.Sprintf(" — [MR](%s)", mr)
				if state := mrStates[t.Path]; state != "" {
					line += fmt.Sprintf(" (%s)", state)
				}
			}
			if branch := integrationBranchFor(t); branch != "" {
				if integrationRoleFor(t) == "owner" {
					line += fmt.Sprintf(" — **OWNER** of `%s`", branch)
				} else {
					line += fmt.Sprintf(" — `%s`", branch)
				}
			}
			if ncKey := t.CustomProperties["jira-nc"]; ncKey != "" {
				line += fmt.Sprintf(" — NC `%s`", ncKey)
			} else if jira := t.CustomProperties["jira"]; jira != "" {
				line += fmt.Sprintf(" — %s", jira)
			}
			lines = append(lines, line)
		}
		if len(lines) == 0 {
			continue
		}
		hasAny = true
		fmt.Fprintf(&b, "### %s %s\n\n", g.emoji, g.label)
		for _, line := range lines {
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
	}

	if !hasAny {
		b.WriteString("_Nothing needs you._\n\n")
	}
	return b.String()
}
