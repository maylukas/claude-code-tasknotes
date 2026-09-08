package tn

import (
	"fmt"
	"log"
	"strings"
	"time"
)

// --- Stranded-message sweep ---

// orchestratorQueueName returns the logical "orchestrator-<slug>" fallback
// recipient for project — the queue name a fresh generation retargets to
// when it registers (retargetLogicalQueueLocked) — or "" if project is
// empty. MUST be the only place that composes this shape: a bare
// "orchestrator-" with nothing after the dash matches no register call
// and no reassignment sweep ever will, so it is a dead letter the moment
// it is created. Live incident (2026-08-27): exactly two messages were
// found permanently stranded on that literal malformed name, composed by
// an earlier, unguarded version of this expression when project was "".
func orchestratorQueueName(project string) string {
	slug := strings.ToLower(strings.TrimSpace(project))
	if slug == "" {
		return ""
	}
	return "orchestrator-" + slug
}

// pickReassignmentTargetLocked returns where a pending message addressed
// to a gone (retired or dead) agent for project should move: a live
// accepting agent for the SAME project if one exists, otherwise the
// logical queue a future registration will pick up — or "" if project is
// empty, meaning neither exists and the caller must NOT invent one (see
// orchestratorQueueName). Must be called with s.mu held.
func (s *Server) pickReassignmentTargetLocked(project string) string {
	if target := s.pickAcceptingAgentForAssignmentLocked(project); target != nil {
		return target.Name
	}
	return orchestratorQueueName(project)
}

// strandedMsgCandidate is a point-in-time, immutable snapshot of one
// pending message that reassignStrandedMessagesForDeadAgent or
// sweepUnroutableMessagesOnce found needing action — taken under s.mu so
// the later,
// unlocked TaskNotes lookup never reads a *Message field a concurrent
// ack/poll could be mutating (the exact race class this daemon's own
// -race suite has caught before; see CLAUDE.md's Build/test section).
type strandedMsgCandidate struct {
	id               string
	to               string
	taskPath         string
	createdAt        time.Time
	deadAgentProject string // "" if 'to' isn't a known (dead) agent at all
	knownDeadAgent   bool
}

// reassignStrandedMessagesForDeadAgent enforces the invariant a 2026-08-27
// root-cause analysis established: NO QUEUED OR DELIVERED MESSAGE MAY
// PERMANENTLY TARGET A NON-ALIVE AGENT. handleRetire already reassigns
// pending mail on a CLEAN retire (see its own doc comment, which documents
// the incident that motivated it), but that fix lives only inside
// handleRetire — a generation that dies WITHOUT calling it, the common
// case, strands its mail forever. Live evidence, verified directly against
// production state: 31 queued/delivered messages sat on dead or
// never-valid recipients, including seven "Task assigned:" directives up
// to six days old (five were done anyway, via other means; two were still
// genuinely open; one had no corresponding task on the board at all).
//
// Called ONLY from triggerOrphanCheckIfNeeded, right alongside
// checkOrphanedTasks — deliberately NOT an independent periodic sweep with
// its own liveness check. agentName arrives here already CONFIRMED dead
// (deadConfirmTicks consecutive misses, gated by the startup grace period
// — see checkOrphanedDeathsOnce) or cleanly retired; this function adds no
// liveness judgement of its own on top of that. A second, independent
// "is it really dead" check here would be redundant at best and, if it
// ever disagreed with the confirmed signal, would reintroduce exactly the
// false-positive risk (reassigning live work — see the g84221 duplicate-MR
// incident referenced elsewhere in this file) that the confirmation
// discipline exists to prevent. (Messages addressed to a name that was
// NEVER a valid agent or queue at all — no liveness question to confirm in
// the first place — are handled separately by sweepUnroutableMessagesOnce.)
//
// Deliberately does NOT bulk-deliver stale assignments as if new (see
// reconcileStrandedMessage: archives rather than reassigns a message whose
// referenced task is already done) — dumping six-day-old directives on a
// live orchestrator would recreate the exact duplicate-work problem this
// project keeps paying for. Shares TN_ENABLE_ORPHAN_SWEEP with
// checkOrphanedDeathsOnce implicitly: triggerOrphanCheckIfNeeded's dirty-
// death caller is only ever reached once that gate is open.
func (s *Server) reassignStrandedMessagesForDeadAgent(agentName, project string) {
	if s.tnClient == nil {
		return
	}

	s.mu.Lock()
	var candidates []strandedMsgCandidate
	for _, m := range s.state.Messages {
		if m.To != agentName || (m.Status != "queued" && m.Status != "delivered") {
			continue
		}
		candidates = append(candidates, strandedMsgCandidate{
			id: m.ID, to: m.To, taskPath: m.TaskPath, createdAt: m.CreatedAt,
			deadAgentProject: project, knownDeadAgent: true,
		})
	}
	s.mu.Unlock()

	if len(candidates) == 0 {
		return
	}

	var reassigned, archived int
	for _, c := range candidates {
		s.reconcileStrandedMessage(c, &reassigned, &archived)
	}
	if reassigned > 0 || archived > 0 {
		log.Printf("serve: %s died/retired with %d pending message(s): %d reassigned, %d archived", agentName, len(candidates), reassigned, archived)
		s.mu.Lock()
		s.saveLocked()
		s.mu.Unlock()
		s.triggerRenders()
	}
}

// sweepUnroutableMessagesOnce is reassignStrandedMessagesForDeadAgent's
// complement: it finds pending messages addressed to a name that was NEVER
// a valid agent or project queue at all — not a liveness question (there
// is no agent record to be alive or dead), so it runs independently, on
// its own periodic tick, with no dead-confirmation discipline to reuse or
// need. Live evidence: 18 of the 31 stranded messages from the 2026-08-27
// incident were addressed to "claude/setup", an identity that was never a
// real registered agent.
//
// A name matching no agent record is legitimate ONLY as the logical
// "orchestrator-<slug>" queue for a project this daemon actually knows
// about (an autoSpawn config entry, or any agent — dead or alive — ever
// registered for it): that's normal in-flight queueing ahead of a fresh
// spawn (retargetLogicalQueueLocked eventually drains it), not a bug.
// Everything else — including the malformed bare "orchestrator-" that
// orchestratorQueueName now refuses to compose, but which may already
// exist in persisted state from before this fix — has no known project to
// reassign to, so reconcileStrandedMessage can only ever archive it.
//
// Gated behind TN_ENABLE_ORPHAN_SWEEP like the rest of this family:
// archiving is a real, visible action on real messages, and this repo's
// established posture is to keep anything with a confirmed false-positive
// history (or, here, the same risk PROFILE as siblings that do) opt-in
// until proven safe in practice — see orphanSweepEnabled's own doc
// comment.
func (s *Server) sweepUnroutableMessagesOnce() {
	if s.tnClient == nil {
		return
	}

	s.mu.Lock()
	knownProjects := map[string]bool{}
	for _, a := range s.state.Agents {
		if a.Project != "" {
			knownProjects[strings.ToLower(a.Project)] = true
		}
	}
	for k := range s.config.Projects {
		knownProjects[strings.ToLower(k)] = true
	}

	var candidates []strandedMsgCandidate
	for _, m := range s.state.Messages {
		if m.Status != "queued" && m.Status != "delivered" {
			continue
		}
		if _, ok := s.state.Agents[m.To]; ok {
			continue // a known agent, dead or alive — reassignStrandedMessagesForDeadAgent's job, not this sweep's
		}
		if slug, ok := strings.CutPrefix(m.To, "orchestrator-"); ok && slug != "" && knownProjects[strings.ToLower(slug)] {
			continue // legitimate pending-spawn queue
		}
		candidates = append(candidates, strandedMsgCandidate{id: m.ID, to: m.To, taskPath: m.TaskPath, createdAt: m.CreatedAt})
	}
	s.mu.Unlock()

	if len(candidates) == 0 {
		return
	}

	if !orphanSweepEnabled() {
		log.Printf("serve: unroutable-message sweep DISABLED (set TN_ENABLE_ORPHAN_SWEEP=1 to enable) — would have archived %d message(s) addressed to a never-valid recipient, left alone", len(candidates))
		return
	}

	var reassigned, archived int
	for _, c := range candidates {
		s.reconcileStrandedMessage(c, &reassigned, &archived)
	}
	if archived > 0 {
		log.Printf("serve: unroutable-message sweep: archived %d message(s) addressed to a name that was never a valid agent or queue", archived)
		s.mu.Lock()
		s.saveLocked()
		s.mu.Unlock()
		s.triggerRenders()
	}
}

// reconcileStrandedMessage decides, for one message already established as
// targeting a non-alive/unroutable recipient, whether to REASSIGN it to a
// live agent (or queue it for a future one) or ARCHIVE it — never silently
// leave it stranded, never silently drop it without a record.
//
// The decision: a message with no TaskPath (nothing to check staleness
// against — a reply, a note, an ad hoc send) always reassigns, same
// default handleRetire already uses — the lowest-risk choice with no
// evidence either way. A message WITH a TaskPath reassigns UNLESS the
// task is CONFIRMED done (fetched successfully AND Status=="done") — a
// fetch error or any other status falls through to reassign, per the same
// "absence of signal is not signal of absence" discipline used throughout
// this daemon (see e.g. checkOrphanedDeathsOnce's own doc comment): an
// UNKNOWN state must never authorize the destructive (archiving) outcome.
// A message with no known project to reassign to at all (an unroutable
// recipient — c.knownDeadAgent false and c.deadAgentProject "") can only
// ever archive; there is nowhere to send it.
func (s *Server) reconcileStrandedMessage(c strandedMsgCandidate, reassigned, archived *int) {
	if c.taskPath != "" {
		if task, err := s.tnClient.GetTask(c.taskPath); err == nil && task.Status == "done" {
			s.archiveStrandedMessage(c.id, fmt.Sprintf(
				"task %s is done — reassigning this now would risk duplicate work", c.taskPath,
			), archived)
			return
		}
	}

	if !c.knownDeadAgent && c.deadAgentProject == "" {
		s.archiveStrandedMessage(c.id, fmt.Sprintf(
			"addressed to %q, which is not a known agent or project queue — no way to determine where to reassign it", c.to,
		), archived)
		return
	}

	s.mu.Lock()
	target := s.pickReassignmentTargetLocked(c.deadAgentProject)
	if target == "" {
		s.mu.Unlock()
		log.Printf("serve: stranded-message sweep: cannot build a reassignment target for message %s (project %q empty) — left addressed to %s, needs manual attention", c.id, c.deadAgentProject, c.to)
		return
	}
	for _, m := range s.state.Messages {
		if m.ID != c.id {
			continue
		}
		if m.Status != "queued" && m.Status != "delivered" {
			break // acked or archived by something else between classification and now — leave it
		}
		m.To = target
		m.Status = "queued"
		m.DeliveredAt = nil
		m.Text = stampStrandedProvenance(m.Text, c.createdAt)
		break
	}
	s.mu.Unlock()
	log.Printf("serve: stranded-message sweep: reassigned message %s from %s to %s", c.id, c.to, target)
	*reassigned++
}

// strandedMessageProvenanceMarker is stampStrandedProvenance's own prefix,
// checked for idempotency (a repeat sweep pass, or a message that's
// somehow reassigned twice, must never stack a second banner).
const strandedMessageProvenanceMarker = "[Reassigned after being stranded since "

// stampStrandedProvenance prepends a provenance banner to a message being
// reassigned off a dead/unroutable recipient, naming the ORIGINAL send
// date. The reassign-vs-archive decision above only asks whether the
// referenced TASK is closed — a necessary proxy, but only a proxy: a task
// can stay open while the world around it changes in a way nothing in
// this daemon can detect. Live incident (2026-08-27) that established
// this: "Reinstall the APK on tainted VMs (host 203.0.113.7)" was
// still technically open, but the VM it named had been terminated the
// same day — reassigning it as a plain, current-looking task assignment
// would have sent a live agent to work on a host that no longer exists.
// The daemon cannot know that; the least it can do is stop presenting
// six-day-old mail as if it just arrived, so whoever receives it verifies
// the premises before acting instead of treating it as fresh.
func stampStrandedProvenance(text string, originalCreatedAt time.Time) string {
	if strings.HasPrefix(text, strandedMessageProvenanceMarker) {
		return text // already stamped — don't stack a second banner on a repeat reassignment
	}
	return fmt.Sprintf(
		"%s%s — the world may have changed since then (a referenced host, branch, or VM may no longer exist); verify this is still current before acting]\n\n%s",
		strandedMessageProvenanceMarker, originalCreatedAt.Format("2006-01-02"), text,
	)
}

// archiveStrandedMessage acquires s.mu itself (do not call with it held)
// and sets a message to the terminal "archived" status with a reason,
// rather than deleting it: same tombstone-not-delete philosophy as
// Agent.DeregisteredAt, so there's always a durable record of what was
// discarded and why.
func (s *Server) archiveStrandedMessage(id, reason string, archived *int) {
	s.mu.Lock()
	now := time.Now()
	for _, m := range s.state.Messages {
		if m.ID != id {
			continue
		}
		if m.Status != "queued" && m.Status != "delivered" {
			s.mu.Unlock()
			return // acked or already archived between classification and now — leave it
		}
		m.Status = "archived"
		m.ArchivedAt = &now
		m.ArchiveReason = reason
		break
	}
	s.mu.Unlock()
	log.Printf("serve: stranded-message sweep: archived message %s: %s", id, reason)
	*archived++
}
