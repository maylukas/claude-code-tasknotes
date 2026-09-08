package tn

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The reaper tests build REAL git repositories in a temp dir: a bare
// "origin", a clone with a default branch, and agent worktrees in every
// state the reaper distinguishes. No stubbed porcelain — the formats are
// the contract.

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

type reaperFixture struct {
	root   string // the clone (project cwd)
	origin string
	agents string // <root>/.claude/worktrees
}

func newReaperFixture(t *testing.T) *reaperFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	gitT(t, base, "init", "--bare", "-b", "development", origin)
	// Seed the bare origin from a throwaway clone (an empty bare repo has
	// no branch to clone -b from), then clone the project cwd properly.
	seed := filepath.Join(base, "seed")
	gitT(t, base, "init", "-q", "-b", "development", seed)
	if err := os.WriteFile(filepath.Join(seed, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, seed, "add", "README")
	gitT(t, seed, "commit", "-q", "-m", "init")
	gitT(t, seed, "push", "-q", origin, "development")
	root := filepath.Join(base, "repo")
	gitT(t, base, "clone", "-q", "-b", "development", origin, root)
	gitT(t, root, "remote", "set-head", "origin", "development")
	agents := filepath.Join(root, ".claude", "worktrees")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	return &reaperFixture{root: root, origin: origin, agents: agents}
}

// addWorktree creates .claude/worktrees/<name> on a new branch with one
// commit, returns its path.
func (f *reaperFixture) addWorktree(t *testing.T, name, branch string) string {
	t.Helper()
	wt := filepath.Join(f.agents, name)
	gitT(t, f.root, "worktree", "add", "-q", "-b", branch, wt, "development")
	if err := os.WriteFile(filepath.Join(wt, name+".txt"), []byte(name), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, wt, "add", ".")
	gitT(t, wt, "commit", "-q", "-m", "work in "+name)
	return wt
}

func newTestReaper(cfg WorktreeReaperConfig) *worktreeReaper {
	r := newWorktreeReaper(cfg)
	old := time.Now().Add(-48 * time.Hour)
	r.mtime = func(string) (time.Time, error) { return old, nil } // everything "old" unless a test overrides
	r.pidAlive = func(int) bool { return false }
	return r
}

func verdictOf(rep *reapPassReport, name string) worktreeReport {
	for _, e := range rep.Entries {
		if filepath.Base(e.Path) == name {
			return e
		}
	}
	return worktreeReport{Verdict: "missing"}
}

func TestReaper_ClassifiesAndRemovesOnlySafeWorktrees(t *testing.T) {
	f := newReaperFixture(t)

	// merged: pushed and merged into origin/development → removed, branch deleted.
	merged := f.addWorktree(t, "agent-merged", "feat/merged")
	gitT(t, merged, "push", "-q", "-u", "origin", "feat/merged")
	gitT(t, f.root, "merge", "-q", "--no-edit", "feat/merged")
	gitT(t, f.root, "push", "-q", "origin", "development")

	// pushed: on a remote branch, not merged → removed, branch kept.
	pushed := f.addWorktree(t, "agent-pushed", "feat/pushed")
	gitT(t, pushed, "push", "-q", "-u", "origin", "feat/pushed")

	// unpushed: local-only commit → kept.
	f.addWorktree(t, "agent-unpushed", "feat/unpushed")

	// dirty: pushed but has an untracked file → kept.
	dirty := f.addWorktree(t, "agent-dirty", "feat/dirty")
	gitT(t, dirty, "push", "-q", "-u", "origin", "feat/dirty")
	if err := os.WriteFile(filepath.Join(dirty, "scratch.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// locked by a live pid → kept; locked by a dead pid → stale, removed.
	liveLocked := f.addWorktree(t, "agent-locked-live", "feat/locked-live")
	gitT(t, liveLocked, "push", "-q", "-u", "origin", "feat/locked-live")
	gitT(t, f.root, "worktree", "lock", "--reason", "claude agent agent-locked-live (pid 4242 start now)", liveLocked)
	staleLocked := f.addWorktree(t, "agent-locked-stale", "feat/locked-stale")
	gitT(t, staleLocked, "push", "-q", "-u", "origin", "feat/locked-stale")
	gitT(t, f.root, "worktree", "lock", "--reason", "claude agent agent-locked-stale (pid 4343)", staleLocked)

	// gone: directory deleted out from under git → pruned.
	gone := f.addWorktree(t, "agent-gone", "feat/gone")
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}

	// gone AND locked: a live holder keeps it (prune would skip it anyway);
	// a dead holder gets unlocked so prune can take it.
	goneLockedLive := f.addWorktree(t, "agent-gone-locked-live", "feat/gone-locked-live")
	gitT(t, f.root, "worktree", "lock", "--reason", "claude agent x (pid 4242 start now)", goneLockedLive)
	if err := os.RemoveAll(goneLockedLive); err != nil {
		t.Fatal(err)
	}
	goneLockedStale := f.addWorktree(t, "agent-gone-locked-stale", "feat/gone-locked-stale")
	gitT(t, f.root, "worktree", "lock", "--reason", "claude agent y (pid 4343)", goneLockedStale)
	if err := os.RemoveAll(goneLockedStale); err != nil {
		t.Fatal(err)
	}

	// young: pushed and clean but created just now → kept.
	young := f.addWorktree(t, "agent-young", "feat/young")
	gitT(t, young, "push", "-q", "-u", "origin", "feat/young")

	// A worktree OUTSIDE .claude/worktrees is never considered.
	outside := filepath.Join(t.TempDir(), "scratch-wt")
	gitT(t, f.root, "worktree", "add", "-q", "-b", "feat/outside", outside, "development")

	cfg := defaultWorktreeReaperConfig()
	cfg.DryRun = false
	r := newTestReaper(cfg)
	r.pidAlive = func(pid int) bool { return pid == 4242 }
	r.mtime = func(p string) (time.Time, error) {
		if filepath.Base(p) == "agent-young" {
			return time.Now(), nil
		}
		return time.Now().Add(-48 * time.Hour), nil
	}

	// Dry run first: nothing changes, verdicts are "removable".
	dry := r.reapProject("p", f.root, true)
	if dry.Error != "" {
		t.Fatalf("dry run error: %s", dry.Error)
	}
	if dry.Default != "origin/development" {
		t.Errorf("default branch = %q", dry.Default)
	}
	for _, name := range []string{"agent-merged", "agent-pushed", "agent-locked-stale"} {
		if v := verdictOf(dry, name); v.Verdict != verdictRemovable {
			t.Errorf("dry run %s = %s (%s), want removable", name, v.Verdict, v.Note)
		}
	}
	if _, err := os.Stat(merged); err != nil {
		t.Fatal("dry run must not remove anything")
	}
	if dry.Removed != 0 || dry.Removable != 3 {
		t.Errorf("dry run counts: removed %d removable %d", dry.Removed, dry.Removable)
	}

	// Apply.
	rep := r.reapProject("p", f.root, false)
	if rep.Error != "" {
		t.Fatalf("apply error: %s", rep.Error)
	}
	want := map[string]worktreeVerdict{
		"agent-merged":            verdictRemoved,
		"agent-pushed":            verdictRemoved,
		"agent-locked-stale":      verdictRemoved,
		"agent-unpushed":          verdictUnpushed,
		"agent-dirty":             verdictDirty,
		"agent-locked-live":       verdictLocked,
		"agent-gone":              verdictPruned,
		"agent-young":             verdictYoung,
		"agent-gone-locked-live":  verdictLocked,
		"agent-gone-locked-stale": verdictPruned,
	}
	for name, wv := range want {
		if v := verdictOf(rep, name); v.Verdict != wv {
			t.Errorf("%s = %s (%s), want %s", name, v.Verdict, v.Note, wv)
		}
	}
	if v := verdictOf(rep, "scratch-wt"); v.Verdict != "missing" {
		t.Errorf("worktree outside .claude/worktrees must never appear, got %s", v.Verdict)
	}
	if rep.Scanned != 10 || rep.Removed != 3 || rep.Pruned != 2 || rep.Locked != 2 {
		t.Errorf("counts: %+v", rep)
	}
	if !verdictOf(rep, "agent-merged").Merged {
		t.Error("agent-merged should be flagged merged")
	}
	if !verdictOf(rep, "agent-locked-stale").StaleLock {
		t.Error("agent-locked-stale should be flagged staleLock")
	}

	// Filesystem + branch effects.
	for _, p := range []string{merged, pushed, staleLocked} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s should be gone", filepath.Base(p))
		}
	}
	for _, p := range []string{dirty, liveLocked, young, outside} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s should still exist", filepath.Base(p))
		}
	}
	branches := gitT(t, f.root, "branch", "--list")
	if strings.Contains(branches, "feat/merged") {
		t.Error("merged branch should be deleted")
	}
	for _, b := range []string{"feat/pushed", "feat/unpushed", "feat/dirty", "feat/locked-live", "feat/locked-stale", "feat/young"} {
		if !strings.Contains(branches, b) {
			t.Errorf("branch %s must be kept", b)
		}
	}
	// The gone worktrees' admin entries were pruned — except the one whose
	// lock holder is alive, which git's prune skips and we must not unlock.
	wtList := gitT(t, f.root, "worktree", "list", "--porcelain")
	if strings.Contains(wtList, "agent-gone\n") || strings.Contains(wtList, "agent-gone-locked-stale") {
		t.Errorf("gone entries should have been pruned:\n%s", wtList)
	}
	if !strings.Contains(wtList, "agent-gone-locked-live") {
		t.Error("a gone entry with a LIVE lock holder must be left for git to prune later")
	}
	if strings.Contains(gitT(t, f.root, "status", "--porcelain"), "README") {
		t.Error("main worktree must be untouched")
	}
}

func TestReaper_MaxRemovePerPassCap(t *testing.T) {
	f := newReaperFixture(t)
	for _, n := range []string{"agent-a", "agent-b", "agent-c"} {
		wt := f.addWorktree(t, n, "feat/"+n)
		gitT(t, wt, "push", "-q", "-u", "origin", "feat/"+n)
	}
	cfg := defaultWorktreeReaperConfig()
	cfg.DryRun = false
	cfg.MaxRemovePerPass = 2
	rep := newTestReaper(cfg).reapProject("p", f.root, false)
	if rep.Removed != 2 || rep.Removable != 1 {
		t.Errorf("cap: removed %d removable %d", rep.Removed, rep.Removable)
	}
}

func TestReaper_NeverForces(t *testing.T) {
	// A pushed, clean worktree whose removal git refuses for its own
	// reasons (here: a submodule-free stand-in — a nested untracked dir
	// counts as dirty anyway, so simulate refusal via a read-only parent)
	// must surface as an error, not be forced. Covered structurally: the
	// reaper never passes --force; assert that by inspecting the git
	// calls it makes.
	f := newReaperFixture(t)
	wt := f.addWorktree(t, "agent-x", "feat/x")
	gitT(t, wt, "push", "-q", "-u", "origin", "feat/x")
	cfg := defaultWorktreeReaperConfig()
	cfg.DryRun = false
	r := newTestReaper(cfg)
	var calls []string
	real := r.git
	r.git = func(dir string, args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		return real(dir, args...)
	}
	r.reapProject("p", f.root, false)
	for _, c := range calls {
		if strings.Contains(c, "--force") || strings.Contains(c, "-D ") || strings.HasSuffix(c, "-D") {
			t.Errorf("reaper must never force: %q", c)
		}
	}
}

func TestParseWorktreeList(t *testing.T) {
	out := "worktree /r\nHEAD aaa\nbranch refs/heads/development\n\n" +
		"worktree /r/.claude/worktrees/agent-1\nHEAD bbb\nbranch refs/heads/feat/x\nlocked claude agent agent-1 (pid 77 start now)\n\n" +
		"worktree /r/.claude/worktrees/agent-2\nHEAD ccc\ndetached\nprunable gitdir file points to non-existent location\n"
	es := parseWorktreeList(out)
	if len(es) != 3 {
		t.Fatalf("got %d entries", len(es))
	}
	if es[0].Branch != "development" || es[0].Locked {
		t.Errorf("entry 0 = %+v", es[0])
	}
	if !es[1].Locked || es[1].LockReason != "claude agent agent-1 (pid 77 start now)" || es[1].Branch != "feat/x" {
		t.Errorf("entry 1 = %+v", es[1])
	}
	if !es[2].Detached || !es[2].Prunable || es[2].Branch != "" {
		t.Errorf("entry 2 = %+v", es[2])
	}
	alive, known := lockHolderAlive(es[1].LockReason, func(pid int) bool { return pid == 77 })
	if !alive || !known {
		t.Errorf("live pid: alive=%v known=%v", alive, known)
	}
	alive, known = lockHolderAlive("manual lock", func(int) bool { return false })
	if !alive || known {
		t.Errorf("unknown holder must be treated as alive: alive=%v known=%v", alive, known)
	}
}

func TestReaper_EndpointAndStatus(t *testing.T) {
	f := newReaperFixture(t)
	wt := f.addWorktree(t, "agent-p", "feat/p")
	gitT(t, wt, "push", "-q", "-u", "origin", "feat/p")
	f.addWorktree(t, "agent-u", "feat/u")

	srv := newDueScannerTestServer(t, map[string]ProjectConfig{"p": {Cwd: f.root}})
	cfg := defaultWorktreeReaperConfig()
	srv.reaper = newTestReaper(cfg)
	mux := newMux(srv)

	// POST starts an asynchronous pass (202); GET /worktrees reports it.
	post := func(body string) (*httptest.ResponseRecorder, map[string]any) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/worktrees/reap", strings.NewReader(body)))
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		if rec.Code == http.StatusAccepted {
			// Wait for the background pass to finish.
			for i := 0; i < 600; i++ {
				_, _, running := srv.reaper.snapshot(false)
				if !running {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
		return rec, m
	}
	get := func(q string) map[string]any {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/worktrees"+q, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /worktrees%s = %d: %s", q, rec.Code, rec.Body)
		}
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return m
	}
	rec, m := post(`{}`)
	if rec.Code != http.StatusAccepted || m["dryRun"] != true || m["started"] != true {
		t.Fatalf("default must start a dry run: %d %s", rec.Code, rec.Body)
	}
	full := get("?project=p")
	if full["running"] != false {
		t.Errorf("pass should have finished: %v", full)
	}
	pRep := full["reports"].(map[string]any)["p"].(map[string]any)
	if ents, ok := pRep["entries"].([]any); !ok || len(ents) != 2 {
		t.Errorf("GET /worktrees must carry entries: %v", pRep)
	}
	if get("?project=zzz")["reports"].(map[string]any)["p"] != nil {
		t.Error("project filter should exclude other projects")
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatal("dry run removed a worktree")
	}
	if rec, _ := post(`{"project":"nope"}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown project = %d", rec.Code)
	}

	// /status carries counts, not entries.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	var st map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	wtStatus, ok := st["worktrees"].(map[string]any)
	if !ok {
		t.Fatalf("expected /status.worktrees: %s", rec.Body)
	}
	p := wtStatus["projects"].(map[string]any)["p"].(map[string]any)
	if p["scanned"] != 2.0 || p["removable"] != 1.0 || p["unpushed"] != 1.0 || wtStatus["running"] != false {
		t.Errorf("status counts = %v", p)
	}
	if _, has := p["entries"]; has {
		t.Error("/status must not carry per-worktree entries")
	}

	// apply:true removes even though the periodic reaper is off.
	rec, m = post(`{"project":"P","apply":true}`)
	if rec.Code != http.StatusAccepted || m["dryRun"] != false {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Error("apply should have removed agent-p")
	}
	if _, err := os.Stat(filepath.Join(f.agents, "agent-u")); err != nil {
		t.Error("unpushed agent-u must survive")
	}

	srv.reaper = nil
	if rec, _ := post(`{}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("nil reaper = %d", rec.Code)
	}
}

func TestReaper_PassBudgetSkipsRemainder(t *testing.T) {
	f := newReaperFixture(t)
	for _, n := range []string{"agent-a", "agent-b", "agent-c"} {
		wt := f.addWorktree(t, n, "feat/"+n)
		gitT(t, wt, "push", "-q", "-u", "origin", "feat/"+n)
	}
	r := newTestReaper(defaultWorktreeReaperConfig())
	// A clock that jumps past the budget after the first entry is examined.
	base := time.Now()
	calls := 0
	r.now = func() time.Time {
		calls++
		if calls > 4 { // reapProject start, budget check, entryStart, record → then over budget
			return base.Add(time.Hour)
		}
		return base
	}
	r.budget = time.Minute
	rep := r.reapProject("p", f.root, true)
	if rep.Scanned != 3 || rep.Skipped != 2 || rep.Removable != 1 {
		t.Errorf("budget: %+v", rep)
	}
	for _, e := range rep.Entries {
		if e.Verdict == verdictSkipped && !strings.Contains(e.Note, "budget") {
			t.Errorf("skipped entry should say why: %+v", e)
		}
	}
	if rep.Duration <= 0 {
		t.Error("duration should be recorded")
	}
}

func TestResolveServeConfig_WorktreesDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := resolveServeConfig(0)
	w := cfg.Worktrees
	if w.Reap || !w.DryRun || w.MinAge != worktreeReaperDefaultMinAge || w.Interval != worktreeReaperDefaultInterval || w.MaxRemovePerPass != worktreeReaperDefaultMaxPer {
		t.Errorf("defaults = %+v", w)
	}
	// File overrides.
	dir := filepath.Join(os.Getenv("HOME"), ".config", "tn")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "serve.json"), []byte(`{"worktrees":{"reap":true,"dryRun":false,"minAge":"1h","interval":"5m","maxRemovePerPass":3}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	w = resolveServeConfig(0).Worktrees
	if !w.Reap || w.DryRun || w.MinAge != time.Hour || w.Interval != 5*time.Minute || w.MaxRemovePerPass != 3 {
		t.Errorf("file config = %+v", w)
	}
}
