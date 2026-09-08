package tn

import (
	"encoding/json"
	"net/http"
	"time"
)

// --- Worker visibility (SPEC-ui-v2.md #2; tn worker start|end) ---
//
// Entirely orchestrator-reported: the daemon has no way to observe a
// worker subagent on its own (it isn't a registered agent, doesn't poll
// an inbox), so this is purely a declaration surface. An orchestrator
// that never calls `tn worker start/end` simply shows no workers in
// /status — getting real orchestrators to actually call it is a prompt-
// contract change (ORCHESTRATOR.md), not a daemon one.

func (s *Server) handleWorkerStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		TaskPath string `json:"taskPath"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.TaskPath == "" {
		http.Error(w, "name and taskPath are required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	// Declaring a worker is an authenticated, agent-initiated action —
	// proof of life just like an inbox poll (see touchAgentLocked's
	// callers generally: an agent's OWN actions count, only the bridge's
	// actions about it do not). This is exactly the case behind the
	// 2026-08-25 duplicate-MR incident: an orchestrator that just
	// dispatched a worker was falsely declared dead because dispatching
	// isn't an inbox poll.
	s.touchAgentLocked(req.Name)
	entries := s.state.Workers[req.Name]
	found := false
	for i, e := range entries {
		if e.TaskPath == req.TaskPath {
			entries[i].StartedAt = time.Now() // re-declaring refreshes, doesn't duplicate
			found = true
			break
		}
	}
	if !found {
		entries = append(entries, workerEntry{TaskPath: req.TaskPath, StartedAt: time.Now()})
	}
	s.state.Workers[req.Name] = entries
	s.saveLocked()
	s.triggerRenders()
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleWorkerEnd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		TaskPath string `json:"taskPath"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.TaskPath == "" {
		http.Error(w, "name and taskPath are required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	// Same reasoning as handleWorkerStart: an agent's own action, proof
	// of life.
	s.touchAgentLocked(req.Name)
	entries := s.state.Workers[req.Name]
	kept := entries[:0]
	for _, e := range entries {
		if e.TaskPath != req.TaskPath {
			kept = append(kept, e)
		}
	}
	s.state.Workers[req.Name] = kept
	s.saveLocked()
	s.triggerRenders()
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
