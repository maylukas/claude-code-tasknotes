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

// gitlabHost is the codeHost implementation backed by the glab CLI. Zero
// value only — glab isn't injectable at the process level (unlike
// githubHost's runner field); existing tests inject at the mrStateFunc /
// mrDiscussionsFunc level instead (see checkMRStatesOnce's compatibility
// wrapper in mr_watcher.go), so glabMRState/glabMRDiscussions never
// exec glab in a test.
type gitlabHost struct{}

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
