package tn

import (
	"fmt"
	"log"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// --- Per-session activity pages ---

var sanitizeSessionNameRe = regexp.MustCompile(`[^A-Za-z0-9._ -]`)

// sanitizeSessionFilename turns an agent name into a safe filename stem:
// "/" and any character outside [A-Za-z0-9._ -] become "-".
func sanitizeSessionFilename(name string) string {
	return sanitizeSessionNameRe.ReplaceAllString(name, "-")
}

// sessionDisplayName returns agentName, or (when sessionPagesDir is
// non-empty) a wikilink to that agent's session page aliased to the
// original name, for use in the dashboard's Sessions table.
func sessionDisplayName(agentName, sessionPagesDir string) string {
	if sessionPagesDir == "" {
		return agentName
	}
	folder := filepath.Base(sessionPagesDir)
	return fmt.Sprintf("[[%s/%s|%s]]", folder, sanitizeSessionFilename(agentName), agentName)
}

// formatEventTimestamp renders t as HH:MM, prefixed with the date when t
// isn't on the same day as now.
func formatEventTimestamp(t, now time.Time) string {
	if t.Format("2006-01-02") == now.Format("2006-01-02") {
		return t.Format("15:04")
	}
	return t.Format("2006-01-02 15:04")
}

// sessionEvent is one line in a session page's merged Activity feed.
type sessionEvent struct {
	At       time.Time
	Text     string
	Wikilink string
}

// buildSessionEvents merges an agent's `tn log` activity entries with the
// message events that involve it — received/acked messages addressed to it,
// and messages it sent — newest first, capped at 200.
func buildSessionEvents(agentName string, messages []Message, activity []ActivityEntry) []sessionEvent {
	var events []sessionEvent

	for _, a := range activity {
		if a.Agent != agentName {
			continue
		}
		events = append(events, sessionEvent{At: a.At, Text: "📝 " + a.Text})
	}

	for _, m := range messages {
		wikilink := taskWikilink(m.TaskPath)
		if m.To == agentName {
			switch m.Status {
			case "acked":
				at := m.CreatedAt
				if m.AckedAt != nil {
					at = *m.AckedAt
				}
				events = append(events, sessionEvent{
					At:       at,
					Text:     fmt.Sprintf("✅ acked: %s — response: %s", truncate(m.Text, 120), truncate(m.Response, 120)),
					Wikilink: wikilink,
				})
			case "queued", "delivered":
				events = append(events, sessionEvent{
					At:       m.CreatedAt,
					Text:     fmt.Sprintf("📨 received: %s", truncate(m.Text, 120)),
					Wikilink: wikilink,
				})
			}
		}
		if m.From == agentName {
			events = append(events, sessionEvent{
				At:       m.CreatedAt,
				Text:     fmt.Sprintf("📤 sent to %s: %s", m.To, truncate(m.Text, 120)),
				Wikilink: wikilink,
			})
		}
	}

	sort.Slice(events, func(i, j int) bool { return events[i].At.After(events[j].At) })
	if len(events) > 200 {
		events = events[:200]
	}
	return events
}

// sessionPageCandidates returns the agents that should get a rendered
// session page: alive, or with at least one message (as sender or
// recipient) or activity entry.
func sessionPageCandidates(agents []Agent, messages []Message, activity []ActivityEntry, now time.Time) []Agent {
	hasMessage := map[string]bool{}
	for _, m := range messages {
		if m.To != "" {
			hasMessage[m.To] = true
		}
		if m.From != "" {
			hasMessage[m.From] = true
		}
	}
	hasActivity := map[string]bool{}
	for _, a := range activity {
		hasActivity[a.Agent] = true
	}

	out := make([]Agent, 0, len(agents))
	for _, a := range agents {
		if agentAliveAt(a, now) || hasMessage[a.Name] || hasActivity[a.Name] {
			out = append(out, a)
		}
	}
	return out
}

// renderSessionPageMarkdown is a pure function from one agent's state plus
// its merged event feed to that agent's session page Markdown, so it's
// unit-testable without a running server.
func renderSessionPageMarkdown(a Agent, alive bool, events []sessionEvent, now time.Time, stuck stuckInfo) string {
	var b strings.Builder

	b.WriteString("---\ngenerated: true\n---\n\n")
	fmt.Fprintf(&b, "# %s\n\n", a.Name)

	status := sessionStatusDisplay(alive, stuck.Stuck, stuck.UnackedBacklog, !a.Accepting, stuck.RateLimited, stuck.RateLimitResetsAt) + contextPctSuffix(a.ContextPct)
	fmt.Fprintf(&b, "- Project: %s\n", a.Project)
	fmt.Fprintf(&b, "- Status: %s\n", status)
	fmt.Fprintf(&b, "- Registered: %s\n", formatEventTimestamp(a.RegisteredAt, now))
	fmt.Fprintf(&b, "- Last seen: %s\n", formatEventTimestamp(a.LastSeenAt, now))
	if session, ok := tmuxSessionForAgent(a); ok {
		fmt.Fprintf(&b, "- Attach: `tmux attach -t %s`\n", session)
	}

	if stuck.RateLimited {
		b.WriteString("\n## Waiting: claude.ai usage limit\n\n")
		if stuck.RateLimitResetsAt != "" {
			fmt.Fprintf(&b, "- Resets: %s\n", stuck.RateLimitResetsAt)
		}
		if !stuck.RateLimitSince.IsZero() {
			fmt.Fprintf(&b, "- Waiting since: %s\n", formatEventTimestamp(stuck.RateLimitSince, now))
		}
		b.WriteString("- The session continues automatically at reset; `/login` with another account in any Claude session continues it now.\n")
	}

	if stuck.Stuck {
		b.WriteString("\n## Stuck: permission prompt\n\n")
		b.WriteString("```\n" + stuck.Excerpt + "\n```\n")
	}

	b.WriteString("\n## Activity\n\n")
	if len(events) == 0 {
		b.WriteString("_No activity yet._\n")
	} else {
		for _, e := range events {
			line := fmt.Sprintf("- %s %s", formatEventTimestamp(e.At, now), e.Text)
			if e.Wikilink != "" {
				line += " (" + e.Wikilink + ")"
			}
			b.WriteString(line + "\n")
		}
	}

	return b.String()
}

// renderSessionPages snapshots state under the lock, renders outside the
// lock, and writes one file per candidate agent atomically. Write failures
// are logged once per distinct error and never crash the daemon.
func (s *Server) renderSessionPages() {
	s.mu.Lock()
	snap := s.snapshotLocked()
	dir := s.sessionPagesDir
	s.mu.Unlock()

	if dir == "" {
		return
	}

	now := time.Now()
	stuckByAgent := s.stuck.snapshot()
	for _, a := range sessionPageCandidates(snap.Agents, snap.Messages, snap.Activity, now) {
		events := buildSessionEvents(a.Name, snap.Messages, snap.Activity)
		md := renderSessionPageMarkdown(a, agentAliveAt(a, now), events, now, stuckByAgent[a.Name])
		path := filepath.Join(dir, sanitizeSessionFilename(a.Name)+".md")
		if err := atomicWriteFile(path, []byte(md)); err != nil {
			s.logSessionPageErrOnce(err)
		}
	}
}

func (s *Server) logSessionPageErrOnce(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg := err.Error()
	if s.lastSessionPageErr == msg {
		return
	}
	s.lastSessionPageErr = msg
	log.Printf("serve: failed to write session page: %v", err)
}
