package tn

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

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
