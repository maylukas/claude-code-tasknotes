package tn

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- Repo-note parallelism settings ---

// repoSettingsCacheTTL bounds how often a repo note's frontmatter is
// re-read from disk. Short relative to other caches on purpose — the
// whole point is that editing max-orchestrators/max-workers in Obsidian
// takes effect live, without a daemon restart.
const repoSettingsCacheTTL = 60 * time.Second

// repoSettings is per-repo parallelism config, user-editable via the
// vault's Repos/<slug>.md frontmatter.
type repoSettings struct {
	// MaxOrchestrators caps concurrently ACCEPTING agents for this
	// project. Default 1 — today's single-accepting-generation behavior
	// when unset.
	MaxOrchestrators int
	// MaxWorkers, when set (>0), becomes TN_MAX_WORKERS in the spawn env,
	// taking precedence over a serve.json env entry of the same name.
	// 0 means unset — no override, serve.json's value (if any) stands.
	MaxWorkers int
	// MaxOrchestratorsPresent/MaxWorkersPresent: true if the repo note's
	// frontmatter had this KEY at all — even with an invalid value (non-
	// numeric, zero, negative). Distinguishes "the repo note genuinely
	// doesn't set this" from "it set something that got silently
	// dropped," which the resolved MaxOrchestrators/MaxWorkers fields
	// alone can't — both cases collapse to the same default otherwise.
	// This exact ambiguity is what cost a live investigation clarity: a
	// project's effective maxWorkers reading 0 could mean either "never
	// configured" or "configured with something parseRepoSettings
	// silently ignored," and there was no way to tell which from the
	// outside. See `tn repo`.
	MaxOrchestratorsPresent bool
	MaxWorkersPresent       bool
}

func defaultRepoSettings() repoSettings {
	return repoSettings{MaxOrchestrators: 1, MaxWorkers: 0}
}

// parseRepoSettings extracts max-orchestrators/max-workers from a repo
// note's `---`-fenced frontmatter (simple `key: value` lines — this is
// deliberately not a real YAML parser, just enough to read two integer
// keys tolerantly). A missing/malformed value for a key just leaves the
// resolved value at its default, but the corresponding *Present flag is
// still set as long as the KEY itself appeared (see repoSettings' doc
// comment) — content outside the frontmatter fences, and any other
// keys, are ignored entirely (not even considered "present").
func parseRepoSettings(content string) repoSettings {
	settings := defaultRepoSettings()
	fenceCount := 0
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			fenceCount++
			if fenceCount >= 2 {
				break
			}
			continue
		}
		if fenceCount != 1 {
			continue
		}
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key != "max-orchestrators" && key != "max-workers" {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(value))
		valid := err == nil && n > 0
		switch key {
		case "max-orchestrators":
			settings.MaxOrchestratorsPresent = true
			if valid {
				settings.MaxOrchestrators = n
			}
		case "max-workers":
			settings.MaxWorkersPresent = true
			if valid {
				settings.MaxWorkers = n
			}
		}
	}
	return settings
}

// readRepoSettings reads and parses vaultDir/Repos/<slug>.md. A missing
// file (no repo note, or none yet created) just yields
// defaultRepoSettings() — this is optional user config, never required.
func readRepoSettings(vaultDir, slug string) repoSettings {
	b, err := os.ReadFile(filepath.Join(vaultDir, "Repos", slug+".md"))
	if err != nil {
		return defaultRepoSettings()
	}
	return parseRepoSettings(string(b))
}

// repoSettingsCacheEntry pairs a cached repoSettings with when it was read.
type repoSettingsCacheEntry struct {
	settings  repoSettings
	fetchedAt time.Time
}

// repoSettingsCache holds the last-read repoSettings per project slug. Its
// own mutex, independent of Server.mu, since reading is a filesystem call
// rather than a state mutation.
type repoSettingsCache struct {
	mu      sync.Mutex
	entries map[string]repoSettingsCacheEntry
}

func newRepoSettingsCache() *repoSettingsCache {
	return &repoSettingsCache{entries: map[string]repoSettingsCacheEntry{}}
}

// get returns the cached settings for slug if fetched within ttl of now;
// otherwise it calls fetch, caches the result, and returns it.
func (c *repoSettingsCache) get(slug string, now time.Time, ttl time.Duration, fetch func() repoSettings) repoSettings {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[slug]; ok && now.Sub(e.fetchedAt) < ttl {
		return e.settings
	}
	settings := fetch()
	c.entries[slug] = repoSettingsCacheEntry{settings: settings, fetchedAt: now}
	return settings
}

// ServeConfig is the resolved configuration for `tn serve`.
type ServeConfig struct {
	Port            int                      `json:"port"`
	Projects        map[string]ProjectConfig `json:"projects"`
	DashboardPath   string                   `json:"dashboardPath,omitempty"`
	SessionPagesDir string                   `json:"sessionPagesDir,omitempty"`
	Credentials     CredentialsConfig        `json:"credentials"`
	Worktrees       WorktreeReaperConfig     `json:"worktrees"`
	// OrchestratorDoc is serve.json's "orchestratorDoc" override — see
	// resolveOrchestratorDoc for the full precedence order.
	OrchestratorDoc string `json:"orchestratorDoc,omitempty"`
}

// CredentialsConfig controls automatic credential-profile swapping when
// an orchestrator pane parks on a claude.ai usage limit (see creds.go).
// Off by default: a swap switches EVERY Claude session on the machine.
type CredentialsConfig struct {
	AutoSwap        bool          `json:"autoSwap"`
	MinSwapInterval time.Duration `json:"minSwapInterval"`
}

// Server is the daemon's HTTP handler plus its state and dependencies.
// spawnFunc is injectable so tests never touch tmux.
type Server struct {
	mu        sync.Mutex
	state     *State
	statePath string
	config    ServeConfig
	spawnFunc func(project, cwd string, env map[string]string) error
	notify    chan struct{} // closed and replaced on every new/updated message

	// startedAt is when this Server instance was constructed — the
	// liveness-detection startup grace period (checkOrphanedDeathsOnce,
	// sweepOrphanedOwnershipDebt) measures from this, NOT from the
	// package-level daemonStartedAt used for /status's uptime. Kept
	// separate deliberately: tests construct a fresh Server per test
	// (newTestServer) and need to backdate ITS grace period in isolation
	// without mutating a process-wide var shared by every test in the
	// package.
	startedAt time.Time

	// seenSinceStart tracks which agent names have had at least one
	// authenticated action (touchAgentLocked — poll, ack, worker
	// declare/end, log, context report) land THIS process's lifetime.
	// Deliberately NOT persisted to state.json/State: it exists precisely
	// to distinguish "genuinely dead" from "simply hasn't had a chance to
	// tell us it's alive yet," and that distinction resets to unknown on
	// every restart by construction — a name present here from a
	// PREVIOUS lifetime would defeat the entire point. An agent absent
	// from this map is UNKNOWN, not dead, to checkOrphanedDeathsOnce and
	// sweepOrphanedOwnershipDebt: neither may act on it until it's been
	// observed at least once. See the 2026-08-25 incident (orphanSweep
	// Enabled's doc comment) — restart alone flagged 11 of 11 registered
	// agents, including the only genuinely live one, because none of them
	// had polled since the process came back up.
	seenSinceStart map[string]bool

	idSeq int64

	webhookWarnedOnce bool

	dueScannerWarnedOnce bool

	webhookMissWarnedOnce bool

	// mrWatchWarnedOnce tracks which task paths already had an MR-lookup
	// failure (unparseable URL or glab error) warned about for the CURRENT
	// outage, so a persistently-failing lookup doesn't spam the log every
	// mrWatchInterval. Cleared per-task the moment that task's lookup
	// succeeds again.
	mrWatchWarnedOnce map[string]bool

	// mrReviewsWarnedOnce is mrWatchWarnedOnce's sibling for the
	// review-comment discussions fetch (see checkTaskMRReviews) — a
	// separate map, not a shared one with a key suffix, since the two
	// fetches (MR state vs. discussions) fail independently and a warning
	// for one must not suppress a warning for the other.
	mrReviewsWarnedOnce map[string]bool

	// mrDiscussionsFunc resolves review-comment discussions for the MR
	// watcher's checkMRStatesOnce pass (see mrDiscussionsFunc's own doc
	// comment in mr_watcher.go). Nil by default — most existing tests
	// predate the review-comment feature and never set it, and
	// checkMRStatesOnce treats nil as "review watching not wired up this
	// pass" rather than calling a nil function, so none of them needed to
	// change when this field was added. Set once by startMRWatcher (to
	// glabMRDiscussions) before its ticker goroutine begins; a test that
	// DOES exercise review comments assigns a fake directly, same
	// no-lock-needed convention as tnClient.
	mrDiscussionsFunc mrDiscussionsFunc

	// unsupportedHostWarnedOnce tracks which task paths already had an
	// "unsupported change-request URL" (no codeHost provider matched, and
	// no valid override) warned about for the CURRENT outage — same
	// per-task-per-outage dedup convention as mrWatchWarnedOnce/
	// mrReviewsWarnedOnce. Cleared per-task the moment host resolution
	// succeeds again.
	unsupportedHostWarnedOnce map[string]bool

	// mrCursorWarnedOnce is mrWatchWarnedOnce's sibling for the incremental
	// MR-watcher path's ChangedSince call (see incrementalCodeHost,
	// checkMRStatesOnceCore): tracks which group keys
	// ("<codeHost>|<projectPath>") already had a ChangedSince failure
	// warned about for the CURRENT outage, so a persistently-failing
	// listing doesn't spam the log every pass while its group falls back to
	// per-task polling. Cleared per-key the moment that group's
	// ChangedSince call succeeds again.
	mrCursorWarnedOnce map[string]bool

	// mrWatchPassCount counts checkMRStatesOnceCore passes (real watcher
	// ticks and the legacy checkMRStatesOnce test wrapper alike). Used only
	// to trigger mrReviewRefreshEvery's periodic full discussion refresh
	// for the incremental path — every task the incremental listing found
	// unchanged still gets its discussions re-fetched on that cadence,
	// since resolving a review thread bumps an MR's updated_at on GitLab
	// but the watcher shouldn't rely on that alone. Guarded by mu, same as
	// the other simple counters in this struct.
	mrWatchPassCount int

	// codeHostFunc resolves the codeHost provider for a change-request URL
	// (see resolveCodeHost in codehost.go). nil (the zero value) means "use
	// resolveCodeHost" — tests that exercise checkMRStatesOnceReal directly
	// set this field to a fake resolver so no test ever execs glab or gh;
	// the ~30 legacy MR-watcher tests instead call the checkMRStatesOnce
	// compatibility wrapper, which never consults this field at all.
	codeHostFunc func(changeURL string, override string) (codeHost, error)

	dashboardPath    string
	dashboardDeb     *debouncer
	lastDashboardErr string

	sessionPagesDir    string
	sessionPagesDeb    *debouncer
	lastSessionPageErr string

	// tnClient is the TaskNotes API client used by the dashboard's Needs
	// your action section. Set by cmdServe after construction; nil in tests
	// that don't wire one up (treated as the API being unreachable).
	tnClient    *Client
	needsAction needsActionCache

	// stuck tracks per-orchestrator stuck/unackedBacklog detection state. Its own
	// mutex, independent of mu: the approval dialog can block for up to
	// 120s, and must never be called while mu is held (that would freeze
	// every other bridge endpoint for up to two minutes).
	stuck *stuckTracker

	// sse fans out "something changed" to every GET /events client — see
	// triggerRenders/handleEvents. Its own mutex for the same reason as
	// stuck: a slow/stuck SSE client must never be able to block s.mu.
	sse *sseHub

	// lastSleepAt/lastSleepDuration record the most recently detected host
	// sleep (see startSleepDetector) — zero value means none detected this
	// process lifetime. Entirely in-memory, like stuckTracker: a fresh
	// daemon restart simply has nothing to report until the next sleep.
	// Guarded by mu, same as dashboardPath and the other simple fields.
	lastSleepAt       time.Time
	lastSleepDuration time.Duration

	// killSession is what handleRetire uses to actually kill a retiring
	// agent's tmux session. nil (the zero value) means "use the real tmux
	// kill-session command" — tests set this field directly so retire tests
	// never touch tmux.
	killSession tmuxKillSessionFunc

	// stuckCapture and stuckSendKeys are what handleApprove uses to
	// actually touch tmux. nil (the zero value) means "use the real
	// capturePaneReal/sendKeysReal" — tests set these fields directly so
	// approve tests never touch tmux.
	stuckCapture  captureFunc
	stuckSendKeys sendKeysFunc

	// approveRescanDelay/approveRescanDone: see handleApprove's post-send
	// re-scan. Guarded by mu. approveRescanDone is a tests-only completion
	// hook (nil in production).
	approveRescanDelay time.Duration
	approveRescanDone  func()

	// creds is the credential-profile store (creds.go). nil in tests that
	// don't set it and in newServer itself — only cmdServe wires the real
	// Keychain-backed store, so no test can ever reach /usr/bin/security
	// by accident. Endpoints answer 503 while nil; auto-swap is skipped.
	creds *credsStore

	// reaper is the worktree reaper (reaper.go). nil in tests that don't
	// set it; cmdServe always wires one (the periodic sweep itself only
	// runs when worktrees.reap is on — the manual endpoint works
	// regardless).
	reaper *worktreeReaper

	// repoSettings caches per-project parallelism config read from the
	// vault's Repos/<slug>.md frontmatter (see repoSettingsFor).
	repoSettings *repoSettingsCache

	// statusTaskCounts caches /status's per-project task-status counts —
	// a single global TaskNotes query, not one per project (see
	// buildStatusTaskCountsQuery).
	statusTaskCounts statusTaskCountsCache

	// statusAskBrief caches /status's needsActionTasks ask/brief text
	// (see statusAskBriefCache).
	statusAskBrief statusAskBriefCache

	// orchestratorDoc is the resolved path to ORCHESTRATOR.md, set by
	// cmdServe (via resolveOrchestratorDoc) before startRenderers/spawn
	// wiring — see resolveOrchestratorDoc's doc comment for the
	// precedence order. Empty in tests that construct a Server directly
	// without setting it; buildOrchestratorPrompt is called with an
	// explicit path in that case, not this field.
	orchestratorDoc string
}

func newServer(statePath string, cfg ServeConfig, spawnFunc func(project, cwd string, env map[string]string) error) *Server {
	s := &Server{
		state:                     loadState(statePath),
		statePath:                 statePath,
		config:                    cfg,
		spawnFunc:                 spawnFunc,
		notify:                    make(chan struct{}),
		dashboardPath:             cfg.DashboardPath,
		sessionPagesDir:           cfg.SessionPagesDir,
		stuck:                     newStuckTracker(),
		sse:                       newSSEHub(),
		mrWatchWarnedOnce:         map[string]bool{},
		mrReviewsWarnedOnce:       map[string]bool{},
		unsupportedHostWarnedOnce: map[string]bool{},
		mrCursorWarnedOnce:        map[string]bool{},
		repoSettings:              newRepoSettingsCache(),
		startedAt:                 time.Now(),
		seenSinceStart:            map[string]bool{},
		approveRescanDelay:        defaultApproveRescanDelay,
	}
	if s.dashboardPath != "" {
		s.dashboardDeb = newDebouncer(dashboardDebounceInterval, s.renderDashboard)
	}
	if s.sessionPagesDir != "" {
		s.sessionPagesDeb = newDebouncer(dashboardDebounceInterval, s.renderSessionPages)
	}
	return s
}

// startRenderers launches the debounce workers for the dashboard note and
// per-session activity pages, plus a shared periodic re-render tick (to
// catch alive→dead flips that happen without a mutation). No-op for either
// renderer that isn't configured. Never called from tests, which exercise
// the render functions/debouncer directly instead.
func (s *Server) startRenderers() {
	if s.dashboardDeb != nil {
		go s.dashboardDeb.run()
		s.dashboardDeb.Trigger() // initial render so the note exists right away
	}
	if s.sessionPagesDeb != nil {
		go s.sessionPagesDeb.run()
		s.sessionPagesDeb.Trigger()
	}
	if s.dashboardDeb == nil && s.sessionPagesDeb == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			s.triggerRenders()
		}
	}()
}

// triggerRenders requests a debounced re-render of the dashboard note and
// per-session activity pages. Safe to call under s.mu (it never re-acquires
// it) and safe to call when either or both are disabled.
func (s *Server) triggerRenders() {
	if s.dashboardDeb != nil {
		s.dashboardDeb.Trigger()
	}
	if s.sessionPagesDeb != nil {
		s.sessionPagesDeb.Trigger()
	}
	if s.sse != nil {
		// triggerRenders is called from dozens of sites throughout this
		// file, some still holding s.mu at the call point and some not —
		// auditing every one to guarantee "always unlocked here" isn't
		// practical, and getting it wrong would stall whichever handler
		// holds s.mu on hub.mu contention (the SAME class of mistake the
		// stuck detector's own mutex exists to avoid, just for a much
		// less severe case — sseHub's operations are fast, never a
		// 120s-blocking dialog, but "fast" isn't "free"). `go` makes the
		// notify unconditionally run outside whatever lock scope the
		// caller happens to be in, without needing to know what that is.
		go s.sse.notify()
	}
}
