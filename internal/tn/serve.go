// Daemon for `tn serve`: an HTTP bridge between the TaskNotes plugin and
// Claude Code agent sessions running on this machine. It keeps a registry of
// agent sessions per project, a message queue consumed via inbox
// long-polling, and reacts to TaskNotes webhooks by routing task assignments
// and messages to the right agent session.
package tn

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
