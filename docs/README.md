# Documentation index

Documentation for tasknotes-cli is organized using the [Diátaxis](https://diataxis.fr/)
framework: tutorials teach by doing, how-to guides solve one task, reference is
exhaustive, and explanation gives you the why.

## Tutorials

Learn by doing, one guided path at a time.

- [Your first task](tutorials/first-task.md): install the CLI, connect it to a
  TaskNotes vault, and create and close your first task.

## How-to guides

Goal-oriented instructions for a specific task, assuming you already know the basics.

- [Set up TaskNotes for tn](how-to/set-up-tasknotes.md): configure TaskNotes itself
  (HTTP API, custom statuses, user fields, tags) for everything else in this list.
- [Add a project](how-to/add-a-project.md): wire a new repository into the bridge
  daemon so it can spawn orchestrator sessions for it.
- [Run the daemon as a service](how-to/run-as-a-service.md): keep `tn serve` running
  under launchd.
- [Install the Claude Code hook](how-to/install-claude-hook.md): wire up the
  `PermissionRequest`/`PermissionDenied` hook so stuck-session detection doesn't have
  to fall back to pane scraping.
- [Credential profiles](how-to/credential-profiles.md): store and auto-swap Claude
  Code credentials via the macOS Keychain.
- [Worktree reaper](how-to/worktree-reaper.md): configure automatic cleanup of stale
  git worktrees left behind by isolated worker sessions.
- [Obsidian plugin](how-to/obsidian-plugin.md): install and configure the companion
  Obsidian plugin.
- [Web UI development](how-to/web-ui-development.md): work on `webui/` and get your
  changes embedded in the daemon.
- [Jira integration](how-to/jira-integration.md): route task fields to an external
  Jira integration.
- [Troubleshooting](how-to/troubleshooting.md): diagnose the most common failure
  modes.

## Reference

Exhaustive, neutral descriptions of the system's surface.

- [CLI reference](reference/cli.md): every `tn` subcommand, flag, and output shape.
- [HTTP API reference](reference/http-api.md): every endpoint the bridge daemon
  exposes.
- [Configuration reference](reference/configuration.md): every config file, key,
  default, and environment variable.
- [Note layout reference](reference/note-layout.md): the structured task-note body
  format (ask/brief/description/history/reply/links blocks).

## Explanation

Background and design rationale. No step-by-step instructions here.

- [Lifecycle of a task](explanation/lifecycle-of-a-task.md): how a task moves from
  creation through routing, spawning, and completion.
- [Generations and drain](explanation/generations-and-drain.md): why orchestrator
  sessions are versioned as generations, and how drain/retire works.
- [Security model](explanation/security-model.md): what trust boundaries this system
  assumes, and what it deliberately does not defend against.
- [Design history](explanation/design-history.md): historical design documents,
  kept for context on decisions that shaped the current implementation.

## Project-level documents

- [Architecture](../ARCHITECTURE.md): how the binary, daemon, web UI, and Obsidian
  plugin fit together.
- [Contributing](../CONTRIBUTING.md): development setup, test conventions, and the
  pull request process.
- [Changelog](../CHANGELOG.md): notable changes by release.
- [Security policy](../SECURITY.md): how to report a vulnerability, and this
  project's scope assumptions.

### Historical design documents

The `SPEC-*.md` files in [`docs/design/`](design/) (`SPEC-serve.md`, `SPEC-dashboard.md`,
`SPEC-generations.md`, `SPEC-note-layout.md`, `SPEC-ui-v2.md`, `SPEC-obsidian-plugin.md`,
`IMPLEMENTATION-PLAN-REMOTE-EXECUTION.md`)
are the original implementation specs written as the system was built. They're partly
stale relative to the code, but useful as design history. See
[Design history](explanation/design-history.md) for context on how to read them.
