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

// --- GitHub code host provider (gh) ---

// githubHost is the codeHost implementation backed by the gh CLI. runner,
// when set, replaces the real `gh` invocation — tests inject a fake here so
// no test ever spawns a process (same convention as the spawnFunc injection
// used elsewhere in this package). The zero value githubHost{} (used by
// resolveCodeHost/codeHosts) runs the real binary.
type githubHost struct {
	runner func(args ...string) ([]byte, error)
}

func (githubHost) Name() string { return "github" }

// Matches reports whether changeURL is a GitHub pull-request URL — a path
// of the form /<owner>/<repo>/pull/<n>.
func (githubHost) Matches(changeURL string) bool {
	_, _, _, ok := parsePRURL(changeURL)
	return ok
}

// parsePRURL extracts owner, repo, and PR number from a GitHub pull-request
// URL of the form https://<host>/<owner>/<repo>/pull/<number>. Mirrors
// parseMRURL's shape (codehost_gitlab.go) for symmetry: trailing slashes
// are tolerated, the number must be all-digits.
func parsePRURL(rawURL string) (owner, repo, number string, ok bool) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" {
		return "", "", "", false
	}
	owner, repo, number = parts[0], parts[1], parts[3]
	if owner == "" || repo == "" || number == "" {
		return "", "", "", false
	}
	for _, r := range number {
		if r < '0' || r > '9' {
			return "", "", "", false
		}
	}
	return owner, repo, number, true
}

// resolveGhBin finds gh by absolute path — same launchd-PATH reasoning as
// resolveGlabBin (codehost_gitlab.go) and resolveTmuxBin.
func resolveGhBin() string {
	for _, p := range []string{"/opt/homebrew/bin/gh", "/usr/local/bin/gh", "/usr/bin/gh"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "gh"
}

// run shells out to `gh <args>` (or calls h.runner, when injected for
// tests) and returns its stdout. Same HOME/env handling as the glab
// helpers, for the same launchd reason.
func (h githubHost) run(args ...string) ([]byte, error) {
	if h.runner != nil {
		return h.runner(args...)
	}
	cmd := exec.Command(resolveGhBin(), args...)
	cmd.Env = os.Environ()
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Env = append(cmd.Env, "HOME="+home)
	}
	return cmd.Output()
}

// ChangeState resolves changeURL's current state via `gh pr view`. mergedAt
// being non-null always means "merged" regardless of the reported state —
// gh reports a merged PR's state as MERGED, but this guards against any
// state string this doesn't recognize still carrying a merge timestamp.
func (h githubHost) ChangeState(changeURL string) (string, error) {
	out, err := h.run("pr", "view", changeURL, "--json", "state,mergedAt,author")
	if err != nil {
		return "", err
	}
	var resp struct {
		State    string `json:"state"`
		MergedAt string `json:"mergedAt"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", err
	}
	if resp.MergedAt != "" {
		return "merged", nil
	}
	switch strings.ToUpper(resp.State) {
	case "OPEN":
		return "opened", nil
	case "CLOSED":
		return "closed", nil
	case "MERGED":
		return "merged", nil
	}
	return "", fmt.Errorf("gh returned unrecognized state %q for %s", resp.State, changeURL)
}

// githubReviewThreadsQuery fetches everything Discussions needs in one
// GraphQL round trip: the PR author (to filter the agent's own notes out of
// "new reviewer activity", same purpose as glabMRDiscussions' resource
// call), every review thread with its resolution state and comments, and
// the PR's top-level (non-review) comments.
const githubReviewThreadsQuery = `
query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      author { login }
      reviewThreads(first: 100) {
        nodes {
          isResolved
          comments(first: 100) {
            nodes { databaseId body author { login } createdAt }
          }
        }
      }
      comments(first: 100) {
        nodes { databaseId body author { login } createdAt }
      }
    }
  }
}
`

// Discussions resolves changeURL's review threads via gh's GraphQL API.
// Each GitHub review thread becomes one mrDiscussions thread with
// Resolvable=true and Resolved set to the thread's isResolved on every
// note (GitHub resolves at the thread level, unlike GitLab's per-note
// Resolved/Resolvable fields, so every note in a thread shares the same
// resolution state). The PR's top-level comments (outside any review
// thread) are NOT resolvable on GitHub — they're appended as one extra
// thread with Resolvable=false on every note, so "new reviewer note"
// detection (checkTaskMRReviews) still sees them, but a top-level comment
// thread never counts toward the open-thread total (checkTaskMRReviews
// only counts a thread whose FIRST resolvable note is unresolved, and
// firstResolvable stays nil when every note in the thread has
// Resolvable=false).
func (h githubHost) Discussions(changeURL string) (mrDiscussions, error) {
	owner, repo, number, ok := parsePRURL(changeURL)
	if !ok {
		return mrDiscussions{}, fmt.Errorf("unparseable PR URL: %s", changeURL)
	}

	out, err := h.run("api", "graphql",
		"-f", "query="+githubReviewThreadsQuery,
		"-F", "owner="+owner,
		"-F", "name="+repo,
		"-F", "number="+number,
	)
	if err != nil {
		return mrDiscussions{}, err
	}

	type ghComment struct {
		DatabaseID int    `json:"databaseId"`
		Body       string `json:"body"`
		Author     struct {
			Login string `json:"login"`
		} `json:"author"`
		CreatedAt string `json:"createdAt"`
	}
	var resp struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					Author struct {
						Login string `json:"login"`
					} `json:"author"`
					ReviewThreads struct {
						Nodes []struct {
							IsResolved bool `json:"isResolved"`
							Comments   struct {
								Nodes []ghComment `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
					Comments struct {
						Nodes []ghComment `json:"nodes"`
					} `json:"comments"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return mrDiscussions{}, err
	}

	pr := resp.Data.Repository.PullRequest
	toNote := func(c ghComment, resolvable, resolved bool) mrNote {
		created, _ := time.Parse(time.RFC3339, c.CreatedAt) // zero value on parse failure — never fatal
		return mrNote{
			ID: c.DatabaseID, Author: c.Author.Login, Body: c.Body,
			System: false, Resolvable: resolvable, Resolved: resolved,
			CreatedAt: created,
		}
	}

	out2 := mrDiscussions{MRAuthor: pr.Author.Login}
	for _, rt := range pr.ReviewThreads.Nodes {
		thread := make([]mrNote, 0, len(rt.Comments.Nodes))
		for _, c := range rt.Comments.Nodes {
			thread = append(thread, toNote(c, true, rt.IsResolved))
		}
		out2.Threads = append(out2.Threads, thread)
	}
	if len(pr.Comments.Nodes) > 0 {
		thread := make([]mrNote, 0, len(pr.Comments.Nodes))
		for _, c := range pr.Comments.Nodes {
			thread = append(thread, toNote(c, false, false))
		}
		out2.Threads = append(out2.Threads, thread)
	}
	return out2, nil
}
