package tn

import (
	"fmt"
	"log"
	"os"
	"time"
)

// --- Due-task scanner ---

const (
	dueScannerInterval     = 10 * time.Minute
	dueScannerInitialDelay = 30 * time.Second
)

// buildDueTasksQuery builds the FilterQuery for the due-task scanner: open,
// non-archived, tagged "claude", scheduled on or before today, not blocked
// by an incomplete dependency (dependencies.isBlocked is-not-checked —
// verified live against the real API, see buildListQuery), not parked in a
// human-attention status (triage/needs-input/review — see
// humanAttentionStatuses), and not already in-progress — that last one
// matters just as much as excluding done: without it, a recurring task
// picked up by an agent would get re-assigned on its next scheduled
// occurrence while still being worked (see assignableStatuses, the same
// fix applied to webhook assignment routing).
func buildDueTasksQuery(today string) FilterGroup {
	return FilterGroup{
		Type:        "group",
		ID:          "root",
		Conjunction: "and",
		Children: []FilterCondition{
			{Type: "condition", ID: "archived", Property: "archived", Operator: "is-not-checked"},
			{Type: "condition", ID: "completed", Property: "status.isCompleted", Operator: "is-not-checked"},
			{Type: "condition", ID: "not-triage", Property: "status", Operator: "is-not", Value: "triage"},
			{Type: "condition", ID: "not-needs-input", Property: "status", Operator: "is-not", Value: "needs-input"},
			{Type: "condition", ID: "not-review", Property: "status", Operator: "is-not", Value: "review"},
			{Type: "condition", ID: "not-in-progress", Property: "status", Operator: "is-not", Value: "in-progress"},
			{Type: "condition", ID: "not-blocked", Property: "dependencies.isBlocked", Operator: "is-not-checked"},
			{Type: "condition", ID: "tag-claude", Property: "tags", Operator: "contains", Value: "claude"},
			{Type: "condition", ID: "scheduled", Property: "scheduled", Operator: "is-on-or-before", Value: today},
		},
		SortKey:       "due",
		SortDirection: "asc",
		GroupKey:      "none",
	}
}

// unblockPassTaskCap bounds per-pass processing, same convention as the
// other reconciler passes.
const unblockPassTaskCap = 50

// buildUnblockedDependentsQuery builds the FilterQuery for the unblock
// pass, triggered when a task completes: open (non-archived, not
// status-completed), claude-tagged, has at least one blockedBy entry, and
// TaskNotes itself now considers it startable (dependencies.isBlocked
// is-not-checked) — i.e. a dependency-bearing task whose blocker(s) just
// cleared.
func buildUnblockedDependentsQuery() FilterGroup {
	return FilterGroup{
		Type:        "group",
		ID:          "root",
		Conjunction: "and",
		Children: []FilterCondition{
			{Type: "condition", ID: "archived", Property: "archived", Operator: "is-not-checked"},
			{Type: "condition", ID: "completed", Property: "status.isCompleted", Operator: "is-not-checked"},
			{Type: "condition", ID: "tag-claude", Property: "tags", Operator: "contains", Value: "claude"},
			{Type: "condition", ID: "has-blockers", Property: "blockedBy", Operator: "is-not-empty"},
			{Type: "condition", ID: "not-blocked", Property: "dependencies.isBlocked", Operator: "is-not-checked"},
		},
		SortKey:       "due",
		SortDirection: "asc",
		GroupKey:      "none",
	}
}

// runUnblockPass queries for claude-tagged, dependency-bearing tasks that
// are now startable and routes an assignment for each one that isn't
// already parked for the user, already in-progress, or already has a
// pending assignment — making "finish A → auto-assign B" work without
// waiting for the 10-minute due-task scanner to notice. Tolerates API
// failure: logs and returns, no retry (nothing here is otherwise lost —
// the due-task scanner and webhook-miss reconciler both independently
// cover the same tasks on their own cadence).
func (s *Server) runUnblockPass(completedTitle string) {
	if s.tnClient == nil {
		return
	}
	tasks, err := s.tnClient.QueryTasksRaw(buildUnblockedDependentsQuery())
	if err != nil {
		log.Printf("serve: unblock pass: TaskNotes API query failed: %v", err)
		return
	}
	if len(tasks) > unblockPassTaskCap {
		log.Printf("serve: unblock pass: %d candidate tasks exceeds cap %d, processing the first %d", len(tasks), unblockPassTaskCap, unblockPassTaskCap)
		tasks = tasks[:unblockPassTaskCap]
	}
	for _, task := range tasks {
		s.routeUnblockedDependent(task, completedTitle)
	}
}

// routeUnblockedDependent routes one now-startable dependent task, unless
// it's parked in a human-attention status, already in-progress, or already
// has a pending ("Task assigned:") message.
func (s *Server) routeUnblockedDependent(task Task, completedTitle string) {
	slug := routingSlugForTask(task)
	if slug == "" {
		return
	}

	s.mu.Lock()
	// AssignedTasks additionally covers a multi-blocker task: if two
	// blockers complete separately, two runUnblockPass calls can both find
	// this same still-open, still-unblocked task — see markAssignedLocked.
	_, alreadyAssignedThisEpisode := s.state.AssignedTasks[task.Path]
	skip := humanAttentionStatuses[task.Status] || task.Status == "in-progress" || alreadyAssignedThisEpisode || s.hasPendingAssignmentLocked(task.Path)
	if !skip {
		s.markAssignedLocked(task.Path, time.Now().Format("2006-01-02"))
		s.saveLocked()
	}
	s.mu.Unlock()
	if skip {
		return
	}

	text := fmt.Sprintf("Task assigned: %s (%s) [unblocked by %s]", task.Title, task.Path, completedTitle)
	// A task reaching this pass with role owner has, by construction, just
	// had its last blockedBy member complete (that's what unblocked it) —
	// call that out explicitly, since routeUnblockedDependent is the ONE
	// place this specific "all members done" moment is known; the owner
	// suffix from appendIntegrationBranchSuffix alone doesn't say WHY it's
	// being assigned now.
	if integrationRoleFor(task) == "owner" {
		text = "All members complete — " + text
	}
	s.dispatchMessage(sendRequest{
		Project:  slug,
		TaskPath: task.Path,
		Text:     appendIntegrationBranchSuffix(text, integrationBranchFor(task), integrationRoleFor(task)),
	})
}

// startDueTaskScanner launches a background goroutine running two passes on
// the same cadence (once ~30s after startup, then every 10 minutes):
//  1. scanDueTasksRemote — open, claude-tagged tasks scheduled on or before
//     today, routing an assignment for each one not already routed today
//     (recurrence/scheduled-date rollover never touch the note file, so
//     webhooks don't fire when a new occurrence comes due).
//  2. reconcileWebhookMissesRemote — recently-modified tasks, re-run through
//     the exact same routing pipeline webhooks use (catches deliveries
//     TaskNotes fired while the daemon was mid-restart or otherwise missed;
//     TaskNotes doesn't retry webhook deliveries).
//
// TaskNotes being unreachable is logged once per outage and retried on the
// next tick, never fatal. Disabled entirely when TN_NO_SCANNER=1 (tests and
// smoke runs that start the real daemon should set this).
func (s *Server) startDueTaskScanner(client *Client) {
	if os.Getenv("TN_NO_SCANNER") == "1" {
		return
	}
	go func() {
		time.Sleep(dueScannerInitialDelay)
		s.scanDueTasksRemote(client)
		s.reconcileWebhookMissesRemote(client)

		ticker := time.NewTicker(dueScannerInterval)
		defer ticker.Stop()
		for range ticker.C {
			s.scanDueTasksRemote(client)
			s.reconcileWebhookMissesRemote(client)
		}
	}()
}

// scanDueTasksRemote fetches due tasks from the TaskNotes API and runs one
// scanner pass. A fetch failure is logged once per outage (reset on the
// next success, so a later outage logs again) and otherwise ignored.
func (s *Server) scanDueTasksRemote(client *Client) {
	today := time.Now().Format("2006-01-02")
	tasks, err := client.QueryTasks(buildDueTasksQuery(today))
	if err != nil {
		if !s.dueScannerWarnedOnce {
			log.Printf("serve: due-task scanner: TaskNotes API not reachable, will keep retrying: %v", err)
			s.dueScannerWarnedOnce = true
		}
		return
	}
	s.dueScannerWarnedOnce = false
	s.scanDueTasksOnce(tasks, today)
}

// scanDueTasksOnce is one scanner pass over pre-fetched tasks, factored out
// so tests can exercise it without HTTP.
func (s *Server) scanDueTasksOnce(tasks []Task, today string) {
	for _, task := range tasks {
		s.processDueTask(task, today)
	}
}

// processDueTask resolves a due task's routing slug and, unless it should
// be skipped, records the occurrence and dispatches an assignment. Message
// dispatch always happens outside s.mu (dispatchMessage locks internally).
func (s *Server) processDueTask(task Task, today string) {
	slug := routingSlugForTask(task)

	s.mu.Lock()
	if slug == "" {
		s.mu.Unlock()
		return
	}
	// Skip unroutable projects entirely (unlike webhook routing, which
	// always queues to orchestrator-<slug> with a warning): a scanner pass
	// runs unattended every 10 minutes, so silently accumulating assignments
	// for a typo'd or unconfigured project would just strand work nobody is
	// watching.
	hasAliveAcceptingAgent := s.aliveAcceptingAgentForProjectLocked(slug) != nil
	cfg := s.projectConfigFor(slug)
	if !cfg.AutoSpawn && !hasAliveAcceptingAgent {
		s.mu.Unlock()
		return
	}
	// A new occurrence (today's AssignedOccurrences entry doesn't match)
	// starts a NEW episode — clear any stale AssignedTasks marker left
	// over from a PREVIOUS occurrence first, so a recurring task whose
	// due date just rolled over isn't blocked by yesterday's marker (it
	// can sit in "open" across day boundaries without ever leaving
	// assignableStatuses, so nothing else would have cleared it).
	if s.state.AssignedOccurrences[task.Path] != today {
		delete(s.state.AssignedTasks, task.Path)
	}
	if s.state.AssignedOccurrences[task.Path] == today {
		s.mu.Unlock()
		return
	}
	// AssignedTasks is ALSO checked here now, not just AssignedOccurrences:
	// this is the actual fix for the live leak. Before it, a task assigned
	// via the webhook/reconciler path got an AssignedTasks marker but no
	// AssignedOccurrences entry (markAssignedLocked only wrote one ledger
	// per call site), so the very next scanner pass — checking only
	// AssignedOccurrences — saw no record for today and re-assigned it
	// (confirmed live: three tasks each assigned twice, second assignment
	// for all three at the same scanner-pass timestamp, neither task
	// modified in between). markAssignedLocked now writes both ledgers
	// together from every call site, so this check and the occurrence
	// check above are consistent going forward; this direct check is
	// belt-and-suspenders for any state left over from before this fix.
	if _, alreadyAssignedThisEpisode := s.state.AssignedTasks[task.Path]; alreadyAssignedThisEpisode {
		s.mu.Unlock()
		return
	}
	if s.hasPendingAssignmentLocked(task.Path) {
		s.mu.Unlock()
		return
	}
	s.markAssignedLocked(task.Path, today)
	s.saveLocked()
	s.mu.Unlock()

	s.dispatchMessage(sendRequest{
		Project:  slug,
		TaskPath: task.Path,
		Text:     appendIntegrationBranchSuffix(fmt.Sprintf("Task assigned: %s (%s) [scheduled %s]", task.Title, task.Path, task.Scheduled), integrationBranchFor(task), integrationRoleFor(task)),
	})
}
