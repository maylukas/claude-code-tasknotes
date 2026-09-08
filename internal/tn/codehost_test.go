package tn

import (
	"encoding/json"
	"testing"
)

// --- resolveCodeHost: URL-shape detection + override ---

// TestResolveCodeHost_DetectsByURLShape covers detection for gitlab.com, a
// self-hosted GitLab instance (host-agnostic — only the path shape
// matters), github.com, an unsupported URL shape, and an override that
// wins regardless of the URL's shape.
func TestResolveCodeHost_DetectsByURLShape(t *testing.T) {
	cases := []struct {
		name         string
		url          string
		override     string
		wantProvider string
		wantErr      bool
	}{
		{"gitlab.com", "https://gitlab.com/team/project/-/merge_requests/5", "", "gitlab", false},
		{"self-hosted gitlab", "https://gitlab.example.com/team/subgroup/project/-/merge_requests/5", "", "gitlab", false},
		{"github.com", "https://github.com/acme/widgets/pull/9", "", "github", false},
		{"unsupported shape", "https://bitbucket.example.com/projects/ABC/repos/xyz/pull-requests/9", "", "", true},
		{"override wins over URL shape", "https://gitlab.example.com/team/project/-/merge_requests/5", "github", "github", false},
		{"override case-insensitive", "https://github.com/acme/widgets/pull/9", "GitLab", "gitlab", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host, err := resolveCodeHost(c.url, c.override)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got host %v", host)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if host.Name() != c.wantProvider {
				t.Errorf("got provider %q, want %q", host.Name(), c.wantProvider)
			}
		})
	}
}

// TestCodeHostOverrideForTask_ForcesProviderForUnsupportedURL verifies a
// project's codeHost override (ProjectConfig.CodeHost) is read via the
// task's routed project, and that resolveCodeHost honors it even for a URL
// shape neither provider's Matches would otherwise accept — proving the
// override genuinely forces provider selection rather than merely breaking
// a tie between two matching providers.
func TestCodeHostOverrideForTask_ForcesProviderForUnsupportedURL(t *testing.T) {
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{"myapp": {CodeHost: "github"}})
	task := Task{Path: "Tasks/X.md", Projects: []string{"myapp"}}

	override := srv.codeHostOverrideForTask(task)
	if override != "github" {
		t.Fatalf("expected override %q, got %q", "github", override)
	}

	unsupportedURL := "https://bitbucket.example.com/projects/ABC/repos/xyz/pull-requests/9"
	if _, err := resolveCodeHost(unsupportedURL, ""); err == nil {
		t.Fatalf("expected the unsupported URL to fail detection with no override")
	}
	host, err := resolveCodeHost(unsupportedURL, override)
	if err != nil {
		t.Fatalf("expected the override to force a provider despite the URL shape: %v", err)
	}
	if host.Name() != "github" {
		t.Errorf("got provider %q, want %q", host.Name(), "github")
	}
}

// --- githubHost: state mapping ---

func fakeGhRunner(t *testing.T, wantArgsPrefix []string, response string, err error) func(args ...string) ([]byte, error) {
	t.Helper()
	return func(args ...string) ([]byte, error) {
		for i, want := range wantArgsPrefix {
			if i >= len(args) || args[i] != want {
				t.Fatalf("gh args %v: expected prefix %v", args, wantArgsPrefix)
			}
		}
		if err != nil {
			return nil, err
		}
		return []byte(response), nil
	}
}

func TestGithubHost_ChangeState_MapsGhStates(t *testing.T) {
	prURL := "https://github.com/acme/widgets/pull/42"
	cases := []struct {
		name string
		json string
		want string
	}{
		{"open", `{"state":"OPEN","mergedAt":null,"author":{"login":"agent-bot"}}`, "opened"},
		{"merged", `{"state":"MERGED","mergedAt":"2026-01-01T00:00:00Z","author":{"login":"agent-bot"}}`, "merged"},
		{"closed unmerged", `{"state":"CLOSED","mergedAt":null,"author":{"login":"agent-bot"}}`, "closed"},
		{"closed but mergedAt set", `{"state":"CLOSED","mergedAt":"2026-01-01T00:00:00Z","author":{"login":"agent-bot"}}`, "merged"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host := githubHost{runner: fakeGhRunner(t, []string{"pr", "view", prURL, "--json"}, c.json, nil)}
			got, err := host.ChangeState(prURL)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got state %q, want %q", got, c.want)
			}
		})
	}
}

func TestGithubHost_ChangeState_UnrecognizedStateErrors(t *testing.T) {
	prURL := "https://github.com/acme/widgets/pull/42"
	host := githubHost{runner: fakeGhRunner(t, nil, `{"state":"DRAFT","mergedAt":null}`, nil)}
	if _, err := host.ChangeState(prURL); err == nil {
		t.Fatalf("expected an error for an unrecognized gh state")
	}
}

// --- githubHost: discussions parsing ---

const cannedGithubGraphQLResponse = `{
  "data": {
    "repository": {
      "pullRequest": {
        "author": {"login": "pr-author"},
        "reviewThreads": {
          "nodes": [
            {
              "isResolved": true,
              "comments": {"nodes": [
                {"databaseId": 101, "body": "looks fine now", "author": {"login": "reviewer1"}, "createdAt": "2026-01-01T00:00:00Z"}
              ]}
            },
            {
              "isResolved": false,
              "comments": {"nodes": [
                {"databaseId": 102, "body": "please fix this", "author": {"login": "reviewer2"}, "createdAt": "2026-01-02T00:00:00Z"}
              ]}
            }
          ]
        },
        "comments": {"nodes": [
          {"databaseId": 200, "body": "great PR overall", "author": {"login": "reviewer3"}, "createdAt": "2026-01-03T00:00:00Z"}
        ]}
      }
    }
  }
}`

// TestGithubHost_Discussions_ParsesReviewThreadsAndTopLevelComments verifies
// Discussions turns the GraphQL response into: the PR author, one
// mrDiscussions thread per review thread (Resolvable=true on every note,
// Resolved mirroring the thread's isResolved), and ONE extra thread for the
// PR's top-level comments with Resolvable=false on every note (so they
// never count toward the open-thread total, but still surface as "new
// reviewer activity").
func TestGithubHost_Discussions_ParsesReviewThreadsAndTopLevelComments(t *testing.T) {
	prURL := "https://github.com/acme/widgets/pull/42"
	host := githubHost{runner: fakeGhRunner(t, []string{"api", "graphql"}, cannedGithubGraphQLResponse, nil)}

	disc, err := host.Discussions(prURL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if disc.MRAuthor != "pr-author" {
		t.Errorf("got MRAuthor %q, want %q", disc.MRAuthor, "pr-author")
	}
	if len(disc.Threads) != 3 {
		t.Fatalf("expected 3 threads (2 review + 1 top-level), got %d: %+v", len(disc.Threads), disc.Threads)
	}

	resolved := disc.Threads[0]
	if len(resolved) != 1 || resolved[0].ID != 101 || !resolved[0].Resolvable || !resolved[0].Resolved || resolved[0].Author != "reviewer1" {
		t.Errorf("unexpected resolved thread: %+v", resolved)
	}
	unresolved := disc.Threads[1]
	if len(unresolved) != 1 || unresolved[0].ID != 102 || !unresolved[0].Resolvable || unresolved[0].Resolved || unresolved[0].Author != "reviewer2" {
		t.Errorf("unexpected unresolved thread: %+v", unresolved)
	}
	topLevel := disc.Threads[2]
	if len(topLevel) != 1 || topLevel[0].ID != 200 || topLevel[0].Resolvable || topLevel[0].Resolved || topLevel[0].Author != "reviewer3" {
		t.Errorf("expected the top-level comment thread to be non-resolvable, got %+v", topLevel)
	}
}

// TestGithubHost_Discussions_UnparseableURL verifies a non-PR URL is
// rejected before any gh call is attempted.
func TestGithubHost_Discussions_UnparseableURL(t *testing.T) {
	host := githubHost{runner: func(args ...string) ([]byte, error) {
		t.Fatalf("expected gh never called for an unparseable URL, got args %v", args)
		return nil, nil
	}}
	if _, err := host.Discussions("https://example.com/not/a/pr"); err == nil {
		t.Fatalf("expected an error for an unparseable PR URL")
	}
}

// TestParsePRURL covers owner/repo/number extraction, including a trailing
// slash and non-parseable input, mirroring TestParseMRURL's GitLab coverage.
func TestParsePRURL(t *testing.T) {
	cases := []struct {
		name      string
		url       string
		wantOwner string
		wantRepo  string
		wantNum   string
		wantOK    bool
	}{
		{"simple", "https://github.com/acme/widgets/pull/42", "acme", "widgets", "42", true},
		{"trailing slash", "https://github.com/acme/widgets/pull/42/", "acme", "widgets", "42", true},
		{"not a PR url", "https://github.com/acme/widgets/issues/42", "", "", "", false},
		{"non-numeric number", "https://github.com/acme/widgets/pull/abc", "", "", "", false},
		{"not a url", "::not a url::", "", "", "", false},
		{"empty", "", "", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			owner, repo, number, ok := parsePRURL(c.url)
			if owner != c.wantOwner || repo != c.wantRepo || number != c.wantNum || ok != c.wantOK {
				t.Errorf("parsePRURL(%q) = (%q, %q, %q, %v), want (%q, %q, %q, %v)",
					c.url, owner, repo, number, ok, c.wantOwner, c.wantRepo, c.wantNum, c.wantOK)
			}
		})
	}
}

// --- watcher end-to-end: provider-agnostic via a fake codeHost ---

// fakeCodeHost is a directly-injectable codeHost double for exercising
// checkMRStatesOnceReal (the production per-task host-resolution pass)
// without going through checkMRStatesOnce's mrStateFunc-shaped
// compatibility wrapper. Deliberately does NOT implement
// discussionsOptional (mr_watcher.go) — a real provider never does either.
type fakeCodeHost struct {
	name     string
	state    string
	stateErr error
	disc     mrDiscussions
	discErr  error
}

func (f *fakeCodeHost) Name() string        { return f.name }
func (f *fakeCodeHost) Matches(string) bool { return true }
func (f *fakeCodeHost) ChangeState(string) (string, error) {
	return f.state, f.stateErr
}
func (f *fakeCodeHost) Discussions(string) (mrDiscussions, error) {
	return f.disc, f.discErr
}

// TestCheckMRStatesOnceReal_GitHubProviderAutoDonesReviewTask is the
// end-to-end proof that the watcher is provider-agnostic: a review task
// whose MR/PR merges auto-dones exactly the same way whether the resolved
// host is GitLab or, here, a fake standing in for GitHub — the watcher
// itself never branches on provider.
func TestCheckMRStatesOnceReal_GitHubProviderAutoDonesReviewTask(t *testing.T) {
	prURL := "https://github.com/acme/widgets/pull/7"
	task := reviewTaskFixture(prURL)
	apiSrv, puts := fakeMRWatcherAPI(t, []Task{task}, nil, map[string]Task{reviewTaskPath: task})
	client := NewClient(Config{URL: apiSrv.URL})
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.tnClient = client

	fake := &fakeCodeHost{name: "github", state: "opened"}
	srv.codeHostFunc = func(changeURL, override string) (codeHost, error) { return fake, nil }

	srv.checkMRStatesOnceReal(client)
	if len(*puts) != 0 {
		t.Fatalf("expected no PUT on the opened seed, got %+v", *puts)
	}

	fake.state = "merged"
	srv.checkMRStatesOnceReal(client)
	if len(*puts) != 1 {
		t.Fatalf("expected exactly 1 PUT for the merge transition, got %+v", *puts)
	}
	if (*puts)[0]["status"] != "done" {
		t.Errorf("expected status done, got %+v", (*puts)[0])
	}
}

// TestCheckMRStatesOnceReal_UnsupportedURLSkipsTask verifies a task whose mr
// URL matches no provider (and has no override) is skipped without a PUT,
// and doesn't stop the pass from processing other tasks.
func TestCheckMRStatesOnceReal_UnsupportedURLSkipsTask(t *testing.T) {
	task := reviewTaskFixture("https://bitbucket.example.com/projects/ABC/repos/xyz/pull-requests/9")
	apiSrv, puts := fakeMRWatcherAPI(t, []Task{task}, nil, map[string]Task{reviewTaskPath: task})
	client := NewClient(Config{URL: apiSrv.URL})
	srv := newDueScannerTestServer(t, map[string]ProjectConfig{})
	srv.tnClient = client

	srv.checkMRStatesOnceReal(client)
	if len(*puts) != 0 {
		t.Errorf("expected no PUT for an unsupported change-request URL, got %+v", *puts)
	}
	srv.mu.Lock()
	_, seen := srv.state.MRStates[reviewTaskPath]
	srv.mu.Unlock()
	if seen {
		t.Errorf("expected MRStates untouched for an unsupported URL")
	}
}

// jsonRoundTrip is a small sanity check that githubReviewThreadsQuery's
// canned response fixture above is itself valid JSON — a broken fixture
// would otherwise fail with a confusing message deep inside
// TestGithubHost_Discussions_ParsesReviewThreadsAndTopLevelComments.
func TestCannedGithubGraphQLResponse_IsValidJSON(t *testing.T) {
	var v any
	if err := json.Unmarshal([]byte(cannedGithubGraphQLResponse), &v); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
}
