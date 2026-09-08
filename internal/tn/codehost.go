package tn

import (
	"fmt"
	"strings"
	"time"
)

// codeHost is a merge/pull-request provider the MR watcher can talk to. Two
// implementations exist: gitlabHost (glab, codehost_gitlab.go) and
// githubHost (gh, codehost_github.go). The watcher (mr_watcher.go) resolves
// one per task via resolveCodeHost and never calls glab/gh directly itself.
type codeHost interface {
	// Name identifies the provider ("gitlab" or "github") — used for the
	// per-project codeHost override in ProjectConfig and for log lines.
	Name() string
	// Matches reports whether changeURL is a shape this provider handles
	// (a GitLab merge-request URL or a GitHub pull-request URL). Used by
	// resolveCodeHost's URL-shape detection when no override is given.
	Matches(changeURL string) bool
	// ChangeState resolves changeURL's current state: "opened", "merged",
	// or "closed".
	ChangeState(changeURL string) (state string, err error)
	// Discussions resolves changeURL's review-comment threads.
	Discussions(changeURL string) (mrDiscussions, error)
}

// incrementalCodeHost is implemented by providers that can list change
// requests updated since a cursor, so the MR watcher polls one endpoint per
// PROJECT per pass instead of one per MR-bearing TASK plus two more per
// open MR (state + discussions). Only gitlabHost implements it today —
// GitHub's equivalent would be `gh api search/issues?q=repo:...
// is:pr updated:>...`, left for a future change (see codehost_github.go).
// A host that doesn't implement this interface (or a task whose codeHost
// can't be grouped by project — see projectPathHost below) simply keeps
// today's one-call-per-task polling; see checkMRStatesOnceCore.
type incrementalCodeHost interface {
	codeHost
	// ChangedSince lists change requests of projectPath updated at or after
	// since (zero time = everything), returning their canonical web URLs
	// mapped to state ("opened"/"merged"/"closed") and the newest updated_at
	// seen across every item returned (zero time if none were returned) —
	// the watcher's next cursor for this project. A provider's own
	// query-parameter semantics (e.g. GitLab's second-granularity
	// updated_after) are this method's problem, not the caller's: the
	// caller always passes the raw watermark it wants "at or after".
	ChangedSince(projectPath string, since time.Time) (changes map[string]string, newest time.Time, err error)
}

// projectPathHost is implemented by a codeHost that can extract its own
// provider-specific project identifier from a change-request URL — the key
// checkMRStatesOnceCore groups MR-bearing tasks by before calling
// ChangedSince once per project instead of once per task. A separate,
// unexported interface (same pattern as discussionsOptional in
// mr_watcher.go) rather than folding this into incrementalCodeHost itself,
// since the project-path shape is entirely provider-specific (GitLab's
// group/subgroup/.../project vs. a future GitHub owner/repo) and callers
// only ever need it for grouping, never as part of the codeHost contract
// proper.
type projectPathHost interface {
	projectPathOf(changeURL string) (string, bool)
}

// codeHosts is every known provider, in detection order — checked in order
// by resolveCodeHost when no override is given. Order doesn't matter today
// (GitLab and GitHub URL shapes never overlap) but is fixed for determinism.
var codeHosts = []codeHost{gitlabHost{}, githubHost{}}

// resolveCodeHost picks the codeHost for changeURL: override ("gitlab" or
// "github", case-insensitive), when non-empty, always wins; otherwise the
// first provider in codeHosts whose Matches reports true. An unrecognized
// override falls through to detection (resolveServeConfig already warns and
// discards an unknown codeHost config value at load time, so this is a
// last-resort safety net, not the primary validation point). Returns an
// error when nothing matches either way.
func resolveCodeHost(changeURL string, override string) (codeHost, error) {
	switch strings.ToLower(strings.TrimSpace(override)) {
	case "gitlab":
		return gitlabHost{}, nil
	case "github":
		return githubHost{}, nil
	}
	for _, h := range codeHosts {
		if h.Matches(changeURL) {
			return h, nil
		}
	}
	return nil, fmt.Errorf("unsupported change-request URL: %s", changeURL)
}
