package tn

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

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
