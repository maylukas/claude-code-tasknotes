package tn

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"
)

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
