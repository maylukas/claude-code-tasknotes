// Daemon for `tn serve`: an HTTP bridge between the TaskNotes plugin and
// Claude Code agent sessions running on this machine. It keeps a registry of
// agent sessions per project, a message queue consumed via inbox
// long-polling, and reacts to TaskNotes webhooks by routing task assignments
// and messages to the right agent session.
package main

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// daemonStartedAt is when this process started, used to compute /status
// and /health's uptime.
var daemonStartedAt = time.Now()

// daemonVersion is the tray-app-facing daemon version, surfaced via
// /status and /health. Bump on notable changes (see CLAUDE.md).
const daemonVersion = "0.7.0"

// aliveWindow is how recently an agent must have polled its inbox (or
// registered) to be considered alive.
const aliveWindow = 90 * time.Second

// orphanSweepEnabled reports whether liveness-based orphan detection — the
// periodic died-without-retiring check (checkOrphanedDeathsOnce) and the
// startup ownership-debt sweep (sweepOrphanedOwnershipDebt) — is allowed to
// actually reset tasks. Defaults DISABLED, opt in with
// TN_ENABLE_ORPHAN_SWEEP=1. This is the inverse of TN_NO_REPAIR/
// TN_NO_STUCKCHECK's default-on-opt-out pattern, deliberately: those gate
// mechanisms with no known false-positive path, this one gates a mechanism
// with a CONFIRMED one. Incident (2026-08-25): checkOrphanedDeathsOnce
// declared orchestrator-myapp-g84221 dead 3m11s after it genuinely acked a
// task and dispatched a worker — LastSeenAt only advanced on inbox poll, so
// synchronous work (deciding, dispatching) went dark. The task was reset
// and reassigned while g84221's own worker kept running, and both produced
// MRs for the same fix. Worse: EVERY agent's LastSeenAt predates a daemon
// restart, so every agent looks dead until it next polls — and the startup
// sweep runs inside exactly that window on every restart. Does NOT gate
// handleRetire's own orphan check (triggerOrphanCheckIfNeeded called from a
// clean retirement): that path is driven by the agent's own explicit
// action, not a liveness guess, and has no false-positive risk to gate.
// Re-enable once the startup grace period, two-tick confirmation, and
// activity-based liveness fixes are in place and verified.
func orphanSweepEnabled() bool {
	return os.Getenv("TN_ENABLE_ORPHAN_SWEEP") == "1"
}

// Agent is a registered agent session for a project.
type Agent struct {
	Name           string     `json:"name"`
	Project        string     `json:"project"`
	SessionID      string     `json:"sessionId,omitempty"`
	RegisteredAt   time.Time  `json:"registeredAt"`
	LastSeenAt     time.Time  `json:"lastSeenAt"`
	DeregisteredAt *time.Time `json:"deregisteredAt,omitempty"`
	// Accepting is true on register (re-register resets it to true) and
	// false once the agent has run `tn drain` — generations & drain (see
	// SPEC-generations.md): a draining agent keeps polling/acking and
	// finishing what it owns, it just stops receiving NEW task assignments,
	// so a fresh generation can take over new work without a hard cutover.
	Accepting bool `json:"accepting"`
	// TmuxSession is the tmux session this agent registered from, if it
	// told us (generation-aware spawns always do). Everything that used to
	// derive a session name from the agent name (stuck detector, dashboard
	// Attach column, session pages) prefers this when set — see
	// tmuxSessionForAgent — falling back to the legacy tn-<project>
	// derivation only for agents that never reported one.
	TmuxSession string `json:"tmuxSession,omitempty"`
	// ContextPct is the last-reported context-window usage percentage
	// (0-100), fed by the user's statusline wrapper via POST
	// /agents/context. Nil until at least one report has arrived — nil,
	// not merely zero, is what "unknown" means here, since 0% is itself a
	// valid report. Crossing drainContextPct while Accepting auto-drains
	// the agent (see handleContext).
	ContextPct *float64 `json:"contextPct,omitempty"`
	// ContextPctAt is when ContextPct was last reported.
	ContextPctAt time.Time `json:"contextPctAt,omitempty"`
	// OrphanHandledAt marks that the orphaned-in-progress-task safety net
	// has already run for this agent's CURRENT lifecycle (either it retired
	// cleanly, or it died without retiring and was caught by the
	// liveness-transition check) — zero value means "not yet handled".
	// Reset to zero on every register, so a later death of a re-registered
	// name is eligible for its own, separate check ("once per agent per
	// death", not once ever).
	OrphanHandledAt time.Time `json:"orphanHandledAt,omitempty"`
	// ConsecutiveDeadTicks counts how many checkOrphanedDeathsOnce passes
	// in a row have observed this agent as not-alive (agentAliveAt false).
	// Reset to 0 the moment it's observed alive, and on every fresh
	// register (same "new lifecycle" reasoning as OrphanHandledAt) — a
	// single missed poll must not read as death; see deadConfirmTicks.
	ConsecutiveDeadTicks int `json:"consecutiveDeadTicks,omitempty"`
	// HookConfirmedAt is set the first time this agent's Claude Code
	// PermissionRequest hook is ever observed to fire (POST
	// /hooks/permission-request with this agent's name) — proof that its
	// hook plumbing (settings config, env var propagation) actually works,
	// not merely that it was configured to. Nil means UNPROVEN, never
	// "not stuck": the same three-state discipline as
	// Server.seenSinceStart/deadConfirmTicks (see SPEC-serve.md's design
	// rule) applied to a different absence — a missing hook signal must
	// never be read as "this agent is fine" when it could equally mean
	// "this agent's hook never got wired up." Only an agent with a non-nil
	// HookConfirmedAt gets the near-instant hookTriggered confirmation
	// path in updateStuckState; pane-scraping stays authoritative for
	// every other agent, permanently — not a transitional fallback, since
	// hooks structurally cannot see every case (a hard permissions.deny
	// rule denies before ANY hook fires, confirmed empirically 2026-08-26).
	// Persisted on the Agent record itself (unlike seenSinceStart) because
	// the fact it proves — this agent's own environment has working hook
	// plumbing — is scoped to the agent's identity, not the daemon's
	// process lifetime; a daemon restart doesn't invalidate it the way it
	// invalidates "have I personally heard from you since I came up."
	HookConfirmedAt *time.Time `json:"hookConfirmedAt,omitempty"`
}

// permissionDenialRecord aggregates PermissionDenied hook observations —
// PURE TELEMETRY, never a decision and never a retry (see SPEC-serve.md's
// "PermissionRequest hook detection" section for the full reasoning,
// including why this was originally dropped from scope and then
// reinstated once the negative evidence behind that drop turned out to
// test the wrong triggers). Keyed by agentName+toolName+CommandSummary,
// not by the exact command text — a denial is about the SHAPE of what
// got blocked (e.g. "git push", "glab mr update"), and the same shape
// recurring with different arguments (different MR numbers, different
// branches) should accumulate into one counter, not a fresh entry each
// time.
type permissionDenialRecord struct {
	AgentName string `json:"agentName"`
	ToolName  string `json:"toolName"`
	// CommandSummary is the first ~2 whitespace-separated tokens of
	// tool_input.command when the denied tool was Bash-shaped (e.g. "git
	// push"), empty otherwise. This is what makes the telemetry actually
	// actionable — "Bash denied 4 times" tells a human much less than
	// "glab mr update denied 4 times".
	CommandSummary string    `json:"commandSummary,omitempty"`
	Count          int       `json:"count"`
	FirstSeenAt    time.Time `json:"firstSeenAt"`
	LastSeenAt     time.Time `json:"lastSeenAt"`
	// LastCommand is the full command text from the MOST RECENT denial —
	// enough for a human to see exactly what was blocked without needing
	// every historical occurrence.
	LastCommand string `json:"lastCommand,omitempty"`
}

// Message is a queued message routed to an agent's inbox.
type Message struct {
	ID          string     `json:"id"`
	To          string     `json:"to"`
	Project     string     `json:"project,omitempty"`
	TaskPath    string     `json:"taskPath,omitempty"`
	Text        string     `json:"text"`
	From        string     `json:"from,omitempty"`
	Status      string     `json:"status"` // queued|delivered|acked|archived
	CreatedAt   time.Time  `json:"createdAt"`
	DeliveredAt *time.Time `json:"deliveredAt,omitempty"`
	AckedAt     *time.Time `json:"ackedAt,omitempty"`
	Response    string     `json:"response,omitempty"`
	// ArchivedAt/ArchiveReason are set only by reassignStrandedMessages when
	// a pending message addressed to a confirmed-dead agent references a
	// task that's already done — reassigning it would dump a six-day-old
	// "Task assigned:" directive on a live orchestrator for work that's
	// already finished, exactly the duplicate-work problem this project
	// keeps paying for (see that function's doc comment for the live
	// incident: 31 messages stranded on dead generations, 7 were assignment
	// directives, most already done via other means). Never used for the
	// original queued->delivered->acked flow — this is a fourth, terminal
	// outcome for a message nobody will ever act on, kept (not deleted) so
	// there's a durable record of what was discarded and why, same
	// tombstone-not-delete philosophy as Agent.DeregisteredAt.
	ArchivedAt    *time.Time `json:"archivedAt,omitempty"`
	ArchiveReason string     `json:"archiveReason,omitempty"`
	// Informational marks a message that asks nothing of its recipient —
	// pure FYI, e.g. "died/retired with N in-progress tasks, already reset
	// to open automatically, no adoption needed". Set only by internal
	// dispatchMessage callers via sendRequest.Informational, never by an
	// external POST /messages body — same shape as SkipOwnerRouting.
	// Excluded from oldestDeliveredUnackedAge (see its own doc comment):
	// nothing was asked of the recipient, so there's nothing to ack, and an
	// agent that never acks it is not evidence of anything wrong. 2026-08-25
	// incident: a bridge-authored informational message sat delivered-but-
	// unacked for ~3h and flagged a demonstrably working agent (fresh
	// LastSeenAt, a 42-minute/184k-token subagent actively running) as
	// "stalled" — the field's name AT THE TIME — when what it actually
	// measured was inbox hygiene, not agent health. Renamed to
	// UnackedBacklog as part of the same fix (see stuckInfo.UnackedBacklog).
	Informational bool `json:"informational,omitempty"`
}

// State is the full persisted state of the daemon.
type State struct {
	Agents   map[string]*Agent `json:"agents"`
	Messages []*Message        `json:"messages"`
	// TaskDetailsHash is a cheap short-circuit for @claude: line routing:
	// sha256 hex of a task's whole details blob, so an unchanged-details
	// webhook event skips line extraction entirely. Line-level dedup itself
	// lives in RoutedClaudeLines.
	TaskDetailsHash map[string]string `json:"taskDetailsHash"`
	// RoutedClaudeLines maps a task's path to the sha256 hex hashes of
	// @claude: line texts (trimmed) already routed for it, so dedup
	// survives arbitrary detail edits (e.g. tn note appends changing the
	// overall hash) instead of re-routing every previously-routed line
	// whenever anything about the details changes.
	RoutedClaudeLines map[string][]string `json:"routedClaudeLines"`
	Activity          []*ActivityEntry    `json:"activity"`
	// AssignedOccurrences maps a due-task's taskPath to the "YYYY-MM-DD" date
	// it was last routed by the due-task scanner, so a recurring/scheduled
	// task is assigned at most once per due day.
	AssignedOccurrences map[string]string `json:"assignedOccurrences"`
	// LastReconciledAt is the start time of the last SUCCESSFUL
	// reconcileWebhookMissesRemote pass (query ok, every candidate task's
	// details fetched). The next pass queries from this watermark minus a
	// buffer; a failed pass leaves it unchanged so the same window is
	// retried. Zero value (fresh state) means "no successful pass yet".
	LastReconciledAt time.Time `json:"lastReconciledAt"`
	// SpawnPaused is the menu-bar client's manual spawn kill-switch (POST
	// /spawn/pause): while true, every spawn path skips actually spawning
	// (see spawnOrchestrator's guard and reconcileSpawnsOnce's batch
	// check) — everything else (routing, drain, retire, MR watcher,
	// stuck-prompt approval) keeps working normally.
	SpawnPaused bool `json:"spawnPaused"`
	// MRStates maps a task's path to the last-observed GitLab MR state
	// ("opened"/"merged"/"closed") for its customProperties.mr field, so
	// the MR watcher only acts on an observed CHANGE, never re-transitions
	// on every pass. A task's first-ever observation seeds this map without
	// transitioning (like the other migration-style dedup maps) — see
	// checkMRStateOnce.
	MRStates map[string]string `json:"mrStates"`
	// AssignedTasks maps a task's path to the RFC3339 timestamp of the last
	// assignment routed for it, making assignment one-shot per "open
	// episode" rather than per-ack. hasPendingAssignmentLocked alone only
	// blocks while the PRIOR assignment message is unacked — and orchestrators
	// ack immediately per contract — so without this, every subsequent
	// webhook-miss-reconciler pass (every 10min) re-assigns any task that
	// legitimately just sits in "open", visibly reverting its status back to
	// in-progress each time. See markAssignedLocked/observeTaskStatusLocked:
	// an entry is set on every routed assignment and cleared the moment the
	// task is next observed with a status outside assignableStatuses, so a
	// genuine reopen still gets assigned exactly once for its new episode.
	AssignedTasks map[string]string `json:"assignedTasks"`
	// LastKnownStatus maps a task's path to the last status observed for
	// it — used ONLY to detect a genuine status CHANGE for Jira transition
	// requests (see checkJiraTransitionLocked). A task's first-ever
	// observation seeds this without firing a request, like the other
	// migration-style dedup maps in this file, so shipping this feature
	// doesn't flood every existing jira-tagged task with a request the
	// moment it's enabled.
	LastKnownStatus map[string]string `json:"lastKnownStatus"`
	// JiraRequests maps a composite key ("<taskPath>|<jiraKey>|<target>",
	// where <target> is a status category for a transition request or the
	// literal "nc-comment" for a jira-nc comment request) to the RFC3339
	// timestamp of the last request emitted for it — the Jira-request
	// equivalent of AssignedTasks: one-shot per (task, key, target) until
	// the task's status changes to something else, at which point every
	// entry for that path is cleared (a new status episode can request
	// the same target again later).
	JiraRequests map[string]string `json:"jiraRequests"`
	// PendingSpawns maps a project slug to the time a spawn was DECIDED
	// (not yet necessarily registered) — closes the check-then-act race
	// between the two independent spawn paths (dispatchMessage's
	// on-demand spawn and reconcileSpawnsOnce's periodic pass): a real
	// spawn is slow (tmux + claude startup + registration, observed
	// multiple seconds), so both paths could independently observe zero
	// accepting agents and both spawn, exceeding max-orchestrators. See
	// effectiveAcceptingCountLocked, recordSpawnIntentLocked,
	// clearSpawnIntentLocked.
	PendingSpawns map[string]time.Time `json:"pendingSpawns"`
	// CreatedTasks maps a client-supplied idempotency key (`tn create`'s
	// hash of title+claude-project+details) to the task it created —
	// makes a `tn create` retry after a client-side timeout harmless
	// rather than merely rarer: TaskNotes' own create handler can take
	// long enough to trip the CLI's HTTP timeout even though the create
	// actually lands, so the retry must be recognized as the SAME request,
	// not a new one. See handleCreateTask/createIdempotencyTTL.
	CreatedTasks map[string]createdTaskRecord `json:"createdTasks"`
	// Workers maps an agent name to the worker subtasks it has declared
	// via `tn worker start`, keyed within each slice by TaskPath (a
	// second `start` for the same path refreshes StartedAt rather than
	// duplicating — see handleWorkerStart). Entirely orchestrator-
	// reported: the daemon never infers this on its own, and an
	// orchestrator that never calls `tn worker start/end` simply shows no
	// workers (SPEC-ui-v2.md #2 — the ORCHESTRATOR.md contract change
	// that gets real orchestrators actually calling this is a separate,
	// non-daemon change).
	Workers map[string][]workerEntry `json:"workers"`
	// DoneMeansNoted marks a task's path once mechanism 2's DONE MEANS
	// check has already appended its "missing evidence" informational note
	// for the task's CURRENT done episode — at most one note per episode,
	// not one per webhook/reconciler observation of an unchanged done
	// task. Cleared the moment the task is observed leaving "done"
	// (self-healing, mirrors AssignedTasks/observeTaskStatusLocked), so a
	// genuine reopen-then-reclose is checked fresh. See
	// noteDoneMeansGapIfUnmet — this ledger is purely about note dedup; it
	// never gates or reverts a status write.
	DoneMeansNoted map[string]bool `json:"doneMeansNoted"`
	// PermissionDenials is the PermissionDenied hook's telemetry ledger —
	// see permissionDenialRecord's doc comment. Keyed by
	// "<agentName>|<toolName>|<CommandSummary>".
	PermissionDenials map[string]*permissionDenialRecord `json:"permissionDenials"`
}

// workerEntry is one declared worker subtask under an owning agent.
type workerEntry struct {
	TaskPath  string    `json:"taskPath"`
	StartedAt time.Time `json:"startedAt"`
}

// createdTaskRecord is CreatedTasks' value: just enough to answer a
// repeated idempotency-keyed create request without a second TaskNotes
// round trip (which could itself be the slow/timing-out call).
type createdTaskRecord struct {
	Path      string    `json:"path"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"createdAt"`
}

// ActivityEntry is one `tn log` entry appended to an agent's activity feed.
type ActivityEntry struct {
	Agent string    `json:"agent"`
	At    time.Time `json:"at"`
	Text  string    `json:"text"`
	// TaskPath is set only for entries clearly tied to ONE task (currently
	// just the MR-merged/reopened auto-transition entries — see
	// checkMRStateOnce's appendActivityLocked calls) — a multi-task summary
	// entry (e.g. host-sleep recovery listing several paths in Text) leaves
	// this empty rather than picking one arbitrarily. SPEC-ui-v2.md's feed
	// ("each row that belongs to a task links to it") is what this exists
	// for — see buildFeed.
	TaskPath string `json:"taskPath,omitempty"`
}

// ProjectConfig is the per-project section of ~/.config/tn/serve.json.
type ProjectConfig struct {
	AutoSpawn bool                `json:"autoSpawn"`
	Cwd       string              `json:"cwd"`
	Env       map[string]EnvEntry `json:"env,omitempty"`
	// JiraStatusMap maps a TaskNotes task status (in-progress/review/done)
	// to this PROJECT's actual Jira workflow status NAME to target — e.g.
	// "In Development"/"Code Review"/"Merged" — not a status category.
	// Configurable per project because different Jira projects/issue
	// types have different graphs (see Reference/Jira workflows.md): CX
	// Bug's board-"done" status is MERGED, several hops before the
	// category-done status Fertig, which is reached only after
	// acceptance testing and release — a category-only request could
	// both hit the rejection trap AND, done "correctly," falsely mark
	// work as released. A task status with no entry here (including an
	// unconfigured map) falls back to the safer-but-less-precise
	// category-only request.
	JiraStatusMap map[string]string `json:"jiraStatusMap,omitempty"`
}

// EnvEntry describes one per-project environment variable for spawned
// orchestrator sessions. An entry with a non-empty Value is injected into
// the spawn command; an entry with an empty Value is advisory-only — its
// Description explains how to obtain the material at use time (e.g. a
// keychain command) — and is never injected.
type EnvEntry struct {
	Value       string `json:"value,omitempty"`
	Description string `json:"description,omitempty"`
}

// UnmarshalJSON accepts either a plain string (treated as Value, no
// Description) or the {"value":..., "description":...} object form, so
// serve.json can use whichever is more convenient per entry.
func (e *EnvEntry) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		e.Value = s
		e.Description = ""
		return nil
	}
	type envEntryAlias EnvEntry // avoid recursing back into this method
	var a envEntryAlias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*e = EnvEntry(a)
	return nil
}

// injectedEnv returns cfg's environment variables that should actually be
// injected into a spawn command — entries with a non-empty Value. Advisory
// (empty-Value) entries are excluded.
func injectedEnv(cfg ProjectConfig) map[string]string {
	out := make(map[string]string, len(cfg.Env))
	for name, entry := range cfg.Env {
		if entry.Value != "" {
			out[name] = entry.Value
		}
	}
	return out
}

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
		state:              loadState(statePath),
		statePath:          statePath,
		config:             cfg,
		spawnFunc:          spawnFunc,
		notify:             make(chan struct{}),
		dashboardPath:      cfg.DashboardPath,
		sessionPagesDir:    cfg.SessionPagesDir,
		stuck:              newStuckTracker(),
		sse:                newSSEHub(),
		mrWatchWarnedOnce:  map[string]bool{},
		repoSettings:       newRepoSettingsCache(),
		startedAt:          time.Now(),
		seenSinceStart:     map[string]bool{},
		approveRescanDelay: defaultApproveRescanDelay,
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

// --- Orphaned-task safety net ---

// triggerOrphanCheckIfNeeded marks agentName's orphan-death handling done
// for its CURRENT lifecycle (idempotent — a repeat call before the agent
// next registers is a no-op, so a re-retire or a repeated liveness-check
// tick never double-fires) and, only the first time, checks for and
// notifies about any in-progress tasks it still owns, and reassigns/
// archives any pending messages still addressed to it (see
// reassignStrandedMessagesForDeadAgent — for a clean retire this is
// normally a no-op, since handleRetire already reassigned everything
// inline before calling here; it only does real work for a dirty death,
// which has no other code path that ever does). Used by both
// handleRetire's success path (a clean retirement) and
// checkOrphanedDeathsOnce (died without retiring).
func (s *Server) triggerOrphanCheckIfNeeded(agentName, project string) {
	s.mu.Lock()
	a, ok := s.state.Agents[agentName]
	if !ok || !a.OrphanHandledAt.IsZero() {
		s.mu.Unlock()
		return
	}
	a.OrphanHandledAt = time.Now()
	s.saveLocked()
	s.mu.Unlock()

	s.checkOrphanedTasks(agentName, project)
	s.reassignStrandedMessagesForDeadAgent(agentName, project)
}

// hasLiveDeclaredWorkerLocked reports whether ANY alive agent currently
// has a declared worker (`tn worker start`, not yet ended) for path — the
// orphan-reset guard's second half: resetting a task that's actively
// being worked by a live-declared subtask would be worse than the
// ownership-pileup bug this exists to fix. Checks every agent's Workers
// list, not just one candidate owner — a worker can be declared under a
// DIFFERENT agent than whoever originally owned the task (an adopting
// agent that never issued a fresh "Task assigned:" message for it,
// exactly the gap this whole mechanism is patching around). Must be
// called with s.mu held.
func (s *Server) hasLiveDeclaredWorkerLocked(path string) bool {
	now := time.Now()
	for name, entries := range s.state.Workers {
		a, ok := s.state.Agents[name]
		if !ok || !agentAliveAt(*a, now) {
			continue
		}
		for _, e := range entries {
			if e.TaskPath == path {
				return true
			}
		}
	}
	return false
}

// lastActivityForAgentLocked returns the text of agentName's most recent
// activity log entry, or "" if it never logged anything. Most `tn log`
// entries don't carry a TaskPath (see ActivityEntry.TaskPath's doc
// comment — only the two MR-transition call sites do), so this is
// AGENT-scoped, not task-scoped: the closest available signal to "what
// was I doing right before I died," used by buildOrphanResetNote. Must
// be called with s.mu held.
func (s *Server) lastActivityForAgentLocked(agentName string) string {
	var latest *ActivityEntry
	for _, e := range s.state.Activity {
		if e.Agent != agentName {
			continue
		}
		if latest == nil || e.At.After(latest.At) {
			latest = e
		}
	}
	if latest == nil {
		return ""
	}
	return latest.Text
}

// buildOrphanResetNote composes the note appended when a task is reset
// to open after its owner died/retired: the "not abandoned, not
// drifted" cause line (reconcileSleepKilledAgent's same voice), PLUS
// whatever in-flight pointers the daemon already holds — the task's own
// mr/integration-branch/jira custom properties, and the dead agent's
// last logged activity entry, if any. This is what stops the next
// picker from silently redoing work that's already sitting on a branch,
// or colliding with it — frontmatter alone survives the reset, but it's
// easy to miss on a task that no longer LOOKS like it has anything
// attached, since the reset is what clears the in-progress signal that
// would otherwise prompt a look.
//
// Deliberately does NOT go hunting the filesystem for an orphaned
// worktree or uncommitted changes — only surfaces what's already known
// to the daemon, and says so explicitly: none of this is verified
// against what the dead worker actually left behind uncommitted.
func (s *Server) buildOrphanResetNote(agentName string, task Task, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Reset to open: worker %s died/retired at %s without completing. Not abandoned, not drifted.",
		agentName, now.Format("2006-01-02 15:04"))

	var pointers []string
	if mr := task.CustomProperties["mr"]; mr != "" {
		pointers = append(pointers, "MR: "+mr)
	}
	if branch := integrationBranchFor(task); branch != "" {
		pointers = append(pointers, "integration branch: "+branch)
	}
	if jira := task.CustomProperties["jira"]; jira != "" {
		pointers = append(pointers, "Jira: "+jira)
	}
	if ncKey := task.CustomProperties["jira-nc"]; ncKey != "" {
		pointers = append(pointers, "Jira NC: "+ncKey)
	}
	s.mu.Lock()
	lastActivity := s.lastActivityForAgentLocked(agentName)
	s.mu.Unlock()
	if lastActivity != "" {
		pointers = append(pointers, "last logged activity: "+lastActivity)
	}
	if len(pointers) > 0 {
		fmt.Fprintf(&b, " Known pointers: %s.", strings.Join(pointers, "; "))
	}

	b.WriteString(" Any uncommitted or unpushed work from the dead worker may exist and is unverified — check the pointers above before redoing anything.")
	return b.String()
}

// resetOrphanedInProgressTasksForAgent resets every in-progress task
// still owned by agentName (per ownedTaskPathsLocked/taskOwnerLocked)
// back to open, each with a bridge-attributed history note (see
// buildOrphanResetNote — same "not abandoned, not drifted" voice as the
// sleep-death recovery path, plus whatever MR/branch/Jira/last-activity
// pointers the daemon already holds) via bridgeTransitionTask, a single
// GET+PUT round trip per task.
//
// GUARDED, per task, immediately before writing: skipped (left entirely
// alone) if ownership has since moved to someone else (a fresh "Task
// assigned:" message landed — an adopting agent did the right thing) OR
// a live agent currently has a declared worker on it (hasLiveDeclared
// WorkerLocked) — resetting live work out from under it would be worse
// than the bug this fixes. Returns the reset and skipped labels
// ("Title (path)") separately so callers can log/notify accordingly.
// Idempotent by construction: once reset, a task's status is no longer
// "in-progress", so a repeat call for the same agentName (the forward
// path is one-shot per lifecycle anyway — see triggerOrphanCheckIfNeeded
// — and the startup sweep re-derives its candidate list fresh each time)
// finds nothing left to do. s.tnClient must be non-nil; callers check
// that first.
func (s *Server) resetOrphanedInProgressTasksForAgent(agentName string) (reset, skipped []string) {
	s.mu.Lock()
	paths := s.ownedTaskPathsLocked(agentName)
	s.mu.Unlock()

	for _, p := range paths {
		task, err := s.tnClient.GetTask(p)
		if err != nil {
			log.Printf("serve: orphan reset: failed to fetch %s for %s: %v", p, agentName, err)
			continue
		}
		if task.Status != "in-progress" {
			continue
		}

		s.mu.Lock()
		stillOwned := s.taskOwnerLocked(p) == agentName
		hasLiveWorker := s.hasLiveDeclaredWorkerLocked(p)
		s.mu.Unlock()

		label := fmt.Sprintf("%s (%s)", task.Title, p)
		if !stillOwned || hasLiveWorker {
			skipped = append(skipped, label)
			continue
		}

		note := s.buildOrphanResetNote(agentName, task, time.Now())
		if _, err := bridgeTransitionTask(s.tnClient, p, "open", note); err != nil {
			log.Printf("serve: orphan reset: failed to reset %s after %s's death: %v", p, agentName, err)
			continue
		}
		reset = append(reset, label)
	}
	return reset, skipped
}

// checkOrphanedTasks resets agentName's still-in-progress owned tasks to
// open (resetOrphanedInProgressTasksForAgent — the guarded reset is the
// PRIMARY mechanism, not a message asking someone else to do it: the
// original message-only design is what let a week of orphaned tasks pile
// up undetected — see SPEC-note-layout.md-adjacent history and the
// checkOrphanedTasks investigation this replaced). Records an activity
// entry either way, and — only when something was actually reset — sends
// one FYI message to the project so there's a "here's what happened"
// narrative, explicitly framed as context rather than an action item:
// the reset has ALREADY happened by the time this message is read, so
// nothing downstream depends on anyone reading it. No-op if s.tnClient is
// nil or agentName owns nothing still in-progress.
func (s *Server) checkOrphanedTasks(agentName, project string) {
	if s.tnClient == nil {
		return
	}
	reset, skipped := s.resetOrphanedInProgressTasksForAgent(agentName)
	if len(reset) == 0 && len(skipped) == 0 {
		return
	}

	log.Printf("serve: %s left %d orphaned in-progress task(s): %d reset to open, %d left alone (already adopted or have a live worker)",
		agentName, len(reset)+len(skipped), len(reset), len(skipped))

	var activityText string
	switch {
	case len(reset) > 0 && len(skipped) > 0:
		activityText = fmt.Sprintf("Orphaned tasks from %s — reset to open: %s (left alone, already adopted or has a live worker: %s)",
			agentName, strings.Join(reset, "; "), strings.Join(skipped, "; "))
	case len(reset) > 0:
		activityText = fmt.Sprintf("Orphaned tasks from %s reset to open: %s", agentName, strings.Join(reset, "; "))
	default:
		activityText = fmt.Sprintf("Orphaned tasks from %s already adopted or have a live worker, nothing reset: %s", agentName, strings.Join(skipped, "; "))
	}

	// Attributed to "bridge", not agentName: appendActivityLocked's
	// touchAgentLocked side effect bumps LastSeenAt for whoever it's
	// logged against — logging against the dead agent's OWN name would
	// revive it (isAlive again) right before the dispatchMessage call
	// below resolves a target, looping the notification about its own
	// death back to the dead agent instead of a live one.
	s.mu.Lock()
	s.appendActivityLocked("bridge", activityText, "")
	s.saveLocked()
	s.mu.Unlock()
	s.triggerRenders()

	if project == "" || len(reset) == 0 {
		return
	}
	s.dispatchMessage(sendRequest{
		Project: project,
		Text: fmt.Sprintf(
			"FYI, not an action item: %s died/retired with %d in-progress task(s) still assigned; already reset to open automatically, no adoption needed: %s",
			agentName, len(reset), strings.Join(reset, "; "),
		),
		Informational: true,
	})
}

// deadConfirmTicks is how many consecutive checkOrphanedDeathsOnce passes
// must observe an agent as not-alive (Agent.ConsecutiveDeadTicks) before
// it is treated as dead-without-retiring — the same "one miss is not
// enough, two in a row is" discipline that fixed the stuck detector's own
// false positives (see stuckPersistenceBackstopTicks and the
// fastConfirm/backstopConfirm split). A single missed aliveWindow is
// common for an agent doing real synchronous work between polls (see
// orphanSweepEnabled's doc comment for the incident this addresses); two
// consecutive misses is a materially stronger signal.
const deadConfirmTicks = 2

// checkOrphanedDeathsOnce is one pass detecting agents that died WITHOUT
// retiring — not alive for deadConfirmTicks consecutive passes, and never
// cleanly deregistered/retired (DeregisteredAt nil) — and running the
// orphan-task check for each. Runs on the same cadence as the stuck
// detector (see startStuckDetector) since that's where liveness is
// already being observed every tick; no separate ticker needed for
// something this lightweight.
//
// Two layered gates, not one, because a global timer alone was verified
// insufficient (2026-08-25 incident: a restart flagged 11 of 11 registered
// agents, including the only genuinely live one — none had polled yet).
//
//  1. Daemon-level: for aliveWindow after THIS process started, the
//     detector is a complete no-op — a LastSeenAt predating the restart is
//     absence of signal, not evidence of death, for every agent equally.
//  2. Per-agent: an agent absent from s.seenSinceStart (no authenticated
//     action observed yet THIS lifetime) is UNKNOWN, not dead, and stays
//     excluded — no streak accumulation, no candidacy — until its first
//     observed action, however long that takes. Unknown must never
//     authorise a destructive action; only once an agent has proven it CAN
//     report in does a subsequent silence become evidence.
//
// Skipping entirely rather than just not acting also means
// ConsecutiveDeadTicks doesn't start accumulating against an agent that
// hasn't had a fair chance to be seen.
func (s *Server) checkOrphanedDeathsOnce() {
	if time.Since(s.startedAt) < aliveWindow {
		return
	}

	now := time.Now()
	s.mu.Lock()
	var candidates []Agent
	var unknownStale []string // registered, LastSeenAt looks stale, but never observed THIS lifetime
	changed := false
	for _, a := range s.state.Agents {
		if !s.seenSinceStart[a.Name] {
			if a.DeregisteredAt == nil && !agentAliveAt(*a, now) {
				unknownStale = append(unknownStale, a.Name)
			}
			continue // unknown this lifecycle, not dead — no streak, no candidacy
		}
		if agentAliveAt(*a, now) {
			if a.ConsecutiveDeadTicks != 0 {
				a.ConsecutiveDeadTicks = 0
				changed = true
			}
			continue
		}
		a.ConsecutiveDeadTicks++
		changed = true
		if a.DeregisteredAt != nil || !a.OrphanHandledAt.IsZero() {
			continue // already handled or cleanly gone: streak tracked, not actionable
		}
		if a.ConsecutiveDeadTicks >= deadConfirmTicks {
			candidates = append(candidates, *a)
		}
	}
	if changed {
		s.saveLocked()
	}
	s.mu.Unlock()

	// Observability requested explicitly after the 2026-08-25 incident:
	// this is the regression detector. If a future restart logs a live
	// agent here, the per-agent gate has broken — it should be silent
	// (or absent) for every agent that's actually alive and just hasn't
	// polled yet within a normal window.
	if len(unknownStale) > 0 {
		sort.Strings(unknownStale)
		log.Printf("serve: orphan check: %d agent(s) look stale but have not been observed this daemon lifetime yet — treated as unknown, not dead, no action taken: %s", len(unknownStale), strings.Join(unknownStale, ", "))
	}

	if !orphanSweepEnabled() {
		if len(candidates) > 0 {
			names := make([]string, len(candidates))
			for i, a := range candidates {
				names[i] = a.Name
			}
			log.Printf("serve: orphan sweep DISABLED (set TN_ENABLE_ORPHAN_SWEEP=1 to enable) — would have processed %d dead-without-retiring candidate(s), left alone: %s", len(candidates), strings.Join(names, ", "))
		}
		return
	}

	for _, a := range candidates {
		s.triggerOrphanCheckIfNeeded(a.Name, a.Project)
	}
}

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

// --- Feed + reply (GET /feed, POST /reply — SPEC-ui-v2.md #3) ---

const feedCap = 100

// feedEntry is one row in the merged, newest-first /feed timeline: a
// message send, its ack (a distinct later event with its own timestamp,
// not folded into the send), or an activity log entry. Kind lets the UI
// pick an icon without parsing Text. TaskPath, when known, is what "each
// row that belongs to a task links to it" hangs off — Message always
// carries one when routed for a task; ActivityEntry only for the entries
// clearly tied to one (see ActivityEntry.TaskPath).
//
// Deliberately does NOT special-case MR state changes or Jira request
// events as their own Kind: MR auto-transitions and stuck-approval/drain/
// retire events already flow through appendActivityLocked ("activity"),
// and Jira transition/comment requests already flow through
// dispatchMessage ("message") — see checkMRStateOnce and
// checkJiraTransition. Reusing those two existing primitives is what lets
// this be a pure merge with no new call sites anywhere else in the file.
type feedEntry struct {
	At       time.Time `json:"at"`
	Kind     string    `json:"kind"` // "message" | "acked" | "activity"
	Text     string    `json:"text"`
	Agent    string    `json:"agent,omitempty"` // sender (message) or subject (activity/acked)
	To       string    `json:"to,omitempty"`    // recipient, "message" only
	TaskPath string    `json:"taskPath,omitempty"`
}

// feedTextCap bounds how much of a message/response body rides along in
// the feed — the feed is a skim list, not the full transcript (that's
// still `tn inbox`/the task note itself).
const feedTextCap = 200

// buildFeed merges messages and activity into one newest-first timeline,
// capped at feedCap. Pure function of a snapshot, so it's unit-testable
// without a running server — same shape as buildSessionEvents, but global
// (every agent/task) rather than filtered to one agent.
func buildFeed(messages []Message, activity []ActivityEntry) []feedEntry {
	var out []feedEntry
	for _, a := range activity {
		out = append(out, feedEntry{At: a.At, Kind: "activity", Text: a.Text, Agent: a.Agent, TaskPath: a.TaskPath})
	}
	for _, m := range messages {
		out = append(out, feedEntry{
			At: m.CreatedAt, Kind: "message", Text: truncate(m.Text, feedTextCap),
			Agent: m.From, To: m.To, TaskPath: m.TaskPath,
		})
		if m.Status == "acked" {
			at := m.CreatedAt
			if m.AckedAt != nil {
				at = *m.AckedAt
			}
			out = append(out, feedEntry{
				At: at, Kind: "acked", Text: truncate(m.Response, feedTextCap),
				Agent: m.To, TaskPath: m.TaskPath,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	if len(out) > feedCap {
		out = out[:feedCap]
	}
	return out
}

func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	snap := s.snapshotLocked()
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"feed": buildFeed(snap.Messages, snap.Activity)})
}

// handleReply implements POST /reply {taskPath, text}: writes text as an
// "@claude: " line into taskPath's reply block (SPEC-note-layout.md),
// exactly what typing into that block in Obsidian would produce. Does NOT
// route the message itself — the write goes through s.tnClient's normal
// task update, which fires TaskNotes' own webhook the same as any other
// edit, and the EXISTING webhook handler (claudeLineRe / diffClaudeLinesLocked)
// notices the new line and routes it — no new delivery mechanism, per
// SPEC-ui-v2.md #3 ("routes through the existing webhook path").
func (s *Server) handleReply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TaskPath string `json:"taskPath"`
		Text     string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	req.TaskPath = strings.TrimSpace(req.TaskPath)
	req.Text = strings.TrimSpace(req.Text)
	if req.TaskPath == "" || req.Text == "" {
		http.Error(w, "taskPath and text are required", http.StatusBadRequest)
		return
	}
	if s.tnClient == nil {
		http.Error(w, "no TaskNotes client configured", http.StatusServiceUnavailable)
		return
	}

	// A caller pasting their own "@claude:" prefix shouldn't end up with
	// it doubled — strip one if present, then always add exactly one.
	text := req.Text
	for _, prefix := range []string{"@claude:", "@claude,", "@claude"} {
		if strings.HasPrefix(strings.ToLower(text), prefix) {
			text = strings.TrimSpace(text[len(prefix):])
			break
		}
	}
	if text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}

	task, err := s.tnClient.GetTask(req.TaskPath)
	if err != nil {
		http.Error(w, "failed to fetch task: "+err.Error(), http.StatusBadGateway)
		return
	}
	// 2026-08-25 truncation incident: the ROUTING extraction used to only
	// ever capture the first line after "@claude:" (see
	// extractClaudeDirectives' doc comment for the full incident and fix).
	// That's now fixed at the extraction layer — diffClaudeLinesLocked
	// parses the note's structured blocks and correctly reconstructs the
	// full multi-line reply — so this handler no longer needs to work
	// around it: writing the complete text straight after "@claude:" is
	// correct again, and is what a multi-line callout-quoted reply always
	// looked like anyway. (A same-day stopgap briefly split multi-line
	// replies into a short routed pointer plus the full text below it;
	// removed once the real fix landed, since the routed message now
	// carries the full text either way and the pointer wording would just
	// be misleading noise on top of a correct delivery.)
	newDetails := applyNoteBodyEdit(task.Details, func(nb *noteBody) {
		nb.Reply = "@claude: " + text
	})
	if _, err := s.tnClient.UpdateTask(req.TaskPath, map[string]any{"details": newDetails}); err != nil {
		http.Error(w, "failed to update task: "+err.Error(), http.StatusBadGateway)
		return
	}

	s.triggerRenders()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// createIdempotencyTTL bounds how long a CreatedTasks entry answers a
// repeat `tn create` before a genuinely new create with the same
// idempotency key is allowed — long enough to cover a client retrying
// after a timeout (the reported failure mode: TaskNotes' own create
// handler can take long enough to trip the CLI's default 10s HTTP timeout
// even though the create actually lands), short enough that a deliberate
// later re-creation of something with the same title/project/details
// isn't permanently blocked.
const createIdempotencyTTL = 10 * time.Minute

// handleCreateTask implements POST /tasks/create {idempotencyKey?,
// fields}: proxies to TaskNotes' own POST /api/tasks, but makes a retry
// after a client-side timeout HARMLESS rather than merely rarer. With a
// non-empty idempotencyKey that already has a live (< createIdempotencyTTL)
// CreatedTasks entry, returns THAT task without calling TaskNotes again —
// a `tn create` retry is then indistinguishable from a first-time success,
// since the CLI only ever prints the returned path. Otherwise creates via
// TaskNotes and, on success, records the mapping. No dedup at all when
// idempotencyKey is empty (older binaries / `--json` direct callers),
// preserving prior behavior exactly.
//
// Known residual gap, not closed by this: if THIS daemon's own call to
// TaskNotes times out but the create actually lands server-side (the same
// underlying failure mode, just one hop further in), the mapping is never
// recorded (the Go http.Client returned an error, not a response), so a
// retry with the same key would still see no entry and create again. A
// fully durable fix would need a marker readable back FROM TaskNotes
// itself (e.g. a hidden customProperty), not just an in-memory/persisted
// map here — deliberately not implemented without sign-off, since it's
// materially more complexity than what was asked for.
func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IdempotencyKey string         `json:"idempotencyKey,omitempty"`
		Fields         map[string]any `json:"fields"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if s.tnClient == nil {
		http.Error(w, "TaskNotes API not configured", http.StatusServiceUnavailable)
		return
	}

	if req.IdempotencyKey != "" {
		s.mu.Lock()
		rec, ok := s.state.CreatedTasks[req.IdempotencyKey]
		s.mu.Unlock()
		if ok && time.Since(rec.CreatedAt) < createIdempotencyTTL {
			writeJSON(w, http.StatusOK, map[string]any{
				"task":      Task{Path: rec.Path, Title: rec.Title},
				"duplicate": true,
			})
			return
		}
	}

	task, err := s.tnClient.CreateTask(req.Fields)
	if err != nil && isAmbiguousCreateError(err) {
		// Timeout / dropped connection — genuinely unknown whether the
		// create landed (the same underlying failure mode the CLI-facing
		// fix exists for, just one hop further in: daemon->TaskNotes
		// instead of CLI->daemon). A client-side timeout tells us nothing
		// about whether the write will eventually land, so a single
		// immediate lookup cannot distinguish "not written" from
		// "not-yet-written" — under a real stall (Obsidian hung for well
		// over a minute) the write was merely queued, an immediate lookup
		// found nothing, and the code below used to retry the create,
		// landing BOTH writes as a duplicate. Keep re-querying on a
		// bounded backoff instead of guessing from one sample, and if the
		// whole window elapses with no match, STOP — do not retry the
		// create blind, report the ambiguity to the caller and let a
		// human or the agent decide.
		title, _ := req.Fields["title"].(string)
		claudeProject := customPropertyFromFields(req.Fields, "claude-project")
		if existing, found := s.findRecentDuplicateTaskWithBackoff(title, claudeProject); found {
			log.Printf("serve: create-idempotency: ambiguous create failure for %q, found existing task %s after backoff — treating as success, not retrying", title, existing.Path)
			if req.IdempotencyKey != "" {
				s.mu.Lock()
				if s.state.CreatedTasks == nil {
					s.state.CreatedTasks = map[string]createdTaskRecord{}
				}
				s.state.CreatedTasks[req.IdempotencyKey] = createdTaskRecord{
					Path: existing.Path, Title: existing.Title, CreatedAt: time.Now(),
				}
				s.saveLocked()
				s.mu.Unlock()
			}
			writeJSON(w, http.StatusOK, map[string]any{"task": existing, "duplicate": true})
			return
		}
		log.Printf("serve: create-idempotency: ambiguous create failure for %q, still no matching task after full backoff window — reporting ambiguity, not retrying blind", title)
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":     fmt.Sprintf("create for %q timed out and its outcome is still unconfirmed after re-querying with backoff — DO NOT retry this create. First check whether %q now exists (tn list / tn get, or search the board) — if it does, you're done; if it genuinely doesn't, only then create it again", title, title),
			"ambiguous": true,
			"title":     title,
		})
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	if req.IdempotencyKey != "" {
		s.mu.Lock()
		if s.state.CreatedTasks == nil {
			s.state.CreatedTasks = map[string]createdTaskRecord{}
		}
		s.state.CreatedTasks[req.IdempotencyKey] = createdTaskRecord{
			Path: task.Path, Title: task.Title, CreatedAt: time.Now(),
		}
		s.saveLocked()
		s.mu.Unlock()
	}

	writeJSON(w, http.StatusOK, map[string]any{"task": task})
}

// isAmbiguousCreateError reports whether err from a TaskNotes CreateTask
// call leaves it genuinely UNKNOWN whether the create actually landed — a
// timeout or dropped connection — as opposed to a clean HTTP error
// response (*httpError, e.g. a 400 validation failure: definitely did NOT
// create anything) or connection-refused (*connRefusedError: TaskNotes
// wasn't even listening, so nothing was ever sent). Only the ambiguous
// case justifies the extra lookup-before-retry cost; a clean failure is
// surfaced immediately, no lookup, no retry.
func isAmbiguousCreateError(err error) bool {
	if err == nil {
		return false
	}
	var he *httpError
	if errors.As(err, &he) {
		return false
	}
	var ce *connRefusedError
	if errors.As(err, &ce) {
		return false
	}
	return true
}

// customPropertyFromFields extracts key from fields["customProperties"] —
// the same nested map shape cmdCreate builds (fields["customProperties"] =
// map[string]any{...}) — or "" if absent/not a string.
func customPropertyFromFields(fields map[string]any, key string) string {
	cp, ok := fields["customProperties"].(map[string]any)
	if !ok {
		return ""
	}
	v, _ := cp[key].(string)
	return v
}

// findRecentDuplicateTask looks for a task matching title (server-side
// query — TaskNotes' own "title is <exact>" filter, confirmed live to
// work, unlike customProperties.* filter paths which were confirmed live
// to silently match nothing on this TaskNotes version) AND claudeProject
// (client-side, from CustomProperties — since server-side custom-property
// filtering isn't usable) AND created within createIdempotencyTTL
// (client-side, from DateCreated — bounds a coincidentally identically-
// titled OLDER, unrelated task from matching; the risk of two genuinely
// distinct tasks sharing both title and claude-project within the same
// ~10-minute window is accepted, degrading to pre-fix behavior in that
// rare case). Returns the first match and true, or a zero Task and false.
func (s *Server) findRecentDuplicateTask(title, claudeProject string) (Task, bool) {
	if s.tnClient == nil || title == "" {
		return Task{}, false
	}
	query := FilterGroup{
		Type: "group", ID: "root", Conjunction: "and", GroupKey: "none",
		Children: []FilterCondition{
			{Type: "condition", ID: "c1", Property: "title", Operator: "is", Value: title},
		},
	}
	tasks, err := s.tnClient.QueryTasksRaw(query)
	if err != nil {
		log.Printf("serve: create-idempotency: ambiguous-failure lookup for %q failed: %v", title, err)
		return Task{}, false
	}
	cutoff := time.Now().Add(-createIdempotencyTTL)
	for _, t := range tasks {
		if t.CustomProperties["claude-project"] != claudeProject {
			continue
		}
		created, err := time.Parse(time.RFC3339Nano, t.DateCreated)
		if err != nil || created.Before(cutoff) {
			continue
		}
		return t, true
	}
	return Task{}, false
}

// createAmbiguityBackoffSchedule bounds how long handleCreateTask keeps
// re-querying TaskNotes for a task that may have landed after a
// client-side-ambiguous create failure, before giving up and reporting the
// ambiguity honestly instead of guessing from one sample. Sums to ~39.5s;
// combined with the ~10s the initial CreateTask call can take before it
// times out ambiguously, that's comfortably inside the 65s the CLI's
// bridge client allows for the whole /tasks/create round trip (bridge.go),
// leaving margin for the response to actually get back.
//
// var, not const, so tests can shrink it instead of running for real.
var createAmbiguityBackoffSchedule = []time.Duration{
	500 * time.Millisecond, 1 * time.Second, 2 * time.Second, 4 * time.Second,
	8 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second,
}

// findRecentDuplicateTaskWithBackoff re-queries findRecentDuplicateTask on
// createAmbiguityBackoffSchedule instead of just once, giving a write that
// is merely queued (not failed) time to actually flush before concluding
// it never landed. Returns the first match found, or false if the whole
// schedule elapses without one.
func (s *Server) findRecentDuplicateTaskWithBackoff(title, claudeProject string) (Task, bool) {
	if existing, found := s.findRecentDuplicateTask(title, claudeProject); found {
		return existing, true
	}
	for _, delay := range createAmbiguityBackoffSchedule {
		time.Sleep(delay)
		if existing, found := s.findRecentDuplicateTask(title, claudeProject); found {
			return existing, true
		}
	}
	return Task{}, false
}

// handleCloseTask implements POST /tasks/close {path}: mechanism 2's
// synchronous half. `tn status <path> done` / `tn done <path>` route
// through this endpoint specifically (see closeTaskChecked in main.go)
// rather than writing status directly, so a task carrying a DONE MEANS
// block gets its criteria checked BEFORE the status write happens — a
// rejection here means TaskNotes was never touched, so there's nothing to
// revert and no flicker.
//
// A task with no DONE MEANS block at all closes immediately — this is
// opt-in friction, scoped to tasks that actually carry the heading (see
// checkDoneMeans).
//
// This endpoint is deliberately the ONLY place this check runs
// synchronously. Everything that reaches "done" WITHOUT going through it —
// which, by construction, is every human/Obsidian-direct edit, since this
// is now the sole agent-side path — is picked up later, informationally
// only, by noteDoneMeansGapIfUnmet via the webhook/reconciler. See that
// function's doc comment for why routing itself is the attribution and
// why a human close is never reverted.
func (s *Server) handleCloseTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	if s.tnClient == nil {
		http.Error(w, "TaskNotes API not configured", http.StatusServiceUnavailable)
		return
	}

	task, err := s.tnClient.GetTask(req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	if check := checkDoneMeans(task.Details); !check.Satisfied() {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":           "DONE MEANS criteria unmet",
			"missingEvidence": check.MissingNumbers,
			"requiredCount":   check.RequiredCount,
		})
		return
	}

	updated, err := s.tnClient.UpdateTask(req.Path, map[string]any{"status": "done"})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": updated})
}

// envItem is one entry in the GET /projects/env response. Never carries a
// value — secrets stay out of the bridge/messages entirely.
type envItem struct {
	Project     string `json:"project"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Injected    bool   `json:"injected"`
}

// envItemsForProject returns cfg's env entries as envItems, sorted by name.
func envItemsForProject(slug string, cfg ProjectConfig) []envItem {
	names := make([]string, 0, len(cfg.Env))
	for name := range cfg.Env {
		names = append(names, name)
	}
	sort.Strings(names)

	items := make([]envItem, 0, len(names))
	for _, name := range names {
		entry := cfg.Env[name]
		items = append(items, envItem{
			Project:     slug,
			Name:        name,
			Description: entry.Description,
			Injected:    entry.Value != "",
		})
	}
	return items
}

// handleProjectsEnv serves GET /projects/env?project=<slug>: with a project
// given, that project's configured env entries; without one, every
// configured project's entries. Values are never included in the response.
func (s *Server) handleProjectsEnv(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")

	s.mu.Lock()
	defer s.mu.Unlock()

	var items []envItem
	if project != "" {
		slug := strings.ToLower(project)
		items = envItemsForProject(slug, s.projectConfigFor(slug))
	} else {
		slugs := make([]string, 0, len(s.config.Projects))
		for slug := range s.config.Projects {
			slugs = append(slugs, slug)
		}
		sort.Strings(slugs)
		for _, slug := range slugs {
			items = append(items, envItemsForProject(slug, s.config.Projects[slug])...)
		}
	}
	if items == nil {
		items = []envItem{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"env": items})
}

// repoSettingsView is GET /repos?slug=<slug>'s response shape — surfaces
// state the daemon already holds (repoSettingsFor's parsed repo note,
// ProjectConfig's cwd/autoSpawn) as structured fields, so an agent gets
// the resolved values instead of parsing frontmatter out of markdown
// itself. See CLAUDE.md's "Registering a test/verification agent" entry
// and the repoSettings doc comment for why *Set exists: a resolved value
// alone can't distinguish "the repo note doesn't set this" from "it set
// something invalid that got silently dropped" — both read as the same
// default otherwise, which is exactly what cost a live investigation
// clarity before this existed.
type repoSettingsView struct {
	Slug             string `json:"slug"`
	RepoPath         string `json:"repoPath,omitempty"`
	AutoSpawn        bool   `json:"autoSpawn"`
	MaxOrchestrators int    `json:"maxOrchestrators"`
	MaxWorkers       int    `json:"maxWorkers"`
	// MaxOrchestratorsSet/MaxWorkersSet: true only if the repo note's own
	// frontmatter set this key (see repoSettings.MaxOrchestratorsPresent/
	// MaxWorkersPresent) — false means the resolved value above is a
	// fallback (serve.json's env entry, for MaxWorkers) or the plain
	// built-in default, not something the repo note configured.
	MaxOrchestratorsSet bool `json:"maxOrchestratorsSet"`
	MaxWorkersSet       bool `json:"maxWorkersSet"`
	// MaxWorkersServeJSONFallback is serve.json's own TN_MAX_WORKERS env
	// entry for this project, if any — what MaxWorkers actually resolves
	// to at spawn time when the repo note doesn't set it (see
	// spawnOrchestrator: repo-note value wins when present, this is what
	// stands when it isn't). "" when serve.json doesn't set one either.
	MaxWorkersServeJSONFallback string `json:"maxWorkersServeJsonFallback,omitempty"`
}

// handleRepoSettings implements GET /repos?slug=<slug>: the parsed repo
// note settings PLUS the project's actual configured cwd/autoSpawn from
// serve.json, in one structured response. 404 for a slug that isn't a
// configured project at all (repoSettingsFor would silently return
// defaults for it, which would misrepresent an unconfigured project as
// "configured with defaults").
func (s *Server) handleRepoSettings(w http.ResponseWriter, r *http.Request) {
	slug := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("slug")))
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	cfg, ok := s.config.Projects[slug]
	if !ok {
		for k, c := range s.config.Projects {
			if strings.EqualFold(k, slug) {
				cfg, ok = c, true
				break
			}
		}
	}
	s.mu.Unlock()
	if !ok {
		http.Error(w, "unknown project slug", http.StatusNotFound)
		return
	}

	settings := s.repoSettingsFor(slug)
	view := repoSettingsView{
		Slug:                slug,
		RepoPath:            cfg.Cwd,
		AutoSpawn:           cfg.AutoSpawn,
		MaxOrchestrators:    settings.MaxOrchestrators,
		MaxWorkers:          settings.MaxWorkers,
		MaxOrchestratorsSet: settings.MaxOrchestratorsPresent,
		MaxWorkersSet:       settings.MaxWorkersPresent,
	}
	if entry, ok := cfg.Env["TN_MAX_WORKERS"]; ok {
		view.MaxWorkersServeJSONFallback = entry.Value
	}
	writeJSON(w, http.StatusOK, view)
}

// handleSpawnPause implements the menu-bar client's manual spawn
// kill-switch: POST /spawn/pause {"paused": bool} sets it (persisted);
// GET /spawn/pause just reports the current value. While paused, every
// spawn path skips actually spawning (see spawnOrchestrator's guard and
// reconcileSpawnsOnce's batch check) — everything else (routing, drain,
// retire, MR watcher, stuck-prompt dialogs/approval) keeps working.
func (s *Server) handleSpawnPause(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req struct {
			Paused bool `json:"paused"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.state.SpawnPaused = req.Paused
		s.saveLocked()
		s.mu.Unlock()
		s.triggerRenders()
	}

	s.mu.Lock()
	paused := s.state.SpawnPaused
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"paused": paused})
}

// handleApprove implements POST /agents/approve {"name", "option":
// "once"|"always"|"deny"} — the server-side equivalent of clicking the
// stuck-prompt approval dialog, for the menu-bar client. 404 unknown
// agent; 400 an unrecognized option; 409 if the agent has no CURRENTLY
// tracked stuck prompt (checked before ever touching tmux — the same
// discipline as the dialog path, which only ever prompts a genuinely
// stuck agent). Reuses sendApprovalIfStillStuck/resolveStuckPromptMapping
// — the same fresh re-capture and re-parse safety as the dialog path, so
// a race with the user resolving it manually (or the prompt changing
// shape between the client's last poll and this call) is handled
// identically either way.
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string `json:"name"`
		Option string `json:"option"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	var kind stuckApprovalKind
	switch req.Option {
	case "once":
		kind = approvalAllowOnce
	case "always":
		kind = approvalAlwaysAllow
	case "deny":
		kind = approvalDeny
	default:
		http.Error(w, `option must be "once", "always", or "deny"`, http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	a, ok := s.state.Agents[req.Name]
	var session string
	var deregistered bool
	if ok {
		session, _ = tmuxSessionForAgent(*a)
		deregistered = a.DeregisteredAt != nil
	}
	s.mu.Unlock()
	if !ok {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	if deregistered {
		// A death path marked this agent gone; checkStuckSessionsOnce
		// stops alerting on it the moment that happens (see its own doc
		// comment) but never retroactively clears whatever Stuck value
		// was already recorded — same reasoning as buildStatusResponse's
		// stuckOut filter. Reject explicitly rather than trusting a
		// possibly-stale snapshot: sending an approval keystroke into an
		// abandoned pane (deliberately never killed — it's forensic
		// scrollback, see checkStuckSessionsOnce) would act on a session
		// nobody is actually waiting on.
		writeJSON(w, http.StatusConflict, map[string]any{"sent": false, "reason": "agent is deregistered"})
		return
	}

	if info, stuck := s.stuck.snapshot()[req.Name]; !stuck || !info.Stuck {
		writeJSON(w, http.StatusConflict, map[string]any{"sent": false, "reason": "agent has no current stuck prompt"})
		return
	}

	capture := s.stuckCapture
	if capture == nil {
		capture = capturePaneReal
	}
	sendKeys := s.stuckSendKeys
	if sendKeys == nil {
		sendKeys = sendKeysReal
	}

	sent, reason := s.sendApprovalIfStillStuck(req.Name, session, kind, capture, sendKeys)
	// Re-scan right away rather than leaving /status (which the tray and
	// webui poll every 5s) showing a resolved prompt as stuck until the
	// next stuckCheckInterval tick — live (2026-09-04): the user approved
	// from the tray, the prompt was already gone ("cleared before send"),
	// and ⛔ stayed up for another 95s while the attached pane showed
	// nothing to answer. Not sent → the pane is already past the prompt,
	// so re-scan synchronously so the tray's own post-action refresh sees
	// the cleared state. Sent → give Claude a moment to consume the key,
	// then re-scan in the background. dialogSuppressed: this path must
	// never open a dialog of its own.
	if sent {
		s.mu.Lock()
		delay, done := s.approveRescanDelay, s.approveRescanDone
		s.mu.Unlock()
		go func() {
			time.Sleep(delay)
			s.checkStuckSessionsOnce(capture, dialogSuppressed, sendKeys)
			if done != nil {
				done()
			}
		}()
	} else {
		s.checkStuckSessionsOnce(capture, dialogSuppressed, sendKeys)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": sent, "reason": reason})
}

// defaultApproveRescanDelay is how long handleApprove waits after a
// successfully sent approval before re-scanning the pane — long enough
// for Claude Code to consume the keystroke and repaint. Per-Server
// (Server.approveRescanDelay) so tests can zero it without racing another
// test's still-sleeping rescan goroutine.
const defaultApproveRescanDelay = 3 * time.Second

// handleHookPermissionRequest implements POST /hooks/permission-request:
// the daemon-side receiver for a Claude Code PermissionRequest hook
// (2026-08-26 design — see SPEC-serve.md). A throwaway hook script running
// inside each daemon-spawned orchestrator's tmux session reads its own
// $TN_AGENT_NAME (confirmed live to reach the hook's environment — see the
// spike results referenced in SPEC-serve.md) and POSTs here whenever
// Claude Code reports a permission decision is genuinely pending.
//
// This is DETECTION only — it never decides anything on the agent's
// behalf. The hook script itself is required to exit 0 with empty stdout
// unconditionally (Claude Code's own documented contract: silence is
// never approval), so the real interactive prompt is completely
// unaffected by whether this endpoint is even reachable. This handler
// responds immediately and does the actual (potentially slow, human-
// waiting) work in a goroutine — the HTTP round trip must never make the
// hook's own short timeout expire and delay Claude Code's own prompt from
// rendering to a real human sitting at that session.
//
// A hard `permissions.deny` rule short-circuits before ANY hook fires
// (confirmed empirically 2026-08-26) — this endpoint structurally cannot
// see that class of block. Pane-scraping remains the only signal for it,
// permanently, not as a transitional gap.
func (s *Server) handleHookPermissionRequest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentName      string `json:"agentName"`
		SessionID      string `json:"sessionId,omitempty"`
		PromptID       string `json:"promptId,omitempty"`
		ToolName       string `json:"toolName,omitempty"`
		PermissionMode string `json:"permissionMode,omitempty"`
		// Command is tool_input.command when the pending tool is
		// Bash-shaped — the hook script already extracts and sends this
		// (see hooks/claude-hook.sh; handleHookPermissionDenied already
		// consumes the identical field on its sibling event), but until
		// 2026-08-26 this handler silently discarded it. Threaded through
		// to recordHookCommand so the eventually-displayed excerpt can
		// show the actual command a human is being asked to approve —
		// see stuckSessionState.HookCommand's doc comment.
		Command string `json:"command,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.AgentName == "" {
		http.Error(w, "agentName is required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	a, ok := s.state.Agents[req.AgentName]
	var session string
	if ok {
		session, _ = tmuxSessionForAgent(*a)
		if a.HookConfirmedAt == nil {
			now := time.Now()
			a.HookConfirmedAt = &now
			s.saveLocked()
			log.Printf("serve: hook: %s's PermissionRequest hook confirmed working for the first time", req.AgentName)
		}
	}
	s.mu.Unlock()

	if !ok {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	if session == "" {
		http.Error(w, "no resolvable tmux session for agent", http.StatusConflict)
		return
	}

	log.Printf("serve: hook: permission request pending for %s (tool=%s, command=%q, prompt=%s, mode=%s)", req.AgentName, req.ToolName, req.Command, req.PromptID, req.PermissionMode)

	writeJSON(w, http.StatusOK, map[string]any{"received": true})

	go s.triggerHookConfirmedStuck(req.AgentName, session, req.ToolName, req.Command)
}

// triggerHookConfirmedStuck is handleHookPermissionRequest's async body,
// pulled out as its own function so tests can call it directly and
// synchronously instead of racing a goroutine. Captures the pane fresh —
// never trusts the hook payload itself for display or option mapping (see
// resolveStuckPromptMapping's own doc comment: menu-option numbering is a
// terminal-rendering detail no hook carries) — and re-derives stuck from
// THAT capture via the same detectLiveStuckPrompt the periodic tick uses,
// rather than assuming true just because the hook fired: by the time this
// runs, the prompt may already be resolved (a fast auto-response, or a
// human who was already watching). Feeds the result into the SAME
// updateStuckState state machine the periodic pane-scraping tick uses,
// with hookTriggered=true — the only thing that changes is HOW FAST
// confirmedStuck is reached (immediately, vs. up to 2 ticks or the
// persistence backstop), not any of the dedup/re-prompt/clear logic.
// toolName/command are the hook payload's tool_name/tool_input.command
// (see handleHookPermissionRequest) — recorded against this episode's
// signature (recordHookCommand) BEFORE updateStuckState runs, so the
// excerpt it builds can use them. command may be "" (a hard
// permissions.deny rule never reaches a hook at all — structurally out of
// scope, see this function's own doc comment above — and not every tool
// is Bash-shaped); a "" command is simply not recorded, and display falls
// back to the wider pane capture alone.
func (s *Server) triggerHookConfirmedStuck(agentName, session, toolName, command string) {
	capture := s.stuckCapture
	if capture == nil {
		capture = capturePaneReal
	}
	sendKeys := s.stuckSendKeys
	if sendKeys == nil {
		sendKeys = sendKeysReal
	}
	dialog := dialogFuncForEnv()

	pane, err := capture(session)
	if err != nil {
		log.Printf("serve: hook: failed to capture pane for %s: %v", agentName, err)
		return
	}
	markerLine, stuck := detectLiveStuckPrompt(pane)
	if stuck && command != "" {
		signature := stuckEpisodeSignature(markerLine, parseStuckPromptOptions(pane))
		s.recordHookCommand(agentName, signature, toolName, command)
	}
	s.updateStuckState(agentName, session, stuck, false, pane, time.Now(), capture, dialog, sendKeys, true)
}

// recordHookCommand caches a PermissionRequest hook's structured
// tool_input.command/tool_name against the specific prompt episode
// (signature) it was reported for — see stuckSessionState.HookCommand's
// doc comment for why this is preferred, when available, over anything
// derived from the pane capture alone. No-ops on an empty command rather
// than caching nothing useful under a real signature.
func (s *Server) recordHookCommand(agentName, signature, toolName, command string) {
	if command == "" {
		return
	}
	s.stuck.mu.Lock()
	st, ok := s.stuck.byName[agentName]
	if !ok {
		st = &stuckSessionState{}
		s.stuck.byName[agentName] = st
	}
	st.HookCommand = command
	st.HookToolName = toolName
	st.HookCommandSignature = signature
	s.stuck.mu.Unlock()
}

// commandSummary returns the first two whitespace-separated tokens of
// command (e.g. "git push" from "git push --force origin main"), or ""
// for an empty command — this is the grouping key that makes
// PermissionDenied telemetry actionable: "glab mr update denied 4 times"
// tells a human something; "Bash denied 4 times" doesn't.
func commandSummary(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return ""
	}
	if len(fields) == 1 {
		return fields[0]
	}
	return fields[0] + " " + fields[1]
}

// handleHookPermissionDenied implements POST /hooks/permission-denied:
// PURE TELEMETRY for Claude Code's PermissionDenied hook — never a
// decision, never a retry (see SPEC-serve.md's "PermissionRequest hook
// detection" section, which documents why this was dropped from scope
// and then reinstated: the negative evidence behind the drop tested the
// wrong triggers, and a telemetry-only handler costs almost nothing
// either way — if it never fires in production that's learned for free,
// and if it fires, this is exactly the signal the user asked for). No
// session/pane interaction at all, unlike handleHookPermissionRequest —
// there is nothing pending to capture or resolve, only a fact to record.
func (s *Server) handleHookPermissionDenied(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentName      string `json:"agentName"`
		SessionID      string `json:"sessionId,omitempty"`
		PromptID       string `json:"promptId,omitempty"`
		ToolName       string `json:"toolName,omitempty"`
		PermissionMode string `json:"permissionMode,omitempty"`
		Command        string `json:"command,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.AgentName == "" {
		http.Error(w, "agentName is required", http.StatusBadRequest)
		return
	}

	summary := commandSummary(req.Command)
	key := req.AgentName + "|" + req.ToolName + "|" + summary
	now := time.Now()

	s.mu.Lock()
	if _, ok := s.state.Agents[req.AgentName]; !ok {
		s.mu.Unlock()
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	rec, ok := s.state.PermissionDenials[key]
	if !ok {
		rec = &permissionDenialRecord{
			AgentName:      req.AgentName,
			ToolName:       req.ToolName,
			CommandSummary: summary,
			FirstSeenAt:    now,
		}
		s.state.PermissionDenials[key] = rec
	}
	rec.Count++
	rec.LastSeenAt = now
	if req.Command != "" {
		rec.LastCommand = req.Command
	}
	s.saveLocked()
	s.mu.Unlock()

	log.Printf("serve: hook: permission denied for %s (tool=%s, command=%q, count=%d)", req.AgentName, req.ToolName, req.Command, rec.Count)

	writeJSON(w, http.StatusOK, map[string]any{"received": true})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	queued := 0
	for _, m := range s.state.Messages {
		if m.Status == "queued" {
			queued++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"agents":  len(s.state.Agents),
		"queued":  queued,
		"uptime":  time.Since(daemonStartedAt).Seconds(),
		"version": daemonVersion,
	})
}

// --- Status endpoint (tray-app foundation) ---

// statusTaskCountsCacheTTL bounds how often /status's per-project
// task-status counts are refreshed — ONE global TaskNotes query shared
// across every project (see buildStatusTaskCountsQuery), never a
// per-project or per-request query.
const statusTaskCountsCacheTTL = 30 * time.Second

// statusTaskCountsCap bounds how many non-archived tasks a single
// task-counts pass processes, same defensive-cap convention as the other
// background passes (unblockPassTaskCap, webhookMissTaskCap).
const statusTaskCountsCap = 500

// statusNeedsActionCap and statusRecentActivityCap bound the two list
// sections of the /status response.
const (
	statusNeedsActionCap    = 20
	statusRecentActivityCap = 20
)

// buildStatusTaskCountsQuery builds the FilterQuery for /status's
// per-project task-status breakdown: every non-archived task, no status
// filter (all six statuses need counting) — bucketed by routing slug and
// status client-side (bucketTaskCountsByProject), same "one global query,
// bucket locally" pattern as the due-task scanner/MR watcher/reconciler.
func buildStatusTaskCountsQuery() FilterGroup {
	return FilterGroup{
		Type:        "group",
		ID:          "root",
		Conjunction: "and",
		Children: []FilterCondition{
			{Type: "condition", ID: "archived", Property: "archived", Operator: "is-not-checked"},
		},
		SortKey:       "dateModified",
		SortDirection: "desc",
		GroupKey:      "none",
	}
}

// statusTaskCounts is the bucketed result of buildStatusTaskCountsQuery:
// project slug (lowercased) -> status -> count.
type statusTaskCounts map[string]map[string]int

// bucketTaskCountsByProject groups tasks by routing slug (lowercased,
// unroutable tasks dropped) and status (empty status bucketed as "open",
// matching TaskNotes' own convention for a task with no status set).
func bucketTaskCountsByProject(tasks []Task) statusTaskCounts {
	out := statusTaskCounts{}
	for _, t := range tasks {
		slug := strings.ToLower(routingSlugForTask(t))
		if slug == "" {
			continue
		}
		if out[slug] == nil {
			out[slug] = map[string]int{}
		}
		status := t.Status
		if status == "" {
			status = "open"
		}
		out[slug][status]++
	}
	return out
}

// statusTaskCountsCache caches the bucketed per-project task-status counts,
// refetched at most every statusTaskCountsCacheTTL. On a fetch failure it
// keeps serving the last successfully bucketed counts rather than
// blanking the response (same "stay informative on a transient TaskNotes
// outage" spirit as needsActionCache, though that one blanks — counts are
// lower-stakes to show stale than to show zeroed).
type statusTaskCountsCache struct {
	mu        sync.Mutex
	fetchedAt time.Time
	counts    statusTaskCounts
	// byPath is a path -> {title, status} lookup built from the SAME
	// fetch that produces counts — added for /status agents[].ownedTasks
	// (SPEC-ui-v2.md #2: titles, not just a count) so that feature needs
	// no additional TaskNotes query of its own, reusing this cache's
	// existing TTL/staleness behavior exactly.
	byPath map[string]taskSummary
}

// taskSummary is the minimal per-task shape /status needs beyond a bare
// count: enough to show WHAT an agent is working on, not the full Task.
type taskSummary struct {
	Title  string
	Status string
}

func (c *statusTaskCountsCache) get(now time.Time, ttl time.Duration, fetch func() ([]Task, error)) statusTaskCounts {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fetchedAt.IsZero() && now.Sub(c.fetchedAt) < ttl {
		return c.counts
	}
	tasks, err := fetch()
	if err != nil {
		c.fetchedAt = now
		return c.counts
	}
	if len(tasks) > statusTaskCountsCap {
		tasks = tasks[:statusTaskCountsCap]
	}
	c.counts = bucketTaskCountsByProject(tasks)
	byPath := make(map[string]taskSummary, len(tasks))
	for _, t := range tasks {
		byPath[t.Path] = taskSummary{Title: t.Title, Status: t.Status}
	}
	c.byPath = byPath
	c.fetchedAt = now
	return c.counts
}

// taskSummaryByPath returns the byPath lookup built by the most recent
// successful get() call (nil before any successful fetch). Deliberately
// does NOT trigger its own fetch — callers always call get() first in the
// same request (for taskCounts), which is what actually populates this.
func (c *statusTaskCountsCache) taskSummaryByPath() map[string]taskSummary {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byPath
}

// statusAskBrief is one task's ask/brief block plain text (see
// note_layout.go), truncated to 200 chars.
type statusAskBrief struct {
	Ask   string
	Brief string
}

// statusAskBriefCacheTTL matches needsActionCacheTTL: ask/brief text
// requires a per-task GetTask (POST /api/tasks/query responses never
// include Details — confirmed against the live API, see the
// webhook-miss reconciler), so — unlike the cheap fields already on a
// QueryTasksRaw result — this is cached rather than fetched fresh on
// every 5s /status poll from /ui.
const statusAskBriefCacheTTL = needsActionCacheTTL

// statusAskBriefCache caches the bounded per-task GetTask fetches needed
// to populate ask/brief for /status's needsActionTasks — capped at
// statusNeedsActionCap tasks per refresh, same bound as the list itself.
type statusAskBriefCache struct {
	mu        sync.Mutex
	fetchedAt time.Time
	byPath    map[string]statusAskBrief
}

func (c *statusAskBriefCache) get(now time.Time, ttl time.Duration, fetch func() map[string]statusAskBrief) map[string]statusAskBrief {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fetchedAt.IsZero() && now.Sub(c.fetchedAt) < ttl {
		return c.byPath
	}
	c.byPath = fetch()
	c.fetchedAt = now
	return c.byPath
}

// statusDaemon, statusAgent, statusProject, statusNeedsActionTask,
// statusStuckPrompt, statusActivityEntry, and statusResponse are /status's
// JSON shape.
type statusDaemon struct {
	Uptime      float64 `json:"uptime"`
	Version     string  `json:"version"`
	PID         int     `json:"pid"`
	Port        int     `json:"port"`
	SpawnPaused bool    `json:"spawnPaused"`
	// LastSleepAt/LastSleepSeconds are set only when a host sleep has been
	// detected this process lifetime (see startSleepDetector) — pointers
	// so "never detected" omits both entirely rather than encoding a
	// misleading zero time/duration.
	LastSleepAt      *time.Time `json:"lastSleepAt,omitempty"`
	LastSleepSeconds *float64   `json:"lastSleepSeconds,omitempty"`
	// VaultName is the Obsidian vault's folder basename, derived from
	// DashboardPath's parent directory — NOT part of SPEC-ui-v2.md's
	// itemized daemon-work list, added because the webui's standalone-
	// browser host needs it to construct an obsidian://open?vault=...
	// fallback link (the Obsidian-iframe host doesn't: it opens notes via
	// the postMessage bridge instead, which needs no vault name at all).
	// Empty when no dashboard path is configured.
	VaultName string `json:"vaultName,omitempty"`
	// BuildHash is webUIBuildHash — a short hash of the embedded webui
	// bundle's actual bytes, rendered in the UI's own footer so a stale
	// rebuild (CLAUDE.md's documented footgun: `go build` embeds whatever
	// is on disk in webui/dist without checking it against webui/src) is
	// visible instead of silent.
	BuildHash string `json:"buildHash,omitempty"`
}

type statusAgent struct {
	Name           string `json:"name"`
	Alive          bool   `json:"alive"`
	Accepting      bool   `json:"accepting"`
	Draining       bool   `json:"draining"`
	Stuck          bool   `json:"stuck"`
	UnackedBacklog bool   `json:"unackedBacklog"`
	// RateLimited/RateLimitResetsAt/RateLimitSince report a claude.ai
	// usage-limit wait (see stuckSessionState.RateLimited). Reported
	// alongside — never instead of — UnackedBacklog: while rate-limited
	// a backlog is expected rather than alarming, but it is still real,
	// so the field keeps its value and only the DISPLAYED badge defers
	// (sessionStatusDisplay). RateLimitResetsAt is raw pane text, not a
	// parsed timestamp.
	RateLimited       bool       `json:"rateLimited"`
	RateLimitResetsAt string     `json:"rateLimitResetsAt,omitempty"`
	RateLimitSince    *time.Time `json:"rateLimitSince,omitempty"`
	ContextPct        *float64   `json:"contextPct,omitempty"`
	TmuxSession       string     `json:"tmuxSession,omitempty"`
	LastSeenAt        time.Time  `json:"lastSeenAt"`
	// OwnedInProgress is how many tasks this agent currently owns per
	// message-history (ownedTaskPathsLocked) — a cheap in-memory count,
	// NOT verified against each task's live TaskNotes status (that would
	// mean a per-agent live query on every /status poll, which the spec
	// explicitly rules out). In practice ownership only persists while a
	// task remains actively assigned and un-reassigned, so this is a
	// reasonable proxy for "how much is this agent carrying" even though
	// it isn't a status-verified count.
	OwnedInProgress int `json:"ownedInProgress"`
	// OwnedTasks lists the same owned tasks OwnedInProgress counts, BY
	// TITLE — SPEC-ui-v2.md #2: "the user specifically wants to see WHAT
	// each orchestrator is working on, not a count." Title/Status are
	// filled in from statusTaskCountsCache's byPath lookup (built from
	// the SAME query that produces taskCounts — no additional TaskNotes
	// round trip); a path not present there (evicted by the 500-task cap,
	// or a genuinely stale ownership record) still appears with an empty
	// Title/Status rather than being silently dropped, so ownership drift
	// stays visible instead of disappearing.
	OwnedTasks []statusOwnedTask `json:"ownedTasks,omitempty"`
	// HistoricalOwned is set ONLY for a non-alive agent (OwnedInProgress/
	// OwnedTasks are both empty then — see the hiding logic below): the
	// same ownedTaskPathsLocked count, kept visible under a name that
	// makes clear it's not current work. ownedTaskPathsLocked is pure
	// message-history ("who was this task's latest acked 'Task assigned:'
	// recipient") with no expiry, so a generation retired days ago still
	// "owns" everything it ever touched forever unless reassigned —
	// presenting that as live OwnedInProgress on a dead agent reads as
	// work assigned to a corpse. Hidden from OwnedInProgress/OwnedTasks,
	// not deleted: the message history this is derived from is untouched,
	// and a same-named agent re-registering within aliveWindow gets its
	// live ownedTasks view back automatically (same pattern as Workers).
	HistoricalOwned int `json:"historicalOwned,omitempty"`
	// Workers are this agent's declared worker subtasks (`tn worker
	// start`) — entirely orchestrator-reported, see handleWorkerStart.
	Workers []statusWorker `json:"workers,omitempty"`
}

// statusOwnedTask is one entry in statusAgent.OwnedTasks.
type statusOwnedTask struct {
	Path   string `json:"path"`
	Title  string `json:"title,omitempty"`
	Status string `json:"status,omitempty"`
}

// statusWorker is one entry in statusAgent.Workers.
type statusWorker struct {
	Path      string    `json:"path"`
	Title     string    `json:"title,omitempty"`
	Status    string    `json:"status,omitempty"`
	StartedAt time.Time `json:"startedAt"`
}

type statusProject struct {
	Agents           []statusAgent  `json:"agents"`
	Queued           int            `json:"queued"`
	Delivered        int            `json:"delivered"`
	TaskCounts       map[string]int `json:"taskCounts"`
	NeedsAction      int            `json:"needsAction"`
	MaxOrchestrators int            `json:"maxOrchestrators"`
	MaxWorkers       int            `json:"maxWorkers"`
	// ActiveOwners counts this project's needsActionTasks with
	// integration-role "owner" (see integrationRoleFor) — a best-effort
	// rollup, not an exhaustive count of every owner task in the project:
	// it's only computed from the same needsActionTasks list already
	// fetched for the needs-your-action section, so an owner task that's
	// open-but-still-blocked (waiting on its members, not yet in a
	// needs-action status) isn't counted here. Surfaced so the project
	// header can show "N workstream(s) awaiting merge" at a glance.
	ActiveOwners int `json:"activeOwners,omitempty"`
}

type statusNeedsActionTask struct {
	Title   string `json:"title"`
	Path    string `json:"path"`
	Status  string `json:"status"`
	Project string `json:"project,omitempty"`
	MR      string `json:"mr,omitempty"`
	// IntegrationBranch is the task's own customProperties["integration-branch"]
	// (see integrationBranchFor) — task-level only, no repo-wide fallback,
	// so this is "" for the overwhelming majority of tasks that target the
	// default branch.
	IntegrationBranch string `json:"integrationBranch,omitempty"`
	// IntegrationRole is the task's own customProperties["integration-role"]
	// (see integrationRoleFor) — "owner" or "" (unset = member).
	IntegrationRole string `json:"integrationRole,omitempty"`
	// Jira is the task's own customProperties["jira"] (comma-separated
	// issue keys); JiraNC is customProperties["jira-nc"] (a regulated
	// nonconformity key, comment-only — see checkJiraTransition).
	Jira   string `json:"jira,omitempty"`
	JiraNC string `json:"jiraNc,omitempty"`
	// Ask and Brief are the task's ask/brief block plain text (see
	// SPEC-note-layout.md / note_layout.go), truncated to 200 chars, so
	// the menu-bar app and /ui can show what's actually being asked
	// rather than only the title. "" for a legacy (unstructured) task or
	// one with no ask/brief set.
	Ask   string `json:"ask,omitempty"`
	Brief string `json:"brief,omitempty"`
}

type statusStuckPrompt struct {
	Agent       string `json:"agent"`
	TmuxSession string `json:"tmuxSession,omitempty"`
	Excerpt     string `json:"excerpt"`
}

// statusPermissionDenial is /status.permissionDenials' per-record shape —
// see permissionDenialRecord's doc comment for what this measures (pure
// telemetry, never a decision) and why it's grouped by command shape.
type statusPermissionDenial struct {
	// AgentName, not the shorter "Agent"/"agent" statusStuckPrompt uses —
	// deliberately matching the hook payload's own field name
	// (agentName, see handleHookPermissionDenied) and
	// permissionDenialRecord.AgentName, not the sibling status struct.
	// Live incident (2026-08-26): the earlier "agent"/"agent" naming
	// looked like a null/missing agentName to a query that reasonably
	// expected the SAME name the hook payload and the persisted record
	// both already used — the underlying data was correct the whole
	// time; only the /status field name was the odd one out in its own
	// pipeline. Consistency within one data's own name, end to end,
	// matters more here than matching an unrelated sibling struct's
	// shorter field.
	AgentName      string    `json:"agentName"`
	ToolName       string    `json:"toolName,omitempty"`
	CommandSummary string    `json:"commandSummary,omitempty"`
	Count          int       `json:"count"`
	FirstSeenAt    time.Time `json:"firstSeenAt"`
	LastSeenAt     time.Time `json:"lastSeenAt"`
	LastCommand    string    `json:"lastCommand,omitempty"`
}

type statusActivityEntry struct {
	Agent   string    `json:"agent"`
	Project string    `json:"project,omitempty"`
	At      time.Time `json:"at"`
	Text    string    `json:"text"`
}

type statusResponse struct {
	Daemon           statusDaemon             `json:"daemon"`
	Projects         map[string]statusProject `json:"projects"`
	NeedsActionTasks []statusNeedsActionTask  `json:"needsActionTasks"`
	StuckPrompts     []statusStuckPrompt      `json:"stuckPrompts"`
	RecentActivity   []statusActivityEntry    `json:"recentActivity"`
	// Triage is a passive, pull-based visibility signal, not an alert —
	// added 2026-08-25 after triage tasks sat unassigned and unpicked-up
	// for hours while their status was reported as "handled" to the user.
	// Deliberately NOT a push notification: a nagging mechanism gets
	// disabled the first time it fires during legitimate waiting (the
	// emulator reaper's own history), and triage is BY DEFINITION things
	// correctly waiting on a human decision — most of them are supposed to
	// sit there. This only answers "how long has the oldest one been
	// waiting," honestly, for whoever looks — it does not decide anything
	// is wrong.
	Triage statusTriageSummary `json:"triage"`
	// PermissionDenials surfaces the PermissionDenied hook's telemetry —
	// pure visibility, exactly like Triage: it does not decide anything is
	// wrong, it answers "what has been blocked, how often, most recently
	// when" for whoever looks. See permissionDenialRecord's doc comment.
	PermissionDenials []statusPermissionDenial `json:"permissionDenials"`
	// Credentials is the credential-profile summary (creds.go) for the
	// tray/webui account switcher — omitted entirely when no store is
	// wired (tests, or a daemon built without one). Never carries a
	// token: profiles are label/email/cooldown only.
	Credentials *statusCredentials `json:"credentials,omitempty"`
	// Worktrees is the worktree reaper's last per-project pass summary
	// (reaper.go) — counts only; omitted when no reaper is wired.
	Worktrees *statusWorktrees `json:"worktrees,omitempty"`
}

// statusCredentials is /status.credentials — see statusResponse.Credentials.
type statusCredentials struct {
	AutoSwap        bool               `json:"autoSwap"`
	MinSwapInterval string             `json:"minSwapInterval"`
	Active          string             `json:"active"`
	Profiles        []credsProfileView `json:"profiles"`
	LastSwap        *credsSwapRecord   `json:"lastSwap,omitempty"`
	// Error is set (and Profiles empty) when the store could not be read
	// — surfaced rather than hidden so a broken Keychain path reads as
	// "broken", not as "no profiles saved".
	Error string `json:"error,omitempty"`
}

// statusTriageSummary is /status.triage — see statusResponse.Triage's doc
// comment for why this exists and why it isn't an alert.
type statusTriageSummary struct {
	Count int `json:"count"`
	// OldestTitle/OldestPath/OldestAgeSeconds describe the single
	// longest-waiting triage task, empty/omitted when Count is 0 or the
	// oldest task's DateCreated couldn't be parsed (still counted in
	// Count either way — a date-parse failure is not "no triage debt").
	OldestTitle      string  `json:"oldestTitle,omitempty"`
	OldestPath       string  `json:"oldestPath,omitempty"`
	OldestAgeSeconds float64 `json:"oldestAgeSeconds,omitempty"`
}

// computeTriageSummary scans tasks for Status == "triage" and returns the
// count plus the single oldest one by parsed DateCreated. Shared by
// buildStatusResponse (/status.triage) and renderDashboardMarkdown (the
// dashboard's own triage line) so the two never drift apart.
func computeTriageSummary(tasks []Task, now time.Time) statusTriageSummary {
	var triage statusTriageSummary
	var oldestCreated time.Time
	for _, t := range tasks {
		if t.Status != "triage" {
			continue
		}
		triage.Count++
		created, err := time.Parse(time.RFC3339Nano, t.DateCreated)
		if err != nil {
			continue // still counted; just can't be THE oldest without a parseable date
		}
		if triage.OldestPath == "" || created.Before(oldestCreated) {
			oldestCreated = created
			triage.OldestTitle = t.Title
			triage.OldestPath = t.Path
		}
	}
	if !oldestCreated.IsZero() {
		triage.OldestAgeSeconds = now.Sub(oldestCreated).Seconds()
	}
	return triage
}

// buildStatusResponse assembles the /status payload from already-fetched
// inputs — no locking, no network calls — so it's directly unit-testable.
// repoSettingsFor is injected (rather than requiring a *Server) so tests
// never need a real vault on disk.
func buildStatusResponse(
	snap dashboardSnapshot,
	stuckByAgent map[string]stuckInfo,
	ownedTaskPaths map[string][]string,
	needsActionTasks []Task,
	taskCounts statusTaskCounts,
	configuredProjects map[string]ProjectConfig,
	repoSettingsFor func(string) repoSettings,
	now time.Time,
	port int,
	spawnPaused bool,
	askBriefByPath map[string]statusAskBrief,
	lastSleepAt time.Time,
	lastSleepDuration time.Duration,
	vaultName string,
	taskSummaryByPath map[string]taskSummary,
	workersByAgent map[string][]workerEntry,
	permissionDenials map[string]*permissionDenialRecord,
) statusResponse {
	projects := map[string]*statusProject{}
	getProject := func(slug string) *statusProject {
		if slug == "" {
			return nil
		}
		p, ok := projects[slug]
		if !ok {
			settings := repoSettingsFor(slug)
			p = &statusProject{
				Agents:           []statusAgent{},
				TaskCounts:       map[string]int{},
				MaxOrchestrators: settings.MaxOrchestrators,
				MaxWorkers:       settings.MaxWorkers,
			}
			projects[slug] = p
		}
		return p
	}

	// Seed every configured (serve.json) project even with zero agents, so
	// an idle-but-configured project still shows up.
	for slug := range configuredProjects {
		getProject(strings.ToLower(slug))
	}

	pendingBy := map[string]struct{ queued, delivered int }{}
	for _, m := range snap.Messages {
		switch m.Status {
		case "queued":
			c := pendingBy[m.To]
			c.queued++
			pendingBy[m.To] = c
		case "delivered":
			c := pendingBy[m.To]
			c.delivered++
			pendingBy[m.To] = c
		}
	}

	agents := append([]Agent(nil), snap.Agents...)
	sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })

	agentByName := make(map[string]Agent, len(agents))
	for _, a := range agents {
		if isTestVerificationAgentName(a.Name) {
			// A one-off registration made to exercise an endpoint against
			// the real daemon (see CLAUDE.md's Conventions section) —
			// never a real generation. Excluded from every derived
			// section below, not deleted from state: the registry itself
			// still has the record (POST /agents/purge removes it
			// outright when it's just leftover pollution), this only
			// keeps it off the user-facing board. Live incident that
			// prompted this: zz-worker-verify and two ad-hoc test
			// registrations sat in the Projects view looking like real
			// retired generations.
			continue
		}
		agentByName[a.Name] = a

		slug := strings.ToLower(a.Project)
		p := getProject(slug)
		if p == nil {
			continue
		}
		si := stuckByAgent[a.Name]
		session, _ := tmuxSessionForAgent(a)
		pc := pendingBy[a.Name]
		paths := ownedTaskPaths[a.Name]
		// ownedTasks/ownedInProgress are hidden for a non-alive agent —
		// same reasoning and same aliveWindow check as Workers below: pure
		// message-history ownership never expires on its own, so a dead
		// generation would otherwise show as still carrying every task it
		// ever touched. See statusAgent.HistoricalOwned for where that
		// count goes instead.
		alive := agentAliveAt(a, now)
		var ownedTasks []statusOwnedTask
		var historicalOwned int
		if alive {
			ownedTasks = make([]statusOwnedTask, 0, len(paths))
			for _, path := range paths {
				ts := taskSummaryByPath[path] // zero value (empty Title/Status) if evicted/stale — see statusAgent.OwnedTasks doc
				ownedTasks = append(ownedTasks, statusOwnedTask{Path: path, Title: ts.Title, Status: ts.Status})
			}
		} else {
			historicalOwned = len(paths)
		}
		// Workers are hidden (not deleted — this is display-layer only,
		// state.Workers is untouched) once their owning agent goes stale
		// past aliveWindow: nothing currently prunes a worker entry when
		// its orchestrator dies mid-task without a matching `tn worker
		// end` (start/end symmetry from something that can be killed is
		// exactly the assumption that's bitten this daemon before — sleep-
		// death, spawn-race, retire deadlock), so tying visibility to the
		// orchestrator's own liveness check is what actually keeps a dead
		// orchestrator's workers from lingering in the UI forever. If the
		// same agent name re-registers within aliveWindow, its still-there
		// worker entries reappear automatically.
		var workers []statusWorker
		if alive {
			for _, we := range workersByAgent[a.Name] {
				ts := taskSummaryByPath[we.TaskPath]
				workers = append(workers, statusWorker{Path: we.TaskPath, Title: ts.Title, Status: ts.Status, StartedAt: we.StartedAt})
			}
		}
		ownedInProgress := 0
		if alive {
			ownedInProgress = len(paths)
		}
		var rateLimitSince *time.Time
		if si.RateLimited && !si.RateLimitSince.IsZero() {
			since := si.RateLimitSince
			rateLimitSince = &since
		}
		p.Agents = append(p.Agents, statusAgent{
			Name:              a.Name,
			Alive:             alive,
			Accepting:         a.Accepting,
			Draining:          !a.Accepting,
			Stuck:             si.Stuck,
			UnackedBacklog:    si.UnackedBacklog,
			RateLimited:       si.RateLimited,
			RateLimitResetsAt: si.RateLimitResetsAt,
			RateLimitSince:    rateLimitSince,
			ContextPct:        a.ContextPct,
			TmuxSession:       session,
			LastSeenAt:        a.LastSeenAt,
			OwnedInProgress:   ownedInProgress,
			OwnedTasks:        ownedTasks,
			HistoricalOwned:   historicalOwned,
			Workers:           workers,
		})
		p.Queued += pc.queued
		p.Delivered += pc.delivered
	}

	for slug, counts := range taskCounts {
		p := getProject(slug)
		if p == nil {
			continue
		}
		for status, n := range counts {
			p.TaskCounts[status] = n
		}
	}

	for _, t := range needsActionTasks {
		if p := getProject(strings.ToLower(routingSlugForTask(t))); p != nil {
			p.NeedsAction++
			if integrationRoleFor(t) == "owner" {
				p.ActiveOwners++
			}
		}
	}

	needsActionOut := make([]statusNeedsActionTask, 0, statusNeedsActionCap)
	for i, t := range needsActionTasks {
		if i >= statusNeedsActionCap {
			break
		}
		proj := ""
		if len(t.Projects) > 0 {
			proj = t.Projects[0]
		}
		ab := askBriefByPath[t.Path]
		needsActionOut = append(needsActionOut, statusNeedsActionTask{
			Title: t.Title, Path: t.Path, Status: t.Status, Project: proj, MR: t.CustomProperties["mr"],
			IntegrationBranch: integrationBranchFor(t), IntegrationRole: integrationRoleFor(t),
			Jira: t.CustomProperties["jira"], JiraNC: t.CustomProperties["jira-nc"],
			Ask: ab.Ask, Brief: ab.Brief,
		})
	}

	// Computed from the FULL needsActionTasks list, not needsActionOut —
	// the oldest triage task must never be missed just because it fell
	// past statusNeedsActionCap in the capped display list.
	triage := computeTriageSummary(needsActionTasks, now)

	var stuckNames []string
	for name, si := range stuckByAgent {
		if !si.Stuck {
			continue
		}
		// Belt-and-suspenders alongside checkStuckSessionsOnce's own
		// DeregisteredAt gate: that gate stops NEW alerting for a
		// deregistered agent, but doesn't retroactively clear whatever
		// Stuck value was already recorded the moment BEFORE it got
		// deregistered (e.g. an agent retired while genuinely stuck) —
		// without this, that frozen true would surface here forever,
		// same class of bug as ownedTasks/Workers before their own
		// alive-gating. A deregistered agent's stuck state doesn't get
		// re-evaluated (never false, either) once it stops being
		// checked, so the safe reading here is "not applicable," never
		// "still stuck."
		if a, ok := agentByName[name]; ok && a.DeregisteredAt != nil {
			continue
		}
		stuckNames = append(stuckNames, name)
	}
	sort.Strings(stuckNames)
	stuckOut := make([]statusStuckPrompt, 0, len(stuckNames))
	for _, name := range stuckNames {
		session := ""
		if a, ok := agentByName[name]; ok {
			session, _ = tmuxSessionForAgent(a)
		}
		stuckOut = append(stuckOut, statusStuckPrompt{Agent: name, TmuxSession: session, Excerpt: stuckByAgent[name].Excerpt})
	}

	permissionDenialsOut := make([]statusPermissionDenial, 0, len(permissionDenials))
	for _, rec := range permissionDenials {
		if isTestVerificationAgentName(rec.AgentName) {
			continue // zz- test registrations never surface here — same convention as agents/stuckPrompts
		}
		permissionDenialsOut = append(permissionDenialsOut, statusPermissionDenial{
			AgentName: rec.AgentName, ToolName: rec.ToolName, CommandSummary: rec.CommandSummary,
			Count: rec.Count, FirstSeenAt: rec.FirstSeenAt, LastSeenAt: rec.LastSeenAt, LastCommand: rec.LastCommand,
		})
	}
	sort.Slice(permissionDenialsOut, func(i, j int) bool {
		return permissionDenialsOut[i].LastSeenAt.After(permissionDenialsOut[j].LastSeenAt)
	})

	activitySorted := append([]ActivityEntry(nil), snap.Activity...)
	sort.Slice(activitySorted, func(i, j int) bool { return activitySorted[i].At.After(activitySorted[j].At) })
	if len(activitySorted) > statusRecentActivityCap {
		activitySorted = activitySorted[:statusRecentActivityCap]
	}
	activityOut := make([]statusActivityEntry, 0, len(activitySorted))
	for _, e := range activitySorted {
		proj := ""
		if a, ok := agentByName[e.Agent]; ok {
			proj = a.Project
		}
		activityOut = append(activityOut, statusActivityEntry{Agent: e.Agent, Project: proj, At: e.At, Text: e.Text})
	}

	projectsOut := make(map[string]statusProject, len(projects))
	for slug, p := range projects {
		projectsOut[slug] = *p
	}

	daemon := statusDaemon{
		Uptime:      now.Sub(daemonStartedAt).Seconds(),
		Version:     daemonVersion,
		PID:         os.Getpid(),
		Port:        port,
		SpawnPaused: spawnPaused,
		VaultName:   vaultName,
		BuildHash:   webUIBuildHash,
	}
	if !lastSleepAt.IsZero() {
		at := lastSleepAt
		secs := lastSleepDuration.Seconds()
		daemon.LastSleepAt = &at
		daemon.LastSleepSeconds = &secs
	}

	return statusResponse{
		Daemon:            daemon,
		Projects:          projectsOut,
		NeedsActionTasks:  needsActionOut,
		StuckPrompts:      stuckOut,
		RecentActivity:    activityOut,
		Triage:            triage,
		PermissionDenials: permissionDenialsOut,
	}
}

// handleStatus implements GET /status: a read-only, SPECTATOR-SAFE
// aggregate view for the tray-app foundation. Must never mark a message
// delivered or bump any agent's LastSeenAt — unlike /inbox, this is pure
// observation, so it only ever reads snapshotLocked()/stuck.snapshot()
// (no touchAgentLocked-triggering call anywhere in this path) plus two
// TaskNotes queries shared across every project and cached (needsAction,
// statusTaskCounts) — never a per-project or per-request live query.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	snap := s.snapshotLocked()
	port := s.config.Port
	spawnPaused := s.state.SpawnPaused
	configuredProjects := make(map[string]ProjectConfig, len(s.config.Projects))
	for k, v := range s.config.Projects {
		configuredProjects[k] = v
	}
	ownedTaskPaths := make(map[string][]string, len(snap.Agents))
	for _, a := range snap.Agents {
		ownedTaskPaths[a.Name] = s.ownedTaskPathsLocked(a.Name)
	}
	workersByAgent := make(map[string][]workerEntry, len(s.state.Workers))
	for name, entries := range s.state.Workers {
		if len(entries) > 0 {
			workersByAgent[name] = append([]workerEntry(nil), entries...) // copy — don't leak the live slice outside the lock
		}
	}
	lastSleepAt := s.lastSleepAt
	lastSleepDuration := s.lastSleepDuration
	dashboardPath := s.dashboardPath
	permissionDenials := make(map[string]*permissionDenialRecord, len(s.state.PermissionDenials))
	for k, v := range s.state.PermissionDenials {
		cp := *v // copy — don't leak the live record outside the lock
		permissionDenials[k] = &cp
	}
	s.mu.Unlock()

	var vaultName string
	if dashboardPath != "" {
		vaultName = filepath.Base(filepath.Dir(dashboardPath))
	}

	stuckByAgent := s.stuck.snapshot()

	var needsActionTasks []Task
	if s.tnClient != nil {
		needsActionTasks, _ = s.needsAction.get(time.Now(), needsActionCacheTTL, func() ([]Task, error) {
			return s.tnClient.QueryTasksRaw(buildNeedsActionQuery())
		})
	}

	var taskCounts statusTaskCounts
	var taskSummaryByPath map[string]taskSummary
	if s.tnClient != nil {
		taskCounts = s.statusTaskCounts.get(time.Now(), statusTaskCountsCacheTTL, func() ([]Task, error) {
			return s.tnClient.QueryTasksRaw(buildStatusTaskCountsQuery())
		})
		// Reuses the SAME query result taskCounts was just bucketed from —
		// no additional TaskNotes round trip for ownedTasks' titles.
		taskSummaryByPath = s.statusTaskCounts.taskSummaryByPath()
	}

	var askBriefByPath map[string]statusAskBrief
	if s.tnClient != nil && len(needsActionTasks) > 0 {
		askBriefByPath = s.statusAskBrief.get(time.Now(), statusAskBriefCacheTTL, func() map[string]statusAskBrief {
			limit := len(needsActionTasks)
			if limit > statusNeedsActionCap {
				limit = statusNeedsActionCap
			}
			out := make(map[string]statusAskBrief, limit)
			for _, t := range needsActionTasks[:limit] {
				full, err := s.tnClient.GetTask(t.Path)
				if err != nil {
					continue
				}
				if nb, ok := parseNoteBody(full.Details); ok {
					out[t.Path] = statusAskBrief{Ask: truncate(nb.Ask, 200), Brief: truncate(nb.Brief, 200)}
				}
			}
			return out
		})
	}

	resp := buildStatusResponse(snap, stuckByAgent, ownedTaskPaths, needsActionTasks, taskCounts, configuredProjects, s.repoSettingsFor, time.Now(), port, spawnPaused, askBriefByPath, lastSleepAt, lastSleepDuration, vaultName, taskSummaryByPath, workersByAgent, permissionDenials)
	resp.Credentials = s.credentialsStatus()
	resp.Worktrees = s.worktreesStatus()
	writeJSON(w, http.StatusOK, resp)
}

// --- UI v2: embedded React SPA (SPEC-ui-v2.md) ---
//
// webUIAssets embeds webui/dist — the committed, pre-built Vite bundle.
// Go stays stdlib-only: `embed` is stdlib, and the daemon never runs
// npm/pnpm itself. `webui/dist` must exist and be current at `go build`
// time — see CLAUDE.md's build section for the `pnpm build` step and the
// stale-bundle footgun (the daemon will happily embed and serve whatever
// was there at the last Go build, silently, with no version check against
// webui/'s source).

//go:embed all:webui/dist
var webUIAssets embed.FS

// webUIDist is webUIAssets rooted at webui/dist, so paths match exactly
// what Vite emitted (index.html, assets/...) — see vite.config.ts's
// base:'/ui/' and outDir:'dist'.
var webUIDist = mustSubFS(webUIAssets, "webui/dist")

func mustSubFS(f embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		// embed.FS is compiled into the binary; a bad path here is a
		// build-time bug (dir renamed without updating the embed
		// directive), never a runtime condition — fail loudly and early.
		panic("serve: bad embed sub-path " + dir + ": " + err.Error())
	}
	return sub
}

// webUIBuildHash is a short hash of the CONTENT actually embedded in this
// binary (path+bytes of every file under webui/dist, computed once at
// program init) — surfaced via /status.daemon.buildHash and the UI's own
// footer. This is what turns the stale-bundle footgun (CLAUDE.md: `go
// build` embeds whatever's on disk in webui/dist with no check against
// webui/src/) from something you have to remember into something you can
// SEE: if the footer's hash doesn't match what `pnpm build` just produced
// moments ago, the running binary is stale, instantly obvious instead of
// silently serving old UI. Computed from the embedded bytes themselves
// (not a separate metadata file Vite would need to keep in sync) so it
// can never drift from what's actually served.
var webUIBuildHash = computeWebUIBuildHash(webUIDist)

func computeWebUIBuildHash(fsys fs.FS) string {
	var paths []string
	fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	sort.Strings(paths) // deterministic regardless of walk order
	h := sha256.New()
	for _, p := range paths {
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			continue
		}
		h.Write([]byte(p))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// webUIActuallyBuilt reports whether the embedded bundle looks like a
// REAL Vite build rather than an empty/placeholder webui/dist — Vite
// always emits an assets/ directory for this app's script-based SPA
// output, so its absence is a reliable signal that `pnpm build` was never
// actually run (a fresh checkout whose webui/dist somehow ended up with
// nothing but a hand-placed placeholder, or was partially cleaned).
// Deliberately does NOT catch a genuinely EMPTY webui/dist (zero files) —
// that's a Go `//go:embed` COMPILE-time failure ("no matching files"),
// not something a runtime check can paper over; this only covers the
// "dist has *something* but it isn't a real build" case.
func webUIActuallyBuilt() bool {
	_, err := fs.Stat(webUIDist, "assets")
	return err == nil
}

// handleUI implements GET /ui/*: serves the embedded React SPA. Any
// requested path that doesn't exist as a real file in the embedded bundle
// (a client-side TanStack Router route like /ui/task/Foo.md, or a fresh
// deep link) falls back to index.html — the SERVER doesn't know about
// app routes, only the bundled router does, so any unrecognized path is
// assumed to be one of its routes rather than a 404.
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if !webUIActuallyBuilt() {
		// Loud, not silent: a missing real build (fresh checkout with only
		// a placeholder in webui/dist, or a partially-cleaned dist/) would
		// otherwise either fail to compile (empty dir — a genuine go:embed
		// hard limit, nothing to do about that one at runtime) or silently
		// 404/serve garbage. This is the case we CAN catch: dist has files,
		// but not a real Vite build.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `<!doctype html><html><body style="font-family:system-ui;max-width:40em;margin:4em auto;line-height:1.5">`+
			`<h1>Web UI not built</h1>`+
			`<p>The embedded bundle in <code>webui/dist</code> isn't a real build (no `+
			`<code>assets/</code> directory found). Run:</p>`+
			`<pre>cd webui &amp;&amp; pnpm build</pre>`+
			`<p>then rebuild the daemon: <code>go build -o ~/bin/tn .</code></p>`+
			`</body></html>`)
		return
	}
	upath := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/ui"), "/")
	if upath == "" {
		upath = "index.html"
	}
	if _, err := fs.Stat(webUIDist, upath); err != nil {
		upath = "index.html" // SPA fallback for a client-side route
	}

	// Deliberately NOT http.FileServer here: it special-cases any request
	// whose path ends in "index.html" by 301-redirecting to the same path
	// WITHOUT that suffix — exactly what upath is set to on every SPA-
	// fallback and every bare /ui/ request, which turned into an infinite
	// redirect loop (each redirect re-entering this handler, re-triggering
	// the same fallback, redirecting again). http.ServeContent has no such
	// special-casing — it just serves bytes.
	f, err := webUIDist.Open(upath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		// Every embed.FS file implements io.ReadSeeker; this only exists
		// so a broken assumption fails loudly instead of panicking below.
		http.Error(w, "internal error: embedded UI asset is not seekable", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, upath, stat.ModTime(), rs)
}

// --- TaskNotes webhook handling ---

type webhookPayload struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

type webhookTask struct {
	Path     string   `json:"path"`
	Title    string   `json:"title"`
	Status   string   `json:"status"`
	Tags     []string `json:"tags"`
	Projects []string `json:"projects"`
	Details  string   `json:"details"`

	// ClaudeProject captures the "claude-project" key when TaskNotes puts it
	// directly on the task object rather than nested under
	// customProperties.
	ClaudeProject string `json:"claude-project,omitempty"`
	// CustomProperties captures task.customProperties (or, if absent there,
	// a sibling "customProperties" on the outer data object — see
	// extractWebhookTask).
	CustomProperties map[string]string `json:"customProperties,omitempty"`
	// BlockedBy mirrors Task.BlockedBy — present when the webhook payload
	// (or a fetched Task, via taskToWebhookTask) includes it.
	BlockedBy []TaskDependency `json:"blockedBy,omitempty"`
	// IsBlocked is a pointer so nil ("no evidence either way" — a webhook
	// payload may omit it) is distinguishable from a known false (a live
	// fetch confirmed the task isn't actually blocked despite having
	// BlockedBy entries, e.g. all of them are now complete).
	IsBlocked *bool `json:"isBlocked,omitempty"`
	// DetailsKnown is true only when Details reflects a confirmed-current,
	// complete read of the task's actual body: a GetTask-sourced
	// conversion (taskToWebhookTask, always true — a live fetch always
	// returns the real body), or a raw webhook payload whose JSON actually
	// contained a "details" key (see extractWebhookTask/hasJSONKey).
	// False — NOT "confirmed empty" — for a query-sourced task (POST
	// /api/tasks/query never returns details) or any webhook payload that
	// omitted the key (e.g. a partial/status-only update). json.Unmarshal
	// alone can't distinguish "field absent" from "field present and
	// empty", both leaving Details=="" — LIVE INCIDENT: diffClaudeLinesLocked
	// used to treat every Details=="" observation as "content changed to
	// nothing", wiping routedClaudeLines for that path; the next
	// confirmed-real observation then saw "changed, nothing previously
	// routed" and re-dispatched every already-handled @claude line,
	// re-triggering the needs-input/review auto-flip — confirmed live
	// across 20+ tasks (taskDetailsHash == sha256("")).
	DetailsKnown bool `json:"-"`
}

// hasJSONKey reports whether raw (a JSON object) contains key, regardless
// of that key's value — including an explicit null. Used to distinguish
// "the field is absent from this payload" from "the field is present and
// happens to be zero-valued", which json.Unmarshal alone cannot: a missing
// "details" key and an explicit "details":"" both unmarshal a Go string
// field to the same "". Returns false (not present) if raw doesn't parse
// as a JSON object at all.
func hasJSONKey(raw json.RawMessage, key string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

// extractWebhookTask pulls the task out of a webhook data payload, which may
// hold the task directly or nested under a "task" key. When nested, a
// sibling "customProperties" on the outer data object is merged in as a
// fallback, since TaskNotes sometimes places it there instead of on the task
// itself. DetailsKnown is set from whichever JSON object actually became
// the task (wrapper.Task when nested, data otherwise) — see hasJSONKey and
// webhookTask.DetailsKnown: a webhook payload that omits "details"
// entirely (e.g. a partial/status-only update) must not be mistaken for
// "this task's content is now empty".
func extractWebhookTask(data json.RawMessage) (webhookTask, bool) {
	if len(data) == 0 {
		return webhookTask{}, false
	}
	var wrapper struct {
		Task             json.RawMessage   `json:"task"`
		CustomProperties map[string]string `json:"customProperties,omitempty"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil && len(wrapper.Task) > 0 {
		var t webhookTask
		if err := json.Unmarshal(wrapper.Task, &t); err == nil {
			if t.CustomProperties == nil {
				t.CustomProperties = wrapper.CustomProperties
			}
			t.DetailsKnown = hasJSONKey(wrapper.Task, "details")
			return t, true
		}
	}
	var t webhookTask
	if err := json.Unmarshal(data, &t); err != nil {
		return webhookTask{}, false
	}
	t.DetailsKnown = hasJSONKey(data, "details")
	return t, true
}

// routingSlug resolves the bridge routing slug for a task: the
// "claude-project" custom property (whether TaskNotes put it directly on
// the task or nested under customProperties) if set; otherwise the first
// projects[] entry, normalized — TaskNotes projects are wikilinks to project
// notes (e.g. "[[Myapp/Subprojects/E2E Tests]]"), not plain slugs.
func (t webhookTask) routingSlug() string {
	cp := t.ClaudeProject
	if cp == "" {
		cp = t.CustomProperties["claude-project"]
	}
	return resolveRoutingSlugFrom(cp, t.Projects)
}

// resolveRoutingSlugFrom resolves the bridge routing slug given a
// claude-project custom property value (if any) and a projects[] fallback —
// shared by webhook routing (webhookTask.routingSlug) and the due-task
// scanner (routingSlugForTask), so both agree on the same task.
func resolveRoutingSlugFrom(claudeProject string, projects []string) string {
	if claudeProject != "" {
		return normalizeProjectSlug(claudeProject)
	}
	if len(projects) == 0 {
		return ""
	}
	return normalizeProjectSlug(projects[0])
}

// routingSlugForTask resolves the bridge routing slug for a TaskNotes API
// task (as returned by Client.QueryTasks) the same way webhookTask.routingSlug
// does for webhook payloads.
func routingSlugForTask(t Task) string {
	return resolveRoutingSlugFrom(t.CustomProperties["claude-project"], t.Projects)
}

// normalizeProjectSlug turns a TaskNotes project reference into a bare
// routing slug: strips surrounding "[[" "]]" wikilink brackets, drops any
// path prefix (keeping only the text after the last "/"), drops a trailing
// ".md", and lowercases the result — the claude-project user field's
// autosuggestion inserts wikilinks too, so all slug sources share this,
// and config keys / orchestrator names / tmux sessions are matched on the
// lowercase form.
func normalizeProjectSlug(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "[[")
	s = strings.TrimSuffix(s, "]]")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(s, ".md")
	return strings.ToLower(strings.TrimSpace(s))
}

// claudeLineRe matches a line starting with "@claude" — optionally preceded
// by one or more Obsidian blockquote markers ("> ", possibly repeated for
// nested quoting) — followed by EITHER an explicit separator (colon,
// comma, hyphen, or em-dash, optionally padded with spaces/tabs) OR
// required whitespace with no punctuation, then the rest of the line as
// the captured text. The leading "(?:>[ \t]*)*" is a hard requirement, not
// cosmetic: SPEC-note-layout.md's reply block instructs the user to write
// "@claude: ..." INSIDE a callout, where wrapCallout `> `-prefixes every
// line — without this, a reply typed exactly where we tell the user to
// write it would never route (a reply block that's a trap). Requiring \b
// after "@claude" excludes "@claudeX"-style false positives; requiring the
// line to START with "@claude" (after stripping any quote markers), not
// just contain it, excludes mid-line mentions.
//
// The separator and the required-whitespace forms are a DELIBERATE
// alternation, not "optional separator then optional whitespace": the
// reply block's own unfilled placeholder line is literally "@claude:" with
// nothing after it (see defaultReplyBody) — with an optional (rather than
// alternated) separator, a backtracking-equivalent match can satisfy the
// capture group's minimum-one-char requirement by NOT consuming the colon
// and instead capturing the colon itself as "content", so the still-empty
// placeholder line spuriously extracts as a routed reply with text ":"
// (verified live: this silently self-triggered the needs-input auto-flip
// the instant `tn ask` wrote the block, before the user replied to
// anything). Forcing the separator to be part of a mandatory alternative —
// never optional on its own — makes "separator with nothing after"
// structurally unable to match at all.
var claudeLineRe = regexp.MustCompile(`(?m)^(?:>[ \t]*)*@claude\b(?:[ \t]*[:,\-—][ \t]*|[ \t]+)(.+)$`)

// claudeMarkerRe matches an "@claude:" (or @claude, / @claude-em-dash /
// bare-whitespace-separated) MARKER LINE — no trailing capture group,
// unlike claudeLineRe. Used by extractClaudeDirectives against ALREADY-
// UNWRAPPED block text (see parseNoteBody/unwrapCallout), so — unlike
// claudeLineRe — it never needs to account for a leading ">" blockquote
// prefix; that was stripped before this ever runs. Same \b + mandatory-
// separator-or-whitespace shape as claudeLineRe, for the same reasons
// (rejects "@claudeX" false positives; a bare "@claude:" placeholder with
// nothing after it still matches the marker but captures no content, see
// extractClaudeDirectivesFromBlock).
var claudeMarkerRe = regexp.MustCompile(`(?m)^@claude\b(?:[ \t]*[:,\-—][ \t]*|[ \t]+)`)

// extractClaudeDirectives is the PRIMARY entry point for finding routable
// "@claude:" directives in a task's details — replacing extractClaudeLines
// for any note with at least one structured tn: anchor (i.e., every note
// this daemon or `tn` itself has ever written; extractClaudeLines survives
// below as the fallback for a genuinely legacy, unanchored body).
//
// THE BUG THIS FIXES (2026-08-25 incident): extractClaudeLines' claudeLineRe
// is line-anchored — Go's regexp "." never matches a newline, so "(.+)$"
// stops at the first line break. That alone would be harmless if a reply
// stayed one line, but wrapCallout renders a multi-line reply by splitting
// on "\n" and "> "-prefixing EACH line separately — so only the very FIRST
// line of a multi-paragraph reply literally starts with "@claude:", and
// every line after the first newline was silently invisible to the old
// regex. Confirmed live against real production messages: a reply's note
// held the complete multi-paragraph text (the WRITE path was always fine)
// while the ROUTED message was permanently cut to just the first
// paragraph — an audit that day found this had silently truncated 10 of
// 10 multi-line replies from one sender alone, including three of the
// user's own typed-in-Obsidian replies, and the false design HARDEN
// emulator-vm.sh was built from was one of the casualties.
//
// THE FIX: parseNoteBody already correctly reconstructs each block's full
// multi-line text — unwrapCallout strips every line's "> " prefix and
// rejoins with "\n" — that was NEVER broken; only the separate whole-
// details regex re-scan was. So: parse the note into its structured
// blocks first, then within each ROUTABLE block (Ask/Reply/Links/Brief/
// Description — History is NEVER scanned, same rule extractClaudeLines
// already enforced, for the same reason: it's a permanent record of past,
// already-acted-on directives, not a live instruction, and Ask/Reply/etc
// happen to share the "> "-quoting wrapCallout applies to History too),
// find each "@claude" marker line and capture from it up to (but not
// including) the START of the next marker line in the SAME block, or the
// end of the block if it's the last one in that block. This is bounded by
// actual document structure — a callout's own close tag, or
// parseNoteBody's own block split — never by a regex guessing where a
// paragraph ends, which is exactly the failure mode a naive "(?s)" or
// greedy multiline pattern would reintroduce: it would over-capture into
// whatever text follows in the SAME block, not just recover what this
// bug lost.
func extractClaudeDirectives(details string) []string {
	nb, ok := parseNoteBody(details)
	if !ok {
		// No structured tn: anchors at all — a body this daemon has never
		// rewritten (or a minimal ad-hoc payload, as several of this
		// file's own dedup tests deliberately construct). No reply
		// callout exists here to fix, so the historical single-line
		// extraction is exactly right, not merely tolerated.
		return extractClaudeLines(details)
	}

	// nb.Reply is the ONE block where capturing the full multi-line text
	// is safe to do unconditionally: per SPEC-note-layout.md, the reply
	// block's entire purpose is to hold the user's answer in full — nobody
	// writes unrelated notes inside their own reply the way Description
	// (a general free-text field that legitimately mixes a directive with
	// other, unrelated prose) or an ad-hoc unanchored body can. It is also
	// the ONLY block every real 2026-08-25 truncation incident actually
	// happened in: POST /reply and every user-typed Obsidian reply both
	// write here, and nowhere else.
	//
	// Ask/Links/Brief/Description deliberately keep the historical,
	// single-line-only extraction (extractClaudeLines) rather than the new
	// capture-to-block-end logic: an @claude marker there can legitimately
	// have unrelated content on a following line within the SAME block
	// (the daemon's own dedup tests exercise exactly this shape —
	// "@claude: do X" followed by an unrelated "more notes" line in the
	// same block, which must NOT be swallowed into the directive), so
	// "capture to block end" would over-capture in a way it never does
	// for Reply. This is a deliberate, narrower scope, not an oversight —
	// widening it needs a real boundary signal Description doesn't have,
	// not just extending the same rule further.
	var out []string
	out = append(out, extractClaudeDirectivesFromBlock(nb.Reply)...)
	rest := details
	if _, r, found := extractBlock(details, "reply"); found {
		rest = r // scan everything else via the old path, minus Reply's own text, so it isn't double-counted
	}
	out = append(out, extractClaudeLines(rest)...)
	return out
}

// extractClaudeDirectivesFromBlock finds every "@claude" directive within
// one already-unwrapped block of plain text (no blockquote markup) — see
// extractClaudeDirectives.
func extractClaudeDirectivesFromBlock(block string) []string {
	if block == "" {
		return nil
	}
	locs := claudeMarkerRe.FindAllStringIndex(block, -1)
	out := make([]string, 0, len(locs))
	for i, loc := range locs {
		start := loc[1] // just past the marker + its separator
		end := len(block)
		if i+1 < len(locs) {
			end = locs[i+1][0] // stop before the NEXT marker in this block
		}
		text := strings.TrimSpace(block[start:end])
		if text == "" {
			// A bare "@claude:" placeholder (or one with only trailing
			// whitespace before the next marker/block end) carries nothing
			// to route — same rule extractClaudeLines already enforced.
			continue
		}
		out = append(out, text)
	}
	return out
}

// extractClaudeLines is the PRE-2026-08-25 extraction implementation —
// NOT called from any live path anymore (superseded by
// extractClaudeDirectives). Kept only as the historical-bug reference and
// regression fixture: TestExtractClaudeDirectives_MultiLineReply_
// CapturesEverything calls this directly to prove the truncation-incident
// bug it used to have is real and reproducible, and its own table-driven
// tests stay as a record of the marker-matching edge cases the new
// claudeMarkerRe was built to preserve (see extractClaudeDirectives).
func extractClaudeLines(details string) []string {
	// NEVER scan the history block: it's a permanent, append-only record of
	// PAST directives already acted on, not live instructions. wrapCallout
	// applies the exact same "> " quote-prefix to the history block as it
	// does to the reply block, so claudeLineRe's callout-quote support
	// (needed for a live reply typed where the reply block instructs) would
	// otherwise ALSO match a user's original "@claude: ..." line the moment
	// it's swept into a history entry — which happens naturally over a
	// task's lifecycle (a reply gets both routed AND logged to history) —
	// and keep matching it on every future observation forever, regardless
	// of RoutedClaudeLines dedup succeeding or not. LIVE INCIDENT: this
	// caused the same already-actioned reply to be re-dispatched as a
	// "new" assignment four times over 30 minutes on a real task, each one
	// re-triggering the needs-input/review auto-flip and silently reverting
	// its status — exactly the board-unreliability class of bug this
	// session already fixed once for a different root cause. Only the live
	// portion of the body (everything outside the history block: ask,
	// reply, links, brief, description) is ever routable.
	scannable := details
	if _, rest, found := extractBlock(details, "history"); found {
		scannable = rest
	}

	matches := claudeLineRe.FindAllStringSubmatch(scannable, -1)
	lines := make([]string, 0, len(matches))
	for _, m := range matches {
		text := strings.TrimSpace(m[1])
		if text == "" {
			// A bare "@claude" line (or one with only trailing whitespace)
			// carries nothing to route.
			continue
		}
		lines = append(lines, text)
	}
	return lines
}

// humanAttentionStatuses are TaskNotes statuses that park a task for the
// user rather than an agent. Used by the auto-flip logic (needs-input/review
// replies) and the due-task scanner query.
var humanAttentionStatuses = map[string]bool{
	"triage":      true,
	"needs-input": true,
	"review":      true,
}

// assignableStatuses is the POSITIVE allowlist routeTaskNotification's
// tag-assignment branch requires: a task is only ever (re-)assigned while
// its status is genuinely open, unstarted work. A negative check
// (excluding only humanAttentionStatuses) is NOT enough — it lets
// in-progress and done tasks through too, and hasPendingAssignmentLocked
// alone doesn't save it, since that guard stops blocking the moment the
// PRIOR assignment gets acked: every subsequent webhook-miss-reconciler
// pass (every 10min, and on every daemon restart) would then re-assign any
// claude-tagged recently-modified task, even one already done. "" and
// "none" are included alongside "open" since TaskNotes tasks can have no
// status set at all.
var assignableStatuses = map[string]bool{
	"open": true,
	"":     true,
	"none": true,
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

// isBlockedByUnresolvedDependency reports whether t's dependencies should
// hold back an assignment: it has blockedBy entries and there's no fetched
// evidence (a known-false IsBlocked) that it's actually startable despite
// them. A webhook payload lacking isBlocked entirely (IsBlocked nil) is
// treated as "not proven unblocked" — pragmatically conservative, since the
// unblock pass (triggered when a blocker completes) is what routes it once
// TaskNotes itself confirms it's startable.
func isBlockedByUnresolvedDependency(t webhookTask) bool {
	if len(t.BlockedBy) == 0 {
		return false
	}
	return t.IsBlocked == nil || *t.IsBlocked
}

// integrationBranchFor resolves a task's target integration branch: the
// task's OWN customProperties["integration-branch"], or "" — deliberately
// no repo-note or other fallback source. The user's model is that
// "development" is the default target for everything; only tasks that
// explicitly carry the field (set directly, or inherited via `tn create
// --from`, see cmdCreate) target something else. A repo-wide default would
// wrongly capture every unrelated task in the repo — this was corrected
// from an earlier draft of the spec that would have added a repo-note
// fallback.
func integrationBranchFor(t Task) string {
	return t.CustomProperties["integration-branch"]
}

// integrationBranchForWebhookTask is integrationBranchFor's webhookTask
// counterpart, for the routeTaskNotification entry point (webhook +
// reconciler), which never carries a full Task.
func integrationBranchForWebhookTask(t webhookTask) string {
	return t.CustomProperties["integration-branch"]
}

// integrationRoleFor resolves a task's workstream role from its OWN
// customProperties["integration-role"] — "owner" or "" (unset = member),
// task-level only like integrationBranchFor. An owner holds the single
// consolidated MR from the integration branch into development and is
// `blockedBy` every member task; a member's MR targets the integration
// branch itself. `tn create --from` deliberately does NOT inherit this —
// see cmdCreate — a child must never inherit ownership of its parent's
// consolidated MR.
func integrationRoleFor(t Task) string {
	return t.CustomProperties["integration-role"]
}

// integrationRoleForWebhookTask is integrationRoleFor's webhookTask
// counterpart, for the routeTaskNotification entry point.
func integrationRoleForWebhookTask(t webhookTask) string {
	return t.CustomProperties["integration-role"]
}

// appendIntegrationBranchSuffix appends the integration-branch/role suffix
// to an assignment message's text — how a fresh generation (the
// orchestrator that assigned the task may have since retired) learns what
// to do without reading the board: a member gets " [integration branch:
// <b>]" (stack onto it), the OWNER gets " [integration OWNER of <b> —
// consolidated MR to development]" instead (finalize, don't stack — it
// must know its job is different from a member's the moment it's
// assigned). text is returned unchanged when branch is "".
func appendIntegrationBranchSuffix(text, branch, role string) string {
	if branch == "" {
		return text
	}
	if role == "owner" {
		return fmt.Sprintf("%s [integration OWNER of %s — consolidated MR to development]", text, branch)
	}
	return fmt.Sprintf("%s [integration branch: %s]", text, branch)
}

// hasPendingAssignmentLocked reports whether state already has a
// not-yet-acked "Task assigned:" message for taskPath. Must be called with
// s.mu held.
func (s *Server) hasPendingAssignmentLocked(taskPath string) bool {
	for _, m := range s.state.Messages {
		if m.TaskPath == taskPath && m.Status != "acked" && strings.HasPrefix(m.Text, "Task assigned:") {
			return true
		}
	}
	return false
}

// markAssignedLocked records that taskPath was just routed an assignment,
// making the current "open episode" one-shot. Writes BOTH dedup ledgers in
// the same locked section — AssignedTasks (per-episode, cleared by
// observeTaskStatusLocked) and AssignedOccurrences (per-calendar-day, used
// by processDueTask) — because writing only one, as an earlier version of
// this fix did per call site, is exactly what let a webhook-first
// assignment be silently re-assigned by the very next scanner pass: the
// scanner's own dedup only ever checked AssignedOccurrences, which the
// webhook/reconciler path never touched. today is the caller's own
// "YYYY-MM-DD" (not computed fresh here) so a single logical assignment
// event writes one consistent date to both ledgers, and so processDueTask
// can keep using the same today value it already threads through its own
// call chain (some existing tests construct a fixed 'today' rather than
// relying on time.Now()). Must be called with s.mu held; does not itself
// persist state (callers already call saveLocked, either directly or via
// dispatchMessage).
func (s *Server) markAssignedLocked(taskPath, today string) {
	s.state.AssignedTasks[taskPath] = time.Now().Format(time.RFC3339)
	s.state.AssignedOccurrences[taskPath] = today
}

// observeTaskStatusLocked clears BOTH of taskPath's dedup ledger entries
// (see markAssignedLocked) the moment its observed status leaves
// assignableStatuses — a task genuinely leaving assignable status ends its
// current episode entirely, in both ledgers, not just the one
// routeTaskNotification's assignment branch happens to check; a task only
// gets assigned again after a genuine non-open -> open transition starts a
// NEW episode, never merely because a prior assignment message got acked.
// Called from every place a task's current status is observed
// (routeTaskNotification — covering both the webhook handler and the
// reconciler, since both funnel through it — plus the due-task scanner and
// the MR watcher), so the marker self-heals regardless of which pass
// happens to notice the transition first. Reports whether anything was
// actually deleted, so callers only need to persist state when this
// returns true. Must be called with s.mu held.
func (s *Server) observeTaskStatusLocked(taskPath, status string) bool {
	if taskPath == "" || assignableStatuses[status] {
		return false
	}
	_, hasAssigned := s.state.AssignedTasks[taskPath]
	_, hasOccurrence := s.state.AssignedOccurrences[taskPath]
	if !hasAssigned && !hasOccurrence {
		return false
	}
	delete(s.state.AssignedTasks, taskPath)
	delete(s.state.AssignedOccurrences, taskPath)
	return true
}

func (s *Server) handleTaskNotesWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	var payload webhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	// task.completed is NOT a variant of task.updated — TaskNotes fires
	// them as MUTUALLY EXCLUSIVE alternatives: a status transition INTO a
	// completed status fires ONLY task.completed, never task.updated (see
	// requiredWebhookEvents). Missing it here means every transition into
	// "done" is invisible until the reconciler's next pass (up to 10min).
	if payload.Event == "task.created" || payload.Event == "task.updated" || payload.Event == "task.completed" {
		if t, ok := extractWebhookTask(payload.Data); ok {
			s.routeTaskNotification(t)
		}
	}

	w.WriteHeader(http.StatusOK)
}

func (s *Server) routeTaskNotification(t webhookTask) {
	project := t.routingSlug()

	// Self-healing re-arm (see observeTaskStatusLocked): the moment this
	// task is observed outside assignableStatuses, its one-shot assignment
	// marker is cleared, so a genuine future reopen still gets assigned
	// exactly once. Runs unconditionally on every observation — both the
	// webhook handler and the reconciler funnel through this one function.
	if t.Path != "" {
		s.mu.Lock()
		if s.observeTaskStatusLocked(t.Path, t.Status) {
			s.saveLocked()
		}
		s.mu.Unlock()
	}

	// Assignment routing only fires for a task in an assignableStatuses
	// status (open/""/none) — NOT a negative "skip these few" check: a task
	// that's triage/needs-input/review is parked for the user (moving it to
	// open is their approval, which fires task.updated and routes it
	// normally then), but a task that's in-progress or already done must
	// never be re-assigned either, and only the positive allowlist
	// guarantees that. @claude: line routing below stays status-agnostic,
	// since users comment on parked tasks (e.g. answering a needs-input
	// question) without reopening them. Assignment also skips a task with
	// an unresolved dependency (see isBlockedByUnresolvedDependency) — the
	// unblock pass (triggered when a blocker completes) is what routes it
	// once it's actually startable.
	if (hasTag(t.Tags, "spawn") || hasTag(t.Tags, "claude")) && assignableStatuses[t.Status] && !isBlockedByUnresolvedDependency(t) {
		s.mu.Lock()
		// AssignedTasks is the fix for the live re-assignment-storm bug:
		// hasPendingAssignmentLocked alone only blocks while the PRIOR
		// assignment message is unacked, and orchestrators ack immediately
		// per contract — so without this, every subsequent
		// webhook-miss-reconciler pass (every 10min, and on every daemon
		// restart) re-assigns any task that legitimately just sits in
		// "open", visibly reverting its status back to in-progress each
		// time. AssignedTasks makes assignment one-shot per open episode
		// instead; hasPendingAssignmentLocked still additionally covers the
		// task.created/task.updated double-fire within the same episode.
		_, alreadyAssignedThisEpisode := s.state.AssignedTasks[t.Path]
		alreadyPending := s.hasPendingAssignmentLocked(t.Path)
		shouldAssign := !alreadyAssignedThisEpisode && !alreadyPending
		if shouldAssign {
			s.markAssignedLocked(t.Path, time.Now().Format("2006-01-02"))
			s.saveLocked()
		}
		s.mu.Unlock()

		if shouldAssign {
			s.dispatchMessage(sendRequest{
				Project:  project,
				TaskPath: t.Path,
				Text:     appendIntegrationBranchSuffix(fmt.Sprintf("Task assigned: %s (%s)", t.Title, t.Path), integrationBranchForWebhookTask(t), integrationRoleForWebhookTask(t)),
			})
		}
	}

	if t.Path == "" {
		return
	}

	routedLines := s.diffClaudeLinesLocked(t.Path, t.Details, t.DetailsKnown)
	for _, line := range routedLines {
		s.dispatchMessage(sendRequest{
			Project:  project,
			TaskPath: t.Path,
			Text:     line,
		})
	}

	// A user's @claude: reply IS the unblock signal for a needs-input task
	// — no reason to wait for the agent to notice and flip the status
	// itself. Applies via both entry points (webhook and the reconciler),
	// since both funnel through this shared pipeline.
	//
	// A reply on a review task is treated the same way: it's a review
	// rejection/change request (per ORCHESTRATOR.md — "a reply on a review
	// task is a review rejection"), so the task goes back to in-progress for
	// the agent to address before anything else on it. Worst case a
	// "looks good" comment briefly reopens a task the agent then re-reviews
	// — acceptable.
	if len(routedLines) > 0 && (t.Status == "needs-input" || t.Status == "review") {
		s.autoFlipToInProgress(t.Path, t.Status, strings.Join(routedLines, " "))
	}

	// A task finishing may be exactly what unblocks other tasks' own
	// blockedBy dependency — check for any now-startable dependents.
	// Applies via both entry points, same as the auto-flip above.
	if t.Status == "done" {
		s.runUnblockPass(t.Title)
	}

	// Mechanism 2's inform-never-revert half. Called unconditionally
	// (like observeTaskStatusLocked above) since it needs to self-heal its
	// own dedup marker on a status that's NOT "done" too — see
	// noteDoneMeansGapIfUnmet's doc comment. Same "both entry points
	// funnel through here" reasoning as everything else in this function.
	s.noteDoneMeansGapIfUnmet(t)

	// Jira transition/comment requests: same "both entry points funnel
	// through here" reasoning as everything else above.
	s.checkJiraTransition(t)
}

// autoFlipToInProgress sets a needs-input or review task back to
// in-progress right after a user's @claude: reply routes (fromStatus is
// whichever of the two it was, purely for the log line) — and, per
// SPEC-note-layout.md, clears the ask block (the question has now been
// answered) and prepends a "user replied: <first 120 chars>" history
// entry, in the SAME PUT as the status change. Tolerates API failure:
// logs and moves on — the agent's own contract (set in-progress on reply,
// `tn ask --clear`) still covers it as a fallback, so this is never
// retried here.
func (s *Server) autoFlipToInProgress(taskPath, fromStatus, replyText string) {
	if s.tnClient == nil {
		return
	}
	task, err := s.tnClient.GetTask(taskPath)
	if err != nil {
		log.Printf("serve: failed to fetch %s for auto-flip: %v", taskPath, err)
		return
	}
	entry := formatHistoryEntry("bridge", fmt.Sprintf("user replied: %s", truncate(replyText, 120)), time.Now())
	newDetails := applyNoteBodyEdit(task.Details, func(nb *noteBody) {
		nb.Ask = ""
		nb.History = append([]string{entry}, nb.History...)
	})
	if _, err := s.tnClient.UpdateTask(taskPath, map[string]any{"details": newDetails, "status": "in-progress"}); err != nil {
		log.Printf("serve: failed to auto-flip %s task %s to in-progress: %v", fromStatus, taskPath, err)
		return
	}
	log.Printf("serve: %s task %s auto-flipped to in-progress on user reply (ask cleared)", fromStatus, taskPath)
}

// jiraTarget describes what to ask an agent to do with a Jira issue when a
// task transitions into a TaskNotes status of interest: category is the
// Jira status CATEGORY to aim for (never a specific transition/status
// name — those are localized and per-project workflow, so only an agent
// with mcp__jira__jira_transitions can resolve one), and hint is
// human-readable guidance included in the request text.
type jiraTarget struct{ category, hint string }

// jiraStatusesOfInterest maps a TaskNotes status to the Jira target it
// implies. Any status not listed here (open, triage, needs-input) produces
// no Jira request — those aren't meaningful Jira-workflow signals. review
// deliberately doesn't claim a distinct category from in-progress: Jira's
// own category set doesn't have a universal "review" category, and
// whether a specific project's workflow has a named "In Review" status is
// something only the agent, via jira_transitions, can determine — the hint
// says so explicitly rather than the daemon guessing.
var jiraStatusesOfInterest = map[string]jiraTarget{
	"in-progress": {category: "indeterminate", hint: "In Progress"},
	"review":      {category: "indeterminate", hint: `an "In Review" transition if the workflow has one, otherwise In Progress`},
	"done":        {category: "done", hint: "Done"},
}

// splitJiraKeys splits a comma-separated `jira` custom-property value
// ("PROJ-101,PROJ-102") into trimmed, non-empty issue keys.
func splitJiraKeys(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// checkJiraTransition emits a Jira transition (for `jira`) or comment-only
// (for `jira-nc`) REQUEST as a bridge message whenever it observes a
// GENUINE status change into one of jiraStatusesOfInterest, for a task
// carrying either custom property. The daemon has no Jira credentials —
// jira is an HTTP MCP an AGENT calls, not the daemon — so it can only
// ask; per SPEC-serve.md this is a hard constraint, not a convenience.
// `jira-nc` (a regulated nonconformity record) NEVER gets a transition
// request, only a comment request: the applicable nonconformity procedure requires closure stay a human/
// agent decision, never an automatic status change.
//
// "Genuine change" is tracked via LastKnownStatus: a task's first-ever
// observation only seeds it (no request — matches the other migration-
// style dedup maps in this file, so shipping this doesn't flood every
// existing jira-tagged task with a request the moment it's enabled), and
// repeated observations of the SAME status are no-ops. Dedup beyond that
// is one-shot per (task, key, target) via JiraRequests, cleared for the
// whole path the moment its status changes to anything else — a new
// status episode can request the same target again later. Must be called
// with s.mu NOT held; it manages its own locking (and may dispatch).
func (s *Server) checkJiraTransition(t webhookTask) {
	if t.Path == "" {
		return
	}

	s.mu.Lock()
	prevStatus, hadPrior := s.state.LastKnownStatus[t.Path]
	changed := hadPrior && prevStatus != t.Status
	if changed {
		prefix := t.Path + "|"
		for key := range s.state.JiraRequests {
			if strings.HasPrefix(key, prefix) {
				delete(s.state.JiraRequests, key)
			}
		}
	}
	s.state.LastKnownStatus[t.Path] = t.Status
	s.saveLocked()
	s.mu.Unlock()

	if !hadPrior || !changed {
		return
	}

	target, hasTarget := jiraStatusesOfInterest[t.Status]
	if !hasTarget {
		return
	}

	jiraKeys := splitJiraKeys(t.CustomProperties["jira"])
	ncKey := strings.TrimSpace(t.CustomProperties["jira-nc"])
	if len(jiraKeys) == 0 && ncKey == "" {
		return
	}

	project := t.routingSlug()
	mrURL := t.CustomProperties["mr"]
	cfg := s.projectConfigFor(project)
	namedStatus, hasNamed := cfg.JiraStatusMap[t.Status]
	namedStatus = strings.TrimSpace(namedStatus)
	hasNamed = hasNamed && namedStatus != ""

	for _, key := range jiraKeys {
		dedupKey := t.Path + "|" + key + "|" + target.category
		s.mu.Lock()
		_, already := s.state.JiraRequests[dedupKey]
		if !already {
			s.state.JiraRequests[dedupKey] = time.Now().Format(time.RFC3339)
			s.saveLocked()
		}
		s.mu.Unlock()
		if already {
			continue
		}
		var text string
		if hasNamed {
			// A NAMED target status, not a category: this project's
			// configured graph (Reference/Jira workflows.md) means the
			// board-level status this task status implies has a specific
			// name that a category alone can't express — CX Bug's "done"
			// is MERGED, several hops before the category-done status
			// Fertig (reached only after acceptance testing/release).
			// Requesting a category here was wrong in both directions: it
			// could hit the rejection trap, and even matched "correctly"
			// it would skip QA/release states and falsely claim the work
			// released. Multi-hop is explicitly expected, not a fallback
			// — "approve" and similar mechanical edges are meant to be
			// walked, only human/QA/rejection states are off-limits.
			text = fmt.Sprintf(
				"Jira: transition %s because task %s moved to %s. Target Jira status: %q — this is the instruction, not a category (category %q is only a sanity hint; a category match alone can hit rejection or a QA/release state that only looks similar). Use mcp__jira__jira_transitions and walk the graph one legitimate edge at a time toward %q, re-checking the resulting status after every hop — multi-hop is expected here, not a fallback. Never transition into any of: Acceptance Test, In AT, Ready for Release, Fertig, Retired, Zurückgewiesen, Duplicate. If no path to %q exists from the current status, stop and tn ask on the task naming %q as the desired status rather than settling for a same-category stand-in.",
				key, t.Path, t.Status, namedStatus, target.category, namedStatus, namedStatus, namedStatus,
			)
		} else {
			// No configured mapping for this project/status: fall back to
			// the category-only request. Safer but less precise — still
			// guards the rejection/blocked trap explicitly, since category
			// alone genuinely cannot distinguish them.
			text = fmt.Sprintf(
				"Jira: transition %s to %s because task %s moved to %s. Use mcp__jira__jira_transitions to list, pick the transition whose toStatus category matches %q AND whose meaning genuinely matches reaching %s — never a transition that merely shares the category, such as a rejection/withdrawal (\"Won't do\", \"Zurückgewiesen\") when the target is a done category, or a \"Blocked\"-style status when the target is indeterminate. If no transition satisfies BOTH the category and that meaning (the issue may be several workflow states behind the work), do not pick the nearest same-category option and do not force a multi-hop jump — refuse and tn ask on the task naming the desired category instead.",
				key, target.hint, t.Path, t.Status, target.category, target.hint,
			)
		}
		s.dispatchMessage(sendRequest{Project: project, TaskPath: t.Path, Text: text, SkipOwnerRouting: true})
	}

	if ncKey != "" {
		dedupKey := t.Path + "|" + ncKey + "|nc-comment"
		s.mu.Lock()
		_, already := s.state.JiraRequests[dedupKey]
		if !already {
			s.state.JiraRequests[dedupKey] = time.Now().Format(time.RFC3339)
			s.saveLocked()
		}
		s.mu.Unlock()
		if !already {
			text := fmt.Sprintf("Jira NC: add a comment to %s noting task %s reached %s", ncKey, t.Path, t.Status)
			if mrURL != "" {
				text += fmt.Sprintf(" (MR: %s)", mrURL)
			}
			text += " — do NOT change its status; it's a regulated record and closure follows your nonconformity procedure."
			s.dispatchMessage(sendRequest{Project: project, TaskPath: t.Path, Text: text, SkipOwnerRouting: true})
		}
	}
}

// noteDoneMeansGapIfUnmet is mechanism 2's inform-never-revert half. For a
// task observed in "done" status (from EITHER the webhook or the
// reconciler — both funnel through routeTaskNotification), checks its DONE
// MEANS criteria and, if unmet, appends a single "bridge"-attributed note
// listing which Evidence numbers are missing. It NEVER changes status —
// per the 2026-08-25 decision, a close made by a human in Obsidian is a
// decision, not an error, and only gets informed, never overridden.
//
// This function cannot tell whether a given "done" observation came from a
// human closing the task in Obsidian or an agent that reached "done" some
// other way than the checked path — and it does not need to: it only ever
// finds something to report for a close that did NOT go through POST
// /tasks/close, because a checked close's criteria were already verified
// synchronously before the status write happened (see handleCloseTask), so
// this function re-checks the same now-committed Details, finds nothing
// missing, and is a silent no-op. Routing IS the attribution — this
// function never needs to guess who made the change.
//
// Requires t.DetailsKnown: a partial webhook payload's empty Details must
// never be read as "no DONE MEANS block, nothing to check" — that is
// exactly the absence-of-signal bug class that has already bitten this
// file more than once (see webhookTask.DetailsKnown's own doc comment). A
// check skipped here for that reason is caught on the reconciler's next
// pass, which always has a live-fetched, real Details.
//
// One note per done EPISODE, not per observation: State.DoneMeansNoted
// records that the note has already fired for this path and is cleared
// the moment the task is next observed outside "done" (self-healing,
// mirrors AssignedTasks/observeTaskStatusLocked) — so a repeat webhook or
// reconciler pass over an unchanged done task never re-notes, but a
// genuine reopen-then-reclose is checked fresh.
func (s *Server) noteDoneMeansGapIfUnmet(t webhookTask) {
	if t.Path == "" {
		return
	}

	if t.Status != "done" {
		s.mu.Lock()
		if _, had := s.state.DoneMeansNoted[t.Path]; had {
			delete(s.state.DoneMeansNoted, t.Path)
			s.saveLocked()
		}
		s.mu.Unlock()
		return
	}

	if !t.DetailsKnown {
		return
	}

	s.mu.Lock()
	_, already := s.state.DoneMeansNoted[t.Path]
	s.mu.Unlock()
	if already {
		return
	}

	check := checkDoneMeans(t.Details)
	if check.Satisfied() {
		return
	}

	if s.tnClient == nil {
		return
	}
	note := fmt.Sprintf(
		"DONE MEANS check: %d of %d criteria have no matching Evidence line (missing: %s). Informational only — this close was NOT reverted.",
		len(check.MissingNumbers), check.RequiredCount, formatMissingNumbers(check.MissingNumbers),
	)
	if _, err := s.tnClient.AddNote(t.Path, note, "bridge"); err != nil {
		log.Printf("serve: done-means: failed to note unmet criteria on %s: %v", t.Path, err)
		return
	}
	log.Printf("serve: done-means: noted %d unmet criteria on %s (not reverted)", len(check.MissingNumbers), t.Path)

	s.mu.Lock()
	if s.state.DoneMeansNoted == nil {
		s.state.DoneMeansNoted = map[string]bool{}
	}
	s.state.DoneMeansNoted[t.Path] = true
	s.saveLocked()
	s.mu.Unlock()
}

// formatMissingNumbers renders e.g. [2, 5] as "2, 5" for the informational
// note's text — a tiny helper purely to avoid Sprintf'ing a raw Go slice.
func formatMissingNumbers(nums []int) string {
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
}

// hashClaudeLine returns the sha256 hex hash of a routed @claude: line's
// trimmed text — its dedup key in State.RoutedClaudeLines.
func hashClaudeLine(text string) string {
	return sha256Hex(text)
}

// sha256Hex returns the sha256 hex digest of s.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// diffClaudeLinesLocked computes which @claude: lines in details are newly
// routable for taskPath and returns them (dispatch happens outside the
// lock, in the caller). Dedup is line-level, not hash-level: every tn note
// append changes the details hash, so hash-level dedup used to re-route
// every previously-routed line on every unrelated edit. TaskDetailsHash is
// kept as a cheap short-circuit for the common steady-state case (details
// genuinely unchanged, task already on line-level dedup) — extraction is
// skipped entirely then.
//
// detailsKnown MUST be false for any observation that can't guarantee
// details reflects the task's real, current body (see
// webhookTask.DetailsKnown) — LIVE INCIDENT: this function used to trust
// every details value at face value, including query-sourced/partial-
// update observations where details is simply absent from the source
// payload and therefore always "". Treating that as "content changed to
// nothing" wiped RoutedClaudeLines for the path; the next
// confirmed-real observation then saw "changed, nothing previously
// routed" and re-dispatched every already-handled @claude line — this
// happened repeatedly, roughly every 10 minutes, for 20+ tasks, each
// redispatch re-triggering the needs-input/review auto-flip.
func (s *Server) diffClaudeLinesLocked(taskPath, details string, detailsKnown bool) []string {
	sum := sha256.Sum256([]byte(details))
	hash := hex.EncodeToString(sum[:])

	s.mu.Lock()

	storedHash, hashKnown := s.state.TaskDetailsHash[taskPath]
	_, hasRoutedSet := s.state.RoutedClaudeLines[taskPath]

	if hashKnown && storedHash == hash && hasRoutedSet {
		// Steady state: details haven't changed since we last looked, and
		// this task is already on line-level dedup. Nothing to do.
		s.mu.Unlock()
		return nil
	}

	if details == "" && !detailsKnown && (hashKnown || hasRoutedSet) {
		// An unconfirmed-empty observation of a task we already have
		// state for carries NO information about its actual @claude
		// lines — skip entirely, touching neither ledger, rather than
		// let it look like "content changed to nothing" and wipe what we
		// already knew. A task with no prior state at all (first-ever
		// observation happening to be unconfirmed-empty) is safe to fall
		// through: there's nothing to wipe, and the next confirmed-real
		// observation will correctly detect the hash change and extract
		// fresh.
		s.mu.Unlock()
		return nil
	}

	lines := extractClaudeDirectives(details)

	if hashKnown && storedHash == hash && !hasRoutedSet {
		// Migration: this task's hash already existed from before
		// line-level dedup was introduced (RoutedClaudeLines didn't exist
		// yet). Seed its current lines as already-routed WITHOUT
		// dispatching them, so the first pass after upgrading doesn't fire
		// every pre-existing line at once.
		hashes := make([]string, 0, len(lines))
		for _, line := range lines {
			hashes = append(hashes, hashClaudeLine(line))
		}
		s.state.RoutedClaudeLines[taskPath] = hashes
		s.saveLocked()
		s.triggerRenders()
		s.mu.Unlock()
		return nil
	}

	// Details are new or changed: route only lines whose hash isn't
	// already recorded. Rebuilding the stored set from exactly today's
	// lines (rather than merging into the old set) naturally drops hashes
	// for lines no longer present — if the user deletes then re-adds an
	// identical line, it can be asked again.
	existing := make(map[string]bool, len(s.state.RoutedClaudeLines[taskPath]))
	for _, h := range s.state.RoutedClaudeLines[taskPath] {
		existing[h] = true
	}

	var toRoute []string
	newHashes := make([]string, 0, len(lines))
	for _, line := range lines {
		h := hashClaudeLine(line)
		newHashes = append(newHashes, h)
		if !existing[h] {
			toRoute = append(toRoute, line)
		}
	}

	s.state.RoutedClaudeLines[taskPath] = newHashes
	s.state.TaskDetailsHash[taskPath] = hash
	s.saveLocked()
	s.triggerRenders()
	s.mu.Unlock()

	return toRoute
}

// --- Webhook self-registration ---

// ensureWebhookRegistered retries registering our webhook URL with TaskNotes
// on start and every 60s until it succeeds. TaskNotes being unreachable is
// expected and never fatal.
func (s *Server) ensureWebhookRegistered(client *Client, selfURL string) {
	for {
		if s.tryRegisterWebhook(client, selfURL) {
			return
		}
		time.Sleep(60 * time.Second)
	}
}

// requiredWebhookEvents is what our webhook subscription must carry.
// task.completed is NOT covered by task.updated: TaskNotes fires them as
// mutually exclusive alternatives (a status transition INTO a completed
// status fires task.completed ONLY, per the plugin's own
// isCompletedStatus(old)!=isCompletedStatus(new) branch) — confirmed by
// reading the installed plugin's main.js directly, not inferred. Without
// this event, every transition into "done" was invisible to the live
// webhook path and relied entirely on the reconciler's next pass (up to
// 10min) to notice — not a flaky drop, a deterministic, 100%-reproducible
// gap for that one direction.
var requiredWebhookEvents = []string{"task.created", "task.updated", "task.completed"}

func (s *Server) tryRegisterWebhook(client *Client, selfURL string) bool {
	body, err := client.ListWebhooks()
	if err != nil {
		if !s.webhookWarnedOnce {
			log.Printf("serve: TaskNotes API not reachable for webhook self-registration, will keep retrying: %v", err)
			s.webhookWarnedOnce = true
		}
		return false
	}

	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return false
	}
	for _, entry := range extractWebhookEntries(env.Data) {
		if entry.URL != selfURL {
			continue
		}
		if hasAllEvents(entry.Events, requiredWebhookEvents) {
			return true
		}
		// Registered, but with a stale/incomplete event list (e.g. an
		// older daemon version registered before task.completed was
		// added here) — TaskNotes has no update endpoint, so replace it:
		// delete, then fall through to create the correct one below.
		if entry.ID != "" {
			if err := client.DeleteWebhook(entry.ID); err != nil {
				log.Printf("serve: failed to delete stale webhook %s (id=%s) before re-registering: %v", selfURL, entry.ID, err)
				return false
			}
			log.Printf("serve: deleted stale webhook %s (id=%s, events=%v) — missing required events, re-registering", selfURL, entry.ID, entry.Events)
		}
		break
	}

	if err := client.CreateWebhook(selfURL, requiredWebhookEvents); err != nil {
		if !s.webhookWarnedOnce {
			log.Printf("serve: failed to register webhook: %v", err)
			s.webhookWarnedOnce = true
		}
		return false
	}
	log.Printf("serve: registered webhook %s (events=%v)", selfURL, requiredWebhookEvents)
	return true
}

// hasAllEvents reports whether have contains every entry in want.
func hasAllEvents(have, want []string) bool {
	set := make(map[string]bool, len(have))
	for _, e := range have {
		set[e] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

// --- Due-task scanner ---

const (
	dueScannerInterval     = 10 * time.Minute
	dueScannerInitialDelay = 30 * time.Second
)

// buildDueTasksQuery builds the FilterQuery for the due-task scanner: open,
// non-archived, tagged "claude", scheduled on or before today, not blocked
// by an incomplete dependency (dependencies.isBlocked is-not-checked —
// verified live against the real API, see buildListQuery), not parked in a
// human-attention status (triage/needs-input/review — see
// humanAttentionStatuses), and not already in-progress — that last one
// matters just as much as excluding done: without it, a recurring task
// picked up by an agent would get re-assigned on its next scheduled
// occurrence while still being worked (see assignableStatuses, the same
// fix applied to webhook assignment routing).
func buildDueTasksQuery(today string) FilterGroup {
	return FilterGroup{
		Type:        "group",
		ID:          "root",
		Conjunction: "and",
		Children: []FilterCondition{
			{Type: "condition", ID: "archived", Property: "archived", Operator: "is-not-checked"},
			{Type: "condition", ID: "completed", Property: "status.isCompleted", Operator: "is-not-checked"},
			{Type: "condition", ID: "not-triage", Property: "status", Operator: "is-not", Value: "triage"},
			{Type: "condition", ID: "not-needs-input", Property: "status", Operator: "is-not", Value: "needs-input"},
			{Type: "condition", ID: "not-review", Property: "status", Operator: "is-not", Value: "review"},
			{Type: "condition", ID: "not-in-progress", Property: "status", Operator: "is-not", Value: "in-progress"},
			{Type: "condition", ID: "not-blocked", Property: "dependencies.isBlocked", Operator: "is-not-checked"},
			{Type: "condition", ID: "tag-claude", Property: "tags", Operator: "contains", Value: "claude"},
			{Type: "condition", ID: "scheduled", Property: "scheduled", Operator: "is-on-or-before", Value: today},
		},
		SortKey:       "due",
		SortDirection: "asc",
		GroupKey:      "none",
	}
}

// unblockPassTaskCap bounds per-pass processing, same convention as the
// other reconciler passes.
const unblockPassTaskCap = 50

// buildUnblockedDependentsQuery builds the FilterQuery for the unblock
// pass, triggered when a task completes: open (non-archived, not
// status-completed), claude-tagged, has at least one blockedBy entry, and
// TaskNotes itself now considers it startable (dependencies.isBlocked
// is-not-checked) — i.e. a dependency-bearing task whose blocker(s) just
// cleared.
func buildUnblockedDependentsQuery() FilterGroup {
	return FilterGroup{
		Type:        "group",
		ID:          "root",
		Conjunction: "and",
		Children: []FilterCondition{
			{Type: "condition", ID: "archived", Property: "archived", Operator: "is-not-checked"},
			{Type: "condition", ID: "completed", Property: "status.isCompleted", Operator: "is-not-checked"},
			{Type: "condition", ID: "tag-claude", Property: "tags", Operator: "contains", Value: "claude"},
			{Type: "condition", ID: "has-blockers", Property: "blockedBy", Operator: "is-not-empty"},
			{Type: "condition", ID: "not-blocked", Property: "dependencies.isBlocked", Operator: "is-not-checked"},
		},
		SortKey:       "due",
		SortDirection: "asc",
		GroupKey:      "none",
	}
}

// runUnblockPass queries for claude-tagged, dependency-bearing tasks that
// are now startable and routes an assignment for each one that isn't
// already parked for the user, already in-progress, or already has a
// pending assignment — making "finish A → auto-assign B" work without
// waiting for the 10-minute due-task scanner to notice. Tolerates API
// failure: logs and returns, no retry (nothing here is otherwise lost —
// the due-task scanner and webhook-miss reconciler both independently
// cover the same tasks on their own cadence).
func (s *Server) runUnblockPass(completedTitle string) {
	if s.tnClient == nil {
		return
	}
	tasks, err := s.tnClient.QueryTasksRaw(buildUnblockedDependentsQuery())
	if err != nil {
		log.Printf("serve: unblock pass: TaskNotes API query failed: %v", err)
		return
	}
	if len(tasks) > unblockPassTaskCap {
		log.Printf("serve: unblock pass: %d candidate tasks exceeds cap %d, processing the first %d", len(tasks), unblockPassTaskCap, unblockPassTaskCap)
		tasks = tasks[:unblockPassTaskCap]
	}
	for _, task := range tasks {
		s.routeUnblockedDependent(task, completedTitle)
	}
}

// routeUnblockedDependent routes one now-startable dependent task, unless
// it's parked in a human-attention status, already in-progress, or already
// has a pending ("Task assigned:") message.
func (s *Server) routeUnblockedDependent(task Task, completedTitle string) {
	slug := routingSlugForTask(task)
	if slug == "" {
		return
	}

	s.mu.Lock()
	// AssignedTasks additionally covers a multi-blocker task: if two
	// blockers complete separately, two runUnblockPass calls can both find
	// this same still-open, still-unblocked task — see markAssignedLocked.
	_, alreadyAssignedThisEpisode := s.state.AssignedTasks[task.Path]
	skip := humanAttentionStatuses[task.Status] || task.Status == "in-progress" || alreadyAssignedThisEpisode || s.hasPendingAssignmentLocked(task.Path)
	if !skip {
		s.markAssignedLocked(task.Path, time.Now().Format("2006-01-02"))
		s.saveLocked()
	}
	s.mu.Unlock()
	if skip {
		return
	}

	text := fmt.Sprintf("Task assigned: %s (%s) [unblocked by %s]", task.Title, task.Path, completedTitle)
	// A task reaching this pass with role owner has, by construction, just
	// had its last blockedBy member complete (that's what unblocked it) —
	// call that out explicitly, since routeUnblockedDependent is the ONE
	// place this specific "all members done" moment is known; the owner
	// suffix from appendIntegrationBranchSuffix alone doesn't say WHY it's
	// being assigned now.
	if integrationRoleFor(task) == "owner" {
		text = "All members complete — " + text
	}
	s.dispatchMessage(sendRequest{
		Project:  slug,
		TaskPath: task.Path,
		Text:     appendIntegrationBranchSuffix(text, integrationBranchFor(task), integrationRoleFor(task)),
	})
}

// startDueTaskScanner launches a background goroutine running two passes on
// the same cadence (once ~30s after startup, then every 10 minutes):
//  1. scanDueTasksRemote — open, claude-tagged tasks scheduled on or before
//     today, routing an assignment for each one not already routed today
//     (recurrence/scheduled-date rollover never touch the note file, so
//     webhooks don't fire when a new occurrence comes due).
//  2. reconcileWebhookMissesRemote — recently-modified tasks, re-run through
//     the exact same routing pipeline webhooks use (catches deliveries
//     TaskNotes fired while the daemon was mid-restart or otherwise missed;
//     TaskNotes doesn't retry webhook deliveries).
//
// TaskNotes being unreachable is logged once per outage and retried on the
// next tick, never fatal. Disabled entirely when TN_NO_SCANNER=1 (tests and
// smoke runs that start the real daemon should set this).
func (s *Server) startDueTaskScanner(client *Client) {
	if os.Getenv("TN_NO_SCANNER") == "1" {
		return
	}
	go func() {
		time.Sleep(dueScannerInitialDelay)
		s.scanDueTasksRemote(client)
		s.reconcileWebhookMissesRemote(client)

		ticker := time.NewTicker(dueScannerInterval)
		defer ticker.Stop()
		for range ticker.C {
			s.scanDueTasksRemote(client)
			s.reconcileWebhookMissesRemote(client)
		}
	}()
}

// scanDueTasksRemote fetches due tasks from the TaskNotes API and runs one
// scanner pass. A fetch failure is logged once per outage (reset on the
// next success, so a later outage logs again) and otherwise ignored.
func (s *Server) scanDueTasksRemote(client *Client) {
	today := time.Now().Format("2006-01-02")
	tasks, err := client.QueryTasks(buildDueTasksQuery(today))
	if err != nil {
		if !s.dueScannerWarnedOnce {
			log.Printf("serve: due-task scanner: TaskNotes API not reachable, will keep retrying: %v", err)
			s.dueScannerWarnedOnce = true
		}
		return
	}
	s.dueScannerWarnedOnce = false
	s.scanDueTasksOnce(tasks, today)
}

// scanDueTasksOnce is one scanner pass over pre-fetched tasks, factored out
// so tests can exercise it without HTTP.
func (s *Server) scanDueTasksOnce(tasks []Task, today string) {
	for _, task := range tasks {
		s.processDueTask(task, today)
	}
}

// processDueTask resolves a due task's routing slug and, unless it should
// be skipped, records the occurrence and dispatches an assignment. Message
// dispatch always happens outside s.mu (dispatchMessage locks internally).
func (s *Server) processDueTask(task Task, today string) {
	slug := routingSlugForTask(task)

	s.mu.Lock()
	if slug == "" {
		s.mu.Unlock()
		return
	}
	// Skip unroutable projects entirely (unlike webhook routing, which
	// always queues to orchestrator-<slug> with a warning): a scanner pass
	// runs unattended every 10 minutes, so silently accumulating assignments
	// for a typo'd or unconfigured project would just strand work nobody is
	// watching.
	hasAliveAcceptingAgent := s.aliveAcceptingAgentForProjectLocked(slug) != nil
	cfg := s.projectConfigFor(slug)
	if !cfg.AutoSpawn && !hasAliveAcceptingAgent {
		s.mu.Unlock()
		return
	}
	// A new occurrence (today's AssignedOccurrences entry doesn't match)
	// starts a NEW episode — clear any stale AssignedTasks marker left
	// over from a PREVIOUS occurrence first, so a recurring task whose
	// due date just rolled over isn't blocked by yesterday's marker (it
	// can sit in "open" across day boundaries without ever leaving
	// assignableStatuses, so nothing else would have cleared it).
	if s.state.AssignedOccurrences[task.Path] != today {
		delete(s.state.AssignedTasks, task.Path)
	}
	if s.state.AssignedOccurrences[task.Path] == today {
		s.mu.Unlock()
		return
	}
	// AssignedTasks is ALSO checked here now, not just AssignedOccurrences:
	// this is the actual fix for the live leak. Before it, a task assigned
	// via the webhook/reconciler path got an AssignedTasks marker but no
	// AssignedOccurrences entry (markAssignedLocked only wrote one ledger
	// per call site), so the very next scanner pass — checking only
	// AssignedOccurrences — saw no record for today and re-assigned it
	// (confirmed live: three tasks each assigned twice, second assignment
	// for all three at the same scanner-pass timestamp, neither task
	// modified in between). markAssignedLocked now writes both ledgers
	// together from every call site, so this check and the occurrence
	// check above are consistent going forward; this direct check is
	// belt-and-suspenders for any state left over from before this fix.
	if _, alreadyAssignedThisEpisode := s.state.AssignedTasks[task.Path]; alreadyAssignedThisEpisode {
		s.mu.Unlock()
		return
	}
	if s.hasPendingAssignmentLocked(task.Path) {
		s.mu.Unlock()
		return
	}
	s.markAssignedLocked(task.Path, today)
	s.saveLocked()
	s.mu.Unlock()

	s.dispatchMessage(sendRequest{
		Project:  slug,
		TaskPath: task.Path,
		Text:     appendIntegrationBranchSuffix(fmt.Sprintf("Task assigned: %s (%s) [scheduled %s]", task.Title, task.Path, task.Scheduled), integrationBranchFor(task), integrationRoleFor(task)),
	})
}

// --- MR watcher ---

const (
	// mrWatchInterval is how often the MR watcher checks tracked tasks.
	mrWatchInterval = 5 * time.Minute
	// mrWatchInitialDelay is the eager first-run delay after daemon
	// startup, mirroring the other background passes' eager-first-run
	// convention (a fresh daemon shouldn't wait a full interval to catch
	// an MR that merged/closed while it was down).
	mrWatchInitialDelay = 45 * time.Second
)

// buildMRWatchQuery builds the FilterQuery for the MR watcher: non-archived
// tasks whose status is review, in-progress, or open. customProperties.mr
// isn't filtered server-side — the query just narrows to statuses the
// watcher ever acts on, and the mr-is-set check happens client-side per
// task after fetch (same pattern as buildNeedsActionQuery's status-OR
// shape, since FilterQuery has no "in" operator).
func buildMRWatchQuery() filterNode {
	statusOr := filterNode{
		Type: "group", ID: "status-or", Conjunction: "or",
		Children: []filterNode{
			{Type: "condition", ID: "s-review", Property: "status", Operator: "is", Value: "review"},
			{Type: "condition", ID: "s-in-progress", Property: "status", Operator: "is", Value: "in-progress"},
			{Type: "condition", ID: "s-open", Property: "status", Operator: "is", Value: "open"},
		},
	}
	return filterNode{
		Type: "group", ID: "root", Conjunction: "and",
		Children: []filterNode{
			{Type: "condition", ID: "archived", Property: "archived", Operator: "is-not-checked"},
			statusOr,
		},
		SortKey:       "dateModified",
		SortDirection: "desc",
	}
}

// mrStateFunc resolves a GitLab MR URL to its current state
// ("opened"/"merged"/"closed"), injectable so tests never exec glab.
type mrStateFunc func(mrURL string) (string, error)

// parseMRURL extracts glab's "-R" project path (group/subgroup/.../project
// — nested groups are common and must be preserved whole) and the merge
// request iid from a GitLab MR URL of the form
// https://<host>/<group/.../project>/-/merge_requests/<iid>.
func parseMRURL(rawURL string) (project, iid string, ok bool) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", false
	}
	const marker = "/-/merge_requests/"
	idx := strings.Index(u.Path, marker)
	if idx <= 0 {
		return "", "", false
	}
	project = strings.Trim(u.Path[:idx], "/")
	iid = strings.Trim(u.Path[idx+len(marker):], "/")
	if project == "" || iid == "" {
		return "", "", false
	}
	for _, r := range iid {
		if r < '0' || r > '9' {
			return "", "", false
		}
	}
	return project, iid, true
}

// resolveGlabBin finds glab by absolute path — under launchd the daemon's
// PATH lacks the Homebrew dirs, so a bare "glab" is not found (same
// reasoning as resolveTmuxBin above).
func resolveGlabBin() string {
	for _, p := range []string{"/opt/homebrew/bin/glab", "/usr/local/bin/glab", "/usr/bin/glab"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "glab"
}

// glabMRState is mrStateFunc's real implementation: parses the MR URL,
// then shells out to the authenticated glab CLI. HOME is set explicitly
// (not just inherited) since under launchd the daemon's ambient
// environment can be stripped down, same class of gotcha as tmux's
// absolute-path resolution elsewhere in this file.
func glabMRState(mrURL string) (string, error) {
	project, iid, ok := parseMRURL(mrURL)
	if !ok {
		return "", fmt.Errorf("unparseable MR URL: %s", mrURL)
	}
	cmd := exec.Command(resolveGlabBin(), "mr", "view", iid, "-R", project, "-F", "json")
	cmd.Env = os.Environ()
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Env = append(cmd.Env, "HOME="+home)
	}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	var resp struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", err
	}
	if resp.State == "" {
		return "", fmt.Errorf("glab returned no state for %s", mrURL)
	}
	return resp.State, nil
}

// startMRWatcher launches a background goroutine that checks GitLab MR
// state for review/in-progress/open tasks with a customProperties.mr URL
// set, transitioning the task's status on an OBSERVED state change (see
// checkMRStateOnce). Disabled when TN_NO_MRWATCH=1 (tests and smoke runs
// that start the real daemon should set this).
func (s *Server) startMRWatcher(client *Client) {
	if os.Getenv("TN_NO_MRWATCH") == "1" {
		return
	}
	go func() {
		time.Sleep(mrWatchInitialDelay)
		s.checkMRStatesOnce(client, glabMRState)

		ticker := time.NewTicker(mrWatchInterval)
		defer ticker.Stop()
		for range ticker.C {
			s.checkMRStatesOnce(client, glabMRState)
		}
	}()
}

// checkMRStatesOnce is one MR-watcher pass, factored out of the ticker loop
// so it's directly testable with an injected mrStateFunc (tests never exec
// glab) and TaskNotes client (a fake httptest server).
func (s *Server) checkMRStatesOnce(client *Client, mrState mrStateFunc) {
	if client == nil {
		return
	}
	tasks, err := client.QueryTasksRaw(buildMRWatchQuery())
	if err != nil {
		log.Printf("serve: mr watcher: TaskNotes API query failed: %v", err)
		return
	}
	for _, t := range tasks {
		// Self-healing re-arm (see observeTaskStatusLocked): the MR watcher
		// observes review/in-progress/open tasks every 5min, one more
		// vantage point from which a task's AssignedTasks marker should
		// clear once it's no longer open.
		s.mu.Lock()
		if s.observeTaskStatusLocked(t.Path, t.Status) {
			s.saveLocked()
		}
		s.mu.Unlock()

		mrURL := t.CustomProperties["mr"]
		if mrURL == "" {
			continue
		}
		s.checkTaskMRState(client, mrState, t, mrURL)
	}
}

// checkTaskMRState resolves the current MR state for one task and, if it
// represents a genuine change (or the one legitimate first-observation
// catch-up — see below), transitions the task accordingly, then records
// the observed state either way. A failed lookup (unparseable URL or glab
// error) never mutates MRStates — a later successful pass just picks up
// wherever the last confirmed state left off — and is logged once per task
// per outage via mrWatchWarnedOnce.
func (s *Server) checkTaskMRState(client *Client, mrState mrStateFunc, t Task, mrURL string) {
	state, err := mrState(mrURL)
	if err != nil {
		s.mu.Lock()
		alreadyWarned := s.mrWatchWarnedOnce[t.Path]
		s.mrWatchWarnedOnce[t.Path] = true
		s.mu.Unlock()
		if !alreadyWarned {
			log.Printf("serve: mr watcher: failed to resolve MR state for %s (%s): %v", t.Path, mrURL, err)
		}
		return
	}

	s.mu.Lock()
	prev, seen := s.state.MRStates[t.Path]
	s.mrWatchWarnedOnce[t.Path] = false
	s.mu.Unlock()

	// A task's first-ever observation seeds MRStates without transitioning
	// (like the other migration-style dedup maps in this file) — EXCEPT
	// merged+review, which is a legitimate catch-up: the user merged the MR
	// before the watcher ever saw this task.
	transition := (seen && state != prev) || (!seen && state == "merged" && t.Status == "review")
	if transition {
		switch state {
		case "merged":
			s.transitionMRMerged(client, t, mrURL)
		case "closed":
			s.transitionMRClosed(client, t, mrURL)
			// "opened": nothing to do beyond recording it below.
		}
	}

	s.mu.Lock()
	s.state.MRStates[t.Path] = state
	s.saveLocked()
	s.mu.Unlock()
}

// bridgeTransitionTask fetches path, appends a "bridge"-attributed note
// (same format `tn note`'s AddNote produces), and PUTs both the updated
// details AND newStatus in one partial update — a single round trip, and
// no window where the status changed but the note hadn't landed (or vice
// versa).
func bridgeTransitionTask(client *Client, path, newStatus, noteText string) (Task, error) {
	task, err := client.GetTask(path)
	if err != nil {
		return Task{}, err
	}
	entry := formatHistoryEntry("bridge", noteText, time.Now())
	newDetails := applyNoteBodyEdit(task.Details, func(nb *noteBody) {
		nb.History = append([]string{entry}, nb.History...)
	})
	return client.UpdateTask(path, map[string]any{"details": newDetails, "status": newStatus})
}

// transitionMRMerged auto-completes t when its MR merges: only review or
// in-progress tasks are actually transitioned (an open task whose MR
// happens to be merged was never assigned/being-worked in the first place
// — its state is still recorded by the caller, just nothing to transition
// here). Invokes runUnblockPass directly afterward since we already know
// the status change succeeded, rather than waiting for a webhook/reconciler
// pass to notice it independently.
func (s *Server) transitionMRMerged(client *Client, t Task, mrURL string) {
	if t.Status != "review" && t.Status != "in-progress" {
		return
	}
	note := fmt.Sprintf("MR merged (%s) — auto-transitioned to done", mrURL)
	if _, err := bridgeTransitionTask(client, t.Path, "done", note); err != nil {
		log.Printf("serve: mr watcher: failed to auto-done %s after MR merge: %v", t.Path, err)
		return
	}
	log.Printf("serve: task %s auto-done (MR merged)", t.Path)

	s.mu.Lock()
	s.appendActivityLocked("bridge", fmt.Sprintf("Auto-done %s (MR merged: %s)", t.Path, mrURL), t.Path)
	s.saveLocked()
	s.mu.Unlock()
	s.triggerRenders()

	s.runUnblockPass(t.Title)
}

// transitionMRClosed handles an MR closing without merging: only a review
// task gets reopened to in-progress with a note (the note literally says
// "reopened", which would be misleading for a task that was never in
// review) — an in-progress or open task's status is left alone. Either
// way, the task's owner (or fallback accepting agent, or the logical
// queue — see resolveTargetLocked) gets an informational message, since a
// closed-unmerged MR always needs a human or the owning agent to look at
// it regardless of the task's current status.
func (s *Server) transitionMRClosed(client *Client, t Task, mrURL string) {
	if t.Status == "review" {
		note := fmt.Sprintf("MR closed without merge (%s) — reopened", mrURL)
		if _, err := bridgeTransitionTask(client, t.Path, "in-progress", note); err != nil {
			log.Printf("serve: mr watcher: failed to reopen %s after MR close: %v", t.Path, err)
		} else {
			log.Printf("serve: task %s reopened to in-progress (MR closed unmerged)", t.Path)
			s.mu.Lock()
			s.appendActivityLocked("bridge", fmt.Sprintf("Reopened %s to in-progress (MR closed unmerged: %s)", t.Path, mrURL), t.Path)
			s.saveLocked()
			s.mu.Unlock()
			s.triggerRenders()
		}
	}

	project := routingSlugForTask(t)
	if project == "" {
		return
	}
	s.dispatchMessage(sendRequest{
		Project:  project,
		TaskPath: t.Path,
		Text:     fmt.Sprintf("MR closed unmerged for %s (%s) — investigate and reopen or supersede.", t.Title, t.Path),
	})
}

// --- Webhook-miss reconciler ---

const (
	// webhookMissWatermarkBuffer is subtracted from the last successful
	// pass's start time to compute the query window's lower bound, so a
	// task modified in the few minutes right before that pass started
	// (possibly not yet reflected in dateModified when queried) still gets
	// picked up.
	webhookMissWatermarkBuffer = 15 * time.Minute
	// webhookMissFreshStateLookback is the fallback window for the very
	// first pass on fresh state (no prior successful pass to build a
	// watermark from).
	webhookMissFreshStateLookback = 24 * time.Hour
	// webhookMissTaskCap bounds per-pass GET /api/tasks/:id fetches. Raised
	// from 50 to 200 after chronic overflow (71-78 candidates per pass
	// observed live, repeatedly, not a one-off burst): with ascending
	// (oldest-first) sort, a truncated pass ALWAYS drops the newest
	// candidates, and since buildRecentlyModifiedQuery's "since" filter is
	// DAY-granularity only (sinceDate below is a bare "YYYY-MM-DD", not a
	// full timestamp — confirmed by computeReconcileSince/its own tests),
	// the query returns the SAME full same-day candidate set on every pass
	// regardless of how far the watermark has crept within that day. A
	// watermark-based "resume where we left off" rotation therefore
	// wouldn't actually shrink the query results intra-day — it would need
	// either true API pagination (not available through this FilterQuery
	// shape: no offset/limit field exists) or a separately-persisted
	// client-side slice cursor, meaningfully more state and edge-case
	// surface than this fix warrants. Raising the cap is the simpler,
	// lower-risk fix: TaskNotes returns the full candidate list in ONE
	// query response regardless of cap (no pagination cost either way),
	// and each additional GET /api/tasks/:id is cheap. 200 gives roughly
	// 2.5x headroom over the worst observed volume; the existing
	// truncation log line (still present below) is the "pass incomplete"
	// signal if real volume ever exceeds this too.
	webhookMissTaskCap = 200
)

// buildRecentlyModifiedQuery builds the FilterQuery for the webhook-miss
// reconciler: open (non-archived) tasks modified on or after sinceDate.
// Deliberately has no status/tag filter — @claude: line routing applies
// regardless of status or tags, and the shared routing pipeline
// (routeTaskNotification) applies the assignment-specific tag/status guards
// itself.
//
// SortDirection is ASCENDING (oldest-modified first) — deliberately, not
// the more obvious "newest first": reconcileWebhookMissesRemote truncates
// results at webhookMissTaskCap per pass, and a high-volume window (a real
// burst produced 71 candidates against a 50 cap) means SOME candidates get
// dropped every pass regardless of sort order. With newest-first, the
// dropped tail is always the OLDEST candidates — and since the query's
// window only ever grows forward from the advancing watermark, an oldest
// candidate that's truncated this pass has no guarantee of surviving into
// a later one before it ages out of `dateModified is-on-or-after
// sinceDate` entirely, i.e. permanent starvation: a task whose webhook
// delivery was genuinely missed could go unprocessed forever. With
// oldest-first, the dropped tail is the NEWEST candidates instead — and a
// newer candidate has more runway before the watermark catches up to it,
// so it reliably gets processed on a LATER pass instead. Net effect:
// truncation under sustained high volume delays freshly-modified tasks by
// a pass or two (tolerable — the live webhook already routes them
// promptly; the reconciler is only the safety net for misses) rather than
// silently dropping old ones that may have never been routed at all.
func buildRecentlyModifiedQuery(sinceDate string) FilterGroup {
	return FilterGroup{
		Type:        "group",
		ID:          "root",
		Conjunction: "and",
		Children: []FilterCondition{
			{Type: "condition", ID: "archived", Property: "archived", Operator: "is-not-checked"},
			{Type: "condition", ID: "modified", Property: "dateModified", Operator: "is-on-or-after", Value: sinceDate},
		},
		SortKey:       "dateModified",
		SortDirection: "asc",
		GroupKey:      "none",
	}
}

// computeReconcileSince applies the watermark model: from `watermark` minus
// a buffer, or — on fresh state (zero watermark) — from `now` minus a
// larger fallback lookback, so the very first pass after a fresh install
// (or a downtime long enough to have never completed a successful pass)
// still covers a reasonable window.
func computeReconcileSince(watermark, now time.Time) time.Time {
	if watermark.IsZero() {
		return now.Add(-webhookMissFreshStateLookback)
	}
	return watermark.Add(-webhookMissWatermarkBuffer)
}

// taskToWebhookTask converts a TaskNotes API Task (as returned by
// Client.GetTask) into the webhookTask shape the shared
// routeTaskNotification pipeline expects, so the reconciler and the webhook
// handler both funnel through the exact same routing logic.
func taskToWebhookTask(t Task) webhookTask {
	// A live fetch (GetTask/QueryTasksRaw) always carries a real
	// isBlocked, so this is always "known" evidence — never nil — for the
	// reconciler path.
	isBlocked := t.IsBlocked
	return webhookTask{
		Path:             t.Path,
		Title:            t.Title,
		Status:           t.Status,
		Tags:             t.Tags,
		Projects:         t.Projects,
		Details:          t.Details,
		CustomProperties: t.CustomProperties,
		BlockedBy:        t.BlockedBy,
		IsBlocked:        &isBlocked,
		// DetailsKnown is unconditionally true here: taskToWebhookTask's
		// one caller (the reconciler) always passes a GetTask result,
		// which always carries the task's real, current body — unlike
		// POST /api/tasks/query, which never returns details at all. If
		// this function is ever reused with a query-sourced Task, this
		// must become conditional — see webhookTask.DetailsKnown's doc.
		DetailsKnown: true,
	}
}

// reconcileWebhookMissesRemote is one webhook-miss reconciler pass: query
// TaskNotes for tasks modified since the watermark (see
// computeReconcileSince), fetch each candidate's full details individually
// (POST /api/tasks/query never includes details — confirmed against the
// live API; only GET /api/tasks/:id does), and route each through the same
// pipeline the webhook handler uses. This catches @claude: lines and
// assignments from webhook deliveries TaskNotes fired while the daemon was
// unavailable (e.g. mid self-restart) — TaskNotes does not retry failed
// deliveries. Reprocessing is safe and cheap even when nothing actually
// changed: routeTaskNotification's own line-level/hash dedup and
// assignment dedup make it a no-op.
//
// The watermark (State.LastReconciledAt) only advances when the query
// succeeded AND every candidate's details fetch succeeded — otherwise it's
// left unchanged so the next pass retries the same window.
func (s *Server) reconcileWebhookMissesRemote(client *Client) {
	passStart := time.Now()

	s.mu.Lock()
	watermark := s.state.LastReconciledAt
	s.mu.Unlock()

	since := computeReconcileSince(watermark, passStart)
	sinceDate := since.Format("2006-01-02")

	tasks, err := client.QueryTasksRaw(buildRecentlyModifiedQuery(sinceDate))
	if err != nil {
		if !s.webhookMissWarnedOnce {
			log.Printf("serve: webhook-miss reconciler: TaskNotes API not reachable, will keep retrying: %v", err)
			s.webhookMissWarnedOnce = true
		}
		return
	}
	s.webhookMissWarnedOnce = false

	if len(tasks) > webhookMissTaskCap {
		log.Printf("serve: webhook-miss reconciler: %d candidate tasks exceeds cap %d, processing the first %d", len(tasks), webhookMissTaskCap, webhookMissTaskCap)
		tasks = tasks[:webhookMissTaskCap]
	}

	allFetched := true
	for _, candidate := range tasks {
		full, err := client.GetTask(candidate.Path)
		if err != nil {
			log.Printf("serve: webhook-miss reconciler: failed to fetch details for %s: %v", candidate.Path, err)
			allFetched = false
			continue
		}
		s.routeTaskNotification(taskToWebhookTask(full))
	}

	if allFetched {
		s.mu.Lock()
		s.state.LastReconciledAt = passStart
		s.saveLocked()
		s.mu.Unlock()
	}
}

// repairPoisonedTaskCap bounds one repairPoisonedDedupState pass, same
// defensive-cap convention as the other background passes.
const repairPoisonedTaskCap = 200

// repairPoisonedDedupState runs ONCE at daemon startup: for every task
// path whose stored dedup state looks like a product of the empty-details
// poisoning bug fixed alongside this (see diffClaudeLinesLocked's
// detailsKnown guard) — taskDetailsHash == sha256("") (an unconfirmed-empty
// observation was trusted before the fix existed), OR a routedClaudeLines
// entry that's present but empty while the task's LIVE details actually
// contain at least one @claude line (the specific shape that primes a
// spurious re-dispatch the next time this task is genuinely observed,
// since a real fetch's hash can never match the poisoned one) — fetch the
// task's real current body and reseed BOTH ledgers from it via
// reseedDedupState, which NEVER dispatches (a repair is a seed, not
// a diff: don't turn a one-time cleanup into a fresh redelivery storm of
// its own). Bounded and logged like the other background passes;
// idempotent — a repaired path's stored hash then matches its live
// content, so a second run is a no-op for it, and a healthy (never
// poisoned) path never matches either candidate condition and is left
// untouched. Disabled when TN_NO_REPAIR=1 (tests and smoke runs that
// start the real daemon should set this, same convention as
// TN_NO_SCANNER/TN_NO_MRWATCH/TN_NO_STUCKCHECK).
func (s *Server) repairPoisonedDedupState(client *Client) {
	if client == nil || os.Getenv("TN_NO_REPAIR") == "1" {
		return
	}

	s.mu.Lock()
	emptyHash := sha256Hex("")
	seen := map[string]bool{}
	var candidates []string
	for path, hash := range s.state.TaskDetailsHash {
		if hash == emptyHash && !seen[path] {
			seen[path] = true
			candidates = append(candidates, path)
		}
	}
	for path, lines := range s.state.RoutedClaudeLines {
		if len(lines) == 0 && !seen[path] {
			seen[path] = true
			candidates = append(candidates, path)
		}
	}
	s.mu.Unlock()

	if len(candidates) == 0 {
		return
	}
	sort.Strings(candidates) // deterministic order for logging and tests
	if len(candidates) > repairPoisonedTaskCap {
		log.Printf("serve: dedup repair: %d candidate tasks exceeds cap %d, processing the first %d", len(candidates), repairPoisonedTaskCap, repairPoisonedTaskCap)
		candidates = candidates[:repairPoisonedTaskCap]
	}

	repaired := 0
	for _, path := range candidates {
		full, err := client.GetTask(path)
		if err != nil {
			log.Printf("serve: dedup repair: failed to fetch %s: %v", path, err)
			continue
		}
		if s.reseedDedupState(path, full.Details) {
			repaired++
		}
	}
	if repaired > 0 {
		log.Printf("serve: dedup repair: reseeded dedup state for %d task(s) poisoned by the empty-details bug", repaired)
	}
}

// deadOrUnobservedTaskOwnersLocked returns every distinct agent name that
// currently owns at least one task (per taskOwnerLocked's "latest acked
// 'Task assigned:' recipient" definition), split into two disjoint sets:
//
//   - dead: observed at least once THIS lifetime (seenSinceStart) and not
//     alive right now — directly actionable, no external check needed.
//   - unobserved: never observed THIS lifetime at all. Their fate is
//     genuinely unknown from in-memory state alone — a stale pre-restart
//     LastSeenAt says nothing (see checkOrphanedDeathsOnce's doc comment)
//     — so they are returned as full Agent records rather than resolved
//     here, for deadTaskOwners to check via tmux has-session, which must
//     NOT run under this lock (same "no external I/O while holding s.mu"
//     rule as every network call elsewhere in this file).
//
// Deduplicates by task path first (same pattern as ownedTaskPathsLocked)
// so a path with several historical assignment messages only contributes
// taskOwnerLocked's one current answer, not one call per message. Must be
// called with s.mu held.
func (s *Server) deadOrUnobservedTaskOwnersLocked(now time.Time) (dead []string, unobserved []Agent) {
	seenPath := map[string]bool{}
	deadSet := map[string]bool{}
	unobservedSet := map[string]bool{}
	for _, m := range s.state.Messages {
		if m.TaskPath == "" || m.Status != "acked" || !strings.HasPrefix(m.Text, "Task assigned:") {
			continue
		}
		if seenPath[m.TaskPath] {
			continue
		}
		seenPath[m.TaskPath] = true
		owner := s.taskOwnerLocked(m.TaskPath)
		if owner == "" || deadSet[owner] || unobservedSet[owner] {
			continue
		}
		a, ok := s.state.Agents[owner]
		if !ok {
			continue
		}
		if !s.seenSinceStart[owner] {
			unobservedSet[owner] = true
			continue
		}
		if !agentAliveAt(*a, now) {
			deadSet[owner] = true
		}
	}
	dead = make([]string, 0, len(deadSet))
	for name := range deadSet {
		dead = append(dead, name)
	}
	sort.Strings(dead) // deterministic order for logging and tests
	for name := range unobservedSet {
		if a, ok := s.state.Agents[name]; ok {
			unobserved = append(unobserved, *a)
		}
	}
	sort.Slice(unobserved, func(i, j int) bool { return unobserved[i].Name < unobserved[j].Name })
	return dead, unobserved
}

// deadTaskOwners is deadOrUnobservedTaskOwnersLocked's orchestrator: adds
// back the cross-restart recovery deadOrUnobservedTaskOwnersLocked alone
// cannot provide, via a POSITIVE, VERIFIABLE fact rather than an
// inference from silence — an agent's tmux session either currently
// exists or it does not, and that is knowable independent of whether
// this daemon process has personally heard from the agent yet.
//
// An unobserved agent is added to the result ONLY when hasSession reports
// tmuxSessionGone for its session — never on tmuxSessionExists (that is a
// HUNG agent, not a dead one; the stuck detector owns that case) and
// never on tmuxSessionUnknown (the check itself failed — no tmux binary,
// no server reachable, a permissions error — which is a fact about this
// machine right now, not evidence about any specific agent; collapsing
// "unknown" into "gone" here would reproduce the exact mass-false-
// positive shape the seenSinceStart gate exists to prevent, just gated by
// a different signal). An agent with no resolvable tmux session at all
// (tmuxSessionForAgent's second return false) is likewise left alone —
// there's nothing to check, so nothing to confirm.
//
// This restores sweepOrphanedOwnershipDebt's original purpose (recovering
// debt from an agent dead since before the current restart — see its own
// doc comment) while never inferring death from an agent simply not
// having reported in yet.
//
// Runs the tmux checks OUTSIDE s.mu — must not be called with it held.
func (s *Server) deadTaskOwners(now time.Time, hasSession tmuxSessionExistenceFunc) []string {
	s.mu.Lock()
	dead, unobserved := s.deadOrUnobservedTaskOwnersLocked(now)
	s.mu.Unlock()

	var confirmedGone, stillUnknown []string
	for _, a := range unobserved {
		session, ok := tmuxSessionForAgent(a)
		if !ok {
			continue
		}
		switch hasSession(session) {
		case tmuxSessionGone:
			dead = append(dead, a.Name)
			confirmedGone = append(confirmedGone, a.Name)
		case tmuxSessionUnknown:
			stillUnknown = append(stillUnknown, a.Name)
		case tmuxSessionExists:
			// Hung, not dead — leave it alone; the stuck detector owns this case.
		}
	}
	if len(confirmedGone) > 0 {
		sort.Strings(confirmedGone)
		log.Printf("serve: orphan check: %d never-observed agent(s) confirmed dead via tmux session absence (cross-restart recovery): %s", len(confirmedGone), strings.Join(confirmedGone, ", "))
	}
	if len(stillUnknown) > 0 {
		sort.Strings(stillUnknown)
		log.Printf("serve: orphan check: %d never-observed agent(s) have an unresolvable tmux check (no server/binary/permission — treated as unknown, not dead): %s", len(stillUnknown), strings.Join(stillUnknown, ", "))
	}
	sort.Strings(dead)
	return dead
}

// sweepOrphanedOwnershipDebtCap bounds one sweepOrphanedOwnershipDebt
// pass, same defensive-cap convention as repairPoisonedTaskCap.
const sweepOrphanedOwnershipDebtCap = 200

// sweepOrphanedOwnershipDebt runs ONCE at daemon startup, same placement
// and env-gating as repairPoisonedDedupState. Fixing the orphan-adoption
// hole forward (checkOrphanedTasks, triggered on each new death) only
// stops NEW debt from accumulating — it does nothing for tasks already
// stuck owned by a generation that died before this fix shipped (the
// live incident that prompted this: a week-long chain of dead
// generations left a project with 12 in-progress tasks against a single
// live agent, none of them actually being worked). This sweeps every
// currently-dead agent that owns at least one task (deadTaskOwners,
// combining agents observed-but-not-alive with agents never observed
// this lifetime whose tmux session is CONFIRMED gone — see its own doc
// comment) and resets each qualifying in-progress task via the SAME
// guarded resetOrphanedInProgressTasksForAgent the forward path uses — an
// already-adopted or live-worker-owned task is left alone here exactly
// as it would be on the forward path.
//
// Idempotent by construction, same reasoning as
// resetOrphanedInProgressTasksForAgent's own doc comment: a reset task's
// status is no longer "in-progress", so its owning agent drops out of
// deadTaskOwners's candidate list on the next run (nothing left to reset
// for it) — no separate one-shot marker needed, unlike
// repairPoisonedDedupState (whose candidates are keyed by a STORED hash
// that repair itself updates, not by a live TaskNotes-tracked status).
// Disabled when TN_NO_REPAIR=1, the same env var repairPoisonedDedupState
// uses — both are startup cleanup passes with the same "opt out for
// tests/smoke runs that start the real daemon" need.
func (s *Server) sweepOrphanedOwnershipDebt(client *Client, hasSession tmuxSessionExistenceFunc) {
	if client == nil || os.Getenv("TN_NO_REPAIR") == "1" {
		return
	}

	names := s.deadTaskOwners(time.Now(), hasSession)
	if len(names) == 0 {
		return
	}
	if !orphanSweepEnabled() {
		log.Printf("serve: startup orphan sweep DISABLED (set TN_ENABLE_ORPHAN_SWEEP=1 to enable) — would have processed %d dead task owner(s), left alone: %s", len(names), strings.Join(names, ", "))
		return
	}

	// Startup grace, same reasoning and same aliveWindow as
	// checkOrphanedDeathsOnce's own gate (see its doc comment): this is
	// the pass that runs INSIDE the most dangerous window of all, seconds
	// after every restart, when every agent's LastSeenAt still predates
	// the process and none of them have had a chance to poll yet. Only
	// paid when actually about to act (enabled AND there's a backlog) —
	// blocking here is fine either way since this already runs on its own
	// goroutine (see the startup call site). Blocking rather than
	// skipping outright, because unlike the periodic check this one is a
	// single startup shot: a skip would mean the real backlog this exists
	// to sweep (agents genuinely dead since BEFORE this restart) never
	// gets swept at all this lifecycle. Re-derive names after waiting —
	// the roster this was computed against may be stale by the time the
	// wait ends. (The tmux-confirmed-gone half of the list is already a
	// stable fact and doesn't actually need re-checking, but re-deriving
	// the whole list is simpler than splitting the recompute, and the
	// extra tmux calls are cheap and one-shot.)
	if wait := startupGraceRemaining(s.startedAt, time.Now()); wait > 0 {
		time.Sleep(wait)
		names = s.deadTaskOwners(time.Now(), hasSession)
		if len(names) == 0 {
			return
		}
	}

	if len(names) > sweepOrphanedOwnershipDebtCap {
		log.Printf("serve: orphan sweep: %d dead owner(s) exceeds cap %d, processing the first %d", len(names), sweepOrphanedOwnershipDebtCap, sweepOrphanedOwnershipDebtCap)
		names = names[:sweepOrphanedOwnershipDebtCap]
	}

	var totalReset, totalSkipped int
	for _, name := range names {
		reset, skipped := s.resetOrphanedInProgressTasksForAgent(name)
		totalReset += len(reset)
		totalSkipped += len(skipped)
		if len(reset) > 0 {
			s.mu.Lock()
			s.appendActivityLocked("bridge", fmt.Sprintf("Orphan sweep: reset %d in-progress task(s) from dead agent %s to open: %s", len(reset), name, strings.Join(reset, "; ")), "")
			s.saveLocked()
			s.mu.Unlock()
		}
	}
	if totalReset > 0 {
		s.triggerRenders()
	}
	log.Printf("serve: orphan sweep: inspected %d dead agent(s) with owned tasks, reset %d task(s) to open (%d left alone — already adopted or have a live worker)",
		len(names), totalReset, totalSkipped)
}

// deadOrUnobservedMessageRecipientsLocked is
// deadOrUnobservedTaskOwnersLocked's counterpart for stranded MESSAGES
// rather than owned tasks: every distinct agent name that's the recipient
// of at least one pending (queued/delivered) message, split the same way
// (confirmed dead this lifetime vs. never observed this lifetime — the
// latter is deadMessageRecipients's tmux-verified cross-restart candidate
// list). Deliberately NOT scoped to "Task assigned:"/acked messages like
// the task-owner version — a stranded message doesn't need to be about a
// task at all, and specifically must NOT require acked (an unacked,
// never-delivered message is exactly the stranded case this exists for).
// A recipient with no matching Agent record at all is excluded — that's
// sweepUnroutableMessagesOnce's job, not this one's: there's no liveness
// question to confirm for a name that was never a real agent.
func (s *Server) deadOrUnobservedMessageRecipientsLocked(now time.Time) (dead []string, unobserved []Agent) {
	deadSet := map[string]bool{}
	unobservedSet := map[string]bool{}
	for _, m := range s.state.Messages {
		if m.Status != "queued" && m.Status != "delivered" {
			continue
		}
		if deadSet[m.To] || unobservedSet[m.To] {
			continue
		}
		a, ok := s.state.Agents[m.To]
		if !ok {
			continue
		}
		if !s.seenSinceStart[m.To] {
			unobservedSet[m.To] = true
			continue
		}
		if !agentAliveAt(*a, now) {
			deadSet[m.To] = true
		}
	}
	dead = make([]string, 0, len(deadSet))
	for name := range deadSet {
		dead = append(dead, name)
	}
	sort.Strings(dead)
	for name := range unobservedSet {
		if a, ok := s.state.Agents[name]; ok {
			unobserved = append(unobserved, *a)
		}
	}
	sort.Slice(unobserved, func(i, j int) bool { return unobserved[i].Name < unobserved[j].Name })
	return dead, unobserved
}

// deadMessageRecipients mirrors deadTaskOwners exactly, for message
// recipients instead of task owners — same cross-restart recovery via a
// POSITIVE, VERIFIABLE fact (tmux session confirmed gone) rather than an
// inference from silence, for the identical reason: an agent dead since
// BEFORE this daemon restart, and never re-observed, is otherwise
// permanently invisible to reassignStrandedMessagesForDeadAgent (which
// only ever sees agents this process has watched transition from alive to
// dead). This is precisely the live incident's headline case:
// orchestrator-myapp-g17165, dead since 2026-08-25, holding 9 stranded
// messages including 7 "Task assigned:" directives — several daemon
// restarts old by the time this fix shipped, so the forward-only path
// alone would never have touched them. Runs the tmux checks OUTSIDE
// s.mu — must not be called with it held.
func (s *Server) deadMessageRecipients(now time.Time, hasSession tmuxSessionExistenceFunc) []string {
	s.mu.Lock()
	dead, unobserved := s.deadOrUnobservedMessageRecipientsLocked(now)
	s.mu.Unlock()

	for _, a := range unobserved {
		session, ok := tmuxSessionForAgent(a)
		if !ok {
			continue
		}
		if hasSession(session) == tmuxSessionGone {
			dead = append(dead, a.Name)
		}
	}
	sort.Strings(dead)
	return dead
}

// sweepStrandedMessagesAtStartup runs ONCE at daemon startup, same
// placement and reasoning as sweepOrphanedOwnershipDebt: fixing the
// stranded-message gap forward (reassignStrandedMessagesForDeadAgent,
// triggered on each new confirmed death) only stops NEW debt from
// accumulating — it does nothing for a generation that died before this
// fix shipped, or before this specific daemon restart. This sweeps every
// currently-dead-or-cross-restart-confirmed-dead agent with pending mail
// (deadMessageRecipients) through the same reconcileStrandedMessage
// archive-vs-reassign decision the forward path uses.
//
// Same TN_ENABLE_ORPHAN_SWEEP gate, same startup-grace wait/re-derive
// pattern, same reasoning throughout as sweepOrphanedOwnershipDebt — see
// its own doc comment for why each piece exists; not re-derived here.
func (s *Server) sweepStrandedMessagesAtStartup(client *Client, hasSession tmuxSessionExistenceFunc) {
	if client == nil || os.Getenv("TN_NO_REPAIR") == "1" {
		return
	}

	names := s.deadMessageRecipients(time.Now(), hasSession)
	if len(names) == 0 {
		return
	}
	if !orphanSweepEnabled() {
		log.Printf("serve: startup stranded-message sweep DISABLED (set TN_ENABLE_ORPHAN_SWEEP=1 to enable) — would have processed %d dead recipient(s), left alone: %s", len(names), strings.Join(names, ", "))
		return
	}

	if wait := startupGraceRemaining(s.startedAt, time.Now()); wait > 0 {
		time.Sleep(wait)
		names = s.deadMessageRecipients(time.Now(), hasSession)
		if len(names) == 0 {
			return
		}
	}

	s.mu.Lock()
	projectByName := map[string]string{}
	for _, name := range names {
		if a, ok := s.state.Agents[name]; ok {
			projectByName[name] = a.Project
		}
	}
	s.mu.Unlock()

	for _, name := range names {
		s.reassignStrandedMessagesForDeadAgent(name, projectByName[name])
	}
	log.Printf("serve: startup stranded-message sweep: inspected %d dead recipient(s) with pending mail", len(names))
}

// reseedDedupState recomputes taskDetailsHash/routedClaudeLines for path
// from details directly — NEVER dispatching anything, unlike
// diffClaudeLinesLocked (a seed, not a diff; mirrors the existing
// migration-seed branch's own no-dispatch contract). Acquires and releases
// s.mu itself — callers must NOT already hold it. Returns whether state
// actually changed, so callers only log a repair when one truly happened.
func (s *Server) reseedDedupState(path, details string) bool {
	sum := sha256.Sum256([]byte(details))
	hash := hex.EncodeToString(sum[:])

	lines := extractClaudeDirectives(details)
	hashes := make([]string, 0, len(lines))
	for _, line := range lines {
		hashes = append(hashes, hashClaudeLine(line))
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state.TaskDetailsHash[path] == hash {
		existing := s.state.RoutedClaudeLines[path]
		if slicesEqual(existing, hashes) {
			return false
		}
	}
	s.state.TaskDetailsHash[path] = hash
	s.state.RoutedClaudeLines[path] = hashes
	s.saveLocked()
	return true
}

// slicesEqual reports whether a and b contain the same strings in the
// same order.
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

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

// --- Debounce ---

const dashboardDebounceInterval = 2 * time.Second

// debouncer coalesces rapid Trigger calls into at most one call to fn per
// interval. It never busy-loops: the worker goroutine blocks on the trigger
// channel (or a bounded sleep) between renders.
type debouncer struct {
	interval time.Duration
	fn       func()
	trigger  chan struct{}
}

func newDebouncer(interval time.Duration, fn func()) *debouncer {
	return &debouncer{interval: interval, fn: fn, trigger: make(chan struct{}, 1)}
}

// Trigger requests a run of fn, coalescing with any already-pending request.
func (d *debouncer) Trigger() {
	select {
	case d.trigger <- struct{}{}:
	default:
	}
}

// run processes triggers for the lifetime of the process. Call it in its
// own goroutine.
func (d *debouncer) run() {
	var last time.Time
	for range d.trigger {
		if since := time.Since(last); since < d.interval {
			time.Sleep(d.interval - since)
		}
		d.drainPending()
		d.fn()
		last = time.Now()
	}
}

// drainPending discards any triggers that piled up while we were debouncing,
// since the upcoming render already covers them.
func (d *debouncer) drainPending() {
	for {
		select {
		case <-d.trigger:
		default:
			return
		}
	}
}

// --- Stuck session detection ---

const (
	// stuckCheckInitialDelay is how long after daemon startup the first
	// detector pass runs, ahead of the steady-state ticker — binary
	// self-restarts reset any ticker, so without an eager pass a prompt
	// appearing around a restart could wait up to stuckCheckInterval+.
	stuckCheckInitialDelay = 15 * time.Second
	// stuckCheckInterval is how often the detector inspects each alive
	// orchestrator's tmux pane.
	stuckCheckInterval = 2 * time.Minute
	// stuckExcerptLines bounds BOTH how much of the pane is scanned for a
	// live permission prompt (marker + numbered options must both appear
	// within this window — see detectLiveStuckPrompt) and how much is kept
	// as the excerpt shown on the agent's session page when stuck. Used to
	// be two separate windows (a 40-line marker scan, a 15-line excerpt) —
	// unified to one narrow window per the actual live incident: a marker
	// substring anywhere in a wide scan routinely matched an
	// already-answered prompt still sitting in scrollback (Claude prints
	// resolved prompts verbatim as history), which fired a fresh osascript
	// dialog for something the user had already dealt with.
	stuckExcerptLines = 15
	// stuckContextScanLines is a WIDER window used ONLY to build the
	// excerpt/dialog text actually DISPLAYED to a human, once a prompt is
	// already confirmed live via the narrow stuckExcerptLines window above
	// — never used for detection itself (detectLiveStuckPrompt/
	// parseStuckPromptOptions keep using stuckExcerptLines, unchanged).
	// Live incident (2026-08-26): a user asked "I would like to see the
	// command I'm allowing" — the daemon's own stuckExcerptLines=15 window
	// captured only the marker line and the numbered options, dropping the
	// "Bash command · from the X agent" header and the actual command text
	// sitting immediately above them in the pane. Verified live against
	// the real pane that reported the gap: a 40-line capture contains the
	// whole block. Widening ONLY the display window (not detection) is
	// safe in a way bumping stuckExcerptLines itself wouldn't be: this
	// only ever runs after detectLiveStuckPrompt has already confirmed a
	// live prompt on the narrow window, so it cannot reintroduce the
	// stale-scrollback false positive that narrowed detection from 40 to
	// 15 in the first place (see stuckExcerptLines's own doc comment) —
	// worst case it shows a little more leading context than the true
	// block, never less, which is the direction degrading is acceptable.
	stuckContextScanLines = 40
	// unackedBacklogThreshold is how long a (non-Informational) message can
	// sit delivered-but-unacked before it counts as backlog worth surfacing
	// — a DIFFERENT thing from stuck (an agent visibly wedged on a
	// permission prompt). Named UnackedBacklog, not "stalled" (its
	// 2026-08-25-and-earlier name): this measures inbox hygiene, not
	// whether the agent is actually doing anything — a demonstrably
	// working agent can carry this. See Message.Informational's doc
	// comment for the incident that forced the rename.
	unackedBacklogThreshold = 30 * time.Minute
	// stuckDialogRepromptInterval bounds how often the approval dialog is
	// re-shown for an agent that stays stuck on the SAME prompt (see
	// stuckSessionState.LastPromptedHash — a genuinely new prompt episode
	// always reprompts immediately, regardless of this interval).
	stuckDialogRepromptInterval = 15 * time.Minute
	// rateLimitScanLines is the window detectRateLimitWait scans — wider
	// than stuckExcerptLines (15) because a usage-limit block is several
	// lines tall and can be pushed down by whatever the session printed
	// just before it stopped (a failed teammate line, a partial tool
	// result), while the permission-prompt detector's own window stays
	// deliberately narrow (see stuckExcerptLines's doc comment: widening
	// THAT window is what reintroduced stale-scrollback false positives).
	// Widening is safe here because there is nothing to falsely
	// "re-answer": a usage-limit wait has no interactive response, the
	// dialog is purely informational and fires at most once per episode,
	// and the explicit "usage limit has reset" clear (checked BEFORE the
	// markers) is what ends an episode whose banner is still in
	// scrollback.
	rateLimitScanLines = 30
)

// stuckMarkers are substrings that, found in a captured tmux pane, indicate
// the session is blocked on an interactive permission prompt no one is
// watching (a headless session can never answer these itself).
var stuckMarkers = []string{
	"Do you want to proceed?",
	"requires confirmation",
	"Esc to cancel",
}

// rateLimitMarkers are lowercase substrings that, found in the last
// rateLimitScanLines of a captured pane, mean the session is parked on a
// claude.ai usage limit rather than working or blocked on a permission
// prompt. Matched case-insensitively; any one is enough. Verbatim shapes
// seen live:
//
//	You've hit your session limit · resets 2:30pm (Europe/Berlin)
//	Continuing automatically at 2:30pm · esc to cancel
//	⏺ Usage limit reached · continuing automatically at 2:30pm · esc or type to cancel
//	⏺ Teammate @doc-placement-research failed: You've hit your session limit · resets 2:30pm (Europe/Berlin)
//
// This is a SEPARATE condition from Stuck, not a variant of it: a
// permission prompt needs a human to answer it, a usage limit resolves
// itself at the reset time. Neither suppresses detection of the other.
var rateLimitMarkers = []string{
	"hit your session limit",
	"hit your limit",
	"usage limit reached",
	"continuing automatically at",
}

// rateLimitClearedMarker is the line Claude prints when the limit lifts
// ("Your claude.ai usage limit has reset. Continue the task you were
// working on..."), after which the session resumes on its own. Checked
// BEFORE rateLimitMarkers: the banner that announced the limit is still
// sitting in the same window at that moment, so marker-presence alone
// would keep the episode open for as long as it took to scroll away.
const rateLimitClearedMarker = "usage limit has reset"

// detectRateLimitWait reports whether the pane shows a live usage-limit
// wait, plus the RAW reset-time text as printed (e.g. "2:30pm
// (Europe/Berlin)" or "2:30pm") — deliberately never parsed into a
// time.Time: the pane prints a bare local clock time whose date, DST
// treatment and timezone label are all Claude's, and re-deriving an
// absolute instant from that would invent precision the source doesn't
// have. It is display text and an episode identity, nothing more, so ""
// (no reset text found) is a perfectly valid limited=true result.
func detectRateLimitWait(pane string) (resetsAt string, limited bool) {
	recent := lastNLines(pane, rateLimitScanLines)
	lower := strings.ToLower(recent)
	if strings.Contains(lower, rateLimitClearedMarker) {
		return "", false
	}
	found := false
	for _, m := range rateLimitMarkers {
		if strings.Contains(lower, m) {
			found = true
			break
		}
	}
	if !found {
		return "", false
	}
	return parseRateLimitResetText(recent, lower), true
}

// parseRateLimitResetText pulls the raw reset-time text out of a pane
// excerpt: whatever follows "resets " up to end-of-line or a " ·"
// separator, falling back to whatever follows "continuing automatically
// at ". recent and lower must be the same text, the latter lowercased —
// the match is case-insensitive but the returned slice comes from the
// original so its capitalization survives.
func parseRateLimitResetText(recent, lower string) string {
	for _, prefix := range []string{"resets ", "continuing automatically at "} {
		i := strings.Index(lower, prefix)
		if i < 0 {
			continue
		}
		rest := recent[i+len(prefix):]
		if j := strings.IndexByte(rest, '\n'); j >= 0 {
			rest = rest[:j]
		}
		if j := strings.Index(rest, " ·"); j >= 0 {
			rest = rest[:j]
		}
		if v := strings.TrimSpace(rest); v != "" {
			return v
		}
	}
	return ""
}

// captureFunc, dialogFunc, and sendKeysFunc are injectable so tests never
// touch tmux or osascript. dialogFunc shows a dialog with exactly the given
// buttons (built per-prompt by resolveStuckPromptMapping — permission
// prompts come in more than one shape, so the buttons vary) and returns the
// clicked button's label, or "" for a timeout/no selection — both are
// treated as "do nothing" by the caller, so dialogFunc only returns a
// non-nil error for an actual invocation failure.
type captureFunc func(session string) (string, error)
type dialogFunc func(title, text string, buttons []string) (string, error)
type sendKeysFunc func(session, keys string) error

// stuckSessionState is one orchestrator's live detection state. Entirely
// in-memory — never persisted; a fresh daemon restart simply re-detects.
type stuckSessionState struct {
	Stuck          bool
	UnackedBacklog bool
	Excerpt        string    // last stuckExcerptLines of the captured pane, only while Stuck
	StuckSince     time.Time // start of the current stuck episode
	LastPrompted   time.Time // last time the approval dialog was shown for it
	// LastPromptedHash is stuckEpisodeSignature (marker + numbered
	// options), NOT a hash of the raw Excerpt text, at the last dialog
	// show. A tick whose SIGNATURE differs is a NEW prompt episode (e.g.
	// several subagents each triggering their own worktree-relocation
	// prompt back-to-back) and reprompts immediately regardless of
	// stuckDialogRepromptInterval; an unchanged signature means it's still
	// the same prompt (even if the raw excerpt text changed — a
	// repainting pane, elapsed-time counter, or streaming output would
	// otherwise look like a new episode on almost every tick and defeat
	// the reprompt interval entirely), so the normal interval applies.
	LastPromptedHash string
	// LastRawExcerpt/PendingExcerptCandidate implement two-tick
	// confirmation for Stuck (see updateStuckState) — a per-tick marker+
	// options match alone isn't enough: a resolved dialog's text can sit
	// in the scan window while the session is genuinely working below it
	// (confirmed live — g53286's false positive: a running subagent with
	// a climbing token count and elapsed timer, stale "Teach auto mode"
	// prompt text still rendered above it). The distinguishing signal is
	// behavioural, not positional: does the window change between ticks?
	// LastRawExcerpt is the FULL scanned window from the previous tick,
	// captured regardless of stuck status — unlike the public Excerpt
	// field, which only holds content while CONFIRMED stuck.
	// PendingExcerptCandidate records whether a signature was ALSO
	// present last tick, so a signature appearing for the first time
	// never confirms on the same tick it's first seen (a real block
	// takes one extra tick to alert; accepted deliberately — see
	// updateStuckState).
	LastRawExcerpt          string
	PendingExcerptCandidate bool
	// PersistentSignatureCount/LastSeenSignature implement the
	// persistence backstop (see updateStuckState and
	// stuckPersistenceBackstopTicks): the exact-match fast path above
	// requires the FULL window byte-identical across ticks, which
	// correctly rejects a stale dialog beside UNRELATED progress
	// (g53286) — but a GENUINELY blocked agent whose own pane keeps
	// changing for an unrelated reason would never confirm via that path
	// either, and could hang silently forever. ORCHESTRATOR.md has
	// agents own long-running background jobs, which keep emitting
	// output while the foreground is frozen on a permission prompt —
	// exactly that shape. This tracks how many CONSECUTIVE ticks the
	// same dialog SIGNATURE (marker+options — stuckEpisodeSignature, not
	// the raw text) has been seen, regardless of what else in the pane
	// changed, and confirms once the streak reaches
	// stuckPersistenceBackstopTicks — bounding the worst case to a LATE
	// alert instead of a silent one.
	PersistentSignatureCount int
	LastSeenSignature        string
	// HookCommand/HookToolName cache a PermissionRequest hook payload's
	// structured tool_input.command/tool_name (see recordHookCommand) —
	// sourced from Claude Code's own hook JSON, immune to truncation by
	// any pane scan window, unlike anything derived from captured pane
	// text. HookCommandSignature records WHICH prompt episode (see
	// LastSeenSignature) they belong to; updateStuckState only surfaces
	// them when this matches the CURRENT signature, and clears all three
	// once it no longer does — so a command reported for a RESOLVED
	// episode is never shown for a later, different one. Empty whenever
	// the hook hasn't fired for the current episode (e.g. a hard
	// permissions.deny rule bypasses the hook entirely — SPEC-serve.md —
	// or the tool wasn't Bash-shaped), which is fine: display falls back
	// to the wider pane capture alone (stuckContextScanLines).
	HookCommand          string
	HookToolName         string
	HookCommandSignature string
	// RateLimited/RateLimitResetsAt/RateLimitSince track a claude.ai
	// usage-limit wait (detectRateLimitWait) — an INDEPENDENT condition
	// from Stuck, tracked on the same struct only because it comes from
	// the same pane capture on the same tick. A rate-limited session is
	// not stuck (nothing to answer) and not dead (it resumes by itself
	// at the reset time), but it does stop polling its inbox, so without
	// this it reads as "⚫ gone" with an unexplained growing backlog.
	// RateLimitResetsAt is raw pane text (see detectRateLimitWait), and
	// doubles as the episode identity: a different reset string is a
	// different episode.
	RateLimited       bool
	RateLimitResetsAt string
	RateLimitSince    time.Time
	// RateLimitDialogShown/RateLimitDialogResets enforce exactly one
	// dialog per episode. Unlike the stuck dialog there is no reprompt
	// interval: the notification is purely informational (nothing to
	// approve), so re-showing it every 15 minutes for something that
	// resolves itself would be pure nagging. Both are cleared when the
	// episode ends, so an identically-shaped later episode still alerts
	// — the same re-arming reason LastPromptedHash is cleared on clear.
	RateLimitDialogShown  bool
	RateLimitDialogResets string
}

// stuckPersistenceBackstopTicks bounds the worst-case delay before a
// genuinely blocked agent alerts even when its pane keeps changing for
// an unrelated reason (see stuckSessionState.PersistentSignatureCount's
// doc comment). Chosen so the bound is roughly 10 minutes at
// stuckCheckInterval — late, never absent, and short enough that "the
// user is ignoring a stuck badge that never fired" isn't itself the
// failure mode.
const stuckPersistenceBackstopTicks = 5 // 5 * stuckCheckInterval (2min) = 10min

// stuckInfo is a read-only, point-in-time view of one agent's stuck state,
// for dashboard/session-page rendering.
type stuckInfo struct {
	Stuck             bool
	UnackedBacklog    bool
	Excerpt           string
	RateLimited       bool
	RateLimitResetsAt string
	RateLimitSince    time.Time
}

// stuckTracker holds per-agent stuckSessionState behind its own mutex,
// independent of Server.mu (see Server.stuck's doc comment for why).
type stuckTracker struct {
	mu     sync.Mutex
	byName map[string]*stuckSessionState
}

func newStuckTracker() *stuckTracker {
	return &stuckTracker{byName: map[string]*stuckSessionState{}}
}

// snapshot returns a read-only copy of every tracked agent's stuck state,
// for rendering.
func (t *stuckTracker) snapshot() map[string]stuckInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]stuckInfo, len(t.byName))
	for name, st := range t.byName {
		out[name] = stuckInfo{
			Stuck:             st.Stuck,
			UnackedBacklog:    st.UnackedBacklog,
			Excerpt:           st.Excerpt,
			RateLimited:       st.RateLimited,
			RateLimitResetsAt: st.RateLimitResetsAt,
			RateLimitSince:    st.RateLimitSince,
		}
	}
	return out
}

// detectLiveStuckPrompt reports whether pane shows a permission prompt that
// is LIVE right now — not the transcript of one already answered and
// sitting in scrollback. Claude's own pane prints a resolved prompt
// verbatim as history, so a bare marker-substring match alone routinely
// re-detects something the user already dealt with (confirmed live:
// /status.stuckPrompts read 0 while an osascript dialog was firing for
// exactly this reason). A prompt only counts as live if BOTH a
// stuckMarkers substring AND at least one numbered menu option
// (parseStuckPromptOptions) appear within the SAME narrow window — the
// last stuckExcerptLines lines. A prompt that has scrolled out of that
// window is by definition resolved: its option list scrolled with it.
// Returns the matching marker line (trimmed) and true only when both hold.
func detectLiveStuckPrompt(pane string) (string, bool) {
	recent := lastNLines(pane, stuckExcerptLines)
	var markerLine string
	for _, line := range strings.Split(recent, "\n") {
		for _, marker := range stuckMarkers {
			if strings.Contains(line, marker) {
				markerLine = strings.TrimSpace(line)
			}
		}
		if markerLine != "" {
			break
		}
	}
	if markerLine == "" {
		return "", false
	}
	if len(parseStuckPromptOptions(pane)) == 0 {
		return "", false
	}
	return markerLine, true
}

// stuckOptionRe matches a numbered menu option line, e.g. "❯ 1. Yes" or
// "  2. Yes, and don't ask again this session" or "3. No, and tell Claude
// what to do differently (esc)".
var stuckOptionRe = regexp.MustCompile(`(?m)^\s*(?:❯\s*)?(\d)\.\s*(.+)$`)

// stuckPromptOption is one parsed numbered menu option.
type stuckPromptOption struct {
	Number int
	Text   string
}

// stuckEpisodeSignature returns a stable identifier for the CURRENT prompt
// episode: the marker line plus every parsed numbered option's number and
// text — deliberately NOT a hash of the raw excerpt text. A repainting
// pane (elapsed-time counter, spinner, streaming tool output) changes the
// raw excerpt on nearly every detector tick while the SAME prompt is still
// showing; hashing that raw text made almost every tick look like a
// brand-new episode and reprompt immediately, defeating
// stuckDialogRepromptInterval entirely (the live incident this fixes).
// Only a genuine change in the marker or WHICH options are offered means a
// different prompt.
func stuckEpisodeSignature(markerLine string, opts []stuckPromptOption) string {
	parts := make([]string, 0, len(opts)+1)
	parts = append(parts, markerLine)
	for _, o := range opts {
		parts = append(parts, strconv.Itoa(o.Number)+":"+o.Text)
	}
	return sha256Hex(strings.Join(parts, "\n"))
}

// parseStuckPromptOptions extracts numbered menu options from the last
// stuckExcerptLines of a captured pane — the SAME window
// detectLiveStuckPrompt requires a marker in, so an option belonging to an
// old, scrolled-away prompt is never mistaken for one on a current prompt.
// Permission prompts come in more than one shape (2-option Yes/No,
// 3-option Yes/Yes-don't-ask-again/No), so callers must resolve option
// meaning from the parsed text — never assume a fixed position/count.
func parseStuckPromptOptions(pane string) []stuckPromptOption {
	recent := lastNLines(pane, stuckExcerptLines)
	matches := stuckOptionRe.FindAllStringSubmatch(recent, -1)
	opts := make([]stuckPromptOption, 0, len(matches))
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		opts = append(opts, stuckPromptOption{Number: n, Text: strings.TrimSpace(m[2])})
	}
	return opts
}

// stuckPromptMapping resolves which numbered option corresponds to "Allow
// once" (the bare "Yes" option), "Always allow" (an option whose text
// mentions not asking again), and "Deny" (an option whose text starts with
// "No" — denial is a legitimate response the dialog must be able to send,
// not just a way to dismiss without acting) for one specific captured
// prompt. Any key may be "" if that option isn't present (e.g. a 2-option
// Yes/No prompt has no "Always allow" equivalent).
type stuckPromptMapping struct {
	AllowOnceKey   string
	AlwaysAllowKey string
	DenyKey        string
}

// resolveStuckPromptMapping parses pane fresh and maps its numbered options
// by TEXT, not position — a "don't ask again" option is 2 on some prompts
// and 3 on others.
func resolveStuckPromptMapping(pane string) stuckPromptMapping {
	var m stuckPromptMapping
	for _, opt := range parseStuckPromptOptions(pane) {
		switch {
		case strings.Contains(strings.ToLower(opt.Text), "don't ask again"):
			m.AlwaysAllowKey = strconv.Itoa(opt.Number)
		case opt.Text == "Yes":
			m.AllowOnceKey = strconv.Itoa(opt.Number)
		case strings.HasPrefix(opt.Text, "No"):
			m.DenyKey = strconv.Itoa(opt.Number)
		}
	}
	return m
}

// stuckDialogButtons builds the dialog's button list from a resolved
// mapping: "Dismiss" always first (and the default), then whichever of
// "Deny"/"Allow once"/"Always allow" actually has a corresponding option on
// this specific prompt.
func stuckDialogButtons(m stuckPromptMapping) []string {
	buttons := []string{"Dismiss"}
	if m.DenyKey != "" {
		buttons = append(buttons, "Deny")
	}
	if m.AllowOnceKey != "" {
		buttons = append(buttons, "Allow once")
	}
	if m.AlwaysAllowKey != "" {
		buttons = append(buttons, "Always allow")
	}
	return buttons
}

// stuckDialogTextMaxLen caps the dialog body's length (runes, not bytes,
// so multi-byte glyphs like "❯" count once) — long enough to show real
// context, short enough that osascript's dialog stays readable.
const stuckDialogTextMaxLen = 600

// buildDisplayExcerpt builds what updateStuckState stores as st.Excerpt —
// the text ultimately shown both in /status (tray/webui) and, via
// stuckPromptContext downstream, in the native approval dialog. Two
// sources, combined in priority order (command, subagent/reason context,
// then the marker+options the wider capture and stuckPromptContext already
// carry — see this function's own callers for where options get trimmed):
//
//  1. hookCommand, if hookSignature matches signature (this exact prompt
//     episode) — Claude Code's own structured tool_input.command, reported
//     by the PermissionRequest hook (see stuckSessionState.HookCommand's
//     doc comment). Reliable and un-truncatable, but only ever present
//     when the hook fired for THIS episode (Bash-shaped tool, not blocked
//     by a hard permissions.deny rule that bypasses the hook entirely).
//  2. pane, captured wide (stuckContextScanLines, not the narrow detection
//     window) — the only source for the human-readable framing (which
//     subagent asked, the classifier's stated reason) a hook payload
//     doesn't carry at all. Prepending (1) here, rather than replacing (2)
//     with it, means a command missing from the pane's own rendering (long
//     command truncated/wrapped oddly) is still shown exactly, while the
//     surrounding context a human actually needs to judge the request
//     stays intact either way.
func buildDisplayExcerpt(pane, hookCommand, hookToolName, hookSignature, signature string) string {
	wide := lastNLines(pane, stuckContextScanLines)
	if hookCommand == "" || hookSignature != signature {
		return wide
	}
	label := hookToolName
	if label == "" {
		label = "command"
	}
	return fmt.Sprintf("[hook] %s: %s\n\n%s", label, hookCommand, wide)
}

// stuckPromptContext extracts the request's context from a stored excerpt:
// from the first non-empty line (typically a "Tool use ..." header or the
// action description) down to and including the permission/marker line,
// with numbered menu option lines (see stuckOptionRe) dropped and runs of
// blank lines collapsed to one. This is what actually lets a human judge
// what's being approved — showing just the bare marker line ("Do you want
// to proceed?") gives no context at all.
//
// If no marker line is found in the excerpt (shouldn't happen when called
// only while stuck, but defensive), the whole excerpt is used as a
// fallback rather than returning nothing.
func stuckPromptContext(excerpt string) string {
	lines := strings.Split(excerpt, "\n")

	markerIdx := -1
	for i, line := range lines {
		for _, marker := range stuckMarkers {
			if strings.Contains(line, marker) {
				markerIdx = i
				break
			}
		}
		if markerIdx != -1 {
			break
		}
	}
	end := len(lines)
	if markerIdx != -1 {
		end = markerIdx + 1
	}

	start := 0
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}

	var out []string
	prevBlank := false
	for _, line := range lines[start:end] {
		if stuckOptionRe.MatchString(line) {
			continue
		}
		blank := strings.TrimSpace(line) == ""
		if blank && prevBlank {
			continue
		}
		out = append(out, line)
		prevBlank = blank
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// truncateMiddle shortens s to at most maxRunes runes, cutting out the
// middle (replaced with "…") so both the start of the request and its
// ending (permission line) stay visible — a tail-only truncation would
// lose the part that actually says what's being asked for.
func truncateMiddle(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	const ellipsis = "…"
	avail := maxRunes - 1
	if avail < 0 {
		avail = 0
	}
	head := avail / 2
	tail := avail - head
	return string(r[:head]) + ellipsis + string(r[len(r)-tail:])
}

// buildStuckDialogText renders the approval dialog's body: the request's
// context (stuckPromptContext), middle-truncated to stuckDialogTextMaxLen,
// plus a tmux attach hint.
func buildStuckDialogText(excerpt, session string) string {
	ctx := truncateMiddle(stuckPromptContext(excerpt), stuckDialogTextMaxLen)
	return fmt.Sprintf("%s\n\n(tmux attach -t %s to inspect)", ctx, session)
}

// buildRateLimitDialogText is the once-per-episode usage-limit
// notification's body. Purely informational — there is nothing to approve
// and no keys to send — so it states the one thing that is NOT obvious
// from the pane: the session comes back on its own, and the way to get it
// back sooner is another account's credentials, which every Claude
// session on this machine picks up (they share the same credential
// store), not something done per-pane.
func buildRateLimitDialogText(resetsAt string) string {
	resets := resetsAt
	if resets == "" {
		resets = "an unknown time"
	}
	return fmt.Sprintf("Claude Code session limit hit; resets %s. The session continues automatically at reset. "+
		"To continue now: run /login in any Claude session with another account (all sessions on this machine pick up "+
		"the new credentials), or /usage-credits in the pane.", resets)
}

// lastNLines returns the last n lines of text (fewer if text is shorter).
func lastNLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// oldestDeliveredUnackedAge returns how long the oldest delivered-but-unacked
// message addressed to agentName has been waiting, or 0 if there is none.
// Only "delivered" messages count — a merely "queued" one hasn't reached
// the agent yet, so its age isn't evidence of a stuck/unackedBacklog session.
// Informational messages (see Message.Informational) are excluded entirely:
// they ask nothing of the recipient, so an agent never acking one is not
// evidence of anything — counting it here is exactly the 2026-08-25
// incident (a working agent flagged off one FYI message that sat unacked
// for ~3h because there was nothing to ack).
func oldestDeliveredUnackedAge(agentName string, messages []Message, now time.Time) time.Duration {
	var oldest time.Time
	found := false
	for _, m := range messages {
		if m.To != agentName || m.Status != "delivered" || m.Informational {
			continue
		}
		at := m.CreatedAt
		if m.DeliveredAt != nil {
			at = *m.DeliveredAt
		}
		if !found || at.Before(oldest) {
			oldest = at
			found = true
		}
	}
	if !found {
		return 0
	}
	return now.Sub(oldest)
}

// sessionStatusDisplay implements the dashboard/session-page status
// precedence: rate-limited > gone > stuck > unackedBacklog > draining >
// alive.
//
// rateLimited deliberately outranks even "gone", which every other state
// loses to: a session parked on a usage limit stops polling its inbox, so
// it goes not-alive within aliveWindow BY DEFINITION, and reporting that
// as "⚫ gone" would hide the one fact that actually explains it — it is
// coming back by itself at the reset time, and nobody needs to
// investigate or respawn it. It also outranks unackedBacklog, which
// satisfies "suppress the backlog alarm while rate-limited" at the only
// place that alarm exists (the badge); /status keeps reporting
// unackedBacklog as a field regardless, since the backlog is real, just
// expected.
func sessionStatusDisplay(alive, stuck, unackedBacklog, draining, rateLimited bool, rateLimitResetsAt string) string {
	if rateLimited {
		if rateLimitResetsAt != "" {
			return "⏳ usage limit (resets " + rateLimitResetsAt + ")"
		}
		return "⏳ usage limit"
	}
	if !alive {
		return "⚫ gone"
	}
	if stuck {
		return "⛔ stuck (permission prompt)"
	}
	if unackedBacklog {
		return "🟡 unacked backlog (>30min)"
	}
	if draining {
		return "🌀 draining"
	}
	return "🟢 alive"
}

// contextPctSuffix returns " (N%)" for a known context-usage report, or ""
// when pct is nil (never reported) — appended next to the status wherever
// it's shown (dashboard Sessions table, session pages, `tn agents`), per
// SPEC-generations.md's daemon-managed auto-drain.
func contextPctSuffix(pct *float64) string {
	if pct == nil {
		return ""
	}
	return fmt.Sprintf(" (%.0f%%)", *pct)
}

// startStuckDetector launches a background goroutine that inspects every
// alive orchestrator's tmux pane: once after stuckCheckInitialDelay (so a
// prompt appearing around a binary self-restart isn't stuck waiting for the
// full steady-state interval), then every stuckCheckInterval. Disabled
// entirely when TN_NO_STUCKCHECK=1 (tests and smoke runs that start the
// real daemon should set this) — that turns off DETECTION itself. To keep
// detection but drop only the osascript popup, see dialogFuncForEnv/
// TN_NO_DIALOG instead.
func (s *Server) startStuckDetector() {
	if os.Getenv("TN_NO_STUCKCHECK") == "1" {
		return
	}
	dialog := dialogFuncForEnv()
	go func() {
		time.Sleep(stuckCheckInitialDelay)
		s.checkStuckSessionsOnce(capturePaneReal, dialog, sendKeysReal)
		s.checkOrphanedDeathsOnce()
		s.sweepUnroutableMessagesOnce()

		ticker := time.NewTicker(stuckCheckInterval)
		defer ticker.Stop()
		for range ticker.C {
			s.checkStuckSessionsOnce(capturePaneReal, dialog, sendKeysReal)
			s.checkOrphanedDeathsOnce()
			s.sweepUnroutableMessagesOnce()
		}
	}()
}

// dialogFuncForEnv returns dialogReal, or — when TN_NO_DIALOG=1 — a no-op
// that always returns ("", nil), the same "no selection" outcome
// updateStuckState already treats as do-nothing (dismiss/timeout). This is
// the daemon's ONLY Apple Events dependency (osascript); every other piece
// of stuck-prompt handling — Stuck/UnackedBacklog state, /status.stuckPrompts, the
// dashboard flag, and the session-page excerpt — is computed and recorded
// in updateStuckState BEFORE dialog is ever called, so suppressing it here
// changes nothing about what the daemon detects or reports. The SwiftBar
// tray's Attach / Allow once / Deny actions (POST /agents/approve, see
// handleApprove) already cover the exact same response surface without
// Apple Events, which is what makes the dialog redundant rather than load-
// bearing. Defaults to the real dialog (unset/anything else = enabled);
// the LaunchAgent plist sets TN_NO_DIALOG=1 by default on this machine.
func dialogFuncForEnv() dialogFunc {
	if os.Getenv("TN_NO_DIALOG") == "1" {
		return dialogSuppressed
	}
	return dialogReal
}

// dialogSuppressed is TN_NO_DIALOG=1's dialogFunc — a named top-level
// function (not an inline closure) specifically so tests can verify
// dialogFuncForEnv's return value by identity (reflect.ValueOf(...).
// Pointer()) without ever having to actually CALL the returned function to
// find out which one it is — calling the wrong one by accident in a test
// would mean actually invoking dialogReal's osascript on whatever machine
// runs the suite.
func dialogSuppressed(title, text string, buttons []string) (string, error) {
	return "", nil
}

// checkStuckSessionsOnce is one detector pass, factored out of the ticker
// loop so it's directly testable with injected capture/dialog/sendKeys.
// Inspects every REGISTERED (not deregistered) agent with a known tmux
// session (see tmuxSessionForAgent) — not just orchestrator-<slug>-named
// ones, since generation-aware agents report their own session and
// needn't follow that naming convention.
//
// Deliberately NOT gated on liveness (agentAliveAt/aliveWindow) anywhere
// in this function — that used to be the top-of-loop gate, and it made
// the detector blind in exactly the case it exists for: a session
// blocked on an interactive permission dialog stops polling its inbox
// (that IS what "not alive" measures — see aliveWindow's own doc
// comment), so liveness is a SYMPTOM of being stuck, not an independent
// precondition for checking. Gating capture on it meant the longer a
// session stayed blocked, the more certain the detector was to stop
// looking at it. Capture runs for every known session regardless;
// alerting (recording Stuck/Excerpt, showing the dialog) is what's
// gated, and it's gated on DeregisteredAt instead — a death path
// (finishRetire, reconcileSleepKilledAgent, checkOrphanedDeathsOnce) is
// what should silence a pane, not a stale heartbeat. This deliberately
// does NOT kill the tmux session for a dead agent (only finishRetire's
// explicit retire flow does that) — a deregistered agent's scrollback
// stays real forensic evidence (this exact investigation used it
// repeatedly), it just stops being alerted on.
func (s *Server) checkStuckSessionsOnce(capture captureFunc, dialog dialogFunc, sendKeys sendKeysFunc) {
	s.mu.Lock()
	snap := s.snapshotLocked()
	s.mu.Unlock()

	now := time.Now()
	for _, a := range snap.Agents {
		session, ok := tmuxSessionForAgent(a)
		if !ok {
			continue
		}

		pane, err := capture(session)
		if err != nil {
			// No tmux session (or capture failed) — nothing to detect.
			continue
		}

		if a.DeregisteredAt != nil {
			// A death path already marked this agent gone. Its pane may
			// still be sitting there (never killed, on purpose — see this
			// function's doc comment) showing whatever it last showed,
			// possibly an old unanswered prompt — that must never alert,
			// and must never re-alert on every subsequent tick either.
			continue
		}

		_, stuck := detectLiveStuckPrompt(pane)
		unackedBacklog := !stuck && oldestDeliveredUnackedAge(a.Name, snap.Messages, now) > unackedBacklogThreshold

		s.updateStuckState(a.Name, session, stuck, unackedBacklog, pane, now, capture, dialog, sendKeys, false)
	}
}

// updateStuckState records this pass's stuck/unackedBacklog result for agentName,
// then shows the approval dialog when either this is a genuinely new
// prompt episode (excerpt hash differs from the last one dialog'd — see
// stuckSessionState.LastPromptedHash) or the re-prompt interval has elapsed
// while stuck on the same prompt. The dedup bookkeeping happens under
// s.stuck's own lock, released before the (potentially minutes-long) dialog
// call. The dialog's buttons are built fresh from THIS pane's parsed
// options (resolveStuckPromptMapping) — a 2-option Yes/No prompt only ever
// offers "Allow once", never a mismapped "Always allow".
//
// stuck (the parameter) is only THIS TICK's raw marker+options detection —
// it does NOT by itself mean the agent is treated as stuck. Fresh
// confirmation (confirmedStuck below — what's allowed to fire a NEW
// dialog) comes from either of two independent signals, biased toward
// alerting when ambiguous (a false positive costs an unnecessary
// notification; a false negative costs a hung agent nobody notices):
//
//   - FAST PATH: the SAME signature on the PREVIOUS tick too, AND the
//     scanned window byte-identical between the two — see
//     stuckSessionState.LastRawExcerpt/PendingExcerptCandidate's doc
//     comment. Confirms in 2 ticks when nothing at all is happening
//     (live-confirmed false positive this rejects: a resolved dialog's
//     text sitting in the window while a running subagent below it has a
//     climbing token count/elapsed timer — g53286).
//   - PERSISTENCE BACKSTOP: the same dialog SIGNATURE seen for
//     stuckPersistenceBackstopTicks consecutive ticks, regardless of
//     whether the raw window changed — see PersistentSignatureCount's
//     doc comment. Catches the fast path's own blind spot: a genuinely
//     blocked agent whose pane keeps changing anyway because it owns a
//     background job still emitting output (ORCHESTRATOR.md). Slower
//     (~10 minutes worst case), but bounded — late, never silent.
//
// Any repaint anywhere in the scanned window — even inside the dialog's
// own text, even just a cosmetic element — costs at least one extra
// confirmation tick under the fast path; there's no special case for
// "but the marker+options matched." The detector cannot tell a cosmetic
// repaint from a background job's real output from captured text alone,
// so it doesn't try. This is believed to cost nothing in the common case:
// every real dialog captured this session — five samples, two
// independent sessions, both team-lead's live examples and this
// session's own throwaway reproductions — was completely static once
// rendered, no self-updating element inside the dialog block itself,
// consistent with Claude's own agent loop (the thing that drives
// elapsed/token displays) being paused while blocked on a modal input
// wait. Five samples is thin, but the fast path doesn't depend on it
// being universally true — it only costs an extra tick if it's ever
// wrong, and the backstop bounds even that.
//
// What's DISPLAYED (Stuck/Excerpt/StuckSince, and whether a stuck->clear
// transition gets logged) is a separate, STICKIER signal than fresh
// confirmation — see displayStuck below.
// hookTriggered is true only when this call originated from
// triggerHookConfirmedStuck (a real PermissionRequest hook POST, re-
// verified against a fresh pane capture) — see confirmedStuck below for
// what it changes and SPEC-serve.md's hook-detection section for why
// pane-scraping's fastConfirm/backstopConfirm paths stay fully active
// regardless: an agent whose hook has never fired (Agent.HookConfirmedAt
// nil) never reaches this call with hookTriggered=true at all, so this
// parameter existing doesn't weaken coverage for anyone pane-scraping
// still has to cover.
func (s *Server) updateStuckState(agentName, session string, stuck, unackedBacklog bool, pane string, now time.Time, capture captureFunc, dialog dialogFunc, sendKeys sendKeysFunc, hookTriggered bool) {
	s.stuck.mu.Lock()
	st, ok := s.stuck.byName[agentName]
	if !ok {
		st = &stuckSessionState{}
		s.stuck.byName[agentName] = st
	}
	wasStuck := st.Stuck

	// Usage-limit detection is fully independent of everything below it
	// (its own wider window, its own markers, its own once-per-episode
	// dialog): a session can be rate-limited while also showing a stale
	// permission prompt, and neither result may gate the other.
	rateResets, rateLimited := detectRateLimitWait(pane)
	wasRateLimited := st.RateLimited
	prevRateResets := st.RateLimitResetsAt
	showRateLimitDialog := false
	if rateLimited {
		if !wasRateLimited {
			st.RateLimitSince = now
		}
		st.RateLimited = true
		st.RateLimitResetsAt = rateResets
		// One dialog per episode; a changed reset string IS a new
		// episode (the limit moved, so the previous notification's
		// headline fact is now wrong).
		if !st.RateLimitDialogShown || st.RateLimitDialogResets != rateResets {
			showRateLimitDialog = true
			st.RateLimitDialogShown = true
			st.RateLimitDialogResets = rateResets
		}
	} else {
		st.RateLimited = false
		st.RateLimitResetsAt = ""
		st.RateLimitSince = time.Time{}
		st.RateLimitDialogShown = false
		st.RateLimitDialogResets = ""
	}

	rawExcerpt := lastNLines(pane, stuckExcerptLines)

	var signature string
	if stuck {
		markerLine, _ := detectLiveStuckPrompt(pane)
		signature = stuckEpisodeSignature(markerLine, parseStuckPromptOptions(pane))
	}
	switch {
	case stuck && signature != "" && signature == st.LastSeenSignature:
		st.PersistentSignatureCount++
	case stuck:
		st.PersistentSignatureCount = 1
	default:
		st.PersistentSignatureCount = 0
	}
	st.LastSeenSignature = signature

	// A cached hook command belongs to WHICHEVER episode it was recorded
	// against (HookCommandSignature) — if that's not the CURRENT signature
	// (a different, resolved episode's leftover, or "" now that nothing's
	// stuck), it must not survive into a later, different prompt that
	// never had one. recordHookCommand may run for THIS episode moments
	// before this exact call (triggerHookConfirmedStuck), so this only
	// clears a MISMATCH, never something just set for the tick in
	// progress.
	if st.HookCommandSignature != "" && st.HookCommandSignature != signature {
		st.HookCommand = ""
		st.HookToolName = ""
		st.HookCommandSignature = ""
	}

	fastConfirm := stuck && st.PendingExcerptCandidate && rawExcerpt == st.LastRawExcerpt
	backstopConfirm := stuck && st.PersistentSignatureCount >= stuckPersistenceBackstopTicks
	// hookConfirm is ANDed with stuck (not a bare hookTriggered) for the
	// same reason sendApprovalIfStillStuck always re-verifies from a fresh
	// capture rather than trusting the moment the hook fired: by the time
	// triggerHookConfirmedStuck's own capture lands, the prompt could
	// already be resolved (a fast auto-response, a human who was already
	// watching). hookTriggered alone would mean the hook path can never be
	// WRONG about there being a marker present — that's not true, only
	// that IT doesn't need a second tick to trust one once it's actually
	// there.
	hookConfirm := stuck && hookTriggered
	confirmedStuck := fastConfirm || backstopConfirm || hookConfirm

	// Comparison logging for the hook-vs-pane-scraping reliability check
	// (SPEC-serve.md): every FRESH confirmation (not a continuing streak)
	// logs which path found it. The acceptance bar is that every pane-
	// detector confirmation should also show up as a hook confirmation,
	// sooner — grep this line for both sources and compare timestamps
	// rather than trusting either mechanism's own self-report.
	if !wasStuck && confirmedStuck {
		source := "pane-fast"
		switch {
		case hookConfirm:
			source = "hook"
		case backstopConfirm && !fastConfirm:
			source = "pane-backstop"
		}
		log.Printf("serve: %s confirmed stuck via %s", agentName, source)
	}

	// Sticky display: once genuinely confirmed (via either path above),
	// a repaint of the SAME signature keeps the PUBLIC Stuck status
	// (and StuckSince/Excerpt) stable instead of flickering clear ->
	// candidate -> clear on every tick something repaints — a background
	// job's own output, a spinner, a statusline element. That flicker
	// would otherwise put a "cleared" log line and a bridge activity
	// entry into the user's feed on every such tick: noise, since a
	// dialog still on screen has not actually cleared. This does NOT
	// change confirmedStuck itself (still requires its own fresh
	// evidence — fastConfirm or the backstop — before firing another
	// dialog; see shouldPrompt below), only what gets DISPLAYED/LOGGED
	// once something is already confirmed. PersistentSignatureCount>=2
	// means THIS tick continues an existing streak of the same
	// signature, not a fresh one — a genuinely DIFFERENT signature
	// resets the count to 1 and correctly breaks stickiness (a new
	// prompt replacing the old one is real progress, not a repaint).
	sameSignatureContinuing := stuck && st.PersistentSignatureCount >= 2
	displayStuck := confirmedStuck || (wasStuck && sameSignatureContinuing)

	st.LastRawExcerpt = rawExcerpt
	st.PendingExcerptCandidate = stuck

	st.Stuck = displayStuck
	st.UnackedBacklog = unackedBacklog

	var excerptHash string
	var excerpt string
	if displayStuck {
		st.Excerpt = buildDisplayExcerpt(pane, st.HookCommand, st.HookToolName, st.HookCommandSignature, signature)
		excerpt = st.Excerpt
		excerptHash = signature
		if !wasStuck {
			st.StuckSince = now
		}
	} else {
		st.Excerpt = ""
		// LastPromptedHash/LastPrompted must be cleared here too, not just
		// Excerpt/Stuck — each detection pass has to be authoritative for
		// the CURRENT state, and leaving a resolved episode's signature in
		// place is exactly what silently disarms the alert path for the
		// NEXT episode: a later prompt of the identical shape (this
		// codebase's own worked example is a recurring worktree-relocation
		// prompt — see this struct's LastPromptedHash doc comment) would
		// compute the SAME excerptHash, so `newEpisode` below would read
		// false even though the agent was genuinely unstuck in between,
		// and the dialog — and tray-alert.sh, which dedupes on this exact
		// signature — would stay silent. Confirmed as a real gap by
		// TestStuckStateSequence_IdenticallyShapedRecurrenceStillReArms
		// (fails without this reset, passes with it).
		st.LastPromptedHash = ""
		st.LastPrompted = time.Time{}
	}
	wentClear := wasStuck && !displayStuck

	newEpisode := confirmedStuck && excerptHash != st.LastPromptedHash
	shouldPrompt := confirmedStuck && (newEpisode || now.Sub(st.LastPrompted) >= stuckDialogRepromptInterval)
	if shouldPrompt {
		st.LastPrompted = now
		st.LastPromptedHash = excerptHash
	}
	s.stuck.mu.Unlock()

	// Make the stuck->clear transition visible rather than an unexplained
	// silence: log it, and put it in the feed via the same activity log
	// everything else (MR merges, retires, orphan notices) already flows
	// through — see checkOrphanedTasks's identical "log against 'bridge',
	// not agentName" reasoning just below for why this is attributed to
	// "bridge" too (appendActivityLocked's touchAgentLocked side effect
	// would otherwise bump agentName's LastSeenAt from this call alone).
	// Usage-limit transitions are logged (not pushed into the activity
	// feed): an episode is self-resolving and typically minutes long, so
	// it belongs in the daemon log next to the stuck transitions, not as
	// two entries in the user's feed every time a limit is hit.
	if rateLimited && (!wasRateLimited || prevRateResets != rateResets) {
		log.Printf("serve: %s usage-limit wait detected (resets %s)", agentName, rateResets)
	}
	if !rateLimited && wasRateLimited {
		log.Printf("serve: %s usage-limit wait cleared", agentName)
	}
	if showRateLimitDialog {
		// Auto-swap (creds.go) runs BEFORE the dialog: the dialog can
		// block for up to 120s and the parked panes should be moving
		// again by then, not waiting on a click.
		title := fmt.Sprintf("Usage limit — %s", agentName)
		text := buildRateLimitDialogText(rateResets)
		if res := s.autoSwapCredentials(agentName, rateResets, now, sendKeys); res != nil {
			if res.swapped {
				title = "Usage limit — credentials swapped"
			}
			text = res.dialogText(agentName, rateResets)
		}
		if _, err := dialog(title, text, []string{"OK"}); err != nil {
			log.Printf("serve: failed to show usage-limit dialog for %s: %v", agentName, err)
		}
	}

	if wentClear {
		log.Printf("serve: %s stuck prompt cleared (session %s)", agentName, session)
		s.mu.Lock()
		s.appendActivityLocked("bridge", fmt.Sprintf("%s's stuck prompt cleared", agentName), "")
		s.saveLocked()
		s.mu.Unlock()
		s.triggerRenders()
	}

	if !shouldPrompt {
		return
	}

	mapping := resolveStuckPromptMapping(pane)
	buttons := stuckDialogButtons(mapping)
	title := fmt.Sprintf("Claude orchestrator stuck: %s", agentName)
	text := buildStuckDialogText(excerpt, session)
	choice, err := dialog(title, text, buttons)
	if err != nil {
		log.Printf("serve: failed to show stuck-session dialog for %s: %v", agentName, err)
		return
	}

	switch choice {
	case "Deny":
		s.sendApprovalIfStillStuck(agentName, session, approvalDeny, capture, sendKeys)
	case "Allow once":
		s.sendApprovalIfStillStuck(agentName, session, approvalAllowOnce, capture, sendKeys)
	case "Always allow":
		s.sendApprovalIfStillStuck(agentName, session, approvalAlwaysAllow, capture, sendKeys)
	default:
		// Dismiss, timeout, or an unrecognized response: do nothing.
	}
}

// stuckApprovalKind identifies which numbered option to send, resolved
// fresh from a re-capture — never a number carried over from dialog time.
type stuckApprovalKind int

const (
	approvalAllowOnce stuckApprovalKind = iota
	approvalAlwaysAllow
	approvalDeny
)

func (k stuckApprovalKind) label() string {
	switch k {
	case approvalAlwaysAllow:
		return "Always allow"
	case approvalDeny:
		return "Deny"
	default:
		return "Allow once"
	}
}

// sendApprovalIfStillStuck re-captures the pane, verifies a marker is
// STILL present, and re-parses the option mapping from that fresh capture
// before sending anything — option numbers can differ between the capture
// shown in the dialog and this one (a different prompt may now be showing),
// so neither the marker presence nor the option number is ever assumed
// stale. The user may also have already resolved it by attaching directly.
// Returns whether a key was actually sent, and — when it wasn't — a short
// machine-readable reason (used by POST /agents/approve's response; the
// dialog-response caller ignores both).
func (s *Server) sendApprovalIfStillStuck(agentName, session string, kind stuckApprovalKind, capture captureFunc, sendKeys sendKeysFunc) (sent bool, reason string) {
	pane, err := capture(session)
	if err != nil {
		log.Printf("serve: stuck-session re-check failed for %s: %v", agentName, err)
		return false, fmt.Sprintf("failed to re-check session: %v", err)
	}
	if _, stillStuck := detectLiveStuckPrompt(pane); !stillStuck {
		log.Printf("serve: stuck-session prompt for %s cleared before send, skipping", agentName)
		return false, "prompt cleared before send"
	}

	mapping := resolveStuckPromptMapping(pane)
	var key string
	switch kind {
	case approvalAlwaysAllow:
		key = mapping.AlwaysAllowKey
	case approvalDeny:
		key = mapping.DenyKey
	default:
		key = mapping.AllowOnceKey
	}
	if key == "" {
		log.Printf("serve: stuck-session prompt for %s no longer offers %q on re-check, skipping", agentName, kind.label())
		return false, fmt.Sprintf("prompt no longer offers %q", kind.label())
	}

	if err := sendKeys(session, key); err != nil {
		log.Printf("serve: failed to send approval keys for %s: %v", agentName, err)
		return false, fmt.Sprintf("failed to send keys: %v", err)
	}
	log.Printf("serve: user approved stuck prompt for %s (option %s)", agentName, kind.label())

	s.mu.Lock()
	s.appendActivityLocked(agentName, fmt.Sprintf("Approved stuck-session prompt: %s", kind.label()), "")
	s.saveLocked()
	s.triggerRenders()
	s.mu.Unlock()

	return true, ""
}

// capturePaneReal captures the visible content of a tmux pane read-only.
func capturePaneReal(session string) (string, error) {
	out, err := exec.Command(resolveTmuxBin(), "capture-pane", "-t", session, "-p").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// sendKeysReal types keys into a tmux pane followed by Enter, e.g. to
// answer a numbered permission-prompt menu.
func sendKeysReal(session, keys string) error {
	return exec.Command(resolveTmuxBin(), "send-keys", "-t", session, keys, "Enter").Run()
}

// tmuxSessionState is the three-state result of checking whether a tmux
// session currently exists — deliberately NOT a bool. "Unknown" must
// never be treated as "gone" by any caller: see hasTmuxSessionReal's doc
// comment for why the check itself can fail in ways that mean nothing
// about the specific session being asked about.
type tmuxSessionState int

const (
	tmuxSessionUnknown tmuxSessionState = iota
	tmuxSessionExists
	tmuxSessionGone
)

// tmuxSessionExistenceFunc checks whether a named tmux session currently
// exists, three-state (see tmuxSessionState) — distinct from the
// existing boolean tmuxHasSessionFunc used by the spawn path, where "we
// couldn't tell" and "it's not there" are safe to collapse together (the
// worst case is a redundant spawn attempt). Here they must NOT collapse:
// this feeds a decision to reset another agent's owned tasks, and
// "unknown" must never authorise that. Injectable (same convention as
// captureFunc/dialogFunc/sendKeysFunc) so tests never touch a real tmux
// server.
type tmuxSessionExistenceFunc func(session string) tmuxSessionState

// hasTmuxSessionReal checks session existence via `tmux has-session`.
// Three-state rather than boolean because `has-session`'s exit code
// ALONE cannot distinguish "this session doesn't exist" (exit 1, stderr
// "can't find session: ...") from "the tmux server itself isn't
// reachable at all" (exit 1, stderr "error connecting to ... (No such
// file or directory)", e.g. no tmux binary, no server running, a
// permissions error) — verified against the installed tmux (3.6b) on
// this machine. Those two cases mean opposite things here: a session
// genuinely gone is a positive, verifiable fact about THAT agent;
// tmux being unreachable is a fact about this machine right now that
// would read identically for every agent simultaneously — exactly the
// mass-false-positive shape the rest of this mechanism exists to avoid.
// So only an explicit "can't find session" in stderr counts as gone;
// anything else (including any future tmux version's differently-worded
// message) is unknown, and unknown must never authorise a sweep.
func hasTmuxSessionReal(session string) tmuxSessionState {
	var stderr bytes.Buffer
	cmd := exec.Command(resolveTmuxBin(), "has-session", "-t", session)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		return tmuxSessionExists
	}
	if strings.Contains(stderr.String(), "can't find session") {
		return tmuxSessionGone
	}
	return tmuxSessionUnknown
}

// appleScriptQuote escapes a string for embedding in an AppleScript string
// literal.
func appleScriptQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// dialogReal shows a macOS display dialog via osascript with the given
// buttons (built per-prompt by the caller from what the pane actually
// offers — see resolveStuckPromptMapping/stuckDialogButtons), default
// buttons[0] ("Dismiss" by construction), giving up after 120s. Returns the
// clicked button's label, or "" (no error) on a timeout/give-up — the
// caller treats both "Dismiss" and "" as no-ops.
func dialogReal(title, text string, buttons []string) (string, error) {
	quoted := make([]string, len(buttons))
	for i, b := range buttons {
		quoted[i] = `"` + appleScriptQuote(b) + `"`
	}
	script := fmt.Sprintf(
		`display dialog "%s" with title "%s" buttons {%s} default button %s giving up after 120`,
		appleScriptQuote(text), appleScriptQuote(title), strings.Join(quoted, ", "), quoted[0],
	)
	out, err := exec.Command("/usr/bin/osascript", "-e", script).Output()
	if err != nil {
		return "", err
	}

	result := strings.TrimSpace(string(out))
	if strings.Contains(result, "gave up:true") {
		return "", nil
	}
	const marker = "button returned:"
	idx := strings.Index(result, marker)
	if idx == -1 {
		return "", nil
	}
	rest := result[idx+len(marker):]
	if commaIdx := strings.Index(rest, ", gave up:"); commaIdx != -1 {
		rest = rest[:commaIdx]
	}
	return strings.TrimSpace(rest), nil
}

// --- Needs your action (dashboard section) ---

// needsActionCacheTTL bounds how often the dashboard queries TaskNotes for
// the Needs your action section — the dashboard debounce can fire often
// (every mutation), and this avoids hammering the API on every render.
const needsActionCacheTTL = 30 * time.Second

// needsActionCache holds the last fetched Needs-your-action query result.
// Guarded by its own mutex, independent of Server.mu, since fetching can
// block on a network call and rendering already runs outside Server.mu.
type needsActionCache struct {
	mu        sync.Mutex
	fetchedAt time.Time
	tasks     []Task
	apiErr    bool
}

// get returns the cached tasks (and whether the cached fetch had failed) if
// fetched within ttl of now; otherwise it calls fetch, caches the result —
// including a failure, so a down API isn't retried on every render either —
// and returns it.
func (c *needsActionCache) get(now time.Time, ttl time.Duration, fetch func() ([]Task, error)) ([]Task, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.fetchedAt.IsZero() && now.Sub(c.fetchedAt) < ttl {
		return c.tasks, c.apiErr
	}

	tasks, err := fetch()
	c.fetchedAt = now
	if err != nil {
		c.tasks = nil
		c.apiErr = true
		return nil, true
	}
	c.tasks = tasks
	c.apiErr = false
	return tasks, false
}

// buildNeedsActionQuery builds the FilterQuery for the dashboard's Needs
// your action section: open (non-archived) tasks whose status is triage,
// needs-input, or review. FilterQuery has no "in" operator, so the three
// statuses are OR'd together in a nested group inside the AND root — a
// shape FilterGroup/FilterCondition can't express, hence filterNode.
// Requests dateModified-desc sorting; if TaskNotes doesn't support that
// sort key it's expected to fall back to its default ordering rather than
// erroring, per the same "always send a sortKey" precedent buildListQuery
// and buildDueTasksQuery already follow.
func buildNeedsActionQuery() filterNode {
	statusOr := filterNode{
		Type: "group", ID: "status-or", Conjunction: "or",
		Children: []filterNode{
			{Type: "condition", ID: "s-triage", Property: "status", Operator: "is", Value: "triage"},
			{Type: "condition", ID: "s-needs-input", Property: "status", Operator: "is", Value: "needs-input"},
			{Type: "condition", ID: "s-review", Property: "status", Operator: "is", Value: "review"},
		},
	}
	return filterNode{
		Type: "group", ID: "root", Conjunction: "and",
		Children: []filterNode{
			{Type: "condition", ID: "archived", Property: "archived", Operator: "is-not-checked"},
			statusOr,
		},
		SortKey:       "dateModified",
		SortDirection: "desc",
	}
}

// needsActionGroups defines the render order, emoji, and label for each
// status bucket in the Needs your action section.
var needsActionGroups = []struct{ status, emoji, label string }{
	{"triage", "🔴", "Triage (your decision)"},
	{"needs-input", "🟠", "Needs input (agent waiting on you)"},
	{"review", "🟣", "Review (verify & mark done)"},
}

// renderNeedsActionSection renders the "## Needs your action" dashboard
// section from a fetched task list, grouped by status with an emoji
// subheader per non-empty group. Each line links the task and, if present,
// its first project. apiErr renders a one-line unreachable fallback instead
// (the section is never omitted outright, so its position at the top of the
// note stays stable across renders).
func renderNeedsActionSection(tasks []Task, apiErr bool, mrStates map[string]string) string {
	var b strings.Builder
	b.WriteString("## Needs your action\n\n")

	if apiErr {
		b.WriteString("_TaskNotes API unreachable._\n\n")
		return b.String()
	}

	hasAny := false
	for _, g := range needsActionGroups {
		var lines []string
		for _, t := range tasks {
			if t.Status != g.status {
				continue
			}
			line := fmt.Sprintf("- %s — %s", taskWikilink(t.Path), t.Title)
			if len(t.Projects) > 0 && t.Projects[0] != "" {
				line += " (" + t.Projects[0] + ")"
			}
			if mr := t.CustomProperties["mr"]; mr != "" {
				line += fmt.Sprintf(" — [MR](%s)", mr)
				if state := mrStates[t.Path]; state != "" {
					line += fmt.Sprintf(" (%s)", state)
				}
			}
			if branch := integrationBranchFor(t); branch != "" {
				if integrationRoleFor(t) == "owner" {
					line += fmt.Sprintf(" — **OWNER** of `%s`", branch)
				} else {
					line += fmt.Sprintf(" — `%s`", branch)
				}
			}
			if ncKey := t.CustomProperties["jira-nc"]; ncKey != "" {
				line += fmt.Sprintf(" — NC `%s`", ncKey)
			} else if jira := t.CustomProperties["jira"]; jira != "" {
				line += fmt.Sprintf(" — %s", jira)
			}
			lines = append(lines, line)
		}
		if len(lines) == 0 {
			continue
		}
		hasAny = true
		fmt.Fprintf(&b, "### %s %s\n\n", g.emoji, g.label)
		for _, line := range lines {
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
	}

	if !hasAny {
		b.WriteString("_Nothing needs you._\n\n")
	}
	return b.String()
}

// --- Dashboard rendering ---

// dashboardSnapshot is a point-in-time copy of the state relevant to the
// dashboard and session pages, taken under the lock so rendering itself can
// run lock-free.
type dashboardSnapshot struct {
	Agents   []Agent
	Messages []Message
	Activity []ActivityEntry
	// MRStates is a copy of State.MRStates (taskPath -> last-observed
	// GitLab MR state), used to annotate Needs-your-action MR links with
	// their current state when known.
	MRStates map[string]string
}

func (s *Server) snapshotLocked() dashboardSnapshot {
	agents := make([]Agent, 0, len(s.state.Agents))
	for _, a := range s.state.Agents {
		agents = append(agents, *a)
	}
	messages := make([]Message, 0, len(s.state.Messages))
	for _, m := range s.state.Messages {
		messages = append(messages, *m)
	}
	activity := make([]ActivityEntry, 0, len(s.state.Activity))
	for _, a := range s.state.Activity {
		activity = append(activity, *a)
	}
	mrStates := make(map[string]string, len(s.state.MRStates))
	for k, v := range s.state.MRStates {
		mrStates[k] = v
	}
	return dashboardSnapshot{Agents: agents, Messages: messages, Activity: activity, MRStates: mrStates}
}

// renderDashboard snapshots state under the lock, fetches (or reuses a
// cached) Needs-your-action task list, renders outside the lock, and writes
// atomically. Write failures are logged once per distinct error and never
// crash the daemon.
func (s *Server) renderDashboard() {
	s.mu.Lock()
	snap := s.snapshotLocked()
	sessionPagesDir := s.sessionPagesDir
	client := s.tnClient
	s.mu.Unlock()

	var needsActionTasks []Task
	needsActionErr := true
	if client != nil {
		needsActionTasks, needsActionErr = s.needsAction.get(time.Now(), needsActionCacheTTL, func() ([]Task, error) {
			return client.QueryTasksRaw(buildNeedsActionQuery())
		})
	}

	md := renderDashboardMarkdown(snap, time.Now(), sessionPagesDir, needsActionTasks, needsActionErr, s.stuck.snapshot())
	if err := atomicWriteFile(s.dashboardPath, []byte(md)); err != nil {
		s.logDashboardErrOnce(err)
	}
}

func (s *Server) logDashboardErrOnce(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg := err.Error()
	if s.lastDashboardErr == msg {
		return
	}
	s.lastDashboardErr = msg
	log.Printf("serve: failed to write dashboard: %v", err)
}

// agentAliveAt reports whether a is alive as of now (a testable variant of
// isAlive that doesn't depend on time.Now()).
func agentAliveAt(a Agent, now time.Time) bool {
	return a.DeregisteredAt == nil && now.Sub(a.LastSeenAt) <= aliveWindow
}

// testVerificationAgentPrefix names an agent registered ONLY to exercise
// an endpoint against the real running daemon while verifying a change —
// never a real orchestrator generation. Registering one this way (rather
// than against a throwaway test server) is sometimes the only way to
// prove a fix works against live state, but a name like that has no
// business showing up in the user's Projects view looking like a genuine
// retired generation. Reserve this prefix for exactly that use; anything
// registered under it is excluded from /status's per-project agent lists
// (isTestVerificationAgentName below) automatically, and should still be
// purged afterward (POST /agents/purge) once its job is done rather than
// left to rely solely on the filter.
const testVerificationAgentPrefix = "zz-"

// isTestVerificationAgentName reports whether name looks like a
// verification-only registration per testVerificationAgentPrefix.
func isTestVerificationAgentName(name string) bool {
	return strings.HasPrefix(name, testVerificationAgentPrefix)
}

// tmuxSessionForAgent returns the tmux session to use for a, and whether
// one could be determined at all. Generation-aware agents report their
// session explicitly at register time (Agent.TmuxSession); legacy
// orchestrator-<slug>-named agents that never reported one fall back to
// the old tn-<slug> derivation. Anything else (a non-orchestrator agent
// that never registered a tmux session) has none to report. Used wherever
// a session name used to be derived purely from the agent name: the stuck
// detector, the dashboard's Attach column, and session pages.
func tmuxSessionForAgent(a Agent) (string, bool) {
	if a.TmuxSession != "" {
		return a.TmuxSession, true
	}
	if strings.HasPrefix(a.Name, "orchestrator-") {
		return "tn-" + strings.TrimPrefix(a.Name, "orchestrator-"), true
	}
	return "", false
}

func humanizeAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// truncate shortens s to at most n runes, appending an ellipsis if it was
// cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// taskWikilink returns an Obsidian wikilink for a vault-relative .md task
// path, or "" if path doesn't end in .md.
func taskWikilink(path string) string {
	if !strings.HasSuffix(path, ".md") {
		return ""
	}
	return "[[" + strings.TrimSuffix(path, ".md") + "]]"
}

type dashboardSessionRow struct {
	Name              string
	DisplayName       string // Name, or a wikilink to its session page when enabled
	Project           string
	Alive             bool
	Stuck             bool
	UnackedBacklog    bool
	Draining          bool
	RateLimited       bool
	RateLimitResetsAt string
	ContextPct        *float64
	LastSeen          time.Time
	Queued            int
	Attach            string
}

// buildSessionRows implements the Sessions table rules: status (stuck >
// unackedBacklog > draining > alive > gone precedence, see sessionStatusDisplay),
// queued+delivered counts, tmux attach hints, and omitting deregistered
// agents with nothing pending. When sessionPagesDir is non-empty, each
// row's DisplayName links to that agent's session page.
func buildSessionRows(agents []Agent, messages []Message, now time.Time, sessionPagesDir string, stuckByAgent map[string]stuckInfo) []dashboardSessionRow {
	pendingCount := map[string]int{}
	for _, m := range messages {
		if m.Status == "queued" || m.Status == "delivered" {
			pendingCount[m.To]++
		}
	}

	rows := make([]dashboardSessionRow, 0, len(agents))
	for _, a := range agents {
		if a.DeregisteredAt != nil && pendingCount[a.Name] == 0 {
			continue
		}
		attach := "—"
		if session, ok := tmuxSessionForAgent(a); ok {
			attach = fmt.Sprintf("`tmux attach -t %s`", session)
		}
		si := stuckByAgent[a.Name]
		rows = append(rows, dashboardSessionRow{
			Name:              a.Name,
			DisplayName:       sessionDisplayName(a.Name, sessionPagesDir),
			Project:           a.Project,
			Alive:             agentAliveAt(a, now),
			Stuck:             si.Stuck,
			UnackedBacklog:    si.UnackedBacklog,
			Draining:          !a.Accepting,
			RateLimited:       si.RateLimited,
			RateLimitResetsAt: si.RateLimitResetsAt,
			ContextPct:        a.ContextPct,
			LastSeen:          a.LastSeenAt,
			Queued:            pendingCount[a.Name],
			Attach:            attach,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}

type dashboardPendingRow struct {
	ID       string
	To       string
	Status   string
	Age      string
	Text     string
	Wikilink string
}

// buildPendingRows implements the Pending messages rules: every non-acked
// message, oldest first, with humanized age and truncated text.
func buildPendingRows(messages []Message, now time.Time) []dashboardPendingRow {
	pending := make([]Message, 0, len(messages))
	for _, m := range messages {
		if m.Status != "acked" {
			pending = append(pending, m)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].CreatedAt.Before(pending[j].CreatedAt) })

	rows := make([]dashboardPendingRow, 0, len(pending))
	for _, m := range pending {
		rows = append(rows, dashboardPendingRow{
			ID:       m.ID,
			To:       m.To,
			Status:   m.Status,
			Age:      humanizeAge(now.Sub(m.CreatedAt)),
			Text:     truncate(m.Text, 120),
			Wikilink: taskWikilink(m.TaskPath),
		})
	}
	return rows
}

type dashboardActivityRow struct {
	Time     string
	Agent    string
	ID       string
	Response string
	Wikilink string
}

// buildActivityRows implements the Recent activity rules: last 10 acked
// messages, newest first, with truncated response text.
func buildActivityRows(messages []Message) []dashboardActivityRow {
	acked := make([]Message, 0, len(messages))
	for _, m := range messages {
		if m.Status == "acked" && m.AckedAt != nil {
			acked = append(acked, m)
		}
	}
	sort.Slice(acked, func(i, j int) bool { return acked[i].AckedAt.After(*acked[j].AckedAt) })
	if len(acked) > 10 {
		acked = acked[:10]
	}

	rows := make([]dashboardActivityRow, 0, len(acked))
	for _, m := range acked {
		rows = append(rows, dashboardActivityRow{
			Time:     m.AckedAt.Format("15:04"),
			Agent:    m.To,
			ID:       m.ID,
			Response: truncate(m.Response, 120),
			Wikilink: taskWikilink(m.TaskPath),
		})
	}
	return rows
}

// renderDashboardMarkdown is a pure function from a state snapshot to the
// dashboard note's Markdown content, so it's unit-testable without a
// running server.
func renderDashboardMarkdown(snap dashboardSnapshot, now time.Time, sessionPagesDir string, needsActionTasks []Task, needsActionErr bool, stuckByAgent map[string]stuckInfo) string {
	var b strings.Builder

	b.WriteString("---\ngenerated: true\n---\n\n")
	fmt.Fprintf(&b, "> Auto-generated by `tn serve` — do not edit. Updated: %s\n\n", now.Format("2006-01-02 15:04:05"))

	b.WriteString(renderNeedsActionSection(needsActionTasks, needsActionErr, snap.MRStates))

	if !needsActionErr {
		// Passive visibility, not an alert — see statusResponse.Triage's
		// doc comment in serve.go for why. Piggybacks onto a section
		// that's already read rather than adding a new one.
		if triage := computeTriageSummary(needsActionTasks, now); triage.Count > 0 {
			if triage.OldestPath != "" {
				fmt.Fprintf(&b, "_Triage: %d waiting, oldest %s — %s._\n\n",
					triage.Count, humanizeAge(time.Duration(triage.OldestAgeSeconds*float64(time.Second))), taskWikilink(triage.OldestPath))
			} else {
				// Every triage task has an unparseable/missing DateCreated —
				// still real debt, just can't be aged.
				fmt.Fprintf(&b, "_Triage: %d waiting._\n\n", triage.Count)
			}
		}
	}

	b.WriteString("## Sessions\n\n")
	sessions := buildSessionRows(snap.Agents, snap.Messages, now, sessionPagesDir, stuckByAgent)
	if len(sessions) == 0 {
		b.WriteString("_No sessions registered._\n\n")
	} else {
		b.WriteString("| Session | Project | Status | Last seen | Queued | Attach |\n")
		b.WriteString("|---|---|---|---|---|---|\n")
		for _, r := range sessions {
			status := sessionStatusDisplay(r.Alive, r.Stuck, r.UnackedBacklog, r.Draining, r.RateLimited, r.RateLimitResetsAt) + contextPctSuffix(r.ContextPct)
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %d | %s |\n",
				r.DisplayName, r.Project, status, r.LastSeen.Format("15:04:05"), r.Queued, r.Attach)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Pending messages\n\n")
	pending := buildPendingRows(snap.Messages, now)
	if len(pending) == 0 {
		b.WriteString("_No pending messages._\n\n")
	} else {
		for _, p := range pending {
			line := fmt.Sprintf("- `%s` → **%s** (%s, %s): %s", p.ID, p.To, p.Status, p.Age, p.Text)
			if p.Wikilink != "" {
				line += " (" + p.Wikilink + ")"
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
	}

	activity := buildActivityRows(snap.Messages)
	if len(activity) > 0 {
		b.WriteString("## Recent activity\n\n")
		for _, a := range activity {
			line := fmt.Sprintf(`- %s ✅ %s acked %s — "%s"`, a.Time, a.Agent, a.ID, a.Response)
			if a.Wikilink != "" {
				line += " (" + a.Wikilink + ")"
			}
			b.WriteString(line + "\n")
		}
	}

	return b.String()
}

// --- Per-session activity pages ---

var sanitizeSessionNameRe = regexp.MustCompile(`[^A-Za-z0-9._ -]`)

// sanitizeSessionFilename turns an agent name into a safe filename stem:
// "/" and any character outside [A-Za-z0-9._ -] become "-".
func sanitizeSessionFilename(name string) string {
	return sanitizeSessionNameRe.ReplaceAllString(name, "-")
}

// sessionDisplayName returns agentName, or (when sessionPagesDir is
// non-empty) a wikilink to that agent's session page aliased to the
// original name, for use in the dashboard's Sessions table.
func sessionDisplayName(agentName, sessionPagesDir string) string {
	if sessionPagesDir == "" {
		return agentName
	}
	folder := filepath.Base(sessionPagesDir)
	return fmt.Sprintf("[[%s/%s|%s]]", folder, sanitizeSessionFilename(agentName), agentName)
}

// formatEventTimestamp renders t as HH:MM, prefixed with the date when t
// isn't on the same day as now.
func formatEventTimestamp(t, now time.Time) string {
	if t.Format("2006-01-02") == now.Format("2006-01-02") {
		return t.Format("15:04")
	}
	return t.Format("2006-01-02 15:04")
}

// sessionEvent is one line in a session page's merged Activity feed.
type sessionEvent struct {
	At       time.Time
	Text     string
	Wikilink string
}

// buildSessionEvents merges an agent's `tn log` activity entries with the
// message events that involve it — received/acked messages addressed to it,
// and messages it sent — newest first, capped at 200.
func buildSessionEvents(agentName string, messages []Message, activity []ActivityEntry) []sessionEvent {
	var events []sessionEvent

	for _, a := range activity {
		if a.Agent != agentName {
			continue
		}
		events = append(events, sessionEvent{At: a.At, Text: "📝 " + a.Text})
	}

	for _, m := range messages {
		wikilink := taskWikilink(m.TaskPath)
		if m.To == agentName {
			switch m.Status {
			case "acked":
				at := m.CreatedAt
				if m.AckedAt != nil {
					at = *m.AckedAt
				}
				events = append(events, sessionEvent{
					At:       at,
					Text:     fmt.Sprintf("✅ acked: %s — response: %s", truncate(m.Text, 120), truncate(m.Response, 120)),
					Wikilink: wikilink,
				})
			case "queued", "delivered":
				events = append(events, sessionEvent{
					At:       m.CreatedAt,
					Text:     fmt.Sprintf("📨 received: %s", truncate(m.Text, 120)),
					Wikilink: wikilink,
				})
			}
		}
		if m.From == agentName {
			events = append(events, sessionEvent{
				At:       m.CreatedAt,
				Text:     fmt.Sprintf("📤 sent to %s: %s", m.To, truncate(m.Text, 120)),
				Wikilink: wikilink,
			})
		}
	}

	sort.Slice(events, func(i, j int) bool { return events[i].At.After(events[j].At) })
	if len(events) > 200 {
		events = events[:200]
	}
	return events
}

// sessionPageCandidates returns the agents that should get a rendered
// session page: alive, or with at least one message (as sender or
// recipient) or activity entry.
func sessionPageCandidates(agents []Agent, messages []Message, activity []ActivityEntry, now time.Time) []Agent {
	hasMessage := map[string]bool{}
	for _, m := range messages {
		if m.To != "" {
			hasMessage[m.To] = true
		}
		if m.From != "" {
			hasMessage[m.From] = true
		}
	}
	hasActivity := map[string]bool{}
	for _, a := range activity {
		hasActivity[a.Agent] = true
	}

	out := make([]Agent, 0, len(agents))
	for _, a := range agents {
		if agentAliveAt(a, now) || hasMessage[a.Name] || hasActivity[a.Name] {
			out = append(out, a)
		}
	}
	return out
}

// renderSessionPageMarkdown is a pure function from one agent's state plus
// its merged event feed to that agent's session page Markdown, so it's
// unit-testable without a running server.
func renderSessionPageMarkdown(a Agent, alive bool, events []sessionEvent, now time.Time, stuck stuckInfo) string {
	var b strings.Builder

	b.WriteString("---\ngenerated: true\n---\n\n")
	fmt.Fprintf(&b, "# %s\n\n", a.Name)

	status := sessionStatusDisplay(alive, stuck.Stuck, stuck.UnackedBacklog, !a.Accepting, stuck.RateLimited, stuck.RateLimitResetsAt) + contextPctSuffix(a.ContextPct)
	fmt.Fprintf(&b, "- Project: %s\n", a.Project)
	fmt.Fprintf(&b, "- Status: %s\n", status)
	fmt.Fprintf(&b, "- Registered: %s\n", formatEventTimestamp(a.RegisteredAt, now))
	fmt.Fprintf(&b, "- Last seen: %s\n", formatEventTimestamp(a.LastSeenAt, now))
	if session, ok := tmuxSessionForAgent(a); ok {
		fmt.Fprintf(&b, "- Attach: `tmux attach -t %s`\n", session)
	}

	if stuck.RateLimited {
		b.WriteString("\n## Waiting: claude.ai usage limit\n\n")
		if stuck.RateLimitResetsAt != "" {
			fmt.Fprintf(&b, "- Resets: %s\n", stuck.RateLimitResetsAt)
		}
		if !stuck.RateLimitSince.IsZero() {
			fmt.Fprintf(&b, "- Waiting since: %s\n", formatEventTimestamp(stuck.RateLimitSince, now))
		}
		b.WriteString("- The session continues automatically at reset; `/login` with another account in any Claude session continues it now.\n")
	}

	if stuck.Stuck {
		b.WriteString("\n## Stuck: permission prompt\n\n")
		b.WriteString("```\n" + stuck.Excerpt + "\n```\n")
	}

	b.WriteString("\n## Activity\n\n")
	if len(events) == 0 {
		b.WriteString("_No activity yet._\n")
	} else {
		for _, e := range events {
			line := fmt.Sprintf("- %s %s", formatEventTimestamp(e.At, now), e.Text)
			if e.Wikilink != "" {
				line += " (" + e.Wikilink + ")"
			}
			b.WriteString(line + "\n")
		}
	}

	return b.String()
}

// renderSessionPages snapshots state under the lock, renders outside the
// lock, and writes one file per candidate agent atomically. Write failures
// are logged once per distinct error and never crash the daemon.
func (s *Server) renderSessionPages() {
	s.mu.Lock()
	snap := s.snapshotLocked()
	dir := s.sessionPagesDir
	s.mu.Unlock()

	if dir == "" {
		return
	}

	now := time.Now()
	stuckByAgent := s.stuck.snapshot()
	for _, a := range sessionPageCandidates(snap.Agents, snap.Messages, snap.Activity, now) {
		events := buildSessionEvents(a.Name, snap.Messages, snap.Activity)
		md := renderSessionPageMarkdown(a, agentAliveAt(a, now), events, now, stuckByAgent[a.Name])
		path := filepath.Join(dir, sanitizeSessionFilename(a.Name)+".md")
		if err := atomicWriteFile(path, []byte(md)); err != nil {
			s.logSessionPageErrOnce(err)
		}
	}
}

func (s *Server) logSessionPageErrOnce(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg := err.Error()
	if s.lastSessionPageErr == msg {
		return
	}
	s.lastSessionPageErr = msg
	log.Printf("serve: failed to write session page: %v", err)
}

// --- Self-restart on binary change ---

const (
	binaryWatchInterval  = 10 * time.Second
	binaryWatchMaxErrors = 3
)

// resolveOwnBinaryPath resolves the real on-disk path of the currently
// running executable, following symlinks (e.g. ~/bin/tn -> the actual build
// output), so a `go build -o ~/bin/tn` rebuild is detected regardless of how
// the daemon was launched.
func resolveOwnBinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// binaryInfoChanged is a pure comparison: it reports whether b differs from
// a in a way that indicates the on-disk binary was rebuilt (different size
// or mtime). Either side may be nil (treated as "changed" unless both are).
func binaryInfoChanged(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return a != nil || b != nil
	}
	return a.Size() != b.Size() || !a.ModTime().Equal(b.ModTime())
}

// watchBinaryForSelfRestart starts a background watcher that exits the
// process when the on-disk binary changes, so launchd's KeepAlive brings the
// new build back up within ~binaryWatchInterval. A stat error only counts as
// "changed" after binaryWatchMaxErrors consecutive failures, so a transient
// rename window during `go build -o` doesn't trigger a spurious restart.
// Disabled entirely when TN_NO_SELFRESTART=1 (foreground/dev runs, tests,
// and smoke runs that start the real daemon should set this).
func (s *Server) watchBinaryForSelfRestart() {
	if os.Getenv("TN_NO_SELFRESTART") == "1" {
		return
	}

	path, err := resolveOwnBinaryPath()
	if err != nil {
		log.Printf("serve: could not resolve own binary path, self-restart watcher disabled: %v", err)
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		log.Printf("serve: could not stat own binary, self-restart watcher disabled: %v", err)
		return
	}

	go func() {
		ticker := time.NewTicker(binaryWatchInterval)
		defer ticker.Stop()

		consecutiveErrors := 0
		for range ticker.C {
			cur, err := os.Stat(path)
			if err != nil {
				consecutiveErrors++
				if consecutiveErrors < binaryWatchMaxErrors {
					continue
				}
			} else {
				consecutiveErrors = 0
				if !binaryInfoChanged(info, cur) {
					continue
				}
			}
			log.Printf("serve: binary changed, exiting for launchd restart")
			s.exitForSelfRestart(os.Exit)
		}
	}()
}

// exitForSelfRestart acquires s.mu — the SAME mutex every state-mutating
// handler holds for the duration of its own read-modify-write — before
// calling exit, so the self-restart watcher can never interrupt a
// half-applied mutation (e.g. an assignment marker set in memory a moment
// before saveLocked persists it): the watcher blocks here until whichever
// handler currently holds the lock finishes its own atomic Lock/mutate/
// saveLocked/Unlock cycle, and only then exits. Deliberately the cheapest
// correct fix, not a WAL/journal: saveLocked's atomicWriteFile is already
// tmp+rename and fully synchronous, so state.json itself was never at risk
// of a torn write — the actual gap was purely in exit timing relative to
// in-memory mutation, which a shared mutex closes completely. Never
// unlocks: the process is exiting. exit is injectable (the real caller
// passes os.Exit) so a test can verify the mutex-wait behavior without
// terminating the test process.
func (s *Server) exitForSelfRestart(exit func(code int)) {
	s.mu.Lock()
	exit(0)
}

// --- Config / entrypoint ---

type serveFileConfig struct {
	Port            int                       `json:"port"`
	Projects        map[string]ProjectConfig  `json:"projects"`
	DashboardPath   string                    `json:"dashboardPath,omitempty"`
	SessionPagesDir string                    `json:"sessionPagesDir,omitempty"`
	Credentials     *credentialsFileConfig    `json:"credentials,omitempty"`
	Worktrees       *worktreeReaperFileConfig `json:"worktrees,omitempty"`
	OrchestratorDoc string                    `json:"orchestratorDoc,omitempty"`
}

// credentialsFileConfig is serve.json's "credentials" block:
//
//	"credentials": {"autoSwap": true, "minSwapInterval": "10m"}
type credentialsFileConfig struct {
	AutoSwap        bool   `json:"autoSwap"`
	MinSwapInterval string `json:"minSwapInterval,omitempty"`
}

// resolveServeConfig applies precedence: --port flag > env TN_BRIDGE_PORT >
// ~/.config/tn/serve.json "port" > default 8391. A missing config file
// yields an empty project map (autoSpawn defaults false) and dashboard
// disabled.
func resolveServeConfig(portFlag int) ServeConfig {
	cfg := ServeConfig{Port: 8391, Projects: map[string]ProjectConfig{},
		Credentials: CredentialsConfig{MinSwapInterval: credsDefaultMinSwapInterval},
		Worktrees:   defaultWorktreeReaperConfig()}

	if home, err := os.UserHomeDir(); err == nil {
		path := filepath.Join(home, ".config", "tn", "serve.json")
		if b, err := os.ReadFile(path); err == nil {
			var fc serveFileConfig
			if err := json.Unmarshal(b, &fc); err == nil {
				if fc.Port != 0 {
					cfg.Port = fc.Port
				}
				for k, v := range fc.Projects {
					// slugs are matched lowercase (see normalizeProjectSlug)
					cfg.Projects[strings.ToLower(k)] = v
				}
				cfg.DashboardPath = fc.DashboardPath
				cfg.SessionPagesDir = fc.SessionPagesDir
				cfg.OrchestratorDoc = fc.OrchestratorDoc
				cfg.Worktrees.applyFile(fc.Worktrees)
				if fc.Credentials != nil {
					cfg.Credentials.AutoSwap = fc.Credentials.AutoSwap
					if d, err := time.ParseDuration(fc.Credentials.MinSwapInterval); err == nil && d > 0 {
						cfg.Credentials.MinSwapInterval = d
					}
				}
			}
		}
	}
	if v := os.Getenv("TN_BRIDGE_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Port = p
		}
	}
	if portFlag != 0 {
		cfg.Port = portFlag
	}
	// Default session pages next to the dashboard note when configured but
	// no explicit sessionPagesDir was given. Both empty = feature off.
	if cfg.SessionPagesDir == "" && cfg.DashboardPath != "" {
		cfg.SessionPagesDir = filepath.Join(filepath.Dir(cfg.DashboardPath), "Claude Sessions")
	}
	return cfg
}

func defaultStatePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "state.json"
	}
	return filepath.Join(home, ".local", "state", "tn", "state.json")
}

func newMux(s *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /agents/register", s.handleRegister)
	mux.HandleFunc("POST /agents/drain", s.handleDrain)
	mux.HandleFunc("POST /agents/context", s.handleContext)
	mux.HandleFunc("POST /agents/retire", s.handleRetire)
	mux.HandleFunc("POST /agents/approve", s.handleApprove)
	mux.HandleFunc("POST /hooks/permission-request", s.handleHookPermissionRequest)
	mux.HandleFunc("POST /hooks/permission-denied", s.handleHookPermissionDenied)
	mux.HandleFunc("POST /spawn/pause", s.handleSpawnPause)
	mux.HandleFunc("GET /spawn/pause", s.handleSpawnPause)
	mux.HandleFunc("POST /agents/deregister", s.handleDeregister)
	mux.HandleFunc("POST /agents/purge", s.handlePurgeAgent)
	mux.HandleFunc("GET /agents", s.handleListAgents)
	mux.HandleFunc("GET /inbox", s.handleInbox)
	mux.HandleFunc("POST /ack", s.handleAck)
	mux.HandleFunc("POST /messages", s.handleSendMessage)
	mux.HandleFunc("GET /messages", s.handleGetMessages)
	mux.HandleFunc("POST /activity", s.handleActivity)
	mux.HandleFunc("POST /workers/start", s.handleWorkerStart)
	mux.HandleFunc("POST /workers/end", s.handleWorkerEnd)
	mux.HandleFunc("POST /tasks/create", s.handleCreateTask)
	mux.HandleFunc("POST /tasks/close", s.handleCloseTask)
	mux.HandleFunc("GET /projects/env", s.handleProjectsEnv)
	mux.HandleFunc("GET /repos", s.handleRepoSettings)
	mux.HandleFunc("POST /webhooks/tasknotes", s.handleTaskNotesWebhook)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /creds", s.handleCredsList)
	mux.HandleFunc("POST /creds/swap", s.handleCredsSwap)
	mux.HandleFunc("POST /worktrees/reap", s.handleWorktreesReap)
	mux.HandleFunc("GET /worktrees", s.handleWorktreesGet)
	mux.HandleFunc("GET /ui/", s.handleUI)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("GET /feed", s.handleFeed)
	mux.HandleFunc("POST /reply", s.handleReply)
	return mux
}

// cmdServe implements `tn serve [--port N]`.
func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", 0, "port to listen on (default 8391, env TN_BRIDGE_PORT)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg := resolveServeConfig(*port)
	orchestratorDoc, orchestratorDocFound := resolveOrchestratorDoc(cfg)
	if !orchestratorDocFound {
		log.Printf("serve: ORCHESTRATOR.md not found (set TN_ORCHESTRATOR_DOC or serve.json orchestratorDoc); spawned orchestrators will get a prompt without the contract path")
	}
	log.Printf("serve: orchestrator contract: %s", orchestratorDoc)

	var srv *Server
	srv = newServer(defaultStatePath(), cfg, func(project, cwd string, env map[string]string) error {
		return defaultSpawnFunc(project, cwd, env, srv.sessionAlive, srv.orchestratorDoc)
	})
	srv.orchestratorDoc = orchestratorDoc

	leaveBackgroundTier()
	srv.creds = newCredsStore(securityCLIKeychain{}, defaultCredsMetaPath())
	srv.reaper = newWorktreeReaper(cfg.Worktrees)
	if cfg.Credentials.AutoSwap {
		log.Printf("serve: credential auto-swap ON (min interval %s) — a usage limit on any orchestrator swaps EVERY Claude session on this machine", cfg.Credentials.MinSwapInterval)
	}

	tnClient := NewClient(resolveConfig())
	srv.tnClient = tnClient
	go srv.repairPoisonedDedupState(tnClient)
	go srv.sweepOrphanedOwnershipDebt(tnClient, hasTmuxSessionReal)
	go srv.sweepStrandedMessagesAtStartup(tnClient, hasTmuxSessionReal)

	srv.startRenderers()
	srv.watchBinaryForSelfRestart()
	srv.startSpawnReconciler()
	srv.startStuckDetector()
	srv.startSleepDetector()
	if cfg.DashboardPath != "" {
		log.Printf("serve: rendering dashboard to %s", cfg.DashboardPath)
	}
	if cfg.SessionPagesDir != "" {
		log.Printf("serve: rendering session pages to %s", cfg.SessionPagesDir)
	}

	selfURL := fmt.Sprintf("http://localhost:%d/webhooks/tasknotes", cfg.Port)
	go srv.ensureWebhookRegistered(tnClient, selfURL)
	srv.startDueTaskScanner(tnClient)
	srv.startMRWatcher(tnClient)
	srv.startWorktreeReaper()

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	log.Printf("serve: listening on %s", addr)
	return http.ListenAndServe(addr, newMux(srv))
}
