package tn

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

// --- Orphaned-task safety net ---

// triggerOrphanCheckIfNeeded marks agentName's orphan-death handling done
// for its CURRENT lifecycle (idempotent — a repeat call before the agent
// next registers is a no-op, so a re-retire or a repeated liveness-check
// tick never double-fires) and, only the first time, checks for and
// notifies about any in-progress tasks it still owns, and reassigns/
// archives any pending messages still addressed to it (see
// reassignStrandedMessagesForDeadAgent — for a clean retire this is
// normally a no-op, since handleRetire already reassigned everything
// inline before calling here; it only does real work for a dirty death,
// which has no other code path that ever does). Used by both
// handleRetire's success path (a clean retirement) and
// checkOrphanedDeathsOnce (died without retiring).
func (s *Server) triggerOrphanCheckIfNeeded(agentName, project string) {
	s.mu.Lock()
	a, ok := s.state.Agents[agentName]
	if !ok || !a.OrphanHandledAt.IsZero() {
		s.mu.Unlock()
		return
	}
	a.OrphanHandledAt = time.Now()
	s.saveLocked()
	s.mu.Unlock()

	s.checkOrphanedTasks(agentName, project)
	s.reassignStrandedMessagesForDeadAgent(agentName, project)
}

// hasLiveDeclaredWorkerLocked reports whether ANY alive agent currently
// has a declared worker (`tn worker start`, not yet ended) for path — the
// orphan-reset guard's second half: resetting a task that's actively
// being worked by a live-declared subtask would be worse than the
// ownership-pileup bug this exists to fix. Checks every agent's Workers
// list, not just one candidate owner — a worker can be declared under a
// DIFFERENT agent than whoever originally owned the task (an adopting
// agent that never issued a fresh "Task assigned:" message for it,
// exactly the gap this whole mechanism is patching around). Must be
// called with s.mu held.
func (s *Server) hasLiveDeclaredWorkerLocked(path string) bool {
	now := time.Now()
	for name, entries := range s.state.Workers {
		a, ok := s.state.Agents[name]
		if !ok || !agentAliveAt(*a, now) {
			continue
		}
		for _, e := range entries {
			if e.TaskPath == path {
				return true
			}
		}
	}
	return false
}

// lastActivityForAgentLocked returns the text of agentName's most recent
// activity log entry, or "" if it never logged anything. Most `tn log`
// entries don't carry a TaskPath (see ActivityEntry.TaskPath's doc
// comment — only the two MR-transition call sites do), so this is
// AGENT-scoped, not task-scoped: the closest available signal to "what
// was I doing right before I died," used by buildOrphanResetNote. Must
// be called with s.mu held.
func (s *Server) lastActivityForAgentLocked(agentName string) string {
	var latest *ActivityEntry
	for _, e := range s.state.Activity {
		if e.Agent != agentName {
			continue
		}
		if latest == nil || e.At.After(latest.At) {
			latest = e
		}
	}
	if latest == nil {
		return ""
	}
	return latest.Text
}

// buildOrphanResetNote composes the note appended when a task is reset
// to open after its owner died/retired: the "not abandoned, not
// drifted" cause line (reconcileSleepKilledAgent's same voice), PLUS
// whatever in-flight pointers the daemon already holds — the task's own
// mr/integration-branch/jira custom properties, and the dead agent's
// last logged activity entry, if any. This is what stops the next
// picker from silently redoing work that's already sitting on a branch,
// or colliding with it — frontmatter alone survives the reset, but it's
// easy to miss on a task that no longer LOOKS like it has anything
// attached, since the reset is what clears the in-progress signal that
// would otherwise prompt a look.
//
// Deliberately does NOT go hunting the filesystem for an orphaned
// worktree or uncommitted changes — only surfaces what's already known
// to the daemon, and says so explicitly: none of this is verified
// against what the dead worker actually left behind uncommitted.
func (s *Server) buildOrphanResetNote(agentName string, task Task, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Reset to open: worker %s died/retired at %s without completing. Not abandoned, not drifted.",
		agentName, now.Format("2006-01-02 15:04"))

	var pointers []string
	if mr := task.CustomProperties["mr"]; mr != "" {
		pointers = append(pointers, "MR: "+mr)
	}
	if branch := integrationBranchFor(task); branch != "" {
		pointers = append(pointers, "integration branch: "+branch)
	}
	if jira := task.CustomProperties["jira"]; jira != "" {
		pointers = append(pointers, "Jira: "+jira)
	}
	if ncKey := task.CustomProperties["jira-nc"]; ncKey != "" {
		pointers = append(pointers, "Jira NC: "+ncKey)
	}
	s.mu.Lock()
	lastActivity := s.lastActivityForAgentLocked(agentName)
	s.mu.Unlock()
	if lastActivity != "" {
		pointers = append(pointers, "last logged activity: "+lastActivity)
	}
	if len(pointers) > 0 {
		fmt.Fprintf(&b, " Known pointers: %s.", strings.Join(pointers, "; "))
	}

	b.WriteString(" Any uncommitted or unpushed work from the dead worker may exist and is unverified — check the pointers above before redoing anything.")
	return b.String()
}

// resetOrphanedInProgressTasksForAgent resets every in-progress task
// still owned by agentName (per ownedTaskPathsLocked/taskOwnerLocked)
// back to open, each with a bridge-attributed history note (see
// buildOrphanResetNote — same "not abandoned, not drifted" voice as the
// sleep-death recovery path, plus whatever MR/branch/Jira/last-activity
// pointers the daemon already holds) via bridgeTransitionTask, a single
// GET+PUT round trip per task.
//
// GUARDED, per task, immediately before writing: skipped (left entirely
// alone) if ownership has since moved to someone else (a fresh "Task
// assigned:" message landed — an adopting agent did the right thing) OR
// a live agent currently has a declared worker on it (hasLiveDeclared
// WorkerLocked) — resetting live work out from under it would be worse
// than the bug this fixes. Returns the reset and skipped labels
// ("Title (path)") separately so callers can log/notify accordingly.
// Idempotent by construction: once reset, a task's status is no longer
// "in-progress", so a repeat call for the same agentName (the forward
// path is one-shot per lifecycle anyway — see triggerOrphanCheckIfNeeded
// — and the startup sweep re-derives its candidate list fresh each time)
// finds nothing left to do. s.tnClient must be non-nil; callers check
// that first.
func (s *Server) resetOrphanedInProgressTasksForAgent(agentName string) (reset, skipped []string) {
	s.mu.Lock()
	paths := s.ownedTaskPathsLocked(agentName)
	s.mu.Unlock()

	for _, p := range paths {
		task, err := s.tnClient.GetTask(p)
		if err != nil {
			log.Printf("serve: orphan reset: failed to fetch %s for %s: %v", p, agentName, err)
			continue
		}
		if task.Status != "in-progress" {
			continue
		}

		s.mu.Lock()
		stillOwned := s.taskOwnerLocked(p) == agentName
		hasLiveWorker := s.hasLiveDeclaredWorkerLocked(p)
		s.mu.Unlock()

		label := fmt.Sprintf("%s (%s)", task.Title, p)
		if !stillOwned || hasLiveWorker {
			skipped = append(skipped, label)
			continue
		}

		note := s.buildOrphanResetNote(agentName, task, time.Now())
		if _, err := bridgeTransitionTask(s.tnClient, p, "open", note); err != nil {
			log.Printf("serve: orphan reset: failed to reset %s after %s's death: %v", p, agentName, err)
			continue
		}
		reset = append(reset, label)
	}
	return reset, skipped
}

// checkOrphanedTasks resets agentName's still-in-progress owned tasks to
// open (resetOrphanedInProgressTasksForAgent — the guarded reset is the
// PRIMARY mechanism, not a message asking someone else to do it: the
// original message-only design is what let a week of orphaned tasks pile
// up undetected — see SPEC-note-layout.md-adjacent history and the
// checkOrphanedTasks investigation this replaced). Records an activity
// entry either way, and — only when something was actually reset — sends
// one FYI message to the project so there's a "here's what happened"
// narrative, explicitly framed as context rather than an action item:
// the reset has ALREADY happened by the time this message is read, so
// nothing downstream depends on anyone reading it. No-op if s.tnClient is
// nil or agentName owns nothing still in-progress.
func (s *Server) checkOrphanedTasks(agentName, project string) {
	if s.tnClient == nil {
		return
	}
	reset, skipped := s.resetOrphanedInProgressTasksForAgent(agentName)
	if len(reset) == 0 && len(skipped) == 0 {
		return
	}

	log.Printf("serve: %s left %d orphaned in-progress task(s): %d reset to open, %d left alone (already adopted or have a live worker)",
		agentName, len(reset)+len(skipped), len(reset), len(skipped))

	var activityText string
	switch {
	case len(reset) > 0 && len(skipped) > 0:
		activityText = fmt.Sprintf("Orphaned tasks from %s — reset to open: %s (left alone, already adopted or has a live worker: %s)",
			agentName, strings.Join(reset, "; "), strings.Join(skipped, "; "))
	case len(reset) > 0:
		activityText = fmt.Sprintf("Orphaned tasks from %s reset to open: %s", agentName, strings.Join(reset, "; "))
	default:
		activityText = fmt.Sprintf("Orphaned tasks from %s already adopted or have a live worker, nothing reset: %s", agentName, strings.Join(skipped, "; "))
	}

	// Attributed to "bridge", not agentName: appendActivityLocked's
	// touchAgentLocked side effect bumps LastSeenAt for whoever it's
	// logged against — logging against the dead agent's OWN name would
	// revive it (isAlive again) right before the dispatchMessage call
	// below resolves a target, looping the notification about its own
	// death back to the dead agent instead of a live one.
	s.mu.Lock()
	s.appendActivityLocked("bridge", activityText, "")
	s.saveLocked()
	s.mu.Unlock()
	s.triggerRenders()

	if project == "" || len(reset) == 0 {
		return
	}
	s.dispatchMessage(sendRequest{
		Project: project,
		Text: fmt.Sprintf(
			"FYI, not an action item: %s died/retired with %d in-progress task(s) still assigned; already reset to open automatically, no adoption needed: %s",
			agentName, len(reset), strings.Join(reset, "; "),
		),
		Informational: true,
	})
}

// deadConfirmTicks is how many consecutive checkOrphanedDeathsOnce passes
// must observe an agent as not-alive (Agent.ConsecutiveDeadTicks) before
// it is treated as dead-without-retiring — the same "one miss is not
// enough, two in a row is" discipline that fixed the stuck detector's own
// false positives (see stuckPersistenceBackstopTicks and the
// fastConfirm/backstopConfirm split). A single missed aliveWindow is
// common for an agent doing real synchronous work between polls (see
// orphanSweepEnabled's doc comment for the incident this addresses); two
// consecutive misses is a materially stronger signal.
const deadConfirmTicks = 2

// checkOrphanedDeathsOnce is one pass detecting agents that died WITHOUT
// retiring — not alive for deadConfirmTicks consecutive passes, and never
// cleanly deregistered/retired (DeregisteredAt nil) — and running the
// orphan-task check for each. Runs on the same cadence as the stuck
// detector (see startStuckDetector) since that's where liveness is
// already being observed every tick; no separate ticker needed for
// something this lightweight.
//
// Two layered gates, not one, because a global timer alone was verified
// insufficient (2026-08-25 incident: a restart flagged 11 of 11 registered
// agents, including the only genuinely live one — none had polled yet).
//
//  1. Daemon-level: for aliveWindow after THIS process started, the
//     detector is a complete no-op — a LastSeenAt predating the restart is
//     absence of signal, not evidence of death, for every agent equally.
//  2. Per-agent: an agent absent from s.seenSinceStart (no authenticated
//     action observed yet THIS lifetime) is UNKNOWN, not dead, and stays
//     excluded — no streak accumulation, no candidacy — until its first
//     observed action, however long that takes. Unknown must never
//     authorise a destructive action; only once an agent has proven it CAN
//     report in does a subsequent silence become evidence.
//
// Skipping entirely rather than just not acting also means
// ConsecutiveDeadTicks doesn't start accumulating against an agent that
// hasn't had a fair chance to be seen.
func (s *Server) checkOrphanedDeathsOnce() {
	if time.Since(s.startedAt) < aliveWindow {
		return
	}

	now := time.Now()
	s.mu.Lock()
	var candidates []Agent
	var unknownStale []string // registered, LastSeenAt looks stale, but never observed THIS lifetime
	changed := false
	for _, a := range s.state.Agents {
		if !s.seenSinceStart[a.Name] {
			if a.DeregisteredAt == nil && !agentAliveAt(*a, now) {
				unknownStale = append(unknownStale, a.Name)
			}
			continue // unknown this lifecycle, not dead — no streak, no candidacy
		}
		if agentAliveAt(*a, now) {
			if a.ConsecutiveDeadTicks != 0 {
				a.ConsecutiveDeadTicks = 0
				changed = true
			}
			continue
		}
		a.ConsecutiveDeadTicks++
		changed = true
		if a.DeregisteredAt != nil || !a.OrphanHandledAt.IsZero() {
			continue // already handled or cleanly gone: streak tracked, not actionable
		}
		if a.ConsecutiveDeadTicks >= deadConfirmTicks {
			candidates = append(candidates, *a)
		}
	}
	if changed {
		s.saveLocked()
	}
	s.mu.Unlock()

	// Observability requested explicitly after the 2026-08-25 incident:
	// this is the regression detector. If a future restart logs a live
	// agent here, the per-agent gate has broken — it should be silent
	// (or absent) for every agent that's actually alive and just hasn't
	// polled yet within a normal window.
	if len(unknownStale) > 0 {
		sort.Strings(unknownStale)
		log.Printf("serve: orphan check: %d agent(s) look stale but have not been observed this daemon lifetime yet — treated as unknown, not dead, no action taken: %s", len(unknownStale), strings.Join(unknownStale, ", "))
	}

	if !orphanSweepEnabled() {
		if len(candidates) > 0 {
			names := make([]string, len(candidates))
			for i, a := range candidates {
				names[i] = a.Name
			}
			log.Printf("serve: orphan sweep DISABLED (set TN_ENABLE_ORPHAN_SWEEP=1 to enable) — would have processed %d dead-without-retiring candidate(s), left alone: %s", len(candidates), strings.Join(names, ", "))
		}
		return
	}

	for _, a := range candidates {
		s.triggerOrphanCheckIfNeeded(a.Name, a.Project)
	}
}
