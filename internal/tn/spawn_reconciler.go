package tn

import (
	"log"
	"os"
	"strings"
	"time"
)

// --- Spawn reconciler ---

const reconcilerInterval = 30 * time.Second

// startSpawnReconciler launches a background ticker that retries spawning
// orchestrators for autoSpawn projects that have queued work but no live
// agent. Covers the gap where a webhook-triggered spawn fails (or hasn't
// finished starting up yet): the "Task assigned:" dedup then suppresses any
// later retry via the normal routing path, so without this the queued work
// strands. spawnFunc is idempotent per tmux session, so retrying a spawn
// that's already in flight is a harmless no-op. Disabled when
// TN_NO_RECONCILER=1 (tests and smoke runs that start the real daemon
// should set this).
func (s *Server) startSpawnReconciler() {
	if os.Getenv("TN_NO_RECONCILER") == "1" {
		return
	}
	go func() {
		ticker := time.NewTicker(reconcilerInterval)
		defer ticker.Stop()
		for range ticker.C {
			s.reconcileSpawnsOnce()
		}
	}()
}

// buildStartableOpenTasksQuery builds the FilterQuery for the reconciler's
// scale-out and spawn-on-pending-work triggers: open, non-archived,
// claude-tagged tasks not blocked by an incomplete dependency. Global, not
// project-scoped — TaskNotes has no queryable "routing slug" property (see
// routingSlugForTask) — so reconcileSpawnsOnce buckets the results
// per-project client-side, the same pattern the due-task scanner and MR
// watcher already use.
func buildStartableOpenTasksQuery() FilterGroup {
	return FilterGroup{
		Type:        "group",
		ID:          "root",
		Conjunction: "and",
		Children: []FilterCondition{
			{Type: "condition", ID: "archived", Property: "archived", Operator: "is-not-checked"},
			{Type: "condition", ID: "status-open", Property: "status", Operator: "is", Value: "open"},
			{Type: "condition", ID: "not-blocked", Property: "dependencies.isBlocked", Operator: "is-not-checked"},
			{Type: "condition", ID: "tag-claude", Property: "tags", Operator: "contains", Value: "claude"},
		},
		SortKey:       "due",
		SortDirection: "asc",
		GroupKey:      "none",
	}
}

// defaultMaxWorkers is the effective max-workers used in the reconciler's
// saturation math when a project has no repo-note max-workers override —
// matches ORCHESTRATOR.md's documented $TN_MAX_WORKERS default. Purely a
// scale-out heuristic input; it does NOT itself get injected as an env var
// (only an explicit repo-note max-workers does — see spawnOrchestrator).
const defaultMaxWorkers = 3

// reconcileSpawnsOnce is a single reconciler pass, factored out of the
// ticker loop so it's directly testable. For every configured autoSpawn
// project, spawns a new generation when EITHER:
//   - no alive ACCEPTING agent exists (matched case-insensitively, a
//     draining-only generation doesn't count, per SPEC-generations.md) AND
//     there's routable work — a queued message addressed to
//     orchestrator-<slug> (the original trigger) OR a startable open
//     claude-tagged task (so a draining orchestrator plus an idle-but-
//     nonempty open queue no longer deadlocks waiting for an external
//     event); OR
//   - the accepting count is below the project's max-orchestrators (repo
//     note, default 1) AND the startable-task backlog exceeds what the
//     current accepting generations can plausibly keep up with
//     (acceptingCount * effective max-workers) — scale out another.
//
// The startable-task count comes from ONE global TaskNotes query (see
// buildStartableOpenTasksQuery), skipped entirely when no project is
// autoSpawn or s.tnClient is nil; a query failure just degrades both new
// triggers to "no startable work known" (falls back to the original
// queued-message-only behavior) rather than blocking the pass. Spawns run
// outside the lock.
func (s *Server) reconcileSpawnsOnce() {
	type spawnCandidate struct {
		slug string
		cwd  string
	}
	type projectEntry struct {
		slug string
		cfg  ProjectConfig
	}

	s.mu.Lock()
	var autoSpawnProjects []projectEntry
	for slug, cfg := range s.config.Projects {
		if cfg.AutoSpawn {
			autoSpawnProjects = append(autoSpawnProjects, projectEntry{slug, cfg})
		}
	}
	s.mu.Unlock()
	if len(autoSpawnProjects) == 0 {
		return
	}

	settingsBySlug := make(map[string]repoSettings, len(autoSpawnProjects))
	for _, pe := range autoSpawnProjects {
		settingsBySlug[pe.slug] = s.repoSettingsFor(pe.slug)
	}

	startableCountBySlug := map[string]int{}
	if s.tnClient != nil {
		if tasks, err := s.tnClient.QueryTasksRaw(buildStartableOpenTasksQuery()); err == nil {
			for _, t := range tasks {
				if slug := strings.ToLower(routingSlugForTask(t)); slug != "" {
					startableCountBySlug[slug]++
				}
			}
		} else {
			log.Printf("serve: reconciler: TaskNotes API query for startable tasks failed: %v", err)
		}
	}

	var candidates []spawnCandidate
	s.mu.Lock()
	paused := s.state.SpawnPaused
	for _, pe := range autoSpawnProjects {
		slug, cfg := pe.slug, pe.cfg
		orchestratorName := "orchestrator-" + slug

		hasQueued := false
		for _, m := range s.state.Messages {
			if m.To == orchestratorName && m.Status == "queued" {
				hasQueued = true
				break
			}
		}

		// effectiveAcceptingCountLocked, not a local live-agent count: the
		// same helper resolveTargetLocked's on-demand spawn path uses, so
		// the cap can't diverge between the two paths again — that
		// divergence (each independently counting only live agents) is
		// exactly what let both decide to spawn ~11s apart and exceed
		// max-orchestrators.
		acceptingCount := s.effectiveAcceptingCountLocked(slug)

		startable := startableCountBySlug[slug]
		settings := settingsBySlug[slug]
		effectiveMaxWorkers := settings.MaxWorkers
		if effectiveMaxWorkers <= 0 {
			effectiveMaxWorkers = defaultMaxWorkers
		}

		noAcceptingButRoutable := acceptingCount == 0 && (hasQueued || startable > 0)
		scaleOutSaturated := acceptingCount < settings.MaxOrchestrators && startable > acceptingCount*effectiveMaxWorkers
		if !noAcceptingButRoutable && !scaleOutSaturated {
			continue
		}
		candidates = append(candidates, spawnCandidate{slug: slug, cwd: cfg.Cwd})
		// Record the intent at decision time, under the same lock — not
		// paused: while paused nothing will actually spawn, and recording
		// an intent anyway would waste spawnIntentTTL suppressing a real
		// spawn once spawning resumes.
		if !paused {
			s.recordSpawnIntentLocked(slug)
		}
	}
	s.mu.Unlock()

	if len(candidates) == 0 {
		return
	}
	if paused {
		log.Printf("serve: spawning paused, %d project(s) would have spawned", len(candidates))
		return
	}

	for _, c := range candidates {
		log.Printf("serve: reconciler spawning orchestrator for %s", c.slug)
		s.spawnOrchestrator(c.slug, c.cwd)
	}
}
