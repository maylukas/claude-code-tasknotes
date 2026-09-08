package main

import (
	"fmt"
	"testing"
)

// fakeSpawnDeps builds a set of recording fakes for
// spawnOrchestratorSession's injected tmux/alive dependencies.
type fakeSpawnDeps struct {
	hasSession  bool
	alive       bool
	panes       []string
	panesErr    error
	killCalled  bool
	killSession string
	newCalled   bool
	newSession  string
	newCwd      string
	sendCalled  bool
	sendSession string
	sendKeys    string
}

func (f *fakeSpawnDeps) hasSessionFunc(session string) bool { return f.hasSession }
func (f *fakeSpawnDeps) aliveFunc() bool                    { return f.alive }
func (f *fakeSpawnDeps) listPanesFunc(session string) ([]string, error) {
	if f.panesErr != nil {
		return nil, f.panesErr
	}
	return f.panes, nil
}
func (f *fakeSpawnDeps) killSessionFunc(session string) error {
	f.killCalled = true
	f.killSession = session
	return nil
}
func (f *fakeSpawnDeps) newSessionFunc(session, cwd string) error {
	f.newCalled = true
	f.newSession, f.newCwd = session, cwd
	return nil
}
func (f *fakeSpawnDeps) sendKeysFunc(session, keys string) error {
	f.sendCalled = true
	f.sendSession, f.sendKeys = session, keys
	return nil
}

// TestSpawnOrchestratorSession_NoExistingSessionSpawnsFresh verifies the
// common case: no tmux session exists yet, so it's created directly without
// ever consulting the alive check or pane lister.
func TestSpawnOrchestratorSession_NoExistingSessionSpawnsFresh(t *testing.T) {
	f := &fakeSpawnDeps{hasSession: false}
	err := spawnOrchestratorSession("proj", "/repo", nil, "orchestrator-proj-g1", "tn-proj", f.aliveFunc,
		f.hasSessionFunc, f.listPanesFunc, f.killSessionFunc, f.newSessionFunc, f.sendKeysFunc, "claude", "ORCHESTRATOR.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.killCalled {
		t.Error("expected no kill-session call when no session existed")
	}
	if !f.newCalled || f.newSession != "tn-proj" || f.newCwd != "/repo" {
		t.Errorf("expected new-session tn-proj in /repo, got called=%v session=%q cwd=%q", f.newCalled, f.newSession, f.newCwd)
	}
	if !f.sendCalled || f.sendSession != "tn-proj" {
		t.Errorf("expected send-keys to tn-proj, got called=%v session=%q", f.sendCalled, f.sendSession)
	}
}

// TestSpawnOrchestratorSession_AliveAgentSkipsPaneCheck verifies that when
// the fresh alive check says an agent IS alive, the existing session is
// left alone WITHOUT ever consulting the pane lister — trusting the bridge
// over inspecting tmux when the two would agree anyway.
func TestSpawnOrchestratorSession_AliveAgentSkipsPaneCheck(t *testing.T) {
	paneListCalls := 0
	f := &fakeSpawnDeps{hasSession: true, alive: true}
	listPanes := func(session string) ([]string, error) {
		paneListCalls++
		return f.listPanesFunc(session)
	}
	err := spawnOrchestratorSession("proj", "/repo", nil, "orchestrator-proj-g1", "tn-proj", f.aliveFunc,
		f.hasSessionFunc, listPanes, f.killSessionFunc, f.newSessionFunc, f.sendKeysFunc, "claude", "ORCHESTRATOR.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if paneListCalls != 0 {
		t.Errorf("expected pane lister never consulted when an agent is alive, got %d calls", paneListCalls)
	}
	if f.killCalled || f.newCalled || f.sendCalled {
		t.Error("expected a pure no-op when the session exists and an agent is alive")
	}
}

// TestSpawnOrchestratorSession_StaleSessionKilledAndRespawned is the core
// fix: session exists, no agent alive, and the panes show no claude/node
// process running (the shell survived a dead/rotated orchestrator) — the
// stale session must be killed and a fresh one spawned in its place.
func TestSpawnOrchestratorSession_StaleSessionKilledAndRespawned(t *testing.T) {
	f := &fakeSpawnDeps{hasSession: true, alive: false, panes: []string{"zsh"}}
	err := spawnOrchestratorSession("proj", "/repo", nil, "orchestrator-proj-g1", "tn-proj", f.aliveFunc,
		f.hasSessionFunc, f.listPanesFunc, f.killSessionFunc, f.newSessionFunc, f.sendKeysFunc, "claude", "ORCHESTRATOR.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !f.killCalled || f.killSession != "tn-proj" {
		t.Errorf("expected kill-session tn-proj, got called=%v session=%q", f.killCalled, f.killSession)
	}
	if !f.newCalled || f.newSession != "tn-proj" {
		t.Errorf("expected a fresh new-session tn-proj after the kill, got called=%v session=%q", f.newCalled, f.newSession)
	}
	if !f.sendCalled {
		t.Error("expected send-keys to launch claude in the fresh session")
	}
}

// TestSpawnOrchestratorSession_LiveClaudePreserved verifies the safety
// side: session exists, no agent alive, but a claude/node process IS still
// running in a pane (maybe stuck, not dead) — must NOT be killed.
func TestSpawnOrchestratorSession_LiveClaudePreserved(t *testing.T) {
	for _, cmd := range []string{"claude", "node"} {
		t.Run(cmd, func(t *testing.T) {
			f := &fakeSpawnDeps{hasSession: true, alive: false, panes: []string{cmd}}
			err := spawnOrchestratorSession("proj", "/repo", nil, "orchestrator-proj-g1", "tn-proj", f.aliveFunc,
				f.hasSessionFunc, f.listPanesFunc, f.killSessionFunc, f.newSessionFunc, f.sendKeysFunc, "claude", "ORCHESTRATOR.md")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if f.killCalled {
				t.Error("expected no kill-session when a claude process is still running in a pane")
			}
			if f.newCalled || f.sendCalled {
				t.Error("expected no new-session/send-keys when leaving the live session alone")
			}
		})
	}
}

// TestSpawnOrchestratorSession_PaneListErrorLeavesSessionAlone verifies a
// pane-lister failure is treated conservatively: no kill, no respawn — an
// ambiguous read is never grounds for destroying a session.
func TestSpawnOrchestratorSession_PaneListErrorLeavesSessionAlone(t *testing.T) {
	f := &fakeSpawnDeps{hasSession: true, alive: false, panesErr: fmt.Errorf("tmux: no such session")}
	err := spawnOrchestratorSession("proj", "/repo", nil, "orchestrator-proj-g1", "tn-proj", f.aliveFunc,
		f.hasSessionFunc, f.listPanesFunc, f.killSessionFunc, f.newSessionFunc, f.sendKeysFunc, "claude", "ORCHESTRATOR.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.killCalled || f.newCalled || f.sendCalled {
		t.Error("expected no action taken when the pane list couldn't be read")
	}
}

// TestPaneHasLiveClaude covers the pane-command matching directly.
func TestPaneHasLiveClaude(t *testing.T) {
	cases := []struct {
		name  string
		panes []string
		want  bool
	}{
		{"claude present", []string{"zsh", "claude"}, true},
		{"node present", []string{"node"}, true},
		{"neither present", []string{"zsh", "bash"}, false},
		{"empty", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := paneHasLiveClaude(c.panes); got != c.want {
				t.Errorf("paneHasLiveClaude(%v) = %v, want %v", c.panes, got, c.want)
			}
		})
	}
}
