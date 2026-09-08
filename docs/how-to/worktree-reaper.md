# Worktree reaper

Reclaim disk space from finished agent git worktrees without risking anyone's unpushed work.

## The problem

Every worker subagent spawned with `isolation:"worktree"` gets a git worktree under `<repo>/.claude/worktrees/agent-*`. The harness only removes a worktree automatically when it's unchanged at exit, and a real worker almost always commits something, so in practice these accumulate. A two-week census of one busy repo found over a hundred: some locked, some dirty, some carrying commits never pushed anywhere, some already merged and safe to remove, some whose directory was already gone but the git admin metadata still pointed at it. `ORCHESTRATOR.md` makes cleanup the orchestrator's own job on task completion, but the census shows that doesn't reliably happen either.

## Safety rules

The reaper only ever looks under `<cwd>/.claude/worktrees/`, never the main worktree, never a session's own scratchpad worktree. A worktree is removed only when **every** one of these holds:

- **Not locked by a live process.** The harness locks a worktree with a reason string like `claude agent <name> (pid N ...)`. A lock whose pid is no longer running is a stale lock and gets unlocked first; a lock with no parseable pid at all, or a live pid, is left alone and reported as `locked`.
- **Older than `minAge`** (by directory modification time), so a worker between creation and its first commit is never raced.
- **Clean**: `git status --porcelain` (including untracked files) is empty.
- **Its HEAD commit exists on some remote branch**: `git branch -r --contains HEAD`, regardless of whether that branch has been merged. A pushed-but-unmerged branch is enough to protect the worktree; an unpushed commit is the one thing a worktree can uniquely hold, and that's never touched.

Additionally:

- The local branch is deleted only when it's merged into the repository's default branch, with `git branch -d`, never `-D`.
- `git worktree remove` is never forced.
- Everything that doesn't meet every rule above (dirty, unpushed, locked, too young, or unparseable) is counted and reported, never acted on. The report is as much the point as the removal, so you always have an honest picture of what's actually out there.

## Dry run

```bash
tn worktrees                    # every configured project
tn worktrees -p myapp           # one project
```

Expected output, one block per project:

```text
myapp (dry run), /path/to/myapp
  scanned 42 in 3s (14:02): removed 0, removable 12, pruned 2, locked 1, young 3, dirty 9, unpushed 15, skipped 0, errors 0 (default origin/main)
  removable  agent-a1b2c3d4             feature/foo                            50h       worktree and merged branch removed
  ...
dry run: nothing was removed; re-run with --apply to remove the 'removable' entries
```

Verdict column meanings: `removed`, `removable` (dry run, or the per-pass cap was hit), `pruned` (directory already gone, admin entry cleaned up), `locked`, `young`, `dirty`, `unpushed`, `error`, `skipped` (pass time budget exhausted before reaching this entry).

## Apply

```bash
tn worktrees -p myapp --apply
```

This calls the daemon (`POST /worktrees/reap`) and polls until the pass finishes. A repo with a hundred-plus worktrees can take minutes even at normal I/O priority, so the request itself returns immediately (202 Accepted) and `tn worktrees` polls `GET /worktrees` for you.

## Enable the background sweep

By default the periodic sweep is off. Turn it on in `~/.config/tn/serve.json`:

```json
{
  "worktrees": {
    "reap": true,
    "dryRun": true,
    "minAge": "2h",
    "interval": "30m",
    "maxRemovePerPass": 20
  }
}
```

Leave `dryRun: true` for the first several passes: read them in `~/Library/Logs/tn-serve.log` (or your `tn serve` terminal) before flipping `dryRun` to `false`. Reload the daemon the same way as any other `serve.json` change (see [Add a project](add-a-project.md), step 2) for this to take effect. `TN_NO_REAPER=1` disables the periodic sweep entirely regardless of `reap`.

## Read the results

```bash
curl -s localhost:8391/status | python3 -c 'import json,sys; print(json.load(sys.stdin)["worktrees"])'
```

`/status.worktrees` gives you `enabled`, `dryRun`, whether a pass is `running` right now, `lastRunAt`, and per-project counts (no per-entry detail; use `GET /worktrees` or `tn worktrees` for that). The log line format for each pass:

```text
serve: worktree reaper [dry-run] myapp: scanned 42 in 3s — removed 0, removable 12, pruned 2, locked 1, young 3, dirty 9, unpushed 15, skipped 0, errors 0 (default origin/main)
```

## Tuning keys

| Key | Default | Meaning |
|---|---|---|
| `reap` | `false` | Whether the periodic background sweep runs at all. |
| `dryRun` | `true` | Report only; nothing is removed until set to `false`. |
| `minAge` | `2h` | Minimum directory age before a worktree is even considered for removal. |
| `interval` | `30m` | How often the periodic sweep runs. |
| `maxRemovePerPass` | `20` | Cap on removals in one pass; remaining removable entries carry over to the next pass rather than all going at once. |

## Removal-timeout behavior

Two separate timeouts protect the sweep from a wedged git process: most git queries (`status`, `branch -r --contains`, etc.) are bounded at 3 minutes each, but `git worktree remove` itself gets a much longer 20-minute allowance. Deleting a large working tree (a fresh monorepo checkout with a full `node_modules`, for example) can genuinely take that long, and killing it partway through is worse than letting it run: a half-deleted tree reads as "dirty" afterward and is then never touched again by the safety rules above. Separately, an entire pass over one project is capped at 30 minutes; anything not reached inside that budget is reported as `skipped` and picked up on the next scheduled pass rather than stalling the sweep indefinitely.

## See also

- [Troubleshooting](troubleshooting.md)
- [Run tn serve as a service](run-as-a-service.md): the background-QoS note explains why an unthrottled `git status` matters here.
