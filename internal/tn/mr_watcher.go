package tn

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
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

// --- MR review-comment watching ---

// mrNote is one note (comment) within a GitLab MR discussion thread, as
// returned by GET /projects/:id/merge_requests/:iid/discussions. Parsed
// defensively — a missing/unexpected field just yields its zero value,
// never an error, since a discussions payload is expected to vary (a
// diff-note carries position data this watcher doesn't need at all, a
// system note carries no useful Author for our purposes, etc).
type mrNote struct {
	ID         int
	Author     string
	Body       string
	System     bool
	Resolvable bool
	Resolved   bool
	CreatedAt  time.Time
}

// mrDiscussions is one MR's review-comment state as fetched by
// mrDiscussionsFunc: its author (an MR watcher note authored by the same
// user as the MR itself is presumed to be the acting agent posting on the
// user's behalf — e.g. replying to its own thread — and never counts as
// new reviewer activity) plus every discussion thread, each thread an
// ordered slice of notes exactly as GitLab returns them.
type mrDiscussions struct {
	MRAuthor string
	Threads  [][]mrNote
}

// mrDiscussionsFunc resolves a GitLab MR URL to its discussion threads,
// injectable (like mrStateFunc) so tests never exec glab.
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

// mrDiscussionsAPIPath and mrResourceAPIPath build glab's "api" subcommand
// path for an MR's discussions / the MR resource itself, from mrURL —
// factored out from glabMRDiscussions so the URL-building and project-path
// escaping is directly unit-testable without exec'ing glab. per_page=100
// bounds the discussions fetch to a first-page snapshot (a GitLab MR with
// more than 100 discussion THREADS, not notes, in flight is vanishingly
// rare, and the watcher only needs a bounded per-pass read, not exhaustive
// pagination).
func mrDiscussionsAPIPath(mrURL string) (string, error) {
	project, iid, ok := parseMRURL(mrURL)
	if !ok {
		return "", fmt.Errorf("unparseable MR URL: %s", mrURL)
	}
	return fmt.Sprintf("projects/%s/merge_requests/%s/discussions?per_page=100", url.PathEscape(project), iid), nil
}

func mrResourceAPIPath(mrURL string) (string, error) {
	project, iid, ok := parseMRURL(mrURL)
	if !ok {
		return "", fmt.Errorf("unparseable MR URL: %s", mrURL)
	}
	return fmt.Sprintf("projects/%s/merge_requests/%s", url.PathEscape(project), iid), nil
}

// runGlabAPI shells out to `glab api <path>` and decodes the JSON response
// into out. Same HOME/env handling as glabMRState, for the same launchd
// reason.
func runGlabAPI(path string, out any) error {
	cmd := exec.Command(resolveGlabBin(), "api", path)
	cmd.Env = os.Environ()
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Env = append(cmd.Env, "HOME="+home)
	}
	b, err := cmd.Output()
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// glabMRDiscussions is mrDiscussionsFunc's real implementation. It makes
// TWO glab calls: one for the MR resource itself (to learn its author's
// username) and one for its discussions. This — rather than extending
// glabMRState's JSON parsing to also capture author.username, the other
// option raised when this was designed — was chosen because mrStateFunc's
// (string, error) signature is used pervasively (checkTaskMRState and
// every existing MR-watcher test): widening it to also return an author
// would ripple through all of them for a value only the review-comment
// path needs. An isolated second call keeps that blast radius to this one
// function.
func glabMRDiscussions(mrURL string) (mrDiscussions, error) {
	discPath, err := mrDiscussionsAPIPath(mrURL)
	if err != nil {
		return mrDiscussions{}, err
	}
	resourcePath, err := mrResourceAPIPath(mrURL)
	if err != nil {
		return mrDiscussions{}, err
	}

	var resource struct {
		Author struct {
			Username string `json:"username"`
		} `json:"author"`
	}
	if err := runGlabAPI(resourcePath, &resource); err != nil {
		return mrDiscussions{}, err
	}

	var raw []struct {
		Notes []struct {
			ID         int    `json:"id"`
			Body       string `json:"body"`
			System     bool   `json:"system"`
			Resolvable bool   `json:"resolvable"`
			Resolved   bool   `json:"resolved"`
			Author     struct {
				Username string `json:"username"`
			} `json:"author"`
			CreatedAt string `json:"created_at"`
		} `json:"notes"`
	}
	if err := runGlabAPI(discPath, &raw); err != nil {
		return mrDiscussions{}, err
	}

	out := mrDiscussions{MRAuthor: resource.Author.Username}
	for _, d := range raw {
		thread := make([]mrNote, 0, len(d.Notes))
		for _, n := range d.Notes {
			created, _ := time.Parse(time.RFC3339, n.CreatedAt) // zero value on parse failure — never fatal
			thread = append(thread, mrNote{
				ID: n.ID, Author: n.Author.Username, Body: n.Body,
				System: n.System, Resolvable: n.Resolvable, Resolved: n.Resolved,
				CreatedAt: created,
			})
		}
		out.Threads = append(out.Threads, thread)
	}
	return out, nil
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

// startMRWatcher launches a background goroutine that checks GitLab MR
// state for every non-completed task with a customProperties.mr URL set,
// transitioning the task's status on an OBSERVED state change (see
// checkMRStateOnce). Disabled when TN_NO_MRWATCH=1 (tests and smoke runs
// that start the real daemon should set this).
func (s *Server) startMRWatcher(client *Client) {
	if os.Getenv("TN_NO_MRWATCH") == "1" {
		return
	}
	// Set once, before the ticker goroutine is spawned below — the `go`
	// statement itself is a happens-before edge (Go memory model), so no
	// separate synchronization is needed for the goroutine's later reads.
	s.mrDiscussionsFunc = glabMRDiscussions
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
		s.checkTaskMRState(client, mrState, t, mrURL)

		// Review-comment watching only makes sense for an MR that's still
		// open — merged/closed MRs don't need thread tracking. s.mrDiscussionsFunc
		// is nil for most existing tests (which predate this feature and
		// never set it, same convention as s.tnClient) — treated as "review
		// watching not wired up this pass" rather than a nil-func call.
		if s.mrDiscussionsFunc != nil {
			s.mu.Lock()
			state := s.state.MRStates[t.Path]
			s.mu.Unlock()
			if state == "opened" {
				s.checkTaskMRReviews(client, s.mrDiscussionsFunc, t, mrURL)
			}
		}
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
