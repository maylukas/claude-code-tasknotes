package tn

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// --- Orchestrator startup (real implementation; tests inject spawnFunc) ---

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// resolveOrchestratorDoc resolves the path to ORCHESTRATOR.md, the operating
// contract read by daemon-spawned orchestrator sessions (kept out of the
// global tasknotes skill so unrelated interactive sessions don't inherit
// headless-only rules). Precedence order:
//
//  1. env TN_ORCHESTRATOR_DOC
//  2. serve.json's top-level "orchestratorDoc"
//  3. ~/.config/tn/ORCHESTRATOR.md
//  4. the directory containing the running daemon binary (os.Executable)
//  5. ./ORCHESTRATOR.md relative to the daemon's working directory
//
// Returns the first candidate that exists on disk (os.Stat) and found=true.
// If none exist, returns candidate (3)'s path (falling back to (5)'s if the
// home directory can't be resolved) with found=false — the caller is
// expected to log a warning in that case; the path is still usable as the
// self-explanatory location in the prompt text even though nothing lives
// there yet.
func resolveOrchestratorDoc(cfg ServeConfig) (path string, found bool) {
	var candidates []string
	if v := os.Getenv("TN_ORCHESTRATOR_DOC"); v != "" {
		candidates = append(candidates, v)
	}
	if cfg.OrchestratorDoc != "" {
		candidates = append(candidates, cfg.OrchestratorDoc)
	}
	fallback := "ORCHESTRATOR.md"
	if home, err := os.UserHomeDir(); err == nil {
		fallback = filepath.Join(home, ".config", "tn", "ORCHESTRATOR.md")
		candidates = append(candidates, fallback)
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "ORCHESTRATOR.md"))
	}
	candidates = append(candidates, "ORCHESTRATOR.md")

	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, true
		}
	}
	return fallback, false
}

// buildOrchestratorPrompt returns the bootstrap prompt sent to a
// daemon-spawned orchestrator session for project. Per SPEC-generations.md,
// it must carry agentName and tmuxSession literally — the session
// registers itself under the exact identity the daemon is already
// expecting for this generation. orchestratorDoc is the resolved path from
// resolveOrchestratorDoc (or an explicit path in tests).
func buildOrchestratorPrompt(project, agentName, tmuxSession, orchestratorDoc string) string {
	return fmt.Sprintf(
		"You are the TaskNotes orchestrator for project %s. Read %s and follow it exactly; your project slug is %s; "+
			"register with the bridge as %s; your tmux session is %s (tn register --name %s --project %s --tmux-session %s).",
		project, orchestratorDoc, project, agentName, tmuxSession, agentName, project, tmuxSession,
	)
}

// buildLaunchCommand returns the shell command line typed into the spawned
// tmux session: any injected per-project env vars (sorted for determinism),
// then the claude CLI invoked with prompt as its argument, prefixed with
// TN_HEADLESS=1 so the orchestrator doc (and the tasknotes skill) can scope
// headless-output rules to daemon-spawned sessions only, and TN_AGENT_NAME
// so the user's statusline wrapper (~/.config/tn/statusline-wrapper.sh)
// knows which agent to report context usage for via POST /agents/context
// (see SPEC-generations.md's daemon-managed auto-drain).
func buildLaunchCommand(claudeBin, agentName, prompt string, env map[string]string) string {
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		b.WriteString(name)
		b.WriteString("=")
		b.WriteString(shellQuote(env[name]))
		b.WriteString(" ")
	}
	b.WriteString("TN_HEADLESS=1 ")
	b.WriteString("TN_AGENT_NAME=")
	b.WriteString(shellQuote(agentName))
	b.WriteString(" ")
	b.WriteString(claudeBin)
	// --permission-mode auto: unattended sessions can never answer an
	// interactive prompt; auto approves safe calls and denies risky ones
	// with an error the session reacts to (→ needs-input, per
	// ORCHESTRATOR.md). Set per explicit user decision 2026-08-17.
	b.WriteString(" --permission-mode auto ")
	b.WriteString(shellQuote(prompt))
	return b.String()
}

// resolveTmuxBin finds tmux by absolute path — under launchd the daemon's
// PATH lacks the Homebrew dirs, so a bare "tmux" is not found.
func resolveTmuxBin() string {
	for _, p := range []string{"/opt/homebrew/bin/tmux", "/usr/local/bin/tmux", "/usr/bin/tmux"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "tmux"
}

// tmuxHasSessionFunc, tmuxListPanesFunc, tmuxKillSessionFunc, and
// tmuxNewSessionFunc are injectable so tests never touch tmux.
// sendKeysFunc (defined above for the stuck detector) is reused here too —
// launching the claude command is exactly the same "type text, hit Enter"
// operation.
type tmuxHasSessionFunc func(session string) bool
type tmuxListPanesFunc func(session string) ([]string, error)
type tmuxKillSessionFunc func(session string) error
type tmuxNewSessionFunc func(session, cwd string) error

// claudePaneCommands are the pane_current_command values that indicate a
// claude CLI is actually running in a pane: "claude" when invoked
// directly, or "node" since the claude CLI is a node script and tmux may
// report the interpreter rather than the wrapper name.
var claudePaneCommands = map[string]bool{"claude": true, "node": true}

// paneHasLiveClaude reports whether any pane's current command looks like
// a running claude process.
func paneHasLiveClaude(panes []string) bool {
	for _, p := range panes {
		if claudePaneCommands[strings.TrimSpace(p)] {
			return true
		}
	}
	return false
}

// spawnOrchestratorSession is defaultSpawnFunc's core logic with every
// tmux/alive-check dependency injected, so it's directly testable.
// agentName and tmuxSession are the caller-computed identity for this
// generation (see defaultSpawnFunc — generation naming lives there so this
// core function stays deterministic and testable).
//
// A tmux session already existing used to be enough to no-op unconditionally
// — but after the claude process inside exits (orchestrator rotation or a
// crash), the session's shell lives on, so has-session keeps succeeding
// forever and a dead orchestrator's tmux session blocks respawn permanently.
// So when the session exists AND agentAlive reports no live agent for this
// SESSION (a fresh check at spawn time, not whatever stale snapshot the
// caller originally decided to spawn from — and session-scoped rather than
// project-scoped, since with generations more than one agent can be alive
// for the same project at once), its panes are inspected: no claude process
// running means the session is genuinely stale — kill it and fall through
// to a normal fresh spawn. A claude process still running (maybe just
// stuck, not dead) is left alone — killing it would drop a live/stuck
// session out from under the user — so that case stays the original no-op.
// A pane-list failure is treated the same conservative way: log and leave
// the session alone rather than guess.
func spawnOrchestratorSession(project, cwd string, env map[string]string, agentName, tmuxSession string, agentAlive func() bool, hasSession tmuxHasSessionFunc, listPanes tmuxListPanesFunc, killSession tmuxKillSessionFunc, newSession tmuxNewSessionFunc, sendKeys sendKeysFunc, claudeBin, orchestratorDoc string) error {
	session := tmuxSession

	if hasSession(session) {
		if agentAlive() {
			log.Printf("serve: spawn: tmux session %s exists and an agent is alive for it; no-op", session)
			return nil
		}
		panes, err := listPanes(session)
		if err != nil {
			log.Printf("serve: spawn: failed to list panes for stale-session check on %s: %v; leaving it alone", session, err)
			return nil
		}
		if paneHasLiveClaude(panes) {
			log.Printf("serve: spawn: tmux session %s exists with a claude process still running (no agent registered as alive — possibly stuck, not dead); leaving it alone", session)
			return nil
		}
		log.Printf("serve: spawn: tmux session %s exists but no claude process is running and no agent is alive for it; killing the stale session and respawning", session)
		if err := killSession(session); err != nil {
			return fmt.Errorf("tmux kill-session: %w", err)
		}
	}

	if cwd == "" {
		if home, err := os.UserHomeDir(); err == nil {
			cwd = home
		}
	}
	if err := newSession(session, cwd); err != nil {
		return fmt.Errorf("tmux new-session: %w", err)
	}

	launch := buildLaunchCommand(claudeBin, agentName, buildOrchestratorPrompt(project, agentName, tmuxSession, orchestratorDoc), env)
	if err := sendKeys(session, launch); err != nil {
		return fmt.Errorf("tmux send-keys: %w", err)
	}
	return nil
}

// defaultSpawnFunc starts a detached tmux session running the claude CLI
// for a NEW generation of project's orchestrator — see
// spawnOrchestratorSession for the stale-session-detection logic.
//
// Generation naming (SPEC-generations.md): agent name
// orchestrator-<project>-g<gen>, tmux session tn-<project>-g<gen>, where
// gen := time.Now().Unix() mod 100000 — collision-safe enough day to day;
// the ~27.7h wrap means an exact collision with a much older session name
// is possible in principle, which is exactly what the stale-session check
// above exists to resolve safely either way.
//
// sessionAlive is called with the computed tmux session name to get a
// fresh alive check at spawn time (the caller that decided to spawn may
// have done so from a snapshot that's since gone stale, e.g. a generation
// registered a moment later).
func defaultSpawnFunc(project, cwd string, env map[string]string, sessionAlive func(tmuxSession string) bool, orchestratorDoc string) error {
	tmux := resolveTmuxBin()

	gen := time.Now().Unix() % 100000
	agentName := fmt.Sprintf("orchestrator-%s-g%d", project, gen)
	tmuxSession := fmt.Sprintf("tn-%s-g%d", project, gen)

	hasSession := func(session string) bool {
		return exec.Command(tmux, "has-session", "-t", session).Run() == nil
	}
	listPanes := func(session string) ([]string, error) {
		out, err := exec.Command(tmux, "list-panes", "-t", session, "-F", "#{pane_current_command}").Output()
		if err != nil {
			return nil, err
		}
		return strings.Split(strings.TrimSpace(string(out)), "\n"), nil
	}
	killSession := func(session string) error {
		return exec.Command(tmux, "kill-session", "-t", session).Run()
	}
	newSession := func(session, cwd string) error {
		return exec.Command(tmux, "new-session", "-d", "-s", session, "-c", cwd).Run()
	}

	claudeBin := "claude"
	if home, err := os.UserHomeDir(); err == nil {
		local := filepath.Join(home, ".local", "bin", "claude")
		if _, err := os.Stat(local); err == nil {
			claudeBin = local
		}
	}

	return spawnOrchestratorSession(project, cwd, env, agentName, tmuxSession,
		func() bool { return sessionAlive(tmuxSession) },
		hasSession, listPanes, killSession, newSession, sendKeysReal, claudeBin, orchestratorDoc)
}
