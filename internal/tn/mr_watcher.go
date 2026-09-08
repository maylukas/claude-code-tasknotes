package tn

import (
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"
)

// --- MR watcher ---

const (
	// mrWatchInterval is how often the MR watcher checks tracked tasks.
	mrWatchInterval = 5 * time.Minute
	// mrWatchInitialDelay is the eager first-run delay after daemon
	// startup, mirroring the other background passes' eager-first-run
	// convention (a fresh daemon shouldn't wait a full interval to catch
	// an MR that merged/closed while it was down).
	mrWatchInitialDelay = 45 * time.Second
	// mrReviewRefreshEvery is how often (in checkMRStatesOnceCore passes)
	// the incremental path re-fetches discussions for every open MR in a
	// group, even ones the incremental listing found unchanged — belt and
	// braces against a review thread being resolved without the MR's own
	// updated_at moving in a way this watcher notices otherwise. At
	// mrWatchInterval (5min) this is ~30min.
	mrReviewRefreshEvery = 6
)

// buildMRWatchQuery builds the FilterQuery for the MR watcher: every
// non-archived, non-completed task, regardless of status — a task parked
// in needs-input or triage (very common: "fixed, MR open, waiting on the
// user" plus a question) must keep being polled too, or its MRStates entry
// never updates once the MR merges/closes. customProperties.mr isn't
// filtered server-side — the query just narrows to non-terminal tasks, and
// the mr-is-set check happens client-side per task after fetch, which is
// what actually bounds the glab call rate (only mr-bearing tasks reach
// glab at all).
func buildMRWatchQuery() filterNode {
	return filterNode{
		Type: "group", ID: "root", Conjunction: "and",
		Children: []filterNode{
			{Type: "condition", ID: "archived", Property: "archived", Operator: "is-not-checked"},
			{Type: "condition", ID: "mr-watch-not-completed", Property: "status.isCompleted", Operator: "is-not-checked"},
		},
		SortKey:       "dateModified",
		SortDirection: "desc",
	}
}

// mrStateFunc resolves a merge/pull-request URL to its current state
// ("opened"/"merged"/"closed"), injectable so tests never exec glab/gh. A
// codeHost's ChangeState method value satisfies this type directly.
type mrStateFunc func(mrURL string) (string, error)

// --- MR review-comment watching ---

// mrNote is one note (comment) within an MR/PR discussion thread, as
// returned by either provider's codeHost.Discussions. Parsed defensively —
// a missing/unexpected field just yields its zero value, never an error,
// since a discussions payload is expected to vary (a diff-note carries
// position data this watcher doesn't need at all, a system note carries no
// useful Author for our purposes, etc).
type mrNote struct {
	ID         int
	Author     string
	Body       string
	System     bool
	Resolvable bool
	Resolved   bool
	CreatedAt  time.Time
}

// mrDiscussions is one MR/PR's review-comment state as fetched by
// mrDiscussionsFunc: its author (an MR watcher note authored by the same
// user as the MR/PR itself is presumed to be the acting agent posting on
// the user's behalf — e.g. replying to its own thread — and never counts as
// new reviewer activity) plus every discussion thread, each thread an
// ordered slice of notes exactly as the provider returns them.
type mrDiscussions struct {
	MRAuthor string
	Threads  [][]mrNote
}

// mrDiscussionsFunc resolves a merge/pull-request URL to its discussion
// threads, injectable (like mrStateFunc) so tests never exec glab/gh. A
// codeHost's Discussions method value satisfies this type directly.
type mrDiscussionsFunc func(mrURL string) (mrDiscussions, error)

// mrReviewState is State.MRReviews' per-task value: enough to detect new
// reviewer activity and a change in open-thread count on the next pass
// without re-deriving it from scratch. See checkTaskMRReviews.
type mrReviewState struct {
	// LastNoteID is the highest note ID observed across every thread as of
	// the last pass (system notes and the MR author's own notes included —
	// this is a high-water mark over ALL note IDs, not just reviewer ones,
	// so a note ID space that isn't strictly increasing per-thread still
	// bounds "new" correctly).
	LastNoteID int `json:"lastNoteId"`
	// OpenThreads is the count of discussion threads whose first resolvable
	// note is unresolved, as of the last pass.
	OpenThreads int `json:"openThreads"`
	// LastNotifiedAt is when a reviewer-comment notification was last sent
	// for this task (zero value: never, including the silent first-seed).
	LastNotifiedAt time.Time `json:"lastNotifiedAt,omitempty"`
}

// collapseWhitespace joins s's whitespace-separated fields with a single
// space each — used to fold a (possibly multi-line) note body into one
// readable line for a bridge note / message.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// checkTaskMRReviews checks t's MR for new reviewer comments and a change
// in open-thread count, called by checkMRStatesOnce ONLY for a task whose
// MR state this pass observed as "opened" (merged/closed MRs don't need
// review-thread tracking — the MR itself is no longer actionable). Mirrors
// checkTaskMRState's seed-then-observe-changes shape: a task's first-ever
// observation seeds State.MRReviews silently (no note, no message — avoids
// a notification storm the moment this feature ships against every
// already-open MR), and a failed discussions fetch never mutates
// MRReviews, logged once per task per outage via mrReviewsWarnedOnce, so a
// later successful pass picks up cleanly from the last confirmed state —
// same contract as checkTaskMRState's glab-error handling.
func (s *Server) checkTaskMRReviews(client *Client, discussions mrDiscussionsFunc, t Task, mrURL string) {
	if os.Getenv("TN_NO_MRCOMMENTS") == "1" {
		return
	}

	disc, err := discussions(mrURL)
	if err != nil {
		s.mu.Lock()
		alreadyWarned := s.mrReviewsWarnedOnce[t.Path]
		s.mrReviewsWarnedOnce[t.Path] = true
		s.mu.Unlock()
		if !alreadyWarned {
			log.Printf("serve: mr watcher: failed to resolve MR discussions for %s (%s): %v", t.Path, mrURL, err)
		}
		return
	}
	s.mu.Lock()
	s.mrReviewsWarnedOnce[t.Path] = false
	s.mu.Unlock()

	// openThreads: a thread counts as open when its first resolvable note
	// (in thread order) is unresolved; a thread with no resolvable note at
	// all never counts as open. candidateNotes collects every non-system
	// note NOT authored by the MR author, across every thread, in API
	// order — the eventual "new" set is this filtered by note ID against
	// the previously-recorded high-water mark.
	openThreads := 0
	maxNoteID := 0
	var candidateNotes []mrNote
	for _, thread := range disc.Threads {
		var firstResolvable *mrNote
		for i := range thread {
			n := &thread[i]
			if n.ID > maxNoteID {
				maxNoteID = n.ID
			}
			if firstResolvable == nil && n.Resolvable {
				firstResolvable = n
			}
			if !n.System && n.Author != disc.MRAuthor {
				candidateNotes = append(candidateNotes, *n)
			}
		}
		if firstResolvable != nil && !firstResolvable.Resolved {
			openThreads++
		}
	}

	s.mu.Lock()
	prev, seen := s.state.MRReviews[t.Path]
	s.mu.Unlock()

	if !seen {
		s.mu.Lock()
		if s.state.MRReviews == nil {
			s.state.MRReviews = map[string]mrReviewState{}
		}
		s.state.MRReviews[t.Path] = mrReviewState{LastNoteID: maxNoteID, OpenThreads: openThreads}
		s.saveLocked()
		s.mu.Unlock()
		log.Printf("serve: mr watcher: seeded review state for %s (%d open thread(s))", t.Path, openThreads)
		return
	}

	var newNotes []mrNote
	for _, n := range candidateNotes {
		if n.ID > prev.LastNoteID {
			newNotes = append(newNotes, n)
		}
	}

	if len(newNotes) == 0 {
		// Threads may still have resolved/reopened with no NEW note driving
		// it (e.g. resolving via the "Resolve thread" button posts no
		// note), and the high-water mark itself may have advanced from a
		// system/MR-author note that never counts as "new" — persist both
		// unconditionally, same "always record what was actually observed"
		// convention as checkTaskMRState; only the note/message/activity
		// below is gated on genuine new reviewer activity.
		s.mu.Lock()
		s.state.MRReviews[t.Path] = mrReviewState{LastNoteID: maxNoteID, OpenThreads: openThreads, LastNotifiedAt: prev.LastNotifiedAt}
		s.saveLocked()
		s.mu.Unlock()
		return
	}

	sort.Slice(newNotes, func(i, j int) bool { return newNotes[i].ID < newNotes[j].ID })

	seenAuthor := map[string]bool{}
	var authors []string
	for _, n := range newNotes {
		if n.Author != "" && !seenAuthor[n.Author] {
			seenAuthor[n.Author] = true
			authors = append(authors, n.Author)
		}
	}
	firstBody := truncate(collapseWhitespace(newNotes[0].Body), 140)

	noteText := fmt.Sprintf("Review comments on MR (%s): %d new by %s, %d unresolved thread(s). First: %q",
		mrURL, len(newNotes), strings.Join(authors, ", "), openThreads, firstBody)
	if _, err := bridgeNoteTask(client, t.Path, noteText); err != nil {
		log.Printf("serve: mr watcher: failed to note review comments for %s: %v", t.Path, err)
		return
	}

	if project := routingSlugForTask(t); project != "" {
		s.dispatchMessage(sendRequest{
			Project:  project,
			TaskPath: t.Path,
			Text: fmt.Sprintf("Review comments on MR %s for %s (%s): %d new, %d unresolved thread(s). Address them, push, then reply/resolve on GitLab.",
				mrURL, t.Title, t.Path, len(newNotes), openThreads),
		})
	}

	s.mu.Lock()
	s.appendActivityLocked("bridge", fmt.Sprintf("Review comments on %s: %d new (%s)", t.Path, len(newNotes), mrURL), t.Path)
	s.state.MRReviews[t.Path] = mrReviewState{LastNoteID: maxNoteID, OpenThreads: openThreads, LastNotifiedAt: time.Now()}
	s.saveLocked()
	s.mu.Unlock()
	s.triggerRenders()
}

// startMRWatcher launches a background goroutine that checks merge/pull
// request state for every non-completed task with a customProperties.mr URL
// set, transitioning the task's status on an OBSERVED state change (see
// checkMRStateOnce). Disabled when TN_NO_MRWATCH=1 (tests and smoke runs
// that start the real daemon should set this).
func (s *Server) startMRWatcher(client *Client) {
	if os.Getenv("TN_NO_MRWATCH") == "1" {
		return
	}
	go func() {
		time.Sleep(mrWatchInitialDelay)
		s.checkMRStatesOnceReal(client)

		ticker := time.NewTicker(mrWatchInterval)
		defer ticker.Stop()
		for range ticker.C {
			s.checkMRStatesOnceReal(client)
		}
	}()
}

// checkMRStatesOnceReal is the production MR-watcher pass: for each task it
// resolves a codeHost (GitLab or GitHub, by URL shape or the task's
// project's codeHost override) via s.codeHostFunc, defaulting to
// resolveCodeHost.
func (s *Server) checkMRStatesOnceReal(client *Client) {
	s.checkMRStatesOnceCore(client, func(t Task, mrURL string) (codeHost, error) {
		hostFunc := s.codeHostFunc
		if hostFunc == nil {
			hostFunc = resolveCodeHost
		}
		return hostFunc(mrURL, s.codeHostOverrideForTask(t))
	})
}

// codeHostOverrideForTask returns the codeHost override configured for t's
// routed project (ProjectConfig.CodeHost), or "" when none is set / t
// doesn't route to a known project.
func (s *Server) codeHostOverrideForTask(t Task) string {
	slug := routingSlugForTask(t)
	if slug == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.projectConfigFor(strings.ToLower(slug)).CodeHost
}

// checkMRStatesOnce is a compatibility wrapper preserved for the ~30
// existing call sites (mr_watcher_test.go, mr_reviews_test.go) that inject
// an mrStateFunc directly and predate the multi-provider codeHost
// abstraction. It installs a funcCodeHost wrapping mrState (for
// ChangeState) and s.mrDiscussionsFunc, read fresh on each call (for
// Discussions) — exactly the glab-shaped single-provider pass those tests
// were written against.
func (s *Server) checkMRStatesOnce(client *Client, mrState mrStateFunc) {
	host := funcCodeHost{changeState: mrState, discussions: s.mrDiscussionsFunc}
	s.checkMRStatesOnceCore(client, func(Task, string) (codeHost, error) { return host, nil })
}

// mrGroupTask is one MR-bearing task queued for processing by
// checkMRStatesOnceCore, alongside the codeHost already resolved for it (so
// grouping/fallback never needs to re-resolve).
type mrGroupTask struct {
	task  Task
	mrURL string
	host  codeHost
}

// checkMRStatesOnceDiscussions runs t's review-comment check when host has
// discussions wired up (see discussionsOptional's doc comment) and its
// last-observed MRStates entry is "opened" — merged/closed MRs don't need
// thread tracking, and this always reads MRStates fresh rather than trusting
// a caller-computed value, so it's correct whether or not t's state was
// actually re-checked this pass (the incremental path's periodic refresh
// calls this for tasks it otherwise skipped entirely — see
// checkMRStatesOnceCore).
// It reports whether it actually fetched (for the incremental path's
// per-project log line) — false covers both "discussions not configured
// for this host" and "not currently opened".
func (s *Server) checkMRStatesOnceDiscussions(client *Client, mt mrGroupTask) bool {
	discussionsConfigured := true
	if do, ok := mt.host.(discussionsOptional); ok {
		discussionsConfigured = do.discussionsConfigured()
	}
	if !discussionsConfigured {
		return false
	}
	s.mu.Lock()
	state := s.state.MRStates[mt.task.Path]
	s.mu.Unlock()
	if state != "opened" {
		return false
	}
	s.checkTaskMRReviews(client, mt.host.Discussions, mt.task, mt.mrURL)
	return true
}

// checkMRStatesOnceIndividual is the original one-call-per-task path: used
// for every task whose codeHost doesn't support incremental listing (no
// incrementalCodeHost/projectPathHost), for every task when
// TN_NO_MRINCREMENTAL=1, and as the fallback for an entire incremental
// group when its ChangedSince call itself fails this pass (so a provider
// hiccup never hides a merge/close — see checkMRStatesOnceCore).
func (s *Server) checkMRStatesOnceIndividual(client *Client, mt mrGroupTask) {
	s.checkTaskMRState(client, mt.host.ChangeState, mt.task, mt.mrURL)
	s.checkMRStatesOnceDiscussions(client, mt)
}

// checkMRStatesOnceCore is one MR-watcher pass, factored out so it's
// directly testable with an injected host resolver and TaskNotes client (a
// fake httptest server) — tests never exec glab or gh.
//
// Tasks are split into two paths. A task whose resolved codeHost implements
// both incrementalCodeHost and projectPathHost (gitlabHost today — see
// codehost.go) is grouped with every other task sharing that
// "<codeHost.Name()>|<projectPath>" key and processed via ChangedSince: one
// listing call per project, then per-task work only for what the listing
// says changed plus any task never seen before (first observation always
// needs an individual ChangeState call regardless of the listing, so
// seeding still works for an MR that predates the watcher). Everything else
// — a non-incremental host, TN_NO_MRINCREMENTAL=1, or a group whose
// ChangedSince call itself failed — goes through
// checkMRStatesOnceIndividual exactly as before this feature existed.
func (s *Server) checkMRStatesOnceCore(client *Client, hostFor func(t Task, mrURL string) (codeHost, error)) {
	if client == nil {
		return
	}
	tasks, err := client.QueryTasksRaw(buildMRWatchQuery())
	if err != nil {
		log.Printf("serve: mr watcher: TaskNotes API query failed: %v", err)
		return
	}

	s.mu.Lock()
	s.mrWatchPassCount++
	refreshPass := s.mrWatchPassCount%mrReviewRefreshEvery == 0
	s.mu.Unlock()

	noIncremental := os.Getenv("TN_NO_MRINCREMENTAL") == "1"

	type incGroup struct {
		host        incrementalCodeHost
		projectPath string
		tasks       []mrGroupTask
	}
	groups := map[string]*incGroup{}
	var individual []mrGroupTask

	for _, t := range tasks {
		// Self-healing re-arm (see observeTaskStatusLocked): the MR watcher
		// observes every non-completed task every 5min, one more vantage
		// point from which a task's AssignedTasks marker should clear once
		// it's no longer open.
		s.mu.Lock()
		if s.observeTaskStatusLocked(t.Path, t.Status) {
			s.saveLocked()
		}
		s.mu.Unlock()

		mrURL := t.CustomProperties["mr"]
		if mrURL == "" {
			continue
		}

		host, err := hostFor(t, mrURL)
		if err != nil {
			s.mu.Lock()
			alreadyWarned := s.unsupportedHostWarnedOnce[t.Path]
			s.unsupportedHostWarnedOnce[t.Path] = true
			s.mu.Unlock()
			if !alreadyWarned {
				log.Printf("serve: mr watcher: %s (%s): %v", t.Path, mrURL, err)
			}
			continue
		}
		s.mu.Lock()
		s.unsupportedHostWarnedOnce[t.Path] = false
		s.mu.Unlock()

		mt := mrGroupTask{task: t, mrURL: mrURL, host: host}

		incHost, isInc := host.(incrementalCodeHost)
		pathHost, hasPath := host.(projectPathHost)
		if noIncremental || !isInc || !hasPath {
			individual = append(individual, mt)
			continue
		}
		projectPath, ok := pathHost.projectPathOf(mrURL)
		if !ok {
			individual = append(individual, mt)
			continue
		}
		key := host.Name() + "|" + projectPath
		g, exists := groups[key]
		if !exists {
			g = &incGroup{host: incHost, projectPath: projectPath}
			groups[key] = g
		}
		g.tasks = append(g.tasks, mt)
	}

	for key, g := range groups {
		s.mu.Lock()
		cursor := s.state.MRCursors[key]
		s.mu.Unlock()

		changes, newest, err := g.host.ChangedSince(g.projectPath, cursor)
		if err != nil {
			s.mu.Lock()
			alreadyWarned := s.mrCursorWarnedOnce[key]
			s.mrCursorWarnedOnce[key] = true
			s.mu.Unlock()
			if !alreadyWarned {
				log.Printf("serve: mr watcher: %s: incremental listing failed, falling back to per-task polling: %v", key, err)
			}
			individual = append(individual, g.tasks...)
			continue
		}
		s.mu.Lock()
		s.mrCursorWarnedOnce[key] = false
		newCursor := s.state.MRCursors[key]
		if newest.After(newCursor) {
			newCursor = newest
		}
		s.state.MRCursors[key] = newCursor
		s.saveLocked()
		s.mu.Unlock()

		changedCount, polledCount, discCount := 0, 0, 0
		for _, mt := range g.tasks {
			s.mu.Lock()
			_, seen := s.state.MRStates[mt.task.Path]
			s.mu.Unlock()

			processed := false
			switch listedState, inChanges := changes[mt.mrURL]; {
			case !seen:
				// First-ever observation always gets an individual
				// ChangeState call, whether or not the listing happened to
				// include it — seeding must work even for an MR that
				// predates this task's first appearance in the query.
				s.checkTaskMRState(client, mt.host.ChangeState, mt.task, mt.mrURL)
				polledCount++
				processed = true
			case inChanges:
				s.checkTaskMRState(client, func(string) (string, error) { return listedState, nil }, mt.task, mt.mrURL)
				changedCount++
				processed = true
			}

			if !processed && !refreshPass {
				continue // nothing changed, not a refresh pass: skip entirely
			}
			if s.checkMRStatesOnceDiscussions(client, mt) {
				discCount++
			}
		}
		log.Printf("serve: mr watcher: %s incremental: %d changed, %d polled individually, %d discussions fetched", g.projectPath, changedCount, polledCount, discCount)
	}

	for _, mt := range individual {
		s.checkMRStatesOnceIndividual(client, mt)
	}
}

// funcCodeHost is a codeHost test double built from a bare mrStateFunc /
// mrDiscussionsFunc pair — see checkMRStatesOnce's doc comment. It
// implements discussionsOptional so checkMRStatesOnceCore can tell whether
// discussions were actually wired up for this pass.
type funcCodeHost struct {
	changeState mrStateFunc
	discussions mrDiscussionsFunc
}

func (h funcCodeHost) Name() string                             { return "gitlab" }
func (h funcCodeHost) Matches(string) bool                      { return true }
func (h funcCodeHost) ChangeState(mrURL string) (string, error) { return h.changeState(mrURL) }
func (h funcCodeHost) Discussions(mrURL string) (mrDiscussions, error) {
	if h.discussions == nil {
		return mrDiscussions{}, fmt.Errorf("discussions not configured")
	}
	return h.discussions(mrURL)
}
func (h funcCodeHost) discussionsConfigured() bool { return h.discussions != nil }

// discussionsOptional is implemented only by funcCodeHost — a codeHost that
// may not have review-comment discussions wired up. Real providers
// (gitlabHost, githubHost) don't implement it, so a type assertion against
// this interface naturally treats them as always-capable. See
// checkMRStatesOnceCore.
type discussionsOptional interface {
	discussionsConfigured() bool
}

// checkTaskMRState resolves the current MR/PR state for one task and, if it
// represents a genuine change (or the one legitimate first-observation
// catch-up — see below), transitions the task accordingly, then records
// the observed state either way. A failed lookup (unparseable URL or a
// provider CLI error) never mutates MRStates — a later successful pass just
// picks up wherever the last confirmed state left off — and is logged once
// per task per outage via mrWatchWarnedOnce.
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
	// merged+{review,needs-input,triage}, which is a legitimate catch-up:
	// the user merged the MR before the watcher ever saw this task. This
	// matters even more for needs-input/triage than it did for review
	// alone: those statuses were never polled before this query widened to
	// every non-completed task, so their first-ever observation is exactly
	// the case a merged MR would otherwise sit unnoticed under.
	transition := (seen && state != prev) ||
		(!seen && state == "merged" && (t.Status == "review" || t.Status == "needs-input" || t.Status == "triage"))
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

// bridgeNoteTask is bridgeTransitionTask's status-preserving sibling:
// fetches path and appends a "bridge"-attributed note the same way, but
// PUTs only details — no status key at all — for cases where an MR
// outcome is worth recording on a task without touching its current status
// (e.g. a merge or unmerged close observed while the task is parked in
// needs-input or triage for an unrelated reason).
func bridgeNoteTask(client *Client, path, noteText string) (Task, error) {
	task, err := client.GetTask(path)
	if err != nil {
		return Task{}, err
	}
	entry := formatHistoryEntry("bridge", noteText, time.Now())
	newDetails := applyNoteBodyEdit(task.Details, func(nb *noteBody) {
		nb.History = append([]string{entry}, nb.History...)
	})
	return client.UpdateTask(path, map[string]any{"details": newDetails})
}

// transitionMRMerged handles t's MR merging, branching on t's current
// status:
//   - review/in-progress: auto-completes the task, then invokes
//     runUnblockPass directly (we already know the status change
//     succeeded, rather than waiting for a webhook/reconciler pass to
//     notice it independently).
//   - needs-input/triage: the task was parked for a reason unrelated to
//     the MR itself (an open question, a decision to make) — auto-closing
//     it would silently drop that. Instead this only appends a note; the
//     status and any ask block are left untouched.
//   - anything else (open, none, ""): the task was never assigned/being
//     worked in the first place — its MR state is still recorded by the
//     caller, just nothing to transition or note here.
func (s *Server) transitionMRMerged(client *Client, t Task, mrURL string) {
	switch t.Status {
	case "review", "in-progress":
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

	case "needs-input", "triage":
		note := fmt.Sprintf("MR merged (%s) — left in %s: the open question/decision may still be real, close it yourself once resolved", mrURL, t.Status)
		if _, err := bridgeNoteTask(client, t.Path, note); err != nil {
			log.Printf("serve: mr watcher: failed to note %s after MR merge: %v", t.Path, err)
			return
		}
		log.Printf("serve: task %s MR merged while %s (noted, not auto-closed)", t.Path, t.Status)

		s.mu.Lock()
		s.appendActivityLocked("bridge", fmt.Sprintf("MR merged for %s while %s (not auto-closed): %s", t.Path, t.Status, mrURL), t.Path)
		s.saveLocked()
		s.mu.Unlock()
		s.triggerRenders()
	}
}

// transitionMRClosed handles an MR closing without merging, branching on
// t's current status:
//   - review: reopened to in-progress with a note (the note literally says
//     "reopened", which would be misleading for any other status).
//   - needs-input/triage: NOT reopened — reopening a task parked for an
//     unrelated open question/decision would silently override that. Only
//     a note is appended; status and any ask block are left untouched.
//   - in-progress/open: left alone entirely, as before.
//
// Either way, the task's owner (or fallback accepting agent, or the
// logical queue — see resolveTargetLocked) gets an informational message,
// since a closed-unmerged MR always needs a human or the owning agent to
// look at it regardless of the task's current status.
func (s *Server) transitionMRClosed(client *Client, t Task, mrURL string) {
	switch t.Status {
	case "review":
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

	case "needs-input", "triage":
		note := fmt.Sprintf("MR closed without merge (%s) — left in %s", mrURL, t.Status)
		if _, err := bridgeNoteTask(client, t.Path, note); err != nil {
			log.Printf("serve: mr watcher: failed to note %s after MR close: %v", t.Path, err)
		} else {
			log.Printf("serve: task %s MR closed unmerged while %s (noted)", t.Path, t.Status)
			s.mu.Lock()
			s.appendActivityLocked("bridge", fmt.Sprintf("MR closed unmerged for %s while %s (noted): %s", t.Path, t.Status, mrURL), t.Path)
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
