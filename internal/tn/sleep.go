package tn

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// --- Host sleep detection & self-heal ---

const (
	// sleepCheckInterval is how often the sleep-detector ticker records
	// wall-clock time.
	sleepCheckInterval = 30 * time.Second
	// sleepGapThreshold is how much wider than sleepCheckInterval a gap
	// between consecutive ticks must be to count as a detected host sleep
	// rather than scheduling jitter or a briefly busy machine.
	sleepGapThreshold = 3 * sleepCheckInterval
)

// startupGraceRemaining returns how much longer, measured from startedAt,
// liveness-based orphan detection must wait before it may run — one full
// aliveWindow, so every registered agent has had a realistic chance to
// poll since this process started before a LastSeenAt predating that
// start gets treated as evidence of death rather than absence of signal.
// Zero once the window has elapsed. Factored out purely for direct,
// deterministic testability (same reasoning as isHostSleepGap) — real
// callers may block on the result; tests pass a startedAt far enough in
// the past that it's always zero.
func startupGraceRemaining(startedAt, now time.Time) time.Duration {
	remaining := aliveWindow - now.Sub(startedAt)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// isHostSleepGap reports whether gap — the time between two consecutive
// sleepCheckInterval ticks — is wide enough to mean the host was actually
// asleep, not just briefly busy or a delayed tick. Factored out of the
// ticker loop purely so it's directly, deterministically testable without
// needing a real 90+ second wait.
func isHostSleepGap(gap time.Duration) bool {
	return gap > sleepGapThreshold
}

// startSleepDetector launches a background goroutine that records
// wall-clock time every sleepCheckInterval. A gap between consecutive
// ticks wider than sleepGapThreshold means the host was ASLEEP, not merely
// busy — a sleeping Mac stops scheduling goroutines entirely, so the next
// tick simply arrives late by however long the sleep lasted. On detection:
// logs it, records it for /status (lastSleepAt/lastSleepSeconds), and runs
// handleHostSleep to reconcile any agent that was silently killed by it.
// This is why sleep-death was previously indistinguishable from ordinary
// bookkeeping drift: a dead agent and a merely-idle one look identical on
// the board (task reads in-progress, no live worker) unless the CAUSE is
// recorded somewhere — this ticker is that record.
func (s *Server) startSleepDetector() {
	go func() {
		last := time.Now()
		ticker := time.NewTicker(sleepCheckInterval)
		defer ticker.Stop()
		for now := range ticker.C {
			gap := now.Sub(last)
			if isHostSleepGap(gap) {
				log.Printf("serve: detected host sleep of ~%ds (%s -> %s)", int(gap.Seconds()), last.Format("15:04"), now.Format("15:04"))
				s.mu.Lock()
				s.lastSleepAt = now
				s.lastSleepDuration = gap
				s.mu.Unlock()
				s.handleHostSleep(last)
			}
			last = now
		}
	}()
}

// handleHostSleep runs the sleep-triggered reconciliation pass: every
// agent whose LastSeenAt falls before sleepStart (so its last poll
// happened before the host went to sleep — it never got a chance to poll
// again either during the sleep or since waking) and is now beyond
// aliveWindow is treated as SLEEP-KILLED rather than merely dead. Must be
// called with s.mu NOT held.
func (s *Server) handleHostSleep(sleepStart time.Time) {
	now := time.Now()
	s.mu.Lock()
	var candidates []Agent
	for _, a := range s.state.Agents {
		if a.DeregisteredAt == nil && a.LastSeenAt.Before(sleepStart) && !agentAliveAt(*a, now) {
			candidates = append(candidates, *a)
		}
	}
	s.mu.Unlock()

	for _, a := range candidates {
		s.reconcileSleepKilledAgent(a, sleepStart)
	}
}

// reconcileSleepKilledAgent marks a deregistered with a specific, honest
// cause (host sleep, not an unclean death or ordinary drift) and, for
// every in-progress task it owned, appends a note naming the sleep and
// resets the task to `open` so it's visibly re-startable — via
// bridgeTransitionTask, the same path transitionMRMerged uses, which
// already attributes its note to "bridge", never the dead agent (see the
// resurrection-bug reasoning on checkOrphanedTasks: logging activity
// against a dead agent's own name would revive it via
// appendActivityLocked's LastSeenAt side effect). Idempotent: the
// check-and-set of DeregisteredAt happens under a single lock acquisition,
// so a second call for the same agent (e.g. handleHostSleep running twice,
// or racing checkOrphanedDeathsOnce) is a no-op. Must be called with s.mu
// NOT held (fetches/updates tasks over the network).
func (s *Server) reconcileSleepKilledAgent(a Agent, sleepStart time.Time) {
	s.mu.Lock()
	cur, ok := s.state.Agents[a.Name]
	if !ok || cur.DeregisteredAt != nil {
		s.mu.Unlock()
		return
	}
	now := time.Now()
	cur.DeregisteredAt = &now
	cur.OrphanHandledAt = now
	paths := s.ownedTaskPathsLocked(a.Name)
	s.saveLocked()
	s.mu.Unlock()

	var recovered []string
	if s.tnClient != nil {
		for _, p := range paths {
			task, err := s.tnClient.GetTask(p)
			if err != nil {
				log.Printf("serve: sleep-recovery: failed to fetch %s owned by %s: %v", p, a.Name, err)
				continue
			}
			if task.Status != "in-progress" {
				continue
			}
			note := fmt.Sprintf(
				"Worker %s was lost to a host sleep at %s — not abandoned, not drifted. Reset to open for pickup.",
				a.Name, sleepStart.Format("2006-01-02 15:04"),
			)
			if _, err := bridgeTransitionTask(s.tnClient, p, "open", note); err != nil {
				log.Printf("serve: sleep-recovery: failed to reset %s after %s's sleep-death: %v", p, a.Name, err)
				continue
			}
			recovered = append(recovered, fmt.Sprintf("%s (%s)", task.Title, p))
		}
	}

	log.Printf("serve: %s treated as sleep-killed (last seen before %s); reset %d task(s)", a.Name, sleepStart.Format("15:04"), len(recovered))

	s.mu.Lock()
	if len(recovered) > 0 {
		s.appendActivityLocked("bridge", fmt.Sprintf("Host sleep: %s lost mid-task, reset to open: %s", a.Name, strings.Join(recovered, "; ")), "")
	} else {
		s.appendActivityLocked("bridge", fmt.Sprintf("Host sleep: %s marked sleep-killed (no in-progress tasks owned)", a.Name), "")
	}
	s.saveLocked()
	s.mu.Unlock()
	s.triggerRenders()
}

func (s *Server) handleDeregister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.state.Agents[req.Name]
	if !ok {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	now := time.Now()
	a.DeregisteredAt = &now
	s.saveLocked()
	s.triggerRenders()

	writeJSON(w, http.StatusOK, map[string]any{"agent": a})
}

// handlePurgeAgent implements POST /agents/purge {name}: unlike
// deregister/retire (which only tombstone — DeregisteredAt set, the
// Agent record itself kept forever, same as every other agent-record
// path in this file), this actually DELETES the record from
// state.Agents (and any declared Workers entries) — for cleaning up pure
// registry pollution, not for normal agent lifecycle. The live incident
// that prompted it: registrations made only to verify an endpoint
// against the real daemon (see testVerificationAgentPrefix) ended up
// sitting in state.Agents forever, same as a real retired generation,
// showing up in the user's Projects view. isTestVerificationAgentName-
// prefixed names are excluded from /status going forward regardless, but
// a name registered before that convention existed (or one that didn't
// use the prefix at all) needs this to actually go away rather than
// linger as a permanently-dimmed "Not running" row. 404s an unknown
// name — deliberately NOT restricted to prefix-matching names: the
// convention is a go-forward filter, this is a general admin action for
// whichever specific name needs to actually disappear.
func (s *Server) handlePurgeAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.state.Agents[req.Name]; !ok {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	delete(s.state.Agents, req.Name)
	delete(s.state.Workers, req.Name)
	// PermissionDenials is keyed by "agentName|toolName|CommandSummary" —
	// a composite key, not a direct match on req.Name — so this needs a
	// scan rather than a single delete, same reasoning as the rest of
	// this handler: a purged name's telemetry shouldn't linger any more
	// than its agent record should.
	for key, rec := range s.state.PermissionDenials {
		if rec.AgentName == req.Name {
			delete(s.state.PermissionDenials, key)
		}
	}
	s.saveLocked()
	s.triggerRenders()

	writeJSON(w, http.StatusOK, map[string]any{"purged": req.Name})
}

// agentView is Agent plus a computed Alive flag, for GET /agents.
type agentView struct {
	Name           string     `json:"name"`
	Project        string     `json:"project"`
	SessionID      string     `json:"sessionId,omitempty"`
	RegisteredAt   time.Time  `json:"registeredAt"`
	LastSeenAt     time.Time  `json:"lastSeenAt"`
	DeregisteredAt *time.Time `json:"deregisteredAt,omitempty"`
	Alive          bool       `json:"alive"`
	Accepting      bool       `json:"accepting"`
	TmuxSession    string     `json:"tmuxSession,omitempty"`
	ContextPct     *float64   `json:"contextPct,omitempty"`
}

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	views := make([]agentView, 0, len(s.state.Agents))
	for _, a := range s.state.Agents {
		views = append(views, agentView{
			Name: a.Name, Project: a.Project, SessionID: a.SessionID,
			RegisteredAt: a.RegisteredAt, LastSeenAt: a.LastSeenAt,
			DeregisteredAt: a.DeregisteredAt,
			Alive:          isAlive(a),
			Accepting:      a.Accepting,
			TmuxSession:    a.TmuxSession,
			ContextPct:     a.ContextPct,
		})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })

	writeJSON(w, http.StatusOK, map[string]any{"agents": views})
}

func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	wait := 25
	if v := r.URL.Query().Get("wait"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			wait = n
		}
	}
	if wait > 55 {
		wait = 55
	}
	if wait < 0 {
		wait = 0
	}
	deadline := time.Now().Add(time.Duration(wait) * time.Second)

	for {
		s.mu.Lock()
		s.touchAgentLocked(name)
		msgs := s.popQueuedLocked(name)
		if len(msgs) > 0 {
			s.saveLocked()
			s.triggerRenders()
		}
		ch := s.notify
		s.mu.Unlock()

		if len(msgs) > 0 {
			writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
			return
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			writeJSON(w, http.StatusOK, map[string]any{"messages": []*Message{}})
			return
		}

		select {
		case <-ch:
			// state changed; loop and re-check for messages addressed to us
		case <-time.After(remaining):
			writeJSON(w, http.StatusOK, map[string]any{"messages": []*Message{}})
			return
		}
	}
}

func (s *Server) handleAck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		MsgID    string `json:"msgId"`
		Response string `json:"response,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.MsgID == "" {
		http.Error(w, "msgId is required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	// Acking is an authenticated, agent-initiated action — proof of life
	// just like an inbox poll (see touchAgentLocked's callers generally:
	// an agent's OWN actions count, only the bridge's actions about it do
	// not). Safe no-op if req.Name is empty or unknown.
	s.touchAgentLocked(req.Name)
	var snapshot *Message
	for _, m := range s.state.Messages {
		if m.ID == req.MsgID {
			now := time.Now()
			m.Status = "acked"
			m.AckedAt = &now
			m.Response = req.Response
			cp := *m
			snapshot = &cp
			break
		}
	}
	if snapshot == nil {
		s.mu.Unlock()
		http.Error(w, "message not found", http.StatusNotFound)
		return
	}
	s.saveLocked()
	s.triggerRenders()
	s.mu.Unlock()

	// An ack's response is otherwise invisible to the user: `tn ack
	// --response` only ever stored it in bridge state, so the contract
	// used to require agents to say the same thing twice (ack AND a
	// separate `tn note`) and the user only ever saw whichever one they
	// happened to be looking at. Mirroring the response into the task's
	// own history — same mechanism transitionMRMerged/bridgeTransitionTask
	// use — makes a correct ack sufficient on its own.
	if snapshot.Response != "" && snapshot.TaskPath != "" {
		s.appendAckResponseToTask(snapshot.TaskPath, req.Name, snapshot.Response)
	}

	writeJSON(w, http.StatusOK, map[string]any{"message": snapshot})
}

// appendAckResponseToTask prepends an acked message's response to
// taskPath's history block, attributed "<agentName> (answering)". Skipped
// (not an error) when s.tnClient is nil (tests / no API configured), when
// the task can't be fetched, or when its current details already contain
// the (trimmed) response text — an agent that obeyed the old double-entry
// contract and already ran `tn note` with the same text shouldn't get a
// duplicate entry. Tolerates API failure: logs and moves on, same as
// autoFlipToInProgress — the ack itself has already succeeded and must
// not be undone by a note-surfacing failure.
func (s *Server) appendAckResponseToTask(taskPath, agentName, response string) {
	if s.tnClient == nil {
		return
	}
	response = strings.TrimSpace(response)
	if response == "" || taskPath == "" {
		return
	}
	task, err := s.tnClient.GetTask(taskPath)
	if err != nil {
		log.Printf("serve: ack-response: failed to fetch %s to note answer: %v", taskPath, err)
		return
	}
	if strings.Contains(task.Details, response) {
		return
	}
	by := agentName
	if by == "" {
		by = "agent"
	}
	entry := formatHistoryEntry(by+" (answering)", response, time.Now())
	newDetails := applyNoteBodyEdit(task.Details, func(nb *noteBody) {
		nb.History = append([]string{entry}, nb.History...)
	})
	if _, err := s.tnClient.UpdateTask(taskPath, map[string]any{"details": newDetails}); err != nil {
		log.Printf("serve: ack-response: failed to note answer on %s: %v", taskPath, err)
	}
}

func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	var req sendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}
	if req.To == "" && req.Project == "" {
		http.Error(w, "to or project is required", http.StatusBadRequest)
		return
	}

	msg, status, extra := s.dispatchMessage(req)

	resp := map[string]any{"message": msg}
	for k, v := range extra {
		resp[k] = v
	}
	writeJSON(w, status, resp)
}

func (s *Server) handleGetMessages(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	to := r.URL.Query().Get("to")
	statusFilter := r.URL.Query().Get("status")

	s.mu.Lock()
	defer s.mu.Unlock()

	if id != "" {
		for _, m := range s.state.Messages {
			if m.ID == id {
				writeJSON(w, http.StatusOK, map[string]any{"message": m})
				return
			}
		}
		http.Error(w, "message not found", http.StatusNotFound)
		return
	}

	out := make([]*Message, 0)
	for _, m := range s.state.Messages {
		if to != "" && m.To != to {
			continue
		}
		if statusFilter != "" && m.Status != statusFilter {
			continue
		}
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": out})
}

// maxActivityEntries caps stored activity; oldest entries are dropped on
// append once the cap is exceeded.
const maxActivityEntries = 500

// appendActivityLocked appends an activity entry, bumps the agent's
// lastSeenAt if registered, and caps stored activity at maxActivityEntries
// (dropping oldest). Must be called with s.mu held. taskPath is "" for
// entries not clearly tied to one task — see ActivityEntry.TaskPath.
func (s *Server) appendActivityLocked(name, text, taskPath string) *ActivityEntry {
	entry := &ActivityEntry{Agent: name, At: time.Now(), Text: text, TaskPath: taskPath}
	s.state.Activity = append(s.state.Activity, entry)
	if len(s.state.Activity) > maxActivityEntries {
		s.state.Activity = s.state.Activity[len(s.state.Activity)-maxActivityEntries:]
	}
	s.touchAgentLocked(name)
	return entry
}

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Text == "" {
		http.Error(w, "name and text are required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	entry := s.appendActivityLocked(req.Name, req.Text, "")
	s.saveLocked()
	s.triggerRenders()
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"activity": entry})
}
