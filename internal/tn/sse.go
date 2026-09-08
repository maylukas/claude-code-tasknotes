package tn

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- SSE (GET /events, SPEC-ui-v2.md) ---
//
// sseHub fans a "something changed" signal out to every connected GET
// /events client. Piggybacks on triggerRenders — the same choke point the
// dashboard/session-page debounced writers already use — so every one of
// the many existing call sites (already present after every mutation
// throughout this file) starts pushing SSE events for free, with no new
// call sites needed anywhere else.
type sseHub struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

func newSSEHub() *sseHub {
	return &sseHub{subs: map[chan struct{}]struct{}{}}
}

func (h *sseHub) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *sseHub) unsubscribe(ch chan struct{}) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

// notify wakes every subscriber, non-blockingly. A full channel means that
// subscriber hasn't consumed its previous wake yet — dropping a duplicate
// "something changed" is fine (never lossy in a way that matters): the
// client re-fetches the FULL /status on any event, it doesn't try to
// apply a delta, so two rapid mutations coalescing into one client
// refresh is indistinguishable from two refreshes to the end user.
func (h *sseHub) notify() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// sseHeartbeatInterval keeps proxies and sleeping laptops from silently
// dropping an idle GET /events connection.
const sseHeartbeatInterval = 30 * time.Second

// handleEvents implements GET /events: a long-lived text/event-stream
// connection that emits {"type":"status"} whenever ANY daemon mutation
// happens (via sseHub, fed by triggerRenders) plus a comment-only
// heartbeat every sseHeartbeatInterval. The event payload deliberately
// carries no data beyond its type — the client's whole job on receiving
// one is to re-fetch GET /status, so there's nothing here to keep in sync
// with the mutation that triggered it.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := s.sse.subscribe()
	defer s.sse.unsubscribe(ch)

	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			if _, err := fmt.Fprint(w, "data: {\"type\":\"status\"}\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func loadState(path string) *State {
	st := &State{
		Agents:              map[string]*Agent{},
		Messages:            []*Message{},
		TaskDetailsHash:     map[string]string{},
		RoutedClaudeLines:   map[string][]string{},
		Activity:            []*ActivityEntry{},
		AssignedOccurrences: map[string]string{},
		MRStates:            map[string]string{},
		AssignedTasks:       map[string]string{},
		LastKnownStatus:     map[string]string{},
		JiraRequests:        map[string]string{},
		PendingSpawns:       map[string]time.Time{},
		CreatedTasks:        map[string]createdTaskRecord{},
		Workers:             map[string][]workerEntry{},
		DoneMeansNoted:      map[string]bool{},
		PermissionDenials:   map[string]*permissionDenialRecord{},
		MRReviews:           map[string]mrReviewState{},
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	if err := json.Unmarshal(b, st); err != nil {
		return st
	}
	if st.Agents == nil {
		st.Agents = map[string]*Agent{}
	}
	if st.TaskDetailsHash == nil {
		st.TaskDetailsHash = map[string]string{}
	}
	if st.RoutedClaudeLines == nil {
		st.RoutedClaudeLines = map[string][]string{}
	}
	if st.Activity == nil {
		st.Activity = []*ActivityEntry{}
	}
	if st.AssignedOccurrences == nil {
		st.AssignedOccurrences = map[string]string{}
	}
	if st.MRStates == nil {
		st.MRStates = map[string]string{}
	}
	if st.AssignedTasks == nil {
		st.AssignedTasks = map[string]string{}
	}
	if st.LastKnownStatus == nil {
		st.LastKnownStatus = map[string]string{}
	}
	if st.JiraRequests == nil {
		st.JiraRequests = map[string]string{}
	}
	if st.PendingSpawns == nil {
		st.PendingSpawns = map[string]time.Time{}
	}
	if st.CreatedTasks == nil {
		st.CreatedTasks = map[string]createdTaskRecord{}
	}
	if st.Workers == nil {
		st.Workers = map[string][]workerEntry{}
	}
	if st.DoneMeansNoted == nil {
		st.DoneMeansNoted = map[string]bool{}
	}
	if st.PermissionDenials == nil {
		st.PermissionDenials = map[string]*permissionDenialRecord{}
	}
	if st.MRReviews == nil {
		st.MRReviews = map[string]mrReviewState{}
	}
	return st
}

// atomicWriteFile writes data to path via a temp file in the same directory
// followed by a rename, so a concurrent reader never observes a partial
// file. The temp file is removed on any failure before the rename.
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// saveLocked atomically persists state to disk. Must be called with s.mu held.
func (s *Server) saveLocked() {
	b, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		log.Printf("serve: failed to marshal state: %v", err)
		return
	}
	if err := atomicWriteFile(s.statePath, b); err != nil {
		log.Printf("serve: failed to write state: %v", err)
	}
}

// broadcastLocked wakes every inbox long-poller. Must be called with s.mu held.
func (s *Server) broadcastLocked() {
	close(s.notify)
	s.notify = make(chan struct{})
}

func isAlive(a *Agent) bool {
	return a.DeregisteredAt == nil && time.Since(a.LastSeenAt) <= aliveWindow
}

func (s *Server) nextMsgIDLocked() string {
	s.idSeq++
	return fmt.Sprintf("msg-%d-%d", time.Now().UnixNano(), s.idSeq)
}

func (s *Server) touchAgentLocked(name string) {
	if a, ok := s.state.Agents[name]; ok {
		a.LastSeenAt = time.Now()
		s.seenSinceStart[name] = true
	}
}

// popQueuedLocked marks every queued message addressed to name as delivered
// and returns them. Must be called with s.mu held.
func (s *Server) popQueuedLocked(name string) []*Message {
	var out []*Message
	now := time.Now()
	for _, m := range s.state.Messages {
		if m.To == name && m.Status == "queued" {
			m.Status = "delivered"
			m.DeliveredAt = &now
			out = append(out, m)
		}
	}
	return out
}

// aliveAgentForProjectLocked returns an alive agent registered for project,
// or nil. When more than one matches, the lexicographically first name wins
// so the result is deterministic. Must be called with s.mu held.
func (s *Server) aliveAgentForProjectLocked(project string) *Agent {
	var names []string
	for name, a := range s.state.Agents {
		if strings.EqualFold(a.Project, project) && isAlive(a) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	return s.state.Agents[names[0]]
}

// aliveAcceptingAgentForProjectLocked is like aliveAgentForProjectLocked but
// additionally requires Accepting — used for NEW task-assignment routing
// (a draining agent finishes what it owns but must not receive new work; a
// draining-only project is exactly what should trigger a fresh generation
// spawn, per SPEC-generations.md). Must be called with s.mu held.
func (s *Server) aliveAcceptingAgentForProjectLocked(project string) *Agent {
	var names []string
	for name, a := range s.state.Agents {
		if strings.EqualFold(a.Project, project) && isAlive(a) && a.Accepting {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	return s.state.Agents[names[0]]
}

// pickAcceptingAgentForAssignmentLocked returns the agent NEW work should
// go to among the alive+accepting agents for project: the one owning the
// FEWEST tasks (ownedTaskPathsLocked count, taskOwnerLocked-derived — a
// cheap in-memory count, not a live TaskNotes query, since this runs under
// s.mu on every routing decision), tie-broken lexicographically for
// determinism. With max-orchestrators == 1 (the default) there's only ever
// one candidate, so this behaves exactly like the old plain-lexicographic
// pick; it only starts load-balancing once more than one generation is
// accepting at once. nil if none are alive and accepting. Must be called
// with s.mu held.
func (s *Server) pickAcceptingAgentForAssignmentLocked(project string) *Agent {
	var candidates []string
	for name, a := range s.state.Agents {
		if strings.EqualFold(a.Project, project) && isAlive(a) && a.Accepting {
			candidates = append(candidates, name)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.Strings(candidates)
	best := candidates[0]
	bestCount := s.assignmentLoadLocked(best)
	for _, name := range candidates[1:] {
		if count := s.assignmentLoadLocked(name); count < bestCount {
			best, bestCount = name, count
		}
	}
	return s.state.Agents[best]
}

// currentAssignmentTargetLocked returns the recipient of the LATEST "Task
// assigned:" message for taskPath, by CreatedAt — regardless of ack
// status. Unlike taskOwnerLocked (acked only — real ownership, used for
// routing task-scoped follow-ups to whoever actually took the work),
// this is for load-balancing at DISPATCH time: a message the daemon has
// already sent counts as work on that agent's plate before anyone has
// had the chance to ack it, since the whole point of the comparison is
// picking the least-loaded candidate for the NEXT dispatch, not waiting
// for confirmation of the last one. Must be called with s.mu held.
func (s *Server) currentAssignmentTargetLocked(taskPath string) string {
	var target string
	var latest time.Time
	for _, m := range s.state.Messages {
		if m.TaskPath != taskPath || !strings.HasPrefix(m.Text, "Task assigned:") {
			continue
		}
		if target == "" || m.CreatedAt.After(latest) {
			target = m.To
			latest = m.CreatedAt
		}
	}
	return target
}

// assignmentLoadLocked returns how many distinct tasks are currently
// dispatched to agentName — queued, delivered, OR acked, unlike
// ownedTaskPathsLocked (acked only). This is what fixes the burst-
// blindness bug: pickAcceptingAgentForAssignmentLocked used to compare
// ACKED ownership only, so within one burst (many tasks routed in a
// tight loop, nothing acked yet) every comparison saw every candidate as
// equally idle and the same one won every tie — confirmed live (a
// 13-task burst landed 13/0 on two equally-eligible agents, and the real
// system showed the same skew at a smaller scale) and reproduced in
// TestReconcileWebhookMisses_ReassignmentStormNeverDoubleAssigns. Counting
// dispatched-but-unacked work fixes the load signal without inventing any
// new policy — assignment stays unbounded (concurrency is the
// ORCHESTRATOR.md contract's job via $TN_MAX_WORKERS, not the daemon's).
// Must be called with s.mu held.
func (s *Server) assignmentLoadLocked(agentName string) int {
	seen := map[string]bool{}
	count := 0
	for _, m := range s.state.Messages {
		if m.TaskPath == "" || !strings.HasPrefix(m.Text, "Task assigned:") {
			continue
		}
		if seen[m.TaskPath] {
			continue
		}
		seen[m.TaskPath] = true
		if s.currentAssignmentTargetLocked(m.TaskPath) == agentName {
			count++
		}
	}
	return count
}

// taskOwnerLocked returns the agent that owns taskPath — the To of the
// latest ACKED "Task assigned: ..." message for that path — or "" if the
// task has never been assigned (fresh work, no owner yet). Ownership is
// deliberately derived rather than stored: a generation that acked an
// assignment owns that task until it's done, even while draining, so
// task-scoped follow-ups (a @claude: reply, `tn send --task`) keep reaching
// it instead of a newer generation that never touched the task. Must be
// called with s.mu held.
func (s *Server) taskOwnerLocked(taskPath string) string {
	var owner string
	var latest time.Time
	for _, m := range s.state.Messages {
		if m.TaskPath != taskPath || m.Status != "acked" || m.AckedAt == nil || !strings.HasPrefix(m.Text, "Task assigned:") {
			continue
		}
		if owner == "" || m.AckedAt.After(latest) {
			owner = m.To
			latest = *m.AckedAt
		}
	}
	return owner
}

// ownedTaskPathsLocked returns every distinct task path currently owned by
// agentName per taskOwnerLocked's definition (the latest acked "Task
// assigned:" message's To) — used by the orphaned-task safety net to find
// what a retired/dead agent was still working on. Must be called with
// s.mu held.
func (s *Server) ownedTaskPathsLocked(agentName string) []string {
	seen := map[string]bool{}
	var paths []string
	for _, m := range s.state.Messages {
		if m.TaskPath == "" || m.Status != "acked" || !strings.HasPrefix(m.Text, "Task assigned:") {
			continue
		}
		if seen[m.TaskPath] {
			continue
		}
		seen[m.TaskPath] = true
		if s.taskOwnerLocked(m.TaskPath) == agentName {
			paths = append(paths, m.TaskPath)
		}
	}
	return paths
}

// queueMessageLocked appends a new queued message and returns it. Must be
// called with s.mu held.
func (s *Server) queueMessageLocked(to string, req sendRequest) *Message {
	msg := &Message{
		ID:            s.nextMsgIDLocked(),
		To:            to,
		Project:       req.Project,
		TaskPath:      req.TaskPath,
		Text:          req.Text,
		From:          req.From,
		Status:        "queued",
		CreatedAt:     time.Now(),
		Informational: req.Informational,
	}
	s.state.Messages = append(s.state.Messages, msg)
	return msg
}

// spawnIntentTTL bounds how long a recorded spawn intent (PendingSpawns)
// counts as "an accepting agent is on the way" for cap-enforcement
// purposes. Must cover real spawn latency (tmux + claude startup +
// registration — observed several seconds to low tens of seconds in
// practice) but stay short enough that a spawn which silently failed
// (spawnFunc error, crashed session) doesn't wedge a project out of ever
// spawning again; the periodic reconciler re-checks every 30s regardless,
// so a few minutes of dead air is the actual cost of expiry being wrong.
const spawnIntentTTL = 3 * time.Minute

// effectiveAcceptingCountLocked returns project's accepting-agent count
// for cap-enforcement purposes: live accepting agents PLUS a recent,
// still-unfulfilled spawn intent for the same slug (see PendingSpawns) —
// counted as one more, since it represents an agent that will register
// shortly. Both spawn paths (resolveTargetLocked's on-demand spawn and
// reconcileSpawnsOnce's periodic pass) MUST call this same helper rather
// than counting live agents themselves: the original bug was exactly that
// divergence — each path independently saw zero live accepting agents (the
// three prior generations were all draining) and both decided to spawn,
// ~11s apart, before either new agent had finished registering. An expired
// intent is lazily cleaned up here rather than needing a separate sweep.
// Must be called with s.mu held.
func (s *Server) effectiveAcceptingCountLocked(slug string) int {
	count := 0
	for _, a := range s.state.Agents {
		if strings.EqualFold(a.Project, slug) && isAlive(a) && a.Accepting {
			count++
		}
	}
	if at, ok := s.state.PendingSpawns[slug]; ok {
		if time.Since(at) < spawnIntentTTL {
			count++
		} else {
			delete(s.state.PendingSpawns, slug)
		}
	}
	return count
}

// recordSpawnIntentLocked marks slug as having a spawn decided just now —
// see effectiveAcceptingCountLocked. Called at the moment of DECISION
// (still under s.mu), not at the moment spawnOrchestrator actually runs
// (which happens after the lock is released, since the real spawn is
// slow) — recording anywhere later would reopen exactly the race this
// closes. Must be called with s.mu held.
func (s *Server) recordSpawnIntentLocked(slug string) {
	if s.state.PendingSpawns == nil {
		s.state.PendingSpawns = map[string]time.Time{}
	}
	s.state.PendingSpawns[slug] = time.Now()
}

// clearSpawnIntentLocked drops slug's spawn intent — called the moment an
// agent registers for that project (see handleRegister), since the intent
// has been fulfilled and a fast subsequent spawn decision shouldn't have
// to wait out spawnIntentTTL unnecessarily. Also fine to call for a slug
// with no recorded intent (no-op). Must be called with s.mu held.
func (s *Server) clearSpawnIntentLocked(slug string) {
	delete(s.state.PendingSpawns, slug)
}

// resolveTargetLocked implements the POST /messages resolution rules: `to`
// wins outright; otherwise, if useOwnerRouting is true and the message is
// task-scoped (taskPath set) and that task already has a live owner (see
// taskOwnerLocked), the owner wins even while draining — a task-scoped
// follow-up must keep reaching whoever is actually doing the work;
// otherwise an alive ACCEPTING agent for `project` is used — when more
// than one generation is accepting at once (max-orchestrators > 1, see
// SPEC-generations.md), the one owning the FEWEST tasks gets it
// (pickAcceptingAgentForAssignmentLocked, load balancing across
// concurrent generations), tie-broken lexicographically — a
// draining-only project counts as no agent here, which is what triggers
// a fresh generation spawn below; otherwise the message is queued to the
// logical name orchestrator-<slug>,
// optionally triggering a spawn — both using project lowercased, and the
// autoSpawn config lookup matched case-insensitively too (see
// projectConfigFor). This keeps the orchestrator name, tmux session, and
// config entry consistent regardless of how the caller cased the project
// (webhook-derived slugs are already lowercased by normalizeProjectSlug,
// but direct POST /messages / `tn send` callers may not have normalized
// their input).
//
// useOwnerRouting is false for Jira transition/comment requests
// (checkJiraTransition): those are a fresh action about a task that has
// already reached its target status, not a follow-up on work in flight —
// there is no continuity with the owner to preserve, and routing to a
// draining owner (alive, but not accepting new work) left a real Jira
// request sitting unacked indefinitely during the !1067 live test. Must
// be called with s.mu held.
func (s *Server) resolveTargetLocked(to, project, taskPath string, useOwnerRouting bool) (target string, needSpawn bool, cwd string, status int, extra map[string]any) {
	extra = map[string]any{}
	status = http.StatusOK

	if to != "" {
		return to, false, "", status, extra
	}

	if useOwnerRouting && taskPath != "" {
		if owner := s.taskOwnerLocked(taskPath); owner != "" {
			if a, ok := s.state.Agents[owner]; ok && isAlive(a) {
				return owner, false, "", status, extra
			}
		}
	}

	if a := s.pickAcceptingAgentForAssignmentLocked(project); a != nil {
		return a.Name, false, "", status, extra
	}

	slug := strings.ToLower(project)
	target = orchestratorQueueName(project)
	if target == "" {
		// No explicit recipient, no task owner, no live accepting agent,
		// AND no project to even build a fallback queue name from — there
		// is genuinely nowhere for this message to go. Fail loudly here
		// rather than composing a bare "orchestrator-" that matches no
		// register call and would strand it permanently (see
		// orchestratorQueueName's doc comment for the live incident).
		return "", false, "", http.StatusBadRequest, map[string]any{
			"error": "cannot route message: no recipient, no task owner, and no project to route by",
		}
	}
	status = http.StatusAccepted
	extra["queued"] = true

	cfg := s.projectConfigFor(slug)
	if cfg.AutoSpawn {
		// pickAcceptingAgentForAssignmentLocked already established zero
		// LIVE accepting agents; effectiveAcceptingCountLocked additionally
		// checks for a recent spawn intent from the OTHER path (the
		// reconciler) that hasn't registered yet — without this check, a
		// message arriving in the seconds after the reconciler already
		// decided to spawn would decide to spawn again too.
		if s.effectiveAcceptingCountLocked(slug) == 0 {
			needSpawn = true
			cwd = cfg.Cwd
			s.recordSpawnIntentLocked(slug)
		}
	} else {
		extra["warning"] = "no live agent"
	}
	return target, needSpawn, cwd, status, extra
}

// projectConfigFor returns the ProjectConfig for slug, matching
// s.config.Projects keys case-insensitively (serve.json keys are
// conventionally written lowercase, but the resolved slug's casing depends
// on the caller). Must be called with s.mu held.
func (s *Server) projectConfigFor(slug string) ProjectConfig {
	if cfg, ok := s.config.Projects[slug]; ok {
		return cfg
	}
	for k, cfg := range s.config.Projects {
		if strings.EqualFold(k, slug) {
			return cfg
		}
	}
	return ProjectConfig{}
}

// repoSettingsFor returns slug's parallelism config (repoSettings),
// cached for repoSettingsCacheTTL — see repoSettingsCache. Returns
// defaultRepoSettings() when no dashboard (hence no vault) is configured;
// tests that don't set DashboardPath get the default behavior (single
// accepting generation, no TN_MAX_WORKERS override) automatically.
func (s *Server) repoSettingsFor(slug string) repoSettings {
	s.mu.Lock()
	dashboardPath := s.dashboardPath
	s.mu.Unlock()
	if dashboardPath == "" {
		return defaultRepoSettings()
	}
	vaultDir := filepath.Dir(dashboardPath)
	return s.repoSettings.get(slug, time.Now(), repoSettingsCacheTTL, func() repoSettings {
		return readRepoSettings(vaultDir, slug)
	})
}

// sessionAlive reports whether an alive agent is currently associated with
// tmuxSession (via Agent.TmuxSession, or the legacy tn-<project> derivation
// for agents that never reported one — see tmuxSessionForAgent). Used as
// the fresh, spawn-time check by spawnOrchestratorSession's stale-session
// detection (defaultSpawnFunc calls it right before deciding whether an
// existing session by that exact name is safe to leave alone). Session-
// scoped rather than project-scoped: with generations, more than one agent
// can be alive for the same project at once, so "is any agent alive for
// this project" is the wrong question — the one that matters is whether
// THIS specific session name is actually owned by a live agent.
func (s *Server) sessionAlive(tmuxSession string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, a := range s.state.Agents {
		if !agentAliveAt(*a, now) {
			continue
		}
		if session, ok := tmuxSessionForAgent(*a); ok && session == tmuxSession {
			return true
		}
	}
	return false
}

// spawnOrchestrator resolves a cwd default and calls the injected spawnFunc.
// Never called with s.mu held.
func (s *Server) spawnOrchestrator(project, cwd string) {
	s.mu.Lock()
	paused := s.state.SpawnPaused
	env := injectedEnv(s.projectConfigFor(project))
	s.mu.Unlock()
	if paused {
		log.Printf("serve: spawn skipped for %s: spawning paused", project)
		return
	}

	if cwd == "" {
		if home, err := os.UserHomeDir(); err == nil {
			cwd = home
		}
	}
	// A repo note's max-workers, when set, takes precedence over a
	// serve.json env entry of the same name — it's the more specific,
	// more easily user-editable source.
	if mw := s.repoSettingsFor(project).MaxWorkers; mw > 0 {
		env["TN_MAX_WORKERS"] = strconv.Itoa(mw)
	}

	log.Printf("serve: starting orchestrator for project %s (cwd=%s)", project, cwd)
	if err := s.spawnFunc(project, cwd, env); err != nil {
		log.Printf("serve: failed to start orchestrator for project %s: %v", project, err)
	}
}

// dispatchMessage runs the full send resolution (including a spawn if
// needed) and queues the message, without producing an HTTP response. Used
// by both POST /messages and webhook routing.
//
// Returns a Message VALUE, not the *Message stored in s.state.Messages:
// queueMessageLocked's pointer stays live and mutable (an inbox poll or ack
// arriving concurrently writes Status/DeliveredAt/AckedAt directly on it).
// handleSendMessage used to JSON-encode that same pointer AFTER releasing
// s.mu, racing with exactly those concurrent writers — confirmed by
// `go test -race`. Snapshotting the struct here, while s.mu is still held,
// gives the caller an independent copy immune to later mutation.
func (s *Server) dispatchMessage(req sendRequest) (Message, int, map[string]any) {
	s.mu.Lock()
	to, needSpawn, cwd, status, extra := s.resolveTargetLocked(req.To, req.Project, req.TaskPath, !req.SkipOwnerRouting)
	s.mu.Unlock()

	// resolveTargetLocked returns to=="" ONLY on the "nowhere to route
	// this" failure (see its own doc comment) — never a normal outcome, so
	// this never happens for an external POST /messages caller (already
	// rejected earlier, before dispatchMessage is even called, by
	// handleSendMessage's own to-or-project check). It CAN happen for an
	// internal caller that derived an empty project (routeTaskNotification
	// from a task with no claude-project/projects[] set — the live
	// incident this closes: a "Task assigned:" notification silently
	// queued to a bare "orchestrator-" that could never be delivered).
	// Fail loudly here instead of queueing into a void — there is no HTTP
	// client to return an error to for most of these callers, so a clear
	// log line naming the dropped text is the only failure surface that
	// exists.
	if to == "" {
		log.Printf("serve: dispatchMessage: cannot route message (project=%q, taskPath=%q) — dropped, not queued: %q", req.Project, req.TaskPath, req.Text)
		return Message{}, status, extra
	}

	if needSpawn {
		// Match the lowercased slug resolveTargetLocked used to name the
		// orchestrator/tmux session, so a mixed-case caller still gets a
		// consistent spawn (webhook-derived project is already lowercase
		// via normalizeProjectSlug; this covers direct callers too).
		s.spawnOrchestrator(strings.ToLower(req.Project), cwd)
	}

	s.mu.Lock()
	msg := s.queueMessageLocked(to, req)
	s.saveLocked()
	s.broadcastLocked()
	s.triggerRenders()
	snapshot := *msg
	s.mu.Unlock()

	return snapshot, status, extra
}
