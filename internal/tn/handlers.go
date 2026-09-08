package tn

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// --- HTTP handlers ---

type sendRequest struct {
	To       string `json:"to,omitempty"`
	Project  string `json:"project,omitempty"`
	Text     string `json:"text"`
	TaskPath string `json:"taskPath,omitempty"`
	From     string `json:"from,omitempty"`
	// SkipOwnerRouting bypasses task-owner routing in resolveTargetLocked
	// even though TaskPath is set (TaskPath is still stored on the
	// resulting Message for history/dashboard association — only the
	// routing decision changes). json:"-": settable only by internal
	// callers constructing a sendRequest directly (checkJiraTransition),
	// never by an external POST /messages body.
	SkipOwnerRouting bool `json:"-"`
	// Informational carries through to Message.Informational — see that
	// field's doc comment. json:"-": same "internal callers only" shape as
	// SkipOwnerRouting; use ONLY for a message that asks nothing of its
	// recipient. Misclassifying a real instruction as Informational would
	// let it sit unacked forever with no signal at all, which is worse
	// than the misnamed flag this exists to fix — when in doubt, leave it
	// false.
	Informational bool `json:"-"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Project     string `json:"project"`
		SessionID   string `json:"sessionId,omitempty"`
		TmuxSession string `json:"tmuxSession,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Project == "" {
		http.Error(w, "name and project are required", http.StatusBadRequest)
		return
	}

	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.state.Agents[req.Name]
	if !ok {
		a = &Agent{Name: req.Name, RegisteredAt: now}
		s.state.Agents[req.Name] = a
	}
	a.Project = req.Project
	if req.SessionID != "" {
		a.SessionID = req.SessionID
	}
	if req.TmuxSession != "" {
		a.TmuxSession = req.TmuxSession
	}
	a.LastSeenAt = now
	s.seenSinceStart[req.Name] = true // registering is itself an observation
	a.DeregisteredAt = nil
	// Every register — fresh or re-register — starts (or resumes) accepting
	// new work; `tn drain` is the only thing that turns it off again.
	a.Accepting = true
	// A fresh registration is a new lifecycle for this name — a later
	// death is eligible for its own orphan-task check again.
	a.OrphanHandledAt = time.Time{}
	a.ConsecutiveDeadTicks = 0

	// This registration fulfills any spawn intent recorded for the
	// project (see PendingSpawns/effectiveAcceptingCountLocked) — clear it
	// immediately rather than waiting out spawnIntentTTL, so a spawn
	// decision arriving moments later (which now sees a real accepting
	// agent via pickAcceptingAgentForAssignmentLocked) doesn't matter
	// either way, but a project that legitimately needs ANOTHER spawn soon
	// (e.g. scale-out) isn't stuck waiting on a stale intent.
	s.clearSpawnIntentLocked(strings.ToLower(req.Project))

	// The logical queue name orchestrator-<slug> is what gets used when
	// nothing was alive to receive an assignment. Whichever concrete agent
	// registers next for that project inherits anything still queued there
	// — otherwise a generation's queued work would strand forever behind
	// the OLD generation's name.
	if n := s.retargetLogicalQueueLocked(req.Project, req.Name); n > 0 {
		log.Printf("serve: register: re-targeted %d message(s) from the orchestrator-%s logical queue to %s", n, strings.ToLower(req.Project), req.Name)
	}

	s.saveLocked()
	s.triggerRenders()

	writeJSON(w, http.StatusOK, map[string]any{"agent": a})
}

// retargetLogicalQueueLocked re-addresses every still-queued message
// currently sent to the logical queue name orchestrator-<slug> (used when
// no agent was alive to receive it) over to newAgentName, so a fresh
// generation registering for that project picks up anything stranded there
// instead of it waiting for an agent literally named orchestrator-<slug>
// that no generation actually uses anymore. Returns the number re-targeted.
// Must be called with s.mu held.
func (s *Server) retargetLogicalQueueLocked(project, newAgentName string) int {
	logicalName := "orchestrator-" + strings.ToLower(project)
	if newAgentName == logicalName {
		return 0
	}
	n := 0
	for _, m := range s.state.Messages {
		if m.To == logicalName && m.Status == "queued" {
			m.To = newAgentName
			n++
		}
	}
	return n
}

// handleDrain implements POST /agents/drain {name}: flips Accepting to
// false (idempotent — draining an already-draining or unknown agent is not
// an error). A draining agent keeps polling/acking and finishing what it
// owns; it just stops receiving new task assignments.
func (s *Server) handleDrain(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.state.Agents[req.Name]
	if !ok {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	a.Accepting = false
	s.saveLocked()
	s.triggerRenders()

	writeJSON(w, http.StatusOK, map[string]any{"agent": a})
}

// drainContextPct is the context-usage percentage (0-100) at or above which
// handleContext auto-drains an accepting agent (SPEC-generations.md).
const drainContextPct = 40.0

// handleContext implements POST /agents/context {name, usedPct}: stores the
// agent's last-reported context-window usage, fed by the user's statusline
// wrapper on every render. Crossing drainContextPct while the agent is
// still accepting auto-drains it (Accepting=false) exactly once — a report
// arriving while already draining is a no-op past the storage itself, so
// this never re-queues the informational message on every subsequent
// high-percentage report.
func (s *Server) handleContext(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string  `json:"name"`
		UsedPct float64 `json:"usedPct"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	a, ok := s.state.Agents[req.Name]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	// Posting context usage is an authenticated, agent-initiated action —
	// proof of life just like an inbox poll (see touchAgentLocked's
	// callers generally: an agent's OWN actions count, only the bridge's
	// actions about it do not).
	s.touchAgentLocked(req.Name)
	pct := req.UsedPct
	a.ContextPct = &pct
	a.ContextPctAt = time.Now()

	autoDrain := a.Accepting && pct >= drainContextPct
	if autoDrain {
		a.Accepting = false
		s.appendActivityLocked(req.Name, fmt.Sprintf("Auto-drained at %.0f%% context", pct), "")
	}
	s.saveLocked()
	s.mu.Unlock()

	if autoDrain {
		log.Printf("serve: auto-draining %s at %.0f%% context", req.Name, pct)
		s.dispatchMessage(sendRequest{
			To: req.Name,
			Text: fmt.Sprintf(
				"You are draining (context %.0f%%): no new assignments will be routed. Finish owned work, then tn retire --name %s.",
				pct, req.Name,
			),
		})
	}

	s.triggerRenders()
	writeJSON(w, http.StatusOK, map[string]any{"agent": a})
}

// retireKillDelay is how long handleRetire waits before actually killing
// the retiring agent's tmux session — long enough that the `tn retire`
// tool call gets its result back before the session dies out from under
// it, rather than a mid-call SIGHUP.
const retireKillDelay = 2 * time.Second

// handleRetire implements POST /agents/retire {name}: daemon-mediated
// self-termination, replacing the old "deregister then kill your own tmux
// session" pair an orchestrator used to run on itself. Fails 404 for an
// unknown agent.
//
// Pending (queued/delivered, unacked) messages no longer unconditionally
// block retirement. A live ACCEPTING agent retiring with unacked work still
// gets 409 {"error":"pending messages"} — that's a mistake worth blocking
// (why would a generation still taking new work be retiring with mail
// unread?). A DRAINING agent's pending messages are instead REASSIGNED —
// to the current accepting agent for its project, or the logical
// orchestrator-<slug> queue name if none — and retirement proceeds. This
// replaces an earlier design (plain 409 for anyone with pending messages)
// that deadlocked: a drained generation that stops acting on stale FYIs
// (correctly — acting on them was the actual bug, not retiring) could
// never retire, and its messages were addressed to its own concrete agent
// name, which retargetLogicalQueueLocked's re-registration handoff never
// reaches (that only re-targets the LOGICAL queue name). Three real
// generations accumulated exactly this way (8/16/8 pending messages) before
// this fix. Each reassigned message keeps its id/taskPath/from/text — a
// re-delivery, not a new message, so dedup and history stay coherent —
// only To/Status/DeliveredAt change.
//
// Deliberately does NOT check whether a reassigned message's task is
// already done/closed before re-routing it (which would let a genuinely
// superseded assignment be dropped instead of waking the next generation
// for nothing): that needs a TaskNotes API call per message, and this
// handler holds s.mu for a single synchronous mutation, not a series of
// network round trips — the network call itself must never happen under
// this lock, and doing it unlocked-then-relocked adds a second
// state-consistency window for what's a minor triage cost, not a
// correctness one. Everything is reassigned; the receiving agent triages.
//
// On success the agent is marked deregistered immediately and the response
// returns right away; the actual kill (plus its log line, activity entry,
// and render trigger) happens asynchronously after retireKillDelay via
// finishRetire.
func (s *Server) handleRetire(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	a, ok := s.state.Agents[req.Name]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}

	var pending []*Message
	for _, m := range s.state.Messages {
		if m.To == req.Name && (m.Status == "queued" || m.Status == "delivered") {
			pending = append(pending, m)
		}
	}

	if len(pending) > 0 && a.Accepting {
		s.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{"error": "pending messages"})
		return
	}

	var reassignedTo string
	if len(pending) > 0 {
		// pickReassignmentTargetLocked/orchestratorQueueName, shared with
		// reassignStrandedMessagesForDeadAgent's dirty-death sweep — see
		// orchestratorQueueName's doc comment for why this must never
		// inline-compose "orchestrator-"+project again (an empty project
		// produced exactly that malformed, permanently-stranded name live).
		reassignedTo = s.pickReassignmentTargetLocked(a.Project)
		if reassignedTo == "" {
			log.Printf("serve: retiring %s: cannot build a reassignment target (empty project) — %d pending message(s) left addressed to %s, needs manual attention", req.Name, len(pending), req.Name)
		} else {
			for _, m := range pending {
				m.To = reassignedTo
				m.Status = "queued"
				m.DeliveredAt = nil
			}
			s.broadcastLocked() // wake any long-poller already waiting on reassignedTo
		}
	}

	now := time.Now()
	a.DeregisteredAt = &now
	session, hasSession := tmuxSessionForAgent(*a)
	project := a.Project
	s.saveLocked()
	s.mu.Unlock()

	if len(pending) > 0 && reassignedTo != "" {
		log.Printf("serve: retiring %s, reassigned %d unacked message(s) to %s", req.Name, len(pending), reassignedTo)
	}

	s.triggerRenders()
	writeJSON(w, http.StatusOK, map[string]any{"agent": a})

	if hasSession {
		go func() {
			time.Sleep(retireKillDelay)
			s.finishRetire(req.Name, session)
		}()
	}

	s.triggerOrphanCheckIfNeeded(req.Name, project)
}

// finishRetire does the actual kill for handleRetire — factored out so
// tests can call it directly without a real sleep. Logs and appends an
// activity entry on success; on a kill failure it logs the error instead
// (the agent is already deregistered either way — a failed kill just
// leaves an inert tmux session behind for someone to clean up manually).
func (s *Server) finishRetire(name, session string) {
	kill := s.killSession
	if kill == nil {
		tmux := resolveTmuxBin()
		kill = func(session string) error {
			return exec.Command(tmux, "kill-session", "-t", session).Run()
		}
	}
	if err := kill(session); err != nil {
		log.Printf("serve: retire: failed to kill session %s for %s: %v", session, name, err)
		return
	}
	log.Printf("serve: retired %s (killed %s)", name, session)

	s.mu.Lock()
	s.appendActivityLocked(name, fmt.Sprintf("Retired (killed %s)", session), "")
	s.saveLocked()
	s.mu.Unlock()
	s.triggerRenders()
}
