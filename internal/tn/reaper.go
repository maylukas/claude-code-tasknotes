package tn

// Worktree reaper.
//
// Every worker the orchestrator spawns with isolation:"worktree" gets a git
// worktree under <repo>/.claude/worktrees/agent-<id>. The harness removes
// it only when it is UNCHANGED at exit — and every real worker commits, so
// in practice none are ever removed: 103 accumulated in myapp in
// two weeks (2026-09-03 census: 7 locked, 28 dirty, 28 with unpushed
// commits, 8 merged, 11 whose directory was already gone). ORCHESTRATOR.md
// makes teardown the orchestrator's job on task done; the count shows
// that does not happen either.
//
// The reaper is deliberately narrow. It only ever looks under
// .claude/worktrees/ (never the main worktree, never a session's
// scratchpad worktree), and it removes a worktree only when NOTHING can
// be lost:
//
//   - not locked by a live process (the harness locks with
//     "claude agent <name> (pid N ...)"; a dead pid is a stale lock and is
//     unlocked, a live one is skipped);
//   - older than minAge (directory mtime), so a worker between creation
//     and its first commit is never raced;
//   - clean: `git status --porcelain` empty, untracked files included;
//   - and its HEAD commit exists on SOME remote branch
//     (`git branch -r --contains HEAD`) — merged or not. A pushed branch
//     survives the worktree; an unpushed commit is the one thing a
//     worktree can uniquely hold, and that is never touched.
//
// The local branch is deleted only when it is merged into the default
// branch, with `git branch -d` (never -D). `git worktree remove` is never
// forced. Anything else — dirty, unpushed, locked, young, unparseable —
// is counted and reported, not acted on: the report is the product as
// much as the removal, so the census above stays honest over time.
//
// Default is dryRun:true. The first passes should be read in the log
// before anything is allowed to delete; turning dryRun off is a
// deliberate serve.json edit.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	worktreeReaperDefaultInterval = 30 * time.Minute
	worktreeReaperDefaultMinAge   = 2 * time.Hour
	worktreeReaperDefaultMaxPer   = 20
	worktreeReaperInitialDelay    = 2 * time.Minute
	// worktreeReaperPassBudget bounds one project's pass. Live incident
	// (2026-09-03): the first daemon pass over 104 worktrees took FIVE
	// HOURS — git children of a launchd agent run at background QoS,
	// where a `git status` over an 8k-file worktree measured 6s warm
	// (0.3s foreground) and far worse cold. Whatever is not reached
	// inside the budget is reported as skipped and picked up next pass.
	worktreeReaperPassBudget = 30 * time.Minute
	// worktreeReaperGitTimeout bounds any single git QUERY so one wedged
	// process (index lock, hung filesystem) cannot stall the sweep.
	worktreeReaperGitTimeout = 3 * time.Minute
	// worktreeReaperRemoveTimeout is the separate, much longer bound for
	// `git worktree remove`: it deletes the working tree itself, and a
	// fresh monorepo worktree carries a full node_modules — live
	// (2026-09-04) two removals were killed at 3 minutes mid-delete. A
	// killed remove is worse than a slow one: it leaves a half-deleted
	// tree that then reads as "dirty" and is never touched again.
	worktreeReaperRemoveTimeout = 20 * time.Minute
	// agentWorktreeDir is the only subtree the reaper ever considers,
	// relative to a project's cwd.
	agentWorktreeDir = ".claude/worktrees"
)

// WorktreeReaperConfig is serve.json's "worktrees" block:
//
//	"worktrees": {"reap": true, "dryRun": true, "minAge": "2h",
//	              "interval": "30m", "maxRemovePerPass": 20}
type WorktreeReaperConfig struct {
	Reap             bool          `json:"reap"`
	DryRun           bool          `json:"dryRun"`
	MinAge           time.Duration `json:"minAge"`
	Interval         time.Duration `json:"interval"`
	MaxRemovePerPass int           `json:"maxRemovePerPass"`
}

type worktreeReaperFileConfig struct {
	Reap             *bool  `json:"reap,omitempty"`
	DryRun           *bool  `json:"dryRun,omitempty"`
	MinAge           string `json:"minAge,omitempty"`
	Interval         string `json:"interval,omitempty"`
	MaxRemovePerPass *int   `json:"maxRemovePerPass,omitempty"`
}

func defaultWorktreeReaperConfig() WorktreeReaperConfig {
	return WorktreeReaperConfig{
		Reap:             false,
		DryRun:           true,
		MinAge:           worktreeReaperDefaultMinAge,
		Interval:         worktreeReaperDefaultInterval,
		MaxRemovePerPass: worktreeReaperDefaultMaxPer,
	}
}

func (c *WorktreeReaperConfig) applyFile(fc *worktreeReaperFileConfig) {
	if fc == nil {
		return
	}
	if fc.Reap != nil {
		c.Reap = *fc.Reap
	}
	if fc.DryRun != nil {
		c.DryRun = *fc.DryRun
	}
	if d, err := time.ParseDuration(fc.MinAge); err == nil && d >= 0 {
		c.MinAge = d
	}
	if d, err := time.ParseDuration(fc.Interval); err == nil && d > 0 {
		c.Interval = d
	}
	if fc.MaxRemovePerPass != nil && *fc.MaxRemovePerPass > 0 {
		c.MaxRemovePerPass = *fc.MaxRemovePerPass
	}
}

// gitRunFunc runs git in dir and returns stdout. Injectable so a test can
// stub it, though the reaper tests use real temporary repositories — the
// porcelain formats are the contract, and a stub would only restate our
// own assumptions about them.
type gitRunFunc func(dir string, args ...string) (string, error)

func resolveGitBin() string {
	for _, p := range []string{"/opt/homebrew/bin/git", "/usr/local/bin/git", "/usr/bin/git"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "git"
}

// taskpolicyBin (macOS) is used by leaveBackgroundTier at daemon startup —
// see that function; git children then inherit the daemon's normal tier.
const taskpolicyBin = "/usr/sbin/taskpolicy"

// leaveBackgroundTier moves THIS process out of PRIO_DARWIN_BG. A launchd
// agent's process and everything it spawns run in the background tier,
// where I/O is throttled hard: `git status` over an 8k-file worktree
// measured 0.23s in a foreground shell and 21–43s under `taskpolicy -b`
// (2026-09-04), and the first reaper pass over 104 worktrees took five
// hours. It is not only git: the same night the daemon's 2-minute orphan
// check ran 9 times in 9 hours — timers of a background-tier process are
// coalesced/deferred while the machine is idle, which quietly disables
// the liveness detection the daemon exists for. There is no per-child
// escape (`taskpolicy -B` only takes -p <pid>, and -c only clamps DOWN),
// so the daemon lifts itself once at startup; children inherit. Best
// effort: logs and carries on if taskpolicy is missing or refuses.
func leaveBackgroundTier() {
	if _, err := os.Stat(taskpolicyBin); err != nil {
		return
	}
	out, err := exec.Command(taskpolicyBin, "-B", "-p", strconv.Itoa(os.Getpid())).CombinedOutput()
	if err != nil {
		log.Printf("serve: could not leave background QoS tier (git sweeps and timers will be throttled): %v: %s", err, strings.TrimSpace(string(out)))
		return
	}
	log.Printf("serve: left background QoS tier (taskpolicy -B -p %d)", os.Getpid())
}

func gitRunReal(dir string, args ...string) (string, error) {
	timeout := worktreeReaperGitTimeout
	if len(args) >= 2 && args[0] == "worktree" && args[1] == "remove" {
		timeout = worktreeReaperRemoveTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, resolveGitBin(), args...)
	cmd.Dir = dir
	// Never let git block on a prompt from a daemon context. Optional
	// locks are deliberately NOT disabled: `git status` refreshing the
	// index is what makes the next pass fast, and every worktree we
	// touch is idle by construction.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return string(out), fmt.Errorf("git %s: timed out after %s", strings.Join(args, " "), timeout)
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return string(out), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return string(out), fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// worktreeEntry is one block of `git worktree list --porcelain`.
type worktreeEntry struct {
	Path       string
	Head       string
	Branch     string // short name, "" when detached
	Detached   bool
	Locked     bool
	LockReason string
	Prunable   bool
	Bare       bool
}

// parseWorktreeList parses `git worktree list --porcelain` output.
func parseWorktreeList(out string) []worktreeEntry {
	var entries []worktreeEntry
	var cur *worktreeEntry
	flush := func() {
		if cur != nil {
			entries = append(entries, *cur)
			cur = nil
		}
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur = &worktreeEntry{Path: strings.TrimPrefix(line, "worktree ")}
		case cur == nil:
			continue
		case strings.HasPrefix(line, "HEAD "):
			cur.Head = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			cur.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		case line == "detached":
			cur.Detached = true
		case line == "bare":
			cur.Bare = true
		case strings.HasPrefix(line, "locked"):
			cur.Locked = true
			cur.LockReason = strings.TrimSpace(strings.TrimPrefix(line, "locked"))
		case strings.HasPrefix(line, "prunable"):
			cur.Prunable = true
		}
	}
	flush()
	return entries
}

var lockPidRe = regexp.MustCompile(`\(pid (\d+)`)

// lockHolderAlive interprets the harness's lock reason ("claude agent
// <name> (pid N start ...)"). Returns (alive, known): known=false when
// the reason carries no pid at all — an unknown holder is treated as
// alive, never as stale.
func lockHolderAlive(reason string, pidAlive func(int) bool) (alive, known bool) {
	m := lockPidRe.FindStringSubmatch(reason)
	if m == nil {
		return true, false
	}
	pid, err := strconv.Atoi(m[1])
	if err != nil || pid <= 0 {
		return true, false
	}
	return pidAlive(pid), true
}

func pidAliveReal(pid int) bool {
	err := syscall.Kill(pid, 0)
	// EPERM = exists but not ours; still alive.
	return err == nil || errors.Is(err, syscall.EPERM)
}

// worktreeVerdict is why one worktree was or wasn't removed.
type worktreeVerdict string

const (
	verdictRemoved     worktreeVerdict = "removed"
	verdictRemovable   worktreeVerdict = "removable" // dry run, or over the per-pass cap
	verdictPruned      worktreeVerdict = "pruned"    // directory gone; admin entry cleaned
	verdictLocked      worktreeVerdict = "locked"
	verdictYoung       worktreeVerdict = "young"
	verdictDirty       worktreeVerdict = "dirty"
	verdictUnpushed    worktreeVerdict = "unpushed"
	verdictError       worktreeVerdict = "error"
	verdictSkipped     worktreeVerdict = "skipped" // pass time budget exhausted before this entry
	verdictNotAgentDir worktreeVerdict = "outside" // not under .claude/worktrees — never considered
)

type worktreeReport struct {
	Path      string          `json:"path"`
	Branch    string          `json:"branch,omitempty"`
	Verdict   worktreeVerdict `json:"verdict"`
	Merged    bool            `json:"merged"`
	StaleLock bool            `json:"staleLock,omitempty"`
	AgeHours  float64         `json:"ageHours,omitempty"`
	Note      string          `json:"note,omitempty"`
	Seconds   float64         `json:"seconds,omitempty"` // wall time spent on this entry
}

// reapPassReport is one project's pass — what /status and the log show.
type reapPassReport struct {
	Project   string           `json:"project"`
	Cwd       string           `json:"cwd"`
	At        time.Time        `json:"at"`
	DryRun    bool             `json:"dryRun"`
	Default   string           `json:"defaultBranch"`
	Scanned   int              `json:"scanned"`
	Removed   int              `json:"removed"`
	Removable int              `json:"removable"`
	Pruned    int              `json:"pruned"`
	Locked    int              `json:"locked"`
	Young     int              `json:"young"`
	Dirty     int              `json:"dirty"`
	Unpushed  int              `json:"unpushed"`
	Skipped   int              `json:"skipped"`
	Errors    int              `json:"errors"`
	Error     string           `json:"error,omitempty"`
	Duration  float64          `json:"durationSeconds"`
	Entries   []worktreeReport `json:"entries,omitempty"`
}

// worktreeReaper holds the reaper's dependencies and last reports.
type worktreeReaper struct {
	cfg      WorktreeReaperConfig
	git      gitRunFunc
	pidAlive func(int) bool
	now      func() time.Time
	mtime    func(path string) (time.Time, error)

	budget time.Duration

	mu      sync.Mutex
	last    map[string]*reapPassReport // by project
	lastRun time.Time
	running bool
}

func newWorktreeReaper(cfg WorktreeReaperConfig) *worktreeReaper {
	return &worktreeReaper{
		cfg:      cfg,
		git:      gitRunReal,
		pidAlive: pidAliveReal,
		now:      time.Now,
		mtime: func(p string) (time.Time, error) {
			st, err := os.Stat(p)
			if err != nil {
				return time.Time{}, err
			}
			return st.ModTime(), nil
		},
		last:   map[string]*reapPassReport{},
		budget: worktreeReaperPassBudget,
	}
}

// defaultBranchRef returns the remote ref the "merged" test compares
// against: origin/HEAD's target when set, else origin/main or
// origin/master if they exist, else "" (merged is then never true — the
// pushed-anywhere rule still applies).
func (r *worktreeReaper) defaultBranchRef(cwd string) string {
	if out, err := r.git(cwd, "symbolic-ref", "-q", "refs/remotes/origin/HEAD"); err == nil {
		ref := strings.TrimSpace(out) // refs/remotes/origin/development
		return strings.TrimPrefix(ref, "refs/remotes/")
	}
	for _, cand := range []string{"origin/main", "origin/master", "origin/development"} {
		if _, err := r.git(cwd, "rev-parse", "--verify", "-q", cand+"^{commit}"); err == nil {
			return cand
		}
	}
	return ""
}

// reapProject runs one pass over cwd. dryRun overrides the config when
// non-nil (the manual endpoint/CLI use it).
func (r *worktreeReaper) reapProject(project, cwd string, dryRun bool) *reapPassReport {
	now := r.now()
	rep := &reapPassReport{Project: project, Cwd: cwd, At: now, DryRun: dryRun}
	if cwd == "" {
		rep.Error = "no cwd configured"
		return rep
	}
	// git prints real paths; macOS hides /var behind a symlink to
	// /private/var, so compare against the resolved cwd or nothing under
	// a symlinked project dir would ever match.
	realCwd := cwd
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		realCwd = resolved
	}
	agentRoot := filepath.Join(realCwd, agentWorktreeDir)
	out, err := r.git(cwd, "worktree", "list", "--porcelain")
	if err != nil {
		rep.Error = err.Error()
		return rep
	}
	rep.Default = r.defaultBranchRef(cwd)
	entries := parseWorktreeList(out)

	budget := r.cfg.MaxRemovePerPass
	started := now
	for _, e := range entries {
		if e.Bare {
			continue
		}
		if r.now().Sub(started) > r.budget {
			// Budget exhausted: count the rest as skipped (only agent
			// worktrees — the path filter below is duplicated here so the
			// count is honest) and stop touching git.
			entryPath := e.Path
			if resolved, err := filepath.EvalSymlinks(e.Path); err == nil {
				entryPath = resolved
			}
			if rel, err := filepath.Rel(agentRoot, entryPath); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
				rep.Scanned++
				rep.Skipped++
				rep.Entries = append(rep.Entries, worktreeReport{Path: e.Path, Branch: e.Branch, Verdict: verdictSkipped, Note: "pass time budget exhausted"})
			}
			continue
		}
		entryStart := r.now()
		entryPath := e.Path
		if resolved, err := filepath.EvalSymlinks(e.Path); err == nil {
			entryPath = resolved
		}
		rel, relErr := filepath.Rel(agentRoot, entryPath)
		if relErr != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue // main worktree, scratchpad worktrees — never ours
		}
		rep.Scanned++
		wr := worktreeReport{Path: e.Path, Branch: e.Branch}
		record := func() {
			wr.Seconds = r.now().Sub(entryStart).Seconds()
			rep.Entries = append(rep.Entries, wr)
		}

		// Directory gone: `git worktree prune` is the only correct fix
		// and loses nothing (the admin entry points at nothing). Except
		// that prune SKIPS locked entries — so a lock whose holder is dead
		// is lifted first (apply mode), and a lock whose holder is alive
		// (or unknown) is reported as locked, not counted as pruned
		// forever (live: three such entries, 2026-09-04).
		if _, statErr := os.Stat(e.Path); e.Prunable || os.IsNotExist(statErr) {
			if e.Locked {
				alive, known := lockHolderAlive(e.LockReason, r.pidAlive)
				if alive || !known {
					wr.Verdict = verdictLocked
					wr.Note = "directory gone but still locked: " + e.LockReason
					rep.Locked++
					record()
					continue
				}
				wr.StaleLock = true
				wr.Note = "directory gone, stale lock lifted: " + e.LockReason
				if !dryRun {
					if _, err := r.git(cwd, "worktree", "unlock", e.Path); err != nil {
						wr.Verdict, wr.Note = verdictError, err.Error()
						rep.Errors++
						record()
						continue
					}
				}
			}
			wr.Verdict = verdictPruned
			rep.Pruned++
			record()
			continue
		}

		if e.Locked {
			alive, known := lockHolderAlive(e.LockReason, r.pidAlive)
			if alive || !known {
				wr.Verdict = verdictLocked
				wr.Note = e.LockReason
				rep.Locked++
				rep.Entries = append(rep.Entries, wr)
				continue
			}
			wr.StaleLock = true
			wr.Note = "stale lock: " + e.LockReason
		}

		if mt, err := r.mtime(e.Path); err == nil {
			wr.AgeHours = now.Sub(mt).Hours()
			if now.Sub(mt) < r.cfg.MinAge {
				wr.Verdict = verdictYoung
				rep.Young++
				rep.Entries = append(rep.Entries, wr)
				continue
			}
		}

		st, err := r.git(e.Path, "status", "--porcelain", "--untracked-files=normal")
		if err != nil {
			wr.Verdict, wr.Note = verdictError, err.Error()
			rep.Errors++
			record()
			continue
		}
		if strings.TrimSpace(st) != "" {
			wr.Verdict = verdictDirty
			wr.Note = fmt.Sprintf("%d changed/untracked path(s)", len(strings.Split(strings.TrimRight(st, "\n"), "\n")))
			rep.Dirty++
			record()
			continue
		}

		if rep.Default != "" {
			if _, err := r.git(cwd, "merge-base", "--is-ancestor", e.Head, rep.Default); err == nil {
				wr.Merged = true
			}
		}
		if !wr.Merged {
			remotes, err := r.git(cwd, "branch", "-r", "--contains", e.Head)
			if err != nil {
				wr.Verdict, wr.Note = verdictError, err.Error()
				rep.Errors++
				rep.Entries = append(rep.Entries, wr)
				continue
			}
			if strings.TrimSpace(remotes) == "" {
				wr.Verdict = verdictUnpushed
				if n, err := r.git(e.Path, "rev-list", "--count", "@{u}..HEAD"); err == nil {
					wr.Note = strings.TrimSpace(n) + " commit(s) ahead of upstream"
				} else {
					wr.Note = "HEAD is on no remote branch"
				}
				rep.Unpushed++
				rep.Entries = append(rep.Entries, wr)
				continue
			}
		}

		// Removable.
		if dryRun || budget <= 0 {
			wr.Verdict = verdictRemovable
			rep.Removable++
			record()
			continue
		}
		if wr.StaleLock {
			if _, err := r.git(cwd, "worktree", "unlock", e.Path); err != nil {
				wr.Verdict, wr.Note = verdictError, err.Error()
				rep.Errors++
				rep.Entries = append(rep.Entries, wr)
				continue
			}
		}
		if _, err := r.git(cwd, "worktree", "remove", e.Path); err != nil {
			wr.Verdict, wr.Note = verdictError, err.Error()
			rep.Errors++
			record()
			continue
		}
		budget--
		wr.Verdict = verdictRemoved
		rep.Removed++
		if wr.Merged && e.Branch != "" {
			if _, err := r.git(cwd, "branch", "-d", e.Branch); err != nil {
				wr.Note = "worktree removed; branch kept: " + strings.TrimSpace(err.Error())
			} else {
				wr.Note = "worktree and merged branch removed"
			}
		} else if e.Branch != "" {
			wr.Note = "worktree removed; branch " + e.Branch + " kept (pushed, not merged)"
		}
		record()
	}

	if rep.Pruned > 0 && !dryRun {
		if _, err := r.git(cwd, "worktree", "prune"); err != nil {
			rep.Error = "prune: " + err.Error()
		}
	}
	sort.Slice(rep.Entries, func(i, j int) bool { return rep.Entries[i].Path < rep.Entries[j].Path })
	rep.Duration = r.now().Sub(now).Seconds()
	return rep
}

// runOnce sweeps every configured project with a cwd. Serialised: a pass
// can take a while on a repo with a hundred worktrees, and two overlapping
// passes would race each other's removals.
func (r *worktreeReaper) runOnce(projects map[string]ProjectConfig, dryRun bool) []*reapPassReport {
	if !r.tryStart() {
		return nil
	}
	return r.runOnceStarted(projects, dryRun)
}

// tryStart claims the running flag; false if a pass is already running.
func (r *worktreeReaper) tryStart() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return false
	}
	r.running = true
	return true
}

// runOnceStarted is runOnce after a successful tryStart; it always
// releases the running flag.
func (r *worktreeReaper) runOnceStarted(projects map[string]ProjectConfig, dryRun bool) []*reapPassReport {
	defer func() {
		r.mu.Lock()
		r.running = false
		r.lastRun = r.now()
		r.mu.Unlock()
	}()

	slugs := make([]string, 0, len(projects))
	for slug, pc := range projects {
		if pc.Cwd != "" {
			slugs = append(slugs, slug)
		}
	}
	sort.Strings(slugs)
	var reports []*reapPassReport
	for _, slug := range slugs {
		rep := r.reapProject(slug, projects[slug].Cwd, dryRun)
		r.mu.Lock()
		r.last[slug] = rep
		r.mu.Unlock()
		reports = append(reports, rep)
		r.logReport(rep)
	}
	return reports
}

func (r *worktreeReaper) logReport(rep *reapPassReport) {
	mode := "apply"
	if rep.DryRun {
		mode = "dry-run"
	}
	if rep.Error != "" {
		log.Printf("serve: worktree reaper [%s] %s: %s", mode, rep.Project, rep.Error)
	}
	if rep.Scanned == 0 && rep.Error != "" {
		return
	}
	log.Printf("serve: worktree reaper [%s] %s: scanned %d in %.0fs — removed %d, removable %d, pruned %d, locked %d, young %d, dirty %d, unpushed %d, skipped %d, errors %d (default %s)",
		mode, rep.Project, rep.Scanned, rep.Duration, rep.Removed, rep.Removable, rep.Pruned, rep.Locked, rep.Young, rep.Dirty, rep.Unpushed, rep.Skipped, rep.Errors, rep.Default)
	for _, e := range rep.Entries {
		switch e.Verdict {
		case verdictRemoved, verdictRemovable, verdictError:
			log.Printf("serve: worktree reaper   %s %s (%s) %s", e.Verdict, filepath.Base(e.Path), e.Branch, e.Note)
		}
	}
}

// snapshot returns the last report per project (copies). withEntries=false
// for /status (counts only); true for GET /worktrees and the CLI.
func (r *worktreeReaper) snapshot(withEntries bool) (reports map[string]*reapPassReport, lastRun time.Time, running bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]*reapPassReport, len(r.last))
	for k, v := range r.last {
		cp := *v
		if !withEntries {
			cp.Entries = nil
		}
		out[k] = &cp
	}
	return out, r.lastRun, r.running
}

// ---- daemon integration ------------------------------------------------

// startWorktreeReaper runs the periodic sweep (TN_NO_REAPER=1 disables;
// so does worktrees.reap=false, which is the default).
func (s *Server) startWorktreeReaper() {
	if s.reaper == nil || !s.reaper.cfg.Reap || os.Getenv("TN_NO_REAPER") == "1" {
		return
	}
	mode := "apply"
	if s.reaper.cfg.DryRun {
		mode = "dry-run"
	}
	log.Printf("serve: worktree reaper ON (%s, every %s, minAge %s, max %d removals/pass)", mode, s.reaper.cfg.Interval, s.reaper.cfg.MinAge, s.reaper.cfg.MaxRemovePerPass)
	go func() {
		time.Sleep(worktreeReaperInitialDelay)
		s.reaper.runOnce(s.config.Projects, s.reaper.cfg.DryRun)
		ticker := time.NewTicker(s.reaper.cfg.Interval)
		defer ticker.Stop()
		for range ticker.C {
			s.reaper.runOnce(s.config.Projects, s.reaper.cfg.DryRun)
		}
	}()
}

// statusWorktrees is /status.worktrees.
type statusWorktrees struct {
	Enabled  bool                       `json:"enabled"`
	DryRun   bool                       `json:"dryRun"`
	Running  bool                       `json:"running"`
	LastRun  *time.Time                 `json:"lastRunAt,omitempty"`
	Projects map[string]*reapPassReport `json:"projects"`
}

func (s *Server) worktreesStatus() *statusWorktrees {
	if s.reaper == nil {
		return nil
	}
	last, at, running := s.reaper.snapshot(false)
	out := &statusWorktrees{Enabled: s.reaper.cfg.Reap, DryRun: s.reaper.cfg.DryRun, Running: running, Projects: last}
	if !at.IsZero() {
		out.LastRun = &at
	}
	return out
}

// handleWorktreesReap implements POST /worktrees/reap {"project"?: slug,
// "apply"?: bool}: one pass now, full per-worktree entries in the reply.
// apply defaults to false — a manual call is a dry run unless it says
// otherwise, regardless of the daemon's own dryRun setting; apply:true
// is honoured even when the periodic reaper is off, so `tn worktrees
// --apply` works without enabling the background sweep.
func (s *Server) handleWorktreesReap(w http.ResponseWriter, r *http.Request) {
	if s.reaper == nil {
		http.Error(w, "worktree reaper unavailable", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Project string `json:"project"`
		Apply   bool   `json:"apply"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
	}
	projects := s.config.Projects
	if req.Project != "" {
		pc, ok := projects[strings.ToLower(req.Project)]
		if !ok {
			http.Error(w, "unknown project", http.StatusNotFound)
			return
		}
		projects = map[string]ProjectConfig{strings.ToLower(req.Project): pc}
	}
	// Asynchronous: a pass over a big repo can take minutes even at
	// default QoS, and an HTTP client holding a connection open for that
	// long is how the first version stranded its own CLI (10-minute
	// client timeout, pass still running server-side). Start it, return
	// 202, and let GET /worktrees (or /status.worktrees.running) report.
	if !s.reaper.tryStart() {
		http.Error(w, "a reaper pass is already running", http.StatusConflict)
		return
	}
	dry := !req.Apply
	go s.reaper.runOnceStarted(projects, dry)
	writeJSON(w, http.StatusAccepted, map[string]any{"started": true, "dryRun": dry})
}

// handleWorktreesGet implements GET /worktrees[?project=slug]: the last
// full report per project, entries included, plus whether a pass is
// running right now.
func (s *Server) handleWorktreesGet(w http.ResponseWriter, r *http.Request) {
	if s.reaper == nil {
		http.Error(w, "worktree reaper unavailable", http.StatusServiceUnavailable)
		return
	}
	reports, lastRun, running := s.reaper.snapshot(true)
	if p := strings.ToLower(r.URL.Query().Get("project")); p != "" {
		if rep, ok := reports[p]; ok {
			reports = map[string]*reapPassReport{p: rep}
		} else {
			reports = map[string]*reapPassReport{}
		}
	}
	out := map[string]any{"running": running, "reports": reports}
	if !lastRun.IsZero() {
		out["lastRunAt"] = lastRun
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- CLI: tn worktrees -------------------------------------------------

func cmdWorktrees(args []string) error {
	project := ""
	apply := false
	asJSON := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--project", "-p":
			if i+1 >= len(args) {
				return fmt.Errorf("--project needs a value")
			}
			project = args[i+1]
			i++
		case "--apply":
			apply = true
		case "--json":
			asJSON = true
		default:
			return fmt.Errorf("usage: tn worktrees [--project P] [--apply] [--json]")
		}
	}
	b := NewBridgeClient(resolveBridgeURL())
	if _, err := b.request("POST", "/worktrees/reap", map[string]any{"project": project, "apply": apply}); err != nil {
		var he *httpError
		if errors.As(err, &he) && he.Code == http.StatusConflict {
			fmt.Fprintln(os.Stderr, "tn: a reaper pass is already running — waiting for it")
		} else {
			return err
		}
	}
	// Poll until the pass finishes (a big repo takes minutes).
	q := ""
	if project != "" {
		q = "?project=" + url.QueryEscape(strings.ToLower(project))
	}
	var body []byte
	deadline := time.Now().Add(worktreeReaperPassBudget * 2)
	for {
		var err error
		body, err = b.request("GET", "/worktrees"+q, nil)
		if err != nil {
			return err
		}
		var probe struct {
			Running bool `json:"running"`
		}
		_ = json.Unmarshal(body, &probe)
		if !probe.Running {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("reaper pass still running after %s — check `tn-serve.log` or GET /worktrees later", worktreeReaperPassBudget*2)
		}
		time.Sleep(2 * time.Second)
	}
	if asJSON {
		printRawJSON(body)
		return nil
	}
	var res struct {
		Reports map[string]*reapPassReport `json:"reports"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return err
	}
	slugs := make([]string, 0, len(res.Reports))
	for slug := range res.Reports {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	anyDry := false
	for _, slug := range slugs {
		rep := res.Reports[slug]
		anyDry = anyDry || rep.DryRun
		mode := "APPLY"
		if rep.DryRun {
			mode = "dry run"
		}
		fmt.Printf("%s (%s) — %s\n", rep.Project, mode, rep.Cwd)
		if rep.Error != "" {
			fmt.Printf("  error: %s\n", rep.Error)
		}
		fmt.Printf("  scanned %d in %.0fs (%s): removed %d, removable %d, pruned %d, locked %d, young %d, dirty %d, unpushed %d, skipped %d, errors %d (default %s)\n",
			rep.Scanned, rep.Duration, rep.At.Local().Format("15:04"), rep.Removed, rep.Removable, rep.Pruned, rep.Locked, rep.Young, rep.Dirty, rep.Unpushed, rep.Skipped, rep.Errors, rep.Default)
		for _, e := range rep.Entries {
			if e.Verdict == verdictPruned || e.Verdict == verdictYoung {
				continue
			}
			flag := ""
			if e.Merged {
				flag = " merged"
			}
			fmt.Printf("  %-10s %-28s %-45s %5.0fh%s  %s\n", e.Verdict, filepath.Base(e.Path), e.Branch, e.AgeHours, flag, e.Note)
		}
	}
	if len(slugs) == 0 {
		fmt.Println("no report yet")
	} else if anyDry {
		fmt.Println("dry run — nothing was removed; re-run with --apply to remove the 'removable' entries")
	}
	return nil
}
