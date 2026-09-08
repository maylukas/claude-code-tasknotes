package tn

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

// --- Dashboard rendering ---

// dashboardSnapshot is a point-in-time copy of the state relevant to the
// dashboard and session pages, taken under the lock so rendering itself can
// run lock-free.
type dashboardSnapshot struct {
	Agents   []Agent
	Messages []Message
	Activity []ActivityEntry
	// MRStates is a copy of State.MRStates (taskPath -> last-observed
	// GitLab MR state), used to annotate Needs-your-action MR links with
	// their current state when known.
	MRStates map[string]string
	// MRReviews is a copy of State.MRReviews (taskPath -> last-observed
	// review-comment state), used to annotate Needs-your-action MR links
	// with their open-thread count.
	MRReviews map[string]mrReviewState
}

func (s *Server) snapshotLocked() dashboardSnapshot {
	agents := make([]Agent, 0, len(s.state.Agents))
	for _, a := range s.state.Agents {
		agents = append(agents, *a)
	}
	messages := make([]Message, 0, len(s.state.Messages))
	for _, m := range s.state.Messages {
		messages = append(messages, *m)
	}
	activity := make([]ActivityEntry, 0, len(s.state.Activity))
	for _, a := range s.state.Activity {
		activity = append(activity, *a)
	}
	mrStates := make(map[string]string, len(s.state.MRStates))
	for k, v := range s.state.MRStates {
		mrStates[k] = v
	}
	mrReviews := make(map[string]mrReviewState, len(s.state.MRReviews))
	for k, v := range s.state.MRReviews {
		mrReviews[k] = v
	}
	return dashboardSnapshot{Agents: agents, Messages: messages, Activity: activity, MRStates: mrStates, MRReviews: mrReviews}
}

// renderDashboard snapshots state under the lock, fetches (or reuses a
// cached) Needs-your-action task list, renders outside the lock, and writes
// atomically. Write failures are logged once per distinct error and never
// crash the daemon.
func (s *Server) renderDashboard() {
	s.mu.Lock()
	snap := s.snapshotLocked()
	sessionPagesDir := s.sessionPagesDir
	client := s.tnClient
	s.mu.Unlock()

	var needsActionTasks []Task
	needsActionErr := true
	if client != nil {
		needsActionTasks, needsActionErr = s.needsAction.get(time.Now(), needsActionCacheTTL, func() ([]Task, error) {
			return client.QueryTasksRaw(buildNeedsActionQuery())
		})
	}

	md := renderDashboardMarkdown(snap, time.Now(), sessionPagesDir, needsActionTasks, needsActionErr, s.stuck.snapshot())
	if err := atomicWriteFile(s.dashboardPath, []byte(md)); err != nil {
		s.logDashboardErrOnce(err)
	}
}

func (s *Server) logDashboardErrOnce(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg := err.Error()
	if s.lastDashboardErr == msg {
		return
	}
	s.lastDashboardErr = msg
	log.Printf("serve: failed to write dashboard: %v", err)
}

// agentAliveAt reports whether a is alive as of now (a testable variant of
// isAlive that doesn't depend on time.Now()).
func agentAliveAt(a Agent, now time.Time) bool {
	return a.DeregisteredAt == nil && now.Sub(a.LastSeenAt) <= aliveWindow
}

// testVerificationAgentPrefix names an agent registered ONLY to exercise
// an endpoint against the real running daemon while verifying a change —
// never a real orchestrator generation. Registering one this way (rather
// than against a throwaway test server) is sometimes the only way to
// prove a fix works against live state, but a name like that has no
// business showing up in the user's Projects view looking like a genuine
// retired generation. Reserve this prefix for exactly that use; anything
// registered under it is excluded from /status's per-project agent lists
// (isTestVerificationAgentName below) automatically, and should still be
// purged afterward (POST /agents/purge) once its job is done rather than
// left to rely solely on the filter.
const testVerificationAgentPrefix = "zz-"

// isTestVerificationAgentName reports whether name looks like a
// verification-only registration per testVerificationAgentPrefix.
func isTestVerificationAgentName(name string) bool {
	return strings.HasPrefix(name, testVerificationAgentPrefix)
}

// tmuxSessionForAgent returns the tmux session to use for a, and whether
// one could be determined at all. Generation-aware agents report their
// session explicitly at register time (Agent.TmuxSession); legacy
// orchestrator-<slug>-named agents that never reported one fall back to
// the old tn-<slug> derivation. Anything else (a non-orchestrator agent
// that never registered a tmux session) has none to report. Used wherever
// a session name used to be derived purely from the agent name: the stuck
// detector, the dashboard's Attach column, and session pages.
func tmuxSessionForAgent(a Agent) (string, bool) {
	if a.TmuxSession != "" {
		return a.TmuxSession, true
	}
	if strings.HasPrefix(a.Name, "orchestrator-") {
		return "tn-" + strings.TrimPrefix(a.Name, "orchestrator-"), true
	}
	return "", false
}

func humanizeAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// truncate shortens s to at most n runes, appending an ellipsis if it was
// cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// taskWikilink returns an Obsidian wikilink for a vault-relative .md task
// path, or "" if path doesn't end in .md.
func taskWikilink(path string) string {
	if !strings.HasSuffix(path, ".md") {
		return ""
	}
	return "[[" + strings.TrimSuffix(path, ".md") + "]]"
}

type dashboardSessionRow struct {
	Name              string
	DisplayName       string // Name, or a wikilink to its session page when enabled
	Project           string
	Alive             bool
	Stuck             bool
	UnackedBacklog    bool
	Draining          bool
	RateLimited       bool
	RateLimitResetsAt string
	ContextPct        *float64
	LastSeen          time.Time
	Queued            int
	Attach            string
}

// buildSessionRows implements the Sessions table rules: status (stuck >
// unackedBacklog > draining > alive > gone precedence, see sessionStatusDisplay),
// queued+delivered counts, tmux attach hints, and omitting deregistered
// agents with nothing pending. When sessionPagesDir is non-empty, each
// row's DisplayName links to that agent's session page.
func buildSessionRows(agents []Agent, messages []Message, now time.Time, sessionPagesDir string, stuckByAgent map[string]stuckInfo) []dashboardSessionRow {
	pendingCount := map[string]int{}
	for _, m := range messages {
		if m.Status == "queued" || m.Status == "delivered" {
			pendingCount[m.To]++
		}
	}

	rows := make([]dashboardSessionRow, 0, len(agents))
	for _, a := range agents {
		if a.DeregisteredAt != nil && pendingCount[a.Name] == 0 {
			continue
		}
		attach := "—"
		if session, ok := tmuxSessionForAgent(a); ok {
			attach = fmt.Sprintf("`tmux attach -t %s`", session)
		}
		si := stuckByAgent[a.Name]
		rows = append(rows, dashboardSessionRow{
			Name:              a.Name,
			DisplayName:       sessionDisplayName(a.Name, sessionPagesDir),
			Project:           a.Project,
			Alive:             agentAliveAt(a, now),
			Stuck:             si.Stuck,
			UnackedBacklog:    si.UnackedBacklog,
			Draining:          !a.Accepting,
			RateLimited:       si.RateLimited,
			RateLimitResetsAt: si.RateLimitResetsAt,
			ContextPct:        a.ContextPct,
			LastSeen:          a.LastSeenAt,
			Queued:            pendingCount[a.Name],
			Attach:            attach,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}

type dashboardPendingRow struct {
	ID       string
	To       string
	Status   string
	Age      string
	Text     string
	Wikilink string
}

// buildPendingRows implements the Pending messages rules: every non-acked
// message, oldest first, with humanized age and truncated text.
func buildPendingRows(messages []Message, now time.Time) []dashboardPendingRow {
	pending := make([]Message, 0, len(messages))
	for _, m := range messages {
		if m.Status != "acked" {
			pending = append(pending, m)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].CreatedAt.Before(pending[j].CreatedAt) })

	rows := make([]dashboardPendingRow, 0, len(pending))
	for _, m := range pending {
		rows = append(rows, dashboardPendingRow{
			ID:       m.ID,
			To:       m.To,
			Status:   m.Status,
			Age:      humanizeAge(now.Sub(m.CreatedAt)),
			Text:     truncate(m.Text, 120),
			Wikilink: taskWikilink(m.TaskPath),
		})
	}
	return rows
}

type dashboardActivityRow struct {
	Time     string
	Agent    string
	ID       string
	Response string
	Wikilink string
}

// buildActivityRows implements the Recent activity rules: last 10 acked
// messages, newest first, with truncated response text.
func buildActivityRows(messages []Message) []dashboardActivityRow {
	acked := make([]Message, 0, len(messages))
	for _, m := range messages {
		if m.Status == "acked" && m.AckedAt != nil {
			acked = append(acked, m)
		}
	}
	sort.Slice(acked, func(i, j int) bool { return acked[i].AckedAt.After(*acked[j].AckedAt) })
	if len(acked) > 10 {
		acked = acked[:10]
	}

	rows := make([]dashboardActivityRow, 0, len(acked))
	for _, m := range acked {
		rows = append(rows, dashboardActivityRow{
			Time:     m.AckedAt.Format("15:04"),
			Agent:    m.To,
			ID:       m.ID,
			Response: truncate(m.Response, 120),
			Wikilink: taskWikilink(m.TaskPath),
		})
	}
	return rows
}

// renderDashboardMarkdown is a pure function from a state snapshot to the
// dashboard note's Markdown content, so it's unit-testable without a
// running server.
func renderDashboardMarkdown(snap dashboardSnapshot, now time.Time, sessionPagesDir string, needsActionTasks []Task, needsActionErr bool, stuckByAgent map[string]stuckInfo) string {
	var b strings.Builder

	b.WriteString("---\ngenerated: true\n---\n\n")
	fmt.Fprintf(&b, "> Auto-generated by `tn serve` — do not edit. Updated: %s\n\n", now.Format("2006-01-02 15:04:05"))

	mrOpenThreads := make(map[string]int, len(snap.MRReviews))
	for path, rev := range snap.MRReviews {
		mrOpenThreads[path] = rev.OpenThreads
	}
	b.WriteString(renderNeedsActionSection(needsActionTasks, needsActionErr, snap.MRStates, mrOpenThreads))

	if !needsActionErr {
		// Passive visibility, not an alert — see statusResponse.Triage's
		// doc comment in serve.go for why. Piggybacks onto a section
		// that's already read rather than adding a new one.
		if triage := computeTriageSummary(needsActionTasks, now); triage.Count > 0 {
			if triage.OldestPath != "" {
				fmt.Fprintf(&b, "_Triage: %d waiting, oldest %s — %s._\n\n",
					triage.Count, humanizeAge(time.Duration(triage.OldestAgeSeconds*float64(time.Second))), taskWikilink(triage.OldestPath))
			} else {
				// Every triage task has an unparseable/missing DateCreated —
				// still real debt, just can't be aged.
				fmt.Fprintf(&b, "_Triage: %d waiting._\n\n", triage.Count)
			}
		}
	}

	b.WriteString("## Sessions\n\n")
	sessions := buildSessionRows(snap.Agents, snap.Messages, now, sessionPagesDir, stuckByAgent)
	if len(sessions) == 0 {
		b.WriteString("_No sessions registered._\n\n")
	} else {
		b.WriteString("| Session | Project | Status | Last seen | Queued | Attach |\n")
		b.WriteString("|---|---|---|---|---|---|\n")
		for _, r := range sessions {
			status := sessionStatusDisplay(r.Alive, r.Stuck, r.UnackedBacklog, r.Draining, r.RateLimited, r.RateLimitResetsAt) + contextPctSuffix(r.ContextPct)
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %d | %s |\n",
				r.DisplayName, r.Project, status, r.LastSeen.Format("15:04:05"), r.Queued, r.Attach)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Pending messages\n\n")
	pending := buildPendingRows(snap.Messages, now)
	if len(pending) == 0 {
		b.WriteString("_No pending messages._\n\n")
	} else {
		for _, p := range pending {
			line := fmt.Sprintf("- `%s` → **%s** (%s, %s): %s", p.ID, p.To, p.Status, p.Age, p.Text)
			if p.Wikilink != "" {
				line += " (" + p.Wikilink + ")"
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
	}

	activity := buildActivityRows(snap.Messages)
	if len(activity) > 0 {
		b.WriteString("## Recent activity\n\n")
		for _, a := range activity {
			line := fmt.Sprintf(`- %s ✅ %s acked %s — "%s"`, a.Time, a.Agent, a.ID, a.Response)
			if a.Wikilink != "" {
				line += " (" + a.Wikilink + ")"
			}
			b.WriteString(line + "\n")
		}
	}

	return b.String()
}
