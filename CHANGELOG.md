# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/).

> **Note:** Versions before 0.7.0 were unreleased internal iterations and are not
> individually documented here. This changelog starts from the project's first
> public snapshot.

## [Unreleased]

### Added

- MR watcher now also tracks GitLab review-comment threads on an open MR: a new reviewer comment (never the acting agent's own) appends a note to the task and messages its owner, and `/status.needsActionTasks[].mrOpenThreads` (also surfaced on an agent's `ownedTasks`) reports the unresolved-thread count. `TN_NO_MRCOMMENTS=1` disables just this sub-pass.
- GitHub pull requests are supported by the MR watcher next to GitLab merge requests; provider is detected from the URL or set per project with `codeHost`.

### Changed

- Repository restructured into `cmd/tn` (entrypoint) and `internal/tn` (all packages), with `serve.go` split by topic and the web UI bundle embedded from `webui/embed.go`; design documents moved to `docs/design/`. Build with `go build ./cmd/tn`.
- MR watcher polls GitLab incrementally: MR-bearing tasks routed to the same project are grouped and checked with one `glab api .../merge_requests?updated_after=...` listing call per project per pass, instead of a `glab mr view` call per task — a per-task state/discussions call now only happens for a task the listing reports changed, one never seen before, or (every ~30 minutes) as a periodic full refresh. GitHub polling is unchanged (no incremental listing wired up for it yet). `TN_NO_MRINCREMENTAL=1` reverts to the old one-call-per-task polling for every provider.

### Fixed

- MR watcher now polls every non-completed task with an `mr` field; a merged MR on a `needs-input` or `triage` task is noted on the task and shown as `mrState` in `/status.needsActionTasks` instead of being ignored (and is not auto-closed).

## [0.7.0] - 2026-09-08

First public snapshot.

### Added

- `orchestratorDoc` key in `serve.json` and `TN_ORCHESTRATOR_DOC` environment
  variable to locate the operating contract (`ORCHESTRATOR.md`) that spawned
  orchestrator sessions read. Resolution order: env var, `serve.json`,
  `~/.config/tn/ORCHESTRATOR.md`, next to the `tn` binary, current directory.
  Previously the path was compiled into the binary.
- `glab` is now resolved from Homebrew, `/usr/local/bin`, `/usr/bin`, or `PATH`
  instead of a single hardcoded location.

- Task CLI (`tn`) for creating, updating, listing, and querying tasks against the
  Obsidian TaskNotes plugin's HTTP API.
- Structured task-note body layout (ask/brief/description/history/reply/links blocks
  with stable anchors), so the CLI and daemon can update part of a note without
  clobbering the rest.
- Bridge daemon (`tn serve`): local HTTP server providing an agent registry, an
  inbox long-poll message bus, and a TaskNotes webhook receiver.
- Generation-based orchestrator lifecycle: spawns Claude Code orchestrator sessions
  in tmux per project, with drain/retire semantics across generations.
- Dashboard note and per-session pages rendered into the vault, kept live via the
  daemon's internal state.
- Embedded web UI (`GET /ui`) built with React and TanStack Router, with live
  updates over Server-Sent Events and a `/status` endpoint for tray-app and web UI
  polling.
- Obsidian plugin: a thin iframe wrapper around the daemon's own `/ui`, with a
  postMessage bridge for opening vault notes and syncing Obsidian's theme into the
  iframe.
- Stuck-session detection, combining tmux pane scraping with a Claude Code
  `PermissionRequest`/`PermissionDenied` hook, surfaced as native macOS approval
  dialogs.
- Merge-request watcher (via `glab`) that tracks MR state for in-flight work.
- Due-task scanner that surfaces tasks needing attention without manual polling.
- Jira request support: routes `--jira`/`--jira-nc` fields to an external Jira
  integration, with a `jiraStatusMap` for status transitions and a deliberate
  comment-only path for non-conformity issues.
- Credential profiles with automatic swap on usage-limit errors, backed by the
  macOS Keychain.
- Worktree reaper that cleans up stale git worktrees left behind by isolated
  worker sessions.
- A background-QoS fix (`taskpolicy`) so spawned sessions don't get starved of CPU
  under macOS App Nap-style throttling.

[Unreleased]: https://github.com/maylukas/claude-code-tasknotes/compare/v0.7.0...HEAD
[0.7.0]: https://github.com/maylukas/claude-code-tasknotes/releases/tag/v0.7.0
