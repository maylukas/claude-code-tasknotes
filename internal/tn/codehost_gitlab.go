package tn

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

// --- GitLab code host provider (glab) ---

// gitlabHost is the codeHost implementation backed by the glab CLI.
// ChangeState and Discussions still call the free-function
// glabMRState/glabMRDiscussions below directly, not runner — existing
// tests inject at the mrStateFunc / mrDiscussionsFunc level instead (see
// checkMRStatesOnce's compatibility wrapper in mr_watcher.go), so those two
// methods never exec glab in a test regardless of runner. runner exists
// only for ChangedSince (added for incremental MR-watcher polling — see
// codehost.go's incrementalCodeHost): nil (the zero value, used by
// codeHosts/resolveCodeHost) runs the real glab binary via run() below;
// tests inject a fake there the same way githubHost's runner does.
type gitlabHost struct {
	runner func(args ...string) ([]byte, error)
}

func (gitlabHost) Name() string { return "gitlab" }

// Matches reports whether changeURL is a GitLab merge-request URL — a path
// containing "/-/merge_requests/<n>".
func (gitlabHost) Matches(changeURL string) bool {
	_, _, ok := parseMRURL(changeURL)
	return ok
}

func (gitlabHost) ChangeState(changeURL string) (string, error) {
	return glabMRState(changeURL)
}

func (gitlabHost) Discussions(changeURL string) (mrDiscussions, error) {
	return glabMRDiscussions(changeURL)
}

// projectPathOf extracts the glab "-R" project path from an MR URL — the
// same value ChangedSince needs to group tasks by project. Thin wrapper
// around parseMRURL for callers (checkMRStatesOnceCore, via the
// projectPathHost interface) that only need the project path, not the iid.
func (gitlabHost) projectPathOf(changeURL string) (string, bool) {
	project, _, ok := parseMRURL(changeURL)
	return project, ok
}

const (
	// glabMRListPerPage is ChangedSince's per-page size for `glab api
	// projects/:id/merge_requests`.
	glabMRListPerPage = 100
	// glabMRListMaxPages bounds ChangedSince's pagination so a project with
	// an unusually large number of updated MRs in one window can never make
	// a single watcher pass block indefinitely — a bounded, possibly
	// incomplete listing on a pathological project is preferable to an
	// unbounded one.
	glabMRListMaxPages = 10
)

// ChangedSince is incrementalCodeHost's GitLab implementation: `glab api
// projects/:id/merge_requests?state=all&order_by=updated_at&sort=asc&
// updated_after=...`, paginated (page= while a full page returns, capped at
// glabMRListMaxPages). updated_after is set to since minus one second when
// since is non-zero — GitLab's updated_after filter is inclusive, but its
// granularity is whole seconds, so without the second of slack an MR
// updated in the same second as the previous pass's watermark could be
// silently skipped. newest is computed purely from returned items' own
// updated_at (never defaulted to since/updated_after), so a call that finds
// nothing changed reports a zero newest — callers must treat that as "no
// advance", not regress their cursor to since-1s (see
// checkMRStatesOnceCore).
func (h gitlabHost) ChangedSince(projectPath string, since time.Time) (map[string]string, time.Time, error) {
	apiSince := since
	if !apiSince.IsZero() {
		apiSince = apiSince.Add(-time.Second)
	}

	changes := map[string]string{}
	var newest time.Time
	for page := 1; page <= glabMRListMaxPages; page++ {
		path := fmt.Sprintf("projects/%s/merge_requests?state=all&order_by=updated_at&sort=asc&per_page=%d&page=%d",
			url.PathEscape(projectPath), glabMRListPerPage, page)
		if !apiSince.IsZero() {
			path += "&updated_after=" + url.QueryEscape(apiSince.Format(time.RFC3339))
		}
		out, err := h.run("api", path)
		if err != nil {
			return nil, time.Time{}, err
		}
		var items []struct {
			WebURL    string `json:"web_url"`
			State     string `json:"state"`
			UpdatedAt string `json:"updated_at"`
		}
		if err := json.Unmarshal(out, &items); err != nil {
			return nil, time.Time{}, err
		}
		if len(items) == 0 {
			break
		}
		for _, mr := range items {
			state := mr.State
			if state == "locked" {
				state = "opened" // see gitlabHost.Discussions/glabMRState's own treatment elsewhere
			}
			changes[mr.WebURL] = state
			if t, err := time.Parse(time.RFC3339, mr.UpdatedAt); err == nil && t.After(newest) {
				newest = t
			}
		}
		if len(items) < glabMRListPerPage {
			break
		}
	}
	return changes, newest, nil
}

// run shells out to `glab <args>` (or calls h.runner, when injected for
// tests) and returns its stdout. Same HOME/env handling as glabMRState/
// runGlabAPI, for the same launchd reason. Used only by ChangedSince today
// — see the gitlabHost struct doc comment for why ChangeState/Discussions
// don't go through it.
func (h gitlabHost) run(args ...string) ([]byte, error) {
	if h.runner != nil {
		return h.runner(args...)
	}
	cmd := exec.Command(resolveGlabBin(), args...)
	cmd.Env = os.Environ()
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Env = append(cmd.Env, "HOME="+home)
	}
	return cmd.Output()
}

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
