package tn

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

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
	// MRState is the MR watcher's last-observed GitLab MR state for this
	// task (see State.MRStates / dashboardSnapshot.MRStates) —
	// "opened"/"merged"/"closed", or "" if the task has no mr set or
	// hasn't been observed by the watcher yet. Lets the dashboard/webui
	// tell a task whose MR merged apart from one still open, instead of
	// showing only the bare mr URL.
	MRState string `json:"mrState,omitempty"`
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
			Ask: ab.Ask, Brief: ab.Brief, MRState: snap.MRStates[t.Path],
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
