package tn

import (
	"fmt"
	"strings"
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
