package tn

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
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
