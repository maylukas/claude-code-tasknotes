package tn

import (
	"bytes"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"
)

// --- Spawn evidence tracking ---
//
// See State.SpawnedGenerations' doc comment for the incident this exists to
// fix: a spawn intent used to expire on a fixed clock (spawnIntentTTL)
// regardless of whether the spawned tmux session was still alive-but-
// unregistered, so a spawn stuck before `tn register` (e.g. blocked on a
// macOS keychain prompt) got silently respawned every few minutes forever.
// This file replaces that with evidence-based resolution: a tracked
// generation (SpawnedGeneration) stays "pending" for as long as there's no
// proof either way, and only gets resolved — removed, and counted as a
// failure if it never registered — once a tmux snapshot or a registration
// actually says so.

// spawnStartupGrace is how long a freshly spawned generation is given
// before its tmux session is inspected at all — covers claude's own
// startup time (shell prompt still showing, or claude not yet past its own
// init) during which the pane legitimately doesn't show a claude process
// yet. An entry younger than this is always Pending, regardless of
// whatever the tmux snapshot says about it.
const spawnStartupGrace = 60 * time.Second

// spawnRegisterDeadline is how long a generation may stay alive (tmux
// session up, a claude process genuinely running in it) without ever
// calling `tn register` before it's given up on: killed, and counted as a
// failure. This is the actual fix for the incident — the old
// spawnIntentTTL (3min) bore no relation to how long a real but blocked
// startup (keychain prompt, network hang, anything upstream of register)
// might legitimately take, so it kept expiring under live-but-stuck
// spawns and triggering a respawn every cycle. 10 minutes is generous
// enough that a genuinely slow-but-working startup isn't punished, while
// still bounding how long one broken generation can occupy a slot.
const spawnRegisterDeadline = 10 * time.Minute

// maxConsecutiveSpawnFailures caps how many CONSECUTIVE spawned
// generations for one project slug may fail (never register before being
// killed — see evaluateSpawnEvidenceLocked) before both spawn paths
// refuse to spawn that slug at all (see resolveTargetLocked,
// reconcileSpawnsOnce) — the cap that actually stops an unbounded loop
// like the 232-session incident, on top of the slower-but-still-unbounded
// throttling spawnRegisterDeadline alone provides. Reset by a successful
// registration for the slug, or by a human un-pausing spawning via POST
// /spawn/pause {"paused":false} — see handleRegister/handleSpawnPause.
const maxConsecutiveSpawnFailures = 3

// spawnSuspendLogThrottle bounds how often the "spawn SUSPENDED" line
// (logSpawnSuspendedLocked) repeats for the same slug — a suspended
// project's spawn decision is re-evaluated (and would re-log without this)
// every reconciler tick (reconcilerInterval = 30s) for as long as it stays
// suspended and has routable work.
const spawnSuspendLogThrottle = time.Hour

// spawnSnapshotEntry is one tmux session's evidence as of the most recent
// snapshot: whether the session exists at all, and — only meaningful when
// Exists is true — whether any of its panes is currently running a claude
// process (see paneHasLiveClaude).
type spawnSnapshotEntry struct {
	Exists     bool
	LiveClaude bool
}

// spawnSnapshotFunc captures, in ONE pass, the tmux evidence for exactly
// the given session names — see realSpawnSnapshot for the real
// implementation and reconcileSpawnsOnce for why this must be called
// BEFORE s.mu is taken (tmux execs are slow syscalls that must never run
// while the lock is held). Injectable so tests never touch a real tmux
// server.
type spawnSnapshotFunc func(sessions []string) (map[string]spawnSnapshotEntry, error)

// realSpawnSnapshot is spawnSnapshotFunc's real implementation: one `tmux
// ls` to learn which sessions currently exist, then one `tmux list-panes`
// per tracked session that's actually alive (never for sessions already
// known missing — no point). Returns an empty, error-free result for zero
// tracked sessions without ever invoking tmux at all.
func realSpawnSnapshot(sessions []string) (map[string]spawnSnapshotEntry, error) {
	result := make(map[string]spawnSnapshotEntry, len(sessions))
	if len(sessions) == 0 {
		return result, nil
	}

	tmux := resolveTmuxBin()
	var stderr bytes.Buffer
	cmd := exec.Command(tmux, "ls", "-F", "#{session_name}")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// tmux reports "no sessions at all" as a non-zero exit with one of
		// a few wordings depending on version/platform — verified against
		// the installed tmux on this machine (see hasTmuxSessionReal's own
		// doc comment for the has-session equivalent). Matched broadly and
		// deliberately biased toward "this means zero sessions": the
		// alternative failure mode — misreading a genuine "no server"
		// state as a real snapshot error — would leave every tracked entry
		// pending forever (see reconcileSpawnsOnce's doc comment), a
		// silent, permanent respawn-block for that slug. Misreading a
		// GENUINE tmux error as "zero sessions" is bounded instead: worst
		// case it kills/fails entries that are actually fine, which the
		// evidence pass runs again 30s later regardless.
		stderrText := strings.ToLower(stderr.String())
		if strings.Contains(stderrText, "no server running") ||
			strings.Contains(stderrText, "no such file or directory") ||
			strings.Contains(stderrText, "error connecting to") {
			for _, sess := range sessions {
				result[sess] = spawnSnapshotEntry{}
			}
			return result, nil
		}
		return nil, fmt.Errorf("tmux ls: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}

	live := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			live[line] = true
		}
	}
	for _, sess := range sessions {
		if !live[sess] {
			result[sess] = spawnSnapshotEntry{}
			continue
		}
		panesOut, err := exec.Command(tmux, "list-panes", "-t", sess, "-F", "#{pane_current_command}").Output()
		if err != nil {
			log.Printf("serve: reconciler: failed to list panes for %s during spawn-evidence snapshot: %v; treating as no live claude this tick", sess, err)
			result[sess] = spawnSnapshotEntry{Exists: true}
			continue
		}
		panes := strings.Split(strings.TrimSpace(string(panesOut)), "\n")
		result[sess] = spawnSnapshotEntry{Exists: true, LiveClaude: paneHasLiveClaude(panes)}
	}
	return result, nil
}

// realKillSpawnedSession is the real tmuxKillSessionFunc used to tear down
// a spawned generation the evidence pass has given up on.
func realKillSpawnedSession(session string) error {
	return exec.Command(resolveTmuxBin(), "kill-session", "-t", session).Run()
}

// spawnKill describes one tracked tmux session the evidence pass decided
// to tear down — collected while s.mu is held (evaluateSpawnEvidenceLocked)
// and actually killed afterward, outside the lock (reconcileSpawnsOnce).
type spawnKill struct {
	Slug    string
	Session string
	Age     time.Duration
	Reason  string
}

// evaluateSpawnEvidenceLocked applies one tick's tmux snapshot to every
// tracked SpawnedGenerations entry, across every slug (not just autoSpawn
// projects — a slug could have had entries recorded before being toggled
// off, and those still deserve resolving). For each entry:
//
//   - age < spawnStartupGrace: always Pending — the pane may still be a
//     bare shell or claude may not have gotten past its own init yet.
//   - snapshot == nil (the tmux snapshot itself failed or wasn't taken —
//     see reconcileSpawnsOnce): always Pending, conservatively — no
//     evidence means no verdict, not a guess in either direction.
//   - session missing entirely: resolved as a failure (killed is a no-op
//     here, nothing to kill, but still logged/counted for consistency).
//   - session exists, a claude process is running, age within
//     spawnRegisterDeadline: Pending — still plausibly starting up.
//   - session exists, a claude process is running, age past
//     spawnRegisterDeadline: resolved as a failure — alive but never
//     reached `tn register` in a generous window. This is the exact shape
//     of the incident being fixed.
//   - session exists, no claude process running, age past
//     spawnStartupGrace: resolved as a failure — the session outlived its
//     claude process (crash, or it exited) without ever registering.
//
// Every "resolved as a failure" case: removes the entry, increments
// SpawnFailures[slug] (via countSpawnFailureLocked, which also clears any
// still-fresh PendingSpawns entry for the slug — see that function's doc
// comment), and appends a spawnKill for the caller to actually run outside
// the lock (killing a session the evidence says is already gone is a
// harmless no-op; still included so the log line + failure count stay
// together as one record).
//
// Must be called with s.mu held.
func (s *Server) evaluateSpawnEvidenceLocked(snapshot map[string]spawnSnapshotEntry, now time.Time) []spawnKill {
	var kills []spawnKill
	for slug, entries := range s.state.SpawnedGenerations {
		var kept []SpawnedGeneration
		for _, g := range entries {
			age := now.Sub(g.SpawnedAt)

			if snapshot == nil || age < spawnStartupGrace {
				g.Pending = true
				g.LastCheckedAt = now
				kept = append(kept, g)
				continue
			}

			ev := snapshot[g.TmuxSession]
			switch {
			case !ev.Exists:
				kills = append(kills, spawnKill{Slug: slug, Session: g.TmuxSession, Age: age, Reason: "tmux session no longer exists"})
				s.countSpawnFailureLocked(slug, g.TmuxSession)
			case ev.LiveClaude && age <= spawnRegisterDeadline:
				g.Pending = true
				g.LastCheckedAt = now
				kept = append(kept, g)
			case ev.LiveClaude:
				kills = append(kills, spawnKill{Slug: slug, Session: g.TmuxSession, Age: age, Reason: fmt.Sprintf("alive but never registered within %s", spawnRegisterDeadline)})
				s.countSpawnFailureLocked(slug, g.TmuxSession)
			default:
				kills = append(kills, spawnKill{Slug: slug, Session: g.TmuxSession, Age: age, Reason: "no claude process running in the session"})
				s.countSpawnFailureLocked(slug, g.TmuxSession)
			}
		}
		if len(kept) == 0 {
			delete(s.state.SpawnedGenerations, slug)
		} else {
			s.state.SpawnedGenerations[slug] = kept
		}
	}
	return kills
}

// countSpawnFailureLocked increments SpawnFailures[slug] and clears any
// still-fresh PendingSpawns entry for slug — without this, a slug whose
// generation the evidence pass just resolved as a failure would still read
// as "an accepting agent is on the way" via PendingSpawns for up to
// spawnIntentTTL after the failure was already confirmed, delaying a
// legitimate respawn for no reason (see effectiveAcceptingCountLocked's
// doc comment). Also records the failed session for
// logSpawnSuspendedLocked's log line. Must be called with s.mu held.
func (s *Server) countSpawnFailureLocked(slug, tmuxSession string) {
	if s.state.SpawnFailures == nil {
		s.state.SpawnFailures = map[string]int{}
	}
	s.state.SpawnFailures[slug]++
	delete(s.state.PendingSpawns, slug)
	if s.spawnLastFailedSession == nil {
		s.spawnLastFailedSession = map[string]string{}
	}
	s.spawnLastFailedSession[slug] = tmuxSession
}

// recordSpawnFailureLocked is spawnOrchestrator's failure path: the
// spawnFunc call itself errored (never even produced a viable session), so
// unlike evaluateSpawnEvidenceLocked there's no tmux evidence to weigh —
// remove the tracked entry outright and count the failure. Must be called
// with s.mu held.
func (s *Server) recordSpawnFailureLocked(slug, tmuxSession string) {
	entries := s.state.SpawnedGenerations[slug]
	for i, g := range entries {
		if g.TmuxSession == tmuxSession {
			s.state.SpawnedGenerations[slug] = append(entries[:i], entries[i+1:]...)
			if len(s.state.SpawnedGenerations[slug]) == 0 {
				delete(s.state.SpawnedGenerations, slug)
			}
			break
		}
	}
	s.countSpawnFailureLocked(slug, tmuxSession)
}

// logSpawnSuspendedLocked logs the "spawn SUSPENDED" warning for slug, at
// most once per spawnSuspendLogThrottle (see spawnSuspendLoggedAt) — called
// only from the two spawn-decision paths, and only when they've otherwise
// decided a spawn is warranted but the cap refuses it, so a suspended slug
// with no routable work stays silent. Must be called with s.mu held.
func (s *Server) logSpawnSuspendedLocked(slug string, now time.Time) {
	if last, ok := s.spawnSuspendLoggedAt[slug]; ok && now.Sub(last) < spawnSuspendLogThrottle {
		return
	}
	if s.spawnSuspendLoggedAt == nil {
		s.spawnSuspendLoggedAt = map[string]time.Time{}
	}
	s.spawnSuspendLoggedAt[slug] = now
	log.Printf("serve: spawn SUSPENDED for %s: %d consecutive orchestrators never registered (last: %s); fix the cause then POST /spawn/pause {\"paused\":false} or restart",
		slug, s.state.SpawnFailures[slug], s.spawnLastFailedSession[slug])
}
